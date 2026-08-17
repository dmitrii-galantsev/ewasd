package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dmitrii-galantsev/ewasd/internal/domain"
	"github.com/dmitrii-galantsev/ewasd/internal/fsops"
	"github.com/dmitrii-galantsev/ewasd/internal/gitutil"
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

func (e *Engine) PlanLink(cwd, explicit string) (domain.Plan, error) {
	state, err := e.store.Read()
	if err != nil {
		return domain.Plan{}, err
	}
	return e.planLinkFromState(state, cwd, explicit)
}

func (e *Engine) planLinkFromState(state domain.State, cwd, explicit string) (domain.Plan, error) {
	detection, err := detectFromState(state, cwd, explicit)
	if err != nil {
		return domain.Plan{}, err
	}
	if !detection.Matched {
		return domain.Plan{}, fmt.Errorf("%w: %s", ErrNotFound, strings.Join(detection.Trace, "; "))
	}
	templateID := detection.TemplateProjectID
	projectID := detection.ProjectID
	lookupID := projectID
	if lookupID == "" {
		lookupID = templateID
	}
	template := state.ProjectByID(lookupID)
	if template == nil {
		return domain.Plan{}, ErrNotFound
	}
	project := *template
	project.Root = detection.TargetRoot
	project.GitRoot = detection.GitRoot
	project.Remote = detection.Remote
	plan := e.basePlan("link", state.Revision, project)
	plan.ProjectID = projectID
	plan.TemplateProjectID = templateID
	plan.TargetRoot = detection.TargetRoot
	plan.GitRoot = detection.GitRoot
	plan.Remote = detection.Remote
	plan.DetectionMethod = detection.Method
	plan.NewProject = projectID == ""
	if e.blockPlanForRecovery(&plan) {
		sealPlan(&plan)
		return plan, nil
	}
	if plan.NewProject && detection.Method == "path" && explicit == "" {
		plan.Conflicts = append(plan.Conflicts, conflict(detection.TargetRoot, "path-guess-needs-override", "path-only inference is preview-only; rerun with --project ID or name to confirm the source profile"))
		plan.Summary = fmt.Sprintf("Path component suggests %q, but automatic registration requires a remote match or explicit --project", project.Name)
		plan.Safe = false
		sealPlan(&plan)
		return plan, nil
	}
	if plan.NewProject {
		plan.Steps = append(plan.Steps, domain.PlanStep{Path: detection.TargetRoot, Action: "register", Detail: fmt.Sprintf("Bind inferred checkout to shared source profile %s", template.SourceID)})
	}
	linked := 0
	for _, entry := range project.Entries {
		view := e.inspect(project, entry)
		switch view.Status {
		case "linked":
			linked++
		case "missing":
			plan.Steps = append(plan.Steps, domain.PlanStep{Path: entry.Path, Action: "link", From: view.Source, To: view.Target, Detail: "Create the missing managed symlink without replacing another path"})
		default:
			plan.Conflicts = append(plan.Conflicts, conflict(entry.Path, view.Status, view.Detail))
		}
	}
	ignoreOK, ignoreState := gitutil.CheckExclude(project.ID, project.Root, project.GitRoot, entryPaths(project.Entries))
	if plan.NewProject || !ignoreOK {
		plan.Steps = append(plan.Steps, domain.PlanStep{Path: ".git/info/exclude", Action: "git-ignore", Detail: ignoreState + "; replace only this project's marked block"})
	}
	plan.Safe = true
	plan.Summary = fmt.Sprintf("%s %q by %s: %d already linked, %d safe step(s), %d conflict(s) left untouched",
		map[bool]string{true: "Register and link", false: "Reconcile"}[plan.NewProject], project.Name, detection.Method, linked, len(plan.Steps), len(plan.Conflicts))
	sealPlan(&plan)
	return plan, nil
}

func (e *Engine) Link(cwd, explicit, approvedFingerprint string) (domain.ApplyResult, error) {
	initial, err := e.PlanLink(cwd, explicit)
	if err != nil {
		return domain.ApplyResult{}, err
	}
	if approvedFingerprint == "" {
		approvedFingerprint = initial.Fingerprint
	}
	if !initial.Safe {
		return domain.ApplyResult{}, fmt.Errorf("%w: %s", ErrConflict, initial.Summary)
	}
	if len(initial.Steps) == 0 {
		return domain.ApplyResult{OK: true, Outcome: "no_change", Revision: initial.ExpectedRevision, Action: "link", Skipped: conflictPaths(initial.Conflicts), Summary: initial.Summary}, nil
	}
	operationID := randomID(12)
	changed := []string{}
	skipped := conflictPaths(initial.Conflicts)
	state, warnings, err := e.store.Transact(&initial.ExpectedRevision, func(state *domain.State) (store.Effects, error) {
		if err := e.requireNoRecovery(); err != nil {
			return store.Effects{}, err
		}
		current, err := e.planLinkFromState(*state, cwd, explicit)
		if err != nil {
			return store.Effects{}, err
		}
		if current.Fingerprint != approvedFingerprint {
			return store.Effects{}, fmt.Errorf("%w: link steps or repository detection changed after preview", store.ErrStaleRevision)
		}
		project := state.ProjectByID(current.ProjectID)
		if current.NewProject {
			template := state.ProjectByID(current.TemplateProjectID)
			if template == nil {
				return store.Effects{}, ErrNotFound
			}
			for _, existing := range state.Projects {
				if pathsOverlap(existing.Root, current.TargetRoot) {
					return store.Effects{}, fmt.Errorf("%w: inferred root overlaps %s", ErrConflict, existing.Root)
				}
			}
			id, err := uniqueProjectID(state, template.Name)
			if err != nil {
				return store.Effects{}, err
			}
			now := e.now()
			entries := append([]domain.Entry(nil), template.Entries...)
			projectValue := domain.Project{
				ID: id, SourceID: template.SourceID, Name: template.Name,
				Root: current.TargetRoot, GitRoot: current.GitRoot, Remote: current.Remote,
				LegacySourceRoot: template.LegacySourceRoot,
				CreatedAt:        now, UpdatedAt: now, Entries: entries,
			}
			state.Projects = append(state.Projects, projectValue)
			project = &state.Projects[len(state.Projects)-1]
			changed = append(changed, current.TargetRoot)
		}
		if project == nil {
			return store.Effects{}, ErrNotFound
		}
		projectCopy := *project
		steps := append([]domain.PlanStep(nil), current.Steps...)
		state.AddEvent(domain.Event{ID: randomID(8), ProjectID: project.ID, Action: "link", Summary: current.Summary, CreatedAt: e.now()})
		effects := store.Effects{Commit: func() error {
			var errs []error
			for _, step := range steps {
				if step.Action != "link" {
					continue
				}
				if err := os.MkdirAll(filepath.Dir(step.To), 0o755); err != nil {
					errs = append(errs, err)
					continue
				}
				if err := fsops.AtomicSymlink(step.From, step.To, operationID); err != nil {
					if !fsops.LinkPointsTo(step.To, step.From) {
						errs = append(errs, fmt.Errorf("link %s: %w", step.Path, err))
					}
					continue
				}
				changed = append(changed, step.Path)
			}
			if err := gitutil.UpdateExclude(projectCopy.ID, projectCopy.Root, projectCopy.GitRoot, entryPaths(projectCopy.Entries)); err != nil {
				errs = append(errs, err)
			}
			return errors.Join(errs...)
		}}
		return effects, nil
	})
	if err != nil {
		return domain.ApplyResult{}, err
	}
	outcome := "completed"
	if len(warnings) > 0 || len(skipped) > 0 {
		outcome = "completed_with_skips"
	}
	sort.Strings(changed)
	return domain.ApplyResult{OK: true, Outcome: outcome, OperationID: operationID, Revision: state.Revision, Action: "link", Changed: changed, Skipped: skipped, Warnings: warnings, Summary: initial.Summary}, nil
}
