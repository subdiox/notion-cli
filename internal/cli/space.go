package cli

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/subdiox/notion-cli/internal/config"
	"github.com/subdiox/notion-cli/internal/notion"
)

func newSpaceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "space", Aliases: []string{"spaces", "workspace", "ws"}, Short: "List and switch workspaces (including guest workspaces)"}

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List all workspaces of all logged-in accounts",
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			ws, err := e.workspaces()
			if err != nil {
				return err
			}
			if gf.json {
				return printJSON(ws.Spaces)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, " \tNAME\tMEMBERSHIP\tACCOUNT\tID")
			for _, s := range ws.Spaces {
				mark := " "
				if s.ID == e.cfg.ActiveSpaceID && s.UserID == e.client.UserID {
					mark = "*"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", mark, s.Name, s.Membership, s.UserEmail, s.ID)
			}
			return w.Flush()
		},
	}

	use := &cobra.Command{
		Use:   "use <name|domain|id>",
		Short: "Set the default workspace (and its account)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			ws, err := e.workspaces()
			if err != nil {
				return err
			}
			sp, err := findSpace(ws, args[0])
			if err != nil {
				return err
			}
			e.cfg.ActiveSpaceID, e.cfg.ActiveUserID = sp.ID, sp.UserID
			if err := config.Save(e.cfg); err != nil {
				return err
			}
			fmt.Printf("Using %s (%s) as %s\n", sp.Name, sp.Membership, sp.UserEmail)
			return nil
		},
	}

	current := &cobra.Command{
		Use:   "current",
		Short: "Show the active workspace",
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			sp, err := e.currentSpace()
			if err != nil {
				return err
			}
			if gf.json {
				return printJSON(sp)
			}
			fmt.Printf("%s (%s) as %s — %s\n", sp.Name, sp.Membership, sp.UserEmail, sp.ID)
			return nil
		},
	}

	pages := &cobra.Command{
		Use:   "pages",
		Short: "List top-level pages of the workspace visible to you (sidebar, private, shared with guest)",
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			sp, err := e.currentSpace()
			if err != nil {
				return err
			}
			ids, err := e.topLevelPages(sp)
			if err != nil {
				return err
			}
			return e.printPageList(ids)
		},
	}

	cmd.AddCommand(ls, use, current, pages)
	return cmd
}

type pageListItem struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Type    string `json:"type"`
	Section string `json:"section,omitempty"`
}

// topLevelPages returns root pages for the sidebar: teamspace pages, private
// pages, favorites and pages shared directly with the user.
func (e *env) topLevelPages(sp *notion.Space) ([]pageListItem, error) {
	st := e.store()
	var items []pageListItem
	seen := map[string]bool{}
	add := func(section string, ids []string) error {
		var ptrs []notion.Pointer
		for _, id := range ids {
			ptrs = append(ptrs, notion.Pointer{Table: "block", ID: id, SpaceID: sp.ID})
		}
		if err := st.Fetch(e.ctx, ptrs, false); err != nil {
			return err
		}
		for _, id := range ids {
			b := st.Records.Block(id)
			if b == nil || !b.Alive() || seen[id] {
				continue
			}
			seen[id] = true
			items = append(items, pageListItem{ID: id, Title: st.PageTitle(id), URL: notion.PageURL(id), Type: b.Type(), Section: section})
		}
		return nil
	}

	// Teamspaces (team.pages) for members.
	ws, err := e.workspaces()
	if err != nil {
		return nil, err
	}
	rm := ws.Records[sp.UserID]
	for _, team := range rm["team"] {
		if team.Value == nil || team.Str("space_id") != sp.ID || !team.Alive() {
			continue
		}
		if err := add("teamspace:"+team.Str("name"), strSliceAny(team.Value["pages"])); err != nil {
			return nil, err
		}
	}
	if sv := ws.SpaceView(sp.UserID, sp.ID); sv != nil {
		if err := add("favorites", strSliceAny(sv.Value["bookmarked_pages"])); err != nil {
			return nil, err
		}
		if err := add("private", strSliceAny(sv.Value["private_pages"])); err != nil {
			return nil, err
		}
	}
	// Pages shared directly with the user (the only entry point for guests).
	var shared struct {
		Pages []any `json:"pages"`
	}
	if err := e.client.Call(e.ctx, "getUserSharedPagesInSpace", map[string]any{"spaceId": sp.ID, "includeDeleted": false}, &shared); err == nil {
		var ids []string
		for _, p := range shared.Pages {
			switch v := p.(type) {
			case string:
				ids = append(ids, v)
			case map[string]any:
				ids = append(ids, fmt.Sprint(v["id"]))
			}
		}
		if err := add("shared", ids); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func strSliceAny(v any) []string {
	arr, _ := v.([]any)
	var out []string
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (e *env) printPageList(items []pageListItem) error {
	if gf.json {
		return printJSON(items)
	}
	section := ""
	for _, it := range items {
		if it.Section != section {
			section = it.Section
			fmt.Printf("## %s\n", section)
		}
		kind := ""
		if it.Type == "collection_view_page" {
			kind = " (database)"
		}
		fmt.Printf("- [%s](%s)%s\n", notion.EscapeMarkdown(it.Title), it.URL, kind)
	}
	return nil
}
