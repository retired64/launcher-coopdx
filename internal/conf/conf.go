// Package conf reads and edits sm64coopdx's sm64config.txt.
//
// The game has no database: the state of every mod lives in plain lines of
// this file. The loader changes only the lines it owns and preserves
// everything else byte for byte, including the line endings.
package conf

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

// Prefixes understood by the game.
const (
	ModPrefix      = "enable-mod:"
	PackPrefix     = "dynos-pack:"
	SaveNamePrefix = "save-name:"
)

// Doc is an editable sm64config.txt.
type Doc struct {
	eol   string
	lines []string
}

// Parse splits the config while remembering its line ending style.
func Parse(data []byte) *Doc {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // UTF-8 BOM
	eol := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		eol = "\r\n"
	}
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	return &Doc{eol: eol, lines: strings.Split(normalized, "\n")}
}

// Bytes renders the document with its original line endings.
func (d *Doc) Bytes() []byte {
	return []byte(strings.Join(d.lines, d.eol))
}

// Lines returns a copy of the current lines (without terminators).
func (d *Doc) Lines() []string {
	out := make([]string, len(d.lines))
	copy(out, d.lines)
	return out
}

// Mods lists the enabled mods in file order.
func (d *Doc) Mods() []string {
	var out []string
	for _, line := range d.lines {
		if name, ok := cutPrefix(line, ModPrefix); ok && name != "" {
			out = append(out, name)
		}
	}
	return out
}

// SetMod enables or disables one mod, keeping the rest of the block intact.
func (d *Doc) SetMod(name string, on bool) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "\r\n") {
		return
	}
	mods := d.Mods()
	next := make([]string, 0, len(mods)+1)
	for _, m := range mods {
		if m != name {
			next = append(next, m)
		}
	}
	if on {
		next = append(next, name)
	}
	d.SetMods(next)
}

// SetMods replaces the whole enable-mod block.
func (d *Doc) SetMods(names []string) {
	first := -1
	kept := make([]string, 0, len(d.lines))
	for _, line := range d.lines {
		if _, ok := cutPrefix(line, ModPrefix); ok {
			if first < 0 {
				first = len(kept)
			}
			continue
		}
		kept = append(kept, line)
	}
	if first < 0 {
		first = insertPoint(kept)
	}
	block := make([]string, 0, len(names))
	for _, name := range dedupe(names) {
		block = append(block, ModPrefix+" "+name)
	}
	d.lines = insertLines(kept, first, block)
}

// Packs returns the DynOS pack states (name -> on).
func (d *Doc) Packs() map[string]bool {
	out := map[string]bool{}
	for _, line := range d.lines {
		if rest, ok := cutPrefix(line, PackPrefix); ok {
			name, on := splitPack(rest)
			if name != "" {
				out[name] = on
			}
		}
	}
	return out
}

// SetPack enables or disables one DynOS pack.
func (d *Doc) SetPack(name string, on bool) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "\r\n") {
		return
	}
	packs := d.Packs()
	packs[name] = on
	d.SetPacks(packs)
}

// SetPacks replaces the whole dynos-pack block. Names are sorted so the file
// does not churn between writes.
func (d *Doc) SetPacks(packs map[string]bool) {
	first := -1
	kept := make([]string, 0, len(d.lines))
	for _, line := range d.lines {
		if _, ok := cutPrefix(line, PackPrefix); ok {
			if first < 0 {
				first = len(kept)
			}
			continue
		}
		kept = append(kept, line)
	}
	if first < 0 {
		first = insertPoint(kept)
	}
	names := make([]string, 0, len(packs))
	for name := range packs {
		names = append(names, name)
	}
	sort.Strings(names)
	block := make([]string, 0, len(names))
	for _, name := range names {
		state := "false"
		if packs[name] {
			state = "true"
		}
		block = append(block, PackPrefix+" "+name+" "+state)
	}
	d.lines = insertLines(kept, first, block)
}

// Value returns the first plain "key value" line for key.
func (d *Doc) Value(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	prefix := key + " "
	for _, line := range d.lines {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}

// SetValue sets a plain "key value" line, like the reference client's writes
// of coop_host_save_slot and coop_host_port. Unknown lines are preserved.
func (d *Doc) SetValue(key, value string) {
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, " \t\r\n") {
		return
	}
	value = strings.TrimSpace(strings.ReplaceAll(value, "\n", " "))
	found := false
	for i, line := range d.lines {
		name := line
		if j := strings.IndexByte(line, ' '); j >= 0 {
			name = line[:j]
		}
		if name == key && !found {
			d.lines[i] = key + " " + value
			found = true
		}
	}
	if found {
		return
	}
	at := insertPoint(d.lines)
	d.lines = insertLines(d.lines, at, []string{key + " " + value})
}

// SaveNames returns the four slot names ("", "A", ...) in slot order.
func (d *Doc) SaveNames() [4]string {
	var out [4]string
	for _, line := range d.lines {
		rest, ok := cutPrefix(line, SaveNamePrefix)
		if !ok {
			continue
		}
		slot, name, ok := splitSaveName(rest)
		if !ok {
			continue
		}
		out[slot] = name
	}
	return out
}

// SetSaveName sets one slot name. An empty name removes the line.
func (d *Doc) SetSaveName(slot int, name string) {
	if slot < 0 || slot > 3 {
		return
	}
	name = strings.TrimSpace(strings.ReplaceAll(name, "\n", " "))
	first := -1
	kept := make([]string, 0, len(d.lines))
	for _, line := range d.lines {
		rest, ok := cutPrefix(line, SaveNamePrefix)
		if ok {
			s, _, valid := splitSaveName(rest)
			if valid && s == slot {
				if first < 0 {
					first = len(kept)
				}
				continue
			}
		}
		kept = append(kept, line)
	}
	if name == "" {
		d.lines = kept
		return
	}
	if first < 0 {
		first = insertPoint(kept)
	}
	d.lines = insertLines(kept, first, []string{SaveNamePrefix + " " + strconv.Itoa(slot) + " " + name})
}

func cutPrefix(line, prefix string) (string, bool) {
	if strings.HasPrefix(line, prefix) {
		return strings.TrimSpace(line[len(prefix):]), true
	}
	return "", false
}

func splitPack(rest string) (string, bool) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", false
	}
	if i := strings.LastIndexByte(rest, ' '); i > 0 {
		switch rest[i+1:] {
		case "true":
			return strings.TrimSpace(rest[:i]), true
		case "false":
			return strings.TrimSpace(rest[:i]), false
		}
	}
	return rest, rest != "false"
}

func splitSaveName(rest string) (int, string, bool) {
	space := strings.IndexByte(rest, ' ')
	if space < 1 {
		return 0, "", false
	}
	slot, err := strconv.Atoi(rest[:space])
	if err != nil || slot < 0 || slot > 3 {
		return 0, "", false
	}
	return slot, strings.TrimSpace(rest[space+1:]), true
}

// insertPoint is where a new block goes when the file has none yet: just
// before the final empty line, so the file keeps its trailing newline.
func insertPoint(lines []string) int {
	if n := len(lines); n > 0 && lines[n-1] == "" {
		return n - 1
	}
	return len(lines)
}

func insertLines(lines []string, at int, add []string) []string {
	if len(add) == 0 {
		return lines
	}
	if at < 0 || at > len(lines) {
		at = len(lines)
	}
	out := make([]string, 0, len(lines)+len(add))
	out = append(out, lines[:at]...)
	out = append(out, add...)
	out = append(out, lines[at:]...)
	return out
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
