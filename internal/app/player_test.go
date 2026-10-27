package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlayerLookAndPresets(t *testing.T) {
	svc, gameData := newTestService(t)
	gameDir := filepath.Join(filepath.Dir(gameData), "game")
	writeConfig(t, svc, "sound = 1\n"+
		"coop_player_name Testy Player\n"+
		"coop_player_model 2\n"+
		"coop_player_palette_shirt ff 80 10\n"+
		"coop_player_palette_pants 10 20 30\n")

	st, err := svc.Player()
	if err != nil {
		t.Fatal(err)
	}
	if st.Look.Name != "Testy Player" || st.Look.Model != 2 {
		t.Fatalf("look = %+v", st.Look)
	}
	if st.Look.Colors["shirt"] != "#ff8010" || st.Look.Colors["pants"] != "#102030" {
		t.Fatalf("colors = %+v", st.Look.Colors)
	}
	if st.Look.Colors["cap"] == "" {
		t.Fatal("missing colours were not filled with the character defaults")
	}

	colors := map[string]string{}
	for key, value := range defaultLook[1] {
		colors[key] = value
	}
	st, err = svc.SetPlayer(PlayerLook{Name: "Nuevo", Model: 1, Colors: colors})
	if err != nil {
		t.Fatal(err)
	}
	if st.Look.Name != "Nuevo" || st.Look.Model != 1 {
		t.Fatalf("look = %+v", st.Look)
	}
	data, err := os.ReadFile(svc.configPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"coop_player_name Nuevo",
		"coop_player_model 1",
		"coop_player_palette_shirt 00 ff 00",
		"sound = 1",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config lacks %q:\n%s", want, data)
		}
	}

	// A palette shipped with the game shows up as a built-in preset.
	paletteDir := filepath.Join(gameDir, "palettes")
	if err := os.MkdirAll(paletteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	palette := "[PALETTE]\nPANTS_R = 1\nPANTS_G = 2\nPANTS_B = 3\nSHIRT_R = 4\nSHIRT_G = 5\nSHIRT_B = 6\n"
	if err := os.WriteFile(filepath.Join(paletteDir, "Mario.ini"), []byte(palette), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err = svc.Player()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Presets) != 1 || !st.Presets[0].BuiltIn || st.Presets[0].Name != "Mario" {
		t.Fatalf("presets = %+v", st.Presets)
	}
	if st.Presets[0].Model == nil || *st.Presets[0].Model != 0 {
		t.Fatalf("built-in model = %+v", st.Presets[0].Model)
	}

	st, err = svc.SavePlayerPreset("MiLook")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Presets) != 2 || st.Presets[1].Name != "MiLook" || st.Presets[1].BuiltIn {
		t.Fatalf("presets after save = %+v", st.Presets)
	}
	if _, err := os.Stat(filepath.Join(gameData, "palettes", "MiLook.ini")); err != nil {
		t.Fatal(err)
	}
	models, err := os.ReadFile(filepath.Join(svc.LoaderDir(), "player-presets.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(models), `"MiLook": 1`) {
		t.Fatalf("player-presets.json = %s", models)
	}

	if _, err := svc.DeletePlayerPreset("MiLook"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(gameData, "palettes", "MiLook.ini")); !os.IsNotExist(err) {
		t.Fatal("the preset file was not moved aside")
	}
	if !dirHasPrefix(filepath.Join(gameData, "removed-by-coop-loader"), "presets-") {
		t.Fatal("no preset backup folder")
	}
}

func TestCreateMod(t *testing.T) {
	svc, gameData := newTestService(t)
	name, err := svc.CreateMod("Mi Mod")
	if err != nil {
		t.Fatal(err)
	}
	if name != "Mi Mod" {
		t.Fatalf("name = %q", name)
	}
	data, err := os.ReadFile(filepath.Join(gameData, "mods", "Mi Mod", "main.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "-- name: Mi Mod") {
		t.Fatalf("main.lua = %s", data)
	}
	if _, err := svc.CreateMod("Mi Mod"); err == nil {
		t.Fatal("an existing mod was overwritten")
	}
	svc.running = func() bool { return true }
	if _, err := svc.CreateMod("Otro"); err != ErrGameRunning {
		t.Fatalf("while the game runs = %v", err)
	}
}
