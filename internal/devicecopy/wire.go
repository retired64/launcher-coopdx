package devicecopy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Wire-level errors. The UI shows these strings directly, so they match the
// reference client's wording.
var (
	ErrMixed        = errors.New("The copy got mixed up.")
	ErrStopped      = errors.New("Stopped.")
	ErrLost         = errors.New("The connection to your other device was lost.")
	ErrManifestBig  = errors.New("The list of what to copy is too big.")
	ErrBadFrame     = errors.New("The copy got mixed up.")
	ErrFileChanged  = errors.New("A file changed while it was being copied.")
	ErrFileDamaged  = errors.New("A file arrived damaged. Nothing was copied; try again.")
	ErrFileMissing  = errors.New("Some files didn't arrive. Nothing was copied; try again.")
	ErrCompiledSwap = errors.New("A file changed on the way. Nothing was copied.")
)

// Frame is one binary copy message: a file index, an absolute offset, and up
// to 64 KiB of data.
type Frame struct {
	File   int
	Offset int64
	Data   []byte
}

// EncodeFrame builds the exact 12-byte little-endian header used by the
// reference client.
func EncodeFrame(file int, offset int64, data []byte) []byte {
	out := make([]byte, FrameHeader+len(data))
	binary.LittleEndian.PutUint32(out[0:4], uint32(file))
	binary.LittleEndian.PutUint32(out[4:8], uint32(offset&0xffffffff))
	binary.LittleEndian.PutUint32(out[8:12], uint32(offset>>32))
	copy(out[FrameHeader:], data)
	return out
}

// DecodeFrame parses a binary message. It returns ok=false for anything that
// cannot be a frame, including oversized payloads.
func DecodeFrame(data []byte) (Frame, bool) {
	if len(data) < FrameHeader || len(data) > FrameHeader+FileChunk {
		return Frame{}, false
	}
	file := int(binary.LittleEndian.Uint32(data[0:4]))
	lo := uint64(binary.LittleEndian.Uint32(data[4:8]))
	hi := uint64(binary.LittleEndian.Uint32(data[8:12]))
	offset := int64(lo | hi<<32)
	return Frame{File: file, Offset: offset, Data: data[FrameHeader:]}, true
}

// ManifestChunks splits the JSON into text messages. The reference client
// slices 60 000 UTF-16 code units; splitting by runes is equivalent for the
// manifest's ASCII keys and never cuts a UTF-8 byte in half.
func ManifestChunks(text string) []string {
	if text == "" {
		return []string{""}
	}
	var out []string
	var b strings.Builder
	count := 0
	for _, r := range text {
		b.WriteRune(r)
		count++
		if count == ManifestChunk {
			out = append(out, b.String())
			b.Reset()
			count = 0
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	if len(out) == 0 {
		out = append(out, "")
	}
	return out
}

// ManifestChunkLimit is the maximum number of manifest messages accepted.
// It mirrors ceil(4 MiB / 60 000).
func ManifestChunkLimit() int {
	return (ManifestMaxBytes + ManifestChunk - 1) / ManifestChunk
}

// SanitizeControl removes control characters and bounds the result.
func SanitizeControl(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) {
			r = ' '
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if max > 0 && utf8.RuneCountInString(out) > max {
		runes := []rune(out)
		out = string(runes[:max])
	}
	return out
}

// SafeComponent implements the reference client's filename check (its ze()):
// non-empty, not "." or "..", no reserved characters or control characters,
// no trailing dot/space, and no Windows device names.
func SafeComponent(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			return false
		}
	}
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ") {
		return false
	}
	base := strings.ToLower(s)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	switch base {
	case "con", "prn", "aux", "nul":
		return false
	}
	if len(base) == 4 {
		if (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) && base[3] >= '1' && base[3] <= '9' {
			return false
		}
	}
	return true
}

// SafeRel checks a manifest relative path: at most 400 bytes, at most 16
// components, and every component safe.
func SafeRel(rel string) bool {
	if rel == "" || len(rel) > 400 || strings.ContainsRune(rel, '\\') {
		return false
	}
	parts := strings.Split(rel, "/")
	if len(parts) > 16 {
		return false
	}
	for _, p := range parts {
		if !SafeComponent(p) {
			return false
		}
	}
	return true
}

// SafeID is SafeComponent for a top-level mod or pack name.
func SafeID(id string) bool {
	return id != "" && len(id) <= 200 && SafeComponent(id)
}

// StripColors removes the game's inline colour escapes, like the reference
// client does before showing or storing a mod name.
func StripColors(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == '#' {
			j := i + 2
			for j < len(s) && isHex(s[j]) {
				j++
			}
			if j-i == 8 || j-i == 10 {
				// \#rrggbb\ or \#rrggbbaa\
				if j < len(s) && s[j] == '\\' {
					i = j + 1
					continue
				}
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return strings.TrimSpace(b.String())
}

func isHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// Latin1Encode turns arbitrary bytes into a JSON-safe string where every byte
// becomes the code point of the same value. The reference client reads .sav
// files as Latin-1, so this preserves them byte for byte through JSON.
func Latin1Encode(data []byte) string {
	runes := make([]rune, len(data))
	for i, b := range data {
		runes[i] = rune(b)
	}
	return string(runes)
}

// Latin1Decode reverses Latin1Encode. It rejects runes above 0xff.
func Latin1Decode(s string) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xff {
			return nil, fmt.Errorf("a save contains a character outside Latin-1")
		}
		out = append(out, byte(r))
	}
	return out, nil
}
