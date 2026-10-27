package conf

import (
	"strings"
	"testing"
)

func TestSetModsKeepsCRLFAndOtherLines(t *testing.T) {
	in := "coopnet_ip = net.coop64.us\r\nenable-mod: old\r\nsound = 1\r\n\r\n"
	doc := Parse([]byte(in))
	doc.SetMods([]string{"a", "b", "a"})

	want := "coopnet_ip = net.coop64.us\r\nenable-mod: a\r\nenable-mod: b\r\nsound = 1\r\n\r\n"
	if got := string(doc.Bytes()); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestToggleModOffRemovesLine(t *testing.T) {
	doc := Parse([]byte("enable-mod: a\nenable-mod: b\n"))
	doc.SetMod("a", false)

	if got := doc.Mods(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("mods = %v", got)
	}
	if got := string(doc.Bytes()); got != "enable-mod: b\n" {
		t.Fatalf("bytes = %q", got)
	}
}

func TestSetModOnWithoutConfigTail(t *testing.T) {
	doc := Parse([]byte("a=1"))
	doc.SetMod("m", true)

	if got := string(doc.Bytes()); got != "a=1\nenable-mod: m" {
		t.Fatalf("bytes = %q", got)
	}
}

func TestPacks(t *testing.T) {
	doc := Parse([]byte("dynos-pack: X true\ndynos-pack: Y false\n"))
	doc.SetPack("Z", true)

	packs := doc.Packs()
	if !packs["X"] || packs["Y"] || !packs["Z"] {
		t.Fatalf("packs = %v", packs)
	}
	want := "dynos-pack: X true\ndynos-pack: Y false\ndynos-pack: Z true\n"
	if got := string(doc.Bytes()); got != want {
		t.Fatalf("bytes = %q\nwant %q", got, want)
	}
}

func TestSaveNames(t *testing.T) {
	doc := Parse([]byte("save-name: 1 Luigi\nsave-name: 3 Wario\n"))
	names := doc.SaveNames()
	if names[1] != "Luigi" || names[3] != "Wario" || names[0] != "" {
		t.Fatalf("names = %v", names)
	}

	doc.SetSaveName(0, "Mario")
	doc.SetSaveName(1, "")
	if got := doc.SaveNames(); got[0] != "Mario" || got[1] != "" {
		t.Fatalf("after edit = %v", got)
	}
}

func TestSetValuePreservesEverythingElse(t *testing.T) {
	doc := Parse([]byte("sound = 1\r\ncoop_host_save_slot 2\r\ncustom line here\r\n"))
	doc.SetValue("coop_host_save_slot", "3")
	doc.SetValue("coop_host_port", "7777")
	got := string(doc.Bytes())
	want := "sound = 1\r\ncoop_host_save_slot 3\r\ncustom line here\r\ncoop_host_port 7777\r\n"
	if got != want {
		t.Fatalf("bytes = %q\nwant %q", got, want)
	}

	if got := doc.Value("coop_host_save_slot"); got != "3" {
		t.Fatalf("Value = %q", got)
	}
	if got := doc.Value("missing"); got != "" {
		t.Fatalf("missing Value = %q", got)
	}

	// The first occurrence is replaced and the key is not appended twice.
	doc.SetValue("coop_host_port", "7778")
	if strings.Count(string(doc.Bytes()), "coop_host_port") != 1 {
		t.Fatalf("duplicate key: %q", doc.Bytes())
	}
	if !strings.Contains(string(doc.Bytes()), "coop_host_port 7778") {
		t.Fatalf("value not replaced: %q", doc.Bytes())
	}
}
