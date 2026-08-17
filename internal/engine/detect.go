package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/dmitrii-galantsev/ewasd/internal/domain"
	"github.com/dmitrii-galantsev/ewasd/internal/gitutil"
)

var ErrAmbiguousDetection = errors.New("repository detection is ambiguous")

func (e *Engine) Detect(cwd, explicit string) (domain.DetectionResult, error) {
	state, err := e.store.Read()
	if err != nil {
		return domain.DetectionResult{}, err
	}
	return detectFromState(state, cwd, explicit)
}

func detectFromState(state domain.State, cwd, explicit string) (domain.DetectionResult, error) {
	canonical, err := canonicalDirectory(cwd)
	if err != nil {
		return domain.DetectionResult{}, err
	}
	checkout, err := gitutil.InspectCheckout(canonical)
	if err != nil {
		return domain.DetectionResult{}, err
	}
	result := domain.DetectionResult{
		CWD: canonical, GitRoot: checkout.GitRoot, Remote: checkout.Remote,
		Remotes: checkout.Remotes, Trace: []string{}, Candidates: []domain.DetectionCandidate{},
	}

	if explicit != "" {
		matches := projectsBySelector(state, explicit)
		if len(matches) == 0 {
			return result, fmt.Errorf("%w: no project matches %q", ErrNotFound, explicit)
		}
		matches = dedupeSources(matches)
		if len(matches) != 1 {
			result.Candidates = projectCandidates(matches, canonical, "explicit", "multiple registered source profiles match the selector")
			return result, ErrAmbiguousDetection
		}
		template := matches[0]
		if existing := containingProject(state, canonical); existing != nil {
			if existing.SourceID != template.SourceID {
				result.Candidates = []domain.DetectionCandidate{
					{ProjectID: existing.ID, ProjectName: existing.Name, SourceID: existing.SourceID, TargetRoot: existing.Root, Method: "registered-root", Reason: "cwd belongs to this registered project"},
					{ProjectID: template.ID, ProjectName: template.Name, SourceID: template.SourceID, TargetRoot: canonical, Method: "explicit", Reason: "explicit selector resolves to a different source profile"},
				}
				return result, fmt.Errorf("%w: explicit project %q does not own cwd", ErrAmbiguousDetection, explicit)
			}
			return detectionMatch(result, *existing, existing.Root, "explicit", "explicit", true), nil
		}
		return detectionMatch(result, template, canonical, "explicit", "explicit", false), nil
	}

	if existing := containingProject(state, canonical); existing != nil {
		result.Trace = append(result.Trace, "matched the deepest registered root containing cwd")
		return detectionMatch(result, *existing, existing.Root, "registered-root", "exact", true), nil
	}
	result.Trace = append(result.Trace, "cwd is not inside a registered project root")

	remoteMatches := []detectionTemplate{}
	remoteSet := make(map[string]bool, len(checkout.Remotes))
	for _, remote := range checkout.Remotes {
		remoteSet[remote] = true
	}
	for _, project := range state.Projects {
		if project.Remote == "" || !remoteSet[project.Remote] {
			continue
		}
		scope, err := filepath.Rel(project.GitRoot, project.Root)
		if err != nil || scope == ".." || strings.HasPrefix(scope, ".."+string(filepath.Separator)) {
			continue
		}
		target := filepath.Clean(filepath.Join(checkout.GitRoot, scope))
		if !isInside(canonical, target) && scope != "." {
			continue
		}
		remoteMatches = append(remoteMatches, detectionTemplate{Project: project, TargetRoot: target, Reason: "normalized Git remote and registered monorepo scope match"})
	}
	remoteMatches = dedupeDetectionTemplates(remoteMatches)
	if len(remoteMatches) == 1 {
		result.Trace = append(result.Trace, remoteMatches[0].Reason)
		return detectionMatch(result, remoteMatches[0].Project, remoteMatches[0].TargetRoot, "remote", "high", false), nil
	}
	if len(remoteMatches) > 1 {
		result.Candidates = templateCandidates(remoteMatches, "remote")
		result.Trace = append(result.Trace, "multiple source profiles or monorepo scopes match the Git remote")
		return result, ErrAmbiguousDetection
	}
	result.Trace = append(result.Trace, "no unique registered source profile matches any Git remote")

	pathMatches := pathTemplates(state, checkout.GitRoot, canonical)
	if len(pathMatches) == 1 {
		result.Trace = append(result.Trace, pathMatches[0].Reason)
		return detectionMatch(result, pathMatches[0].Project, pathMatches[0].TargetRoot, "path", "medium", false), nil
	}
	if len(pathMatches) > 1 {
		result.Candidates = templateCandidates(pathMatches, "path")
		result.Trace = append(result.Trace, "multiple registered names match components in the checkout path")
		return result, ErrAmbiguousDetection
	}
	result.Trace = append(result.Trace, "no registered project name matches a checkout path component")
	return result, nil
}

type detectionTemplate struct {
	Project    domain.Project
	TargetRoot string
	Reason     string
}

func detectionMatch(result domain.DetectionResult, project domain.Project, target, method, confidence string, existing bool) domain.DetectionResult {
	result.Matched = true
	result.Method = method
	result.Confidence = confidence
	result.TargetRoot = target
	result.ProjectName = project.Name
	result.SourceID = project.SourceID
	if method == "registered-root" || method == "remote" || containsRemote(result.Remotes, project.Remote) {
		result.Remote = project.Remote
	}
	if existing {
		result.ProjectID = project.ID
	} else {
		result.TemplateProjectID = project.ID
	}
	return result
}

func containsRemote(remotes []string, wanted string) bool {
	for _, remote := range remotes {
		if remote == wanted && wanted != "" {
			return true
		}
	}
	return false
}

func containingProject(state domain.State, cwd string) *domain.Project {
	index := -1
	depth := -1
	for i := range state.Projects {
		if !isInside(cwd, state.Projects[i].Root) {
			continue
		}
		candidateDepth := len(strings.Split(filepath.Clean(state.Projects[i].Root), string(filepath.Separator)))
		if candidateDepth > depth {
			index, depth = i, candidateDepth
		}
	}
	if index < 0 {
		return nil
	}
	return &state.Projects[index]
}

func projectsBySelector(state domain.State, selector string) []domain.Project {
	normalized := normalizeName(selector)
	result := []domain.Project{}
	for _, project := range state.Projects {
		if project.ID == selector || normalizeName(project.Name) == normalized || normalizeName(filepath.Base(project.Root)) == normalized {
			result = append(result, project)
		}
	}
	return result
}

func dedupeSources(projects []domain.Project) []domain.Project {
	seen := map[string]bool{}
	result := []domain.Project{}
	for _, project := range projects {
		key := project.SourceID
		if !seen[key] {
			seen[key] = true
			result = append(result, project)
		}
	}
	return result
}

func dedupeDetectionTemplates(templates []detectionTemplate) []detectionTemplate {
	seen := map[string]bool{}
	result := []detectionTemplate{}
	for _, template := range templates {
		key := template.Project.SourceID + "\x00" + filepath.Clean(template.TargetRoot)
		if !seen[key] {
			seen[key] = true
			result = append(result, template)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].TargetRoot != result[j].TargetRoot {
			return result[i].TargetRoot < result[j].TargetRoot
		}
		return result[i].Project.SourceID < result[j].Project.SourceID
	})
	return result
}

func pathTemplates(state domain.State, gitRoot, cwd string) []detectionTemplate {
	relative, err := filepath.Rel(gitRoot, cwd)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil
	}
	parts := []string{filepath.Base(gitRoot)}
	if relative != "." {
		parts = append(parts, strings.Split(relative, string(filepath.Separator))...)
	}
	result := []detectionTemplate{}
	for index, part := range parts {
		partName := normalizeName(part)
		for _, project := range state.Projects {
			if partName != normalizeName(project.Name) && partName != normalizeName(filepath.Base(project.Root)) {
				continue
			}
			target := gitRoot
			if index > 0 {
				target = filepath.Join(append([]string{gitRoot}, parts[1:index+1]...)...)
			}
			result = append(result, detectionTemplate{Project: project, TargetRoot: target, Reason: fmt.Sprintf("path component %q uniquely matches registered project %q", part, project.Name)})
		}
	}
	return dedupeDetectionTemplates(result)
}

func projectCandidates(projects []domain.Project, target, method, reason string) []domain.DetectionCandidate {
	result := make([]domain.DetectionCandidate, 0, len(projects))
	for _, project := range projects {
		result = append(result, domain.DetectionCandidate{ProjectID: project.ID, ProjectName: project.Name, SourceID: project.SourceID, TargetRoot: target, Method: method, Reason: reason})
	}
	return result
}

func templateCandidates(templates []detectionTemplate, method string) []domain.DetectionCandidate {
	result := make([]domain.DetectionCandidate, 0, len(templates))
	for _, template := range templates {
		result = append(result, domain.DetectionCandidate{ProjectID: template.Project.ID, ProjectName: template.Project.Name, SourceID: template.Project.SourceID, TargetRoot: template.TargetRoot, Method: method, Reason: template.Reason})
	}
	return result
}

func canonicalDirectory(path string) (string, error) {
	if path == "" {
		path = "."
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", resolved)
	}
	return resolved, nil
}

func isInside(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

func normalizeName(value string) string {
	var output strings.Builder
	for _, character := range strings.ToLower(value) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			output.WriteRune(character)
		}
	}
	return output.String()
}
