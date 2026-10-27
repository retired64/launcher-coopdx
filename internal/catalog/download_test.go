package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDownloadAttachment(t *testing.T) {
	payload := []byte("PK\x03\x04fake zip")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mods/cool-mod.123/download" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="cool.zip"`)
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(payload)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	dl, err := c.Download(context.Background(), ts.URL+"/mods/cool-mod.123/")
	if err != nil {
		t.Fatal(err)
	}
	if dl.Kind != "file" || dl.Filename != "cool.zip" {
		t.Fatalf("download = %+v", dl)
	}
	if string(dl.Data) != string(payload) {
		t.Fatalf("data = %q", dl.Data)
	}
}

func TestDownloadChooseFile(t *testing.T) {
	html := fixture(t, "download.html")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mods/cool-mod.123/download" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(html)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	dl, err := c.Download(context.Background(), ts.URL+"/mods/cool-mod.123/")
	if err != nil {
		t.Fatal(err)
	}
	if dl.Kind != "choose" || len(dl.Files) != 2 {
		t.Fatalf("download = %+v", dl)
	}
	if !strings.HasSuffix(dl.Files[0].URL, "/mods/cool-mod.123/download?file=111") {
		t.Fatalf("file url = %q", dl.Files[0].URL)
	}
}

func TestDownloadExternal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/file.zip", http.StatusFound)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	dl, err := c.Download(context.Background(), ts.URL+"/mods/cool-mod.123/")
	if err != nil {
		t.Fatal(err)
	}
	if dl.Kind != "external" || dl.URL != "https://example.com/file.zip" {
		t.Fatalf("download = %+v", dl)
	}
}

func TestDownloadFileScopedToMod(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mods/cool-mod.123/download" {
			w.Header().Set("Content-Disposition", `attachment; filename="chosen.zip"`)
			_, _ = w.Write([]byte("zip"))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	ctx := context.Background()
	detail := ts.URL + "/mods/cool-mod.123/"

	dl, err := c.DownloadFile(ctx, detail, ts.URL+"/mods/cool-mod.123/download?file=111")
	if err != nil {
		t.Fatal(err)
	}
	if dl.Kind != "file" || dl.Filename != "chosen.zip" {
		t.Fatalf("download = %+v", dl)
	}

	if _, err := c.DownloadFile(ctx, detail, ts.URL+"/mods/other.9/download?file=1"); err == nil {
		t.Fatal("a file from another mod should be rejected")
	}
}
