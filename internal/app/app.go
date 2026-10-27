// Package app is the loader's core: it owns the state machine that the CLI,
// the HTTP API and (later) the desktop shell talk to.
//
// Every mutation follows the same rules:
//   - refuse to run while sm64coopdx is open (it rewrites its files on exit);
//   - back up sm64config.txt once before the first change of the session;
//   - write atomically;
//   - keep removed files in removed-by-coop-loader instead of deleting them.
package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/retired64/coop-loader/internal/catalog"
	"github.com/retired64/coop-loader/internal/conf"
	"github.com/retired64/coop-loader/internal/devicecopy"
	"github.com/retired64/coop-loader/internal/eeprom"
	"github.com/retired64/coop-loader/internal/fsutil"
	"github.com/retired64/coop-loader/internal/game"
	"github.com/retired64/coop-loader/internal/installed"
	"github.com/retired64/coop-loader/internal/mods"
	"github.com/retired64/coop-loader/internal/paths"
	"github.com/retired64/coop-loader/internal/session"
	"github.com/retired64/coop-loader/internal/settings"
)

// Version is the loader version shown in the UI.
const Version = "0.1.0"

// ErrGameRunning blocks mutations while the game is open.
var ErrGameRunning = errors.New("close the game first: it rewrites its files when it exits")

// ErrNoConfig means sm64coopdx has never run, so it has no settings file.
var ErrNoConfig = errors.New("start the game once so it creates its settings file")

// Service is the loader core.
type Service struct {
	mu sync.Mutex

	loaderDir string
	gameData  string
	settings  *settings.Store

	// running is a seam for tests; production always asks the OS.
	running func() bool

	installMu sync.Mutex

	catalogMu   sync.Mutex
	catalogBase string
	catalogGap  time.Duration
	catalogCli  *catalog.Client
	catalogErr  error

	records *installed.Store

	// startMu serialises starting and stopping a game room and a copy, so
	// both cannot become active at the same time.
	startMu sync.Mutex

	sessMu sync.Mutex
	sess   *session.Engine

	copyMu      sync.Mutex
	copyApplyMu sync.Mutex
	copyEng     *devicecopy.Engine

	configBackup sync.Once
}

// New builds a service rooted at explicit directories (used by tests).
func New(loaderDir, gameData string) *Service {
	records, _ := installed.Load(filepath.Join(loaderDir, "installed.json"))
	return &Service{
		loaderDir:   loaderDir,
		gameData:    gameData,
		settings:    settings.NewStore(loaderDir),
		running:     game.Running,
		catalogBase: catalog.DefaultBaseURL,
		records:     records,
	}
}

// NewDefault builds a service using the platform directories.
func NewDefault() (*Service, error) {
	loaderDir, err := paths.LoaderData()
	if err != nil {
		return nil, err
	}
	gameData, err := paths.GameData()
	if err != nil {
		return nil, err
	}
	return New(loaderDir, gameData), nil
}

// LoaderDir is where the loader keeps settings.json and profiles.json.
func (s *Service) LoaderDir() string { return s.loaderDir }

// GameDataDir is where sm64coopdx keeps mods, saves and sm64config.txt.
func (s *Service) GameDataDir() string { return s.gameData }

// Status is the snapshot the UI polls on start.
type Status struct {
	Version     string            `json:"version"`
	GamePath    string            `json:"gamePath"`
	GameFound   bool              `json:"gameFound"`
	GameData    string            `json:"gameData"`
	LoaderDir   string            `json:"loaderDir"`
	ConfigPath  string            `json:"configPath"`
	ConfigFound bool              `json:"configFound"`
	SavePath    string            `json:"savePath"`
	SaveFound   bool              `json:"saveFound"`
	Running     bool              `json:"running"`
	CloseOnPlay bool              `json:"closeOnPlay"`
	NetConsent  *settings.Consent `json:"netConsent,omitempty"`
	HostSlot    int               `json:"hostSlot"`
}

// Status returns the current state.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	gamePath := s.gamePathLocked()
	cfg := s.configPath()
	save := s.savePath()
	st := Status{
		Version:     Version,
		GamePath:    gamePath,
		GameFound:   game.Valid(gamePath),
		GameData:    s.gameData,
		LoaderDir:   s.loaderDir,
		ConfigPath:  cfg,
		ConfigFound: fsutil.Exists(cfg),
		SavePath:    save,
		SaveFound:   fsutil.Exists(save),
		Running:     game.Running(),
	}
	if v, err := s.settings.Load(); err == nil {
		st.CloseOnPlay = v.CloseOnPlay
		if v.NetConsent != nil {
			consent := *v.NetConsent
			st.NetConsent = &consent
		}
	}
	if doc, err := s.readConfigLocked(); err == nil {
		if slot, err := strconv.Atoi(doc.Value("coop_host_save_slot")); err == nil && slot >= 1 && slot <= 4 {
			st.HostSlot = slot
		}
	}
	if st.HostSlot == 0 {
		st.HostSlot = 1
	}
	return st
}

// Mods lists installed mods with their enabled state.
func (s *Service) Mods() []mods.Mod {
	s.mu.Lock()
	defer s.mu.Unlock()

	enabledMods := map[string]bool{}
	enabledPacks := map[string]bool{}
	if doc, err := s.readConfigLocked(); err == nil {
		for _, id := range doc.Mods() {
			enabledMods[id] = true
		}
		enabledPacks = doc.Packs()
	}
	return mods.List(s.pathsLocked(), enabledMods, enabledPacks)
}

// Toggle enables or disables one mod or pack. Enabling a mod that conflicts
// with others (same token in "-- incompatible:") turns those off and returns
// their names so the UI can explain what happened.
func (s *Service) Toggle(id string, kind mods.Kind, on bool) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running() {
		return nil, ErrGameRunning
	}
	doc, err := s.readConfigLocked()
	if err != nil {
		return nil, err
	}

	var disabled []string
	if on && kind == mods.KindMod {
		enabledMods := map[string]bool{}
		for _, id := range doc.Mods() {
			enabledMods[id] = true
		}
		list := mods.List(s.pathsLocked(), enabledMods, doc.Packs())
		var target mods.Mod
		for _, m := range list {
			if m.Kind == mods.KindMod && m.ID == id {
				target = m
				break
			}
		}
		tokens := tokensOf(target.Incompatible)
		if len(tokens) > 0 {
			for _, m := range list {
				if m.Kind != mods.KindMod || !m.Enabled || m.ID == id {
					continue
				}
				if sharesToken(tokens, tokensOf(m.Incompatible)) {
					doc.SetMod(m.ID, false)
					disabled = append(disabled, m.Name)
				}
			}
		}
	}

	if kind == mods.KindPack {
		doc.SetPack(id, on)
	} else {
		doc.SetMod(id, on)
	}
	if err := s.writeConfigLocked(doc); err != nil {
		return disabled, err
	}
	return disabled, nil
}

// Install adds a local zip/Lua mod. It does not enable it.
func (s *Service) Install(file string, allowCompiled bool) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running() {
		return nil, ErrGameRunning
	}
	return mods.InstallAny(s.pathsLocked(), file, allowCompiled)
}

// Remove takes a user mod or pack out of the game.
func (s *Service) Remove(id string, kind mods.Kind) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running() {
		return "", ErrGameRunning
	}
	moved, err := mods.Remove(s.pathsLocked(), id, kind)
	if err != nil {
		return "", err
	}
	// Drop it from the config and from every profile too.
	if doc, err := s.readConfigLocked(); err == nil {
		if kind == mods.KindPack {
			packs := doc.Packs()
			delete(packs, id)
			doc.SetPacks(packs)
		} else {
			doc.SetMod(id, false)
		}
		_ = s.writeConfigLocked(doc)
	}
	_ = s.records.Delete(id)
	if profiles, err := conf.LoadProfiles(s.profilesPath()); err == nil {
		changed := false
		for name, set := range profiles.Sets {
			filtered := set[:0]
			for _, v := range set {
				if v != id {
					filtered = append(filtered, v)
					continue
				}
				changed = true
			}
			profiles.Sets[name] = filtered
		}
		if changed {
			_ = profiles.Save(s.profilesPath())
		}
	}
	return moved, nil
}

// SaveState is the saves view.
type SaveState struct {
	Path   string        `json:"path"`
	Exists bool          `json:"exists"`
	Names  [4]string     `json:"names"`
	Slots  []eeprom.Slot `json:"slots"`
}

// Saves decodes the save file, or returns four empty slots when it is missing.
func (s *Service) Saves() (SaveState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state := SaveState{Path: s.savePath(), Slots: make([]eeprom.Slot, 0, eeprom.SlotCount)}
	if doc, err := s.readConfigLocked(); err == nil {
		state.Names = doc.SaveNames()
	}
	file, err := eeprom.Load(state.Path)
	if err != nil {
		if os.IsNotExist(err) {
			for i := 0; i < eeprom.SlotCount; i++ {
				state.Slots = append(state.Slots, eeprom.Slot{Index: i})
			}
			return state, nil
		}
		return state, err
	}
	state.Exists = true
	for _, slot := range file.Slots {
		state.Slots = append(state.Slots, slot)
	}
	return state, nil
}

// CopySlot duplicates one save slot onto another.
func (s *Service) CopySlot(from, to int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running() {
		return ErrGameRunning
	}
	file, err := s.loadSaveLocked()
	if err != nil {
		return err
	}
	if err := file.CopySlot(from, to); err != nil {
		return err
	}
	return file.Save(s.savePath())
}

// ClearSlot erases one save slot.
func (s *Service) ClearSlot(slot int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running() {
		return ErrGameRunning
	}
	file, err := s.loadSaveLocked()
	if err != nil {
		return err
	}
	if err := file.ClearSlot(slot); err != nil {
		return err
	}
	return file.Save(s.savePath())
}

// ImportSave installs a 512-byte save file, keeping a backup of the old one.
func (s *Service) ImportSave(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running() {
		return ErrGameRunning
	}
	file, err := eeprom.Load(path)
	if err != nil {
		return err
	}
	valid := false
	for _, slot := range file.Slots {
		if slot.Valid {
			valid = true
			break
		}
	}
	if !valid {
		return errors.New("that file has no valid save slots")
	}
	return file.Save(s.savePath())
}

// Profiles returns the profile document, creating the Default set from the
// current config the first time.
func (s *Service) Profiles() (conf.Profiles, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	profiles, err := conf.LoadProfiles(s.profilesPath())
	if err != nil {
		return profiles, err
	}
	if _, ok := profiles.Sets["Default"]; !ok {
		set := []string{}
		if doc, err := s.readConfigLocked(); err == nil {
			set = doc.Mods()
		}
		profiles.Sets["Default"] = set
		profiles.Active = "Default"
		_ = profiles.Save(s.profilesPath())
	}
	return profiles, nil
}

// SwitchProfile saves the current mod set and applies another one.
func (s *Service) SwitchProfile(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running() {
		return ErrGameRunning
	}
	doc, err := s.readConfigLocked()
	if err != nil {
		return err
	}
	profiles, err := conf.LoadProfiles(s.profilesPath())
	if err != nil {
		return err
	}
	set, ok := profiles.Sets[name]
	if !ok {
		return fmt.Errorf("no profile named %q", name)
	}
	profiles.Sets[profiles.Active] = doc.Mods()
	doc.SetMods(set)
	if err := s.writeConfigLocked(doc); err != nil {
		return err
	}
	profiles.Active = name
	return profiles.Save(s.profilesPath())
}

// CreateProfile creates a profile with the current mod set and switches to it.
func (s *Service) CreateProfile(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("give the profile a name")
	}
	if len(name) > 40 {
		return errors.New("profile names are at most 40 characters")
	}
	if name == "__proto__" {
		return errors.New("pick another name")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running() {
		return ErrGameRunning
	}
	doc, err := s.readConfigLocked()
	if err != nil {
		return err
	}
	profiles, err := conf.LoadProfiles(s.profilesPath())
	if err != nil {
		return err
	}
	if _, ok := profiles.Sets[name]; ok {
		return fmt.Errorf("there is already a profile named %q", name)
	}
	current := doc.Mods()
	profiles.Sets[profiles.Active] = current
	profiles.Sets[name] = append([]string(nil), current...)
	profiles.Active = name
	return profiles.Save(s.profilesPath())
}

// DeleteProfile removes an inactive profile.
func (s *Service) DeleteProfile(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	profiles, err := conf.LoadProfiles(s.profilesPath())
	if err != nil {
		return err
	}
	if name == profiles.Active {
		return errors.New("switch to another profile before deleting this one")
	}
	if _, ok := profiles.Sets[name]; !ok {
		return fmt.Errorf("no profile named %q", name)
	}
	delete(profiles.Sets, name)
	return profiles.Save(s.profilesPath())
}

// SetGamePath validates and stores the game folder.
func (s *Service) SetGamePath(dir string) error {
	dir = strings.TrimSpace(dir)
	resolved, ok := game.Resolve(dir)
	if !ok {
		return game.ErrNotFound
	}
	dir = resolved
	s.mu.Lock()
	defer s.mu.Unlock()

	v, err := s.settings.Load()
	if err != nil {
		return err
	}
	v.GamePath = dir
	return s.settings.Save(v)
}

// SetCloseOnPlay stores the "close the loader when the game starts" choice.
func (s *Service) SetCloseOnPlay(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	v, err := s.settings.Load()
	if err != nil {
		return err
	}
	v.CloseOnPlay = on
	return s.settings.Save(v)
}

// Launch starts the game with optional extra arguments.
func (s *Service) Launch(args []string) error {
	return s.LaunchNotify(args, nil)
}

// LaunchNotify starts the game and runs onExit when it closes. The callback
// must not block.
func (s *Service) LaunchNotify(args []string, onExit func()) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.gamePathLocked()
	if !game.Valid(dir) {
		return game.ErrNotFound
	}
	return game.LaunchNotify(dir, args, onExit)
}

// DetectGame tries the usual folders and stores the result when found.
func (s *Service) DetectGame() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := game.Detect()
	if dir != "" {
		if v, err := s.settings.Load(); err == nil {
			v.GamePath = dir
			_ = s.settings.Save(v)
		}
	}
	return dir
}

// --- internals -------------------------------------------------------------

func (s *Service) pathsLocked() mods.Paths {
	return mods.Paths{GameDir: s.gamePathLocked(), GameData: s.gameData}
}

func (s *Service) configPath() string { return filepath.Join(s.gameData, "sm64config.txt") }

func (s *Service) profilesPath() string { return filepath.Join(s.loaderDir, "profiles.json") }

func (s *Service) savePath() string { return filepath.Join(s.gameData, "sm64_save_file.bin") }

// gamePathLocked returns the configured folder, falling back to detection.
func (s *Service) gamePathLocked() string {
	v, err := s.settings.Load()
	if err == nil && game.Valid(v.GamePath) {
		return v.GamePath
	}
	if dir := game.Detect(); dir != "" {
		if err == nil {
			v.GamePath = dir
			_ = s.settings.Save(v)
		}
		return dir
	}
	return v.GamePath
}

func (s *Service) readConfigLocked() (*conf.Doc, error) {
	data, err := os.ReadFile(s.configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoConfig
		}
		return nil, err
	}
	return conf.Parse(data), nil
}

// writeConfigLocked backs up the original once, then writes atomically.
func (s *Service) writeConfigLocked(doc *conf.Doc) error {
	s.configBackup.Do(func() {
		backup := filepath.Join(s.gameData, "sm64config.modloader-backup.txt")
		original := filepath.Join(s.gameData, "sm64config.modloader-original.txt")
		if fsutil.Exists(s.configPath()) {
			_ = fsutil.CopyFile(s.configPath(), backup, 0o644)
			if !fsutil.Exists(original) {
				_ = fsutil.CopyFile(s.configPath(), original, 0o644)
			}
		}
	})
	return fsutil.WriteFileAtomic(s.configPath(), doc.Bytes(), 0o644)
}

func (s *Service) loadSaveLocked() (*eeprom.File, error) {
	file, err := eeprom.Load(s.savePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New("there is no save file yet: start the game once")
		}
		return nil, err
	}
	return file, nil
}

func tokensOf(s string) []string {
	fields := strings.Fields(strings.ToLower(s))
	out := fields[:0]
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func sharesToken(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}
