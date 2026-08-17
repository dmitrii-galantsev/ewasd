package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/dmitrii-galantsev/ewasd/internal/domain"
	"github.com/dmitrii-galantsev/ewasd/internal/fsops"
	"github.com/dmitrii-galantsev/ewasd/internal/gitutil"
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

var (
	ErrNotFound        = errors.New("project or entry not found")
	ErrConflict        = errors.New("operation has conflicts and was not applied")
	ErrRecoveryPending = errors.New("an interrupted transaction must be recovered before new writes")
)

type Engine struct {
	store *store.Store
	now   func() time.Time
}

func New(stateStore *store.Store) *Engine {
	return &Engine{store: stateStore, now: func() time.Time { return time.Now().UTC() }}
}

func (e *Engine) Register(root, name string) (domain.Project, uint64, error) {
	checkout, err := gitutil.InspectCheckout(root)
	if err != nil {
		return domain.Project{}, 0, err
	}
	if name == "" {
		name = filepath.Base(checkout.Root)
	}
	created := domain.Project{}
	state, _, err := e.store.Transact(nil, func(state *domain.State) (store.Effects, error) {
		if err := e.requireNoRecovery(); err != nil {
			return store.Effects{}, err
		}
		for _, existing := range state.Projects {
			if pathsOverlap(existing.Root, checkout.Root) {
				return store.Effects{}, fmt.Errorf("%w: checkout overlaps registered root %s", ErrConflict, existing.Root)
			}
		}
		id := ""
		profileRoot := ""
		for range 10 {
			candidate := slug(name) + "-" + randomID(4)
			candidateRoot := filepath.Join(e.store.Root(), "profiles", candidate)
			if err := os.Mkdir(candidateRoot, 0o700); err == nil {
				id, profileRoot = candidate, candidateRoot
				break
			} else if !errors.Is(err, os.ErrExist) {
				return store.Effects{}, err
			}
		}
		if id == "" {
			return store.Effects{}, errors.New("could not allocate a unique project id")
		}
		if err := os.Mkdir(filepath.Join(profileRoot, "files"), 0o700); err != nil {
			_ = os.Remove(profileRoot)
			return store.Effects{}, err
		}
		rollback := func() error { return os.RemoveAll(profileRoot) }
		now := e.now()
		created = domain.Project{
			ID:        id,
			SourceID:  id,
			Name:      name,
			Root:      checkout.Root,
			GitRoot:   checkout.GitRoot,
			Remote:    checkout.Remote,
			CreatedAt: now,
			UpdatedAt: now,
			Entries:   []domain.Entry{},
		}
		state.Projects = append(state.Projects, created)
		state.AddEvent(domain.Event{
			ID: randomID(8), ProjectID: id, Action: "register",
			Summary: "Registered checkout without adopting files", CreatedAt: now,
		})
		return store.Effects{Rollback: rollback}, nil
	})
	if err != nil {
		return domain.Project{}, state.Revision, err
	}
	return created, state.Revision, nil
}

func (e *Engine) ImportLegacyProject(plan domain.LegacyProjectPlan) (domain.ApplyResult, error) {
	operationID := randomID(12)
	changed := []string{}
	state, warnings, err := e.store.Transact(nil, func(state *domain.State) (store.Effects, error) {
		if err := e.requireNoRecovery(); err != nil {
			return store.Effects{}, err
		}
		root, err := filepath.EvalSymlinks(plan.Root)
		if err != nil {
			return store.Effects{}, err
		}
		if existing := state.ProjectByRoot(root); existing != nil {
			for _, item := range plan.Entries {
				entry := existing.EntryByPath(item.Path)
				if entry == nil {
					return store.Effects{}, fmt.Errorf("%w: registered project is missing legacy entry %s", ErrConflict, item.Path)
				}
				source, err := fsops.SafeTarget(e.store.Root(), entry.SourceRel)
				if err == nil {
					err = fsops.SyncTreeModes(item.LegacySource, source)
				}
				if err != nil || !fsops.LinkPointsTo(item.Target, source) {
					return store.Effects{}, fmt.Errorf("%w: existing migrated entry %s is not healthy", ErrConflict, item.Path)
				}
			}
			return store.Effects{}, nil
		}
		for _, existing := range state.Projects {
			if pathsOverlap(existing.Root, root) {
				return store.Effects{}, fmt.Errorf("%w: checkout overlaps registered root %s", ErrConflict, existing.Root)
			}
		}
		checkout, err := gitutil.InspectCheckout(root)
		if err != nil {
			return store.Effects{}, err
		}
		if checkout.GitRoot != plan.GitRoot {
			return store.Effects{}, errors.New("legacy plan Git root changed after discovery")
		}
		id, err := uniqueProjectID(state, plan.Name)
		if err != nil {
			return store.Effects{}, err
		}
		sourceID := ""
		profileRoot := ""
		profileCreated := false
		for _, existing := range state.Projects {
			if existing.LegacySourceRoot == plan.SourceRoot {
				sourceID = existing.SourceID
				profileRoot = filepath.Join(e.store.Root(), "profiles", sourceID)
				break
			}
		}
		if sourceID == "" {
			sourceID = id
			profileRoot = filepath.Join(e.store.Root(), "profiles", sourceID)
			if err := os.Mkdir(profileRoot, 0o700); err != nil {
				return store.Effects{}, err
			}
			if err := os.Mkdir(filepath.Join(profileRoot, "files"), 0o700); err != nil {
				_ = os.Remove(profileRoot)
				return store.Effects{}, err
			}
			profileCreated = true
		}
		rollback := func() error {
			var errs []error
			journals, _ := e.listJournals()
			for _, journal := range journals {
				if journal.ProjectID != id || journal.Action != "legacy-import" {
					continue
				}
				if err := e.rollbackLegacySwitch(journal); err != nil {
					errs = append(errs, err)
				}
			}
			if len(errs) == 0 && profileCreated {
				if err := os.RemoveAll(profileRoot); err != nil {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		}
		effects := store.Effects{Rollback: rollback}
		entries := make([]domain.Entry, 0, len(plan.Entries))
		journals := make([]domain.Journal, 0, len(plan.Entries))
		for _, item := range plan.Entries {
			path, err := fsops.ValidateRelative(item.Path)
			if err != nil || containsGitControlPath(path) {
				return effects, fmt.Errorf("unsafe legacy entry %q", item.Path)
			}
			target, err := fsops.SafeTarget(root, path)
			if err != nil || filepath.Clean(target) != filepath.Clean(item.Target) {
				return effects, fmt.Errorf("legacy target mapping changed for %s", path)
			}
			if !fsops.LinkPointsTo(target, item.LegacySource) {
				return effects, fmt.Errorf("%w: %s no longer points to its legacy source", ErrConflict, path)
			}
			kind, err := fsops.Kind(item.LegacySource)
			if err != nil || kind != item.Kind {
				return effects, fmt.Errorf("legacy source kind changed for %s", path)
			}
			sourceRel := e.sourceRel(sourceID, path)
			newSource, err := fsops.SafeTarget(e.store.Root(), sourceRel)
			if err != nil {
				return effects, err
			}
			if err := os.MkdirAll(filepath.Dir(newSource), 0o700); err != nil {
				return effects, err
			}
			if _, err := os.Lstat(newSource); err == nil {
				if err := fsops.SyncTreeModes(item.LegacySource, newSource); err != nil {
					return effects, fmt.Errorf("synchronize shared source mode %s: %w", path, err)
				}
				equal, compareErr := fsops.EqualTree(item.LegacySource, newSource)
				if compareErr != nil || !equal {
					return effects, fmt.Errorf("shared legacy source %s differs from the existing Go source", path)
				}
			} else if errors.Is(err, os.ErrNotExist) {
				if err := fsops.CopyTree(item.LegacySource, newSource); err != nil {
					return effects, fmt.Errorf("copy legacy source %s: %w", path, err)
				}
			} else {
				return effects, err
			}
			backup := filepath.Join(filepath.Dir(target), ".ewasd-migrate-"+operationID+"-"+randomID(4)+".link")
			journal := domain.Journal{
				ID: randomID(12), Action: "legacy-import", Phase: "prepared",
				ProjectID: id, SourceID: sourceID, ProjectRoot: root, Path: path, Source: newSource,
				Target: target, Backup: backup, LegacySource: item.LegacySource,
				CreatedAt: e.now(), UpdatedAt: e.now(),
			}
			if err := e.writeJournal(journal); err != nil {
				return effects, err
			}
			journals = append(journals, journal)
			entries = append(entries, domain.Entry{Path: path, Kind: kind, SourceRel: sourceRel, CreatedAt: e.now()})
			changed = append(changed, path)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
		now := e.now()
		project := domain.Project{ID: id, SourceID: sourceID, Name: plan.Name, Root: root, GitRoot: checkout.GitRoot, Remote: checkout.Remote, LegacySourceRoot: plan.SourceRoot, CreatedAt: now, UpdatedAt: now, Entries: entries}
		state.Projects = append(state.Projects, project)
		state.AddEvent(domain.Event{ID: randomID(8), ProjectID: id, Action: "legacy-import", Summary: fmt.Sprintf("Imported %d legacy-managed entries", len(entries)), CreatedAt: now})
		effects.Commit = func() error {
			var errs []error
			for _, journal := range journals {
				if err := e.completeLegacySwitch(journal); err != nil {
					errs = append(errs, err)
					break
				}
			}
			if len(errs) == 0 {
				if err := gitutil.UpdateExclude(project.ID, project.Root, project.GitRoot, entryPaths(project.Entries)); err != nil {
					errs = append(errs, err)
				}
			}
			if len(errs) == 0 {
				for _, journal := range journals {
					if err := fsops.RemoveAny(journal.Backup); err != nil {
						errs = append(errs, err)
					}
					if err := e.removeJournal(journal.ID); err != nil {
						errs = append(errs, err)
					}
				}
			}
			return errors.Join(errs...)
		}
		return effects, nil
	})
	if err != nil {
		return domain.ApplyResult{}, err
	}
	outcome := "completed"
	if len(changed) == 0 {
		outcome = "no_change"
	}
	return domain.ApplyResult{OK: true, Outcome: outcome, OperationID: operationID, Revision: state.Revision, Action: "legacy-import", Changed: changed, Warnings: warnings, Summary: fmt.Sprintf("Imported %d legacy entry(s) for %s", len(changed), plan.Name)}, nil
}

func uniqueProjectID(state *domain.State, name string) (string, error) {
	for range 10 {
		id := slug(name) + "-" + randomID(4)
		if state.ProjectByID(id) == nil {
			return id, nil
		}
	}
	return "", errors.New("could not allocate a unique project id")
}

func (e *Engine) completeLegacySwitch(journal domain.Journal) error {
	if fsops.LinkPointsTo(journal.Target, journal.Source) {
		return nil
	}
	if fsops.LinkPointsTo(journal.Target, journal.LegacySource) {
		if _, err := os.Lstat(journal.Backup); err == nil {
			return errors.New("legacy backup already exists before switch")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(journal.Target, journal.Backup); err != nil {
			return err
		}
		if err := fsops.SyncDir(filepath.Dir(journal.Target)); err != nil {
			return err
		}
		journal.Phase = "legacy-backed-up"
		journal.UpdatedAt = e.now()
		if err := e.writeJournal(journal); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(journal.Target); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("legacy target is occupied by unexpected content")
		}
		return err
	}
	if _, err := os.Lstat(journal.Backup); err != nil {
		return errors.New("legacy link backup is missing")
	}
	if err := fsops.AtomicSymlink(journal.Source, journal.Target, journal.ID); err != nil {
		return err
	}
	journal.Phase = "switched"
	journal.UpdatedAt = e.now()
	return e.writeJournal(journal)
}

func (e *Engine) rollbackLegacySwitch(journal domain.Journal) error {
	if fsops.LinkPointsTo(journal.Target, journal.Source) {
		if err := os.Remove(journal.Target); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(journal.Target); errors.Is(err, os.ErrNotExist) {
		if _, backupErr := os.Lstat(journal.Backup); backupErr == nil {
			if err := os.Rename(journal.Backup, journal.Target); err != nil {
				return err
			}
		} else if errors.Is(backupErr, os.ErrNotExist) && journal.Phase == "prepared" {
			// The old legacy link was never moved.
		} else if backupErr != nil {
			return backupErr
		}
	} else if err != nil {
		return err
	} else if !fsops.LinkPointsTo(journal.Target, journal.LegacySource) {
		return errors.New("legacy target changed externally; journal retained")
	}
	return e.removeJournal(journal.ID)
}

func (e *Engine) Unregister(projectID string, expected uint64) (domain.ApplyResult, error) {
	operationID := randomID(12)
	state, warnings, err := e.store.Transact(&expected, func(state *domain.State) (store.Effects, error) {
		if err := e.requireNoRecovery(); err != nil {
			return store.Effects{}, err
		}
		index := -1
		for i := range state.Projects {
			if state.Projects[i].ID == projectID {
				index = i
				break
			}
		}
		if index < 0 {
			return store.Effects{}, ErrNotFound
		}
		project := state.Projects[index]
		if len(project.Entries) != 0 {
			return store.Effects{}, fmt.Errorf("%w: detach all managed entries before unregistering", ErrConflict)
		}
		sourceInUse := false
		for i := range state.Projects {
			if i != index && state.Projects[i].SourceID == project.SourceID {
				sourceInUse = true
				break
			}
		}
		filesDir := filepath.Join(e.store.Root(), "profiles", project.SourceID, "files")
		if !sourceInUse {
			children, err := os.ReadDir(filesDir)
			if err != nil {
				return store.Effects{}, err
			}
			if len(children) != 0 {
				return store.Effects{}, fmt.Errorf("%w: profile contains unowned source files; inspect them before unregistering", ErrConflict)
			}
		}
		state.Projects = append(state.Projects[:index], state.Projects[index+1:]...)
		state.AddEvent(domain.Event{ID: randomID(8), ProjectID: project.ID, Action: "unregister", Summary: "Unregistered empty checkout without deleting content", CreatedAt: e.now()})
		effects := store.Effects{Commit: func() error {
			var errs []error
			if err := gitutil.UpdateExclude(project.ID, project.Root, project.GitRoot, nil); err != nil {
				errs = append(errs, err)
			}
			if !sourceInUse {
				if err := os.Remove(filesDir); err != nil && !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, err)
				}
				profileRoot := filepath.Dir(filesDir)
				if err := os.Remove(profileRoot); err != nil && !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		}}
		return effects, nil
	})
	if err != nil {
		return domain.ApplyResult{}, err
	}
	return domain.ApplyResult{OK: true, Outcome: "completed", OperationID: operationID, Revision: state.Revision, Action: "unregister", Changed: []string{projectID}, Warnings: warnings, Summary: "Unregistered empty checkout"}, nil
}

func pathsOverlap(left, right string) bool {
	contains := func(parent, child string) bool {
		relative, err := filepath.Rel(parent, child)
		return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
	}
	return contains(left, right) || contains(right, left)
}

func (e *Engine) Snapshot() (domain.Snapshot, error) {
	state, err := e.store.Read()
	if err != nil {
		return domain.Snapshot{}, err
	}
	views := make([]domain.ProjectView, 0, len(state.Projects))
	for _, project := range state.Projects {
		entries := make([]domain.EntryView, 0, len(project.Entries))
		health := domain.Health{Total: len(project.Entries)}
		for _, entry := range project.Entries {
			view := e.inspect(project, entry)
			entries = append(entries, view)
			switch view.Status {
			case "linked":
				health.Linked++
			case "missing":
				health.Missing++
			case "source-missing":
				health.SourceMissing++
			default:
				health.Conflicts++
			}
		}
		ignoreOK, ignoreState := gitutil.CheckExclude(project.ID, project.Root, project.GitRoot, entryPaths(project.Entries))
		views = append(views, domain.ProjectView{Project: project, EntriesView: entries, Health: health, GitIgnoreOK: ignoreOK, GitIgnoreState: ignoreState})
	}
	journals, err := e.listJournals()
	if err != nil {
		return domain.Snapshot{}, fmt.Errorf("inspect recovery journals: %w", err)
	}
	return domain.Snapshot{
		SchemaVersion: state.SchemaVersion,
		Revision:      state.Revision,
		DataRoot:      e.store.Root(),
		Projects:      views,
		Activity:      state.Activity,
		Recovery:      journals,
	}, nil
}

func (e *Engine) PlanAdopt(projectSelector, rawPath string) (plan domain.Plan, err error) {
	defer func() {
		if err == nil {
			sealPlan(&plan)
		}
	}()
	state, project, err := e.readProject(projectSelector)
	if err != nil {
		return domain.Plan{}, err
	}
	path, err := fsops.ValidateRelative(rawPath)
	if err != nil {
		return domain.Plan{}, err
	}
	if containsGitControlPath(path) {
		return domain.Plan{}, errors.New("paths inside .git are never manageable")
	}
	plan = e.basePlan("adopt", state.Revision, *project)
	if e.blockPlanForRecovery(&plan) {
		return plan, nil
	}
	target, err := fsops.SafeTarget(project.Root, path)
	if err != nil {
		plan.Conflicts = append(plan.Conflicts, conflict(path, "unsafe-path", err.Error()))
		plan.Summary = "Adoption blocked by an unsafe path"
		return plan, nil
	}
	source, err := e.sourcePath(*project, path)
	if err != nil {
		plan.Conflicts = append(plan.Conflicts, conflict(path, "unsafe-source", err.Error()))
		plan.Summary = "Adoption blocked by an unsafe central source path"
		return plan, nil
	}
	if existing := project.EntryByPath(path); existing != nil {
		view := e.inspect(*project, *existing)
		if view.Status == "linked" {
			plan.Safe = true
			plan.Summary = path + " is already managed and healthy; no change is needed"
			return plan, nil
		}
		plan.Conflicts = append(plan.Conflicts, conflict(path, "already-managed", view.Detail))
		plan.Summary = "Adoption blocked because the manifest already owns this path"
		return plan, nil
	}
	if _, err := os.Lstat(source); err == nil {
		plan.Conflicts = append(plan.Conflicts, conflict(path, "orphan-source", "central source exists without a manifest entry; recover or move it first"))
		plan.Summary = "Adoption blocked by unowned central content"
		return plan, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.Plan{}, err
	}
	kind, err := fsops.Kind(target)
	if errors.Is(err, os.ErrNotExist) {
		plan.Conflicts = append(plan.Conflicts, conflict(path, "missing-target", "there is no local file or directory to adopt"))
		plan.Summary = "Adoption needs an existing local path"
		return plan, nil
	}
	if err != nil {
		return domain.Plan{}, err
	}
	if kind != "file" && kind != "directory" {
		plan.Conflicts = append(plan.Conflicts, conflict(path, "unsupported-target", "foreign symlinks and special files are never adopted"))
		plan.Summary = "Adoption blocked by an unsupported target"
		return plan, nil
	}
	if err := fsops.ValidateCopyable(target); err != nil {
		plan.Conflicts = append(plan.Conflicts, conflict(path, "unsupported-content", err.Error()))
		plan.Summary = "Adoption blocked by unsupported nested content"
		return plan, nil
	}
	plan.Safe = true
	plan.Summary = "Copy " + path + " to durable storage, then atomically replace it with an owned link"
	plan.Steps = []domain.PlanStep{
		{Path: path, Action: "copy", From: target, To: source, Detail: "Copy and sync source content before changing the checkout"},
		{Path: path, Action: "link", From: target, To: source, Detail: "Keep a sibling backup while atomically installing the link"},
		{Path: path, Action: "manifest", Detail: "Record exact ownership and update the private Git exclude block"},
	}
	return plan, nil
}

func (e *Engine) Adopt(projectID, rawPath string, expected uint64, approvedFingerprint string) (domain.ApplyResult, error) {
	path, err := fsops.ValidateRelative(rawPath)
	if err != nil {
		return domain.ApplyResult{}, err
	}
	if containsGitControlPath(path) {
		return domain.ApplyResult{}, errors.New("paths inside .git are never manageable")
	}
	approvedPlan, err := e.PlanAdopt(projectID, path)
	if err != nil {
		return domain.ApplyResult{}, err
	}
	if err := verifyApprovedPlan(approvedPlan, expected, approvedFingerprint); err != nil {
		return domain.ApplyResult{}, err
	}
	operationID := randomID(12)
	changed := []string{}
	state, warnings, err := e.store.Transact(&expected, func(state *domain.State) (store.Effects, error) {
		if err := e.requireNoRecovery(); err != nil {
			return store.Effects{}, err
		}
		project := state.ProjectByID(projectID)
		if project == nil {
			return store.Effects{}, ErrNotFound
		}
		if project.EntryByPath(path) != nil {
			return store.Effects{}, fmt.Errorf("%w: path is already in the manifest", ErrConflict)
		}
		target, err := fsops.SafeTarget(project.Root, path)
		if err != nil {
			return store.Effects{}, err
		}
		source, err := e.sourcePath(*project, path)
		if err != nil {
			return store.Effects{}, err
		}
		if _, err := os.Lstat(source); err == nil {
			return store.Effects{}, fmt.Errorf("%w: unowned central source already exists", ErrConflict)
		} else if !errors.Is(err, os.ErrNotExist) {
			return store.Effects{}, err
		}
		kind, err := fsops.Kind(target)
		if err != nil || (kind != "file" && kind != "directory") {
			return store.Effects{}, fmt.Errorf("%w: target must still be a regular file or directory", ErrConflict)
		}
		if err := fsops.ValidateCopyable(target); err != nil {
			return store.Effects{}, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
			return store.Effects{}, err
		}
		stage := filepath.Join(e.store.Root(), "transactions", operationID+".stage")
		backup := filepath.Join(filepath.Dir(target), ".ewasd-"+operationID+".backup")
		journal := domain.Journal{
			ID: operationID, Action: "adopt", Phase: "copying", ProjectID: project.ID,
			Path: path, Source: source, Target: target, Stage: stage, Backup: backup,
			ExpectedRev: expected, CreatedAt: e.now(), UpdatedAt: e.now(),
		}
		preEntries := entryPaths(project.Entries)
		projectIdentity := *project
		effects := store.Effects{
			Rollback: func() error {
				if err := e.rollbackAdopt(journal, false); err != nil {
					return err
				}
				return gitutil.UpdateExclude(projectIdentity.ID, projectIdentity.Root, projectIdentity.GitRoot, preEntries)
			},
		}
		if err := e.writeJournal(journal); err != nil {
			return effects, err
		}
		if err := fsops.CopyTree(target, stage); err != nil {
			return effects, err
		}
		if err := os.Rename(stage, source); err != nil {
			return effects, err
		}
		if err := fsops.SyncDir(filepath.Dir(source)); err != nil {
			return effects, err
		}
		journal.Phase = "source-durable"
		journal.UpdatedAt = e.now()
		if err := e.writeJournal(journal); err != nil {
			return effects, err
		}
		backupRel, err := filepath.Rel(project.Root, backup)
		if err != nil || backupRel == ".." || strings.HasPrefix(backupRel, ".."+string(filepath.Separator)) {
			return effects, errors.New("backup path escaped the project root")
		}
		temporaryExcludes := append(append([]string{}, preEntries...), filepath.ToSlash(backupRel))
		if err := gitutil.UpdateExclude(project.ID, project.Root, project.GitRoot, temporaryExcludes); err != nil {
			return effects, err
		}
		if err := os.Rename(target, backup); err != nil {
			return effects, err
		}
		if err := fsops.SyncDir(filepath.Dir(target)); err != nil {
			return effects, err
		}
		journal.Phase = "target-backed-up"
		journal.UpdatedAt = e.now()
		if err := e.writeJournal(journal); err != nil {
			return effects, err
		}
		if equal, err := fsops.EqualTree(source, backup); err != nil {
			return effects, err
		} else if !equal {
			return effects, fmt.Errorf("%w: target content changed after preview; original replacement was retained", store.ErrStaleRevision)
		}
		if err := fsops.AtomicSymlink(source, target, operationID); err != nil {
			return effects, err
		}
		journal.Phase = "linked"
		journal.UpdatedAt = e.now()
		if err := e.writeJournal(journal); err != nil {
			return effects, err
		}
		now := e.now()
		project.Entries = append(project.Entries, domain.Entry{
			Path: path, Kind: kind, SourceRel: e.sourceRel(project.SourceID, path), CreatedAt: now,
		})
		sort.Slice(project.Entries, func(i, j int) bool { return project.Entries[i].Path < project.Entries[j].Path })
		project.UpdatedAt = now
		state.AddEvent(domain.Event{ID: randomID(8), ProjectID: project.ID, Action: "adopt", Path: path, Summary: "Adopted local content with a rollback backup", CreatedAt: now})
		entries := entryPaths(project.Entries)
		projectCopy := *project
		effects.Commit = func() error {
			var errs []error
			if err := fsops.RemoveAny(backup); err != nil {
				errs = append(errs, err)
			}
			if err := gitutil.UpdateExclude(projectCopy.ID, projectCopy.Root, projectCopy.GitRoot, entries); err != nil {
				errs = append(errs, err)
			}
			if err := e.removeJournal(operationID); err != nil {
				errs = append(errs, err)
			}
			return errors.Join(errs...)
		}
		changed = append(changed, path)
		return effects, nil
	})
	if err != nil {
		return domain.ApplyResult{}, err
	}
	return domain.ApplyResult{OK: true, Outcome: "completed", OperationID: operationID, Revision: state.Revision, Action: "adopt", Changed: changed, Warnings: warnings, Summary: "Adopted " + path}, nil
}

func (e *Engine) PlanDetach(projectSelector, rawPath string) (plan domain.Plan, err error) {
	defer func() {
		if err == nil {
			sealPlan(&plan)
		}
	}()
	state, project, err := e.readProject(projectSelector)
	if err != nil {
		return domain.Plan{}, err
	}
	path, err := fsops.ValidateRelative(rawPath)
	if err != nil {
		return domain.Plan{}, err
	}
	plan = e.basePlan("detach", state.Revision, *project)
	if e.blockPlanForRecovery(&plan) {
		return plan, nil
	}
	entry := project.EntryByPath(path)
	if entry == nil {
		plan.Conflicts = append(plan.Conflicts, conflict(path, "not-managed", "the manifest does not own this path"))
		plan.Summary = "Detach is only available for explicitly managed entries"
		return plan, nil
	}
	if sourceReferenceCount(state, entry.SourceRel) > 1 {
		plan.Conflicts = append(plan.Conflicts, conflict(path, "shared-source", "source is shared by multiple migrated checkouts; detach is blocked to preserve cross-worktree sharing"))
		plan.Summary = "Detach blocked because this legacy source is intentionally shared"
		return plan, nil
	}
	view := e.inspect(*project, *entry)
	if view.Status != "linked" {
		plan.Conflicts = append(plan.Conflicts, conflict(path, view.Status, view.Detail))
		plan.Summary = "Detach blocked until the owned link is healthy"
		return plan, nil
	}
	archive := filepath.Join(e.store.Root(), "archive", "<operation-id>", project.ID, filepath.FromSlash(path))
	plan.Safe = true
	plan.Summary = "Materialize a normal local copy and archive the central source"
	plan.Steps = []domain.PlanStep{
		{Path: path, Action: "materialize", From: view.Source, To: view.Target, Detail: "Copy and sync content beside the link"},
		{Path: path, Action: "archive", From: view.Source, To: archive, Detail: "Move source content to the retained archive; do not delete it"},
		{Path: path, Action: "manifest", Detail: "Remove ownership and the corresponding private Git exclude entry"},
	}
	return plan, nil
}

func (e *Engine) Detach(projectID, rawPath string, expected uint64, approvedFingerprint string) (domain.ApplyResult, error) {
	path, err := fsops.ValidateRelative(rawPath)
	if err != nil {
		return domain.ApplyResult{}, err
	}
	approvedPlan, err := e.PlanDetach(projectID, path)
	if err != nil {
		return domain.ApplyResult{}, err
	}
	if err := verifyApprovedPlan(approvedPlan, expected, approvedFingerprint); err != nil {
		return domain.ApplyResult{}, err
	}
	operationID := randomID(12)
	state, warnings, err := e.store.Transact(&expected, func(state *domain.State) (store.Effects, error) {
		if err := e.requireNoRecovery(); err != nil {
			return store.Effects{}, err
		}
		project := state.ProjectByID(projectID)
		if project == nil {
			return store.Effects{}, ErrNotFound
		}
		entryIndex := -1
		for i := range project.Entries {
			if project.Entries[i].Path == path {
				entryIndex = i
				break
			}
		}
		if entryIndex < 0 {
			return store.Effects{}, ErrNotFound
		}
		entry := project.Entries[entryIndex]
		if sourceReferenceCount(*state, entry.SourceRel) > 1 {
			return store.Effects{}, fmt.Errorf("%w: source is shared by multiple checkouts", ErrConflict)
		}
		target, err := fsops.SafeTarget(project.Root, path)
		if err != nil {
			return store.Effects{}, err
		}
		source, err := fsops.SafeTarget(e.store.Root(), entry.SourceRel)
		if err != nil {
			return store.Effects{}, fmt.Errorf("unsafe recorded source: %w", err)
		}
		if !fsops.LinkPointsTo(target, source) {
			return store.Effects{}, fmt.Errorf("%w: target is not the healthy owned link", ErrConflict)
		}
		if _, err := os.Lstat(source); err != nil {
			return store.Effects{}, fmt.Errorf("%w: source is unavailable: %v", ErrConflict, err)
		}
		materialized := filepath.Join(filepath.Dir(target), ".ewasd-"+operationID+".materialize")
		archive := filepath.Join(e.store.Root(), "archive", operationID, project.ID, filepath.FromSlash(path))
		journal := domain.Journal{
			ID: operationID, Action: "detach", Phase: "materializing", ProjectID: project.ID,
			Path: path, Source: source, Target: target, Stage: materialized, Archive: archive,
			ExpectedRev: expected, CreatedAt: e.now(), UpdatedAt: e.now(),
		}
		effects := store.Effects{Rollback: func() error { return e.rollbackDetach(journal) }}
		if err := e.writeJournal(journal); err != nil {
			return effects, err
		}
		if err := fsops.CopyTree(source, materialized); err != nil {
			return effects, err
		}
		if err := os.MkdirAll(filepath.Dir(archive), 0o700); err != nil {
			_ = fsops.RemoveAny(materialized)
			return store.Effects{}, err
		}
		journal.Phase = "materialized"
		journal.UpdatedAt = e.now()
		if err := e.writeJournal(journal); err != nil {
			return effects, err
		}
		if err := os.Rename(source, archive); err != nil {
			_ = fsops.RemoveAny(materialized)
			return effects, err
		}
		if err := errors.Join(fsops.SyncDir(filepath.Dir(source)), fsops.SyncDir(filepath.Dir(archive))); err != nil {
			return effects, err
		}
		journal.Phase = "source-archived"
		journal.UpdatedAt = e.now()
		if err := e.writeJournal(journal); err != nil {
			return effects, err
		}
		if err := os.Rename(materialized, target); err != nil {
			return effects, err
		}
		if err := fsops.SyncDir(filepath.Dir(target)); err != nil {
			return effects, err
		}
		journal.Phase = "detached"
		journal.UpdatedAt = e.now()
		if err := e.writeJournal(journal); err != nil {
			return effects, err
		}
		project.Entries = append(project.Entries[:entryIndex], project.Entries[entryIndex+1:]...)
		project.UpdatedAt = e.now()
		state.AddEvent(domain.Event{ID: randomID(8), ProjectID: project.ID, Action: "detach", Path: path, Summary: "Materialized a local copy and retained the source in archive", CreatedAt: e.now()})
		entries := entryPaths(project.Entries)
		projectCopy := *project
		effects.Commit = func() error {
			return errors.Join(
				gitutil.UpdateExclude(projectCopy.ID, projectCopy.Root, projectCopy.GitRoot, entries),
				e.removeJournal(operationID),
			)
		}
		return effects, nil
	})
	if err != nil {
		return domain.ApplyResult{}, err
	}
	return domain.ApplyResult{OK: true, Outcome: "completed", OperationID: operationID, Revision: state.Revision, Action: "detach", Changed: []string{path}, Warnings: warnings, Summary: "Detached " + path + "; source retained in archive"}, nil
}

func (e *Engine) PlanReconcile(projectSelector string) (plan domain.Plan, err error) {
	defer func() {
		if err == nil {
			sealPlan(&plan)
		}
	}()
	state, project, err := e.readProject(projectSelector)
	if err != nil {
		return domain.Plan{}, err
	}
	plan = e.basePlan("reconcile", state.Revision, *project)
	if e.blockPlanForRecovery(&plan) {
		return plan, nil
	}
	missingLinks := 0
	for _, entry := range project.Entries {
		view := e.inspect(*project, entry)
		switch view.Status {
		case "linked":
			continue
		case "missing":
			missingLinks++
			plan.Steps = append(plan.Steps, domain.PlanStep{Path: entry.Path, Action: "link", From: view.Source, To: view.Target, Detail: "Restore the missing owned link without replacing another path"})
		default:
			plan.Conflicts = append(plan.Conflicts, conflict(entry.Path, view.Status, view.Detail))
		}
	}
	ignoreOK, ignoreState := gitutil.CheckExclude(project.ID, project.Root, project.GitRoot, entryPaths(project.Entries))
	if !ignoreOK {
		plan.Steps = append(plan.Steps, domain.PlanStep{Path: ".git/info/exclude", Action: "git-ignore", Detail: ignoreState + "; replace only this project's marked block"})
	}
	plan.Safe = true
	if len(plan.Steps) == 0 {
		plan.Summary = "No missing links can be restored; conflicts, if any, will remain untouched"
	} else {
		ignoreRepairs := len(plan.Steps) - missingLinks
		plan.Summary = fmt.Sprintf("Restore %d missing link(s), repair %d Git ignore block(s), and leave %d conflict(s) unchanged", missingLinks, ignoreRepairs, len(plan.Conflicts))
	}
	return plan, nil
}

func (e *Engine) Reconcile(projectID string, expected uint64, approvedFingerprint string) (domain.ApplyResult, error) {
	operationID := randomID(12)
	plan, err := e.PlanReconcile(projectID)
	if err != nil {
		return domain.ApplyResult{}, err
	}
	if plan.ExpectedRevision != expected || approvedFingerprint == "" || plan.Fingerprint != approvedFingerprint {
		return domain.ApplyResult{}, fmt.Errorf("%w: reconcile steps or filesystem preconditions changed after review", store.ErrStaleRevision)
	}
	if len(plan.Steps) == 0 {
		return domain.ApplyResult{OK: true, Outcome: "no_change", OperationID: operationID, Revision: expected, Action: "reconcile", Skipped: conflictPaths(plan.Conflicts), Summary: plan.Summary}, nil
	}
	changed := []string{}
	skipped := conflictPaths(plan.Conflicts)
	state, warnings, err := e.store.Transact(&expected, func(state *domain.State) (store.Effects, error) {
		if err := e.requireNoRecovery(); err != nil {
			return store.Effects{}, err
		}
		project := state.ProjectByID(projectID)
		if project == nil {
			return store.Effects{}, ErrNotFound
		}
		created := []domain.EntryView{}
		journals := []string{}
		needsIgnoreRepair := false
		effects := store.Effects{Rollback: func() error {
			var errs []error
			for _, view := range created {
				if !fsops.LinkPointsTo(view.Target, view.Source) {
					errs = append(errs, fmt.Errorf("created target %s changed during rollback; it was retained", view.Target))
					continue
				}
				if err := fsops.RemoveAny(view.Target); err != nil {
					errs = append(errs, err)
				}
			}
			if len(errs) == 0 {
				for _, id := range journals {
					if err := e.removeJournal(id); err != nil {
						errs = append(errs, err)
					}
				}
			}
			return errors.Join(errs...)
		}}
		for _, step := range plan.Steps {
			if step.Action == "git-ignore" {
				ignoreOK, _ := gitutil.CheckExclude(project.ID, project.Root, project.GitRoot, entryPaths(project.Entries))
				if ignoreOK {
					return effects, fmt.Errorf("%w: Git ignore state changed after review", store.ErrStaleRevision)
				}
				needsIgnoreRepair = true
				continue
			}
			if step.Action != "link" {
				return effects, errors.New("approved reconcile plan contains an unknown step")
			}
			entry := project.EntryByPath(step.Path)
			if entry == nil {
				return effects, fmt.Errorf("%w: manifest entry changed after review", store.ErrStaleRevision)
			}
			view := e.inspect(*project, *entry)
			if view.Status != "missing" {
				return effects, fmt.Errorf("%w: %s is now %s", store.ErrStaleRevision, entry.Path, view.Status)
			}
			if err := os.MkdirAll(filepath.Dir(view.Target), 0o755); err != nil {
				return effects, err
			}
			id := randomID(12)
			journal := domain.Journal{ID: id, Action: "reconcile", Phase: "prepared", ProjectID: project.ID, Path: entry.Path, Source: view.Source, Target: view.Target, ExpectedRev: expected, CreatedAt: e.now(), UpdatedAt: e.now()}
			if err := e.writeJournal(journal); err != nil {
				return effects, err
			}
			journals = append(journals, id)
			if err := fsops.AtomicSymlink(view.Source, view.Target, id); err != nil {
				return effects, err
			}
			created = append(created, view)
			changed = append(changed, entry.Path)
		}
		if len(changed) == 0 && !needsIgnoreRepair {
			return effects, fmt.Errorf("%w: filesystem changed after the plan; create a fresh plan", store.ErrStaleRevision)
		}
		if needsIgnoreRepair {
			changed = append(changed, ".git/info/exclude")
		}
		project.UpdatedAt = e.now()
		state.AddEvent(domain.Event{ID: randomID(8), ProjectID: project.ID, Action: "reconcile", Summary: fmt.Sprintf("Reconciled %d approved item(s)", len(changed)), CreatedAt: e.now()})
		entries := entryPaths(project.Entries)
		projectCopy := *project
		effects.Commit = func() error {
			var errs []error
			if err := gitutil.UpdateExclude(projectCopy.ID, projectCopy.Root, projectCopy.GitRoot, entries); err != nil {
				errs = append(errs, err)
			}
			for _, id := range journals {
				if err := e.removeJournal(id); err != nil {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		}
		return effects, nil
	})
	if err != nil {
		return domain.ApplyResult{}, err
	}
	return domain.ApplyResult{OK: true, Outcome: "completed", OperationID: operationID, Revision: state.Revision, Action: "reconcile", Changed: changed, Skipped: skipped, Warnings: warnings, Summary: fmt.Sprintf("Reconciled %d item(s)", len(changed))}, nil
}

func (e *Engine) inspect(project domain.Project, entry domain.Entry) domain.EntryView {
	source, sourcePathErr := fsops.SafeTarget(e.store.Root(), entry.SourceRel)
	if sourcePathErr != nil {
		return domain.EntryView{Entry: entry, Source: filepath.Join(e.store.Root(), filepath.FromSlash(entry.SourceRel)), Target: filepath.Join(project.Root, filepath.FromSlash(entry.Path)), Status: "source-invalid", Detail: sourcePathErr.Error()}
	}
	target, err := fsops.SafeTarget(project.Root, entry.Path)
	if err != nil {
		return domain.EntryView{Entry: entry, Source: source, Target: filepath.Join(project.Root, filepath.FromSlash(entry.Path)), Status: "conflict", Detail: err.Error()}
	}
	sourceInfo, err := os.Lstat(source)
	if err != nil {
		return domain.EntryView{Entry: entry, Source: source, Target: target, Status: "source-missing", Detail: "manifest source is missing; no automatic write is safe"}
	}
	actualKind := "special"
	if sourceInfo.Mode().IsRegular() {
		actualKind = "file"
	} else if sourceInfo.IsDir() && sourceInfo.Mode()&os.ModeSymlink == 0 {
		actualKind = "directory"
	} else if sourceInfo.Mode()&os.ModeSymlink != 0 {
		actualKind = "symlink"
	}
	if actualKind != entry.Kind {
		return domain.EntryView{Entry: entry, Source: source, Target: target, Status: "source-invalid", Detail: fmt.Sprintf("recorded %s source is now %s; no automatic write is safe", entry.Kind, actualKind)}
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return domain.EntryView{Entry: entry, Source: source, Target: target, Status: "missing", Detail: "owned destination is absent and can be reconciled"}
	}
	if err != nil {
		return domain.EntryView{Entry: entry, Source: source, Target: target, Status: "conflict", Detail: err.Error()}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if fsops.LinkPointsTo(target, source) {
			return domain.EntryView{Entry: entry, Source: source, Target: target, Status: "linked", Detail: "destination points to the recorded source"}
		}
		return domain.EntryView{Entry: entry, Source: source, Target: target, Status: "conflict", Detail: "destination is a foreign or mispointed symlink; it will not be replaced"}
	}
	return domain.EntryView{Entry: entry, Source: source, Target: target, Status: "conflict", Detail: "a normal file or directory occupies the owned destination; it will not be replaced"}
}

func (e *Engine) readProject(selector string) (domain.State, *domain.Project, error) {
	state, err := e.store.Read()
	if err != nil {
		return state, nil, err
	}
	project := state.ProjectByID(selector)
	if project == nil && selector != "" {
		root, resolveErr := filepath.Abs(selector)
		if resolveErr == nil {
			if resolved, evalErr := filepath.EvalSymlinks(root); evalErr == nil {
				root = resolved
			}
			project = state.ProjectByRoot(root)
		}
	}
	if project == nil {
		return state, nil, ErrNotFound
	}
	return state, project, nil
}

func (e *Engine) sourceRel(projectID, path string) string {
	return filepath.ToSlash(filepath.Join("profiles", projectID, "files", filepath.FromSlash(path)))
}

func (e *Engine) sourcePath(project domain.Project, path string) (string, error) {
	return fsops.SafeTarget(e.store.Root(), e.sourceRel(project.SourceID, path))
}

func (e *Engine) basePlan(action string, revision uint64, project domain.Project) domain.Plan {
	return domain.Plan{
		ID: randomID(12), Action: action, ProjectID: project.ID, ProjectName: project.Name,
		ExpectedRevision: revision, Safe: false, Steps: []domain.PlanStep{}, Conflicts: []domain.Conflict{},
		Guarantees: []string{
			"state revision is rechecked under a cross-process lock",
			"normal files, directories, and foreign links are never overwritten",
			"a durable recovery journal precedes destructive renames",
			"source content is never deleted by this operation",
		},
		CreatedAt: e.now(),
	}
}

func sealPlan(plan *domain.Plan) {
	payload := struct {
		Action            string
		ProjectID         string
		TemplateProjectID string
		TargetRoot        string
		GitRoot           string
		Remote            string
		DetectionMethod   string
		NewProject        bool
		ExpectedRevision  uint64
		Safe              bool
		Steps             []domain.PlanStep
		Conflicts         []domain.Conflict
	}{plan.Action, plan.ProjectID, plan.TemplateProjectID, plan.TargetRoot, plan.GitRoot, plan.Remote, plan.DetectionMethod, plan.NewProject, plan.ExpectedRevision, plan.Safe, plan.Steps, plan.Conflicts}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic("seal plan: " + err.Error())
	}
	digest := sha256.Sum256(encoded)
	plan.Fingerprint = hex.EncodeToString(digest[:])
}

func verifyApprovedPlan(plan domain.Plan, expected uint64, fingerprint string) error {
	if plan.ExpectedRevision != expected {
		return fmt.Errorf("%w: planned revision %d, supplied revision %d", store.ErrStaleRevision, plan.ExpectedRevision, expected)
	}
	if fingerprint == "" || plan.Fingerprint != fingerprint {
		return fmt.Errorf("%w: plan steps or filesystem preconditions changed after review", store.ErrStaleRevision)
	}
	if !plan.Safe || len(plan.Steps) == 0 {
		return fmt.Errorf("%w: plan is blocked or has no approved changes", ErrConflict)
	}
	return nil
}

func (e *Engine) requireNoRecovery() error {
	journals, err := e.listJournals()
	if err != nil {
		return err
	}
	if len(journals) > 0 {
		return fmt.Errorf("%w: %d journal(s) pending", ErrRecoveryPending, len(journals))
	}
	return nil
}

func (e *Engine) blockPlanForRecovery(plan *domain.Plan) bool {
	journals, err := e.listJournals()
	if err != nil {
		plan.Conflicts = append(plan.Conflicts, conflict("", "recovery-check-failed", err.Error()))
		plan.Summary = "Cannot verify transaction recovery state"
		return true
	}
	if len(journals) == 0 {
		return false
	}
	plan.Safe = false
	plan.Conflicts = append(plan.Conflicts, conflict("", "recovery-required", fmt.Sprintf("%d interrupted transaction journal(s) must be recovered first", len(journals))))
	plan.Summary = "New writes are blocked until interrupted work is recovered"
	return true
}

func (e *Engine) rollbackAdopt(journal domain.Journal, retainSource bool) error {
	var errs []error
	if err := fsops.RemoveAny(journal.Stage); err != nil {
		errs = append(errs, err)
	}
	ours := fsops.LinkPointsTo(journal.Target, journal.Source)
	if ours {
		if err := fsops.RemoveAny(journal.Target); err != nil {
			errs = append(errs, err)
		}
	}
	_, backupErr := os.Lstat(journal.Backup)
	if backupErr == nil {
		if _, targetErr := os.Lstat(journal.Target); errors.Is(targetErr, os.ErrNotExist) {
			if err := os.Rename(journal.Backup, journal.Target); err != nil {
				errs = append(errs, err)
			}
		} else if targetErr == nil {
			errs = append(errs, errors.New("target was replaced externally; original backup and journal were retained"))
		} else {
			errs = append(errs, targetErr)
		}
	} else if !errors.Is(backupErr, os.ErrNotExist) {
		errs = append(errs, backupErr)
	} else if journal.Phase == "target-backed-up" || journal.Phase == "linked" {
		errs = append(errs, errors.New("original backup is missing after the target was moved; copied source and journal were retained"))
	}
	if len(errs) == 0 {
		if retainSource {
			if _, sourceErr := os.Lstat(journal.Source); sourceErr == nil {
				recovery := filepath.Join(e.store.Root(), "recovery", journal.ID, filepath.FromSlash(journal.Path))
				if err := os.MkdirAll(filepath.Dir(recovery), 0o700); err != nil {
					errs = append(errs, err)
				} else if _, recoveryErr := os.Lstat(recovery); recoveryErr == nil {
					errs = append(errs, errors.New("recovery destination already exists; copied source and journal were retained"))
				} else if !errors.Is(recoveryErr, os.ErrNotExist) {
					errs = append(errs, recoveryErr)
				} else if err := os.Rename(journal.Source, recovery); err != nil {
					errs = append(errs, err)
				}
			}
		} else if err := fsops.RemoveAny(journal.Source); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		if err := e.removeJournal(journal.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) rollbackDetach(journal domain.Journal) error {
	var errs []error
	if err := fsops.RemoveAny(journal.Stage); err != nil {
		errs = append(errs, err)
	}
	if _, err := os.Lstat(journal.Archive); err == nil {
		if _, sourceErr := os.Lstat(journal.Source); sourceErr == nil {
			errs = append(errs, errors.New("source path was replaced externally; archive and journal were retained"))
		} else if !errors.Is(sourceErr, os.ErrNotExist) {
			errs = append(errs, sourceErr)
		} else if err := os.MkdirAll(filepath.Dir(journal.Source), 0o700); err != nil {
			errs = append(errs, err)
		} else if err := os.Rename(journal.Archive, journal.Source); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		if _, err := os.Lstat(journal.Source); err != nil {
			errs = append(errs, fmt.Errorf("source unavailable during rollback: %w", err))
		}
	}
	if len(errs) == 0 {
		targetInfo, targetErr := os.Lstat(journal.Target)
		switch {
		case errors.Is(targetErr, os.ErrNotExist):
			if err := fsops.AtomicSymlink(journal.Source, journal.Target, journal.ID+"-rollback"); err != nil {
				errs = append(errs, err)
			}
		case targetErr != nil:
			errs = append(errs, targetErr)
		case targetInfo.Mode()&os.ModeSymlink != 0 && fsops.LinkPointsTo(journal.Target, journal.Source):
			// The original owned link is already restored.
		case targetInfo.Mode().IsRegular() || targetInfo.IsDir():
			equal, compareErr := fsops.EqualTree(journal.Source, journal.Target)
			if compareErr != nil {
				errs = append(errs, compareErr)
			} else if !equal {
				errs = append(errs, errors.New("materialized target changed after interruption; it was retained for manual recovery"))
			} else if err := fsops.RemoveAny(journal.Target); err != nil {
				errs = append(errs, err)
			} else if err := fsops.AtomicSymlink(journal.Source, journal.Target, journal.ID+"-rollback"); err != nil {
				errs = append(errs, err)
			}
		default:
			errs = append(errs, errors.New("target has an unexpected type and was retained for manual recovery"))
		}
	}
	if len(errs) == 0 {
		if err := e.removeJournal(journal.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func conflict(path, reason, detail string) domain.Conflict {
	return domain.Conflict{Path: path, Reason: reason, Detail: detail}
}

func containsGitControlPath(path string) bool {
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == ".git" {
			return true
		}
	}
	return false
}

func entryPaths(entries []domain.Entry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	sort.Strings(paths)
	return paths
}

func sourceReferenceCount(state domain.State, sourceRel string) int {
	count := 0
	for _, project := range state.Projects {
		for _, entry := range project.Entries {
			if entry.SourceRel == sourceRel {
				count++
			}
		}
	}
	return count
}

func conflictPaths(conflicts []domain.Conflict) []string {
	paths := make([]string, 0, len(conflicts))
	for _, item := range conflicts {
		paths = append(paths, item.Path)
	}
	return paths
}

func slug(value string) string {
	var out strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
			lastDash = false
		} else if !lastDash && out.Len() > 0 {
			out.WriteByte('-')
			lastDash = true
		}
	}
	result := strings.Trim(out.String(), "-")
	if result == "" {
		return "project"
	}
	return result
}

func randomID(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}
