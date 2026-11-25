// Package notion implements Notion's data model (records, blocks, collections)
// on top of the internal API client.
package notion

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Record is one row of a Notion table (block, collection, space, ...).
type Record struct {
	Table   string         `json:"table"`
	ID      string         `json:"id"`
	SpaceID string         `json:"space_id,omitempty"`
	Role    string         `json:"role,omitempty"`
	Value   map[string]any `json:"value"`
}

// RecordMap is table -> id -> record.
type RecordMap map[string]map[string]*Record

// ParseRecordMap decodes the "recordMap" object of an API response. It accepts
// both the v3 shape {spaceId, value:{role, value}} and the legacy {role, value}.
func ParseRecordMap(raw json.RawMessage) (RecordMap, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return RecordMap{}, nil
	}
	var tables map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tables); err != nil {
		return nil, err
	}
	rm := RecordMap{}
	for table, rawRecs := range tables {
		if strings.HasPrefix(table, "__") {
			continue
		}
		var recs map[string]map[string]any
		if err := json.Unmarshal(rawRecs, &recs); err != nil {
			continue // not a table
		}
		for id, r := range recs {
			rec := &Record{Table: table, ID: id}
			rec.SpaceID, _ = r["spaceId"].(string)
			inner, _ := r["value"].(map[string]any)
			if inner != nil {
				if v, ok := inner["value"].(map[string]any); ok {
					rec.Role, _ = inner["role"].(string)
					rec.Value = v
				} else if _, hasRole := inner["role"]; hasRole && len(inner) == 1 {
					rec.Role, _ = inner["role"].(string)
				} else {
					rec.Role, _ = r["role"].(string)
					rec.Value = inner
				}
			}
			if rec.SpaceID == "" && rec.Value != nil {
				rec.SpaceID, _ = rec.Value["space_id"].(string)
			}
			rm.Put(rec)
		}
	}
	return rm, nil
}

func (rm RecordMap) Put(r *Record) {
	if rm[r.Table] == nil {
		rm[r.Table] = map[string]*Record{}
	}
	// Never let an inaccessible stub overwrite a real record.
	if old := rm[r.Table][r.ID]; old != nil && old.Value != nil && r.Value == nil {
		return
	}
	rm[r.Table][r.ID] = r
}

func (rm RecordMap) Merge(other RecordMap) {
	for _, recs := range other {
		for _, r := range recs {
			rm.Put(r)
		}
	}
}

func (rm RecordMap) Get(table, id string) *Record {
	if t := rm[table]; t != nil {
		if r := t[id]; r != nil && r.Value != nil {
			return r
		}
	}
	return nil
}

func (rm RecordMap) Block(id string) *Record { return rm.Get("block", id) }

// --- value accessors -------------------------------------------------------

func (r *Record) Str(key string) string { return str(r.Value[key]) }

func (r *Record) Type() string { return r.Str("type") }

func (r *Record) Alive() bool {
	a, ok := r.Value["alive"].(bool)
	return !ok || a
}

func (r *Record) Content() []string { return strSlice(r.Value["content"]) }

func (r *Record) Properties() map[string]any { return mapAt(r.Value, "properties") }

func (r *Record) Format() map[string]any { return mapAt(r.Value, "format") }

// Prop returns a raw rich-text property value (e.g. "title").
func (r *Record) Prop(key string) []any {
	v, _ := r.Properties()[key].([]any)
	return v
}

func (r *Record) Title() string { return PlainText(r.Prop("title")) }

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprint(int64(t))
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

func strSlice(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func mapAt(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	if v == nil {
		return map[string]any{}
	}
	return v
}

// --- IDs -------------------------------------------------------------------

var (
	uuidRe  = regexp.MustCompile(`(?i)[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}`)
	hex32Re = regexp.MustCompile(`(?i)^[0-9a-f]{32}$`)
)

// NormalizeID turns 32-hex or dashed UUIDs into canonical dashed lowercase.
func NormalizeID(s string) string {
	s = strings.ToLower(strings.ReplaceAll(s, "-", ""))
	if !hex32Re.MatchString(s) {
		return ""
	}
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// ParseRef extracts the target ID from a Notion URL or raw ID.
// It returns (id, viewID, blockAnchor). For URLs with ?p=<id> (peek) the peeked page wins.
func ParseRef(ref string) (id, viewID, anchor string, err error) {
	ref = strings.TrimSpace(ref)
	if !strings.Contains(ref, "/") && !strings.Contains(ref, "?") {
		if id = NormalizeID(ref); id != "" {
			return id, "", "", nil
		}
		if m := uuidRe.FindString(ref); m != "" {
			return NormalizeID(m), "", "", nil
		}
		return "", "", "", fmt.Errorf("not a Notion ID or URL: %q", ref)
	}
	path, query, frag := ref, "", ""
	if i := strings.Index(path, "#"); i >= 0 {
		path, frag = path[:i], path[i+1:]
	}
	if i := strings.Index(path, "?"); i >= 0 {
		path, query = path[:i], path[i+1:]
	}
	for _, kv := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "p":
			if pid := NormalizeID(v); pid != "" {
				id = pid
			}
		case "v":
			viewID = NormalizeID(v)
		}
	}
	if id == "" {
		matches := uuidRe.FindAllString(path, -1)
		if len(matches) == 0 {
			return "", "", "", fmt.Errorf("no Notion ID in URL: %q", ref)
		}
		id = NormalizeID(matches[len(matches)-1])
	}
	if frag != "" {
		anchor = NormalizeID(frag)
	}
	return id, viewID, anchor, nil
}

// CompactID is the 32-hex form used in URLs.
func CompactID(id string) string { return strings.ReplaceAll(id, "-", "") }
