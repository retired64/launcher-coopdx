package mods

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadMeta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.lua")
	body := "-- name: My \\#ff0000\\Mod\n" +
		"-- description: Hello\\nworld with  spaces\n" +
		"-- category: gamemode\n" +
		"-- incompatible: romhack gamemode\n" +
		"print('hi')\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	meta := ReadMeta(path)
	if meta.Name != "My Mod" {
		t.Fatalf("name = %q", meta.Name)
	}
	if meta.Description != "Hello world with spaces" {
		t.Fatalf("description = %q", meta.Description)
	}
	if meta.Category != "gamemode" {
		t.Fatalf("category = %q", meta.Category)
	}
	if meta.Incompatible != "romhack gamemode" {
		t.Fatalf("incompatible = %q", meta.Incompatible)
	}
}

func TestClean(t *testing.T) {
	cases := map[string]string{
		`a \#00ff00\b`:    "a b",
		`one\ntwo`:        "one two",
		`escaped\*star`:   "escaped*star",
		"lots   of space": "lots of space",
	}
	for in, want := range cases {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}
