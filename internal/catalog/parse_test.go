package catalog

import (
	"net/url"
	"os"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testBase(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse(DefaultBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestParseListing(t *testing.T) {
	items, pages := parseList(fixture(t, "listing.html"), testBase(t))
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	item := items[0]
	if item.ID != 123 {
		t.Errorf("id = %d", item.ID)
	}
	if item.URL != "https://mods.sm64coopdx.com/mods/cool-mod.123/" {
		t.Errorf("url = %q", item.URL)
	}
	if item.Title != "Cool Mod" || item.Version != "v1.2.3" {
		t.Errorf("title/version = %q / %q", item.Title, item.Version)
	}
	if item.Author != "Alice" {
		t.Errorf("author = %q", item.Author)
	}
	if item.Category != "Gamemode" {
		t.Errorf("category = %q", item.Category)
	}
	if item.Tagline != "Does cool things." {
		t.Errorf("tagline = %q", item.Tagline)
	}
	if item.Downloads != 1234 {
		t.Errorf("downloads = %d", item.Downloads)
	}
	if item.Rating != 4.5 || item.RatingCount != 12 {
		t.Errorf("rating = %v (%d)", item.Rating, item.RatingCount)
	}
	if item.Icon != "https://mods.sm64coopdx.com/data/resource_icons/0/123.jpg" {
		t.Errorf("icon = %q", item.Icon)
	}
	if item.Updated != "2026-01-02T03:04:05Z" {
		t.Errorf("updated = %q", item.Updated)
	}
	if pages != 3 {
		t.Errorf("pages = %d", pages)
	}
}

func TestParseSearch(t *testing.T) {
	items, _ := parseList(fixture(t, "search.html"), testBase(t))
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	item := items[0]
	if item.ID != 9 || item.Title != "Other Mod" || item.Version != "2.0" {
		t.Errorf("item = %+v", item)
	}
	if item.Author != "Bob" || item.Category != "Romhack" {
		t.Errorf("author/category = %q / %q", item.Author, item.Category)
	}
	if item.Icon == "" {
		t.Error("missing synthesized icon")
	}
}

func TestParseCategories(t *testing.T) {
	categories := parseCategories(fixture(t, "categories.html"))
	if len(categories) != 3 {
		t.Fatalf("categories = %d: %+v", len(categories), categories)
	}
	bySlug := map[string]Category{}
	for _, c := range categories {
		bySlug[c.Slug] = c
	}
	if bySlug["root"].Depth != 0 || bySlug["sub"].Depth != 1 {
		t.Errorf("depths = %+v", bySlug)
	}
	if bySlug["gamemode"].ID != 5 {
		t.Errorf("gamemode = %+v", bySlug["gamemode"])
	}
}

func TestParseDetail(t *testing.T) {
	final := "https://mods.sm64coopdx.com/mods/cool-mod.123/"
	d := parseDetail(fixture(t, "detail.html"), final, testBase(t))

	if d.ID != 123 || d.URL != final {
		t.Errorf("id/url = %d / %q", d.ID, d.URL)
	}
	if d.Title != "Cool Mod" {
		t.Errorf("title = %q", d.Title)
	}
	if d.Version != "v1.2.3" {
		t.Errorf("version = %q", d.Version)
	}
	if d.Author != "Alice" || d.Category != "Gamemode" {
		t.Errorf("author/category = %q / %q", d.Author, d.Category)
	}
	if d.Downloads != 1234 || d.Rating != 4.5 {
		t.Errorf("downloads/rating = %d / %v", d.Downloads, d.Rating)
	}
	if d.Updated != "2026-01-02T03:04:05Z" {
		t.Errorf("updated = %q", d.Updated)
	}

	kinds := map[string]int{}
	for _, b := range d.Blocks {
		kinds[b.Kind]++
	}
	if kinds["p"] != 3 || kinds["list"] != 1 || kinds["quote"] != 1 || kinds["image"] != 1 {
		t.Errorf("blocks = %+v (%d total)", kinds, len(d.Blocks))
	}

	if len(d.Links) != 1 {
		t.Fatalf("links = %+v", d.Links)
	}
	if d.Links[0].ID != 50 || d.Links[0].Title != "Character Select" {
		t.Errorf("link = %+v", d.Links[0])
	}

	var bold, link bool
	for _, b := range d.Blocks {
		for _, in := range b.Inline {
			if in.Bold && in.Text == "bold" {
				bold = true
			}
			if in.Href == "https://example.com/x" && in.Text == "a link" {
				link = true
			}
		}
	}
	if !bold || !link {
		t.Errorf("inline formatting lost: bold=%v link=%v", bold, link)
	}
}

func TestParseDownloadPage(t *testing.T) {
	base := testBase(t)
	detail, _ := url.Parse("https://mods.sm64coopdx.com/mods/cool-mod.123/")
	files := parseDownloadPage(fixture(t, "download.html"), base, detail)
	if len(files) != 2 {
		t.Fatalf("files = %+v", files)
	}
	if files[0].Name != "Cool Mod v1.2.3" || files[0].Size != "4.2 MB" {
		t.Errorf("file[0] = %+v", files[0])
	}
	if files[1].Name != "Cool Mod Lite" {
		t.Errorf("file[1] = %+v", files[1])
	}
}

func TestFilenameFromDisposition(t *testing.T) {
	cases := map[string]string{
		`attachment; filename="mod.zip"`:              "mod.zip",
		`attachment; filename*=UTF-8''cool%20mod.zip`: "cool mod.zip",
		`attachment; filename="../../etc/passwd"`:     "passwd",
		`inline`: "",
	}
	for in, want := range cases {
		if got := filenameFromDisposition(in); got != want {
			t.Errorf("filenameFromDisposition(%q) = %q, want %q", in, got, want)
		}
	}
}
