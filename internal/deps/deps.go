// Package deps finds which Lua globals a mod needs and which mods provide
// them.
//
// sm64coopdx mods talk to each other through globals (`_G.Foo`). A mod that
// depends on another usually checks the global and bails out:
//
//	if not _G.characterSelectExists then return end
//
// This package scans source without running it, so the loader can offer the
// missing mod instead of letting the game fail at load time.
package deps

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Result is the outcome of scanning one mod.
type Result struct {
	Provided map[string]struct{}
	Required []string
}

// Provider is an installed mod that defines a global.
type Provider struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// Source describes an installed mod on disk.
type Source struct {
	ID       string
	Name     string
	Dir      string
	IsFolder bool
	Enabled  bool
}

const (
	maxFile  = 4 << 20
	maxTotal = 64 << 20
	maxFiles = 600
)

var (
	assignDot = regexp.MustCompile(`_G\s*\.\s*([A-Za-z_]\w*)\s*=`)
	assignIdx = regexp.MustCompile(`_G\s*\[\s*["']([A-Za-z_]\w*)["']\s*\]\s*=`)

	reqNotDot  = regexp.MustCompile(`\bnot\s+_G\s*\.\s*([A-Za-z_]\w*)`)
	reqNotIdx  = regexp.MustCompile(`\bnot\s+_G\s*\[\s*["']([A-Za-z_]\w*)["']\s*\]`)
	reqNilDot  = regexp.MustCompile(`_G\s*\.\s*([A-Za-z_]\w*)\s*==\s*nil`)
	reqNil2Dot = regexp.MustCompile(`nil\s*==\s*_G\s*\.\s*([A-Za-z_]\w*)`)
	reqNilIdx  = regexp.MustCompile(`_G\s*\[\s*["']([A-Za-z_]\w*)["']\s*\]\s*==\s*nil`)
	reqNil2Idx = regexp.MustCompile(`nil\s*==\s*_G\s*\[\s*["']([A-Za-z_]\w*)["']\s*\]`)
)

// Scan analyzes one Lua source.
func Scan(src []byte) Result {
	code, inside := analyze(src)
	provided := map[string]struct{}{}

	for _, re := range []*regexp.Regexp{assignDot, assignIdx} {
		for _, m := range re.FindAllSubmatchIndex(code, -1) {
			if inside[m[0]] {
				continue
			}
			// Skip comparisons: "_G.X == nil" also matches "...X =".
			if m[1] < len(code) && code[m[1]] == '=' {
				continue
			}
			provided[string(code[m[2]:m[3]])] = struct{}{}
		}
	}

	required := map[string]struct{}{}
	for _, re := range []*regexp.Regexp{reqNotDot, reqNotIdx, reqNilDot, reqNil2Dot, reqNilIdx, reqNil2Idx} {
		for _, m := range re.FindAllSubmatchIndex(code, -1) {
			if inside[m[0]] {
				continue
			}
			name := string(code[m[2]:m[3]])
			if _, own := provided[name]; own {
				continue
			}
			if !ifBodyStops(code, m[1]) {
				continue
			}
			required[name] = struct{}{}
		}
	}

	out := Result{Provided: provided, Required: make([]string, 0, len(required))}
	for name := range required {
		out.Required = append(out.Required, name)
	}
	sort.Strings(out.Required)
	return out
}

// ifBodyStops reports whether the "if" that contains this check returns or
// tells the player. That is the signal mods use to say "I cannot run without X".
func ifBodyStops(code []byte, start int) bool {
	window := code[start:]
	if len(window) > 300 {
		window = window[:300]
	}
	idx := bytes.Index(window, []byte("then"))
	if idx < 0 {
		return false
	}
	body := code[start+idx+4:]
	depth := 1
	for i := 0; i < len(body) && i < 4000; {
		for i < len(body) && !isIdentByte(body[i]) {
			i++
		}
		j := i
		for j < len(body) && isIdentByte(body[j]) {
			j++
		}
		if i == j {
			break
		}
		switch string(body[i:j]) {
		case "if", "function", "do", "repeat":
			depth++
		case "end", "until":
			depth--
			if depth == 0 {
				return false
			}
		case "return":
			if depth == 1 {
				return true
			}
		case "error", "log_to_console", "djui_popup_create", "djui_chat_message_create":
			if depth == 1 {
				return true
			}
		}
		i = j
	}
	return false
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// ScanLuaFile reads and scans one .lua file.
func ScanLuaFile(path string) (Result, error) {
	data, err := readCapped(path, maxFile)
	if err != nil {
		return Result{}, err
	}
	return Scan(data), nil
}

// ScanZip scans every Lua source inside a zip.
func ScanZip(path string) (Result, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return Result{}, err
	}
	defer zr.Close()

	out := Result{Provided: map[string]struct{}{}}
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(f.Name), ".lua") {
			continue
		}
		if total += int64(f.UncompressedSize64); total > maxTotal {
			break
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		data, err := readLimited(rc, maxFile)
		rc.Close()
		if err != nil || len(data) == 0 || data[0] == 0x1b {
			continue
		}
		merge(&out, Scan(data))
	}
	return out, nil
}

// ScanDir scans the Lua files of a mod folder (up to four levels deep).
func ScanDir(dir string) (Result, error) {
	out := Result{Provided: map[string]struct{}{}}
	root, err := filepath.Abs(dir)
	if err != nil {
		return out, err
	}
	var files int
	var total int64
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // ignore unreadable entries
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			rel, err := filepath.Rel(root, path)
			if err == nil && rel != "." && strings.Count(rel, string(filepath.Separator)) >= 4 {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".lua") {
			return nil
		}
		if files >= maxFiles || total >= maxTotal {
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxFile {
			return nil
		}
		files++
		total += info.Size()
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		merge(&out, Scan(data))
		return nil
	})
	return out, err
}

// ScanPath scans a zip, a Lua file or a folder.
func ScanPath(path string) (Result, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Result{}, err
	}
	if info.IsDir() {
		return ScanDir(path)
	}
	if strings.EqualFold(filepath.Ext(path), ".zip") {
		return ScanZip(path)
	}
	return ScanLuaFile(path)
}

// Index maps every provided global to the installed mods that define it.
func Index(sources []Source) map[string][]Provider {
	index := map[string][]Provider{}
	for _, src := range sources {
		var res Result
		var err error
		if src.IsFolder {
			res, err = ScanDir(src.Dir)
		} else {
			res, err = ScanLuaFile(src.Dir)
		}
		if err != nil {
			continue
		}
		for name := range res.Provided {
			index[name] = append(index[name], Provider{ID: src.ID, Name: src.Name, Enabled: src.Enabled})
		}
	}
	for name := range index {
		list := index[name]
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Enabled != list[j].Enabled {
				return list[i].Enabled
			}
			return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
		})
		index[name] = list
	}
	return index
}

func merge(dst *Result, src Result) {
	if dst.Provided == nil {
		dst.Provided = map[string]struct{}{}
	}
	for name := range src.Provided {
		dst.Provided[name] = struct{}{}
	}
	dst.Required = append(dst.Required, src.Required...)
	sort.Strings(dst.Required)
	dst.Required = dedupe(dst.Required)
}

func dedupe(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readLimited(f, limit)
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file too big to scan")
	}
	return data, nil
}

// analyze returns the source with comments blanked and a mask marking every
// byte that belongs to a comment or a string. String contents are kept so that
// _G["Name"] can still be parsed; the mask is what keeps literals harmless.
func analyze(src []byte) (code []byte, inside []bool) {
	code = make([]byte, 0, len(src))
	inside = make([]bool, 0, len(src))
	put := func(b byte, literal bool) {
		code = append(code, b)
		inside = append(inside, literal)
	}

	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			if i+2 < len(src) && src[i+2] == '[' {
				if end, ok := longBracket(src, i+2); ok {
					for ; i < end; i++ {
						if src[i] == '\n' {
							put('\n', true)
						} else {
							put(' ', true)
						}
					}
					continue
				}
			}
			for i < len(src) && src[i] != '\n' {
				put(' ', true)
				i++
			}
		case c == '"' || c == '\'':
			quote := c
			put(c, true)
			i++
			for i < len(src) {
				if src[i] == '\\' {
					put(src[i], true)
					i++
					if i < len(src) {
						put(src[i], true)
						i++
					}
					continue
				}
				put(src[i], true)
				if src[i] == quote {
					i++
					break
				}
				if src[i] == '\n' {
					i++
					break
				}
				i++
			}
		case c == '[':
			if end, ok := longBracket(src, i); ok {
				for ; i < end; i++ {
					if src[i] == '\n' {
						put('\n', true)
					} else {
						put(' ', true)
					}
				}
				continue
			}
			put(c, false)
			i++
		default:
			put(c, false)
			i++
		}
	}
	return code, inside
}

func longBracket(src []byte, i int) (int, bool) {
	if i >= len(src) || src[i] != '[' {
		return 0, false
	}
	j := i + 1
	level := 0
	for j < len(src) && src[j] == '=' {
		level++
		j++
	}
	if j >= len(src) || src[j] != '[' {
		return 0, false
	}
	close := "]" + strings.Repeat("=", level) + "]"
	idx := bytes.Index(src[j+1:], []byte(close))
	if idx < 0 {
		return len(src), true
	}
	return j + 1 + idx + len(close), true
}

// --- matching a global to a mod title --------------------------------------

var noiseTokens = map[string]bool{
	"exists": true, "api": true, "enabled": true, "loaded": true,
	"on": true, "active": true, "installed": true, "version": true,
	"lib": true, "mod": true, "main": true, "data": true, "global": true,
	"is": true, "has": true, "ready": true,
}

// Match reports whether a global name plausibly belongs to a mod title.
//
//	characterSelectExists  ~  Character Select
//	charSelect             ~  Character Select
//	hudAPI                 ~  HUD
func Match(global, title string) bool {
	g := normalize(global)
	t := normalize(title)
	if g == "" || t == "" {
		return false
	}
	if g == t || strings.HasPrefix(g, t) || strings.Contains(g, t) {
		return true
	}

	gt := significantTokens(global)
	tt := significantTokens(title)
	if len(gt) == 0 || len(tt) == 0 {
		return false
	}
	// Compare as many leading tokens as both have. Extra words on either side
	// are common ("Weather Cycle DX" vs weatherCycleVersion).
	n := len(gt)
	if len(tt) < n {
		n = len(tt)
	}
	for i := 0; i < n; i++ {
		if !prefixEither(tt[i], gt[i]) {
			return false
		}
	}
	return true
}

// HumanName turns a global into something a person can search for.
func HumanName(global string) string {
	return strings.Join(significantTokens(global), " ")
}

func significantTokens(s string) []string {
	tokens := tokenize(s)
	for len(tokens) > 1 && noiseTokens[tokens[len(tokens)-1]] {
		tokens = tokens[:len(tokens)-1]
	}
	return tokens
}

func normalize(s string) string {
	return strings.Join(tokenize(s), "")
}

func tokenize(s string) []string {
	var b strings.Builder
	prevLower := false
	for _, r := range s {
		switch {
		case unicode.IsUpper(r) && prevLower:
			b.WriteByte(' ')
			b.WriteRune(unicode.ToLower(r))
			prevLower = false
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			prevLower = unicode.IsLower(r)
		default:
			b.WriteByte(' ')
			prevLower = false
		}
	}
	return strings.Fields(b.String())
}

func prefixEither(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	shorter, longer := a, b
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	return len(shorter) >= 3 && strings.HasPrefix(longer, shorter)
}
