package notion

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
)

// BlockSpec is a block parsed from Markdown, before it is written.
type BlockSpec struct {
	Type     string         `json:"type"`
	Props    map[string]any `json:"properties,omitempty"`
	Format   map[string]any `json:"format,omitempty"`
	Children []*BlockSpec   `json:"children,omitempty"`
	// RefID is an existing block this spec refers to (sub-page, database,
	// synced block, unknown block). It is kept as-is instead of recreated.
	RefID string `json:"ref_id,omitempty"`
	// LocalFile is a local path to upload for media blocks.
	LocalFile string `json:"local_file,omitempty"`
	// Truncated marks "<truncated/>": keep the existing children of the parent.
	Truncated bool `json:"truncated,omitempty"`
	// Table cells (for type "table"): rows of rich text cells.
	Rows [][][]any `json:"rows,omitempty"`

	fileID string // set after uploading LocalFile
}

type mdLine struct {
	indent int
	text   string // without indentation
	raw    string
}

func splitLines(md string) []mdLine {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	var out []mdLine
	for _, raw := range strings.Split(md, "\n") {
		ind := 0
		i := 0
		for ; i < len(raw); i++ {
			if raw[i] == '\t' {
				ind += 4
			} else if raw[i] == ' ' {
				ind++
			} else {
				break
			}
		}
		text := strings.TrimRight(raw[i:], " \t")
		if text == "" {
			ind = 0
		}
		out = append(out, mdLine{indent: ind, text: text, raw: raw})
	}
	return out
}

// dedent removes `n` columns of indentation (tabs count as 4) from raw lines.
func dedentRaw(raw string, n int) string {
	col := 0
	i := 0
	for i < len(raw) && col < n {
		if raw[i] == '\t' {
			col += 4
		} else if raw[i] == ' ' {
			col++
		} else {
			break
		}
		i++
	}
	return raw[i:]
}

// ParseMarkdown parses Notion-flavored Markdown into block specs.
func ParseMarkdown(md string) ([]*BlockSpec, error) {
	md = stripFrontMatter(md)
	return parseBlocks(splitLines(md))
}

func stripFrontMatter(md string) string {
	if !strings.HasPrefix(md, "---\n") {
		return md
	}
	rest := md[4:]
	// Only strip if it looks like YAML (key: value lines) up to the closing ---.
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		if strings.HasSuffix(rest, "\n---") {
			end = len(rest) - 4
		} else {
			return md
		}
	}
	for _, l := range strings.Split(rest[:end], "\n") {
		if l != "" && !strings.Contains(l, ":") {
			return md
		}
	}
	if end+5 > len(rest) {
		return ""
	}
	return rest[end+5:]
}

// FrontMatter returns the raw front matter lines of a Markdown document.
func FrontMatter(md string) (fields []string, body string) {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	b := stripFrontMatter(md)
	if b == md {
		return nil, md
	}
	fm := strings.TrimPrefix(md, "---\n")
	fm = fm[:len(fm)-len(b)]
	fm = strings.TrimSuffix(strings.TrimSuffix(fm, "\n"), "---")
	for _, l := range strings.Split(fm, "\n") {
		if strings.TrimSpace(l) != "" {
			fields = append(fields, l)
		}
	}
	return fields, b
}

var (
	headingRe   = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	todoRe      = regexp.MustCompile(`^[-*+]\s+\[([ xX])\]\s?(.*)$`)
	bulletRe    = regexp.MustCompile(`^[-*+]\s+(.*)$`)
	numberRe    = regexp.MustCompile(`^(\d+)[.)]\s+(.*)$`)
	imageRe     = regexp.MustCompile(`^!\[(.*)\]\((.*)\)$`)
	trailAttrRe = regexp.MustCompile(`\s*\{((?:\s*[a-zA-Z_-]+\s*=\s*"[^"]*")+)\s*\}$`)
	openTagRe   = regexp.MustCompile(`^<([a-zA-Z_][a-zA-Z0-9_-]*)((?:\s+[a-zA-Z_-]+\s*=\s*(?:"[^"]*"|'[^']*'))*)\s*(/?)>`)
	tdRe        = regexp.MustCompile(`(?s)<t[dh](?:\s[^>]*)?>(.*?)</t[dh]>`)
)

var containerTags = map[string]bool{
	"details": true, "callout": true, "columns": true, "column": true,
	"synced_block": true, "synced_block_reference": true, "table": true,
}

func parseBlocks(lines []mdLine) ([]*BlockSpec, error) {
	var out []*BlockSpec
	i := 0
	for i < len(lines) {
		l := lines[i]
		if l.text == "" {
			i++
			continue
		}
		base := l.indent
		t := l.text

		// fenced code
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenceChar := t[0]
			n := runLen(t, 0, fenceChar)
			fence := strings.Repeat(string(fenceChar), n)
			lang := strings.TrimSpace(t[n:])
			var code []string
			j := i + 1
			for ; j < len(lines); j++ {
				if strings.HasPrefix(lines[j].text, fence) && strings.Trim(lines[j].text, string(fenceChar)) == "" {
					break
				}
				code = append(code, dedentRaw(lines[j].raw, base))
			}
			out = append(out, codeSpec(strings.Join(code, "\n"), lang))
			i = j + 1
			if i < len(lines) && strings.HasPrefix(lines[i].text, "<caption>") {
				cap := strings.TrimSuffix(strings.TrimPrefix(lines[i].text, "<caption>"), "</caption>")
				out[len(out)-1].Props["caption"] = ParseInline(cap)
				i++
			}
			continue
		}
		// block equation
		if t == "$$" || (strings.HasPrefix(t, "$$") && strings.HasSuffix(t, "$$") && len(t) > 4) {
			if t != "$$" {
				out = append(out, &BlockSpec{Type: "equation", Props: map[string]any{"title": []any{[]any{strings.TrimSpace(t[2 : len(t)-2])}}}})
				i++
				continue
			}
			var eq []string
			j := i + 1
			for ; j < len(lines) && lines[j].text != "$$"; j++ {
				eq = append(eq, dedentRaw(lines[j].raw, base))
			}
			out = append(out, &BlockSpec{Type: "equation", Props: map[string]any{"title": []any{[]any{strings.Join(eq, "\n")}}}})
			i = j + 1
			continue
		}
		// container tags spanning multiple lines
		if m := openTagRe.FindStringSubmatch(t); m != nil && containerTags[strings.ToLower(m[1])] && m[3] != "/" {
			name := strings.ToLower(m[1])
			closeTag := "</" + m[1] + ">"
			attrs := parseAttrs(m[2])
			rest := t[len(m[0]):]
			var inner []mdLine
			j := i
			if strings.HasSuffix(rest, closeTag) && countTag(rest, m[1]) == 0 {
				// single-line container
				inner = splitLines(strings.TrimSuffix(rest, closeTag))
				j = i + 1
			} else {
				depth := 1
				if rest != "" {
					inner = append(inner, mdLine{indent: base + 4, text: rest, raw: strings.Repeat(" ", base+4) + rest})
				}
				for j = i + 1; j < len(lines); j++ {
					lt := lines[j].text
					depth += countTag(lt, m[1])
					if strings.HasSuffix(lt, closeTag) {
						depth--
						if depth == 0 {
							before := strings.TrimSuffix(lt, closeTag)
							if strings.TrimSpace(before) != "" {
								inner = append(inner, mdLine{indent: lines[j].indent, text: before, raw: strings.TrimSuffix(lines[j].raw, closeTag)})
							}
							break
						}
					}
					inner = append(inner, lines[j])
				}
				if j >= len(lines) {
					return nil, fmt.Errorf("line %d: <%s> is not closed", i+1, m[1])
				}
				j++
			}
			spec, err := containerSpec(name, attrs, dedentLines(inner))
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
			out = append(out, spec)
			i = j
			continue
		}
		// GFM table
		if strings.HasPrefix(t, "|") {
			var rows []string
			j := i
			for ; j < len(lines) && strings.HasPrefix(lines[j].text, "|"); j++ {
				rows = append(rows, lines[j].text)
			}
			out = append(out, gfmTableSpec(rows))
			i = j
			continue
		}
		// quote: merge consecutive "> " lines
		if t == ">" || strings.HasPrefix(t, "> ") {
			var parts []string
			j := i
			for ; j < len(lines) && lines[j].indent == base && (lines[j].text == ">" || strings.HasPrefix(lines[j].text, "> ")); j++ {
				parts = append(parts, strings.TrimPrefix(strings.TrimPrefix(lines[j].text, ">"), " "))
			}
			text := strings.Join(parts, "<br>")
			spec := &BlockSpec{Type: "quote"}
			text = applyTrailAttrs(spec, text)
			spec.Props = map[string]any{"title": ParseInline(text)}
			children, k := childLines(lines, j, base)
			if len(children) > 0 {
				c, err := parseBlocks(dedentLines(children))
				if err != nil {
					return nil, err
				}
				spec.Children = c
			}
			out = append(out, spec)
			i = k
			continue
		}

		spec := lineSpec(t)
		children, j := childLines(lines, i+1, base)
		if len(children) > 0 {
			c, err := parseBlocks(dedentLines(children))
			if err != nil {
				return nil, err
			}
			spec.Children = append(spec.Children, c...)
		}
		out = append(out, spec)
		i = j
	}
	return out, nil
}

func countTag(s, name string) int {
	n := 0
	open := "<" + name
	for idx := 0; ; {
		k := strings.Index(s[idx:], open)
		if k < 0 {
			return n
		}
		p := idx + k + len(open)
		if p < len(s) && (s[p] == ' ' || s[p] == '>') {
			n++
		}
		idx = p
	}
}

// childLines returns the indented lines following index i that belong to a block at `base`.
func childLines(lines []mdLine, i, base int) ([]mdLine, int) {
	j := i
	last := i
	for ; j < len(lines); j++ {
		if lines[j].text == "" {
			continue
		}
		if lines[j].indent <= base {
			break
		}
		last = j + 1
	}
	if last <= i {
		return nil, i
	}
	return lines[i:last], last
}

func dedentLines(lines []mdLine) []mdLine {
	minInd := -1
	for _, l := range lines {
		if l.text != "" && (minInd < 0 || l.indent < minInd) {
			minInd = l.indent
		}
	}
	if minInd <= 0 {
		return lines
	}
	out := make([]mdLine, len(lines))
	for i, l := range lines {
		if l.text == "" {
			out[i] = l
			continue
		}
		raw := dedentRaw(l.raw, minInd)
		out[i] = mdLine{indent: l.indent - minInd, text: l.text, raw: raw}
	}
	return out
}

func codeSpec(code, lang string) *BlockSpec {
	return &BlockSpec{Type: "code", Props: map[string]any{
		"title":    []any{[]any{code}},
		"language": []any{[]any{codeLanguage(lang)}},
	}}
}

var codeLangs = map[string]string{
	"": "Plain Text", "text": "Plain Text", "txt": "Plain Text", "plaintext": "Plain Text", "plain text": "Plain Text",
	"js": "JavaScript", "javascript": "JavaScript", "ts": "TypeScript", "typescript": "TypeScript",
	"py": "Python", "python": "Python", "go": "Go", "golang": "Go", "rs": "Rust", "rust": "Rust",
	"sh": "Shell", "shell": "Shell", "bash": "Bash", "zsh": "Shell", "json": "JSON", "yaml": "YAML", "yml": "YAML",
	"html": "HTML", "css": "CSS", "scss": "SCSS", "sql": "SQL", "java": "Java", "kotlin": "Kotlin", "kt": "Kotlin",
	"swift": "Swift", "c": "C", "cpp": "C++", "c++": "C++", "cs": "C#", "csharp": "C#", "c#": "C#", "rb": "Ruby", "ruby": "Ruby",
	"php": "PHP", "md": "Markdown", "markdown": "Markdown", "mermaid": "Mermaid", "diff": "Diff", "dockerfile": "Docker",
	"docker": "Docker", "graphql": "GraphQL", "xml": "XML", "toml": "TOML", "lua": "Lua", "r": "R", "scala": "Scala",
	"dart": "Dart", "elixir": "Elixir", "haskell": "Haskell", "makefile": "Makefile", "objective-c": "Objective-C",
	"perl": "Perl", "powershell": "PowerShell", "latex": "LaTeX", "tex": "LaTeX", "jsx": "JavaScript", "tsx": "TypeScript",
	"vb.net": "VB.Net", "f#": "F#", "clojure": "Clojure", "coffeescript": "CoffeeScript", "erlang": "Erlang",
	"protobuf": "Protobuf", "sass": "Sass", "less": "Less", "solidity": "Solidity", "nix": "Nix", "zig": "Zig",
}

func codeLanguage(lang string) string {
	l := strings.ToLower(strings.TrimSpace(lang))
	if v, ok := codeLangs[l]; ok {
		return v
	}
	return lang
}

// applyTrailAttrs strips a trailing {color="red" toggle="true"} and applies it.
func applyTrailAttrs(spec *BlockSpec, text string) string {
	m := trailAttrRe.FindStringSubmatchIndex(text)
	if m == nil {
		return text
	}
	attrs := parseAttrs(text[m[2]:m[3]])
	if spec.Format == nil {
		spec.Format = map[string]any{}
	}
	if c := attrs["color"]; c != "" {
		spec.Format["block_color"] = c
	}
	if attrs["toggle"] == "true" {
		spec.Format["toggleable"] = true
	}
	return text[:m[0]]
}

func richSpec(typ, text string) *BlockSpec {
	s := &BlockSpec{Type: typ}
	text = applyTrailAttrs(s, text)
	s.Props = map[string]any{"title": ParseInline(text)}
	return s
}

func lineSpec(t string) *BlockSpec {
	if m := headingRe.FindStringSubmatch(t); m != nil {
		typ := []string{"header", "sub_header", "sub_sub_header", "sub_sub_header", "sub_sub_header", "sub_sub_header"}[len(m[1])-1]
		return richSpec(typ, m[2])
	}
	if m := todoRe.FindStringSubmatch(t); m != nil {
		s := richSpec("to_do", m[2])
		if m[1] != " " {
			s.Props["checked"] = []any{[]any{"Yes"}}
		}
		return s
	}
	if t == "---" || t == "***" || t == "___" {
		return &BlockSpec{Type: "divider"}
	}
	if m := bulletRe.FindStringSubmatch(t); m != nil {
		return richSpec("bulleted_list", m[1])
	}
	if t == "-" || t == "*" {
		return richSpec("bulleted_list", "")
	}
	if m := numberRe.FindStringSubmatch(t); m != nil {
		return richSpec("numbered_list", m[2])
	}
	if m := imageRe.FindStringSubmatch(t); m != nil && !strings.Contains(m[1], "](") {
		return mediaSpec("image", m[2], m[1])
	}
	if t == "<empty-block/>" || t == "<empty-block />" {
		return &BlockSpec{Type: "text"}
	}
	if m := openTagRe.FindStringSubmatch(t); m != nil {
		if s := tagLineSpec(strings.ToLower(m[1]), parseAttrs(m[2]), t, m); s != nil {
			return s
		}
	}
	return richSpec("text", t)
}

// tagInner returns the text between an opening tag and its closing tag on one line.
func tagInner(t string, m []string) string {
	rest := t[len(m[0]):]
	return strings.TrimSuffix(rest, "</"+m[1]+">")
}

func tagLineSpec(name string, attrs map[string]string, t string, m []string) *BlockSpec {
	inner := ""
	if m[3] != "/" {
		inner = tagInner(t, m)
		if !strings.HasSuffix(t, "</"+m[1]+">") {
			return nil
		}
	}
	refID := func(key string) string {
		if attrs[key] == "" {
			return ""
		}
		id, _, _, err := ParseRef(attrs[key])
		if err != nil {
			return ""
		}
		return id
	}
	switch name {
	case "page":
		s := &BlockSpec{Type: "page", RefID: refID("url")}
		s.Props = map[string]any{"title": ParseInline(html.UnescapeString(inner))}
		return s
	case "database":
		s := &BlockSpec{Type: "collection_view_page", RefID: refID("url")}
		if attrs["inline"] == "true" {
			s.Type = "collection_view"
		}
		s.Props = map[string]any{"title": ParseInline(html.UnescapeString(inner))}
		return s
	case "link-to-page":
		id := refID("url")
		if id == "" {
			return nil
		}
		return &BlockSpec{Type: "alias", Format: map[string]any{"alias_pointer": map[string]any{"id": id, "table": "block"}}}
	case "image", "file", "pdf", "video", "audio":
		return mediaSpec(name, attrs["src"], inner)
	case "bookmark":
		s := &BlockSpec{Type: "bookmark", Props: map[string]any{"link": []any{[]any{attrs["url"]}}}}
		if inner != "" {
			s.Props["title"] = ParseInline(inner)
		}
		return s
	case "embed":
		typ := attrs["type"]
		if typ == "" {
			typ = "embed"
		}
		s := &BlockSpec{Type: typ, Props: map[string]any{"source": []any{[]any{attrs["src"]}}}, Format: map[string]any{"display_source": attrs["src"]}}
		if inner != "" {
			s.Props["caption"] = ParseInline(inner)
		}
		return s
	case "table_of_contents":
		s := &BlockSpec{Type: "table_of_contents"}
		if c := attrs["color"]; c != "" {
			s.Format = map[string]any{"block_color": c}
		}
		return s
	case "breadcrumb":
		return &BlockSpec{Type: "breadcrumb"}
	case "unknown", "button", "external-object":
		return &BlockSpec{Type: "unknown", RefID: refID("url")}
	case "truncated":
		return &BlockSpec{Type: "truncated", Truncated: true}
	}
	return nil
}

func mediaSpec(typ, src, caption string) *BlockSpec {
	s := &BlockSpec{Type: typ, Props: map[string]any{}}
	src = html.UnescapeString(strings.TrimSpace(src))
	if src != "" && !strings.Contains(src, "://") && !strings.HasPrefix(src, "attachment:") && !strings.HasPrefix(src, "/images/") {
		s.LocalFile = src
	} else if src != "" {
		s.Props["source"] = []any{[]any{src}}
		if typ == "image" {
			s.Format = map[string]any{"display_source": src}
		}
	}
	if caption != "" {
		s.Props["caption"] = ParseInline(caption)
	}
	return s
}

func containerSpec(name string, attrs map[string]string, inner []mdLine) (*BlockSpec, error) {
	switch name {
	case "details":
		title := ""
		for k, l := range inner {
			if l.text == "" {
				continue
			}
			if strings.HasPrefix(l.text, "<summary>") && strings.HasSuffix(l.text, "</summary>") {
				title = strings.TrimSuffix(strings.TrimPrefix(l.text, "<summary>"), "</summary>")
				inner = inner[k+1:]
			}
			break
		}
		s := &BlockSpec{Type: "toggle", Props: map[string]any{"title": ParseInline(title)}}
		if c := attrs["color"]; c != "" {
			s.Format = map[string]any{"block_color": c}
		}
		children, err := parseBlocks(dedentLines(inner))
		if err != nil {
			return nil, err
		}
		s.Children = children
		return s, nil
	case "callout":
		s := &BlockSpec{Type: "callout", Props: map[string]any{"title": []any{}}, Format: map[string]any{}}
		if ic := attrs["icon"]; ic != "" {
			s.Format["page_icon"] = ic
		}
		if c := attrs["color"]; c != "" {
			s.Format["block_color"] = c
		} else {
			s.Format["block_color"] = "gray_background"
		}
		children, err := parseBlocks(inner)
		if err != nil {
			return nil, err
		}
		// The first plain paragraph becomes the callout's own text.
		if len(children) > 0 && children[0].Type == "text" && len(children[0].Children) == 0 && children[0].Format == nil {
			s.Props["title"] = children[0].Props["title"]
			children = children[1:]
		}
		s.Children = children
		return s, nil
	case "columns":
		children, err := parseBlocks(inner)
		if err != nil {
			return nil, err
		}
		s := &BlockSpec{Type: "column_list"}
		for _, c := range children {
			if c.Type != "column" {
				return nil, fmt.Errorf("<columns> may only contain <column> elements")
			}
		}
		if len(children) > 0 {
			// default to equal widths
			for _, c := range children {
				if c.Format == nil {
					c.Format = map[string]any{}
				}
				if _, ok := c.Format["column_ratio"]; !ok {
					c.Format["column_ratio"] = 1.0 / float64(len(children))
				}
			}
		}
		s.Children = children
		return s, nil
	case "column":
		children, err := parseBlocks(inner)
		if err != nil {
			return nil, err
		}
		s := &BlockSpec{Type: "column", Children: children}
		if w, err := strconv.ParseFloat(attrs["width"], 64); err == nil {
			s.Format = map[string]any{"column_ratio": w}
		}
		return s, nil
	case "synced_block":
		children, err := parseBlocks(inner)
		if err != nil {
			return nil, err
		}
		s := &BlockSpec{Type: "transclusion_container", Children: children}
		if attrs["url"] != "" {
			if id, _, _, err := ParseRef(attrs["url"]); err == nil {
				s.RefID = id
			}
		}
		return s, nil
	case "synced_block_reference":
		id, _, _, err := ParseRef(attrs["url"])
		if err != nil {
			return nil, fmt.Errorf("<synced_block_reference> needs url: %w", err)
		}
		return &BlockSpec{Type: "transclusion_reference", Format: map[string]any{
			"transclusion_reference_pointer": map[string]any{"id": id, "table": "block"},
		}}, nil
	case "table":
		var text []string
		for _, l := range inner {
			text = append(text, l.text)
		}
		joined := strings.Join(text, "\n")
		s := &BlockSpec{Type: "table", Format: map[string]any{
			"table_block_column_header": attrs["header-row"] == "true",
			"table_block_row_header":    attrs["header-column"] == "true",
		}}
		for _, tr := range regexp.MustCompile(`(?s)<tr(?:\s[^>]*)?>(.*?)</tr>`).FindAllStringSubmatch(joined, -1) {
			var row [][]any
			for _, td := range tdRe.FindAllStringSubmatch(tr[1], -1) {
				row = append(row, ParseInline(strings.TrimSpace(td[1])))
			}
			s.Rows = append(s.Rows, row)
		}
		return s, nil
	}
	return nil, fmt.Errorf("unsupported container <%s>", name)
}

var gfmSepRe = regexp.MustCompile(`^\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?$`)

func gfmTableSpec(rows []string) *BlockSpec {
	s := &BlockSpec{Type: "table", Format: map[string]any{"table_block_column_header": false, "table_block_row_header": false}}
	for i, r := range rows {
		if i == 1 && gfmSepRe.MatchString(strings.TrimSpace(r)) {
			s.Format["table_block_column_header"] = true
			continue
		}
		s.Rows = append(s.Rows, splitGFMRow(r))
	}
	return s
}

func splitGFMRow(r string) [][]any {
	r = strings.TrimSpace(r)
	r = strings.TrimPrefix(r, "|")
	if strings.HasSuffix(r, "|") && !strings.HasSuffix(r, `\|`) {
		r = r[:len(r)-1]
	}
	var cells [][]any
	var cur strings.Builder
	for i := 0; i < len(r); i++ {
		if r[i] == '\\' && i+1 < len(r) && r[i+1] == '|' {
			cur.WriteByte('|')
			i++
			continue
		}
		if r[i] == '|' {
			cells = append(cells, ParseInline(strings.TrimSpace(cur.String())))
			cur.Reset()
			continue
		}
		cur.WriteByte(r[i])
	}
	cells = append(cells, ParseInline(strings.TrimSpace(cur.String())))
	return cells
}
