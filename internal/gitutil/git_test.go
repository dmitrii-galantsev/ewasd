package gitutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeRemotePreservesHostOwnerAndRepo(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"git@github.com:Owner/Project.git":      "github.com/owner/project",
		"https://GitHub.com/Owner/Project.git/": "github.com/owner/project",
		"ssh://git@gitlab.example/Team/Repo":    "gitlab.example/team/repo",
	}
	for input, want := range cases {
		if got := NormalizeRemote(input); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestUpdateExcludePreservesUserContentAndReplacesOnlyOwnBlock(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	exclude := filepath.Join(repo, ".git", "info", "exclude")
	original := "# user content\n*.scratch\n"
	if err := os.WriteFile(exclude, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := UpdateExclude("project-1", repo, repo, []string{"AGENT.md", "src/info.md"}); err != nil {
		t.Fatal(err)
	}
	if ok, detail := CheckExclude("project-1", repo, repo, []string{"AGENT.md", "src/info.md"}); !ok {
		t.Fatalf("fresh exclude block reported drift: %s", detail)
	}
	data, _ := os.ReadFile(exclude)
	content := string(data)
	for _, expected := range []string{"# user content", "*.scratch", "# >>> ewasd project-1", "/AGENT.md", "/src/info.md"} {
		if !strings.Contains(content, expected) {
			t.Fatalf("exclude missing %q:\n%s", expected, content)
		}
	}
	if err := UpdateExclude("project-1", repo, repo, []string{"new.md"}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(exclude)
	content = string(data)
	if strings.Contains(content, "/AGENT.md") || strings.Count(content, "# >>> ewasd project-1") != 1 {
		t.Fatalf("old block was not replaced exactly:\n%s", content)
	}
	if ok, _ := CheckExclude("project-1", repo, repo, []string{"different.md"}); ok {
		t.Fatal("drifted expected entries were reported healthy")
	}
	if err := UpdateExclude("project-1", repo, repo, nil); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(exclude)
	content = string(data)
	if content != original {
		t.Fatalf("user content changed: got %q want %q", content, original)
	}
}

func TestUpdateExcludeRefusesMalformedMarkerWithoutChangingFile(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	exclude := filepath.Join(repo, ".git", "info", "exclude")
	original := "# user content\n# >>> ewasd project-1\n/old\nimportant-after-marker\n"
	if err := os.WriteFile(exclude, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := UpdateExclude("project-1", repo, repo, []string{"new"}); err == nil {
		t.Fatal("malformed marker should be refused")
	}
	data, err := os.ReadFile(exclude)
	if err != nil || string(data) != original {
		t.Fatalf("malformed update changed user content: %q, %v", data, err)
	}
}

func TestUpdateExcludeEscapesMetacharactersWithoutHidingNeighbors(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	managed := "report[1]*?.md"
	neighbor := "report1xx.md"
	if err := os.WriteFile(filepath.Join(repo, managed), []byte("managed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, neighbor), []byte("neighbor"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateExclude("project-1", repo, repo, []string{managed}); err != nil {
		t.Fatal(err)
	}
	if ok, detail := CheckExclude("project-1", repo, repo, []string{managed}); !ok {
		t.Fatalf("escaped block reported drift: %s", detail)
	}
	managedCheck := exec.Command("git", "check-ignore", "--no-index", "-q", "--", managed)
	managedCheck.Dir = repo
	if err := managedCheck.Run(); err != nil {
		t.Fatalf("managed metacharacter path was not ignored: %v", err)
	}
	neighborCheck := exec.Command("git", "check-ignore", "--no-index", "-q", "--", neighbor)
	neighborCheck.Dir = repo
	if err := neighborCheck.Run(); err == nil {
		t.Fatal("escaped pattern hid an unrelated neighboring path")
	}
}

func TestUpdateExcludePreservesModeAndTrailingBytes(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	exclude := filepath.Join(repo, ".git", "info", "exclude")
	original := "# user\n\n*.scratch\n\n\n"
	if err := os.WriteFile(exclude, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(exclude, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := UpdateExclude("project-1", repo, repo, []string{"one"}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateExclude("project-1", repo, repo, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exclude)
	if err != nil || string(data) != original {
		t.Fatalf("exclude bytes changed: %q, %v", data, err)
	}
	info, err := os.Stat(exclude)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("exclude mode changed: %v, %v", info.Mode().Perm(), err)
	}
}

func TestUpdateExcludeDoesNotTreatUserSeparatorTextAsMetadata(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	exclude := filepath.Join(repo, ".git", "info", "exclude")
	original := "# ewasd-separator: inserted\n*.scratch\n"
	if err := os.WriteFile(exclude, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := UpdateExclude("project-1", repo, repo, []string{"one"}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateExclude("project-1", repo, repo, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exclude)
	if err != nil || string(data) != original {
		t.Fatalf("user sentinel text changed byte preservation: %q, %v", data, err)
	}
}

func TestRemovingFirstOfTwoBlocksPreservesBoundaryAndOriginalBytes(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	exclude := filepath.Join(repo, ".git", "info", "exclude")
	original := "*.user-secret-pattern"
	if err := os.WriteFile(exclude, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := UpdateExclude("alpha", repo, repo, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateExclude("beta", repo, repo, []string{"beta"}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateExclude("alpha", repo, repo, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "*.user-secret-pattern\n# >>> ewasd beta") {
		t.Fatalf("user pattern was glued to next block: %q", data)
	}
	if ok, detail := CheckExclude("beta", repo, repo, []string{"beta"}); !ok {
		t.Fatalf("second block was orphaned: %s", detail)
	}
	if err := UpdateExclude("beta", repo, repo, nil); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(exclude)
	if err != nil || string(data) != original {
		t.Fatalf("original bytes did not round trip: %q, %v", data, err)
	}
}

func TestInspectCheckoutDefaultsToOriginWhenNoKeysGiven(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	runGitCommand(t, repo, "remote", "add", "origin", "git@github.com:Example/Widget.git")
	checkout, err := InspectCheckout(repo)
	if err != nil {
		t.Fatal(err)
	}
	if checkout.Remote != "github.com/example/widget" {
		t.Fatalf("Remote = %q", checkout.Remote)
	}
}

func TestInspectCheckoutTriesConfiguredKeysInOrderFirstNonEmptyWins(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	// No "remote.origin.url" configured at all; only upstream is set, so
	// the second configured key should be the one that wins.
	runGitCommand(t, repo, "remote", "add", "upstream", "git@github.com:Example/Upstream.git")
	checkout, err := InspectCheckout(repo, "remote.origin.url", "remote.upstream.url")
	if err != nil {
		t.Fatal(err)
	}
	if checkout.Remote != "github.com/example/upstream" {
		t.Fatalf("Remote = %q, want upstream to win when origin is unset", checkout.Remote)
	}
}

func TestInspectCheckoutPrefersEarlierConfiguredKeyWhenBothPresent(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	runGitCommand(t, repo, "remote", "add", "origin", "git@github.com:Example/Origin.git")
	runGitCommand(t, repo, "remote", "add", "upstream", "git@github.com:Example/Upstream.git")
	checkout, err := InspectCheckout(repo, "remote.origin.url", "remote.upstream.url")
	if err != nil {
		t.Fatal(err)
	}
	if checkout.Remote != "github.com/example/origin" {
		t.Fatalf("Remote = %q, want the first configured key (origin) to win", checkout.Remote)
	}
}

func TestInspectCheckoutRemoteEmptyWhenNoConfiguredKeyIsSet(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	checkout, err := InspectCheckout(repo, "remote.origin.url")
	if err != nil {
		t.Fatal(err)
	}
	if checkout.Remote != "" {
		t.Fatalf("Remote = %q, want empty", checkout.Remote)
	}
}

func TestCloneRepositoryClonesLocalRepository(t *testing.T) {
	t.Parallel()
	source := initRepo(t)
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, source, "add", "README.md")
	runGitCommand(t, source, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "init")

	dest := filepath.Join(t.TempDir(), "clone")
	if err := CloneRepository("file://"+source, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("cloned repo missing expected file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		t.Fatalf("cloned repo missing .git: %v", err)
	}
}

func TestCloneRepositoryFailureReturnsDescriptiveError(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "clone")
	err := CloneRepository(filepath.Join(t.TempDir(), "does-not-exist"), dest)
	if err == nil {
		t.Fatal("expected an error cloning a nonexistent source")
	}
}

func TestCollectRemotesNormalizesAndDeduplicatesAllConfiguredRemotes(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	runGitCommand(t, repo, "remote", "add", "origin", "git@github.com:Example/Widget.git")
	runGitCommand(t, repo, "remote", "add", "mirror", "https://github.com/example/widget/")
	runGitCommand(t, repo, "remote", "add", "upstream", "https://gitlab.example/Team/Other.git")
	remotes := CollectRemotes(repo)
	want := []string{"github.com/example/widget", "gitlab.example/team/other"}
	if strings.Join(remotes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("remotes = %v, want %v", remotes, want)
	}
}

func TestCleanPreviewDecodesQuotedPathsAndProtectsExactPattern(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	for _, name := range []string{"a file", "line\nbreak", "keep[1].txt"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	preview, err := PreviewClean(repo, CleanOptions{Mode: "all", IncludeDirectories: true, ProtectedPatterns: []string{"/keep\\[1\\].txt"}, Scope: "."})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(preview.Candidates, "a file") || !contains(preview.Candidates, "line\nbreak") || contains(preview.Candidates, "keep[1].txt") {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	if _, err := ApplyClean(repo, CleanOptions{Mode: "all", IncludeDirectories: true, ProtectedPatterns: []string{"/keep\\[1\\].txt"}, Scope: "."}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "keep[1].txt")); err != nil {
		t.Fatalf("protected path was removed: %v", err)
	}
}

func runGitCommand(t *testing.T, cwd string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = cwd
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func initRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return repo
}
