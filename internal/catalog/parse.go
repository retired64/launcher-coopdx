package catalog

import (
	"bytes"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

func parseList(body []byte, base *url.URL) ([]Item, int) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, 1
	}
	switch {
	case doc.Find(".structItem--resource").Length() > 0:
		return parseListing(doc, base), pageCount(doc)
	case doc.Find("li.block-row").Length() > 0:
		return parseSearch(doc, base), pageCount(doc)
	default:
		return nil, 1
	}
}

func parseListing(doc *goquery.Document, base *url.URL) []Item {
	var out []Item
	doc.Find(".structItem--resource").Each(func(_ int, s *goquery.Selection) {
		link := s.Find(".structItem-title a[data-tp-primary]").First()
		if link.Length() == 0 {
			link = s.Find(".structItem-title a").First()
		}
		href, _ := link.Attr("href")
		url := absURL(base, href)
		id := idFromURL(url)
		if id == 0 {
			return
		}

		item := Item{
			ID:       id,
			URL:      url,
			Title:    strings.TrimSpace(link.Text()),
			Version:  strings.TrimSpace(s.Find(".structItem-title .u-muted").First().Text()),
			Tagline:  strings.TrimSpace(s.Find(".structItem-resourceTagLine").First().Text()),
			Category: strings.TrimSpace(s.Find(`.structItem-parts a[href*="/categories/"]`).First().Text()),
			Rating:   parseRating(attr(s.Find(".ratingStars").First(), "title")),
			Updated:  attr(s.Find(".structItem-metaItem--lastUpdate time").First(), "datetime"),
		}
		item.Author = strings.TrimSpace(s.Find(".structItem-parts .username").First().Text())
		if item.Author == "" {
			item.Author, _ = s.Attr("data-author")
		}
		item.RatingCount = parseCount(s.Find(".ratingStarsRow-text").First().Text())
		item.Downloads = parseCount(s.Find(".structItem-metaItem--downloads dd").First().Text())
		item.Icon = absURL(base, attr(s.Find(".structItem-iconContainer img").First(), "src"))
		if item.Icon == "" {
			item.Icon = absURL(base, attr(s.Find(`img[src*="/resource_icons/"]`).First(), "src"))
		}
		if item.Icon == "" {
			item.Icon = iconFor(base, id)
		}
		out = append(out, item)
	})
	return out
}

func parseSearch(doc *goquery.Document, base *url.URL) []Item {
	var out []Item
	doc.Find("li.block-row").Each(func(_ int, s *goquery.Selection) {
		link := s.Find(".contentRow-title a").First()
		href, _ := link.Attr("href")
		url := absURL(base, href)
		id := idFromURL(url)
		if id == 0 {
			return
		}
		item := Item{
			ID:       id,
			URL:      url,
			Title:    strings.TrimSpace(link.Text()),
			Version:  strings.TrimSpace(s.Find(".contentRow-title .u-muted").First().Text()),
			Tagline:  strings.TrimSpace(s.Find(".contentRow-snippet").First().Text()),
			Category: strings.TrimSpace(s.Find(`.contentRow-minor a[href*="/categories/"]`).First().Text()),
			Updated:  attr(s.Find("time").First(), "datetime"),
			Icon:     iconFor(base, id),
		}
		item.Author = strings.TrimSpace(s.Find(".contentRow-minor .username").First().Text())
		if item.Author == "" {
			item.Author, _ = s.Attr("data-author")
		}
		out = append(out, item)
	})
	return out
}

func parseCategories(body []byte) []Category {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	seen := map[int]bool{}
	var out []Category
	doc.Find("a.categoryList-link").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		m := catHrefRe.FindStringSubmatch(href)
		if len(m) != 3 {
			return
		}
		id, _ := strconv.Atoi(m[2])
		if id == 0 || seen[id] {
			return
		}
		seen[id] = true

		depth := s.ParentsFiltered("ol.categoryList").Length()
		if depth > 0 {
			depth--
		}
		out = append(out, Category{
			ID:    id,
			Slug:  m[1],
			Title: strings.TrimSpace(s.Text()),
			Depth: depth,
		})
	})
	return out
}

func parseDetail(body []byte, final string, base *url.URL) Detail {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return Detail{}
	}

	d := Detail{Item: Item{URL: final, ID: idFromURL(final)}}

	title := doc.Find("h1.p-title-value").First()
	if title.Length() == 0 {
		title = doc.Find("h1").First()
	}
	clone := title.Clone()
	clone.Find(".u-muted").Remove()
	d.Title = strings.TrimSpace(clone.Text())
	d.Version = strings.TrimSpace(title.Find(".u-muted").First().Text())

	d.Author = strings.TrimSpace(doc.Find(".p-description .username").First().Text())
	doc.Find(`.p-breadcrumbs a[href*="/categories/"]`).Each(func(_ int, s *goquery.Selection) {
		d.Category = strings.TrimSpace(s.Text())
	})

	d.Icon = absURL(base, attr(doc.Find(`img[src*="/resource_icons/"]`).First(), "src"))
	if d.Icon == "" {
		d.Icon = iconFor(base, d.ID)
	}
	d.Rating = parseRating(attr(doc.Find(".resourceSidebarGroup--rating .ratingStars").First(), "title"))
	d.RatingCount = parseCount(doc.Find(".resourceSidebarGroup--rating .ratingStarsRow-text").First().Text())

	doc.Find("dl.pairs").Each(func(_ int, s *goquery.Selection) {
		label := strings.ToLower(strings.TrimSpace(s.Find("dt").First().Text()))
		value := s.Find("dd").First()
		switch label {
		case "downloads":
			d.Downloads = parseCount(value.Text())
		case "last update":
			if t := s.Find("time").First(); t.Length() > 0 {
				d.Updated = attr(t, "datetime")
			}
		}
	})

	root := doc.Find(".bbWrapper").First()
	if root.Length() > 0 {
		d.Blocks = parseBlocks(root, base)
		d.Links = collectLinks(d.Blocks, d.ID, base)
	}
	return d
}

func parseBlocks(root *goquery.Selection, base *url.URL) []Block {
	var blocks []Block
	images := 0

	var walk func(*goquery.Selection)
	walk = func(sel *goquery.Selection) {
		sel.Contents().Each(func(_ int, node *goquery.Selection) {
			if len(blocks) >= maxBlocks {
				return
			}
			n := node.Get(0)
			if n == nil {
				return
			}
			if n.Type == html.TextNode {
				if text := normText(n.Data); strings.TrimSpace(text) != "" {
					blocks = append(blocks, Block{Kind: "p", Inline: trimInline([]Inline{{Text: text}})})
				}
				return
			}
			if n.Type != html.ElementNode {
				return
			}
			switch goquery.NodeName(node) {
			case "script", "style", "noscript", "iframe", "svg":
				return
			case "h1", "h2", "h3", "h4", "h5", "h6":
				if inline := parseInline(node, base); len(inline) > 0 {
					level := int(goquery.NodeName(node)[1] - '0')
					blocks = append(blocks, Block{Kind: "h", Level: level, Inline: inline})
				}
			case "ul", "ol":
				var items [][]Inline
				node.ChildrenFiltered("li").Each(func(_ int, li *goquery.Selection) {
					if inline := parseInline(li, base); len(inline) > 0 {
						items = append(items, inline)
					}
				})
				if len(items) > 0 {
					blocks = append(blocks, Block{Kind: "list", Items: items})
				}
			case "blockquote":
				if inline := parseInline(node, base); len(inline) > 0 {
					blocks = append(blocks, Block{Kind: "quote", Inline: inline})
				}
			case "img":
				if images < maxImages {
					if src := imageURL(node, base); src != "" {
						blocks = append(blocks, Block{Kind: "image", Src: src})
						images++
					}
				}
			case "br":
				return
			case "div", "section", "center", "article", "figure", "table":
				// Layout wrappers are common around images and text. Recurse when
				// they contain blocks so an image is not flattened into its alt text.
				if hasBlockChild(node) {
					walk(node)
					return
				}
				if inline := parseInline(node, base); len(inline) > 0 {
					blocks = append(blocks, Block{Kind: "p", Inline: inline})
				}
			default:
				if inline := parseInline(node, base); len(inline) > 0 {
					blocks = append(blocks, Block{Kind: "p", Inline: inline})
				}
			}
		})
	}
	walk(root)
	return blocks
}

func hasBlockChild(sel *goquery.Selection) bool {
	found := false
	sel.Children().EachWithBreak(func(_ int, child *goquery.Selection) bool {
		switch goquery.NodeName(child) {
		case "p", "div", "ul", "ol", "blockquote",
			"h1", "h2", "h3", "h4", "h5", "h6", "img", "figure", "table":
			found = true
			return false
		}
		return true
	})
	return found
}

func parseInline(sel *goquery.Selection, base *url.URL) []Inline {
	var out []Inline

	var walk func(*goquery.Selection, Inline)
	walk = func(s *goquery.Selection, style Inline) {
		s.Contents().Each(func(_ int, node *goquery.Selection) {
			if len(out) >= maxInline {
				return
			}
			n := node.Get(0)
			if n == nil {
				return
			}
			if n.Type == html.TextNode {
				if text := normText(n.Data); text != "" {
					st := style
					st.Text = text
					out = append(out, st)
				}
				return
			}
			if n.Type != html.ElementNode {
				return
			}
			switch goquery.NodeName(node) {
			case "script", "style", "noscript", "iframe", "svg":
				return
			case "br":
				out = append(out, Inline{Break: true})
			case "b", "strong":
				st := style
				st.Bold = true
				walk(node, st)
			case "i", "em":
				st := style
				st.Italic = true
				walk(node, st)
			case "u", "ins":
				st := style
				st.Underline = true
				walk(node, st)
			case "s", "del", "strike":
				st := style
				st.Strike = true
				walk(node, st)
			case "code":
				st := style
				st.Code = true
				walk(node, st)
			case "a":
				st := style
				href, _ := node.Attr("href")
				st.Href = absURL(base, href)
				walk(node, st)
			case "img":
				src, _ := node.Attr("src")
				if strings.Contains(src, "/smilies/") {
					return
				}
				if alt, _ := node.Attr("alt"); strings.TrimSpace(alt) != "" {
					st := style
					st.Text = alt
					out = append(out, st)
				}
			default:
				walk(node, style)
			}
		})
	}
	walk(sel, Inline{})
	return trimInline(out)
}

func trimInline(in []Inline) []Inline {
	// Drop empty runs and trim the outer whitespace of the paragraph.
	out := in[:0]
	for _, run := range in {
		if run.Break {
			out = append(out, run)
			continue
		}
		if run.Text == "" {
			continue
		}
		out = append(out, run)
	}
	if len(out) == 0 {
		return nil
	}
	if !out[0].Break {
		out[0].Text = strings.TrimLeftFunc(out[0].Text, unicode.IsSpace)
	}
	if last := len(out) - 1; !out[last].Break {
		out[last].Text = strings.TrimRightFunc(out[last].Text, unicode.IsSpace)
	}
	// Remove runs that became empty at the edges.
	for len(out) > 0 && !out[0].Break && out[0].Text == "" {
		out = out[1:]
	}
	for len(out) > 0 {
		last := len(out) - 1
		if out[last].Break || out[last].Text != "" {
			break
		}
		out = out[:last]
	}
	return out
}

func collectLinks(blocks []Block, selfID int, base *url.URL) []Candidate {
	seen := map[int]bool{}
	var out []Candidate
	add := func(in Inline) {
		if in.Href == "" || len(out) >= maxLinks {
			return
		}
		id := idFromURL(in.Href)
		if id == 0 || id == selfID || seen[id] {
			return
		}
		seen[id] = true
		title := strings.TrimSpace(in.Text)
		if title == "" {
			title = titleFromURL(in.Href)
		}
		out = append(out, Candidate{ID: id, Title: title, URL: absURL(base, in.Href)})
	}
	for _, b := range blocks {
		for _, in := range b.Inline {
			add(in)
		}
		for _, item := range b.Items {
			for _, in := range item {
				add(in)
			}
		}
	}
	return out
}

func pageCount(doc *goquery.Document) int {
	max := 1
	doc.Find(".pageNav-page a, .pageNav-page--current a, .pageNav-page--current").Each(func(_ int, s *goquery.Selection) {
		n, err := strconv.Atoi(strings.TrimSpace(s.Text()))
		if err == nil && n > max {
			max = n
		}
	})
	return max
}

func iconFor(base *url.URL, id int) string {
	if id == 0 {
		return ""
	}
	u, err := base.Parse("/data/resource_icons/" + strconv.Itoa(id/1000) + "/" + strconv.Itoa(id) + ".jpg")
	if err != nil {
		return ""
	}
	return u.String()
}

func titleFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	slug := strings.Trim(u.Path, "/")
	if i := strings.LastIndex(slug, "/"); i >= 0 {
		slug = slug[i+1:]
	}
	if i := strings.LastIndex(slug, "."); i > 0 {
		slug = slug[:i]
	}
	return strings.TrimSpace(strings.ReplaceAll(slug, "-", " "))
}

func attr(s *goquery.Selection, name string) string {
	v, _ := s.Attr(name)
	return strings.TrimSpace(v)
}

func absURL(base *url.URL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	if !u.IsAbs() {
		u = base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.String()
}

func imageURL(s *goquery.Selection, base *url.URL) string {
	for _, name := range []string{"data-src", "data-url", "src"} {
		raw := attr(s, name)
		if raw == "" {
			continue
		}
		if strings.Contains(raw, "/smilies/") {
			return ""
		}
		if u := absURL(base, raw); u != "" {
			return u
		}
	}
	return ""
}

func normText(s string) string {
	s = strings.ReplaceAll(s, "\u00a0", " ")
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == '\u200b' || r == '\u200c' || r == '\u200d' || r == '\ufeff' {
			continue
		}
		if unicode.IsSpace(r) {
			if !space {
				b.WriteByte(' ')
				space = true
			}
			continue
		}
		b.WriteRune(r)
		space = false
	}
	return b.String()
}

func parseCount(s string) int {
	lower := strings.ToLower(s)
	mult := 1.0
	if strings.Contains(lower, "k") {
		mult = 1e3
	} else if strings.Contains(lower, "m") {
		mult = 1e6
	}
	num := numberRe.FindString(strings.ReplaceAll(lower, ",", ""))
	if num == "" {
		return 0
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0
	}
	return int(f * mult)
}

var numberRe = regexp.MustCompile(`\d+(?:[.,]\d+)?`)

func parseRating(s string) float64 {
	m := numberRe.FindString(s)
	if m == "" {
		return 0
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(m, ",", "."), 64)
	if err != nil {
		return 0
	}
	return f
}

const (
	maxBlocks = 160
	maxImages = 10
	maxInline = 240
	maxLinks  = 10
)

var catHrefRe = regexp.MustCompile(`/(?:mods/)?categories/([^/]+)\.(\d+)/`)
