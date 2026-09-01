package cli

import (
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/output"
)

func newEventCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "event",
		Aliases: []string{"events", "incident", "incidents"},
		Short:   "Triage monitoring events and incidents",
		Long: `See what is broken, and act on it.

'ups event list' shows currently open events. Use 'ups change log' alongside
it to answer "what changed here recently?" before touching anything.`,
	}
	c.AddCommand(
		newEventListCmd(app), newEventShowCmd(app), newEventRecoverCmd(app),
		newEventAlertsCmd(app), newIncidentsCmd(app), newEventWatchCmd(app),
	)
	return c
}

// eventTable is shared by list and watch so both render identically.
func eventTable(app *App, items []jsonRaw, truncated bool, total int) *output.Table {
	t := &output.Table{
		Columns:   []string{"ID", "SEVERITY", "HOST", "EVENT", "STARTED"},
		Truncated: truncated, Total: total,
		Empty: "Nothing open. All quiet.",
	}
	for i, m := range decodeRows(items) {
		t.Add(str(m, "id"), items[i],
			str(m, "id"),
			dash(str(m, "severity", "priority", "level")),
			dash(str(m, "host_name", "host")),
			truncate(dash(str(m, "name", "description", "message")), 46),
			dash(str(m, "start_time", "created", "datetime")),
		)
	}
	return t
}

func newEventListCmd(app *App) *cobra.Command {
	var severity string
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List open monitoring events",
		RunE: func(cmd *cobra.Command, args []string) error {
			q := app.infraQuery(nil)
			if severity != "" {
				q.Set("severity", severity)
			}
			cl, err := app.Client()
			if err != nil {
				return err
			}
			ctx, cancel := app.Ctx()
			defer cancel()
			list, err := cl.GetList(ctx, request("GET", "/api/monitoring_event/open_events/", q), app.Limit)
			if err != nil {
				return err
			}
			return app.Printer.Print(eventTable(app, list.Items, list.Truncated, list.Count))
		},
	}
	c.Flags().StringVar(&severity, "severity", "", "filter by severity")
	return c
}

func newEventWatchCmd(app *App) *cobra.Command {
	var interval time.Duration
	c := &cobra.Command{
		Use:   "watch",
		Short: "Follow open events live",
		Long: `Poll for open events and redraw as they change.

The API offers no event stream, so this polls. Ctrl-C stops it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if app.AsJSON || app.IDOnly {
				return errs.Usage("watch renders a live table and cannot emit --json").
					WithHint("poll instead: watch -n5 'ups event list --json'")
			}
			cl, err := app.Client()
			if err != nil {
				return err
			}
			sigc := make(chan os.Signal, 1)
			signal.Notify(sigc, os.Interrupt)
			defer signal.Stop(sigc)

			q := app.infraQuery(nil)
			theme := app.Theme()
			for {
				ctx, cancel := app.Ctx()
				list, err := cl.GetList(ctx, request("GET", "/api/monitoring_event/open_events/", q), app.Limit)
				cancel()
				if err != nil {
					return err
				}
				fmt.Fprint(app.Stdout, "\x1b[2J\x1b[H")
				fmt.Fprintf(app.Stdout, "%s  %s\n\n",
					theme.Bold.Apply("Open events"),
					theme.Dim.Apply(time.Now().Format("15:04:05")+" - ctrl-c to stop"))
				if err := app.Printer.Print(eventTable(app, list.Items, list.Truncated, list.Count)); err != nil {
					return err
				}
				select {
				case <-sigc:
					fmt.Fprintln(app.Stdout)
					return nil
				case <-time.After(interval):
				}
			}
		},
	}
	c.Flags().DurationVar(&interval, "interval", 10*time.Second, "poll interval")
	return c
}

func newEventShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one event",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, raw, err := app.getOne("/api/monitoring_event/"+args[0]+"/", nil)
			if err != nil {
				return err
			}
			if app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			return app.Printer.Object(raw, [][2]string{
				{"ID", str(m, "id")},
				{"Name", dash(str(m, "name", "description"))},
				{"Host", dash(str(m, "host_name", "host"))},
				{"Severity", dash(str(m, "severity", "priority"))},
				{"Started", dash(str(m, "start_time", "created"))},
				{"Acknowledged", dash(str(m, "acknowledged"))},
			})
		},
	}
}

func newEventRecoverCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "recover <id>",
		Short: "Mark an event recovered",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := app.mutate("POST", "/api/monitoring_event/"+args[0]+"/recover/", nil, nil); err != nil {
				return err
			}
			if !app.DryRun {
				t, sym := app.Theme(), app.Sym()
				fmt.Fprintf(app.Stderr, "%s Event %s marked recovered.\n", t.Green.Apply(sym.OK), args[0])
			}
			return nil
		},
	}
}

func newEventAlertsCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "silence <id>",
		Short: "Toggle alerting for an event",
		Long: `Toggle whether an event sends alerts.

For planned work, prefer a maintenance window over silencing individual
events: a window covers every host you are touching and keeps self-inflicted
downtime out of availability reporting.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := app.mutate("POST", "/api/monitoring_event/"+args[0]+"/toggle_alerts/", nil, nil); err != nil {
				return err
			}
			if !app.DryRun {
				app.Printer.Infof("Toggled alerting for event %s.", args[0])
			}
			return nil
		},
	}
}

func newIncidentsCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "current",
		Short: "Show current incidents",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runList(listOpts{
				Path:    "/api/monitoring/current_incidents/",
				Query:   app.infraQuery(nil),
				Columns: []string{"ID", "HOST", "INCIDENT", "SEVERITY", "SINCE"},
				Empty:   "No current incidents.",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "host_name", "host")),
						truncate(dash(str(m, "name", "description")), 46),
						dash(str(m, "severity", "priority")),
						dash(str(m, "start_time", "created")),
					}
				},
			})
		},
	}
}

func newMaintenanceCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "maintenance",
		Short: "Schedule maintenance windows",
		Long: `Suppress alerts for hosts you are about to work on.

Work on a live device generates alerts that page people, and self-inflicted
downtime pollutes customer-facing availability numbers unless it falls
inside a declared window.`,
	}
	c.AddCommand(newMaintListCmd(app), newMaintCreateCmd(app), newMaintDeleteCmd(app))
	return c
}

func newMaintListCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List maintenance windows",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runList(listOpts{
				Path:    "/api/maintenance/",
				Columns: []string{"ID", "NAME", "FROM", "UNTIL", "HOSTS"},
				Empty:   "No maintenance windows.",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "name", "description")),
						dash(str(m, "start_time", "active_since", "start")),
						dash(str(m, "end_time", "active_till", "end")),
						dash(str(m, "hostids", "hosts")),
					}
				},
			})
		},
	}
}

func newMaintCreateCmd(app *App) *cobra.Command {
	var hosts []string
	var name, reason string
	var duration time.Duration
	c := &cobra.Command{
		Use:     "create",
		Short:   "Open a maintenance window over one or more hosts",
		Example: `  ups maintenance create --hosts 12,13 --duration 2h --name "switch firmware"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(hosts) == 0 {
				return errs.Usage("--hosts is required").
					WithHint("find host ids with: ups host list --id-only")
			}
			if duration <= 0 {
				return errs.Usage("--duration must be positive")
			}
			ids := make([]any, 0, len(hosts))
			for _, h := range hosts {
				ids = append(ids, atoiOr(h))
			}
			start := time.Now().UTC()
			body := map[string]any{
				"name":       orDefaultStr(name, "CLI maintenance"),
				"hostids":    ids,
				"start_time": start.Format(time.RFC3339),
				"end_time":   start.Add(duration).Format(time.RFC3339),
			}
			addIf(body, "description", reason)

			var raw jsonRaw
			if err := app.mutate("POST", "/api/maintenance/", body, &raw); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			var m row
			_ = jsonUnmarshal(raw, &m)
			t, sym := app.Theme(), app.Sym()
			fmt.Fprintf(app.Stderr, "%s Maintenance window %s open for %s over %d host(s).\n",
				t.Green.Apply(sym.OK), str(m, "id"), duration, len(hosts))
			return nil
		},
	}
	c.Flags().StringSliceVar(&hosts, "hosts", nil, "host ids to cover")
	c.Flags().StringVar(&name, "name", "", "window name")
	c.Flags().StringVar(&reason, "reason", "", "why the work is happening")
	c.Flags().DurationVar(&duration, "duration", time.Hour, "how long to suppress alerts")
	return c
}

func newMaintDeleteCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "close <id>",
		Short: "Close a maintenance window early",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := app.Confirm(fmt.Sprintf(
				"Close maintenance window %s? Alerting resumes immediately.", args[0])); err != nil {
				return err
			}
			if err := app.mutate("DELETE", "/api/maintenance/"+args[0]+"/", nil, nil); err != nil {
				return err
			}
			if !app.DryRun {
				app.Printer.Infof("Closed maintenance window %s.", args[0])
			}
			return nil
		},
	}
}

func newChangeCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "change",
		Short: "Manage changes and read the audit trail",
	}
	c.AddCommand(newChangeListCmd(app), newChangeShowCmd(app), newChangeCreateCmd(app), newChangeLogCmd(app))
	return c
}

func newChangeListCmd(app *App) *cobra.Command {
	var performer, customer string
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List changes",
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if app.Resolved.Infrastructure.IsSet() {
				q.Set("infrastructure", app.Resolved.Infrastructure.Value)
			}
			if performer != "" {
				q.Set("change_performer", performer)
			}
			if customer != "" {
				q.Set("customer_id", customer)
			}
			return app.runList(listOpts{
				Path:    "/api/change/",
				Query:   q,
				Columns: []string{"ID", "TITLE", "STATUS", "PERFORMER", "CREATED"},
				Empty:   "No changes found.",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), truncate(dash(str(m, "name", "title", "description")), 44),
						dash(str(m, "status", "state")), dash(str(m, "change_performer", "performer")),
						dash(str(m, "created", "created_date")),
					}
				},
			})
		},
	}
	c.Flags().StringVar(&performer, "performer", "", "filter by who performed the change")
	c.Flags().StringVar(&customer, "customer-id", "", "filter by customer id")
	return c
}

func newChangeShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one change",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, raw, err := app.getOne("/api/change/"+args[0]+"/", nil)
			if err != nil {
				return err
			}
			if app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			return app.Printer.Object(raw, [][2]string{
				{"ID", str(m, "id")},
				{"Title", dash(str(m, "name", "title"))},
				{"Status", dash(str(m, "status", "state"))},
				{"Performer", dash(str(m, "change_performer", "performer"))},
				{"Infrastructure", dash(str(m, "infrastructure"))},
				{"Created", dash(str(m, "created"))},
				{"Completed", dash(str(m, "completed"))},
				{"Description", dash(truncate(str(m, "description"), 200))},
			})
		},
	}
}

func newChangeCreateCmd(app *App) *cobra.Command {
	var title, description string
	var hosts []string
	var window time.Duration
	c := &cobra.Command{
		Use:   "create",
		Short: "Open a change, optionally with a maintenance window",
		Long: `Open a change.

With --window and --hosts, a maintenance window is opened alongside the
change so the work does not page anyone or distort availability figures.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if title == "" {
				return errs.Usage("--title is required")
			}
			body := map[string]any{"name": title}
			addIf(body, "description", description)
			if app.Resolved.Infrastructure.IsSet() {
				body["infrastructure"] = atoiOr(app.Resolved.Infrastructure.Value)
			}
			var raw jsonRaw
			if err := app.mutate("POST", "/api/change/", body, &raw); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			var m row
			_ = jsonUnmarshal(raw, &m)
			t, sym := app.Theme(), app.Sym()
			fmt.Fprintf(app.Stderr, "%s Opened change %s.\n", t.Green.Apply(sym.OK), str(m, "id"))

			if window > 0 && len(hosts) > 0 {
				ids := make([]any, 0, len(hosts))
				for _, h := range hosts {
					ids = append(ids, atoiOr(h))
				}
				start := time.Now().UTC()
				mw := map[string]any{
					"name": "Change " + str(m, "id") + ": " + title, "hostids": ids,
					"start_time": start.Format(time.RFC3339),
					"end_time":   start.Add(window).Format(time.RFC3339),
				}
				if err := app.mutate("POST", "/api/maintenance/", mw, nil); err != nil {
					fmt.Fprintf(app.Stderr, "%s change created, but the maintenance window failed: %v\n",
						t.Yellow.Apply(sym.Warn), err)
					return nil
				}
				fmt.Fprintf(app.Stderr, "%s Maintenance window open for %s over %d host(s).\n",
					t.Green.Apply(sym.OK), window, len(hosts))
			} else if len(hosts) > 0 {
				fmt.Fprintf(app.Stderr, "  %s suppress alerts while you work: add --window 2h\n",
					t.Dim.Apply("tip:"))
			}
			return nil
		},
	}
	c.Flags().StringVar(&title, "title", "", "what the change is (required)")
	c.Flags().StringVar(&description, "description", "", "details")
	c.Flags().StringSliceVar(&hosts, "hosts", nil, "hosts the change affects")
	c.Flags().DurationVar(&window, "window", 0, "also open a maintenance window of this length")
	return c
}

func newChangeLogCmd(app *App) *cobra.Command {
	var since, field string
	c := &cobra.Command{
		Use:   "log",
		Short: "Field-level audit trail: what changed, by whom",
		Long: `Show what actually changed.

During an incident this answers "what changed here recently?", which is
usually the fastest route to the cause.`,
		Example: `  ups change log --since 24h
  ups change log --field i_ip_address`,
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if app.Resolved.Infrastructure.IsSet() {
				q.Set("infrastructure", app.Resolved.Infrastructure.Value)
			}
			if field != "" {
				q.Set("changed_field", field)
			}
			if since != "" {
				d, err := time.ParseDuration(since)
				if err != nil {
					return errs.Usage("invalid --since %q", since).
						WithHint("use a duration like 24h or 30m")
				}
				q.Set("created_gte", time.Now().Add(-d).UTC().Format(time.RFC3339))
			}
			return app.runList(listOpts{
				Path:    "/api/change_log/",
				Query:   q,
				Columns: []string{"WHEN", "WHO", "OBJECT", "FIELD", "NEW VALUE"},
				Empty:   "No changes recorded for this window.",
				Cells: func(m row) []string {
					return []string{
						dash(str(m, "change_datetime", "created")),
						dash(str(m, "change_by_name", "change_by")),
						truncate(dash(str(m, "name")), 24),
						dash(str(m, "changed_field")),
						truncate(dash(str(m, "new_value")), 32),
					}
				},
			})
		},
	}
	c.Flags().StringVar(&since, "since", "", "only changes newer than this duration (e.g. 24h)")
	c.Flags().StringVar(&field, "field", "", "filter by changed field name")
	return c
}

func orDefaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
