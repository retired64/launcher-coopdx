package mods

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func writeZip(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallZipWithModsFolder(t *testing.T) {
	data := t.TempDir()
	zipPath := filepath.Join(data, "Cool Mod.zip")
	writeZip(t, zipPath, map[string][]byte{
		"mods/CoolMod/main.lua":  []byte("-- name: Cool\n"),
		"mods/CoolMod/extra.txt": []byte("x"),
	})

	p := Paths{GameData: data}
	installed, err := InstallZip(p, zipPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 || installed[0] != "CoolMod" {
		t.Fatalf("installed = %v", installed)
	}
	if _, err := os.Stat(filepath.Join(data, "mods", "CoolMod", "main.lua")); err != nil {
		t.Fatalf("main.lua missing: %v", err)
	}
}

func TestInstallZipRootMainLuaUsesArchiveName(t *testing.T) {
	data := t.TempDir()
	zipPath := filepath.Join(data, "Neat Mod.zip")
	writeZip(t, zipPath, map[string][]byte{
		"main.lua": []byte("-- name: Neat\n"),
		"assets/a": []byte("a"),
	})

	p := Paths{GameData: data}
	installed, err := InstallZip(p, zipPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 || installed[0] != "Neat Mod" {
		t.Fatalf("installed = %v", installed)
	}
}

func TestInstallZipRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(root, "evil.zip")
	writeZip(t, zipPath, map[string][]byte{
		"mods/Good/main.lua": []byte("ok"),
		"../../evil.txt":     []byte("nope"),
		"mods/../evil2.txt":  []byte("nope"),
	})

	p := Paths{GameData: data}
	if _, err := InstallZip(p, zipPath, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "evil.txt")); !os.IsNotExist(err) {
		t.Fatalf("traversal escaped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "evil2.txt")); !os.IsNotExist(err) {
		t.Fatalf("traversal escaped into data: %v", err)
	}
}

func TestInstallZipCompiled(t *testing.T) {
	data := t.TempDir()
	zipPath := filepath.Join(data, "bytecode.zip")
	writeZip(t, zipPath, map[string][]byte{
		"mods/Byte/main.lua": {0x1b, 'L', 'u', 'a'},
	})

	p := Paths{GameData: data}
	if _, err := InstallZip(p, zipPath, false); err != ErrCompiled {
		t.Fatalf("err = %v, want ErrCompiled", err)
	}
	if _, err := InstallZip(p, zipPath, true); err != nil {
		t.Fatalf("allowCompiled: %v", err)
	}
}

func TestInstallZipDynosPack(t *testing.T) {
	data := t.TempDir()
	zipPath := filepath.Join(data, "model.zip")
	writeZip(t, zipPath, map[string][]byte{
		"dynos/packs/MyModel/actors/mario.bin": []byte("bin"),
		"dynos/packs/MyModel/actors/mario.tex": []byte("tex"),
	})

	p := Paths{GameData: data}
	installed, err := InstallZip(p, zipPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 || installed[0] != "MyModel" {
		t.Fatalf("installed = %v", installed)
	}
	if _, err := os.Stat(filepath.Join(data, "dynos", "packs", "MyModel", "actors", "mario.bin")); err != nil {
		t.Fatal(err)
	}
}

func TestInstallZipLooseLua(t *testing.T) {
	data := t.TempDir()
	zipPath := filepath.Join(data, "loose.zip")
	writeZip(t, zipPath, map[string][]byte{
		"One.lua": []byte("-- name: One\n"),
		"Two.lua": []byte("-- name: Two\n"),
	})

	p := Paths{GameData: data}
	installed, err := InstallZip(p, zipPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 2 {
		t.Fatalf("installed = %v", installed)
	}
}
