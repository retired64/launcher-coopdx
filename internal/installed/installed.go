// Package installed remembers which mods came from the mod site.
//
// It is the loader's own bookkeeping: the game never sees it. It lets the UI
// answer "is this installed?", "is there a newer version?" and "where did this
// mod come from?" without scanning the whole site.
package installed

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/retired64/coop-loader/internal/fsutil"
)

// Record is the provenance of one installed mod.
type Record struct {
	SiteID      int       `json:"siteId"`
	SiteURL     string    `json:"siteUrl"`
	Title       string    `json:"title"`
	Version     string    `json:"version"`
	InstalledAt time.Time `json:"installedAt"`
}

// Store is a JSON file mapping mod id -> Record.
type Store struct {
	path string

	mu   sync.Mutex
	recs map[string]Record
}

type file struct {
	Version int               `json:"v"`
	Mods    map[string]Record `json:"mods"`
}

// Load reads installed.json. A missing or corrupt file yields an empty store.
func Load(path string) (*Store, error) {
	s := &Store{path: path, recs: map[string]Record{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return s, nil // never brick the loader because of our own cache
	}
	for id, rec := range f.Mods {
		s.recs[id] = rec
	}
	return s, nil
}

// Get returns the record for one mod id.
func (s *Store) Get(id string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.recs[id]
	return rec, ok
}

// All returns a copy of every record.
func (s *Store) All() map[string]Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Record, len(s.recs))
	for id, rec := range s.recs {
		out[id] = rec
	}
	return out
}

// Set associates one site record with every installed mod id.
func (s *Store) Set(ids []string, rec Record) error {
	if len(ids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if id == "" {
			continue
		}
		s.recs[id] = rec
	}
	return s.saveLocked()
}

// Delete forgets one mod.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.recs[id]; !ok {
		return nil
	}
	delete(s.recs, id)
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(file{Version: 1, Mods: s.recs}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsutil.WriteFileAtomic(s.path, data, 0o644)
}

// SameVersion compares two version strings after trimming spaces and a leading
// "v". "1.0" and "1.0.0" are treated as different; the site is consistent about
// this and guessing further would hide real updates.
func SameVersion(a, b string) bool {
	return NormalizeVersion(a) == NormalizeVersion(b)
}

// NormalizeVersion is the form used for comparisons and display.
func NormalizeVersion(v string) string {
	v = trimSpace(v)
	for len(v) > 0 && (v[0] == 'v' || v[0] == 'V') {
		v = v[1:]
	}
	return trimSpace(v)
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\r' || s[start] == '\n') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\r' || s[end-1] == '\n') {
		end--
	}
	return s[start:end]
}
