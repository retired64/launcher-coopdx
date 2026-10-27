package devicecopy

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/bits"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Manifest is the JSON document the sender announces. Its field order and
// names match the reference client's manifest.
type Manifest struct {
	V         int        `json:"v"`
	Device    DeviceInfo `json:"device"`
	Player    *Player    `json:"player"`
	SaveNames []*string  `json:"saveNames"`
	Presets   []Preset   `json:"presets"`
	Save      *string    `json:"save"`
	Savs      []ModSave  `json:"savs"`
	Mods      []Mod      `json:"mods"`
}

// Player is the sender's current look.
type Player struct {
	Name   string            `json:"name"`
	Model  int               `json:"model"`
	Colors map[string]string `json:"colors"`
}

// Preset is one user palette preset.
type Preset struct {
	Name   string            `json:"name"`
	Colors map[string]string `json:"colors"`
	Model  *int              `json:"model"`
}

// ModSave is one .sav file from the game's sav folder, read as Latin-1.
type ModSave struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// Mod is one installed mod or DynOS pack. Built-in entries have no files and
// only carry their enabled state.
type Mod struct {
	ID       string  `json:"id"`
	Type     string  `json:"type"`
	Location string  `json:"location"`
	Name     string  `json:"name"`
	Enabled  bool    `json:"enabled"`
	IsFolder bool    `json:"isFolder"`
	Record   *Record `json:"record"`
	Files    []File  `json:"files"`
}

// File is one file inside a mod.
type File struct {
	Rel    string `json:"rel"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Code   bool   `json:"code,omitempty"`
}

// Record is the optional site provenance.
type Record struct {
	SiteID      int    `json:"siteId"`
	SiteURL     string `json:"siteUrl"`
	Title       string `json:"title"`
	Version     string `json:"version"`
	InstalledAt string `json:"installedAt"`
}

// Validation errors with user-facing meaning.
var (
	ErrNewer   = errors.New("newer")
	ErrOlder   = errors.New("older")
	ErrDamaged = errors.New("The list of what to copy is damaged.")
)

var (
	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hexColor  = regexp.MustCompile(`^#[0-9a-f]{6}$`)
)

// PaletteKeys are the eight colours the game stores for one Mario palette.
var PaletteKeys = []string{"shirt", "pants", "cap", "emblem", "gloves", "shoes", "skin", "hair"}

// ParseManifest validates and decodes a manifest.
func ParseManifest(text string) (*Manifest, error) {
	if len(text) > ManifestMaxBytes {
		return nil, ErrManifestBig
	}
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(text))
	if err := dec.Decode(&m); err != nil {
		return nil, ErrDamaged
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate applies the same limits as the reference client.
func (m *Manifest) Validate() error {
	if m.V != ProtocolVersion {
		if m.V > ProtocolVersion {
			return ErrNewer
		}
		return ErrOlder
	}
	if err := validateDevice(&m.Device); err != nil {
		return err
	}
	if m.Player != nil {
		if err := validatePlayer(m.Player); err != nil {
			return err
		}
	}
	if m.SaveNames == nil {
		m.SaveNames = []*string{}
	}
	if len(m.SaveNames) > 4 {
		return errors.New("bad save names")
	}
	for _, name := range m.SaveNames {
		if name != nil {
			if !plainString(*name, 32) {
				return errors.New("bad save name")
			}
		}
	}
	for len(m.SaveNames) < 4 {
		m.SaveNames = append(m.SaveNames, nil)
	}
	if len(m.Presets) > MaxPresets {
		return errors.New("bad presets")
	}
	for i := range m.Presets {
		if err := validatePreset(&m.Presets[i]); err != nil {
			return err
		}
	}
	if m.Save != nil {
		data, err := base64.StdEncoding.DecodeString(*m.Save)
		if err != nil || len(*m.Save) != 684 || len(data) != 512 {
			return errors.New("bad save file")
		}
	}
	if len(m.Savs) > MaxModSaves {
		return errors.New("bad mod saves")
	}
	for _, save := range m.Savs {
		if !plainString(save.Name, 200) || !strings.HasSuffix(strings.ToLower(save.Name), ".sav") || !SafeComponent(save.Name) {
			return errors.New("bad mod save name")
		}
		data, err := Latin1Decode(save.Text)
		if err != nil || len(data) > MaxModSave || strings.IndexByte(string(data), 0) >= 0 {
			return errors.New("bad mod save")
		}
	}
	if len(m.Mods) > MaxMods {
		return errors.New("too much to copy")
	}
	totalFiles := 0
	var totalBytes int64
	seenMods := map[string]bool{}
	for i := range m.Mods {
		mod := &m.Mods[i]
		if err := validateMod(mod); err != nil {
			return err
		}
		key := strings.ToLower(mod.Type) + ":" + strings.ToLower(mod.ID) + ":" + mod.Location
		if seenMods[key] {
			return errors.New("a mod came twice")
		}
		seenMods[key] = true
		totalFiles += len(mod.Files)
		for _, f := range mod.Files {
			totalBytes += f.Size
		}
	}
	if totalFiles > MaxFiles || totalBytes > MaxTotal {
		return errors.New("too much to copy")
	}
	return nil
}

func validateDevice(d *DeviceInfo) error {
	name := strings.TrimSpace(d.Name)
	if name == "" || utf8.RuneCountInString(name) > 64 || hasControl(name) {
		return errors.New("bad device")
	}
	d.Name = name
	switch d.Kind {
	case "windows", "linux", "android":
	default:
		return errors.New("bad device")
	}
	return nil
}

func validatePlayer(p *Player) error {
	if !plainString(p.Name, 20) {
		return errors.New("bad look")
	}
	p.Name = strings.TrimSpace(strings.Split(p.Name, " ")[0])
	if p.Name == "" {
		p.Name = "Player"
	}
	if p.Model < 0 || p.Model > 4 {
		return errors.New("bad character")
	}
	colors, err := validateColors(p.Colors)
	if err != nil {
		return err
	}
	p.Colors = colors
	return nil
}

func validatePreset(p *Preset) error {
	if !plainString(p.Name, 30) || p.Name == "" || SanitizePresetName(p.Name) != p.Name {
		return errors.New("bad preset name")
	}
	colors, err := validateColors(p.Colors)
	if err != nil {
		return err
	}
	p.Colors = colors
	if p.Model != nil && (*p.Model < 0 || *p.Model > 4) {
		return errors.New("bad preset character")
	}
	return nil
}

func validateColors(in map[string]string) (map[string]string, error) {
	if in == nil {
		return nil, errors.New("bad colours")
	}
	out := make(map[string]string, len(PaletteKeys))
	for _, key := range PaletteKeys {
		value, ok := in[key]
		if !ok {
			return nil, errors.New("bad colours")
		}
		value = strings.ToLower(strings.TrimSpace(value))
		if !hexColor.MatchString(value) {
			return nil, errors.New("bad colours")
		}
		out[key] = value
	}
	return out, nil
}

func validateMod(mod *Mod) error {
	if !SafeID(mod.ID) {
		return errors.New("bad mod name")
	}
	if mod.Type != "mod" && mod.Type != "pack" {
		return errors.New("bad mod")
	}
	if mod.Location != "user" && mod.Location != "builtin" {
		return errors.New("bad mod")
	}
	if !plainString(mod.Name, 300) {
		return errors.New("bad mod name")
	}
	mod.Name = StripColors(mod.Name)
	if mod.Name == "" {
		mod.Name = mod.ID
	}
	if mod.Location == "builtin" && len(mod.Files) > 0 {
		return errors.New("a built-in mod came with files")
	}
	if mod.Location == "user" && len(mod.Files) == 0 {
		return errors.New("a mod came without files")
	}
	seen := map[string]bool{}
	for i := range mod.Files {
		f := &mod.Files[i]
		if !SafeRel(f.Rel) {
			return errors.New("bad file path")
		}
		if f.Size < 0 || f.Size > MaxFileSize {
			return errors.New("bad file size")
		}
		if !sha256Hex.MatchString(f.SHA256) {
			return errors.New("bad file hash")
		}
		lower := strings.ToLower(f.Rel)
		if seen[lower] {
			return errors.New("a file came twice")
		}
		seen[lower] = true
	}
	if !mod.IsFolder {
		if mod.Type != "mod" || !isLuaName(mod.ID) {
			return errors.New("bad single-file mod")
		}
		if mod.Location == "user" && (len(mod.Files) != 1 || mod.Files[0].Rel != mod.ID) {
			return errors.New("bad single-file mod")
		}
	}
	if mod.Record != nil {
		if err := validateRecord(mod.Record); err != nil {
			return err
		}
	}
	return nil
}

func validateRecord(r *Record) error {
	if r.SiteID < 1 || r.SiteID > 1_000_000_000 {
		return errors.New("bad install record")
	}
	if !plainString(r.SiteURL, 500) || !strings.HasPrefix(strings.ToLower(r.SiteURL), "https://") {
		return errors.New("bad install record")
	}
	if !plainString(r.Title, 300) || !plainString(r.Version, 100) || !plainString(r.InstalledAt, 40) {
		return errors.New("bad install record")
	}
	return nil
}

func plainString(s string, max int) bool {
	return utf8.RuneCountInString(s) <= max && !hasControl(s)
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func isLuaName(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".lua") || strings.HasSuffix(lower, ".luac")
}

// SanitizePresetName is the reference client's palette-name sanitizer.
func SanitizePresetName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			continue
		}
		switch r {
		case '/', '\\', ':', '<', '>', '"', '|', '?', '*':
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimRight(out, ". ")
	if utf8.RuneCountInString(out) > 30 {
		out = string([]rune(out)[:30])
	}
	return out
}

// Flat is one file in manifest order. The index is the number used on the
// wire.
type Flat struct {
	Mod  int
	File int
	Ref  File
}

// Flatten returns every file in wire order.
func (m *Manifest) Flatten() []Flat {
	var out []Flat
	for mi := range m.Mods {
		for fi := range m.Mods[mi].Files {
			out = append(out, Flat{Mod: mi, File: fi, Ref: m.Mods[mi].Files[fi]})
		}
	}
	return out
}

// Sizes returns the size of every file in wire order.
func (m *Manifest) Sizes() []int64 {
	flat := m.Flatten()
	out := make([]int64, len(flat))
	for i, f := range flat {
		out[i] = f.Ref.Size
	}
	return out
}

// ValidateWant checks the receiver's file index list: in range, strictly
// increasing, and therefore free of duplicates.
func ValidateWant(want []int, sizes []int64) error {
	for i, index := range want {
		if index < 0 || index >= len(sizes) {
			return errors.New("The other device asked for files that aren't here.")
		}
		if i > 0 && index <= want[i-1] {
			return errors.New("The other device asked for files that aren't here.")
		}
	}
	return nil
}

// SaveStars decodes the manifest's 512-byte save and returns the star count
// per slot, or nil for an empty/invalid slot.
func (m *Manifest) SaveStars() [4]*int {
	var out [4]*int
	if m.Save == nil {
		return out
	}
	data, err := base64.StdEncoding.DecodeString(*m.Save)
	if err != nil || len(data) != 512 {
		return out
	}
	return StarsFromSave(data)
}

// StarsFromSave is the reference client's Fa() summary for the copy review.
func StarsFromSave(data []byte) [4]*int {
	var out [4]*int
	if len(data) != 512 {
		return out
	}
	for i := 0; i < 4; i++ {
		stars, ok := slotStars(data[i*112 : (i+1)*112])
		if ok {
			out[i] = &stars
		}
	}
	return out
}

// slotStars mirrors the reference client's Fa(): it validates the signature,
// checksum and existence bit, then counts the set bits of the star bitmap.
func slotStars(slot []byte) (int, bool) {
	if len(slot) != 112 {
		return 0, false
	}
	var orderBig bool
	switch {
	case slot[52] == 'D' && slot[53] == 'A':
		orderBig = true
	case slot[52] == 'A' && slot[53] == 'D':
		orderBig = false
	default:
		return 0, false
	}
	var sum uint16
	for _, b := range slot[:54] {
		sum += uint16(b)
	}
	var checksum uint16
	if orderBig {
		checksum = uint16(slot[54])<<8 | uint16(slot[55])
	} else {
		checksum = uint16(slot[55])<<8 | uint16(slot[54])
	}
	if sum != checksum {
		return 0, false
	}
	var flags uint32
	if orderBig {
		flags = uint32(slot[8])<<24 | uint32(slot[9])<<16 | uint32(slot[10])<<8 | uint32(slot[11])
	} else {
		flags = uint32(slot[11])<<24 | uint32(slot[10])<<16 | uint32(slot[9])<<8 | uint32(slot[8])
	}
	if flags&1 == 0 {
		return 0, false
	}
	// The reference client counts the seven high flag bits (Toad/MIPS
	// secrets) as stars too, then the 25 star bytes.
	stars := bits.OnesCount32((flags >> 24) & 0x7f)
	for _, b := range slot[12:37] {
		stars += bits.OnesCount8(b)
	}
	return stars, true
}
