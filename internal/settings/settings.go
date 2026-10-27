// Package settings persists the loader's own preferences.
package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/retired64/coop-loader/internal/fsutil"
)

// Consent is the record of the player accepting that online play shares
// their IP with meeting points, the relay and the address-lookup services.
type Consent struct {
	Accepted     bool   `json:"accepted"`
	TermsVersion int    `json:"termsVersion"`
	At           string `json:"at,omitempty"`
}

// Settings is the JSON document stored in the loader data directory.
type Settings struct {
	// GamePath is the folder that contains the sm64coopdx executable.
	GamePath string `json:"gamePath"`
	// ROMPath is the Super Mario 64 (USA) file the look was read from.
	ROMPath string `json:"romPath,omitempty"`
	// CloseOnPlay quits the loader when the game starts from the Mods tab.
	CloseOnPlay bool `json:"closeOnPlay"`
	// Profile is the last active mod profile.
	Profile string `json:"profile"`
	// NetConsent remembers the online-play notice; a new terms version asks
	// again.
	NetConsent *Consent `json:"netConsent,omitempty"`
}

// Default returns the settings used on first start.
func Default() Settings {
	return Settings{CloseOnPlay: true, Profile: "Default"}
}

// Store reads and writes settings.json safely.
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore returns a store rooted at dir.
func NewStore(dir string) *Store { return &Store{dir: dir} }

// Path is the settings file location.
func (s *Store) Path() string { return filepath.Join(s.dir, "settings.json") }

// Load returns the stored settings merged over the defaults.
func (s *Store) Load() (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := Default()
	data, err := os.ReadFile(s.Path())
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		// A corrupt settings file must not brick the loader.
		return Default(), nil
	}
	if out.Profile == "" {
		out.Profile = "Default"
	}
	return out, nil
}

// Save writes the settings atomically.
func (s *Store) Save(v Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsutil.WriteFileAtomic(s.Path(), data, 0o644)
}
