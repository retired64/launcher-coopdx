package app

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/retired64/coop-loader/internal/conf"
	"github.com/retired64/coop-loader/internal/devicecopy"
	"github.com/retired64/coop-loader/internal/eeprom"
	"github.com/retired64/coop-loader/internal/fsutil"
	"github.com/retired64/coop-loader/internal/installed"
	"github.com/retired64/coop-loader/internal/mods"
	"github.com/retired64/coop-loader/internal/session"
)

// Copy errors used by the API.
var (
	ErrCopyInput   = errors.New("that copy setting is not valid")
	ErrNoCopy      = errors.New("there is no copy running")
	ErrCopyActive  = errors.New("finish the copy first")
	ErrSessionBusy = errors.New("finish playing with friends first")
)

// copyFile is one file in the sender's manifest order.
type copyFile struct {
	path string
	rel  string
	size int64
	code bool
	hash string

	modIndex  int
	fileIndex int
}

// copySnapshot is the local data captured under s.mu before hashing.
type copySnapshot struct {
	device    devicecopy.DeviceInfo
	mods      []copyMod
	files     []copyFile
	save      []byte
	saveNames [4]string
	player    *devicecopy.Player
	presets   []devicecopy.Preset
	savs      []devicecopy.ModSave
}

type copyMod struct {
	mod   mods.Mod
	files []copyFile
}

// copyDeviceInfo names this installation the way the reference client does.
func copyDeviceInfo() devicecopy.DeviceInfo {
	name := "This computer"
	if host, err := os.Hostname(); err == nil {
		name = strings.TrimSpace(host)
	}
	if name == "" {
		name = "This computer"
	}
	kind := "linux"
	if runtime.GOOS == "windows" {
		kind = "windows"
	}
	return devicecopy.DeviceInfo{Name: devicecopy.SanitizeControl(name, 64), Kind: kind}
}

// StartCopy begins a copy/v1 session. The receiver offers; the sender answers.
func (s *Service) StartCopy(role devicecopy.Role, code string) (devicecopy.State, error) {
	if role != devicecopy.RoleSend && role != devicecopy.RoleGet {
		return devicecopy.State{}, fmt.Errorf("%w: role must be send or get", ErrCopyInput)
	}
	if strings.TrimSpace(code) == "" {
		if role != devicecopy.RoleSend {
			return devicecopy.State{}, fmt.Errorf("%w: type the code your other device shows", ErrCopyInput)
		}
	} else if _, err := session.Normalize(code); err != nil {
		return devicecopy.State{}, fmt.Errorf("%w: %v", ErrCopyInput, err)
	}

	s.sessMu.Lock()
	plying := s.sess != nil
	s.sessMu.Unlock()
	if plying {
		return devicecopy.State{}, ErrSessionBusy
	}

	s.startMu.Lock()
	defer s.startMu.Unlock()

	s.copyMu.Lock()
	old := s.copyEng
	s.copyEng = nil
	s.copyMu.Unlock()
	if old != nil {
		old.Stop()
		// Wait for an in-flight apply from the old copy. Stopping the
		// engine does not interrupt file renames; the new copy must not
		// race with them.
		s.copyApplyMu.Lock()
		s.copyApplyMu.Unlock()
	}

	s.mu.Lock()
	running := s.running()
	stagingRoot := filepath.Join(s.gameData, ".coop-copy")
	s.mu.Unlock()
	if running {
		return devicecopy.State{}, ErrGameRunning
	}
	if err := os.MkdirAll(stagingRoot, 0o755); err != nil {
		return devicecopy.State{}, err
	}

	device := copyDeviceInfo()
	cfg := devicecopy.Config{
		Role:            role,
		Code:            code,
		Device:          device,
		RelayURL:        strings.TrimSpace(os.Getenv("COOP_RELAY_URL")),
		DisableTrackers: os.Getenv("COOP_NO_TRACKERS") == "1",
		StagingRoot:     stagingRoot,
		Logf: func(format string, args ...any) {
			// The engine keeps its own UI log; the process log is useful when
			// running the CLI.
			fmt.Printf("[copy] "+format+"\n", args...)
		},
		BuildManifest: func() (*devicecopy.Source, error) {
			return s.buildCopySource(device)
		},
		Review: func(manifest *devicecopy.Manifest, route string) (devicecopy.Review, error) {
			return s.reviewCopy(manifest, route)
		},
		Plan: func(manifest *devicecopy.Manifest, choice devicecopy.Choice, route string) (*devicecopy.Plan, error) {
			return s.planCopy(manifest, choice, route)
		},
		Apply: func(manifest *devicecopy.Manifest, plan *devicecopy.Plan, staging string) (string, error) {
			return s.applyCopy(manifest, plan, staging)
		},
	}
	if os.Getenv("COOP_RELAY_ONLY") == "1" {
		cfg.RelayOnly = true
	}

	eng := devicecopy.New(cfg)
	state, err := eng.Start(nil)
	if err != nil {
		eng.Stop()
		return devicecopy.State{}, err
	}
	s.copyMu.Lock()
	s.copyEng = eng
	s.copyMu.Unlock()
	return state, nil
}

// StopCopy ends any running copy.
func (s *Service) StopCopy() {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	s.copyMu.Lock()
	eng := s.copyEng
	s.copyEng = nil
	s.copyMu.Unlock()
	if eng != nil {
		eng.Stop()
	}
	// Let an apply that was already running finish before another start.
	s.copyApplyMu.Lock()
	s.copyApplyMu.Unlock()
}

// CopyState returns the current copy, or ok=false when idle.
func (s *Service) CopyState() (devicecopy.State, bool) {
	s.copyMu.Lock()
	eng := s.copyEng
	s.copyMu.Unlock()
	if eng == nil {
		return devicecopy.State{}, false
	}
	return eng.State(), true
}

// CopyAccept tells the receiver which sections to apply.
func (s *Service) CopyAccept(choice devicecopy.Choice) error {
	s.copyMu.Lock()
	eng := s.copyEng
	s.copyMu.Unlock()
	if eng == nil {
		return ErrNoCopy
	}
	return eng.Accept(choice)
}

// CopyLogs returns the recent copy log, newest last.
func (s *Service) CopyLogs() []string {
	s.copyMu.Lock()
	eng := s.copyEng
	s.copyMu.Unlock()
	if eng == nil {
		return []string{}
	}
	return eng.Logs()
}

// --- manifest building -----------------------------------------------------

func (s *Service) buildCopySource(device devicecopy.DeviceInfo) (*devicecopy.Source, error) {
	snap, err := s.copySnapshotData(device)
	if err != nil {
		return nil, err
	}
	if err := hashSnapshot(&snap); err != nil {
		return nil, err
	}

	manifest := devicecopy.Manifest{
		V:         devicecopy.ProtocolVersion,
		Device:    snap.device,
		Player:    snap.player,
		SaveNames: make([]*string, 4),
		Presets:   snap.presets,
		Savs:      snap.savs,
	}
	for i, name := range snap.saveNames {
		name = devicecopy.SanitizeControl(name, 32)
		if name != "" {
			value := name
			manifest.SaveNames[i] = &value
		}
	}
	if len(snap.save) == eeprom.FileSize {
		encoded := base64.StdEncoding.EncodeToString(snap.save)
		manifest.Save = &encoded
	}
	for _, item := range snap.mods {
		entry := devicecopy.Mod{
			ID:       item.mod.ID,
			Type:     string(item.mod.Kind),
			Location: string(item.mod.Origin),
			Name:     item.mod.Name,
			Enabled:  item.mod.Enabled,
			IsFolder: item.mod.IsFolder,
		}
		if entry.Name == "" {
			entry.Name = entry.ID
		}
		entry.Name = devicecopy.SanitizeControl(devicecopy.StripColors(entry.Name), 300)
		if entry.Name == "" {
			entry.Name = entry.ID
		}
		if item.mod.Origin == mods.OriginUser {
			if rec, ok := s.records.Get(item.mod.ID); ok && rec.SiteID >= 1 && strings.HasPrefix(strings.ToLower(rec.SiteURL), "https://") && rec.Title != "" {
				entry.Record = &devicecopy.Record{
					SiteID:      rec.SiteID,
					SiteURL:     rec.SiteURL,
					Title:       devicecopy.SanitizeControl(rec.Title, 300),
					Version:     devicecopy.SanitizeControl(rec.Version, 100),
					InstalledAt: rec.InstalledAt.UTC().Format(time.RFC3339),
				}
			}
		}
		for _, f := range item.files {
			entry.Files = append(entry.Files, devicecopy.File{
				Rel:    f.rel,
				Size:   f.size,
				SHA256: f.hash,
				Code:   f.code,
			})
		}
		manifest.Mods = append(manifest.Mods, entry)
	}

	text, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if len(text) > devicecopy.ManifestMaxBytes {
		return nil, devicecopy.ErrManifestBig
	}

	files := snap.files
	source := &devicecopy.Source{
		Text:  string(text),
		Files: make([]devicecopy.FileRef, len(files)),
		Read: func(index int, offset int64, length int) ([]byte, error) {
			if index < 0 || index >= len(files) {
				return nil, devicecopy.ErrFileChanged
			}
			file := files[index]
			info, err := os.Stat(file.path)
			if err != nil || info.Size() != file.size || !info.Mode().IsRegular() {
				return nil, devicecopy.ErrFileChanged
			}
			f, err := os.Open(file.path)
			if err != nil {
				return nil, devicecopy.ErrFileChanged
			}
			defer f.Close()
			buf := make([]byte, length)
			n, err := f.ReadAt(buf, offset)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, devicecopy.ErrFileChanged
			}
			if n != length {
				return nil, devicecopy.ErrFileChanged
			}
			return buf, nil
		},
	}
	for i, f := range files {
		source.Files[i] = devicecopy.FileRef{Size: f.size}
	}
	return source, nil
}

func (s *Service) copySnapshotData(device devicecopy.DeviceInfo) (copySnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running() {
		return copySnapshot{}, ErrGameRunning
	}
	snap := copySnapshot{device: device}
	doc, err := s.readConfigLocked()
	if err != nil {
		return copySnapshot{}, err
	}
	enabledMods := map[string]bool{}
	for _, id := range doc.Mods() {
		enabledMods[id] = true
	}
	enabledPacks := doc.Packs()
	list := mods.List(s.pathsLocked(), enabledMods, enabledPacks)
	snap.saveNames = doc.SaveNames()

	if data, err := os.ReadFile(s.savePath()); err == nil && len(data) == eeprom.FileSize {
		snap.save = data
	}

	if player, ok := playerFromConfig(doc); ok {
		snap.player = &player
	}

	snap.presets = readPresets(s.gameData, s.loaderDir)
	snap.savs = readModSaves(s.gameData)

	fileCount := 0
	var totalBytes int64
	for _, mod := range list {
		if !devicecopy.SafeID(mod.ID) {
			continue
		}
		item := copyMod{mod: mod}
		if mod.Origin == mods.OriginUser {
			files, err := walkModFiles(mod, &fileCount, &totalBytes)
			if err != nil {
				return copySnapshot{}, err
			}
			item.files = files
			if len(files) == 0 {
				continue
			}
		}
		for i := range item.files {
			item.files[i].modIndex = len(snap.mods)
			item.files[i].fileIndex = i
			snap.files = append(snap.files, item.files[i])
		}
		snap.mods = append(snap.mods, item)
	}
	return snap, nil
}

func walkModFiles(mod mods.Mod, fileCount *int, totalBytes *int64) ([]copyFile, error) {
	if !mod.IsFolder {
		info, err := os.Stat(mod.Path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, nil
		}
		if info.Size() > devicecopy.MaxFileSize {
			return nil, fmt.Errorf("%s is too big to copy", mod.Name)
		}
		*fileCount = *fileCount + 1
		*totalBytes += info.Size()
		if *fileCount > devicecopy.MaxFiles || *totalBytes > devicecopy.MaxTotal {
			return nil, errors.New("too much to copy")
		}
		return []copyFile{{path: mod.Path, rel: mod.ID, size: info.Size()}}, nil
	}

	var files []copyFile
	root := mod.Path
	// A user mod may be a symlink (the Create tab uses them). Walk the
	// resolved root, but still skip symlinks inside it.
	if info, err := os.Lstat(root); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !devicecopy.SafeComponent(name) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if strings.Count(rel, "/")+1 > 16 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !devicecopy.SafeRel(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() > devicecopy.MaxFileSize {
			return fmt.Errorf("%s is too big to copy", rel)
		}
		*fileCount = *fileCount + 1
		*totalBytes += info.Size()
		if *fileCount > devicecopy.MaxFiles || *totalBytes > devicecopy.MaxTotal {
			return errors.New("too much to copy")
		}
		files = append(files, copyFile{path: path, rel: rel, size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i].rel) < strings.ToLower(files[j].rel)
	})
	return files, nil
}

func hashSnapshot(snap *copySnapshot) error {
	for i := range snap.files {
		file := &snap.files[i]
		before, err := os.Stat(file.path)
		if err != nil || !before.Mode().IsRegular() || before.Size() != file.size {
			return devicecopy.ErrFileChanged
		}
		hash, code, err := hashFile(file.path)
		if err != nil {
			return err
		}
		after, err := os.Stat(file.path)
		if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
			return devicecopy.ErrFileChanged
		}
		file.hash = hash
		file.code = code
	}
	// Keep the per-mod slices in sync with the flat list.
	index := 0
	for mi := range snap.mods {
		for fi := range snap.mods[mi].files {
			if index < len(snap.files) {
				snap.mods[mi].files[fi] = snap.files[index]
			}
			index++
		}
	}
	return nil
}

func hashFile(path string) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	h := sha256.New()
	first := make([]byte, 1)
	n, _ := f.Read(first)
	if n > 0 {
		h.Write(first[:n])
	}
	if _, err := io.Copy(h, f); err != nil {
		return "", false, err
	}
	code := strings.HasSuffix(strings.ToLower(path), ".lua") && n > 0 && first[0] == 0x1b
	return hex.EncodeToString(h.Sum(nil)), code, nil
}

func playerFromConfig(doc *conf.Doc) (devicecopy.Player, bool) {
	if doc == nil {
		return devicecopy.Player{}, false
	}
	if doc.Value("coop_player_name") == "" && doc.Value("coop_player_model") == "" &&
		doc.Value("coop_player_palette_shirt") == "" {
		return devicecopy.Player{}, false
	}
	name := strings.TrimSpace(doc.Value("coop_player_name"))
	if name == "" {
		name = "Player"
	}
	name = strings.Split(name, " ")[0]
	name = devicecopy.SanitizeControl(name, 20)
	if name == "" {
		name = "Player"
	}
	model, _ := strconv.Atoi(doc.Value("coop_player_model"))
	if model < 0 || model > 4 {
		model = 0
	}
	colors := map[string]string{}
	for _, key := range devicecopy.PaletteKeys {
		colors[key] = parsePaletteColor(doc.Value("coop_player_palette_" + key))
	}
	return devicecopy.Player{Name: name, Model: model, Colors: colors}, true
}

func parsePaletteColor(value string) string {
	parts := strings.Fields(strings.TrimSpace(value))
	if len(parts) != 3 {
		return "#808080"
	}
	out := "#"
	for _, p := range parts {
		if len(p) > 2 {
			return "#808080"
		}
		n, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return "#808080"
		}
		out += fmt.Sprintf("%02x", n)
	}
	return out
}

func paletteColorValue(color string) string {
	color = strings.ToLower(strings.TrimSpace(color))
	if len(color) != 7 || color[0] != '#' {
		return "80 80 80"
	}
	return color[1:3] + " " + color[3:5] + " " + color[5:7]
}

func readPresets(gameData, loaderDir string) []devicecopy.Preset {
	// The game reads user palettes from <data>/palettes. The loader's JSON
	// only remembers which character each preset belongs to.
	models := map[string]int{}
	if data, err := os.ReadFile(filepath.Join(loaderDir, "player-presets.json")); err == nil {
		var doc struct {
			Models map[string]int `json:"models"`
		}
		if json.Unmarshal(data, &doc) == nil {
			models = doc.Models
		}
	}
	dir := filepath.Join(gameData, "palettes")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []devicecopy.Preset
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if !strings.EqualFold(ext, ".ini") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ext)
		if name == "" || devicecopy.SanitizePresetName(name) != name {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		colors := parsePaletteINI(string(data))
		preset := devicecopy.Preset{Name: name, Colors: colors}
		if model, ok := models[name]; ok && model >= 0 && model <= 4 {
			value := model
			preset.Model = &value
		}
		out = append(out, preset)
		if len(out) >= devicecopy.MaxPresets {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func parsePaletteINI(text string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "[") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.ToUpper(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	out := map[string]string{}
	for _, key := range devicecopy.PaletteKeys {
		upper := strings.ToUpper(key)
		r := parseByte(values[upper+"_R"])
		g := parseByte(values[upper+"_G"])
		b := parseByte(values[upper+"_B"])
		out[key] = fmt.Sprintf("#%02x%02x%02x", r, g, b)
	}
	return out
}

func parseByte(value string) byte {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0x80
	}
	n, err := strconv.ParseUint(value, 0, 8)
	if err != nil {
		return 0x80
	}
	return byte(n)
}

func readModSaves(gameData string) []devicecopy.ModSave {
	dir := filepath.Join(gameData, "sav")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []devicecopy.ModSave
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.EqualFold(filepath.Ext(entry.Name()), ".sav") {
			continue
		}
		name := entry.Name()
		if !devicecopy.SafeComponent(name) || len(name) > 200 {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > devicecopy.MaxModSave {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || strings.IndexByte(string(data), 0) >= 0 {
			continue
		}
		out = append(out, devicecopy.ModSave{Name: name, Text: devicecopy.Latin1Encode(data)})
		if len(out) >= devicecopy.MaxModSaves {
			break
		}
	}
	return out
}

// --- review and plan -------------------------------------------------------

func (s *Service) reviewCopy(manifest *devicecopy.Manifest, route string) (devicecopy.Review, error) {
	local, err := s.localCopyState()
	if err != nil {
		return devicecopy.Review{}, err
	}
	plan := devicecopy.BuildPlan(manifest, local, manifest.SaveStars(), devicecopy.Choice{Mods: true, Look: true, Saves: true}, route)
	return plan.Review, nil
}

func (s *Service) planCopy(manifest *devicecopy.Manifest, choice devicecopy.Choice, route string) (*devicecopy.Plan, error) {
	local, err := s.localCopyState()
	if err != nil {
		return nil, err
	}
	return devicecopy.BuildPlan(manifest, local, manifest.SaveStars(), choice, route), nil
}

func (s *Service) localCopyState() (devicecopy.LocalState, error) {
	s.mu.Lock()
	if s.running() {
		return devicecopy.LocalState{}, ErrGameRunning
	}
	doc, err := s.readConfigLocked()
	if err != nil {
		return devicecopy.LocalState{}, err
	}
	enabledMods := map[string]bool{}
	for _, id := range doc.Mods() {
		enabledMods[id] = true
	}
	enabledPacks := doc.Packs()
	list := mods.List(s.pathsLocked(), enabledMods, enabledPacks)
	savePath := s.savePath()
	s.mu.Unlock()

	state := devicecopy.LocalState{}
	for _, mod := range list {
		entry := devicecopy.LocalMod{
			ID:       mod.ID,
			Type:     string(mod.Kind),
			Location: string(mod.Origin),
			Name:     mod.Name,
			Enabled:  mod.Enabled,
		}
		if mod.Origin == mods.OriginUser {
			fileCount := 0
			var total int64
			files, err := walkModFiles(mod, &fileCount, &total)
			if err != nil {
				return devicecopy.LocalState{}, err
			}
			for _, f := range files {
				hash, _, err := hashFile(f.path)
				if err != nil {
					return devicecopy.LocalState{}, err
				}
				entry.Files = append(entry.Files, devicecopy.LocalFile{Rel: f.rel, Size: f.size, SHA256: hash})
			}
		}
		state.Mods = append(state.Mods, entry)
	}

	if data, err := os.ReadFile(savePath); err == nil && len(data) == eeprom.FileSize {
		state.SaveStars = devicecopy.StarsFromSave(data)
	}
	return state, nil
}

// --- applying --------------------------------------------------------------

func (s *Service) applyCopy(manifest *devicecopy.Manifest, plan *devicecopy.Plan, staging string) (string, error) {
	s.copyApplyMu.Lock()
	defer s.copyApplyMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running() {
		return "", ErrGameRunning
	}
	sessActive := false
	s.sessMu.Lock()
	sessActive = s.sess != nil
	s.sessMu.Unlock()
	if sessActive {
		return "", ErrSessionBusy
	}

	doc, err := s.readConfigLocked()
	if err != nil {
		return "", err
	}
	backupRoot := filepath.Join(s.gameData, "removed-by-coop-loader", "copy-"+time.Now().UTC().Format("20060102-150405"))
	for i := 2; fsutil.Exists(backupRoot); i++ {
		backupRoot = filepath.Join(s.gameData, "removed-by-coop-loader", fmt.Sprintf("copy-%s-%d", time.Now().UTC().Format("20060102-150405"), i))
	}

	var skipped []string
	if plan.Choice.Mods {
		flat := manifest.Flatten()
		for _, modIndex := range plan.Mods {
			sm := manifest.Mods[modIndex]
			if sm.Location != "user" {
				continue
			}
			base := s.pathsLocked().UserMods()
			if sm.Type == "pack" {
				base = s.pathsLocked().UserPacks()
			}
			dest := filepath.Join(base, sm.ID)
			if info, err := os.Lstat(dest); err == nil && info.Mode()&os.ModeSymlink != 0 {
				skipped = append(skipped, sm.Name)
				continue
			}
			build := filepath.Join(staging, "built", sm.Type, sm.ID)
			if sm.IsFolder {
				if err := os.MkdirAll(build, 0o755); err != nil {
					return "", err
				}
			} else if err := os.MkdirAll(filepath.Dir(build), 0o755); err != nil {
				return "", err
			}
			for flatIndex, f := range flat {
				if f.Mod != modIndex {
					continue
				}
				part := filepath.Join(staging, "in", strconv.Itoa(flatIndex))
				target := build
				if sm.IsFolder {
					target = filepath.Join(build, filepath.FromSlash(f.Ref.Rel))
					if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
						return "", err
					}
				}
				if err := os.Rename(part, target); err != nil {
					return "", err
				}
			}
			relBackup := filepath.Join(sm.Type, sm.ID)
			if err := moveToBackup(dest, backupRoot, relBackup); err != nil {
				return "", err
			}
			if err := os.MkdirAll(base, 0o755); err != nil {
				return "", err
			}
			if err := os.Rename(build, dest); err != nil {
				return "", err
			}
			if sm.Record != nil {
				when, _ := time.Parse(time.RFC3339, sm.Record.InstalledAt)
				if when.IsZero() {
					when = time.Now()
				}
				if err := s.records.Set([]string{sm.ID}, installed.Record{
					SiteID:      sm.Record.SiteID,
					SiteURL:     sm.Record.SiteURL,
					Title:       sm.Record.Title,
					Version:     sm.Record.Version,
					InstalledAt: when,
				}); err != nil {
					return "", err
				}
			} else if err := s.records.Delete(sm.ID); err != nil {
				return "", err
			}
		}
		doc.SetMods(plan.EnableMods)
		doc.SetPacks(boolSet(plan.EnablePacks))
	}

	if plan.Choice.Look {
		if manifest.Player != nil {
			doc.SetValue("coop_player_name", devicecopy.SanitizeControl(strings.Split(manifest.Player.Name, " ")[0], 20))
			doc.SetValue("coop_player_model", strconv.Itoa(manifest.Player.Model))
			for _, key := range devicecopy.PaletteKeys {
				doc.SetValue("coop_player_palette_"+key, paletteColorValue(manifest.Player.Colors[key]))
			}
		}
		for _, preset := range manifest.Presets {
			name := devicecopy.SanitizePresetName(preset.Name)
			if name == "" {
				continue
			}
			target := filepath.Join(s.pathsLocked().Palettes(), name+".ini")
			if err := moveToBackup(target, backupRoot, filepath.Join("palettes", name+".ini")); err != nil {
				return "", err
			}
			if err := fsutil.WriteFileAtomic(target, []byte(paletteINI(preset.Colors)), 0o644); err != nil {
				return "", err
			}
			if err := writePresetModel(s.loaderDir, name, preset.Model); err != nil {
				return "", err
			}
		}
	}

	if plan.Choice.Saves {
		for i, name := range manifest.SaveNames {
			if name == nil {
				doc.SetSaveName(i, "")
			} else {
				doc.SetSaveName(i, *name)
			}
		}
		if manifest.Save != nil {
			data, err := base64.StdEncoding.DecodeString(*manifest.Save)
			if err != nil || len(data) != eeprom.FileSize {
				return "", errors.New("bad save file")
			}
			if err := moveToBackup(s.savePath(), backupRoot, "sm64_save_file.bin"); err != nil {
				return "", err
			}
			if err := fsutil.WriteFileAtomic(s.savePath(), data, 0o644); err != nil {
				return "", err
			}
		}
		for _, save := range manifest.Savs {
			name := save.Name
			if !devicecopy.SafeComponent(name) || !strings.EqualFold(filepath.Ext(name), ".sav") {
				continue
			}
			data, err := devicecopy.Latin1Decode(save.Text)
			if err != nil {
				return "", err
			}
			target := filepath.Join(s.gameData, "sav", name)
			if err := moveToBackup(target, backupRoot, filepath.Join("sav", name)); err != nil {
				return "", err
			}
			if err := fsutil.WriteFileAtomic(target, data, 0o644); err != nil {
				return "", err
			}
		}
	}

	if err := s.writeConfigLocked(doc); err != nil {
		return "", err
	}
	if len(skipped) > 0 {
		return "Left as they are (projects from Create on this computer): " + strings.Join(skipped, ", ") + ".", nil
	}
	return "", nil
}

func moveToBackup(src, root, rel string) error {
	if !fsutil.Exists(src) {
		return nil
	}
	dst := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	for i := 2; fsutil.Exists(dst); i++ {
		dst = filepath.Join(root, filepath.FromSlash(rel)+"-"+strconv.Itoa(i))
	}
	return os.Rename(src, dst)
}

func boolSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}

func paletteINI(colors map[string]string) string {
	// The game writes these keys in this order; keeping it makes the copied
	// file look exactly like one saved by the game's palette menu.
	order := []string{"pants", "shirt", "gloves", "shoes", "hair", "skin", "cap", "emblem"}
	var b strings.Builder
	b.WriteString("[PALETTE]\n")
	for _, key := range order {
		value := colors[key]
		if len(value) != 7 {
			value = "#808080"
		}
		r, _ := strconv.ParseUint(value[1:3], 16, 8)
		g, _ := strconv.ParseUint(value[3:5], 16, 8)
		bl, _ := strconv.ParseUint(value[5:7], 16, 8)
		upper := strings.ToUpper(key)
		fmt.Fprintf(&b, "%s_R = %d\n%s_G = %d\n%s_B = %d\n", upper, r, upper, g, upper, bl)
	}
	return b.String()
}

func writePresetModel(loaderDir, name string, model *int) error {
	path := filepath.Join(loaderDir, "player-presets.json")
	doc := struct {
		Models map[string]int `json:"models"`
	}{Models: map[string]int{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &doc)
		if doc.Models == nil {
			doc.Models = map[string]int{}
		}
	}
	if model == nil {
		delete(doc.Models, name)
	} else {
		doc.Models[name] = *model
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsutil.WriteFileAtomic(path, data, 0o644)
}
