package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dmitrii-galantsev/ewasd/internal/completion"
	"github.com/dmitrii-galantsev/ewasd/internal/config"
	"github.com/dmitrii-galantsev/ewasd/internal/domain"
	"github.com/dmitrii-galantsev/ewasd/internal/engine"
	"github.com/dmitrii-galantsev/ewasd/internal/gitutil"
	"github.com/dmitrii-galantsev/ewasd/internal/mcpserver"
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

const version = "2.0.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(rawArgs []string) error {
	if len(rawArgs) == 0 {
		usage()
		return nil
	}
	if rawArgs[0] == "__complete" {
		// __complete is a machine-facing helper invoked from a live shell's
		// TAB key. It must see the *raw*, unstripped argument list —
		// including a "--workspace" flag or its still-being-typed value —
		// so the completion engine in internal/completion can recognize
		// "--workspace" as a flag name to offer and complete its value with
		// the directory marker, the same way it already does for --root.
		// Stripping it here first (the way every other command's dispatch
		// does, below) would delete the very word being completed. It must
		// also never propagate a hard error or print an "error: ..." line
		// to a user's terminal, so its own setup failures (e.g. an
		// unwritable EWASD_HOME) are swallowed here rather than returned
		// like every other command's.
		runComplete(rawArgs[1:])
		return nil
	}

	// --workspace is accepted by every subcommand that builds a store
	// (everything below except completion script generation, help, and
	// version), both before and after the subcommand name, without every
	// flag.NewFlagSet below having to declare it individually. Stripping it
	// once, here, before dispatch is what makes that possible.
	workspaceFlag, args, err := extractWorkspaceFlag(rawArgs)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "version", "--version", "-V":
		fmt.Println("ewasd", version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	case "completion":
		return completion.Run(args[1:], os.Getenv, os.Stdout)
	case "config":
		// config is intentionally handled before the store is built below:
		// its entire purpose is read-only provenance reporting, and it must
		// never create the data root as a side effect.
		return configCmd(workspaceFlag, args[1:])
	case "init":
		// init manages the data root's creation itself (including the
		// --from-git clone-then-validate path), so it must run before the
		// unconditional store.New below rather than through it.
		return initCmd(workspaceFlag, args[1:])
	}
	resolved, err := resolveWorkspace(workspaceFlag)
	if err != nil {
		return err
	}
	stateStore, err := store.New(resolved.DataRoot)
	if err != nil {
		return err
	}
	// Detection identity policy comes from the resolved configuration, so the
	// remote_keys setting reaches every code path that inspects a checkout.
	domainEngine := engine.New(stateStore).WithRemoteKeys(resolved.RemoteKeys...)
	switch args[0] {
	case "register":
		return register(domainEngine, args[1:])
	case "detect":
		return detect(domainEngine, args[1:])
	case "link":
		return link(domainEngine, args[1:])
	case "clean":
		return clean(domainEngine, args[1:])
	case "status":
		return status(domainEngine, args[1:])
	case "unregister":
		return unregister(domainEngine, args[1:])
	case "adopt":
		return adopt(domainEngine, args[1:])
	case "detach":
		return detach(domainEngine, args[1:])
	case "reconcile":
		return reconcile(domainEngine, args[1:])
	case "migrate":
		return migrate(domainEngine, args[1:])
	case "recover":
		return recover(domainEngine, args[1:])
	case "mcp":
		return runMCP(domainEngine, args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// runComplete backs the hidden `ewasd __complete` helper. It deliberately
// swallows every error instead of returning it: a completion helper wired
// into a shell's TAB key must never print a Go error or exit non-zero, so
// any failure to build the engine (an unwritable or unreadable EWASD_HOME,
// for example) simply yields no completion candidates.
func runComplete(args []string) {
	resolved, err := resolveWorkspace("")
	if err != nil {
		return
	}
	stateStore, err := store.New(resolved.DataRoot)
	if err != nil {
		return
	}
	completion.RunComplete(engine.New(stateStore), args, os.Stdout)
}

// extractWorkspaceFlag removes the first "--workspace PATH" or
// "--workspace=PATH" occurrence from args, wherever it appears, and
// returns its value plus the remaining arguments in their original order.
// This is what lets every subcommand accept --workspace both before and
// after its name without each of the ~12 flag.NewFlagSets below having to
// declare it individually.
func extractWorkspaceFlag(args []string) (value string, rest []string, err error) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--workspace":
			if i+1 >= len(args) {
				return "", nil, errors.New("flag needs an argument: --workspace")
			}
			value = args[i+1]
			i++
		case strings.HasPrefix(arg, "--workspace="):
			value = strings.TrimPrefix(arg, "--workspace=")
		default:
			rest = append(rest, arg)
		}
	}
	return value, rest, nil
}

func runMCP(domainEngine *engine.Engine, args []string) error {
	set := flag.NewFlagSet("mcp", flag.ContinueOnError)
	if err := set.Parse(args); err != nil {
		return err
	}
	return mcpserver.New(domainEngine, version).Run(os.Stdin, os.Stdout, os.Stderr)
}

func detect(domainEngine *engine.Engine, args []string) error {
	set := flag.NewFlagSet("detect", flag.ContinueOnError)
	root := set.String("root", ".", "directory to detect from")
	project := set.String("project", "", "explicit project ID/name override")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	result, err := domainEngine.Detect(*root, *project)
	if *jsonOutput {
		if jsonErr := printJSON(result); jsonErr != nil {
			return jsonErr
		}
		return err
	}
	if result.Matched {
		fmt.Printf("%s · %s confidence · %s\n  project: %s\n  target:  %s\n  source:  %s\n", result.Method, result.Confidence, result.ProjectName, empty(result.ProjectID, result.TemplateProjectID), result.TargetRoot, result.SourceID)
	} else {
		fmt.Println("no project detected")
	}
	for _, trace := range result.Trace {
		fmt.Println("  -", trace)
	}
	for _, candidate := range result.Candidates {
		fmt.Printf("  candidate: %s [%s] -> %s (%s)\n", candidate.ProjectName, candidate.ProjectID, candidate.TargetRoot, candidate.Reason)
	}
	return err
}

func link(domainEngine *engine.Engine, args []string) error {
	set := flag.NewFlagSet("link", flag.ContinueOnError)
	root := set.String("root", ".", "directory to detect and link")
	project := set.String("project", "", "explicit project ID/name override")
	dryRun := set.Bool("dry-run", false, "preview inferred links without writing")
	set.BoolVar(dryRun, "n", false, "preview inferred links without writing")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	plan, err := domainEngine.PlanLink(*root, *project)
	if err != nil {
		return err
	}
	if *dryRun {
		return printPlan(plan, *jsonOutput)
	}
	result, err := domainEngine.Link(*root, *project, plan.Fingerprint)
	return printResult(result, err, *jsonOutput)
}

func clean(domainEngine *engine.Engine, args []string) error {
	set := flag.NewFlagSet("clean", flag.ContinueOnError)
	root := set.String("root", ".", "directory to detect and clean")
	project := set.String("project", "", "explicit project ID/name override")
	dryRun := set.Bool("dry-run", false, "show exactly what git clean would remove (default)")
	set.BoolVar(dryRun, "n", false, "show exactly what git clean would remove (default)")
	apply := set.Bool("apply", false, "execute the reviewed clean plan")
	revision := set.Uint64("revision", 0, "revision printed by the clean preview")
	fingerprint := set.String("fingerprint", "", "fingerprint printed by the clean preview")
	directories := set.Bool("directories", false, "also remove untracked directories")
	set.BoolVar(directories, "d", false, "also remove untracked directories")
	mode := set.String("mode", "untracked", "clean mode: all, untracked, or ignored")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *apply && *dryRun {
		return errors.New("--apply and --dry-run are mutually exclusive")
	}
	options := engine.CleanOptions{Mode: *mode, IncludeDirectories: *directories}
	plan, err := domainEngine.PlanClean(*root, *project, options)
	if err != nil {
		return err
	}
	if !*apply {
		if *jsonOutput {
			return printJSON(plan)
		}
		printCleanPlan(plan)
		return nil
	}
	if *revision != plan.ExpectedRevision {
		return fmt.Errorf("supply --revision %d from the clean preview", plan.ExpectedRevision)
	}
	if *fingerprint != plan.Fingerprint {
		return fmt.Errorf("supply --fingerprint %s from the clean preview", plan.Fingerprint)
	}
	result, err := domainEngine.Clean(*root, *project, options, *revision, *fingerprint)
	return printResult(result, err, *jsonOutput)
}

func printCleanPlan(plan domain.CleanPlan) {
	fmt.Printf("clean preview · %s · %s · %d candidate(s)\n", plan.ProjectName, plan.Mode, len(plan.Candidates))
	for _, candidate := range plan.Candidates {
		fmt.Println("  remove ", candidate)
	}
	for _, repository := range plan.SkippedRepositories {
		fmt.Println("  skip nested repository ", repository)
	}
	fmt.Printf("protected by %d exact ewasd pattern(s)\n", len(plan.ProtectedPatterns))
	fmt.Println("command:", strings.Join(plan.Command, " "))
}

func unregister(domainEngine *engine.Engine, args []string) error {
	set := flag.NewFlagSet("unregister", flag.ContinueOnError)
	project := set.String("project", "", "project ID to unregister")
	revision := set.Uint64("revision", 0, "current manifest revision")
	confirm := set.Bool("confirm", false, "confirm unregistering an empty checkout")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *project == "" || !*confirm {
		return errors.New("unregister requires --project ID and --confirm; managed or unowned source files are never removed")
	}
	result, err := domainEngine.Unregister(*project, *revision)
	return printResult(result, err, *jsonOutput)
}

func register(domainEngine *engine.Engine, args []string) error {
	set := flag.NewFlagSet("register", flag.ContinueOnError)
	root := set.String("root", "", "absolute checkout or monorepo-scope path")
	name := set.String("name", "", "display name")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	project, revision, err := domainEngine.Register(*root, *name)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return printJSON(map[string]any{"ok": true, "revision": revision, "project": project})
	}
	fmt.Printf("registered %s\n  id:       %s\n  root:     %s\n  remote:   %s\n  revision: %d\n", project.Name, project.ID, project.Root, empty(project.Remote, "(none)"), revision)
	return nil
}

func status(domainEngine *engine.Engine, args []string) error {
	set := flag.NewFlagSet("status", flag.ContinueOnError)
	root := set.String("root", "", "filter to a registered root or project ID")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	snapshot, err := domainEngine.Snapshot()
	if err != nil {
		return err
	}
	if *jsonOutput {
		return printJSON(snapshot)
	}
	fmt.Printf("ewasd · revision %d · data %s\n", snapshot.Revision, snapshot.DataRoot)
	matched := 0
	for _, project := range snapshot.Projects {
		if *root != "" && project.ID != *root && project.Root != canonicalLoose(*root) {
			continue
		}
		matched++
		fmt.Printf("\n%s [%s]\n  %s\n  %d linked · %d missing · %d conflicts · %d source missing\n", project.Name, project.ID, project.Root, project.Health.Linked, project.Health.Missing, project.Health.Conflicts, project.Health.SourceMissing)
		for _, entry := range project.EntriesView {
			fmt.Printf("  %-14s %s\n", entry.Status, entry.Path)
		}
	}
	if matched == 0 && *root != "" {
		return engine.ErrNotFound
	}
	if len(snapshot.Recovery) > 0 {
		fmt.Printf("\nattention: %d interrupted transaction(s); run `ewasd recover`\n", len(snapshot.Recovery))
	}
	return nil
}

func adopt(domainEngine *engine.Engine, args []string) error {
	set := flag.NewFlagSet("adopt", flag.ContinueOnError)
	root := set.String("root", "", "registered root or project ID")
	apply := set.Bool("apply", false, "apply the reviewed plan")
	revision := set.Uint64("revision", 0, "revision printed by the plan")
	fingerprint := set.String("fingerprint", "", "fingerprint printed by the reviewed plan")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return errors.New("adopt requires exactly one relative path")
	}
	plan, err := domainEngine.PlanAdopt(*root, set.Arg(0))
	if err != nil {
		return err
	}
	if !*apply {
		return printPlan(plan, *jsonOutput)
	}
	if *revision != plan.ExpectedRevision {
		return fmt.Errorf("supply --revision %d from the current plan", plan.ExpectedRevision)
	}
	if *fingerprint != plan.Fingerprint {
		return fmt.Errorf("supply --fingerprint %s from the current plan", plan.Fingerprint)
	}
	if !plan.Safe || len(plan.Steps) == 0 {
		return fmt.Errorf("%w: %s", engine.ErrConflict, plan.Summary)
	}
	result, err := domainEngine.Adopt(plan.ProjectID, set.Arg(0), *revision, *fingerprint)
	return printResult(result, err, *jsonOutput)
}

func detach(domainEngine *engine.Engine, args []string) error {
	set := flag.NewFlagSet("detach", flag.ContinueOnError)
	root := set.String("root", "", "registered root or project ID")
	apply := set.Bool("apply", false, "apply the reviewed plan")
	revision := set.Uint64("revision", 0, "revision printed by the plan")
	fingerprint := set.String("fingerprint", "", "fingerprint printed by the reviewed plan")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return errors.New("detach requires exactly one relative path")
	}
	plan, err := domainEngine.PlanDetach(*root, set.Arg(0))
	if err != nil {
		return err
	}
	if !*apply {
		return printPlan(plan, *jsonOutput)
	}
	if *revision != plan.ExpectedRevision {
		return fmt.Errorf("supply --revision %d from the current plan", plan.ExpectedRevision)
	}
	if *fingerprint != plan.Fingerprint {
		return fmt.Errorf("supply --fingerprint %s from the current plan", plan.Fingerprint)
	}
	if !plan.Safe || len(plan.Steps) == 0 {
		return fmt.Errorf("%w: %s", engine.ErrConflict, plan.Summary)
	}
	result, err := domainEngine.Detach(plan.ProjectID, set.Arg(0), *revision, *fingerprint)
	return printResult(result, err, *jsonOutput)
}

// migrate repoints managed symlinks after the ewasd data root has been moved.
//
// This is the successor to the Python implementation's
// `ewasd migrate --old-workspace`. That version had to scan a directory tree
// for broken symlinks and string-replace their targets, which is why it also
// needed a --scan-dir flag. The manifest makes this a replay instead: every
// managed destination is already recorded, so the set of links to repoint is
// known exactly and no scan flag is required. A link is only repointed when it
// currently points at exactly the old root's copy of its recorded source;
// anything else stays an untouched conflict.
func migrate(domainEngine *engine.Engine, args []string) error {
	set := flag.NewFlagSet("migrate", flag.ContinueOnError)
	oldWorkspace := set.String("old-workspace", "", "previous data root the existing symlinks point at")
	project := set.String("project", "", "limit to one registered project ID or name")
	apply := set.Bool("apply", false, "apply the reviewed plan")
	revision := set.Uint64("revision", 0, "revision printed by the plan")
	fingerprint := set.String("fingerprint", "", "fingerprint printed by the reviewed plan")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *oldWorkspace == "" {
		return errors.New("--old-workspace is required: pass the previous data root that the existing symlinks point at")
	}
	plan, err := domainEngine.PlanRelocate(*oldWorkspace, *project)
	if err != nil {
		return err
	}
	if !*apply {
		return printPlan(plan, *jsonOutput)
	}
	if *revision != plan.ExpectedRevision {
		return fmt.Errorf("supply --revision %d from the current plan", plan.ExpectedRevision)
	}
	if *fingerprint != plan.Fingerprint {
		return fmt.Errorf("supply --fingerprint %s from the current plan", plan.Fingerprint)
	}
	if !plan.Safe {
		return fmt.Errorf("%w: %s", engine.ErrConflict, plan.Summary)
	}
	result, err := domainEngine.Relocate(*oldWorkspace, *project, *revision, *fingerprint)
	return printResult(result, err, *jsonOutput)
}

func reconcile(domainEngine *engine.Engine, args []string) error {
	set := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	root := set.String("root", "", "registered root or project ID")
	apply := set.Bool("apply", false, "apply the reviewed plan")
	revision := set.Uint64("revision", 0, "revision printed by the plan")
	fingerprint := set.String("fingerprint", "", "fingerprint printed by the reviewed plan")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	plan, err := domainEngine.PlanReconcile(*root)
	if err != nil {
		return err
	}
	if !*apply {
		return printPlan(plan, *jsonOutput)
	}
	if *revision != plan.ExpectedRevision {
		return fmt.Errorf("supply --revision %d from the current plan", plan.ExpectedRevision)
	}
	if *fingerprint != plan.Fingerprint {
		return fmt.Errorf("supply --fingerprint %s from the current plan", plan.Fingerprint)
	}
	if !plan.Safe {
		return fmt.Errorf("%w: %s", engine.ErrConflict, plan.Summary)
	}
	result, err := domainEngine.Reconcile(plan.ProjectID, *revision, *fingerprint)
	return printResult(result, err, *jsonOutput)
}

func recover(domainEngine *engine.Engine, args []string) error {
	set := flag.NewFlagSet("recover", flag.ContinueOnError)
	apply := set.Bool("apply", false, "resolve interrupted journals conservatively")
	discard := set.String("discard", "", "archive one unresolved journal without touching filesystem paths")
	confirm := set.Bool("confirm", false, "confirm --discard after manual filesystem inspection")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *discard != "" {
		if !*confirm {
			return errors.New("--discard requires --confirm; it only archives the journal and does not repair files")
		}
		archived, err := domainEngine.DiscardJournal(*discard)
		if err != nil {
			return err
		}
		if *jsonOutput {
			return printJSON(map[string]any{"ok": true, "outcome": "journal_archived", "archive": archived})
		}
		fmt.Println("journal archived without touching source or target:", archived)
		return nil
	}
	if !*apply {
		snapshot, err := domainEngine.Snapshot()
		if err != nil {
			return err
		}
		if *jsonOutput {
			return printJSON(map[string]any{"ok": true, "recovery": snapshot.Recovery})
		}
		if len(snapshot.Recovery) == 0 {
			fmt.Println("no interrupted transactions")
			return nil
		}
		for _, journal := range snapshot.Recovery {
			fmt.Printf("%s  %-10s %-18s %s\n", journal.ID, journal.Action, journal.Phase, journal.Path)
		}
		fmt.Println("review these records, then run `ewasd recover --apply`")
		return nil
	}
	messages, err := domainEngine.Recover()
	if *jsonOutput {
		if jsonErr := printJSON(map[string]any{"ok": err == nil, "outcome": map[bool]string{true: "completed", false: "partial_failure"}[err == nil], "messages": messages, "error": errorText(err)}); jsonErr != nil {
			return jsonErr
		}
		return err
	}
	if len(messages) == 0 {
		fmt.Println("no interrupted transactions")
	}
	for _, message := range messages {
		fmt.Println(message)
	}
	return err
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func printPlan(plan domain.Plan, jsonOutput bool) error {
	if jsonOutput {
		return printJSON(plan)
	}
	label := "SAFE"
	if !plan.Safe {
		label = "BLOCKED"
	}
	fmt.Printf("%s plan · %s · revision %d\n%s\n", label, plan.Action, plan.ExpectedRevision, plan.Summary)
	for _, step := range plan.Steps {
		fmt.Printf("  %-12s %-28s %s\n", step.Action, step.Path, step.Detail)
	}
	for _, conflict := range plan.Conflicts {
		fmt.Printf("  CONFLICT     %-28s %s\n", conflict.Path, conflict.Detail)
	}
	if plan.Safe && len(plan.Steps) > 0 {
		fmt.Printf("apply only after review: --revision %d --fingerprint %s --apply\n", plan.ExpectedRevision, plan.Fingerprint)
	}
	return nil
}

func printResult(result domain.ApplyResult, err error, jsonOutput bool) error {
	if err != nil {
		return err
	}
	if jsonOutput {
		return printJSON(result)
	}
	fmt.Printf("%s · revision %d\n", result.Summary, result.Revision)
	for _, warning := range result.Warnings {
		fmt.Println("warning:", warning)
	}
	return nil
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// Resolved describes where the data root and remote_keys came from, so
// `ewasd config` can report provenance rather than just resolved values.
type Resolved struct {
	DataRoot         string
	DataRootSource   string
	ConfigPath       string
	ConfigExists     bool
	RemoteKeys       []string
	RemoteKeysSource string
}

// Provenance source labels for Resolved. These are also the exact strings
// `ewasd config` prints and returns in --json.
const (
	sourceFlag           = "flag"
	sourceEwasdHome      = "EWASD_HOME"
	sourceEwasdNextHome  = "EWASD_NEXT_HOME"
	sourceEwasdWorkspace = "EWASD_WORKSPACE"
	sourceConfigTOML     = "config.toml"
	sourceDefault        = "default"
)

// resolveWorkspace implements ewasd's full workspace/data-root resolution
// order:
//
//  1. --workspace flag (flagValue, already stripped from argv by
//     extractWorkspaceFlag)
//  2. EWASD_HOME environment variable
//  3. EWASD_NEXT_HOME environment variable (kept in its existing position,
//     immediately after EWASD_HOME: this was a transitional alias for
//     comparing the Go rewrite against the old Python tool side by side,
//     and predates every level below)
//  4. EWASD_WORKSPACE environment variable — the *old* Python tool's name
//     for this same override, restored here for backward compatibility
//  5. the "workspace" key in $XDG_CONFIG_HOME/ewasd/config.toml
//  6. $XDG_DATA_HOME/ewasd-v2 (default ~/.local/share/ewasd-v2)
//
// Deliberately NOT restored: the old tool's legacy auto-discovery, which
// guessed at a workspace by checking for the presence of an
// "editors.toml" file at $XDG_DATA_HOME/ewasd, next to the package
// install location, and at ~/git/editor_workspaces. That heuristic is
// exactly the kind of implicit, ambient-state guessing this rewrite's
// explicit-registration model exists to remove; a workspace is either
// named explicitly (flag, env, config) or defaults predictably (XDG data
// home), never inferred from what happens to exist on disk.
//
// config.toml is loaded unconditionally (not only when a lower-priority
// source is actually needed), because remote_keys has no other resolution
// path and a malformed config.toml must be reported clearly rather than
// silently ignored on every invocation, not just the ones that happen to
// fall through to level 5.
func resolveWorkspace(flagValue string) (Resolved, error) {
	configPath := config.FilePath()
	cfg, configExists, err := config.Load(configPath)
	if err != nil {
		return Resolved{}, err
	}
	resolved := Resolved{ConfigPath: configPath, ConfigExists: configExists}

	switch {
	case flagValue != "":
		resolved.DataRoot, resolved.DataRootSource = config.ExpandHome(flagValue), sourceFlag
	case os.Getenv("EWASD_HOME") != "":
		resolved.DataRoot, resolved.DataRootSource = config.ExpandHome(os.Getenv("EWASD_HOME")), sourceEwasdHome
	case os.Getenv("EWASD_NEXT_HOME") != "":
		resolved.DataRoot, resolved.DataRootSource = config.ExpandHome(os.Getenv("EWASD_NEXT_HOME")), sourceEwasdNextHome
	case os.Getenv("EWASD_WORKSPACE") != "":
		resolved.DataRoot, resolved.DataRootSource = config.ExpandHome(os.Getenv("EWASD_WORKSPACE")), sourceEwasdWorkspace
	case cfg.Workspace != "":
		resolved.DataRoot, resolved.DataRootSource = cfg.Workspace, sourceConfigTOML
	default:
		resolved.DataRoot, resolved.DataRootSource = defaultDataRoot(), sourceDefault
	}

	if len(cfg.RemoteKeys) > 0 {
		resolved.RemoteKeys, resolved.RemoteKeysSource = cfg.RemoteKeys, sourceConfigTOML
	} else {
		resolved.RemoteKeys, resolved.RemoteKeysSource = config.DefaultRemoteKeys, sourceDefault
	}
	return resolved, nil
}

// defaultDataRoot is resolution level 6: $XDG_DATA_HOME/ewasd-v2, default
// ~/.local/share/ewasd-v2.
func defaultDataRoot() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "ewasd-v2")
}

// configCmd implements `ewasd config`: read-only reporting of resolved
// configuration and, for each value, where it came from. It must never
// create the data root as a side effect, which is exactly why it never
// calls store.New.
func configCmd(workspaceFlag string, args []string) error {
	set := flag.NewFlagSet("config", flag.ContinueOnError)
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	resolved, err := resolveWorkspace(workspaceFlag)
	if err != nil {
		return err
	}
	dataRootAbs, err := filepath.Abs(resolved.DataRoot)
	if err != nil {
		return fmt.Errorf("resolve data root: %w", err)
	}
	rootExists := isDir(dataRootAbs)
	state := readStateInfo(filepath.Join(dataRootAbs, "state.json"))

	if *jsonOutput {
		return printJSON(map[string]any{
			"data_root": map[string]any{
				"path":   dataRootAbs,
				"source": resolved.DataRootSource,
				"exists": rootExists,
			},
			"config_file": map[string]any{
				"path":   resolved.ConfigPath,
				"exists": resolved.ConfigExists,
			},
			"remote_keys": map[string]any{
				"values": resolved.RemoteKeys,
				"source": resolved.RemoteKeysSource,
			},
			"state": state,
		})
	}

	fmt.Printf("data root:   %s (%s)\n", dataRootAbs, resolved.DataRootSource)
	fmt.Printf("             exists: %s\n", yesNo(rootExists))
	fmt.Printf("config file: %s (%s)\n", resolved.ConfigPath, existsWord(resolved.ConfigExists))
	fmt.Printf("remote keys: [%s] (%s)\n", strings.Join(resolved.RemoteKeys, ", "), resolved.RemoteKeysSource)
	switch {
	case !state.Exists:
		fmt.Printf("state.json:  %s (not found)\n", state.Path)
	case state.Error != "":
		fmt.Printf("state.json:  %s (%s)\n", state.Path, state.Error)
	default:
		fmt.Printf("state.json:  %s (revision %d)\n", state.Path, *state.Revision)
	}
	return nil
}

// stateInfo is the read-only view of state.json shown by `ewasd config`.
// Revision is a pointer purely so its absence (file missing, or present
// but unparsable) is distinguishable from an honest revision 0 in --json
// output (a nil pointer omits the field entirely; see the json tag).
type stateInfo struct {
	Path     string  `json:"path"`
	Exists   bool    `json:"exists"`
	Revision *uint64 `json:"revision,omitempty"`
	Error    string  `json:"error,omitempty"`
}

// readStateInfo inspects path (a candidate state.json) without ever
// creating or modifying anything, so it is safe to call from the
// read-only `config` command.
func readStateInfo(path string) stateInfo {
	info := stateInfo{Path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return info
	}
	if err != nil {
		info.Error = "unreadable: " + err.Error()
		return info
	}
	info.Exists = true
	var partial struct {
		SchemaVersion int    `json:"schema_version"`
		Revision      uint64 `json:"revision"`
	}
	if err := json.Unmarshal(data, &partial); err != nil {
		info.Error = "does not parse: " + err.Error()
		return info
	}
	if partial.SchemaVersion != domain.SchemaVersion {
		info.Error = fmt.Sprintf("unsupported schema version %d (expected %d)", partial.SchemaVersion, domain.SchemaVersion)
		return info
	}
	revision := partial.Revision
	info.Revision = &revision
	return info
}

// initCmd implements `ewasd init [--from-git URL]`.
func initCmd(workspaceFlag string, args []string) error {
	set := flag.NewFlagSet("init", flag.ContinueOnError)
	fromGit := set.String("from-git", "", "clone the workspace from a Git repository URL")
	if err := set.Parse(args); err != nil {
		return err
	}
	resolved, err := resolveWorkspace(workspaceFlag)
	if err != nil {
		return err
	}
	root, err := filepath.Abs(resolved.DataRoot)
	if err != nil {
		return fmt.Errorf("resolve data root: %w", err)
	}
	if *fromGit != "" {
		return initFromGit(root, *fromGit)
	}
	return initFresh(root)
}

// initFresh creates the data root and its required subdirectories with
// mode 0700, idempotently, reporting whether it already existed.
func initFresh(root string) error {
	existedBefore := isDir(root)
	stateStore, err := store.New(root)
	if err != nil {
		return err
	}
	if existedBefore {
		fmt.Printf("data root already exists at %s\n", stateStore.Root())
	} else {
		fmt.Printf("initialized data root at %s\n", stateStore.Root())
	}
	return nil
}

// initFromGit bootstraps the data root by cloning url into it — how a
// second machine gets set up from an existing workspace. It refuses to
// clone into an existing non-empty root, and cleans up after itself on any
// failure so a retry starts from a clean slate.
func initFromGit(root, url string) error {
	existedBefore := isDir(root)
	if existedBefore {
		entries, err := os.ReadDir(root)
		if err != nil {
			return fmt.Errorf("inspect existing data root: %w", err)
		}
		if len(entries) > 0 {
			return fmt.Errorf("refusing to clone into non-empty data root %s; move it aside first", root)
		}
	}
	if err := gitutil.CloneRepository(url, root); err != nil {
		cleanupPartialRoot(root, existedBefore)
		return err
	}
	if err := finishFromGitInit(root); err != nil {
		cleanupPartialRoot(root, existedBefore)
		return err
	}
	fmt.Printf("initialized data root at %s from %s\n", root, url)
	return nil
}

// finishFromGitInit runs the post-clone steps: create any required
// subdirectories the cloned repository didn't have, lock down permissions,
// and validate that a cloned state.json (if any) actually parses as the
// schema version this binary understands.
func finishFromGitInit(root string) error {
	for _, dir := range []string{"profiles", "archive", "transactions", "recovery"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("chmod data root: %w", err)
	}
	return validateClonedState(root)
}

// validateClonedState reports clearly, rather than leaving a
// half-initialised root, when a cloned state.json doesn't parse as the
// expected schema version. A cloned repository with no state.json at all
// is fine — there is nothing to validate.
func validateClonedState(root string) error {
	path := filepath.Join(root, "state.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read cloned state.json: %w", err)
	}
	var state domain.State
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("cloned state.json does not parse: %w", err)
	}
	if state.SchemaVersion != domain.SchemaVersion {
		return fmt.Errorf("cloned state.json has schema version %d, expected %d", state.SchemaVersion, domain.SchemaVersion)
	}
	return nil
}

// cleanupPartialRoot removes whatever a failed clone-and-validate attempt
// left behind, so a retry of `ewasd init --from-git` starts clean. If the
// root pre-existed (empty, since initFromGit already refused a non-empty
// one), only its contents are removed, not the directory itself; if this
// attempt created the root, the whole thing is removed.
func cleanupPartialRoot(root string, existedBefore bool) {
	if !existedBefore {
		_ = os.RemoveAll(root)
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		_ = os.RemoveAll(filepath.Join(root, entry.Name()))
	}
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func existsWord(value bool) string {
	if value {
		return "found"
	}
	return "not found"
}

func canonicalLoose(path string) string {
	abs, _ := filepath.Abs(path)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func empty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func usage() {
	fmt.Print(`ewasd — explicit, journaled repository overlays

Usage:
  ewasd config     [--json]  # show resolved configuration paths and settings
  ewasd init       [--from-git URL]
  ewasd register   --root PATH [--name NAME]
  ewasd detect     [--root PATH] [--project ID|NAME]
  ewasd link       [--root PATH] [--project ID|NAME] [--dry-run]
  ewasd clean      [--root PATH] [--project ID|NAME] [--apply --revision N --fingerprint HASH] [--mode untracked|all|ignored] [--directories]
  ewasd unregister --project ID --revision N --confirm
  ewasd status     [--root PATH|ID] [--json]
  ewasd adopt      --root PATH|ID [--revision N --apply] RELATIVE_PATH
  ewasd detach     --root PATH|ID [--revision N --apply] RELATIVE_PATH
  ewasd reconcile  --root PATH|ID [--revision N --apply]
  ewasd migrate    --old-workspace PATH [--project ID|NAME] [--revision N --fingerprint HASH --apply]
  ewasd recover    [--apply] [--discard ID --confirm] [--json]
  ewasd mcp        # run a Model Context Protocol server over stdio
  ewasd completion [bash|fish|zsh] [--install]  # print or install shell completions

Global flag, accepted before or after any subcommand above except
completion/help/version:
  --workspace PATH  # overrides all other workspace/data-root resolution

Destructive operations are preview-first. link is the exception: it applies
only missing symlinks and never replaces a conflict.
`)
}
