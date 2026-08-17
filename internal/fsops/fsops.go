package fsops

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func ValidateRelative(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("path is required")
	}
	if strings.ContainsRune(raw, '\x00') {
		return "", errors.New("path contains a NUL byte")
	}
	if strings.ContainsAny(raw, "\r\n") {
		return "", errors.New("path contains a line break")
	}
	if filepath.IsAbs(raw) {
		return "", errors.New("path must be relative to the registered checkout")
	}
	clean := filepath.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path must stay below the registered checkout")
	}
	if filepath.ToSlash(clean) != filepath.ToSlash(raw) {
		return "", fmt.Errorf("path must be normalized; use %q", filepath.ToSlash(clean))
	}
	for _, component := range strings.Split(filepath.ToSlash(clean), "/") {
		if strings.TrimSpace(component) != component {
			return "", errors.New("path components cannot start or end with whitespace")
		}
	}
	return filepath.ToSlash(clean), nil
}

func SafeTarget(root, relative string) (string, error) {
	relative, err := ValidateRelative(relative)
	if err != nil {
		return "", err
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve checkout root: %w", err)
	}
	canonicalRoot, err = filepath.Abs(canonicalRoot)
	if err != nil {
		return "", err
	}
	parts := strings.Split(filepath.FromSlash(relative), string(filepath.Separator))
	parent := canonicalRoot
	for _, part := range parts[:len(parts)-1] {
		parent = filepath.Join(parent, part)
		info, statErr := os.Lstat(parent)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return "", fmt.Errorf("inspect parent %s: %w", parent, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("parent %s is a symlink; refusing a path that can escape the checkout", parent)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("parent %s is not a directory", parent)
		}
	}
	target := filepath.Join(canonicalRoot, filepath.FromSlash(relative))
	rel, err := filepath.Rel(canonicalRoot, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("resolved path escapes the checkout")
	}
	return target, nil
}

func Kind(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	switch {
	case info.Mode().IsRegular():
		return "file", nil
	case info.IsDir():
		return "directory", nil
	case info.Mode()&os.ModeSymlink != 0:
		return "symlink", nil
	default:
		return "special", nil
	}
}

func LinkPointsTo(link, expected string) bool {
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return false
	}
	raw, err := os.Readlink(link)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(filepath.Dir(link), raw)
	}
	actual, err := filepath.Abs(filepath.Clean(raw))
	if err != nil {
		return false
	}
	want, err := filepath.Abs(filepath.Clean(expected))
	return err == nil && actual == want
}

func CopyTree(source, destination string) error {
	if err := ValidateCopyable(source); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	switch {
	case info.Mode().IsRegular():
		return copyFile(source, destination, info.Mode())
	case info.IsDir():
		return copyDirectory(source, destination, info.Mode(), source)
	case info.Mode()&os.ModeSymlink != 0:
		return errors.New("adopting symlinks is intentionally unsupported; resolve or copy the target explicitly")
	default:
		return fmt.Errorf("special file mode %s is unsupported", info.Mode())
	}
}

func ValidateCopyable(source string) error {
	root, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if root.Mode()&os.ModeSymlink != 0 {
		return errors.New("adopting symlinks is intentionally unsupported; resolve or copy the target explicitly")
	}
	if !root.Mode().IsRegular() && !root.IsDir() {
		return fmt.Errorf("special file mode %s is unsupported", root.Mode())
	}
	if !root.IsDir() {
		return nil
	}
	canonicalRoot, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			raw, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(raw) {
				return fmt.Errorf("nested symlink %s has an absolute target", path)
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("nested symlink %s is broken: %w", path, err)
			}
			relative, err := filepath.Rel(canonicalRoot, resolved)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return fmt.Errorf("nested symlink %s escapes the managed directory", path)
			}
			return nil
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("special file %s is unsupported", path)
		}
		return nil
	})
}

func copyDirectory(source, destination string, mode os.FileMode, sourceRoot string) error {
	if err := os.Mkdir(destination, mode.Perm()); err != nil {
		return err
	}
	if err := os.Chmod(destination, mode.Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		from := filepath.Join(source, entry.Name())
		to := filepath.Join(destination, entry.Name())
		info, err := os.Lstat(from)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			raw, err := os.Readlink(from)
			if err != nil {
				return err
			}
			if err := os.Symlink(raw, to); err != nil {
				return err
			}
			continue
		}
		if info.IsDir() {
			if err := copyDirectory(from, to, info.Mode(), sourceRoot); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("special file %s is unsupported", from)
		}
		if err := copyFile(from, to, info.Mode()); err != nil {
			return err
		}
	}
	return SyncDir(destination)
}

func copyFile(source, destination string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	if err := out.Chmod(mode.Perm()); err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func AtomicSymlink(target, destination, operationID string) error {
	_ = operationID // retained in the API so journal call sites remain explicit.
	// symlink(2) creates the complete directory entry atomically and fails with
	// EEXIST. Unlike rename-over, it can never replace a path that appeared
	// after preflight.
	if err := os.Symlink(target, destination); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(destination))
}

func SyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func RemoveAny(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return os.RemoveAll(path)
	}
	return os.Remove(path)
}

func EqualTree(left, right string) (bool, error) {
	leftDigest, err := TreeDigest(left)
	if err != nil {
		return false, err
	}
	rightDigest, err := TreeDigest(right)
	if err != nil {
		return false, err
	}
	return leftDigest == rightDigest, nil
}

func SyncTreeModes(source, destination string) error {
	if err := ValidateCopyable(source); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		sourceInfo, err := os.Lstat(path)
		if err != nil {
			return err
		}
		targetInfo, err := os.Lstat(target)
		if err != nil {
			return err
		}
		if sourceInfo.Mode()&os.ModeSymlink != 0 {
			if targetInfo.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("target %s is not the expected symlink", target)
			}
			return nil
		}
		if sourceInfo.IsDir() != targetInfo.IsDir() || sourceInfo.Mode().IsRegular() != targetInfo.Mode().IsRegular() {
			return fmt.Errorf("target %s has a different type", target)
		}
		return os.Chmod(target, sourceInfo.Mode().Perm())
	})
}

func TreeDigest(root string) (string, error) {
	if err := ValidateCopyable(root); err != nil {
		return "", err
	}
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%s\x00", filepath.ToSlash(relative), info.Mode().String())
		if info.Mode()&os.ModeSymlink != 0 {
			raw, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(hash, "%s\x00", raw)
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
