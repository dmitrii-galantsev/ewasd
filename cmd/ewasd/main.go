package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dmitrii-galantsev/ewasd/internal/domain"
	"github.com/dmitrii-galantsev/ewasd/internal/engine"
	"github.com/dmitrii-galantsev/ewasd/internal/httpapi"
	"github.com/dmitrii-galantsev/ewasd/internal/legacy"
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

const version = "1.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
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
	}
	stateStore, err := store.New(dataRoot())
	if err != nil {
		return err
	}
	domainEngine := engine.New(stateStore)
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
	case "recover":
		return recover(domainEngine, args[1:])
	case "migrate-legacy":
		return migrateLegacy(domainEngine, stateStore, args[1:])
	case "serve":
		return serve(domainEngine, args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
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

func migrateLegacy(domainEngine *engine.Engine, stateStore *store.Store, args []string) error {
	set := flag.NewFlagSet("migrate-legacy", flag.ContinueOnError)
	workspace := set.String("workspace", legacyWorkspaceDefault(), "legacy Python ewasd workspace")
	apply := set.Bool("apply", false, "copy sources, switch exact links, and retire generated markers")
	jsonOutput := set.Bool("json", false, "emit JSON")
	keepMarkers := set.Bool("keep-markers", false, "leave generated .ewasd_gitignore files after verified import")
	var scanRoots pathList
	set.Var(&scanRoots, "scan-root", "root to scan for generated .ewasd_gitignore files (repeatable)")
	if err := set.Parse(args); err != nil {
		return err
	}
	if len(scanRoots) == 0 {
		home, _ := os.UserHomeDir()
		defaultRoot := filepath.Join(home, "git")
		if info, err := os.Stat(defaultRoot); err == nil && info.IsDir() {
			scanRoots = append(scanRoots, defaultRoot)
		} else {
			scanRoots = append(scanRoots, ".")
		}
	}
	if *apply {
		if err := legacy.ResumeFinalization(stateStore.Root()); err != nil {
			return fmt.Errorf("resume legacy marker finalization: %w", err)
		}
	}
	plan, err := legacy.Discover(*workspace, scanRoots)
	if err != nil {
		return err
	}
	if !*apply {
		if *jsonOutput {
			return printJSON(plan)
		}
		printLegacyPlan(plan)
		return nil
	}
	if snapshot, err := domainEngine.Snapshot(); err == nil && len(snapshot.Recovery) > 0 {
		if messages, recoverErr := domainEngine.Recover(); recoverErr != nil {
			return fmt.Errorf("recover prior migration after %v: %w", messages, recoverErr)
		}
	}
	snapshotBefore, err := domainEngine.Snapshot()
	if err != nil {
		return err
	}
	markAlreadyMigrated(&plan, snapshotBefore)
	for _, skipped := range plan.Skipped {
		if skipped.Blocking {
			return fmt.Errorf("legacy migration is blocked at %s: %s", skipped.Path, skipped.Reason)
		}
	}
	results := []domain.ApplyResult{}
	for _, project := range plan.Projects {
		result, err := domainEngine.ImportLegacyProject(project)
		if err != nil {
			return fmt.Errorf("import %s (%s): %w", project.Name, project.Root, err)
		}
		results = append(results, result)
	}
	for {
		snapshot, err := domainEngine.Snapshot()
		if err != nil {
			return err
		}
		if len(snapshot.Recovery) == 0 {
			break
		}
		messages, err := domainEngine.Recover()
		if err != nil {
			return fmt.Errorf("recover imported links after %v: %w", messages, err)
		}
	}
	snapshot, err := domainEngine.Snapshot()
	if err != nil {
		return err
	}
	for _, projectPlan := range plan.Projects {
		var view *domain.ProjectView
		for i := range snapshot.Projects {
			if snapshot.Projects[i].Root == projectPlan.Root {
				view = &snapshot.Projects[i]
				break
			}
		}
		if view == nil || view.Health.Linked != view.Health.Total || view.Health.Linked < len(projectPlan.Entries) || view.Health.Conflicts != 0 || view.Health.Missing != 0 || view.Health.SourceMissing != 0 || !view.GitIgnoreOK {
			return fmt.Errorf("post-migration verification failed for %s", projectPlan.Root)
		}
	}
	archives := []string{}
	if !*keepMarkers {
		archives, err = legacy.FinalizeMarkers(plan.Markers, plan.LegacyWorkspace, stateStore.Root())
		if err != nil {
			return fmt.Errorf("finalize legacy markers (resume with the same command): %w", err)
		}
	}
	receipt := map[string]any{
		"ok": true, "outcome": "completed", "legacy_workspace": plan.LegacyWorkspace,
		"data_root": stateStore.Root(), "projects": results, "marker_archives": archives,
		"skipped": plan.Skipped, "completed_at": time.Now().UTC(),
	}
	receiptData, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	receiptPath := filepath.Join(stateStore.Root(), "legacy", "migration-receipt.json")
	if err := store.AtomicWrite(receiptPath, append(receiptData, '\n'), 0o600); err != nil {
		return err
	}
	if *jsonOutput {
		return printJSON(receipt)
	}
	fmt.Printf("legacy migration complete · %d project(s) · %d link(s) · receipt %s\n", len(results), countLegacyEntries(plan), receiptPath)
	return nil
}

func markAlreadyMigrated(plan *domain.LegacyMigrationPlan, snapshot domain.Snapshot) {
	markerRoots := map[string]string{}
	for _, marker := range plan.Markers {
		markerRoots[marker.Path] = marker.GitRoot
	}
	for index := range plan.Skipped {
		item := &plan.Skipped[index]
		if !item.Blocking || item.Marker == "" || item.Path == "" {
			continue
		}
		root := markerRoots[item.Marker]
		if root == "" {
			continue
		}
		target := filepath.Clean(filepath.Join(root, filepath.FromSlash(item.Path)))
		for _, project := range snapshot.Projects {
			for _, entry := range project.EntriesView {
				if filepath.Clean(entry.Target) == target && entry.Status == "linked" {
					item.Blocking = false
					item.Reason = "already migrated and healthy in Go state"
				}
			}
		}
	}
}

type pathList []string

func (values *pathList) String() string { return strings.Join(*values, ",") }
func (values *pathList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("scan root cannot be empty")
	}
	*values = append(*values, value)
	return nil
}

func printLegacyPlan(plan domain.LegacyMigrationPlan) {
	fmt.Printf("legacy migration preview · %d project(s) · %d generated marker(s)\n", len(plan.Projects), len(plan.Markers))
	for _, project := range plan.Projects {
		fmt.Printf("\n%s\n  checkout: %s\n  source:   %s\n", project.Name, project.Root, project.SourceRoot)
		for _, entry := range project.Entries {
			fmt.Printf("  %-10s %s\n", entry.Kind, entry.Path)
		}
	}
	for _, skipped := range plan.Skipped {
		label := "stale"
		if skipped.Blocking {
			label = "BLOCKING"
		}
		fmt.Printf("\n  %-8s %s · %s\n", label, skipped.Path, skipped.Reason)
	}
	fmt.Println("\npreview only; rerun with --apply after reviewing this inventory")
}

func countLegacyEntries(plan domain.LegacyMigrationPlan) int {
	total := 0
	for _, project := range plan.Projects {
		total += len(project.Entries)
	}
	return total
}

func legacyWorkspaceDefault() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "ewasd")
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

func serve(domainEngine *engine.Engine, args []string) error {
	set := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := set.String("listen", "127.0.0.1:7337", "listen address")
	tokenFlag := set.String("token", "", "bearer token override (prefer EWASD_TOKEN or the generated token file)")
	tlsCert := set.String("tls-cert", "", "PEM certificate for HTTPS")
	tlsKey := set.String("tls-key", "", "PEM private key for HTTPS")
	var allowHosts stringList
	set.Var(&allowHosts, "allow-host", "additional approved Host name or IP (repeatable; required for wildcard LAN binds)")
	if err := set.Parse(args); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		return errors.New("--tls-cert and --tls-key must be supplied together")
	}
	token, err := loadConsoleToken(dataRoot(), *tokenFlag)
	if err != nil {
		return err
	}
	_, port, _ := net.SplitHostPort(*listen)
	approvedHosts := []string{}
	if isLoopback(host) {
		approvedHosts = append(approvedHosts, "127.0.0.1", "localhost", "::1")
	} else if host != "0.0.0.0" && host != "::" && host != "[::]" {
		approvedHosts = append(approvedHosts, strings.Trim(host, "[]"))
	}
	approvedHosts = append(approvedHosts, allowHosts...)
	if len(approvedHosts) == 0 {
		return errors.New("wildcard listen requires at least one --allow-host IP or DNS name")
	}
	server, err := httpapi.New(domainEngine, token, approvedHosts...)
	if err != nil {
		return err
	}
	scheme := "http"
	if *tlsCert != "" {
		scheme = "https"
	}
	displayHost := host
	if host == "0.0.0.0" || host == "::" || host == "[::]" {
		displayHost = allowHosts[0]
	}
	fmt.Printf("ewasd console: %s://%s/?token=%s\n", scheme, net.JoinHostPort(strings.Trim(displayHost, "[]"), port), token)
	fmt.Println("pairing token is stored locally with mode 0600; the browser removes it from the address bar after loading")
	httpServer := &http.Server{
		Addr: *listen, Handler: server.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 5 * time.Minute, IdleTimeout: 60 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() {
		if *tlsCert != "" {
			serverErrors <- httpServer.ListenAndServeTLS(*tlsCert, *tlsKey)
			return
		}
		serverErrors <- httpServer.ListenAndServe()
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signals:
		fmt.Println("shutting down after active requests finish…")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		return httpServer.Shutdown(ctx)
	}
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "://") || strings.ContainsAny(value, "/?#") {
		return errors.New("allow-host must be a bare IP or DNS host without a scheme or path")
	}
	*values = append(*values, strings.Trim(value, "[]"))
	return nil
}

func loadConsoleToken(root, explicit string) (string, error) {
	if explicit == "" {
		explicit = os.Getenv("EWASD_TOKEN")
	}
	if explicit != "" {
		if len(explicit) < 24 {
			return "", errors.New("console token must contain at least 24 characters")
		}
		return explicit, nil
	}
	path := filepath.Join(root, "console.token")
	if data, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(data))
		if len(token) < 24 {
			return "", errors.New("stored console token is invalid; remove console.token and restart")
		}
		return token, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	token := hex.EncodeToString(bytes)
	if err := store.AtomicWrite(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
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

func dataRoot() string {
	if explicit := os.Getenv("EWASD_HOME"); explicit != "" {
		return explicit
	}
	// Compatibility with the parallel preview; the final replacement uses
	// EWASD_HOME and ewasd-v2 by default.
	if explicit := os.Getenv("EWASD_NEXT_HOME"); explicit != "" {
		return explicit
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "ewasd-v2")
}

func canonicalLoose(path string) string {
	abs, _ := filepath.Abs(path)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func isLoopback(host string) bool {
	if host == "localhost" || host == "" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
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
  ewasd register   --root PATH [--name NAME]
  ewasd detect     [--root PATH] [--project ID|NAME]
  ewasd link       [--root PATH] [--project ID|NAME] [--dry-run]
  ewasd clean      [--root PATH] [--project ID|NAME] [--apply --revision N --fingerprint HASH] [--mode untracked|all|ignored] [--directories]
  ewasd unregister --project ID --revision N --confirm
  ewasd status     [--root PATH|ID] [--json]
  ewasd adopt      --root PATH|ID [--revision N --apply] RELATIVE_PATH
  ewasd detach     --root PATH|ID [--revision N --apply] RELATIVE_PATH
  ewasd reconcile  --root PATH|ID [--revision N --apply]
  ewasd recover    [--apply] [--discard ID --confirm] [--json]
  ewasd migrate-legacy [--workspace PATH] [--scan-root PATH ...] [--apply]
  ewasd serve      [--listen 127.0.0.1:7337] [--token TOKEN] [--tls-cert PEM --tls-key PEM]

Destructive operations are preview-first. link is the exception: it applies
only missing symlinks and never replaces conflicts. migrate-legacy reads but
never changes the old workspace; it copies sources and retires generated marker
files in checkouts.
`)
}
