package cli

import (
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
)

func newInfraCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "infra",
		Aliases: []string{"infrastructure"},
		Short:   "Inspect infrastructures",
	}
	c.AddCommand(
		&cobra.Command{
			Use:     "list",
			Aliases: []string{"ls"},
			Short:   "List infrastructures you can see",
			RunE: func(cmd *cobra.Command, args []string) error {
				return app.runList(listOpts{
					Path:    "/api/infrastructure/",
					Columns: []string{"ID", "NAME", "CODE", "CUSTOMER", "ENABLED"},
					Empty:   "No infrastructures visible to this account.",
					Cells: func(m row) []string {
						return []string{
							str(m, "id"), dash(str(m, "name")), dash(str(m, "infra_code")),
							dash(str(m, "customer_name", "customer")), dash(str(m, "enabled")),
						}
					},
				})
			},
		},
		&cobra.Command{
			Use:   "show [id]",
			Short: "Show one infrastructure",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := app.argOrContextInfra(args)
				if err != nil {
					return err
				}
				m, raw, err := app.getOne("/api/infrastructure/"+id+"/", nil)
				if err != nil {
					return err
				}
				if app.AsJSON {
					return app.Printer.Object(raw, nil)
				}
				return app.Printer.Object(raw, [][2]string{
					{"ID", str(m, "id")},
					{"Name", dash(str(m, "name"))},
					{"Code", dash(str(m, "infra_code"))},
					{"Customer", dash(str(m, "customer_name", "customer"))},
					{"Enabled", dash(str(m, "enabled"))},
					{"Healthcheck", dash(str(m, "healthcheck_state"))},
					{"Healthcheck running", dash(str(m, "healthcheck_running"))},
				})
			},
		},
		&cobra.Command{
			Use:   "hosts [id]",
			Short: "List the hosts in an infrastructure",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := app.argOrContextInfra(args)
				if err != nil {
					return err
				}
				return app.runList(listOpts{
					Path:    "/api/infrastructure/" + id + "/hosts/",
					Columns: []string{"ID", "NAME", "HOSTNAME", "IP", "TYPE"},
					Empty:   "No hosts in this infrastructure.",
					Cells: func(m row) []string {
						return []string{
							str(m, "id"), dash(str(m, "name")), dash(str(m, "i_hostname")),
							dash(str(m, "i_ip_address")), dash(str(m, "i_type", "node_type")),
						}
					},
				})
			},
		},
		newInfraHealthcheckCmd(app),
		&cobra.Command{
			Use:   "status",
			Short: "Show host status counts across infrastructures",
			RunE: func(cmd *cobra.Command, args []string) error {
				m, raw, err := app.getOne("/api/infrastructure/host_status/", app.infraQuery(nil))
				if err != nil {
					return err
				}
				if app.AsJSON {
					return app.Printer.Object(raw, nil)
				}
				var fields [][2]string
				for k, v := range m {
					fields = append(fields, [2]string{k, str(row{k: v}, k)})
				}
				return app.Printer.Object(raw, fields)
			},
		},
	)
	return c
}

// newInfraHealthcheckCmd is the platform-side scan. Deliberately named apart
// from `ups doctor`, which only inspects the local setup.
func newInfraHealthcheckCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "healthcheck [id]",
		Short: "Start a platform healthcheck scan of an infrastructure",
		Long: `Start a healthcheck scan on the platform.

This is a live operation against the customer environment and needs API
credentials on the infrastructure. It is not 'ups doctor', which only
checks your local setup and touches nothing remote.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := app.argOrContextInfra(args)
			if err != nil {
				return err
			}
			m, _, err := app.getOne("/api/infrastructure/"+id+"/", nil)
			if err != nil {
				return err
			}
			name := str(m, "name", "infra_code")
			if err := app.Confirm(fmt.Sprintf(
				"Start a healthcheck scan against infrastructure %s (%s)?", id, name)); err != nil {
				return err
			}
			q := url.Values{}
			q.Set("infrastructure", id)
			if app.DryRun {
				app.Printer.Infof("dry-run: GET /api/healthcheck/start/?%s", q.Encode())
				return nil
			}
			var raw jsonRaw
			c, err := app.Client()
			if err != nil {
				return err
			}
			ctx, cancel := app.Ctx()
			defer cancel()
			if err := c.Do(ctx, request("GET", "/api/healthcheck/start/", q), &raw); err != nil {
				return err
			}
			t, sym := app.Theme(), app.Sym()
			fmt.Fprintf(app.Stderr, "%s Healthcheck started for %s. Track it with: ups infra show %s\n",
				t.Green.Apply(sym.OK), name, id)
			return nil
		},
	}
}

// argOrContextInfra takes an explicit id or falls back to the active context.
func (a *App) argOrContextInfra(args []string) (string, error) {
	if len(args) == 1 && args[0] != "" {
		return args[0], nil
	}
	id, err := a.Resolved.RequireInfra()
	if err != nil {
		return "", errs.Usage("no infrastructure given and none is selected").
			WithHint("pass an id, or set one: ups context set")
	}
	return id, nil
}
