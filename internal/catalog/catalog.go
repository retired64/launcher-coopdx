// Package catalog reads the community mod site (a XenForo forum) and resolves
// downloads.
//
// The site has no JSON API, so this package fetches HTML and extracts the same
// information a browser would show. Every request goes through a one-hour
// cache and an 800 ms gap so the loader is a polite client.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/retired64/coop-loader/internal/fsutil"
)

// DefaultBaseURL is the mod site the loader talks to.
const DefaultBaseURL = "https://mods.sm64coopdx.com"

// Sort values accepted by the site.
const (
	SortDownloads = "downloads"
	SortRating    = "rating"
	SortUpdated   = "updated"
	SortNewest    = "newest"
)

// Category is one entry of the site's category list.
type Category struct {
	ID    int    `json:"id"`
	Slug  string `json:"slug"`
	Title string `json:"title"`
	Depth int    `json:"depth"`
}

// Item is a mod as shown in a listing or search result.
type Item struct {
	ID          int     `json:"id"`
	URL         string  `json:"url"`
	Title       string  `json:"title"`
	Version     string  `json:"version"`
	Author      string  `json:"author"`
	Category    string  `json:"category"`
	Tagline     string  `json:"tagline"`
	Icon        string  `json:"icon"`
	Rating      float64 `json:"rating"`
	RatingCount int     `json:"ratingCount"`
	Downloads   int     `json:"downloads"`
	Updated     string  `json:"updated"`
}

// Inline is one run of text with optional formatting or a link.
type Inline struct {
	Text      string `json:"text,omitempty"`
	Href      string `json:"href,omitempty"`
	Bold      bool   `json:"bold,omitempty"`
	Italic    bool   `json:"italic,omitempty"`
	Underline bool   `json:"underline,omitempty"`
	Strike    bool   `json:"strike,omitempty"`
	Code      bool   `json:"code,omitempty"`
	Break     bool   `json:"br,omitempty"`
}

// Block is a paragraph, heading, list, quote or image in a mod description.
type Block struct {
	Kind   string     `json:"kind"`
	Level  int        `json:"level,omitempty"`
	Inline []Inline   `json:"inline,omitempty"`
	Items  [][]Inline `json:"items,omitempty"`
	Src    string     `json:"src,omitempty"`
}

// Candidate is a mod linked from a description, usually a requirement.
type Candidate struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Detail is a mod page.
type Detail struct {
	Item
	Blocks []Block     `json:"blocks"`
	Links  []Candidate `json:"links"`
}

// Query describes a listing request.
type Query struct {
	Sort     string
	Category int
	Search   string
	Page     int
}

// Listing is one page of results.
type Listing struct {
	Items     []Item `json:"items"`
	Page      int    `json:"page"`
	Pages     int    `json:"pages"`
	FromCache bool   `json:"fromCache"`
}

// FileOption is one file inside a mod download page.
type FileOption struct {
	Name string `json:"name"`
	Size string `json:"size"`
	URL  string `json:"url"`
}

// Download is what a mod's download button resolves to.
type Download struct {
	Kind     string // file, choose, external
	URL      string
	Filename string
	Data     []byte
	Files    []FileOption
}

// ExternalError means a request left the allowed hosts.
type ExternalError struct{ URL string }

func (e *ExternalError) Error() string { return "external URL: " + e.URL }

// Options configures a Client. Zero values pick the defaults.
type Options struct {
	BaseURL    string
	CachePath  string
	UserAgent  string
	Gap        time.Duration
	TTL        time.Duration
	MaxEntries int
	Timeout    time.Duration
	MaxHTML    int64
}

// Client fetches and parses the mod site.
type Client struct {
	base    *url.URL
	http    *http.Client
	ua      string
	gap     time.Duration
	ttl     time.Duration
	max     int
	maxHTML int64
	cache   string

	mu      sync.Mutex
	next    time.Time
	entries map[string]cacheEntry
	dirty   bool
}

type cacheEntry struct {
	At    time.Time `json:"at"`
	Final string    `json:"final"`
	HTML  string    `json:"html"`
}

type cacheFile struct {
	Entries map[string]cacheEntry `json:"entries"`
}

// New builds a client.
func New(opts Options) (*Client, error) {
	base := opts.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("catalog: unsupported scheme %q", u.Scheme)
	}

	c := &Client{
		base:    u,
		ua:      opts.UserAgent,
		gap:     opts.Gap,
		ttl:     opts.TTL,
		max:     opts.MaxEntries,
		maxHTML: opts.MaxHTML,
		cache:   opts.CachePath,
		entries: map[string]cacheEntry{},
	}
	if c.ua == "" {
		c.ua = "coop-loader (desktop mod manager)"
	}
	if c.gap == 0 {
		c.gap = 800 * time.Millisecond
	}
	if c.ttl == 0 {
		c.ttl = time.Hour
	}
	if c.max == 0 {
		c.max = 120
	}
	if c.maxHTML == 0 {
		c.maxHTML = 4 << 20
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	c.http = &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	c.loadCache()
	return c, nil
}

// BaseURL is the site the client talks to.
func (c *Client) BaseURL() string { return c.base.String() }

// Close persists the cache.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == "" || !c.dirty {
		return nil
	}
	c.pruneLocked()
	data, err := json.Marshal(cacheFile{Entries: c.entries})
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(c.cache, data, 0o644); err != nil {
		return err
	}
	c.dirty = false
	return nil
}

// ClearCache drops every cached page.
func (c *Client) ClearCache() error {
	c.mu.Lock()
	c.entries = map[string]cacheEntry{}
	c.dirty = true
	c.mu.Unlock()
	return c.Close()
}

// Page fetches an HTML page, using the cache when possible.
func (c *Client) Page(ctx context.Context, ref string) (body []byte, final string, fromCache bool, err error) {
	u, err := c.resolve(ref)
	if err != nil {
		return nil, "", false, err
	}
	if !c.allowedPage(u) {
		return nil, u.String(), false, &ExternalError{URL: u.String()}
	}

	key := u.String()
	if data, cachedFinal, ok := c.lookup(key); ok {
		return data, cachedFinal, true, nil
	}
	if err := c.wait(ctx); err != nil {
		return nil, key, false, err
	}
	resp, finalURL, err := c.fetch(ctx, u, c.allowedPage, 5)
	if err != nil {
		return nil, key, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, finalURL.String(), false, fmt.Errorf("the mod site answered %d", resp.StatusCode)
	}
	ctype := resp.Header.Get("Content-Type")
	if ctype != "" && !strings.Contains(strings.ToLower(ctype), "html") {
		return nil, finalURL.String(), false, fmt.Errorf("expected a page, got %s", ctype)
	}
	data, err := readLimited(resp.Body, c.maxHTML)
	if err != nil {
		return nil, finalURL.String(), false, err
	}
	c.store(key, finalURL.String(), data)
	return data, finalURL.String(), false, nil
}

// List returns one page of mods.
func (c *Client) List(ctx context.Context, q Query) (Listing, error) {
	page := q.Page
	if page < 1 {
		page = 1
	}
	q.Page = page

	if strings.TrimSpace(q.Search) != "" {
		path := "/search/search?" + url.Values{
			"keywords":    {strings.TrimSpace(q.Search)},
			"search_type": {"resource"},
			"order":       {"relevance"},
		}.Encode()
		body, final, cached, err := c.Page(ctx, path)
		if err != nil {
			return Listing{}, err
		}
		if page > 1 {
			body, final, cached, err = c.Page(ctx, withPage(final, page))
			if err != nil {
				return Listing{}, err
			}
		}
		items, pages := parseList(body, c.base)
		return Listing{Items: items, Page: page, Pages: pages, FromCache: cached}, nil
	}

	body, _, cached, err := c.Page(ctx, listPath(q))
	if err != nil {
		return Listing{}, err
	}
	items, pages := parseList(body, c.base)
	return Listing{Items: items, Page: page, Pages: pages, FromCache: cached}, nil
}

// Categories returns the site's category tree.
func (c *Client) Categories(ctx context.Context) ([]Category, error) {
	body, _, _, err := c.Page(ctx, "/mods/")
	if err != nil {
		return nil, err
	}
	return parseCategories(body), nil
}

// Detail returns one mod page.
func (c *Client) Detail(ctx context.Context, ref string) (Detail, error) {
	body, final, _, err := c.Page(ctx, ref)
	if err != nil {
		return Detail{}, err
	}
	return parseDetail(body, final, c.base), nil
}

func listPath(q Query) string {
	base := "/mods/"
	if q.Category > 0 {
		base = fmt.Sprintf("/mods/categories/%d/", q.Category)
	}
	order := map[string]string{
		SortDownloads: "download_count",
		SortRating:    "rating_weighted",
		SortUpdated:   "last_update",
		SortNewest:    "resource_date",
	}[q.Sort]
	if order == "" {
		order = "download_count"
	}
	v := url.Values{"order": {order}, "direction": {"desc"}}
	if q.Page > 1 {
		v.Set("page", strconv.Itoa(q.Page))
	}
	return base + "?" + v.Encode()
}

func withPage(raw string, page int) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set("page", strconv.Itoa(page))
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Client) resolve(ref string) (*url.URL, error) {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return url.Parse(ref)
	}
	return c.base.Parse(ref)
}

func (c *Client) sameOrigin(u *url.URL) bool {
	return strings.EqualFold(u.Scheme, c.base.Scheme) && strings.EqualFold(u.Host, c.base.Host)
}

func (c *Client) allowedPage(u *url.URL) bool {
	return c.sameOrigin(u)
}

func (c *Client) allowedDownload(u *url.URL) bool {
	return c.sameOrigin(u) || isGitHubURL(u)
}

func (c *Client) fetch(ctx context.Context, start *url.URL, allow func(*url.URL) bool, hops int) (*http.Response, *url.URL, error) {
	current := start
	for i := 0; i <= hops; i++ {
		if !allow(current) {
			return nil, current, &ExternalError{URL: current.String()}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return nil, current, err
		}
		req.Header.Set("User-Agent", c.ua)
		req.Header.Set("Accept", "text/html,application/json,application/zip,application/octet-stream,*/*")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, current, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if loc == "" {
				return nil, current, fmt.Errorf("redirect without a location")
			}
			next, err := current.Parse(loc)
			if err != nil {
				return nil, current, err
			}
			current = next
			continue
		}
		return resp, current, nil
	}
	return nil, current, fmt.Errorf("too many redirects")
}

func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	now := time.Now()
	at := c.next
	if at.Before(now) {
		at = now
	}
	c.next = at.Add(c.gap)
	c.mu.Unlock()

	if d := time.Until(at); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

func (c *Client) lookup(key string) ([]byte, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || time.Since(entry.At) > c.ttl || entry.HTML == "" {
		return nil, "", false
	}
	return []byte(entry.HTML), entry.Final, true
}

func (c *Client) store(key, final string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := cacheEntry{At: time.Now(), Final: final, HTML: string(body)}
	c.entries[key] = entry
	if final != "" && final != key {
		c.entries[final] = entry
	}
	c.pruneLocked()
	c.dirty = true
}

func (c *Client) pruneLocked() {
	now := time.Now()
	for k, e := range c.entries {
		if now.Sub(e.At) > c.ttl {
			delete(c.entries, k)
		}
	}
	if len(c.entries) <= c.max {
		return
	}
	type pair struct {
		key string
		at  time.Time
	}
	all := make([]pair, 0, len(c.entries))
	for k, e := range c.entries {
		all = append(all, pair{k, e.At})
	}
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].at.Before(all[j-1].at); j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	for _, p := range all[:len(all)-c.max] {
		delete(c.entries, p.key)
	}
}

func (c *Client) loadCache() {
	if c.cache == "" {
		return
	}
	data, err := os.ReadFile(c.cache)
	if err != nil {
		return
	}
	var f cacheFile
	if err := json.Unmarshal(data, &f); err != nil {
		return
	}
	now := time.Now()
	for k, e := range f.Entries {
		if now.Sub(e.At) <= c.ttl {
			c.entries[k] = e
		}
	}
	c.pruneLocked()
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, fmt.Errorf("the response is bigger than %d bytes", limit)
	}
	return buf.Bytes(), nil
}

var modPathRe = regexp.MustCompile(`/mods/(?:[^/]+\.)?(\d+)/?$`)

func idFromURL(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	m := modPathRe.FindStringSubmatch(u.Path)
	if len(m) != 2 {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
