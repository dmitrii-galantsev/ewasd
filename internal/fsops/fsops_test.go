package fsops

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateRelativeRejectsEscapesAndNonCanonicalPaths(t *testing.T) {
	t.Parallel()
	invalid := []string{"", ".", "..", "../secret", "a/../../secret", "/absolute", "a/../b", "a//b", "a\x00b", "notes\nsrc", " leading", "trailing ", "dir/ trailing"}
	for _, input := range invalid {
		if _, err := ValidateRelative(input); err == nil {
			t.Errorf("ValidateRelative(%q) unexpectedly succeeded", input)
		}
	}
	if got, err := ValidateRelative("src/AGENT.md"); err != nil || got != "src/AGENT.md" {
		t.Fatalf("valid nested path: got %q, %v", got, err)
	}
}

func TestSafeTargetRejectsSymlinkParent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeTarget(root, "escape/file"); err == nil {
		t.Fatal("expected symlink parent to be rejected")
	}
}

func TestCopyTreePreservesContainedNestedSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "copy")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "ok"), []byte("content"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "ok"), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("ok", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(source, destination); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.Readlink(filepath.Join(destination, "link")); err != nil || raw != "ok" {
		t.Fatalf("nested symlink was not preserved: %q, %v", raw, err)
	}
	equal, err := EqualTree(source, destination)
	if err != nil || !equal {
		t.Fatalf("copied tree differs: %v, %v", equal, err)
	}
	info, err := os.Stat(filepath.Join(destination, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o664 {
		t.Fatalf("copy did not preserve mode: %v", info.Mode().Perm())
	}
}

func TestCopyTreeRejectsEscapingNestedSymlinkBeforeDestinationCreation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "copy")
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside", filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(source, destination); err == nil {
		t.Fatal("expected escaping nested symlink to be rejected")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("validation should happen before destination creation, got %v", err)
	}
}

func TestAtomicSymlinkReplacesNothingAndPointsExactly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	if err := os.WriteFile(source, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicSymlink(source, target, "test"); err != nil {
		t.Fatal(err)
	}
	if !LinkPointsTo(target, source) {
		t.Fatal("link does not point to exact source")
	}
	occupied := filepath.Join(root, "occupied")
	if err := os.WriteFile(occupied, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicSymlink(source, occupied, "race"); err == nil {
		t.Fatal("symlink creation unexpectedly replaced an occupied path")
	}
	if data, _ := os.ReadFile(occupied); string(data) != "keep" {
		t.Fatalf("occupied path changed: %q", data)
	}
}
