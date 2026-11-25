package notion

import (
	"reflect"
	"strings"
	"testing"
)

// applyOps applies the subset of transaction semantics the Writer emits to an
// in-memory store, so Markdown → ops → records → Markdown can be tested offline.
func applyOps(t *testing.T, st *Store, ops []Op) {
	t.Helper()
	for _, op := range ops {
		rec := st.Records.Get(op.Pointer.Table, op.Pointer.ID)
		switch op.Command {
		case "set":
			if len(op.Path) == 0 {
				v := op.Args.(map[string]any)
				st.Records.Put(&Record{Table: op.Pointer.Table, ID: op.Pointer.ID, SpaceID: op.Pointer.SpaceID, Value: normalize(v)})
				continue
			}
			setPath(rec.Value, op.Path, normalizeAny(op.Args))
		case "update":
			m := rec.Value
			for _, p := range op.Path {
				m = m[p].(map[string]any)
			}
			for k, v := range op.Args.(map[string]any) {
				m[k] = normalizeAny(v)
			}
		case "listAfter", "listBefore":
			args := op.Args.(map[string]any)
			list := strSlice(rec.Value[op.Path[0]])
			id := args["id"].(string)
			ref, _ := args["after"].(string)
			if b, ok := args["before"].(string); ok {
				ref = b
			}
			pos := len(list)
			for i, x := range list {
				if x == ref {
					pos = i
					if op.Command == "listAfter" {
						pos = i + 1
					}
				}
			}
			if ref == "" && op.Command == "listBefore" {
				pos = 0
			}
			list = append(list[:pos], append([]string{id}, list[pos:]...)...)
			rec.Value[op.Path[0]] = toAnySlice(list)
		case "listRemove":
			id := op.Args.(map[string]any)["id"].(string)
			var out []string
			for _, x := range strSlice(rec.Value[op.Path[0]]) {
				if x != id {
					out = append(out, x)
				}
			}
			rec.Value[op.Path[0]] = toAnySlice(out)
		default:
			t.Fatalf("unexpected op %s", op.Command)
		}
	}
}

func setPath(m map[string]any, path []string, v any) {
	for _, p := range path[:len(path)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[path[len(path)-1]] = v
}

func toAnySlice(s []string) []any {
	out := make([]any, len(s))
	for i, x := range s {
		out[i] = x
	}
	return out
}

// normalize converts typed slices ([]string) into []any like decoded JSON.
func normalize(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = normalizeAny(v)
	}
	return out
}

func normalizeAny(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return normalize(t)
	case []string:
		return toAnySlice(t)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = normalizeAny(x)
		}
		return out
	case int64:
		return float64(t)
	case int:
		return float64(t)
	}
	return v
}

func newTestPage(st *Store) *Record {
	page := &Record{Table: "block", ID: "00000000-0000-4000-8000-000000000001", SpaceID: "s", Value: map[string]any{"type": "page", "alive": true, "content": []any{}}}
	st.Records.Put(page)
	return page
}

func renderPage(st *Store, id string) string {
	return (&Renderer{Store: st}).RenderChildren(st.Records.Block(id))
}

const sample = `# Heading {color="blue"}
Text with **bold**, *italic*, ~~strike~~, ` + "`code`" + `, [link](https://example.com), $` + "`x^2`" + `$ and <span color="red">red</span>.
snake_case_identifier and 2021_0106 stay literal
<mention-user url="user://11111111-2222-4333-8444-555555555555">someone</mention-user> <mention-date start="2026-12-01T10:00" end="2026-12-02"/>
## Lists
- bullet
	- nested
		1. deep numbered
- [ ] todo
- [x] done
1. one
2. two
> quote<br>second line
<details>
<summary>Toggle</summary>
	Hidden
</details>
<callout icon="💡" color="yellow_background">
	Callout text
	- child
</callout>
` + "```go\nfunc main() {\n\tprintln(\"hi\")\n}\n```" + `
$$
\int_0^1 x\,dx
$$
---
<table header-row="true">
	<tr><td>A</td><td>B</td></tr>
	<tr><td>1</td><td>**2**</td></tr>
</table>
<columns>
	<column width="0.5">
		Left
	</column>
	<column width="0.5">
		Right
	</column>
</columns>
![cap](https://example.com/a.png)
<bookmark url="https://example.com">Example</bookmark>
<table_of_contents/>
<empty-block/>
`

func TestRoundTrip(t *testing.T) {
	st := NewStore(nil)
	page := newTestPage(st)
	specs, err := ParseMarkdown(sample)
	if err != nil {
		t.Fatal(err)
	}
	w := &Writer{Store: st, Tx: NewTx("s", "u", "test")}
	w.Create(specs, page.ID, "block", "")
	applyOps(t, st, w.Tx.Ops)
	got := renderPage(st, page.ID)
	want := strings.ReplaceAll(sample, ">someone<", "><") // names come from the resolver
	want = strings.ReplaceAll(want, "\t\t1. deep", "\t\t1. deep")
	got = strings.ReplaceAll(got, `>11111111-2222-4333-8444-555555555555<`, "><")
	if got != want {
		t.Errorf("round trip mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestSyncNoopAndMinimalDiff(t *testing.T) {
	st := NewStore(nil)
	page := newTestPage(st)
	specs, _ := ParseMarkdown(sample)
	w := &Writer{Store: st, Tx: NewTx("s", "u", "test")}
	w.Create(specs, page.ID, "block", "")
	applyOps(t, st, w.Tx.Ops)
	md := renderPage(st, page.ID)

	// Same content: no operations.
	specs, _ = ParseMarkdown(md)
	w = &Writer{Store: st, Tx: NewTx("s", "u", "test")}
	if err := w.Sync(st.Records.Block(page.ID), specs); err != nil {
		t.Fatal(err)
	}
	if len(w.Tx.Ops) != 0 {
		t.Fatalf("expected no ops, got %d: %+v", len(w.Tx.Ops), w.Tx.Ops[0])
	}

	// One edited line: the block keeps its ID.
	before := st.Records.Block(page.ID).Content()
	edited := strings.Replace(md, "- [x] done", "- [x] done and edited", 1)
	specs, _ = ParseMarkdown(edited)
	w = &Writer{Store: st, Tx: NewTx("s", "u", "test")}
	if err := w.Sync(st.Records.Block(page.ID), specs); err != nil {
		t.Fatal(err)
	}
	applyOps(t, st, w.Tx.Ops)
	if after := st.Records.Block(page.ID).Content(); !reflect.DeepEqual(before, after) {
		t.Fatalf("content IDs changed:\n%v\n%v", before, after)
	}
	if got := renderPage(st, page.ID); got != edited {
		t.Fatalf("after edit mismatch:\n%s", got)
	}

	// Insert and delete.
	edited2 := strings.Replace(edited, "## Lists\n", "## Lists\nInserted paragraph\n", 1)
	edited2 = strings.Replace(edited2, "<table_of_contents/>\n", "", 1)
	specs, _ = ParseMarkdown(edited2)
	w = &Writer{Store: st, Tx: NewTx("s", "u", "test")}
	if err := w.Sync(st.Records.Block(page.ID), specs); err != nil {
		t.Fatal(err)
	}
	applyOps(t, st, w.Tx.Ops)
	if got := renderPage(st, page.ID); got != edited2 {
		t.Fatalf("after insert/delete mismatch:\n%s", got)
	}
}

func TestParseInline(t *testing.T) {
	cases := map[string][]any{
		"plain":           {[]any{"plain"}},
		"a **b** c":       {[]any{"a "}, []any{"b", []any{[]any{"b"}}}, []any{" c"}},
		"2021_0106_x":     {[]any{"2021_0106_x"}},
		"_it_":            {[]any{"it", []any{[]any{"i"}}}},
		`\*not\*`:         {[]any{"*not*"}},
		"[a](http://x.y)": {[]any{"a", []any{[]any{"a", "http://x.y"}}}},
		"x <br> y":        {[]any{"x \n y"}},
		"**[l](u)**":      {[]any{"l", []any{[]any{"b"}, []any{"a", "u"}}}},
		"$`a+b`$":         {[]any{"⁍", []any{[]any{"e", "a+b"}}}},
		"`a*b*`":          {[]any{"a*b*", []any{[]any{"c"}}}},
	}
	for in, want := range cases {
		if got := ParseInline(in); !reflect.DeepEqual(normalizeAny(got), normalizeAny(want)) {
			t.Errorf("ParseInline(%q) = %#v, want %#v", in, got, want)
		}
	}
}

func TestParseRef(t *testing.T) {
	cases := map[string]string{
		"https://www.notion.so/My-Page-aaaaaaaabbbb4cccadddeeeeeeeeeeee":                                     "aaaaaaaa-bbbb-4ccc-addd-eeeeeeeeeeee",
		"https://app.notion.com/p/0123456789ab4cdeaf0123456789abcd":                                          "01234567-89ab-4cde-af01-23456789abcd",
		"aaaaaaaa-bbbb-4ccc-addd-eeeeeeeeeeee":                                                               "aaaaaaaa-bbbb-4ccc-addd-eeeeeeeeeeee",
		"https://www.notion.so/ws/abc?v=fedcba9876544321a0fedcba98765432&p=aaaaaaaabbbb4cccadddeeeeeeeeeeee": "aaaaaaaa-bbbb-4ccc-addd-eeeeeeeeeeee",
	}
	for in, want := range cases {
		got, _, _, err := ParseRef(in)
		if err != nil || got != want {
			t.Errorf("ParseRef(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
