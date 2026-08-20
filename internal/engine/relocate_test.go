package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitrii-galantsev/ewasd/internal/domain"
	"github.com/dmitrii-galantsev/ewasd/internal/fsops"
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

// moveDataRoot physically renames a fixture's data root directory to a
// fresh sibling path and opens a brand-new Engine against it -- exactly
// the scenario a moved ewasd data root produces: entries recomputed
// against the new root all exist, but every on-disk symlink still encodes
// the old, now-gone, absolute path.
func moveDataRoot(t *testing.T, f fixture) (oldRoot string, e2 *Engine) {
	t.Helper()
	oldRoot = f.store.Root()
	newRoot := filepath.Join(filepath.Dir(oldRoot), "moved-data-root")
	if err := os.Rename(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	s, err := store.New(newRoot)
	if err != nil {
		t.Fatal(err)
	}
	return oldRoot, New(s)
}

func requireSymlinkTo(t *testing.T, link, wantTarget string) {
	t.Helper()
	raw, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("expected %s to remain a symlink: %v", link, err)
	}
	if raw != wantTarget {
		t.Fatalf("expected %s to point at %s, got %s", link, wantTarget, raw)
	}
}

func adoptFile(t *testing.T, f fixture, name, content string) {
	t.Helper()
	target := filepath.Join(f.repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := f.engine.PlanAdopt(f.project.ID, name)
	if err != nil {
		t.Fatalf("plan adopt %s: %v", name, err)
	}
	if _, err := f.engine.Adopt(f.project.ID, name, plan.ExpectedRevision, plan.Fingerprint); err != nil {
		t.Fatalf("adopt %s: %v", name, err)
	}
}

// TestRelocateFullScenarioAfterDataRootMove is the load-bearing test: it
// reproduces the exact data-stranding bug described in the AUDIT (moving
// the data root strands every managed link as a permanent "conflict" that
// no other command repairs), then proves PlanRelocate/Relocate recovers
// every entry back to "linked" with file content completely intact.
func TestRelocateFullScenarioAfterDataRootMove(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	adoptFile(t, f, "AGENT.md", "guardrails")
	adoptFile(t, f, "config/settings.toml", "[core]\nvalue = 1\n")

	oldRoot, e2 := moveDataRoot(t, f)

	// Confirm the bug: the new engine correctly resolves sources under the
	// new root, but every on-disk symlink still encodes the old root, so
	// every entry is misclassified as an unrepairable conflict.
	before, err := e2.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Projects) != 1 || before.Projects[0].Health.Conflicts != 2 || before.Projects[0].Health.Linked != 0 {
		t.Fatalf("expected the data-root move to strand both entries as conflicts: %+v", before.Projects[0].Health)
	}
	for _, view := range before.Projects[0].EntriesView {
		if view.Status != "conflict" {
			t.Fatalf("expected %s to be a conflict pre-relocate, got %s: %s", view.Path, view.Status, view.Detail)
		}
	}
	reconcileBlocked, err := e2.PlanReconcile("")
	if err == nil && len(reconcileBlocked.Steps) != 0 {
		t.Fatalf("reconcile should never repair a conflict: %+v", reconcileBlocked)
	}

	plan, err := e2.PlanRelocate(oldRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Safe || len(plan.Steps) != 2 || len(plan.Conflicts) != 0 {
		t.Fatalf("unexpected relocate plan: %+v", plan)
	}
	result, err := e2.Relocate(oldRoot, "", plan.ExpectedRevision, plan.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "completed" || len(result.Changed) != 2 {
		t.Fatalf("unexpected relocate result: %+v", result)
	}

	after, err := e2.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if after.Projects[0].Health.Linked != 2 || after.Projects[0].Health.Conflicts != 0 {
		t.Fatalf("relocate did not restore health: %+v", after.Projects[0].Health)
	}
	newSourceRoot := e2.store.Root()
	for _, view := range after.Projects[0].EntriesView {
		if !fsops.LinkPointsTo(view.Target, view.Source) || !strings.HasPrefix(view.Source, newSourceRoot+string(filepath.Separator)) {
			t.Fatalf("entry %s not correctly relinked under the new root: %+v", view.Path, view)
		}
	}
	agentContent, err := os.ReadFile(filepath.Join(f.repo, "AGENT.md"))
	if err != nil || string(agentContent) != "guardrails" {
		t.Fatalf("AGENT.md content changed: %q, %v", agentContent, err)
	}
	settingsContent, err := os.ReadFile(filepath.Join(f.repo, "config", "settings.toml"))
	if err != nil || string(settingsContent) != "[core]\nvalue = 1\n" {
		t.Fatalf("settings.toml content changed: %q, %v", settingsContent, err)
	}
}

func TestRelocateAlreadyCorrectLinkIsNoOp(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	adoptFile(t, f, "healthy.txt", "x")
	fictitiousOldRoot := filepath.Join(t.TempDir(), "never-existed")

	plan, err := f.engine.PlanRelocate(fictitiousOldRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Safe || len(plan.Steps) != 0 || len(plan.Conflicts) != 0 {
		t.Fatalf("expected a no-op plan for an already-healthy link: %+v", plan)
	}
	result, err := f.engine.Relocate(fictitiousOldRoot, "", plan.ExpectedRevision, plan.Fingerprint)
	if err != nil {
		t.Fatalf("no-op relocate must not error: %v", err)
	}
	if result.Outcome != "no_change" {
		t.Fatalf("expected a no_change outcome, got %+v", result)
	}
	target := filepath.Join(f.repo, "healthy.txt")
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "x" {
		t.Fatalf("no-op relocate touched content: %q, %v", content, err)
	}
}

func TestRelocateLeavesForeignSymlinkByteIdentical(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	adoptFile(t, f, "owned.txt", "x")
	target := filepath.Join(f.repo, "owned.txt")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.txt")
	if err := os.WriteFile(elsewhere, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, target); err != nil {
		t.Fatal(err)
	}
	fictitiousOldRoot := filepath.Join(t.TempDir(), "never-existed")

	plan, err := f.engine.PlanRelocate(fictitiousOldRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Safe || len(plan.Steps) != 0 || len(plan.Conflicts) != 1 || plan.Conflicts[0].Reason != "foreign-symlink" {
		t.Fatalf("expected a single foreign-symlink conflict: %+v", plan)
	}
	if _, err := f.engine.Relocate(fictitiousOldRoot, "", plan.ExpectedRevision, plan.Fingerprint); err != nil {
		t.Fatalf("relocate over an all-conflict plan should be a no-op, not an error: %v", err)
	}
	requireSymlinkTo(t, target, elsewhere)
	content, err := os.ReadFile(elsewhere)
	if err != nil || string(content) != "foreign" {
		t.Fatalf("foreign symlink target content changed: %q, %v", content, err)
	}
}

func TestRelocateNeverReplacesARealFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	adoptFile(t, f, "owned.txt", "x")
	target := filepath.Join(f.repo, "owned.txt")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("local file now"), 0o600); err != nil {
		t.Fatal(err)
	}
	fictitiousOldRoot := filepath.Join(t.TempDir(), "never-existed")

	plan, err := f.engine.PlanRelocate(fictitiousOldRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 0 || len(plan.Conflicts) != 1 || plan.Conflicts[0].Reason != "occupied" {
		t.Fatalf("expected a single occupied conflict: %+v", plan)
	}
	info, err := os.Lstat(target)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("planning must not touch a real file: %v, %v", info, err)
	}
	content, _ := os.ReadFile(target)
	if string(content) != "local file now" {
		t.Fatal("planning mutated a real file's content")
	}
}

func TestRelocateReportsMissingDestinationAsReconcilesJob(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	adoptFile(t, f, "owned.txt", "x")
	target := filepath.Join(f.repo, "owned.txt")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	fictitiousOldRoot := filepath.Join(t.TempDir(), "never-existed")

	plan, err := f.engine.PlanRelocate(fictitiousOldRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 0 || len(plan.Conflicts) != 1 || plan.Conflicts[0].Reason != "missing-destination" {
		t.Fatalf("expected a single missing-destination conflict: %+v", plan)
	}
	if !strings.Contains(plan.Conflicts[0].Detail, "reconcile") {
		t.Fatalf("detail should point the caller at reconcile: %+v", plan.Conflicts[0])
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("relocate must never create a missing destination: %v", err)
	}
}

func TestRelocateRefusesWholeOperationWhenNewSourceMissing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	adoptFile(t, f, "owned.txt", "x")
	adoptFile(t, f, "healthy.txt", "y")

	oldRoot, e2 := moveDataRoot(t, f)
	missingSource := filepath.Join(e2.store.Root(), "profiles", f.project.SourceID, "files", "owned.txt")
	if err := os.Remove(missingSource); err != nil {
		t.Fatal(err)
	}

	plan, err := e2.PlanRelocate(oldRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Safe {
		t.Fatalf("plan must refuse the whole operation when a new-root source is missing: %+v", plan)
	}
	foundMissing := false
	for _, c := range plan.Conflicts {
		if c.Reason == "new-source-missing" {
			foundMissing = true
		}
	}
	if !foundMissing {
		t.Fatalf("expected a new-source-missing conflict: %+v", plan.Conflicts)
	}
	if _, err := e2.Relocate(oldRoot, "", plan.ExpectedRevision, plan.Fingerprint); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected relocate to refuse via ErrConflict, got %v", err)
	}
	// Neither entry -- including the perfectly healthy-for-relocation one --
	// should have been touched by the refusal.
	oldOwned := filepath.Join(oldRoot, "profiles", f.project.SourceID, "files", "owned.txt")
	oldHealthy := filepath.Join(oldRoot, "profiles", f.project.SourceID, "files", "healthy.txt")
	requireSymlinkTo(t, filepath.Join(f.repo, "owned.txt"), oldOwned)
	requireSymlinkTo(t, filepath.Join(f.repo, "healthy.txt"), oldHealthy)
}

func TestRelocateRejectsStaleRevision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	adoptFile(t, f, "owned.txt", "x")
	oldRoot, e2 := moveDataRoot(t, f)

	plan, err := e2.PlanRelocate(oldRoot, "")
	if err != nil {
		t.Fatal(err)
	}

	second := filepath.Join(filepath.Dir(f.repo), "unrelated-registration")
	if err := os.Mkdir(second, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, second, "init", "-q")
	if _, _, err := e2.Register(second, "Unrelated"); err != nil {
		t.Fatal(err)
	}

	if _, err := e2.Relocate(oldRoot, "", plan.ExpectedRevision, plan.Fingerprint); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("expected a stale revision rejection, got %v", err)
	}
	requireSymlinkTo(t, filepath.Join(f.repo, "owned.txt"), filepath.Join(oldRoot, "profiles", f.project.SourceID, "files", "owned.txt"))
}

func TestRelocateRejectsStaleFingerprint(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	adoptFile(t, f, "one.txt", "one")
	adoptFile(t, f, "two.txt", "two")
	oldRoot, e2 := moveDataRoot(t, f)

	plan, err := e2.PlanRelocate(oldRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("expected both entries eligible for relocation: %+v", plan)
	}

	// Change what a fresh plan would see, at the same revision, so only
	// the fingerprint (recomputed step/conflict set) diverges.
	twoTarget := filepath.Join(f.repo, "two.txt")
	if err := os.Remove(twoTarget); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "unrelated"), twoTarget); err != nil {
		t.Fatal(err)
	}

	if _, err := e2.Relocate(oldRoot, "", plan.ExpectedRevision, plan.Fingerprint); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("expected a stale fingerprint rejection, got %v", err)
	}
	// The re-plan-under-lock must abort before touching anything, including
	// the entry that was still perfectly eligible.
	requireSymlinkTo(t, filepath.Join(f.repo, "one.txt"), filepath.Join(oldRoot, "profiles", f.project.SourceID, "files", "one.txt"))
}

func TestRelocatePlanBlockedByPendingRecoveryJournal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := filepath.Join(f.repo, "crash.txt")
	backup := filepath.Join(f.repo, ".ewasd-crash.backup")
	source, _ := f.engine.sourcePath(f.project, "crash.txt")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, target); err != nil {
		t.Fatal(err)
	}
	journal := domain.Journal{ID: "crash", Action: "adopt", Phase: "linked", ProjectID: f.project.ID, Path: "crash.txt", Source: source, Target: target, Stage: filepath.Join(f.store.Root(), "transactions", "crash.stage"), Backup: backup}
	if err := f.engine.writeJournal(journal); err != nil {
		t.Fatal(err)
	}

	oldRoot := filepath.Join(t.TempDir(), "old")
	plan, err := f.engine.PlanRelocate(oldRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Safe || len(plan.Conflicts) != 1 || plan.Conflicts[0].Reason != "recovery-required" {
		t.Fatalf("pending recovery must block the relocate plan: %+v", plan)
	}
	if _, err := f.engine.Relocate(oldRoot, "", plan.ExpectedRevision, plan.Fingerprint); err == nil {
		t.Fatal("expected relocate apply to be refused while recovery is pending")
	}
}

func TestRelocateApplyRefusedWhenRecoveryAppearsAfterPlanning(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	adoptFile(t, f, "owned.txt", "x")
	oldRoot, e2 := moveDataRoot(t, f)

	plan, err := e2.PlanRelocate(oldRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Safe || len(plan.Steps) != 1 {
		t.Fatalf("expected a single safe step: %+v", plan)
	}
	// A concurrent writer leaves an unrelated recovery journal behind
	// between preview and apply.
	stray := domain.Journal{ID: "stray", Action: "reconcile", Phase: "prepared", ProjectID: f.project.ID, Path: "owned.txt", Source: plan.Steps[0].From, Target: plan.Steps[0].To}
	if err := e2.writeJournal(stray); err != nil {
		t.Fatal(err)
	}
	// However Relocate ultimately classifies it (either its own outer
	// re-plan already sees the stray journal and marks itself unsafe, or
	// the inner requireNoRecovery gate catches it under the lock), it must
	// refuse to apply anything while recovery is outstanding, exactly like
	// Adopt/Detach/Reconcile do.
	if _, err := e2.Relocate(oldRoot, "", plan.ExpectedRevision, plan.Fingerprint); err == nil {
		t.Fatal("expected relocate to be refused once a recovery journal appears")
	}
	requireSymlinkTo(t, filepath.Join(f.repo, "owned.txt"), filepath.Join(oldRoot, "profiles", f.project.SourceID, "files", "owned.txt"))
}

// TestRelocateRecoversInterruptedPhases simulates a crash landing on each
// side of relocate's single destructive step (renaming a staged temporary
// symlink over the destination), following the same crash-injection
// pattern as TestRecoverAdoptPrecommitPhases / TestRecoverDetachPhasesWithoutLosingBytes:
// manually construct the on-disk state a real interruption would leave at
// that phase, write the matching journal by hand, then call Recover and
// assert no content is lost and state converges either way.
func TestRelocateRecoversInterruptedPhases(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"prepared", "staged", "staged-after-rename"} {
		phase := phase
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t)
			adoptFile(t, f, "phase.txt", "content")
			target := filepath.Join(f.repo, "phase.txt")
			oldRoot, e2 := moveDataRoot(t, f)
			oldSource := filepath.Join(oldRoot, "profiles", f.project.SourceID, "files", "phase.txt")
			newSource := filepath.Join(e2.store.Root(), "profiles", f.project.SourceID, "files", "phase.txt")
			id := "relocate-" + phase
			stage := filepath.Join(filepath.Dir(target), ".ewasd-"+id+".relocate")
			journalPhase := "staged"

			switch phase {
			case "prepared":
				journalPhase = "prepared"
				// Nothing on disk yet: target is still whatever Adopt left
				// it as (a symlink to the now-gone old root).
			case "staged":
				if err := fsops.AtomicSymlink(newSource, stage, id); err != nil {
					t.Fatal(err)
				}
			case "staged-after-rename":
				// The rename already completed; the journal just never got
				// removed before the crash. No stage file remains (it was
				// consumed by the rename).
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(newSource, target); err != nil {
					t.Fatal(err)
				}
			}
			journal := domain.Journal{
				ID: id, Action: "relocate", Phase: journalPhase, ProjectID: f.project.ID, Path: "phase.txt",
				Source: newSource, Target: target, OldSource: oldSource, Stage: stage,
			}
			if err := e2.writeJournal(journal); err != nil {
				t.Fatal(err)
			}
			if _, err := e2.Recover(); err != nil {
				t.Fatalf("recover: %v", err)
			}
			if _, err := os.Lstat(e2.journalPath(id)); !os.IsNotExist(err) {
				t.Fatalf("journal was not cleared in phase %s: %v", phase, err)
			}
			if _, err := os.Lstat(stage); !os.IsNotExist(err) {
				t.Fatalf("staged temp symlink leaked in phase %s: %v", phase, err)
			}
			switch phase {
			case "prepared", "staged":
				requireSymlinkTo(t, target, oldSource)
			case "staged-after-rename":
				if !fsops.LinkPointsTo(target, newSource) {
					t.Fatalf("completed relocation was not preserved by recovery in phase %s", phase)
				}
				content, err := os.ReadFile(target)
				if err != nil || string(content) != "content" {
					t.Fatalf("content lost in phase %s: %q, %v", phase, content, err)
				}
			}
		})
	}
}

func TestRelocateOldRootPathVariants(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	adoptFile(t, f, "owned.txt", "x")
	oldRoot, e2 := moveDataRoot(t, f)

	t.Run("trailing slash", func(t *testing.T) {
		plan, err := e2.PlanRelocate(oldRoot+string(filepath.Separator), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Steps) != 1 {
			t.Fatalf("trailing slash old root was not matched: %+v", plan)
		}
	})

	t.Run("relative path", func(t *testing.T) {
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(cwd, oldRoot)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := e2.PlanRelocate(rel, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Steps) != 1 {
			t.Fatalf("relative old root %q was not matched: %+v", rel, plan)
		}
	})
}

// TestRelocateOldRootViaSymlinkedAncestor covers an old root reached
// through a symlinked ancestor. Unlike moveDataRoot's "physically gone"
// scenario, this leaves the original store directory in place (as if it
// were still-mounted old storage) and stands up an independent copy as the
// active new data root, then resolves the old root through a fresh
// symlink alias -- exercising normalizeExternalRoot's EvalSymlinks path
// rather than its not-found fallback.
func TestRelocateOldRootViaSymlinkedAncestor(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	adoptFile(t, f, "owned.txt", "x")
	target := filepath.Join(f.repo, "owned.txt")

	oldRoot := f.store.Root()
	newRoot := filepath.Join(t.TempDir(), "copied-state")
	if err := fsops.CopyTree(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	newStore, err := store.New(newRoot)
	if err != nil {
		t.Fatal(err)
	}
	e2 := New(newStore)

	alias := filepath.Join(t.TempDir(), "alias-to-old-root")
	if err := os.Symlink(oldRoot, alias); err != nil {
		t.Fatal(err)
	}

	plan, err := e2.PlanRelocate(alias, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Path != "owned.txt" {
		t.Fatalf("symlinked-ancestor old root was not resolved: %+v", plan)
	}
	result, err := e2.Relocate(alias, "", plan.ExpectedRevision, plan.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changed) != 1 {
		t.Fatalf("unexpected relocate result: %+v", result)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "x" {
		t.Fatalf("content changed after relocation via symlinked ancestor: %q, %v", content, err)
	}
	// The original store directory (the "still-mounted old storage") must
	// be completely untouched by relocation.
	untouched, err := os.ReadFile(filepath.Join(oldRoot, "profiles", f.project.SourceID, "files", "owned.txt"))
	if err != nil || string(untouched) != "x" {
		t.Fatalf("relocation modified content under the old root: %q, %v", untouched, err)
	}
}

func TestRelocateMultipleProjectsAndScoping(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	adoptFile(t, f, "one.txt", "one")

	secondRepo := filepath.Join(filepath.Dir(f.repo), "second-project")
	if err := os.Mkdir(secondRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, secondRepo, "init", "-q")
	secondProject, _, err := f.engine.Register(secondRepo, "Second")
	if err != nil {
		t.Fatal(err)
	}
	target2 := filepath.Join(secondRepo, "two.txt")
	if err := os.WriteFile(target2, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	p2, err := f.engine.PlanAdopt(secondProject.ID, "two.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Adopt(secondProject.ID, "two.txt", p2.ExpectedRevision, p2.Fingerprint); err != nil {
		t.Fatal(err)
	}

	oldRoot, e2 := moveDataRoot(t, f)

	scoped, err := e2.PlanRelocate(oldRoot, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Steps) != 1 || scoped.Steps[0].Path != "one.txt" || scoped.ProjectID != f.project.ID {
		t.Fatalf("scoped plan should only cover the selected project: %+v", scoped)
	}
	if _, err := e2.Relocate(oldRoot, f.project.ID, scoped.ExpectedRevision, scoped.Fingerprint); err != nil {
		t.Fatal(err)
	}

	mid, err := e2.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	healthByID := map[string]domain.Health{}
	for _, pv := range mid.Projects {
		healthByID[pv.ID] = pv.Health
	}
	if healthByID[f.project.ID].Linked != 1 || healthByID[f.project.ID].Conflicts != 0 {
		t.Fatalf("scoped project was not relocated: %+v", healthByID[f.project.ID])
	}
	if healthByID[secondProject.ID].Conflicts != 1 {
		t.Fatalf("unscoped project must remain stranded after a scoped relocate: %+v", healthByID[secondProject.ID])
	}

	all, err := e2.PlanRelocate(oldRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Steps) != 1 || all.Steps[0].Path != "two.txt" || all.Steps[0].ProjectID != secondProject.ID {
		t.Fatalf("expected only the remaining stranded entry in an all-projects plan: %+v", all)
	}
	if _, err := e2.Relocate(oldRoot, "", all.ExpectedRevision, all.Fingerprint); err != nil {
		t.Fatal(err)
	}
	final, err := e2.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, pv := range final.Projects {
		if pv.Health.Conflicts != 0 || pv.Health.Linked != pv.Health.Total {
			t.Fatalf("project %s still unhealthy after relocation: %+v", pv.ID, pv.Health)
		}
	}
}

// TestRelocateSharedSourceAcrossTwoCheckouts covers the repo's real
// OrcaSlicer-style topology: two independent checkouts (e.g. worktrees or
// clones) of the same remote sharing one SourceID/profile via Link. Each
// checkout owns its own Project record and its own on-disk symlink, so a
// data-root move strands both independently, and relocate must repair
// both independently too.
func TestRelocateSharedSourceAcrossTwoCheckouts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	adoptFile(t, f, "shared.txt", "shared content")

	secondRepo := filepath.Join(filepath.Dir(f.repo), "second-worktree")
	if err := os.Mkdir(secondRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, secondRepo, "init", "-q")
	git(t, secondRepo, "remote", "add", "origin", "https://github.com/example/widget.git")
	linkPlan, err := f.engine.PlanLink(secondRepo, "")
	if err != nil {
		t.Fatal(err)
	}
	if !linkPlan.NewProject || linkPlan.TemplateProjectID != f.project.ID {
		t.Fatalf("expected the second checkout to template off the first via remote match: %+v", linkPlan)
	}
	if _, err := f.engine.Link(secondRepo, "", linkPlan.Fingerprint); err != nil {
		t.Fatal(err)
	}

	preMove, err := f.engine.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var secondProjectID string
	for _, pv := range preMove.Projects {
		if pv.Root == secondRepo {
			secondProjectID = pv.ID
			if pv.SourceID != f.project.SourceID {
				t.Fatalf("second checkout does not share the first's source: %+v", pv)
			}
		}
	}
	if secondProjectID == "" {
		t.Fatal("second checkout was not registered")
	}

	oldRoot, e2 := moveDataRoot(t, f)
	stranded, err := e2.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, pv := range stranded.Projects {
		if pv.Health.Conflicts != 1 {
			t.Fatalf("expected the move to strand project %s: %+v", pv.ID, pv.Health)
		}
	}

	plan, err := e2.PlanRelocate(oldRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("expected both shared-source checkouts to need relocation: %+v", plan)
	}
	result, err := e2.Relocate(oldRoot, "", plan.ExpectedRevision, plan.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changed) != 2 {
		t.Fatalf("unexpected relocate result: %+v", result)
	}

	after, err := e2.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, pv := range after.Projects {
		if pv.Health.Linked != 1 || pv.Health.Conflicts != 0 {
			t.Fatalf("project %s not fully relocated: %+v", pv.ID, pv.Health)
		}
	}
	for _, repoDir := range []string{f.repo, secondRepo} {
		content, err := os.ReadFile(filepath.Join(repoDir, "shared.txt"))
		if err != nil || string(content) != "shared content" {
			t.Fatalf("content wrong for %s after relocation: %q, %v", repoDir, content, err)
		}
	}
}
