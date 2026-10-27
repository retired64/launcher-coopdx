package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/retired64/coop-loader/internal/conf"
	"github.com/retired64/coop-loader/internal/devicecopy"
	"github.com/retired64/coop-loader/internal/fsutil"
)

// PlayerModels is the order of the game's built-in characters.
var PlayerModels = []string{"Mario", "Luigi", "Toad", "Waluigi", "Wario"}

// PlayerLook is the name, character and eight colours the game stores.
type PlayerLook struct {
	Name   string            `json:"name"`
	Model  int               `json:"model"`
	Colors map[string]string `json:"colors"`
}

// PlayerPreset is a palette file from the game or from the player.
type PlayerPreset struct {
	Name    string            `json:"name"`
	Model   *int              `json:"model"`
	Colors  map[string]string `json:"colors"`
	BuiltIn bool              `json:"builtIn"`
}

// PlayerState is what the Jugador tab shows.
type PlayerState struct {
	Look    PlayerLook     `json:"look"`
	Presets []PlayerPreset `json:"presets"`
	Models  []string       `json:"models"`
}

var defaultLook = map[int]map[string]string{
	0: {"shirt": "#ff0000", "pants": "#0000ff", "cap": "#ff0000", "emblem": "#ff0000", "gloves": "#ffffff", "shoes": "#721c0e", "skin": "#fec179", "hair": "#730600"},
	1: {"shirt": "#00ff00", "pants": "#0000ff", "cap": "#00ff00", "emblem": "#00ff00", "gloves": "#ffffff", "shoes": "#721c0e", "skin": "#fec179", "hair": "#730600"},
	2: {"shirt": "#4c2cd3", "pants": "#ffffff", "cap": "#ff0000", "emblem": "#ff0000", "gloves": "#ffffff", "shoes": "#68401b", "skin": "#fed5a1", "hair": "#730600"},
	3: {"shirt": "#6126b0", "pants": "#161627", "cap": "#6126b0", "emblem": "#ffde00", "gloves": "#ffffff", "shoes": "#fe7600", "skin": "#fec179", "hair": "#735300"},
	4: {"shirt": "#ffbd00", "pants": "#7f207a", "cap": "#ffbd00", "emblem": "#008000", "gloves": "#ffffff", "shoes": "#0e721c", "skin": "#fec179", "hair": "#735300"},
}

// Player returns the current look and every preset.
func (s *Service) Player() (PlayerState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.readConfigLocked()
	if err != nil {
		return PlayerState{}, err
	}
	look := playerLook(doc)
	presets := s.playerPresetsLocked()
	return PlayerState{Look: look, Presets: presets, Models: append([]string(nil), PlayerModels...)}, nil
}

func playerLook(doc *conf.Doc) PlayerLook {
	name := strings.TrimSpace(doc.Value("coop_player_name"))
	if name == "" {
		name = "Player"
	}
	name = devicecopy.SanitizeControl(name, 20)
	if name == "" {
		name = "Player"
	}
	model, _ := strconv.Atoi(doc.Value("coop_player_model"))
	if model < 0 || model >= len(PlayerModels) {
		model = 0
	}
	colors := map[string]string{}
	for _, key := range devicecopy.PaletteKeys {
		value := strings.TrimSpace(doc.Value("coop_player_palette_" + key))
		if value == "" {
			colors[key] = defaultLook[model][key]
			continue
		}
		colors[key] = parsePaletteColor(value)
	}
	return PlayerLook{Name: name, Model: model, Colors: colors}
}

// SetPlayer writes the name, character and colours in one atomic config write.
func (s *Service) SetPlayer(look PlayerLook) (PlayerState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running() {
		return PlayerState{}, ErrGameRunning
	}
	doc, err := s.readConfigLocked()
	if err != nil {
		return PlayerState{}, err
	}
	name := strings.TrimSpace(devicecopy.SanitizeControl(look.Name, 20))
	if name == "" {
		name = "Player"
	}
	model := look.Model
	if model < 0 || model >= len(PlayerModels) {
		model = 0
	}
	colors, err := cleanColors(look.Colors)
	if err != nil {
		return PlayerState{}, err
	}
	doc.SetValue("coop_player_name", name)
	doc.SetValue("coop_player_model", strconv.Itoa(model))
	for _, key := range devicecopy.PaletteKeys {
		doc.SetValue("coop_player_palette_"+key, paletteColorValue(colors[key]))
	}
	if err := s.writeConfigLocked(doc); err != nil {
		return PlayerState{}, err
	}
	presets := s.playerPresetsLocked()
	return PlayerState{Look: PlayerLook{Name: name, Model: model, Colors: colors}, Presets: presets, Models: append([]string(nil), PlayerModels...)}, nil
}

func cleanColors(in map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for _, key := range devicecopy.PaletteKeys {
		value := strings.ToLower(strings.TrimSpace(in[key]))
		if len(value) != 7 || value[0] != '#' {
			return nil, errors.New("that colour is not valid")
		}
		for _, c := range value[1:] {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return nil, errors.New("that colour is not valid")
			}
		}
		out[key] = value
	}
	return out, nil
}

// SavePlayerPreset writes the current look into the game's palettes folder.
func (s *Service) SavePlayerPreset(name string) (PlayerState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running() {
		return PlayerState{}, ErrGameRunning
	}
	doc, err := s.readConfigLocked()
	if err != nil {
		return PlayerState{}, err
	}
	clean := devicecopy.SanitizePresetName(name)
	if clean == "" || clean != strings.TrimSpace(name) {
		return PlayerState{}, errors.New("give the preset a name")
	}
	look := playerLook(doc)
	target := filepath.Join(s.gameData, "palettes", clean+".ini")
	if fsutil.Exists(target) {
		s.backupPresetLocked(target)
	}
	if err := fsutil.WriteFileAtomic(target, []byte(paletteINI(look.Colors)), 0o644); err != nil {
		return PlayerState{}, err
	}
	if err := s.setPresetModel(clean, look.Model); err != nil {
		return PlayerState{}, err
	}
	return PlayerState{Look: look, Presets: s.playerPresetsLocked(), Models: append([]string(nil), PlayerModels...)}, nil
}

// DeletePlayerPreset moves a user preset aside; it never deletes it.
func (s *Service) DeletePlayerPreset(name string) (PlayerState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running() {
		return PlayerState{}, ErrGameRunning
	}
	clean := devicecopy.SanitizePresetName(name)
	if clean == "" || clean != strings.TrimSpace(name) {
		return PlayerState{}, errors.New("that preset can't be removed")
	}
	target := filepath.Join(s.gameData, "palettes", clean+".ini")
	if fsutil.Exists(target) {
		s.backupPresetLocked(target)
	}
	if err := s.setPresetModel(clean, -1); err != nil {
		return PlayerState{}, err
	}
	doc, err := s.readConfigLocked()
	if err != nil {
		return PlayerState{}, err
	}
	return PlayerState{Look: playerLook(doc), Presets: s.playerPresetsLocked(), Models: append([]string(nil), PlayerModels...)}, nil
}

func (s *Service) backupPresetLocked(path string) {
	root := filepath.Join(s.gameData, "removed-by-coop-loader")
	dir := filepath.Join(root, "presets-"+time.Now().UTC().Format("20060102-150405"))
	_ = os.MkdirAll(dir, 0o755)
	dst := filepath.Join(dir, filepath.Base(path))
	for i := 2; fsutil.Exists(dst); i++ {
		dst = filepath.Join(dir, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))+"-"+strconv.Itoa(i)+filepath.Ext(path))
	}
	_ = os.Rename(path, dst)
}

func (s *Service) playerPresetsLocked() []PlayerPreset {
	models := s.presetModels()
	out := []PlayerPreset{}
	seen := map[string]bool{}
	addDir := func(dir string, builtIn bool) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.EqualFold(filepath.Ext(entry.Name()), ".ini") {
				continue
			}
			name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
			if name == "" || devicecopy.SanitizePresetName(name) != name {
				continue
			}
			key := strings.ToLower(name)
			if seen[key] {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				continue
			}
			colors := parsePaletteINI(string(data))
			preset := PlayerPreset{Name: name, Colors: colors, BuiltIn: builtIn}
			if model, ok := models[name]; ok && model >= 0 && model < len(PlayerModels) {
				value := model
				preset.Model = &value
			} else if builtIn {
				if index := modelIndex(name); index >= 0 {
					value := index
					preset.Model = &value
				}
			}
			out = append(out, preset)
			seen[key] = true
		}
	}
	if gamePath := s.gamePathLocked(); gamePath != "" {
		addDir(filepath.Join(gamePath, "palettes"), true)
	}
	addDir(filepath.Join(s.gameData, "palettes"), false)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].BuiltIn != out[j].BuiltIn {
			return out[i].BuiltIn
		}
		if out[i].BuiltIn {
			mi, mj := -1, -1
			if out[i].Model != nil {
				mi = *out[i].Model
			}
			if out[j].Model != nil {
				mj = *out[j].Model
			}
			if mi != mj {
				return mi < mj
			}
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func modelIndex(name string) int {
	for i, model := range PlayerModels {
		if strings.EqualFold(model, name) {
			return i
		}
	}
	return -1
}

type presetModelsFile struct {
	Models map[string]int `json:"models"`
}

func (s *Service) presetModels() map[string]int {
	path := filepath.Join(s.loaderDir, "player-presets.json")
	var doc presetModelsFile
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &doc)
	}
	if doc.Models == nil {
		doc.Models = map[string]int{}
	}
	return doc.Models
}

func (s *Service) setPresetModel(name string, model int) error {
	path := filepath.Join(s.loaderDir, "player-presets.json")
	models := s.presetModels()
	if model < 0 {
		delete(models, name)
	} else {
		models[name] = model
	}
	data, err := json.MarshalIndent(presetModelsFile{Models: models}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsutil.WriteFileAtomic(path, data, 0o644)
}
