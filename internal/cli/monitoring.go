package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
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
field, so 'ups monitoring item dry-run' is the feedback loop that
distinguishes healthy from never-collected-anything: it runs the real
pipeline and shows the data the config would have published, without
publishing any of it.`,
	}
	c.AddCommand(newMonItemCmd(app), newMonModuleCmd(app), newMonTemplateCmd(app),
		newMonSchemaCmd(app), newMonActionCmd(app), newMonHostsCmd(app))
	return c
}

func newMonItemCmd(app *App) *cobra.Command {
	c := &cobra.Command{Use: "item", Short: "Monitoring items"}
	c.AddCommand(
		newMonItemListCmd(app), newMonItemShowCmd(app),
		newMonItemDryRunCmd(app), newMonItemTestCmd(app),
		newMonItemCreateCmd(app), newMonItemUpdateCmd(app),
		newMonItemDeleteCmd(app), newMonItemResultsCmd(app),
		newMonItemMappingCmd(app),
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
				Columns: []string{"ID", "NAME", "HOST", "MODULE", "INTERVAL", "CONFIG"},
				Empty:   "No monitoring items found.",
				Cells: func(m row) []string {
					// CONFIG is INCOMPLETE until a dry run has confirmed the
					// item's current config against its current device, so it
					// is the column that says "nobody has checked this".
					return []string{
						str(m, "id"), dash(str(m, "name")), dash(str(m, "host_name", "host")),
						dash(str(m, "monitoring_module_name", "monitoring_module")),
						dash(str(m, "interval")),
						dash(str(m, "monitoring_item_config_status")),
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
				{"Config status", dash(str(m, "monitoring_item_config_status"))},
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
		Short: "Fetch a monitoring item's raw response (weaker than a dry run)",
		Long: `Run a monitoring item once and report the raw response.

This stops at the fetch. It never runs host or schema mapping, so it cannot
tell you whether the config produces data - only whether the device answered.
Prefer 'ups monitoring item dry-run', which runs the whole pipeline and shows
what would have been published. Reach for this one when the item's data source
has no mapping stages to preview and the dry run refuses it.

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
	var host, testHost, template, org, name, module, params, credential, credType, description string
	var rootPath, fromFile, dataSource string
	var interval int
	var skipTest bool

	c := &cobra.Command{
		Use:   "create",
		Short: "Add a monitoring item to a host or a template",
		Long: `Create a monitoring item.

With --host the item is created on that device and, unless --skip-test is
given, dry-run immediately so a silently-broken check is caught now rather than
during an incident. The dry run publishes nothing; it only reports what the
config would have collected.

With --template the item is created without a host: a blank that the template
stamps onto every device it is applied to. Write host-specific values as Jinja
references, e.g. {{ host.i_ip_address }}. A host-less item is checked against
its --test-host; without one there is no device to poll until it is applied.

--data-source is required. It decides whether the check speaks SNMP, HTTP or
ICMP, and an item without one polls nothing at all - so this refuses rather
than creating a check that can never run. Take an id, a "type:name" pair, or a
bare type that offers only one action: ups monitoring action list.

--from-file starts from a JSON config in the same shape 'dry-run --from-file'
takes, so a config already proved on one device can be copied onto another in
one step rather than retyped as flags. Explicit flags override the file.

The item is only half the check: it says how to reach the data, not what the
data means. Give it a schema mapping next, or it fetches happily and publishes
nothing - see 'ups monitoring item mapping'.`,
		Example: `  ups monitoring item create --host 12 --name "CPU" --module 3 --data-source snmp:walk
  ups monitoring item create --host 12 --name "API health" --module 7 --data-source api_data --credential-type api
  ups monitoring item create --host 12 --name "Interfaces" --module 3 --data-source snmp:walk --from-file config.json
  ups monitoring item create --template 4 --name "uptime" --module 3 --data-source snmp --params '{"oid":"1.3.6.1.2.1.1.3.0"}'`,
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
			body, err := itemUpdateBody(fromFile)
			if err != nil {
				return err
			}
			body["name"] = name

			// Refused rather than defaulted: an item with no data source does
			// not know which protocol to speak, so it polls nothing - and per
			// the coverage rule, nothing reports that later.
			if dataSource == "" {
				_, hasAction := body["action_type"]
				_, hasSource := body["data_source"]
				if !hasAction && !hasSource {
					return errs.Usage("--data-source is required").
						WithHint("it decides whether the check speaks SNMP, HTTP or ICMP: ups monitoring action list")
				}
			} else if err := app.setDataSource(body, dataSource); err != nil {
				return err
			}

			var orgID string
			if host != "" {
				body["host"] = atoiOr(host)
				orgID, err = app.resolveOrganization(org)
			} else {
				delete(body, "host")
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
			addIf(body, "response_root_path", rootPath)
			if credential != "" {
				body["credential"] = atoiOr(credential)
			}

			if testHost != "" {
				body["test_host"] = atoiOr(testHost)
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

			if template != "" && testHost == "" {
				// Nothing to poll yet, so the usual test is not skipped so much
				// as impossible. Say which it is.
				fmt.Fprintf(app.Stderr, "  %s a template item cannot be checked until it is applied to a host.\n",
					t.Dim.Apply("note:"))
				fmt.Fprintf(app.Stderr, "  %s ups monitoring template apply %s --host <id>\n",
					t.Dim.Apply("next:"), template)
				fmt.Fprintf(app.Stderr, "  %s check it now against one device: ups monitoring item update %s --test-host <host-id>\n",
					t.Dim.Apply("or:"), id)
				return nil
			}

			if skipTest || id == "" {
				fmt.Fprintf(app.Stderr, "  %s verify it collects data: ups monitoring item dry-run %s\n",
					t.Yellow.Apply(sym.Warn), id)
				return nil
			}
			app.verifyCreatedItem(id)
			return nil
		},
	}
	c.Flags().StringVar(&host, "host", "", "host id (mutually exclusive with --template)")
	c.Flags().StringVar(&testHost, "test-host", "", "device a template item's checks run against")
	c.Flags().StringVar(&template, "template", "", "add the item to this monitoring template instead of a host")
	c.Flags().StringVar(&org, "org", "", "organization id (defaults to yours when you belong to exactly one)")
	c.Flags().StringVar(&name, "name", "", "item name (required)")
	c.Flags().StringVar(&module, "module", "", "monitoring module id")
	c.Flags().StringVar(&params, "params", "", "module parameters")
	c.Flags().StringVar(&credential, "credential", "", "credential id")
	c.Flags().StringVar(&credType, "credential-type", "", "credential type (api, snmpv2, snmpv3, viptela, no auth)")
	c.Flags().StringVar(&description, "description", "", "description")
	c.Flags().StringVar(&rootPath, "response-root-path", "", "JSON path the field paths are evaluated relative to")
	c.Flags().StringVar(&dataSource, "data-source", "", "api, snmp or icmp; or a legacy action id, \"type:name\" or unambiguous type (required)")
	c.Flags().StringVar(&fromFile, "from-file", "", "JSON config to start from, in the same shape 'dry-run --from-file' takes")
	c.Flags().IntVar(&interval, "interval", 0, "polling interval")
	c.Flags().BoolVar(&skipTest, "skip-test", false, "do not verify the item after creating it")
	return c
}

// verifyCreatedItem confirms a freshly created item actually collects something.
//
// A dry run is the check that answers the question - it runs the mapping stages
// the test endpoint never reaches - but only three data sources have those
// stages, and the server refuses the rest with a 400. Falling back to the
// weaker test is better than leaving the item unverified, so long as which
// check ran is said out loud.
//
// Nothing here fails the command: the item exists either way, and a caller who
// is told "created" and nothing else would assume it works. So a failure is
// always reported, and never reported as a pass.
// patchItem updates a monitoring item without erasing what the body leaves out.
//
// Older servers reset parameters and description on any PATCH that omits them,
// which silently erases an SNMP item's OIDs. The stored values are sent back
// when they are not being changed.
func (a *App) patchItem(id string, body map[string]any) error {
	_, hasParams := body["parameters"]
	_, hasDesc := body["description"]
	if (!hasParams || !hasDesc) && !a.DryRun {
		current, _, err := a.getOne("/api/monitoring/items/"+id+"/", nil)
		if err != nil {
			return err
		}
		body = copyBody(body)
		if !hasParams {
			body["parameters"] = current["parameters"]
		}
		if !hasDesc {
			body["description"] = current["description"]
		}
	}
	return a.mutate("PATCH", "/api/monitoring/items/"+id+"/", body, nil)
}

func (a *App) verifyCreatedItem(id string) {
	t, sym := a.Theme(), a.Sym()

	m, _, err := a.dryRunItem(id, "", "", dryRunWait)
	switch {
	case err == nil:
		if rerr := a.reportDryRun(m); rerr != nil {
			// The hint distinguishes a config that collects nothing from a run
			// that never happened; a blanket "delete it" would be wrong advice
			// for the second.
			fmt.Fprintf(a.Stderr, "%s %v\n", t.Red.Apply(sym.Fail), rerr)
			if hint := errs.HintOf(rerr); hint != "" {
				fmt.Fprintf(a.Stderr, "  %s %s\n", t.Dim.Apply("what to do:"), hint)
			}
			fmt.Fprintf(a.Stderr, "  %s the item exists either way: ups monitoring item show %s\n",
				t.Dim.Apply("note:"), id)
		}
		return
	case errs.StatusOf(err) != http.StatusBadRequest:
		fmt.Fprintf(a.Stderr, "%s The item was created but could not be dry-run: %v\n",
			t.Yellow.Apply(sym.Warn), err)
		fmt.Fprintf(a.Stderr, "  %s an item that collects nothing never alerts. Fix it or remove it.\n",
			t.Dim.Apply("why it matters:"))
		return
	}

	// A test is refused for the same reason, so falling back to one only adds
	// a second failure. Say how to give the item a device instead.
	if strings.Contains(err.Error(), "no host to run against") {
		fmt.Fprintf(a.Stderr, "%s Not checked: item %s has no device to run against.\n",
			t.Yellow.Apply(sym.Warn), id)
		fmt.Fprintf(a.Stderr, "  %s ups monitoring item update %s --test-host <host-id>\n",
			t.Dim.Apply("give it one:"), id)
		return
	}

	// Report the server's reason rather than assuming it was the data source:
	// an item with no host to run against is refused the same way, and calling
	// that "no mapping stages to preview" would be a plain lie. Say which check
	// ran either way, because a test proves less than a dry run does.
	fmt.Fprintf(a.Stderr, "  %s the dry run was refused, so the item was tested instead.\n",
		t.Dim.Apply("note:"))
	fmt.Fprintf(a.Stderr, "        A test stops at the raw response and cannot confirm it collects data.\n")
	fmt.Fprintf(a.Stderr, "  %s %v\n", t.Dim.Apply("refused because:"), err)

	testRow, testRaw, terr := a.testItem(id, testWait)
	if terr != nil {
		fmt.Fprintf(a.Stderr, "%s The item was created but could not be tested: %v\n",
			t.Yellow.Apply(sym.Warn), terr)
		fmt.Fprintf(a.Stderr, "  %s an item that collects nothing never alerts. Fix it or remove it.\n",
			t.Dim.Apply("why it matters:"))
		return
	}
	if rerr := a.reportTestOutcome(testRow, testRaw, id); rerr != nil {
		fmt.Fprintf(a.Stderr, "%s %v\n", t.Red.Apply(sym.Fail), rerr)
		fmt.Fprintf(a.Stderr, "  %s the item exists. Fix it or remove it: ups monitoring item delete %s\n",
			t.Dim.Apply("next:"), id)
	}
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
	c.AddCommand(newMonModuleCreateCmd(app))
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

// itemUpdateKeys are the fields 'item update' will write from --from-file.
//
// Deliberately the dry-run override set plus the plain descriptive fields: a
// config proved with 'dry-run --from-file' is then saved with the same file,
// with nothing retyped in between and nothing able to drift.
var itemUpdateKeys = map[string]bool{
	"host": true, "host_specific_api_call": true, "mapping_rules": true,
	"parameters": true, "response_root_path": true, "timeout": true,
	"name": true, "description": true, "interval": true,
	"credential": true, "credential_type": true, "monitoring_module": true,
	"action_type": true, "data_source": true, "test_host": true,
}

func newMonItemUpdateCmd(app *App) *cobra.Command {
	var name, description, params, rootPath, credential, credType, host, testHost, fromFile, dataSource string
	var interval, timeout int
	var hostSpecific, skipTest bool

	c := &cobra.Command{
		Use:   "update <id>",
		Short: "Change a monitoring item's configuration",
		Long: `Change a monitoring item's configuration and re-check it.

This is the save half of the dry-run loop. 'dry-run --from-file' applies a
candidate config in memory and shows what it would collect; this writes the
same file to the item, so what was proved is what gets stored:

    ups monitoring item dry-run 412 --from-file config.json   # until it collects
    ups monitoring item update  412 --from-file config.json   # then save it

--from-file accepts the same keys as the dry run - parameters, mapping_rules,
response_root_path, host_specific_api_call, timeout, host - plus name,
description, interval, credential, credential_type and monitoring_module. Any
other key is refused rather than dropped, because a dropped field reads back
as saved when nothing saved it.

Every edit invalidates the dry run that confirmed the item: the config status
returns to INCOMPLETE and stays there until a new dry run passes. So this
dry-runs the item afterwards unless --skip-test.`,
		Example: `  ups monitoring item update 412 --from-file config.json
  ups monitoring item update 412 --response-root-path '$.data.items'
  ups monitoring item update 412 --params '{"oid":"1.3.6.1.4.1.9.9.109.1.1.1.1.8"}'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := itemUpdateBody(fromFile)
			if err != nil {
				return err
			}
			addIf(body, "name", name)
			addIf(body, "description", description)
			addIf(body, "parameters", params)
			addIf(body, "response_root_path", rootPath)
			addIf(body, "credential_type", credType)
			if dataSource != "" {
				if err := app.setDataSource(body, dataSource); err != nil {
					return err
				}
			}
			if credential != "" {
				body["credential"] = atoiOr(credential)
			}
			if host != "" {
				body["host"] = atoiOr(host)
			}
			if testHost != "" {
				body["test_host"] = atoiOr(testHost)
			}
			if interval > 0 {
				body["interval"] = interval
			}
			if timeout > 0 {
				body["timeout"] = timeout
			}
			if cmd.Flags().Changed("host-specific-api-call") {
				body["host_specific_api_call"] = hostSpecific
			}
			if len(body) == 0 {
				return errs.Usage("nothing to change").
					WithHint("pass --from-file, or one of --name, --params, --response-root-path, --data-source, --interval, --credential, --host, --test-host")
			}

			if _, ok := body["host"]; ok {
				// Repointing an item is not an edit of the same check: the
				// confirmation it carried was against the old device.
				fmt.Fprintf(app.Stderr, "  %s repointing the item at another host discards the dry run that confirmed it.\n",
					app.Theme().Dim.Apply("note:"))
			}

			if err := app.patchItem(args[0], body); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			t := app.Theme()
			fmt.Fprintf(app.Stderr, "%s Updated monitoring item %s\n", t.Green.Apply(app.Sym().OK), args[0])
			if skipTest {
				fmt.Fprintf(app.Stderr, "  %s the config status is now INCOMPLETE: ups monitoring item dry-run %s\n",
					t.Yellow.Apply(app.Sym().Warn), args[0])
				return nil
			}
			app.verifyCreatedItem(args[0])
			return nil
		},
	}
	c.Flags().StringVar(&fromFile, "from-file", "", "JSON config to save, in the same shape 'dry-run --from-file' takes")
	c.Flags().StringVar(&name, "name", "", "new name")
	c.Flags().StringVar(&description, "description", "", "new description")
	c.Flags().StringVar(&params, "params", "", "module parameters")
	c.Flags().StringVar(&rootPath, "response-root-path", "", "JSON path the field paths are evaluated relative to")
	c.Flags().StringVar(&dataSource, "data-source", "", "api, snmp or icmp; or a legacy action id, \"type:name\" or unambiguous type")
	c.Flags().StringVar(&credential, "credential", "", "credential id")
	c.Flags().StringVar(&credType, "credential-type", "", "credential type (api, snmpv2, snmpv3, viptela, no auth)")
	c.Flags().StringVar(&host, "host", "", "repoint the item at another host")
	c.Flags().StringVar(&testHost, "test-host", "", "device a template item's checks run against")
	c.Flags().IntVar(&interval, "interval", 0, "polling interval")
	c.Flags().IntVar(&timeout, "timeout", 0, "per-poll timeout in seconds")
	c.Flags().BoolVar(&hostSpecific, "host-specific-api-call", false, "the API is called once per host rather than once for all")
	c.Flags().BoolVar(&skipTest, "skip-test", false, "do not dry-run the item after saving")
	return c
}

// itemUpdateBody reads --from-file and refuses keys the update will not write.
func itemUpdateBody(path string) (map[string]any, error) {
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
	var unknown []string
	for k := range body {
		if !itemUpdateKeys[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, errs.Usage("%s sets fields this command does not write: %s",
			path, strings.Join(unknown, ", ")).
			WithHint("schema_mapping is written by 'ups monitoring item mapping'; remove the rest rather than leaving them to be ignored")
	}
	return body, nil
}

func newMonModuleCreateCmd(app *App) *cobra.Command {
	var name, org string
	c := &cobra.Command{
		Use:   "create",
		Short: "Create a monitoring module",
		Long: `Create a monitoring module: the definition a check is an instance of.

Modules are how checks reach a template. A template holds modules, and an
item belongs to a template because its module is in that template's set - so
grouping related checks under one module is what makes them travel together.

Grouping has a consequence worth knowing before you choose: a module added to
two templates carries its items into both. That is the point when the checks
really are the same, and a surprise when they are not.`,
		Example: `  ups monitoring module create --name "Cisco CPU"
  ups monitoring template update 4 --add-module <module-id>`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return errs.Usage("--name is required")
			}
			orgID, err := app.resolveOrganization(org)
			if err != nil {
				return err
			}
			body := map[string]any{"name": name, "organization": atoiOr(orgID)}
			var raw jsonRaw
			if err := app.mutate("POST", "/api/monitoring/modules/", body, &raw); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			var m row
			_ = jsonUnmarshal(raw, &m)
			id := str(m, "id")
			t := app.Theme()
			fmt.Fprintf(app.Stderr, "%s Created monitoring module %s (%s)\n",
				t.Green.Apply(app.Sym().OK), name, id)
			fmt.Fprintf(app.Stderr, "  %s ups monitoring template update <template-id> --add-module %s\n",
				t.Dim.Apply("next:"), id)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "module name (required)")
	c.Flags().StringVar(&org, "org", "", "organization id (defaults to yours when you belong to exactly one)")
	return c
}
