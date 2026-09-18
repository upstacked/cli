package cli

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/history"
	"github.com/upstacked/cli/internal/mib"
	"github.com/upstacked/cli/internal/output"
	"github.com/upstacked/cli/internal/skill"
)

func newDebugCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "debug",
		Short: "What this CLI is, and what it has been doing",
		Long: `Report the CLI's own state and recent activity.

'ups doctor' answers "is the setup correct". This answers "what actually
happened", which is the question a support conversation starts from:

  ups debug info      # version, server, profile, context, caches
  ups debug log       # recent invocations, with the requests they made
  ups debug bundle    # both at once, for pasting into a bug report

Every invocation is recorded locally to the config directory - the command,
its exit code, and the method, path and status of each API request. Response
bodies are never recorded, because they carry customer data and a debug log is
the last place it should be copied. Nothing is sent anywhere. Set
UPS_NO_HISTORY=1 to turn recording off.`,
	}
	c.AddCommand(newDebugInfoCmd(app), newDebugLogCmd(app), newDebugBundleCmd(app))
	return c
}

func newDebugInfoCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "info",
		Aliases: []string{"env"},
		Short:   "Show version, server, context and cache state",
		RunE: func(cmd *cobra.Command, args []string) error {
			info := app.debugInfo()
			if app.AsJSON {
				return app.Printer.JSON(info)
			}
			fields := make([][2]string, 0, len(info))
			for _, k := range debugInfoOrder {
				if v, ok := info[k]; ok {
					fields = append(fields, [2]string{k, fmt.Sprint(v)})
				}
			}
			return app.Printer.Object(nil, fields)
		},
	}
}

var debugInfoOrder = []string{
	"version", "os", "arch", "config_dir", "profile", "api_url",
	"api_url_source", "infrastructure", "infrastructure_source", "customer",
	"authenticated", "skill_version", "mib_cache", "mib_index", "history",
}

func (a *App) debugInfo() map[string]any {
	info := map[string]any{
		"version": Version,
		"os":      runtime.GOOS,
		"arch":    runtime.GOARCH,
	}
	if a.Store != nil {
		info["config_dir"] = a.Store.Dir
		if recs, err := history.Read(a.Store.Dir, 0); err == nil {
			info["history"] = fmt.Sprintf("%d invocations recorded", len(recs))
		}
		if history.Disabled() {
			info["history"] = "disabled (UPS_NO_HISTORY)"
		}
	}
	if a.Resolved != nil {
		info["profile"] = a.Resolved.ProfileName
		info["api_url"] = dash(a.Resolved.APIURL.Value)
		// Where a setting came from matters more than its value: staging and
		// production differ by one line of config and nothing in the prompt.
		info["api_url_source"] = a.Resolved.APIURL.Describe()
		info["infrastructure"] = dash(a.Resolved.Infrastructure.Value)
		info["infrastructure_source"] = a.Resolved.Infrastructure.Describe()
		info["customer"] = dash(a.Resolved.Customer.Value)
	}
	info["authenticated"] = a.haveCredentials()
	info["skill_version"] = skill.Checksum(skill.Content)

	if store, err := mib.NewStore(""); err == nil {
		cached := store.Cached()
		names := make([]string, 0, len(cached))
		for _, r := range cached {
			names = append(names, r.Name+"@"+dash(r.Revision))
		}
		info["mib_cache"] = dash(strings.Join(names, ", "))
		if objs, _, err := store.Search(mib.Query{Limit: 1}); err == nil && len(objs) > 0 {
			info["mib_index"] = store.IndexPath()
		} else {
			info["mib_index"] = "not built (ups mib sync)"
		}
	}
	return info
}

// haveCredentials reports whether a token exists, never what it is.
func (a *App) haveCredentials() bool {
	if v := envToken(); v != "" {
		return true
	}
	if a.Store == nil || a.Resolved == nil {
		return false
	}
	creds, err := a.Store.LoadCredentials()
	if err != nil {
		return false
	}
	_, ok := creds.For(a.Resolved.ProfileName, a.Resolved.APIURL.Value)
	return ok
}

func newDebugLogCmd(app *App) *cobra.Command {
	var n int
	var failed bool
	c := &cobra.Command{
		Use:     "log",
		Aliases: []string{"history"},
		Short:   "Show recent invocations and the requests they made",
		Long: `Print the recorded invocations, oldest first.

Each row is one run of the CLI: what was invoked, how it exited, and how many
API requests it made. --json includes the requests themselves - method, path,
status and duration - which is usually the thing worth reading.

Recording is local and nothing is sent anywhere. Response bodies are never
kept.`,
		Example: `  ups debug log -n 20
  ups debug log --failed
  ups debug log --json | jq '.[] | select(.exit != 0)'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if app.Store == nil {
				return nil
			}
			recs, err := history.Read(app.Store.Dir, 0)
			if err != nil {
				return err
			}
			if failed {
				var keep []history.Record
				for _, r := range recs {
					if r.Exit != 0 {
						keep = append(keep, r)
					}
				}
				recs = keep
			}
			if n > 0 && len(recs) > n {
				recs = recs[len(recs)-n:]
			}
			if app.AsJSON {
				return app.Printer.JSON(recs)
			}

			t := &output.Table{
				Columns: []string{"WHEN", "COMMAND", "EXIT", "MS", "CALLS", "ERROR"},
				Empty:   "Nothing recorded yet.",
			}
			for _, r := range recs {
				raw, _ := json.Marshal(r)
				t.Add(r.Command, raw,
					r.Time.Local().Format("01-02 15:04:05"),
					dash(r.Command),
					fmt.Sprint(r.Exit),
					fmt.Sprint(r.MS),
					fmt.Sprint(len(r.Requests)),
					truncate(dash(r.Error), 46))
			}
			return app.Printer.Print(t)
		},
	}
	c.Flags().IntVarP(&n, "lines", "n", 25, "how many invocations to show (0 for all)")
	c.Flags().BoolVar(&failed, "failed", false, "only invocations that exited non-zero")
	return c
}

func newDebugBundleCmd(app *App) *cobra.Command {
	var n int
	c := &cobra.Command{
		Use:   "bundle",
		Short: "Print environment and recent activity together, for a bug report",
		Long: `Print everything a bug report needs, in one pasteable block.

This is what to attach when something is not behaving: the CLI's own state and
the last few invocations with the status codes they got back. It contains no
tokens and no response bodies - check it before sending it anyway, since
command arguments can name hosts and customers.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			bundle := map[string]any{
				"generated": time.Now().UTC().Format(time.RFC3339),
				"info":      app.debugInfo(),
			}
			if app.Store != nil {
				recs, err := history.Read(app.Store.Dir, n)
				if err != nil {
					return err
				}
				bundle["recent"] = recs
			}
			return app.Printer.JSON(bundle)
		},
	}
	c.Flags().IntVarP(&n, "lines", "n", 25, "how many invocations to include (0 for all)")
	return c
}
