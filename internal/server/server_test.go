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

	"github.com/retired64/coop-loader/internal/app"
	"github.com/retired64/coop-loader/internal/game"
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
	if !strings.Contains(string(body), "COOP LOADER") {
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
