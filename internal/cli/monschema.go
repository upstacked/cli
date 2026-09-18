package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/output"
)

const schemasPath = "/api/monitoring/defined_data_schema/"

// newMonSchemaCmd exposes the data schemas a monitoring item publishes into.
//
// A schema is the contract between a check and everything downstream: the
// graphs, the alert rules and the host's metric pages all read named fields,
// not whatever the device happened to return. An item that collects real data
// into no schema, or into the wrong fields, is invisible to all of them - and
// per the coverage rule, nothing says so.
func newMonSchemaCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "schema",
		Aliases: []string{"schemas"},
		Short:   "Data schemas a monitoring item can publish into",
		Long: `A data schema names the fields a check is expected to produce.

Graphs, alert rules and the host metric pages read schema fields by name, so
a check that maps into no schema collects data nobody can see. Deciding which
schema to populate comes before writing any JSON paths:

  ups monitoring schema list                 # the catalogue
  ups monitoring schema show <id>            # the fields, and which one identifies a row
  ups monitoring schema host <host-id>       # what a device already publishes

The last one is the shortcut worth knowing. A device of the same type that is
already monitored has the answer for this one, and copying a working mapping
beats deriving one.`,
	}
	c.AddCommand(newMonSchemaListCmd(app), newMonSchemaShowCmd(app), newMonSchemaHostCmd(app),
		newMonSchemaCreateCmd(app), newMonSchemaAddKeyCmd(app))
	return c
}

func newMonSchemaListCmd(app *App) *cobra.Command {
	var search string
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the defined data schemas",
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if search != "" {
				q.Set("search", search)
			}
			return app.runList(listOpts{
				Path:    schemasPath,
				Query:   q,
				Columns: []string{"ID", "NAME", "FIELDS", "IDENTIFIER", "ORG"},
				Empty:   "No data schemas defined.",
				Cells: func(m row) []string {
					fields := schemaFields(m)
					return []string{
						str(m, "id"), dash(str(m, "name")),
						fmt.Sprintf("%d", len(fields)),
						dash(identifierField(fields)),
						dash(str(m, "organization")),
					}
				},
			})
		},
	}
	c.Flags().StringVar(&search, "search", "", "filter by name")
	return c
}

func newMonSchemaShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show a data schema's fields",
		Long: `Show the fields a schema expects.

Every key listed here is a key a monitoring item's schema mapping can fill.
The field marked as the identifier is the one that tells rows apart - the
interface, the sensor, the disk - and a multi-valued check that does not set
it collapses every row onto one.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, raw, err := app.getOne(schemasPath+args[0]+"/", nil)
			if err != nil {
				return err
			}
			if app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			if err := app.Printer.Object(raw, [][2]string{
				{"ID", str(m, "id")},
				{"Name", dash(str(m, "name"))},
				{"Organization", dash(str(m, "organization"))},
			}); err != nil {
				return err
			}
			t := &output.Table{
				Columns: []string{"KEY", "TYPE", "DISPLAY", "IDENTIFIER"},
				Empty:   "This schema defines no fields.",
			}
			for _, f := range schemaFields(m) {
				rawField, _ := json.Marshal(f)
				t.Add(str(f, "id"), rawField,
					dash(str(f, "key")), dash(str(f, "of_type", "type")),
					dash(str(f, "display_name")), yesNo(f["is_identifier"]))
			}
			return app.Printer.Print(t)
		},
	}
}

// newMonSchemaHostCmd reports the schemas a host already publishes into.
func newMonSchemaHostCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "host <host-id>",
		Short: "Show the data schemas a host already publishes into",
		Long: `Show which schemas a host's existing monitoring populates.

This is the fastest way to answer "what does a device of this type need":
find one that is already monitored properly and read it off, rather than
deciding from scratch and discovering the gap during an incident.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runList(listOpts{
				Path:    "/api/monitoring-metrics/host/" + args[0] + "/data-schema/",
				Columns: []string{"ID", "NAME", "TYPE", "IDENTIFIER", "FIELDS"},
				Empty:   "This host publishes into no data schemas.",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "name")), dash(str(m, "type")),
						dash(str(m, "identifier_display_name", "identifier")),
						fmt.Sprintf("%d", len(schemaFields(m))),
					}
				},
			})
		},
	}
}

func schemaFields(m row) []row {
	raw, _ := m["fields"].([]any)
	out := make([]row, 0, len(raw))
	for _, v := range raw {
		if f, ok := v.(map[string]any); ok {
			out = append(out, row(f))
		}
	}
	return out
}

func identifierField(fields []row) string {
	for _, f := range fields {
		if b, ok := f["is_identifier"].(bool); ok && b {
			return str(f, "key")
		}
	}
	return ""
}

func yesNo(v any) string {
	if b, ok := v.(bool); ok && b {
		return "yes"
	}
	return "-"
}

// schemaFieldTypes are the value types a schema key can declare.
var schemaFieldTypes = map[string]bool{
	"STRING": true, "INTEGER": true, "FLOAT": true, "BOOLEAN": true,
}

func newMonSchemaCreateCmd(app *App) *cobra.Command {
	var name, org, identifier string
	var fields []string

	c := &cobra.Command{
		Use:   "create",
		Short: "Define a new data schema",
		Long: `Create a data schema: the named fields a class of check publishes into.

--field takes key:TYPE, repeatably, where TYPE is STRING, INTEGER, FLOAT or
BOOLEAN. --identifier names the key that tells rows apart, and every schema a
multi-valued check will map into needs one.

Prefer an existing schema. A schema is shared by every check that maps into it
and read by name from graphs and alert rules, so a near-duplicate splits a
device's history across two names that nothing joins back up. Check first:

  ups monitoring schema list`,
		Example: `  ups monitoring schema create --name interface \
    --field if_name:STRING --field in_octets:INTEGER --field out_octets:INTEGER \
    --identifier if_name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return errs.Usage("--name is required")
			}
			parsed, err := parseSchemaFields(fields, identifier)
			if err != nil {
				return err
			}
			if len(parsed) == 0 {
				return errs.Usage("a schema with no fields cannot be mapped into").
					WithHint("add at least one: --field <key>:STRING")
			}
			orgID, err := app.resolveOrganization(org)
			if err != nil {
				return err
			}

			body := map[string]any{
				"name": name, "organization": atoiOr(orgID), "fields": parsed,
			}
			var raw jsonRaw
			if err := app.mutate("POST", schemasPath, body, &raw); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			var m row
			_ = jsonUnmarshal(raw, &m)
			id := str(m, "id")
			t := app.Theme()
			fmt.Fprintf(app.Stderr, "%s Created data schema %s (%s)\n",
				t.Green.Apply(app.Sym().OK), name, id)
			if identifier == "" {
				fmt.Fprintf(app.Stderr, "  %s no identifier field, so a multi-valued check mapping into this schema collapses every row onto one series.\n",
					t.Yellow.Apply(app.Sym().Warn))
			}
			fmt.Fprintf(app.Stderr, "  %s ups monitoring item mapping create --item <id> --schema %s\n",
				t.Dim.Apply("next:"), id)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "schema name (required)")
	c.Flags().StringArrayVar(&fields, "field", nil, "schema key and type, as key:TYPE (repeatable)")
	c.Flags().StringVar(&identifier, "identifier", "", "the key that distinguishes rows")
	c.Flags().StringVar(&org, "org", "", "organization id (defaults to yours when you belong to exactly one)")
	return c
}

func newMonSchemaAddKeyCmd(app *App) *cobra.Command {
	var fields []string
	var identifier string

	c := &cobra.Command{
		Use:   "add-key <schema-id>",
		Short: "Add fields to an existing data schema",
		Long: `Add keys to a data schema.

Adding is safe: existing mappings keep filling the keys they already fill, and
the new ones stay empty until something maps into them. Removing is not, which
is why there is no remove here - the API's delete endpoint takes no argument
saying which key to drop, so this CLI will not guess at it. Edit the schema in
the web UI if a key really has to go.`,
		Example: `  ups monitoring schema add-key 7 --field errors:INTEGER`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			parsed, err := parseSchemaFields(fields, identifier)
			if err != nil {
				return err
			}
			if len(parsed) == 0 {
				return errs.Usage("nothing to add").
					WithHint("pass --field <key>:STRING")
			}
			m, _, err := app.getOne(schemasPath+args[0]+"/", nil)
			if err != nil {
				return err
			}
			body := map[string]any{
				"name": str(m, "name"), "organization": atoiOr(str(m, "organization")),
				"fields": parsed,
			}
			if err := app.mutate("POST", schemasPath+args[0]+"/create-data-schema-key/", body, nil); err != nil {
				return err
			}
			if !app.DryRun {
				app.Printer.Infof("%s Added %d key(s) to schema %s.", app.Sym().OK, len(parsed), args[0])
			}
			return nil
		},
	}
	c.Flags().StringArrayVar(&fields, "field", nil, "schema key and type, as key:TYPE (repeatable)")
	c.Flags().StringVar(&identifier, "identifier", "", "mark this key as the row identifier")
	return c
}

// parseSchemaFields turns key:TYPE specs into the API's field objects.
func parseSchemaFields(specs []string, identifier string) ([]any, error) {
	out := make([]any, 0, len(specs))
	seen := map[string]bool{}
	for _, spec := range specs {
		key, typ, hasType := strings.Cut(spec, ":")
		key = strings.TrimSpace(key)
		typ = strings.ToUpper(strings.TrimSpace(typ))
		if key == "" {
			return nil, errs.Usage("--field takes key:TYPE, got %q", spec)
		}
		if !hasType || typ == "" {
			typ = "STRING"
		}
		if !schemaFieldTypes[typ] {
			return nil, errs.Usage("%q is not a field type", typ).
				WithHint("one of: STRING, INTEGER, FLOAT, BOOLEAN")
		}
		if seen[key] {
			return nil, errs.Usage("duplicate field %q", key)
		}
		seen[key] = true
		out = append(out, map[string]any{
			"key": key, "of_type": typ, "display_name": key,
			"is_identifier": identifier != "" && key == identifier,
		})
	}
	if identifier != "" && !seen[identifier] {
		return nil, errs.Usage("--identifier %q is not one of the fields given", identifier).
			WithHint("add it: --field %s:STRING", identifier)
	}
	return out, nil
}
