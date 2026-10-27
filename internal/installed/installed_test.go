package installed

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installed.json")
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.All()) != 0 {
		t.Fatal("new store should be empty")
	}

	rec := Record{
		SiteID:      42,
		SiteURL:     "https://mods.example/mod.42/",
		Title:       "Test",
		Version:     "1.2.3",
		InstalledAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := store.Set([]string{"Alpha", "Beta"}, rec); err != nil {
		t.Fatal(err)
	}

	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.Get("Alpha")
	if !ok || got.SiteID != 42 || got.Version != "1.2.3" {
		t.Fatalf("record = %+v ok=%v", got, ok)
	}
	if len(again.All()) != 2 {
		t.Fatalf("all = %+v", again.All())
	}

	if err := again.Delete("Alpha"); err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Get("Alpha"); ok {
		t.Fatal("Alpha should be gone")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	empty, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.All()) != 0 {
		t.Fatal("missing file should load empty")
	}
}

func TestCorruptFileDoesNotFail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installed.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.All()) != 0 {
		t.Fatal("corrupt file should load empty")
	}
}

func TestSameVersion(t *testing.T) {
	cases := []struct {
		a, b string
		same bool
	}{
		{"1.0", "1.0", true},
		{"v1.0", "1.0", true},
		{" V2.1 ", "2.1", true},
		{"1.0", "1.0.1", false},
	}
	for _, c := range cases {
		if got := SameVersion(c.a, c.b); got != c.same {
			t.Errorf("SameVersion(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
