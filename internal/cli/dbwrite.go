package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/subdiox/notion-cli/internal/notion"
)

var propTypes = []string{"title", "text", "number", "select", "multi_select", "status", "date", "person", "file", "checkbox", "url", "email", "phone_number", "relation", "created_time", "created_by", "last_edited_time", "last_edited_by", "auto_increment_id"}

const propSpecHelp = `Property specs: "Name:type" or "Name:type=arg".
  types: ` + "text, number, select, multi_select, status, date, person, file, checkbox, url,\n  email, phone_number, relation, created_time, created_by, last_edited_time, last_edited_by,\n  auto_increment_id" + `
  select/multi_select/status: "Priority:select=High,Medium,Low"
  number: "Price:number=yen" (format: number, number_with_commas, percent, dollar, yen, euro, ...)
  relation: "Project:relation=<database-url>"
  auto_increment_id: "ID:auto_increment_id=TASK" (prefix)`

// propSchema builds a schema entry from "Name:type=arg".
func (e *env) propSchema(spec string) (string, map[string]any, error) {
	name, rest, ok := strings.Cut(spec, ":")
	if !ok {
		return "", nil, fmt.Errorf("property spec %q: expected Name:type", spec)
	}
	typ, arg, _ := strings.Cut(rest, "=")
	name, typ = strings.TrimSpace(name), strings.TrimSpace(strings.ToLower(typ))
	if typ == "rich_text" {
		typ = "text"
	}
	valid := false
	for _, t := range propTypes {
		if t == typ {
			valid = true
		}
	}
	if !valid {
		return "", nil, fmt.Errorf("unknown property type %q", typ)
	}
	def := map[string]any{"name": name, "type": typ}
	opts := func() []any {
		var out []any
		for i, v := range strings.Split(arg, ",") {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, map[string]any{"id": notion.NewID(), "value": v, "color": []string{"default", "gray", "brown", "orange", "yellow", "green", "blue", "purple", "pink", "red"}[(i+1)%10]})
			}
		}
		return out
	}
	switch typ {
	case "select", "multi_select":
		if arg != "" {
			def["options"] = opts()
		}
	case "status":
		if arg == "" {
			arg = "Not started,In progress,Done"
		}
		o := opts()
		def["options"] = o
		// Distribute options over the three status groups: first / middle / last.
		groups := []map[string]any{
			{"id": notion.NewID(), "name": "To-do", "color": "gray", "optionIds": []any{}},
			{"id": notion.NewID(), "name": "In progress", "color": "blue", "optionIds": []any{}},
			{"id": notion.NewID(), "name": "Complete", "color": "green", "optionIds": []any{}},
		}
		for i, x := range o {
			g := 1
			if i == 0 {
				g = 0
			} else if i == len(o)-1 {
				g = 2
			}
			groups[g]["optionIds"] = append(groups[g]["optionIds"].([]any), x.(map[string]any)["id"])
		}
		def["groups"] = []any{groups[0], groups[1], groups[2]}
	case "number":
		if arg != "" {
			def["number_format"] = arg
		}
	case "relation":
		if arg == "" {
			return "", nil, fmt.Errorf("relation %q needs a target database: Name:relation=<database-url>", name)
		}
		_, coll, _, err := e.dbTarget(arg)
		if err != nil {
			return "", nil, err
		}
		def["collection_id"] = coll.ID
		def["version"] = "v2"
		def["property"] = ""
	case "auto_increment_id":
		if arg != "" {
			def["prefix"] = arg
		}
	}
	return name, def, nil
}

func randomPropID(existing map[string]any) string {
	for {
		id := notion.NewID()[:4]
		if _, ok := existing[id]; !ok && id != "titl" {
			return id
		}
	}
}

func newDBWriteCmds() []*cobra.Command {
	// --- db create ---
	var (
		parent, title string
		props         []string
		inline, dry   bool
		viewType      string
	)
	create := &cobra.Command{
		Use:     "create",
		Short:   "Create a database (full page or --inline in a page)",
		Long:    "Create a new database.\n\n" + propSpecHelp,
		Example: `  notion db create --parent <page-url> -t Tasks -p "Status:status" -p "Due:date" -p "Tags:multi_select=bug,feature"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			var spaceID, parentID, parentTable string
			if parent == "" {
				if inline {
					return fmt.Errorf("--inline needs --parent")
				}
				sp, err := e.currentSpace()
				if err != nil {
					return err
				}
				spaceID, parentID, parentTable = sp.ID, sp.ID, "space"
			} else {
				pb, err := e.target(parent, "block")
				if err != nil {
					return err
				}
				spaceID, parentID, parentTable = pb.SpaceID, pb.ID, "block"
			}
			w := e.newWriter(spaceID, "createDatabase")
			blockID, collID, viewID := notion.NewID(), notion.NewID(), notion.NewID()
			titleName := "Name"
			schema := map[string]any{}
			var order []string
			for _, p := range props {
				name, def, err := e.propSchema(p)
				if err != nil {
					return err
				}
				if def["type"] == "title" {
					titleName = name
					continue
				}
				id := randomPropID(schema)
				schema[id] = def
				order = append(order, id)
			}
			schema["title"] = map[string]any{"name": titleName, "type": "title"}
			now := nowMs()
			w.Tx.Set("collection", collID, nil, map[string]any{
				"id": collID, "version": 1, "name": []any{[]any{title}}, "schema": schema,
				"parent_id": blockID, "parent_table": "block", "alive": true, "space_id": spaceID,
				"format": map[string]any{"collection_page_properties": []any{}},
			})
			tableProps := []any{map[string]any{"property": "title", "visible": true, "width": 280}}
			for _, id := range order {
				tableProps = append(tableProps, map[string]any{"property": id, "visible": true})
			}
			w.Tx.Set("collection_view", viewID, nil, map[string]any{
				"id": viewID, "version": 1, "type": viewType, "name": "Default view",
				"format":    map[string]any{viewType + "_properties": tableProps, "collection_pointer": map[string]any{"id": collID, "table": "collection", "spaceId": spaceID}},
				"parent_id": blockID, "parent_table": "block", "alive": true, "space_id": spaceID,
			})
			btype := "collection_view_page"
			if inline {
				btype = "collection_view"
			}
			v := w.Tx.NewBlockValue(blockID, btype, parentID, parentTable)
			v["collection_id"] = collID
			v["view_ids"] = []any{viewID}
			v["format"] = map[string]any{"collection_pointer": map[string]any{"id": collID, "table": "collection", "spaceId": spaceID}}
			v["created_time"], v["last_edited_time"] = now, now
			if parentTable == "space" {
				v["permissions"] = []any{map[string]any{"role": "editor", "type": "user_permission", "user_id": e.client.UserID}}
			}
			// The block must exist before the collection/view that point at it.
			w.Tx.Ops = append([]notion.Op{{Pointer: notion.Pointer{Table: "block", ID: blockID, SpaceID: spaceID}, Path: []string{}, Command: "set", Args: v}}, w.Tx.Ops...)
			switch parentTable {
			case "block":
				w.Tx.ListAfter("block", parentID, []string{"content"}, blockID, "")
			case "space":
				ws, err := e.workspaces()
				if err != nil {
					return err
				}
				if sv := ws.SpaceView(e.client.UserID, spaceID); sv != nil {
					w.Tx.ListAfter("space_view", sv.ID, []string{"private_pages"}, blockID, "")
				}
			}
			if err := e.commit(w, dry); err != nil {
				return err
			}
			if dry {
				return nil
			}
			return printWriteResult("created database", blockID, map[string]any{"collection_id": collID, "view_id": viewID})
		},
	}
	create.Flags().StringVar(&parent, "parent", "", "parent page (default: private page in the current workspace)")
	create.Flags().StringVarP(&title, "title", "t", "Untitled", "database title")
	create.Flags().StringArrayVarP(&props, "prop", "p", nil, "property spec (repeatable), e.g. Status:select=Todo,Done")
	create.Flags().BoolVar(&inline, "inline", false, "create an inline database inside --parent")
	create.Flags().StringVar(&viewType, "view", "table", "initial view type: table, board, list, gallery, calendar, timeline")
	create.Flags().BoolVar(&dry, "dry-run", false, "print the transaction instead of sending it")

	// --- db prop ---
	prop := &cobra.Command{Use: "prop", Aliases: []string{"property", "schema"}, Short: "Change database properties (schema)"}
	propAdd := &cobra.Command{
		Use:   "add <database> <spec>...",
		Short: "Add properties",
		Long:  "Add properties to a database.\n\n" + propSpecHelp,
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			_, coll, views, err := e.dbTarget(args[0])
			if err != nil {
				return err
			}
			schema, _ := coll.Value["schema"].(map[string]any)
			w := e.newWriter(coll.SpaceID, "addProperty")
			for _, spec := range args[1:] {
				_, def, err := e.propSchema(spec)
				if err != nil {
					return err
				}
				id := randomPropID(schema)
				schema[id] = def
				w.Tx.Schema(coll.ID, []string{"schema", id}, "update", def)
				for _, v := range views {
					key := v.Type() + "_properties"
					if _, ok := v.Format()[key]; ok {
						w.Tx.Ops = append(w.Tx.Ops, notion.Op{
							Pointer: notion.Pointer{Table: "collection_view", ID: v.ID, SpaceID: v.SpaceID},
							Path:    []string{"format", key}, Command: "keyedObjectListAfter",
							Args: map[string]any{"value": map[string]any{"property": id, "visible": true}},
						})
					}
				}
			}
			if err := e.commit(w, dry); err != nil {
				return err
			}
			if !dry {
				fmt.Println("added", len(args)-1, "properties")
			}
			return nil
		},
	}
	propAdd.Flags().BoolVar(&dry, "dry-run", false, "print the transaction instead of sending it")

	var newName string
	var addOpts, rmOpts []string
	propUpdate := &cobra.Command{
		Use:   "update <database> <property>",
		Short: "Rename a property or add/remove select options",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			_, coll, _, err := e.dbTarget(args[0])
			if err != nil {
				return err
			}
			d := notion.FindProp(notion.Schema(coll, nil), args[1])
			if d == nil {
				return fmt.Errorf("unknown property %q", args[1])
			}
			w := e.newWriter(coll.SpaceID, "updateProperty")
			if newName != "" {
				w.Tx.Schema(coll.ID, []string{"schema", d.ID, "name"}, "set", newName)
			}
			if len(addOpts) > 0 || len(rmOpts) > 0 {
				var opts []any
				rm := map[string]bool{}
				for _, r := range rmOpts {
					rm[r] = true
				}
				for _, o := range d.Options {
					if !rm[o.Value] {
						opts = append(opts, map[string]any{"id": o.ID, "value": o.Value, "color": o.Color})
					}
				}
				for i, a := range addOpts {
					opts = append(opts, map[string]any{"id": notion.NewID(), "value": a, "color": []string{"gray", "brown", "orange", "yellow", "green", "blue", "purple", "pink", "red"}[(len(d.Options)+i)%9]})
				}
				w.Tx.Schema(coll.ID, []string{"schema", d.ID, "options"}, "set", opts)
			}
			if err := e.commit(w, dry); err != nil {
				return err
			}
			if !dry {
				fmt.Println("updated property", d.Name)
			}
			return nil
		},
	}
	propUpdate.Flags().StringVar(&newName, "rename", "", "new property name")
	propUpdate.Flags().StringArrayVar(&addOpts, "add-option", nil, "add a select option (repeatable)")
	propUpdate.Flags().StringArrayVar(&rmOpts, "remove-option", nil, "remove a select option (repeatable)")
	propUpdate.Flags().BoolVar(&dry, "dry-run", false, "print the transaction instead of sending it")

	propRm := &cobra.Command{
		Use:   "rm <database> <property>...",
		Short: "Delete properties",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			_, coll, _, err := e.dbTarget(args[0])
			if err != nil {
				return err
			}
			defs := notion.Schema(coll, nil)
			w := e.newWriter(coll.SpaceID, "deleteProperty")
			for _, name := range args[1:] {
				d := notion.FindProp(defs, name)
				if d == nil {
					return fmt.Errorf("unknown property %q", name)
				}
				if d.Type == "title" {
					return fmt.Errorf("the title property cannot be deleted")
				}
				w.Tx.DeleteProperty(coll.ID, d.ID, d.Raw)
			}
			if err := e.commit(w, dry); err != nil {
				return err
			}
			if !dry {
				fmt.Println("deleted", len(args)-1, "properties")
			}
			return nil
		},
	}
	propRm.Flags().BoolVar(&dry, "dry-run", false, "print the transaction instead of sending it")
	prop.AddCommand(propAdd, propUpdate, propRm)

	// --- db rename/description ---
	var desc string
	update := &cobra.Command{
		Use:   "update <database>",
		Short: "Rename a database or change its description/icon",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			_, coll, _, err := e.dbTarget(args[0])
			if err != nil {
				return err
			}
			w := e.newWriter(coll.SpaceID, "updateDatabase")
			if title != "" && cmd.Flags().Changed("title") {
				w.Tx.Set("collection", coll.ID, []string{"name"}, notion.ParseInline(title))
			}
			if cmd.Flags().Changed("description") {
				w.Tx.Set("collection", coll.ID, []string{"description"}, notion.ParseInline(desc))
			}
			if icon, _ := cmd.Flags().GetString("icon"); icon != "" {
				w.Tx.Set("collection", coll.ID, []string{"icon"}, icon)
			}
			if err := e.commit(w, dry); err != nil {
				return err
			}
			if !dry {
				fmt.Println("updated database", notion.CollectionName(coll))
			}
			return nil
		},
	}
	update.Flags().StringVarP(&title, "title", "t", "", "new title")
	update.Flags().StringVar(&desc, "description", "", "new description (Markdown)")
	update.Flags().String("icon", "", "icon (emoji or URL)")
	update.Flags().BoolVar(&dry, "dry-run", false, "print the transaction instead of sending it")

	// --- row add / update: thin wrappers over page create/update ---
	row := &cobra.Command{Use: "row", Aliases: []string{"rows"}, Short: "Add or update database rows (pages)"}
	rowAdd := newPageCreateCmd()
	rowAdd.Use = "add <database>"
	rowAdd.Short = "Add a row: notion db row add <db> -p Name=... -p Status=Done [-f body.md]"
	rowAdd.Args = cobra.ExactArgs(1)
	innerAdd := rowAdd.RunE
	rowAdd.RunE = func(cmd *cobra.Command, args []string) error {
		_ = cmd.Flags().Set("parent", args[0])
		return innerAdd(cmd, nil)
	}
	rowUpdate := newPageUpdateCmd()
	rowUpdate.Use = "update <row-page>"
	rowUpdate.Short = "Update a row: notion db row update <row> -p Status=Done"
	row.AddCommand(rowAdd, rowUpdate)

	return []*cobra.Command{create, update, prop, row}
}
