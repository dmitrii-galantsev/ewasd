package httpapi

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dmitrii-galantsev/ewasd/internal/domain"
	"github.com/dmitrii-galantsev/ewasd/internal/engine"
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

//go:embed web/*
var assets embed.FS

type Server struct {
	engine       *engine.Engine
	token        string
	static       http.Handler
	plans        map[string]approvedPlan
	allowedHosts map[string]bool
	mu           sync.Mutex
}

type approvedPlan struct {
	ProjectID   string
	Action      string
	Path        string
	Revision    uint64
	Fingerprint string
	ExpiresAt   time.Time
}

type errorResponse struct {
	OK      bool   `json:"ok"`
	Outcome string `json:"outcome"`
	Error   string `json:"error"`
	Detail  string `json:"detail"`
	Recover string `json:"recover,omitempty"`
}

func New(domainEngine *engine.Engine, token string, allowedHosts ...string) (*Server, error) {
	if len(token) < 24 {
		return nil, errors.New("HTTP API requires an authentication token of at least 24 characters")
	}
	web, err := fs.Sub(assets, "web")
	if err != nil {
		return nil, err
	}
	hosts := map[string]bool{}
	for _, host := range allowedHosts {
		hosts[strings.ToLower(strings.TrimSpace(host))] = true
	}
	if len(hosts) == 0 {
		return nil, errors.New("HTTP API requires at least one allowed Host")
	}
	return &Server{engine: domainEngine, token: token, static: http.FileServer(http.FS(web)), plans: map[string]approvedPlan{}, allowedHosts: hosts}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/snapshot", s.snapshot)
	mux.HandleFunc("/api/v1/projects", s.projects)
	mux.HandleFunc("/api/v1/projects/", s.projectAction)
	mux.HandleFunc("/api/v1/recover", s.recover)
	mux.HandleFunc("/api/v1/recovery/discard", s.discardRecovery)
	mux.Handle("/", s.static)
	return s.security(s.hostGuard(s.auth(mux)))
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	snapshot, err := s.engine.Snapshot()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, errorResponse{OK: false, Outcome: "failed", Error: "origin_rejected", Detail: "write origin does not match this server"})
		return
	}
	var request struct {
		Root string `json:"root"`
		Name string `json:"name"`
	}
	if err := decode(w, r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Outcome: "failed", Error: "invalid_request", Detail: err.Error()})
		return
	}
	project, revision, err := s.engine.Register(request.Root, request.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "outcome": "completed", "revision": revision, "project": project})
}

func (s *Server) projectAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, errorResponse{OK: false, Outcome: "failed", Error: "origin_rejected", Detail: "write origin does not match this server"})
		return
	}
	remainder := strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")
	parts := strings.Split(strings.Trim(remainder, "/"), "/")
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	projectID, endpoint := parts[0], parts[1]
	var request struct {
		Action   string `json:"action"`
		Path     string `json:"path"`
		PlanID   string `json:"plan_id"`
		Revision uint64 `json:"revision"`
		Confirm  bool   `json:"confirm"`
	}
	if err := decode(w, r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Outcome: "failed", Error: "invalid_request", Detail: err.Error()})
		return
	}
	if endpoint == "unregister" {
		if !request.Confirm {
			writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Outcome: "failed", Error: "confirmation_required", Detail: "unregister requires explicit confirmation"})
			return
		}
		result, err := s.engine.Unregister(projectID, request.Revision)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	if endpoint == "plans" {
		plan, err := s.plan(projectID, request.Action, request.Path)
		if err != nil {
			writeError(w, err)
			return
		}
		s.rememberPlan(plan, request.Path)
		writeJSON(w, http.StatusOK, plan)
		return
	}
	if endpoint != "apply" {
		http.NotFound(w, r)
		return
	}
	approved, ok := s.consumePlan(request.PlanID, projectID, request.Action, request.Path, request.Revision)
	if !ok {
		writeJSON(w, http.StatusConflict, errorResponse{OK: false, Outcome: "failed", Error: "plan_required", Detail: "apply requires the matching unexpired plan_id returned by the preview endpoint", Recover: "Create and review a fresh plan before applying."})
		return
	}
	result, err := s.apply(projectID, request.Action, request.Path, request.Revision, approved.Fingerprint)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) recover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, errorResponse{OK: false, Outcome: "failed", Error: "origin_rejected", Detail: "write origin does not match this server"})
		return
	}
	messages, err := s.engine.Recover()
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "outcome": "partial_failure", "error": "recovery_incomplete", "detail": err.Error(), "messages": messages, "recover": "Resolved journals were recorded. Inspect the remaining journal in Safety before retrying or archiving it."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "outcome": "completed", "messages": messages})
}

func (s *Server) discardRecovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, errorResponse{OK: false, Outcome: "failed", Error: "origin_rejected", Detail: "write origin does not match this server"})
		return
	}
	var request struct {
		ID      string `json:"id"`
		Confirm bool   `json:"confirm"`
	}
	if err := decode(w, r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Outcome: "failed", Error: "invalid_request", Detail: err.Error()})
		return
	}
	if !request.Confirm {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Outcome: "failed", Error: "confirmation_required", Detail: "discard requires explicit confirmation"})
		return
	}
	archive, err := s.engine.DiscardJournal(request.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "outcome": "journal_archived", "archive": archive})
}

func (s *Server) plan(projectID, action, path string) (domain.Plan, error) {
	switch action {
	case "adopt":
		return s.engine.PlanAdopt(projectID, path)
	case "detach":
		return s.engine.PlanDetach(projectID, path)
	case "reconcile":
		return s.engine.PlanReconcile(projectID)
	default:
		return domain.Plan{}, fmt.Errorf("unknown action %q", action)
	}
}

func (s *Server) rememberPlan(plan domain.Plan, path string) {
	if !plan.Safe || len(plan.Steps) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, candidate := range s.plans {
		if candidate.ExpiresAt.Before(now) {
			delete(s.plans, id)
		}
	}
	if len(s.plans) >= 256 {
		for id := range s.plans {
			delete(s.plans, id)
			break
		}
	}
	s.plans[plan.ID] = approvedPlan{ProjectID: plan.ProjectID, Action: plan.Action, Path: path, Revision: plan.ExpectedRevision, Fingerprint: plan.Fingerprint, ExpiresAt: now.Add(10 * time.Minute)}
}

func (s *Server) consumePlan(id, projectID, action, path string, revision uint64) (approvedPlan, bool) {
	if id == "" {
		return approvedPlan{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.plans[id]
	delete(s.plans, id)
	valid := ok && plan.ExpiresAt.After(time.Now()) && plan.ProjectID == projectID && plan.Action == action && plan.Path == path && plan.Revision == revision
	return plan, valid
}

func (s *Server) apply(projectID, action, path string, revision uint64, fingerprint string) (any, error) {
	switch action {
	case "adopt":
		return s.engine.Adopt(projectID, path, revision, fingerprint)
	case "detach":
		return s.engine.Detach(projectID, path, revision, fingerprint)
	case "reconcile":
		return s.engine.Reconcile(projectID, revision, fingerprint)
	default:
		return nil, fmt.Errorf("unknown action %q", action)
	}
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(provided) != len(s.token) || subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, errorResponse{OK: false, Outcome: "failed", Error: "unauthorized", Detail: "open the pairing URL again or provide the bearer token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		host := strings.ToLower(r.Host)
		hostname := host
		if parsed, _, err := net.SplitHostPort(host); err == nil {
			hostname = strings.Trim(parsed, "[]")
		}
		if !s.allowedHosts[host] && !s.allowedHosts[hostname] {
			writeJSON(w, http.StatusForbidden, errorResponse{OK: false, Outcome: "failed", Error: "host_rejected", Detail: "request Host is not approved for this console"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Non-browser clients may omit Origin, but must authenticate directly.
		// Browser fetches carry Sec-Fetch-Site and are never exempted.
		return r.Header.Get("Sec-Fetch-Site") == "" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	parsed, err := url.Parse(origin)
	fetchSite := r.Header.Get("Sec-Fetch-Site")
	return err == nil && parsed.Host == r.Host && (parsed.Scheme == "http" || parsed.Scheme == "https") && (fetchSite == "" || fetchSite == "same-origin")
}

func decode(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) == nil {
		return errors.New("request must contain one JSON object")
	}
	return nil
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := "internal_error"
	recover := "No files were intentionally overwritten. Inspect the Safety view and transaction journals before retrying."
	switch {
	case errors.Is(err, engine.ErrNotFound):
		status, code, recover = http.StatusNotFound, "not_found", "Refresh the repository list and choose a registered checkout."
	case errors.Is(err, engine.ErrConflict):
		status, code, recover = http.StatusConflict, "conflict", "Resolve the named local or central conflict, then create a fresh plan."
	case errors.Is(err, store.ErrStaleRevision):
		status, code, recover = http.StatusConflict, "stale_revision", "Refresh and review a fresh plan; another operation changed state."
	case errors.Is(err, store.ErrBusy):
		status, code, recover = http.StatusConflict, "busy", "Wait for the active operation to finish, then refresh."
	case errors.Is(err, store.ErrDurabilityUncertain):
		status, code, recover = http.StatusServiceUnavailable, "recovery_required", "The manifest may be committed. Do not retry; open Safety and run recovery."
	case errors.Is(err, engine.ErrRecoveryPending):
		status, code, recover = http.StatusConflict, "recovery_required", "Open Safety and resolve the interrupted transaction before any new write."
	case strings.Contains(err.Error(), "path") || strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "unknown action"):
		status, code, recover = http.StatusBadRequest, "invalid_request", "Correct the request and preview it again."
	}
	writeJSON(w, status, errorResponse{OK: false, Outcome: "failed", Error: code, Detail: err.Error(), Recover: recover})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeJSON(w, http.StatusMethodNotAllowed, errorResponse{OK: false, Outcome: "failed", Error: "method_not_allowed", Detail: "method is not supported"})
}
