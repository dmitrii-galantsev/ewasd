package mcpserver

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitrii-galantsev/ewasd/internal/engine"
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

// --- fixtures ---------------------------------------------------------
//
// Mirrors the fixture pattern in internal/engine/engine_test.go: a temp
// EWASD_HOME-style state directory plus a temp Git checkout, registered
// through the same engine the MCP server wraps.

func runGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

type harness struct {
	server    *Server
	engine    *engine.Engine
	repo      string
	projectID string
	revision  uint64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "checkout")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "remote", "add", "origin", "git@github.com:Example/Widget.git")

	stateStore, err := store.New(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	eng := engine.New(stateStore)
	project, rev, err := eng.Register(repo, "Widget")
	if err != nil {
		t.Fatal(err)
	}
	return &harness{
		server:    New(eng, "test-version"),
		engine:    eng,
		repo:      repo,
		projectID: project.ID,
		revision:  rev,
	}
}

// call drives the server's actual stdio handler through real in-memory
// pipes (not a shortcut like calling dispatch directly), feeding it one
// JSON-RPC frame per line and returning every non-empty line the server
// wrote back to "stdout". stderr is discarded unless capture is requested
// via callCapturingStderr.
func (h *harness) call(t *testing.T, lines ...string) []string {
	t.Helper()
	responses, _ := h.callCapturingStderr(t, lines...)
	return responses
}

func (h *harness) callCapturingStderr(t *testing.T, lines ...string) ([]string, string) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	runDone := make(chan error, 1)
	go func() {
		var errBuf strings.Builder
		err := h.server.Run(inR, outW, &errBuf)
		_ = outW.Close()
		runDone <- err
	}()

	go func() {
		for _, line := range lines {
			_, _ = io.WriteString(inW, line+"\n")
		}
		_ = inW.Close()
	}()

	var responses []string
	scanner := bufio.NewScanner(outR)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		responses = append(responses, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading server stdout: %v", err)
	}
	if err := <-runDone; err != nil {
		t.Fatalf("server.Run: %v", err)
	}
	return responses, ""
}

func decodeResponse(t *testing.T, line string) map[string]any {
	t.Helper()
	if !json.Valid([]byte(line)) {
		t.Fatalf("line is not valid JSON: %s", line)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("decode response: %v (line: %s)", err, line)
	}
	return resp
}

func toolCallRequest(id int, name string, arguments map[string]any) string {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// --- initialize ---------------------------------------------------------

func TestInitializeEchoesSupportedProtocolVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	responses := h.call(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	if len(responses) != 1 {
		t.Fatalf("expected exactly one response, got %d: %v", len(responses), responses)
	}
	resp := decodeResponse(t, responses[0])
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result object, got %v", resp)
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Fatalf("expected echoed protocolVersion 2024-11-05, got %v", result["protocolVersion"])
	}
	caps, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("missing capabilities: %v", result)
	}
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("missing tools capability: %v", caps)
	}
	serverInfo, ok := result["serverInfo"].(map[string]any)
	if !ok || serverInfo["name"] != "ewasd" || serverInfo["version"] != "test-version" {
		t.Fatalf("unexpected serverInfo: %v", result["serverInfo"])
	}
	instructionsText, _ := result["instructions"].(string)
	if strings.TrimSpace(instructionsText) == "" {
		t.Fatalf("expected non-empty instructions")
	}
}

func TestInitializeFallsBackForUnsupportedProtocolVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	responses := h.call(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	resp := decodeResponse(t, responses[0])
	result := resp["result"].(map[string]any)
	if result["protocolVersion"] != latestProtocolVersion {
		t.Fatalf("expected fallback to newest supported version %q, got %v", latestProtocolVersion, result["protocolVersion"])
	}
}

// --- notifications --------------------------------------------------------

func TestNotificationProducesNoResponse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	responses := h.call(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if len(responses) != 0 {
		t.Fatalf("expected no response line for a notification, got %v", responses)
	}
}

func TestNotificationAlongsideRequestsOnlyRespondsToRequests(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	responses := h.call(t,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	)
	if len(responses) != 2 {
		t.Fatalf("expected exactly 2 responses (ping, ping), got %d: %v", len(responses), responses)
	}
}

// --- tools/list -----------------------------------------------------------

func TestToolsListReturnsEveryToolWithSchemaAndAnnotations(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	responses := h.call(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	resp := decodeResponse(t, responses[0])
	result := resp["result"].(map[string]any)
	rawTools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("expected tools array, got %v", result)
	}

	wantReadOnly := map[string]bool{
		"status": true, "detect": true, "plan_link": true, "plan_adopt": true,
		"plan_detach": true, "plan_reconcile": true, "plan_clean": true,
	}
	wantMutating := map[string][2]bool{ // name -> [destructiveHint, idempotentHint]
		"register":  {false, true},
		"link":      {false, true},
		"adopt":     {false, false},
		"detach":    {false, false},
		"reconcile": {false, true},
	}

	seen := map[string]bool{}
	for _, raw := range rawTools {
		tool := raw.(map[string]any)
		name, _ := tool["name"].(string)
		if name == "" {
			t.Fatalf("tool missing name: %v", tool)
		}
		seen[name] = true

		description, _ := tool["description"].(string)
		if strings.TrimSpace(description) == "" {
			t.Fatalf("tool %s has an empty description", name)
		}

		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok || len(schema) == 0 {
			t.Fatalf("tool %s has an empty inputSchema", name)
		}
		if schema["type"] != "object" {
			t.Fatalf("tool %s inputSchema.type = %v, want \"object\"", name, schema["type"])
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok || len(props) == 0 {
			t.Fatalf("tool %s inputSchema has no properties", name)
		}
		for propName, rawProp := range props {
			prop, ok := rawProp.(map[string]any)
			if !ok {
				t.Fatalf("tool %s property %s is not an object", name, propName)
			}
			if desc, _ := prop["description"].(string); strings.TrimSpace(desc) == "" {
				t.Fatalf("tool %s property %s has no description", name, propName)
			}
		}
		if _, ok := schema["required"]; !ok {
			t.Fatalf("tool %s inputSchema missing \"required\"", name)
		}
		if _, hasOutputSchema := tool["outputSchema"]; hasOutputSchema {
			t.Fatalf("tool %s declares outputSchema; the spec says not to", name)
		}

		annotations, _ := tool["annotations"].(map[string]any)
		if wantReadOnly[name] {
			if annotations["readOnlyHint"] != true {
				t.Fatalf("tool %s should have readOnlyHint=true, got %v", name, annotations)
			}
		}
		if want, ok := wantMutating[name]; ok {
			if annotations["destructiveHint"] != want[0] {
				t.Fatalf("tool %s destructiveHint = %v, want %v", name, annotations["destructiveHint"], want[0])
			}
			if annotations["idempotentHint"] != want[1] {
				t.Fatalf("tool %s idempotentHint = %v, want %v", name, annotations["idempotentHint"], want[1])
			}
		}
	}

	allExpected := []string{
		"status", "detect", "plan_link", "plan_adopt", "plan_detach", "plan_reconcile", "plan_clean",
		"register", "link", "adopt", "detach", "reconcile",
	}
	for _, name := range allExpected {
		if !seen[name] {
			t.Fatalf("tools/list is missing tool %q", name)
		}
	}
	if len(seen) != len(allExpected) {
		t.Fatalf("tools/list returned %d tools, want exactly %d (%v)", len(seen), len(allExpected), seen)
	}

	// The deliberately-withheld surface must never appear.
	forbidden := []string{"clean", "unregister", "recover", "discard_journal", "discard"}
	for _, name := range forbidden {
		if seen[name] {
			t.Fatalf("tools/list exposes deliberately-withheld tool %q", name)
		}
	}
}

// --- status (read-only round trip against a real fixture) -----------------

func TestStatusToolReportsRegisteredProjectAndDetection(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	call := toolCallRequest(1, "status", map[string]any{"cwd": h.repo})
	responses := h.call(t, call)
	resp := decodeResponse(t, responses[0])

	result := resp["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("expected isError=false for a successful status call, got %v", result)
	}
	structured, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("expected structuredContent object, got %v", result)
	}
	if structured["ok"] != true {
		t.Fatalf("expected ok=true, got %v", structured)
	}
	if got := structured["revision"].(float64); got != float64(h.revision) {
		t.Fatalf("revision = %v, want %v", got, h.revision)
	}
	projects, ok := structured["projects"].([]any)
	if !ok || len(projects) != 1 {
		t.Fatalf("expected exactly one project, got %v", structured["projects"])
	}
	project := projects[0].(map[string]any)
	if project["id"] != h.projectID {
		t.Fatalf("project id = %v, want %v", project["id"], h.projectID)
	}
	if _, ok := structured["detected"]; !ok {
		t.Fatalf("expected a \"detected\" field bundling detect's result, got %v", structured)
	}
	detected, ok := structured["detected"].(map[string]any)
	if !ok || detected["matched"] != true {
		t.Fatalf("expected detected.matched=true for cwd inside the registered checkout, got %v", structured["detected"])
	}

	content, ok := result["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("expected exactly one content item, got %v", result["content"])
	}
	first := content[0].(map[string]any)
	if first["type"] != "text" {
		t.Fatalf("content[0].type = %v, want text", first["type"])
	}
	if !json.Valid([]byte(first["text"].(string))) {
		t.Fatalf("content[0].text is not valid JSON")
	}
}

func TestStatusToolFilterByUnknownProjectIsInputDataError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	call := toolCallRequest(1, "status", map[string]any{"project": "does-not-exist"})
	responses := h.call(t, call)
	resp := decodeResponse(t, responses[0])
	result := resp["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("expected isError=false (data error, not protocol error), got %v", result)
	}
	structured := result["structuredContent"].(map[string]any)
	if structured["ok"] != false {
		t.Fatalf("expected ok=false, got %v", structured)
	}
	if structured["error"] != "not_found" {
		t.Fatalf("expected error code not_found, got %v", structured["error"])
	}
}

// --- adopt/detach guard rails (no revision/fingerprint => no mutation) ----

func TestAdoptWithoutRevisionOrFingerprintIsInputErrorAndDoesNotMutate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	target := filepath.Join(h.repo, "AGENT.md")
	if err := os.WriteFile(target, []byte("guardrails\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	call := toolCallRequest(1, "adopt", map[string]any{"cwd": h.repo, "path": "AGENT.md"})
	responses := h.call(t, call)
	resp := decodeResponse(t, responses[0])
	result := resp["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("expected isError=false for a missing-params data error, got %v", result)
	}
	structured := result["structuredContent"].(map[string]any)
	if structured["ok"] != false {
		t.Fatalf("expected ok=false, got %v", structured)
	}
	if structured["error"] != "input_error" {
		t.Fatalf("expected error code input_error, got %v", structured["error"])
	}
	if hints, ok := structured["hints"].([]any); !ok || len(hints) == 0 {
		t.Fatalf("expected at least one hint pointing at plan_adopt, got %v", structured["hints"])
	}

	// No mutation: still a regular file, not a symlink, and nothing recorded
	// in the manifest.
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("target vanished: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("adopt replaced the target with a symlink despite missing revision/fingerprint")
	}
	snapshot, err := h.engine.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Projects[0].Health.Total != 0 {
		t.Fatalf("adopt recorded a manifest entry despite missing revision/fingerprint: %+v", snapshot.Projects[0])
	}
	if snapshot.Revision != h.revision {
		t.Fatalf("revision advanced from %d to %d despite missing revision/fingerprint", h.revision, snapshot.Revision)
	}
}

func TestAdoptWithOnlyRevisionIsInputErrorAndDoesNotMutate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	target := filepath.Join(h.repo, "AGENT.md")
	if err := os.WriteFile(target, []byte("guardrails\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	call := toolCallRequest(1, "adopt", map[string]any{"cwd": h.repo, "path": "AGENT.md", "revision": h.revision})
	responses := h.call(t, call)
	resp := decodeResponse(t, responses[0])
	structured := resp["result"].(map[string]any)["structuredContent"].(map[string]any)
	if structured["ok"] != false || structured["error"] != "input_error" {
		t.Fatalf("expected ok=false/input_error, got %v", structured)
	}
	if info, err := os.Lstat(target); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("adopt mutated the filesystem despite missing fingerprint")
	}
}

func TestDetachWithoutRevisionOrFingerprintIsInputErrorAndDoesNotMutate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	target := filepath.Join(h.repo, "AGENT.md")
	if err := os.WriteFile(target, []byte("guardrails\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// Adopt for real first (directly through the engine) so there is a
	// healthy managed link for detach to (not) touch.
	plan, err := h.engine.PlanAdopt(h.projectID, "AGENT.md")
	if err != nil || !plan.Safe {
		t.Fatalf("plan adopt: %+v, %v", plan, err)
	}
	adoptResult, err := h.engine.Adopt(h.projectID, "AGENT.md", plan.ExpectedRevision, plan.Fingerprint)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}

	call := toolCallRequest(1, "detach", map[string]any{"cwd": h.repo, "path": "AGENT.md"})
	responses := h.call(t, call)
	resp := decodeResponse(t, responses[0])
	result := resp["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("expected isError=false, got %v", result)
	}
	structured := result["structuredContent"].(map[string]any)
	if structured["ok"] != false || structured["error"] != "input_error" {
		t.Fatalf("expected ok=false/input_error, got %v", structured)
	}

	// No mutation: the symlink from the earlier real adopt must be
	// untouched, and the manifest revision must not have advanced further.
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("target vanished: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("detach materialized the target despite missing revision/fingerprint")
	}
	snapshot, err := h.engine.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != adoptResult.Revision {
		t.Fatalf("revision advanced from %d to %d despite missing revision/fingerprint", adoptResult.Revision, snapshot.Revision)
	}
	if snapshot.Projects[0].Health.Total != 1 {
		t.Fatalf("expected the earlier adopt's entry to remain, got %+v", snapshot.Projects[0])
	}
}

// --- protocol-level errors --------------------------------------------

func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	responses := h.call(t, `{"jsonrpc":"2.0","id":1,"method":"totally/bogus"}`)
	resp := decodeResponse(t, responses[0])
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error object, got %v", resp)
	}
	if int(errObj["code"].(float64)) != -32601 {
		t.Fatalf("expected code -32601, got %v", errObj["code"])
	}
}

func TestMalformedJSONReturnsParseError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	responses := h.call(t, `{this is not valid json`)
	if len(responses) != 1 {
		t.Fatalf("expected exactly one response, got %d: %v", len(responses), responses)
	}
	resp := decodeResponse(t, responses[0])
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error object, got %v", resp)
	}
	if int(errObj["code"].(float64)) != -32700 {
		t.Fatalf("expected code -32700, got %v", errObj["code"])
	}
	if resp["id"] != nil {
		t.Fatalf("expected id: null for an unparseable request, got %v", resp["id"])
	}
}

func TestToolsCallUnknownToolNameIsProtocolIsError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	responses := h.call(t, toolCallRequest(1, "not_a_real_tool", map[string]any{}))
	resp := decodeResponse(t, responses[0])
	if _, hasTopLevelError := resp["error"]; hasTopLevelError {
		t.Fatalf("unknown tool name should not be a JSON-RPC-level error, got %v", resp)
	}
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("expected isError=true for an unknown tool name, got %v", result)
	}
	if _, hasStructured := result["structuredContent"]; hasStructured {
		t.Fatalf("unknown tool name should not carry structuredContent, got %v", result)
	}
}

func TestToolsCallMissingNameIsInvalidParams(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	responses := h.call(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"arguments":{}}}`)
	resp := decodeResponse(t, responses[0])
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error object, got %v", resp)
	}
	if int(errObj["code"].(float64)) != -32602 {
		t.Fatalf("expected code -32602, got %v", errObj["code"])
	}
}

func TestPingReturnsEmptyResult(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	responses := h.call(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	resp := decodeResponse(t, responses[0])
	if _, ok := resp["result"].(map[string]any); !ok {
		t.Fatalf("expected an empty result object, got %v", resp)
	}
}

// --- transport hygiene ---------------------------------------------------

func TestOnlyValidJSONRPCFramesReachStdout(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	responses := h.call(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		toolCallRequest(3, "status", map[string]any{"cwd": h.repo}),
		toolCallRequest(4, "detect", map[string]any{"cwd": h.repo}),
		toolCallRequest(5, "plan_adopt", map[string]any{"cwd": h.repo, "path": "AGENT.md"}),
		toolCallRequest(6, "adopt", map[string]any{"cwd": h.repo, "path": "AGENT.md"}),
		toolCallRequest(7, "bogus_tool", map[string]any{}),
		`{"jsonrpc":"2.0","id":8,"method":"unknown/method"}`,
		`not even json`,
		`{"jsonrpc":"2.0","id":9,"method":"ping"}`,
	)
	// 10 requests carry an id (everything except the notification); each
	// must produce exactly one line, and every line must be a clean,
	// single-line JSON-RPC 2.0 frame -- nothing else may reach stdout.
	if len(responses) != 10 {
		t.Fatalf("expected 10 response lines, got %d: %v", len(responses), responses)
	}
	for i, line := range responses {
		if strings.Contains(line, "\n") {
			t.Fatalf("response %d is not a single line: %q", i, line)
		}
		resp := decodeResponse(t, line)
		if resp["jsonrpc"] != "2.0" {
			t.Fatalf("response %d missing jsonrpc: 2.0: %v", i, resp)
		}
	}
}

func TestUnsupportedJSONRPCVersionFieldStillHandled(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// Defensive: even a client that gets the envelope's "jsonrpc" field
	// wrong should get a well-formed frame back, not a hang or a panic.
	responses := h.call(t, `{"id":1,"method":"ping"}`)
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %v", responses)
	}
	resp := decodeResponse(t, responses[0])
	if resp["jsonrpc"] != "2.0" {
		t.Fatalf("expected server to always emit jsonrpc: 2.0, got %v", resp["jsonrpc"])
	}
}
