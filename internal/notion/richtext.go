package notion

import (
	"encoding/json"
	"fmt"
	"html"
	"sort"
	"strings"
)

// Rich text in Notion's internal format is [[text, [[decorator, arg...], ...]], ...].
//
// Decorators handled:
//   b bold, i italic, s strike, c code, _ underline, a link, h color,
//   e inline equation, p page mention, u user mention, d date mention,
//   m comment marker, lm link mention, eoi external object, tag, si, sa.

// Segment is a decoded rich-text run.
type Segment struct {
	Text  string
	Decos [][]any
}

func DecodeRich(v []any) []Segment {
	segs := make([]Segment, 0, len(v))
	for _, x := range v {
		arr, ok := x.([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		s := Segment{Text: str(arr[0])}
		if len(arr) > 1 {
			if ds, ok := arr[1].([]any); ok {
				for _, d := range ds {
					if da, ok := d.([]any); ok && len(da) > 0 {
						s.Decos = append(s.Decos, da)
					}
				}
			}
		}
		segs = append(segs, s)
	}
	return segs
}

func EncodeRich(segs []Segment) []any {
	out := make([]any, 0, len(segs))
	for _, s := range segs {
		if s.Text == "" {
			continue
		}
		if len(s.Decos) == 0 {
			out = append(out, []any{s.Text})
			continue
		}
		ds := make([]any, len(s.Decos))
		for i, d := range s.Decos {
			ds[i] = d
		}
		out = append(out, []any{s.Text, ds})
	}
	// merge adjacent runs with identical decorations
	merged := make([]any, 0, len(out))
	for _, x := range out {
		if n := len(merged); n > 0 {
			prev := merged[n-1].([]any)
			cur := x.([]any)
			if decoKey(prev) == decoKey(cur) && !hasEntityDeco(cur) {
				prev[0] = prev[0].(string) + cur[0].(string)
				continue
			}
		}
		merged = append(merged, x)
	}
	return merged
}

func decoKey(run []any) string {
	if len(run) < 2 {
		return ""
	}
	b, _ := json.Marshal(run[1])
	return string(b)
}

func hasEntityDeco(run []any) bool {
	if len(run) < 2 {
		return false
	}
	for _, d := range run[1].([]any) {
		switch str(d.([]any)[0]) {
		case "p", "u", "d", "e", "lm", "eoi":
			return true
		}
	}
	return false
}

func (s Segment) deco(name string) ([]any, bool) {
	for _, d := range s.Decos {
		if str(d[0]) == name {
			return d, true
		}
	}
	return nil, false
}

// Resolver provides display names for mentions.
type Resolver interface {
	PageTitle(id string) string
	UserName(id string) string
	PageURL(id string) string
}

// PlainText concatenates rich text without formatting (mentions become their titles if known).
func PlainText(v []any) string {
	var b strings.Builder
	for _, s := range DecodeRich(v) {
		if d, ok := s.deco("e"); ok && len(d) > 1 {
			b.WriteString(str(d[1]))
			continue
		}
		if d, ok := s.deco("d"); ok && len(d) > 1 {
			b.WriteString(formatDate(d[1]))
			continue
		}
		b.WriteString(s.Text)
	}
	return b.String()
}

func PlainTextR(v []any, r Resolver) string {
	var b strings.Builder
	for _, s := range DecodeRich(v) {
		switch {
		case has(s, "p") && r != nil:
			d, _ := s.deco("p")
			b.WriteString(r.PageTitle(str(d[1])))
		case has(s, "u") && r != nil:
			d, _ := s.deco("u")
			b.WriteString("@" + r.UserName(str(d[1])))
		case has(s, "e"):
			d, _ := s.deco("e")
			b.WriteString(str(d[1]))
		case has(s, "d"):
			d, _ := s.deco("d")
			b.WriteString(formatDate(d[1]))
		default:
			b.WriteString(s.Text)
		}
	}
	return b.String()
}

func has(s Segment, name string) bool { _, ok := s.deco(name); return ok }

// RichToMarkdown renders rich text as Notion-flavored Markdown.
func RichToMarkdown(v []any, r Resolver) string {
	var b strings.Builder
	for _, s := range DecodeRich(v) {
		b.WriteString(segmentToMarkdown(s, r))
	}
	return b.String()
}

var mdEscaper = strings.NewReplacer(`\`, `\\`, `*`, `\*`, "`", "\\`", `~~`, `\~~`, `[`, `\[`, `]`, `\]`, `<`, `\<`, `$`, `\$`)

// EscapeMarkdown escapes characters that the parser would treat as markup.
func EscapeMarkdown(s string) string { return mdEscaper.Replace(s) }

func segmentToMarkdown(s Segment, r Resolver) string {
	// Entities first: they replace the placeholder text ("‣" / "⁍").
	if d, ok := s.deco("p"); ok && len(d) > 1 {
		id := str(d[1])
		title := ""
		url := "https://www.notion.so/" + CompactID(id)
		if r != nil {
			title = r.PageTitle(id)
			url = r.PageURL(id)
		}
		return fmt.Sprintf(`<mention-page url="%s">%s</mention-page>`, url, html.EscapeString(title))
	}
	if d, ok := s.deco("u"); ok && len(d) > 1 {
		id := str(d[1])
		name := ""
		if r != nil {
			name = r.UserName(id)
		}
		return fmt.Sprintf(`<mention-user url="user://%s">%s</mention-user>`, id, html.EscapeString(name))
	}
	if d, ok := s.deco("d"); ok && len(d) > 1 {
		return dateMentionTag(d[1])
	}
	if d, ok := s.deco("lm"); ok && len(d) > 1 {
		m, _ := d[1].(map[string]any)
		href := str(m["href"])
		title := str(m["title"])
		if title == "" {
			title = href
		}
		return fmt.Sprintf("[%s](%s)", EscapeMarkdown(title), href)
	}
	if d, ok := s.deco("eoi"); ok && len(d) > 1 {
		return fmt.Sprintf(`<mention-external id="%s"/>`, str(d[1]))
	}

	text := s.Text
	var code bool
	if d, ok := s.deco("e"); ok && len(d) > 1 {
		text = "$`" + str(d[1]) + "`$"
	} else if _, ok := s.deco("c"); ok {
		code = true
		text = codeSpan(text)
	} else {
		text = EscapeMarkdown(text)
		text = strings.ReplaceAll(text, "\n", "<br>")
	}
	_ = code

	// Apply wrappers; whitespace stays outside markers so Markdown stays valid.
	lead, core, trail := splitSpace(text)
	if core == "" {
		return text
	}
	if _, ok := s.deco("b"); ok {
		core = "**" + core + "**"
	}
	if _, ok := s.deco("i"); ok {
		core = "*" + core + "*"
	}
	if _, ok := s.deco("s"); ok {
		core = "~~" + core + "~~"
	}
	if _, ok := s.deco("_"); ok {
		core = `<span underline="true">` + core + "</span>"
	}
	if d, ok := s.deco("h"); ok && len(d) > 1 && !strings.HasPrefix(str(d[1]), "default") {
		core = fmt.Sprintf(`<span color="%s">%s</span>`, str(d[1]), core)
	}
	if d, ok := s.deco("a"); ok && len(d) > 1 {
		href := str(d[1])
		if strings.HasPrefix(href, "/") {
			href = "https://www.notion.so" + href
		}
		core = "[" + core + "](" + strings.ReplaceAll(href, " ", "%20") + ")"
	}
	if d, ok := s.deco("m"); ok && len(d) > 1 {
		core = fmt.Sprintf(`<span discussion-urls="discussion://%s">%s</span>`, str(d[1]), core)
	}
	return lead + core + trail
}

func codeSpan(s string) string {
	fence := "`"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		return fence + " " + s + " " + fence
	}
	return fence + s + fence
}

func splitSpace(s string) (lead, core, trail string) {
	core = strings.TrimLeft(s, " \t")
	lead = s[:len(s)-len(core)]
	t := strings.TrimRight(core, " \t")
	trail = core[len(t):]
	return lead, t, trail
}

func dateMentionTag(v any) string {
	m, _ := v.(map[string]any)
	if m == nil {
		return ""
	}
	attrs := []string{}
	start := str(m["start_date"])
	if t := str(m["start_time"]); t != "" {
		start += "T" + t
	}
	attrs = append(attrs, fmt.Sprintf(`start="%s"`, start))
	if e := str(m["end_date"]); e != "" {
		if t := str(m["end_time"]); t != "" {
			e += "T" + t
		}
		attrs = append(attrs, fmt.Sprintf(`end="%s"`, e))
	}
	if tz := str(m["time_zone"]); tz != "" {
		attrs = append(attrs, fmt.Sprintf(`timezone="%s"`, tz))
	}
	if r, ok := m["reminder"].(map[string]any); ok {
		b, _ := json.Marshal(r)
		attrs = append(attrs, fmt.Sprintf(`reminder='%s'`, b))
	}
	return "<mention-date " + strings.Join(attrs, " ") + "/>"
}

func formatDate(v any) string {
	m, _ := v.(map[string]any)
	if m == nil {
		return ""
	}
	s := str(m["start_date"])
	if t := str(m["start_time"]); t != "" {
		s += " " + t
	}
	if e := str(m["end_date"]); e != "" {
		s += " → " + e
		if t := str(m["end_time"]); t != "" {
			s += " " + t
		}
	}
	if tz := str(m["time_zone"]); tz != "" {
		s += " (" + tz + ")"
	}
	return s
}

// MentionIDs returns page and user IDs referenced from rich text.
func MentionIDs(v []any) (pages, users []string) {
	for _, s := range DecodeRich(v) {
		if d, ok := s.deco("p"); ok && len(d) > 1 {
			pages = append(pages, str(d[1]))
		}
		if d, ok := s.deco("u"); ok && len(d) > 1 {
			users = append(users, str(d[1]))
		}
	}
	return
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
