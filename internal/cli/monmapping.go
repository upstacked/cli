package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/output"
)

const mappingsPath = "/api/monitoring/monitoring_item_schema_mapping/"

// newMonItemMappingCmd manages the schema mapping half of a monitoring item.
//
// The item says how to reach the data; the mapping says what the data means.
// Both are needed, and an item with the first and not the second fetches
// happily and publishes nothing - the failure this whole command family
// exists to make visible.
func newMonItemMappingCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "mapping",
		Aliases: []string{"map"},
		Short:   "Map a monitoring item's response onto data schema fields",
		Long: `A schema mapping turns a device's response into named schema fields.

The monitoring item says how to reach the data. The mapping says what the
data means: which JSON path fills which schema key, and - for a response with
many rows - which path tells the rows apart.

  ups monitoring item mapping list --item <id>
  ups monitoring item mapping create --item <id> --schema <id> --field in_octets=$.ifHCInOctets
  ups monitoring item mapping show <mapping-id>

Paths are evaluated against the response after 'response_root_path' has been
applied, so write them relative to that root rather than to the whole body.

Creating or changing a mapping invalidates whatever dry run had confirmed the
item, so this dry-runs the item afterwards. Read the result: a mapping whose
paths do not resolve leaves the item collecting nothing, and nothing pages
anyone about that.`,
	}
	c.AddCommand(
		newMonMappingListCmd(app), newMonMappingShowCmd(app),
		newMonMappingCreateCmd(app), newMonMappingUpdateCmd(app),
		newMonMappingDeleteCmd(app),
	)
	return c
}

func newMonMappingListCmd(app *App) *cobra.Command {
	var item, schema string
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List an item's schema mappings",
		RunE: func(cmd *cobra.Command, args []string) error {
			if item == "" && schema == "" {
				return errs.Usage("--item or --schema is required").
					WithHint("mappings are only meaningful per item: ups monitoring item mapping list --item <id>")
			}
			q := url.Values{}
			if item != "" {
				q.Set("monitoring_item", item)
			}
			if schema != "" {
				q.Set("schema", schema)
			}
			return app.runList(listOpts{
				Path:    mappingsPath,
				Query:   q,
				Columns: []string{"ID", "SCHEMA", "FIELDS", "MULTI", "IDENTIFIER"},
				Empty:   "No schema mappings. This item publishes into nothing.",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "schema_name", "schema")),
						fmt.Sprintf("%d", len(mappingFields(m))),
						yesNo(m["is_multi_valued"]),
						dash(truncate(str(m, "identifier"), 30)),
					}
				},
			})
		},
	}
	c.Flags().StringVar(&item, "item", "", "monitoring item id")
	c.Flags().StringVar(&schema, "schema", "", "data schema id")
	return c
}

func newMonMappingShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <mapping-id>",
		Short: "Show one schema mapping and its field paths",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, raw, err := app.getOne(mappingsPath+args[0]+"/", nil)
			if err != nil {
				return err
			}
			if app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			if err := app.Printer.Object(raw, [][2]string{
				{"ID", str(m, "id")},
				{"Monitoring item", dash(str(m, "monitoring_item"))},
				{"Schema", dash(str(m, "schema_name", "schema"))},
				{"Multi-valued", yesNo(m["is_multi_valued"])},
				{"Identifier", dash(str(m, "identifier"))},
			}); err != nil {
				return err
			}
			t := &output.Table{
				Columns: []string{"KEY", "PATH", "FILTERS"},
				Empty:   "This mapping fills no fields.",
			}
			for _, f := range mappingFields(m) {
				rawField, _ := json.Marshal(f)
				rules, _ := f["filter_rules"].([]any)
				t.Add(str(f, "key"), rawField,
					dash(str(f, "key")), dash(str(f, "path")),
					fmt.Sprintf("%d", len(rules)))
			}
			return app.Printer.Print(t)
		},
	}
}

func newMonMappingCreateCmd(app *App) *cobra.Command {
	var item, schema, identifier, fromFile string
	var fields, valueMaps []string
	var multi, skipTest bool

	c := &cobra.Command{
		Use:   "create",
		Short: "Map a monitoring item's response onto a data schema",
		Long: `Create a schema mapping for a monitoring item.

--field takes key=path, repeatably: the schema key on the left, the JSON path
into the response on the right. Paths are relative to the item's
'response_root_path'.

--identifier is the schema key whose value tells rows apart when the response
carries many of them - the interface index or name, the sensor id, the disk.
It names one of the keys given to --field, not a path: the engine reads the
identifier from the mapped row. A --multi-valued mapping without one collapses
every row onto a single series, which looks like working monitoring and is not.

--value-mapping key=<id|name> shows a field through a value mapping ("1" as
UP in green). Reuse one from 'ups monitoring value-mapping list' before
creating another.

--from-file takes the whole request body as JSON, for the parts flags do not
express: per-field 'filter_rules' and 'alert_rule_config'. Flags override
what the file sets.

The item is dry-run afterwards, because a mapping is exactly the kind of
config that is structurally valid and collects nothing.`,
		Example: `  ups monitoring item mapping create --item 412 --schema 7 \
    --field if_name=$.ifName --field in_octets=$.ifHCInOctets \
    --identifier if_name --multi-valued
  ups monitoring item mapping create --item 412 --schema 7 --from-file mapping.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if item == "" {
				return errs.Usage("--item is required")
			}
			body, err := mappingBody(fromFile)
			if err != nil {
				return err
			}
			body["monitoring_item"] = atoiOr(item)
			if schema != "" {
				body["schema"] = atoiOr(schema)
			}
			if _, ok := body["schema"]; !ok {
				return errs.Usage("--schema is required").
					WithHint("list them: ups monitoring schema list")
			}

			existing := fieldsFromBody(body)
			merged, err := mergeFields(existing, fields, nil, false)
			if err != nil {
				return err
			}
			if len(merged) == 0 {
				return errs.Usage("a mapping with no fields publishes nothing").
					WithHint("add at least one: --field <schema-key>=<json-path>   (keys: ups monitoring schema show %s)", str(row(body), "schema"))
			}
			if err := app.attachValueMappings(merged, valueMaps); err != nil {
				return err
			}
			body["field_mappings"] = merged
			if identifier != "" {
				if err := checkIdentifier(identifier, fieldsFromBody(body)); err != nil {
					return err
				}
				if err := app.checkSchemaIdentifier(str(row(body), "schema"), identifier); err != nil {
					return err
				}
				body["identifier"] = identifier
			}
			if cmd.Flags().Changed("multi-valued") {
				body["is_multi_valued"] = multi
			}
			// The API declares both required and rejects a body without them,
			// so they are defaulted rather than left to the caller.
			if _, ok := body["alert_rule_config"]; !ok {
				body["alert_rule_config"] = []any{}
			}
			if _, ok := body["selected_json_path"]; !ok {
				body["selected_json_path"] = map[string]any{}
			}
			if cols := rowColumns(isMulti(body), body["selected_json_path"],
				fieldsFromBody(body)); cols != nil {
				body["selected_json_path"] = cols
			}

			if isMulti(body) && str(row(body), "identifier") == "" {
				fmt.Fprintf(app.Stderr, "  %s a multi-valued mapping with no --identifier collapses every row onto one series.\n",
					app.Theme().Yellow.Apply(app.Sym().Warn))
			}

			var raw jsonRaw
			if err := app.create(mappingsPath, body, &raw); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			var m row
			_ = jsonUnmarshal(raw, &m)
			app.Printer.Infof("%s Created schema mapping %s on item %s",
				app.Sym().OK, str(m, "id"), item)
			app.verifyMappedItem(item, skipTest)
			return nil
		},
	}
	c.Flags().StringVar(&item, "item", "", "monitoring item id (required)")
	c.Flags().StringVar(&schema, "schema", "", "data schema id (required)")
	c.Flags().StringArrayVar(&fields, "field", nil, "schema key and JSON path, as key=path (repeatable)")
	c.Flags().StringVar(&identifier, "identifier", "", "the --field key whose value tells rows apart in a multi-valued response")
	c.Flags().StringArrayVar(&valueMaps, "value-mapping", nil, "show a field through a value mapping, as key=<id|name> (repeatable)")
	c.Flags().BoolVar(&multi, "multi-valued", false, "the response carries many rows, not one")
	c.Flags().StringVar(&fromFile, "from-file", "", "JSON request body; flags override it")
	c.Flags().BoolVar(&skipTest, "skip-test", false, "do not dry-run the item afterwards")
	return c
}

func newMonMappingUpdateCmd(app *App) *cobra.Command {
	var schema, identifier, fromFile string
	var fields, removeFields, valueMaps []string
	var multi, replace, skipTest bool

	c := &cobra.Command{
		Use:   "update <mapping-id>",
		Short: "Change a schema mapping's fields or identifier",
		Long: `Change an existing schema mapping.

--field merges by key: a key already mapped gets the new path, keys not named
are left alone. That is deliberate - sending the field set wholesale to
change one path is how the other fields silently stop being collected.

--remove-field drops a key, and --replace-fields makes --field the complete
set. Both remove coverage, so both confirm first.

The item is dry-run afterwards: a change to a mapping invalidates whatever
dry run had confirmed the item, and the platform will not tell you the new
paths resolve to nothing.`,
		Example: `  ups monitoring item mapping update 88 --field in_octets=$.ifHCInOctets
  ups monitoring item mapping update 88 --identifier if_name --multi-valued
  ups monitoring item mapping update 88 --remove-field errors`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			current, _, err := app.getOne(mappingsPath+args[0]+"/", nil)
			if err != nil {
				return err
			}
			item := str(current, "monitoring_item")

			body, err := mappingBody(fromFile)
			if err != nil {
				return err
			}
			if schema != "" {
				body["schema"] = atoiOr(schema)
			}
			if identifier != "" {
				body["identifier"] = identifier
			}
			if cmd.Flags().Changed("multi-valued") {
				body["is_multi_valued"] = multi
			}

			if len(fields) > 0 || len(removeFields) > 0 || replace {
				base := mappingFields(current)
				if fromFile != "" {
					base = fieldsFromBody(body)
				}
				merged, err := mergeFields(base, fields, removeFields, replace)
				if err != nil {
					return err
				}
				if lost := droppedKeys(base, merged); len(lost) > 0 {
					if err := app.Confirm(fmt.Sprintf(
						"Stop collecting %s on mapping %s? Nothing alerts when a field disappears.",
						strings.Join(lost, ", "), args[0])); err != nil {
						return err
					}
				}
				if len(merged) == 0 {
					return errs.Usage("that would leave the mapping with no fields, so the item would publish nothing").
						WithHint("delete it outright if that is the intent: ups monitoring item mapping delete %s", args[0])
				}
				body["field_mappings"] = merged
			}
			if len(valueMaps) > 0 {
				base, _ := body["field_mappings"].([]any)
				if base == nil {
					base, _ = current["field_mappings"].([]any)
				}
				if err := app.attachValueMappings(base, valueMaps); err != nil {
					return err
				}
				body["field_mappings"] = base
			}

			// The mapping as it will be after this PATCH, to derive its row
			// columns from.
			after := row{}
			for k, v := range current {
				after[k] = v
			}
			for k, v := range body {
				after[k] = v
			}
			if identifier != "" {
				if err := checkIdentifier(identifier, mappingFields(after)); err != nil {
					return err
				}
				if err := app.checkSchemaIdentifier(str(after, "schema"), identifier); err != nil {
					return err
				}
			}
			if cols := rowColumns(isMulti(after), after["selected_json_path"],
				mappingFields(after)); cols != nil {
				body["selected_json_path"] = cols
			}

			if len(body) == 0 {
				return errs.Usage("nothing to change").
					WithHint("pass --field, --remove-field, --value-mapping, --identifier, --multi-valued or --schema")
			}
			// Older servers refuse a PATCH without a schema ("Schema must be
			// provided") or without fields, even when neither is changing, so
			// resend the current ones.
			if _, ok := body["schema"]; !ok {
				body["schema"] = current["schema"]
			}
			if _, ok := body["field_mappings"]; !ok {
				body["field_mappings"] = current["field_mappings"]
			}

			if err := app.mutate("PATCH", mappingsPath+args[0]+"/", body, nil); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			app.Printer.Infof("%s Updated schema mapping %s", app.Sym().OK, args[0])
			app.verifyMappedItem(item, skipTest)
			return nil
		},
	}
	c.Flags().StringArrayVar(&fields, "field", nil, "schema key and JSON path, as key=path (repeatable, merged by key)")
	c.Flags().StringArrayVar(&removeFields, "remove-field", nil, "stop filling this schema key (repeatable)")
	c.Flags().BoolVar(&replace, "replace-fields", false, "make --field the complete set, dropping any key not named")
	c.Flags().StringVar(&identifier, "identifier", "", "the field key whose value tells rows apart")
	c.Flags().StringArrayVar(&valueMaps, "value-mapping", nil, "show a field through a value mapping, as key=<id|name>, or key=none to stop (repeatable)")
	c.Flags().BoolVar(&multi, "multi-valued", false, "the response carries many rows, not one")
	c.Flags().StringVar(&schema, "schema", "", "move the mapping to another data schema")
	c.Flags().StringVar(&fromFile, "from-file", "", "JSON request body; flags override it")
	c.Flags().BoolVar(&skipTest, "skip-test", false, "do not dry-run the item afterwards")
	return c
}

func newMonMappingDeleteCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <mapping-id>",
		Short: "Delete a schema mapping",
		Long: `Delete a schema mapping.

The item survives and keeps fetching. It just stops publishing into this
schema, which means the graphs and alert rules that read it go quiet without
anything reporting a fault.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, _, err := app.getOne(mappingsPath+args[0]+"/", nil)
			if err != nil {
				return err
			}
			keys := make([]string, 0, len(mappingFields(m)))
			for _, f := range mappingFields(m) {
				keys = append(keys, str(f, "key"))
			}
			sort.Strings(keys)
			if err := app.Confirm(fmt.Sprintf(
				"Delete mapping %s (schema %s, fields: %s) on item %s? Those fields stop being collected, silently.",
				args[0], dash(str(m, "schema_name", "schema")),
				dash(strings.Join(keys, ", ")), dash(str(m, "monitoring_item")))); err != nil {
				return err
			}
			if err := app.mutate("DELETE", mappingsPath+args[0]+"/", nil, nil); err != nil {
				return err
			}
			if !app.DryRun {
				app.Printer.Infof("Deleted schema mapping %s.", args[0])
			}
			return nil
		},
	}
}

// verifyMappedItem re-checks the item a mapping belongs to.
//
// Nothing here fails the command: the mapping was written either way, and a
// caller told only "updated" would assume the paths resolve.
func (a *App) verifyMappedItem(item string, skip bool) {
	t := a.Theme()
	if item == "" {
		return
	}
	if skip {
		fmt.Fprintf(a.Stderr, "  %s nobody has confirmed the paths resolve: ups monitoring item dry-run %s\n",
			t.Yellow.Apply(a.Sym().Warn), item)
		return
	}
	a.verifyCreatedItem(item)
}

// attachValueMappings sets --value-mapping key=<id|name> on the matching
// fields, in place. "none" detaches one.
func (a *App) attachValueMappings(fields []any, specs []string) error {
	for _, spec := range specs {
		key, ref, ok := strings.Cut(spec, "=")
		key, ref = strings.TrimSpace(key), strings.TrimSpace(ref)
		if !ok || key == "" || ref == "" {
			return errs.Usage("--value-mapping takes key=<id|name>, got %q", spec).
				WithHint("for example: --value-mapping oper_status=ifOperStatus")
		}
		var target map[string]any
		var keys []string
		for _, v := range fields {
			if f, ok := v.(map[string]any); ok {
				if str(row(f), "key") == key {
					target = f
				}
				keys = append(keys, str(row(f), "key"))
			}
		}
		if target == nil {
			return errs.Usage("--value-mapping %q: the mapping has no field %q", spec, key).
				WithHint("one of: %s", strings.Join(keys, ", "))
		}
		if strings.EqualFold(ref, "none") {
			target["value_mapping"] = nil
			continue
		}
		id, err := a.resolveValueMapping(ref)
		if err != nil {
			return err
		}
		target["value_mapping"] = atoiOr(id)
	}
	return nil
}

// mappingBody reads the optional --from-file body.
func mappingBody(path string) (map[string]any, error) {
	if path == "" {
		return map[string]any{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errs.Usage("cannot read %s: %v", path, err)
	}
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		return nil, errs.Usage("%s is not a JSON object: %v", path, err)
	}
	return body, nil
}

func mappingFields(m row) []row {
	raw, _ := m["field_mappings"].([]any)
	out := make([]row, 0, len(raw))
	for _, v := range raw {
		if f, ok := v.(map[string]any); ok {
			out = append(out, row(f))
		}
	}
	return out
}

func fieldsFromBody(body map[string]any) []row {
	return mappingFields(row(body))
}

// mergeFields applies --field / --remove-field to an existing field set.
func mergeFields(base []row, add, remove []string, replace bool) ([]any, error) {
	byKey := map[string]row{}
	order := []string{}
	if !replace {
		for _, f := range base {
			k := str(f, "key")
			if k == "" {
				continue
			}
			if _, seen := byKey[k]; !seen {
				order = append(order, k)
			}
			byKey[k] = f
		}
	}
	for _, spec := range add {
		key, path, ok := strings.Cut(spec, "=")
		key, path = strings.TrimSpace(key), strings.TrimSpace(path)
		if !ok || key == "" || path == "" {
			return nil, errs.Usage("--field takes key=path, got %q", spec).
				WithHint("for example: --field in_octets=$.ifHCInOctets")
		}
		if existing, seen := byKey[key]; seen {
			// Keep filter_rules and value_mapping: replacing the whole entry to
			// change its path would drop them unasked.
			existing["path"] = path
			byKey[key] = existing
			continue
		}
		order = append(order, key)
		byKey[key] = row{"key": key, "path": path}
	}
	for _, k := range remove {
		k = strings.TrimSpace(k)
		if _, seen := byKey[k]; !seen {
			return nil, errs.Usage("mapping has no field %q to remove", k)
		}
		delete(byKey, k)
	}

	out := make([]any, 0, len(byKey))
	for _, k := range order {
		if f, ok := byKey[k]; ok {
			out = append(out, map[string]any(f))
		}
	}
	return out, nil
}

// droppedKeys names the fields a change stops collecting.
func droppedKeys(before []row, after []any) []string {
	kept := map[string]bool{}
	for _, v := range after {
		if f, ok := v.(map[string]any); ok {
			kept[str(row(f), "key")] = true
		}
	}
	var lost []string
	for _, f := range before {
		if k := str(f, "key"); k != "" && !kept[k] {
			lost = append(lost, k)
		}
	}
	sort.Strings(lost)
	return lost
}

var rowColumnRef = regexp.MustCompile(`item\[['"](\$[^'"]*)['"]\]`)

// rowColumns derives selected_json_path for a multi-valued mapping that has
// none, or returns nil. The engine joins rows across exactly those collections,
// so an empty one itemizes nothing: the mapping saves, dry-runs to zero rows,
// and publishes nothing. Every collection a field reads is one it needs.
func rowColumns(multi bool, existing any, fields []row) []any {
	if !multi || !emptyJSON(existing) {
		return nil
	}
	var cols []any
	seen := map[string]bool{}
	for _, expr := range fieldPaths(fields) {
		for _, m := range rowColumnRef.FindAllStringSubmatch(expr, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				cols = append(cols, m[1])
			}
		}
	}
	return cols
}

// checkSchemaIdentifier refuses an identifier that is not the schema's own.
//
// A schema names the field that tells its rows apart, and the portal offers
// only that one. Identifying rows by something else splits or merges a
// device's history against what every other item publishing into the schema
// did, which reads as working monitoring.
func (a *App) checkSchemaIdentifier(schemaID, identifier string) error {
	if schemaID == "" || identifier == "" {
		return nil
	}
	m, _, err := a.getOne(schemasPath+schemaID+"/", nil)
	if err != nil {
		// Not the caller's problem to solve: the key check already passed.
		return nil
	}
	want := identifierField(schemaFields(m))
	if want == "" || want == identifier {
		return nil
	}
	return errs.Usage("schema %s identifies rows by %q, not %q", schemaID, want, identifier).
		WithHint("map that key and use it: --field %s=<path> --identifier %s", want, want)
}

// checkIdentifier refuses an identifier that is not one of the mapping's keys.
// The engine looks the identifier up in the mapped row by key; a path or an
// unmapped key finds nothing, so every row's alerts share one identity.
func checkIdentifier(identifier string, fields []row) error {
	var keys []string
	for _, f := range fields {
		k := str(f, "key")
		if k == identifier {
			return nil
		}
		keys = append(keys, k)
	}
	return errs.Usage("--identifier %q is not one of this mapping's keys", identifier).
		WithHint("name the schema key whose value tells rows apart, not a path: one of %s", strings.Join(keys, ", "))
}

func fieldPaths(fields []row) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, str(f, "path"))
	}
	return out
}

func emptyJSON(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	return false
}

func isMulti(body map[string]any) bool {
	b, _ := body["is_multi_valued"].(bool)
	return b
}
