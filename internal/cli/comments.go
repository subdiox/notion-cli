package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/subdiox/notion-cli/internal/notion"
)

type commentOut struct {
	ID       string `json:"id"`
	Author   string `json:"author"`
	AuthorID string `json:"author_id"`
	Created  string `json:"created_time"`
	Markdown string `json:"markdown"`
}

type discussionOut struct {
	ID       string       `json:"id"`
	BlockID  string       `json:"block_id"`
	Context  string       `json:"context,omitempty"`
	Resolved bool         `json:"resolved"`
	Comments []commentOut `json:"comments"`
}

func newCommentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "comment", Aliases: []string{"comments"}, Short: "Read and write comments"}

	var withResolved bool
	ls := &cobra.Command{
		Use:   "ls <page>",
		Short: "List comment threads on a page and its blocks",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			page, err := e.target(args[0], "block")
			if err != nil {
				return err
			}
			st := e.store()
			if err := st.LoadPage(e.ctx, page.ID); err != nil {
				return err
			}
			var ptrs []notion.Pointer
			for _, b := range e.cachedBlocks() {
				for _, did := range strSliceAny(b.Value["discussions"]) {
					ptrs = append(ptrs, notion.Pointer{Table: "discussion", ID: did, SpaceID: b.SpaceID})
				}
			}
			if err := st.Fetch(e.ctx, ptrs, false); err != nil {
				return err
			}
			var cptrs []notion.Pointer
			for _, p := range ptrs {
				if d := st.Records.Get("discussion", p.ID); d != nil {
					for _, c := range strSliceAny(d.Value["comments"]) {
						cptrs = append(cptrs, notion.Pointer{Table: "comment", ID: c, SpaceID: p.SpaceID})
					}
				}
			}
			if err := st.Fetch(e.ctx, cptrs, false); err != nil {
				return err
			}
			var users []notion.Pointer
			for _, c := range st.Records["comment"] {
				if c.Value != nil {
					users = append(users, notion.Pointer{Table: "notion_user", ID: c.Str("created_by_id")})
					if pages, us := notion.MentionIDs(asSlice(c.Value["text"])); len(pages)+len(us) > 0 {
						for _, u := range us {
							users = append(users, notion.Pointer{Table: "notion_user", ID: u})
						}
						for _, p := range pages {
							users = append(users, notion.Pointer{Table: "block", ID: p})
						}
					}
				}
			}
			if err := st.Fetch(e.ctx, users, false); err != nil {
				return err
			}
			var out []discussionOut
			for _, p := range ptrs {
				d := st.Records.Get("discussion", p.ID)
				if d == nil {
					continue
				}
				resolved, _ := d.Value["resolved"].(bool)
				if resolved && !withResolved {
					continue
				}
				do := discussionOut{ID: d.ID, BlockID: d.Str("parent_id"), Resolved: resolved,
					Context: notion.RichToMarkdown(asSlice(d.Value["context"]), st)}
				for _, cid := range strSliceAny(d.Value["comments"]) {
					c := st.Records.Get("comment", cid)
					if c == nil || !c.Alive() {
						continue
					}
					do.Comments = append(do.Comments, commentOut{
						ID: c.ID, AuthorID: c.Str("created_by_id"), Author: st.UserName(c.Str("created_by_id")),
						Created: msISO(c.Value["created_time"]), Markdown: notion.RichToMarkdown(asSlice(c.Value["text"]), st),
					})
				}
				if len(do.Comments) > 0 {
					out = append(out, do)
				}
			}
			if gf.json {
				if out == nil {
					out = []discussionOut{}
				}
				return printJSON(out)
			}
			if len(out) == 0 {
				fmt.Println("No comments.")
			}
			for _, d := range out {
				where := "page"
				if d.BlockID != page.ID {
					where = "block " + d.BlockID
				}
				state := ""
				if d.Resolved {
					state = " (resolved)"
				}
				fmt.Printf("## discussion %s on %s%s\n", d.ID, where, state)
				if d.Context != "" {
					fmt.Printf("> %s\n", strings.ReplaceAll(d.Context, "\n", " "))
				}
				for _, c := range d.Comments {
					fmt.Printf("- **%s** (%s): %s\n", c.Author, c.Created, c.Markdown)
				}
				fmt.Println()
			}
			return nil
		},
	}
	ls.Flags().BoolVar(&withResolved, "resolved", false, "include resolved threads")

	var dry bool
	add := &cobra.Command{
		Use:   "add <page-or-block> <markdown>",
		Short: "Start a new comment thread on a page or block",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			b, err := e.target(args[0], "block")
			if err != nil {
				return err
			}
			w := e.newWriter(b.SpaceID, "addComment")
			did, cid := notion.NewID(), notion.NewID()
			now := nowMs()
			w.Tx.Set("discussion", did, nil, map[string]any{
				"id": did, "version": 1, "parent_id": b.ID, "parent_table": "block",
				"resolved": false, "comments": []any{cid}, "space_id": b.SpaceID,
			})
			w.Tx.Set("comment", cid, nil, e.commentValue(cid, did, b.SpaceID, args[1], now))
			w.Tx.ListAfter("block", b.ID, []string{"discussions"}, did, "")
			if err := e.commit(w, dry); err != nil {
				return err
			}
			if dry {
				return nil
			}
			return printWriteResult("commented on", b.ID, map[string]any{"discussion_id": did, "comment_id": cid})
		},
	}
	add.Flags().BoolVar(&dry, "dry-run", false, "print the transaction instead of sending it")

	reply := &cobra.Command{
		Use:   "reply <discussion-id> <markdown>",
		Short: "Reply to a comment thread",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			d, err := e.target(args[0], "discussion")
			if err != nil {
				return err
			}
			w := e.newWriter(d.SpaceID, "replyComment")
			cid := notion.NewID()
			w.Tx.Set("comment", cid, nil, e.commentValue(cid, d.ID, d.SpaceID, args[1], nowMs()))
			w.Tx.ListAfter("discussion", d.ID, []string{"comments"}, cid, "")
			if err := e.commit(w, dry); err != nil {
				return err
			}
			if dry {
				return nil
			}
			return printWriteResult("replied on", d.Str("parent_id"), map[string]any{"discussion_id": d.ID, "comment_id": cid})
		},
	}
	reply.Flags().BoolVar(&dry, "dry-run", false, "print the transaction instead of sending it")

	resolve := func(resolved bool) *cobra.Command {
		use, short := "resolve", "Resolve a comment thread"
		if !resolved {
			use, short = "reopen", "Re-open a resolved comment thread"
		}
		return &cobra.Command{
			Use:   use + " <discussion-id>",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				e, err := newEnv(cmd)
				if err != nil {
					return err
				}
				d, err := e.target(args[0], "discussion")
				if err != nil {
					return err
				}
				w := e.newWriter(d.SpaceID, use+"Discussion")
				w.Tx.Set("discussion", d.ID, []string{"resolved"}, resolved)
				if err := e.commit(w, false); err != nil {
					return err
				}
				return printWriteResult(use+"d thread on", d.Str("parent_id"), map[string]any{"discussion_id": d.ID})
			},
		}
	}

	rm := &cobra.Command{
		Use:   "rm <comment-id>",
		Short: "Delete a comment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			c, err := e.target(args[0], "comment")
			if err != nil {
				return err
			}
			w := e.newWriter(c.SpaceID, "deleteComment")
			w.Tx.Update("comment", c.ID, nil, map[string]any{"alive": false})
			w.Tx.ListRemove("discussion", c.Str("parent_id"), []string{"comments"}, c.ID)
			if err := e.commit(w, false); err != nil {
				return err
			}
			fmt.Println("deleted comment", c.ID)
			return nil
		},
	}

	edit := &cobra.Command{
		Use:   "edit <comment-id> <markdown>",
		Short: "Edit a comment",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			c, err := e.target(args[0], "comment")
			if err != nil {
				return err
			}
			w := e.newWriter(c.SpaceID, "editComment")
			w.Tx.Set("comment", c.ID, []string{"text"}, notion.ParseInline(args[1]))
			w.Tx.Update("comment", c.ID, nil, map[string]any{"last_edited_time": nowMs()})
			if err := e.commit(w, false); err != nil {
				return err
			}
			fmt.Println("edited comment", c.ID)
			return nil
		},
	}

	cmd.AddCommand(ls, add, reply, resolve(true), resolve(false), edit, rm)
	return cmd
}

func (e *env) commentValue(id, discussionID, spaceID, md string, now int64) map[string]any {
	return map[string]any{
		"id": id, "version": 1, "alive": true,
		"parent_id": discussionID, "parent_table": "discussion",
		"text":          notion.ParseInline(strings.ReplaceAll(md, `\n`, "\n")),
		"created_by_id": e.client.UserID, "created_by_table": "notion_user",
		"created_time": now, "last_edited_time": now, "space_id": spaceID,
	}
}
