package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/subdiox/notion-cli/internal/notion"
)

func nowMs() int64 { return time.Now().UnixMilli() }

type blockItem struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	HasChildren bool   `json:"has_children"`
	Markdown    string `json:"markdown"`
}

func newBlockCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "block", Aliases: []string{"blocks"}, Short: "Work with individual blocks (by ID)"}

	ls := &cobra.Command{
		Use:   "ls <page-or-block>",
		Short: "List child blocks with their IDs (for targeted edits)",
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
			st := e.store()
			if err := st.LoadDescendants(e.ctx, parent.ID); err != nil {
				return err
			}
			if err := st.PrefetchMentions(e.ctx, e.cachedBlocks()); err != nil {
				return err
			}
			r := &notion.Renderer{Store: st}
			var items []blockItem
			for _, id := range parent.Content() {
				b := st.Records.Block(id)
				if b == nil || !b.Alive() {
					continue
				}
				wrapper := &notion.Record{Table: "block", Value: map[string]any{"content": []any{id}}}
				items = append(items, blockItem{ID: id, Type: b.Type(), HasChildren: len(b.Content()) > 0, Markdown: r.RenderChildren(wrapper)})
			}
			if gf.json {
				return printJSON(items)
			}
			for _, it := range items {
				first, _, _ := strings.Cut(strings.TrimRight(it.Markdown, "\n"), "\n")
				more := ""
				if strings.Count(strings.TrimRight(it.Markdown, "\n"), "\n") > 0 {
					more = " …"
				}
				fmt.Printf("%s  %-14s %s%s\n", it.ID, it.Type, first, more)
			}
			return nil
		},
	}

	var o writeOpts
	update := &cobra.Command{
		Use:   "update <block>",
		Short: "Replace a block with new Markdown (edits in place when the type is compatible)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			b, err := e.target(args[0], "block")
			if err != nil {
				return err
			}
			md, baseDir, has, err := o.readMarkdown()
			if err != nil {
				return err
			}
			if !has {
				return fmt.Errorf("no content: pass --md or --file")
			}
			specs, err := notion.ParseMarkdown(md)
			if err != nil {
				return err
			}
			st := e.store()
			parent, err := st.Get(e.ctx, "block", b.Str("parent_id"))
			if err != nil || parent == nil {
				return fmt.Errorf("cannot load parent of %s", b.ID)
			}
			if err := st.LoadDescendants(e.ctx, b.ID); err != nil {
				return err
			}
			// Sync a virtual parent that only contains this block, so the diff
			// logic decides between in-place update and replacement.
			before := ""
			for _, id := range parent.Content() {
				if id == b.ID {
					break
				}
				before = id
			}
			virtual := &notion.Record{Table: "block", ID: parent.ID, SpaceID: parent.SpaceID, Value: map[string]any{"content": []any{b.ID}}}
			w := e.newWriter(b.SpaceID, "updateBlock")
			w.BaseDir = baseDir
			w.AllowDeletePages = true
			if !o.dryRun {
				if err := w.PrepareUploads(specs, notion.Pointer{Table: "block", ID: parent.ID, SpaceID: parent.SpaceID}); err != nil {
					return err
				}
			}
			if err := w.SyncAt(virtual, specs, before); err != nil {
				return err
			}
			if err := e.commit(w, o.dryRun); err != nil {
				return err
			}
			if o.dryRun {
				return nil
			}
			return printWriteResult("updated block in", parent.ID, map[string]any{"block_id": b.ID, "operations": len(w.Tx.Ops)})
		},
	}
	addWriteFlags(update, &o, true)

	insert := newAppendCmd("append")
	insert.Short = "Append Markdown as children of a block or page (or after a child with --after)"

	var dry bool
	rm := &cobra.Command{
		Use:     "rm <block>...",
		Aliases: []string{"delete"},
		Short:   "Delete blocks",
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
				w := e.newWriter(b.SpaceID, "deleteBlock")
				w.Remove(b)
				if err := e.commit(w, dry); err != nil {
					return err
				}
				if !dry {
					fmt.Println("deleted", b.ID)
				}
			}
			return nil
		},
	}
	rm.Flags().BoolVar(&dry, "dry-run", false, "print the transaction instead of sending it")

	move := newPageMoveCmd()
	move.Short = "Move blocks under another block/page (optionally --after a sibling)"

	cmd.AddCommand(ls, update, insert, rm, move)
	return cmd
}
