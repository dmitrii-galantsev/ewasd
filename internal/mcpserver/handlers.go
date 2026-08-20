package mcpserver

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dmitrii-galantsev/ewasd/internal/domain"
	"github.com/dmitrii-galantsev/ewasd/internal/engine"
)

// baseArgs is embedded by every tool's argument struct. cwd is intentionally
// left blank rather than defaulted here: for tools backed by engine.Detect
// (detect, plan_link, link, plan_clean) an empty cwd is handled correctly
// downstream (it means "the process's own working directory", same as the
// CLI's "." default). Tools that need a concrete non-empty path before
// calling the engine (register, and the project-resolving plan_*/apply
// tools) resolve the default explicitly themselves.
type baseArgs struct {
	CWD     string `json:"cwd"`
	Project string `json:"project"`
}

type pathArgs struct {
	baseArgs
	Path string `json:"path"`
}

type cleanArgs struct {
	baseArgs
	Mode        string `json:"mode"`
	Directories bool   `json:"directories"`
}

type applyArgs struct {
	baseArgs
	Revision    *uint64 `json:"revision"`
	Fingerprint *string `json:"fingerprint"`
}

type applyPathArgs struct {
	pathArgs
	Revision    *uint64 `json:"revision"`
	Fingerprint *string `json:"fingerprint"`
}

type registerArgs struct {
	CWD  string `json:"cwd"`
	Name string `json:"name"`
}

// orEmptyObject makes an empty/absent "arguments" value behave like `{}` so
// every field decodes to its zero value instead of failing to unmarshal.
func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

func decodeArgs(raw json.RawMessage, dst any) *rpcError {
	if err := json.Unmarshal(orEmptyObject(raw), dst); err != nil {
		return &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	return nil
}

// okPayload marshals an engine result struct (domain.DetectionResult,
// domain.Plan, domain.CleanPlan, domain.ApplyResult, domain.Snapshot, ...)
// through JSON into a plain map and stamps "ok": true onto it, so every
// successful tool result carries the same envelope without hand-copying
// every field of every domain type.
func okPayload(value any) map[string]any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return errorPayload("internal_error", "failed to encode result: "+err.Error())
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return errorPayload("internal_error", "failed to encode result: "+err.Error())
	}
	payload["ok"] = true
	return payload
}

// missingApplyParams reports which of the two mandatory apply-tool
// parameters (revision, fingerprint) are absent, or "" if both are present.
// Both must come from a matching plan_* call; there is no default and no
// auto-fetch.
func missingApplyParams(revision *uint64, fingerprint *string) string {
	haveRevision := revision != nil
	haveFingerprint := fingerprint != nil && *fingerprint != ""
	switch {
	case !haveRevision && !haveFingerprint:
		return "\"revision\" and \"fingerprint\" are both required and must be copied from the matching plan_* tool's response; they are never inferred automatically"
	case !haveRevision:
		return "\"revision\" is required and must be copied from the matching plan_* tool's response; it is never inferred automatically"
	default:
		return "\"fingerprint\" is required and must be copied from the matching plan_* tool's response; it is never inferred automatically"
	}
}

// resolveProjectID turns a (cwd, project) pair into a concrete, already
// registered project ID using the same detection engine.Detect uses for
// detect/plan_link/link, so name lookups, monorepo scoping, and remote
// matching all behave identically across every tool. It returns a non-nil
// data-error payload (never a Go error) when resolution fails, ready to
// return directly from a handler.
func (s *Server) resolveProjectID(cwd, project string) (string, map[string]any) {
	detection, err := s.engine.Detect(cwd, project)
	if err != nil {
		return "", classifyEngineError(err)
	}
	if !detection.Matched {
		return "", errorPayload("not_found",
			"no registered project matches this cwd/project",
			"Call the \"detect\" tool for the full reasoning trace, or \"register\" first if this checkout has never been registered.")
	}
	if detection.ProjectID == "" {
		return "", errorPayload("not_registered",
			fmt.Sprintf("cwd matches source profile %q (via %s detection) but nothing is registered at this exact path yet", detection.ProjectName, detection.Method),
			"Call the \"link\" tool to register this checkout at this exact path, then retry.")
	}
	return detection.ProjectID, nil
}

func projectMatchesSelector(project domain.Project, selector string) bool {
	if selector == "" {
		return true
	}
	if project.ID == selector {
		return true
	}
	if strings.EqualFold(project.Name, selector) {
		return true
	}
	if strings.EqualFold(filepath.Base(project.Root), selector) {
		return true
	}
	return canonicalPathLoose(selector) == project.Root
}

func canonicalPathLoose(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// --- status -----------------------------------------------------------

func (s *Server) handleStatus(raw json.RawMessage) (map[string]any, *rpcError) {
	var args baseArgs
	if rpcErr := decodeArgs(raw, &args); rpcErr != nil {
		return nil, rpcErr
	}
	snapshot, err := s.engine.Snapshot()
	if err != nil {
		return classifyEngineError(err), nil
	}
	payload := okPayload(snapshot)
	if args.Project != "" {
		filtered := make([]domain.ProjectView, 0, len(snapshot.Projects))
		for _, view := range snapshot.Projects {
			if projectMatchesSelector(view.Project, args.Project) {
				filtered = append(filtered, view)
			}
		}
		if len(filtered) == 0 {
			return errorPayload("not_found",
				fmt.Sprintf("no registered project matches %q", args.Project),
				"Call \"status\" without a \"project\" filter to see every registered project's ID and name."), nil
		}
		encoded, _ := json.Marshal(filtered)
		var asAny []any
		_ = json.Unmarshal(encoded, &asAny)
		payload["projects"] = asAny
	}
	// Best-effort detection for cwd, bundled in so an agent rarely needs a
	// separate detect call. A detection failure (e.g. cwd is not inside any
	// Git checkout) is reported alongside the rest of status, not fatal to
	// it.
	detection, detectErr := s.engine.Detect(args.CWD, args.Project)
	if detectErr != nil {
		payload["detected"] = nil
		payload["detection_error"] = detectErr.Error()
	} else {
		encoded, _ := json.Marshal(detection)
		var asAny any
		_ = json.Unmarshal(encoded, &asAny)
		payload["detected"] = asAny
	}
	return payload, nil
}

// --- detect -------------------------------------------------------------

func (s *Server) handleDetect(raw json.RawMessage) (map[string]any, *rpcError) {
	var args baseArgs
	if rpcErr := decodeArgs(raw, &args); rpcErr != nil {
		return nil, rpcErr
	}
	result, err := s.engine.Detect(args.CWD, args.Project)
	if err != nil {
		return classifyEngineError(err), nil
	}
	return okPayload(result), nil
}

// --- plan_link / link -----------------------------------------------------

func (s *Server) handlePlanLink(raw json.RawMessage) (map[string]any, *rpcError) {
	var args baseArgs
	if rpcErr := decodeArgs(raw, &args); rpcErr != nil {
		return nil, rpcErr
	}
	plan, err := s.engine.PlanLink(args.CWD, args.Project)
	if err != nil {
		return classifyEngineError(err), nil
	}
	return okPayload(plan), nil
}

func (s *Server) handleLink(raw json.RawMessage) (map[string]any, *rpcError) {
	var args baseArgs
	if rpcErr := decodeArgs(raw, &args); rpcErr != nil {
		return nil, rpcErr
	}
	// Engine.Link self-approves against a freshly recomputed plan when no
	// fingerprint is supplied; link is defined to only ever add missing
	// symlinks and never replace a conflict, so no external revision/
	// fingerprint review is required here (unlike adopt/detach/reconcile).
	result, err := s.engine.Link(args.CWD, args.Project, "")
	if err != nil {
		return classifyEngineError(err), nil
	}
	return okPayload(result), nil
}

// --- plan_clean -----------------------------------------------------------

func (s *Server) handlePlanClean(raw json.RawMessage) (map[string]any, *rpcError) {
	var args cleanArgs
	if rpcErr := decodeArgs(raw, &args); rpcErr != nil {
		return nil, rpcErr
	}
	mode := args.Mode
	if mode == "" {
		mode = "untracked"
	}
	plan, err := s.engine.PlanClean(args.CWD, args.Project, engine.CleanOptions{
		Mode:               mode,
		IncludeDirectories: args.Directories,
	})
	if err != nil {
		return classifyEngineError(err), nil
	}
	return okPayload(plan), nil
}

// --- plan_adopt / adopt ---------------------------------------------------

func (s *Server) handlePlanAdopt(raw json.RawMessage) (map[string]any, *rpcError) {
	var args pathArgs
	if rpcErr := decodeArgs(raw, &args); rpcErr != nil {
		return nil, rpcErr
	}
	if args.Path == "" {
		return errorPayload("input_error", "\"path\" is required: the project-root-relative file or directory to adopt"), nil
	}
	projectID, errPayload := s.resolveProjectID(args.CWD, args.Project)
	if errPayload != nil {
		return errPayload, nil
	}
	plan, err := s.engine.PlanAdopt(projectID, args.Path)
	if err != nil {
		return classifyEngineError(err), nil
	}
	return okPayload(plan), nil
}

func (s *Server) handleAdopt(raw json.RawMessage) (map[string]any, *rpcError) {
	var args applyPathArgs
	if rpcErr := decodeArgs(raw, &args); rpcErr != nil {
		return nil, rpcErr
	}
	if args.Path == "" {
		return errorPayload("input_error", "\"path\" is required: the project-root-relative file or directory to adopt"), nil
	}
	if msg := missingApplyParams(args.Revision, args.Fingerprint); msg != "" {
		return errorPayload("input_error", msg,
			"Call \"plan_adopt\" with the same cwd/project/path first, then pass its exact revision and fingerprint here unchanged."), nil
	}
	projectID, errPayload := s.resolveProjectID(args.CWD, args.Project)
	if errPayload != nil {
		return errPayload, nil
	}
	result, err := s.engine.Adopt(projectID, args.Path, *args.Revision, *args.Fingerprint)
	if err != nil {
		return classifyEngineError(err), nil
	}
	return okPayload(result), nil
}

// --- plan_detach / detach -------------------------------------------------

func (s *Server) handlePlanDetach(raw json.RawMessage) (map[string]any, *rpcError) {
	var args pathArgs
	if rpcErr := decodeArgs(raw, &args); rpcErr != nil {
		return nil, rpcErr
	}
	if args.Path == "" {
		return errorPayload("input_error", "\"path\" is required: the project-root-relative managed entry to detach"), nil
	}
	projectID, errPayload := s.resolveProjectID(args.CWD, args.Project)
	if errPayload != nil {
		return errPayload, nil
	}
	plan, err := s.engine.PlanDetach(projectID, args.Path)
	if err != nil {
		return classifyEngineError(err), nil
	}
	return okPayload(plan), nil
}

func (s *Server) handleDetach(raw json.RawMessage) (map[string]any, *rpcError) {
	var args applyPathArgs
	if rpcErr := decodeArgs(raw, &args); rpcErr != nil {
		return nil, rpcErr
	}
	if args.Path == "" {
		return errorPayload("input_error", "\"path\" is required: the project-root-relative managed entry to detach"), nil
	}
	if msg := missingApplyParams(args.Revision, args.Fingerprint); msg != "" {
		return errorPayload("input_error", msg,
			"Call \"plan_detach\" with the same cwd/project/path first, then pass its exact revision and fingerprint here unchanged."), nil
	}
	projectID, errPayload := s.resolveProjectID(args.CWD, args.Project)
	if errPayload != nil {
		return errPayload, nil
	}
	result, err := s.engine.Detach(projectID, args.Path, *args.Revision, *args.Fingerprint)
	if err != nil {
		return classifyEngineError(err), nil
	}
	return okPayload(result), nil
}

// --- plan_reconcile / reconcile --------------------------------------------

func (s *Server) handlePlanReconcile(raw json.RawMessage) (map[string]any, *rpcError) {
	var args baseArgs
	if rpcErr := decodeArgs(raw, &args); rpcErr != nil {
		return nil, rpcErr
	}
	projectID, errPayload := s.resolveProjectID(args.CWD, args.Project)
	if errPayload != nil {
		return errPayload, nil
	}
	plan, err := s.engine.PlanReconcile(projectID)
	if err != nil {
		return classifyEngineError(err), nil
	}
	return okPayload(plan), nil
}

func (s *Server) handleReconcile(raw json.RawMessage) (map[string]any, *rpcError) {
	var args applyArgs
	if rpcErr := decodeArgs(raw, &args); rpcErr != nil {
		return nil, rpcErr
	}
	if msg := missingApplyParams(args.Revision, args.Fingerprint); msg != "" {
		return errorPayload("input_error", msg,
			"Call \"plan_reconcile\" with the same cwd/project first, then pass its exact revision and fingerprint here unchanged."), nil
	}
	projectID, errPayload := s.resolveProjectID(args.CWD, args.Project)
	if errPayload != nil {
		return errPayload, nil
	}
	result, err := s.engine.Reconcile(projectID, *args.Revision, *args.Fingerprint)
	if err != nil {
		return classifyEngineError(err), nil
	}
	return okPayload(result), nil
}

// --- register ---------------------------------------------------------

func (s *Server) handleRegister(raw json.RawMessage) (map[string]any, *rpcError) {
	var args registerArgs
	if rpcErr := decodeArgs(raw, &args); rpcErr != nil {
		return nil, rpcErr
	}
	root := args.CWD
	if root == "" {
		root = processWorkingDirectory()
	}
	project, revision, err := s.engine.Register(root, args.Name)
	if err != nil {
		return classifyEngineError(err), nil
	}
	return okPayload(map[string]any{"project": project, "revision": revision}), nil
}
