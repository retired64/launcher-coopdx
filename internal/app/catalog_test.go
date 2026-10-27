package app

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/retired64/coop-loader/internal/deps"
	"github.com/retired64/coop-loader/internal/mods"
)

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInstallFromSiteRecordsAndFindsRequirements(t *testing.T) {
	svc, gameData := newTestService(t)
	svc.catalogGap = time.Millisecond

	archive := zipBytes(t, map[string]string{
		"mods/TestMod/main.lua": `
-- name: Test Mod
_G.testModApi = {}
if not _G.helperApi then
    print("need Helper Mod")
    return
end
`,
	})

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mods/test-mod.42/":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<html><body>
			  <div class="p-description"><a class="username">Tester</a></div>
			  <h1 class="p-title-value">Test Mod <span class="u-muted">v1.0.0</span></h1>
			  <div class="bbWrapper"><p>Needs <a href="%s/mods/helper-mod.7/">Helper Mod</a>.</p></div>
			</body></html>`, srv.URL)
		case "/mods/test-mod.42/download":
			w.Header().Set("Content-Disposition", `attachment; filename="TestMod.zip"`)
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	svc.SetCatalogBaseURL(srv.URL)
	out, err := svc.InstallFromSite(context.Background(), srv.URL+"/mods/test-mod.42/", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != "installed" || len(out.Installed) != 1 || out.Installed[0] != "TestMod" {
		t.Fatalf("outcome = %+v", out)
	}
	if _, err := os.Stat(filepath.Join(gameData, "mods", "TestMod", "main.lua")); err != nil {
		t.Fatalf("mod not installed: %v", err)
	}

	list := svc.LocalMods()
	var local *LocalMod
	for i := range list {
		if list[i].ID == "TestMod" {
			local = &list[i]
			break
		}
	}
	if local == nil || local.Site == nil {
		t.Fatalf("no site record: %+v", local)
	}
	if local.Site.SiteID != 42 || local.Site.Version != "1.0.0" {
		t.Fatalf("record = %+v", local.Site)
	}

	if len(out.Requires) != 1 {
		t.Fatalf("requires = %+v", out.Requires)
	}
	req := out.Requires[0]
	if req.Global != "helperApi" {
		t.Fatalf("global = %q", req.Global)
	}
	if len(req.Site) != 1 || req.Site[0].ID != 7 || req.Site[0].Title != "Helper Mod" {
		t.Fatalf("site suggestions = %+v", req.Site)
	}
}

func TestRequirementsUseLocalProvider(t *testing.T) {
	svc, gameData := newTestService(t)
	addMod(t, gameData, "Helper", "-- name: Helper\n_G.helperApi = {}\n")

	cli, err := svc.cat()
	if err != nil {
		t.Fatal(err)
	}
	scan := deps.Scan([]byte("_G.newMod = {}\nif not _G.helperApi then return end\n"))

	// Disabled provider: the loader should offer to enable it.
	reqs := svc.requirements(context.Background(), cli, scan, []string{"NewMod"}, nil)
	if len(reqs) != 1 {
		t.Fatalf("disabled provider requirements = %+v", reqs)
	}
	if len(reqs[0].Local) != 1 || reqs[0].Local[0].ID != "Helper" || reqs[0].Local[0].Enabled {
		t.Fatalf("local = %+v", reqs[0].Local)
	}

	// Enable it: no requirement is left.
	if _, err := svc.Toggle("Helper", mods.KindMod, true); err != nil {
		t.Fatal(err)
	}
	if reqs := svc.requirements(context.Background(), cli, scan, []string{"NewMod"}, nil); len(reqs) != 0 {
		t.Fatalf("enabled provider still warned: %+v", reqs)
	}
}
