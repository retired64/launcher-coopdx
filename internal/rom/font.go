package rom

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// A tiny TrueType writer. The game's fonts are bitmaps, so every glyph becomes
// a set of filled rectangles. That keeps the outlines pixel-exact and avoids
// depending on a font compiler at runtime.

const fontPixel = 64

type contour []fontPoint

type fontPoint struct {
	X, Y int
}

type fontGlyph struct {
	Name     string
	R        rune
	Advance  int // in pixels
	Contours []contour
}

type twobyte [2]int

type accent struct {
	R    rune
	Base rune
	Mark string
}

var accents = []accent{
	{'á', 'a', "acute"}, {'à', 'a', "grave"}, {'â', 'a', "circumflex"},
	{'ã', 'a', "tilde"}, {'ä', 'a', "diaeresis"},
	{'é', 'e', "acute"}, {'è', 'e', "grave"}, {'ê', 'e', "circumflex"},
	{'ë', 'e', "diaeresis"},
	{'í', 'i', "acute"}, {'ì', 'i', "grave"}, {'î', 'i', "circumflex"},
	{'ï', 'i', "diaeresis"},
	{'ó', 'o', "acute"}, {'ò', 'o', "grave"}, {'ô', 'o', "circumflex"},
	{'õ', 'o', "tilde"}, {'ö', 'o', "diaeresis"},
	{'ú', 'u', "acute"}, {'ù', 'u', "grave"}, {'û', 'u', "circumflex"},
	{'ü', 'u', "diaeresis"},
	{'ÿ', 'y', "diaeresis"},
	{'ñ', 'n', "tilde"},
	{'ç', 'c', "cedilla"},
}

type bitmap struct {
	W, H int
	On   []bool // row-major, alpha > 127
}

func (b bitmap) at(x, y int) bool {
	if x < 0 || y < 0 || x >= b.W || y >= b.H {
		return false
	}
	return b.On[y*b.W+x]
}

func (b bitmap) pixels() map[twobyte]bool {
	out := make(map[twobyte]bool, b.W*b.H)
	for y := 0; y < b.H; y++ {
		for x := 0; x < b.W; x++ {
			if b.at(x, y) {
				out[twobyte{x, y}] = true
			}
		}
	}
	return out
}

func (b bitmap) firstRow() int {
	for y := 0; y < b.H; y++ {
		for x := 0; x < b.W; x++ {
			if b.at(x, y) {
				return y
			}
		}
	}
	return 0
}

func (b bitmap) lastRow() int {
	for y := b.H - 1; y >= 0; y-- {
		for x := 0; x < b.W; x++ {
			if b.at(x, y) {
				return y
			}
		}
	}
	return 0
}

// rotate is the reference client's ww(): it turns a horizontal glyph cell into
// the upright cell the dialog font uses.
func rotate(in bitmap) bitmap {
	out := bitmap{W: in.H, H: in.W, On: make([]bool, in.W*in.H)}
	for y := 0; y < out.H; y++ {
		for x := 0; x < out.W; x++ {
			out.On[y*out.W+x] = in.at(in.W-1-y, in.H-1-x)
		}
	}
	return out
}

func decodeGlyph(r *ROM, def glyphDef, turned bool) (bitmap, error) {
	block, err := r.Block(def.Block)
	if err != nil {
		return bitmap{}, err
	}
	if def.Sub < 0 || def.Sub+def.Size > len(block) {
		return bitmap{}, errors.New("a glyph is outside its block")
	}
	img, err := DecodeTexture(block[def.Sub:def.Sub+def.Size], def.textureDef)
	if err != nil {
		return bitmap{}, err
	}
	b := bitmap{W: def.W, H: def.H, On: make([]bool, def.W*def.H)}
	for i := range b.On {
		b.On[i] = img.Pix[i*4+3] > 127
	}
	if turned {
		b = rotate(b)
	}
	return b, nil
}

func buildFont(family string, cellH, space int, defs map[rune]glyphDef, turned, aliasLower bool, r *ROM) ([]byte, error) {
	raw := make(map[rune]bitmap, len(defs))
	upright := make(map[rune]bitmap, len(defs))
	for ch, def := range defs {
		b, err := decodeGlyph(r, def, turned)
		if err != nil {
			return nil, fmt.Errorf("%c: %w", ch, err)
		}
		upright[ch] = b
		raw[ch] = b
	}
	a, ok := upright['A']
	if !ok {
		return nil, errors.New("the font has no A to set the baseline")
	}
	baseline := a.lastRow() + 1
	rawA := raw['A']

	glyphs := make([]fontGlyph, 0, len(defs)+64)
	used := map[rune]bool{}
	add := func(g fontGlyph) {
		if g.R != 0 && used[g.R] {
			return
		}
		if g.R != 0 {
			used[g.R] = true
		}
		glyphs = append(glyphs, g)
	}
	add(fontGlyph{Name: ".notdef", Advance: 5})
	add(fontGlyph{Name: "space", R: ' ', Advance: space})
	add(fontGlyph{Name: "uni00A0", R: 0x00A0, Advance: space})
	add(fontGlyph{Name: "uni202F", R: 0x202F, Advance: maxInt(1, int(math.Round(float64(space)/2)))})

	order := make([]rune, 0, len(upright))
	for ch := range upright {
		order = append(order, ch)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	for _, ch := range order {
		b := upright[ch]
		g := fontGlyph{
			Name:     fmt.Sprintf("u%04x", ch),
			R:        ch,
			Advance:  defs[ch].Adv,
			Contours: pixelsToContours(b.pixels(), baseline),
		}
		add(g)
		if aliasLower {
			lower := toLowerRune(ch)
			if lower != ch && !hasGlyph(upright, lower) {
				alias := g
				alias.R = lower
				alias.Name = fmt.Sprintf("u%04x", lower)
				add(alias)
			}
		}
	}

	// Accents are drawn by copying the base glyph and adding the two or four
	// pixels the reference client uses for each mark.
	e, hasE := upright['e']
	xHeightTop := 0
	if hasE {
		for y := e.H - 1; y >= 0; y-- {
			found := false
			for x := 0; x < e.W; x++ {
				if e.at(x, y) {
					xHeightTop = y
					found = true
					break
				}
			}
			_ = found
		}
	}
	for _, item := range accents {
		for _, ch := range []rune{item.R, toUpperRune(item.R)} {
			baseCh := item.Base
			if ch == toUpperRune(item.R) {
				baseCh = toUpperRune(item.Base)
			}
			base, ok := upright[baseCh]
			if !ok {
				continue
			}
			px := base.pixels()
			if baseCh == 'i' && xHeightTop > 0 {
				for p := range px {
					if p[1] < xHeightTop {
						delete(px, p)
					}
				}
			}
			if len(px) == 0 {
				continue
			}
			top := 1 << 30
			bottom := -1
			for p := range px {
				if p[1] < top {
					top = p[1]
				}
				if p[1] > bottom {
					bottom = p[1]
				}
			}
			sum := 0
			count := 0
			for p := range px {
				if p[1] == top {
					sum += p[0]
					count++
				}
			}
			x := int(math.Floor(float64(sum)/float64(maxInt(count, 1)) + 0.5))
			first := top - 3
			second := top - 2
			marks := map[string][][2]int{
				"acute":      {{1, first}, {0, second}},
				"grave":      {{-1, first}, {0, second}},
				"circumflex": {{0, first}, {-1, second}, {1, second}},
				"tilde":      {{0, first}, {2, first}, {-1, second}, {1, second}},
				"diaeresis":  {{-1, second}, {1, second}},
				"cedilla":    {{0, bottom + 1}, {-1, bottom + 2}},
			}[item.Mark]
			for _, m := range marks {
				px[twobyte{x + m[0], m[1]}] = true
			}
			g := fontGlyph{
				Name:     fmt.Sprintf("u%04x", ch),
				R:        ch,
				Advance:  defs[baseCh].Adv,
				Contours: pixelsToContours(px, baseline),
			}
			add(g)
			if aliasLower {
				lower := toLowerRune(ch)
				if lower != ch {
					alias := g
					alias.R = lower
					alias.Name = fmt.Sprintf("u%04x", lower)
					add(alias)
				}
			}
		}
	}

	// Upside-down question and exclamation marks mirror the base glyphs. The
	// reference client anchors them on the top row of the unrotated A.
	topA := rawA.firstRow()
	for _, pair := range [][2]rune{{'¡', '!'}, {'¿', '?'}} {
		base, ok := upright[pair[1]]
		if !ok {
			continue
		}
		src := base.pixels()
		left := 1 << 30
		right := -1
		for p := range src {
			if p[0] < left {
				left = p[0]
			}
			if p[0] > right {
				right = p[0]
			}
		}
		flipped := map[twobyte]bool{}
		for p := range src {
			flipped[twobyte{left + right - p[0], topA + baseline - 1 - p[1]}] = true
		}
		g := fontGlyph{
			Name:     fmt.Sprintf("u%04x", pair[0]),
			R:        pair[0],
			Advance:  defs[pair[1]].Adv,
			Contours: pixelsToContours(flipped, baseline),
		}
		add(g)
	}

	asc := baseline * fontPixel
	desc := -(cellH - baseline) * fontPixel
	return writeTTF(family, cellH*fontPixel, asc, desc, glyphs)
}

func hasGlyph(m map[rune]bitmap, ch rune) bool {
	_, ok := m[ch]
	return ok
}

func baseRune(ch rune) rune {
	for _, a := range accents {
		if a.R == ch || toUpperRune(a.R) == ch {
			return a.Base
		}
	}
	return ch
}

func toLowerRune(ch rune) rune {
	if ch >= 'A' && ch <= 'Z' {
		return ch + ('a' - 'A')
	}
	if lower, ok := upperToLower[ch]; ok {
		return lower
	}
	return ch
}

func toUpperRune(ch rune) rune {
	if ch >= 'a' && ch <= 'z' {
		return ch - ('a' - 'A')
	}
	switch ch {
	case 'á', 'à', 'â', 'ã', 'ä':
		return map[rune]rune{'á': 'Á', 'à': 'À', 'â': 'Â', 'ã': 'Ã', 'ä': 'Ä'}[ch]
	case 'é', 'è', 'ê', 'ë':
		return map[rune]rune{'é': 'É', 'è': 'È', 'ê': 'Ê', 'ë': 'Ë'}[ch]
	case 'í', 'ì', 'î', 'ï':
		return map[rune]rune{'í': 'Í', 'ì': 'Ì', 'î': 'Î', 'ï': 'Ï'}[ch]
	case 'ó', 'ò', 'ô', 'õ', 'ö':
		return map[rune]rune{'ó': 'Ó', 'ò': 'Ò', 'ô': 'Ô', 'õ': 'Õ', 'ö': 'Ö'}[ch]
	case 'ú', 'ù', 'û', 'ü':
		return map[rune]rune{'ú': 'Ú', 'ù': 'Ù', 'û': 'Û', 'ü': 'Ü'}[ch]
	case 'ÿ':
		return 'Ÿ'
	case 'ñ':
		return 'Ñ'
	case 'ç':
		return 'Ç'
	}
	return ch
}

var upperToLower = map[rune]rune{
	'Á': 'á', 'À': 'à', 'Â': 'â', 'Ã': 'ã', 'Ä': 'ä',
	'É': 'é', 'È': 'è', 'Ê': 'ê', 'Ë': 'ë',
	'Í': 'í', 'Ì': 'ì', 'Î': 'î', 'Ï': 'ï',
	'Ó': 'ó', 'Ò': 'ò', 'Ô': 'ô', 'Õ': 'õ', 'Ö': 'ö',
	'Ú': 'ú', 'Ù': 'ù', 'Û': 'û', 'Ü': 'ü',
	'Ÿ': 'ÿ', 'Ñ': 'ñ', 'Ç': 'ç',
}

// pixelsToContours turns every horizontal run of pixels into one rectangle.
func pixelsToContours(px map[twobyte]bool, baseline int) []contour {
	rows := map[int][]int{}
	for p := range px {
		rows[p[1]] = append(rows[p[1]], p[0])
	}
	ys := make([]int, 0, len(rows))
	for y := range rows {
		ys = append(ys, y)
	}
	sort.Ints(ys)
	var out []contour
	for _, y := range ys {
		xs := rows[y]
		sort.Ints(xs)
		for i := 0; i < len(xs); {
			start := xs[i]
			end := start
			j := i + 1
			for j < len(xs) && xs[j] == end+1 {
				end = xs[j]
				j++
			}
			bottom := (baseline - y - 1) * fontPixel
			top := (baseline - y) * fontPixel
			out = append(out, contour{
				{start * fontPixel, bottom},
				{(end + 1) * fontPixel, bottom},
				{(end + 1) * fontPixel, top},
				{start * fontPixel, top},
			})
			i = j
		}
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// --- TrueType writer -------------------------------------------------------

type table struct {
	tag  string
	data []byte
}

func writeTTF(family string, unitsPerEm, ascender, descender int, glyphs []fontGlyph) ([]byte, error) {
	if len(glyphs) == 0 || len(glyphs) > 0xffff {
		return nil, errors.New("the font has no glyphs or too many")
	}
	// Glyph order is the slice order. The cmap points at it.
	index := map[rune]int{}
	for i, g := range glyphs {
		if g.R != 0 {
			index[g.R] = i
		}
	}

	glyf := make([]byte, 0, 1<<16)
	loca := make([]uint32, len(glyphs)+1)
	maxPoints, maxContours := 0, 0
	xMinAll, yMinAll := 1<<30, 1<<30
	xMaxAll, yMaxAll := -1<<30, -1<<30
	for i, g := range glyphs {
		loca[i] = uint32(len(glyf))
		if len(g.Contours) == 0 {
			continue
		}
		points := 0
		for _, c := range g.Contours {
			points += len(c)
		}
		if points > maxPoints {
			maxPoints = points
		}
		if len(g.Contours) > maxContours {
			maxContours = len(g.Contours)
		}
		xMin, yMin := 1<<30, 1<<30
		xMax, yMax := -1<<30, -1<<30
		for _, c := range g.Contours {
			for _, p := range c {
				if p.X < xMin {
					xMin = p.X
				}
				if p.X > xMax {
					xMax = p.X
				}
				if p.Y < yMin {
					yMin = p.Y
				}
				if p.Y > yMax {
					yMax = p.Y
				}
			}
		}
		if xMin < xMinAll {
			xMinAll = xMin
		}
		if xMax > xMaxAll {
			xMaxAll = xMax
		}
		if yMin < yMinAll {
			yMinAll = yMin
		}
		if yMax > yMaxAll {
			yMaxAll = yMax
		}
		glyf = appendInt16(glyf, len(g.Contours))
		glyf = appendInt16(glyf, xMin)
		glyf = appendInt16(glyf, yMin)
		glyf = appendInt16(glyf, xMax)
		glyf = appendInt16(glyf, yMax)
		end := -1
		for _, c := range g.Contours {
			end += len(c)
			glyf = appendUint16(glyf, uint16(end))
		}
		glyf = appendUint16(glyf, 0) // no instructions
		for range pointsAll(g.Contours) {
			glyf = append(glyf, 0x01) // on-curve, 16-bit coordinates
		}
		prev := 0
		for _, c := range g.Contours {
			for _, p := range c {
				glyf = appendInt16(glyf, p.X-prev)
				prev = p.X
			}
		}
		prev = 0
		for _, c := range g.Contours {
			for _, p := range c {
				glyf = appendInt16(glyf, p.Y-prev)
				prev = p.Y
			}
		}
		for len(glyf)%4 != 0 {
			glyf = append(glyf, 0)
		}
	}
	loca[len(glyphs)] = uint32(len(glyf))
	if len(glyf) > 0x1fffe {
		return nil, errors.New("the font is too large for short offsets")
	}

	hmtx := make([]byte, 0, len(glyphs)*4)
	advMax := 0
	for i, g := range glyphs {
		adv := g.Advance * fontPixel
		if adv < 0 {
			adv = 0
		}
		if adv > 65535 {
			adv = 65535
		}
		if adv > advMax {
			advMax = adv
		}
		lsb := 0
		if len(g.Contours) > 0 {
			lsb = g.Contours[0][0].X
			for _, c := range g.Contours {
				for _, p := range c {
					if p.X < lsb {
						lsb = p.X
					}
				}
			}
		}
		hmtx = appendUint16(hmtx, uint16(adv))
		hmtx = appendInt16(hmtx, lsb)
		_ = i
	}

	cmap := buildCmap(index)
	head := buildHead(unitsPerEm, xMinAll, yMinAll, xMaxAll, yMaxAll)
	hhea := buildHhea(ascender, descender, advMax, xMinAll, xMaxAll, len(glyphs))
	maxp := buildMaxp(len(glyphs), maxPoints, maxContours)
	name := buildName(family)
	os2 := buildOS2(ascender, descender, yMinAll, yMaxAll, glyphs, index)
	post := buildPost()
	locaTable, err := buildLoca(loca)
	if err != nil {
		return nil, err
	}

	tables := []table{
		{"OS/2", os2},
		{"cmap", cmap},
		{"glyf", glyf},
		{"head", head},
		{"hhea", hhea},
		{"hmtx", hmtx},
		{"loca", locaTable},
		{"maxp", maxp},
		{"name", name},
		{"post", post},
	}
	return assemble(tables)
}

func pointsAll(cs []contour) []fontPoint {
	var out []fontPoint
	for _, c := range cs {
		out = append(out, c...)
	}
	return out
}

func buildCmap(index map[rune]int) []byte {
	codes := make([]int, 0, len(index))
	for r := range index {
		if r > 0 && r <= 0xffff {
			codes = append(codes, int(r))
		}
	}
	sort.Ints(codes)
	// One segment per code plus the final 0xFFFF segment.
	segCount := len(codes) + 1
	segments := append(append([]int(nil), codes...), 0xffff)
	entrySelector := 0
	for 1<<(entrySelector+1) <= segCount {
		entrySelector++
	}
	searchRange := 2 * (1 << entrySelector)
	rangeShift := 2*segCount - searchRange
	sub := make([]byte, 0, 16+segCount*8)
	sub = appendUint16(sub, 4)
	sub = appendUint16(sub, uint16(16+segCount*8))
	sub = appendUint16(sub, 0)
	sub = appendUint16(sub, uint16(segCount*2))
	sub = appendUint16(sub, uint16(searchRange))
	sub = appendUint16(sub, uint16(entrySelector))
	sub = appendUint16(sub, uint16(rangeShift))
	for _, code := range segments {
		sub = appendUint16(sub, uint16(code))
	}
	sub = appendUint16(sub, 0) // reservedPad
	for _, code := range segments {
		sub = appendUint16(sub, uint16(code))
	}
	for _, code := range segments {
		glyph := 0
		if code != 0xffff {
			glyph = index[rune(code)]
		}
		sub = appendUint16(sub, uint16((glyph-code)&0xffff))
	}
	for range segments {
		sub = appendUint16(sub, 0)
	}
	header := make([]byte, 12)
	// version 0, one encoding record, Windows Unicode BMP, offset 12.
	binary.BigEndian.PutUint16(header[2:], 1)
	binary.BigEndian.PutUint16(header[4:], 3)
	binary.BigEndian.PutUint16(header[6:], 1)
	binary.BigEndian.PutUint32(header[8:], 12)
	return append(header, sub...)
}

func buildHead(unitsPerEm, xMin, yMin, xMax, yMax int) []byte {
	b := make([]byte, 54)
	binary.BigEndian.PutUint32(b[0:], 0x00010000) // version
	binary.BigEndian.PutUint32(b[4:], 0x00010000) // fontRevision
	// 8: checksumAdjustment, patched after assembly
	binary.BigEndian.PutUint32(b[12:], 0x5F0F3CF5)
	binary.BigEndian.PutUint16(b[16:], 0x000b)
	binary.BigEndian.PutUint16(b[18:], uint16(unitsPerEm))
	// 20..27 created, 28..35 modified: zero
	binary.BigEndian.PutUint16(b[36:], uint16(int16clamp(xMin)))
	binary.BigEndian.PutUint16(b[38:], uint16(int16clamp(yMin)))
	binary.BigEndian.PutUint16(b[40:], uint16(int16clamp(xMax)))
	binary.BigEndian.PutUint16(b[42:], uint16(int16clamp(yMax)))
	binary.BigEndian.PutUint16(b[44:], 0) // macStyle
	binary.BigEndian.PutUint16(b[46:], 3) // lowestRecPPEM
	binary.BigEndian.PutUint16(b[48:], 2) // fontDirectionHint
	binary.BigEndian.PutUint16(b[50:], 0) // indexToLocFormat: short
	binary.BigEndian.PutUint16(b[52:], 0) // glyphDataFormat
	return b
}

func buildHhea(asc, desc, advMax, xMin, xMax, glyphs int) []byte {
	b := make([]byte, 36)
	binary.BigEndian.PutUint32(b[0:], 0x00010000)
	binary.BigEndian.PutUint16(b[4:], uint16(uint16clamp(asc)))
	binary.BigEndian.PutUint16(b[6:], uint16(int16clamp(desc)))
	binary.BigEndian.PutUint16(b[8:], 0)
	binary.BigEndian.PutUint16(b[10:], uint16clamp(advMax))
	binary.BigEndian.PutUint16(b[12:], uint16clamp(xMin))
	binary.BigEndian.PutUint16(b[14:], 0)
	binary.BigEndian.PutUint16(b[16:], uint16clamp(xMax))
	binary.BigEndian.PutUint16(b[18:], 1) // caretSlopeRise
	binary.BigEndian.PutUint16(b[20:], 0)
	binary.BigEndian.PutUint16(b[22:], 0)
	for i := 24; i < 32; i += 2 {
		binary.BigEndian.PutUint16(b[i:], 0)
	}
	binary.BigEndian.PutUint16(b[32:], 0)
	binary.BigEndian.PutUint16(b[34:], uint16(glyphs))
	return b
}

func buildMaxp(glyphs, points, contours int) []byte {
	b := make([]byte, 32)
	binary.BigEndian.PutUint32(b[0:], 0x00010000)
	binary.BigEndian.PutUint16(b[4:], uint16(glyphs))
	binary.BigEndian.PutUint16(b[6:], uint16(points))
	binary.BigEndian.PutUint16(b[8:], uint16(contours))
	binary.BigEndian.PutUint16(b[10:], 0)
	binary.BigEndian.PutUint16(b[12:], 0)
	binary.BigEndian.PutUint16(b[14:], 2)
	for i := 16; i < 32; i += 2 {
		binary.BigEndian.PutUint16(b[i:], 0)
	}
	return b
}

func buildLoca(offsets []uint32) ([]byte, error) {
	b := make([]byte, 0, len(offsets)*2)
	for _, off := range offsets {
		if off%2 != 0 {
			return nil, errors.New("a glyph has an odd offset")
		}
		if off/2 > 0xffff {
			return nil, errors.New("the font is too large for short loca")
		}
		b = appendUint16(b, uint16(off/2))
	}
	return b, nil
}

func buildPost() []byte {
	b := make([]byte, 32)
	binary.BigEndian.PutUint32(b[0:], 0x00030000)
	// italicAngle, underlinePosition, underlineThickness, isFixedPitch and the
	// four memory fields stay zero.
	return b
}

func buildOS2(asc, desc, yMin, yMax int, glyphs []fontGlyph, index map[rune]int) []byte {
	b := make([]byte, 96)
	avg := 0
	if len(glyphs) > 0 {
		sum := 0
		for _, g := range glyphs {
			sum += g.Advance * fontPixel
		}
		avg = sum / len(glyphs)
	}
	binary.BigEndian.PutUint16(b[0:], 3)
	binary.BigEndian.PutUint16(b[2:], uint16clamp(avg))
	binary.BigEndian.PutUint16(b[4:], 500)
	binary.BigEndian.PutUint16(b[6:], 5)
	binary.BigEndian.PutUint16(b[8:], 0)
	binary.BigEndian.PutUint16(b[10:], 650)
	binary.BigEndian.PutUint16(b[12:], 600)
	binary.BigEndian.PutUint16(b[14:], 0)
	binary.BigEndian.PutUint16(b[16:], 75)
	binary.BigEndian.PutUint16(b[18:], 650)
	binary.BigEndian.PutUint16(b[20:], 600)
	binary.BigEndian.PutUint16(b[22:], 0)
	binary.BigEndian.PutUint16(b[24:], 350)
	binary.BigEndian.PutUint16(b[26:], 49)
	binary.BigEndian.PutUint16(b[28:], 258)
	binary.BigEndian.PutUint16(b[30:], 0)
	// panose at 32..41
	copy(b[42:46], []byte("NONE"))
	binary.BigEndian.PutUint16(b[46:], 0x40) // fsSelection: regular
	first, last := 32, 32
	for r := range index {
		if int(r) < first {
			first = int(r)
		}
		if int(r) > last {
			last = int(r)
		}
	}
	binary.BigEndian.PutUint16(b[48:], uint16(first))
	binary.BigEndian.PutUint16(b[50:], uint16(last))
	binary.BigEndian.PutUint16(b[52:], uint16(int16clamp(asc)))
	binary.BigEndian.PutUint16(b[54:], uint16(int16clamp(desc)))
	binary.BigEndian.PutUint16(b[56:], 0)
	binary.BigEndian.PutUint16(b[58:], uint16clamp(maxInt(asc, yMax)))
	binary.BigEndian.PutUint16(b[60:], uint16clamp(maxInt(0, -yMin)))
	binary.BigEndian.PutUint32(b[62:], 0x80000007)
	binary.BigEndian.PutUint32(b[66:], 0)
	binary.BigEndian.PutUint32(b[70:], 1)
	binary.BigEndian.PutUint32(b[74:], 0)
	binary.BigEndian.PutUint16(b[86:], 384)
	binary.BigEndian.PutUint16(b[88:], 640)
	binary.BigEndian.PutUint16(b[90:], 32)
	binary.BigEndian.PutUint16(b[92:], 32)
	binary.BigEndian.PutUint16(b[94:], 0)
	return b
}

func buildName(family string) []byte {
	style := "Regular"
	full := family + " " + style
	ps := psName(family) + style
	version := "Version 0.1"
	unique := " : " + full
	values := []struct {
		id   uint16
		text string
	}{
		{1, family}, {2, style}, {3, unique}, {4, full}, {5, version}, {6, ps},
	}
	var stringsData []byte
	records := make([]byte, 0, len(values)*12)
	for _, v := range values {
		encoded := utf16BE(v.text)
		records = appendUint16(records, 3) // Windows
		records = appendUint16(records, 1) // Unicode BMP
		records = appendUint16(records, 0x0409)
		records = appendUint16(records, v.id)
		records = appendUint16(records, uint16(len(encoded)))
		records = appendUint16(records, uint16(len(stringsData)))
		stringsData = append(stringsData, encoded...)
	}
	b := make([]byte, 0, 6+len(records)+len(stringsData))
	b = appendUint16(b, 0)
	b = appendUint16(b, uint16(len(values)))
	b = appendUint16(b, uint16(6+len(records)))
	b = append(b, records...)
	b = append(b, stringsData...)
	return b
}

func psName(family string) string {
	out := make([]byte, 0, len(family))
	for i := 0; i < len(family); i++ {
		c := family[i]
		if c != ' ' && c != '-' {
			out = append(out, c)
		}
	}
	return string(out)
}

func utf16BE(s string) []byte {
	runes := []rune(s)
	out := make([]byte, 0, len(runes)*2)
	for _, r := range runes {
		if r > 0xffff {
			r -= 0x10000
			hi := 0xd800 + (r >> 10)
			lo := 0xdc00 + (r & 0x3ff)
			out = appendUint16(out, uint16(hi))
			out = appendUint16(out, uint16(lo))
			continue
		}
		out = appendUint16(out, uint16(r))
	}
	return out
}

func assemble(tables []table) ([]byte, error) {
	n := len(tables)
	entrySelector := 0
	for 1<<(entrySelector+1) <= n {
		entrySelector++
	}
	searchRange := (1 << entrySelector) * 16
	rangeShift := n*16 - searchRange
	header := make([]byte, 12+16*n)
	binary.BigEndian.PutUint32(header[0:], 0x00010000)
	binary.BigEndian.PutUint16(header[4:], uint16(n))
	binary.BigEndian.PutUint16(header[6:], uint16(searchRange))
	binary.BigEndian.PutUint16(header[8:], uint16(entrySelector))
	binary.BigEndian.PutUint16(header[10:], uint16(rangeShift))

	offset := 12 + 16*n
	body := make([]byte, 0, 1<<16)
	offsets := make([]int, n)
	for i, t := range tables {
		if len(body)%4 != 0 {
			body = append(body, make([]byte, 4-len(body)%4)...)
		}
		offsets[i] = offset + len(body)
		body = append(body, t.data...)
		body = append(body, 0, 0, 0)
	}
	font := append(header, body...)
	for i, t := range tables {
		rec := 12 + 16*i
		copy(font[rec:rec+4], t.tag)
		sum := checksum(font[offsets[i] : offsets[i]+len(t.data)])
		binary.BigEndian.PutUint32(font[rec+4:], sum)
		binary.BigEndian.PutUint32(font[rec+8:], uint32(offsets[i]))
		binary.BigEndian.PutUint32(font[rec+12:], uint32(len(t.data)))
	}
	// Patch the head checksum adjustment now that the whole font is known.
	for i, t := range tables {
		if t.tag == "head" {
			at := offsets[i] + 8
			if at+4 > len(font) {
				return nil, errors.New("bad head offset")
			}
			binary.BigEndian.PutUint32(font[at:], 0)
			sum := checksum(font)
			binary.BigEndian.PutUint32(font[at:], 0xb1b0afba-sum)
			break
		}
	}
	return font, nil
}

func checksum(data []byte) uint32 {
	var sum uint32
	for i := 0; i < len(data); i += 4 {
		var word [4]byte
		copy(word[:], data[i:minInt(i+4, len(data))])
		sum += binary.BigEndian.Uint32(word[:])
	}
	return sum
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func appendUint16(b []byte, v uint16) []byte {
	return append(b, byte(v>>8), byte(v))
}

func appendInt16(b []byte, v int) []byte {
	return appendUint16(b, uint16(int16clamp(v)))
}

func int16clamp(v int) int16 {
	if v > 32767 {
		v = 32767
	}
	if v < -32768 {
		v = -32768
	}
	return int16(v)
}

func uint16clamp(v int) uint16 {
	if v > 65535 {
		v = 65535
	}
	if v < 0 {
		v = 0
	}
	return uint16(v)
}
