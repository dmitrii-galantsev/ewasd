package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dmitrii-galantsev/ewasd/internal/domain"
	"github.com/dmitrii-galantsev/ewasd/internal/fsops"
	"github.com/dmitrii-galantsev/ewasd/internal/gitutil"
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

type CleanOptions struct {
	Mode               string
	IncludeDirectories bool
}

func (e *Engine) PlanClean(cwd, explicit string, options CleanOptions) (domain.CleanPlan, error) {
	state, err := e.store.Read()
	if err != nil {
		return domain.CleanPlan{}, err
	}
	return e.planCleanFromState(state, cwd, explicit, options)
}

func (e *Engine) planCleanFromState(state domain.State, cwd, explicit string, options CleanOptions) (domain.CleanPlan, error) {
	if err := e.requireNoRecovery(); err != nil {
		return domain.CleanPlan{}, err
	}
	detection, err := detectFromState(state, cwd, explicit)
	if err != nil {
		return domain.CleanPlan{}, err
	}
	if !detection.Matched {
		return domain.CleanPlan{}, fmt.Errorf("%w: %s", ErrNotFound, strings.Join(detection.Trace, "; "))
	}
	if detection.ProjectID == "" {
		return domain.CleanPlan{}, fmt.Errorf("%w: clean requires a registered checkout; run ewasd link first", ErrConflict)
	}
	lookupID := detection.ProjectID
	if lookupID == "" {
		lookupID = detection.TemplateProjectID
	}
	project := state.ProjectByID(lookupID)
	if project == nil {
		return domain.CleanPlan{}, ErrNotFound
	}
	if filepath.Clean(project.Root) != filepath.Clean(detection.TargetRoot) {
		return domain.CleanPlan{}, fmt.Errorf("%w: detected project does not own clean root", ErrConflict)
	}
	if ok, detail := gitutil.CheckExclude(project.ID, project.Root, project.GitRoot, entryPaths(project.Entries)); !ok {
		return domain.CleanPlan{}, fmt.Errorf("%w: Git protection is not healthy: %s; run ewasd link first", ErrConflict, detail)
	}
	cleanRoot := detection.TargetRoot
	if isInside(detection.CWD, detection.TargetRoot) {
		cleanRoot = detection.CWD
	}
	scope, err := filepath.Rel(detection.GitRoot, cleanRoot)
	if err != nil || scope == ".." || strings.HasPrefix(scope, ".."+string(filepath.Separator)) {
		return domain.CleanPlan{}, errors.New("clean scope is outside the Git root")
	}
	projectScope, err := filepath.Rel(detection.GitRoot, detection.TargetRoot)
	if err != nil || projectScope == ".." || strings.HasPrefix(projectScope, ".."+string(filepath.Separator)) {
		return domain.CleanPlan{}, errors.New("project scope is outside the Git root")
	}
	patterns, err := gitutil.ManagedExcludePatterns(detection.GitRoot)
	if err != nil {
		return domain.CleanPlan{}, err
	}
	for _, entry := range project.Entries {
		rootPath := filepath.ToSlash(filepath.Join(projectScope, filepath.FromSlash(entry.Path)))
		patterns = append(patterns, "/"+gitutil.EscapeGitPattern(strings.TrimPrefix(rootPath, "./")))
	}
	sort.Strings(patterns)
	patterns = compactStrings(patterns)
	preview, err := gitutil.PreviewClean(cleanRoot, gitutil.CleanOptions{
		Mode: options.Mode, IncludeDirectories: options.IncludeDirectories,
		ProtectedPatterns: patterns,
	})
	if err != nil {
		return domain.CleanPlan{}, err
	}
	protectedPaths := []string{}
	healthy := []string{}
	healthyLinks := []domain.CleanProtectedLink{}
	projects := append([]domain.Project(nil), state.Projects...)
	if detection.ProjectID == "" {
		virtual := *project
		virtual.Root = detection.TargetRoot
		virtual.GitRoot = detection.GitRoot
		projects = append(projects, virtual)
	}
	seenTargets := map[string]bool{}
	for _, candidateProject := range projects {
		for _, entry := range candidateProject.Entries {
			target, err := fsops.SafeTarget(candidateProject.Root, entry.Path)
			if err != nil || !isInside(target, cleanRoot) || seenTargets[target] {
				continue
			}
			seenTargets[target] = true
			protectedPaths = append(protectedPaths, target)
			view := e.inspect(candidateProject, entry)
			if view.Status == "linked" {
				healthy = append(healthy, target)
				healthyLinks = append(healthyLinks, domain.CleanProtectedLink{Path: target, Source: view.Source})
			}
		}
	}
	sort.Strings(protectedPaths)
	sort.Strings(healthy)
	plan := domain.CleanPlan{
		ID: randomID(12), ExpectedRevision: state.Revision,
		ProjectID: detection.ProjectID, ProjectName: project.Name,
		Root: cleanRoot, GitRoot: detection.GitRoot, Scope: filepath.ToSlash(scope),
		DetectionMethod: detection.Method, Mode: options.Mode, IncludeDirectories: options.IncludeDirectories,
		Candidates: preview.Candidates, ProtectedPatterns: patterns,
		ProtectedPaths: protectedPaths, HealthyPaths: healthy, HealthyLinks: healthyLinks,
		SkippedRepositories: preview.SkippedRepositories, Command: preview.Command, CreatedAt: e.now(),
	}
	for _, candidate := range plan.Candidates {
		candidatePath := cleanCandidatePath(plan.Root, candidate)
		for _, protected := range plan.ProtectedPaths {
			if candidatePath == protected || isInside(protected, candidatePath) {
				return domain.CleanPlan{}, fmt.Errorf("%w: git clean candidate %s contains protected path %s", ErrConflict, candidate, protected)
			}
		}
	}
	sealCleanPlan(&plan)
	return plan, nil
}

func (e *Engine) Clean(cwd, explicit string, options CleanOptions, expectedRevision uint64, approvedFingerprint string) (domain.ApplyResult, error) {
	var result domain.ApplyResult
	operationID := randomID(12)
	state, warnings, err := e.store.Transact(&expectedRevision, func(state *domain.State) (store.Effects, error) {
		plan, err := e.planCleanFromState(*state, cwd, explicit, options)
		if err != nil {
			return store.Effects{}, err
		}
		if approvedFingerprint != "" && plan.Fingerprint != approvedFingerprint {
			return store.Effects{}, fmt.Errorf("clean plan changed after preview")
		}
		if len(plan.Candidates) == 0 {
			result = domain.ApplyResult{OK: true, Outcome: "no_change", Revision: state.Revision, Action: "clean", Summary: "No cleanable paths found"}
			state.AddEvent(domain.Event{ID: randomID(8), ProjectID: plan.ProjectID, Action: "clean", Summary: "Clean applied with no matching paths", CreatedAt: e.now()})
			return store.Effects{}, nil
		}
		record := cleanRecord{ID: operationID, Phase: "started", ProjectID: plan.ProjectID, Root: plan.Root, Mode: plan.Mode, Candidates: append([]string(nil), plan.Candidates...), ProtectedPatterns: append([]string(nil), plan.ProtectedPatterns...), StartedAt: e.now()}
		if err := e.writeCleanRecord(record); err != nil {
			return store.Effects{}, err
		}
		removed, err := gitutil.ApplyClean(plan.Root, gitutil.CleanOptions{
			Mode: plan.Mode, IncludeDirectories: plan.IncludeDirectories,
			ProtectedPatterns: plan.ProtectedPatterns,
		})
		if err != nil {
			record.Phase, record.Error = "failed", err.Error()
			_ = e.writeCleanRecord(record)
			return store.Effects{}, err
		}
		for _, link := range plan.HealthyLinks {
			if !fsops.LinkPointsTo(link.Path, link.Source) {
				err := fmt.Errorf("managed link %s was not preserved by git clean", link.Path)
				record.Phase, record.Error = "failed", err.Error()
				_ = e.writeCleanRecord(record)
				return store.Effects{}, err
			}
		}
		summary := fmt.Sprintf("Cleaned %d path(s); protected %d ewasd pattern(s)", len(removed), len(plan.ProtectedPatterns))
		state.AddEvent(domain.Event{ID: randomID(8), ProjectID: plan.ProjectID, Action: "clean", Summary: summary, CreatedAt: e.now()})
		result = domain.ApplyResult{
			OK: true, Outcome: "completed", OperationID: operationID, Action: "clean",
			Changed: append([]string(nil), removed...),
			Skipped: append([]string(nil), plan.SkippedRepositories...),
			Summary: summary,
		}
		record.Phase, record.CompletedAt, record.Removed = "completed", e.now(), append([]string(nil), removed...)
		return store.Effects{Commit: func() error { return e.writeCleanRecord(record) }}, nil
	})
	result.Revision = state.Revision
	result.Warnings = warnings
	return result, err
}

type cleanRecord struct {
	ID                string    `json:"id"`
	Phase             string    `json:"phase"`
	ProjectID         string    `json:"project_id"`
	Root              string    `json:"root"`
	Mode              string    `json:"mode"`
	Candidates        []string  `json:"candidates"`
	ProtectedPatterns []string  `json:"protected_patterns"`
	Removed           []string  `json:"removed,omitempty"`
	StartedAt         time.Time `json:"started_at"`
	CompletedAt       time.Time `json:"completed_at,omitempty"`
	Error             string    `json:"error,omitempty"`
}

func (e *Engine) writeCleanRecord(record cleanRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return store.AtomicWrite(filepath.Join(e.store.Root(), "clean-records", record.ID+".json"), append(data, '\n'), 0o600)
}

func sealCleanPlan(plan *domain.CleanPlan) {
	payload := struct {
		ExpectedRevision   uint64
		ProjectID          string
		Root               string
		GitRoot            string
		Scope              string
		Mode               string
		IncludeDirectories bool
		Candidates         []string
		ProtectedPatterns  []string
		ProtectedPaths     []string
		HealthyLinks       []domain.CleanProtectedLink
	}{plan.ExpectedRevision, plan.ProjectID, plan.Root, plan.GitRoot, plan.Scope, plan.Mode, plan.IncludeDirectories, plan.Candidates, plan.ProtectedPatterns, plan.ProtectedPaths, plan.HealthyLinks}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(encoded)
	plan.Fingerprint = hex.EncodeToString(digest[:])
}

func cleanCandidatePath(root, candidate string) string {
	clean := filepath.Clean(filepath.FromSlash(candidate))
	if clean == "." {
		return filepath.Clean(root)
	}
	return filepath.Clean(filepath.Join(root, clean))
}

func compactStrings(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
