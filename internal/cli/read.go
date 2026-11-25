package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/subdiox/notion-cli/internal/notion"
)

type fetchOpts struct {
	signed   bool
	assets   string
	depth    int
	rows     int
	noBody   bool
}

func newFetchCmd() *cobra.Command {
	var o fetchOpts
	cmd := &cobra.Command{
		Use:     "fetch <url|id>",
		Aliases: []string{"get", "read", "cat"},
		Short:   "Read a page, database or block as Notion-flavored Markdown",
		Long: `Read any page, database or block the logged-in accounts can access
(the right account is chosen automatically).

Pages print YAML front matter (id, url, title, parent, database properties)
followed by the body. Databases print their schema, views and rows.

Files and images stored in Notion are shown as their internal source; use
--signed-urls for temporary download links or --download-assets DIR to save
them locally and link to the local paths.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			blk, err := e.target(args[0], "block")
			if err != nil {
				// maybe a collection ID
				if coll, err2 := e.target(args[0], "collection"); err2 == nil {
					return e.printCollection(coll, nil, o)
				}
				return err
			}
			switch blk.Type() {
			case "collection_view_page", "collection_view":
				_, viewID, _, _ := notion.ParseRef(args[0])
				return e.printDatabase(blk, viewID, o)
			case "page":
				return e.printPage(blk, o)
			default:
				return e.printBlock(blk, o)
			}
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.signed, "signed-urls", false, "replace Notion-hosted file sources with temporary signed URLs")
	f.StringVar(&o.assets, "download-assets", "", "download images/files into DIR and link to local paths")
	f.IntVar(&o.depth, "depth", 0, "max nesting depth to render (0 = unlimited)")
	f.IntVar(&o.rows, "rows", 50, "number of rows to show for databases")
	f.BoolVar(&o.noBody, "props-only", false, "print only front matter / properties")
	return cmd
}

var mediaTypes = map[string]bool{"image": true, "video": true, "audio": true, "file": true, "pdf": true}

// renderer builds a Markdown renderer. With --signed-urls / --download-assets,
// all Notion-hosted files below root are signed in one batch (and downloaded
// in parallel) before rendering.
func (e *env) renderer(o fetchOpts, blocks []*notion.Record) (*notion.Renderer, error) {
	st := e.store()
	r := &notion.Renderer{Store: st, MaxDepth: o.depth}
	if !o.signed && o.assets == "" {
		return r, nil
	}
	var owners []*notion.Record
	var srcs []string
	for _, b := range blocks {
		if !mediaTypes[b.Type()] {
			continue
		}
		if src := notion.AbsoluteSource(notion.BlockFileSource(b)); src != "" {
			owners = append(owners, b)
			srcs = append(srcs, src)
		}
	}
	resolved := map[string]string{} // block ID -> URL or local path
	var signOwners []*notion.Record
	var signSrcs []string
	for i, src := range srcs {
		if notion.NeedsSigning(src) {
			signOwners = append(signOwners, owners[i])
			signSrcs = append(signSrcs, src)
		} else {
			resolved[owners[i].ID] = src
		}
	}
	for i := 0; i < len(signSrcs); i += 50 {
		j := min(i+50, len(signSrcs))
		urls, err := st.SignedURLs(e.ctx, signOwners[i:j], signSrcs[i:j])
		if err != nil {
			return nil, err
		}
		for k, u := range urls {
			if u != "" {
				resolved[signOwners[i+k].ID] = u
			}
		}
	}
	if o.assets != "" {
		if err := os.MkdirAll(o.assets, 0o755); err != nil {
			return nil, err
		}
		type job struct{ id, url, dst string }
		var jobs []job
		used := map[string]bool{}
		for i, b := range owners {
			u, ok := resolved[b.ID]
			if !ok {
				continue
			}
			name := notion.FileName(srcs[i], b.ID)
			if used[name] {
				name = b.ID[:8] + "-" + name
			}
			used[name] = true
			jobs = append(jobs, job{b.ID, u, filepath.Join(o.assets, name)})
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for _, jb := range jobs {
			wg.Add(1)
			sem <- struct{}{}
			go func(jb job) {
				defer wg.Done()
				defer func() { <-sem }()
				f, err := os.Create(jb.dst)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: %v\n", err)
					return
				}
				_, err = e.client.Download(e.ctx, jb.url, f)
				f.Close()
				if err != nil {
					os.Remove(jb.dst)
					fmt.Fprintf(os.Stderr, "warning: %s: %v\n", jb.id, err)
					return
				}
				mu.Lock()
				resolved[jb.id] = jb.dst
				mu.Unlock()
			}(jb)
		}
		wg.Wait()
	}
	r.AssetURL = func(b *notion.Record, src string) string {
		if u, ok := resolved[b.ID]; ok {
			return u
		}
		return src
	}
	return r, nil
}

func (e *env) cachedBlocks() []*notion.Record {
	var blocks []*notion.Record
	for _, r := range e.store().Records["block"] {
		if r.Value != nil {
			blocks = append(blocks, r)
		}
	}
	return blocks
}

// frontMatter writes YAML front matter with JSON-encoded values (valid YAML).
func frontMatter(fields [][2]any) string {
	var b strings.Builder
	b.WriteString("---\n")
	for _, kv := range fields {
		v, _ := json.Marshal(kv[1])
		k := fmt.Sprint(kv[0])
		if strings.ContainsAny(k, ":#{}[],&*?|<>=!%@`'\"") || strings.TrimSpace(k) != k {
			kj, _ := json.Marshal(k)
			k = string(kj)
		}
		fmt.Fprintf(&b, "%s: %s\n", k, v)
	}
	b.WriteString("---\n\n")
	return b.String()
}

func (e *env) parentInfo(blk *notion.Record) map[string]any {
	st := e.store()
	pid, ptable := blk.Str("parent_id"), blk.Str("parent_table")
	switch ptable {
	case "collection":
		coll, _ := st.Get(e.ctx, "collection", pid)
		name := ""
		if coll != nil {
			name = notion.CollectionName(coll)
			if p := coll.Str("parent_id"); p != "" {
				return map[string]any{"type": "database", "id": p, "collection_id": pid, "title": name, "url": notion.PageURL(p)}
			}
		}
		return map[string]any{"type": "database", "collection_id": pid, "title": name}
	case "block":
		_, _ = st.Get(e.ctx, "block", pid)
		return map[string]any{"type": "page", "id": pid, "title": st.PageTitle(pid), "url": notion.PageURL(pid)}
	case "space":
		return map[string]any{"type": "workspace", "id": pid}
	case "team":
		t, _ := st.Get(e.ctx, "team", pid)
		name := ""
		if t != nil {
			name = t.Str("name")
		}
		return map[string]any{"type": "teamspace", "id": pid, "title": name}
	}
	return map[string]any{"type": ptable, "id": pid}
}

func (e *env) printPage(page *notion.Record, o fetchOpts) error {
	st := e.store()
	if !o.noBody {
		if err := st.LoadPage(e.ctx, page.ID); err != nil {
			return err
		}
		page = st.Records.Block(page.ID)
	}
	blocks := e.cachedBlocks()
	if err := st.PrefetchMentions(e.ctx, blocks); err != nil {
		return err
	}

	fields := [][2]any{{"id", page.ID}, {"url", notion.PageURL(page.ID)}, {"title", page.Title()}}
	props := map[string]any{}
	var defs []notion.PropDef
	if page.Str("parent_table") == "collection" {
		coll, err := st.Get(e.ctx, "collection", page.Str("parent_id"))
		if err != nil {
			return err
		}
		if coll != nil {
			defs = notion.Schema(coll, nil)
			for _, d := range defs {
				if d.Type == "title" {
					continue
				}
				props[d.Name] = notion.PropValue(page, d, st)
			}
		}
	}
	parent := e.parentInfo(page)
	fields = append(fields, [2]any{"parent", parent})
	if icon := page.Format()["page_icon"]; icon != nil {
		fields = append(fields, [2]any{"icon", icon})
	}
	fields = append(fields,
		[2]any{"created_time", msISO(page.Value["created_time"])},
		[2]any{"last_edited_time", msISO(page.Value["last_edited_time"])},
		[2]any{"created_by", st.UserName(page.Str("created_by_id"))},
		[2]any{"role", page.Role})
	if !page.Alive() {
		fields = append(fields, [2]any{"in_trash", true})
	}

	r, err := e.renderer(o, blocks)
	if err != nil {
		return err
	}
	body := ""
	if !o.noBody {
		body = r.RenderChildren(page)
	}

	if gf.json {
		out := map[string]any{}
		for _, kv := range fields {
			out[kv[0].(string)] = kv[1]
		}
		if len(defs) > 0 {
			out["properties"] = props
		}
		if !o.noBody {
			out["markdown"] = body
		}
		return printJSON(out)
	}
	for _, d := range defs {
		if d.Type != "title" {
			fields = append(fields, [2]any{d.Name, props[d.Name]})
		}
	}
	fmt.Print(frontMatter(fields))
	if !o.noBody {
		fmt.Print(body)
	}
	return nil
}

func msISO(v any) string {
	f, ok := v.(float64)
	if !ok {
		return ""
	}
	return notion.MsToTime(int64(f))
}

func (e *env) printBlock(blk *notion.Record, o fetchOpts) error {
	st := e.store()
	if err := st.LoadDescendants(e.ctx, blk.ID); err != nil {
		return err
	}
	blocks := e.cachedBlocks()
	if err := st.PrefetchMentions(e.ctx, blocks); err != nil {
		return err
	}
	r, err := e.renderer(o, blocks)
	if err != nil {
		return err
	}
	// Render the block itself by wrapping it in a synthetic parent.
	wrapper := &notion.Record{Table: "block", ID: "", Value: map[string]any{"content": []any{blk.ID}}}
	md := r.RenderChildren(wrapper)
	if gf.json {
		return printJSON(map[string]any{"id": blk.ID, "type": blk.Type(), "parent_id": blk.Str("parent_id"), "markdown": md, "record": blk.Value})
	}
	fmt.Print(md)
	return nil
}

func (e *env) printDatabase(blk *notion.Record, viewID string, o fetchOpts) error {
	st := e.store()
	coll, views, err := st.CollectionFor(e.ctx, blk)
	if err != nil {
		return err
	}
	var view *notion.Record
	for _, v := range views {
		if v.ID == viewID {
			view = v
		}
	}
	if view == nil && len(views) > 0 {
		view = views[0]
	}
	return e.printCollectionWithViews(blk, coll, views, view, o)
}

func (e *env) printCollection(coll *notion.Record, view *notion.Record, o fetchOpts) error {
	return e.printCollectionWithViews(nil, coll, nil, view, o)
}

func (e *env) printCollectionWithViews(blk, coll *notion.Record, views []*notion.Record, view *notion.Record, o fetchOpts) error {
	st := e.store()
	defs := notion.Schema(coll, view)
	var res *notion.QueryResult
	if o.rows > 0 {
		var err error
		res, err = st.QueryCollection(e.ctx, coll, view, notion.QueryOptions{Limit: o.rows})
		if err != nil {
			return err
		}
		var rows []*notion.Record
		for _, id := range res.BlockIDs {
			if r := st.Records.Block(id); r != nil {
				rows = append(rows, r)
			}
		}
		if err := st.PrefetchMentions(e.ctx, rows); err != nil {
			return err
		}
	}

	schema := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		m := map[string]any{"id": d.ID, "name": d.Name, "type": d.Type}
		if len(d.Options) > 0 {
			opts := []string{}
			for _, op := range d.Options {
				opts = append(opts, op.Value)
			}
			m["options"] = opts
		}
		for _, k := range []string{"formula", "property", "target_property", "collection_id", "number_format", "prefix"} {
			if v, ok := d.Raw[k]; ok {
				m[k] = v
			}
		}
		schema = append(schema, m)
	}
	viewList := []map[string]any{}
	for _, v := range views {
		viewList = append(viewList, map[string]any{"id": v.ID, "name": v.Str("name"), "type": v.Type(), "url": notion.PageURL(blk.ID) + "?v=" + notion.CompactID(v.ID)})
	}
	rows := []map[string]any{}
	if res != nil {
		for _, id := range res.BlockIDs {
			r := st.Records.Block(id)
			if r == nil {
				continue
			}
			props := map[string]any{}
			for _, d := range defs {
				props[d.Name] = notion.PropValue(r, d, st)
			}
			rows = append(rows, map[string]any{"id": id, "url": notion.PageURL(id), "properties": props})
		}
	}
	id, url := coll.ID, ""
	if blk != nil {
		id, url = blk.ID, notion.PageURL(blk.ID)
	}

	if gf.json {
		out := map[string]any{"id": id, "url": url, "collection_id": coll.ID, "title": notion.CollectionName(coll), "schema": schema, "views": viewList, "rows": rows}
		if res != nil {
			out["has_more"] = res.HasMore
		}
		if d := notion.RichToMarkdown(asSlice(coll.Value["description"]), st); d != "" {
			out["description"] = d
		}
		return printJSON(out)
	}

	fmt.Print(frontMatter([][2]any{{"id", id}, {"url", url}, {"collection_id", coll.ID}, {"title", notion.CollectionName(coll)}, {"type", "database"}}))
	if d := notion.RichToMarkdown(asSlice(coll.Value["description"]), st); d != "" {
		fmt.Println(d)
		fmt.Println()
	}
	fmt.Println("## Schema")
	for _, s := range schema {
		extra := ""
		if opts, ok := s["options"].([]string); ok {
			extra = " — options: " + strings.Join(opts, ", ")
		}
		fmt.Printf("- %s (%s)%s\n", s["name"], s["type"], extra)
	}
	if len(viewList) > 0 {
		fmt.Println("\n## Views")
		for _, v := range viewList {
			mark := ""
			if view != nil && v["id"] == view.ID {
				mark = " ← rows below use this view"
			}
			fmt.Printf("- %s (%s) %s%s\n", v["name"], v["type"], v["url"], mark)
		}
	}
	if res != nil {
		fmt.Printf("\n## Rows (%d shown", len(rows))
		if res.HasMore {
			fmt.Printf(", more available — use `notion db query`")
		}
		fmt.Println(")")
		printRowsTable(rows, defs)
	}
	return nil
}

func printRowsTable(rows []map[string]any, defs []notion.PropDef) {
	cell := func(s string) string {
		s = strings.ReplaceAll(s, "\n", "<br>")
		return strings.ReplaceAll(s, "|", `\|`)
	}
	header := []string{}
	for _, d := range defs {
		header = append(header, cell(d.Name))
	}
	header = append(header, "url")
	fmt.Println("| " + strings.Join(header, " | ") + " |")
	fmt.Println("|" + strings.Repeat(" --- |", len(header)))
	for _, r := range rows {
		props := r["properties"].(map[string]any)
		vals := []string{}
		for _, d := range defs {
			vals = append(vals, cell(displayValue(props[d.Name])))
		}
		vals = append(vals, fmt.Sprint(r["url"]))
		fmt.Println("| " + strings.Join(vals, " | ") + " |")
	}
}

func displayValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "✓"
		}
		return ""
	case []string:
		return strings.Join(t, ", ")
	case []map[string]string:
		var parts []string
		for _, m := range t {
			if m["title"] != "" {
				parts = append(parts, m["title"])
			} else if m["name"] != "" {
				parts = append(parts, m["name"])
			} else {
				parts = append(parts, m["id"])
			}
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		s := fmt.Sprint(t["start"])
		if e, ok := t["end"]; ok {
			s += " → " + fmt.Sprint(e)
		}
		return s
	case float64:
		b, _ := json.Marshal(t)
		return string(b)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func asSlice(v any) []any { a, _ := v.([]any); return a }
