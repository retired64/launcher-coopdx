package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/retired64/coop-loader/internal/devicecopy"
	"github.com/retired64/coop-loader/internal/installed"
	"github.com/retired64/coop-loader/internal/session"
)

func copyFixture(t *testing.T) (*Service, *devicecopy.Manifest, *devicecopy.Source, map[string][]byte) {
	t.Helper()
	svc, gameData := newTestService(t)
	addMod(t, gameData, "CoolMod", "-- name: Cool Mod\n")
	if err := os.MkdirAll(filepath.Join(gameData, "mods", "CoolMod", "actors"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gameData, "mods", "CoolMod", "actors", "model.bin"), []byte{1, 2, 3, 4}, 0o644); err != nil {
		t.Fatal(err)
	}
	single := filepath.Join(gameData, "mods", "single.lua")
	if err := os.WriteFile(single, []byte("-- name: Single\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gameData, "dynos", "packs", "PackA"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gameData, "dynos", "packs", "PackA", "geo.bin"), []byte{9, 8, 7}, 0o644); err != nil {
		t.Fatal(err)
	}

	config := "sound = 1\r\n" +
		"coop_player_name Testy Player\r\n" +
		"coop_player_model 2\r\n"
	for _, key := range devicecopy.PaletteKeys {
		config += "coop_player_palette_" + key + " ff 80 10\r\n"
	}
	config += "enable-mod: CoolMod\r\nenable-mod: single.lua\r\ndynos-pack: PackA true\r\nsave-name: 1 Testy\r\n"
	if err := os.WriteFile(svc.configPath(), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	save := make([]byte, 512)
	copy(save, []byte("save-bytes"))
	if err := os.WriteFile(svc.savePath(), save, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gameData, "sav"), 0o755); err != nil {
		t.Fatal(err)
	}
	sav := []byte{'s', 'a', 'v', 0x80}
	if err := os.WriteFile(filepath.Join(gameData, "sav", "mod.sav"), sav, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gameData, "palettes"), 0o755); err != nil {
		t.Fatal(err)
	}
	palette := "[PALETTE]\nPANTS_R = 1\nPANTS_G = 2\nPANTS_B = 3\n"
	if err := os.WriteFile(filepath.Join(gameData, "palettes", "MyPreset.ini"), []byte(palette), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.LoaderDir(), "player-presets.json"), []byte(`{"models":{"MyPreset":1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.records.Set([]string{"CoolMod"}, installed.Record{
		SiteID:      12,
		SiteURL:     "https://mods.sm64coopdx.com/mods/cool.12/",
		Title:       "Cool Mod",
		Version:     "1.2",
		InstalledAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	src, err := svc.buildCopySource(devicecopy.DeviceInfo{Name: "sender", Kind: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := devicecopy.ParseManifest(src.Text)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for i := range src.Files {
		data, err := src.Read(i, 0, int(src.Files[i].Size))
		if err != nil {
			t.Fatal(err)
		}
		files[manifest.Flatten()[i].Ref.Rel] = data
	}
	return svc, manifest, src, files
}

func TestBuildCopyManifest(t *testing.T) {
	_, manifest, _, _ := copyFixture(t)
	if manifest.Device.Name != "sender" || manifest.Device.Kind != "linux" {
		t.Fatalf("device = %+v", manifest.Device)
	}
	if manifest.Player == nil || manifest.Player.Name != "Testy" || manifest.Player.Model != 2 {
		t.Fatalf("player = %+v", manifest.Player)
	}
	if manifest.Player.Colors["shirt"] != "#ff8010" {
		t.Fatalf("shirt = %q", manifest.Player.Colors["shirt"])
	}
	if manifest.Save == nil {
		t.Fatal("save missing")
	}
	if len(manifest.Savs) != 1 || manifest.Savs[0].Name != "mod.sav" {
		t.Fatalf("savs = %+v", manifest.Savs)
	}
	if len(manifest.Presets) != 1 || manifest.Presets[0].Name != "MyPreset" {
		t.Fatalf("presets = %+v", manifest.Presets)
	}
	if manifest.Presets[0].Model == nil || *manifest.Presets[0].Model != 1 {
		t.Fatalf("preset model = %+v", manifest.Presets[0].Model)
	}
	if len(manifest.Mods) != 3 {
		t.Fatalf("mods = %d", len(manifest.Mods))
	}
	var cool *devicecopy.Mod
	for i := range manifest.Mods {
		if manifest.Mods[i].ID == "CoolMod" {
			cool = &manifest.Mods[i]
		}
	}
	if cool == nil || !cool.Enabled || cool.Record == nil || cool.Record.SiteID != 12 {
		t.Fatalf("cool mod = %+v", cool)
	}
	if len(cool.Files) != 2 {
		t.Fatalf("cool files = %+v", cool.Files)
	}
	// The single-file mod and the pack must have their files too.
	for _, mod := range manifest.Mods {
		if mod.Location == "user" && len(mod.Files) == 0 {
			t.Fatalf("%s has no files", mod.ID)
		}
	}
}

func TestApplyCopy(t *testing.T) {
	_, manifest, src, files := copyFixture(t)
	receiver, gameData := newTestService(t)
	addMod(t, gameData, "CoolMod", "-- name: Old\n")
	if err := os.MkdirAll(filepath.Join(gameData, "palettes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gameData, "palettes", "MyPreset.ini"), []byte("[PALETTE]\nPANTS_R = 9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldSave := bytes.Repeat([]byte{7}, 512)
	if err := os.WriteFile(receiver.savePath(), oldSave, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gameData, "sav"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gameData, "sav", "mod.sav"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	local, err := receiver.localCopyState()
	if err != nil {
		t.Fatal(err)
	}
	plan := devicecopy.BuildPlan(manifest, local, manifest.SaveStars(), devicecopy.Choice{Mods: true, Look: true, Saves: true}, "direct")
	if len(plan.Want) == 0 {
		t.Fatal("nothing to copy")
	}

	staging := t.TempDir()
	if err := os.MkdirAll(filepath.Join(staging, "in"), 0o755); err != nil {
		t.Fatal(err)
	}
	flat := manifest.Flatten()
	for _, index := range plan.Want {
		data, err := src.Read(index, 0, int(flat[index].Ref.Size))
		if err != nil {
			t.Fatal(err)
		}
		part := filepath.Join(staging, "in", strconv.Itoa(index))
		if err := os.WriteFile(part, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	note, err := receiver.applyCopy(manifest, plan, staging)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("note = %q", note)
	}

	// Mods, pack and single file landed.
	if data, err := os.ReadFile(filepath.Join(gameData, "mods", "CoolMod", "actors", "model.bin")); err != nil || !bytes.Equal(data, files["actors/model.bin"]) {
		t.Fatalf("model = %v, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(gameData, "mods", "single.lua")); err != nil || !bytes.Equal(data, files["single.lua"]) {
		t.Fatalf("single = %v, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(gameData, "dynos", "packs", "PackA", "geo.bin")); err != nil || !bytes.Equal(data, files["geo.bin"]) {
		t.Fatalf("pack = %v, %v", data, err)
	}

	// Config was rewritten with the sender's enabled set, look and names.
	config, err := os.ReadFile(receiver.configPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"enable-mod: CoolMod",
		"enable-mod: single.lua",
		"dynos-pack: PackA true",
		"coop_player_name Testy",
		"coop_player_model 2",
		"coop_player_palette_shirt ff 80 10",
		"save-name: 1 Testy",
		"sound = 1",
	} {
		if !strings.Contains(string(config), want) {
			t.Errorf("config lacks %q:\n%s", want, config)
		}
	}

	// Save, mod save and palette.
	if data, err := os.ReadFile(receiver.savePath()); err != nil || !bytes.Equal(data, bytes.Join([][]byte{[]byte("save-bytes"), make([]byte, 502)}, nil)) {
		t.Fatalf("save = %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(gameData, "sav", "mod.sav")); err != nil || !bytes.Equal(data, []byte{'s', 'a', 'v', 0x80}) {
		t.Fatalf("sav = %v, %v", data, err)
	}
	palette, err := os.ReadFile(filepath.Join(gameData, "palettes", "MyPreset.ini"))
	if err != nil || !strings.Contains(string(palette), "PANTS_R = 1") || !strings.Contains(string(palette), "PANTS_G = 2") {
		t.Fatalf("palette = %q, %v", palette, err)
	}

	// Record and backups.
	if rec, ok := receiver.records.Get("CoolMod"); !ok || rec.SiteID != 12 {
		t.Fatalf("record = %+v, %v", rec, ok)
	}
	if !dirHasPrefix(filepath.Join(gameData, "removed-by-coop-loader"), "copy-") {
		t.Fatal("no copy backup folder")
	}
	if _, err := os.Stat(filepath.Join(gameData, "sm64config.modloader-backup.txt")); err != nil {
		t.Fatalf("config backup missing: %v", err)
	}
}

func dirHasPrefix(root, prefix string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			return true
		}
	}
	return false
}

func TestCopyAndSessionAreMutuallyExclusive(t *testing.T) {
	t.Setenv("COOP_NO_TRACKERS", "1")
	t.Setenv("COOP_RELAY_URL", "http://127.0.0.1:1")
	svc, _ := newTestService(t)

	if _, err := svc.StartCopy(devicecopy.RoleSend, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartSession(session.ModeJoin, "ABCD-1234"); !errors.Is(err, ErrCopyActive) {
		t.Fatalf("session while copying = %v", err)
	}
	svc.StopCopy()

	if _, err := svc.StartSession(session.ModeJoin, "ABCD-1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartCopy(devicecopy.RoleSend, ""); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("copy while playing = %v", err)
	}
	svc.StopSession()
}
