package cli

import (
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
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
	c.AddCommand(newMonSchemaListCmd(app), newMonSchemaShowCmd(app), newMonSchemaHostCmd(app))
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
