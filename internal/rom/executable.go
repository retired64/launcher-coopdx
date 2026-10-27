package rom

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

// The vanilla ROM HUD font has no J, Q, V, X or Z. sm64coopdx adds those few
// letters to its executable, so the loader looks them up in the game binary too.
// This is a small, read-only parser: no code is executed and no game data is
// copied into the repository.

type section struct {
	name string
	va   uint64
	raw  uint64
	size uint64
}

type fileImage struct {
	data     []byte
	base     uint64
	sections []section
}

func parseExecutable(data []byte) (*fileImage, error) {
	if len(data) < 64 {
		return nil, errors.New("the game file is too short")
	}
	if bytes.HasPrefix(data, []byte{0x7f, 'E', 'L', 'F'}) {
		return parseELF(data)
	}
	if data[0] == 'M' && data[1] == 'Z' {
		return parsePE(data)
	}
	return nil, errors.New("the game file is not an ELF or PE program")
}

func parseELF(data []byte) (*fileImage, error) {
	if len(data) < 64 || data[4] != 2 || data[5] != 1 {
		return nil, errors.New("not a 64-bit little-endian program")
	}
	imageType := binary.LittleEndian.Uint16(data[16:])
	machine := binary.LittleEndian.Uint16(data[18:])
	if machine != 62 && machine != 183 {
		return nil, errors.New("not an x86-64 or AArch64 program")
	}
	if imageType != 2 {
		// A position-independent executable relocates its pointers while
		// loading; the file alone does not hold usable addresses.
		return nil, errors.New("position-independent build")
	}
	shoff := int(binary.LittleEndian.Uint64(data[40:]))
	shentsize := int(binary.LittleEndian.Uint16(data[58:]))
	shnum := int(binary.LittleEndian.Uint16(data[60:]))
	shstrndx := int(binary.LittleEndian.Uint16(data[62:]))
	if shoff <= 0 || shentsize < 64 || shnum <= 0 || shoff+shentsize*shnum > len(data) || shstrndx >= shnum {
		return nil, errors.New("the program has no usable section table")
	}
	type rawSection struct {
		nameoff, typ uint32
		flags        uint64
		addr, off    uint64
		size         uint64
	}
	raw := make([]rawSection, 0, shnum)
	for i := 0; i < shnum; i++ {
		at := shoff + i*shentsize
		raw = append(raw, rawSection{
			nameoff: binary.LittleEndian.Uint32(data[at:]),
			typ:     binary.LittleEndian.Uint32(data[at+4:]),
			flags:   binary.LittleEndian.Uint64(data[at+8:]),
			addr:    binary.LittleEndian.Uint64(data[at+16:]),
			off:     binary.LittleEndian.Uint64(data[at+24:]),
			size:    binary.LittleEndian.Uint64(data[at+32:]),
		})
	}
	shstr := raw[shstrndx]
	if shstr.off+shstr.size > uint64(len(data)) {
		return nil, errors.New("the section names are outside the file")
	}
	img := &fileImage{data: data}
	for _, s := range raw {
		if s.flags&2 == 0 || s.typ == 8 || s.size == 0 {
			continue
		}
		nameAt := int(shstr.off) + int(s.nameoff)
		if nameAt < 0 || nameAt >= len(data) {
			continue
		}
		end := bytes.IndexByte(data[nameAt:], 0)
		if end < 0 {
			continue
		}
		name := string(data[nameAt : nameAt+end])
		if s.off+s.size > uint64(len(data)) {
			continue
		}
		img.sections = append(img.sections, section{name: name, va: s.addr, raw: s.off, size: s.size})
	}
	if len(img.sections) == 0 {
		return nil, errors.New("the program has no loadable sections")
	}
	return img, nil
}

func parsePE(data []byte) (*fileImage, error) {
	if len(data) < 0x40 {
		return nil, errors.New("the game file is too short")
	}
	pe := int(binary.LittleEndian.Uint32(data[0x3c:]))
	if pe <= 0 || pe+24 > len(data) || string(data[pe:pe+4]) != "PE\x00\x00" {
		return nil, errors.New("not a Windows program")
	}
	machine := binary.LittleEndian.Uint16(data[pe+4:])
	if machine != 0x8664 {
		return nil, errors.New("not a 64-bit Windows program")
	}
	count := int(binary.LittleEndian.Uint16(data[pe+6:]))
	optSize := int(binary.LittleEndian.Uint16(data[pe+20:]))
	opt := pe + 24
	if opt+optSize > len(data) || optSize < 32 {
		return nil, errors.New("the Windows program has no optional header")
	}
	base := binary.LittleEndian.Uint64(data[opt+24:])
	secAt := opt + optSize
	if secAt+count*40 > len(data) {
		return nil, errors.New("the section table is outside the file")
	}
	img := &fileImage{data: data, base: base}
	for i := 0; i < count; i++ {
		at := secAt + i*40
		nameEnd := bytes.IndexByte(data[at:at+8], 0)
		if nameEnd < 0 {
			nameEnd = 8
		}
		va := uint64(binary.LittleEndian.Uint32(data[at+12:]))
		size := uint64(binary.LittleEndian.Uint32(data[at+16:]))
		raw := uint64(binary.LittleEndian.Uint32(data[at+20:]))
		if size == 0 || raw+size > uint64(len(data)) {
			continue
		}
		img.sections = append(img.sections, section{
			name: string(data[at : at+nameEnd]), va: va, raw: raw, size: size,
		})
	}
	if len(img.sections) == 0 {
		return nil, errors.New("the Windows program has no loadable sections")
	}
	return img, nil
}

func (f *fileImage) off(va uint64) int64 {
	want := va - f.base
	for _, s := range f.sections {
		if want >= s.va && want < s.va+s.size {
			return int64(s.raw + want - s.va)
		}
	}
	return -1
}

func (f *fileImage) va(raw int) int64 {
	if raw < 0 || raw >= len(f.data) {
		return -1
	}
	at := uint64(raw)
	for _, s := range f.sections {
		if at >= s.raw && at < s.raw+s.size {
			return int64(f.base + s.va + at - s.raw)
		}
	}
	return -1
}

func (f *fileImage) ptr(raw int) uint64 {
	if raw < 0 || raw+8 > len(f.data) {
		return 0
	}
	return binary.LittleEndian.Uint64(f.data[raw:])
}

type hudName struct {
	symbol string
	char   string
}

var hudNames = []hudName{
	{"J", "J"}, {"Q", "Q"}, {"V", "V"}, {"X", "X"}, {"Z", "Z"},
	{"exclamation", "!"}, {"ampersand", "&"}, {"period", "."},
	{"comma", ","}, {"dash", "-"}, {"slash", "/"}, {"plus", "+"},
}

// executableHUD returns the extra 16x16 RGBA16 letters stored in the game
// executable. It is best-effort: an unsupported build or a missing symbol only
// means those few letters fall back to the ordinary font.
func executableHUD(path string) (map[string][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	img, err := parseExecutable(data)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, item := range hudNames {
		needle := []byte("texture_hud_char_" + item.symbol + "\x00")
		for next := 0; ; {
			at := bytes.Index(data[next:], needle)
			if at < 0 {
				break
			}
			at += next
			next = at + 1
			va := img.va(at)
			if va < 0 {
				continue
			}
			var pointer [8]byte
			binary.LittleEndian.PutUint64(pointer[:], uint64(va))
			for p := 0; ; {
				found := bytes.Index(data[p:], pointer[:])
				if found < 0 {
					break
				}
				found += p
				p = found + 1
				if found%8 != 0 || found < 8 || found+24 > len(data) {
					continue
				}
				if img.ptr(found+8) != 0x1000000010 || img.ptr(found+16) != 512 {
					continue
				}
				raw := img.off(img.ptr(found - 8))
				if raw < 0 || int(raw)+512 > len(data) {
					continue
				}
				out[item.char] = append([]byte(nil), data[raw:raw+512]...)
				break
			}
			if _, ok := out[item.char]; ok {
				break
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no extra HUD letters found in %s", path)
	}
	return out, nil
}
