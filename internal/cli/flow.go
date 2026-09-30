package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/output"
)

const (
	flowLinksPath         = "/api/topology/flow/"
	flowConversationsPath = "/api/topology/flow/conversations/"
	flowSummaryPath       = "/api/flow/summary/"
)

func newFlowCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "flow",
		Short: "Show flow traffic on the topology",
		Long: `Show NetFlow/IPFIX/sFlow traffic laid over the topology.

A flow record names the device that exported it and the interfaces the
traffic entered and left on. Matching those interfaces to topology links
gives the traffic on each link, and chaining them across devices gives the
path a conversation takes.

An exporter is matched to a host by its management IP. A device that sends
flow from any other address (a loopback, an uplink) matches no host, its
records drop out of every link and every path, and the answer comes back
short with nothing in it looking wrong. The server lists those exporters and
ups warns about them on stderr.

Monitoring traffic (SNMP polls and traps, syslog, flow export, ICMP) is left
out by default: it is the monitoring watching itself, and on a busy network
it crowds out everything else. --include-monitoring brings it back.`,
	}
	c.AddCommand(newFlowSummaryCmd(app), newFlowConversationsCmd(app),
		newFlowConversationCmd(app), newFlowLinksCmd(app))
	return c
}

type flowQuery struct {
	window            string
	start             string
	end               string
	through           []string
	seenBy            string
	query             string
	service           string
	host              string
	includeMonitoring bool
}

func (a *App) flowQueryValues(f flowQuery) (url.Values, error) {
	infra, err := a.Resolved.RequireInfra()
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("hostgroups", infra)
	// The server ignores the window next to an explicit range; sending the
	// default anyway would make the request read as if both applied.
	if f.start != "" || f.end != "" {
		setIf(q, "start", f.start)
		setIf(q, "end", f.end)
	} else {
		setIf(q, "window", f.window)
	}
	if len(f.through) > 0 {
		if len(f.through) != 2 {
			return nil, errs.Usage("--through takes exactly two hosts, got %d", len(f.through)).
				WithHint("ups flow conversations --through core-sw-01,core-sw-02")
		}
		ids := make([]string, 0, 2)
		for _, h := range f.through {
			id, err := a.flowHostID(infra, strings.TrimSpace(h))
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		q.Set("through", strings.Join(ids, ","))
	}
	if f.seenBy != "" {
		id, err := a.flowHostID(infra, strings.TrimSpace(f.seenBy))
		if err != nil {
			return nil, err
		}
		q.Set("seen_by", id)
	}
	setIf(q, "q", f.query)
	setIf(q, "service", f.service)
	setIf(q, "host", f.host)
	if f.includeMonitoring {
		q.Set("include_monitoring", "true")
	}
	return q, nil
}

func setIf(q url.Values, key, value string) {
	if value != "" {
		q.Set(key, value)
	}
}

// flowHostID takes an id as given and resolves anything else as a name,
// scoped to the infrastructure so a name shared with another customer's
// device cannot be picked.
func (a *App) flowHostID(infra, host string) (string, error) {
	if _, err := strconv.Atoi(host); err == nil {
		return host, nil
	}
	return a.hostIDByName(infra, host)
}

// hostNames maps host ids to names for rendering paths. A failed lookup
// degrades to "#<id>" rather than failing a read-only command.
func (a *App) hostNames(infra string) map[string]string {
	names := map[string]string{}
	rows, err := a.fetchRows("/api/host/", url.Values{"infrastructure": {infra}})
	if err != nil {
		return names
	}
	for _, m := range rows {
		names[str(m, "id")] = str(m, "name")
	}
	return names
}

func hostLabel(names map[string]string, id string) string {
	if n := names[id]; n != "" {
		return n
	}
	if id == "" {
		return "-"
	}
	return "#" + id
}

type flowResponse struct {
	Window             string              `json:"window"`
	Links              []jsonRaw           `json:"links"`
	Conversations      []jsonRaw           `json:"conversations"`
	Names              map[string]flowName `json:"names"`
	UnmatchedExporters []string            `json:"unmatched_exporters"`
}

// flowName is what the server knows an IP as: a host or IPAM name, or for a
// public address the organisation that owns it.
type flowName struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	HostID any    `json:"host_id"`
	Org    string `json:"org"`
}

// ipLabel keeps the IP visible next to any name: a name is a hint, and the
// address is what the caller can act on or search for.
func ipLabel(names map[string]flowName, ip string) string {
	n := names[ip]
	switch {
	case ip == "":
		return "-"
	case n.Name != "":
		return fmt.Sprintf("%s (%s)", n.Name, ip)
	case n.Org != "":
		return fmt.Sprintf("%s · %s", n.Org, ip)
	}
	return ip
}

func (a *App) getFlow(path string, q url.Values) (flowResponse, jsonRaw, error) {
	var body flowResponse
	raw, err := a.getFlowInto(path, q, &body)
	if err == nil {
		a.warnUnmatchedExporters(body.UnmatchedExporters)
	}
	return body, raw, err
}

func (a *App) getFlowInto(path string, q url.Values, body any) (jsonRaw, error) {
	_, raw, err := a.getOne(path, q)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, body); err != nil {
		return raw, errs.General("unexpected response from %s", path).Wrapping(err)
	}
	return raw, nil
}

// warnUnmatchedExporters goes to stderr even with --json: an answer missing
// a device's traffic looks complete, and the caller needs to know it is not.
func (a *App) warnUnmatchedExporters(ips []string) {
	if len(ips) == 0 {
		return
	}
	a.Printer.Infof("warning: flow from %s matched no host, so its traffic is missing from this answer",
		strings.Join(ips, ", "))
	a.Printer.Infof("set the device's flow export source to its management IP (the host's IP in ups host list)")
}

func formatBps(v float64) string {
	return scaled(v, []string{"bps", "kbps", "Mbps", "Gbps", "Tbps"})
}

func formatBytes(v float64) string {
	return scaled(v, []string{"B", "kB", "MB", "GB", "TB"})
}

func scaled(v float64, units []string) string {
	u := 0
	for v >= 1000 && u < len(units)-1 {
		v /= 1000
		u++
	}
	if v < 10 && u > 0 {
		return fmt.Sprintf("%.1f %s", v, units[u])
	}
	return fmt.Sprintf("%.0f %s", v, units[u])
}

func bps(m row, key string) string {
	v, ok := m[key].(float64)
	if !ok {
		return "-"
	}
	return formatBps(v)
}

func hops(m row, key string) []row {
	items, _ := m[key].([]any)
	out := make([]row, 0, len(items))
	for _, it := range items {
		if h, ok := it.(map[string]any); ok {
			out = append(out, h)
		}
	}
	return out
}

// pathSummary names each hop, bracketing the ones no device exported: their
// place on the path is inferred from a neighbour's interface.
func pathSummary(names map[string]string, path []row) string {
	parts := make([]string, 0, len(path))
	for _, h := range path {
		name := hostLabel(names, str(h, "host"))
		if b, ok := h["observed"].(bool); ok && !b {
			name = "(" + name + ")"
		}
		parts = append(parts, name)
	}
	return dash(strings.Join(parts, " > "))
}

func conversationLabel(names map[string]flowName, m row) string {
	return fmt.Sprintf("%s → %s:%s/%s", ipLabel(names, str(m, "client")),
		ipLabel(names, str(m, "server")), str(m, "service_port"), str(m, "transport"))
}

// gapReasons say why a direction's path stops short, and what fixes it. The
// path itself only shows where it stops; without the reason the natural
// conclusion is that the traffic went nowhere else.
var gapReasons = map[string]struct{ what, fix string }{
	"exporter_unmatched": {
		"flow from %[2]s matched no host",
		"set the device's flow export source to its management IP",
	},
	"no_interface_table": {
		"%[1]s has no interface table, so its ports are unknown",
		"store its ifName/ifDescr (ups host walk <id> ifName ifDescr) as its interface table",
	},
	"unknown_interface": {
		"%[1]s's interface table has no ifIndex %[3]s",
		"refresh its interface table; the device has added or renumbered interfaces",
	},
}

func gapLines(names map[string]string, gaps []row) []string {
	out := make([]string, 0, len(gaps))
	for _, g := range gaps {
		reason := str(g, "reason")
		r, ok := gapReasons[reason]
		if !ok {
			out = append(out, reason)
			continue
		}
		host := hostLabel(names, str(g, "host"))
		out = append(out, fmt.Sprintf(r.what, host, dash(str(g, "ip")), dash(str(g, "if_index")))+
			" - "+strings.ReplaceAll(r.fix, "<id>", dash(str(g, "host"))))
	}
	return out
}

func newFlowConversationsCmd(app *App) *cobra.Command {
	var f flowQuery
	c := &cobra.Command{
		Use:     "conversations",
		Aliases: []string{"ls", "list"},
		Short:   "List the busiest conversations and the path each takes",
		Long: `List the busiest conversations in a time window.

A conversation is client, server, server port and transport, with both
directions merged; the client's ephemeral port is dropped so one session is
one row. PATH lists the devices on the request path. A device in brackets
exports no flow: its place is inferred from its neighbour's interface.

--through keeps conversations whose path passes through both hosts. --seen-by
keeps what one device exported. --query matches an IP, a port or a service
name; --service and --host match one exactly.

Monitoring traffic is left out unless --include-monitoring is given.`,
		Example: `  ups flow conversations
  ups flow conversations --window 1h --limit 20
  ups flow conversations --through core-sw-01,ot-sw-01
  ups flow conversations --seen-by core-sw-01 --service 'https (TCP/443)'
  ups flow conversations --start 2026-09-30T11:00:00Z --end 2026-09-30T11:30:00Z
  ups flow conversations --query 10.20.0.45 --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			q, err := app.flowQueryValues(f)
			if err != nil {
				return err
			}
			if app.Limit > 0 {
				q.Set("size", strconv.Itoa(app.Limit))
			}
			body, raw, err := app.getFlow(flowConversationsPath, q)
			if err != nil {
				return err
			}
			if app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			names := app.hostNames(q.Get("hostgroups"))
			t := &output.Table{
				Columns: []string{"CONVERSATION", "SERVICE", "REQUEST", "RESPONSE", "PATH", "ASYMMETRIC"},
				Empty:   "No conversations in the window. Flow only covers devices that export it.",
			}
			for i, m := range decodeRows(body.Conversations) {
				asym := "no"
				if b, ok := m["asymmetric"].(bool); ok && b {
					asym = "yes"
				}
				t.Add(str(m, "id"), body.Conversations[i],
					conversationLabel(body.Names, m), dash(str(m, "service_name")),
					bps(m, "request_bps"), bps(m, "response_bps"),
					pathSummary(names, hops(m, "request_path")), asym)
			}
			return app.Printer.Print(t)
		},
	}
	addFlowFlags(c, &f)
	addFlowFilterFlags(c, &f)
	c.Flags().StringSliceVar(&f.through, "through", nil, "only conversations passing through both hosts (two names or ids)")
	return c
}

func newFlowConversationCmd(app *App) *cobra.Command {
	var f flowQuery
	c := &cobra.Command{
		Use:   "conversation <id>",
		Short: "Show one conversation hop by hop",
		Long: `Show one conversation's request and response paths, hop by hop.

The id is the CONVERSATION id from "ups flow conversations --id-only", in the
form client|server|port|transport. Quote it: | is a shell pipe.

When the response path differs from the request path (ECMP, asymmetric
routing), both are shown; otherwise the response path is the request path
reversed. When a path stops short, the reason is listed under it with what
fixes it.`,
		Example: `  ups flow conversation '10.10.2.57|52.114.7.20|443|tcp'`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q, err := app.flowQueryValues(f)
			if err != nil {
				return err
			}
			q.Set("conversation", args[0])
			body, _, err := app.getFlow(flowConversationsPath, q)
			if err != nil {
				return err
			}
			if len(body.Conversations) == 0 {
				return errs.NotFound("no conversation %q in the last %s", args[0], dash(body.Window)).
					WithHint("widen the window with --window 1h, or list ids with ups flow conversations --id-only")
			}
			raw := body.Conversations[0]
			if app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			var m row
			if err := json.Unmarshal(raw, &m); err != nil {
				return errs.General("unexpected conversation in response").Wrapping(err)
			}
			if err := app.Printer.Object(raw, [][2]string{
				{"Conversation", conversationLabel(body.Names, m)},
				{"Service", dash(str(m, "service_name"))},
				{"Request", bps(m, "request_bps")},
				{"Response", bps(m, "response_bps")},
				{"Asymmetric", yesNo(m["asymmetric"])},
			}); err != nil {
				return err
			}
			names := app.hostNames(q.Get("hostgroups"))
			for _, p := range []struct{ title, key, gaps string }{
				{"Request path", "request_path", "request_gaps"},
				{"Response path", "response_path", "response_gaps"},
			} {
				app.Printer.Printf("\n%s", p.title)
				if err := app.Printer.Print(hopTable(names, hops(m, p.key))); err != nil {
					return err
				}
				for _, line := range gapLines(names, hops(m, p.gaps)) {
					app.Printer.Printf("  gap: %s", line)
				}
			}
			return nil
		},
	}
	addFlowFlags(c, &f)
	return c
}

func hopTable(names map[string]string, path []row) *output.Table {
	t := &output.Table{
		Columns: []string{"HOP", "DEVICE", "IN", "OUT", "RATE", "SEEN BY"},
		Empty:   "No hops recorded.",
	}
	for i, h := range path {
		seen := "exported"
		if b, ok := h["observed"].(bool); ok && !b {
			seen = "inferred"
		}
		t.Add(strconv.Itoa(i+1), nil,
			strconv.Itoa(i+1), hostLabel(names, str(h, "host")),
			dash(str(h, "in_port")), dash(str(h, "out_port")), bps(h, "bps"), seen)
	}
	return t
}

func newFlowLinksCmd(app *App) *cobra.Command {
	var f flowQuery
	c := &cobra.Command{
		Use:   "links",
		Short: "Show traffic on each topology link",
		Long: `Show the traffic each topology link carried in a time window.

Links no exporter reported on are left out, so a link missing here carried
no traffic that anyone exported - not necessarily no traffic. When both ends
export, each direction uses the larger reading rather than the sum.

Monitoring traffic is left out unless --include-monitoring is given, so a
link's figure here can be lower than its interface counters.`,
		Example: `  ups flow links
  ups flow links --window 1h --json
  ups flow links --include-monitoring`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			q, err := app.flowQueryValues(f)
			if err != nil {
				return err
			}
			body, raw, err := app.getFlow(flowLinksPath, q)
			if err != nil {
				return err
			}
			if app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			infra := q.Get("hostgroups")
			names := app.hostNames(infra)
			links := map[string]row{}
			if rows, err := app.fetchRows("/api/host_link/", url.Values{"infrastructure": {infra}}); err == nil {
				for _, l := range rows {
					links[str(l, "id")] = l
				}
			}
			t := &output.Table{
				Columns: []string{"LINK", "FROM", "FROM PORT", "TO", "TO PORT", "→", "←"},
				Empty:   "No link carried exported flow in the window.",
			}
			for i, m := range decodeRows(body.Links) {
				id := str(m, "link")
				l := links[id]
				t.Add(id, body.Links[i], id,
					hostLabel(names, str(l, "source_node")), dash(str(l, "source_port_name")),
					hostLabel(names, str(l, "destination_node")), dash(str(l, "destination_port_name")),
					bps(m, "source_to_destination_bps"), bps(m, "destination_to_source_bps"))
			}
			return app.Printer.Print(t)
		},
	}
	addFlowFlags(c, &f)
	addMonitoringFlag(c, &f)
	return c
}

type flowSummary struct {
	Range struct {
		Start    string `json:"start"`
		End      string `json:"end"`
		Interval string `json:"interval"`
	} `json:"range"`
	Totals struct {
		Bytes                 float64 `json:"bytes"`
		Packets               float64 `json:"packets"`
		HiddenMonitoringBytes float64 `json:"hidden_monitoring_bytes"`
	} `json:"totals"`
	Services           []row               `json:"services"`
	Hosts              []row               `json:"hosts"`
	Names              map[string]flowName `json:"names"`
	UnmatchedExporters []string            `json:"unmatched_exporters"`
}

func newFlowSummaryCmd(app *App) *cobra.Command {
	f := flowQuery{window: "1h"}
	c := &cobra.Command{
		Use:   "summary",
		Short: "Show total traffic and the top services and hosts",
		Long: `Show an infrastructure's flow traffic: the totals, the busiest services and
the busiest hosts, with the names the server knows them by.

Monitoring traffic is left out unless --include-monitoring is given; the
amount left out is shown, so a small total is not mistaken for a quiet
network.`,
		Example: `  ups flow summary
  ups flow summary --window 24h
  ups flow summary --seen-by core-sw-01 --include-monitoring
  ups flow summary --host 10.30.100.15 --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			q, err := app.flowQueryValues(f)
			if err != nil {
				return err
			}
			var body flowSummary
			raw, err := app.getFlowInto(flowSummaryPath, q, &body)
			if err != nil {
				return err
			}
			app.warnUnmatchedExporters(body.UnmatchedExporters)
			if app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			monitoring := "shown"
			if !f.includeMonitoring {
				monitoring = formatBytes(body.Totals.HiddenMonitoringBytes) + " left out"
			}
			if err := app.Printer.Object(raw, [][2]string{
				{"Range", fmt.Sprintf("%s - %s", dash(body.Range.Start), dash(body.Range.End))},
				{"Traffic", formatBytes(body.Totals.Bytes)},
				{"Packets", strconv.FormatFloat(body.Totals.Packets, 'f', 0, 64)},
				{"Monitoring", monitoring},
			}); err != nil {
				return err
			}
			services := &output.Table{
				Columns: []string{"SERVICE", "BYTES", "PACKETS"},
				Empty:   "No traffic in the range.",
			}
			for _, s := range body.Services {
				services.Add(str(s, "service"), nil, dash(str(s, "service")),
					formatBytes(num(s, "bytes")), dash(str(s, "packets")))
			}
			hosts := &output.Table{
				Columns: []string{"HOST", "ROLE", "BYTES", "PACKETS"},
				Empty:   "No traffic in the range.",
			}
			for _, h := range body.Hosts {
				hosts.Add(str(h, "ip"), nil, ipLabel(body.Names, str(h, "ip")),
					dash(str(h, "role")), formatBytes(num(h, "bytes")), dash(str(h, "packets")))
			}
			for _, t := range []struct {
				title string
				table *output.Table
			}{{"Top services", services}, {"Top hosts", hosts}} {
				app.Printer.Printf("\n%s", t.title)
				if err := app.Printer.Print(t.table); err != nil {
					return err
				}
			}
			return nil
		},
	}
	addFlowFlags(c, &f)
	addFlowFilterFlags(c, &f)
	return c
}

func num(m row, key string) float64 {
	v, _ := m[key].(float64)
	return v
}

// The server validates --window and the range; checking them here too would
// only drift from what it accepts.
func addFlowFlags(c *cobra.Command, f *flowQuery) {
	window := f.window
	if window == "" {
		window = "15m"
	}
	c.Flags().StringVar(&f.window, "window", window, "time window, e.g. 15m, 1h, 6h or 24h")
}

func addMonitoringFlag(c *cobra.Command, f *flowQuery) {
	c.Flags().BoolVar(&f.includeMonitoring, "include-monitoring", false,
		"include SNMP, traps, syslog, flow export and ICMP (left out by default)")
}

func addFlowFilterFlags(c *cobra.Command, f *flowQuery) {
	addMonitoringFlag(c, f)
	c.Flags().StringVar(&f.start, "start", "", "start of an explicit range (RFC 3339), instead of --window")
	c.Flags().StringVar(&f.end, "end", "", "end of an explicit range (RFC 3339), instead of --window")
	c.Flags().StringVar(&f.seenBy, "seen-by", "", "only what this host exported (name or id)")
	c.Flags().StringVar(&f.query, "query", "", "match an IP, port or service name")
	c.Flags().StringVar(&f.service, "service", "", "exactly this service, e.g. 'https (TCP/443)'")
	c.Flags().StringVar(&f.host, "host", "", "only conversations with this IP as client or server")
}
