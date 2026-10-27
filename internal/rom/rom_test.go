package rom

import (
	"encoding/binary"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMIO0Decompression(t *testing.T) {
	// Header, one flag word, one back-reference, then four literal bytes.
	// 0xF0000000 means four literals and then one copy.
	data := []byte{
		'M', 'I', 'O', '0',
		0, 0, 0, 8, // uncompressed size
		0, 0, 0, 20, // compressed stream
		0, 0, 0, 22, // literal stream
		0xF0, 0, 0, 0,
		0x10, 0x03, // length 4, distance 4
		'a', 'b', 'c', 'd',
	}
	out, err := decompressMIO0(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "abcdabcd" {
		t.Fatalf("out = %q", out)
	}
}

func TestWrongROM(t *testing.T) {
	if _, err := Parse(make([]byte, 0x2000), "x.z64"); err == nil {
		t.Fatal("a blank file was accepted as the USA ROM")
	}
}

func TestExtractAgainstReference(t *testing.T) {
	romPath := os.Getenv("COOP_TEST_ROM")
	ref := os.Getenv("COOP_TEST_ASSETS")
	if romPath == "" || ref == "" {
		t.Skip("set COOP_TEST_ROM and COOP_TEST_ASSETS to compare against a real extraction")
	}
	data, err := os.ReadFile(romPath)
	if err != nil {
		t.Fatal(err)
	}
	exe := os.Getenv("COOP_TEST_EXE")
	dir := filepath.Join(t.TempDir(), "rom-assets")
	if _, err := Extract(data, romPath, exe, dir); err != nil {
		t.Fatal(err)
	}

	for name := range textureDefs {
		got := readPNG(t, filepath.Join(dir, "textures", name+".png"))
		want := readPNG(t, filepath.Join(ref, "textures", name+".png"))
		if !samePixels(got, want) {
			t.Errorf("texture %s differs", name)
		}
	}
	hudJSON, err := os.ReadFile(filepath.Join(dir, "hud.json"))
	if err != nil {
		t.Fatal(err)
	}
	var hud map[string]string
	if err := json.Unmarshal(hudJSON, &hud); err != nil {
		t.Fatal(err)
	}
	for key, rel := range hud {
		got := readPNG(t, filepath.Join(dir, rel))
		want := readPNG(t, filepath.Join(ref, rel))
		if !samePixels(got, want) {
			t.Errorf("hud %s differs", key)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "hud", "u004a.png")); err == nil {
		got := readPNG(t, filepath.Join(dir, "hud", "u004a.png"))
		want := readPNG(t, filepath.Join(ref, "hud", "u004a.png"))
		if !samePixels(got, want) {
			t.Error("extra HUD J differs")
		}
	}

	var gotNames, wantNames map[string]any
	readJSON(t, filepath.Join(dir, "names.json"), &gotNames)
	readJSON(t, filepath.Join(ref, "names.json"), &wantNames)
	if !reflect.DeepEqual(gotNames, wantNames) {
		gotJSON, _ := json.Marshal(gotNames)
		wantJSON, _ := json.Marshal(wantNames)
		t.Errorf("names.json differs\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

func readPNG(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func samePixels(a, b image.Image) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	ar, ag, ab, aa := a.Bounds(), b.Bounds(), a.Bounds(), b.Bounds()
	_ = ar
	_ = ag
	_ = ab
	_ = aa
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			r1, g1, b1, a1 := a.At(x, y).RGBA()
			r2, g2, b2, a2 := b.At(x, y).RGBA()
			if a1 == 0 && a2 == 0 {
				continue
			}
			if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
				return false
			}
		}
	}
	return true
}

func readJSON(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}

func TestNormaliseByteOrders(t *testing.T) {
	z64 := make([]byte, 0x2000)
	copy(z64, []byte{0x80, 0x37, 0x12, 0x40, 1, 2, 3, 4})
	v64 := append([]byte(nil), z64...)
	v64[0], v64[1], v64[2], v64[3] = 0x37, 0x80, 0x40, 0x12
	v64[4], v64[5], v64[6], v64[7] = 2, 1, 4, 3
	n64 := append([]byte(nil), z64...)
	n64[0], n64[1], n64[2], n64[3] = 0x40, 0x12, 0x37, 0x80
	n64[4], n64[5], n64[6], n64[7] = 4, 3, 2, 1
	for name, in := range map[string][]byte{"z64": z64, "v64": v64, "n64": n64} {
		out, err := normalise(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(out[:8]) != string(z64[:8]) {
			t.Fatalf("%s: %x", name, out[:8])
		}
	}
}

func TestWriteTTF(t *testing.T) {
	glyphs := []fontGlyph{
		{Name: ".notdef", Advance: 5},
		{Name: "space", R: ' ', Advance: 5},
		{Name: "u0041", R: 'A', Advance: 6, Contours: []contour{{
			{0, 0}, {64, 0}, {64, 64}, {0, 64},
		}}},
	}
	data, err := writeTTF("Test Font", 1024, 832, -192, glyphs)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 12+16*10 {
		t.Fatalf("font is too short: %d", len(data))
	}
	if string(data[:4]) != "\x00\x01\x00\x00" {
		t.Fatalf("sfnt version = %x", data[:4])
	}
	if n := int(binary.BigEndian.Uint16(data[4:])); n != 10 {
		t.Fatalf("tables = %d", n)
	}
	tags := map[string]bool{}
	var headAt int
	for i := 0; i < 10; i++ {
		rec := 12 + i*16
		tag := string(data[rec : rec+4])
		tags[tag] = true
		at := int(binary.BigEndian.Uint32(data[rec+8:]))
		length := int(binary.BigEndian.Uint32(data[rec+12:]))
		if at < 0 || at+length > len(data) {
			t.Fatalf("%s is outside the font", tag)
		}
		if tag == "head" {
			headAt = at
		}
	}
	for _, tag := range []string{"OS/2", "cmap", "glyf", "head", "hhea", "hmtx", "loca", "maxp", "name", "post"} {
		if !tags[tag] {
			t.Fatalf("missing %s table", tag)
		}
	}
	// The whole-font checksum must land on the magic constant.
	if sum := checksum(data); sum != 0xb1b0afba {
		t.Fatalf("whole-font checksum = %#x", sum)
	}
	if headAt == 0 || binary.BigEndian.Uint32(data[headAt+12:]) != 0x5F0F3CF5 {
		t.Fatal("head magic is missing")
	}
}

func TestCacheUsable(t *testing.T) {
	loaderDir := t.TempDir()
	dir := CacheDir(loaderDir)
	for _, name := range []string{
		"textures/wood_yellow.png", "textures/marble_dark.png", "textures/mario_head.png",
		"hud/u0041.png", "dialog.otf", "dialog.ttf", "menu.otf", "menu.ttf", "hud.json",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeJSON(filepath.Join(dir, "version.json"), Version{Version: AssetVersion, ROM: USASHA1}); err != nil {
		t.Fatal(err)
	}
	if !CacheUsable(loaderDir, USASHA1, "") {
		t.Fatal("a complete cache was rejected")
	}
	if CacheUsable(loaderDir, "another-rom", "") {
		t.Fatal("a cache for another ROM was accepted")
	}
	if err := os.Remove(filepath.Join(dir, "dialog.otf")); err != nil {
		t.Fatal(err)
	}
	if CacheUsable(loaderDir, USASHA1, "") {
		t.Fatal("an incomplete cache was accepted")
	}
}
