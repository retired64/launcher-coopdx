package app

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/retired64/coop-loader/internal/mods"
)

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	loaderDir := filepath.Join(root, "loader")
	gameData := filepath.Join(root, "data")
	gameDir := filepath.Join(root, "game")

	for _, dir := range []string{
		loaderDir,
		filepath.Join(gameData, "mods"),
		filepath.Join(gameData, "dynos", "packs"),
		gameDir,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	exe := filepath.Join(gameDir, exeNameForTest())
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	svc := New(loaderDir, gameData)
	svc.running = func() bool { return false }
	if err := svc.SetGamePath(gameDir); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, svc, "sound = 1\n\n")
	return svc, gameData
}

func exeNameForTest() string {
	if runtime.GOOS == "windows" {
		return "sm64coopdx.exe"
	}
	return "sm64coopdx"
}

func writeConfig(t *testing.T, svc *Service, body string) {
	t.Helper()
	if err := os.WriteFile(svc.configPath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func addMod(t *testing.T, gameData, id, body string) {
	t.Helper()
	dir := filepath.Join(gameData, "mods", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.lua"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestToggleWritesConfigAndBackup(t *testing.T) {
	svc, gameData := newTestService(t)
	addMod(t, gameData, "NiceMod", "-- name: Nice Mod\n")

	if _, err := svc.Toggle("NiceMod", mods.KindMod, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(svc.configPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "enable-mod: NiceMod") {
		t.Fatalf("config = %q", data)
	}
	if !strings.Contains(string(data), "sound = 1") {
		t.Fatalf("unrelated line lost: %q", data)
	}
	backup := filepath.Join(gameData, "sm64config.modloader-backup.txt")
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("backup missing: %v", err)
	}

	if _, err := svc.Toggle("NiceMod", mods.KindMod, false); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(svc.configPath())
	if strings.Contains(string(data), "NiceMod") {
		t.Fatalf("mod not removed: %q", data)
	}
}

func TestToggleTurnsOffIncompatibleMods(t *testing.T) {
	svc, gameData := newTestService(t)
	addMod(t, gameData, "RomhackA", "-- name: Hack A\n-- incompatible: romhack\n")
	addMod(t, gameData, "RomhackB", "-- name: Hack B\n-- incompatible: romhack\n")

	if _, err := svc.Toggle("RomhackA", mods.KindMod, true); err != nil {
		t.Fatal(err)
	}
	disabled, err := svc.Toggle("RomhackB", mods.KindMod, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(disabled) != 1 || disabled[0] != "Hack A" {
		t.Fatalf("disabled = %v", disabled)
	}
	data, _ := os.ReadFile(svc.configPath())
	if strings.Contains(string(data), "RomhackA") || !strings.Contains(string(data), "RomhackB") {
		t.Fatalf("config = %q", data)
	}
}

func TestProfiles(t *testing.T) {
	svc, gameData := newTestService(t)
	addMod(t, gameData, "A", "-- name: A\n")
	addMod(t, gameData, "B", "-- name: B\n")

	if _, err := svc.Toggle("A", mods.KindMod, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.CreateProfile("OnlyA"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Toggle("A", mods.KindMod, false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Toggle("B", mods.KindMod, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.SwitchProfile("OnlyA"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(svc.configPath())
	if !strings.Contains(string(data), "enable-mod: A") || strings.Contains(string(data), "enable-mod: B") {
		t.Fatalf("config after switch = %q", data)
	}
	if err := svc.DeleteProfile("OnlyA"); err == nil {
		t.Fatal("deleting the active profile should fail")
	}
}

func TestRemoveKeepsFiles(t *testing.T) {
	svc, gameData := newTestService(t)
	addMod(t, gameData, "Gone", "-- name: Gone\n")
	if _, err := svc.Toggle("Gone", mods.KindMod, true); err != nil {
		t.Fatal(err)
	}
	moved, err := svc.Remove("Gone", mods.KindMod)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("removed copy missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(gameData, "mods", "Gone")); !os.IsNotExist(err) {
		t.Fatalf("original still there: %v", err)
	}
	data, _ := os.ReadFile(svc.configPath())
	if strings.Contains(string(data), "Gone") {
		t.Fatalf("config still enables it: %q", data)
	}
}

func TestOpenExternalOnlyHTTPS(t *testing.T) {
	svc, _ := newTestService(t)
	for _, raw := range []string{"http://example.com", "file:///etc/passwd", "javascript:alert(1)", ""} {
		if err := svc.OpenExternal(raw); err == nil {
			t.Errorf("OpenExternal(%q) should fail", raw)
		}
	}
}

func TestGameRunningBlocksWrites(t *testing.T) {
	svc, gameData := newTestService(t)
	addMod(t, gameData, "X", "-- name: X\n")
	svc.running = func() bool { return true }

	if _, err := svc.Toggle("X", mods.KindMod, true); err != ErrGameRunning {
		t.Fatalf("err = %v, want ErrGameRunning", err)
	}
}

func TestSetHostConfig(t *testing.T) {
	svc, _ := newTestService(t)
	writeConfig(t, svc, "sound = 1\r\nunknown setting stays\r\n")

	if err := svc.setHostConfig(3); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(svc.configPath())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"sound = 1\r\n",
		"unknown setting stays\r\n",
		"coop_host_save_slot 3\r\n",
		"coop_host_port 7777\r\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("config lacks %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "\r\n") || strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") {
		t.Errorf("line endings changed:\n%q", text)
	}
	if st := svc.Status(); st.HostSlot != 3 {
		t.Errorf("Status.HostSlot = %d", st.HostSlot)
	}
}
