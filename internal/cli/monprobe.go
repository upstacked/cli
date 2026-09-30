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
	probesPath = "/api/monitoring/item/dry-runs/"

	probePending = "pending"
	// The run happens on the customer's monitoring agent, which polls for
	// queued work every few seconds, so nothing is ever instant here.
	probePollInterval = 2 * time.Second
	probeWait         = 60 * time.Second
	// The server gives up on a run nobody reported after two minutes, and only
	// notices when the record is read. Waiting past that re-reads a record that
	// will never change again.
	probeServerTimeout = 2 * time.Minute
)

// probeOverrideKeys are the config fields a probe applies in memory.
//
// Anything else is rejected rather than quietly dropped: a key the server
// ignores reads back as "that override was checked" when nothing checked it,
// which is the failure this command exists to prevent.
var probeOverrideKeys = []string{
	"host", "host_specific_api_call", "mapping_rules", "parameters",
	"response_root_path", "schema_mapping", "timeout",
}

// probeStages are the pipeline stages a trace reports, in execution order.
var probeStages = []struct{ key, label string }{
	{"request_status", "fetch"},
	{"host_mapping_status", "host mapping"},
	{"schema_mapping_status", "schema mapping"},
}

func newMonItemProbeCmd(app *App) *cobra.Command {
	var host, fromFile string
	var wait time.Duration

	c := &cobra.Command{
		Use:   "probe <item-id>",
		Short: "Run a monitoring item's config once and show what it would collect",
		Long: `Execute a monitoring item's configuration once, publishing nothing.

A probe runs the real pipeline - fetch, host mapping, schema mapping, alert
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
		Example: `  ups monitoring item probe 412
  ups monitoring item probe 412 --host 88
  ups monitoring item probe 412 --from-file config.json
  ups monitoring item probe 412 --wait 0`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, raw, err := app.probeItem(args[0], host, fromFile, wait)
			if err != nil {
				return err
			}
			if m == nil {
				return nil // --dry-run printed the request instead of sending it
			}
			if wait <= 0 && !app.AsJSON {
				// The server answers the POST with status "pending" too, so
				// only the call site knows whether nobody waited or whether the
				// agent is late. Those need different advice.
				t := app.Theme()
				fmt.Fprintf(app.Stderr, "%s Probe %s queued. Nothing was published.\n",
					t.Green.Apply(app.Sym().OK), dash(str(m, "id")))
				fmt.Fprintf(app.Stderr, "  %s ups monitoring item probe show %s\n",
					t.Dim.Apply("read it later:"), dash(str(m, "id")))
				return nil
			}
			return app.reportProbe(m, raw)
		},
	}
	c.Flags().StringVar(&host, "host", "", "run against this host (defaults to the item's test host, then its host)")
	c.Flags().StringVar(&fromFile, "from-file", "", "JSON file of config overrides to apply in memory, never saved")
	c.Flags().DurationVar(&wait, "wait", probeWait,
		"how long to wait for the result (0 returns once the run is queued)")
	c.AddCommand(newMonItemProbeShowCmd(app))
	return c
}

// newMonItemDryRunCmd keeps the command's old name working for scripts and
// skills installed before the rename. Deprecated hides it from help.
func newMonItemDryRunCmd(app *App) *cobra.Command {
	c := newMonItemProbeCmd(app)
	c.Use = "dry-run <item-id>"
	c.Deprecated = "use 'ups monitoring item probe' instead"
	return c
}

func newMonItemProbeShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <run-id>",
		Short: "Read back a probe queued earlier",
		Long: `Show the record of one probe.

This is how to collect the outcome after 'ups monitoring item probe --wait 0'.
Reading a run never re-dispatches it: a run is handed to an agent once, and one
that is still pending two minutes after it was queued is flipped to failed when
it is read. To try again, queue a new run.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, raw, err := app.getOne(probesPath+args[0]+"/", nil)
			if err != nil {
				return err
			}
			return app.reportProbe(m, raw)
		},
	}
}

// probeItem queues a probe and waits for the agent to report on it.
//
// The 201 only means the run was queued, so reporting on it alone would report
// "checked" for a config that later collected nothing - the exact confusion
// this command exists to remove.
func (a *App) probeItem(itemID, host, fromFile string, wait time.Duration) (row, jsonRaw, error) {
	body := map[string]any{"monitoring_item": atoiOr(itemID)}
	if fromFile != "" {
		over, err := readProbeOverrides(fromFile)
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
	err := a.Spin("Queueing a probe of monitoring item "+itemID, func() error {
		return a.mutate("POST", probesPath, body, &dispatch)
	})
	if err != nil {
		return nil, nil, describeProbeFailure(err, itemID)
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
	if wait > probeServerTimeout {
		wait = probeServerTimeout
	}
	return a.awaitProbe(runID, wait, m, dispatch)
}

// awaitProbe polls until the run leaves pending or the wait runs out. The last
// record read is returned either way, so a caller always reports on something
// the server actually said.
func (a *App) awaitProbe(runID string, wait time.Duration, last row, lastRaw jsonRaw) (row, jsonRaw, error) {
	path := probesPath + runID + "/"
	deadline := time.Now().Add(wait)
	err := a.Spin("Waiting for the monitoring agent", func() error {
		for {
			got, raw, err := a.getOne(path, nil)
			if err != nil {
				return err
			}
			last, lastRaw = got, raw
			if str(got, "status") != probePending {
				return nil
			}
			if time.Now().After(deadline) {
				return nil
			}
			time.Sleep(probePollInterval)
		}
	})
	if err != nil {
		return nil, nil, err
	}
	return last, lastRaw, nil
}

// readProbeOverrides loads the in-memory config overrides for a run.
func readProbeOverrides(path string) (map[string]any, error) {
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
	for _, k := range probeOverrideKeys {
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
		return nil, errs.Usage("%s sets fields a probe cannot override: %s", path, strings.Join(bad, ", ")).
			WithHint("overridable fields are %s. The item id is the positional argument, not a field",
				strings.Join(probeOverrideKeys, ", "))
	}
	return over, nil
}

// describeProbeFailure adds the context the generic HTTP mapping cannot know.
func describeProbeFailure(err error, itemID string) error {
	switch errs.StatusOf(err) {
	case http.StatusBadRequest:
		e := errs.Usage("cannot probe monitoring item %s: %v", itemID, err).
			WithStatus(http.StatusBadRequest)
		// The server's own message is already in there and says why. Adding the
		// data-source hint to every 400 would send a caller whose item simply
		// has no host to a check that fails for the same reason.
		if mentionsDataSource(err.Error()) {
			// Servers before the fix read a legacy field that setting
			// action_type did not fill, so re-setting it only helps on a fixed
			// server. Name the fallback too, or the caller goes round in a circle.
			e = e.WithHint("set it again with 'ups monitoring item update %s --data-source <type:name>' "+
				"(api_data, snmp or icmp). If it is still refused, the server predates the fix that "+
				"reads action_type; the weaker check still applies: ups monitoring item test %s",
				itemID, itemID)
		}
		return e
	case http.StatusForbidden:
		return errs.Auth("cannot probe monitoring item %s: %v", itemID, err).
			WithHint("the item's organization may be outside your scope. "+
				"Compare 'ups monitoring item show %s' with 'ups whoami'", itemID).
			WithStatus(http.StatusForbidden)
	}
	return err
}

// mentionsDataSource reports whether a rejection was about the item's data
// source rather than one of the other things a probe can refuse.
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

// reportProbe prints the trace and the data points, and fails the command when
// the configuration would not have collected anything, so a script notices.
func (a *App) reportProbe(m row, raw jsonRaw) error {
	t, sym := a.Theme(), a.Sym()

	// A caller reading JSON gets the record itself, errors and all, rather than
	// tables it cannot parse. It is emitted here because several outcomes below
	// return early, and each of them is one a caller has to be able to read.
	// Every line written after this goes to stderr, so stdout carries nothing
	// but the document.
	if a.AsJSON && raw != nil {
		if err := a.Printer.Object(raw, nil); err != nil {
			return err
		}
	}
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
	if status == "failed" && !probeExecuted(trace) {
		fmt.Fprintf(a.Stderr, "%s Probe %s never ran.\n", t.Red.Apply(sym.Fail), runID)
		if e := str(m, "error"); e != "" {
			fmt.Fprintf(a.Stderr, "  %s %s\n", t.Dim.Apply("agent reported:"), e)
		}
		return errs.General("probe %s did not execute", runID).
			WithHint("nothing was checked, so this says nothing about the item's config. " +
				"Confirm the infrastructure's monitoring agent is online, then queue a new run")
	}

	switch status {
	case "success":
		fmt.Fprintf(a.Stderr, "%s Probe %s succeeded. Nothing was published.\n",
			t.Green.Apply(sym.OK), runID)
	case "partial":
		fmt.Fprintf(a.Stderr, "%s Probe %s mapped only part of what it fetched.\n",
			t.Yellow.Apply(sym.Warn), runID)
	case "failed":
		where := ""
		if failed != "" {
			where = " at " + failed
		}
		fmt.Fprintf(a.Stderr, "%s Probe %s failed%s.\n", t.Red.Apply(sym.Fail), runID, where)
	case probePending:
		fmt.Fprintf(a.Stderr, "%s Probe %s is still queued. The agent has not reported back.\n",
			t.Yellow.Apply(sym.Warn), runID)
		fmt.Fprintf(a.Stderr, "  %s ups monitoring item probe show %s\n", t.Dim.Apply("read it later:"), runID)
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

	if !a.AsJSON {
		a.printProbeTrace(trace)
		a.printProbePoints(points)
		a.printProbeAlerts(listField(m, "alerts"))
	}
	a.warnProbeTruncation(trace)

	// The command fails on the outcome the whole feature exists to catch: a
	// config that would publish nothing. The header above already said where,
	// so the error says what it means rather than repeating it.
	switch status {
	case "failed":
		where := "the run failed"
		if failed != "" {
			where = "it failed at " + failed
		}
		return errs.General("probe %s collected nothing", runID).
			WithHint("%s. An item that collects nothing never alerts - fix the config or remove the item", where)
	case "success", "partial":
		if len(points) == 0 {
			return errs.General("probe %s collected nothing", runID).
				WithHint("every stage ran, but nothing would have been published. " +
					"An item that collects nothing never alerts")
		}
	}
	return nil
}

// probeExecuted reports whether any pipeline stage actually ran. It is what
// separates "the config collects nothing" from "the config was never tried".
func probeExecuted(trace row) bool {
	for _, st := range probeStages {
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
	for _, st := range probeStages {
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

func (a *App) printProbeTrace(trace row) {
	if trace == nil {
		return
	}
	t := a.Theme()
	out := a.Stdout
	fmt.Fprintf(out, "%s\n", t.Bold.Apply("Stages"))
	for _, st := range probeStages {
		s := objField(trace, st.key)
		if s == nil {
			fmt.Fprintf(out, "  %-15s %s\n", st.label, t.Dim.Apply("not run"))
			continue
		}
		status := str(s, "status")
		fmt.Fprintf(out, "  %-15s %-9s %s\n", st.label, status, probeStageSummary(st.key, s))
		if status == "success" {
			continue
		}
		for _, line := range probeStageDetails(st.key, s) {
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

// probeStageSummary is the one-line "what happened" for a stage.
func probeStageSummary(key string, s row) string {
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

// probeStageDetails is what an operator needs to fix the stage.
func probeStageDetails(key string, s row) []string {
	details := objField(s, "details")
	if details == nil {
		return nil
	}
	var out []string
	if msg := str(details, "message"); msg != "" && probeStageSummary(key, s) != truncate(msg, 70) {
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

func (a *App) printProbePoints(points []row) {
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
			probeValues(objField(extra, "value")))
	}
	fmt.Fprintln(out)
}

func (a *App) printProbeAlerts(alerts []row) {
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
			probeValues(objField(al, "payload")))
	}
	fmt.Fprintln(out)
}

// warnProbeTruncation makes a capped preview impossible to mistake for a
// complete one, the same rule the list commands follow.
func (a *App) warnProbeTruncation(trace row) {
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

// probeValues renders a data point's value map. The engine JSON-encodes every
// value, so `"up"` arrives as a quoted string; it is decoded back for display.
func probeValues(v row) string {
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
