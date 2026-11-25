package cli

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/subdiox/notion-cli/internal/notion"
)

type searchHit struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Type      string `json:"type"`
	Path      string `json:"path,omitempty"`
	Snippet   string `json:"snippet,omitempty"`
	Space     string `json:"space"`
	BlockID   string `json:"matched_block_id,omitempty"`
	EditedAt  string `json:"last_edited_time,omitempty"`
}

var highlightTag = regexp.MustCompile(`</?[A-Za-z0-9]{6,12}>`)

func newSearchCmd() *cobra.Command {
	var (
		limit   int
		all     bool
		sortBy  string
		in      string
		titles  bool
		trashed bool
	)
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search in the current workspace (or --all workspaces)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			query := ""
			if len(args) > 0 {
				query = args[0]
			}
			var spaces []notion.Space
			if all {
				ws, err := e.workspaces()
				if err != nil {
					return err
				}
				spaces = ws.Spaces
			} else {
				sp, err := e.currentSpace()
				if err != nil {
					return err
				}
				spaces = []notion.Space{*sp}
			}
			var ancestors []string
			if in != "" {
				id, _, _, err := notion.ParseRef(in)
				if err != nil {
					return err
				}
				ancestors = []string{id}
			}
			var hits []searchHit
			for _, sp := range spaces {
				e.client.UserID, e.client.SpaceID = sp.UserID, sp.ID
				h, err := e.search(sp, query, limit, sortBy, ancestors, titles, trashed)
				if err != nil {
					return fmt.Errorf("search in %s: %w", sp.Name, err)
				}
				hits = append(hits, h...)
			}
			if gf.json {
				return printJSON(hits)
			}
			if len(hits) == 0 {
				fmt.Println("No results.")
			}
			for _, h := range hits {
				kind := ""
				if h.Type == "collection_view_page" || h.Type == "collection_view" {
					kind = " (database)"
				}
				fmt.Printf("- [%s](%s)%s", notion.EscapeMarkdown(h.Title), h.URL, kind)
				if all {
					fmt.Printf(" — %s", h.Space)
				}
				fmt.Println()
				if h.Path != "" {
					fmt.Printf("  path: %s\n", h.Path)
				}
				if h.Snippet != "" {
					fmt.Printf("  > %s\n", strings.ReplaceAll(h.Snippet, "\n", " "))
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.IntVarP(&limit, "limit", "n", 20, "max results per workspace")
	f.BoolVarP(&all, "all", "a", false, "search all workspaces of all accounts")
	f.StringVar(&sortBy, "sort", "relevance", "relevance | edited | created")
	f.StringVar(&in, "in", "", "only search inside this page (URL or ID)")
	f.BoolVar(&titles, "titles", false, "match page titles only")
	f.BoolVar(&trashed, "trash", false, "search pages in the trash")
	return cmd
}

func (e *env) search(sp notion.Space, query string, limit int, sortBy string, ancestors []string, titles, trashed bool) ([]searchHit, error) {
	sort := map[string]any{"field": "relevance"}
	switch sortBy {
	case "edited":
		sort = map[string]any{"field": "lastEdited", "direction": "desc"}
	case "created":
		sort = map[string]any{"field": "created", "direction": "desc"}
	}
	if ancestors == nil {
		ancestors = []string{}
	}
	body := map[string]any{
		"type":    "BlocksInSpace",
		"query":   query,
		"spaceId": sp.ID,
		"limit":   limit,
		"filters": map[string]any{
			"isDeletedOnly":                          trashed,
			"excludeTemplates":                       false,
			"navigableBlockContentOnly":              titles,
			"requireEditPermissions":                 false,
			"includePublicPagesWithoutExplicitAccess": false,
			"ancestors":                              ancestors,
			"createdBy":                              []any{},
			"editedBy":                               []any{},
			"lastEditedTime":                         map[string]any{},
			"createdTime":                            map[string]any{},
			"inTeams":                                []any{},
		},
		"sort":   sort,
		"source": "quick_find_input_change",
	}
	var resp struct {
		Results []struct {
			ID        string `json:"id"`
			Highlight struct {
				Title    string `json:"title"`
				Text     string `json:"text"`
				PathText string `json:"pathText"`
			} `json:"highlight"`
			HighlightBlockID string `json:"highlightBlockId"`
		} `json:"results"`
		RecordMap json.RawMessage `json:"recordMap"`
	}
	if err := e.client.Call(e.ctx, "search", body, &resp); err != nil {
		return nil, err
	}
	rm, err := notion.ParseRecordMap(resp.RecordMap)
	if err != nil {
		return nil, err
	}
	st := notion.NewStore(e.client)
	st.Records = rm
	var hits []searchHit
	for _, r := range resp.Results {
		h := searchHit{ID: r.ID, URL: notion.PageURL(r.ID), Space: sp.Name, BlockID: r.HighlightBlockID}
		if b := rm.Block(r.ID); b != nil {
			h.Type = b.Type()
			h.Title = st.PageTitle(r.ID)
			h.EditedAt = msISO(b.Value["last_edited_time"])
		} else {
			h.Title = highlightTag.ReplaceAllString(r.Highlight.Title, "")
		}
		h.Path = highlightTag.ReplaceAllString(r.Highlight.PathText, "")
		h.Snippet = highlightTag.ReplaceAllString(r.Highlight.Text, "")
		if h.BlockID == r.ID {
			h.BlockID = ""
		}
		hits = append(hits, h)
	}
	return hits, nil
}
