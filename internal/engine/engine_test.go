package engine

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmitrii-galantsev/ewasd/internal/domain"
	"github.com/dmitrii-galantsev/ewasd/internal/fsops"
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

type fixture struct {
	engine  *Engine
	store   *store.Store
	repo    string
	project domain.Project
	rev     uint64
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "checkout")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	git(t, repo, "remote", "add", "origin", "git@github.com:Example/Widget.git")
	s, err := store.New(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	e := New(s)
	project, rev, err := e.Register(repo, "Widget")
	if err != nil {
		t.Fatal(err)
	}
	return fixture{engine: e, store: s, repo: repo, project: project, rev: rev}
}

func TestAdoptReconcileAndDetachLifecycle(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	exclude := filepath.Join(f.repo, ".git", "info", "exclude")
	if err := os.WriteFile(exclude, []byte("# keep me\n*.local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.repo, "AGENT.md")
	if err := os.WriteFile(target, []byte("guardrails\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	plan, err := f.engine.PlanAdopt(f.project.ID, "AGENT.md")
	if err != nil || !plan.Safe || len(plan.Steps) != 3 || plan.ExpectedRevision != f.rev {
		t.Fatalf("unexpected plan: %+v, %v", plan, err)
	}
	result, err := f.engine.Adopt(f.project.ID, "AGENT.md", plan.ExpectedRevision, plan.Fingerprint)
	if err != nil || result.Revision != f.rev+1 {
		t.Fatalf("adopt: %+v, %v", result, err)
	}
	if !fsops.LinkPointsTo(target, filepath.Join(f.store.Root(), "profiles", f.project.ID, "files", "AGENT.md")) {
		t.Fatal("target is not the owned link")
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "guardrails\n" {
		t.Fatalf("linked content changed: %q, %v", content, err)
	}
	excludeData, _ := os.ReadFile(exclude)
	if !strings.Contains(string(excludeData), "# keep me") || !strings.Contains(string(excludeData), "/AGENT.md") {
		t.Fatalf("exclude content wrong:\n%s", excludeData)
	}
	if err := os.WriteFile(exclude, []byte("# keep me\n*.local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ignoreRepair, err := f.engine.PlanReconcile(f.project.ID)
	if err != nil || len(ignoreRepair.Steps) != 1 || ignoreRepair.Steps[0].Action != "git-ignore" {
		t.Fatalf("Git ignore drift was not planned for repair: %+v, %v", ignoreRepair, err)
	}
	if _, err := f.engine.Reconcile(f.project.ID, ignoreRepair.ExpectedRevision, ignoreRepair.Fingerprint); err != nil {
		t.Fatalf("repair Git ignore drift: %v", err)
	}
	snapshotAfterRepair, _ := f.engine.Snapshot()
	if !snapshotAfterRepair.Projects[0].GitIgnoreOK {
		t.Fatalf("Git ignore still reports drift: %+v", snapshotAfterRepair.Projects[0])
	}

	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	reconcile, err := f.engine.PlanReconcile(f.project.ID)
	if err != nil || len(reconcile.Steps) != 1 {
		t.Fatalf("reconcile plan: %+v, %v", reconcile, err)
	}
	result, err = f.engine.Reconcile(f.project.ID, reconcile.ExpectedRevision, reconcile.Fingerprint)
	if err != nil || len(result.Changed) != 1 || !fsops.LinkPointsTo(target, filepath.Join(f.store.Root(), "profiles", f.project.ID, "files", "AGENT.md")) {
		t.Fatalf("reconcile failed: %+v, %v", result, err)
	}

	detach, err := f.engine.PlanDetach(f.project.ID, "AGENT.md")
	if err != nil || !detach.Safe {
		t.Fatalf("detach plan: %+v, %v", detach, err)
	}
	result, err = f.engine.Detach(f.project.ID, "AGENT.md", detach.ExpectedRevision, detach.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(target)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("detach did not materialize a regular file: %v, %v", info, err)
	}
	content, _ = os.ReadFile(target)
	if string(content) != "guardrails\n" {
		t.Fatalf("materialized bytes changed: %q", content)
	}
	snapshot, _ := f.engine.Snapshot()
	if snapshot.Projects[0].Health.Total != 0 || len(snapshot.Activity) != 5 {
		t.Fatalf("unexpected final snapshot: %+v", snapshot)
	}
	excludeData, _ = os.ReadFile(exclude)
	if string(excludeData) != "# keep me\n*.local\n" {
		t.Fatalf("detach did not restore user-only exclude:\n%s", excludeData)
	}
	archiveMatches, _ := filepath.Glob(filepath.Join(f.store.Root(), "archive", "*", f.project.ID, "AGENT.md"))
	if len(archiveMatches) != 1 {
		t.Fatalf("expected one retained archive, got %v", archiveMatches)
	}
}

func TestAdoptRejectsForeignSymlinkAndNestedSymlink(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	other := filepath.Join(f.repo, "other")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(f.repo, "foreign")); err != nil {
		t.Fatal(err)
	}
	plan, err := f.engine.PlanAdopt(f.project.ID, "foreign")
	if err != nil || plan.Safe || plan.Conflicts[0].Reason != "unsupported-target" {
		t.Fatalf("foreign symlink plan: %+v, %v", plan, err)
	}
	dir := filepath.Join(f.repo, "config")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(dir, "nested")); err != nil {
		t.Fatal(err)
	}
	plan, err = f.engine.PlanAdopt(f.project.ID, "config")
	if err != nil || plan.Safe || plan.Conflicts[0].Reason != "unsupported-content" {
		t.Fatalf("nested symlink plan: %+v, %v", plan, err)
	}
}

func TestAdoptRejectsSymlinkedCentralParent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Join(f.repo, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "nested", "config"), []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	centralParent := filepath.Join(f.store.Root(), "profiles", f.project.ID, "files", "nested")
	if err := os.Symlink(outside, centralParent); err != nil {
		t.Fatal(err)
	}
	plan, err := f.engine.PlanAdopt(f.project.ID, "nested/config")
	if err != nil || plan.Safe || len(plan.Conflicts) != 1 || plan.Conflicts[0].Reason != "unsafe-source" {
		t.Fatalf("symlinked central parent was not rejected: %+v, %v", plan, err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("unsafe central path wrote outside the store: %v", entries)
	}
}

func TestAdoptRejectsGitControlPaths(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := f.engine.PlanAdopt(f.project.ID, ".git/config"); err == nil {
		t.Fatal(".git control path was accepted")
	}
}

func TestStalePlanCannotMutateLocalFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := filepath.Join(f.repo, "config.txt")
	if err := os.WriteFile(target, []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := f.engine.PlanAdopt(f.project.ID, "config.txt")
	if err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(filepath.Dir(f.repo), "second")
	if err := os.Mkdir(second, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, second, "init", "-q")
	if _, _, err := f.engine.Register(second, "Second"); err != nil {
		t.Fatal(err)
	}
	_, err = f.engine.Adopt(f.project.ID, "config.txt", plan.ExpectedRevision, plan.Fingerprint)
	if !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("expected stale revision, got %v", err)
	}
	info, _ := os.Lstat(target)
	content, _ := os.ReadFile(target)
	if !info.Mode().IsRegular() || string(content) != "local" {
		t.Fatal("stale apply mutated the local file")
	}
}

func TestRegisterRejectsOverlappingCheckoutRoots(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	nested := filepath.Join(f.repo, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.engine.Register(nested, "Nested"); !errors.Is(err, ErrConflict) {
		t.Fatalf("nested root was accepted: %v", err)
	}
	parent := filepath.Dir(f.repo)
	if _, _, err := f.engine.Register(parent, "Parent"); err == nil {
		t.Fatal("parent root was accepted")
	}
}

func TestUnregisterOnlyRemovesEmptyRegistration(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result, err := f.engine.Unregister(f.project.ID, f.rev)
	if err != nil || result.Outcome != "completed" {
		t.Fatalf("unregister empty project: %+v, %v", result, err)
	}
	snapshot, err := f.engine.Snapshot()
	if err != nil || len(snapshot.Projects) != 0 {
		t.Fatalf("project remains registered: %+v, %v", snapshot, err)
	}
	if _, err := os.Stat(f.repo); err != nil {
		t.Fatalf("checkout was touched: %v", err)
	}

	withEntry := newFixture(t)
	target := filepath.Join(withEntry.repo, "managed")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, _ := withEntry.engine.PlanAdopt(withEntry.project.ID, "managed")
	adopted, err := withEntry.engine.Adopt(withEntry.project.ID, "managed", plan.ExpectedRevision, plan.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withEntry.engine.Unregister(withEntry.project.ID, adopted.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("managed project was unregistered: %v", err)
	}
}

func TestImportLegacyProjectCopiesSourcesAndSwitchesOnlyExactLinks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	legacyRoot := filepath.Join(filepath.Dir(f.store.Root()), "legacy-source")
	legacyDir := filepath.Join(legacyRoot, ".config")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "settings.json"), []byte("{\"legacy\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("settings.json", filepath.Join(legacyDir, "current.json")); err != nil {
		t.Fatal(err)
	}
	legacyFile := filepath.Join(legacyRoot, "AGENT.md")
	if err := os.WriteFile(legacyFile, []byte("legacy agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	targetDir := filepath.Join(f.repo, ".config")
	targetFile := filepath.Join(f.repo, "AGENT.md")
	if err := os.Symlink(legacyDir, targetDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(legacyFile, targetFile); err != nil {
		t.Fatal(err)
	}
	// Import into a fresh state because newFixture already registered its root.
	stateRoot := filepath.Join(filepath.Dir(f.store.Root()), "migration-state")
	migrationStore, err := store.New(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	migrationEngine := New(migrationStore)
	checkout := f.repo
	plan := domain.LegacyProjectPlan{
		Name: "Migrated", Root: f.repo, GitRoot: checkout, Remote: "github.com/example/widget", SourceRoot: legacyRoot,
		Entries: []domain.LegacyEntryPlan{
			{Path: ".config", Kind: "directory", LegacySource: legacyDir, Target: targetDir},
			{Path: "AGENT.md", Kind: "file", LegacySource: legacyFile, Target: targetFile},
		},
	}
	result, err := migrationEngine.ImportLegacyProject(plan)
	if err != nil || result.Outcome != "completed" || len(result.Changed) != 2 {
		t.Fatalf("import: %+v, %v", result, err)
	}
	snapshot, err := migrationEngine.Snapshot()
	if err != nil || len(snapshot.Projects) != 1 || snapshot.Projects[0].Health.Linked != 2 || !snapshot.Projects[0].GitIgnoreOK {
		t.Fatalf("unhealthy migration: %+v, %v", snapshot, err)
	}
	project := snapshot.Projects[0]
	for _, view := range project.EntriesView {
		if !fsops.LinkPointsTo(view.Target, view.Source) {
			t.Fatalf("target was not switched: %+v", view)
		}
	}
	if data, _ := os.ReadFile(legacyFile); string(data) != "legacy agent\n" {
		t.Fatalf("legacy source changed: %q", data)
	}
	newDir := filepath.Join(migrationStore.Root(), "profiles", project.ID, "files", ".config")
	if raw, err := os.Readlink(filepath.Join(newDir, "current.json")); err != nil || raw != "settings.json" {
		t.Fatalf("contained nested symlink was not preserved: %q, %v", raw, err)
	}
	secondRepo := filepath.Join(filepath.Dir(f.repo), "second-checkout")
	if err := os.Mkdir(secondRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, secondRepo, "init", "-q")
	secondDir := filepath.Join(secondRepo, ".config")
	secondFile := filepath.Join(secondRepo, "AGENT.md")
	if err := os.Symlink(legacyDir, secondDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(legacyFile, secondFile); err != nil {
		t.Fatal(err)
	}
	secondPlan := plan
	secondPlan.Root = secondRepo
	secondPlan.GitRoot = secondRepo
	secondPlan.Entries = []domain.LegacyEntryPlan{
		{Path: ".config", Kind: "directory", LegacySource: legacyDir, Target: secondDir},
		{Path: "AGENT.md", Kind: "file", LegacySource: legacyFile, Target: secondFile},
	}
	if _, err := migrationEngine.ImportLegacyProject(secondPlan); err != nil {
		t.Fatal(err)
	}
	sharedSnapshot, err := migrationEngine.Snapshot()
	if err != nil || len(sharedSnapshot.Projects) != 2 || sharedSnapshot.Projects[0].SourceID != sharedSnapshot.Projects[1].SourceID {
		t.Fatalf("legacy source was not shared: %+v, %v", sharedSnapshot.Projects, err)
	}
	if filepath.Clean(sharedSnapshot.Projects[0].EntriesView[1].Source) != filepath.Clean(sharedSnapshot.Projects[1].EntriesView[1].Source) {
		t.Fatal("shared checkout links point to divergent Go sources")
	}
	if err := os.WriteFile(sharedSnapshot.Projects[0].EntriesView[1].Source, []byte("edited through shared source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(secondFile); string(data) != "edited through shared source\n" {
		t.Fatalf("second checkout did not observe shared edit: %q", data)
	}
	blockedDetach, err := migrationEngine.PlanDetach(sharedSnapshot.Projects[0].ID, "AGENT.md")
	if err != nil || blockedDetach.Safe || len(blockedDetach.Conflicts) == 0 || blockedDetach.Conflicts[0].Reason != "shared-source" {
		t.Fatalf("shared source detach was not blocked: %+v, %v", blockedDetach, err)
	}
	if backups, err := filepath.Glob(filepath.Join(f.repo, ".ewasd-migrate-*.link")); err != nil || len(backups) != 0 {
		t.Fatalf("migration backups remain: %v, %v", backups, err)
	}
}

func TestReconcileLeavesConflictUntouched(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := filepath.Join(f.repo, "owned")
	if err := os.WriteFile(target, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, _ := f.engine.PlanAdopt(f.project.ID, "owned")
	if _, err := f.engine.Adopt(f.project.ID, "owned", plan.ExpectedRevision, plan.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("local conflict"), 0o600); err != nil {
		t.Fatal(err)
	}
	reconcile, err := f.engine.PlanReconcile(f.project.ID)
	if err != nil || len(reconcile.Steps) != 0 || len(reconcile.Conflicts) != 1 {
		t.Fatalf("unexpected reconcile plan: %+v, %v", reconcile, err)
	}
	result, err := f.engine.Reconcile(f.project.ID, reconcile.ExpectedRevision, reconcile.Fingerprint)
	if err != nil || result.Revision != reconcile.ExpectedRevision {
		t.Fatalf("no-op reconcile: %+v, %v", result, err)
	}
	content, _ := os.ReadFile(target)
	if string(content) != "local conflict" {
		t.Fatal("reconcile overwrote conflict")
	}
}

func TestReconcileRejectsFilesystemDriftBeyondReviewedStepSet(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		target := filepath.Join(f.repo, name)
		if err := os.WriteFile(target, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		plan, err := f.engine.PlanAdopt(f.project.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.engine.Adopt(f.project.ID, name, plan.ExpectedRevision, plan.Fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(f.repo, "a.md")); err != nil {
		t.Fatal(err)
	}
	reviewed, err := f.engine.PlanReconcile(f.project.ID)
	if err != nil || len(reviewed.Steps) != 1 || reviewed.Steps[0].Path != "a.md" {
		t.Fatalf("unexpected reviewed plan: %+v, %v", reviewed, err)
	}
	for _, name := range []string{"b.md", "c.md"} {
		if err := os.Remove(filepath.Join(f.repo, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.engine.Reconcile(f.project.ID, reviewed.ExpectedRevision, reviewed.Fingerprint); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("filesystem drift did not stale the reviewed plan: %v", err)
	}
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		if _, err := os.Lstat(filepath.Join(f.repo, name)); !os.IsNotExist(err) {
			t.Fatalf("stale reconcile changed %s: %v", name, err)
		}
	}
}

func TestSourceTypeDriftIsReportedAndNeverReconciled(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := filepath.Join(f.repo, "owned")
	if err := os.WriteFile(target, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, _ := f.engine.PlanAdopt(f.project.ID, "owned")
	if _, err := f.engine.Adopt(f.project.ID, "owned", plan.ExpectedRevision, plan.Fingerprint); err != nil {
		t.Fatal(err)
	}
	source, _ := f.engine.sourcePath(f.project, "owned")
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(f.repo, "other")
	if err := os.WriteFile(other, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, source); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.engine.Snapshot()
	if err != nil || snapshot.Projects[0].EntriesView[0].Status != "source-invalid" {
		t.Fatalf("source type drift not reported: %+v, %v", snapshot, err)
	}
	reconcile, err := f.engine.PlanReconcile(f.project.ID)
	if err != nil || len(reconcile.Conflicts) != 1 || len(reconcile.Steps) != 0 {
		t.Fatalf("source type drift should block reconcile: %+v, %v", reconcile, err)
	}
}

func TestRecoverRollsBackInterruptedAdoptAndRetainsUncertainSource(t *testing.T) {
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
	stage := filepath.Join(f.store.Root(), "transactions", "crash.stage")
	journal := domain.Journal{ID: "crash", Action: "adopt", Phase: "linked", ProjectID: f.project.ID, Path: "crash.txt", Source: source, Target: target, Stage: stage, Backup: backup}
	if err := f.engine.writeJournal(journal); err != nil {
		t.Fatal(err)
	}
	blocked, err := f.engine.PlanReconcile(f.project.ID)
	if err != nil || blocked.Safe || len(blocked.Conflicts) != 1 || blocked.Conflicts[0].Reason != "recovery-required" {
		t.Fatalf("pending recovery did not block a new plan: %+v, %v", blocked, err)
	}
	messages, err := f.engine.Recover()
	if err != nil || len(messages) != 1 {
		t.Fatalf("recover: %v, %v", messages, err)
	}
	info, _ := os.Lstat(target)
	content, _ := os.ReadFile(target)
	if !info.Mode().IsRegular() || string(content) != "original" {
		t.Fatal("recovery did not restore original target")
	}
	recovered := filepath.Join(f.store.Root(), "recovery", "crash", "crash.txt")
	if data, err := os.ReadFile(recovered); err != nil || string(data) != "original" {
		t.Fatalf("uncertain source was not retained: %q, %v", data, err)
	}
	snapshot, err := f.engine.Snapshot()
	if err != nil || len(snapshot.Activity) < 2 || snapshot.Activity[0].Action != "recover" {
		t.Fatalf("recovery was not recorded in activity: %+v, %v", snapshot.Activity, err)
	}
}

func TestConcurrentAdoptWithSameRevisionHasOneWinner(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := filepath.Join(f.repo, "concurrent.txt")
	if err := os.WriteFile(target, []byte("one copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := f.engine.PlanAdopt(f.project.ID, "concurrent.txt")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		apply domain.ApplyResult
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			applied, err := f.engine.Adopt(f.project.ID, "concurrent.txt", plan.ExpectedRevision, plan.Fingerprint)
			results <- result{apply: applied, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	successes := 0
	stale := 0
	for _, item := range []result{first, second} {
		if item.err == nil && item.apply.Outcome == "completed" {
			successes++
		}
		if errors.Is(item.err, store.ErrStaleRevision) {
			stale++
		}
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("concurrent outcomes: first=%+v second=%+v", first, second)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "one copy" {
		t.Fatalf("concurrent adoption lost content: %q, %v", content, err)
	}
}

func TestAdoptRollbackNeverOverwritesExternalReplacement(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := filepath.Join(f.repo, "race.txt")
	backup := filepath.Join(f.repo, ".ewasd-race.backup")
	source, _ := f.engine.sourcePath(f.project, "race.txt")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("external replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := domain.Journal{ID: "race", Action: "adopt", Phase: "linked", ProjectID: f.project.ID, Path: "race.txt", Source: source, Target: target, Backup: backup}
	if err := f.engine.writeJournal(journal); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.rollbackAdopt(journal, true); err == nil {
		t.Fatal("expected conservative rollback to require manual recovery")
	}
	if data, _ := os.ReadFile(target); string(data) != "external replacement" {
		t.Fatalf("external target was overwritten: %q", data)
	}
	if data, _ := os.ReadFile(backup); string(data) != "original" {
		t.Fatalf("original backup was lost: %q", data)
	}
	if _, err := os.Stat(f.engine.journalPath("race")); err != nil {
		t.Fatalf("journal was removed despite uncertainty: %v", err)
	}
}

func TestDetachRollbackRetainsDivergedMaterializedTarget(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := filepath.Join(f.repo, "detached.txt")
	source, _ := f.engine.sourcePath(f.project, "detached.txt")
	archive := filepath.Join(f.store.Root(), "archive", "detach-race", f.project.ID, "detached.txt")
	if err := os.MkdirAll(filepath.Dir(archive), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, []byte("source before detach"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("edited after materialize"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := domain.Journal{ID: "detach-race", Action: "detach", Phase: "detached", ProjectID: f.project.ID, Path: "detached.txt", Source: source, Target: target, Archive: archive}
	if err := f.engine.writeJournal(journal); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.rollbackDetach(journal); err == nil {
		t.Fatal("expected diverged materialized target to require manual recovery")
	}
	if data, _ := os.ReadFile(target); string(data) != "edited after materialize" {
		t.Fatalf("edited target was overwritten: %q", data)
	}
	if data, _ := os.ReadFile(source); string(data) != "source before detach" {
		t.Fatalf("archived source was not restored safely: %q", data)
	}
	if _, err := os.Stat(f.engine.journalPath("detach-race")); err != nil {
		t.Fatalf("journal was removed despite uncertainty: %v", err)
	}
	archivedJournal, err := f.engine.DiscardJournal("detach-race")
	if err != nil {
		t.Fatalf("discard unresolved journal: %v", err)
	}
	if _, err := os.Stat(archivedJournal); err != nil {
		t.Fatalf("discarded journal was not archived: %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "edited after materialize" {
		t.Fatalf("discarding journal touched target content: %q", data)
	}
}

func TestRecoverRejectsTamperedJournalPaths(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.repo, "safe.txt")
	backup := filepath.Join(f.repo, ".ewasd-unsafe.backup")
	stage := filepath.Join(f.store.Root(), "transactions", "unsafe.stage")
	journal := domain.Journal{ID: "unsafe", Action: "adopt", Phase: "source-durable", ProjectID: f.project.ID, Path: "safe.txt", Source: outside, Target: target, Stage: stage, Backup: backup}
	if err := f.engine.writeJournal(journal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Recover(); err == nil {
		t.Fatal("tampered journal unexpectedly recovered")
	}
	if data, _ := os.ReadFile(outside); string(data) != "do not touch" {
		t.Fatalf("tampered journal changed outside data: %q", data)
	}
	if _, err := os.Stat(f.engine.journalPath("unsafe")); err != nil {
		t.Fatalf("unsafe journal should remain for manual inspection: %v", err)
	}
}

func TestLifecycleDoesNotTouchLegacyEwasdStateOrIgnoreFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	legacy := filepath.Join(filepath.Dir(f.store.Root()), "ewasd")
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyManifest := filepath.Join(legacy, "editors.toml")
	legacyIgnore := filepath.Join(f.repo, ".ewasd_gitignore")
	if err := os.WriteFile(legacyManifest, []byte("[repos]\n# legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyIgnore, []byte("# legacy ignore\nAGENT.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.repo, "parallel.txt")
	if err := os.WriteFile(target, []byte("parallel"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := f.engine.PlanAdopt(f.project.ID, "parallel.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Adopt(f.project.ID, "parallel.txt", plan.ExpectedRevision, plan.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(legacyManifest); string(data) != "[repos]\n# legacy\n" {
		t.Fatalf("legacy workspace changed: %q", data)
	}
	if data, _ := os.ReadFile(legacyIgnore); string(data) != "# legacy ignore\nAGENT.md\n" {
		t.Fatalf("legacy ignore changed: %q", data)
	}
}

func TestRecoverAdoptPrecommitPhases(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"copying", "source-durable", "target-backed-up"} {
		phase := phase
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t)
			target := filepath.Join(f.repo, "phase.txt")
			source, _ := f.engine.sourcePath(f.project, "phase.txt")
			id := "adopt-" + phase
			stage := filepath.Join(f.store.Root(), "transactions", id+".stage")
			backup := filepath.Join(f.repo, ".ewasd-"+id+".backup")
			if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			if phase == "copying" {
				if err := os.WriteFile(stage, []byte("partial"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(source, []byte("original"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "target-backed-up" {
				if err := os.Rename(target, backup); err != nil {
					t.Fatal(err)
				}
			}
			journal := domain.Journal{ID: id, Action: "adopt", Phase: phase, ProjectID: f.project.ID, Path: "phase.txt", Source: source, Target: target, Stage: stage, Backup: backup}
			if err := f.engine.writeJournal(journal); err != nil {
				t.Fatal(err)
			}
			if _, err := f.engine.Recover(); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(target); err != nil || string(data) != "original" {
				t.Fatalf("target not restored in %s: %q, %v", phase, data, err)
			}
			if _, err := os.Lstat(stage); !os.IsNotExist(err) {
				t.Fatalf("stage remains in %s: %v", phase, err)
			}
			if _, err := os.Lstat(f.engine.journalPath(id)); !os.IsNotExist(err) {
				t.Fatalf("journal remains in %s: %v", phase, err)
			}
		})
	}
}

func TestRecoverDetachPhasesWithoutLosingBytes(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"materializing", "materialized", "source-archived", "detached"} {
		phase := phase
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t)
			target := filepath.Join(f.repo, "phase.txt")
			if err := os.WriteFile(target, []byte("managed"), 0o600); err != nil {
				t.Fatal(err)
			}
			plan, _ := f.engine.PlanAdopt(f.project.ID, "phase.txt")
			if _, err := f.engine.Adopt(f.project.ID, "phase.txt", plan.ExpectedRevision, plan.Fingerprint); err != nil {
				t.Fatal(err)
			}
			source, _ := f.engine.sourcePath(f.project, "phase.txt")
			id := "detach-" + phase
			stage := filepath.Join(f.repo, ".ewasd-"+id+".materialize")
			archive := filepath.Join(f.store.Root(), "archive", id, f.project.ID, "phase.txt")
			if phase != "materializing" {
				if err := fsops.CopyTree(source, stage); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(stage, []byte("partial"), 0o600); err != nil {
				t.Fatal(err)
			}
			if phase == "source-archived" || phase == "detached" {
				if err := os.MkdirAll(filepath.Dir(archive), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(source, archive); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "detached" {
				if err := os.Rename(stage, target); err != nil {
					t.Fatal(err)
				}
			}
			journal := domain.Journal{ID: id, Action: "detach", Phase: phase, ProjectID: f.project.ID, Path: "phase.txt", Source: source, Target: target, Stage: stage, Archive: archive}
			if err := f.engine.writeJournal(journal); err != nil {
				t.Fatal(err)
			}
			if _, err := f.engine.Recover(); err != nil {
				t.Fatal(err)
			}
			if !fsops.LinkPointsTo(target, source) {
				t.Fatalf("target is not restored link in phase %s", phase)
			}
			if data, err := os.ReadFile(target); err != nil || string(data) != "managed" {
				t.Fatalf("content changed in phase %s: %q, %v", phase, data, err)
			}
		})
	}
}

func TestRecoverPreparedReconcileClearsJournalWithoutCreatingLink(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := filepath.Join(f.repo, "missing.txt")
	if err := os.WriteFile(target, []byte("managed"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, _ := f.engine.PlanAdopt(f.project.ID, "missing.txt")
	if _, err := f.engine.Adopt(f.project.ID, "missing.txt", plan.ExpectedRevision, plan.Fingerprint); err != nil {
		t.Fatal(err)
	}
	source, _ := f.engine.sourcePath(f.project, "missing.txt")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	journal := domain.Journal{ID: "reconcile-prepared", Action: "reconcile", Phase: "prepared", ProjectID: f.project.ID, Path: "missing.txt", Source: source, Target: target}
	if err := f.engine.writeJournal(journal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Recover(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("prepared reconcile recovery unexpectedly created target: %v", err)
	}
}

func TestRecoverPersistsEarlierOutcomeBeforeLaterJournalFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	blockedTarget := filepath.Join(f.repo, "blocked.md")
	if err := os.WriteFile(blockedTarget, []byte("managed"), 0o600); err != nil {
		t.Fatal(err)
	}
	blockedPlan, _ := f.engine.PlanAdopt(f.project.ID, "blocked.md")
	blockedResult, err := f.engine.Adopt(f.project.ID, "blocked.md", blockedPlan.ExpectedRevision, blockedPlan.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blockedTarget); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blockedTarget, []byte("unexpected local content"), 0o600); err != nil {
		t.Fatal(err)
	}
	blockedSource, _ := f.engine.sourcePath(f.project, "blocked.md")

	firstTarget := filepath.Join(f.repo, "one.md")
	firstSource, _ := f.engine.sourcePath(f.project, "one.md")
	if err := os.WriteFile(firstTarget, []byte("original one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firstSource, []byte("original one"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseTime := time.Now().UTC()
	first := domain.Journal{
		ID: "aaa", Action: "adopt", Phase: "source-durable", ProjectID: f.project.ID,
		Path: "one.md", Source: firstSource, Target: firstTarget,
		Stage:  filepath.Join(f.store.Root(), "transactions", "aaa.stage"),
		Backup: filepath.Join(f.repo, ".ewasd-aaa.backup"), CreatedAt: baseTime,
	}
	second := domain.Journal{
		ID: "zzz", Action: "reconcile", Phase: "prepared", ProjectID: f.project.ID,
		Path: "blocked.md", Source: blockedSource, Target: blockedTarget, CreatedAt: baseTime.Add(time.Second),
	}
	if err := f.engine.writeJournal(first); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.writeJournal(second); err != nil {
		t.Fatal(err)
	}
	messages, err := f.engine.Recover()
	if err == nil || len(messages) != 1 {
		t.Fatalf("expected one persisted recovery then one failure: %v, %v", messages, err)
	}
	snapshot, snapshotErr := f.engine.Snapshot()
	if snapshotErr != nil {
		t.Fatal(snapshotErr)
	}
	if snapshot.Revision != blockedResult.Revision+1 || snapshot.Activity[0].Action != "recover" {
		t.Fatalf("earlier recovery outcome was not persisted: revision=%d activity=%+v", snapshot.Revision, snapshot.Activity)
	}
	if len(snapshot.Recovery) != 1 || snapshot.Recovery[0].ID != "zzz" {
		t.Fatalf("unexpected remaining recovery journals: %+v", snapshot.Recovery)
	}
	if data, _ := os.ReadFile(firstTarget); string(data) != "original one" {
		t.Fatalf("first target changed: %q", data)
	}
}

func git(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
