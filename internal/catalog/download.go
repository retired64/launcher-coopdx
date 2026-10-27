package catalog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

const maxDownload = 1 << 30 // 1 GiB, same ceiling the loader uses everywhere

// Download resolves the download button of a mod detail page.
//
// It returns:
//   - Kind "file": the archive is in Data.
//   - Kind "choose": the mod ships several files; the caller picks one and
//     calls DownloadFile.
//   - Kind "external": the file lives somewhere else (often GitHub).
func (c *Client) Download(ctx context.Context, detailURL string) (Download, error) {
	ref, err := c.resolve(detailURL)
	if err != nil {
		return Download{}, err
	}
	if !c.allowedPage(ref) {
		return Download{}, &ExternalError{URL: ref.String()}
	}

	target := *ref
	target.Path = strings.TrimRight(target.Path, "/") + "/download"
	target.RawQuery = ""
	target.Fragment = ""

	if err := c.wait(ctx); err != nil {
		return Download{}, err
	}
	resp, final, err := c.fetch(ctx, &target, c.allowedPage, 5)
	if err != nil {
		var ext *ExternalError
		if errors.As(err, &ext) {
			return c.externalDownload(ctx, ext.URL, ref)
		}
		return Download{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return Download{}, fmt.Errorf("the mod site answered %d", resp.StatusCode)
	}
	return c.fromResponse(resp, final, ref)
}

// DownloadFile fetches one of the files offered by a "choose" download.
func (c *Client) DownloadFile(ctx context.Context, detailURL, fileURL string) (Download, error) {
	detail, err := c.resolve(detailURL)
	if err != nil {
		return Download{}, err
	}
	file, err := c.resolve(fileURL)
	if err != nil {
		return Download{}, err
	}
	if !c.sameOrigin(file) {
		return Download{}, errors.New("that file is not part of this mod")
	}
	if !strings.HasPrefix(file.Path, strings.TrimRight(detail.Path, "/")+"/") {
		return Download{}, errors.New("that file is not part of this mod")
	}
	if err := c.wait(ctx); err != nil {
		return Download{}, err
	}
	resp, final, err := c.fetch(ctx, file, c.allowedDownload, 5)
	if err != nil {
		return Download{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return Download{}, fmt.Errorf("the mod site answered %d", resp.StatusCode)
	}

	disp := resp.Header.Get("Content-Disposition")
	ctype := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ctype, "html") {
		return Download{}, errors.New("the site returned a page instead of a file")
	}
	data, err := readLimited(resp.Body, maxDownload)
	if err != nil {
		return Download{}, err
	}
	name := filenameFromDisposition(disp)
	if name == "" {
		name = path.Base(final.Path)
	}
	return Download{Kind: "file", Filename: name, Data: data, URL: final.String()}, nil
}

func (c *Client) fromResponse(resp *http.Response, final, detail *url.URL) (Download, error) {
	disp := resp.Header.Get("Content-Disposition")
	ctype := strings.ToLower(resp.Header.Get("Content-Type"))

	if strings.Contains(strings.ToLower(disp), "attachment") {
		data, err := readLimited(resp.Body, maxDownload)
		if err != nil {
			return Download{}, err
		}
		name := filenameFromDisposition(disp)
		if name == "" {
			name = path.Base(final.Path)
		}
		return Download{Kind: "file", Filename: name, Data: data, URL: final.String()}, nil
	}

	if strings.Contains(ctype, "zip") || strings.Contains(ctype, "octet-stream") {
		data, err := readLimited(resp.Body, maxDownload)
		if err != nil {
			return Download{}, err
		}
		return Download{Kind: "file", Filename: path.Base(final.Path), Data: data, URL: final.String()}, nil
	}

	// Not an attachment: the site is showing a page, usually a file picker.
	body, err := readLimited(resp.Body, c.maxHTML)
	if err != nil {
		return Download{}, err
	}
	if files := parseDownloadPage(body, c.base, detail); len(files) > 0 {
		return Download{Kind: "choose", Files: files, URL: final.String()}, nil
	}
	return Download{}, errors.New("the download needs the website: open the mod page and get the file there")
}

// externalDownload handles a redirect that leaves the site.
func (c *Client) externalDownload(ctx context.Context, raw string, detail *url.URL) (Download, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Download{}, err
	}
	if !isGitHubURL(u) {
		return Download{Kind: "external", URL: raw}, nil
	}
	if githubRepoRe.MatchString(raw) {
		return c.resolveGitHub(ctx, raw)
	}
	return c.fetchGitHubFile(ctx, raw, "")
}

func parseDownloadPage(body []byte, base *url.URL, detail *url.URL) []FileOption {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	var out []FileOption
	seen := map[string]bool{}

	add := func(href, name, size string) {
		abs := absURL(base, href)
		if abs == "" || seen[abs] {
			return
		}
		// A file must live under the mod's own page.
		u, err := url.Parse(abs)
		if err != nil || !strings.HasPrefix(u.Path, strings.TrimRight(detail.Path, "/")+"/") {
			return
		}
		seen[abs] = true
		if strings.TrimSpace(name) == "" {
			name = path.Base(u.Path)
		}
		out = append(out, FileOption{Name: strings.TrimSpace(name), Size: strings.TrimSpace(size), URL: abs})
	}

	doc.Find("li.block-row").Each(func(_ int, s *goquery.Selection) {
		link := s.Find(`a[href*="download?file="]`).First()
		href, _ := link.Attr("href")
		name := strings.TrimSpace(s.Find(".contentRow-title").First().Text())
		size := strings.TrimSpace(s.Find(".contentRow-minor").First().Text())
		add(href, name, size)
	})
	if len(out) == 0 {
		doc.Find(`a[href*="download?file="]`).Each(func(_ int, s *goquery.Selection) {
			href, _ := s.Attr("href")
			add(href, strings.TrimSpace(s.Text()), "")
		})
	}
	return out
}

var (
	dispRe = regexp.MustCompile(`(?i)filename\*?=(?:UTF-8'')?"?([^";]+)"?`)
)

func filenameFromDisposition(disp string) string {
	m := dispRe.FindStringSubmatch(disp)
	if len(m) != 2 {
		return ""
	}
	name := strings.TrimSpace(m[1])
	if decoded, err := url.QueryUnescape(name); err == nil {
		name = decoded
	}
	// Never let a header choose a path.
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" {
		return ""
	}
	return name
}
