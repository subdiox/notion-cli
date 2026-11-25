package notion

import (
	"fmt"
	"strconv"
	"strings"
)

var optionColors = []string{"default", "gray", "brown", "orange", "yellow", "green", "blue", "purple", "pink", "red"}

// PropEncoder converts user input into internal property values.
type PropEncoder struct {
	ResolveUser func(string) (string, error)
	SpaceID     string
	// NewOptions collects select options that must be added to the schema.
	NewOptions map[string][]SelectOption
}

// Encode converts v (a string, or a JSON value as printed by `fetch --json`)
// into the internal rich-text property format.
func (e *PropEncoder) Encode(d PropDef, v any) (any, error) {
	if v == nil {
		return []any{}, nil
	}
	list := func() []string {
		switch t := v.(type) {
		case []any:
			var out []string
			for _, x := range t {
				switch xx := x.(type) {
				case map[string]any:
					for _, k := range []string{"id", "url", "name", "title", "email"} {
						if s := str(xx[k]); s != "" {
							out = append(out, s)
							break
						}
					}
				default:
					if s := str(xx); s != "" {
						out = append(out, s)
					}
				}
			}
			return out
		case string:
			if strings.TrimSpace(t) == "" {
				return nil
			}
			parts := strings.Split(t, ",")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			return parts
		default:
			return []string{str(t)}
		}
	}
	s, isStr := v.(string)
	switch d.Type {
	case "title", "text":
		if !isStr {
			s = str(v)
		}
		return ParseInline(s), nil
	case "number":
		if isStr {
			if strings.TrimSpace(s) == "" {
				return []any{}, nil
			}
			if _, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err != nil {
				return nil, fmt.Errorf("%s: %q is not a number", d.Name, s)
			}
			return []any{[]any{strings.TrimSpace(s)}}, nil
		}
		if f, ok := v.(float64); ok {
			return []any{[]any{strconv.FormatFloat(f, 'f', -1, 64)}}, nil
		}
	case "select", "status":
		if !isStr {
			s = str(v)
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return []any{}, nil
		}
		e.ensureOption(d, s)
		return []any{[]any{s}}, nil
	case "multi_select":
		vals := list()
		if len(vals) == 0 {
			return []any{}, nil
		}
		for _, x := range vals {
			e.ensureOption(d, x)
		}
		return []any{[]any{strings.Join(vals, ",")}}, nil
	case "checkbox":
		b := false
		switch t := v.(type) {
		case bool:
			b = t
		case string:
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "yes", "true", "1", "x", "✓", "on", "checked":
				b = true
			}
		}
		if b {
			return []any{[]any{"Yes"}}, nil
		}
		return []any{[]any{"No"}}, nil
	case "date":
		var start, end, tz string
		switch t := v.(type) {
		case string:
			t = strings.TrimSpace(t)
			if t == "" {
				return []any{}, nil
			}
			for _, sep := range []string{"→", "->", " to ", "/"} {
				if a, b, ok := strings.Cut(t, sep); ok && sep != "/" || ok && len(a) >= 10 && len(b) >= 10 {
					start, end = strings.TrimSpace(a), strings.TrimSpace(b)
					break
				}
			}
			if start == "" {
				start = t
			}
		case map[string]any:
			start, end, tz = str(t["start"]), str(t["end"]), str(t["time_zone"])
		}
		dv := DateValue(start, end, tz)
		if dv == nil {
			return nil, fmt.Errorf("%s: invalid date %v", d.Name, v)
		}
		return []any{[]any{"‣", []any{[]any{"d", dv}}}}, nil
	case "person":
		var out []any
		for i, x := range list() {
			uid := NormalizeID(strings.TrimPrefix(x, "user://"))
			if uid == "" {
				if e.ResolveUser == nil {
					return nil, fmt.Errorf("%s: cannot resolve user %q", d.Name, x)
				}
				var err error
				if uid, err = e.ResolveUser(x); err != nil {
					return nil, err
				}
			}
			if i > 0 {
				out = append(out, []any{","})
			}
			out = append(out, []any{"‣", []any{[]any{"u", uid}}})
		}
		return out, nil
	case "relation":
		var out []any
		for i, x := range list() {
			id, _, _, err := ParseRef(x)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", d.Name, err)
			}
			if i > 0 {
				out = append(out, []any{","})
			}
			out = append(out, []any{"‣", []any{[]any{"p", id, e.SpaceID}}})
		}
		return out, nil
	case "file":
		var out []any
		for i, x := range list() {
			if i > 0 {
				out = append(out, []any{","})
			}
			out = append(out, []any{FileName(x, x), []any{[]any{"a", x}}})
		}
		return out, nil
	case "url", "email", "phone_number":
		if !isStr {
			s = str(v)
		}
		if strings.TrimSpace(s) == "" {
			return []any{}, nil
		}
		return []any{[]any{strings.TrimSpace(s)}}, nil
	case "formula", "rollup", "created_time", "last_edited_time", "created_by", "last_edited_by", "auto_increment_id", "button", "verification":
		return nil, fmt.Errorf("%s: %s properties are read-only", d.Name, d.Type)
	default:
		if !isStr {
			s = str(v)
		}
		return ParseInline(s), nil
	}
	return nil, fmt.Errorf("%s: cannot convert %v to %s", d.Name, v, d.Type)
}

func (e *PropEncoder) ensureOption(d PropDef, value string) {
	for _, o := range d.Options {
		if o.Value == value {
			return
		}
	}
	for _, o := range e.NewOptions[d.ID] {
		if o.Value == value {
			return
		}
	}
	if d.Type == "status" {
		return // status options can't be created implicitly
	}
	if e.NewOptions == nil {
		e.NewOptions = map[string][]SelectOption{}
	}
	n := len(d.Options) + len(e.NewOptions[d.ID])
	e.NewOptions[d.ID] = append(e.NewOptions[d.ID], SelectOption{ID: NewID(), Value: value, Color: optionColors[(n+1)%len(optionColors)]})
}

// ApplyNewOptions adds collected select options to the collection schema.
func (e *PropEncoder) ApplyNewOptions(tx *Tx, coll *Record) {
	for propID, opts := range e.NewOptions {
		for _, o := range opts {
			tx.Ops = append(tx.Ops, Op{
				Pointer: tx.ptr("collection", coll.ID), Path: []string{"schema", propID, "options"},
				Command: "keyedObjectListAfter",
				Args:    map[string]any{"value": map[string]any{"id": o.ID, "value": o.Value, "color": o.Color}},
			})
		}
	}
}
