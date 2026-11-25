package notion

import (
	"fmt"
	"html"
	"strings"
)

// Renderer converts a cached block tree to Notion-flavored Markdown.
type Renderer struct {
	Store *Store
	// AssetURL rewrites file/image sources (e.g. to signed URLs or local paths).
	AssetURL func(block *Record, src string) string
	// MaxDepth limits rendering of nested blocks (0 = unlimited).
	MaxDepth int
}

func (r *Renderer) src(b *Record, raw string) string {
	if r.AssetURL != nil {
		return r.AssetURL(b, raw)
	}
	return raw
}

// RenderChildren renders the content of a block (typically a page).
func (r *Renderer) RenderChildren(parent *Record) string {
	var b strings.Builder
	r.renderList(&b, parent.Content(), parent.ID, 0)
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func (r *Renderer) renderList(b *strings.Builder, ids []string, parentID string, depth int) {
	num := 0
	prevType := ""
	for _, id := range ids {
		blk := r.Store.Records.Block(id)
		if blk == nil || !blk.Alive() {
			continue
		}
		t := blk.Type()
		if t == "numbered_list" {
			if prevType == "numbered_list" {
				num++
			} else {
				num = 1
				if s, ok := blk.Format()["list_start_index"].(float64); ok {
					num = int(s)
				}
			}
		}
		prevType = t
		r.renderBlock(b, blk, depth, num)
	}
}

func indentLines(s string, depth int) string {
	if depth == 0 || s == "" {
		return s
	}
	pre := strings.Repeat("\t", depth)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pre + l
		}
	}
	return strings.Join(lines, "\n")
}

func (r *Renderer) rich(blk *Record, key string) string {
	return RichToMarkdown(blk.Prop(key), r.Store)
}

func colorAttr(blk *Record) string {
	if c := str(blk.Format()["block_color"]); c != "" && c != "default" {
		return fmt.Sprintf(` color="%s"`, c)
	}
	return ""
}

// blockAttrs renders trailing {color="..."} style attributes for line blocks.
func blockAttrs(blk *Record, extra ...string) string {
	attrs := extra
	if c := str(blk.Format()["block_color"]); c != "" && c != "default" {
		attrs = append(attrs, fmt.Sprintf(`color="%s"`, c))
	}
	if len(attrs) == 0 {
		return ""
	}
	return " {" + strings.Join(attrs, " ") + "}"
}

func (r *Renderer) children(b *strings.Builder, blk *Record, depth int) {
	if r.MaxDepth > 0 && depth >= r.MaxDepth {
		if len(blk.Content()) > 0 {
			b.WriteString(indentLines(fmt.Sprintf("<truncated children=\"%d\" url=\"%s\"/>", len(blk.Content()), PageURL(blk.ID)), depth) + "\n")
		}
		return
	}
	r.renderList(b, blk.Content(), blk.ID, depth)
}

func (r *Renderer) line(b *strings.Builder, depth int, s string) {
	b.WriteString(indentLines(s, depth))
	b.WriteString("\n")
}

func (r *Renderer) renderBlock(b *strings.Builder, blk *Record, depth, num int) {
	t := blk.Type()
	f := blk.Format()
	switch t {
	case "text":
		txt := r.rich(blk, "title")
		if txt == "" && len(blk.Content()) == 0 {
			r.line(b, depth, "<empty-block/>")
			return
		}
		r.line(b, depth, txt+blockAttrs(blk))
		r.children(b, blk, depth+1)
	case "header", "sub_header", "sub_sub_header", "sub_sub_sub_header":
		level := map[string]int{"header": 1, "sub_header": 2, "sub_sub_header": 3, "sub_sub_sub_header": 4}[t]
		var extra []string
		if tg, _ := f["toggleable"].(bool); tg {
			extra = append(extra, `toggle="true"`)
		}
		r.line(b, depth, strings.Repeat("#", level)+" "+r.rich(blk, "title")+blockAttrs(blk, extra...))
		r.children(b, blk, depth+1)
	case "bulleted_list":
		r.line(b, depth, "- "+r.rich(blk, "title")+blockAttrs(blk))
		r.children(b, blk, depth+1)
	case "numbered_list":
		r.line(b, depth, fmt.Sprintf("%d. ", num)+r.rich(blk, "title")+blockAttrs(blk))
		r.children(b, blk, depth+1)
	case "to_do":
		box := "[ ]"
		if PlainText(blk.Prop("checked")) == "Yes" {
			box = "[x]"
		}
		r.line(b, depth, "- "+box+" "+r.rich(blk, "title")+blockAttrs(blk))
		r.children(b, blk, depth+1)
	case "toggle":
		r.line(b, depth, "<details"+colorAttr(blk)+">")
		r.line(b, depth, "<summary>"+r.rich(blk, "title")+"</summary>")
		r.children(b, blk, depth+1)
		r.line(b, depth, "</details>")
	case "quote":
		txt := strings.ReplaceAll(r.rich(blk, "title"), "<br>", "<br>")
		r.line(b, depth, "> "+txt+blockAttrs(blk))
		r.children(b, blk, depth+1)
	case "callout":
		icon := str(f["page_icon"])
		attrs := ""
		if icon != "" {
			attrs += fmt.Sprintf(` icon="%s"`, html.EscapeString(icon))
		}
		attrs += colorAttr(blk)
		r.line(b, depth, "<callout"+attrs+">")
		if txt := r.rich(blk, "title"); txt != "" {
			r.line(b, depth+1, txt)
		}
		r.children(b, blk, depth+1)
		r.line(b, depth, "</callout>")
	case "code":
		lang := strings.ToLower(PlainText(blk.Prop("language")))
		if lang == "plain text" {
			lang = ""
		}
		code := PlainText(blk.Prop("title"))
		fence := "```"
		for strings.Contains(code, fence) {
			fence += "`"
		}
		r.line(b, depth, fence+lang+"\n"+code+"\n"+fence)
		if c := r.rich(blk, "caption"); c != "" {
			r.line(b, depth, "<caption>"+c+"</caption>")
		}
	case "equation":
		r.line(b, depth, "$$\n"+PlainText(blk.Prop("title"))+"\n$$")
	case "divider":
		r.line(b, depth, "---")
	case "image":
		src := BlockFileSource(blk)
		r.line(b, depth, fmt.Sprintf("![%s](%s)", r.rich(blk, "caption"), r.src(blk, src)))
	case "video", "audio", "file", "pdf":
		src := BlockFileSource(blk)
		cap := r.rich(blk, "caption")
		if cap == "" {
			cap = EscapeMarkdown(PlainText(blk.Prop("title")))
		}
		r.line(b, depth, fmt.Sprintf(`<%s src="%s">%s</%s>`, t, html.EscapeString(r.src(blk, src)), cap, t))
	case "bookmark":
		link := PlainText(blk.Prop("link"))
		title := r.rich(blk, "title")
		r.line(b, depth, fmt.Sprintf(`<bookmark url="%s">%s</bookmark>`, html.EscapeString(link), title))
	case "embed", "figma", "maps", "tweet", "codepen", "gist", "drive", "typeform", "framer", "invision", "loom", "miro", "whimsical", "excalidraw", "replit", "sketch", "abstract", "pdf_embed":
		src := PlainText(blk.Prop("source"))
		if src == "" {
			src = str(f["display_source"])
		}
		r.line(b, depth, fmt.Sprintf(`<embed src="%s" type="%s">%s</embed>`, html.EscapeString(src), t, r.rich(blk, "caption")))
	case "page":
		r.line(b, depth, fmt.Sprintf(`<page url="%s">%s</page>`, PageURL(blk.ID), html.EscapeString(r.Store.PageTitle(blk.ID))))
	case "collection_view_page", "collection_view":
		inline := ""
		if t == "collection_view" {
			inline = ` inline="true"`
		}
		r.line(b, depth, fmt.Sprintf(`<database url="%s"%s>%s</database>`, PageURL(blk.ID), inline, html.EscapeString(r.Store.PageTitle(blk.ID))))
	case "alias":
		target := ""
		if p, ok := f["alias_pointer"].(map[string]any); ok {
			target = str(p["id"])
		}
		r.line(b, depth, fmt.Sprintf(`<mention-page url="%s">%s</mention-page>`, PageURL(target), html.EscapeString(r.Store.PageTitle(target))))
	case "column_list":
		r.line(b, depth, "<columns>")
		for _, cid := range blk.Content() {
			col := r.Store.Records.Block(cid)
			if col == nil || !col.Alive() {
				continue
			}
			attr := ""
			if ratio, ok := col.Format()["column_ratio"].(float64); ok {
				attr = fmt.Sprintf(` width="%.3g"`, ratio)
			}
			r.line(b, depth+1, "<column"+attr+">")
			r.children(b, col, depth+2)
			r.line(b, depth+1, "</column>")
		}
		r.line(b, depth, "</columns>")
	case "column":
		r.children(b, blk, depth)
	case "table":
		r.renderTable(b, blk, depth)
	case "transclusion_container":
		r.line(b, depth, fmt.Sprintf(`<synced_block url="%s">`, PageURL(blk.ID)))
		r.children(b, blk, depth+1)
		r.line(b, depth, "</synced_block>")
	case "transclusion_reference":
		src := syncedSource(blk)
		r.line(b, depth, fmt.Sprintf(`<synced_block_reference url="%s">`, PageURL(src)))
		if sb := r.Store.Records.Block(src); sb != nil {
			r.children(b, sb, depth+1)
		}
		r.line(b, depth, "</synced_block_reference>")
	case "table_of_contents":
		r.line(b, depth, "<table_of_contents"+colorAttr(blk)+"/>")
	case "breadcrumb":
		r.line(b, depth, "<breadcrumb/>")
	case "button":
		r.line(b, depth, fmt.Sprintf(`<button>%s</button>`, r.rich(blk, "title")))
	case "external_object_instance":
		r.line(b, depth, fmt.Sprintf(`<external-object url="%s"/>`, html.EscapeString(str(f["uri"]))))
	default:
		attrs := fmt.Sprintf(`type="%s" url="%s"`, t, PageURL(blk.ID))
		txt := r.rich(blk, "title")
		if txt == "" && len(blk.Content()) == 0 {
			r.line(b, depth, "<unknown "+attrs+"/>")
			return
		}
		r.line(b, depth, "<unknown "+attrs+">"+txt+"</unknown>")
		r.children(b, blk, depth+1)
	}
}

func (r *Renderer) renderTable(b *strings.Builder, blk *Record, depth int) {
	f := blk.Format()
	cols := strSlice(f["table_block_column_order"])
	colHeader, _ := f["table_block_column_header"].(bool)
	rowHeader, _ := f["table_block_row_header"].(bool)
	attrs := ""
	if colHeader {
		attrs += ` header-row="true"`
	}
	if rowHeader {
		attrs += ` header-column="true"`
	}
	r.line(b, depth, "<table"+attrs+">")
	for _, rid := range blk.Content() {
		row := r.Store.Records.Block(rid)
		if row == nil || !row.Alive() {
			continue
		}
		var cells strings.Builder
		for _, c := range cols {
			cells.WriteString("<td>" + RichToMarkdown(row.Prop(c), r.Store) + "</td>")
		}
		r.line(b, depth+1, "<tr>"+cells.String()+"</tr>")
	}
	r.line(b, depth, "</table>")
}
