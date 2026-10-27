package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/retired64/coop-loader/internal/catalog"
	"github.com/retired64/coop-loader/internal/deps"
	"github.com/retired64/coop-loader/internal/installed"
	"github.com/retired64/coop-loader/internal/mods"
)

// LocalMod is a mod on disk plus what the loader remembers about where it came
// from.
type LocalMod struct {
	mods.Mod
	Site *installed.Record `json:"site,omitempty"`
}

// SiteInstall is the outcome of installing from the mod site.
type SiteInstall struct {
	Kind      string               `json:"kind"` // installed, choose, external
	Installed []string             `json:"installed,omitempty"`
	Requires  []Requirement        `json:"requires,omitempty"`
	URL       string               `json:"url,omitempty"`
	Files     []catalog.FileOption `json:"files,omitempty"`
}

// Requirement is a global a mod needs but no enabled mod provides.
type Requirement struct {
	Global string              `json:"global"`
	Query  string              `json:"query,omitempty"`
	Local  []deps.Provider     `json:"local,omitempty"`
	Site   []catalog.Candidate `json:"site,omitempty"`
}

// cat is the lazily-created mod site client.
func (s *Service) cat() (*catalog.Client, error) {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	if s.catalogCli != nil || s.catalogErr != nil {
		return s.catalogCli, s.catalogErr
	}
	cli, err := catalog.New(catalog.Options{
		BaseURL:   s.catalogBase,
		CachePath: filepath.Join(s.loaderDir, "site-cache.json"),
		UserAgent: "coop-loader/" + Version + " (desktop mod manager)",
		Gap:       s.catalogGap,
	})
	s.catalogCli, s.catalogErr = cli, err
	return cli, err
}

// Close persists the site cache. Call it before the process exits.
func (s *Service) Close() error {
	// Give the room a chance to send its leave and close its sockets.
	s.StopSession()

	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	if s.catalogCli == nil {
		return nil
	}
	err := s.catalogCli.Close()
	s.catalogCli = nil
	return err
}

// SetCatalogBaseURL points the catalog at another server. Used by tests and by
// people running a mirror.
func (s *Service) SetCatalogBaseURL(base string) {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	if s.catalogCli != nil {
		_ = s.catalogCli.Close()
	}
	s.catalogBase = base
	s.catalogCli = nil
	s.catalogErr = nil
}

// Categories returns the mod site categories.
func (s *Service) Categories(ctx context.Context) ([]catalog.Category, error) {
	cli, err := s.cat()
	if err != nil {
		return nil, err
	}
	return cli.Categories(ctx)
}

// Browse returns one page of the mod site.
func (s *Service) Browse(ctx context.Context, q catalog.Query) (catalog.Listing, error) {
	cli, err := s.cat()
	if err != nil {
		return catalog.Listing{}, err
	}
	return cli.List(ctx, q)
}

// Detail returns one mod page.
func (s *Service) Detail(ctx context.Context, ref string) (catalog.Detail, error) {
	cli, err := s.cat()
	if err != nil {
		return catalog.Detail{}, err
	}
	return cli.Detail(ctx, ref)
}

// ClearCatalogCache forgets every cached page.
func (s *Service) ClearCatalogCache() error {
	cli, err := s.cat()
	if err != nil {
		return err
	}
	return cli.ClearCache()
}

// LocalMods adds the site provenance to the installed mods.
func (s *Service) LocalMods() []LocalMod {
	list := s.Mods()
	records := s.records.All()
	out := make([]LocalMod, 0, len(list))
	for _, m := range list {
		local := LocalMod{Mod: m}
		if rec, ok := records[m.ID]; ok {
			copied := rec
			local.Site = &copied
		}
		out = append(out, local)
	}
	return out
}

// InstallFromSite downloads a mod and installs it, remembering its version.
//
// The flow can stop early with Kind "choose" (the mod ships several files) or
// "external" (the file is hosted somewhere the loader will not download from).
func (s *Service) InstallFromSite(ctx context.Context, detailURL, fileURL string, allowCompiled bool) (SiteInstall, error) {
	if s.running() {
		return SiteInstall{}, ErrGameRunning
	}
	s.installMu.Lock()
	defer s.installMu.Unlock()

	cli, err := s.cat()
	if err != nil {
		return SiteInstall{}, err
	}
	detail, err := cli.Detail(ctx, detailURL)
	if err != nil {
		return SiteInstall{}, err
	}
	if detail.ID == 0 || detail.URL == "" {
		return SiteInstall{}, fmt.Errorf("that is not a mod page")
	}

	var dl catalog.Download
	if strings.TrimSpace(fileURL) != "" {
		dl, err = cli.DownloadFile(ctx, detail.URL, fileURL)
	} else {
		dl, err = cli.Download(ctx, detail.URL)
	}
	if err != nil {
		return SiteInstall{}, err
	}

	switch dl.Kind {
	case "external":
		return SiteInstall{Kind: "external", URL: dl.URL}, nil
	case "choose":
		return SiteInstall{Kind: "choose", Files: dl.Files}, nil
	case "file":
		// continue
	default:
		return SiteInstall{}, fmt.Errorf("unexpected download kind %q", dl.Kind)
	}

	tmpDir := filepath.Join(s.loaderDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return SiteInstall{}, err
	}
	ext := strings.ToLower(filepath.Ext(dl.Filename))
	switch ext {
	case ".zip", ".lua", ".luac":
	default:
		ext = ".zip"
	}
	tmp, err := os.CreateTemp(tmpDir, "download-*"+ext)
	if err != nil {
		return SiteInstall{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(dl.Data); err != nil {
		tmp.Close()
		return SiteInstall{}, err
	}
	if err := tmp.Close(); err != nil {
		return SiteInstall{}, err
	}

	scan, _ := deps.ScanPath(tmpName)

	installedIDs, err := mods.InstallAny(s.paths(), tmpName, allowCompiled)
	if err != nil {
		return SiteInstall{}, err
	}

	record := installed.Record{
		SiteID:      detail.ID,
		SiteURL:     detail.URL,
		Title:       detail.Title,
		Version:     installed.NormalizeVersion(detail.Version),
		InstalledAt: time.Now().UTC(),
	}
	if err := s.records.Set(installedIDs, record); err != nil {
		return SiteInstall{}, err
	}

	return SiteInstall{
		Kind:      "installed",
		Installed: installedIDs,
		Requires:  s.requirements(ctx, cli, scan, installedIDs, detail.Links),
	}, nil
}

// requirements turns the globals a new mod asks for into actionable choices:
// enable an installed provider, or install one from the site.
func (s *Service) requirements(ctx context.Context, cli *catalog.Client, scan deps.Result, installedIDs []string, links []catalog.Candidate) []Requirement {
	local := s.Mods()
	sources := make([]deps.Source, 0, len(local))
	for _, m := range local {
		if m.Kind != mods.KindMod {
			continue
		}
		sources = append(sources, deps.Source{
			ID:       m.ID,
			Name:     m.Name,
			Dir:      m.Path,
			IsFolder: m.IsFolder,
			Enabled:  m.Enabled,
		})
	}
	index := deps.Index(sources)

	newIDs := map[string]bool{}
	for _, id := range installedIDs {
		newIDs[id] = true
	}

	var out []Requirement
	searches := 0
	for _, global := range scan.Required {
		if _, own := scan.Provided[global]; own {
			continue
		}

		var providers []deps.Provider
		active := false
		for _, provider := range index[global] {
			if newIDs[provider.ID] {
				continue
			}
			providers = append(providers, provider)
			if provider.Enabled {
				active = true
			}
		}
		if active {
			continue
		}

		var site []catalog.Candidate
		for _, link := range links {
			if deps.Match(global, link.Title) {
				site = appendCandidate(site, link)
			}
		}
		if len(providers) == 0 && len(site) == 0 && searches < 3 {
			searches++
			query := deps.HumanName(global)
			if query != "" {
				if listing, err := cli.List(ctx, catalog.Query{Search: query}); err == nil {
					for _, item := range listing.Items {
						if deps.Match(global, item.Title) {
							site = appendCandidate(site, catalog.Candidate{ID: item.ID, Title: item.Title, URL: item.URL})
						}
					}
				}
			}
		}

		out = append(out, Requirement{
			Global: global,
			Query:  deps.HumanName(global),
			Local:  limitProviders(providers, 3),
			Site:   site,
		})
		if len(out) >= 8 {
			break
		}
	}
	return out
}

func appendCandidate(list []catalog.Candidate, candidate catalog.Candidate) []catalog.Candidate {
	if candidate.ID == 0 || len(list) >= 3 {
		return list
	}
	for _, existing := range list {
		if existing.ID == candidate.ID {
			return list
		}
	}
	return append(list, candidate)
}

func limitProviders(list []deps.Provider, max int) []deps.Provider {
	if len(list) <= max {
		return list
	}
	return list[:max]
}

func (s *Service) paths() mods.Paths {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pathsLocked()
}
