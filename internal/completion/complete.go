package completion

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dmitrii-galantsev/ewasd/internal/domain"
	"github.com/dmitrii-galantsev/ewasd/internal/engine"
)

// completeTimeout bounds the total work __complete is willing to do before
// giving up and printing nothing. Tab completion that occasionally hangs
// for a couple of seconds is tolerable; tab completion that can hang
// indefinitely is not.
const completeTimeout = 2 * time.Second

// unmanagedPaths (adopt candidates) walks the checkout breadth-first so
// shallower paths are found before we hit either bound below.
const (
	adoptMaxCandidates = 200
	adoptMaxScanned    = 5000
)

// RunComplete implements the hidden `ewasd __complete` helper described in
// the completion package doc comment. It never returns an error and never
// writes to stderr: any failure to read state, resolve a project, or walk
// the filesystem simply yields no candidates, and the whole call is bounded
// by completeTimeout so a locked or unreadable state store can never hang a
// user's shell.
func RunComplete(eng *engine.Engine, args []string, stdout io.Writer) {
	if len(args) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), completeTimeout)
	defer cancel()

	results := make(chan []string, 1)
	go func() {
		defer func() { _ = recover() }()
		results <- safeComplete(ctx, eng, args)
	}()

	select {
	case candidates := <-results:
		for _, candidate := range candidates {
			fmt.Fprintln(stdout, candidate)
		}
	case <-ctx.Done():
		// Too slow to answer within budget: print nothing rather than block
		// or dump a half-finished answer onto the command line.
	}
}

func safeComplete(ctx context.Context, eng *engine.Engine, args []string) (candidates []string) {
	defer func() {
		if recover() != nil {
			candidates = nil
		}
	}()
	return complete(ctx, eng, args)
}

// complete is the pure decision logic behind __complete: given the words
// typed so far (the last of which is the partial word being completed), it
// decides what kind of thing is being completed and returns candidates for
// it. It never returns an error; ambiguous or unresolved contexts simply
// yield no candidates.
func complete(ctx context.Context, eng *engine.Engine, args []string) []string {
	cur := args[len(args)-1]
	rest := args[:len(args)-1]

	// A dynamic value flag (--project, --root, --mode, --discard) always
	// means the same kind of value regardless of which verb it follows, so
	// check for it before anything verb-specific.
	if len(rest) > 0 {
		if values, handled := resolveDynamicFlag(eng, rest); handled {
			return values
		}
	}

	if len(rest) == 0 {
		return append([]string{}, Verbs...)
	}

	verb := rest[0]
	if strings.HasPrefix(verb, "-") {
		// No verb typed yet and the previous word wasn't a recognized
		// dynamic flag (handled above); nothing sensible to offer.
		return nil
	}
	spec, ok := verbSpecs[verb]
	if !ok {
		return nil
	}

	if strings.HasPrefix(cur, "-") {
		return remainingFlags(spec, rest)
	}

	positionals := countPositionals(rest[1:])
	if spec.positional != posNone && positionals < spec.maxPositional {
		switch spec.positional {
		case posShell:
			return append([]string{}, Shells...)
		case posDetachPath:
			return managedPaths(resolveProject(ctx, eng, rest))
		case posAdoptPath:
			return unmanagedPaths(ctx, resolveProject(ctx, eng, rest))
		}
	}
	return nil
}

// resolveDynamicFlag returns the completion candidates for the flag
// immediately preceding the word being completed, if that flag is one of
// the four whose values are resolved from live state. handled is false
// when the previous word isn't one of those flags, so the caller can fall
// through to verb/positional handling.
func resolveDynamicFlag(eng *engine.Engine, rest []string) (values []string, handled bool) {
	switch rest[len(rest)-1] {
	case "--project":
		return projectCandidates(eng), true
	case "--root":
		return rootCandidates(eng), true
	case "--workspace":
		// Global flag (see cmd/ewasd's extractWorkspaceFlag): its value is
		// always a directory, and there is no live-state list of
		// candidates worth suggesting alongside it the way --root offers
		// registered project roots.
		return []string{dirsMarker}, true
	case "--old-workspace":
		// migrate's previous data root. By definition it is a directory that
		// is no longer the configured one, so live state has nothing useful
		// to offer here either.
		return []string{dirsMarker}, true
	case "--mode":
		return []string{"untracked", "all", "ignored"}, true
	case "--discard":
		return discardCandidates(eng), true
	default:
		return nil, false
	}
}

// countPositionals counts how many non-flag, non-flag-value words appear in
// tokens (the words after the verb itself), so callers can tell whether a
// verb's single positional argument slot is already filled.
func countPositionals(tokens []string) int {
	count := 0
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if strings.HasPrefix(tok, "-") {
			if flagTakesValue(tok) {
				i++
			}
			continue
		}
		count++
	}
	return count
}

// remainingFlags lists a verb's flags that have not already been used
// earlier on the command line, so re-completing "--json" three times isn't
// offered as an option.
func remainingFlags(spec verbSpec, rest []string) []string {
	used := make(map[string]bool, len(rest))
	for _, tok := range rest {
		used[tok] = true
	}
	out := make([]string, 0, len(spec.flags))
	for _, def := range spec.flags {
		if !used[def.name] {
			out = append(out, def.name)
		}
	}
	return out
}

// projectCandidates lists registered project IDs and names for --project.
func projectCandidates(eng *engine.Engine) []string {
	state, err := eng.State()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(value string) {
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		out = append(out, value)
	}
	for _, project := range state.Projects {
		add(project.ID)
	}
	for _, project := range state.Projects {
		add(project.Name)
	}
	return out
}

// rootCandidates always leads with dirsMarker (telling the calling script
// to also run its own native directory completion for --root) and then
// offers every registered project root as a convenient literal candidate.
func rootCandidates(eng *engine.Engine) []string {
	out := []string{dirsMarker}
	state, err := eng.State()
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	for _, project := range state.Projects {
		if project.Root == "" || seen[project.Root] {
			continue
		}
		seen[project.Root] = true
		out = append(out, project.Root)
	}
	return out
}

// discardCandidates lists outstanding recovery journal IDs for --discard.
func discardCandidates(eng *engine.Engine) []string {
	snapshot, err := eng.Snapshot()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(snapshot.Recovery))
	for _, journal := range snapshot.Recovery {
		out = append(out, journal.ID)
	}
	return out
}

// findFlagValue returns the value that follows the first occurrence of any
// of names in rest, or "" if none of them appear.
func findFlagValue(rest []string, names ...string) string {
	for i := 0; i+1 < len(rest); i++ {
		for _, name := range names {
			if rest[i] == name {
				return rest[i+1]
			}
		}
	}
	return ""
}

// resolveProject figures out which project a positional path argument
// (detach's or adopt's) belongs to, the same way the CLI does: an explicit
// --project or --root on the command line wins; otherwise it falls back to
// detecting from the current working directory. Any failure yields nil,
// which callers turn into "no candidates" rather than an error.
func resolveProject(ctx context.Context, eng *engine.Engine, rest []string) *domain.Project {
	if ctx.Err() != nil {
		return nil
	}
	state, err := eng.State()
	if err != nil {
		return nil
	}
	explicit := findFlagValue(rest, "--project", "--root")
	if explicit != "" {
		if project := lookupBySelector(state, explicit); project != nil {
			return project
		}
	}
	// Falls back to detecting from cwd when explicit is "", and also
	// handles explicit selectors lookupBySelector didn't resolve directly
	// (e.g. a project name rather than an ID or a root path).
	result, err := eng.Detect(".", explicit)
	if err != nil || !result.Matched {
		return nil
	}
	id := result.ProjectID
	if id == "" {
		id = result.TemplateProjectID
	}
	return state.ProjectByID(id)
}

// lookupBySelector mirrors the engine's internal project-selector
// resolution (an exact ID, or a filesystem path that resolves to a
// registered root) so completion can match the CLI's own --root semantics
// for adopt/detach/reconcile/status without an exported copy of that
// unexported logic.
func lookupBySelector(state domain.State, selector string) *domain.Project {
	if project := state.ProjectByID(selector); project != nil {
		return project
	}
	abs, err := filepath.Abs(selector)
	if err != nil {
		return nil
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return state.ProjectByRoot(abs)
}

// managedPaths lists a project's managed entry paths, for detach's
// positional argument. This is the direct successor to the old Python
// completion's "--rm-file completes from ewasd list": it stops a user from
// detaching a path ewasd does not actually own.
func managedPaths(project *domain.Project) []string {
	if project == nil {
		return nil
	}
	out := make([]string, 0, len(project.Entries))
	for _, entry := range project.Entries {
		out = append(out, entry.Path)
	}
	return out
}

// unmanagedPaths lists files and directories under project's checkout that
// are not already managed, for adopt's positional argument. It excludes
// .git, and walks breadth-first with a bounded number of scanned nodes and
// returned candidates so a huge tree is never walked unboundedly; the
// breadth-first order also means shallower paths are always preferred over
// deeper ones once a bound is hit.
func unmanagedPaths(ctx context.Context, project *domain.Project) []string {
	if project == nil || project.Root == "" {
		return nil
	}
	managed := make(map[string]bool, len(project.Entries))
	for _, entry := range project.Entries {
		managed[entry.Path] = true
	}
	managedOrInside := func(rel string) bool {
		if managed[rel] {
			return true
		}
		for prefix := range managed {
			if strings.HasPrefix(rel, prefix+"/") {
				return true
			}
		}
		return false
	}

	type dirNode struct{ rel string }
	queue := []dirNode{{rel: ""}}
	var out []string
	scanned := 0
	for len(queue) > 0 && len(out) < adoptMaxCandidates && scanned < adoptMaxScanned {
		if ctx.Err() != nil {
			break
		}
		current := queue[0]
		queue = queue[1:]
		absDir := project.Root
		if current.rel != "" {
			absDir = filepath.Join(project.Root, current.rel)
		}
		entries, err := os.ReadDir(absDir)
		if err != nil {
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if scanned >= adoptMaxScanned || len(out) >= adoptMaxCandidates {
				break
			}
			scanned++
			name := entry.Name()
			if current.rel == "" && name == ".git" {
				continue
			}
			rel := name
			if current.rel != "" {
				rel = current.rel + "/" + name
			}
			if managedOrInside(rel) {
				continue
			}
			out = append(out, rel)
			if entry.IsDir() {
				queue = append(queue, dirNode{rel: rel})
			}
		}
	}
	return out
}
