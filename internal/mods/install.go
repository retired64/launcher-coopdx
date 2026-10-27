package mods

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/retired64/coop-loader/internal/fsutil"
)

const (
	maxEntries      = 20_000
	maxUncompressed = 2 << 30 // 2 GiB, same ceiling the reference loader uses
)

// unit is one thing to install: a mod folder, a pack folder or a palette.
type unit struct {
	kind   Kind
	name   string
	from   string // directory prefix inside the zip, or one entry when single
	single bool
}

// InstallAny installs a local .zip, .lua or .luac file.
func InstallAny(p Paths, file string, allowCompiled bool) ([]string, error) {
	switch strings.ToLower(filepath.Ext(file)) {
	case ".zip":
		return InstallZip(p, file, allowCompiled)
	case ".lua", ".luac":
		return InstallLua(p, file, allowCompiled)
	default:
		return nil, ErrUnsupported
	}
}

// InstallZip routes the contents of a zip into mods, DynOS packs and palettes.
//
// The archive is never extracted blindly: entries are sanitised, sizes are
// bounded and each unit is written to its final folder only after the whole
// plan is known.
func InstallZip(p Paths, zipPath string, allowCompiled bool) ([]string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()

	byName := make(map[string]*zip.File)
	names := make([]string, 0, len(zr.File))
	var total uint64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name, ok := cleanEntryName(f.Name)
		if !ok {
			continue
		}
		if _, dup := byName[name]; dup {
			return nil, fmt.Errorf("the zip contains %q twice", name)
		}
		byName[name] = f
		names = append(names, name)
		total += f.UncompressedSize64
		if len(names) > maxEntries {
			return nil, ErrTooBig
		}
		if total > maxUncompressed {
			return nil, ErrTooBig
		}
	}
	if len(names) == 0 {
		return nil, ErrNoMods
	}
	if !allowCompiled {
		for _, name := range names {
			if !strings.HasSuffix(strings.ToLower(name), ".lua") {
				continue
			}
			compiled, err := startsWithBytecode(byName[name])
			if err != nil {
				return nil, err
			}
			if compiled {
				return nil, ErrCompiled
			}
		}
	}

	units := plan(names, archiveBase(zipPath))
	if len(units) == 0 {
		return nil, ErrNoMods
	}
	if err := checkUnits(units); err != nil {
		return nil, err
	}

	sort.Strings(names)
	budget := int64(maxUncompressed)
	installed := make([]string, 0, len(units))
	for _, u := range units {
		dest := target(p, u)
		if fsutil.Exists(dest) {
			if _, err := fsutil.MoveAside(dest, filepath.Join(p.RemovedDir(), string(u.kind))); err != nil {
				return installed, fmt.Errorf("move old %s aside: %w", u.name, err)
			}
		}
		written, err := extractUnit(byName, names, u, dest, &budget)
		if err != nil {
			return installed, err
		}
		if written == 0 {
			_ = os.RemoveAll(dest)
			continue
		}
		installed = append(installed, u.name)
	}
	if len(installed) == 0 {
		return nil, ErrNoMods
	}
	return installed, nil
}

// InstallLua copies a single Lua mod into the user mods folder.
func InstallLua(p Paths, file string, allowCompiled bool) ([]string, error) {
	name := filepath.Base(file)
	if _, ok := safeName(name); !ok {
		return nil, fmt.Errorf("%q is not a name a mod can have", name)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	if isCompiled(data) && !allowCompiled {
		return nil, ErrCompiled
	}
	if err := os.MkdirAll(p.UserMods(), 0o755); err != nil {
		return nil, err
	}
	dest := filepath.Join(p.UserMods(), name)
	if fsutil.Exists(dest) {
		if _, err := fsutil.MoveAside(dest, filepath.Join(p.RemovedDir(), string(KindMod))); err != nil {
			return nil, err
		}
	}
	if err := fsutil.WriteFileAtomic(dest, data, 0o644); err != nil {
		return nil, err
	}
	return []string{name}, nil
}

// plan mirrors how the game expects a download to be laid out.
func plan(names []string, base string) []unit {
	mods := map[string]bool{}
	packs := map[string]bool{}
	palettes := map[string]bool{}

	for _, name := range names {
		parts := strings.Split(name, "/")
		for i := 0; i+1 < len(parts) && i < 3; i++ {
			switch strings.ToLower(parts[i]) {
			case "mods":
				mods[strings.Join(parts[:i+1], "/")] = true
			case "dynos":
				prefix := strings.Join(parts[:i+1], "/")
				if i+2 < len(parts) && strings.EqualFold(parts[i+1], "packs") {
					prefix = strings.Join(parts[:i+2], "/")
				}
				packs[prefix] = true
			case "palettes":
				palettes[strings.Join(parts[:i+1], "/")] = true
			}
		}
	}

	var out []unit
	for _, prefix := range sortedKeys(mods) {
		out = append(out, childrenOf(names, prefix, KindMod)...)
	}
	for _, prefix := range sortedKeys(packs) {
		out = append(out, childrenOf(names, prefix, KindPack)...)
	}
	for _, prefix := range sortedKeys(palettes) {
		out = append(out, childrenOf(names, prefix, "palette")...)
	}
	if len(out) > 0 {
		return out
	}

	// Next: folders that contain main.lua / main.luac.
	var mains []string
	for _, name := range names {
		lower := strings.ToLower(name)
		if lower == "main.lua" || lower == "main.luac" ||
			strings.HasSuffix(lower, "/main.lua") || strings.HasSuffix(lower, "/main.luac") {
			dir := path.Dir(name)
			if dir == "." {
				dir = ""
			}
			mains = append(mains, dir)
		}
	}
	mains = topmost(mains)
	if len(mains) > 0 {
		for _, dir := range mains {
			name := base
			if dir != "" {
				name = path.Base(dir)
			}
			if safe, ok := safeName(name); ok {
				out = append(out, unit{kind: KindMod, name: safe, from: dir})
			}
		}
		return out
	}

	// Next: loose .lua/.luac files become one mod each.
	for _, name := range names {
		if !isLua(name) {
			continue
		}
		if safe, ok := safeName(path.Base(name)); ok {
			out = append(out, unit{kind: KindMod, name: safe, from: name, single: true})
		}
	}
	if len(out) > 0 {
		return out
	}

	// Last: model files at the root become a single DynOS pack.
	for _, name := range names {
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, ".bin") || strings.HasSuffix(lower, ".tex") {
			if safe, ok := safeName(base); ok {
				out = append(out, unit{kind: KindPack, name: safe, from: ""})
			}
			break
		}
	}
	return out
}

// childrenOf turns every immediate child of prefix into a unit.
func childrenOf(names []string, prefix string, kind Kind) []unit {
	seen := map[string]bool{}
	var out []unit
	for _, name := range names {
		if !strings.HasPrefix(name, prefix+"/") {
			continue
		}
		child := strings.SplitN(strings.TrimPrefix(name, prefix+"/"), "/", 2)[0]
		if child == "" || seen[child] {
			continue
		}
		seen[child] = true
		safe, ok := safeName(child)
		if !ok {
			continue
		}
		from := prefix + "/" + child
		out = append(out, unit{
			kind:   kind,
			name:   safe,
			from:   from,
			single: name == from, // the child is a file, not a folder
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func target(p Paths, u unit) string {
	switch u.kind {
	case KindPack:
		return filepath.Join(p.UserPacks(), u.name)
	case "palette":
		return filepath.Join(p.Palettes(), u.name)
	default:
		return filepath.Join(p.UserMods(), u.name)
	}
}

func extractUnit(byName map[string]*zip.File, names []string, u unit, dest string, budget *int64) (int, error) {
	written := 0
	if u.single {
		f := byName[u.from]
		if f == nil {
			return 0, nil
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return 0, err
		}
		if err := writeZipFile(f, dest, budget); err != nil {
			return written, err
		}
		return 1, nil
	}

	for _, name := range names {
		var rel string
		if u.from == "" {
			rel = name
		} else if strings.HasPrefix(name, u.from+"/") {
			rel = strings.TrimPrefix(name, u.from+"/")
		} else {
			continue
		}
		out, err := fsutil.SafeJoin(dest, rel)
		if err != nil {
			return written, err
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return written, err
		}
		if err := writeZipFile(byName[name], out, budget); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

func writeZipFile(f *zip.File, dest string, budget *int64) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	n, err := io.Copy(out, io.LimitReader(rc, *budget+1))
	if err != nil {
		return err
	}
	*budget -= n
	if *budget < 0 {
		return ErrTooBig
	}
	return out.Sync()
}

func startsWithBytecode(f *zip.File) (bool, error) {
	rc, err := f.Open()
	if err != nil {
		return false, err
	}
	defer rc.Close()
	var b [1]byte
	n, _ := io.ReadFull(rc, b[:])
	return n == 1 && b[0] == 0x1b, nil
}

// cleanEntryName rejects absolute paths, traversal, drive letters and the
// macOS metadata folder.
func cleanEntryName(name string) (string, bool) {
	n := strings.ReplaceAll(name, "\\", "/")
	n = strings.TrimPrefix(n, "./")
	if n == "" || strings.HasPrefix(n, "/") || strings.ContainsRune(n, 0) {
		return "", false
	}
	if strings.HasPrefix(strings.ToLower(n), "__macosx/") {
		return "", false
	}
	parts := strings.Split(n, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	if len(parts[0]) >= 2 && parts[0][1] == ':' {
		return "", false
	}
	return n, true
}

// safeName validates the name of a folder created inside the game data.
func safeName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\<>:"|?*`) ||
		strings.ContainsRune(name, 0) ||
		strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return "", false
	}
	stem := strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))
	switch stem {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return "", false
	}
	return name, true
}

func checkUnits(units []unit) error {
	seen := map[string]bool{}
	for _, u := range units {
		key := string(u.kind) + ":" + strings.ToLower(u.name)
		if seen[key] {
			return fmt.Errorf("the zip contains two items called %q", u.name)
		}
		seen[key] = true
	}
	return nil
}

func topmost(dirs []string) []string {
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) < len(dirs[j]) })
	var out []string
	for _, dir := range dirs {
		nested := false
		for _, kept := range out {
			if dir == kept || kept == "" || strings.HasPrefix(dir, kept+"/") {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out
}

func archiveBase(file string) string {
	base := filepath.Base(file)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	base = strings.TrimSpace(base)
	if base == "" {
		return "mod"
	}
	return base
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
