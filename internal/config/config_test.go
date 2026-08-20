package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMissingFileReturnsZeroConfigAndNoError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg, existed, err := Load(path)
	if err != nil {
		t.Fatalf("missing config file must not error: %v", err)
	}
	if existed {
		t.Fatal("existed should be false for a missing file")
	}
	if cfg.Workspace != "" || cfg.RemoteKeys != nil {
		t.Fatalf("expected zero Config, got %+v", cfg)
	}
}

func TestLoadParsesWorkspaceAndRemoteKeys(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `# a comment
workspace = "/tmp/my-workspace"

remote_keys = ["remote.origin.url", "remote.upstream.url"]
`)
	cfg, existed, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !existed {
		t.Fatal("expected existed=true")
	}
	if cfg.Workspace != "/tmp/my-workspace" {
		t.Fatalf("workspace = %q", cfg.Workspace)
	}
	want := []string{"remote.origin.url", "remote.upstream.url"}
	if strings.Join(cfg.RemoteKeys, ",") != strings.Join(want, ",") {
		t.Fatalf("remote_keys = %v, want %v", cfg.RemoteKeys, want)
	}
}

func TestLoadAcceptsTrailingCommaInArray(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `remote_keys = ["remote.origin.url",]`)
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RemoteKeys) != 1 || cfg.RemoteKeys[0] != "remote.origin.url" {
		t.Fatalf("remote_keys = %v", cfg.RemoteKeys)
	}
}

func TestLoadAcceptsEmptyArray(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `remote_keys = []`)
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RemoteKeys) != 0 {
		t.Fatalf("remote_keys = %v, want empty", cfg.RemoteKeys)
	}
}

func TestLoadExpandsLeadingHomeInWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	path := writeConfig(t, dir, `workspace = "~/my-ws"`)
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "my-ws")
	if cfg.Workspace != want {
		t.Fatalf("workspace = %q, want %q", cfg.Workspace, want)
	}
}

func TestLoadExpandsBareTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	path := writeConfig(t, dir, `workspace = "~"`)
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace != home {
		t.Fatalf("workspace = %q, want %q", cfg.Workspace, home)
	}
}

func TestLoadIgnoresBlankLinesAndComments(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "\n  \n# top comment\nworkspace = \"/x\"\n   # indented comment\n\n")
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace != "/x" {
		t.Fatalf("workspace = %q", cfg.Workspace)
	}
}

func TestLoadRejectsTableHeader(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "[section]\nworkspace = \"/x\"\n")
	_, existed, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for a table header")
	}
	if !existed {
		t.Fatal("existed should be true for a present-but-malformed file")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), ":1:") {
		t.Fatalf("error should name path and line 1: %v", err)
	}
}

func TestLoadRejectsUnquotedInt(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "revision = 5\n")
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for a bare int")
	}
}

func TestLoadRejectsUnquotedBool(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "dark_mode = true\n")
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for a bare bool")
	}
}

func TestLoadRejectsMultilineArray(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "remote_keys = [\n  \"remote.origin.url\",\n]\n")
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for a multiline array")
	}
}

func TestLoadRejectsWorkspaceAsArray(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `workspace = ["/a", "/b"]`)
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("expected an error when workspace is an array")
	}
}

func TestLoadRejectsRemoteKeysAsString(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `remote_keys = "remote.origin.url"`)
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("expected an error when remote_keys is a string")
	}
}

func TestLoadReportsCorrectLineNumberDeepInFile(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "workspace = \"/ok\"\n# comment\n\nbroken toml here\n")
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), ":4:") {
		t.Fatalf("expected error to name line 4, got: %v", err)
	}
}

func TestLoadRejectsUnterminatedString(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "workspace = \"/unterminated\n")
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an unterminated string")
	}
}

func TestLoadRejectsTrailingGarbageAfterString(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `workspace = "/x" extra`)
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for trailing content after a quoted value")
	}
}

func TestLoadAllowsTrailingCommentAfterString(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `workspace = "/x" # trailing comment`)
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace != "/x" {
		t.Fatalf("workspace = %q", cfg.Workspace)
	}
}

func TestDirHonoursXDGConfigHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	want := filepath.Join(xdg, "ewasd")
	if got := Dir(); got != want {
		t.Fatalf("Dir() = %q, want %q", got, want)
	}
}

func TestDirDefaultsToHomeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	want := filepath.Join(home, ".config", "ewasd")
	if got := Dir(); got != want {
		t.Fatalf("Dir() = %q, want %q", got, want)
	}
}

func TestFilePathIsDirSlashConfigTOML(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	want := filepath.Join(xdg, "ewasd", "config.toml")
	if got := FilePath(); got != want {
		t.Fatalf("FilePath() = %q, want %q", got, want)
	}
}

func TestExpandHomeLeavesNonTildeValuesUnchanged(t *testing.T) {
	if got := ExpandHome("/absolute/path"); got != "/absolute/path" {
		t.Fatalf("ExpandHome altered a non-tilde path: %q", got)
	}
	if got := ExpandHome("relative/path"); got != "relative/path" {
		t.Fatalf("ExpandHome altered a relative path: %q", got)
	}
}
