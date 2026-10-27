package catalog

import (
	"net/url"
	"testing"
)

func TestGitHubHosts(t *testing.T) {
	ok := []string{
		"https://github.com/a/b",
		"https://api.github.com/repos/a/b/releases/latest",
		"https://codeload.github.com/a/b/zip/refs/heads/main",
		"https://objects.githubusercontent.com/x",
	}
	for _, raw := range ok {
		u, err := url.Parse(raw)
		if err != nil || !isGitHubURL(u) {
			t.Errorf("%q should be GitHub", raw)
		}
	}
	bad := []string{
		"http://github.com/a/b",
		"https://github.com.evil.example/a/b",
		"https://example.com/a/b",
	}
	for _, raw := range bad {
		u, err := url.Parse(raw)
		if err == nil && isGitHubURL(u) {
			t.Errorf("%q should not be GitHub", raw)
		}
	}
}

func TestPickAsset(t *testing.T) {
	var release ghRelease
	release.TagName = "v1.2"
	release.ZipballURL = "https://api.github.com/repos/a/b/zipball/v1.2"
	release.Assets = []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	}{
		{Name: "source.zip", BrowserDownloadURL: "https://github.com/a/b/archive/source.zip"},
		{Name: "mod.zip", BrowserDownloadURL: "https://github.com/a/b/releases/download/v1.2/mod.zip"},
	}
	url, name := pickAsset(release, "b")
	if url != "https://github.com/a/b/releases/download/v1.2/mod.zip" || name != "mod.zip" {
		t.Fatalf("picked %q / %q", url, name)
	}

	release.Assets = nil
	url, name = pickAsset(release, "b")
	if url != release.ZipballURL || name != "b-v1.2.zip" {
		t.Fatalf("fallback %q / %q", url, name)
	}
}
