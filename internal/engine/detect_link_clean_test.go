package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitrii-galantsev/ewasd/internal/domain"
	"github.com/dmitrii-galantsev/ewasd/internal/gitutil"
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

func TestDetectRegisteredRootFromNestedDirectory(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	nested := filepath.Join(f.repo, "src", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	detection, err := f.engine.Detect(nested, "")
	if err != nil || !detection.Matched || detection.Method != "registered-root" || detection.ProjectID != f.project.ID || detection.TargetRoot != f.repo {
		t.Fatalf("unexpected detection: %+v, %v", detection, err)
	}
}

func TestDetectUniqueRemoteAndLinkNewCheckout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := filepath.Join(f.repo, "AGENT.md")
	if err := os.WriteFile(target, []byte("shared rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	adopt, err := f.engine.PlanAdopt(f.project.ID, "AGENT.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Adopt(f.project.ID, "AGENT.md", adopt.ExpectedRevision, adopt.Fingerprint); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(filepath.Dir(f.repo), "clone")
	if err := os.Mkdir(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, clone, "init", "-q")
	git(t, clone, "remote", "add", "origin", "https://github.com/example/widget.git")
	detection, err := f.engine.Detect(clone, "")
	if err != nil || detection.Method != "remote" || detection.TemplateProjectID != f.project.ID || detection.TargetRoot != clone {
		t.Fatalf("unexpected remote detection: %+v, %v", detection, err)
	}
	plan, err := f.engine.PlanLink(clone, "")
	if err != nil || !plan.NewProject || !plan.Safe {
		t.Fatalf("link plan: %+v, %v", plan, err)
	}
	before, err := f.engine.Snapshot()
	if err != nil || len(before.Projects) != 1 {
		t.Fatalf("link preview mutated state: %+v, %v", before, err)
	}
	if _, err := os.Lstat(filepath.Join(clone, "AGENT.md")); !os.IsNotExist(err) {
		t.Fatalf("link preview created target: %v", err)
	}
	result, err := f.engine.Link(clone, "", plan.Fingerprint)
	if err != nil || result.Outcome != "completed" {
		t.Fatalf("link: %+v, %v", result, err)
	}
	snapshot, err := f.engine.Snapshot()
	if err != nil || len(snapshot.Projects) != 2 {
		t.Fatalf("snapshot: %+v, %v", snapshot, err)
	}
	var linkedSource string
	for _, project := range snapshot.Projects {
		if project.Root == clone {
			linkedSource = project.EntriesView[0].Source
			if project.SourceID != f.project.SourceID || project.Health.Linked != 1 {
				t.Fatalf("new checkout did not share source: %+v", project)
			}
		}
	}
	if !filepath.IsAbs(linkedSource) || !strings.Contains(linkedSource, f.project.SourceID) {
		t.Fatalf("unexpected linked source: %s", linkedSource)
	}
}

func TestLinkLeavesConflictUntouchedAndLinksSafeEntries(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, name := range []string{"conflict.md", "safe.md"} {
		if err := os.WriteFile(filepath.Join(f.repo, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		plan, _ := f.engine.PlanAdopt(f.project.ID, name)
		if _, err := f.engine.Adopt(f.project.ID, name, plan.ExpectedRevision, plan.Fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	clone := filepath.Join(filepath.Dir(f.repo), "conflicted-clone")
	if err := os.Mkdir(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, clone, "init", "-q")
	git(t, clone, "remote", "add", "origin", "git@github.com:example/widget.git")
	if err := os.WriteFile(filepath.Join(clone, "conflict.md"), []byte("keep local"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := f.engine.PlanLink(clone, "")
	if err != nil || len(plan.Conflicts) != 1 || !plan.Safe {
		t.Fatalf("unexpected conflict plan: %+v, %v", plan, err)
	}
	result, err := f.engine.Link(clone, "", plan.Fingerprint)
	if err != nil || result.Outcome != "completed_with_skips" || len(result.Skipped) != 1 {
		t.Fatalf("link result: %+v, %v", result, err)
	}
	if data, _ := os.ReadFile(filepath.Join(clone, "conflict.md")); string(data) != "keep local" {
		t.Fatalf("conflict was overwritten: %q", data)
	}
	if info, err := os.Lstat(filepath.Join(clone, "safe.md")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("safe entry was not linked: %v, %v", info, err)
	}
}

func TestLinkRejectsFilesystemDriftAfterPreview(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.repo, "AGENT.md"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	adopt, _ := f.engine.PlanAdopt(f.project.ID, "AGENT.md")
	if _, err := f.engine.Adopt(f.project.ID, "AGENT.md", adopt.ExpectedRevision, adopt.Fingerprint); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(filepath.Dir(f.repo), "drift-clone")
	if err := os.Mkdir(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, clone, "init", "-q")
	git(t, clone, "remote", "add", "origin", "https://github.com/example/widget.git")
	plan, err := f.engine.PlanLink(clone, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "AGENT.md"), []byte("appeared"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Link(clone, "", plan.Fingerprint); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("filesystem drift did not stale link plan: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(clone, "AGENT.md")); string(data) != "appeared" {
		t.Fatalf("drifted file changed: %q", data)
	}
}

func TestLinkReturnsErrorWhenPostCommitGitProtectionFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.repo, "AGENT.md"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	adopt, _ := f.engine.PlanAdopt(f.project.ID, "AGENT.md")
	if _, err := f.engine.Adopt(f.project.ID, "AGENT.md", adopt.ExpectedRevision, adopt.Fingerprint); err != nil {
		t.Fatal(err)
	}
	exclude := filepath.Join(f.repo, ".git", "info", "exclude")
	if err := os.WriteFile(exclude, []byte("# >>> ewasd "+f.project.ID+"\n/no-end-marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := f.engine.PlanLink(f.repo, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.engine.Link(f.repo, "", plan.Fingerprint)
	if !errors.Is(err, store.ErrCommitIncomplete) {
		t.Fatalf("post-commit failure was reported as success: %v", err)
	}
	snapshot, readErr := f.engine.Snapshot()
	if readErr != nil || len(snapshot.Projects) != 1 {
		t.Fatalf("committed binding is not inspectable after failure: %+v, %v", snapshot, readErr)
	}
}

func TestDetectMonorepoScopeByRemoteAndPathFallback(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateStore, err := store.New(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	e := New(stateStore)
	first := filepath.Join(root, "mono-one")
	firstScope := filepath.Join(first, "projects", "rdc")
	if err := os.MkdirAll(firstScope, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, first, "init", "-q")
	git(t, first, "remote", "add", "origin", "https://github.com/example/mono.git")
	project, _, err := e.Register(firstScope, "rdc")
	if err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(root, "mono-two")
	secondScope := filepath.Join(second, "projects", "rdc")
	if err := os.MkdirAll(secondScope, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, second, "init", "-q")
	git(t, second, "remote", "add", "origin", "git@github.com:example/mono.git")
	detected, err := e.Detect(secondScope, "")
	if err != nil || detected.Method != "remote" || detected.TargetRoot != secondScope || detected.TemplateProjectID != project.ID {
		t.Fatalf("monorepo remote detection: %+v, %v", detected, err)
	}
	third := filepath.Join(root, "unrelated", "projects", "rdc")
	if err := os.MkdirAll(third, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Join(root, "unrelated"), "init", "-q")
	git(t, filepath.Join(root, "unrelated"), "remote", "add", "origin", "https://github.com/other/repo.git")
	detected, err = e.Detect(third, "")
	if err != nil || detected.Method != "path" || detected.TargetRoot != third || detected.TemplateProjectID != project.ID {
		t.Fatalf("path fallback detection: %+v, %v", detected, err)
	}
	pathPlan, err := e.PlanLink(third, "")
	if err != nil || pathPlan.Safe || len(pathPlan.Conflicts) != 1 {
		t.Fatalf("path-only link should require confirmation: %+v, %v", pathPlan, err)
	}
	explicitPlan, err := e.PlanLink(third, project.ID)
	if err != nil || !explicitPlan.Safe || !explicitPlan.NewProject {
		t.Fatalf("explicit path confirmation failed: %+v, %v", explicitPlan, err)
	}
}

func TestDetectAmbiguousRemoteRequiresExplicitProject(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateStore, _ := store.New(filepath.Join(root, "state"))
	e := New(stateStore)
	for _, name := range []string{"one", "two"} {
		repository := filepath.Join(root, name)
		if err := os.Mkdir(repository, 0o755); err != nil {
			t.Fatal(err)
		}
		git(t, repository, "init", "-q")
		git(t, repository, "remote", "add", "origin", "https://github.com/example/shared.git")
		if _, _, err := e.Register(repository, name); err != nil {
			t.Fatal(err)
		}
	}
	third := filepath.Join(root, "third")
	if err := os.Mkdir(third, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, third, "init", "-q")
	git(t, third, "remote", "add", "origin", "git@github.com:example/shared.git")
	detection, err := e.Detect(third, "")
	if !errors.Is(err, ErrAmbiguousDetection) || detection.Matched || len(detection.Candidates) != 2 {
		t.Fatalf("ambiguous remote was not rejected: %+v, %v", detection, err)
	}
	explicit, err := e.Detect(third, "one")
	if err != nil || explicit.Method != "explicit" || explicit.SourceID == "" {
		t.Fatalf("explicit override failed: %+v, %v", explicit, err)
	}
}

func TestCleanExplicitProjectCannotRetargetAnotherRegisteredRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateStore, _ := store.New(filepath.Join(root, "state"))
	e := New(stateStore)
	mono := filepath.Join(root, "mono")
	alpha := filepath.Join(mono, "alpha")
	beta := filepath.Join(mono, "beta")
	if err := os.MkdirAll(alpha, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(beta, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, mono, "init", "-q")
	alphaProject, _, err := e.Register(alpha, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	betaProject, _, err := e.Register(beta, "beta")
	if err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(beta, "CLAUDE.md")
	if err := os.WriteFile(managed, []byte("beta"), 0o600); err != nil {
		t.Fatal(err)
	}
	adopt, _ := e.PlanAdopt(betaProject.ID, "CLAUDE.md")
	if _, err := e.Adopt(betaProject.ID, "CLAUDE.md", adopt.ExpectedRevision, adopt.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlanClean(beta, alphaProject.ID, CleanOptions{Mode: "untracked"}); !errors.Is(err, ErrAmbiguousDetection) {
		t.Fatalf("clean retargeted a different registered project: %v", err)
	}
	if info, err := os.Lstat(managed); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("managed beta link changed: %v, %v", info, err)
	}
}

func TestCleanRequiresHealthyGitProtection(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	managed := filepath.Join(f.repo, "AGENT.md")
	if err := os.WriteFile(managed, []byte("managed"), 0o600); err != nil {
		t.Fatal(err)
	}
	adopt, _ := f.engine.PlanAdopt(f.project.ID, "AGENT.md")
	if _, err := f.engine.Adopt(f.project.ID, "AGENT.md", adopt.ExpectedRevision, adopt.Fingerprint); err != nil {
		t.Fatal(err)
	}
	exclude := filepath.Join(f.repo, ".git", "info", "exclude")
	if err := os.WriteFile(exclude, []byte("# block deliberately removed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.PlanClean(f.repo, "", CleanOptions{Mode: "all", IncludeDirectories: true}); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "run ewasd link") {
		t.Fatalf("clean accepted missing protection block: %v", err)
	}
	if info, err := os.Lstat(managed); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("managed link changed: %v, %v", info, err)
	}
}

func TestCleanAllProtectsManagedLinksAndSkipsNestedRepositories(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	managed := filepath.Join(f.repo, "AGENT.md")
	if err := os.WriteFile(managed, []byte("managed"), 0o600); err != nil {
		t.Fatal(err)
	}
	adopt, _ := f.engine.PlanAdopt(f.project.ID, "AGENT.md")
	if _, err := f.engine.Adopt(f.project.ID, "AGENT.md", adopt.ExpectedRevision, adopt.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, ".gitignore"), []byte("ignored.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gitutil.UpdateLegacyResidual("fixture", f.repo, []string{"future.local"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "future.local"), []byte("residual"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, f.repo, "add", ".gitignore")
	git(t, f.repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "ignore")
	for _, path := range []string{"junk.txt", "ignored.log", "dir/junk"} {
		full := filepath.Join(f.repo, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nested := filepath.Join(f.repo, "nested-repository")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, nested, "init", "-q")
	if err := os.WriteFile(filepath.Join(nested, "keep"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := CleanOptions{Mode: "all", IncludeDirectories: true}
	plan, err := f.engine.PlanClean(f.repo, "", options)
	if err != nil || len(plan.Candidates) < 3 || len(plan.HealthyPaths) != 1 {
		t.Fatalf("clean plan: %+v, %v", plan, err)
	}
	if containsString(plan.Candidates, "future.local") {
		t.Fatal("legacy residual path was not protected")
	}
	result, err := f.engine.Clean(f.repo, "", options, plan.ExpectedRevision, plan.Fingerprint)
	if err != nil || result.Outcome != "completed" {
		t.Fatalf("clean: %+v, %v", result, err)
	}
	for _, path := range []string{"junk.txt", "ignored.log", "dir"} {
		if _, err := os.Lstat(filepath.Join(f.repo, path)); !os.IsNotExist(err) {
			t.Fatalf("clean candidate remains %s: %v", path, err)
		}
	}
	if info, err := os.Lstat(managed); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("managed link was removed: %v, %v", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(nested, "keep")); err != nil || string(data) != "nested" {
		t.Fatalf("nested repository changed: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(f.repo, "future.local")); err != nil || string(data) != "residual" {
		t.Fatalf("legacy residual path changed: %q, %v", data, err)
	}
	snapshot, err := f.engine.Snapshot()
	if err != nil || snapshot.Activity[0].Action != "clean" || snapshot.Revision != result.Revision {
		t.Fatalf("clean activity was not recorded: %+v, %v", snapshot.Activity, err)
	}
	if _, err := os.Stat(filepath.Join(f.store.Root(), "clean-records", result.OperationID+".json")); err != nil {
		t.Fatalf("clean audit record missing: %v", err)
	}
}

func TestCleanModesAndScope(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateStore, _ := store.New(filepath.Join(root, "state"))
	e := New(stateStore)
	mono := filepath.Join(root, "mono")
	scope := filepath.Join(mono, "project")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, mono, "init", "-q")
	if err := os.WriteFile(filepath.Join(mono, ".gitignore"), []byte("*.ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, mono, "add", ".gitignore")
	git(t, mono, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "ignore")
	if _, _, err := e.Register(scope, "project"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(mono, "outside.txt")
	untracked := filepath.Join(scope, "untracked.txt")
	ignored := filepath.Join(scope, "cache.ignored")
	for _, file := range []string{outside, untracked, ignored} {
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	untrackedPlan, err := e.PlanClean(scope, "", CleanOptions{Mode: "untracked", IncludeDirectories: true})
	if err != nil || containsString(untrackedPlan.Candidates, "cache.ignored") || !containsString(untrackedPlan.Candidates, "untracked.txt") {
		t.Fatalf("untracked plan: %+v, %v", untrackedPlan, err)
	}
	if _, err := e.Clean(scope, "", CleanOptions{Mode: "untracked", IncludeDirectories: true}, untrackedPlan.ExpectedRevision, untrackedPlan.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(untracked); !os.IsNotExist(err) {
		t.Fatalf("untracked file remains: %v", err)
	}
	if _, err := os.Stat(ignored); err != nil {
		t.Fatalf("ignored file was removed in untracked mode: %v", err)
	}
	ignoredPlan, err := e.PlanClean(scope, "", CleanOptions{Mode: "ignored", IncludeDirectories: true})
	if err != nil || !(containsString(ignoredPlan.Candidates, "cache.ignored") || containsString(ignoredPlan.Candidates, "./cache.ignored") || containsString(ignoredPlan.Candidates, "./")) {
		t.Fatalf("ignored plan: %+v, %v", ignoredPlan, err)
	}
	if _, err := e.Clean(scope, "", CleanOptions{Mode: "ignored", IncludeDirectories: true}, ignoredPlan.ExpectedRevision, ignoredPlan.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ignored); !os.IsNotExist(err) {
		t.Fatalf("ignored file remains: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("clean escaped registered scope: %v", err)
	}
}

func TestIgnoredOnlyCleanNeverDeletesIgnoredManagedEntries(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.repo, ".gitignore"), []byte("AGENT.md\n.claude/\n*.ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, f.repo, "add", ".gitignore")
	git(t, f.repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "ignore")
	for _, path := range []string{"AGENT.md", ".claude/settings.json"} {
		full := filepath.Join(f.repo, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("managed"), 0o600); err != nil {
			t.Fatal(err)
		}
		plan, _ := f.engine.PlanAdopt(f.project.ID, path)
		if _, err := f.engine.Adopt(f.project.ID, path, plan.ExpectedRevision, plan.Fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	ignoredJunk := filepath.Join(f.repo, "junk.ignored")
	if err := os.WriteFile(ignoredJunk, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := CleanOptions{Mode: "ignored", IncludeDirectories: true}
	plan, err := f.engine.PlanClean(f.repo, "", options)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range plan.Candidates {
		if strings.Contains(candidate, "AGENT.md") || strings.Contains(candidate, ".claude") {
			t.Fatalf("managed ignored entry appears in removal plan: %+v", plan)
		}
	}
	if _, err := f.engine.Clean(f.repo, "", options, plan.ExpectedRevision, plan.Fingerprint); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"AGENT.md", ".claude/settings.json"} {
		if info, err := os.Lstat(filepath.Join(f.repo, filepath.FromSlash(path))); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("managed ignored entry was removed: %s, %v, %v", path, info, err)
		}
	}
	if _, err := os.Stat(ignoredJunk); !os.IsNotExist(err) {
		t.Fatalf("ignored junk remains: %v", err)
	}
}

func TestCleanFromNestedDirectoryUsesNarrowerCurrentScope(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	nested := filepath.Join(f.repo, "src", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(nested, "inside.tmp")
	sibling := filepath.Join(f.repo, "sibling.tmp")
	for _, file := range []string{inside, sibling} {
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	options := CleanOptions{Mode: "all", IncludeDirectories: true}
	plan, err := f.engine.PlanClean(nested, "", options)
	if err != nil || plan.Root != nested || !(containsString(plan.Candidates, "inside.tmp") || containsString(plan.Candidates, "./inside.tmp")) || containsString(plan.Candidates, "sibling.tmp") {
		t.Fatalf("nested clean plan: %+v, %v", plan, err)
	}
	if _, err := f.engine.Clean(nested, "", options, plan.ExpectedRevision, plan.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatalf("nested candidate remains: %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("sibling outside current cwd was removed: %v", err)
	}
}

func TestCleanRejectsCandidateDriftAfterPreview(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	first := filepath.Join(f.repo, "first.tmp")
	second := filepath.Join(f.repo, "second.tmp")
	if err := os.WriteFile(first, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := CleanOptions{Mode: "all", IncludeDirectories: true}
	plan, err := f.engine.PlanClean(f.repo, "", options)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Clean(f.repo, "", options, plan.ExpectedRevision, plan.Fingerprint); err == nil || !strings.Contains(err.Error(), "plan changed") {
		t.Fatalf("candidate drift was not rejected: %v", err)
	}
	for _, file := range []string{first, second} {
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("stale clean removed %s: %v", file, err)
		}
	}
}

func TestCleanFilesOnlyDoesNotTraverseUntrackedDirectories(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	file := filepath.Join(f.repo, "root.tmp")
	directoryFile := filepath.Join(f.repo, "untracked-dir", "keep.tmp")
	if err := os.WriteFile(file, []byte("remove"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(directoryFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directoryFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := CleanOptions{Mode: "untracked", IncludeDirectories: false}
	plan, err := f.engine.PlanClean(f.repo, "", options)
	if err != nil || !containsString(plan.Candidates, "root.tmp") || containsString(plan.Candidates, "untracked-dir/") {
		t.Fatalf("files-only plan: %+v, %v", plan, err)
	}
	if _, err := f.engine.Clean(f.repo, "", options, plan.ExpectedRevision, plan.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("root file remains: %v", err)
	}
	if data, err := os.ReadFile(directoryFile); err != nil || string(data) != "keep" {
		t.Fatalf("untracked directory was traversed: %q, %v", data, err)
	}
}

func TestCleanIsBlockedWhileRecoveryJournalExists(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	backup := filepath.Join(f.repo, ".ewasd-pending.backup")
	if err := os.WriteFile(backup, []byte("only copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := domain.Journal{ID: "pending", Action: "adopt", Phase: "target-backed-up", ProjectID: f.project.ID, Path: "pending.md", Source: filepath.Join(f.store.Root(), "profiles", f.project.SourceID, "files", "pending.md"), Target: filepath.Join(f.repo, "pending.md"), Stage: filepath.Join(f.store.Root(), "transactions", "pending.stage"), Backup: backup}
	if err := f.engine.writeJournal(journal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.PlanClean(f.repo, "", CleanOptions{Mode: "all", IncludeDirectories: true}); !errors.Is(err, ErrRecoveryPending) {
		t.Fatalf("clean was not blocked by recovery: %v", err)
	}
	if data, err := os.ReadFile(backup); err != nil || string(data) != "only copy" {
		t.Fatalf("recovery backup changed: %q, %v", data, err)
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
