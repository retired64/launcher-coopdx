package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, base string) *Client {
	t.Helper()
	c, err := New(Options{
		BaseURL:   base,
		UserAgent: "coop-loader-test",
		Gap:       time.Millisecond,
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestPageCachesHits(t *testing.T) {
	html := fixture(t, "listing.html")
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(html)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	ctx := context.Background()

	body, final, cached, err := c.Page(ctx, "/mods/")
	if err != nil {
		t.Fatal(err)
	}
	if cached {
		t.Fatal("first page should not come from cache")
	}
	if len(body) == 0 || !strings.HasSuffix(final, "/mods/") {
		t.Fatalf("body=%d final=%q", len(body), final)
	}

	_, _, cached, err = c.Page(ctx, "/mods/")
	if err != nil {
		t.Fatal(err)
	}
	if !cached {
		t.Fatal("second page should come from cache")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("server hits = %d, want 1", got)
	}
}

func TestCachePersistsAcrossClients(t *testing.T) {
	html := fixture(t, "listing.html")
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(html)
	}))
	defer ts.Close()

	cache := filepath.Join(t.TempDir(), "site-cache.json")
	opts := Options{BaseURL: ts.URL, CachePath: cache, Gap: time.Millisecond, TTL: time.Hour}
	c1, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c1.Page(context.Background(), "/mods/"); err != nil {
		t.Fatal(err)
	}
	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("cache file missing: %v", err)
	}

	c2, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_, _, cached, err := c2.Page(context.Background(), "/mods/")
	if err != nil {
		t.Fatal(err)
	}
	if !cached {
		t.Fatal("second client should read from the persisted cache")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("server hits = %d, want 1", got)
	}
}

func TestPageRejectsOtherSites(t *testing.T) {
	c := newTestClient(t, "https://mods.example.com")
	_, _, _, err := c.Page(context.Background(), "https://evil.example/")
	var ext *ExternalError
	if !errors.As(err, &ext) {
		t.Fatalf("err = %v, want ExternalError", err)
	}
}

func TestPageFollowsSameOriginRedirect(t *testing.T) {
	html := fixture(t, "listing.html")
	var finalPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/mods/", http.StatusFound)
			return
		}
		finalPath = r.URL.Path
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(html)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	_, final, _, err := c.Page(context.Background(), "/old")
	if err != nil {
		t.Fatal(err)
	}
	if finalPath != "/mods/" || !strings.HasSuffix(final, "/mods/") {
		t.Fatalf("final path = %q, final = %q", finalPath, final)
	}
}

func TestPageRefusesOffsiteRedirect(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>offsite</html>"))
	}))
	defer other.Close()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	_, _, _, err := c.Page(context.Background(), "/mods/")
	var ext *ExternalError
	if !errors.As(err, &ext) {
		t.Fatalf("err = %v, want ExternalError", err)
	}
}

func TestListAndSearch(t *testing.T) {
	listHTML := fixture(t, "listing.html")
	searchHTML := fixture(t, "search.html")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch {
		case strings.HasPrefix(r.URL.Path, "/search/search"):
			_, _ = w.Write(searchHTML)
		case strings.HasPrefix(r.URL.Path, "/mods/"):
			_, _ = w.Write(listHTML)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	ctx := context.Background()

	list, err := c.List(ctx, Query{Sort: SortDownloads})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].ID != 123 || list.Pages != 3 {
		t.Fatalf("list = %+v", list)
	}

	search, err := c.List(ctx, Query{Search: "cool"})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Items) != 1 || search.Items[0].ID != 9 {
		t.Fatalf("search = %+v", search)
	}
}

func TestDetailThroughClient(t *testing.T) {
	html := fixture(t, "detail.html")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(html)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	d, err := c.Detail(context.Background(), "/mods/cool-mod.123/")
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != 123 || d.Title != "Cool Mod" || len(d.Blocks) == 0 || len(d.Links) != 1 {
		t.Fatalf("detail = %+v", d)
	}
}

func TestCategoriesThroughClient(t *testing.T) {
	html := fixture(t, "categories.html")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(html)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	list, err := c.Categories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("categories = %+v", list)
	}
}
