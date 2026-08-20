package mcpserver

import (
	"encoding/json"
	"fmt"
)

// toolDef is one entry in the MCP tools/list response plus the handler that
// implements tools/call for it. Handlers receive the raw "arguments" JSON
// (never nil; orEmptyObject normalizes an absent/empty value to "{}") and
// return the full {"ok": ...} data payload described in the package doc, or
// a genuine JSON-RPC error for structurally malformed arguments.
type toolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
	Annotations map[string]any
	Handler     func(*Server, json.RawMessage) (map[string]any, *rpcError)
}

// property is a small helper for building one JSON Schema property entry.
func property(kind, description string, extra map[string]any) map[string]any {
	p := map[string]any{"type": kind, "description": description}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   required,
	}
}

const cwdDescription = "Absolute directory path to run project detection from. Defaults to the MCP server process's OWN current working directory when omitted -- that is almost never where the calling agent's user is actually working, so pass the real project directory or file path explicitly whenever you know it."

const cwdRegisterDescription = "Absolute path to the Git checkout to register (this is the checkout root itself, not a directory to detect from -- register never guesses). Defaults to the MCP server process's own current working directory when omitted, which is almost never correct; pass the checkout path explicitly."

func projectDescription(context string) map[string]any {
	return property("string",
		"Optional project selector: a registered project ID, its display name, or its checkout directory's base name (case-insensitive match). "+context,
		nil)
}

const monorepoRequiredNote = "REQUIRED when cwd alone is ambiguous -- e.g. inside a monorepo where more than one registered scope could match, or when targeting an already-registered source profile that cwd does not currently sit inside."

func (s *Server) registerTools() {
	s.tools = map[string]toolDef{}
	s.toolOrder = nil

	cwdProp := property("string", cwdDescription, nil)
	pathProp := func(action string) map[string]any {
		return property("string", fmt.Sprintf("Path to %s, relative to the resolved project's root (for example \"AGENT.md\" or \"tools/setup.sh\"). Never an absolute path and never inside .git.", action), nil)
	}
	revisionProp := property("integer", "The exact \"revision\" field from the matching plan_* tool's response for this same cwd/project/path. Must be copied unchanged -- there is no default and it is never fetched automatically. A stale value (state changed since the plan) is rejected rather than silently re-approved.", nil)
	fingerprintProp := property("string", "The exact \"fingerprint\" field from the matching plan_* tool's response for this same cwd/project/path. Must be copied unchanged -- there is no default and it is never fetched automatically. A stale value (plan or filesystem changed since the preview) is rejected rather than silently re-approved.", nil)

	s.addTool(toolDef{
		Name: "status",
		Description: "Show the full picture: every registered project with its entries and per-entry health " +
			"(linked/missing/conflict/source-missing), the current manifest revision, the data root path, and any " +
			"interrupted transactions awaiting human recovery. Also runs best-effort project detection for cwd and " +
			"includes it as \"detected\". CALL THIS FIRST, before detect -- it already covers what detect and the " +
			"recovery half of status would otherwise require two more calls to learn.",
		InputSchema: objectSchema(map[string]any{
			"cwd":     cwdProp,
			"project": projectDescription("Narrows the \"projects\" list to a single matching project; also used as the explicit selector for the bundled detection. Omit to see every registered project."),
		}, nil),
		Annotations: map[string]any{"readOnlyHint": true},
		Handler:     (*Server).handleStatus,
	})

	s.addTool(toolDef{
		Name: "detect",
		Description: "Explain which registered project (if any) matches cwd, and exactly how: an exact registered " +
			"root, a normalized Git remote plus monorepo scope match, or a unique path-component match -- including " +
			"the full step-by-step reasoning trace and any ambiguous candidates. Use this when status's bundled " +
			"detection summary isn't enough detail on its own.",
		InputSchema: objectSchema(map[string]any{
			"cwd":     cwdProp,
			"project": projectDescription("Forces detection to consider only this source profile; " + monorepoRequiredNote),
		}, nil),
		Annotations: map[string]any{"readOnlyHint": true},
		Handler:     (*Server).handleDetect,
	})

	s.addTool(toolDef{
		Name: "plan_link",
		Description: "Preview what the link tool would do at cwd: whether a new project would be registered from a " +
			"matched source profile, which already-linked entries need no change, which missing symlinks would be " +
			"created, and which existing conflicts would be left untouched. Never mutates anything. Unlike " +
			"plan_adopt/plan_detach/plan_reconcile, you do NOT need to feed this plan's revision/fingerprint back " +
			"into link -- link is safe to call directly and self-approves against a fresh plan.",
		InputSchema: objectSchema(map[string]any{
			"cwd":     cwdProp,
			"project": projectDescription("Forces detection to consider only this source profile; " + monorepoRequiredNote),
		}, nil),
		Annotations: map[string]any{"readOnlyHint": true},
		Handler:     (*Server).handlePlanLink,
	})

	s.addTool(toolDef{
		Name: "plan_adopt",
		Description: "Preview adopting \"path\" into central storage: copy it to durable storage, atomically replace " +
			"the local file/directory with an owned symlink to that copy, and record the entry in the manifest. " +
			"Never mutates anything. Returns \"revision\" and \"fingerprint\" -- pass both, byte-for-byte unchanged, " +
			"to the adopt tool to apply exactly this reviewed plan and nothing else.",
		InputSchema: objectSchema(map[string]any{
			"cwd":     cwdProp,
			"project": projectDescription(monorepoRequiredNote),
			"path":    pathProp("adopt"),
		}, []string{"path"}),
		Annotations: map[string]any{"readOnlyHint": true},
		Handler:     (*Server).handlePlanAdopt,
	})

	s.addTool(toolDef{
		Name: "plan_detach",
		Description: "Preview detaching \"path\": materialize a normal local file/directory in place of the managed " +
			"symlink, and archive (never delete) the central source. Never mutates anything. Returns \"revision\" " +
			"and \"fingerprint\" -- pass both, byte-for-byte unchanged, to the detach tool to apply exactly this " +
			"reviewed plan and nothing else.",
		InputSchema: objectSchema(map[string]any{
			"cwd":     cwdProp,
			"project": projectDescription(monorepoRequiredNote),
			"path":    pathProp("detach"),
		}, []string{"path"}),
		Annotations: map[string]any{"readOnlyHint": true},
		Handler:     (*Server).handlePlanDetach,
	})

	s.addTool(toolDef{
		Name: "plan_reconcile",
		Description: "Preview restoring any missing managed symlinks and repairing the project's .git/info/exclude " +
			"block, without ever replacing a conflicting path. Never mutates anything. Returns \"revision\" and " +
			"\"fingerprint\" -- pass both, byte-for-byte unchanged, to the reconcile tool to apply exactly this " +
			"reviewed plan and nothing else.",
		InputSchema: objectSchema(map[string]any{
			"cwd":     cwdProp,
			"project": projectDescription(monorepoRequiredNote),
		}, nil),
		Annotations: map[string]any{"readOnlyHint": true},
		Handler:     (*Server).handlePlanReconcile,
	})

	s.addTool(toolDef{
		Name: "plan_clean",
		Description: "Preview exactly what `git clean` would remove within the resolved project's scope, with every " +
			"managed entry protected by an explicit -e pattern, plus the literal git command that was run to " +
			"compute the preview. Never mutates anything, and there is deliberately no matching apply tool over MCP " +
			"-- git clean permanently deletes untracked files, so review this plan's \"candidates\" and \"command\" " +
			"and have a human run it.",
		InputSchema: objectSchema(map[string]any{
			"cwd":     cwdProp,
			"project": projectDescription("Forces detection to consider only this source profile; " + monorepoRequiredNote),
			"mode": property("string", "Which untracked/ignored files git clean would target. \"untracked\" (default) previews ordinary untracked files only; \"ignored\" previews only gitignored files (managed entries stay protected); \"all\" previews both.", map[string]any{
				"enum": []string{"untracked", "ignored", "all"},
			}),
			"directories": property("boolean", "Also preview removing untracked directories, not just files (adds -d to the underlying git clean). Defaults to false.", nil),
		}, nil),
		Annotations: map[string]any{"readOnlyHint": true},
		Handler:     (*Server).handlePlanClean,
	})

	s.addTool(toolDef{
		Name: "register",
		Description: "Register an existing Git checkout at cwd as a new ewasd project. This does NOT adopt any " +
			"files -- it only creates the manifest entry so adopt/link can be used afterward. A checkout whose root " +
			"overlaps an already-registered root is rejected rather than duplicated.",
		InputSchema: objectSchema(map[string]any{
			"cwd":  property("string", cwdRegisterDescription, nil),
			"name": property("string", "Display name for the project. Defaults to the checkout directory's base name when omitted.", nil),
		}, nil),
		Annotations: map[string]any{"destructiveHint": false, "idempotentHint": true},
		Handler:     (*Server).handleRegister,
	})

	s.addTool(toolDef{
		Name: "link",
		Description: "Create any missing managed symlinks for the project detected at cwd/project, registering a " +
			"new project first if cwd matches an existing source profile by name/remote/path but nothing is " +
			"registered at this exact location yet. Only ever fills in what's missing -- it NEVER replaces or " +
			"removes a conflicting path -- so it is safe to call repeatedly and does not require a prior plan_link " +
			"call's revision/fingerprint.",
		InputSchema: objectSchema(map[string]any{
			"cwd":     cwdProp,
			"project": projectDescription("Forces detection to consider only this source profile; " + monorepoRequiredNote),
		}, nil),
		Annotations: map[string]any{"destructiveHint": false, "idempotentHint": true},
		Handler:     (*Server).handleLink,
	})

	s.addTool(toolDef{
		Name: "adopt",
		Description: "Apply a previously reviewed plan_adopt plan. This MOVES the real local file/directory at " +
			"\"path\" into central storage and replaces it with a symlink pointing there: content is preserved " +
			"byte-for-byte and the path still resolves to the same content, but it is now a symlink rather than a " +
			"plain file. Requires the exact \"revision\" and \"fingerprint\" from a plan_adopt call made just " +
			"before this for the same cwd/project/path -- they are NEVER defaulted, auto-filled, or auto-fetched. " +
			"If the filesystem or manifest changed since that preview, this call fails safely instead of adopting " +
			"different content than was reviewed. Never overwrites a conflicting path.",
		InputSchema: objectSchema(map[string]any{
			"cwd":         cwdProp,
			"project":     projectDescription(monorepoRequiredNote),
			"path":        pathProp("adopt"),
			"revision":    revisionProp,
			"fingerprint": fingerprintProp,
		}, []string{"path", "revision", "fingerprint"}),
		Annotations: map[string]any{"destructiveHint": false, "idempotentHint": false},
		Handler:     (*Server).handleAdopt,
	})

	s.addTool(toolDef{
		Name: "detach",
		Description: "Apply a previously reviewed plan_detach plan. This turns the managed symlink at \"path\" back " +
			"into a normal local file/directory (content copied in place) and archives the central source under the " +
			"data root's archive/ directory -- the source is NEVER deleted. Requires the exact \"revision\" and " +
			"\"fingerprint\" from a plan_detach call made just before this for the same cwd/project/path -- they are " +
			"NEVER defaulted, auto-filled, or auto-fetched. If the filesystem or manifest changed since that " +
			"preview, this call fails safely instead of detaching different content than was reviewed.",
		InputSchema: objectSchema(map[string]any{
			"cwd":         cwdProp,
			"project":     projectDescription(monorepoRequiredNote),
			"path":        pathProp("detach"),
			"revision":    revisionProp,
			"fingerprint": fingerprintProp,
		}, []string{"path", "revision", "fingerprint"}),
		Annotations: map[string]any{"destructiveHint": false, "idempotentHint": false},
		Handler:     (*Server).handleDetach,
	})

	s.addTool(toolDef{
		Name: "reconcile",
		Description: "Apply a previously reviewed plan_reconcile plan: restore any missing managed symlinks and " +
			"repair the project's .git/info/exclude block. Never replaces a conflicting path. Requires the exact " +
			"\"revision\" and \"fingerprint\" from a plan_reconcile call made just before this for the same " +
			"cwd/project -- they are NEVER defaulted, auto-filled, or auto-fetched.",
		InputSchema: objectSchema(map[string]any{
			"cwd":         cwdProp,
			"project":     projectDescription(monorepoRequiredNote),
			"revision":    revisionProp,
			"fingerprint": fingerprintProp,
		}, []string{"revision", "fingerprint"}),
		Annotations: map[string]any{"destructiveHint": false, "idempotentHint": true},
		Handler:     (*Server).handleReconcile,
	})
}

func (s *Server) addTool(def toolDef) {
	s.tools[def.Name] = def
	s.toolOrder = append(s.toolOrder, def.Name)
}

func (s *Server) handleToolsList(_ json.RawMessage) (any, *rpcError) {
	tools := make([]map[string]any, 0, len(s.toolOrder))
	for _, name := range s.toolOrder {
		def := s.tools[name]
		entry := map[string]any{
			"name":        def.Name,
			"description": def.Description,
			"inputSchema": def.InputSchema,
		}
		if len(def.Annotations) > 0 {
			entry["annotations"] = def.Annotations
		}
		tools = append(tools, entry)
	}
	return map[string]any{"tools": tools}, nil
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolCallResult struct {
	Content           []toolContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError"`
}

func (s *Server) handleToolsCall(params json.RawMessage) (any, *rpcError) {
	var req struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(params) == 0 {
		return nil, &rpcError{Code: -32602, Message: "invalid params: tools/call requires a params object with a \"name\" field"}
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if req.Name == "" {
		return nil, &rpcError{Code: -32602, Message: "invalid params: \"name\" is required"}
	}

	def, ok := s.tools[req.Name]
	if !ok {
		// Unknown tool name is a protocol-level problem, not engine data:
		// isError stays true and there is no structuredContent.
		return toolCallResult{
			Content: []toolContent{{Type: "text", Text: fmt.Sprintf("Unknown tool: %q", req.Name)}},
			IsError: true,
		}, nil
	}

	payload, rpcErr := def.Handler(s, req.Arguments)
	if rpcErr != nil {
		return nil, rpcErr
	}

	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return toolCallResult{
			Content: []toolContent{{Type: "text", Text: "failed to encode tool result: " + err.Error()}},
			IsError: true,
		}, nil
	}

	return toolCallResult{
		Content:           []toolContent{{Type: "text", Text: string(encoded)}},
		StructuredContent: payload,
		IsError:           false,
	}, nil
}
