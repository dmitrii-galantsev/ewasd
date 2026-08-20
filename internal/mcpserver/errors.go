package mcpserver

import (
	"errors"

	"github.com/dmitrii-galantsev/ewasd/internal/engine"
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

// errorPayload builds the {"ok":false,...} data shape returned inside a
// successful tools/call response (isError stays false: this is an engine or
// input-validation outcome a model can read and self-correct from, not a
// protocol failure).
func errorPayload(code, message string, hints ...string) map[string]any {
	payload := map[string]any{
		"ok":      false,
		"error":   code,
		"message": message,
	}
	if len(hints) > 0 {
		payload["hints"] = hints
	}
	return payload
}

// classifyEngineError maps a sentinel engine/store error into the same
// {"ok":false,...} data shape, with a hint aimed at what an agent should do
// next. Unrecognized errors still return valid data, just with a generic
// "error" code.
func classifyEngineError(err error) map[string]any {
	switch {
	case err == nil:
		return map[string]any{"ok": true}
	case errors.Is(err, engine.ErrAmbiguousDetection):
		return errorPayload("ambiguous", err.Error(),
			"Pass an explicit \"project\" selector (registered ID or name) to disambiguate.")
	case errors.Is(err, engine.ErrNotFound):
		return errorPayload("not_found", err.Error(),
			"Call the \"status\" tool to list registered projects and their IDs/names, or \"detect\" for the full reasoning trace.")
	case errors.Is(err, engine.ErrRecoveryPending):
		return errorPayload("recovery_pending", err.Error(),
			"An interrupted transaction must be resolved before new writes. Check \"status\"'s recovery field; running ewasd recover is a human action and is not exposed over MCP.")
	case errors.Is(err, engine.ErrConflict):
		return errorPayload("conflict", err.Error(),
			"Call the matching plan_* tool again to see current conflicts; nothing was changed.")
	case errors.Is(err, store.ErrStaleRevision):
		return errorPayload("stale_revision", err.Error(),
			"The manifest or filesystem changed since the plan was made. Call the matching plan_* tool again and retry with its fresh revision and fingerprint.")
	case errors.Is(err, store.ErrBusy):
		return errorPayload("busy", err.Error(),
			"Another ewasd operation holds the state lock. Retry shortly.")
	default:
		return errorPayload("error", err.Error())
	}
}
