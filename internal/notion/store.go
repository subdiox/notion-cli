package notion

import (
	"context"
	"encoding/json"

	"github.com/subdiox/notion-cli/internal/api"
)

type Pointer struct {
	Table   string `json:"table"`
	ID      string `json:"id"`
	SpaceID string `json:"spaceId,omitempty"`
}

// Store is a record cache backed by the API.
type Store struct {
	C       *api.Client
	Records RecordMap
	missing map[string]bool // table:id known to be inaccessible
}

func NewStore(c *api.Client) *Store {
	return &Store{C: c, Records: RecordMap{}, missing: map[string]bool{}}
}

func (s *Store) ingest(raw json.RawMessage) error {
	rm, err := ParseRecordMap(raw)
	if err != nil {
		return err
	}
	s.Records.Merge(rm)
	return nil
}

// Fetch loads records that are not cached yet (force=true reloads).
func (s *Store) Fetch(ctx context.Context, ptrs []Pointer, force bool) error {
	var need []Pointer
	seen := map[string]bool{}
	for _, p := range ptrs {
		if p.ID == "" {
			continue
		}
		k := p.Table + ":" + p.ID
		if seen[k] || (!force && (s.Records.Get(p.Table, p.ID) != nil || s.missing[k])) {
			continue
		}
		seen[k] = true
		need = append(need, p)
	}
	for len(need) > 0 {
		n := min(len(need), 100)
		batch := need[:n]
		need = need[n:]
		reqs := make([]map[string]any, len(batch))
		for i, p := range batch {
			reqs[i] = map[string]any{"pointer": p, "version": -1}
		}
		var resp struct {
			RecordMap json.RawMessage `json:"recordMap"`
		}
		if err := s.C.Call(ctx, "syncRecordValues", map[string]any{"requests": reqs}, &resp); err != nil {
			return err
		}
		if err := s.ingest(resp.RecordMap); err != nil {
			return err
		}
		for _, p := range batch {
			if s.Records.Get(p.Table, p.ID) == nil {
				s.missing[p.Table+":"+p.ID] = true
			}
		}
	}
	return nil
}

// Get returns a cached record, fetching it if needed.
func (s *Store) Get(ctx context.Context, table, id string) (*Record, error) {
	if r := s.Records.Get(table, id); r != nil {
		return r, nil
	}
	if err := s.Fetch(ctx, []Pointer{{Table: table, ID: id}}, false); err != nil {
		return nil, err
	}
	return s.Records.Get(table, id), nil
}

// Role returns the caller's role for a record ("none" if inaccessible).
func (s *Store) Role(ctx context.Context, table, id string) (string, string, error) {
	var resp struct {
		RecordMap json.RawMessage `json:"recordMap"`
	}
	err := s.C.Call(ctx, "syncRecordValues", map[string]any{"requests": []any{map[string]any{"pointer": Pointer{Table: table, ID: id}, "version": -1}}}, &resp)
	if err != nil {
		return "", "", err
	}
	rm, err := ParseRecordMap(resp.RecordMap)
	if err != nil {
		return "", "", err
	}
	s.Records.Merge(rm)
	if t := rm[table]; t != nil {
		if r := t[id]; r != nil {
			if r.Value == nil {
				return "none", r.SpaceID, nil
			}
			return r.Role, r.SpaceID, nil
		}
	}
	return "none", "", nil
}

// LoadPage loads all blocks of a page via loadPageChunk (following cursors),
// then fills any children the chunks omitted. Sub-pages are not descended.
func (s *Store) LoadPage(ctx context.Context, pageID string) error {
	// loadPageChunk is rate limited separately from (and much more strictly
	// than) syncRecordValues. While it is limited, load the tree level by level
	// with syncRecordValues instead.
	if !s.C.Degraded("loadPageChunk") {
		err := s.loadPageChunks(api.WithoutRateLimitRetry(ctx), pageID)
		if err == nil {
			return s.LoadDescendants(ctx, pageID)
		}
		if !api.IsRateLimited(err) {
			return err
		}
	}
	if err := s.Fetch(ctx, []Pointer{{Table: "block", ID: pageID}}, false); err != nil {
		return err
	}
	return s.LoadDescendants(ctx, pageID)
}

func (s *Store) loadPageChunks(ctx context.Context, pageID string) error {
	cursor := map[string]any{"stack": []any{}}
	for chunk := 0; chunk < 200; chunk++ {
		var resp struct {
			RecordMap json.RawMessage `json:"recordMap"`
			Cursor    struct {
				Stack []any `json:"stack"`
			} `json:"cursor"`
		}
		err := s.C.Call(ctx, "loadPageChunk", map[string]any{
			"pageId": pageID, "limit": 100, "cursor": cursor, "chunkNumber": chunk, "verticalColumns": false,
		}, &resp)
		if err != nil {
			return err
		}
		if err := s.ingest(resp.RecordMap); err != nil {
			return err
		}
		if len(resp.Cursor.Stack) == 0 {
			return nil
		}
		cursor = map[string]any{"stack": resp.Cursor.Stack}
	}
	return nil
}

// LoadDescendants makes sure every non-page descendant block of root is cached.
func (s *Store) LoadDescendants(ctx context.Context, root string) error {
	frontier := []string{root}
	visited := map[string]bool{}
	for len(frontier) > 0 {
		var ptrs []Pointer
		for _, id := range frontier {
			b := s.Records.Block(id)
			if b == nil {
				continue
			}
			for _, c := range b.Content() {
				if s.Records.Block(c) == nil {
					ptrs = append(ptrs, Pointer{Table: "block", ID: c, SpaceID: b.SpaceID})
				}
			}
		}
		if err := s.Fetch(ctx, ptrs, false); err != nil {
			return err
		}
		var next []string
		for _, id := range frontier {
			if visited[id] {
				continue
			}
			visited[id] = true
			b := s.Records.Block(id)
			if b == nil {
				continue
			}
			for _, c := range b.Content() {
				cb := s.Records.Block(c)
				if cb == nil || visited[c] {
					continue
				}
				if IsPageType(cb.Type()) && c != root {
					continue
				}
				next = append(next, c)
			}
			// Synced block references point at another block's content.
			if b.Type() == "transclusion_reference" {
				if src := syncedSource(b); src != "" && !visited[src] {
					if err := s.Fetch(ctx, []Pointer{{Table: "block", ID: src}}, false); err != nil {
						return err
					}
					next = append(next, src)
				}
			}
		}
		frontier = next
	}
	return nil
}

func syncedSource(b *Record) string {
	f := b.Format()
	if p, ok := f["transclusion_reference_pointer"].(map[string]any); ok {
		return str(p["id"])
	}
	return ""
}

func IsPageType(t string) bool {
	return t == "page" || t == "collection_view_page"
}

// --- Resolver ---------------------------------------------------------------

// PrefetchMentions batch-loads pages/users referenced from the given blocks.
func (s *Store) PrefetchMentions(ctx context.Context, blocks []*Record) error {
	var ptrs []Pointer
	add := func(v []any) {
		pages, users := MentionIDs(v)
		for _, p := range pages {
			ptrs = append(ptrs, Pointer{Table: "block", ID: p})
		}
		for _, u := range users {
			ptrs = append(ptrs, Pointer{Table: "notion_user", ID: u})
		}
	}
	for _, b := range blocks {
		for _, v := range b.Properties() {
			if arr, ok := v.([]any); ok {
				add(arr)
			}
		}
		if t := b.Type(); t == "collection_view" || t == "collection_view_page" {
			if cid := b.Str("collection_id"); cid != "" {
				ptrs = append(ptrs, Pointer{Table: "collection", ID: cid, SpaceID: b.SpaceID})
			}
		}
		if b.Type() == "alias" {
			if p, ok := b.Format()["alias_pointer"].(map[string]any); ok {
				ptrs = append(ptrs, Pointer{Table: "block", ID: str(p["id"])})
			}
		}
		for _, k := range []string{"created_by_id", "last_edited_by_id"} {
			if id := b.Str(k); id != "" {
				ptrs = append(ptrs, Pointer{Table: "notion_user", ID: id})
			}
		}
	}
	return s.Fetch(ctx, ptrs, false)
}

func (s *Store) PageTitle(id string) string {
	b := s.Records.Block(id)
	if b == nil {
		if c := s.Records.Get("collection", id); c != nil {
			return PlainText(asSlice(c.Value["name"]))
		}
		return "Untitled"
	}
	if b.Type() == "collection_view_page" || b.Type() == "collection_view" {
		if c := s.Records.Get("collection", b.Str("collection_id")); c != nil {
			if t := PlainText(asSlice(c.Value["name"])); t != "" {
				return t
			}
		}
	}
	if t := b.Title(); t != "" {
		return t
	}
	return "Untitled"
}

func (s *Store) UserName(id string) string {
	if u := s.Records.Get("notion_user", id); u != nil {
		if n := u.Str("name"); n != "" {
			return n
		}
		if e := u.Str("email"); e != "" {
			return e
		}
	}
	return id
}

func (s *Store) PageURL(id string) string {
	return PageURL(id)
}

// PageURL is the canonical URL of a page; www.notion.so redirects to the
// right host for both the web and the desktop app.
func PageURL(id string) string {
	return "https://www.notion.so/" + CompactID(id)
}

func asSlice(v any) []any { a, _ := v.([]any); return a }
