package notion

import (
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ParseInline converts Notion-flavored inline Markdown into rich text.
func ParseInline(s string) []any {
	return EncodeRich(parseInline(s, nil))
}

func withDeco(decos [][]any, d ...any) [][]any {
	out := make([][]any, len(decos), len(decos)+1)
	copy(out, decos)
	return append(out, d)
}

func isPunct(r rune) bool {
	return r < 128 && unicode.IsPunct(r) || r == '$' || r == '<' || r == '>' || r == '|' || r == '~' || r == '`'
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

var attrRe = regexp.MustCompile(`([a-zA-Z_-]+)\s*=\s*(?:"([^"]*)"|'([^']*)')`)

func parseAttrs(s string) map[string]string {
	m := map[string]string{}
	for _, a := range attrRe.FindAllStringSubmatch(s, -1) {
		v := a[2]
		if v == "" {
			v = a[3]
		}
		m[strings.ToLower(a[1])] = html.UnescapeString(v)
	}
	return m
}

// findClose finds the index of delim in s starting at i, skipping escapes and code spans.
func findClose(s string, i int, delim string, wordBoundary bool) int {
	for i < len(s) {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			i += 2
			continue
		case s[i] == '`' && delim != "`":
			n := runLen(s, i, '`')
			if j := strings.Index(s[i+n:], strings.Repeat("`", n)); j >= 0 {
				i += n + j + n
				continue
			}
		case strings.HasPrefix(s[i:], delim):
			if delim == "*" && strings.HasPrefix(s[i:], "**") {
				// skip a nested bold run entirely
				if j := findClose(s, i+2, "**", false); j >= 0 {
					i = j + 2
					continue
				}
			}
			if wordBoundary {
				next, _ := utf8.DecodeRuneInString(s[i+len(delim):])
				if i+len(delim) < len(s) && isWordRune(next) {
					i++
					continue
				}
			}
			if i > 0 && (s[i-1] == ' ' || s[i-1] == '\t') && delim != "`" {
				i++ // closing delimiters can't follow whitespace
				continue
			}
			return i
		}
		i++
	}
	return -1
}

func runLen(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

// findTagClose finds the matching </name> for an element whose content starts at i.
func findTagClose(s string, i int, name string) (contentEnd, after int) {
	depth := 1
	open := "<" + name
	closeTag := "</" + name + ">"
	for i < len(s) {
		if s[i] == '\\' {
			i += 2
			continue
		}
		if strings.HasPrefix(s[i:], closeTag) {
			depth--
			if depth == 0 {
				return i, i + len(closeTag)
			}
			i += len(closeTag)
			continue
		}
		if strings.HasPrefix(s[i:], open) && i+len(open) < len(s) && (s[i+len(open)] == ' ' || s[i+len(open)] == '>') {
			depth++
		}
		i++
	}
	return -1, -1
}

var tagNameRe = regexp.MustCompile(`^<([a-zA-Z][a-zA-Z0-9_-]*)((?:\s+[a-zA-Z_-]+\s*=\s*(?:"[^"]*"|'[^']*'))*)\s*(/?)>`)

func parseInline(s string, decos [][]any) []Segment {
	var segs []Segment
	var buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			segs = append(segs, Segment{Text: buf.String(), Decos: decos})
			buf.Reset()
		}
	}
	emit := func(inner []Segment) {
		flush()
		segs = append(segs, inner...)
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			r, n := utf8.DecodeRuneInString(s[i+1:])
			if isPunct(r) {
				buf.WriteRune(r)
				i += 1 + n
				continue
			}
		case c == '$' && strings.HasPrefix(s[i:], "$`"):
			if j := strings.Index(s[i+2:], "`$"); j >= 0 {
				flush()
				segs = append(segs, Segment{Text: "⁍", Decos: withDeco(decos, "e", s[i+2:i+2+j])})
				i += 2 + j + 2
				continue
			}
		case c == '`':
			n := runLen(s, i, '`')
			fence := strings.Repeat("`", n)
			if j := strings.Index(s[i+n:], fence); j >= 0 {
				code := s[i+n : i+n+j]
				if len(code) >= 2 && code[0] == ' ' && code[len(code)-1] == ' ' && strings.Trim(code, " ") != "" {
					code = code[1 : len(code)-1]
				}
				flush()
				segs = append(segs, Segment{Text: code, Decos: withDeco(decos, "c")})
				i += n + j + n
				continue
			}
		case c == '*' && strings.HasPrefix(s[i:], "**"):
			if j := findClose(s, i+2, "**", false); j > i+2 {
				emit(parseInline(s[i+2:j], withDeco(decos, "b")))
				i = j + 2
				continue
			}
		case c == '~' && strings.HasPrefix(s[i:], "~~"):
			if j := findClose(s, i+2, "~~", false); j > i+2 {
				emit(parseInline(s[i+2:j], withDeco(decos, "s")))
				i = j + 2
				continue
			}
		case c == '*':
			if i+1 < len(s) && s[i+1] != ' ' {
				if j := findClose(s, i+1, "*", false); j > i+1 {
					emit(parseInline(s[i+1:j], withDeco(decos, "i")))
					i = j + 1
					continue
				}
			}
		case c == '_':
			prev, _ := utf8.DecodeLastRuneInString(s[:i])
			if (i == 0 || !isWordRune(prev)) && i+1 < len(s) && s[i+1] != ' ' {
				if j := findClose(s, i+1, "_", true); j > i+1 {
					emit(parseInline(s[i+1:j], withDeco(decos, "i")))
					i = j + 1
					continue
				}
			}
		case c == '[':
			if end, url, after, ok := parseLink(s, i); ok {
				emit(parseInline(s[i+1:end], withDeco(decos, "a", url)))
				i = after
				continue
			}
		case c == '<':
			if segsTag, after, ok := parseInlineTag(s, i, decos); ok {
				emit(segsTag)
				i = after
				continue
			}
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		buf.WriteRune(r)
		i += n
	}
	flush()
	return segs
}

// parseLink parses [text](url) at s[i]; returns end of text, url, index after ')'.
func parseLink(s string, i int) (textEnd int, url string, after int, ok bool) {
	depth := 0
	j := i
	for ; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
			continue
		case '`':
			n := runLen(s, j, '`')
			if k := strings.Index(s[j+n:], strings.Repeat("`", n)); k >= 0 {
				j += n + k + n - 1
				continue
			}
		case '[':
			depth++
		case ']':
			depth--
		}
		if depth == 0 {
			break
		}
	}
	if j >= len(s) || j+1 >= len(s) || s[j+1] != '(' {
		return 0, "", 0, false
	}
	k := j + 2
	paren := 1
	for ; k < len(s); k++ {
		if s[k] == '(' {
			paren++
		} else if s[k] == ')' {
			paren--
			if paren == 0 {
				break
			}
		}
	}
	if k >= len(s) {
		return 0, "", 0, false
	}
	url = strings.TrimSpace(s[j+2 : k])
	if strings.HasPrefix(url, "<") && strings.HasSuffix(url, ">") {
		url = url[1 : len(url)-1]
	}
	if sp := strings.Index(url, " \""); sp > 0 { // [x](url "title")
		url = url[:sp]
	}
	return j, url, k + 1, true
}

func parseInlineTag(s string, i int, decos [][]any) ([]Segment, int, bool) {
	rest := s[i:]
	if strings.HasPrefix(rest, "<br>") || strings.HasPrefix(rest, "<br/>") || strings.HasPrefix(rest, "<br />") {
		n := strings.Index(rest, ">") + 1
		return []Segment{{Text: "\n", Decos: decos}}, i + n, true
	}
	if strings.HasPrefix(rest, "<http://") || strings.HasPrefix(rest, "<https://") {
		if j := strings.IndexByte(rest, '>'); j > 0 && !strings.ContainsAny(rest[1:j], " <") {
			u := rest[1:j]
			return []Segment{{Text: u, Decos: withDeco(decos, "a", u)}}, i + j + 1, true
		}
	}
	m := tagNameRe.FindStringSubmatch(rest)
	if m == nil {
		return nil, 0, false
	}
	name, attrs, selfClose := strings.ToLower(m[1]), parseAttrs(m[2]), m[3] == "/"
	contentStart := i + len(m[0])
	inner, after := "", contentStart
	if !selfClose {
		end, aft := findTagClose(s, contentStart, m[1])
		if end < 0 {
			if name != "mention-date" {
				return nil, 0, false
			}
		} else {
			inner, after = s[contentStart:end], aft
		}
	}
	switch name {
	case "span":
		d := decos
		if c := attrs["color"]; c != "" {
			d = withDeco(d, "h", c)
		}
		if attrs["underline"] == "true" {
			d = withDeco(d, "_")
		}
		return parseInline(inner, d), after, true
	case "u":
		return parseInline(inner, withDeco(decos, "_")), after, true
	case "b", "strong":
		return parseInline(inner, withDeco(decos, "b")), after, true
	case "i", "em":
		return parseInline(inner, withDeco(decos, "i")), after, true
	case "s", "del":
		return parseInline(inner, withDeco(decos, "s")), after, true
	case "code":
		return []Segment{{Text: html.UnescapeString(inner), Decos: withDeco(decos, "c")}}, after, true
	case "mention-page", "mention-database", "link-to-page":
		id, _, _, err := ParseRef(attrs["url"])
		if err != nil {
			return nil, 0, false
		}
		return []Segment{{Text: "‣", Decos: withDeco(decos, "p", id)}}, after, true
	case "mention-user":
		id := NormalizeID(strings.TrimPrefix(attrs["url"], "user://"))
		if id == "" {
			return nil, 0, false
		}
		return []Segment{{Text: "‣", Decos: withDeco(decos, "u", id)}}, after, true
	case "mention-date":
		d := DateValue(attrs["start"], attrs["end"], attrs["timezone"])
		if d == nil {
			return nil, 0, false
		}
		return []Segment{{Text: "‣", Decos: withDeco(decos, "d", d)}}, after, true
	}
	return nil, 0, false
}

// DateValue builds Notion's internal date object from ISO-ish strings
// ("2025-01-02", "2025-01-02T10:00", "2025-01-02 10:00").
func DateValue(start, end, tz string) map[string]any {
	sd, stime := splitDT(start)
	if sd == "" {
		return nil
	}
	d := map[string]any{"start_date": sd}
	typ := "date"
	if stime != "" {
		d["start_time"] = stime
		typ = "datetime"
	}
	if end != "" {
		ed, etime := splitDT(end)
		d["end_date"] = ed
		if etime != "" {
			d["end_time"] = etime
		}
		typ += "range"
	}
	if tz != "" {
		d["time_zone"] = tz
	}
	d["type"] = typ
	return d
}

func splitDT(s string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	sep := strings.IndexAny(s, "T ")
	if sep < 0 {
		return s, ""
	}
	t := s[sep+1:]
	// strip seconds / zone suffix: 10:00:00Z -> 10:00
	if len(t) >= 5 {
		t = t[:5]
	}
	return s[:sep], t
}
