package notion

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// Writer turns block specs into transaction operations.
type Writer struct {
	Store *Store
	Tx    *Tx
	Ctx   context.Context
	// BaseDir resolves relative local file paths in Markdown.
	BaseDir string
	// Warn receives non-fatal notices.
	Warn func(string)
	// AllowDeletePages lets a sync trash sub-pages/databases missing from the Markdown.
	AllowDeletePages bool
}

func (w *Writer) warn(format string, a ...any) {
	if w.Warn != nil {
		w.Warn(fmt.Sprintf(format, a...))
	}
}

var textLike = map[string]bool{
	"text": true, "header": true, "sub_header": true, "sub_sub_header": true, "bulleted_list": true,
	"numbered_list": true, "to_do": true, "toggle": true, "quote": true, "callout": true,
}

func randomColID() string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#$%&()*+,-./:;<=>?@[]^_`{|}~"
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

// PrepareUploads uploads local files referenced by specs (recursively).
func (w *Writer) PrepareUploads(specs []*BlockSpec, owner Pointer) error {
	for _, s := range specs {
		if s.LocalFile != "" {
			p := s.LocalFile
			if !filepath.IsAbs(p) && w.BaseDir != "" {
				p = filepath.Join(w.BaseDir, p)
			}
			url, fileID, err := w.Store.UploadFile(w.Ctx, p, owner)
			if err != nil {
				return fmt.Errorf("upload %s: %w", s.LocalFile, err)
			}
			if s.Props == nil {
				s.Props = map[string]any{}
			}
			s.Props["source"] = []any{[]any{url}}
			if s.Type != "image" {
				if _, ok := s.Props["title"]; !ok {
					s.Props["title"] = []any{[]any{filepath.Base(p)}}
				}
			}
			if s.Format == nil {
				s.Format = map[string]any{}
			}
			s.Format["display_source"] = url
			s.fileID = fileID
			s.LocalFile = ""
		}
		if err := w.PrepareUploads(s.Children, owner); err != nil {
			return err
		}
	}
	return nil
}

// Create emits operations creating specs under parent (inserted after `after`,
// or appended when after == ""). It returns the new top-level block IDs.
func (w *Writer) Create(specs []*BlockSpec, parentID, parentTable, after string) []string {
	var ids []string
	prev := after
	for _, s := range specs {
		id := w.createOne(s, parentID, parentTable)
		if id == "" {
			continue
		}
		if parentTable == "block" || parentTable == "" {
			w.Tx.ListAfter("block", parentID, []string{"content"}, id, prev)
		}
		prev = id
		ids = append(ids, id)
	}
	return ids
}

// createOne creates a block (and its subtree, via the content array) without
// inserting it into the parent's content list.
func (w *Writer) createOne(s *BlockSpec, parentID, parentTable string) string {
	if parentTable == "" {
		parentTable = "block"
	}
	switch {
	case s.Truncated:
		return ""
	case s.Type == "unknown":
		w.warn("skipping <unknown> block (cannot be recreated)")
		return ""
	case s.RefID != "" && (s.Type == "page" || s.Type == "collection_view_page" || s.Type == "collection_view"):
		// Reference to an existing page: link to it instead of duplicating.
		s = &BlockSpec{Type: "alias", Format: map[string]any{"alias_pointer": map[string]any{"id": s.RefID, "table": "block", "spaceId": w.Tx.SpaceID}}}
	case s.RefID != "" && s.Type == "transclusion_container":
		s = &BlockSpec{Type: "transclusion_reference", Format: map[string]any{"transclusion_reference_pointer": map[string]any{"id": s.RefID, "table": "block", "spaceId": w.Tx.SpaceID}}}
	case s.Type == "collection_view_page" || s.Type == "collection_view":
		w.warn("<database> without url cannot be created from Markdown; use `notion db create`")
		return ""
	}
	id := NewID()
	v := w.Tx.NewBlockValue(id, s.Type, parentID, parentTable)
	if len(s.Props) > 0 {
		v["properties"] = s.Props
	}
	format := map[string]any{}
	for k, val := range s.Format {
		format[k] = val
	}
	if p, ok := format["transclusion_reference_pointer"].(map[string]any); ok {
		p["spaceId"] = w.Tx.SpaceID
	}
	if p, ok := format["alias_pointer"].(map[string]any); ok {
		p["spaceId"] = w.Tx.SpaceID
	}
	if s.fileID != "" {
		v["file_ids"] = []any{s.fileID}
	}
	// Reserve the slot for this block's "set" so it precedes its children.
	w.Tx.Ops = append(w.Tx.Ops, Op{})
	idx := len(w.Tx.Ops) - 1
	var children []string
	switch s.Type {
	case "table":
		ncols := 0
		for _, r := range s.Rows {
			ncols = max(ncols, len(r))
		}
		cols := make([]string, ncols)
		seen := map[string]bool{}
		for i := range cols {
			for {
				c := randomColID()
				if !seen[c] {
					seen[c] = true
					cols[i] = c
					break
				}
			}
		}
		format["table_block_column_order"] = cols
		for _, row := range s.Rows {
			rid := NewID()
			rv := w.Tx.NewBlockValue(rid, "table_row", id, "block")
			props := map[string]any{}
			for i, cell := range row {
				props[cols[i]] = cell
			}
			rv["properties"] = props
			w.Tx.Set("block", rid, nil, rv)
			children = append(children, rid)
		}
	default:
		for _, c := range s.Children {
			if cid := w.createOne(c, id, "block"); cid != "" {
				children = append(children, cid)
			}
		}
	}
	if len(format) > 0 {
		v["format"] = format
	}
	if len(children) > 0 {
		v["content"] = children
	}
	w.Tx.Ops[idx] = Op{Pointer: w.Tx.ptr("block", id), Path: []string{}, Command: "set", Args: v}
	return id
}

// --- trash / move ---------------------------------------------------------

// Remove soft-deletes a block and removes it from its parent's content.
func (w *Writer) Remove(b *Record) {
	w.Tx.Update("block", b.ID, nil, map[string]any{"alive": false})
	if b.Str("parent_table") == "block" {
		w.Tx.ListRemove("block", b.Str("parent_id"), []string{"content"}, b.ID)
	}
}

// Move moves a block under a new parent (block or collection), after `after`.
func (w *Writer) Move(b *Record, newParent, newParentTable, after string) {
	if b.Str("parent_table") == "block" {
		w.Tx.ListRemove("block", b.Str("parent_id"), []string{"content"}, b.ID)
	} else if b.Str("parent_table") == "space" {
		// private top-level page: nothing to unlink here
	}
	w.Tx.Update("block", b.ID, nil, map[string]any{"parent_id": newParent, "parent_table": newParentTable, "alive": true})
	if newParentTable == "block" {
		w.Tx.ListAfter("block", newParent, []string{"content"}, b.ID, after)
	}
}

// --- diff sync --------------------------------------------------------------

// existingSpec renders an existing block and parses it back, so it is directly
// comparable with specs parsed from user Markdown.
func (w *Writer) existingSpec(b *Record) *BlockSpec {
	r := &Renderer{Store: w.Store}
	wrapper := &Record{Table: "block", Value: map[string]any{"content": []any{b.ID}}}
	md := r.RenderChildren(wrapper)
	specs, err := ParseMarkdown(md)
	if err != nil || len(specs) != 1 {
		return &BlockSpec{Type: b.Type(), RefID: b.ID}
	}
	return specs[0]
}

func specKey(s *BlockSpec) string {
	if s.RefID != "" && s.Type != "transclusion_container" {
		return s.Type + ":" + s.RefID
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// Sync makes parent's children match specs with minimal operations, keeping
// IDs of unchanged blocks (so comments, links and backlinks survive).
func (w *Writer) Sync(parent *Record, specs []*BlockSpec) error {
	return w.SyncAt(parent, specs, "")
}

// SyncAt is Sync where insertions before the first kept block go after `start`.
func (w *Writer) SyncAt(parent *Record, specs []*BlockSpec, start string) error {
	keepRest := false
	var clean []*BlockSpec
	for _, s := range specs {
		if s.Truncated {
			keepRest = true
			continue
		}
		clean = append(clean, s)
	}
	specs = clean

	var old []*Record
	for _, id := range parent.Content() {
		if b := w.Store.Records.Block(id); b != nil && b.Alive() {
			old = append(old, b)
		}
	}
	oldSpecs := make([]*BlockSpec, len(old))
	oldKeys := make([]string, len(old))
	for i, b := range old {
		oldSpecs[i] = w.existingSpec(b)
		oldKeys[i] = specKey(oldSpecs[i])
	}
	newKeys := make([]string, len(specs))
	for i, s := range specs {
		newKeys[i] = specKey(s)
	}

	// LCS table
	n, m := len(old), len(specs)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if oldKeys[i] == newKeys[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}

	prev := start // last block in the final order
	var pendingOld []int
	var pendingNew []int
	flush := func() error {
		k := 0
		for ; k < len(pendingOld) && k < len(pendingNew); k++ {
			ob, os_, ns := old[pendingOld[k]], oldSpecs[pendingOld[k]], specs[pendingNew[k]]
			if canUpdateInPlace(os_, ns) {
				if err := w.updateInPlace(ob, os_, ns); err != nil {
					return err
				}
				prev = ob.ID
				continue
			}
			// Not compatible: delete the old one, insert the new one here.
			w.removeOrKeep(ob, &prev, keepRest)
			ids := w.insert(ns, parent.ID, prev, old)
			if len(ids) > 0 {
				prev = ids[len(ids)-1]
			}
		}
		for ; k < len(pendingOld); k++ {
			w.removeOrKeep(old[pendingOld[k]], &prev, keepRest)
		}
		for kk := k; kk < len(pendingNew); kk++ {
			ids := w.insert(specs[pendingNew[kk]], parent.ID, prev, old)
			if len(ids) > 0 {
				prev = ids[len(ids)-1]
			}
		}
		pendingOld, pendingNew = nil, nil
		return nil
	}

	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && oldKeys[i] == newKeys[j]:
			if err := flush(); err != nil {
				return err
			}
			prev = old[i].ID
			i++
			j++
		case j < m && (i == n || dp[i][j+1] >= dp[i+1][j]):
			pendingNew = append(pendingNew, j)
			j++
		default:
			pendingOld = append(pendingOld, i)
			i++
		}
	}
	return flush()
}

func canUpdateInPlace(o, n *BlockSpec) bool {
	if o.RefID != "" || n.RefID != "" || n.LocalFile != "" {
		return false
	}
	if o.Type == n.Type {
		return o.Type != "table" && o.Type != "column_list" && o.Type != "transclusion_reference"
	}
	return textLike[o.Type] && textLike[n.Type]
}

func (w *Writer) removeOrKeep(b *Record, prev *string, keep bool) {
	t := b.Type()
	if keep || (IsPageType(t) || t == "collection_view") && !w.AllowDeletePages {
		if !keep {
			w.warn("kept %s %s (%s) that is missing from the Markdown; pass --allow-delete-pages to trash it", t, b.ID, w.Store.PageTitle(b.ID))
		}
		*prev = b.ID
		return
	}
	w.Remove(b)
}

// insert creates a spec at a position. References to existing pages that live
// elsewhere are moved here; references to existing children are kept.
func (w *Writer) insert(s *BlockSpec, parentID, after string, siblings []*Record) []string {
	if s.RefID != "" && (s.Type == "page" || s.Type == "collection_view_page" || s.Type == "collection_view") {
		for _, sib := range siblings {
			if sib.ID == s.RefID {
				// Reordered sibling: move within the parent.
				w.Tx.ListRemove("block", parentID, []string{"content"}, sib.ID)
				w.Tx.ListAfter("block", parentID, []string{"content"}, sib.ID, after)
				return []string{sib.ID}
			}
		}
		if b := w.Store.Records.Block(s.RefID); b != nil {
			w.Move(b, parentID, "block", after)
			return []string{b.ID}
		}
	}
	return w.Create([]*BlockSpec{s}, parentID, "block", after)
}

func (w *Writer) updateInPlace(b *Record, o, n *BlockSpec) error {
	if o.Type != n.Type {
		w.Tx.Set("block", b.ID, []string{"type"}, n.Type)
	}
	for _, k := range unionKeys(o.Props, n.Props) {
		ov, nv := o.Props[k], n.Props[k]
		if reflect.DeepEqual(ov, nv) {
			continue
		}
		if nv == nil {
			nv = []any{}
		}
		w.Tx.Set("block", b.ID, []string{"properties", k}, nv)
	}
	for _, k := range unionKeys(o.Format, n.Format) {
		ov, nv := o.Format[k], n.Format[k]
		if reflect.DeepEqual(ov, nv) {
			continue
		}
		if k == "column_ratio" {
			if of, ok := ov.(float64); ok {
				if nf, ok := nv.(float64); ok && fmt.Sprintf("%.3g", of) == fmt.Sprintf("%.3g", nf) {
					continue
				}
			}
		}
		w.Tx.Set("block", b.ID, []string{"format", k}, nv)
	}
	if len(o.Children) > 0 || len(n.Children) > 0 {
		if specKeyList(o.Children) != specKeyList(n.Children) {
			if err := w.Sync(b, n.Children); err != nil {
				return err
			}
		}
	}
	w.Tx.Touch("block", b.ID)
	return nil
}

func specKeyList(specs []*BlockSpec) string {
	var b strings.Builder
	for _, s := range specs {
		b.WriteString(specKey(s))
		b.WriteByte('\n')
	}
	return b.String()
}

func unionKeys(a, b map[string]any) []string {
	seen := map[string]bool{}
	var keys []string
	for k := range a {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys
}

// --- uploads ----------------------------------------------------------------

// UploadFile uploads a local file to Notion's file storage and returns the
// "attachment:" URL to store in a block, plus the file ID.
func (s *Store) UploadFile(ctx context.Context, path string, owner Pointer) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", "", err
	}
	name := filepath.Base(path)
	ctype := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if ctype == "" {
		head := make([]byte, 512)
		n, _ := io.ReadFull(f, head)
		ctype = http.DetectContentType(head[:n])
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return "", "", err
		}
	}
	if i := strings.Index(ctype, ";"); i >= 0 && !strings.HasPrefix(ctype, "text/") {
		ctype = ctype[:i]
	}
	var resp struct {
		URL          string `json:"url"`
		SignedPutURL string `json:"signedPutUrl"`
		LinkedFileID string `json:"linkedFileId"`
		PutHeaders   []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"putHeaders"`
	}
	err = s.C.Call(ctx, "getUploadFileUrl", map[string]any{
		"bucket": "secure", "name": name, "contentType": ctype,
		"record": owner, "contentLength": st.Size(),
	}, &resp)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, resp.SignedPutURL, f)
	if err != nil {
		return "", "", err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Content-Type", ctype)
	for _, h := range resp.PutHeaders {
		req.Header.Set(h.Name, h.Value)
	}
	r, err := s.C.HTTP.Do(req)
	if err != nil {
		return "", "", err
	}
	defer r.Body.Close()
	if r.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 500))
		return "", "", fmt.Errorf("upload PUT: HTTP %d: %s", r.StatusCode, b)
	}
	return resp.URL, resp.LinkedFileID, nil
}
