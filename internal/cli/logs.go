package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/output"
)

// LogFilter narrows a set of log records.
//
// The real log API is not built yet: GET /api/logs/ accepts no query
// parameters, so filtering happens here, on records already fetched. This
// interface exists so that a server-side implementation can replace the
// client-side one without changing the command surface.
type LogFilter interface {
	Match(rec map[string]any) bool
	Describe() string
}

type clientFilter struct {
	host  string
	text  string
	since time.Time
	level string
}

func (f clientFilter) Match(rec map[string]any) bool {
	m := row(rec)
	if f.host != "" {
		h := strings.ToLower(str(m, "host", "host_name", "hostname", "source"))
		if !strings.Contains(h, strings.ToLower(f.host)) {
			return false
		}
	}
	if f.level != "" {
		l := strings.ToLower(str(m, "level", "severity", "priority"))
		if !strings.EqualFold(l, f.level) {
			return false
		}
	}
	if f.text != "" {
		blob, _ := json.Marshal(rec)
		if !strings.Contains(strings.ToLower(string(blob)), strings.ToLower(f.text)) {
			return false
		}
	}
	if !f.since.IsZero() {
		ts := str(m, "timestamp", "time", "datetime", "created", "@timestamp")
		if parsed, ok := parseLogTime(ts); ok && parsed.Before(f.since) {
			return false
		}
	}
	return true
}

func (f clientFilter) Describe() string {
	var parts []string
	if f.host != "" {
		parts = append(parts, "host~"+f.host)
	}
	if f.level != "" {
		parts = append(parts, "level="+f.level)
	}
	if f.text != "" {
		parts = append(parts, "text~"+f.text)
	}
	if !f.since.IsZero() {
		parts = append(parts, "since "+f.since.Format(time.RFC3339))
	}
	if len(parts) == 0 {
		return "no filter"
	}
	return strings.Join(parts, ", ")
}

func parseLogTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func newLogsCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "logs",
		Short: "Search infrastructure logs",
		Long: `Search logs for the active infrastructure.

The log API is not finished: GET /api/logs/ accepts no query parameters, so
the CLI fetches records and filters them locally. Two consequences worth
knowing:

  - Filtering is not free. Do not call this in a loop over many hosts.
  - Results are capped, and a truncated set is reported as such. Truncated
    is not the same as "no more matches", and a client-side filter cannot
    see records it never fetched.`,
	}
	c.AddCommand(newLogsSearchCmd(app), newLogsFollowCmd(app))
	return c
}

// fetchLogs retrieves and filters log records.
func (a *App) fetchLogs(f LogFilter, limit int) (*output.Table, int, error) {
	cl, err := a.Client()
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := a.Ctx()
	defer cancel()

	q := a.infraQuery(nil)
	list, err := cl.GetList(ctx, request("GET", "/api/logs/", q), 0)
	if err != nil {
		return nil, 0, err
	}

	t := &output.Table{
		Columns: []string{"TIME", "LEVEL", "HOST", "MESSAGE"},
		Empty:   "No log records matched.",
	}
	fetched := len(list.Items)
	kept := 0
	for i, m := range decodeRows(list.Items) {
		if f != nil && !f.Match(m) {
			continue
		}
		if limit > 0 && kept >= limit {
			t.Truncated = true
			break
		}
		kept++
		t.Add(str(m, "id"), list.Items[i],
			dash(str(m, "timestamp", "time", "datetime", "created", "@timestamp")),
			dash(str(m, "level", "severity", "priority")),
			dash(str(m, "host", "host_name", "hostname", "source")),
			truncate(dash(str(m, "message", "msg", "text", "line")), 70),
		)
	}
	if list.Truncated {
		t.Truncated = true
	}
	return t, fetched, nil
}

func newLogsSearchCmd(app *App) *cobra.Command {
	var host, text, level, since string
	c := &cobra.Command{
		Use:     "search",
		Aliases: []string{"ls", "list"},
		Short:   "Search logs (filtered locally)",
		Example: `  ups logs search --since 1h
  ups logs search --host core-sw-01 --text "link down"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			f := clientFilter{host: host, text: text, level: level}
			if since != "" {
				d, err := time.ParseDuration(since)
				if err != nil {
					return errs.Usage("invalid --since %q", since).
						WithHint("use a duration like 1h or 30m")
				}
				f.since = time.Now().Add(-d)
			}

			var t *output.Table
			var fetched int
			if err := app.Spin("Fetching logs", func() error {
				var e error
				t, fetched, e = app.fetchLogs(f, app.Limit)
				return e
			}); err != nil {
				return err
			}
			if err := app.Printer.Print(t); err != nil {
				return err
			}
			// Make the client-side nature visible: the user must be able to
			// tell "nothing matched" from "the server never sent it".
			if !app.AsJSON && !app.IDOnly {
				app.Printer.Infof("filtered %d fetched record(s) locally (%s); the server API does not filter yet",
					fetched, f.Describe())
			}
			return nil
		},
	}
	c.Flags().StringVar(&host, "host", "", "match host name (substring)")
	c.Flags().StringVar(&text, "text", "", "match free text anywhere in the record")
	c.Flags().StringVar(&level, "level", "", "match log level exactly")
	c.Flags().StringVar(&since, "since", "", "only records newer than this duration")
	return c
}

func newLogsFollowCmd(app *App) *cobra.Command {
	var host, text, level string
	var interval time.Duration
	c := &cobra.Command{
		Use:   "follow",
		Short: "Poll for new log records",
		Long: `Follow logs.

There is no log stream in the API, so this polls and redraws. Ctrl-C stops it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if app.AsJSON || app.IDOnly {
				return errs.Usage("follow renders a live table and cannot emit --json").
					WithHint("poll instead: watch -n5 'ups logs search --json'")
			}
			sigc := make(chan os.Signal, 1)
			signal.Notify(sigc, os.Interrupt)
			defer signal.Stop(sigc)

			f := clientFilter{host: host, text: text, level: level}
			theme := app.Theme()
			for {
				f.since = time.Now().Add(-interval * 2)
				t, _, err := app.fetchLogs(f, app.Limit)
				if err != nil {
					return err
				}
				fmt.Fprint(app.Stdout, "\x1b[2J\x1b[H")
				fmt.Fprintf(app.Stdout, "%s  %s\n\n", theme.Bold.Apply("Logs"),
					theme.Dim.Apply(time.Now().Format("15:04:05")+" - filtered locally - ctrl-c to stop"))
				if err := app.Printer.Print(t); err != nil {
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
	c.Flags().StringVar(&host, "host", "", "match host name (substring)")
	c.Flags().StringVar(&text, "text", "", "match free text")
	c.Flags().StringVar(&level, "level", "", "match log level")
	c.Flags().DurationVar(&interval, "interval", 10*time.Second, "poll interval")
	return c
}
