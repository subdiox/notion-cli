package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PropDef is one column of a collection schema.
type PropDef struct {
	ID      string
	Name    string
	Type    string
	Raw     map[string]any
	Options []SelectOption
}

type SelectOption struct {
	ID    string `json:"id,omitempty"`
	Value string `json:"value"`
	Color string `json:"color,omitempty"`
}

// Schema returns the collection's properties in a stable order (title first,
// then the order of the given view's properties if any, then by name).
func Schema(coll *Record, view *Record) []PropDef {
	schema := mapAt(coll.Value, "schema")
	order := map[string]int{}
	if view != nil {
		for _, key := range []string{"table_properties", "list_properties", "board_properties", "gallery_properties", "calendar_properties", "timeline_properties"} {
			if arr, ok := view.Format()[key].([]any); ok {
				for i, p := range arr {
					if m, ok := p.(map[string]any); ok {
						if _, seen := order[str(m["property"])]; !seen {
							order[str(m["property"])] = i
						}
					}
				}
			}
		}
	}
	var defs []PropDef
	for id, v := range schema {
		m, _ := v.(map[string]any)
		if m == nil {
			continue
		}
		d := PropDef{ID: id, Name: str(m["name"]), Type: str(m["type"]), Raw: m}
		if opts, ok := m["options"].([]any); ok {
			for _, o := range opts {
				if om, ok := o.(map[string]any); ok {
					d.Options = append(d.Options, SelectOption{ID: str(om["id"]), Value: str(om["value"]), Color: str(om["color"])})
				}
			}
		}
		defs = append(defs, d)
	}
	sort.SliceStable(defs, func(i, j int) bool {
		a, b := defs[i], defs[j]
		if (a.Type == "title") != (b.Type == "title") {
			return a.Type == "title"
		}
		oa, okA := order[a.ID]
		ob, okB := order[b.ID]
		if okA != okB {
			return okA
		}
		if okA && oa != ob {
			return oa < ob
		}
		return a.Name < b.Name
	})
	return defs
}

// FindProp finds a property by name (case-insensitive) or ID.
func FindProp(defs []PropDef, key string) *PropDef {
	for i := range defs {
		if defs[i].ID == key || defs[i].Name == key {
			return &defs[i]
		}
	}
	for i := range defs {
		if strings.EqualFold(defs[i].Name, key) {
			return &defs[i]
		}
	}
	if strings.EqualFold(key, "title") {
		for i := range defs {
			if defs[i].Type == "title" {
				return &defs[i]
			}
		}
	}
	return nil
}

func msToISO(v any) string {
	f, ok := v.(float64)
	if !ok || f == 0 {
		return ""
	}
	return time.UnixMilli(int64(f)).UTC().Format(time.RFC3339)
}

// PropValue returns a JSON-friendly value of a page property.
func PropValue(page *Record, d PropDef, r Resolver) any {
	raw := page.Prop(d.ID)
	switch d.Type {
	case "title", "text":
		return RichToMarkdown(raw, r)
	case "number":
		s := PlainText(raw)
		if s == "" {
			return nil
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
		return s
	case "select", "status":
		s := PlainText(raw)
		if s == "" {
			return nil
		}
		return s
	case "multi_select":
		s := PlainText(raw)
		if s == "" {
			return []string{}
		}
		parts := strings.Split(s, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts
	case "checkbox":
		return PlainText(raw) == "Yes"
	case "date":
		for _, s := range DecodeRich(raw) {
			if dd, ok := s.deco("d"); ok && len(dd) > 1 {
				m, _ := dd[1].(map[string]any)
				out := map[string]any{"start": joinDT(m["start_date"], m["start_time"])}
				if e := joinDT(m["end_date"], m["end_time"]); e != "" {
					out["end"] = e
				}
				if tz := str(m["time_zone"]); tz != "" {
					out["time_zone"] = tz
				}
				return out
			}
		}
		return nil
	case "person":
		var out []map[string]string
		for _, s := range DecodeRich(raw) {
			if dd, ok := s.deco("u"); ok && len(dd) > 1 {
				id := str(dd[1])
				name := id
				if r != nil {
					name = r.UserName(id)
				}
				out = append(out, map[string]string{"id": id, "name": name})
			}
		}
		return out
	case "relation":
		var out []map[string]string
		for _, s := range DecodeRich(raw) {
			if dd, ok := s.deco("p"); ok && len(dd) > 1 {
				id := str(dd[1])
				title := ""
				if r != nil {
					title = r.PageTitle(id)
				}
				out = append(out, map[string]string{"id": id, "title": title, "url": PageURL(id)})
			}
		}
		return out
	case "file":
		var out []map[string]string
		for _, s := range DecodeRich(raw) {
			f := map[string]string{"name": s.Text}
			if dd, ok := s.deco("a"); ok && len(dd) > 1 {
				f["url"] = str(dd[1])
			}
			if s.Text != "," {
				out = append(out, f)
			}
		}
		return out
	case "created_time":
		return msToISO(page.Value["created_time"])
	case "last_edited_time":
		return msToISO(page.Value["last_edited_time"])
	case "created_by", "last_edited_by":
		key := strings.TrimSuffix(d.Type, "_by")
		if d.Type == "last_edited_by" {
			key = "last_edited"
		}
		id := page.Str(key + "_by_id")
		if r != nil && id != "" {
			return r.UserName(id)
		}
		return id
	case "auto_increment_id":
		s := PlainText(raw)
		if s == "" {
			return nil
		}
		if p := str(d.Raw["prefix"]); p != "" {
			return p + "-" + s
		}
		return s
	case "formula", "rollup":
		// Computed by Notion clients; the server only sometimes caches a value.
		if len(raw) == 0 {
			return nil
		}
		return PlainTextR(raw, r)
	default:
		if len(raw) == 0 {
			return nil
		}
		return PlainTextR(raw, r)
	}
}

func joinDT(d, t any) string {
	s := str(d)
	if s == "" {
		return ""
	}
	if tt := str(t); tt != "" {
		s += "T" + tt
	}
	return s
}

// PropDisplay formats a property value as a single line of Markdown.
func PropDisplay(page *Record, d PropDef, r Resolver) string {
	v := PropValue(page, d, r)
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "Yes"
		}
		return "No"
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case []string:
		return strings.Join(t, ", ")
	case []map[string]string:
		var parts []string
		for _, m := range t {
			switch {
			case m["title"] != "" || m["url"] != "" && d.Type == "relation":
				parts = append(parts, fmt.Sprintf("[%s](%s)", EscapeMarkdown(m["title"]), m["url"]))
			case m["name"] != "" && m["url"] != "":
				parts = append(parts, fmt.Sprintf("[%s](%s)", EscapeMarkdown(m["name"]), m["url"]))
			case m["name"] != "":
				parts = append(parts, m["name"])
			}
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		s := str(t["start"])
		if e := str(t["end"]); e != "" {
			s += " → " + e
		}
		if tz := str(t["time_zone"]); tz != "" {
			s += " (" + tz + ")"
		}
		return s
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// --- query -------------------------------------------------------------------

type QueryOptions struct {
	ViewID      string
	Limit       int
	Search      string
	Filter      any // internal filter JSON; nil = view filter
	Sort        any // internal sort JSON; nil = view sort
	IgnoreView  bool
	TimeZone    string
}

type QueryResult struct {
	BlockIDs []string
	Total    int
	HasMore  bool
}

// QueryCollection runs a collection query and caches returned rows in the store.
func (s *Store) QueryCollection(ctx context.Context, coll *Record, view *Record, opt QueryOptions) (*QueryResult, error) {
	if opt.Limit <= 0 {
		opt.Limit = 100
	}
	if opt.TimeZone == "" {
		opt.TimeZone = localTZ()
	}
	loader := map[string]any{
		"type":         "reducer",
		"reducers":     map[string]any{"collection_group_results": map[string]any{"type": "results", "limit": opt.Limit}},
		"searchQuery":  opt.Search,
		"userTimeZone": opt.TimeZone,
	}
	var q2 map[string]any
	if view != nil && !opt.IgnoreView {
		q2, _ = view.Value["query2"].(map[string]any)
	}
	if opt.Filter != nil {
		loader["filter"] = opt.Filter
	} else if q2 != nil && q2["filter"] != nil {
		loader["filter"] = q2["filter"]
	}
	if opt.Sort != nil {
		loader["sort"] = opt.Sort
	} else if q2 != nil && q2["sort"] != nil {
		loader["sort"] = q2["sort"]
	} else {
		loader["sort"] = []any{}
	}
	body := map[string]any{
		"source": map[string]any{"type": "collection", "id": coll.ID, "spaceId": coll.SpaceID},
		"loader": loader,
	}
	if view != nil {
		body["collectionView"] = map[string]any{"id": view.ID, "spaceId": view.SpaceID}
	}
	var resp struct {
		RecordMap json.RawMessage `json:"recordMap"`
		Result    struct {
			ReducerResults struct {
				Results struct {
					BlockIDs []string `json:"blockIds"`
					HasMore  bool     `json:"hasMore"`
				} `json:"collection_group_results"`
			} `json:"reducerResults"`
			SizeHint int `json:"sizeHint"`
		} `json:"result"`
	}
	if err := s.C.Call(ctx, "queryCollection", body, &resp); err != nil {
		return nil, err
	}
	if err := s.ingest(resp.RecordMap); err != nil {
		return nil, err
	}
	rr := resp.Result.ReducerResults.Results
	return &QueryResult{BlockIDs: rr.BlockIDs, HasMore: rr.HasMore, Total: resp.Result.SizeHint}, nil
}

func localTZ() string {
	if tz := time.Local.String(); tz != "" && tz != "Local" {
		return tz
	}
	if b, err := readLink("/etc/localtime"); err == nil {
		if i := strings.Index(b, "zoneinfo/"); i >= 0 {
			return b[i+len("zoneinfo/"):]
		}
	}
	return "UTC"
}

// CollectionFor returns the collection and views behind a database block
// (collection_view / collection_view_page) or a collection ID.
func (s *Store) CollectionFor(ctx context.Context, blk *Record) (*Record, []*Record, error) {
	collID := blk.Str("collection_id")
	if p, ok := blk.Format()["collection_pointer"].(map[string]any); ok && collID == "" {
		collID = str(p["id"])
	}
	viewIDs := strSlice(blk.Value["view_ids"])
	ptrs := []Pointer{{Table: "collection", ID: collID, SpaceID: blk.SpaceID}}
	for _, v := range viewIDs {
		ptrs = append(ptrs, Pointer{Table: "collection_view", ID: v, SpaceID: blk.SpaceID})
	}
	if collID == "" && len(viewIDs) > 0 {
		// Linked databases keep the collection pointer on the view.
		if err := s.Fetch(ctx, ptrs[1:], false); err != nil {
			return nil, nil, err
		}
		if v := s.Records.Get("collection_view", viewIDs[0]); v != nil {
			if p, ok := v.Format()["collection_pointer"].(map[string]any); ok {
				collID = str(p["id"])
				ptrs[0].ID = collID
			}
		}
	}
	if collID == "" {
		return nil, nil, fmt.Errorf("block %s is not a database", blk.ID)
	}
	if err := s.Fetch(ctx, ptrs, false); err != nil {
		return nil, nil, err
	}
	coll := s.Records.Get("collection", collID)
	if coll == nil {
		return nil, nil, fmt.Errorf("collection %s is not accessible", collID)
	}
	var views []*Record
	for _, v := range viewIDs {
		if r := s.Records.Get("collection_view", v); r != nil && r.Alive() {
			views = append(views, r)
		}
	}
	return coll, views, nil
}

// CollectionName is the title of a collection.
func CollectionName(coll *Record) string { return PlainText(asSlice(coll.Value["name"])) }
