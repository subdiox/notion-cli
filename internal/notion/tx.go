package notion

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Op is one operation of a Notion transaction.
type Op struct {
	Pointer Pointer  `json:"pointer"`
	Path    []string `json:"path"`
	Command string   `json:"command"`
	Args    any      `json:"args"`
}

// Tx accumulates operations for one space.
type Tx struct {
	SpaceID string
	UserID  string
	Ops     []Op
	Action  string
}

func NewTx(spaceID, userID, action string) *Tx {
	return &Tx{SpaceID: spaceID, UserID: userID, Action: action}
}

func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func nowMs() int64 { return time.Now().UnixMilli() }

func (t *Tx) ptr(table, id string) Pointer { return Pointer{Table: table, ID: id, SpaceID: t.SpaceID} }

func (t *Tx) Set(table, id string, path []string, args any) {
	if path == nil {
		path = []string{}
	}
	t.Ops = append(t.Ops, Op{Pointer: t.ptr(table, id), Path: path, Command: "set", Args: args})
}

func (t *Tx) Update(table, id string, path []string, args map[string]any) {
	if path == nil {
		path = []string{}
	}
	t.Ops = append(t.Ops, Op{Pointer: t.ptr(table, id), Path: path, Command: "update", Args: args})
}

// ListAfter inserts id into table/parent's list at path, after `after` (or at the end).
func (t *Tx) ListAfter(table, parent string, path []string, id, after string) {
	args := map[string]any{"id": id}
	if after != "" {
		args["after"] = after
	}
	t.Ops = append(t.Ops, Op{Pointer: t.ptr(table, parent), Path: path, Command: "listAfter", Args: args})
}

// ListBefore inserts id before `before` (or at the start).
func (t *Tx) ListBefore(table, parent string, path []string, id, before string) {
	args := map[string]any{"id": id}
	if before != "" {
		args["before"] = before
	}
	t.Ops = append(t.Ops, Op{Pointer: t.ptr(table, parent), Path: path, Command: "listBefore", Args: args})
}

func (t *Tx) ListRemove(table, parent string, path []string, id string) {
	t.Ops = append(t.Ops, Op{Pointer: t.ptr(table, parent), Path: path, Command: "listRemove", Args: map[string]any{"id": id}})
}

// Schema emits an updateCollectionPropertySchema op (the only way the server
// accepts schema changes): path is below the collection, e.g. ["schema", id, "name"].
func (t *Tx) Schema(collID string, path []string, command string, args any) {
	t.Ops = append(t.Ops, Op{Pointer: t.ptr("collection", collID), Path: path, Command: "updateCollectionPropertySchema",
		Args: map[string]any{"primitiveOp": map[string]any{"command": command, "args": args}}})
}

// BlockProperty emits an updateBlockPropertyValue op (needed for relations).
func (t *Tx) BlockProperty(blockID, propID, command string, args any) {
	t.Ops = append(t.Ops, Op{Pointer: t.ptr("block", blockID), Path: []string{"properties", propID}, Command: "updateBlockPropertyValue",
		Args: map[string]any{"primitiveOp": map[string]any{"command": command, "args": args}}})
}

// DeleteProperty soft-deletes a property (moves it to deleted_schema).
func (t *Tx) DeleteProperty(collID, propID string, def any) {
	t.Ops = append(t.Ops, Op{Pointer: t.ptr("collection", collID), Path: []string{"deleted_schema", propID},
		Command: "softDeleteOrReinstateCollectionProperty", Args: map[string]any{"propertySchema": def}})
}

// Touch updates last-edited metadata of a record.
func (t *Tx) Touch(table, id string) {
	t.Update(table, id, nil, map[string]any{
		"last_edited_time": nowMs(), "last_edited_by_id": t.UserID, "last_edited_by_table": "notion_user",
	})
}

// NewBlockValue is the base record for a new block.
func (t *Tx) NewBlockValue(id, typ, parentID, parentTable string) map[string]any {
	now := nowMs()
	return map[string]any{
		"id": id, "type": typ, "version": 1, "alive": true,
		"parent_id": parentID, "parent_table": parentTable, "space_id": t.SpaceID,
		"created_time": now, "last_edited_time": now,
		"created_by_id": t.UserID, "created_by_table": "notion_user",
		"last_edited_by_id": t.UserID, "last_edited_by_table": "notion_user",
	}
}

// Submit sends the transaction (split into chunks if it is very large).
func (t *Tx) Submit(ctx context.Context, s *Store) error {
	const chunk = 400
	for i := 0; i < len(t.Ops); i += chunk {
		j := min(i+chunk, len(t.Ops))
		body := map[string]any{
			"requestId": NewID(),
			"transactions": []any{map[string]any{
				"id":         NewID(),
				"spaceId":    t.SpaceID,
				"debug":      map[string]any{"userAction": "notion-cli." + t.Action},
				"operations": t.Ops[i:j],
			}},
		}
		if err := s.C.Call(ctx, "saveTransactionsFanout", body, nil); err != nil {
			return err
		}
	}
	// Invalidate cached records touched by the transaction.
	for _, op := range t.Ops {
		if tbl := s.Records[op.Pointer.Table]; tbl != nil {
			delete(tbl, op.Pointer.ID)
		}
	}
	return nil
}

// Dump writes the operations as JSON (for --dry-run).
func (t *Tx) Dump(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(map[string]any{"spaceId": t.SpaceID, "operations": t.Ops})
}
