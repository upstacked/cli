package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
)

func newMonitoringCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "monitoring",
		Aliases: []string{"mon"},
		Short:   "Manage monitoring items, modules and templates",
		Long: `Monitoring has three distinct nouns:

  module    what to check (the definition)
  item      an instance of a module bound to a host and credential
  event     a fired alert

A misconfigured item does not error. It returns nothing, or the wrong
field, so 'ups monitoring item test' is the only feedback loop that
distinguishes healthy from never-collected-anything.`,
	}
	c.AddCommand(newMonItemCmd(app), newMonModuleCmd(app), newMonTemplateCmd(app), newMonHostsCmd(app))
	return c
}

func newMonItemCmd(app *App) *cobra.Command {
	c := &cobra.Command{Use: "item", Short: "Monitoring items"}
	c.AddCommand(
		newMonItemListCmd(app), newMonItemShowCmd(app),
		newMonItemTestCmd(app), newMonItemCreateCmd(app),
		newMonItemDeleteCmd(app), newMonItemResultsCmd(app),
	)
	return c
}

func newMonItemListCmd(app *App) *cobra.Command {
	var host string
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List monitoring items",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Deliberately not /api/{infra}/monitoring_items/: that endpoint
			// returns `results` as an object keyed by protocol rather than the
			// array every other list endpoint returns. Confirmed against a
			// live server.
			q := app.infraQuery(nil)
			if host != "" {
				q.Set("host", host)
			}
			return app.runList(listOpts{
				Path:    "/api/monitoring/items/",
				Query:   q,
				Columns: []string{"ID", "NAME", "HOST", "MODULE", "INTERVAL"},
				Empty:   "No monitoring items found.",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "name")), dash(str(m, "host_name", "host")),
						dash(str(m, "monitoring_module_name", "monitoring_module")),
						dash(str(m, "interval")),
					}
				},
			})
		},
	}
	c.Flags().StringVar(&host, "host", "", "filter by host id")
	return c
}

func newMonItemShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one monitoring item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, raw, err := app.getOne("/api/monitoring/items/"+args[0]+"/", nil)
			if err != nil {
				return err
			}
			if app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			return app.Printer.Object(raw, [][2]string{
				{"ID", str(m, "id")},
				{"Name", dash(str(m, "name"))},
				{"Host", dash(str(m, "host_name", "host"))},
				{"Module", dash(str(m, "monitoring_module_name", "monitoring_module"))},
				{"Credential type", dash(str(m, "credential_type"))},
				{"Interval", dash(str(m, "interval"))},
				{"Parameters", dash(truncate(str(m, "parameters"), 120))},
				{"Description", dash(str(m, "description"))},
			})
		},
	}
}

func newMonItemTestCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "test <id>",
		Short: "Test a monitoring item and show the raw response",
		Long: `Run a monitoring item once and print what came back.

Do this before trusting a new or edited item. A misconfigured item does not
report an error: it silently collects nothing, or collects the wrong field,
and the gap only surfaces during an incident it failed to catch.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := app.Client()
			if err != nil {
				return err
			}
			ctx, cancel := app.Ctx()
			defer cancel()
			var raw jsonRaw
			err = app.Spin("Testing monitoring item "+args[0], func() error {
				return c.Do(ctx, request("GET", "/api/monitoring/item/"+args[0]+"/test", nil), &raw)
			})
			if err != nil {
				return err
			}
			return app.Printer.Object(raw, nil)
		},
	}
}

func newMonItemResultsCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "results <id>",
		Short: "Show the most recent result for a monitoring item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, raw, err := app.getOne("/api/monitoring/item/"+args[0]+"/results/last/", nil)
			if err != nil {
				return err
			}
			return app.Printer.Object(raw, nil)
		},
	}
}

func newMonItemCreateCmd(app *App) *cobra.Command {
	var host, name, module, params, credential, credType, description string
	var interval int
	var skipTest bool

	c := &cobra.Command{
		Use:   "create",
		Short: "Add a monitoring item to a host",
		Long: `Create a monitoring item.

Unless --skip-test is given, the item is created and then tested, so a
silently-broken check is caught immediately rather than during an incident.`,
		Example: `  ups monitoring item create --host 12 --name "CPU" --module 3
  ups monitoring item create --host 12 --name "API health" --module 7 --credential-type api`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if host == "" || name == "" {
				return errs.Usage("--host and --name are required")
			}
			body := map[string]any{"name": name, "host": atoiOr(host)}
			if module != "" {
				body["monitoring_module"] = atoiOr(module)
			}
			if interval > 0 {
				body["interval"] = interval
			}
			addIf(body, "parameters", params)
			addIf(body, "description", description)
			addIf(body, "credential_type", credType)
			if credential != "" {
				body["credential"] = atoiOr(credential)
			}

			var raw jsonRaw
			if err := app.mutate("POST", "/api/monitoring/items/", body, &raw); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			var m row
			_ = jsonUnmarshal(raw, &m)
			id := str(m, "id")
			t, sym := app.Theme(), app.Sym()
			fmt.Fprintf(app.Stderr, "%s Created monitoring item %s (%s)\n",
				t.Green.Apply(sym.OK), name, id)

			if skipTest || id == "" {
				fmt.Fprintf(app.Stderr, "  %s verify it collects data: ups monitoring item test %s\n",
					t.Yellow.Apply(sym.Warn), id)
				return nil
			}
			cl, err := app.Client()
			if err != nil {
				return err
			}
			ctx, cancel := app.Ctx()
			defer cancel()
			var testRaw jsonRaw
			terr := app.Spin("Testing the new item", func() error {
				return cl.Do(ctx, request("GET", "/api/monitoring/item/"+id+"/test", nil), &testRaw)
			})
			if terr != nil {
				fmt.Fprintf(app.Stderr, "%s The item was created but the test failed: %v\n",
					t.Yellow.Apply(sym.Warn), terr)
				fmt.Fprintf(app.Stderr, "  %s an item that collects nothing never alerts. Fix it or remove it.\n",
					t.Dim.Apply("why it matters:"))
				return nil
			}
			fmt.Fprintf(app.Stderr, "%s Test returned data.\n", t.Green.Apply(sym.OK))
			return app.Printer.Object(testRaw, nil)
		},
	}
	c.Flags().StringVar(&host, "host", "", "host id (required)")
	c.Flags().StringVar(&name, "name", "", "item name (required)")
	c.Flags().StringVar(&module, "module", "", "monitoring module id")
	c.Flags().StringVar(&params, "params", "", "module parameters")
	c.Flags().StringVar(&credential, "credential", "", "credential id")
	c.Flags().StringVar(&credType, "credential-type", "", "credential type (api, snmpv2, snmpv3, viptela, no auth)")
	c.Flags().StringVar(&description, "description", "", "description")
	c.Flags().IntVar(&interval, "interval", 0, "polling interval")
	c.Flags().BoolVar(&skipTest, "skip-test", false, "do not test the item after creating it")
	return c
}

func newMonItemDeleteCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a monitoring item",
		Long: `Delete a monitoring item.

Removing monitoring is silent: nothing alerts when a check disappears, so
this always confirms.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, _, err := app.getOne("/api/monitoring/items/"+args[0]+"/", nil)
			if err != nil {
				return err
			}
			if err := app.Confirm(fmt.Sprintf(
				"Delete monitoring item %s (%s) on host %s? Coverage will stop silently.",
				args[0], str(m, "name"), dash(str(m, "host_name", "host")))); err != nil {
				return err
			}
			if err := app.mutate("DELETE", "/api/monitoring/items/"+args[0]+"/", nil, nil); err != nil {
				return err
			}
			if !app.DryRun {
				app.Printer.Infof("Deleted monitoring item %s.", args[0])
			}
			return nil
		},
	}
}

func newMonModuleCmd(app *App) *cobra.Command {
	c := &cobra.Command{Use: "module", Short: "Monitoring modules (check definitions)"}
	c.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List available modules",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runList(listOpts{
				Path:    "/api/monitoring/modules/",
				Columns: []string{"ID", "NAME", "TYPE", "DESCRIPTION"},
				Empty:   "No monitoring modules available.",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "name")), dash(str(m, "type", "module_type")),
						truncate(dash(str(m, "description")), 50),
					}
				},
			})
		},
	})
	return c
}

func newMonTemplateCmd(app *App) *cobra.Command {
	c := &cobra.Command{Use: "template", Short: "Monitoring templates"}
	c.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List monitoring templates",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runList(listOpts{
				Path:    "/api/monitoring/templates/",
				Columns: []string{"ID", "NAME", "DESCRIPTION"},
				Empty:   "No monitoring templates.",
				Cells: func(m row) []string {
					return []string{str(m, "id"), dash(str(m, "name")),
						truncate(dash(str(m, "description")), 60)}
				},
			})
		},
	})
	return c
}

func newMonHostsCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "hosts",
		Short: "Show monitored hosts and their status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runList(listOpts{
				Path:    "/api/monitoring/hosts/",
				Query:   app.infraQuery(nil),
				Columns: []string{"ID", "NAME", "STATUS", "INFRASTRUCTURE"},
				Empty:   "No monitored hosts.",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "name", "host_name")),
						dash(str(m, "status", "state")),
						dash(str(m, "infrastructure_name", "infrastructure")),
					}
				},
			})
		},
	}
}
