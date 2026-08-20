package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitrii-galantsev/ewasd/internal/engine"
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

func TestCLIInfersLinkAndCleanAppliesByDefault(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	template := filepath.Join(root, "template")
	clone := filepath.Join(root, "clone")
	for _, repository := range []string{template, clone} {
		if err := os.Mkdir(repository, 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, repository, "init", "-q")
		runGit(t, repository, "remote", "add", "origin", "git@github.com:example/inferred.git")
	}
	t.Setenv("EWASD_HOME", stateRoot)
	stateStore, err := store.New(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	domainEngine := engine.New(stateStore)
	project, _, err := domainEngine.Register(template, "inferred")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(template, "AGENT.md"), []byte("shared"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, _ := domainEngine.PlanAdopt(project.ID, "AGENT.md")
	if _, err := domainEngine.Adopt(project.ID, "AGENT.md", plan.ExpectedRevision, plan.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"link", "--root", clone}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(clone, "AGENT.md")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("CLI link did not create symlink: %v, %v", info, err)
	}
	if err := os.WriteFile(filepath.Join(clone, "junk.tmp"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"clean", "--root", clone}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(clone, "junk.tmp")); err != nil {
		t.Fatalf("clean dry-run changed junk: %v", err)
	}
	cleanPlan, err := domainEngine.PlanClean(clone, "", engine.CleanOptions{Mode: "untracked"})
	if err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"clean", "--root", clone, "--apply", "--revision", fmt.Sprint(cleanPlan.ExpectedRevision), "--fingerprint", cleanPlan.Fingerprint}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(clone, "junk.tmp")); !os.IsNotExist(err) {
		t.Fatalf("CLI clean did not remove junk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clone, "AGENT.md")); err != nil {
		t.Fatalf("CLI clean removed managed link: %v", err)
	}
}

func runGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = cwd
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

// isolatedEnv points HOME, XDG_CONFIG_HOME, and XDG_DATA_HOME at fresh
// temp directories and clears every workspace-resolution env var, so tests
// never read or write a developer's real ~/.config or
// ~/.local/share/ewasd-v2. t.Setenv restores the previous values when the
// test (and its subtests) finish.
func isolatedEnv(t *testing.T) (home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("EWASD_HOME", "")
	t.Setenv("EWASD_NEXT_HOME", "")
	t.Setenv("EWASD_WORKSPACE", "")
	return home
}

// writeConfigTOML writes content to $xdgConfigHome/ewasd/config.toml,
// creating the directory as needed.
func writeConfigTOML(t *testing.T, xdgConfigHome, content string) string {
	t.Helper()
	dir := filepath.Join(xdgConfigHome, "ewasd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// captureStdout redirects os.Stdout for the duration of fn (which every
// command in this file writes to directly via fmt.Print*/json.NewEncoder),
// and returns what was written plus fn's own return value.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = w
	outputCh := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		outputCh <- buf.String()
	}()
	fnErr := fn()
	os.Stdout = original
	_ = w.Close()
	return <-outputCh, fnErr
}

// --- workspace resolution order ---

func TestResolveWorkspacePrecedenceOrder(t *testing.T) {
	home := isolatedEnv(t)
	xdgData := filepath.Join(home, ".local", "share")

	// 6. Nothing set at all: default XDG data home.
	got, err := resolveWorkspace("")
	if err != nil {
		t.Fatal(err)
	}
	wantDefault := filepath.Join(xdgData, "ewasd-v2")
	if got.DataRoot != wantDefault || got.DataRootSource != sourceDefault {
		t.Fatalf("default level: got %+v, want path %q source %q", got, wantDefault, sourceDefault)
	}

	// 5. config.toml's "workspace" key overrides the default.
	writeConfigTOML(t, filepath.Join(home, ".config"), `workspace = "/from/config-toml"`)
	got, err = resolveWorkspace("")
	if err != nil {
		t.Fatal(err)
	}
	if got.DataRoot != "/from/config-toml" || got.DataRootSource != sourceConfigTOML {
		t.Fatalf("config.toml level: got %+v", got)
	}

	// 4. EWASD_WORKSPACE (the old Python tool's name) overrides config.toml.
	t.Setenv("EWASD_WORKSPACE", "/from/ewasd-workspace-env")
	got, err = resolveWorkspace("")
	if err != nil {
		t.Fatal(err)
	}
	if got.DataRoot != "/from/ewasd-workspace-env" || got.DataRootSource != sourceEwasdWorkspace {
		t.Fatalf("EWASD_WORKSPACE level: got %+v", got)
	}

	// 3. EWASD_NEXT_HOME overrides EWASD_WORKSPACE.
	t.Setenv("EWASD_NEXT_HOME", "/from/ewasd-next-home")
	got, err = resolveWorkspace("")
	if err != nil {
		t.Fatal(err)
	}
	if got.DataRoot != "/from/ewasd-next-home" || got.DataRootSource != sourceEwasdNextHome {
		t.Fatalf("EWASD_NEXT_HOME level: got %+v", got)
	}

	// 2. EWASD_HOME overrides EWASD_NEXT_HOME.
	t.Setenv("EWASD_HOME", "/from/ewasd-home")
	got, err = resolveWorkspace("")
	if err != nil {
		t.Fatal(err)
	}
	if got.DataRoot != "/from/ewasd-home" || got.DataRootSource != sourceEwasdHome {
		t.Fatalf("EWASD_HOME level: got %+v", got)
	}

	// 1. --workspace flag overrides everything else.
	got, err = resolveWorkspace("/from/flag")
	if err != nil {
		t.Fatal(err)
	}
	if got.DataRoot != "/from/flag" || got.DataRootSource != sourceFlag {
		t.Fatalf("flag level: got %+v", got)
	}
}

func TestResolveWorkspaceExpandsLeadingTildeForFlagAndEnvVars(t *testing.T) {
	home := isolatedEnv(t)

	got, err := resolveWorkspace("~/from-flag")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "from-flag"); got.DataRoot != want {
		t.Fatalf("flag: DataRoot = %q, want %q", got.DataRoot, want)
	}

	t.Setenv("EWASD_HOME", "~/from-ewasd-home")
	got, err = resolveWorkspace("")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "from-ewasd-home"); got.DataRoot != want {
		t.Fatalf("EWASD_HOME: DataRoot = %q, want %q", got.DataRoot, want)
	}

	t.Setenv("EWASD_HOME", "")
	t.Setenv("EWASD_WORKSPACE", "~/from-ewasd-workspace")
	got, err = resolveWorkspace("")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "from-ewasd-workspace"); got.DataRoot != want {
		t.Fatalf("EWASD_WORKSPACE: DataRoot = %q, want %q", got.DataRoot, want)
	}
}

func TestResolveWorkspaceMissingConfigFileIsFine(t *testing.T) {
	isolatedEnv(t)
	got, err := resolveWorkspace("")
	if err != nil {
		t.Fatalf("missing config file must not error: %v", err)
	}
	if got.ConfigExists {
		t.Fatal("expected ConfigExists = false")
	}
}

func TestResolveWorkspaceMalformedConfigErrorsWithPathAndLine(t *testing.T) {
	home := isolatedEnv(t)
	path := writeConfigTOML(t, filepath.Join(home, ".config"), "workspace = \"/ok\"\nrevision = 5\n")
	_, err := resolveWorkspace("")
	if err == nil {
		t.Fatal("expected an error for a malformed config.toml")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the config path: %v", err)
	}
	if !strings.Contains(err.Error(), ":2:") {
		t.Errorf("error does not name line 2: %v", err)
	}
}

func TestResolveWorkspaceRemoteKeysDefaultsToOrigin(t *testing.T) {
	isolatedEnv(t)
	got, err := resolveWorkspace("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RemoteKeys) != 1 || got.RemoteKeys[0] != "remote.origin.url" || got.RemoteKeysSource != sourceDefault {
		t.Fatalf("got %+v", got)
	}
}

func TestResolveWorkspaceRemoteKeysFromConfigMultiKeyFallthroughOrder(t *testing.T) {
	home := isolatedEnv(t)
	writeConfigTOML(t, filepath.Join(home, ".config"), `remote_keys = ["remote.origin.url", "remote.upstream.url"]`)
	got, err := resolveWorkspace("")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"remote.origin.url", "remote.upstream.url"}
	if got.RemoteKeysSource != sourceConfigTOML || strings.Join(got.RemoteKeys, ",") != strings.Join(want, ",") {
		t.Fatalf("got %+v", got)
	}
}

// --- ewasd config ---

func TestConfigCommandTextOutputShowsProvenanceForEveryValue(t *testing.T) {
	isolatedEnv(t)
	out, err := captureStdout(t, func() error { return run([]string{"config"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"data root:", "(default)", "config file:", "not found", "remote keys:", "state.json:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestConfigCommandJSONOutputIncludesProvenance(t *testing.T) {
	isolatedEnv(t)
	out, err := captureStdout(t, func() error { return run([]string{"config", "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	dataRoot, ok := payload["data_root"].(map[string]any)
	if !ok {
		t.Fatalf("payload missing data_root: %v", payload)
	}
	if dataRoot["source"] != "default" {
		t.Errorf("data_root.source = %v, want default", dataRoot["source"])
	}
	if dataRoot["exists"] != false {
		t.Errorf("data_root.exists = %v, want false", dataRoot["exists"])
	}
	remoteKeys, ok := payload["remote_keys"].(map[string]any)
	if !ok || remoteKeys["source"] != "default" {
		t.Fatalf("payload.remote_keys = %v", payload["remote_keys"])
	}
}

func TestConfigCommandHonoursOldEwasdWorkspaceEnvVarName(t *testing.T) {
	isolatedEnv(t)
	ws := filepath.Join(t.TempDir(), "wstest")
	t.Setenv("EWASD_WORKSPACE", ws)
	out, err := captureStdout(t, func() error { return run([]string{"config"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, ws) {
		t.Fatalf("output does not mention %q:\n%s", ws, out)
	}
	if !strings.Contains(out, "EWASD_WORKSPACE") {
		t.Fatalf("output does not name EWASD_WORKSPACE as the source:\n%s", out)
	}
}

func TestConfigCommandNeverCreatesDataRoot(t *testing.T) {
	home := isolatedEnv(t)
	dataRoot := filepath.Join(home, ".local", "share", "ewasd-v2")
	if _, err := captureStdout(t, func() error { return run([]string{"config"}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dataRoot); !os.IsNotExist(err) {
		t.Fatalf("ewasd config created the data root: stat err = %v", err)
	}
}

func TestConfigCommandReflectsConfigTOMLWorkspaceAndRemoteKeys(t *testing.T) {
	home := isolatedEnv(t)
	ws := filepath.Join(t.TempDir(), "configured-ws")
	writeConfigTOML(t, filepath.Join(home, ".config"), fmt.Sprintf("workspace = %q\nremote_keys = [\"remote.origin.url\", \"remote.upstream.url\"]\n", ws))
	out, err := captureStdout(t, func() error { return run([]string{"config"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, ws) || !strings.Contains(out, "config.toml") {
		t.Fatalf("output missing configured workspace/source:\n%s", out)
	}
	if !strings.Contains(out, "remote.upstream.url") {
		t.Fatalf("output missing configured remote key:\n%s", out)
	}
}

// --- --workspace flag positioning ---

func TestWorkspaceFlagAcceptedBeforeSubcommand(t *testing.T) {
	isolatedEnv(t)
	ws := filepath.Join(t.TempDir(), "wstest-before")
	out, err := captureStdout(t, func() error { return run([]string{"--workspace", ws, "config"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, ws) || !strings.Contains(out, "(flag)") {
		t.Fatalf("output = %s", out)
	}
}

func TestWorkspaceFlagAcceptedAfterSubcommand(t *testing.T) {
	isolatedEnv(t)
	ws := filepath.Join(t.TempDir(), "wstest-after")
	out, err := captureStdout(t, func() error { return run([]string{"config", "--workspace", ws}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, ws) || !strings.Contains(out, "(flag)") {
		t.Fatalf("output = %s", out)
	}
}

func TestWorkspaceFlagEqualsSyntax(t *testing.T) {
	isolatedEnv(t)
	ws := filepath.Join(t.TempDir(), "wstest-equals")
	out, err := captureStdout(t, func() error { return run([]string{"config", "--workspace=" + ws}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, ws) {
		t.Fatalf("output = %s", out)
	}
}

func TestWorkspaceFlagWithoutValueIsError(t *testing.T) {
	isolatedEnv(t)
	if err := run([]string{"config", "--workspace"}); err == nil {
		t.Fatal("expected an error for --workspace with no value")
	}
}

func TestWorkspaceFlagAlsoAppliesToOrdinaryCommands(t *testing.T) {
	isolatedEnv(t)
	ws := filepath.Join(t.TempDir(), "wstest-status")
	if err := run([]string{"status", "--workspace", ws, "--json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws); err != nil {
		t.Fatalf("status did not build a store rooted at the --workspace path: %v", err)
	}
}

// --- ewasd init ---

func TestInitCreatesDataRootWithSubdirsAndMode0700AndIsIdempotent(t *testing.T) {
	isolatedEnv(t)
	root := filepath.Join(t.TempDir(), "fresh-root")

	out, err := captureStdout(t, func() error { return run([]string{"init", "--workspace", root}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "initialized") {
		t.Fatalf("first-run output = %s", out)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("data root mode = %o, want 0700", perm)
	}
	for _, dir := range []string{"profiles", "archive", "transactions", "recovery"} {
		subInfo, err := os.Stat(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("missing subdirectory %s: %v", dir, err)
		}
		if perm := subInfo.Mode().Perm(); perm != 0o700 {
			t.Fatalf("%s mode = %o, want 0700", dir, perm)
		}
	}

	out2, err := captureStdout(t, func() error { return run([]string{"init", "--workspace", root}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "already exists") {
		t.Fatalf("second-run output should report idempotency, got: %s", out2)
	}
}

func TestInitFromGitClonesLocalRepoAndValidatesState(t *testing.T) {
	isolatedEnv(t)
	source := t.TempDir()
	runGit(t, source, "init", "-q")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "README.md")
	runGit(t, source, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "init")

	dest := filepath.Join(t.TempDir(), "fresh")
	out, err := captureStdout(t, func() error {
		return run([]string{"init", "--from-git", "file://" + source, "--workspace", dest})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "initialized") {
		t.Fatalf("output = %s", out)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("cloned content missing: %v", err)
	}
	for _, dir := range []string{"profiles", "archive", "transactions", "recovery"} {
		if _, err := os.Stat(filepath.Join(dest, dir)); err != nil {
			t.Fatalf("missing required subdirectory %s: %v", dir, err)
		}
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("cloned data root mode = %o, want 0700", perm)
	}
}

func TestInitFromGitRefusesNonEmptyRootWithoutTouchingIt(t *testing.T) {
	isolatedEnv(t)
	source := t.TempDir()
	runGit(t, source, "init", "-q")

	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "existing.txt"), []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"init", "--from-git", "file://" + source, "--workspace", dest})
	if err == nil {
		t.Fatal("expected refusal to clone into a non-empty data root")
	}
	data, statErr := os.ReadFile(filepath.Join(dest, "existing.txt"))
	if statErr != nil || string(data) != "keep me" {
		t.Fatalf("existing content was touched: data=%q err=%v", data, statErr)
	}
}

func TestInitFromGitCleansUpFreshRootOnSchemaValidationFailure(t *testing.T) {
	isolatedEnv(t)
	source := t.TempDir()
	runGit(t, source, "init", "-q")
	badState := `{"schema_version": 999, "revision": 1, "projects": [], "activity": []}`
	if err := os.WriteFile(filepath.Join(source, "state.json"), []byte(badState), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "state.json")
	runGit(t, source, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "init")

	dest := filepath.Join(t.TempDir(), "fresh")
	err := run([]string{"init", "--from-git", "file://" + source, "--workspace", dest})
	if err == nil {
		t.Fatal("expected a schema validation error")
	}
	if !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("error should mention the schema version problem: %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("partial data root was not cleaned up: stat err = %v", statErr)
	}
}

func TestInitFromGitCleansUpPreexistingEmptyRootOnFailureWithoutDeletingIt(t *testing.T) {
	isolatedEnv(t)
	source := t.TempDir()
	runGit(t, source, "init", "-q")
	badState := `{"schema_version": 999, "revision": 1, "projects": [], "activity": []}`
	if err := os.WriteFile(filepath.Join(source, "state.json"), []byte(badState), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "state.json")
	runGit(t, source, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "init")

	dest := t.TempDir() // pre-existing, empty
	err := run([]string{"init", "--from-git", "file://" + source, "--workspace", dest})
	if err == nil {
		t.Fatal("expected a schema validation error")
	}
	info, statErr := os.Stat(dest)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("pre-existing root directory itself should survive cleanup: %v, %v", info, statErr)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("pre-existing root should be emptied back out after a failed clone, got: %v", entries)
	}
}
