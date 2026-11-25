package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/subdiox/notion-cli/internal/notion"
)

// writeOpts are shared by commands that take Markdown input.
type writeOpts struct {
	md       string
	file     string
	props    []string
	propJSON string
	title    string
	icon     string
	cover    string
	dryRun   bool
	allowDel bool
}

func addWriteFlags(cmd *cobra.Command, o *writeOpts, body bool) {
	f := cmd.Flags()
	if body {
		f.StringVar(&o.md, "md", "", "Markdown content")
		f.StringVarP(&o.file, "file", "f", "", "read Markdown from a file ('-' for stdin)")
	}
	f.BoolVar(&o.dryRun, "dry-run", false, "print the transaction instead of sending it")
}

func addPropFlags(cmd *cobra.Command, o *writeOpts) {
	f := cmd.Flags()
	f.StringVarP(&o.title, "title", "t", "", "page title")
	f.StringArrayVarP(&o.props, "prop", "p", nil, `set a database property: "Name=value" (repeatable)`)
	f.StringVar(&o.propJSON, "props-json", "", `set properties from JSON: {"Status":"Done","Tags":["a","b"]}`)
	f.StringVar(&o.icon, "icon", "", "page icon (emoji or image URL; 'none' to remove)")
	f.StringVar(&o.cover, "cover", "", "page cover image URL ('none' to remove)")
}

// readMarkdown returns the Markdown body (if any) and the base dir for local files.
func (o *writeOpts) readMarkdown() (string, string, bool, error) {
	switch {
	case o.file == "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", "", false, err
		}
		wd, _ := os.Getwd()
		return string(b), wd, true, nil
	case o.file != "":
		b, err := os.ReadFile(o.file)
		if err != nil {
			return "", "", false, err
		}
		abs, _ := filepath.Abs(o.file)
		return string(b), filepath.Dir(abs), true, nil
	case o.md != "":
		wd, _ := os.Getwd()
		return strings.ReplaceAll(o.md, `\n`, "\n"), wd, true, nil
	}
	return "", "", false, nil
}

// frontMatterProps parses `key: value` front matter (values may be JSON).
func frontMatterProps(lines []string) map[string]any {
	out := map[string]any{}
	for _, l := range lines {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if strings.HasPrefix(k, `"`) {
			var ks string
			if json.Unmarshal([]byte(k), &ks) == nil {
				k = ks
			}
		}
		v = strings.TrimSpace(v)
		var val any
		if err := json.Unmarshal([]byte(v), &val); err != nil {
			val = v
		}
		out[k] = val
	}
	return out
}

var readOnlyFrontMatter = map[string]bool{
	"id": true, "url": true, "parent": true, "created_time": true, "last_edited_time": true,
	"created_by": true, "role": true, "in_trash": true, "collection_id": true, "type": true,
}

// collectProps merges front matter, --props-json and --prop into name -> value.
func (o *writeOpts) collectProps(fm map[string]any) (map[string]any, error) {
	props := map[string]any{}
	for k, v := range fm {
		if !readOnlyFrontMatter[k] {
			props[k] = v
		}
	}
	if o.propJSON != "" {
		var m map[string]any
		if err := json.Unmarshal([]byte(o.propJSON), &m); err != nil {
			return nil, fmt.Errorf("--props-json: %w", err)
		}
		for k, v := range m {
			props[k] = v
		}
	}
	for _, p := range o.props {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("--prop %q: expected Name=value", p)
		}
		props[strings.TrimSpace(k)] = v
	}
	if o.title != "" {
		props["title"] = o.title
	}
	return props, nil
}

func (e *env) newWriter(spaceID string, action string) *notion.Writer {
	return &notion.Writer{
		Store: e.store(),
		Tx:    notion.NewTx(spaceID, e.client.UserID, action),
		Ctx:   e.ctx,
		Warn:  func(s string) { fmt.Fprintln(os.Stderr, "warning:", s) },
	}
}

func (e *env) commit(w *notion.Writer, dry bool) error {
	if dry {
		return w.Tx.Dump(os.Stdout)
	}
	if len(w.Tx.Ops) == 0 {
		return nil
	}
	return w.Tx.Submit(e.ctx, e.store())
}

// applyPageProps emits ops setting title/properties/icon/cover on a page.
func (e *env) applyPageProps(w *notion.Writer, pageID string, coll *notion.Record, props map[string]any, o *writeOpts) error {
	current := e.store().Records.Block(pageID)
	var defs []notion.PropDef
	if coll != nil {
		defs = notion.Schema(coll, nil)
	}
	enc := &notion.PropEncoder{SpaceID: w.Tx.SpaceID, ResolveUser: e.resolvePerson}
	for name, v := range props {
		if name == "icon" {
			if s, ok := v.(string); ok && o.icon == "" {
				o.icon = s
			}
			continue
		}
		var d *notion.PropDef
		if coll != nil {
			d = notion.FindProp(defs, name)
		}
		if d == nil {
			if strings.EqualFold(name, "title") || coll == nil && strings.EqualFold(name, "name") {
				d = &notion.PropDef{ID: "title", Name: "title", Type: "title"}
			} else if coll == nil {
				return fmt.Errorf("property %q: page is not in a database (only title can be set)", name)
			} else {
				return fmt.Errorf("unknown property %q (see `notion fetch <database>` for the schema)", name)
			}
		}
		val, err := enc.Encode(*d, v)
		if err != nil {
			return err
		}
		if d.Type == "relation" {
			// Relations can only be changed through relation primitives.
			want := map[string]bool{}
			var order []string
			for _, seg := range notion.DecodeRich(val.([]any)) {
				for _, deco := range seg.Decos {
					if len(deco) > 1 && deco[0] == "p" {
						id := fmt.Sprint(deco[1])
						if !want[id] {
							want[id] = true
							order = append(order, id)
						}
					}
				}
			}
			have := map[string]bool{}
			if current != nil {
				for _, seg := range notion.DecodeRich(current.Prop(d.ID)) {
					for _, deco := range seg.Decos {
						if len(deco) > 1 && deco[0] == "p" {
							id := fmt.Sprint(deco[1])
							have[id] = true
							if !want[id] {
								w.Tx.BlockProperty(pageID, d.ID, "removeRelation", map[string]any{"id": id, "spaceId": w.Tx.SpaceID})
							}
						}
					}
				}
			}
			for _, id := range order {
				if !have[id] {
					w.Tx.BlockProperty(pageID, d.ID, "addRelationAfter", map[string]any{"id": id, "spaceId": w.Tx.SpaceID})
				}
			}
			continue
		}
		w.Tx.Set("block", pageID, []string{"properties", d.ID}, val)
	}
	if coll != nil {
		enc.ApplyNewOptions(w.Tx, coll)
	}
	if o.icon != "" {
		var icon any = o.icon
		if o.icon == "none" {
			icon = nil
		}
		w.Tx.Set("block", pageID, []string{"format", "page_icon"}, icon)
	}
	if o.cover != "" {
		var cover any = o.cover
		if o.cover == "none" {
			cover = nil
		}
		w.Tx.Set("block", pageID, []string{"format", "page_cover"}, cover)
		if cover != nil {
			w.Tx.Set("block", pageID, []string{"format", "page_cover_position"}, 0.5)
		}
	}
	return nil
}

func printWriteResult(action string, id string, extra map[string]any) error {
	if gf.json {
		out := map[string]any{"ok": true, "action": action, "id": id, "url": notion.PageURL(id)}
		for k, v := range extra {
			out[k] = v
		}
		return printJSON(out)
	}
	fmt.Printf("%s %s\n", action, notion.PageURL(id))
	return nil
}

// --- page --------------------------------------------------------------------

func newPageCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "page", Short: "Create, update, move and delete pages"}
	cmd.AddCommand(newPageCreateCmd(), newPageUpdateCmd(), newAppendCmd("append"), newPageMoveCmd(), newTrashCmd(), newRestoreCmd(), newDuplicateCmd())
	return cmd
}

func newPageCreateCmd() *cobra.Command {
	var o writeOpts
	var parent string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a page (in a page, a database, or as a private page)",
		Long: `Create a page from Notion-flavored Markdown.

--parent is a page URL/ID, a database URL/ID (creates a row), or omitted for a
private top-level page in the current workspace. YAML front matter in the
Markdown (as printed by 'notion fetch') sets the title and database properties.
Local images/files referenced from the Markdown are uploaded.`,
		Example: `  notion page create --parent <page-url> -t "Meeting notes" -f notes.md
  notion page create --parent <database-url> -t "Fix login" -p Status=Todo -p Tags=bug,auth
  echo "# Hello" | notion page create -t Draft -f -`,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			md, baseDir, hasBody, err := o.readMarkdown()
			if err != nil {
				return err
			}
			fmLines, body := notion.FrontMatter(md)
			props, err := o.collectProps(frontMatterProps(fmLines))
			if err != nil {
				return err
			}
			st := e.store()
			id := notion.NewID()
			var (
				parentID, parentTable string
				coll                  *notion.Record
				spaceID               string
			)
			if parent == "" {
				sp, err := e.currentSpace()
				if err != nil {
					return err
				}
				spaceID, parentID, parentTable = sp.ID, sp.ID, "space"
			} else {
				pb, err := e.target(parent, "block")
				if err != nil {
					return err
				}
				spaceID = pb.SpaceID
				switch pb.Type() {
				case "collection_view_page", "collection_view":
					c, _, err := st.CollectionFor(e.ctx, pb)
					if err != nil {
						return err
					}
					coll, parentID, parentTable = c, c.ID, "collection"
				default:
					parentID, parentTable = pb.ID, "block"
				}
			}
			w := e.newWriter(spaceID, "createPage")
			w.BaseDir = baseDir
			v := w.Tx.NewBlockValue(id, "page", parentID, parentTable)
			if parentTable == "space" {
				v["permissions"] = []any{map[string]any{"role": "editor", "type": "user_permission", "user_id": e.client.UserID}}
			}
			w.Tx.Set("block", id, nil, v)
			if _, ok := props["title"]; !ok {
				if t := firstHeading(body); t != "" && o.title == "" {
					props["title"] = t
				}
			}
			if err := e.applyPageProps(w, id, coll, props, &o); err != nil {
				return err
			}
			switch parentTable {
			case "block":
				w.Tx.ListAfter("block", parentID, []string{"content"}, id, "")
			case "space":
				ws, err := e.workspaces()
				if err != nil {
					return err
				}
				if sv := ws.SpaceView(e.client.UserID, spaceID); sv != nil {
					w.Tx.ListAfter("space_view", sv.ID, []string{"private_pages"}, id, "")
				}
			}
			if hasBody {
				specs, err := notion.ParseMarkdown(body)
				if err != nil {
					return err
				}
				if !o.dryRun {
					if err := w.PrepareUploads(specs, notion.Pointer{Table: "block", ID: parentOrSelf(parentTable, parentID, id), SpaceID: spaceID}); err != nil {
						return err
					}
				}
				w.Create(specs, id, "block", "")
			}
			if err := e.commit(w, o.dryRun); err != nil {
				return err
			}
			if o.dryRun {
				return nil
			}
			return printWriteResult("created", id, nil)
		},
	}
	cmd.Flags().StringVar(&parent, "parent", "", "parent page or database (URL or ID); default: private page in the current workspace")
	addWriteFlags(cmd, &o, true)
	addPropFlags(cmd, &o)
	return cmd
}

// Uploads must reference an existing record: use the parent page when possible.
func parentOrSelf(parentTable, parentID, self string) string {
	if parentTable == "block" {
		return parentID
	}
	return self
}

// firstHeading pulls a leading "# Title" out of the body as the page title.
func firstHeading(body string) string {
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "# ") {
			return strings.TrimSpace(l[2:])
		}
		return ""
	}
	return ""
}

func newPageUpdateCmd() *cobra.Command {
	var o writeOpts
	cmd := &cobra.Command{
		Use:   "update <page>",
		Short: "Update a page's title, properties, icon/cover and/or replace its content",
		Long: `Update a page. With --md/--file, the page body is replaced by the given
Markdown using a minimal diff: unchanged blocks keep their IDs (so comments and
block links survive), edited blocks are updated in place, and only the rest
is created or deleted. Sub-pages and databases missing from the Markdown are
kept unless --allow-delete-pages is given.

The typical agent workflow is: notion fetch <page> > page.md, edit page.md,
notion page update <page> -f page.md.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			page, err := e.target(args[0], "block")
			if err != nil {
				return err
			}
			md, baseDir, hasBody, err := o.readMarkdown()
			if err != nil {
				return err
			}
			fmLines, body := notion.FrontMatter(md)
			props, err := o.collectProps(frontMatterProps(fmLines))
			if err != nil {
				return err
			}
			st := e.store()
			var coll *notion.Record
			if page.Str("parent_table") == "collection" {
				if coll, err = st.Get(e.ctx, "collection", page.Str("parent_id")); err != nil {
					return err
				}
				props = changedProps(page, coll, props, st)
			} else if t, ok := props["title"]; ok && fmt.Sprint(t) == page.Title() {
				delete(props, "title")
			}
			if ic, ok := props["icon"]; ok && fmt.Sprint(ic) == fmt.Sprint(page.Format()["page_icon"]) {
				delete(props, "icon")
			}
			w := e.newWriter(page.SpaceID, "updatePage")
			w.BaseDir = baseDir
			w.AllowDeletePages = o.allowDel
			if err := e.applyPageProps(w, page.ID, coll, props, &o); err != nil {
				return err
			}
			if hasBody {
				specs, err := notion.ParseMarkdown(body)
				if err != nil {
					return err
				}
				if err := st.LoadPage(e.ctx, page.ID); err != nil {
					return err
				}
				page = st.Records.Block(page.ID)
				if !o.dryRun {
					if err := w.PrepareUploads(specs, notion.Pointer{Table: "block", ID: page.ID, SpaceID: page.SpaceID}); err != nil {
						return err
					}
				}
				if err := w.Sync(page, specs); err != nil {
					return err
				}
			}
			if len(w.Tx.Ops) > 0 {
				w.Tx.Touch("block", page.ID)
			}
			if err := e.commit(w, o.dryRun); err != nil {
				return err
			}
			if o.dryRun {
				return nil
			}
			return printWriteResult("updated", page.ID, map[string]any{"operations": len(w.Tx.Ops)})
		},
	}
	addWriteFlags(cmd, &o, true)
	addPropFlags(cmd, &o)
	cmd.Flags().BoolVar(&o.allowDel, "allow-delete-pages", false, "trash sub-pages/databases that are missing from the Markdown")
	return cmd
}

// changedProps drops properties whose value is unchanged (e.g. read-only
// values round-tripped from `fetch` front matter).
func changedProps(page, coll *notion.Record, props map[string]any, st *notion.Store) map[string]any {
	defs := notion.Schema(coll, nil)
	out := map[string]any{}
	for k, v := range props {
		d := notion.FindProp(defs, k)
		if d == nil {
			out[k] = v
			continue
		}
		cur := notion.PropValue(page, *d, st)
		a, _ := json.Marshal(cur)
		b, _ := json.Marshal(v)
		if string(a) == string(b) {
			continue
		}
		if s, ok := v.(string); ok && s == notion.PropDisplay(page, *d, st) {
			continue
		}
		out[k] = v
	}
	return out
}

func newAppendCmd(use string) *cobra.Command {
	var o writeOpts
	var after string
	var prepend bool
	cmd := &cobra.Command{
		Use:   use + " <page-or-block>",
		Short: "Append Markdown content to a page or block (or insert after a block)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			parent, err := e.target(args[0], "block")
			if err != nil {
				return err
			}
			md, baseDir, hasBody, err := o.readMarkdown()
			if err != nil {
				return err
			}
			if !hasBody {
				return fmt.Errorf("no content: pass --md or --file")
			}
			specs, err := notion.ParseMarkdown(md)
			if err != nil {
				return err
			}
			w := e.newWriter(parent.SpaceID, "appendBlocks")
			w.BaseDir = baseDir
			if !o.dryRun {
				if err := w.PrepareUploads(specs, notion.Pointer{Table: "block", ID: parent.ID, SpaceID: parent.SpaceID}); err != nil {
					return err
				}
			}
			afterID := ""
			if after != "" {
				afterID, _, _, err = notion.ParseRef(after)
				if err != nil {
					return err
				}
			}
			if prepend {
				// insert before the first child
				ids := w.Create(specs, parent.ID, "block", "")
				first := ""
				if c := parent.Content(); len(c) > 0 {
					first = c[0]
				}
				if first != "" {
					// Rewrite listAfter ops into listBefore(first) in order.
					var rewritten []notion.Op
					for _, op := range w.Tx.Ops {
						if op.Command == "listAfter" && op.Pointer.ID == parent.ID {
							args := op.Args.(map[string]any)
							if _, hasAfter := args["after"]; !hasAfter {
								op.Command = "listBefore"
								args["before"] = first
							}
						}
						rewritten = append(rewritten, op)
					}
					w.Tx.Ops = rewritten
					_ = ids
				}
			} else {
				w.Create(specs, parent.ID, "block", afterID)
			}
			w.Tx.Touch("block", parent.ID)
			if err := e.commit(w, o.dryRun); err != nil {
				return err
			}
			if o.dryRun {
				return nil
			}
			return printWriteResult("appended to", parent.ID, nil)
		},
	}
	addWriteFlags(cmd, &o, true)
	cmd.Flags().StringVar(&after, "after", "", "insert after this child block instead of at the end")
	cmd.Flags().BoolVar(&prepend, "prepend", false, "insert at the beginning")
	return cmd
}

func newPageMoveCmd() *cobra.Command {
	var to, after string
	var dry bool
	cmd := &cobra.Command{
		Use:   "move <page-or-block>... --to <parent>",
		Short: "Move pages/blocks to another page, database or position",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			if to == "" {
				return fmt.Errorf("--to is required")
			}
			var blocks []*notion.Record
			for _, a := range args {
				b, err := e.target(a, "block")
				if err != nil {
					return err
				}
				blocks = append(blocks, b)
			}
			dest, err := e.target(to, "block")
			if err != nil {
				return err
			}
			st := e.store()
			parentID, parentTable := dest.ID, "block"
			if t := dest.Type(); t == "collection_view_page" || t == "collection_view" {
				coll, _, err := st.CollectionFor(e.ctx, dest)
				if err != nil {
					return err
				}
				parentID, parentTable = coll.ID, "collection"
			}
			afterID := ""
			if after != "" {
				afterID, _, _, err = notion.ParseRef(after)
				if err != nil {
					return err
				}
			}
			w := e.newWriter(dest.SpaceID, "moveBlocks")
			for _, b := range blocks {
				if b.SpaceID != dest.SpaceID {
					return fmt.Errorf("moving between workspaces is not supported (%s)", b.ID)
				}
				if b.Str("parent_table") == "space" {
					ws, err := e.workspaces()
					if err != nil {
						return err
					}
					if sv := ws.SpaceView(e.client.UserID, b.SpaceID); sv != nil {
						w.Tx.ListRemove("space_view", sv.ID, []string{"private_pages"}, b.ID)
					}
				}
				w.Move(b, parentID, parentTable, afterID)
				if parentTable == "block" {
					afterID = b.ID
				}
			}
			if err := e.commit(w, dry); err != nil {
				return err
			}
			if dry {
				return nil
			}
			return printWriteResult("moved to", dest.ID, nil)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "destination page or database")
	cmd.Flags().StringVar(&after, "after", "", "place after this block in the destination")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "print the transaction instead of sending it")
	return cmd
}

func newTrashCmd() *cobra.Command {
	var dry bool
	cmd := &cobra.Command{
		Use:     "trash <page-or-block>...",
		Aliases: []string{"rm", "delete"},
		Short:   "Move pages/blocks to the trash",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			for _, a := range args {
				b, err := e.target(a, "block")
				if err != nil {
					return err
				}
				w := e.newWriter(b.SpaceID, "trash")
				w.Remove(b)
				if notion.IsPageType(b.Type()) {
					w.Tx.Update("block", b.ID, nil, map[string]any{
						"moved_to_trash_table": "notion_user", "moved_to_trash_id": e.client.UserID,
					})
				}
				if err := e.commit(w, dry); err != nil {
					return err
				}
				if !dry {
					if err := printWriteResult("trashed", b.ID, nil); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dry, "dry-run", false, "print the transaction instead of sending it")
	return cmd
}

func newRestoreCmd() *cobra.Command {
	var dry bool
	cmd := &cobra.Command{
		Use:   "restore <page-or-block>...",
		Short: "Restore pages/blocks from the trash",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			for _, a := range args {
				b, err := e.target(a, "block")
				if err != nil {
					return err
				}
				w := e.newWriter(b.SpaceID, "restore")
				w.Tx.Update("block", b.ID, nil, map[string]any{"alive": true})
				if b.Str("parent_table") == "block" {
					w.Tx.ListAfter("block", b.Str("parent_id"), []string{"content"}, b.ID, "")
				}
				if err := e.commit(w, dry); err != nil {
					return err
				}
				if !dry {
					if err := printWriteResult("restored", b.ID, nil); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dry, "dry-run", false, "print the transaction instead of sending it")
	return cmd
}
