// Package rom reads the pictures and letters the app borrows from the
// player's own Super Mario 64 (USA) ROM.
//
// Nothing copyrighted is stored in this repository. The loader opens the ROM
// the player already has, verifies its SHA-1, decompresses three MIO0 blocks,
// decodes the textures and the two bitmap fonts, and keeps the result in the
// loader's cache. If the ROM is missing, the app keeps its plain fallback look.
package rom

import (
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The one ROM sm64coopdx runs: Super Mario 64 (USA), revision 0.
const USASHA1 = "9bef1128717f958171a4afac3ed78ee2bb4e86ce"

// AssetVersion changes when the extraction format changes. It is stored in
// version.json so an old cache is rebuilt after an update.
const AssetVersion = 1

// ErrWrongROM means the file is readable but is not the ROM the game uses.
var ErrWrongROM = errors.New("this is not the Super Mario 64 (USA) ROM that sm64coopdx uses")

// ErrNoROM means no candidate file exists.
var ErrNoROM = errors.New("no Super Mario 64 (USA) ROM found")

// ROM is a verified, byte-swapped image.
type ROM struct {
	Data []byte
	Path string

	blocks map[int][]byte
}

// Load reads and verifies a ROM file.
func Load(path string) (*ROM, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoROM
		}
		return nil, err
	}
	return Parse(data, path)
}

// Parse verifies bytes already in memory and normalises the byte order.
func Parse(data []byte, path string) (*ROM, error) {
	if len(data) < 0x1000 {
		return nil, ErrWrongROM
	}
	normal, err := normalise(data)
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum(normal)
	if hex.EncodeToString(sum[:]) != USASHA1 {
		return nil, ErrWrongROM
	}
	return &ROM{Data: normal, Path: path, blocks: map[int][]byte{}}, nil
}

// SHA1 returns the verified hash in lower-case hex.
func (r *ROM) SHA1() string { return USASHA1 }

func normalise(data []byte) ([]byte, error) {
	if len(data) < 4 {
		return nil, ErrWrongROM
	}
	out := append([]byte(nil), data...)
	switch binary.BigEndian.Uint32(out[:4]) {
	case 0x80371240: // z64, already big-endian
		return out, nil
	case 0x37804012: // v64, every 16-bit word swapped
		for i := 0; i+1 < len(out); i += 2 {
			out[i], out[i+1] = out[i+1], out[i]
		}
		return out, nil
	case 0x40123780: // n64, every 32-bit word reversed
		for i := 0; i+3 < len(out); i += 4 {
			out[i], out[i+1], out[i+2], out[i+3] = out[i+3], out[i+2], out[i+1], out[i]
		}
		return out, nil
	default:
		return nil, ErrWrongROM
	}
}

// Block returns the decompressed contents of a MIO0 block. An offset that is
// not MIO0 is returned as a tail of the ROM, exactly like the game does for
// uncompressed segments.
func (r *ROM) Block(offset int) ([]byte, error) {
	if b, ok := r.blocks[offset]; ok {
		return b, nil
	}
	if offset < 0 || offset+16 > len(r.Data) {
		return nil, errors.New("a texture block is outside the ROM")
	}
	var out []byte
	if string(r.Data[offset:offset+4]) == "MIO0" {
		var err error
		out, err = decompressMIO0(r.Data, offset)
		if err != nil {
			return nil, err
		}
	} else {
		out = r.Data[offset:]
	}
	r.blocks[offset] = out
	return out, nil
}

// decompressMIO0 is the Nintendo 64 MIO0 decoder. The format keeps a bit mask
// (1 = literal byte, 0 = back-reference) followed by the compressed and literal
// streams.
func decompressMIO0(data []byte, offset int) ([]byte, error) {
	if offset < 0 || offset+16 > len(data) || string(data[offset:offset+4]) != "MIO0" {
		return nil, errors.New("not a MIO0 block")
	}
	size := int(binary.BigEndian.Uint32(data[offset+4:]))
	comp := offset + int(binary.BigEndian.Uint32(data[offset+8:]))
	lit := offset + int(binary.BigEndian.Uint32(data[offset+12:]))
	bitsAt := offset + 16
	if size < 0 || size > 64<<20 || comp < bitsAt || lit < bitsAt || comp > len(data) || lit > len(data) {
		return nil, errors.New("a damaged MIO0 block")
	}
	out := make([]byte, size)
	written := 0
	mask := uint32(0)
	left := 0
	for written < size {
		if left == 0 {
			if bitsAt+4 > len(data) {
				return nil, errors.New("a damaged MIO0 block")
			}
			mask = binary.BigEndian.Uint32(data[bitsAt:])
			bitsAt += 4
			left = 32
		}
		if mask&0x80000000 != 0 {
			if lit >= len(data) {
				return nil, errors.New("a damaged MIO0 block")
			}
			out[written] = data[lit]
			written++
			lit++
		} else {
			if comp+2 > len(data) {
				return nil, errors.New("a damaged MIO0 block")
			}
			word := binary.BigEndian.Uint16(data[comp:])
			comp += 2
			length := int(word>>12) + 3
			distance := int(word&0x0fff) + 1
			if distance > written {
				return nil, errors.New("a damaged MIO0 block")
			}
			for i := 0; i < length && written < size; i++ {
				out[written] = out[written-distance]
				written++
			}
		}
		mask <<= 1
		left--
	}
	return out, nil
}

// DecodeTexture turns one RGBA16, IA8, IA4 or IA1 texture into an image.
func DecodeTexture(data []byte, def textureDef) (*image.RGBA, error) {
	w, h := def.W, def.H
	if w <= 0 || h <= 0 || w*h > 64<<20 {
		return nil, errors.New("a texture has a bad size")
	}
	if len(data) < def.Size {
		return nil, errors.New("a texture is shorter than expected")
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	pixels := w * h
	switch def.Format {
	case "rgba16":
		if len(data) < pixels*2 {
			return nil, errors.New("a texture is shorter than expected")
		}
		for i := 0; i < pixels; i++ {
			v := binary.BigEndian.Uint16(data[i*2:])
			r := expand5(uint8((v >> 11) & 31))
			g := expand5(uint8((v >> 6) & 31))
			b := expand5(uint8((v >> 1) & 31))
			a := uint8(0)
			if v&1 != 0 {
				a = 255
			}
			setPixel(img, i, r, g, b, a)
		}
	case "ia8":
		for i := 0; i < pixels; i++ {
			v := data[i]
			c := (v >> 4) * 17
			setPixel(img, i, c, c, c, (v&15)*17)
		}
	case "ia4":
		for i := 0; i < pixels; i++ {
			n := data[i>>1] >> 4
			if i&1 != 0 {
				n = data[i>>1] & 15
			}
			c := uint8(math.Round(float64((n>>1)&7) * 255 / 7))
			a := uint8(0)
			if n&1 != 0 {
				a = 255
			}
			setPixel(img, i, c, c, c, a)
		}
	case "ia1":
		for i := 0; i < pixels; i++ {
			bit := (data[i>>3] >> (7 - uint(i&7))) & 1
			c := uint8(0)
			if bit != 0 {
				c = 255
			}
			setPixel(img, i, c, c, c, c)
		}
	default:
		return nil, fmt.Errorf("unsupported texture format %q", def.Format)
	}
	return img, nil
}

// expand5 mirrors the reference client: (v << 3) | (v >> 2).
func expand5(v uint8) uint8 { return (v << 3) | (v >> 2) }

// setPixel writes RGBA and clears the colour under a fully transparent pixel,
// as the reference client's bitmap conversion does.
func setPixel(img *image.RGBA, i int, r, g, b, a uint8) {
	o := i * 4
	if a == 0 {
		img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = 0, 0, 0, 0
		return
	}
	img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = r, g, b, a
}

// scale2 doubles a texture without smoothing, for the large cursor hand.
func scale2(src *image.RGBA) *image.RGBA {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	out := image.NewRGBA(image.Rect(0, 0, w*2, h*2))
	for y := 0; y < h*2; y++ {
		for x := 0; x < w*2; x++ {
			so := (y/2)*src.Stride + (x/2)*4
			do := y*out.Stride + x*4
			copy(out.Pix[do:do+4], src.Pix[so:so+4])
		}
	}
	return out
}

// Assets describes a finished cache.
type Assets struct {
	Dir       string
	RomPath   string
	RomSHA1   string
	ExtraHUD  bool
	Textures  int
	HUD       int
	DialogOTF string
	MenuOTF   string
}

// Version is the cache manifest.
type Version struct {
	Version int    `json:"version"`
	ROM     string `json:"rom"`
	ROMPath string `json:"romPath,omitempty"`
	EXE     string `json:"exe,omitempty"`
}

// CacheDir is where the extracted files live.
func CacheDir(loaderDir string) string { return filepath.Join(loaderDir, "rom-assets") }

// ReadCache reads version.json from a cache directory.
func ReadCache(loaderDir string) (Version, error) {
	var v Version
	data, err := os.ReadFile(filepath.Join(CacheDir(loaderDir), "version.json"))
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return v, err
	}
	return v, nil
}

// CacheUsable reports whether a cache is complete for the given ROM and game
// executable. An empty romSHA1 accepts a cache whose files are present.
func CacheUsable(loaderDir, romSHA1, exeStamp string) bool {
	v, err := ReadCache(loaderDir)
	if err != nil || v.Version != AssetVersion {
		return false
	}
	if romSHA1 != "" && v.ROM != romSHA1 {
		return false
	}
	if exeStamp != "" && v.EXE != "" && v.EXE != exeStamp {
		return false
	}
	dir := CacheDir(loaderDir)
	for _, name := range []string{
		"textures/wood_yellow.png", "textures/marble_dark.png", "textures/mario_head.png",
		"hud/u0041.png", "dialog.otf", "dialog.ttf", "menu.otf", "menu.ttf", "hud.json",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return false
		}
	}
	return true
}

// EXEStamp identifies a game executable by size and modification time, so new
// HUD letters are picked up after a game update.
func EXEStamp(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixMilli())
}

// Extract decodes a ROM into dir. exePath is optional; when it is a supported
// game executable, the few HUD letters the vanilla ROM lacks (J, Q, V, X, Z and
// some punctuation) are read from it too.
func Extract(romData []byte, romPath, exePath, dir string) (*Assets, error) {
	r, err := Parse(romData, romPath)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(parent, ".rom-assets-*")
	if err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := os.MkdirAll(filepath.Join(tmp, "textures"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(tmp, "hud"), 0o755); err != nil {
		return nil, err
	}

	assets := &Assets{Dir: tmp, RomPath: romPath, RomSHA1: r.SHA1()}
	for name, def := range textureDefs {
		block, err := r.Block(def.Block)
		if err != nil {
			return nil, err
		}
		if def.Sub < 0 || def.Sub+def.Size > len(block) {
			return nil, fmt.Errorf("%s is outside its block", name)
		}
		img, err := DecodeTexture(block[def.Sub:def.Sub+def.Size], def)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := writePNG(filepath.Join(tmp, "textures", name+".png"), img); err != nil {
			return nil, err
		}
		if strings.HasPrefix(name, "hand_") {
			if err := writePNG(filepath.Join(tmp, "textures", name+"@2x.png"), scale2(img)); err != nil {
				return nil, err
			}
		}
		assets.Textures++
	}

	hudJSON := map[string]string{}
	for key, def := range hudDefs {
		block, err := r.Block(def.Block)
		if err != nil {
			return nil, err
		}
		if def.Sub < 0 || def.Sub+def.Size > len(block) {
			return nil, fmt.Errorf("HUD %s is outside its block", key)
		}
		img, err := DecodeTexture(block[def.Sub:def.Sub+def.Size], def)
		if err != nil {
			return nil, fmt.Errorf("hud %s: %w", key, err)
		}
		name := hudFile(key)
		if err := writePNG(filepath.Join(tmp, "hud", name+".png"), img); err != nil {
			return nil, err
		}
		hudJSON[key] = "hud/" + name + ".png"
		assets.HUD++
	}

	if exePath != "" {
		extra, err := executableHUD(exePath)
		if err == nil {
			for key, raw := range extra {
				if _, ok := hudJSON[key]; ok {
					continue
				}
				img, err := DecodeTexture(raw, textureDef{W: 16, H: 16, Size: 512, Format: "rgba16"})
				if err != nil {
					continue
				}
				name := hudFile(key)
				if err := writePNG(filepath.Join(tmp, "hud", name+".png"), img); err != nil {
					return nil, err
				}
				hudJSON[key] = "hud/" + name + ".png"
				assets.HUD++
				assets.ExtraHUD = true
			}
		}
	}

	if err := writeJSON(filepath.Join(tmp, "hud.json"), hudJSON); err != nil {
		return nil, err
	}

	dialog, err := buildFont("SM64 Dialog", 16, dialogSpace, dialogDefs, true, false, r)
	if err != nil {
		return nil, fmt.Errorf("dialog font: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "dialog.otf"), dialog, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "dialog.ttf"), dialog, 0o644); err != nil {
		return nil, err
	}
	assets.DialogOTF = filepath.Join(tmp, "dialog.otf")

	menu, err := buildFont("SM64 Menu", 8, menuSpace, menuDefs, false, true, r)
	if err != nil {
		return nil, fmt.Errorf("menu font: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "menu.otf"), menu, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "menu.ttf"), menu, 0o644); err != nil {
		return nil, err
	}
	assets.MenuOTF = filepath.Join(tmp, "menu.otf")

	if names, err := extractNames(r); err == nil {
		if err := writeJSON(filepath.Join(tmp, "names.json"), names); err != nil {
			return nil, err
		}
	}

	v := Version{Version: AssetVersion, ROM: r.SHA1(), ROMPath: romPath, EXE: EXEStamp(exePath)}
	if err := writeJSON(filepath.Join(tmp, "version.json"), v); err != nil {
		return nil, err
	}

	old := dir + ".old"
	_ = os.RemoveAll(old)
	if _, err := os.Stat(dir); err == nil {
		if err := os.Rename(dir, old); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		_ = os.Rename(old, dir)
		return nil, err
	}
	_ = os.RemoveAll(old)
	cleanup = false
	assets.Dir = dir
	assets.DialogOTF = filepath.Join(dir, "dialog.otf")
	assets.MenuOTF = filepath.Join(dir, "menu.otf")
	return assets, nil
}

func hudFile(key string) string {
	if len([]rune(key)) == 1 {
		return fmt.Sprintf("u%04x", []rune(key)[0])
	}
	return key
}

func writePNG(path string, img *image.RGBA) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	err = png.Encode(f, img)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

var leadingNumber = regexp.MustCompile(`^\s*\d*\s*`)

// extractNames reads the course and act names from the same text block the
// game's file select uses.
func extractNames(r *ROM) (map[string]any, error) {
	block, err := r.Block(textBlock)
	if err != nil {
		return nil, err
	}
	read := func(ptr int) (string, bool) {
		if ptr < 0 || ptr+4 > len(block) {
			return "", false
		}
		word := binary.BigEndian.Uint32(block[ptr:])
		if word>>24 != 2 {
			return "", false
		}
		var b strings.Builder
		for i := int(word & 0xffffff); i < len(block); i++ {
			if block[i] == 0xff {
				return b.String(), true
			}
			ch, ok := textChars[block[i]]
			if !ok || b.Len() > 64 {
				return "", false
			}
			b.WriteRune(ch)
		}
		return "", false
	}
	list := func(at, count int, stripNumber bool) ([]string, bool) {
		out := make([]string, 0, count)
		for i := 0; i < count; i++ {
			name, ok := read(at + i*4)
			if !ok {
				return nil, false
			}
			if stripNumber {
				name = leadingNumber.ReplaceAllString(name, "")
			}
			out = append(out, strings.TrimSpace(name))
		}
		return out, true
	}
	courses, ok := list(textCourses, textCourseCount, true)
	if !ok {
		return nil, errors.New("course names are damaged")
	}
	acts, ok := list(textActs, textActCount, false)
	if !ok {
		return nil, errors.New("act names are damaged")
	}
	return map[string]any{"courses": courses, "acts": acts}, nil
}

// FindCandidates returns the paths the reference client tries, in order.
func FindCandidates(configured, gameData, gamePath string) []string {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		for _, old := range out {
			if old == p {
				return
			}
		}
		out = append(out, p)
	}
	add(configured)
	if gameData != "" {
		add(filepath.Join(gameData, "baserom.us.z64"))
	}
	if gamePath != "" {
		add(filepath.Join(gamePath, "baserom.us.z64"))
	}
	return out
}

// Find returns the first candidate that is really the USA ROM.
func Find(configured, gameData, gamePath string) (string, *ROM, error) {
	var lastErr error = ErrNoROM
	found := false
	for _, path := range FindCandidates(configured, gameData, gamePath) {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		found = true
		r, err := Load(path)
		if err == nil {
			return path, r, nil
		}
		lastErr = err
	}
	if !found {
		return "", nil, ErrNoROM
	}
	return "", nil, lastErr
}

// DescribeError turns a load failure into a short message for the UI.
func DescribeError(err error) string {
	if errors.Is(err, ErrNoROM) {
		return "No se encontró tu ROM de Super Mario 64 (USA)."
	}
	if errors.Is(err, ErrWrongROM) {
		return "Ese archivo no es la ROM de Super Mario 64 (USA) que usa sm64coopdx."
	}
	return "No se pudo leer la ROM: " + err.Error()
}
