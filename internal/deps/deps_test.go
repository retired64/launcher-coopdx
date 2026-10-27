package deps

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestScanSeparatesCommentsStringsAndCode(t *testing.T) {
	src := []byte(`
--[[ _G.ignored = 1 ]]
-- _G.commented = 1
local s = "_G.inString = 1"
local long = [[ _G.inLongString = 1 ]]

if not _G.characterSelectExists then
    print("need it")
    return
end
if _G.otherApi == nil then return end
if nil == _G.third then return end
if _G["fourth"] == nil then return end

_G.myGlobal = function() end
_G["providedTwo"] = 1
if _G.something == nil then print("just a check") end
`)

	res := Scan(src)
	if _, ok := res.Provided["myGlobal"]; !ok {
		t.Errorf("myGlobal not provided: %v", res.Provided)
	}
	if _, ok := res.Provided["providedTwo"]; !ok {
		t.Errorf("providedTwo not provided: %v", res.Provided)
	}
	for _, bad := range []string{"ignored", "commented", "inString", "inLongString", "something"} {
		if _, ok := res.Provided[bad]; ok {
			t.Errorf("%s should not be provided", bad)
		}
	}

	want := []string{"characterSelectExists", "fourth", "otherApi", "third"}
	if len(res.Required) != len(want) {
		t.Fatalf("required = %v", res.Required)
	}
	for i := range want {
		if res.Required[i] != want[i] {
			t.Fatalf("required = %v, want %v", res.Required, want)
		}
	}
}

func TestScanIgnoresOwnGlobalChecks(t *testing.T) {
	res := Scan([]byte(`
_G.lib = {}
if not _G.lib then return end
`))
	if len(res.Required) != 0 {
		t.Fatalf("required = %v", res.Required)
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		global, title string
		want          bool
	}{
		{"characterSelectExists", "Character Select", true},
		{"charSelect", "Character Select", true},
		{"hudAPI", "HUD", true},
		{"weatherCycleVersion", "Weather Cycle DX", true},
		{"ommApi", "OMM Rebirth", true},
		{"marioWings", "Luigi", false},
		{"", "Anything", false},
		{"abc", "", false},
	}
	for _, c := range cases {
		if got := Match(c.global, c.title); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.global, c.title, got, c.want)
		}
	}
}

func TestHumanName(t *testing.T) {
	if got := HumanName("characterSelectExists"); got != "character select" {
		t.Fatalf("got %q", got)
	}
	if got := HumanName("hudAPI"); got != "hud" {
		t.Fatalf("got %q", got)
	}
}

func TestScanDirAndIndex(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "GoodMod")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "main.lua"), []byte("_G.goodApi = {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	disabled := filepath.Join(root, "DisabledMod")
	if err := os.MkdirAll(disabled, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(disabled, "main.lua"), []byte("_G.disabledApi = {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	index := Index([]Source{
		{ID: "good", Name: "Good Mod", Dir: good, IsFolder: true, Enabled: true},
		{ID: "disabled", Name: "Disabled Mod", Dir: disabled, IsFolder: true, Enabled: false},
	})
	if len(index["goodApi"]) != 1 || !index["goodApi"][0].Enabled {
		t.Fatalf("goodApi = %+v", index["goodApi"])
	}
	if len(index["disabledApi"]) != 1 || index["disabledApi"][0].Enabled {
		t.Fatalf("disabledApi = %+v", index["disabledApi"])
	}
}

func TestScanZip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mod.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("mods/Test/main.lua")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(`
_G.testApi = {}
if not _G.helperApi then return end
`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := ScanZip(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Provided["testApi"]; !ok {
		t.Fatalf("provided = %v", res.Provided)
	}
	if len(res.Required) != 1 || res.Required[0] != "helperApi" {
		t.Fatalf("required = %v", res.Required)
	}
}
