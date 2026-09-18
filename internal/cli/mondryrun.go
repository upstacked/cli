package cli

import (
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
	dryRunsPath = "/api/monitoring/item/dry-runs/"

	dryRunPending = "pending"
	// The run happens on the customer's monitoring agent, which polls for
	// queued work every few seconds, so nothing is ever instant here.
	dryRunPollInterval = 2 * time.Second
	dryRunWait         = 60 * time.Second
	// The server gives up on a run nobody reported after two minutes, and only
	// notices when the record is read. Waiting past that re-reads a record that
	// will never change again.
	dryRunServerTimeout = 2 * time.Minute
)

// dryRunOverrideKeys are the config fields a dry run applies in memory.
//
// Anything else is rejected rather than quietly dropped: a key the server
// ignores reads back as "that override was checked" when nothing checked it,
// which is the failure this command exists to prevent.
var dryRunOverrideKeys = []string{
	"host", "host_specific_api_call", "mapping_rules", "parameters",
	"response_root_path", "schema_mapping", "timeout",
}

// dryRunStages are the pipeline stages a trace reports, in execution order.
var dryRunStages = []struct{ key, label string }{
	{"request_status", "fetch"},
	{"host_mapping_status", "host mapping"},
	{"schema_mapping_status", "schema mapping"},
}

func newMonItemDryRunCmd(app *App) *cobra.Command {
	var host, fromFile string
	var wait time.Duration

	c := &cobra.Command{
		Use:   "dry-run <item-id>",
		Short: "Run a monitoring item's config once and show what it would collect",
		Long: `Execute a monitoring item's configuration once, publishing nothing.

A dry run runs the real pipeline - fetch, host mapping, schema mapping, alert
evaluation - with the writing ends replaced by ones that record instead of
publish. Nothing reaches Elasticsearch and no alert is raised. What comes back
is the data the config would have produced, in the shape real monitoring data
takes, plus a stage-by-stage trace saying where a failure happened.

This is the check to reach for. A config can be structurally valid and still
collect nothing, because whether it works depends on the shape of what the
device returns - which varies by device, firmware and API version, so it cannot
be decided statically. 'ups monitoring item test' answers a weaker question: it
stops at the raw response and never runs the mapping stages, so it cannot tell
you whether a config produces data.

--from-file applies overrides in memory that are never saved, which is how you
check a config before committing it:

    {"parameters": {"url": "https://..."}, "response_root_path": "$.data"}

Only api_data, snmpstd and icmp items have mapping stages to preview; anything
else is refused, and 'ups monitoring item test' is the check that still applies.

The run is dispatched to the monitoring agent and handed out exactly once, so
this polls for the outcome. A run that stays pending is not retried by reading
it again - queue a new one.`,
		Example: `  ups monitoring item dry-run 412
  ups monitoring item dry-run 412 --host 88
  ups monitoring item dry-run 412 --from-file config.json
  ups monitoring item dry-run 412 --wait 0`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, raw, err := app.dryRunItem(args[0], host, fromFile, wait)
			if err != nil {
				return err
			}
			if m == nil {
				return nil // --dry-run printed the request instead of sending it
			}
			if app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			if wait <= 0 {
				// The server answers the POST with status "pending" too, so
				// only the call site knows whether nobody waited or whether the
				// agent is late. Those need different advice.
				t := app.Theme()
				fmt.Fprintf(app.Stderr, "%s Dry run %s queued. Nothing was published.\n",
					t.Green.Apply(app.Sym().OK), dash(str(m, "id")))
				fmt.Fprintf(app.Stderr, "  %s ups monitoring item dry-run show %s\n",
					t.Dim.Apply("read it later:"), dash(str(m, "id")))
				return nil
			}
			return app.reportDryRun(m)
		},
	}
	c.Flags().StringVar(&host, "host", "", "run against this host (defaults to the item's test host, then its host)")
	c.Flags().StringVar(&fromFile, "from-file", "", "JSON file of config overrides to apply in memory, never saved")
	c.Flags().DurationVar(&wait, "wait", dryRunWait,
		"how long to wait for the result (0 returns once the run is queued)")
	c.AddCommand(newMonItemDryRunShowCmd(app))
	return c
}

func newMonItemDryRunShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <run-id>",
		Short: "Read back a dry run queued earlier",
		Long: `Show the record of one dry run.

This is how to collect the outcome after 'ups monitoring item dry-run --wait 0'.
Reading a run never re-dispatches it: a run is handed to an agent once, and one
that is still pending two minutes after it was queued is flipped to failed when
it is read. To try again, queue a new run.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, raw, err := app.getOne(dryRunsPath+args[0]+"/", nil)
			if err != nil {
				return err
			}
			if app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			return app.reportDryRun(m)
		},
	}
}

// dryRunItem queues a dry run and waits for the agent to report on it.
//
// The 201 only means the run was queued, so reporting on it alone would report
// "checked" for a config that later collected nothing - the exact confusion
// this command exists to remove.
func (a *App) dryRunItem(itemID, host, fromFile string, wait time.Duration) (row, jsonRaw, error) {
	body := map[string]any{"monitoring_item": atoiOr(itemID)}
	if fromFile != "" {
		over, err := readDryRunOverrides(fromFile)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range over {
			body[k] = v
		}
	}
	// The flag wins over the file, so a saved override file can be reused
	// against a different device without editing it.
	if host != "" {
		body["host"] = atoiOr(host)
	}

	var dispatch jsonRaw
	err := a.Spin("Queueing a dry run of monitoring item "+itemID, func() error {
		return a.mutate("POST", dryRunsPath, body, &dispatch)
	})
	if err != nil {
		return nil, nil, describeDryRunFailure(err, itemID)
	}
	if a.DryRun {
		return nil, nil, nil
	}
	var m row
	_ = jsonUnmarshal(dispatch, &m)

	runID := str(m, "id")
	if wait <= 0 || runID == "" {
		return m, dispatch, nil
	}
	if wait > dryRunServerTimeout {
		wait = dryRunServerTimeout
	}
	return a.awaitDryRun(runID, wait, m, dispatch)
}

// awaitDryRun polls until the run leaves pending or the wait runs out. The last
// record read is returned either way, so a caller always reports on something
// the server actually said.
func (a *App) awaitDryRun(runID string, wait time.Duration, last row, lastRaw jsonRaw) (row, jsonRaw, error) {
	path := dryRunsPath + runID + "/"
	deadline := time.Now().Add(wait)
	err := a.Spin("Waiting for the monitoring agent", func() error {
		for {
			got, raw, err := a.getOne(path, nil)
			if err != nil {
				return err
			}
			last, lastRaw = got, raw
			if str(got, "status") != dryRunPending {
				return nil
			}
			if time.Now().After(deadline) {
				return nil
			}
			time.Sleep(dryRunPollInterval)
		}
	})
	if err != nil {
		return nil, nil, err
	}
	return last, lastRaw, nil
}

// readDryRunOverrides loads the in-memory config overrides for a run.
func readDryRunOverrides(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errs.Usage("cannot read %s: %v", path, err)
	}
	var over map[string]any
	if err := jsonUnmarshal(b, &over); err != nil {
		return nil, errs.Usage("%s is not a JSON object", path).
			WithHint(`expected the config fields to override, e.g. {"parameters": {"url": "https://..."}}`).
			Wrapping(err)
	}
	allowed := map[string]bool{}
	for _, k := range dryRunOverrideKeys {
		allowed[k] = true
	}
	var bad []string
	for k := range over {
		if !allowed[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return nil, errs.Usage("%s sets fields a dry run cannot override: %s", path, strings.Join(bad, ", ")).
			WithHint("overridable fields are %s. The item id is the positional argument, not a field",
				strings.Join(dryRunOverrideKeys, ", "))
	}
	return over, nil
}

// describeDryRunFailure adds the context the generic HTTP mapping cannot know.
func describeDryRunFailure(err error, itemID string) error {
	switch errs.StatusOf(err) {
	case http.StatusBadRequest:
		e := errs.Usage("cannot dry-run monitoring item %s: %v", itemID, err).
			WithStatus(http.StatusBadRequest)
		// The server's own message is already in there and says why. Adding the
		// data-source hint to every 400 would send a caller whose item simply
		// has no host to a check that fails for the same reason.
		if mentionsDataSource(err.Error()) {
			// Setting --data-source does not satisfy this: an item with an
			// icmp action_type is refused here too, so the dry-run endpoint is
			// reading something the action-based model does not write. Saying
			// "set a data source" would send the caller round in a circle.
			e = e.WithHint("this check does not read action_type, so --data-source will not satisfy it. "+
				"It is a server-side gap, not a missing flag. The weaker check still applies: "+
				"ups monitoring item test %s", itemID)
		}
		return e
	case http.StatusForbidden:
		return errs.Auth("cannot dry-run monitoring item %s: %v", itemID, err).
			WithHint("the item's organization may be outside your scope. "+
				"Compare 'ups monitoring item show %s' with 'ups whoami'", itemID).
			WithStatus(http.StatusForbidden)
	}
	return err
}

// mentionsDataSource reports whether a rejection was about the item's data
// source rather than one of the other things a dry run can refuse.
func mentionsDataSource(msg string) bool {
	msg = strings.ToLower(msg)
	for _, w := range []string{"data source", "data_source", "api_data", "snmpstd", "icmp"} {
		if strings.Contains(msg, w) {
			return true
		}
	}
	return false
}

// --- reporting -----------------------------------------------------------

// reportDryRun prints the trace and the data points, and fails the command when
// the configuration would not have collected anything, so a script notices.
func (a *App) reportDryRun(m row) error {
	t, sym := a.Theme(), a.Sym()
	runID := dash(str(m, "id"))
	status := str(m, "status")
	trace := objField(m, "trace")
	points := listField(m, "data_points")
	failed := firstFailedStage(trace)

	// A run the agent never picked up comes back failed with no trace at all.
	// Reporting that as "this configuration would publish nothing" blames the
	// config for an infrastructure outage, and sends someone to delete a check
	// that was never tested. Confirmed against a live server: a run nobody
	// reports on is flipped to failed on read, with error set and trace null.
	if status == "failed" && !dryRunExecuted(trace) {
		fmt.Fprintf(a.Stderr, "%s Dry run %s never ran.\n", t.Red.Apply(sym.Fail), runID)
		if e := str(m, "error"); e != "" {
			fmt.Fprintf(a.Stderr, "  %s %s\n", t.Dim.Apply("agent reported:"), e)
		}
		return errs.General("dry run %s did not execute", runID).
			WithHint("nothing was checked, so this says nothing about the item's config. " +
				"Confirm the infrastructure's monitoring agent is online, then queue a new run")
	}

	switch status {
	case "success":
		fmt.Fprintf(a.Stderr, "%s Dry run %s succeeded. Nothing was published.\n",
			t.Green.Apply(sym.OK), runID)
	case "partial":
		fmt.Fprintf(a.Stderr, "%s Dry run %s mapped only part of what it fetched.\n",
			t.Yellow.Apply(sym.Warn), runID)
	case "failed":
		where := ""
		if failed != "" {
			where = " at " + failed
		}
		fmt.Fprintf(a.Stderr, "%s Dry run %s failed%s.\n", t.Red.Apply(sym.Fail), runID, where)
	case dryRunPending:
		fmt.Fprintf(a.Stderr, "%s Dry run %s is still queued. The agent has not reported back.\n",
			t.Yellow.Apply(sym.Warn), runID)
		fmt.Fprintf(a.Stderr, "  %s ups monitoring item dry-run show %s\n", t.Dim.Apply("read it later:"), runID)
		fmt.Fprintf(a.Stderr, "  %s a run is handed to an agent once. Reading it again does not re-dispatch it,\n",
			t.Dim.Apply("note:"))
		fmt.Fprintf(a.Stderr, "        and one still pending after 2 minutes is failed. Queue a new run instead.\n")
		return nil
	}

	// Labelled as the agent's, so it is not mistaken for the command's own
	// error line further down.
	if e := str(m, "error"); e != "" {
		fmt.Fprintf(a.Stderr, "  %s %s\n", t.Dim.Apply("agent reported:"), e)
	}

	a.printDryRunTrace(trace)
	a.printDryRunPoints(points)
	a.printDryRunAlerts(listField(m, "alerts"))
	a.warnDryRunTruncation(trace)

	// The command fails on the outcome the whole feature exists to catch: a
	// config that would publish nothing. The header above already said where,
	// so the error says what it means rather than repeating it.
	switch status {
	case "failed":
		where := "the run failed"
		if failed != "" {
			where = "it failed at " + failed
		}
		return errs.General("dry run %s collected nothing", runID).
			WithHint("%s. An item that collects nothing never alerts - fix the config or remove the item", where)
	case "success", "partial":
		if len(points) == 0 {
			return errs.General("dry run %s collected nothing", runID).
				WithHint("every stage ran, but nothing would have been published. " +
					"An item that collects nothing never alerts")
		}
	}
	return nil
}

// dryRunExecuted reports whether any pipeline stage actually ran. It is what
// separates "the config collects nothing" from "the config was never tried".
func dryRunExecuted(trace row) bool {
	for _, st := range dryRunStages {
		if objField(trace, st.key) != nil {
			return true
		}
	}
	return false
}

// firstFailedStage names the earliest stage that did not succeed, which is
// where the operator has to look. A later stage failing because an earlier one
// produced nothing is a consequence, not the cause.
func firstFailedStage(trace row) string {
	for _, st := range dryRunStages {
		s := objField(trace, st.key)
		if s == nil {
			continue
		}
		if v := str(s, "status"); v != "" && v != "success" {
			return st.label
		}
	}
	return ""
}

func (a *App) printDryRunTrace(trace row) {
	if trace == nil {
		return
	}
	t := a.Theme()
	out := a.Stdout
	fmt.Fprintf(out, "%s\n", t.Bold.Apply("Stages"))
	for _, st := range dryRunStages {
		s := objField(trace, st.key)
		if s == nil {
			fmt.Fprintf(out, "  %-15s %s\n", st.label, t.Dim.Apply("not run"))
			continue
		}
		status := str(s, "status")
		fmt.Fprintf(out, "  %-15s %-9s %s\n", st.label, status, dryRunStageSummary(st.key, s))
		if status == "success" {
			continue
		}
		for _, line := range dryRunStageDetails(st.key, s) {
			fmt.Fprintf(out, "      %s\n", line)
		}
	}
	if rules := objField(trace, "alerting_rules"); len(rules) > 0 {
		var parts []string
		for _, k := range sortedKeys(rules) {
			parts = append(parts, fmt.Sprintf("%s: %s", k, plain(rules[k])))
		}
		fmt.Fprintf(out, "  %-15s %s\n", "alerting rules", strings.Join(parts, ", "))
	}
	fmt.Fprintln(out)
}

// dryRunStageSummary is the one-line "what happened" for a stage.
func dryRunStageSummary(key string, s row) string {
	details := objField(s, "details")
	switch key {
	case "request_status":
		if code := str(details, "status_code"); code != "" {
			return "HTTP " + code
		}
	case "schema_mapping_status":
		if total := str(s, "total"); total != "" {
			return fmt.Sprintf("%s/%s mapped", dash(str(s, "success")), total)
		}
	}
	return truncate(str(details, "message"), 70)
}

// dryRunStageDetails is what an operator needs to fix the stage.
func dryRunStageDetails(key string, s row) []string {
	details := objField(s, "details")
	if details == nil {
		return nil
	}
	var out []string
	if msg := str(details, "message"); msg != "" && dryRunStageSummary(key, s) != truncate(msg, 70) {
		out = append(out, msg)
	}
	switch key {
	case "host_mapping_status":
		// The single most useful thing on a host-mapping failure: what each
		// candidate actually rendered to, so the mismatch is visible rather
		// than guessed at.
		if path := str(details, "identifier_path"); path != "" {
			out = append(out, fmt.Sprintf("identifier %s %s %q",
				path, dash(str(details, "operator")), plain(details["value"])))
		}
		if cands := details["candidate_identifiers"]; cands != nil {
			out = append(out, "candidates: "+dash(joinAny(cands, ", ")))
		}
	case "schema_mapping_status":
		for _, e := range listField(details, "errors") {
			out = append(out, fmt.Sprintf("%s.%s  %s: %s",
				dash(str(e, "schema")), dash(str(e, "field")),
				dash(str(e, "expr")), str(e, "message")))
		}
	}
	return out
}

func (a *App) printDryRunPoints(points []row) {
	t := a.Theme()
	out := a.Stdout
	if len(points) == 0 {
		fmt.Fprintf(out, "%s\n  %s\n", t.Bold.Apply("Data points"),
			"none - this configuration would publish nothing")
		return
	}
	fmt.Fprintf(out, "%s (%d)\n", t.Bold.Apply("Data points"), len(points))
	for _, p := range points {
		extra := objField(p, "extra")
		where := "host " + dash(str(extra, "host_id"))
		if ip := str(extra, "ip"); ip != "" {
			where += " (" + ip + ")"
		}
		fmt.Fprintf(out, "  %s  %s  %s  %s\n",
			dash(str(p, "@timestamp", "timestamp")), where,
			dash(str(extra, "schema_name", "schema_id")),
			dryRunValues(objField(extra, "value")))
	}
	fmt.Fprintln(out)
}

func (a *App) printDryRunAlerts(alerts []row) {
	if len(alerts) == 0 {
		return
	}
	t := a.Theme()
	out := a.Stdout
	fmt.Fprintf(out, "%s (%d)\n", t.Bold.Apply("Alerts that would have fired"), len(alerts))
	for _, al := range alerts {
		kind := "alert"
		if str(al, "recovery_event") == "true" {
			kind = "recovery"
		}
		fmt.Fprintf(out, "  %s  %s  %s  %s  %s\n",
			dash(str(al, "host_name", "host_id")), dash(str(al, "identifier")),
			dash(str(al, "monitoring_item")), kind,
			dryRunValues(objField(al, "payload")))
	}
	fmt.Fprintln(out)
}

// warnDryRunTruncation makes a capped preview impossible to mistake for a
// complete one, the same rule the list commands follow.
func (a *App) warnDryRunTruncation(trace row) {
	if trace == nil {
		return
	}
	if cap := str(trace, "data_points_truncated"); cap != "" {
		fmt.Fprintf(a.Stderr,
			"\nnote: only the first %s data points were captured. This is not everything the config would publish.\n", cap)
	}
	req := objField(trace, "request_status")
	if str(objField(req, "details"), "response_truncated") == "true" {
		fmt.Fprintf(a.Stderr,
			"note: the raw response in the trace was truncated. Do not read it as the whole payload.\n")
	}
}

// dryRunValues renders a data point's value map. The engine JSON-encodes every
// value, so `"up"` arrives as a quoted string; it is decoded back for display.
func dryRunValues(v row) string {
	if len(v) == 0 {
		return "-"
	}
	var parts []string
	for _, k := range sortedKeys(v) {
		parts = append(parts, k+"="+decodeDataValue(v[k]))
	}
	return strings.Join(parts, " ")
}

func decodeDataValue(v any) string {
	s, ok := v.(string)
	if !ok {
		return plain(v)
	}
	var decoded any
	if err := jsonUnmarshal([]byte(s), &decoded); err == nil {
		return plain(decoded)
	}
	return s
}

// --- generic JSON navigation --------------------------------------------

func objField(m row, key string) row {
	if m == nil {
		return nil
	}
	v, ok := m[key].(map[string]any)
	if !ok {
		return nil
	}
	return row(v)
}

func listField(m row, key string) []row {
	if m == nil {
		return nil
	}
	v, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]row, 0, len(v))
	for _, it := range v {
		if o, ok := it.(map[string]any); ok {
			out = append(out, row(o))
		}
	}
	return out
}

// plain renders any JSON scalar the way a person would write it.
func plain(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return fmt.Sprintf("%t", t)
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	}
	return fmt.Sprintf("%v", v)
}

func joinAny(v any, sep string) string {
	items, ok := v.([]any)
	if !ok {
		return plain(v)
	}
	var out []string
	for _, it := range items {
		out = append(out, plain(it))
	}
	return strings.Join(out, sep)
}
