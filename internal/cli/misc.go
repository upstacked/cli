package cli

import (
	"fmt"
	"net/url"
	"time"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
)

func newDiscoveryCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "discovery",
		Short: "Discover devices by scanning the network",
		Long: `Topology discovery: scan a network and find devices.

This is network scanning, not log mining. Discovery produces candidate
hosts you then promote into real ones.`,
	}
	c.AddCommand(
		&cobra.Command{
			Use:     "list",
			Aliases: []string{"ls"},
			Short:   "List discoveries",
			RunE: func(cmd *cobra.Command, args []string) error {
				return app.runList(listOpts{
					Path:    "/api/discovery/",
					Columns: []string{"ID", "NAME", "STATE", "INFRASTRUCTURE"},
					Empty:   "No discoveries recorded.",
					Cells: func(m row) []string {
						return []string{
							str(m, "id"), dash(str(m, "name")),
							dash(str(m, "state", "status")),
							dash(str(m, "infrastructure_name", "infrastructure")),
						}
					},
				})
			},
		},
		&cobra.Command{
			Use:   "show <id>",
			Short: "Show one discovery and what it found",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				_, raw, err := app.getOne("/api/discovery/"+args[0]+"/", nil)
				if err != nil {
					return err
				}
				return app.Printer.Object(raw, nil)
			},
		},
		&cobra.Command{
			Use:   "start",
			Short: "Start a discovery scan",
			RunE: func(cmd *cobra.Command, args []string) error {
				infra, err := app.Resolved.RequireInfra()
				if err != nil {
					return err
				}
				if err := app.Confirm(fmt.Sprintf(
					"Start a discovery scan on infrastructure %s? This probes the customer network.", infra)); err != nil {
					return err
				}
				q := url.Values{}
				q.Set("infrastructure", infra)
				if app.DryRun {
					app.Printer.Infof("dry-run: GET /api/discovery/start/?%s", q.Encode())
					return nil
				}
				cl, err := app.Client()
				if err != nil {
					return err
				}
				ctx, cancel := app.Ctx()
				defer cancel()
				var raw jsonRaw
				if err := app.Spin("Starting discovery", func() error {
					return cl.Do(ctx, request("GET", "/api/discovery/start/", q), &raw)
				}); err != nil {
					return err
				}
				t, sym := app.Theme(), app.Sym()
				fmt.Fprintf(app.Stderr, "%s Discovery started. Track it with: ups discovery list\n",
					t.Green.Apply(sym.OK))
				return nil
			},
		},
		&cobra.Command{
			Use:   "topology",
			Short: "Show the discovered topology",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, raw, err := app.getOne("/api/topology/get/", app.infraQuery(nil))
				if err != nil {
					return err
				}
				return app.Printer.Object(raw, nil)
			},
		},
	)
	return c
}

func newTicketCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "ticket",
		Short: "Work with tickets and logged time",
	}
	c.AddCommand(
		&cobra.Command{
			Use:     "list",
			Aliases: []string{"ls"},
			Short:   "List tickets",
			RunE: func(cmd *cobra.Command, args []string) error {
				return app.runList(listOpts{
					Path:    "/api/ticket/",
					Query:   app.infraQuery(nil),
					Columns: []string{"ID", "SUBJECT", "STATUS", "PRIORITY", "ASSIGNEE"},
					Empty:   "No tickets.",
					Cells: func(m row) []string {
						return []string{
							str(m, "id"), truncate(dash(str(m, "subject", "title", "name")), 44),
							dash(str(m, "status", "state")), dash(str(m, "priority")),
							dash(str(m, "assignee", "assigned_to", "owner")),
						}
					},
				})
			},
		},
		&cobra.Command{
			Use:   "show <id>",
			Short: "Show one ticket",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				m, raw, err := app.getOne("/api/ticket/"+args[0]+"/", nil)
				if err != nil {
					return err
				}
				if app.AsJSON {
					return app.Printer.Object(raw, nil)
				}
				return app.Printer.Object(raw, [][2]string{
					{"ID", str(m, "id")},
					{"Subject", dash(str(m, "subject", "title", "name"))},
					{"Status", dash(str(m, "status", "state"))},
					{"Priority", dash(str(m, "priority"))},
					{"Assignee", dash(str(m, "assignee", "assigned_to"))},
					{"Created", dash(str(m, "created"))},
					{"Description", truncate(dash(str(m, "description")), 300)},
				})
			},
		},
		newTicketTimeCmd(app),
		&cobra.Command{
			Use:   "stats",
			Short: "Ticket statistics",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, raw, err := app.getOne("/api/ticket/statistics/", app.infraQuery(nil))
				if err != nil {
					return err
				}
				return app.Printer.Object(raw, nil)
			},
		},
	)
	return c
}

func newTicketTimeCmd(app *App) *cobra.Command {
	var ticket, description string
	var duration time.Duration
	c := &cobra.Command{
		Use:     "log-time",
		Short:   "Log work activity against a ticket",
		Example: `  ups ticket log-time --ticket 812 --duration 45m --description "replaced SFP"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if ticket == "" || duration <= 0 {
				return errs.Usage("--ticket and a positive --duration are required")
			}
			body := map[string]any{
				"ticket":  atoiOr(ticket),
				"minutes": int(duration.Minutes()),
			}
			addIf(body, "description", description)
			var raw jsonRaw
			if err := app.mutate("POST", "/api/ticket/workactivity/", body, &raw); err != nil {
				return err
			}
			if !app.DryRun {
				t, sym := app.Theme(), app.Sym()
				fmt.Fprintf(app.Stderr, "%s Logged %s against ticket %s.\n",
					t.Green.Apply(sym.OK), duration, ticket)
			}
			return nil
		},
	}
	c.Flags().StringVar(&ticket, "ticket", "", "ticket id (required)")
	c.Flags().DurationVar(&duration, "duration", 0, "time spent (required)")
	c.Flags().StringVar(&description, "description", "", "what was done")
	return c
}

func newReportCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "report",
		Short: "Generate reports",
		Long: `Pull reporting data.

Designed for scheduled use: combine with --json to feed a pipeline.`,
	}
	mk := func(use, short, path string) *cobra.Command {
		var from, to string
		sub := &cobra.Command{
			Use:   use,
			Short: short,
			RunE: func(cmd *cobra.Command, args []string) error {
				q := app.infraQuery(nil)
				if from != "" {
					q.Set("start_date", from)
				}
				if to != "" {
					q.Set("end_date", to)
				}
				_, raw, err := app.getOne(path, q)
				if err != nil {
					return err
				}
				return app.Printer.Object(raw, nil)
			},
		}
		sub.Flags().StringVar(&from, "from", "", "start date (YYYY-MM-DD)")
		sub.Flags().StringVar(&to, "to", "", "end date (YYYY-MM-DD)")
		return sub
	}
	c.AddCommand(
		mk("availability", "Availability report", "/api/reporting/availability/"),
		mk("changes", "Change report", "/api/reporting/changes/"),
		mk("tickets", "Ticket report", "/api/reporting/tickets/"),
		&cobra.Command{
			Use:   "templates",
			Short: "List report templates",
			RunE: func(cmd *cobra.Command, args []string) error {
				return app.runList(listOpts{
					Path:    "/api/reporting/templates/",
					Columns: []string{"ID", "NAME", "DESCRIPTION"},
					Empty:   "No report templates.",
					Cells: func(m row) []string {
						return []string{str(m, "id"), dash(str(m, "name")),
							truncate(dash(str(m, "description")), 60)}
					},
				})
			},
		},
	)
	return c
}
