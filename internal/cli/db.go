package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/subdiox/notion-cli/internal/notion"
)

func newDBCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "db", Aliases: []string{"database"}, Short: "Query and modify databases"}
	cmd.AddCommand(newDBQueryCmd())
	cmd.AddCommand(newDBWriteCmds()...)
	return cmd
}

// dbTarget resolves a database URL/ID to its block, collection and views.
func (e *env) dbTarget(ref string) (*notion.Record, *notion.Record, []*notion.Record, error) {
	blk, err := e.target(ref, "block")
	if err != nil {
		coll, err2 := e.target(ref, "collection")
		if err2 != nil {
			return nil, nil, nil, err
		}
		parent, _ := e.store().Get(e.ctx, "block", coll.Str("parent_id"))
		if parent != nil && (parent.Type() == "collection_view_page" || parent.Type() == "collection_view") {
			_, views, err := e.store().CollectionFor(e.ctx, parent)
			return parent, coll, views, err
		}
		return nil, coll, nil, nil
	}
	if t := blk.Type(); t != "collection_view_page" && t != "collection_view" {
		// A database row's page: use its parent database.
		if blk.Str("parent_table") == "collection" {
			return e.dbTarget(blk.Str("parent_id"))
		}
		return nil, nil, nil, fmt.Errorf("%s is a %s, not a database", blk.ID, t)
	}
	coll, views, err := e.store().CollectionFor(e.ctx, blk)
	return blk, coll, views, err
}

func pickView(views []*notion.Record, want string) (*notion.Record, error) {
	if want == "" {
		if len(views) > 0 {
			return views[0], nil
		}
		return nil, nil
	}
	if id := notion.NormalizeID(want); id != "" {
		want = id
	}
	for _, v := range views {
		if v.ID == want || strings.EqualFold(v.Str("name"), want) {
			return v, nil
		}
	}
	return nil, fmt.Errorf("view %q not found", want)
}

func newDBQueryCmd() *cobra.Command {
	var (
		view, search, filterJSON, sortJSON string
		where, sortBy                      []string
		limit                              int
		noView                             bool
	)
	cmd := &cobra.Command{
		Use:   "query <database-url|id>",
		Short: "Query database rows (uses the view's filter/sort unless overridden)",
		Long: `Query rows of a database.

--where adds filters (combined with AND, on top of the view filter unless --no-view):
  "Status=Done"  "Status!=Done"  "Name~foo" (contains)  "Points>3"  "Due<=2025-01-01"
  "Tags=urgent" (multi-select contains)  "Assignee=<user-id|email|name>"  "Done=true"
  "Due?" (is not empty)  "Due!?" (is empty)  relative dates: today, tomorrow, yesterday,
  one_week_ago, one_week_from_now, one_month_ago, one_month_from_now
--sort "Prop" or "Prop:desc" (repeatable).
--filter / --sort-json take Notion's internal filter/sort JSON for anything else.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			blk, coll, views, err := e.dbTarget(args[0])
			if err != nil {
				return err
			}
			if view == "" {
				_, view, _, _ = notion.ParseRef(args[0])
			}
			v, err := pickView(views, view)
			if err != nil {
				return err
			}
			defs := notion.Schema(coll, v)
			opt := notion.QueryOptions{Limit: limit, Search: search, IgnoreView: noView}
			if filterJSON != "" {
				if err := json.Unmarshal([]byte(filterJSON), &opt.Filter); err != nil {
					return fmt.Errorf("--filter: %w", err)
				}
			}
			if len(where) > 0 {
				var filters []any
				for _, w := range where {
					f, err := e.parseWhere(defs, w)
					if err != nil {
						return err
					}
					filters = append(filters, f)
				}
				if !noView && opt.Filter == nil && v != nil {
					if q2, ok := v.Value["query2"].(map[string]any); ok && q2["filter"] != nil {
						filters = append(filters, q2["filter"])
					}
				}
				if opt.Filter != nil {
					filters = append(filters, opt.Filter)
				}
				opt.Filter = map[string]any{"operator": "and", "filters": filters}
			}
			if sortJSON != "" {
				if err := json.Unmarshal([]byte(sortJSON), &opt.Sort); err != nil {
					return fmt.Errorf("--sort-json: %w", err)
				}
			}
			if len(sortBy) > 0 {
				var sorts []any
				for _, s := range sortBy {
					name, dir, _ := strings.Cut(s, ":")
					d := notion.FindProp(defs, name)
					if d == nil {
						return fmt.Errorf("unknown property %q", name)
					}
					direction := "ascending"
					if strings.HasPrefix(strings.ToLower(dir), "desc") {
						direction = "descending"
					}
					sorts = append(sorts, map[string]any{"property": d.ID, "direction": direction})
				}
				opt.Sort = sorts
			}
			if blk == nil {
				blk = &notion.Record{ID: coll.Str("parent_id")}
			}
			o := fetchOpts{rows: limit}
			st := e.store()
			res, err := st.QueryCollection(e.ctx, coll, v, opt)
			if err != nil {
				return err
			}
			var rowsRec []*notion.Record
			for _, id := range res.BlockIDs {
				if r := st.Records.Block(id); r != nil {
					rowsRec = append(rowsRec, r)
				}
			}
			if err := st.PrefetchMentions(e.ctx, rowsRec); err != nil {
				return err
			}
			rows := []map[string]any{}
			for _, r := range rowsRec {
				props := map[string]any{}
				for _, d := range defs {
					props[d.Name] = notion.PropValue(r, d, st)
				}
				rows = append(rows, map[string]any{"id": r.ID, "url": notion.PageURL(r.ID), "properties": props})
			}
			_ = o
			if gf.json {
				return printJSON(map[string]any{"rows": rows, "has_more": res.HasMore, "count": len(rows)})
			}
			printRowsTable(os.Stdout, rows, defs)
			if res.HasMore {
				fmt.Printf("\n(%d rows shown; more available — raise --limit)\n", len(rows))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&view, "view", "", "view name or ID (default: first view, or ?v= in the URL)")
	f.BoolVar(&noView, "no-view", false, "ignore the view's filter and sort")
	f.StringVarP(&search, "search", "q", "", "text search within the database")
	f.StringArrayVarP(&where, "where", "w", nil, "filter expression (repeatable)")
	f.StringArrayVar(&sortBy, "sort", nil, "sort by property, e.g. Due:desc (repeatable)")
	f.StringVar(&filterJSON, "filter", "", "internal filter JSON")
	f.StringVar(&sortJSON, "sort-json", "", "internal sort JSON")
	f.IntVarP(&limit, "limit", "n", 100, "max rows")
	return cmd
}

var whereRe = regexp.MustCompile(`^(.+?)\s*(!=|>=|<=|!\?|=|~|>|<|\?)\s*(.*)$`)

var relativeDates = map[string]bool{"today": true, "tomorrow": true, "yesterday": true, "one_week_ago": true, "one_week_from_now": true, "one_month_ago": true, "one_month_from_now": true}

func (e *env) parseWhere(defs []notion.PropDef, expr string) (any, error) {
	m := whereRe.FindStringSubmatch(expr)
	if m == nil {
		return nil, fmt.Errorf("bad --where %q", expr)
	}
	name, op, val := strings.TrimSpace(m[1]), m[2], strings.TrimSpace(m[3])
	d := notion.FindProp(defs, name)
	if d == nil {
		return nil, fmt.Errorf("unknown property %q", name)
	}
	wrap := func(operator string, value any) any {
		f := map[string]any{"operator": operator}
		if value != nil {
			f["value"] = value
		}
		return map[string]any{"property": d.ID, "filter": f}
	}
	exact := func(v any) any { return map[string]any{"type": "exact", "value": v} }
	if op == "?" {
		return wrap("is_not_empty", nil), nil
	}
	if op == "!?" {
		return wrap("is_empty", nil), nil
	}
	switch d.Type {
	case "title", "text", "url", "email", "phone_number":
		ops := map[string]string{"=": "string_is", "!=": "string_is_not", "~": "string_contains"}
		if o, ok := ops[op]; ok {
			return wrap(o, exact(val)), nil
		}
	case "select", "status":
		ops := map[string]string{"=": "enum_is", "!=": "enum_is_not", "~": "enum_is"}
		if o, ok := ops[op]; ok {
			return wrap(o, exact(val)), nil
		}
	case "multi_select":
		ops := map[string]string{"=": "enum_contains", "~": "enum_contains", "!=": "enum_does_not_contain"}
		if o, ok := ops[op]; ok {
			return wrap(o, exact(val)), nil
		}
	case "checkbox":
		b := val == "true" || val == "yes" || val == "Yes" || val == "1"
		if op == "!=" {
			return wrap("checkbox_is_not", exact(b)), nil
		}
		return wrap("checkbox_is", exact(b)), nil
	case "number", "auto_increment_id":
		n, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a number", name, val)
		}
		ops := map[string]string{"=": "number_equals", "!=": "number_does_not_equal", ">": "number_greater_than", "<": "number_less_than", ">=": "number_greater_than_or_equal_to", "<=": "number_less_than_or_equal_to"}
		if o, ok := ops[op]; ok {
			return wrap(o, exact(n)), nil
		}
	case "date", "created_time", "last_edited_time":
		var v any
		if relativeDates[val] {
			v = map[string]any{"type": "relative", "value": val}
		} else {
			v = exact(map[string]any{"type": "date", "start_date": val})
		}
		ops := map[string]string{"=": "date_is", ">": "date_is_after", "<": "date_is_before", ">=": "date_is_on_or_after", "<=": "date_is_on_or_before"}
		if o, ok := ops[op]; ok {
			return wrap(o, v), nil
		}
	case "person", "created_by", "last_edited_by":
		uid, err := e.resolvePerson(val)
		if err != nil {
			return nil, err
		}
		v := exact(map[string]any{"table": "notion_user", "id": uid})
		switch op {
		case "=", "~":
			return wrap("person_contains", v), nil
		case "!=":
			return wrap("person_does_not_contain", v), nil
		}
	case "relation":
		id, _, _, err := notion.ParseRef(val)
		if err != nil {
			return nil, err
		}
		v := exact(map[string]any{"table": "block", "id": id})
		switch op {
		case "=", "~":
			return wrap("relation_contains", v), nil
		case "!=":
			return wrap("relation_does_not_contain", v), nil
		}
	}
	return nil, fmt.Errorf("operator %q is not supported for %s property %q (use --filter JSON)", op, d.Type, name)
}
