package cli

import (
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
)

func newIPAMCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "ipam",
		Short: "IP address management",
		Long: `Manage subnets and addresses.

'ups ipam next' claims the next free address in a subnet in one call, which
is the operation most worth scripting.`,
	}
	c.AddCommand(newIPAMSubnetCmd(app), newIPAMNextCmd(app), newIPAMAddrCmd(app), newIPAMStatsCmd(app))
	return c
}

func newIPAMSubnetCmd(app *App) *cobra.Command {
	c := &cobra.Command{Use: "subnet", Short: "Subnets"}
	c.AddCommand(
		&cobra.Command{
			Use:     "list",
			Aliases: []string{"ls"},
			Short:   "List subnets",
			RunE: func(cmd *cobra.Command, args []string) error {
				return app.runList(listOpts{
					Path:    "/api/ipam/subnet/",
					Query:   app.infraQuery(nil),
					Columns: []string{"ID", "SUBNET", "NAME", "VLAN", "DESCRIPTION"},
					Empty:   "No subnets.",
					Cells: func(m row) []string {
						return []string{
							str(m, "id"), dash(str(m, "subnet", "cidr", "network")),
							dash(str(m, "name")), dash(str(m, "vlan")),
							truncate(dash(str(m, "description")), 40),
						}
					},
				})
			},
		},
		&cobra.Command{
			Use:   "show <id>",
			Short: "Show one subnet",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				_, raw, err := app.getOne("/api/ipam/subnet/"+args[0]+"/", nil)
				if err != nil {
					return err
				}
				return app.Printer.Object(raw, nil)
			},
		},
	)
	return c
}

func newIPAMNextCmd(app *App) *cobra.Command {
	var subnet, assignTo, description string
	var claim bool
	c := &cobra.Command{
		Use:   "next",
		Short: "Find the next free address in a subnet",
		Long: `Report the next free address in a subnet, and optionally claim it.

Without --claim this only reads, so it is safe to call from a script that
is deciding what to do.`,
		Example: `  ups ipam next --subnet 7
  ups ipam next --subnet 7 --claim --description "core-sw-02 mgmt"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if subnet == "" {
				return errs.Usage("--subnet is required").
					WithHint("list subnets with: ups ipam subnet list")
			}
			m, raw, err := app.getOne("/api/ipam/subnet/"+subnet+"/next/", nil)
			if err != nil {
				return err
			}
			addr := str(m, "ip_address", "address", "ip", "next")
			if addr == "" && app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			if addr == "" {
				return errs.NotFound("no free address available in subnet %s", subnet)
			}

			if !claim {
				if app.AsJSON {
					return app.Printer.Object(raw, nil)
				}
				app.Printer.Printf("%s", addr)
				app.Printer.Infof("not claimed. Re-run with --claim to reserve it.")
				return nil
			}

			body := map[string]any{"ip_address": addr, "subnet": atoiOr(subnet)}
			addIf(body, "description", description)
			if assignTo != "" {
				body["host"] = atoiOr(assignTo)
			}
			var created jsonRaw
			if err := app.mutate("POST", "/api/ipam/ip_address/", body, &created); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			if app.AsJSON {
				return app.Printer.Object(created, nil)
			}
			app.Printer.Printf("%s", addr)
			t, sym := app.Theme(), app.Sym()
			fmt.Fprintf(app.Stderr, "%s Claimed %s in subnet %s.\n", t.Green.Apply(sym.OK), addr, subnet)
			return nil
		},
	}
	c.Flags().StringVar(&subnet, "subnet", "", "subnet id (required)")
	c.Flags().BoolVar(&claim, "claim", false, "reserve the address, not just report it")
	c.Flags().StringVar(&assignTo, "host", "", "host id to assign the address to")
	c.Flags().StringVar(&description, "description", "", "what the address is for")
	return c
}

func newIPAMAddrCmd(app *App) *cobra.Command {
	c := &cobra.Command{Use: "address", Aliases: []string{"addr", "ip"}, Short: "IP addresses"}
	var subnet string
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List IP addresses",
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if subnet != "" {
				q.Set("subnet", subnet)
			}
			return app.runList(listOpts{
				Path:    "/api/ipam/ip_address/",
				Query:   q,
				Columns: []string{"ID", "ADDRESS", "HOST", "DESCRIPTION"},
				Empty:   "No addresses recorded.",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "ip_address", "address")),
						dash(str(m, "host_name", "host")),
						truncate(dash(str(m, "description")), 40),
					}
				},
			})
		},
	}
	list.Flags().StringVar(&subnet, "subnet", "", "filter by subnet id")

	release := &cobra.Command{
		Use:   "release <id>",
		Short: "Release an IP address",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := app.Confirm(fmt.Sprintf("Release IP address %s?", args[0])); err != nil {
				return err
			}
			if err := app.mutate("DELETE", "/api/ipam/ip_address/"+args[0]+"/", nil, nil); err != nil {
				return err
			}
			if !app.DryRun {
				app.Printer.Infof("Released address %s.", args[0])
			}
			return nil
		},
	}
	c.AddCommand(list, release)
	return c
}

func newIPAMStatsCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "IPAM utilisation statistics",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, raw, err := app.getOne("/api/ipam/statistics/", app.infraQuery(nil))
			if err != nil {
				return err
			}
			return app.Printer.Object(raw, nil)
		},
	}
}
