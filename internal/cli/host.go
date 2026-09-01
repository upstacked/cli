package cli

import (
	"fmt"
	"net/url"

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
		newHostDeleteCmd(app), newHostTraceCmd(app), newHostLinksCmd(app),
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
			if err := app.mutate("POST", "/api/host/", body, &raw); err != nil {
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
	return &cobra.Command{
		Use:   "links",
		Short: "List topology links between hosts",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runList(listOpts{
				Path:    "/api/host_link/",
				Query:   app.infraQuery(nil),
				Columns: []string{"ID", "FROM", "TO", "TYPE"},
				Empty:   "No host links recorded.",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "host_a", "from_host", "source")),
						dash(str(m, "host_b", "to_host", "target")), dash(str(m, "link_type", "type")),
					}
				},
			})
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
