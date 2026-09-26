package cli

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
)

func newHostCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "host",
		Aliases: []string{"device"},
		Short:   "Manage devices (hosts)",
		Long: `Hosts are the devices that get monitored.

A host is not an asset: an asset is the procurement and ownership record.
They can be linked, but they have separate lifecycles.`,
	}
	c.AddCommand(
		newHostListCmd(app), newHostShowCmd(app), newHostCreateCmd(app),
		newHostUpdateCmd(app),
		newHostDeleteCmd(app), newHostTraceCmd(app), newHostLinksCmd(app),
		newHostWalkCmd(app),
	)
	return c
}

func newHostListCmd(app *App) *cobra.Command {
	var search string
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List hosts in the active infrastructure",
		RunE: func(cmd *cobra.Command, args []string) error {
			q := app.infraQuery(nil)
			if search != "" {
				q.Set("search", search)
			}
			return app.runList(listOpts{
				Path:    "/api/host/",
				Query:   q,
				Columns: []string{"ID", "NAME", "HOSTNAME", "IP", "VENDOR", "MODEL"},
				Empty:   "No hosts found. Check the active infrastructure: ups context show",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "name")), dash(str(m, "i_hostname")),
						dash(str(m, "i_ip_address")), dash(str(m, "i_vendor", "host_vendor")),
						dash(str(m, "i_model", "host_model")),
					}
				},
			})
		},
	}
	c.Flags().StringVarP(&search, "search", "s", "", "free-text search")
	return c
}

func newHostShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one host",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, raw, err := app.getOne("/api/host/"+args[0]+"/", nil)
			if err != nil {
				return err
			}
			if app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			return app.Printer.Object(raw, [][2]string{
				{"ID", str(m, "id")},
				{"Name", dash(str(m, "name"))},
				{"Infrastructure", dash(str(m, "infrastructure_name", "infrastructure"))},
				{"Hostname", dash(str(m, "i_hostname"))},
				{"IP", dash(str(m, "i_ip_address"))},
				{"MAC", dash(str(m, "i_mac_address"))},
				{"Type", dash(str(m, "i_type", "node_type"))},
				{"Hardware", dash(str(m, "i_hardware"))},
				{"Serial", dash(str(m, "i_serial"))},
				{"Asset", dash(str(m, "asset"))},
				{"In monitoring", yesNo(m["in_monitoring"])},
				{"Monitoring template", dash(str(m, "monitoring_template_name", "monitoring_template"))},
			})
		},
	}
}

func newHostCreateCmd(app *App) *cobra.Command {
	var name, hostname, ip, mac, infra string
	c := &cobra.Command{
		Use:   "create",
		Short: "Add a device",
		Example: `  ups host create --name core-sw-01 --ip 10.0.0.1
  ups host create --name fw-01 --hostname fw01.corp --infra 42`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return errs.Usage("--name is required")
			}
			target := infra
			if target == "" {
				var err error
				if target, err = app.Resolved.RequireInfra(); err != nil {
					return err
				}
			}
			body := map[string]any{"name": name, "infrastructure": atoiOr(target)}
			addIf(body, "i_hostname", hostname)
			addIf(body, "i_ip_address", ip)
			addIf(body, "i_mac_address", mac)

			var raw jsonRaw
			if err := app.create("/api/host/", body, &raw); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			var m row
			_ = jsonUnmarshal(raw, &m)
			t, sym := app.Theme(), app.Sym()
			fmt.Fprintf(app.Stderr, "%s Created host %s (%s)\n",
				t.Green.Apply(sym.OK), dash(str(m, "name")), str(m, "id"))
			fmt.Fprintf(app.Stderr, "  %s add monitoring: ups monitoring item create --host %s\n",
				t.Dim.Apply("next:"), str(m, "id"))
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "host name (required)")
	c.Flags().StringVar(&hostname, "hostname", "", "DNS hostname")
	c.Flags().StringVar(&ip, "ip", "", "IP address")
	c.Flags().StringVar(&mac, "mac", "", "MAC address")
	c.Flags().StringVar(&infra, "infra-id", "", "infrastructure id (defaults to the active context)")
	return c
}

func newHostUpdateCmd(app *App) *cobra.Command {
	var name, hostname, ip, mac string
	var monitoring bool

	c := &cobra.Command{
		Use:   "update <id>",
		Short: "Change a device, or put it in and out of monitoring",
		Long: `Change a device's details, or whether it is monitored at all.

--monitoring is the switch the agent obeys: a host that is not in monitoring
is left out of the payload the agent polls, so every item on it silently
collects nothing. Building monitoring for a device ends here.

--monitoring=false takes it back out, which stops every check on the host
without deleting anything - and without alerting anyone.`,
		Example: `  ups host update 205 --monitoring
  ups host update 205 --ip 10.30.100.8 --hostname border.lab`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{}
			addIf(body, "name", name)
			addIf(body, "i_hostname", hostname)
			addIf(body, "i_ip_address", ip)
			addIf(body, "i_mac_address", mac)
			if cmd.Flags().Changed("monitoring") {
				body["in_monitoring"] = monitoring
			}
			if len(body) == 0 {
				return errs.Usage("nothing to change").
					WithHint("pass --monitoring, --name, --hostname, --ip or --mac")
			}
			if cmd.Flags().Changed("monitoring") && !monitoring {
				m, _, err := app.getOne("/api/host/"+args[0]+"/", nil)
				if err != nil {
					return err
				}
				if err := app.Confirm(fmt.Sprintf(
					"Take host %s (%s) out of monitoring? Every check on it stops, silently.",
					args[0], dash(str(m, "name")))); err != nil {
					return err
				}
			}
			if err := app.mutate("PATCH", "/api/host/"+args[0]+"/", body, nil); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			t := app.Theme()
			app.Printer.Infof("%s Updated host %s", app.Sym().OK, args[0])
			if v, ok := body["in_monitoring"].(bool); ok && v {
				fmt.Fprintf(app.Stderr, "  %s the agent picks its items up on their next interval: ups monitoring schema data %s <schema-id>\n",
					t.Dim.Apply("next:"), args[0])
			}
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "host name")
	c.Flags().StringVar(&hostname, "hostname", "", "DNS hostname")
	c.Flags().StringVar(&ip, "ip", "", "IP address")
	c.Flags().StringVar(&mac, "mac", "", "MAC address")
	c.Flags().BoolVar(&monitoring, "monitoring", false, "put the device in monitoring (--monitoring=false takes it out)")
	return c
}

func newHostDeleteCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a host",
		Long: `Delete a host.

Deleting a host removes its monitoring. Nothing pages anyone when monitoring
disappears - it simply stops watching - so this asks for confirmation.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, _, err := app.getOne("/api/host/"+args[0]+"/", nil)
			if err != nil {
				return err
			}
			if err := app.Confirm(fmt.Sprintf(
				"Delete host %s (%s) and its monitoring?", args[0], str(m, "name"))); err != nil {
				return err
			}
			if err := app.mutate("DELETE", "/api/host/"+args[0]+"/", nil, nil); err != nil {
				return err
			}
			if !app.DryRun {
				app.Printer.Infof("Deleted host %s.", args[0])
			}
			return nil
		},
	}
}

func newHostTraceCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "trace <host-id>",
		Short: "Trace the network path to a host",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			infra, err := app.Resolved.RequireInfra()
			if err != nil {
				return err
			}
			_, raw, err := app.getOne(
				fmt.Sprintf("/api/infrastructure/%s/hosts/%s/trace/", infra, args[0]), nil)
			if err != nil {
				return err
			}
			return app.Printer.Object(raw, nil)
		},
	}
}

func newHostLinksCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "links",
		Short: "Manage topology links between hosts",
		Long: `Topology links are the edges of the map: which port on one host reaches
which port on another.

Links are versioned by the infrastructure's topology revision. The server
stamps the current revision onto a link as it is created, and the portal's
topology view only draws links matching that revision - so a link created
against an older revision exists but is invisible.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runHostLinksList(app) },
	}
	c.AddCommand(newHostLinksListCmd(app), newHostLinksCreateCmd(app), newHostLinksDeleteCmd(app))
	return c
}

func newHostLinksListCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List topology links between hosts",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runHostLinksList(app) },
	}
}

func runHostLinksList(app *App) error {
	return app.runList(listOpts{
		Path:    "/api/host_link/",
		Query:   app.infraQuery(nil),
		Columns: []string{"ID", "NAME", "FROM", "FROM PORT", "TO", "TO PORT", "LAYER", "REV"},
		Empty:   "No host links recorded.",
		Cells: func(m row) []string {
			return []string{
				str(m, "id"), dash(str(m, "name")),
				dash(str(m, "source_node")), dash(str(m, "source_port_name")),
				dash(str(m, "destination_node")), dash(str(m, "destination_port_name")),
				linkLayers(m), dash(str(m, "revison_number")),
			}
		},
	})
}

// linkLayers renders the three layer booleans as the compact "L1,L2" form the
// portal uses. A link can sit on more than one layer at once.
func linkLayers(m row) string {
	var on []string
	for i, key := range []string{"layer_one", "layer_two", "layer_three"} {
		if b, ok := m[key].(bool); ok && b {
			on = append(on, fmt.Sprintf("L%d", i+1))
		}
	}
	return dash(strings.Join(on, ","))
}

func newHostLinksCreateCmd(app *App) *cobra.Command {
	var from, to, fromPort, toPort, name, layers, infra string
	c := &cobra.Command{
		Use:   "create",
		Short: "Record a topology link between two hosts",
		Long: `Record a topology link between two hosts.

--from and --to are host ids, not names: the same hostname exists in many
customers' infrastructures, and resolving one here would be a way to draw an
edge on the wrong customer's map.

The link's name defaults to the source port, which is what the portal labels
the edge with.`,
		Example: `  ups host links create --from 12 --to 19 --from-port Gi1/0/1 --to-port Gi1/0/24
  ups host links create --from 12 --to 19 --name uplink --layer 1,2`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if from == "" || to == "" {
				return errs.Usage("--from and --to are required")
			}
			if from == to {
				return errs.Usage("--from and --to are the same host (%s)", from)
			}
			if name == "" {
				name = fromPort
			}
			if name == "" {
				return errs.Usage("--name is required when --from-port is not given")
			}
			target := infra
			if target == "" {
				var err error
				if target, err = app.Resolved.RequireInfra(); err != nil {
					return err
				}
			}
			body := map[string]any{
				"name":             name,
				"infrastructure":   atoiOr(target),
				"source_node":      atoiOr(from),
				"destination_node": atoiOr(to),
			}
			addIf(body, "source_port_name", fromPort)
			addIf(body, "destination_port_name", toPort)
			l, err := parseLinkLayers(layers)
			if err != nil {
				return err
			}
			for k, v := range l {
				body[k] = v
			}

			var raw jsonRaw
			// revison_number is deliberately not sent: the server stamps the
			// infrastructure's current topology revision, and a link carrying
			// any other value is invisible on the map.
			if err := app.create("/api/host_link/", body, &raw); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			var m row
			_ = jsonUnmarshal(raw, &m)
			t, sym := app.Theme(), app.Sym()
			fmt.Fprintf(app.Stderr, "%s Linked host %s to host %s (link %s)\n",
				t.Green.Apply(sym.OK), from, to, str(m, "id"))
			return nil
		},
	}
	c.Flags().StringVar(&from, "from", "", "source host id (required)")
	c.Flags().StringVar(&to, "to", "", "destination host id (required)")
	c.Flags().StringVar(&fromPort, "from-port", "", "port name on the source host")
	c.Flags().StringVar(&toPort, "to-port", "", "port name on the destination host")
	c.Flags().StringVar(&name, "name", "", "link label (defaults to --from-port)")
	c.Flags().StringVar(&layers, "layer", "1", "OSI layers this link carries: 1, 2, 3, or a comma-separated list")
	c.Flags().StringVar(&infra, "infra-id", "", "infrastructure id (defaults to the active context)")
	return c
}

// parseLinkLayers turns --layer into the three booleans the API stores. The
// API has no validation here, so a typo would otherwise create a link on no
// layer at all, which draws nothing and looks like the create silently failed.
func parseLinkLayers(s string) (map[string]any, error) {
	out := map[string]any{"layer_one": false, "layer_two": false, "layer_three": false}
	names := map[string]string{"1": "layer_one", "2": "layer_two", "3": "layer_three"}
	matched := false
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(part)), "l"))
		if part == "" {
			continue
		}
		key, ok := names[part]
		if !ok {
			return nil, errs.Usage("--layer accepts 1, 2 or 3, got %q", part)
		}
		out[key] = true
		matched = true
	}
	if !matched {
		return nil, errs.Usage("--layer needs at least one of 1, 2 or 3")
	}
	return out, nil
}

func newHostLinksDeleteCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>...",
		Short: "Remove topology links",
		Long: `Remove topology links.

This removes edges from the map. It does not touch the hosts themselves or
their monitoring.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			what := fmt.Sprintf("Delete host link %s?", args[0])
			if len(args) > 1 {
				what = fmt.Sprintf("Delete %d host links (%s)?", len(args), strings.Join(args, ", "))
			}
			if err := app.Confirm(what); err != nil {
				return err
			}
			ids := make([]any, 0, len(args))
			for _, a := range args {
				ids = append(ids, atoiOr(a))
			}
			if err := app.mutate("DELETE", "/api/host_link_bulk_delete/",
				map[string]any{"ids": ids}, nil); err != nil {
				return err
			}
			if !app.DryRun {
				app.Printer.Infof("Deleted %d host link(s).", len(args))
			}
			return nil
		},
	}
}

func addIf(m map[string]any, key, value string) {
	if value != "" {
		m[key] = value
	}
}

func atoiOr(s string) any {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil {
		return n
	}
	return s
}

var _ = url.Values{}
