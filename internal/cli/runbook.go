package cli

import (
	"fmt"
	"net/url"
	"time"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
)

func newRunbookCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "runbook",
		Aliases: []string{"rb"},
		Short:   "Run and inspect automation runbooks",
		Long: `Runbooks execute against live network devices.

A run that fails halfway because a credential was missing can leave a device
partially configured, which is worse than not running at all. 'ups runbook
preflight' checks for that first and costs nothing.`,
	}
	c.AddCommand(
		newRunbookListCmd(app), newRunbookShowCmd(app), newRunbookPreflightCmd(app),
		newRunbookRunCmd(app), newRunbookResultsCmd(app), newRunbookCancelCmd(app),
	)
	return c
}

func newRunbookListCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List runbooks",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runList(listOpts{
				Path:    "/api/orchestration/runbooks/",
				Columns: []string{"ID", "NAME", "DESCRIPTION"},
				Empty:   "No runbooks.",
				Cells: func(m row) []string {
					return []string{str(m, "id"), dash(str(m, "name")),
						truncate(dash(str(m, "description")), 60)}
				},
			})
		},
	}
}

func newRunbookShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one runbook",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, raw, err := app.getOne("/api/orchestration/runbooks/"+args[0]+"/", nil)
			if err != nil {
				return err
			}
			return app.Printer.Object(raw, nil)
		},
	}
}

// newRunbookPreflightCmd reports missing credentials before a run.
func newRunbookPreflightCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "preflight <id>",
		Short: "Check a runbook for missing credentials before running it",
		Long: `Report credentials the runbook needs but cannot find.

Run this before every run. A partial execution against network hardware is
harder to recover from than a run that never started.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			missing, err := app.runbookMissingCredentials(args[0])
			if err != nil {
				return err
			}
			t, sym := app.Theme(), app.Sym()
			if app.AsJSON {
				return app.Printer.JSON(map[string]any{"runbook": args[0], "missing": missing, "ready": len(missing) == 0})
			}
			if len(missing) == 0 {
				fmt.Fprintf(app.Stdout, "%s Runbook %s has every credential it needs.\n",
					t.Green.Apply(sym.OK), args[0])
				return nil
			}
			fmt.Fprintf(app.Stdout, "%s Runbook %s is missing %d credential(s):\n",
				t.Red.Apply(sym.Fail), args[0], len(missing))
			for _, m := range missing {
				fmt.Fprintf(app.Stdout, "  %s %s\n", sym.Bullet, m)
			}
			return errs.Conflict("runbook %s is not ready to run", args[0]).
				WithHint("store the missing credentials: ups credential create <type> --name ...")
		},
	}
}

// runbookMissingCredentials returns human-readable descriptions of what is absent.
func (a *App) runbookMissingCredentials(id string) ([]string, error) {
	cl, err := a.Client()
	if err != nil {
		return nil, err
	}
	ctx, cancel := a.Ctx()
	defer cancel()

	var raw jsonRaw
	err = cl.Do(ctx, request("GET", "/api/orchestration/runbooks/"+id+"/missing-credentials/", nil), &raw)
	if err != nil {
		return nil, err
	}
	var asList []any
	if jsonUnmarshal(raw, &asList) == nil {
		out := make([]string, 0, len(asList))
		for _, item := range asList {
			switch v := item.(type) {
			case string:
				out = append(out, v)
			case map[string]any:
				out = append(out, dash(str(row(v), "name", "credential_type", "type", "tag")))
			}
		}
		return out, nil
	}
	var asObj map[string]any
	if jsonUnmarshal(raw, &asObj) == nil {
		if inner, ok := asObj["missing_credentials"].([]any); ok {
			out := make([]string, 0, len(inner))
			for _, item := range inner {
				out = append(out, fmt.Sprint(item))
			}
			return out, nil
		}
		if len(asObj) == 0 {
			return nil, nil
		}
	}
	return nil, nil
}

func newRunbookRunCmd(app *App) *cobra.Command {
	var infra string
	var follow, skipPreflight bool
	var interval time.Duration

	c := &cobra.Command{
		Use:   "run <id>",
		Short: "Run a runbook against an infrastructure",
		Long: `Run a runbook.

Preflight runs first unless --skip-preflight is given: a missing credential
found before the run costs nothing, and found halfway through can leave a
device partially configured.`,
		Example: `  ups runbook run 5
  ups runbook run 5 --follow`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := infra
			if target == "" {
				var err error
				if target, err = app.Resolved.RequireInfra(); err != nil {
					return err
				}
			}
			t, sym := app.Theme(), app.Sym()

			if !skipPreflight {
				missing, err := app.runbookMissingCredentials(args[0])
				if err != nil {
					fmt.Fprintf(app.Stderr, "%s preflight could not run: %v\n", t.Yellow.Apply(sym.Warn), err)
				} else if len(missing) > 0 {
					fmt.Fprintf(app.Stderr, "%s Runbook %s is missing %d credential(s):\n",
						t.Red.Apply(sym.Fail), args[0], len(missing))
					for _, m := range missing {
						fmt.Fprintf(app.Stderr, "  %s %s\n", sym.Bullet, m)
					}
					return errs.Conflict("refusing to run a runbook with missing credentials").
						WithHint("fix them, or override with --skip-preflight if you know what you are doing")
				} else {
					fmt.Fprintf(app.Stderr, "%s preflight passed\n", t.Green.Apply(sym.OK))
				}
			}

			if err := app.Confirm(fmt.Sprintf(
				"Run runbook %s against infrastructure %s? This executes against live devices.",
				args[0], target)); err != nil {
				return err
			}

			path := fmt.Sprintf("/api/orchestration/runbook/%s/start/%s/", args[0], target)
			var raw jsonRaw
			if err := app.mutate("POST", path, nil, &raw); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			var m row
			_ = jsonUnmarshal(raw, &m)
			runID := str(m, "id", "result_id", "run_id")
			fmt.Fprintf(app.Stderr, "%s Started runbook %s (run %s)\n",
				t.Green.Apply(sym.OK), args[0], dash(runID))

			if !follow {
				fmt.Fprintf(app.Stderr, "  %s ups runbook results %s\n", t.Dim.Apply("watch:"), args[0])
				return app.Printer.Object(raw, nil)
			}
			return app.followRunbook(args[0], interval)
		},
	}
	c.Flags().StringVar(&infra, "infra-id", "", "infrastructure to run against")
	c.Flags().BoolVar(&follow, "follow", false, "poll until the run finishes")
	c.Flags().BoolVar(&skipPreflight, "skip-preflight", false, "do not check for missing credentials first")
	c.Flags().DurationVar(&interval, "interval", 5*time.Second, "poll interval when following")
	return c
}

// followRunbook polls the last result until it stops running.
func (a *App) followRunbook(runbookID string, interval time.Duration) error {
	cl, err := a.Client()
	if err != nil {
		return err
	}
	t, sym := a.Theme(), a.Sym()
	deadline := time.Now().Add(30 * time.Minute)

	for attempt := 0; time.Now().Before(deadline); attempt++ {
		ctx, cancel := a.Ctx()
		var raw jsonRaw
		err := cl.Do(ctx, request("GET",
			"/api/orchestration/runbook/"+runbookID+"/results/last/", nil), &raw)
		cancel()
		if err != nil {
			return err
		}
		var m row
		_ = jsonUnmarshal(raw, &m)
		state := str(m, "state", "status", "result")
		done := str(m, "finished", "completed", "end_time") != ""

		fmt.Fprintf(a.Stderr, "\r\x1b[2K%s %s", t.Dim.Apply("running"), dash(state))
		if done || isTerminalState(state) {
			fmt.Fprintf(a.Stderr, "\r\x1b[2K%s Run finished: %s\n", t.Green.Apply(sym.OK), dash(state))
			return a.Printer.Object(raw, nil)
		}
		time.Sleep(interval)
	}
	fmt.Fprintln(a.Stderr)
	return errs.General("stopped following after 30 minutes; the run may still be going").
		WithHint("check it with: ups runbook results %s", runbookID)
}

func isTerminalState(s string) bool {
	switch s {
	case "finished", "completed", "success", "failed", "error", "cancelled", "canceled":
		return true
	}
	return false
}

func newRunbookResultsCmd(app *App) *cobra.Command {
	var last bool
	c := &cobra.Command{
		Use:   "results <runbook-id>",
		Short: "Show runbook run results",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if last {
				_, raw, err := app.getOne("/api/orchestration/runbook/"+args[0]+"/results/last/", nil)
				if err != nil {
					return err
				}
				return app.Printer.Object(raw, nil)
			}
			return app.runList(listOpts{
				Path:    "/api/orchestration/runbook/" + args[0] + "/results/",
				Columns: []string{"ID", "STATE", "STARTED", "FINISHED"},
				Empty:   "This runbook has not been run.",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "state", "status")),
						dash(str(m, "start_time", "created", "started")),
						dash(str(m, "end_time", "finished", "completed")),
					}
				},
			})
		},
	}
	c.Flags().BoolVar(&last, "last", false, "show only the most recent run")
	return c
}

func newRunbookCancelCmd(app *App) *cobra.Command {
	var runbook string
	c := &cobra.Command{
		Use:   "cancel <result-id>",
		Short: "Cancel a running runbook",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if runbook == "" {
				return errs.Usage("--runbook is required").
					WithHint("the result id alone does not identify the runbook")
			}
			if err := app.Confirm(fmt.Sprintf(
				"Cancel run %s? A partially applied runbook may leave devices half-configured.",
				args[0])); err != nil {
				return err
			}
			path := fmt.Sprintf("/api/orchestration/runbook/%s/results/%s/cancel/", runbook, args[0])
			if err := app.mutate("POST", path, nil, nil); err != nil {
				return err
			}
			if !app.DryRun {
				app.Printer.Infof("Cancellation requested for run %s.", args[0])
			}
			return nil
		},
	}
	c.Flags().StringVar(&runbook, "runbook", "", "runbook id the run belongs to (required)")
	return c
}

var _ = url.Values{}
