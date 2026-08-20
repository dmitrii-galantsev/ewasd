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
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

// Relocate restores checkouts after the ewasd data root itself has moved.
//
// Every owned symlink ewasd ever creates points at an absolute path under
// the data root (see Engine.sourcePath / inspect). If that data root is
// moved -- the directory containing profiles/, archive/, transactions/, and
// state.json -- every one of those absolute targets becomes stale: the new
// Engine correctly recomputes each entry's source under the new root (it
// exists, since the content moved with the root), but the symlink on disk
// still encodes the old root's path, so fsops.LinkPointsTo reports it as a
// foreign or mispointed symlink. Engine.inspect classifies every such entry
// as "conflict", and PlanReconcile/Reconcile deliberately never touch a
// conflict (invariant 12): a data-root move otherwise permanently strands
// every managed checkout with no command able to repair it.
//
// AUDIT.md finding #10 diagnosed the old Python tool's answer to this same
// problem: an unbounded filesystem scan plus string replacement, because it
// had no manifest to consult. ewasd's manifest already lists exactly which
// paths are owned and exactly what each one's source is; relocation is
// therefore a REPLAY of that manifest against a caller-supplied old root,
// not a search. That is also why there is deliberately no --scan-dir flag
// here: the manifest already enumerates every registered checkout, so the
// replay covers all of them by construction, and scanning the filesystem
// for stray symlinks would reintroduce exactly the unbounded, unverifiable
// surface AUDIT.md criticized.
//
// The safety property this replay depends on is narrow and load-bearing:
// an entry is only ever retargeted when its on-disk destination is a
// symlink whose literal (readlink) target -- resolved to an absolute,
// cleaned path but NOT further resolved through additional symlinks -- is
// EXACTLY <oldRoot>/<entry.SourceRel>. That exact match is what proves the
// destination is the very symlink ewasd itself wrote under the old root,
// as opposed to some unrelated or already-fixed-up link that merely
// resolves to the same place. Anything else at the destination (a normal
// file or directory, a foreign symlink, a missing destination, or an
// already-correct link) is left completely alone; see
// evaluateRelocateEntry for the full classification.
func (e *Engine) PlanRelocate(oldRoot, projectSelector string) (domain.Plan, error) {
	state, err := e.store.Read()
	if err != nil {
		return domain.Plan{}, err
	}
	return e.planRelocateFromState(state, oldRoot, projectSelector)
}

func (e *Engine) planRelocateFromState(state domain.State, oldRoot, projectSelector string) (domain.Plan, error) {
	normalizedOldRoot, err := normalizeExternalRoot(oldRoot)
	if err != nil {
		return domain.Plan{}, err
	}
	projects, projectID, projectName, err := relocateScope(state, projectSelector)
	if err != nil {
		return domain.Plan{}, err
	}
	plan := domain.Plan{
		ID: randomID(12), Action: "relocate", ProjectID: projectID, ProjectName: projectName,
		ExpectedRevision: state.Revision, Safe: false, Steps: []domain.PlanStep{}, Conflicts: []domain.Conflict{},
		Guarantees: []string{
			"state revision is rechecked under a cross-process lock",
			"normal files, directories, and foreign links are never overwritten",
			"a durable recovery journal precedes destructive renames",
			"source content is never deleted, moved, or modified under either root",
			"only a symlink proven to be ewasd's own old-root link is ever retargeted",
		},
		CreatedAt: e.now(),
	}
	if e.blockPlanForRecovery(&plan) {
		sealPlan(&plan)
		return plan, nil
	}
	relinked, alreadyLinked, refuse := 0, 0, false
	for _, project := range projects {
		for _, entry := range project.Entries {
			candidate := e.evaluateRelocateEntry(project, entry, normalizedOldRoot)
			switch {
			case candidate.reason != "":
				plan.Conflicts = append(plan.Conflicts, domain.Conflict{
					ProjectID: candidate.projectID, Path: candidate.path,
					Reason: candidate.reason, Detail: candidate.detail,
				})
				if candidate.refuse {
					refuse = true
				}
			case candidate.noop:
				alreadyLinked++
			default:
				plan.Steps = append(plan.Steps, domain.PlanStep{
					ProjectID: candidate.projectID, Path: candidate.path, Action: "relink",
					From: candidate.newSource, To: candidate.target,
					Detail: fmt.Sprintf("Retarget the owned symlink from the old data root (%s) to the current data root", normalizedOldRoot),
				})
				relinked++
			}
		}
	}
	// A missing new-root source is refused outright rather than reported as
	// an ordinary per-entry conflict: every other conflict class leaves a
	// pre-existing, already-explainable state untouched, but silently
	// skipping this one would tempt a caller into applying "the rest" of a
	// batch while a dangerous case sits unresolved. Surfacing it as a
	// summary-level refusal keeps that in front of the operator instead.
	plan.Safe = !refuse
	switch {
	case refuse:
		plan.Summary = fmt.Sprintf("Relocation refused: at least one entry's source is missing under the current data root; investigate it (see conflicts) before relocating anything from %s", normalizedOldRoot)
	case len(plan.Steps) == 0:
		plan.Summary = fmt.Sprintf("No entries point at the old data root %s; %d already linked, %d conflict(s) left untouched", normalizedOldRoot, alreadyLinked, len(plan.Conflicts))
	default:
		plan.Summary = fmt.Sprintf("Retarget %d symlink(s) from the old data root %s; %d already linked, %d conflict(s) left untouched", relinked, normalizedOldRoot, alreadyLinked, len(plan.Conflicts))
	}
	sealPlan(&plan)
	return plan, nil
}

// Relocate applies a plan produced by PlanRelocate. It requires the exact
// revision and fingerprint of a plan already reviewed as safe, then
// re-plans from scratch under the exclusive state lock and refuses to
// proceed if the recomputed step set does not match byte-for-byte -- the
// same preview-then-reverify shape Link uses, so that nothing observed
// during the (lock-free) preview can go stale before the write actually
// happens.
//
// Each eligible entry is retargeted independently: a new symlink to the
// current-root source is created at a temporary sibling path, journaled,
// and then atomically renamed over the existing destination. Nothing under
// the old root is ever read after path computation, deleted, or modified.
func (e *Engine) Relocate(oldRoot, projectSelector string, expected uint64, approvedFingerprint string) (domain.ApplyResult, error) {
	initial, err := e.PlanRelocate(oldRoot, projectSelector)
	if err != nil {
		return domain.ApplyResult{}, err
	}
	if initial.ExpectedRevision != expected || approvedFingerprint == "" || initial.Fingerprint != approvedFingerprint {
		return domain.ApplyResult{}, fmt.Errorf("%w: relocate steps or filesystem preconditions changed after review", store.ErrStaleRevision)
	}
	if !initial.Safe {
		return domain.ApplyResult{}, fmt.Errorf("%w: %s", ErrConflict, initial.Summary)
	}
	if len(initial.Steps) == 0 {
		return domain.ApplyResult{
			OK: true, Outcome: "no_change", OperationID: randomID(12), Revision: expected,
			Action: "relocate", Skipped: conflictPaths(initial.Conflicts), Summary: initial.Summary,
		}, nil
	}
	operationID := randomID(12)
	changed := []string{}
	skipped := conflictPaths(initial.Conflicts)
	state, warnings, err := e.store.Transact(&expected, func(state *domain.State) (store.Effects, error) {
		if err := e.requireNoRecovery(); err != nil {
			return store.Effects{}, err
		}
		current, err := e.planRelocateFromState(*state, oldRoot, projectSelector)
		if err != nil {
			return store.Effects{}, err
		}
		if current.Fingerprint != approvedFingerprint {
			return store.Effects{}, fmt.Errorf("%w: relocate steps or filesystem preconditions changed after preview", store.ErrStaleRevision)
		}
		normalizedOldRoot, err := normalizeExternalRoot(oldRoot)
		if err != nil {
			return store.Effects{}, err
		}
		var attempts []relocateAttempt
		journals := []string{}
		effects := store.Effects{Rollback: func() error { return rollbackRelocate(attempts, journals, e.removeJournal) }}
		for _, step := range current.Steps {
			project := state.ProjectByID(step.ProjectID)
			if project == nil {
				return effects, fmt.Errorf("%w: project %s no longer exists", store.ErrStaleRevision, step.ProjectID)
			}
			entry := project.EntryByPath(step.Path)
			if entry == nil {
				return effects, fmt.Errorf("%w: manifest entry %s changed after review", store.ErrStaleRevision, step.Path)
			}
			candidate := e.evaluateRelocateEntry(*project, *entry, normalizedOldRoot)
			if candidate.reason != "" || candidate.noop {
				return effects, fmt.Errorf("%w: %s is no longer eligible for relocation", store.ErrStaleRevision, step.Path)
			}
			id := randomID(12)
			tempLink := filepath.Join(filepath.Dir(candidate.target), ".ewasd-"+id+".relocate")
			journal := domain.Journal{
				ID: id, Action: "relocate", Phase: "prepared", ProjectID: project.ID, Path: entry.Path,
				Source: candidate.newSource, Target: candidate.target, OldSource: candidate.oldSource, Stage: tempLink,
				ExpectedRev: expected, CreatedAt: e.now(), UpdatedAt: e.now(),
			}
			if err := e.writeJournal(journal); err != nil {
				return effects, err
			}
			journals = append(journals, id)
			attempts = append(attempts, relocateAttempt{
				journalID: id, stage: tempLink, target: candidate.target,
				oldSource: candidate.oldSource, newSource: candidate.newSource,
			})
			if err := fsops.AtomicSymlink(candidate.newSource, tempLink, id); err != nil {
				return effects, err
			}
			journal.Phase = "staged"
			journal.UpdatedAt = e.now()
			if err := e.writeJournal(journal); err != nil {
				return effects, err
			}
			if err := os.Rename(tempLink, candidate.target); err != nil {
				return effects, err
			}
			if err := fsops.SyncDir(filepath.Dir(candidate.target)); err != nil {
				return effects, err
			}
			changed = append(changed, entry.Path)
		}
		now := e.now()
		for _, project := range projectsInScope(current, state) {
			project.UpdatedAt = now
		}
		state.AddEvent(domain.Event{
			ID: randomID(8), ProjectID: current.ProjectID, Action: "relocate",
			Summary:   fmt.Sprintf("Relocated %d symlink(s) from the old data root %s", len(changed), normalizedOldRoot),
			CreatedAt: now,
		})
		effects.Commit = func() error {
			var errs []error
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
	sort.Strings(changed)
	return domain.ApplyResult{
		OK: true, Outcome: "completed", OperationID: operationID, Revision: state.Revision, Action: "relocate",
		Changed: changed, Skipped: skipped, Warnings: warnings,
		Summary: fmt.Sprintf("Relocated %d symlink(s) from the old data root", len(changed)),
	}, nil
}

// relocateAttempt records everything needed to converge or unwind a single
// entry's retarget, independent of any in-memory plan: which temporary
// sibling symlink was staged, and the exact old and new absolute sources
// involved. It intentionally mirrors what is durably written to the
// corresponding journal, so rollbackRelocate needs nothing beyond it.
type relocateAttempt struct {
	journalID string
	stage     string
	target    string
	oldSource string
	newSource string
}

// rollbackRelocate unwinds a partially-applied relocate batch when a later
// step failed its live re-verification. For each attempted step it first
// discards any orphaned staged symlink (harmless: it was never linked into
// place), then checks whether the destructive rename actually completed
// (Target now equals the new source). If it did, the step is swapped back
// to its recorded old source using the identical stage-then-rename
// technique, so a failure partway through a multi-entry relocate never
// leaves some entries newly-linked while the operation as a whole reports
// failure. A step whose Target changed to something other than the
// expected new source since it was applied here is left completely alone
// and reported rather than guessed at.
func rollbackRelocate(attempts []relocateAttempt, journals []string, removeJournal func(string) error) error {
	var errs []error
	for i := len(attempts) - 1; i >= 0; i-- {
		step := attempts[i]
		if err := fsops.RemoveAny(step.stage); err != nil {
			errs = append(errs, err)
			continue
		}
		if !fsops.LinkPointsTo(step.target, step.newSource) {
			// The rename never happened (or something else already changed
			// the target back); nothing to undo for this step.
			continue
		}
		revertID := step.journalID + "-rollback"
		revertLink := filepath.Join(filepath.Dir(step.target), ".ewasd-"+revertID+".relocate")
		if err := fsops.AtomicSymlink(step.oldSource, revertLink, revertID); err != nil {
			errs = append(errs, fmt.Errorf("revert %s: %w", step.target, err))
			continue
		}
		if err := os.Rename(revertLink, step.target); err != nil {
			errs = append(errs, fmt.Errorf("revert %s: %w", step.target, err))
			continue
		}
		if err := fsops.SyncDir(filepath.Dir(step.target)); err != nil {
			errs = append(errs, fmt.Errorf("revert %s: %w", step.target, err))
		}
	}
	if len(errs) == 0 {
		for _, id := range journals {
			if err := removeJournal(id); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// projectsInScope resolves the *domain.Project pointers (inside the live
// state being mutated) that a relocate plan's steps touch, so their
// UpdatedAt can be bumped the same way every other mutation does. It walks
// state fresh rather than trusting anything cached on the plan, since plan
// is itself a snapshot taken moments earlier under the same lock.
func projectsInScope(plan domain.Plan, state *domain.State) []*domain.Project {
	seen := map[string]bool{}
	result := []*domain.Project{}
	for _, step := range plan.Steps {
		if seen[step.ProjectID] {
			continue
		}
		seen[step.ProjectID] = true
		if project := state.ProjectByID(step.ProjectID); project != nil {
			result = append(result, project)
		}
	}
	return result
}

// relocateCandidate is the fully-classified outcome of evaluating one
// manifest entry against a caller-supplied old data root. Exactly one of
// (reason != ""), noop, or "eligible for a relink step" holds.
type relocateCandidate struct {
	projectID string
	path      string
	target    string
	oldSource string
	newSource string
	reason    string
	detail    string
	// refuse marks a conflict severe enough to block the entire relocate
	// operation, not just this entry (see planRelocateFromState).
	refuse bool
	// noop marks a destination that is already correctly linked to the
	// current-root source: not a conflict, and nothing to do.
	noop bool
}

// evaluateRelocateEntry classifies a single manifest entry against a
// normalized old data root. It performs no writes.
func (e *Engine) evaluateRelocateEntry(project domain.Project, entry domain.Entry, normalizedOldRoot string) relocateCandidate {
	result := relocateCandidate{projectID: project.ID, path: entry.Path}
	newSource, err := fsops.SafeTarget(e.store.Root(), entry.SourceRel)
	if err != nil {
		result.reason, result.detail = "unsafe-source", err.Error()
		return result
	}
	result.newSource = newSource
	target, err := fsops.SafeTarget(project.Root, entry.Path)
	if err != nil {
		result.reason, result.detail = "unsafe-path", err.Error()
		return result
	}
	result.target = target
	oldSource, err := joinUnderRoot(normalizedOldRoot, entry.SourceRel)
	if err != nil {
		result.reason, result.detail = "unsafe-old-source", err.Error()
		return result
	}
	result.oldSource = oldSource

	info, statErr := os.Lstat(target)
	switch {
	case errors.Is(statErr, os.ErrNotExist):
		result.reason = "missing-destination"
		result.detail = "owned destination is absent; reconcile restores missing links, relocate only retargets a link that already exists"
		return result
	case statErr != nil:
		result.reason, result.detail = "inspect-failed", statErr.Error()
		return result
	case info.Mode()&os.ModeSymlink == 0:
		result.reason = "occupied"
		result.detail = "a normal file or directory occupies the owned destination; it will not be replaced"
		return result
	}
	if fsops.LinkPointsTo(target, newSource) {
		result.noop = true
		return result
	}
	actual, isSymlink := symlinkTargetExact(target)
	if !isSymlink || actual != oldSource {
		result.reason = "foreign-symlink"
		result.detail = "destination is a symlink that does not exactly match this entry's recorded old-root source; it will not be replaced"
		return result
	}
	if _, err := os.Lstat(newSource); err != nil {
		result.reason = "new-source-missing"
		result.detail = fmt.Sprintf("the relocated source is missing under the current data root (%s); refusing to link to a nonexistent target", newSource)
		result.refuse = true
		return result
	}
	return result
}

// relocateScope resolves which projects a relocate call covers: every
// registered project when selector is empty, or exactly one project
// resolved the same way readProject resolves a single-project selector
// (by ID, or by an absolute/registered checkout root) otherwise.
func relocateScope(state domain.State, selector string) ([]domain.Project, string, string, error) {
	if selector == "" {
		return append([]domain.Project(nil), state.Projects...), "", "", nil
	}
	project := state.ProjectByID(selector)
	if project == nil {
		if root, err := filepath.Abs(selector); err == nil {
			if resolved, evalErr := filepath.EvalSymlinks(root); evalErr == nil {
				root = resolved
			}
			project = state.ProjectByRoot(root)
		}
	}
	if project == nil {
		return nil, "", "", ErrNotFound
	}
	return []domain.Project{*project}, project.ID, project.Name, nil
}

// normalizeExternalRoot resolves a caller-supplied old data-root path to an
// absolute, cleaned form suitable for comparison against literal symlink
// targets. It tolerates a trailing slash (filepath.Abs cleans it), a
// relative path (resolved against the current working directory), and a
// symlinked ancestor (resolved via EvalSymlinks on a best-effort basis) --
// but it deliberately does NOT require the old root to exist, since the
// entire scenario relocate exists for is a data root that has already
// moved away from this path.
func normalizeExternalRoot(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("old data root is required")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve old data root: %w", err)
	}
	if resolved, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		abs = resolved
	}
	return abs, nil
}

// joinUnderRoot resolves a manifest-recorded SourceRel beneath root without
// requiring root to exist on disk, unlike fsops.SafeTarget (which resolves
// root through EvalSymlinks and would fail outright on the very old root
// relocate expects to be gone). The manifest's own validation already
// guarantees sourceRel is a clean, safe, forward-slash relative path (see
// store.validateState), so this only needs to guard the join itself
// against escaping root.
func joinUnderRoot(root, sourceRel string) (string, error) {
	relative, err := fsops.ValidateRelative(sourceRel)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(root, filepath.FromSlash(relative))
	check, err := filepath.Rel(root, joined)
	if err != nil || check == ".." || strings.HasPrefix(check, ".."+string(filepath.Separator)) {
		return "", errors.New("resolved old source escapes the old data root")
	}
	return joined, nil
}

// symlinkTargetExact reads a symlink's literal target and resolves it to a
// clean absolute path without following further symlinks. It deliberately
// stops short of fsops.LinkPointsTo's EvalSymlinks fallback: relocation's
// safety property depends on proving the destination is LITERALLY the
// symlink ewasd wrote for the old data root, not merely a link that
// happens to resolve to the same place by some other path -- and the old
// root will typically no longer exist for EvalSymlinks to resolve through
// in the first place.
func symlinkTargetExact(link string) (string, bool) {
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	raw, err := os.Readlink(link)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(filepath.Dir(link), raw)
	}
	abs, err := filepath.Abs(filepath.Clean(raw))
	if err != nil {
		return "", false
	}
	return abs, true
}
