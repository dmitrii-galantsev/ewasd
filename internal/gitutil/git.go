package gitutil

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

var scpRemote = regexp.MustCompile(`^(?:[^@]+@)?([^:]+):(.+)$`)

type Checkout struct {
	Root    string
	GitRoot string
	Remote  string
	Remotes []string
}

func InspectCheckout(root string) (Checkout, error) {
	if root == "" {
		return Checkout{}, errors.New("checkout root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Checkout{}, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return Checkout{}, fmt.Errorf("resolve checkout root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return Checkout{}, fmt.Errorf("checkout root is not a directory: %s", abs)
	}
	gitRoot, err := git(abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return Checkout{}, fmt.Errorf("%s is not inside a Git checkout: %w", abs, err)
	}
	gitRoot, err = filepath.EvalSymlinks(gitRoot)
	if err != nil {
		return Checkout{}, err
	}
	remotes := CollectRemotes(abs)
	remote, _ := git(abs, "config", "--get", "remote.origin.url")
	return Checkout{Root: abs, GitRoot: gitRoot, Remote: NormalizeRemote(remote), Remotes: remotes}, nil
}

func CollectRemotes(cwd string) []string {
	output, err := git(cwd, "config", "--get-regexp", `^remote\..*\.url$`)
	if err != nil {
		return []string{}
	}
	seen := map[string]bool{}
	result := []string{}
	for _, line := range strings.Split(output, "\n") {
		_, raw, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		normalized := NormalizeRemote(raw)
		if normalized != "" && !seen[normalized] {
			seen[normalized] = true
			result = append(result, normalized)
		}
	}
	sort.Strings(result)
	return result
}

func NormalizeRemote(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if match := scpRemote.FindStringSubmatch(raw); len(match) == 3 && !strings.Contains(raw, "://") {
		return cleanRemote(match[1] + "/" + match[2])
	}
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Host != "" {
		return cleanRemote(parsed.Host + "/" + strings.TrimPrefix(parsed.Path, "/"))
	}
	return cleanRemote(raw)
}

func cleanRemote(remote string) string {
	remote = strings.TrimSuffix(strings.TrimSuffix(remote, "/"), ".git")
	return strings.ToLower(remote)
}

func UpdateExclude(projectID, root, gitRoot string, entries []string) error {
	exclude, err := git(root, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return fmt.Errorf("locate Git exclude file: %w", err)
	}
	if !filepath.IsAbs(exclude) {
		exclude = filepath.Join(root, exclude)
	}
	data, err := os.ReadFile(exclude)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	mode := os.FileMode(0o600)
	if info, statErr := os.Stat(exclude); statErr == nil {
		mode = info.Mode().Perm()
	}
	content := string(data)
	start := "# >>> ewasd " + projectID
	end := "# <<< ewasd " + projectID

	prefix, err := filepath.Rel(gitRoot, root)
	if err != nil || prefix == ".." || strings.HasPrefix(prefix, ".."+string(filepath.Separator)) {
		return errors.New("registered root is outside its Git root")
	}
	patterns := []string{}
	for _, entry := range entries {
		path := filepath.ToSlash(filepath.Join(prefix, filepath.FromSlash(entry)))
		if path == "." {
			continue
		}
		patterns = append(patterns, "/"+escapeGitPattern(strings.TrimPrefix(path, "./")))
	}
	content, err = rewriteBlock(content, start, end, patterns)
	if err != nil {
		return err
	}
	return store.AtomicWrite(exclude, []byte(content), mode)
}

func UpdateLegacyResidual(key, root string, entries []string) error {
	exclude, err := git(root, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return fmt.Errorf("locate Git exclude file: %w", err)
	}
	if !filepath.IsAbs(exclude) {
		exclude = filepath.Join(root, exclude)
	}
	data, err := os.ReadFile(exclude)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	mode := os.FileMode(0o600)
	if info, statErr := os.Stat(exclude); statErr == nil {
		mode = info.Mode().Perm()
	}
	patterns := make([]string, 0, len(entries))
	for _, entry := range entries {
		patterns = append(patterns, "/"+escapeGitPattern(filepath.ToSlash(entry)))
	}
	content, err := rewriteBlock(string(data), "# >>> ewasd legacy-residual "+key, "# <<< ewasd legacy-residual "+key, patterns)
	if err != nil {
		return err
	}
	return store.AtomicWrite(exclude, []byte(content), mode)
}

func CheckExclude(projectID, root, gitRoot string, entries []string) (bool, string) {
	exclude, err := git(root, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return false, "cannot locate Git's private exclude file"
	}
	if !filepath.IsAbs(exclude) {
		exclude = filepath.Join(root, exclude)
	}
	data, err := os.ReadFile(exclude)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, "cannot read Git's private exclude file"
	}
	prefix, err := filepath.Rel(gitRoot, root)
	if err != nil || prefix == ".." || strings.HasPrefix(prefix, ".."+string(filepath.Separator)) {
		return false, "registered root is outside its recorded Git root"
	}
	expected := []string{}
	for _, entry := range entries {
		path := filepath.ToSlash(filepath.Join(prefix, filepath.FromSlash(entry)))
		expected = append(expected, "/"+escapeGitPattern(strings.TrimPrefix(path, "./")))
	}
	sort.Strings(expected)
	content := string(data)
	start := "# >>> ewasd " + projectID
	end := "# <<< ewasd " + projectID
	startCount, endCount, startIndex, endIndex := markerShape(content, start, end)
	if (startCount != 0 || endCount != 0) && (startCount != 1 || endCount != 1 || endIndex < startIndex) {
		return false, "managed Git exclude markers are malformed or duplicated"
	}
	actual, found := blockLines(content, start, end)
	sort.Strings(actual)
	if len(expected) == 0 {
		if found {
			return false, "stale ewasd block remains for an empty manifest"
		}
		return true, "no managed entries need Git exclusions"
	}
	if !found {
		return false, "managed Git exclude block is missing"
	}
	if strings.Join(actual, "\n") != strings.Join(expected, "\n") {
		return false, "managed Git exclude block drifted from the manifest"
	}
	return true, "private Git exclude block matches the manifest"
}

func ManagedExcludePatterns(root string) ([]string, error) {
	exclude, err := git(root, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return nil, fmt.Errorf("locate Git exclude file: %w", err)
	}
	if !filepath.IsAbs(exclude) {
		exclude = filepath.Join(root, exclude)
	}
	data, err := os.ReadFile(exclude)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	inside := false
	patterns := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "# >>> ewasd ") {
			inside = true
			continue
		}
		if inside && strings.HasPrefix(line, "# <<< ewasd ") {
			inside = false
			continue
		}
		if inside && line != "" && !strings.HasPrefix(line, "# ewasd-separator: ") {
			patterns = append(patterns, line)
		}
	}
	sort.Strings(patterns)
	return compact(patterns), nil
}

type CleanOptions struct {
	Mode               string
	IncludeDirectories bool
	ProtectedPatterns  []string
	Scope              string
}

type CleanPreview struct {
	Candidates          []string
	SkippedRepositories []string
	Command             []string
}

func PreviewClean(gitRoot string, options CleanOptions) (CleanPreview, error) {
	args, err := cleanArgs(true, options)
	if err != nil {
		return CleanPreview{}, err
	}
	output, err := gitOutputWithTimeout(gitRoot, 2*time.Minute, []string{"LC_ALL=C", "LANG=C"}, args...)
	if err != nil {
		return CleanPreview{}, err
	}
	preview := CleanPreview{Candidates: []string{}, SkippedRepositories: []string{}, Command: append([]string{"git"}, cleanArgsForDisplay(false, options)...)}
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(line, "Would remove "):
			path, err := parseCleanPath(strings.TrimPrefix(line, "Would remove "))
			if err != nil {
				return CleanPreview{}, err
			}
			preview.Candidates = append(preview.Candidates, path)
		case strings.HasPrefix(line, "Would skip repository "):
			path, err := parseCleanPath(strings.TrimPrefix(line, "Would skip repository "))
			if err != nil {
				return CleanPreview{}, err
			}
			preview.SkippedRepositories = append(preview.SkippedRepositories, path)
		case line == "Would refuse to remove current working directory":
			// With -X, Git may represent all ignored contents as "./" and then
			// explicitly note that the scope directory itself is retained.
			continue
		case line == "":
			continue
		default:
			return CleanPreview{}, fmt.Errorf("unexpected git clean output: %s", line)
		}
	}
	return preview, nil
}

func ApplyClean(gitRoot string, options CleanOptions) ([]string, error) {
	args, err := cleanArgs(false, options)
	if err != nil {
		return nil, err
	}
	command := exec.Command("git", args...)
	command.Dir = gitRoot
	command.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git clean failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	removed := []string{}
	for _, line := range strings.Split(string(output), "\n") {
		switch {
		case strings.HasPrefix(line, "Removing "):
			path, parseErr := parseCleanPath(strings.TrimPrefix(line, "Removing "))
			if parseErr != nil {
				return nil, parseErr
			}
			removed = append(removed, path)
		case strings.HasPrefix(line, "Skipping repository "), line == "", line == "Refusing to remove current working directory", strings.HasPrefix(line, "warning: failed to remove ./"):
			continue
		default:
			return nil, fmt.Errorf("unexpected git clean apply output: %s", line)
		}
	}
	return removed, nil
}

func cleanArgs(dryRun bool, options CleanOptions) ([]string, error) {
	modeFlag := ""
	switch options.Mode {
	case "all":
		modeFlag = "-x"
	case "untracked":
	case "ignored":
		modeFlag = "-X"
	default:
		return nil, fmt.Errorf("unsupported clean mode %q", options.Mode)
	}
	args := []string{"-c", "core.quotePath=true", "clean"}
	if dryRun {
		args = append(args, "-n")
	} else {
		args = append(args, "-f")
	}
	if options.IncludeDirectories {
		args = append(args, "-d")
	}
	if modeFlag != "" {
		args = append(args, modeFlag)
	}
	for _, pattern := range compact(options.ProtectedPatterns) {
		if options.Mode == "ignored" {
			for _, negation := range protectedNegations(pattern) {
				args = append(args, "-e", negation)
			}
		} else {
			args = append(args, "-e", pattern)
		}
	}
	if options.Scope != "" {
		args = append(args, "--", filepath.ToSlash(options.Scope))
	}
	return args, nil
}

func protectedNegations(pattern string) []string {
	trimmed := strings.Trim(strings.TrimSuffix(pattern, "/"), "/")
	if trimmed == "" {
		return []string{"!" + pattern}
	}
	parts := strings.Split(trimmed, "/")
	result := []string{}
	for index := range parts {
		prefix := "/" + strings.Join(parts[:index+1], "/")
		result = append(result, "!"+prefix)
		if index < len(parts)-1 || strings.HasSuffix(pattern, "/") {
			result = append(result, "!"+prefix+"/")
		}
	}
	return result
}

func cleanArgsForDisplay(dryRun bool, options CleanOptions) []string {
	args, _ := cleanArgs(dryRun, options)
	return args
}

func parseCleanPath(raw string) (string, error) {
	if strings.HasPrefix(raw, `"`) {
		decoded, err := strconv.Unquote(raw)
		if err != nil {
			return "", fmt.Errorf("decode git clean path %s: %w", raw, err)
		}
		return decoded, nil
	}
	return raw, nil
}

func compact(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func rewriteBlock(content, start, end string, patterns []string) (string, error) {
	lines := strings.SplitAfter(content, "\n")
	startIndex, endIndex := -1, -1
	startCount, endCount := 0, 0
	separatorInserted := false
	for index, rawLine := range lines {
		line := strings.TrimSuffix(rawLine, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == start {
			startIndex = index
			startCount++
		}
		if line == end {
			endIndex = index
			endCount++
		}
	}
	base := content
	if startCount != 0 || endCount != 0 {
		if startCount != 1 || endCount != 1 || endIndex < startIndex {
			return "", errors.New("managed Git exclude markers are malformed or duplicated; refusing to rewrite user content")
		}
		for _, rawLine := range lines[startIndex+1 : endIndex] {
			line := strings.TrimSuffix(strings.TrimSuffix(rawLine, "\n"), "\r")
			if line == "# ewasd-separator: inserted" {
				separatorInserted = true
			}
		}
		prefix := strings.Join(lines[:startIndex], "")
		suffix := strings.Join(lines[endIndex+1:], "")
		if separatorInserted {
			if suffix == "" && strings.HasSuffix(prefix, "\n") {
				prefix = strings.TrimSuffix(prefix, "\n")
			} else {
				suffix = transferInsertedSeparator(suffix)
			}
		}
		base = prefix + suffix
	}
	if len(patterns) == 0 {
		return base, nil
	}
	separatorKind := "existing"
	separator := ""
	if base == "" {
		separatorKind = "empty"
	} else if !strings.HasSuffix(base, "\n") {
		separatorKind = "inserted"
		separator = "\n"
	}
	block := start + "\n# ewasd-separator: " + separatorKind + "\n" + strings.Join(patterns, "\n") + "\n" + end + "\n"
	return base + separator + block, nil
}

func transferInsertedSeparator(suffix string) string {
	lines := strings.SplitAfter(suffix, "\n")
	if len(lines) < 2 || !strings.HasPrefix(strings.TrimSuffix(lines[0], "\n"), "# >>> ewasd ") {
		return suffix
	}
	metadata := strings.TrimSuffix(strings.TrimSuffix(lines[1], "\n"), "\r")
	if metadata != "# ewasd-separator: existing" {
		return suffix
	}
	ending := "\n"
	if strings.HasSuffix(lines[1], "\r\n") {
		ending = "\r\n"
	} else if !strings.HasSuffix(lines[1], "\n") {
		ending = ""
	}
	lines[1] = "# ewasd-separator: inserted" + ending
	return strings.Join(lines, "")
}

func escapeGitPattern(path string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`*`, `\*`,
		`?`, `\?`,
		`[`, `\[`,
		`]`, `\]`,
	)
	return replacer.Replace(path)
}

func EscapeGitPattern(path string) string {
	return escapeGitPattern(path)
}

func blockLines(content, start, end string) ([]string, bool) {
	inside := false
	found := false
	lines := []string{}
	for _, line := range strings.Split(content, "\n") {
		if line == start {
			inside = true
			found = true
			continue
		}
		if inside && line == end {
			return lines, found
		}
		if inside && line != "" && !strings.HasPrefix(line, "# ewasd-separator: ") {
			lines = append(lines, line)
		}
	}
	return lines, false
}

func markerShape(content, start, end string) (startCount, endCount, startIndex, endIndex int) {
	startIndex, endIndex = -1, -1
	for index, line := range strings.Split(content, "\n") {
		if line == start {
			startCount++
			startIndex = index
		}
		if line == end {
			endCount++
			endIndex = index
		}
	}
	return
}

func git(cwd string, args ...string) (string, error) {
	return gitOutputWithTimeout(cwd, 5*time.Second, nil, args...)
}

func gitOutputWithTimeout(cwd string, timeout time.Duration, environment []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	if len(environment) > 0 {
		cmd.Env = append(os.Environ(), environment...)
	}
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("git command timed out after %s", timeout)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
