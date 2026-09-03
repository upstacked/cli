package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
)

const (
	testInProgress   = "in_progress"
	testPollInterval = 2 * time.Second
	testWait         = 30 * time.Second
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
	var host, template string
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
			if template != "" {
				q.Set("monitoring_template", template)
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
	c.Flags().StringVar(&template, "template", "", "show the host-less items belonging to a template")
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
	var wait time.Duration
	c := &cobra.Command{
		Use:   "test <id>",
		Short: "Test a monitoring item and show what it collected",
		Long: `Run a monitoring item once and report what came back.

Do this before trusting a new or edited item. A misconfigured item does not
report an error: it silently collects nothing, or collects the wrong field,
and the gap only surfaces during an incident it failed to catch.

The test is dispatched to the monitoring agent and runs asynchronously, so
this waits for the outcome rather than reporting the dispatch as a success.
Pass --wait 0 to return as soon as it is queued.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, raw, err := app.testItem(args[0], wait)
			if err != nil {
				return err
			}
			if app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			return app.reportTestOutcome(m, raw, args[0])
		},
	}
	c.Flags().DurationVar(&wait, "wait", 30*time.Second,
		"how long to wait for the result (0 returns once the test is queued)")
	return c
}

// testItem dispatches a test and waits for the outcome.
//
// The endpoint is POST, not GET: running a test enqueues work and writes a
// result row. The 201 only means the monitoring agent accepted the job, so
// reporting on it alone would report "tested" for a check that later failed,
// which is the exact confusion this command exists to remove.
func (a *App) testItem(id string, wait time.Duration) (row, jsonRaw, error) {
	cl, err := a.Client()
	if err != nil {
		return nil, nil, err
	}

	var dispatch jsonRaw
	err = a.Spin("Testing monitoring item "+id, func() error {
		ctx, cancel := a.Ctx()
		defer cancel()
		return cl.Do(ctx, request("POST", "/api/monitoring/item/"+id+"/test", nil), &dispatch)
	})
	if err != nil {
		return nil, nil, describeTestFailure(err, id)
	}
	var m row
	_ = jsonUnmarshal(dispatch, &m)

	resultID := str(m, "monitoring_item_result_id")
	if wait <= 0 || resultID == "" {
		return m, dispatch, nil
	}

	path := "/api/monitoring/item/results/" + resultID + "/"
	deadline := time.Now().Add(wait)
	var last row
	var lastRaw jsonRaw
	err = a.Spin("Waiting for the result", func() error {
		for {
			got, raw, err := a.getOne(path, nil)
			if err != nil {
				return err
			}
			last, lastRaw = got, raw
			if str(got, "status") != testInProgress {
				return nil
			}
			if time.Now().After(deadline) {
				return nil
			}
			time.Sleep(testPollInterval)
		}
	})
	if err != nil {
		return nil, nil, err
	}
	return last, lastRaw, nil
}

// reportTestOutcome prints the result and fails the command when the check did
// not collect anything, so a script notices.
func (a *App) reportTestOutcome(m row, raw jsonRaw, itemID string) error {
	t, sym := a.Theme(), a.Sym()
	status := str(m, "status")

	switch status {
	case "success":
		fmt.Fprintf(a.Stderr, "%s Test succeeded.\n", t.Green.Apply(sym.OK))
	case "partial":
		fmt.Fprintf(a.Stderr, "%s Test returned only part of what it asked for.\n",
			t.Yellow.Apply(sym.Warn))
	case "failed":
		fmt.Fprintf(a.Stderr, "%s Test failed.\n", t.Red.Apply(sym.Fail))
	case testInProgress:
		fmt.Fprintf(a.Stderr, "%s Still running. The test was queued but has not reported back.\n",
			t.Yellow.Apply(sym.Warn))
		fmt.Fprintf(a.Stderr, "  %s ups monitoring item results %s\n", t.Dim.Apply("check later:"), itemID)
		return a.Printer.Object(raw, nil)
	case "":
		// --wait 0, or a server that answered without a result id.
		return a.Printer.Object(raw, nil)
	}

	if err := a.Printer.Object(raw, nil); err != nil {
		return err
	}
	if status == "failed" {
		return errs.General("monitoring item %s collected nothing", itemID).
			WithHint("an item that collects nothing never alerts. Fix it or remove it")
	}
	return nil
}

// describeTestFailure adds the context the generic HTTP mapping cannot know.
func describeTestFailure(err error, id string) error {
	switch errs.CodeOf(err) {
	case errs.CodeAuth:
		return errs.Auth("cannot test monitoring item %s: %v", id, err).
			WithHint("the item's organization may be outside your scope. Compare 'ups monitoring item show %s' with 'ups whoami'", id)
	case errs.CodeUsage:
		return errs.Usage("cannot test monitoring item %s: %v", id, err).
			WithHint("a missing monitoring agent on the infrastructure reports as a validation error here; check that the UMA is provisioned")
	}
	return err
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
	var host, template, org, name, module, params, credential, credType, description string
	var interval int
	var skipTest bool

	c := &cobra.Command{
		Use:   "create",
		Short: "Add a monitoring item to a host or a template",
		Long: `Create a monitoring item.

With --host the item is created on that device and, unless --skip-test is
given, tested immediately so a silently-broken check is caught now rather than
during an incident.

With --template the item is created without a host: a blank that the template
stamps onto every device it is applied to. Write host-specific values as Jinja
references, e.g. {{ host.i_ip_address }}. A host-less item cannot be tested,
because there is nothing to poll until it is applied.`,
		Example: `  ups monitoring item create --host 12 --name "CPU" --module 3
  ups monitoring item create --host 12 --name "API health" --module 7 --credential-type api
  ups monitoring item create --template 4 --name "uptime" --module 3 --params '{"oids":["1.3.6.1.2.1.1.3.0"]}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return errs.Usage("--name is required")
			}
			if (host == "") == (template == "") {
				return errs.Usage("exactly one of --host or --template is required").
					WithHint("--host adds a check to one device; --template adds it to every device the template is applied to")
			}
			// The API requires an organization on every item, host-bound or
			// not: its permission check reads the field straight off the body
			// and rejects the request outright when it is absent.
			body := map[string]any{"name": name}
			var orgID string
			var err error
			if host != "" {
				body["host"] = atoiOr(host)
				orgID, err = app.resolveOrganization(org)
			} else {
				orgID, err = app.templateItemOrg(template, module, org)
			}
			if err != nil {
				return err
			}
			body["organization"] = atoiOr(orgID)
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

			if template != "" {
				// Nothing to poll yet, so the usual test is not skipped so much
				// as impossible. Say which it is.
				fmt.Fprintf(app.Stderr, "  %s a template item cannot be tested until it is applied to a host.\n",
					t.Dim.Apply("note:"))
				fmt.Fprintf(app.Stderr, "  %s ups monitoring template apply %s --host <id>\n",
					t.Dim.Apply("next:"), template)
				return nil
			}

			if skipTest || id == "" {
				fmt.Fprintf(app.Stderr, "  %s verify it collects data: ups monitoring item test %s\n",
					t.Yellow.Apply(sym.Warn), id)
				return nil
			}
			// The item exists either way, so a failing test is reported and
			// does not fail the command - but it is never reported as a pass.
			testRow, testRaw, terr := app.testItem(id, testWait)
			if terr != nil {
				fmt.Fprintf(app.Stderr, "%s The item was created but could not be tested: %v\n",
					t.Yellow.Apply(sym.Warn), terr)
				fmt.Fprintf(app.Stderr, "  %s an item that collects nothing never alerts. Fix it or remove it.\n",
					t.Dim.Apply("why it matters:"))
				return nil
			}
			if err := app.reportTestOutcome(testRow, testRaw, id); err != nil {
				fmt.Fprintf(app.Stderr, "  %s the item exists. Fix it or remove it: ups monitoring item delete %s\n",
					t.Dim.Apply("next:"), id)
			}
			return nil
		},
	}
	c.Flags().StringVar(&host, "host", "", "host id (mutually exclusive with --template)")
	c.Flags().StringVar(&template, "template", "", "add the item to this monitoring template instead of a host")
	c.Flags().StringVar(&org, "org", "", "organization id (defaults to yours when you belong to exactly one)")
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
