package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dmitrii-galantsev/ewasd/internal/engine"
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

const testToken = "a-strong-test-token-with-24-chars"

func TestServerRequiresTokenAndAllowedHost(t *testing.T) {
	t.Parallel()
	_, _, domainEngine := testServer(t, testToken)
	if _, err := New(domainEngine, "", "127.0.0.1"); err == nil {
		t.Fatal("server accepted an empty token")
	}
	if _, err := New(domainEngine, testToken); err == nil {
		t.Fatal("server accepted an empty Host allowlist")
	}
}

func TestStaticShellIsAvailableButTokenProtectsAPI(t *testing.T) {
	t.Parallel()
	handler, _, _ := testServer(t, testToken)
	server := httptest.NewServer(handler)
	defer server.Close()

	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("static shell status = %d", response.StatusCode)
	}
	_ = response.Body.Close()
	response, err = http.Get(server.URL + "/api/v1/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized API status = %d", response.StatusCode)
	}
	if response.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("security headers missing")
	}
	_ = response.Body.Close()

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/snapshot", nil)
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authorized API status = %d", response.StatusCode)
	}
	_ = response.Body.Close()
}

func TestRegisterPlanAndApplyContract(t *testing.T) {
	t.Parallel()
	handler, repo, _ := testServer(t, "")
	server := httptest.NewServer(handler)
	defer server.Close()
	if err := os.WriteFile(filepath.Join(repo, "AGENT.md"), []byte("rules"), 0o600); err != nil {
		t.Fatal(err)
	}

	register := postJSON(t, server.URL+"/api/v1/projects", server.URL, map[string]any{"root": repo, "name": "API repo"})
	if register.Code != http.StatusCreated {
		t.Fatalf("register status %d: %s", register.Code, register.Body.String())
	}
	var registered struct {
		Revision uint64 `json:"revision"`
		Project  struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	if err := json.Unmarshal(register.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	plan := postJSON(t, server.URL+"/api/v1/projects/"+registered.Project.ID+"/plans", server.URL, map[string]any{"action": "adopt", "path": "AGENT.md", "revision": registered.Revision})
	if plan.Code != http.StatusOK {
		t.Fatalf("plan status %d: %s", plan.Code, plan.Body.String())
	}
	var planned struct {
		ID               string `json:"id"`
		Safe             bool   `json:"safe"`
		ExpectedRevision uint64 `json:"expected_revision"`
	}
	_ = json.Unmarshal(plan.Body.Bytes(), &planned)
	if !planned.Safe || planned.ExpectedRevision != registered.Revision {
		t.Fatalf("unexpected plan: %+v", planned)
	}
	missingPlan := postJSON(t, server.URL+"/api/v1/projects/"+registered.Project.ID+"/apply", server.URL, map[string]any{"action": "adopt", "path": "AGENT.md", "revision": planned.ExpectedRevision})
	if missingPlan.Code != http.StatusConflict {
		t.Fatalf("apply without plan id status %d: %s", missingPlan.Code, missingPlan.Body.String())
	}
	apply := postJSON(t, server.URL+"/api/v1/projects/"+registered.Project.ID+"/apply", server.URL, map[string]any{"action": "adopt", "path": "AGENT.md", "plan_id": planned.ID, "revision": planned.ExpectedRevision})
	if apply.Code != http.StatusOK {
		t.Fatalf("apply status %d: %s", apply.Code, apply.Body.String())
	}
	if info, err := os.Lstat(filepath.Join(repo, "AGENT.md")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("API apply did not create link: %v, %v", info, err)
	}
	replay := postJSON(t, server.URL+"/api/v1/projects/"+registered.Project.ID+"/apply", server.URL, map[string]any{"action": "adopt", "path": "AGENT.md", "plan_id": planned.ID, "revision": planned.ExpectedRevision})
	if replay.Code != http.StatusConflict {
		t.Fatalf("replayed plan status %d: %s", replay.Code, replay.Body.String())
	}

	badOrigin := postJSON(t, server.URL+"/api/v1/projects/"+registered.Project.ID+"/plans", "https://attacker.example", map[string]any{"action": "reconcile", "path": ""})
	if badOrigin.Code != http.StatusForbidden {
		t.Fatalf("cross-origin write status = %d", badOrigin.Code)
	}
	hostRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/snapshot", nil)
	hostRequest.Host = "evil.attacker.example:7337"
	hostRequest.Header.Set("Authorization", "Bearer "+testToken)
	hostResponse, err := http.DefaultClient.Do(hostRequest)
	if err != nil {
		t.Fatal(err)
	}
	if hostResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("unapproved Host status = %d", hostResponse.StatusCode)
	}
	_ = hostResponse.Body.Close()
	crossSite, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/projects/"+registered.Project.ID+"/plans", bytes.NewReader([]byte(`{"action":"reconcile","path":""}`)))
	crossSite.Header.Set("Authorization", "Bearer "+testToken)
	crossSite.Header.Set("Content-Type", "application/json")
	crossSite.Header.Set("Sec-Fetch-Site", "cross-site")
	crossResponse, err := http.DefaultClient.Do(crossSite)
	if err != nil {
		t.Fatal(err)
	}
	if crossResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site write without Origin status = %d", crossResponse.StatusCode)
	}
	_ = crossResponse.Body.Close()
}

func testServer(t *testing.T, token string) (http.Handler, string, *engine.Engine) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	s, err := store.New(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	domainEngine := engine.New(s)
	if token == "" {
		token = testToken
	}
	server, err := New(domainEngine, token, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler(), repo, domainEngine
}

func postJSON(t *testing.T, target, origin string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(body)
	request, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	request.Header.Set("Authorization", "Bearer "+testToken)
	recorder := httptest.NewRecorder()
	// Route through a fresh handler supplied by the test server URL is not
	// possible here, so use the URL's registered transport by making a client
	// request and copy the response into a recorder.
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	recorder.Code = response.StatusCode
	recorder.HeaderMap = response.Header.Clone()
	_, _ = recorder.Body.ReadFrom(response.Body)
	_ = response.Body.Close()
	return recorder
}
