package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

var (
	githubRepoRe = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/#?]+)`)
	githubTagRe  = regexp.MustCompile(`/releases/tag/([^/?#]+)`)
)

func isGitHubURL(u *url.URL) bool {
	if u.Scheme != "https" {
		return false
	}
	return isGitHubHost(u.Host)
}

func isGitHubHost(host string) bool {
	switch strings.ToLower(host) {
	case "github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com":
		return true
	}
	return strings.HasSuffix(strings.ToLower(host), ".githubusercontent.com")
}

func isGitHubURLString(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && isGitHubURL(u)
}

func allowGitHubAPI(u *url.URL) bool {
	return u.Scheme == "https" && strings.EqualFold(u.Host, "api.github.com")
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	} `json:"assets"`
	ZipballURL string `json:"zipball_url"`
}

// resolveGitHub turns a GitHub repository/release link into a zip download.
func (c *Client) resolveGitHub(ctx context.Context, page string) (Download, error) {
	m := githubRepoRe.FindStringSubmatch(page)
	if len(m) != 3 {
		return Download{}, errors.New("that GitHub link is not a repository")
	}
	owner := m[1]
	repo := strings.TrimSuffix(m[2], ".git")

	api := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo)
	if tag := githubTagRe.FindStringSubmatch(page); len(tag) == 2 {
		api = fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/tags/%s", owner, repo, tag[1])
	}

	apiURL, err := url.Parse(api)
	if err != nil {
		return Download{}, err
	}
	if err := c.wait(ctx); err != nil {
		return Download{}, err
	}
	resp, _, err := c.fetch(ctx, apiURL, allowGitHubAPI, 3)
	if err != nil {
		return Download{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return Download{}, fmt.Errorf("GitHub answered %d: the mod may have no releases", resp.StatusCode)
	}
	body, err := readLimited(resp.Body, 2<<20)
	if err != nil {
		return Download{}, err
	}
	var release ghRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return Download{}, fmt.Errorf("GitHub's answer could not be read: %w", err)
	}

	assetURL, name := pickAsset(release, repo)
	if assetURL == "" {
		return Download{}, errors.New("this GitHub release has no zip file")
	}
	return c.fetchGitHubFile(ctx, assetURL, name)
}

func pickAsset(release ghRelease, repo string) (string, string) {
	var fallback string
	var fallbackName string
	for _, asset := range release.Assets {
		lower := strings.ToLower(asset.Name)
		if !strings.HasSuffix(lower, ".zip") {
			continue
		}
		if strings.Contains(lower, "source") {
			if fallback == "" {
				fallback = asset.BrowserDownloadURL
				fallbackName = asset.Name
			}
			continue
		}
		return asset.BrowserDownloadURL, asset.Name
	}
	if fallback != "" {
		return fallback, fallbackName
	}
	if release.ZipballURL != "" {
		return release.ZipballURL, repo + "-" + release.TagName + ".zip"
	}
	return "", ""
}

func (c *Client) fetchGitHubFile(ctx context.Context, raw string, name string) (Download, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Download{}, err
	}
	if !isGitHubURL(u) {
		return Download{}, &ExternalError{URL: raw}
	}
	if err := c.wait(ctx); err != nil {
		return Download{}, err
	}
	resp, final, err := c.fetch(ctx, u, c.allowedDownload, 5)
	if err != nil {
		return Download{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return Download{}, fmt.Errorf("the download from GitHub failed (%d)", resp.StatusCode)
	}
	data, err := readLimited(resp.Body, maxDownload)
	if err != nil {
		return Download{}, err
	}
	if name == "" {
		name = filenameFromDisposition(resp.Header.Get("Content-Disposition"))
	}
	if name == "" {
		name = path.Base(final.Path)
	}
	return Download{Kind: "file", Filename: name, Data: data, URL: final.String()}, nil
}
