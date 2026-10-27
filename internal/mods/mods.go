// Package mods discovers installed mods and DynOS packs and installs new ones.
package mods

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/retired64/coop-loader/internal/fsutil"
)

// Kind distinguishes a Lua mod from a DynOS model pack.
type Kind string

const (
	KindMod  Kind = "mod"
	KindPack Kind = "pack"
)

// Origin says where a mod lives. Only user mods can be changed or removed.
type Origin string

const (
	OriginUser    Origin = "user"
	OriginBuiltin Origin = "builtin"
)

// Mod is one entry shown in the Mods list.
type Mod struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Category     string `json:"category"`
	Incompatible string `json:"incompatible"`
	Kind         Kind   `json:"kind"`
	Origin       Origin `json:"origin"`
	Path         string `json:"path"`
	IsFolder     bool   `json:"isFolder"`
	Enabled      bool   `json:"enabled"`
}

// Paths are the folders the game reads mods from.
type Paths struct {
	GameDir  string
	GameData string
}

// UserMods is the writable mods folder.
func (p Paths) UserMods() string { return filepath.Join(p.GameData, "mods") }

// BuiltinMods is the folder next to the game executable.
func (p Paths) BuiltinMods() string {
	if p.GameDir == "" {
		return ""
	}
	return filepath.Join(p.GameDir, "mods")
}

// UserPacks is the writable DynOS pack folder.
func (p Paths) UserPacks() string { return filepath.Join(p.GameData, "dynos", "packs") }

// BuiltinPacks is the DynOS pack folder shipped with the game.
func (p Paths) BuiltinPacks() string {
	if p.GameDir == "" {
		return ""
	}
	return filepath.Join(p.GameDir, "dynos", "packs")
}

// Palettes is where palette .ini files are installed.
func (p Paths) Palettes() string { return filepath.Join(p.GameData, "palettes") }

// RemovedDir keeps things the loader took out of the game, instead of
// deleting them. The user can always put them back.
func (p Paths) RemovedDir() string {
	return filepath.Join(p.GameData, "removed-by-coop-loader")
}

// Meta is the header information a mod declares in main.lua comments.
type Meta struct {
	Name         string
	Description  string
	Category     string
	Incompatible string
}

var (
	colorCode = regexp.MustCompile(`\\#[0-9a-fA-F]{6}(?:[0-9a-fA-F]{2})?\\`)
	escaped   = regexp.MustCompile(`\\(\S)`)
	spaces    = regexp.MustCompile(`\s{2,}`)
)

// Clean strips the game's inline colour codes and escape sequences from a
// description string.
func Clean(s string) string {
	// Mod authors often double the backslash in comments ("\\#ff0000\\"),
	// so collapse it before looking for colour codes.
	s = strings.ReplaceAll(s, `\\`, `\`)
	s = colorCode.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, `\n`, " ")
	s = escaped.ReplaceAllString(s, "$1")
	s = spaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// ReadMeta parses the "-- name:", "-- description:", "-- category:" and
// "-- incompatible:" comments from the top of a Lua file.
func ReadMeta(luaPath string) Meta {
	f, err := os.Open(luaPath)
	if err != nil {
		return Meta{}
	}
	defer f.Close()

	buf := make([]byte, 16*1024)
	n, _ := io.ReadFull(f, buf)
	text := string(buf[:n])

	var meta Meta
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := comment(line)
		if !ok {
			continue
		}
		switch key {
		case "name":
			if meta.Name == "" {
				meta.Name = Clean(value)
			}
		case "description":
			if meta.Description == "" {
				meta.Description = Clean(value)
			}
		case "category":
			if meta.Category == "" {
				meta.Category = Clean(value)
			}
		case "incompatible":
			if meta.Incompatible == "" {
				meta.Incompatible = Clean(value)
			}
		}
	}
	return meta
}

// comment splits "-- key: value" (tolerating "--key: value").
func comment(line string) (key, value string, ok bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "--") {
		return "", "", false
	}
	t = strings.TrimSpace(strings.TrimPrefix(t, "--"))
	colon := strings.IndexByte(t, ':')
	if colon <= 0 {
		return "", "", false
	}
	key = strings.ToLower(strings.TrimSpace(t[:colon]))
	value = strings.TrimSpace(t[colon+1:])
	return key, value, true
}

// MainLua returns the mod entry point inside dir, if any.
func MainLua(dir string) (string, bool) {
	for _, name := range []string{"main.lua", "main.luac"} {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p, true
		}
	}
	return "", false
}

// List returns user and builtin mods and packs, with user entries winning
// when both have the same id.
func List(p Paths, enabledMods map[string]bool, enabledPacks map[string]bool) []Mod {
	user := listMods(p.UserMods(), OriginUser, enabledMods)
	builtin := listMods(p.BuiltinMods(), OriginBuiltin, enabledMods)
	mods := mergeByID(user, builtin)

	userPacks := listPacks(p.UserPacks(), OriginUser, enabledPacks)
	builtinPacks := listPacks(p.BuiltinPacks(), OriginBuiltin, enabledPacks)
	packs := mergeByID(userPacks, builtinPacks)

	out := append(mods, packs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == KindMod
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func listMods(dir string, origin Origin, enabled map[string]bool) []Mod {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Mod
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(dir, name)
		info, err := os.Stat(full) // follow symlinks: mod links are common
		if err != nil {
			continue
		}
		if info.IsDir() {
			entry, ok := MainLua(full)
			if !ok {
				continue
			}
			meta := ReadMeta(entry)
			display := meta.Name
			if display == "" {
				display = name
			}
			out = append(out, Mod{
				ID:           name,
				Name:         display,
				Description:  meta.Description,
				Category:     meta.Category,
				Incompatible: meta.Incompatible,
				Kind:         KindMod,
				Origin:       origin,
				Path:         full,
				IsFolder:     true,
				Enabled:      enabled[name],
			})
			continue
		}
		if !isLua(name) {
			continue
		}
		meta := ReadMeta(full)
		display := meta.Name
		if display == "" {
			display = strings.TrimSuffix(name, filepath.Ext(name))
		}
		out = append(out, Mod{
			ID:           name,
			Name:         display,
			Description:  meta.Description,
			Category:     meta.Category,
			Incompatible: meta.Incompatible,
			Kind:         KindMod,
			Origin:       origin,
			Path:         full,
			IsFolder:     false,
			Enabled:      enabled[name],
		})
	}
	return out
}

func listPacks(dir string, origin Origin, enabled map[string]bool) []Mod {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Mod
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		info, err := os.Stat(full) // follow symlinks
		if err != nil || !info.IsDir() {
			continue
		}
		out = append(out, Mod{
			ID:       e.Name(),
			Name:     e.Name(),
			Kind:     KindPack,
			Origin:   origin,
			Path:     full,
			IsFolder: true,
			Enabled:  enabled[e.Name()],
		})
	}
	return out
}

func mergeByID(primary, secondary []Mod) []Mod {
	seen := make(map[string]struct{}, len(primary))
	out := make([]Mod, 0, len(primary)+len(secondary))
	for _, m := range primary {
		seen[m.ID] = struct{}{}
		out = append(out, m)
	}
	for _, m := range secondary {
		if _, ok := seen[m.ID]; ok {
			continue
		}
		out = append(out, m)
	}
	return out
}

func isLua(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".lua" || ext == ".luac"
}

// ErrCompiled means the download contains bytecode, which can only run, not be
// reviewed. The caller must ask the user before installing it.
var ErrCompiled = errors.New("the mod contains compiled Lua")

// ErrNoMods means nothing installable was found in the archive.
var ErrNoMods = errors.New("no mod files found in this download")

// ErrTooBig means the archive unpacks to more than a mod could reasonably need.
var ErrTooBig = errors.New("this download unpacks to far more than a mod needs")

// ErrUnsupported means the file is neither Lua nor a zip.
var ErrUnsupported = errors.New("that is not a zip or a Lua file")

// isCompiled reports whether data starts with the Lua bytecode signature.
func isCompiled(data []byte) bool {
	return len(data) > 0 && data[0] == 0x1b
}

// Remove takes a user mod or pack out of the game and keeps it in the
// removed-by-coop-loader folder.
func Remove(p Paths, id string, kind Kind) (string, error) {
	if strings.TrimSpace(id) == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return "", fmt.Errorf("bad %s name", kind)
	}
	var base string
	if kind == KindPack {
		base = p.UserPacks()
	} else {
		base = p.UserMods()
	}
	target := filepath.Join(base, id)
	if !fsutil.Exists(target) {
		return "", fmt.Errorf("%s %q is not installed", kind, id)
	}
	return fsutil.MoveAside(target, filepath.Join(p.RemovedDir(), string(kind)))
}
