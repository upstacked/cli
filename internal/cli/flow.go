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
ups warns about them on stderr.`,
	}
	c.AddCommand(newFlowConversationsCmd(app), newFlowConversationCmd(app), newFlowLinksCmd(app))
	return c
}

type flowQuery struct {
	window  string
	through []string
	query   string
}

func (a *App) flowQueryValues(f flowQuery) (url.Values, error) {
	infra, err := a.Resolved.RequireInfra()
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("hostgroups", infra)
	if f.window != "" {
		q.Set("window", f.window)
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
	if f.query != "" {
		q.Set("q", f.query)
	}
	return q, nil
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
	Window             string    `json:"window"`
	Links              []jsonRaw `json:"links"`
	Conversations      []jsonRaw `json:"conversations"`
	UnmatchedExporters []string  `json:"unmatched_exporters"`
}

func (a *App) getFlow(path string, q url.Values) (flowResponse, jsonRaw, error) {
	var body flowResponse
	_, raw, err := a.getOne(path, q)
	if err != nil {
		return body, nil, err
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return body, raw, errs.General("unexpected response from %s", path).Wrapping(err)
	}
	a.warnUnmatchedExporters(body.UnmatchedExporters)
	return body, raw, nil
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
	units := []string{"bps", "kbps", "Mbps", "Gbps", "Tbps"}
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

func conversationLabel(m row) string {
	return fmt.Sprintf("%s → %s:%s/%s", str(m, "client"), str(m, "server"),
		str(m, "service_port"), str(m, "transport"))
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

--through keeps conversations whose path passes through both hosts. --query
matches an IP, a port or a service name.`,
		Example: `  ups flow conversations
  ups flow conversations --window 1h --limit 20
  ups flow conversations --through core-sw-01,ot-sw-01
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
					conversationLabel(m), dash(str(m, "service_name")),
					bps(m, "request_bps"), bps(m, "response_bps"),
					pathSummary(names, hops(m, "request_path")), asym)
			}
			return app.Printer.Print(t)
		},
	}
	addFlowFlags(c, &f)
	c.Flags().StringSliceVar(&f.through, "through", nil, "only conversations passing through both hosts (two names or ids)")
	c.Flags().StringVar(&f.query, "query", "", "match an IP, port or service name")
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
reversed.`,
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
				{"Conversation", conversationLabel(m)},
				{"Service", dash(str(m, "service_name"))},
				{"Request", bps(m, "request_bps")},
				{"Response", bps(m, "response_bps")},
				{"Asymmetric", yesNo(m["asymmetric"])},
			}); err != nil {
				return err
			}
			names := app.hostNames(q.Get("hostgroups"))
			for _, p := range []struct{ title, key string }{
				{"Request path", "request_path"},
				{"Response path", "response_path"},
			} {
				app.Printer.Printf("\n%s", p.title)
				if err := app.Printer.Print(hopTable(names, hops(m, p.key))); err != nil {
					return err
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
export, each direction uses the larger reading rather than the sum.`,
		Example: `  ups flow links
  ups flow links --window 1h --json`,
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
	return c
}

// The server validates --window; checking it here too would only drift from
// the set it accepts.
func addFlowFlags(c *cobra.Command, f *flowQuery) {
	c.Flags().StringVar(&f.window, "window", "15m", "time window: 5m, 15m or 1h")
}
