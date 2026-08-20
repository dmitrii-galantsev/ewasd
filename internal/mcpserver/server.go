// Package mcpserver implements a hand-rolled, dependency-free MCP (Model
// Context Protocol) server for ewasd, speaking newline-delimited JSON-RPC 2.0
// over stdio. There is no MCP SDK or JSON-RPC library dependency anywhere in
// this package by design: go.mod has zero external modules and flake.nix
// relies on that (vendorHash = null). Every byte of protocol handling below
// is standard library only.
//
// Framing: exactly one complete JSON-RPC message per line. This is NOT
// LSP-style Content-Length framing. Nothing except JSON-RPC frames is ever
// written to stdout; all diagnostics go to stderr.
//
// Safety model: this server exposes the same preview-then-apply workflow the
// CLI uses. Read-only tools (status, detect, and every plan_* tool) never
// mutate anything. Mutating tools split into two groups:
//
//   - register and link are safe to call directly. link only ever creates
//     symlinks that are currently missing; it never replaces a conflicting
//     path, so calling it repeatedly is harmless.
//   - adopt, detach, and reconcile require the exact revision and fingerprint
//     returned by a matching plan_adopt / plan_detach / plan_reconcile call
//     made immediately before. These are never defaulted, auto-filled, or
//     auto-fetched by this server: the entire safety model depends on a
//     human or agent having reviewed one specific, unchanged plan before it
//     is applied. If the manifest or filesystem changed since that plan was
//     produced, the apply call fails safely instead of guessing.
//
// Deliberately NOT exposed as tools, and why:
//
//   - Applying `clean`. git clean permanently deletes untracked files and
//     directories, and there is no way to preview-and-lock the exact set the
//     way adopt/detach/reconcile do (the working tree can change between
//     preview and execution in ways a fingerprint can't fully pin down).
//     `plan_clean` still returns the exact plan, including the literal git
//     command, so a human can run it themselves after review.
//   - `unregister`. The CLI gates this behind --confirm plus an "only an
//     empty checkout" rule that is easy for a model to get subtly wrong
//     (e.g. believing a checkout is unmanaged when it merely fell out of
//     sync). Removing a project from the manifest should be a deliberate
//     human action.
//   - Applying `recover`. Crash recovery inspects and repairs interrupted,
//     partially-applied filesystem transactions. Recovering the wrong way
//     can destroy the only remaining copy of real content; it needs a human
//     looking at the actual paths on disk, not a model guessing from a
//     journal summary.
//   - Discarding journals (recover --discard). Same reasoning as recover:
//     discarding a journal without first inspecting the filesystem can hide
//     data loss instead of preventing it.
//
// status still reports outstanding recovery journals (via Snapshot), so an
// agent can always see that human attention is needed and say so, without
// being able to act on it directly.
package mcpserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/dmitrii-galantsev/ewasd/internal/engine"
)

// Supported MCP protocol versions, newest first. initialize echoes the
// client's requested version when it is one of these, and otherwise falls
// back to the newest one this server understands.
const (
	latestProtocolVersion = "2025-06-18"
)

var supportedProtocolVersions = []string{"2025-06-18", "2024-11-05"}

const instructions = `ewasd manages symlinked overlays between a durable central file store and one or more registered Git checkouts.

Start every session with the status tool. It reports every registered project, per-entry health (linked/missing/conflict/source-missing), the current manifest revision, the data root path, and any interrupted transactions awaiting human recovery -- and it already runs best-effort project detection for cwd, so a separate detect call is rarely needed.

Mutations that touch real files -- adopt, detach, reconcile -- are strictly preview-then-apply: call the matching plan_* tool first, review its steps and conflicts, then call the apply tool with the EXACT revision and fingerprint the plan returned. These values are never defaulted or auto-fetched; if the manifest or filesystem changed since the preview, the apply call fails safely instead of guessing.

link is the one exception: it only ever creates missing symlinks and never touches a conflict, so it is safe to call directly without a plan round-trip. register only creates a manifest entry; it never adopts files.

Applying git clean, unregistering a project, running crash recovery, and discarding recovery journals are intentionally not exposed here. Each is either irreversible or requires a human to look at real files on disk before acting; plan_clean still shows exactly what git clean would remove so a human can run it themselves.`

// Server is a stateless-between-calls MCP server bound to one engine
// instance (and therefore one ewasd data root).
type Server struct {
	engine  *engine.Engine
	version string

	tools     map[string]toolDef
	toolOrder []string

	outMu sync.Mutex
}

// New builds an MCP server around an already-constructed engine. version is
// reported verbatim as the MCP serverInfo.version (callers should pass the
// CLI's own version constant so `ewasd mcp` and `ewasd version` never
// disagree).
func New(eng *engine.Engine, version string) *Server {
	s := &Server{engine: eng, version: version}
	s.registerTools()
	return s
}

// rpcRequest is the subset of a JSON-RPC 2.0 request/notification this
// server needs to read. ID is kept as raw JSON so it can be echoed back
// byte-for-byte (numbers, strings, or an explicit null all round-trip), and
// so a present-but-null id can be distinguished from an entirely absent one
// (a notification never has an "id" member at all).
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Run drives the server over in/out using newline-delimited JSON-RPC 2.0
// framing (one full message per line; not LSP-style Content-Length framing).
// It returns nil on a clean stdin EOF. Diagnostics (never protocol frames)
// are written to errOut.
func (s *Server) Run(in io.Reader, out io.Writer, errOut io.Writer) error {
	reader := bufio.NewReaderSize(in, 64*1024)
	writer := bufio.NewWriter(out)

	for {
		line, readErr := reader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			s.handleLine(trimmed, writer, errOut)
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

func (s *Server) handleLine(line string, writer *bufio.Writer, errOut io.Writer) {
	var req rpcRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		s.writeResponse(writer, errOut, rpcResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage("null"),
			Error:   &rpcError{Code: -32700, Message: "parse error: " + err.Error()},
		})
		return
	}

	isNotification := req.ID == nil

	result, rpcErr := s.dispatch(req.Method, req.Params)
	if isNotification {
		// Never reply to a message with no "id" -- it is a notification.
		return
	}

	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result = result
	}
	s.writeResponse(writer, errOut, resp)
}

func (s *Server) dispatch(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return s.handleInitialize(params)
	case "notifications/initialized":
		// Client acknowledgement that initialization finished. Nothing to do,
		// and as a notification it never receives a response anyway.
		return nil, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return s.handleToolsList(params)
	case "tools/call":
		return s.handleToolsCall(params)
	default:
		return nil, &rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", method)}
	}
}

func (s *Server) handleInitialize(params json.RawMessage) (any, *rpcError) {
	var req struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		// Malformed initialize params are not worth failing the handshake
		// over; fall through to the newest supported version.
		_ = json.Unmarshal(params, &req)
	}
	version := latestProtocolVersion
	for _, supported := range supportedProtocolVersions {
		if supported == req.ProtocolVersion {
			version = req.ProtocolVersion
			break
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "ewasd",
			"version": s.version,
		},
		"instructions": instructions,
	}, nil
}

func (s *Server) writeResponse(writer *bufio.Writer, errOut io.Writer, resp rpcResponse) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	encoded, err := json.Marshal(resp)
	if err != nil {
		fmt.Fprintln(errOut, "mcpserver: failed to encode response:", err)
		return
	}
	if _, err := writer.Write(encoded); err != nil {
		fmt.Fprintln(errOut, "mcpserver: failed to write response:", err)
		return
	}
	if _, err := writer.WriteString("\n"); err != nil {
		fmt.Fprintln(errOut, "mcpserver: failed to write response newline:", err)
		return
	}
	if err := writer.Flush(); err != nil {
		fmt.Fprintln(errOut, "mcpserver: failed to flush response:", err)
	}
}

// processWorkingDirectory returns the MCP server process's own current
// working directory, used only as the last-resort default when a tool call
// omits cwd entirely.
func processWorkingDirectory() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
