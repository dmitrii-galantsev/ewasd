package legacy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dmitrii-galantsev/ewasd/internal/domain"
	"github.com/dmitrii-galantsev/ewasd/internal/fsops"
	"github.com/dmitrii-galantsev/ewasd/internal/gitutil"
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

const markerName = ".ewasd_gitignore"

var ErrNotGeneratedMarker = errors.New("not an ewasd-generated marker")

type Definition struct {
	Name       string
	Remote     string
	SourceRoot string
}

func Discover(workspace string, scanRoots []string) (domain.LegacyMigrationPlan, error) {
	workspace, err := canonicalDirectory(workspace)
	if err != nil {
		return domain.LegacyMigrationPlan{}, fmt.Errorf("legacy workspace: %w", err)
	}
	definitions, err := ParseEditors(filepath.Join(workspace, "editors.toml"), workspace)
	if err != nil {
		return domain.LegacyMigrationPlan{}, err
	}
	if len(scanRoots) == 0 {
		return domain.LegacyMigrationPlan{}, errors.New("at least one scan root is required")
	}
	canonicalRoots := make([]string, 0, len(scanRoots))
	for _, root := range scanRoots {
		canonical, err := canonicalDirectory(root)
		if err != nil {
			return domain.LegacyMigrationPlan{}, fmt.Errorf("scan root %s: %w", root, err)
		}
		canonicalRoots = append(canonicalRoots, canonical)
	}
	sort.Strings(canonicalRoots)
	markers, err := findMarkers(canonicalRoots)
	if err != nil {
		return domain.LegacyMigrationPlan{}, err
	}
	plan := domain.LegacyMigrationPlan{
		LegacyWorkspace: workspace,
		ScanRoots:       canonicalRoots,
		Projects:        []domain.LegacyProjectPlan{},
		Markers:         []domain.LegacyMarkerPlan{},
		Skipped:         []domain.LegacySkippedItem{},
	}
	type group struct {
		project domain.LegacyProjectPlan
		marker  string
	}
	groups := map[string]*group{}
	for _, marker := range markers {
		markerPlan, discovered, skipped, err := inspectMarker(marker, definitions, workspace)
		if errors.Is(err, ErrNotGeneratedMarker) {
			plan.Skipped = append(plan.Skipped, domain.LegacySkippedItem{Marker: marker, Reason: ErrNotGeneratedMarker.Error()})
			continue
		}
		if err != nil {
			return domain.LegacyMigrationPlan{}, err
		}
		plan.Markers = append(plan.Markers, markerPlan)
		plan.Skipped = append(plan.Skipped, skipped...)
		for _, item := range discovered {
			key := item.Root + "\x00" + item.SourceRoot
			current := groups[key]
			if current == nil {
				copy := item
				copy.Entries = []domain.LegacyEntryPlan{}
				current = &group{project: copy, marker: marker}
				groups[key] = current
			}
			current.project.Entries = append(current.project.Entries, item.Entries...)
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		project := groups[key].project
		project.Entries, err = normalizeEntries(project.Entries)
		if err != nil {
			return domain.LegacyMigrationPlan{}, fmt.Errorf("legacy project %s: %w", project.Name, err)
		}
		if len(project.Entries) > 0 {
			plan.Projects = append(plan.Projects, project)
		}
	}
	return plan, nil
}

func ParseEditors(path, workspace string) ([]Definition, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open legacy editors.toml: %w", err)
	}
	defer file.Close()
	type partial struct {
		remote  string
		linkDir string
	}
	entries := map[string]*partial{}
	current := ""
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(stripComment(scanner.Text()))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("editors.toml:%d: malformed table", lineNumber)
			}
			table := strings.TrimSpace(line[1 : len(line)-1])
			if table == "repos" {
				current = ""
				continue
			}
			if !strings.HasPrefix(table, "repos.") {
				current = ""
				continue
			}
			name, err := parseTableName(strings.TrimSpace(strings.TrimPrefix(table, "repos.")))
			if err != nil {
				return nil, fmt.Errorf("editors.toml:%d: %w", lineNumber, err)
			}
			if _, exists := entries[name]; exists {
				return nil, fmt.Errorf("editors.toml:%d: duplicate repo %q", lineNumber, name)
			}
			entries[name] = &partial{}
			current = name
			continue
		}
		if current == "" {
			continue
		}
		key, raw, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("editors.toml:%d: expected key = string", lineNumber)
		}
		value, err := parseString(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("editors.toml:%d: %w", lineNumber, err)
		}
		switch strings.TrimSpace(key) {
		case "repo":
			entries[current].remote = value
		case "link_dir":
			entries[current].linkDir = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	definitions := make([]Definition, 0, len(entries))
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := entries[name]
		if entry.remote == "" || entry.linkDir == "" {
			return nil, fmt.Errorf("legacy repo %q is missing repo or link_dir", name)
		}
		if filepath.IsAbs(entry.linkDir) {
			return nil, fmt.Errorf("legacy repo %q has absolute link_dir", name)
		}
		source := filepath.Clean(filepath.Join(workspace, filepath.FromSlash(entry.linkDir)))
		relative, err := filepath.Rel(workspace, source)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("legacy repo %q link_dir escapes the workspace", name)
		}
		resolved, err := filepath.EvalSymlinks(source)
		if errors.Is(err, os.ErrNotExist) {
			// Legacy editors.toml commonly retains definitions that were never
			// linked on this machine. Discovery is driven by exact live symlinks,
			// so an absent, unused source is not a migration blocker.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("legacy repo %q source: %w", name, err)
		}
		definitions = append(definitions, Definition{Name: name, Remote: entry.remote, SourceRoot: resolved})
	}
	return definitions, nil
}

type finalizationState struct {
	Version         int                  `json:"version"`
	LegacyWorkspace string               `json:"legacy_workspace"`
	Completed       bool                 `json:"completed"`
	Markers         []finalizationMarker `json:"markers"`
	Configs         []finalizationConfig `json:"configs"`
}

type finalizationMarker struct {
	Plan            domain.LegacyMarkerPlan `json:"plan"`
	Archive         string                  `json:"archive"`
	ResidualWritten bool                    `json:"residual_written"`
	Removed         bool                    `json:"removed"`
}

type finalizationConfig struct {
	GitRoot     string `json:"git_root"`
	Origin      string `json:"origin"`
	MarkerValue string `json:"marker_value"`
	Scope       string `json:"scope"`
	Unset       bool   `json:"unset"`
}

func FinalizeMarkers(markers []domain.LegacyMarkerPlan, legacyWorkspace, dataRoot string) ([]string, error) {
	statePath := finalizationPath(dataRoot)
	if existing, err := readFinalization(dataRoot); err == nil && existing.Completed && hasNewMarkers(existing, markers) {
		data, marshalErr := json.MarshalIndent(existing, "", "  ")
		if marshalErr != nil {
			return nil, marshalErr
		}
		history := filepath.Join(dataRoot, "legacy", "finalization-history-"+time.Now().UTC().Format("20060102T150405.000000000")+".json")
		if err := store.AtomicWrite(history, append(data, '\n'), 0o600); err != nil {
			return nil, err
		}
		if err := os.Remove(statePath); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(statePath); errors.Is(err, os.ErrNotExist) {
		if err := prepareFinalization(markers, legacyWorkspace, dataRoot); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if err := ResumeFinalization(dataRoot); err != nil {
		return nil, err
	}
	state, err := readFinalization(dataRoot)
	if err != nil {
		return nil, err
	}
	archives := make([]string, 0, len(state.Markers))
	for _, marker := range state.Markers {
		archives = append(archives, marker.Archive)
	}
	return archives, nil
}

func hasNewMarkers(existing finalizationState, markers []domain.LegacyMarkerPlan) bool {
	known := map[string]bool{}
	for _, marker := range existing.Markers {
		known[filepath.Clean(marker.Plan.Path)] = true
	}
	for _, marker := range markers {
		if !known[filepath.Clean(marker.Path)] {
			return true
		}
	}
	return false
}

func FinalizeMarker(marker domain.LegacyMarkerPlan, legacyWorkspace, dataRoot string) (string, error) {
	archives, err := FinalizeMarkers([]domain.LegacyMarkerPlan{marker}, legacyWorkspace, dataRoot)
	if err != nil {
		return "", err
	}
	return archives[0], nil
}

func ResumeFinalization(dataRoot string) error {
	state, err := readFinalization(dataRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.Completed {
		return nil
	}
	for index := range state.Markers {
		marker := &state.Markers[index]
		if marker.ResidualWritten {
			continue
		}
		key := markerArchiveKey(marker.Plan.Path)
		if err := gitutil.UpdateLegacyResidual(key, marker.Plan.GitRoot, marker.Plan.Residual); err != nil {
			return err
		}
		marker.ResidualWritten = true
		if err := writeFinalization(dataRoot, state); err != nil {
			return err
		}
	}
	for index := range state.Configs {
		config := &state.Configs[index]
		if config.Unset {
			continue
		}
		if err := unsetFinalizationConfig(*config); err != nil {
			return err
		}
		config.Unset = true
		if err := writeFinalization(dataRoot, state); err != nil {
			return err
		}
	}
	for index := range state.Markers {
		marker := &state.Markers[index]
		if marker.Removed {
			continue
		}
		content, err := os.ReadFile(marker.Plan.Path)
		if err == nil {
			if !isGeneratedMarker(content) {
				return fmt.Errorf("legacy marker changed before removal: %s", marker.Plan.Path)
			}
			if err := os.Remove(marker.Plan.Path); err != nil {
				return err
			}
			if err := fsops.SyncDir(filepath.Dir(marker.Plan.Path)); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		marker.Removed = true
		if err := writeFinalization(dataRoot, state); err != nil {
			return err
		}
	}
	state.Completed = true
	return writeFinalization(dataRoot, state)
}

func prepareFinalization(markers []domain.LegacyMarkerPlan, legacyWorkspace, dataRoot string) error {
	legacyWorkspace, err := filepath.EvalSymlinks(legacyWorkspace)
	if err != nil {
		return err
	}
	state := finalizationState{Version: 1, LegacyWorkspace: legacyWorkspace, Markers: []finalizationMarker{}, Configs: []finalizationConfig{}}
	markerPaths := map[string]bool{}
	for _, marker := range markers {
		markerPaths[filepath.Clean(marker.Path)] = true
	}
	configs := map[string]finalizationConfig{}
	for _, marker := range markers {
		content, err := os.ReadFile(marker.Path)
		if err != nil {
			return err
		}
		if !isGeneratedMarker(content) {
			return fmt.Errorf("legacy marker no longer has the generated ewasd header: %s", marker.Path)
		}
		for _, relative := range marker.Entries {
			target := filepath.Join(marker.GitRoot, filepath.FromSlash(relative))
			if !targetOutsideLegacy(target, legacyWorkspace) {
				return fmt.Errorf("legacy target still active: %s", target)
			}
		}
		archiveDir := filepath.Join(dataRoot, "legacy", "markers")
		archive := filepath.Join(archiveDir, markerArchiveKey(marker.Path)+".ewasd_gitignore")
		if err := store.AtomicWrite(archive, content, 0o600); err != nil {
			return err
		}
		state.Markers = append(state.Markers, finalizationMarker{Plan: marker, Archive: archive})
		config, ok, err := finalizationConfigFor(marker.GitRoot, markerPaths)
		if err != nil {
			return err
		}
		if ok {
			configs[config.Origin] = config
		}
	}
	keys := make([]string, 0, len(configs))
	for key := range configs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		state.Configs = append(state.Configs, configs[key])
	}
	return writeFinalization(dataRoot, state)
}

func finalizationConfigFor(gitRoot string, markerPaths map[string]bool) (finalizationConfig, bool, error) {
	output, err := git(gitRoot, "config", "--show-origin", "--get", "core.excludesFile")
	if err != nil || output == "" {
		return finalizationConfig{}, false, nil
	}
	originRaw, value, found := strings.Cut(output, "\t")
	if !found {
		return finalizationConfig{}, false, errors.New("cannot parse core.excludesFile origin")
	}
	valuePath := resolveConfigPath(gitRoot, value)
	if !markerPaths[filepath.Clean(valuePath)] {
		// A global/system fallback or unrelated user config is not ours.
		return finalizationConfig{}, false, nil
	}
	origin := strings.TrimPrefix(originRaw, "file:")
	if !filepath.IsAbs(origin) {
		origin = filepath.Join(gitRoot, origin)
	}
	origin, _ = filepath.Abs(origin)
	gitDir, err := git(gitRoot, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return finalizationConfig{}, false, err
	}
	commonDir, err := git(gitRoot, "rev-parse", "--git-common-dir")
	if err != nil {
		return finalizationConfig{}, false, err
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(gitRoot, commonDir)
	}
	allowed := map[string]string{
		filepath.Clean(filepath.Join(commonDir, "config")):       "--local",
		filepath.Clean(filepath.Join(gitDir, "config")):          "--local",
		filepath.Clean(filepath.Join(gitDir, "config.worktree")): "--worktree",
	}
	scope, ok := allowed[filepath.Clean(origin)]
	if !ok {
		return finalizationConfig{}, false, fmt.Errorf("core.excludesFile marker is set outside repository config: %s", origin)
	}
	return finalizationConfig{GitRoot: gitRoot, Origin: filepath.Clean(origin), MarkerValue: filepath.Clean(valuePath), Scope: scope}, true, nil
}

func unsetFinalizationConfig(config finalizationConfig) error {
	output, err := git(config.GitRoot, "config", "--show-origin", "--get", "core.excludesFile")
	if err != nil || output == "" {
		return nil
	}
	originRaw, value, found := strings.Cut(output, "\t")
	if !found {
		return errors.New("cannot parse core.excludesFile while finalizing")
	}
	origin := strings.TrimPrefix(originRaw, "file:")
	if !filepath.IsAbs(origin) {
		origin = filepath.Join(config.GitRoot, origin)
	}
	origin, _ = filepath.Abs(origin)
	if filepath.Clean(origin) != config.Origin || filepath.Clean(resolveConfigPath(config.GitRoot, value)) != config.MarkerValue {
		// Another scope (often the user's global ignore) is now effective.
		return nil
	}
	_, err = git(config.GitRoot, "config", config.Scope, "--unset", "core.excludesFile")
	return err
}

func finalizationPath(dataRoot string) string {
	return filepath.Join(dataRoot, "legacy", "finalization.json")
}

func readFinalization(dataRoot string) (finalizationState, error) {
	data, err := os.ReadFile(finalizationPath(dataRoot))
	if err != nil {
		return finalizationState{}, err
	}
	var state finalizationState
	if err := json.Unmarshal(data, &state); err != nil {
		return finalizationState{}, err
	}
	if state.Version != 1 {
		return finalizationState{}, fmt.Errorf("unsupported legacy finalization version %d", state.Version)
	}
	return state, nil
}

func writeFinalization(dataRoot string, state finalizationState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return store.AtomicWrite(finalizationPath(dataRoot), append(data, '\n'), 0o600)
}

func markerArchiveKey(path string) string {
	digest := sha256.Sum256([]byte(path))
	return hex.EncodeToString(digest[:8])
}

func resolveConfigPath(gitRoot, value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "~/") {
		home, _ := os.UserHomeDir()
		value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
	} else if !filepath.IsAbs(value) {
		value = filepath.Join(gitRoot, value)
	}
	value, _ = filepath.Abs(value)
	return value
}

func inspectMarker(marker string, definitions []Definition, workspace string) (domain.LegacyMarkerPlan, []domain.LegacyProjectPlan, []domain.LegacySkippedItem, error) {
	content, err := os.ReadFile(marker)
	if err != nil {
		return domain.LegacyMarkerPlan{}, nil, nil, err
	}
	if !isGeneratedMarker(content) {
		return domain.LegacyMarkerPlan{}, nil, nil, ErrNotGeneratedMarker
	}
	checkout, err := gitutil.InspectCheckout(filepath.Dir(marker))
	if err != nil {
		return domain.LegacyMarkerPlan{}, nil, nil, err
	}
	markerPlan := domain.LegacyMarkerPlan{Path: marker, GitRoot: checkout.GitRoot, Entries: []string{}, Residual: []string{}}
	groups := map[string]domain.LegacyProjectPlan{}
	skipped := []domain.LegacySkippedItem{}
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || line == markerName || strings.HasSuffix(line, "/"+markerName) {
			continue
		}
		relative, err := fsops.ValidateRelative(filepath.ToSlash(line))
		if err != nil {
			skipped = append(skipped, domain.LegacySkippedItem{Marker: marker, Path: line, Reason: err.Error(), Blocking: true})
			continue
		}
		target := filepath.Join(checkout.GitRoot, filepath.FromSlash(relative))
		info, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			skipped = append(skipped, domain.LegacySkippedItem{Marker: marker, Path: relative, Reason: "stale marker entry; target is missing"})
			markerPlan.Residual = append(markerPlan.Residual, relative)
			continue
		}
		if err != nil {
			return markerPlan, nil, skipped, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			skipped = append(skipped, domain.LegacySkippedItem{Marker: marker, Path: relative, Reason: "target is not a symlink; left untouched"})
			markerPlan.Residual = append(markerPlan.Residual, relative)
			continue
		}
		// Keep every live symlink in the marker finalization inventory. A
		// rerun after an interrupted migration may already point into Go state
		// rather than the legacy workspace.
		markerPlan.Entries = append(markerPlan.Entries, relative)
		legacySource, err := resolvedLinkTarget(target)
		if err != nil {
			return markerPlan, nil, skipped, err
		}
		definition, sourceRelative, err := matchDefinition(legacySource, definitions)
		if err != nil {
			skipped = append(skipped, domain.LegacySkippedItem{Marker: marker, Path: relative, Reason: err.Error(), Blocking: true})
			continue
		}
		projectRoot, err := inferProjectRoot(target, sourceRelative)
		if err != nil {
			return markerPlan, nil, skipped, err
		}
		projectCheckout, err := gitutil.InspectCheckout(projectRoot)
		if err != nil {
			return markerPlan, nil, skipped, err
		}
		name := chooseDefinitionName(definition.SourceRoot, projectCheckout.Remote, filepath.Base(projectRoot), definitions)
		kind, err := fsops.Kind(legacySource)
		if err != nil || (kind != "file" && kind != "directory") {
			skipped = append(skipped, domain.LegacySkippedItem{Marker: marker, Path: relative, Reason: "legacy source is not a regular file or directory", Blocking: true})
			continue
		}
		if err := fsops.ValidateCopyable(legacySource); err != nil {
			skipped = append(skipped, domain.LegacySkippedItem{Marker: marker, Path: relative, Reason: "legacy source cannot be copied safely: " + err.Error(), Blocking: true})
			continue
		}
		projectRelative, err := filepath.Rel(projectRoot, target)
		if err != nil {
			return markerPlan, nil, skipped, err
		}
		projectRelative, err = fsops.ValidateRelative(filepath.ToSlash(projectRelative))
		if err != nil {
			return markerPlan, nil, skipped, err
		}
		key := projectRoot + "\x00" + definition.SourceRoot
		project := groups[key]
		if project.Root == "" {
			project = domain.LegacyProjectPlan{Name: name, Root: projectRoot, GitRoot: projectCheckout.GitRoot, Remote: projectCheckout.Remote, SourceRoot: definition.SourceRoot, Entries: []domain.LegacyEntryPlan{}}
		}
		project.Entries = append(project.Entries, domain.LegacyEntryPlan{Path: projectRelative, Kind: kind, LegacySource: legacySource, Target: target})
		groups[key] = project
	}
	projects := make([]domain.LegacyProjectPlan, 0, len(groups))
	for _, project := range groups {
		projects = append(projects, project)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Root < projects[j].Root })
	sort.Strings(markerPlan.Entries)
	sort.Strings(markerPlan.Residual)
	_ = workspace
	return markerPlan, projects, skipped, nil
}

func findMarkers(roots []string) ([]string, error) {
	seen := map[string]bool{}
	markers := []string{}
	ignored := map[string]bool{".git": true, "node_modules": true, ".venv": true, "venv": true}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				if errors.Is(walkErr, os.ErrPermission) {
					return filepath.SkipDir
				}
				return walkErr
			}
			if entry.IsDir() && path != root && ignored[entry.Name()] {
				return filepath.SkipDir
			}
			if !entry.IsDir() && entry.Name() == markerName {
				canonical, err := filepath.Abs(path)
				if err != nil {
					return err
				}
				if !seen[canonical] {
					seen[canonical] = true
					markers = append(markers, canonical)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(markers)
	return markers, nil
}

func normalizeEntries(entries []domain.LegacyEntryPlan) ([]domain.LegacyEntryPlan, error) {
	sort.Slice(entries, func(i, j int) bool {
		leftDepth := strings.Count(entries[i].Path, "/")
		rightDepth := strings.Count(entries[j].Path, "/")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return entries[i].Path < entries[j].Path
	})
	result := []domain.LegacyEntryPlan{}
	seen := map[string]bool{}
	for _, entry := range entries {
		if seen[entry.Path] {
			continue
		}
		for _, parent := range result {
			if parent.Kind == "directory" && strings.HasPrefix(entry.Path, parent.Path+"/") {
				return nil, fmt.Errorf("marker tracks both directory %s and child %s", parent.Path, entry.Path)
			}
		}
		seen[entry.Path] = true
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func matchDefinition(source string, definitions []Definition) (Definition, string, error) {
	matches := []Definition{}
	for _, definition := range definitions {
		relative, err := filepath.Rel(definition.SourceRoot, source)
		if err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			matches = append(matches, definition)
		}
	}
	if len(matches) == 0 {
		return Definition{}, "", errors.New("symlink does not point into any configured legacy source")
	}
	sort.Slice(matches, func(i, j int) bool { return len(matches[i].SourceRoot) > len(matches[j].SourceRoot) })
	chosen := matches[0]
	relative, err := filepath.Rel(chosen.SourceRoot, source)
	return chosen, filepath.ToSlash(relative), err
}

func chooseDefinitionName(sourceRoot, checkoutRemote, basename string, definitions []Definition) string {
	candidates := []Definition{}
	for _, definition := range definitions {
		if definition.SourceRoot == sourceRoot {
			candidates = append(candidates, definition)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	for _, definition := range candidates {
		if checkoutRemote != "" && gitutil.NormalizeRemote(definition.Remote) == checkoutRemote {
			return definition.Name
		}
	}
	for _, definition := range candidates {
		if strings.EqualFold(definition.Name, basename) {
			return definition.Name
		}
	}
	return candidates[0].Name
}

func inferProjectRoot(target, sourceRelative string) (string, error) {
	components := strings.Split(filepath.Clean(filepath.FromSlash(sourceRelative)), string(filepath.Separator))
	root := target
	for range components {
		root = filepath.Dir(root)
	}
	if filepath.Clean(filepath.Join(root, filepath.FromSlash(sourceRelative))) != filepath.Clean(target) {
		return "", errors.New("cannot infer project root from legacy source mapping")
	}
	return root, nil
}

func resolvedLinkTarget(path string) (string, error) {
	raw, err := os.Readlink(path)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(filepath.Dir(path), raw)
	}
	return filepath.EvalSymlinks(filepath.Clean(raw))
}

func parseTableName(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("empty repo table name")
	}
	if raw[0] == '"' || raw[0] == '\'' {
		return parseString(raw)
	}
	if strings.ContainsAny(raw, " \t[]") {
		return "", fmt.Errorf("unsupported repo table name %q", raw)
	}
	return raw, nil
}

func parseString(raw string) (string, error) {
	if len(raw) < 2 {
		return "", errors.New("expected quoted string")
	}
	if raw[0] == '\'' && raw[len(raw)-1] == '\'' {
		return raw[1 : len(raw)-1], nil
	}
	value, err := strconv.Unquote(raw)
	if err != nil {
		return "", fmt.Errorf("invalid TOML string: %w", err)
	}
	return value, nil
}

func stripComment(line string) string {
	quote := rune(0)
	escaped := false
	for index, character := range line {
		if escaped {
			escaped = false
			continue
		}
		if quote == '"' && character == '\\' {
			escaped = true
			continue
		}
		if character == '"' || character == '\'' {
			if quote == 0 {
				quote = character
			} else if quote == character {
				quote = 0
			}
			continue
		}
		if character == '#' && quote == 0 {
			return line[:index]
		}
	}
	return line
}

func canonicalDirectory(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", path)
	}
	return path, nil
}

func isGeneratedMarker(content []byte) bool {
	return strings.HasPrefix(string(content), "# Auto-generated by ewasd\n")
}

func targetOutsideLegacy(target, legacyWorkspace string) bool {
	if _, err := os.Lstat(target); err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	resolved, err := resolvedLinkTarget(target)
	if err != nil {
		return true
	}
	relative, err := filepath.Rel(legacyWorkspace, resolved)
	return err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func git(cwd string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = cwd
	output, err := command.Output()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", errors.New("git command timed out")
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
