package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/retired64/coop-loader/internal/app"
	"github.com/retired64/coop-loader/internal/game"
	"github.com/retired64/coop-loader/internal/session"
)

func newTestServer(t *testing.T) (*Server, *app.Service) {
	t.Helper()
	root := t.TempDir()
	svc := app.New(filepath.Join(root, "loader"), filepath.Join(root, "data"))
	gameDir := filepath.Join(root, "game")
	if err := os.MkdirAll(gameDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gameDir, game.Exe()), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetGamePath(gameDir); err != nil {
		t.Fatal(err)
	}
	srv, err := New(svc)
	if err != nil {
		t.Fatal(err)
	}
	return srv, svc
}

func do(t *testing.T, method, url string, token string, host string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("X-Token", token)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAPIRequiresToken(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := do(t, http.MethodGet, ts.URL+"/api/status", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without token = %d", resp.StatusCode)
	}

	resp = do(t, http.MethodGet, ts.URL+"/api/status", srv.Token(), "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status with token = %d", resp.StatusCode)
	}
	var status app.Status
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Version != app.Version || !status.GameFound {
		t.Fatalf("status = %+v", status)
	}
}

func TestAPIRejectsForeignHost(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := do(t, http.MethodGet, ts.URL+"/api/status", srv.Token(), "evil.example")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host = %d", resp.StatusCode)
	}
}

func TestStaticPageAndCSP(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := do(t, http.MethodGet, ts.URL+"/", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "COOP MODS") {
		t.Fatal("index.html was not served")
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "https://mods.sm64coopdx.com") {
		t.Fatalf("CSP does not allow mod icons: %q", csp)
	}
}

func TestModsEndpointReturnsArray(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := do(t, http.MethodGet, ts.URL+"/api/mods", srv.Token(), "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mods = %d", resp.StatusCode)
	}
	var out struct {
		Mods []app.LocalMod `json:"mods"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Mods == nil {
		t.Fatal("mods should be an array, not null")
	}
}

func doJSON(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Token", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSessionAPI(t *testing.T) {
	// Keep the test offline: no public trackers, and a relay that refuses.
	t.Setenv("COOP_NO_TRACKERS", "1")
	t.Setenv("COOP_RELAY_URL", "http://127.0.0.1:1")

	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := do(t, http.MethodGet, ts.URL+"/api/session/state", srv.Token(), "")
	var idle struct {
		Session *session.State `json:"session"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&idle); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if idle.Session != nil {
		t.Fatalf("idle session = %+v", idle.Session)
	}

	resp = doJSON(t, ts.URL+"/api/session/start", srv.Token(), `{"mode":"join","code":"NOPE"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad code status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doJSON(t, ts.URL+"/api/session/start", srv.Token(), `{"mode":"host","code":"ABCD-1234"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start host status = %d", resp.StatusCode)
	}
	var started struct {
		Session session.State `json:"session"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if started.Session.Mode != "host" || started.Session.Code != "ABCD-1234" {
		t.Fatalf("session = %+v", started.Session)
	}
	if started.Session.Phase != "waiting" {
		t.Fatalf("phase = %q", started.Session.Phase)
	}

	resp = doJSON(t, ts.URL+"/api/session/stop", srv.Token(), `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = do(t, http.MethodGet, ts.URL+"/api/session/state", srv.Token(), "")
	var after struct {
		Session *session.State `json:"session"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if after.Session != nil {
		t.Fatalf("session after stop = %+v", after.Session)
	}
}

func TestNetConsentAPI(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := doJSON(t, ts.URL+"/api/settings/netconsent", srv.Token(), `{"accepted":true,"termsVersion":2}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("consent status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = do(t, http.MethodGet, ts.URL+"/api/status", srv.Token(), "")
	defer resp.Body.Close()
	var st app.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.NetConsent == nil || !st.NetConsent.Accepted || st.NetConsent.TermsVersion != 2 {
		t.Fatalf("consent = %+v", st.NetConsent)
	}
}

func TestCopyAPI(t *testing.T) {
	// Keep the test offline: no public trackers, and a relay that refuses.
	t.Setenv("COOP_NO_TRACKERS", "1")
	t.Setenv("COOP_RELAY_URL", "http://127.0.0.1:1")

	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := do(t, http.MethodGet, ts.URL+"/api/copy/state", srv.Token(), "")
	var idle struct {
		Copy *appCopyState `json:"copy"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&idle); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if idle.Copy != nil {
		t.Fatalf("idle copy = %+v", idle.Copy)
	}

	resp = doJSON(t, ts.URL+"/api/copy/start", srv.Token(), `{"role":"get","code":"NOPE"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad code status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doJSON(t, ts.URL+"/api/copy/accept", srv.Token(), `{"mods":true,"look":true,"saves":true}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("accept without review status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doJSON(t, ts.URL+"/api/copy/start", srv.Token(), `{"role":"send","code":""}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start send status = %d", resp.StatusCode)
	}
	var started struct {
		Copy appCopyState `json:"copy"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if started.Copy.Role != "send" || started.Copy.Code == "" {
		t.Fatalf("copy = %+v", started.Copy)
	}

	resp = doJSON(t, ts.URL+"/api/copy/stop", srv.Token(), `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = do(t, http.MethodGet, ts.URL+"/api/copy/state", srv.Token(), "")
	var after struct {
		Copy *appCopyState `json:"copy"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if after.Copy != nil {
		t.Fatalf("copy after stop = %+v", after.Copy)
	}
}

// appCopyState is the subset the test decodes. It avoids importing the whole
// devicecopy state in every assertion.
type appCopyState struct {
	Role     string `json:"role"`
	Code     string `json:"code"`
	Phase    string `json:"phase"`
	Trackers int    `json:"trackers"`
}

func TestROMStatusAndBadUpload(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	var last struct {
		ROM struct {
			Ready  bool   `json:"ready"`
			Busy   bool   `json:"busy"`
			Reason string `json:"reason"`
		} `json:"rom"`
	}
	for i := 0; i < 50; i++ {
		resp := do(t, http.MethodGet, ts.URL+"/api/rom/status", srv.Token(), "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if err := json.NewDecoder(resp.Body).Decode(&last); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if !last.ROM.Busy {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last.ROM.Ready {
		t.Fatal("a test service with no ROM reported ready")
	}
	if last.ROM.Reason == "" {
		t.Fatal("no reason for the missing ROM")
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/rom/upload?name=bad.z64", strings.NewReader(string(make([]byte, 0x2000))))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Token", srv.Token())
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad upload = %d", resp.StatusCode)
	}
}
