package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/output"
)

const (
	trafficPath       = "/api/agents/traffic/"
	trafficExpectPath = "/api/agents/traffic/expect/"

	// The server's bounds on a quiet window (TrafficExpect.Body).
	trafficMaxQuiet = 7 * 24 * time.Hour
)

var trafficKinds = []string{"syslog", "flow"}

type trafficInfra struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Customer string `json:"customer"`
}

func (i trafficInfra) label() string {
	if i.Customer != "" {
		return i.Customer + " / " + i.Name
	}
	return i.Name
}

type trafficSignal struct {
	Kind          string           `json:"kind"`
	Status        string           `json:"status"`
	Expected      bool             `json:"expected"`
	Alert         *bool            `json:"alert"`
	Proposed      bool             `json:"proposed"`
	WindowMinutes int              `json:"window_minutes"`
	State         string           `json:"state"`
	Receivers     []int            `json:"receivers"`
	Rate          float64          `json:"rate"`
	LastSeen      string           `json:"last_seen"`
	Formats       map[string]int64 `json:"formats"`
	Transports    map[string]int64 `json:"transports"`
	Last24h       int64            `json:"last_24h"`
	Last7d        int64            `json:"last_7d"`
}

type trafficHost struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	SendsSyslog *bool  `json:"sends_syslog"`
	SendsFlow   *bool  `json:"sends_flow"`
}

type trafficCheck struct {
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type trafficDevice struct {
	Key            string                   `json:"key"`
	Address        string                   `json:"address"`
	Name           string                   `json:"name"`
	Host           *trafficHost             `json:"host"`
	Infrastructure trafficInfra             `json:"infrastructure"`
	Platform       string                   `json:"platform"`
	Status         string                   `json:"status"`
	Signals        map[string]trafficSignal `json:"signals"`
	Checks         []trafficCheck           `json:"checks"`
	Fix            *struct {
		Title string `json:"title"`
		Code  string `json:"code"`
	} `json:"fix"`
}

type trafficListener struct {
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
}

type trafficReceiver struct {
	ID             int               `json:"id"`
	Name           string            `json:"name"`
	Running        bool              `json:"running"`
	Address        string            `json:"address"`
	Infrastructure trafficInfra      `json:"infrastructure"`
	Listeners      []trafficListener `json:"listeners"`
}

type trafficResponse struct {
	Infrastructures     []trafficInfra    `json:"infrastructures"`
	Receivers           []trafficReceiver `json:"receivers"`
	LegacyLoggingAgents []trafficInfra    `json:"legacy_logging_agents"`
	Devices             []jsonRaw         `json:"devices"`
	Proposals           []int             `json:"proposals"`

	devices []trafficDevice
}

func newSendersCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "senders",
		Short: "Devices sending syslog and flows to the agents, and alerts when they stop",
		Long: `Show every device sending syslog or flows to the agents of an infrastructure,
and every host meant to. This is about whether the data arrives and is
readable; what the flows say is ups flow, what the logs say is ups logs.

Each device gets checks per protocol (arriving, stopped, never seen, clock,
format, templates, direction, loss) and, when something failed, the
configuration that fixes it, written for its platform. A device's receiver is
the agent it sends to, or the one to point it at.

An alert decides whether a ticket opens when a host stops sending. It changes
nothing on the device or the agent. Each host holds one decision per protocol:
undecided, alert on, or ignored. A host heard from in the last week with
nothing decided is offered; accept or ignore settles every offer at once, and
alert settles one host. A sender stays quiet for a window before a ticket
opens: 1 hour for syslog, 5 minutes for flows, unless the host says otherwise.

Only upstacked-agent data is shown. An infrastructure still on the legacy
logging agent has no sender addresses, so it lists nothing, and ups says so
on stderr rather than letting the empty list pass as "nothing sends".`,
	}
	c.AddCommand(newSendersListCmd(app), newSendersShowCmd(app),
		newSendersAlertCmd(app), newSendersDecideCmd(app, false),
		newSendersDecideCmd(app, true))
	return c
}

// trafficInfraIDs scopes a traffic request. Without an infrastructure the
// server answers for every one the caller can read, which is a different
// question; it has to be asked for by name.
func (a *App) trafficInfraIDs(all bool) ([]int, error) {
	if all {
		return nil, nil
	}
	id, err := a.Resolved.RequireInfra()
	if err != nil {
		return nil, errs.Usage("no infrastructure selected").
			WithHint("run: ups context set --infra-id <id>, or pass --all-infras for every infrastructure you can read")
	}
	n, err := strconv.Atoi(id)
	if err != nil {
		return nil, errs.Usage("infrastructure %q is not a numeric id", id).
			WithHint("traffic scopes by id: ups context set --infra-id <id>")
	}
	return []int{n}, nil
}

func (a *App) getTraffic(ids []int) (trafficResponse, jsonRaw, error) {
	q := url.Values{}
	if len(ids) > 0 {
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = strconv.Itoa(id)
		}
		q.Set("infrastructure", strings.Join(parts, ","))
	}
	var body trafficResponse
	raw, err := a.getFlowInto(trafficPath, q, &body)
	if err != nil {
		return body, raw, err
	}
	for _, d := range body.Devices {
		var dev trafficDevice
		if err := json.Unmarshal(d, &dev); err != nil {
			return body, raw, errs.General("unexpected device in %s", trafficPath).Wrapping(err)
		}
		body.devices = append(body.devices, dev)
	}
	a.warnLegacyLogging(body.LegacyLoggingAgents)
	return body, raw, nil
}

// warnLegacyLogging goes to stderr even with --json: an infrastructure on the
// legacy agent lists no senders, which reads exactly like one where nothing
// sends.
func (a *App) warnLegacyLogging(infras []trafficInfra) {
	for _, i := range infras {
		a.Printer.Infof("warning: %s still uses the legacy logging agent, which keeps no sender address; its devices are not listed", i.label())
	}
	if len(infras) > 0 {
		a.Printer.Infof("move it to an upstacked agent to see its traffic here")
	}
}

// signalCell reads as the state, then the alert decision: what is happening,
// and whether anyone hears about it when it stops.
func signalCell(s trafficSignal, ok bool) string {
	if !ok {
		return "-"
	}
	var decision string
	switch {
	case s.Alert != nil && *s.Alert:
		decision = "alert"
	case s.Alert != nil:
		decision = "ignored"
	case s.Proposed:
		decision = "offered"
	default:
		decision = "no alert"
	}
	return s.State + ", " + decision
}

func newSendersListCmd(app *App) *cobra.Command {
	var all, problems, offered bool
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List devices sending, or meant to send, syslog and flows",
		Long: `List devices sending syslog or flows, worst first.

SYSLOG and FLOW read as state, then the alert decision. The state is arriving,
stopped (quiet past its window) or never (nothing in the last week). The
decision is alert (a ticket opens when it stops), ignored, offered (heard
lately, nobody has decided) or no alert.

A device with no inventory host is listed by address: its traffic is tied to
no device, and no alert can be set on it until a host has that address.`,
		Example: `  ups senders list
  ups senders list --problems
  ups senders list --offered --id-only
  ups senders list --all-infras --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ids, err := app.trafficInfraIDs(all)
			if err != nil {
				return err
			}
			body, raw, err := app.getTraffic(ids)
			if err != nil {
				return err
			}
			cols := []string{"NAME", "ADDRESS", "STATUS", "SYSLOG", "FLOW"}
			if all {
				cols = append(cols, "INFRASTRUCTURE")
			}
			t := &output.Table{Columns: cols, Empty: "No device sends syslog or flows here, and none is expected to."}
			if !problems && !offered && app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			for i, d := range body.devices {
				if problems && d.Status != "bad" && d.Status != "warn" {
					continue
				}
				if offered && !deviceOffered(d) {
					continue
				}
				id := d.Address
				if d.Host != nil {
					id = strconv.Itoa(d.Host.ID)
				}
				sl, okS := d.Signals["syslog"]
				fl, okF := d.Signals["flow"]
				cells := []string{d.Name, d.Address, d.Status, signalCell(sl, okS), signalCell(fl, okF)}
				if all {
					cells = append(cells, d.Infrastructure.label())
				}
				t.Add(id, body.Devices[i], cells...)
			}
			if err := app.Printer.Print(t); err != nil {
				return err
			}
			if n := len(body.Proposals); n > 0 && !offered && !app.AsJSON && !app.IDOnly {
				app.Printer.Infof("%d host(s) send with no alert decided: ups senders accept, or ups senders ignore", n)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all-infras", false, "every infrastructure you can read, not just the active one")
	c.Flags().BoolVar(&problems, "problems", false, "only devices with a failing or warning check")
	c.Flags().BoolVar(&offered, "offered", false, "only hosts offered for an alert: heard lately, nobody has decided")
	return c
}

func deviceOffered(d trafficDevice) bool {
	if d.Host == nil {
		return false
	}
	for _, s := range d.Signals {
		if s.Proposed {
			return true
		}
	}
	return false
}

// findTrafficDevice matches a host id, host name or sender address. More than
// one match is refused: the same name or private address exists in other
// customers' infrastructures, and picking one shows the wrong device.
func findTrafficDevice(devices []trafficDevice, spec string) (trafficDevice, error) {
	var matches []trafficDevice
	for _, d := range devices {
		if d.Address == spec || d.Name == spec ||
			(d.Host != nil && (strconv.Itoa(d.Host.ID) == spec || d.Host.Name == spec)) {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 0:
		return trafficDevice{}, errs.NotFound("no device %q sends syslog or flows here, and none by that name is expected to", spec).
			WithHint("list them with: ups senders list")
	case 1:
		return matches[0], nil
	}
	where := make([]string, len(matches))
	for i, m := range matches {
		where[i] = m.Infrastructure.label()
	}
	return trafficDevice{}, errs.Conflict("%q matches %d devices (%s)", spec, len(matches), strings.Join(where, ", ")).
		WithHint("select one infrastructure with ups context set --infra-id <id>, or give the host id")
}

func newSendersShowCmd(app *App) *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "show <host|address>",
		Short: "Show one device's checks, receiver and the configuration that fixes it",
		Long: `Show one device: each protocol's state and alert, every check, the agent
receiving it (or the one to point it at) with the address and ports to send
to, and the configuration that fixes what failed, written for its platform.

The fix is a suggestion for the device, not something ups applies. Review it
before pasting it into a live device.`,
		Example: `  ups senders show core-sw-01
  ups senders show 10.20.0.1 --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ids, err := app.trafficInfraIDs(all)
			if err != nil {
				return err
			}
			body, _, err := app.getTraffic(ids)
			if err != nil {
				return err
			}
			d, err := findTrafficDevice(body.devices, args[0])
			if err != nil {
				return err
			}
			if app.AsJSON {
				for i, dev := range body.devices {
					if dev.Key == d.Key && dev.Infrastructure.ID == d.Infrastructure.ID {
						return app.Printer.Object(body.Devices[i], nil)
					}
				}
			}
			return app.renderTrafficDevice(d, body.Receivers)
		},
	}
	c.Flags().BoolVar(&all, "all-infras", false, "look in every infrastructure you can read")
	return c
}

// receiverFor is the agent a device sends to, or failing that the first one
// in its infrastructure, which is where the server points new senders too.
func receiverFor(d trafficDevice, receivers []trafficReceiver) (trafficReceiver, bool) {
	for _, s := range d.Signals {
		for _, id := range s.Receivers {
			for _, r := range receivers {
				if r.ID == id {
					return r, true
				}
			}
		}
	}
	for _, r := range receivers {
		if r.Infrastructure.ID == d.Infrastructure.ID {
			return r, true
		}
	}
	return trafficReceiver{}, false
}

func (a *App) renderTrafficDevice(d trafficDevice, receivers []trafficReceiver) error {
	theme := a.Theme()
	host := "not an inventory host"
	if d.Host != nil {
		host = fmt.Sprintf("%s (#%d)", d.Host.Name, d.Host.ID)
	}
	fields := [][2]string{
		{"Name", d.Name},
		{"Address", d.Address},
		{"Host", host},
		{"Infrastructure", d.Infrastructure.label()},
		{"Platform", dash(d.Platform)},
		{"Status", d.Status},
	}
	if r, ok := receiverFor(d, receivers); ok {
		running := ""
		if !r.Running {
			running = ", not running"
		}
		fields = append(fields, [2]string{"Receiver", fmt.Sprintf("%s at %s%s", r.Name, r.Address, running)})
		for _, l := range r.Listeners {
			fields = append(fields, [2]string{"  " + l.Protocol, l.Address})
		}
	} else {
		fields = append(fields, [2]string{"Receiver", "none: no agent in this infrastructure listens for syslog or flows"})
	}
	if err := a.Printer.Object(nil, fields); err != nil {
		return err
	}

	for _, kind := range trafficKinds {
		s, ok := d.Signals[kind]
		if !ok {
			continue
		}
		fmt.Fprintf(a.Stdout, "\n%s\n", theme.Bold.Apply(strings.ToUpper(kind[:1])+kind[1:]))
		rows := [][2]string{
			{"State", s.State},
			{"Alert", alertLabel(s)},
			{"Quiet window", fmt.Sprintf("%d min", s.WindowMinutes)},
			{"Last seen", dash(s.LastSeen)},
			{"Rate", fmt.Sprintf("%.1f a minute", s.Rate)},
			{"Last 24h / 7d", fmt.Sprintf("%d / %d", s.Last24h, s.Last7d)},
		}
		if len(s.Formats) > 0 {
			rows = append(rows, [2]string{"Formats", countsLabel(s.Formats)})
		}
		if len(s.Transports) > 0 {
			rows = append(rows, [2]string{"Transports", countsLabel(s.Transports)})
		}
		if err := a.Printer.Object(nil, rows); err != nil {
			return err
		}
	}

	if len(d.Checks) > 0 {
		fmt.Fprintf(a.Stdout, "\n%s\n", theme.Bold.Apply("Checks"))
		t := &output.Table{Columns: []string{"STATUS", "CHECK", "DETAIL"}}
		for _, c := range d.Checks {
			t.Add("", nil, c.Status, c.Title, c.Detail)
		}
		if err := a.Printer.Print(t); err != nil {
			return err
		}
	}
	if d.Fix != nil && d.Fix.Code != "" {
		fmt.Fprintf(a.Stdout, "\n%s\n%s\n", theme.Bold.Apply("Fix: "+d.Fix.Title), d.Fix.Code)
	}
	return nil
}

func alertLabel(s trafficSignal) string {
	switch {
	case s.Alert != nil && *s.Alert:
		return "on: a ticket opens when it stops"
	case s.Alert != nil:
		return "ignored"
	case s.Proposed:
		return "undecided, offered"
	}
	return "undecided"
}

func countsLabel(m map[string]int64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

// parseOnOff takes the words an operator types for a switch.
func parseOnOff(flag, v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "yes":
		return true, nil
	case "off", "false", "no", "ignore":
		return false, nil
	}
	return false, errs.Usage("invalid %s %q", flag, v).WithHint("use on or off")
}

// parseQuiet turns a duration into the whole minutes the server stores.
// "default" clears the host's own window, so the protocol default applies.
func parseQuiet(flag, v string) (any, error) {
	if strings.EqualFold(strings.TrimSpace(v), "default") {
		return nil, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Minute || d > trafficMaxQuiet || d%time.Minute != 0 {
		return nil, errs.Usage("invalid %s %q", flag, v).
			WithHint("use whole minutes between 1m and 168h, like 30m or 2h, or default")
	}
	return int(d / time.Minute), nil
}

func newSendersAlertCmd(app *App) *cobra.Command {
	var syslog, flow, syslogQuiet, flowQuiet string
	c := &cobra.Command{
		Use:   "alert <host>",
		Short: "Turn a host's syslog or flow alert on or off, or change its quiet window",
		Long: `Decide whether a ticket opens when a host stops sending syslog or flows.

--syslog and --flow take on or off. Off means ignored: the host is not offered
again, and nothing pages anyone when it goes quiet. Turning off an alert that
is on is a loss of monitoring coverage, so ups names it and asks first;
--yes answers that question, it does not skip it.

--syslog-quiet and --flow-quiet set how long the host may be quiet before a
ticket opens, in whole minutes up to 168h. default goes back to the protocol's
own window (1 hour for syslog, 5 minutes for flows).

This changes nothing on the device or the agent.`,
		Example: `  ups senders alert core-sw-01 --syslog on --flow on
  ups senders alert core-sw-01 --syslog-quiet 4h
  ups senders alert lab-fw-01 --flow off --yes`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			infra, err := app.trafficInfraIDs(false)
			if err != nil {
				return err
			}
			body := map[string]any{"infrastructure": infra[0]}
			for _, f := range []struct{ flag, key, value string }{
				{"syslog", "syslog", syslog}, {"flow", "flow", flow},
			} {
				if cmd.Flags().Changed(f.flag) {
					on, err := parseOnOff("--"+f.flag, f.value)
					if err != nil {
						return err
					}
					body[f.key] = on
				}
			}
			for _, f := range []struct{ flag, key, value string }{
				{"syslog-quiet", "syslog_silence_minutes", syslogQuiet},
				{"flow-quiet", "flow_silence_minutes", flowQuiet},
			} {
				if cmd.Flags().Changed(f.flag) {
					m, err := parseQuiet("--"+f.flag, f.value)
					if err != nil {
						return err
					}
					body[f.key] = m
				}
			}
			if len(body) == 1 {
				return errs.Usage("nothing to change").
					WithHint("pass --syslog on|off, --flow on|off, --syslog-quiet or --flow-quiet")
			}

			traffic, _, err := app.getTraffic(infra)
			if err != nil {
				return err
			}
			hostID, name, current, err := app.trafficAlertTarget(traffic.devices, strconv.Itoa(infra[0]), args[0])
			if err != nil {
				return err
			}
			body["host"] = hostID

			// Only an alert that is on now can be lost; switching off one
			// nobody turned on removes nothing.
			var losing []string
			for _, kind := range trafficKinds {
				if on, ok := body[kind].(bool); ok && !on && current[kind] {
					losing = append(losing, kind)
				}
			}
			if len(losing) > 0 && !app.DryRun {
				app.Printer.Infof("%s: turning off the %s alert. Nothing will page anyone when it stops sending %s.",
					name, strings.Join(losing, " and "), strings.Join(losing, " or "))
				if err := app.Confirm(fmt.Sprintf("Turn off %s alert on %s?", strings.Join(losing, " and "), name)); err != nil {
					return err
				}
			}

			var out jsonRaw
			if err := app.mutate("POST", trafficExpectPath, body, &out); err != nil || app.DryRun {
				return err
			}
			if app.AsJSON {
				return app.Printer.Object(out, nil)
			}
			app.Printer.Infof("updated traffic alerts on %s", name)
			return nil
		},
	}
	c.Flags().StringVar(&syslog, "syslog", "", "syslog alert: on or off")
	c.Flags().StringVar(&flow, "flow", "", "flow alert: on or off")
	c.Flags().StringVar(&syslogQuiet, "syslog-quiet", "", "how long syslog may be quiet before a ticket opens (e.g. 2h, or default)")
	c.Flags().StringVar(&flowQuiet, "flow-quiet", "", "how long flows may be quiet before a ticket opens (e.g. 15m, or default)")
	return c
}

// trafficAlertTarget finds the host to change and which alerts it has on. A
// host that sends nothing and expects nothing is not in the traffic list, so
// it is looked up in the inventory; it has no alert on to lose.
func (a *App) trafficAlertTarget(devices []trafficDevice, infra, spec string) (int, string, map[string]bool, error) {
	d, err := findTrafficDevice(devices, spec)
	if err == nil {
		if d.Host == nil {
			return 0, "", nil, errs.Usage("%s is not an inventory host, so no alert can be set on it", d.Address).
				WithHint("create a host with IP %s first: ups host create", d.Address)
		}
		on := map[string]bool{}
		for kind, s := range d.Signals {
			on[kind] = s.Alert != nil && *s.Alert
		}
		return d.Host.ID, d.Host.Name, on, nil
	}
	if errs.CodeOf(err) != errs.CodeNotFound {
		return 0, "", nil, err
	}
	id, err := a.flowHostID(infra, spec)
	if err != nil {
		return 0, "", nil, err
	}
	n, err := strconv.Atoi(id)
	if err != nil {
		return 0, "", nil, errs.General("host id %q is not numeric", id)
	}
	return n, spec, map[string]bool{}, nil
}

// newSendersDecideCmd builds accept and ignore, which settle every offer
// at once. They differ only in which way.
func newSendersDecideCmd(app *App, ignore bool) *cobra.Command {
	var all bool
	use, short, verb := "accept", "Turn alerts on for every host offered", "Turn on alerts for"
	long := `Turn alerts on for every host offered: heard from in the last week, with
nothing decided. From then on a ticket opens, and the infrastructure's
operation team is mailed, whenever one of them stays quiet past its window.

The hosts are listed before anything is written. The server settles the offers
it holds at the moment of writing, so a host first heard between the listing
and the write is included too.`
	if ignore {
		use, short, verb = "ignore", "Ignore every host offered, so none is alerted on or offered again", "Ignore"
		long = `Ignore every host offered: heard from in the last week, with nothing decided.
They are not offered again, and nothing pages anyone when they stop sending.
An alert already on is left alone; only undecided hosts change.

The hosts are listed before anything is written. The server settles the offers
it holds at the moment of writing, so a host first heard between the listing
and the write is included too. To undo one, ups senders alert <host>.`
	}
	c := &cobra.Command{
		Use:     use,
		Short:   short,
		Long:    long,
		Example: "  ups senders " + use + "\n  ups senders " + use + " --all-infras --yes",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ids, err := app.trafficInfraIDs(all)
			if err != nil {
				return err
			}
			traffic, _, err := app.getTraffic(ids)
			if err != nil {
				return err
			}
			var offers []string
			for _, d := range traffic.devices {
				if !deviceOffered(d) {
					continue
				}
				var kinds []string
				for _, kind := range trafficKinds {
					if s, ok := d.Signals[kind]; ok && s.Proposed {
						kinds = append(kinds, kind)
					}
				}
				offers = append(offers, fmt.Sprintf("%s (%s): %s", d.Host.Name, d.Address, strings.Join(kinds, ", ")))
			}
			if len(offers) == 0 {
				app.Printer.Infof("no host is offered: every sender's alerts are decided")
				return nil
			}
			app.Printer.Infof("%s %d host(s):", verb, len(offers))
			for _, o := range offers {
				app.Printer.Infof("  %s", o)
			}
			if !app.DryRun {
				if err := app.Confirm(fmt.Sprintf("%s %d host(s)?", verb, len(offers))); err != nil {
					return err
				}
			}

			body := map[string]any{"seen": true, "ignore": ignore}
			if ids == nil {
				for _, i := range traffic.Infrastructures {
					ids = append(ids, i.ID)
				}
			}
			body["infrastructures"] = ids
			var out jsonRaw
			if err := app.mutate("POST", trafficExpectPath, body, &out); err != nil || app.DryRun {
				return err
			}
			if app.AsJSON {
				return app.Printer.Object(out, nil)
			}
			var res struct {
				Changed int `json:"changed"`
			}
			_ = json.Unmarshal(out, &res)
			app.Printer.Infof("%d host(s) changed", res.Changed)
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all-infras", false, "every infrastructure you can read, not just the active one")
	return c
}
