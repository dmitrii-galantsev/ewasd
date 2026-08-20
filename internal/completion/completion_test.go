package completion

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitrii-galantsev/ewasd/internal/engine"
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

// fixture mirrors the setup pattern in internal/engine/engine_test.go: a
// temp-dir Git checkout registered with a fresh store-backed engine, with
// one file adopted (so completion has a managed path to find) and a couple
// of ordinary files left unmanaged (so completion has adopt candidates to
// find).
type fixture struct {
	engine    *engine.Engine
	repo      string
	projectID string
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
	e := engine.New(s)
	project, _, err := e.Register(repo, "Widget")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repo, "AGENT.md"), []byte("guardrails\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := e.PlanAdopt(project.ID, "AGENT.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Adopt(project.ID, "AGENT.md", plan.ExpectedRevision, plan.Fingerprint); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	return fixture{engine: e, repo: repo, projectID: project.ID}
}

func git(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func runCompleteLines(eng *engine.Engine, args []string) []string {
	var buf bytes.Buffer
	RunComplete(eng, args, &buf)
	text := strings.TrimRight(buf.String(), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func containsAll(haystack []string, wanted ...string) bool {
	set := make(map[string]bool, len(haystack))
	for _, v := range haystack {
		set[v] = true
	}
	for _, w := range wanted {
		if !set[w] {
			return false
		}
	}
	return true
}

// --- script generation ---

func TestGeneratedScriptsNonEmptyReferenceCompleteAndVerbs(t *testing.T) {
	for _, shell := range Shells {
		shell := shell
		t.Run(shell, func(t *testing.T) {
			script, err := Script(shell)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(script) == "" {
				t.Fatal("generated script is empty")
			}
			if !strings.Contains(script, "__complete") {
				t.Error("generated script does not reference __complete")
			}
			for _, verb := range Verbs {
				if !strings.Contains(script, verb) {
					t.Errorf("generated script does not mention verb %q", verb)
				}
			}
		})
	}
}

func TestScriptUnknownShellIsError(t *testing.T) {
	if _, err := Script("powershell"); err == nil {
		t.Fatal("expected an error for an unsupported shell")
	}
}

// --- syntax checking against the real shells, when available ---

func TestGeneratedScriptSyntax(t *testing.T) {
	cases := []struct {
		shell  string
		binary string
	}{
		{"bash", "bash"},
		{"zsh", "zsh"},
		{"fish", "fish"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.shell, func(t *testing.T) {
			binPath, err := exec.LookPath(tc.binary)
			if err != nil {
				t.Skipf("%s not found on PATH: %v", tc.binary, err)
			}
			script, err := Script(tc.shell)
			if err != nil {
				t.Fatal(err)
			}
			var cmd *exec.Cmd
			switch tc.shell {
			case "fish":
				path := filepath.Join(t.TempDir(), "ewasd.fish")
				if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
					t.Fatal(err)
				}
				cmd = exec.Command(binPath, "--no-execute", path)
			default:
				cmd = exec.Command(binPath, "-n", "-")
				cmd.Stdin = strings.NewReader(script)
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s syntax check failed: %v\n%s", tc.shell, err, out)
			}
		})
	}
}

// --- --install ---

func fakeEnv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestInstallWritesExpectedPathsAndPermsAndIsIdempotent(t *testing.T) {
	cases := []struct {
		shell        string
		expectSuffix string
		expectHint   bool
	}{
		{"bash", filepath.Join(".local", "share", "bash-completion", "completions", "ewasd"), false},
		{"zsh", filepath.Join(".local", "share", "zsh", "site-functions", "_ewasd"), true},
		{"fish", filepath.Join(".config", "fish", "completions", "ewasd.fish"), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.shell, func(t *testing.T) {
			home := t.TempDir()
			getenv := fakeEnv(map[string]string{"HOME": home})
			script, err := Script(tc.shell)
			if err != nil {
				t.Fatal(err)
			}
			wantPath := filepath.Join(home, tc.expectSuffix)

			path, hint, err := Install(tc.shell, getenv, script)
			if err != nil {
				t.Fatal(err)
			}
			if path != wantPath {
				t.Fatalf("path = %q, want %q", path, wantPath)
			}
			if tc.expectHint && hint == "" {
				t.Error("expected an activation hint, got none")
			}
			if !tc.expectHint && hint != "" {
				t.Errorf("expected no activation hint, got %q", hint)
			}

			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != 0o644 {
				t.Errorf("file perm = %o, want 0644", perm)
			}
			dirInfo, err := os.Stat(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			if perm := dirInfo.Mode().Perm(); perm != 0o755 {
				t.Errorf("dir perm = %o, want 0755", perm)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != script {
				t.Error("written content does not match generated script")
			}

			// Re-running must be idempotent: same path, same content, no error.
			path2, _, err := Install(tc.shell, getenv, script)
			if err != nil {
				t.Fatal(err)
			}
			if path2 != path {
				t.Fatalf("second install path = %q, want %q", path2, path)
			}
			data2, err := os.ReadFile(path2)
			if err != nil {
				t.Fatal(err)
			}
			if string(data2) != script {
				t.Error("second install changed the written content")
			}
		})
	}
}

func TestInstallHonoursXDGOverrides(t *testing.T) {
	home := t.TempDir()
	xdgData := t.TempDir()
	xdgConfig := t.TempDir()
	getenv := fakeEnv(map[string]string{
		"HOME":            home,
		"XDG_DATA_HOME":   xdgData,
		"XDG_CONFIG_HOME": xdgConfig,
	})
	script := "# test\n"

	bashPath, _, err := Install("bash", getenv, script)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(xdgData, "bash-completion", "completions", "ewasd"); bashPath != want {
		t.Fatalf("bash path = %q, want %q", bashPath, want)
	}

	fishPath, _, err := Install("fish", getenv, script)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(xdgConfig, "fish", "completions", "ewasd.fish"); fishPath != want {
		t.Fatalf("fish path = %q, want %q", fishPath, want)
	}
}

// --- shell detection ---

func TestDetectShell(t *testing.T) {
	cases := []struct {
		shellEnv string
		want     string
		wantErr  bool
	}{
		{"/bin/bash", "bash", false},
		{"/usr/bin/zsh", "zsh", false},
		{"/opt/homebrew/bin/fish", "fish", false},
		{"/bin/tcsh", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.shellEnv, func(t *testing.T) {
			got, err := DetectShell(fakeEnv(map[string]string{"SHELL": tc.shellEnv}))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for SHELL=%q", tc.shellEnv)
				}
				for _, shell := range Shells {
					if !strings.Contains(err.Error(), shell) {
						t.Errorf("error %q does not mention valid choice %q", err, shell)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- ewasd completion Run() ---

func TestRunPrintsToStdoutByDefault(t *testing.T) {
	var buf bytes.Buffer
	if err := Run([]string{"bash"}, fakeEnv(nil), &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("expected script on stdout")
	}
}

func TestRunAcceptsInstallInEitherOrder(t *testing.T) {
	for _, args := range [][]string{{"bash", "--install"}, {"--install", "bash"}} {
		home := t.TempDir()
		var buf bytes.Buffer
		if err := Run(args, fakeEnv(map[string]string{"HOME": home}), &buf); err != nil {
			t.Fatalf("args=%v: %v", args, err)
		}
		if !strings.Contains(buf.String(), home) {
			t.Errorf("args=%v: expected install path under %q, got %q", args, home, buf.String())
		}
	}
}

func TestRunUnknownShellIsError(t *testing.T) {
	var buf bytes.Buffer
	if err := Run([]string{"powershell"}, fakeEnv(nil), &buf); err == nil {
		t.Fatal("expected an error for an unknown shell")
	}
}

// --- __complete dynamic resolution ---

func TestCompleteFirstWordListsVerbs(t *testing.T) {
	f := newFixture(t)
	got := runCompleteLines(f.engine, []string{""})
	if !containsAll(got, Verbs...) {
		t.Fatalf("expected verb list, got %v", got)
	}
}

func TestCompleteCleanModes(t *testing.T) {
	f := newFixture(t)
	got := runCompleteLines(f.engine, []string{"clean", "--mode", ""})
	want := []string{"untracked", "all", "ignored"}
	if len(got) != len(want) || !containsAll(got, want...) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCompleteCompletionShells(t *testing.T) {
	f := newFixture(t)
	got := runCompleteLines(f.engine, []string{"completion", ""})
	if !containsAll(got, Shells...) {
		t.Fatalf("expected shell list, got %v", got)
	}
}

func TestCompleteDetachListsManagedPathsForResolvedProject(t *testing.T) {
	f := newFixture(t)
	restore := chdir(t, f.repo)
	defer restore()
	got := runCompleteLines(f.engine, []string{"detach", ""})
	if !containsAll(got, "AGENT.md") {
		t.Fatalf("expected managed path AGENT.md, got %v", got)
	}
	for _, path := range got {
		if path == "README.md" || path == "src" {
			t.Fatalf("detach must never suggest an unmanaged path, got %v", got)
		}
	}
}

func TestCompleteDetachViaExplicitRootFlag(t *testing.T) {
	f := newFixture(t)
	// Run from an unrelated directory; --root should still resolve the
	// project without relying on cwd detection.
	elsewhere := t.TempDir()
	restore := chdir(t, elsewhere)
	defer restore()
	got := runCompleteLines(f.engine, []string{"detach", "--root", f.projectID, ""})
	if !containsAll(got, "AGENT.md") {
		t.Fatalf("expected managed path AGENT.md via --root, got %v", got)
	}
}

func TestCompleteAdoptListsUnmanagedPathsOnly(t *testing.T) {
	f := newFixture(t)
	restore := chdir(t, f.repo)
	defer restore()
	got := runCompleteLines(f.engine, []string{"adopt", ""})
	if !containsAll(got, "README.md", "src") {
		t.Fatalf("expected unmanaged candidates, got %v", got)
	}
	for _, path := range got {
		if path == "AGENT.md" || strings.HasPrefix(path, ".git") {
			t.Fatalf("adopt must never suggest a managed path or .git, got %v", got)
		}
	}
}

func TestCompleteProjectFlagListsIDsAndNames(t *testing.T) {
	f := newFixture(t)
	got := runCompleteLines(f.engine, []string{"--project", ""})
	if !containsAll(got, f.projectID, "Widget") {
		t.Fatalf("expected project ID and name, got %v", got)
	}
}

func TestCompleteRootFlagIncludesDirMarkerAndRegisteredRoots(t *testing.T) {
	f := newFixture(t)
	got := runCompleteLines(f.engine, []string{"--root", ""})
	if !containsAll(got, dirsMarker, f.repo) {
		t.Fatalf("expected dir marker and registered root, got %v", got)
	}
}

func TestCompleteDiscardListsOutstandingJournals(t *testing.T) {
	f := newFixture(t)
	got := runCompleteLines(f.engine, []string{"recover", "--discard", ""})
	if len(got) != 0 {
		t.Fatalf("expected no outstanding journals, got %v", got)
	}
}

func TestCompleteFlagNameCompletion(t *testing.T) {
	f := newFixture(t)
	got := runCompleteLines(f.engine, []string{"detach", "--"})
	if !containsAll(got, "--root", "--apply", "--revision", "--fingerprint", "--json") {
		t.Fatalf("expected detach flags, got %v", got)
	}
}

func TestCompleteWithUnreadableOrAbsentStatePrintsNothing(t *testing.T) {
	root := t.TempDir()
	s, err := store.New(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(s)
	var buf bytes.Buffer
	RunComplete(e, []string{"detach", ""}, &buf)
	if buf.Len() != 0 {
		t.Fatalf("expected no output for empty state, got %q", buf.String())
	}
}

func TestCompleteWithCorruptStatePrintsNothing(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "state")
	s, err := store.New(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root(), "state.json"), []byte("not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := engine.New(s)
	var buf bytes.Buffer
	RunComplete(e, []string{"--project", ""}, &buf)
	if buf.Len() != 0 {
		t.Fatalf("expected no output for corrupt state, got %q", buf.String())
	}
}

func TestCompleteEmptyArgsPrintsNothing(t *testing.T) {
	f := newFixture(t)
	var buf bytes.Buffer
	RunComplete(f.engine, nil, &buf)
	if buf.Len() != 0 {
		t.Fatalf("expected no output for empty args, got %q", buf.String())
	}
}

// chdir switches to dir for the duration of a test and returns a func that
// restores the previous working directory. It exists (rather than using
// t.Chdir directly at call sites) only so tests read a little more plainly
// with an explicit restore step.
func chdir(t *testing.T, dir string) func() {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() {
		_ = os.Chdir(previous)
	}
}
