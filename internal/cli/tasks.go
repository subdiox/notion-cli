package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/subdiox/notion-cli/internal/notion"
)

// runTask enqueues an async Notion task and waits for it to finish.
func (e *env) runTask(eventName string, request map[string]any, timeout time.Duration) (map[string]any, error) {
	var enq struct {
		TaskID string `json:"taskId"`
	}
	if err := e.client.Call(e.ctx, "enqueueTask", map[string]any{"task": map[string]any{"eventName": eventName, "request": request}}, &enq); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		var resp struct {
			Results []map[string]any `json:"results"`
		}
		if err := e.client.Call(e.ctx, "getTasks", map[string]any{"taskIds": []string{enq.TaskID}}, &resp); err != nil {
			return nil, err
		}
		if len(resp.Results) > 0 {
			r := resp.Results[0]
			switch r["state"] {
			case "success":
				return r, nil
			case "failure":
				b, _ := json.Marshal(r["error"])
				return nil, fmt.Errorf("task %s failed: %s", eventName, b)
			}
			if gf.debug {
				b, _ := json.Marshal(r["status"])
				fmt.Fprintf(os.Stderr, "task %s: %v %s\n", eventName, r["state"], b)
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("task %s (%s) timed out", eventName, enq.TaskID)
		}
		time.Sleep(time.Second)
	}
}

func newDuplicateCmd() *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "duplicate <page>",
		Short: "Duplicate a page (with all content) next to the original or into --to",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			src, err := e.target(args[0], "block")
			if err != nil {
				return err
			}
			parentID, parentTable := src.Str("parent_id"), src.Str("parent_table")
			after := src.ID
			if to != "" {
				dest, err := e.target(to, "block")
				if err != nil {
					return err
				}
				parentID, parentTable, after = dest.ID, "block", ""
				if t := dest.Type(); t == "collection_view_page" || t == "collection_view" {
					coll, _, err := e.store().CollectionFor(e.ctx, dest)
					if err != nil {
						return err
					}
					parentID, parentTable = coll.ID, "collection"
				}
			}
			w := e.newWriter(src.SpaceID, "duplicate")
			id := notion.NewID()
			v := w.Tx.NewBlockValue(id, src.Type(), parentID, parentTable)
			v["copied_from"] = src.ID
			w.Tx.Set("block", id, nil, v)
			switch parentTable {
			case "block":
				w.Tx.ListAfter("block", parentID, []string{"content"}, id, after)
			case "space":
				ws, err := e.workspaces()
				if err != nil {
					return err
				}
				if sv := ws.SpaceView(e.client.UserID, src.SpaceID); sv != nil {
					w.Tx.ListAfter("space_view", sv.ID, []string{"private_pages"}, id, src.ID)
				}
			}
			if err := e.commit(w, false); err != nil {
				return err
			}
			_, err = e.runTask("duplicateBlock", map[string]any{
				"sourceBlock": map[string]any{"id": src.ID, "spaceId": src.SpaceID},
				"targetBlock": map[string]any{"id": id, "spaceId": src.SpaceID},
				"addCopyName": to == "",
			}, 10*time.Minute)
			if err != nil {
				return err
			}
			return printWriteResult("duplicated to", id, nil)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "destination page or database (default: next to the original)")
	return cmd
}

func newExportCmd() *cobra.Command {
	var (
		format, out string
		recursive   bool
		noFiles     bool
	)
	cmd := &cobra.Command{
		Use:   "export <page-or-database>",
		Short: "Export a page/database with Notion's exporter (markdown, html or pdf zip)",
		Long: `Run Notion's server-side export and download the zip. This is the way to get
computed values such as formulas and rollups as CSV for databases.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			blk, err := e.target(args[0], "block")
			if err != nil {
				return err
			}
			contents := "everything"
			if noFiles {
				contents = "no_files"
			}
			res, err := e.runTask("exportBlock", map[string]any{
				"block":     map[string]any{"id": blk.ID, "spaceId": blk.SpaceID},
				"recursive": recursive,
				"exportOptions": map[string]any{
					"exportType": format, "timeZone": localTZName(), "locale": "en",
					"includeContents": contents, "flattenExportFiletree": false,
					"collectionViewExportType": "all",
				},
			}, 30*time.Minute)
			if err != nil {
				return err
			}
			status, _ := res["status"].(map[string]any)
			url, _ := status["exportURL"].(string)
			if url == "" {
				b, _ := json.Marshal(res)
				return fmt.Errorf("export finished without a URL: %s", b)
			}
			if out == "" {
				out = notion.CompactID(blk.ID) + "-" + format + ".zip"
			}
			f, err := os.Create(out)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := e.client.Download(e.ctx, url, f); err != nil {
				return err
			}
			if gf.json {
				return printJSON(map[string]any{"ok": true, "path": out})
			}
			fmt.Println("exported to", out)
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "markdown", "markdown | html | pdf")
	cmd.Flags().StringVarP(&out, "output", "o", "", "output zip path")
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "include sub-pages")
	cmd.Flags().BoolVar(&noFiles, "no-files", false, "exclude images and attachments")
	return cmd
}

func localTZName() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	if p, err := os.Readlink("/etc/localtime"); err == nil {
		const marker = "zoneinfo/"
		for i := 0; i+len(marker) <= len(p); i++ {
			if p[i:i+len(marker)] == marker {
				return p[i+len(marker):]
			}
		}
	}
	return "UTC"
}
