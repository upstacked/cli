package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/api"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/output"
)

const (
	// logsSearchPath is the Elasticsearch-backed endpoint. It is newer than the
	// oldest server this CLI talks to, so its absence is an expected state, not
	// a failure.
	logsSearchPath = "/api/logs/search/"
	// logsListPath is the original endpoint: no query parameters, everything
	// filtered here.
	logsListPath = "/api/logs/"

	// logsMaxSize is the server's cap on one search page (LogSearchRequest.size).
	logsMaxSize = 1000
	// logsMaxPages bounds search_after traversal, for the same reason
	// api.MaxPages bounds ordinary pagination: an explicit limit beats an
	// unbounded loop against a cursor the server controls.
	logsMaxPages = 100
)

// logQuery is one user-expressed search, independent of which backend answers
// it. Both backends are fed from this so the two paths cannot drift into
// meaning different things by the same flags.
type logQuery struct {
	hosts    []string
	levels   []string
	text     string
	query    string
	datasets []string
	sort     string
	since    time.Time
	until    time.Time
	limit    int
}

// serverOnly names the parts of a query that only the search endpoint can
// honour. A fallback that quietly drops them would answer a question the user
// did not ask, so they are reported by name instead.
func (q logQuery) serverOnly() []string {
	var out []string
	if q.query != "" {
		out = append(out, "--query")
	}
	if len(q.datasets) > 0 {
		out = append(out, "--dataset")
	}
	if q.sort != "" && q.sort != "desc" {
		out = append(out, "--sort")
	}
	return out
}

// LogFilter narrows a set of log records.
//
// Filtering lives behind this interface because it happens in two different
// places depending on the server: in Elasticsearch when /api/logs/search/
// exists, and here, on already-fetched records, when it does not.
type LogFilter interface {
	Match(rec map[string]any) bool
	Describe() string
}

type clientFilter struct {
	hosts  []string
	text   string
	levels []string
	since  time.Time
	until  time.Time
}

func clientFilterFor(q logQuery) clientFilter {
	// --text and --query both mean "match this text"; the search endpoint
	// distinguishes a query string from a substring, this filter cannot.
	text := q.text
	if text == "" {
		text = q.query
	}
	return clientFilter{hosts: q.hosts, text: text, levels: q.levels, since: q.since, until: q.until}
}

func (f clientFilter) Match(rec map[string]any) bool {
	m := row(rec)
	if len(f.hosts) > 0 {
		h := strings.ToLower(str(m, "host", "host_name", "hostname", "source"))
		if !anyContains(h, f.hosts) {
			return false
		}
	}
	if len(f.levels) > 0 {
		l := str(m, "level", "severity", "priority")
		if !anyEqualFold(l, f.levels) {
			return false
		}
	}
	if f.text != "" {
		blob, _ := json.Marshal(rec)
		if !strings.Contains(strings.ToLower(string(blob)), strings.ToLower(f.text)) {
			return false
		}
	}
	if !f.since.IsZero() || !f.until.IsZero() {
		ts := str(m, "timestamp", "time", "datetime", "created", "@timestamp")
		parsed, ok := parseLogTime(ts)
		if ok && !f.since.IsZero() && parsed.Before(f.since) {
			return false
		}
		if ok && !f.until.IsZero() && parsed.After(f.until) {
			return false
		}
	}
	return true
}

func anyContains(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

func anyEqualFold(s string, candidates []string) bool {
	for _, c := range candidates {
		if strings.EqualFold(s, c) {
			return true
		}
	}
	return false
}

func (f clientFilter) Describe() string {
	var parts []string
	if len(f.hosts) > 0 {
		parts = append(parts, "host~"+strings.Join(f.hosts, "|"))
	}
	if len(f.levels) > 0 {
		parts = append(parts, "level="+strings.Join(f.levels, "|"))
	}
	if f.text != "" {
		parts = append(parts, "text~"+f.text)
	}
	if !f.since.IsZero() {
		parts = append(parts, "since "+f.since.Format(time.RFC3339))
	}
	if !f.until.IsZero() {
		parts = append(parts, "until "+f.until.Format(time.RFC3339))
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

// parseLogBound accepts either a duration back from now ("1h") or an absolute
// timestamp, because an operator bounding an incident window has one or the
// other in hand and should not have to convert.
func parseLogBound(flag, v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		return time.Now().Add(-d), nil
	}
	if t, ok := parseLogTime(v); ok {
		return t, nil
	}
	return time.Time{}, errs.Usage("invalid %s %q", flag, v).
		WithHint("use a duration like 1h or 30m, or a timestamp like 2026-09-04T08:00:00Z")
}

// --- backend selection ---------------------------------------------------

// logBackend is which log API a server turned out to have.
type logBackend int

const (
	logBackendAuto   logBackend = iota // not probed yet
	logBackendSearch                   // POST /api/logs/search/, filtered in Elasticsearch
	logBackendLegacy                   // GET /api/logs/, filtered here
)

func (b logBackend) String() string {
	if b == logBackendSearch {
		return "server"
	}
	return "client"
}

// parseLogBackend maps the --search-mode flag onto a backend.
func parseLogBackend(mode string) (logBackend, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "auto":
		return logBackendAuto, nil
	case "server":
		return logBackendSearch, nil
	case "client", "local":
		return logBackendLegacy, nil
	}
	return logBackendAuto, errs.Usage("invalid --search-mode %q", mode).
		WithHint("use auto, server or client")
}

// logSearchAbsent reports whether an error means "this server has no
// Elasticsearch log endpoint" rather than "the search failed".
//
// A server predating the endpoint answers 404; one that routes the path but
// not the verb answers 405; a deployment with the feature switched off answers
// 501. Anything else - an auth failure, a rejected query, a server error - is a
// real failure. Falling back on those would answer a narrower question than
// the user asked and present the result as if it were complete.
func logSearchAbsent(err error) bool {
	switch errs.StatusOf(err) {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	}
	return false
}

// logResult reports how a search was actually answered, so the caller can say
// so rather than presenting two different things identically.
type logResult struct {
	Backend logBackend
	// Fetched counts records the server sent, which on the legacy backend is
	// larger than the number kept.
	Fetched int
	// Dropped names query parts the answering backend could not honour.
	Dropped []string
}

func newLogsCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "logs",
		Short: "Search infrastructure logs",
		Long: `Search logs for the active infrastructure.

There are two log APIs, and which one answers depends on the server:

  - POST /api/logs/search/ searches Elasticsearch. Filters, time bounds and
    ordering are applied server-side, across the whole index.
  - GET /api/logs/ is the original endpoint. It accepts no query parameters,
    so the CLI fetches records and filters them locally.

The CLI tries the search endpoint first and falls back when the server does
not have it. The fallback is never silent: it is reported, along with any
filter the older endpoint could not apply. That distinction matters, because a
client-side filter cannot match a record it never fetched - a short result
there is not evidence that nothing else matched.`,
	}
	c.AddCommand(newLogsSearchCmd(app), newLogsFollowCmd(app))
	return c
}

// searchLogs answers a query with whichever backend the server has.
func (a *App) searchLogs(q logQuery) (*output.Table, logResult, error) {
	if a.logs != logBackendLegacy {
		t, n, err := a.searchLogsRemote(q)
		if err == nil {
			a.logs = logBackendSearch
			return t, logResult{Backend: logBackendSearch, Fetched: n}, nil
		}
		if !logSearchAbsent(err) {
			return nil, logResult{}, err
		}
		if a.logs == logBackendSearch {
			// --search-mode server was asked for explicitly. Downgrading it
			// anyway would hand back a weaker answer under the name of a
			// stronger one.
			return nil, logResult{}, errs.NotFound("this server has no log search endpoint (%s)", logsSearchPath).
				WithHint("drop --search-mode server to filter locally against %s instead", logsListPath)
		}
		// Remembered for the rest of the process: `logs follow` polls, and
		// probing a missing endpoint once per tick doubles the request rate to
		// learn something that cannot have changed.
		a.logs = logBackendLegacy
	}
	t, fetched, err := a.fetchLogsLegacy(q)
	if err != nil {
		return nil, logResult{}, err
	}
	return t, logResult{Backend: logBackendLegacy, Fetched: fetched, Dropped: q.serverOnly()}, nil
}

// searchLogsRemote runs the query in Elasticsearch, paging with search_after.
func (a *App) searchLogsRemote(q logQuery) (*output.Table, int, error) {
	cl, err := a.Client()
	if err != nil {
		return nil, 0, err
	}

	body, err := a.logSearchBody(q)
	if err != nil {
		return nil, 0, err
	}

	t := newLogTable()
	fetched := 0
	var after []any

	for page := 0; ; page++ {
		if page >= logsMaxPages {
			t.Truncated = true
			break
		}
		size := logsMaxSize
		if q.limit > 0 && q.limit-fetched < size {
			size = q.limit - fetched
		}
		body["size"] = size
		if after != nil {
			body["search_after"] = after
		}

		var raw jsonRaw
		if err := func() error {
			ctx, cancel := a.Ctx()
			defer cancel()
			return cl.Do(ctx, api.Request{Method: http.MethodPost, Path: logsSearchPath, Body: body}, &raw)
		}(); err != nil {
			return nil, 0, err
		}

		hits, cursor := logHits(raw)
		if len(hits) == 0 {
			break
		}
		fetched += len(hits)
		for i, m := range decodeRows(hits) {
			addLogRow(t, m, hits[i])
		}
		if q.limit > 0 && fetched >= q.limit {
			// More pages only matter if the server had more to give.
			t.Truncated = len(hits) == size && cursor != nil
			break
		}
		// A short page means the index had nothing more to give. This also
		// terminates against a server that accepts search_after and ignores
		// it: without it, such a server would serve page one forever.
		if len(hits) < size {
			break
		}
		// A full page with no cursor leaves no defensible way to ask for the
		// next one. Saying the set was capped beats guessing.
		if cursor == nil {
			t.Truncated = true
			break
		}
		after = cursor
	}
	return t, fetched, nil
}

// logSearchBody builds a LogSearchRequest from the query and active context.
func (a *App) logSearchBody(q logQuery) (map[string]any, error) {
	body := map[string]any{}

	// The search endpoint scopes by numeric infrastructure id. An unscoped
	// search would silently widen to everything the caller can read, so the
	// scope is required rather than dropped.
	id, err := a.Resolved.RequireInfra()
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(id)
	if err != nil {
		return nil, errs.Usage("infrastructure %q is not a numeric id", id).
			WithHint("the log search endpoint scopes by id: ups context set --infra-id <id>")
	}
	body["infrastructures"] = []int{n}

	if len(q.datasets) > 0 {
		body["datasets"] = q.datasets
	}
	if text := firstNonEmpty(q.query, q.text); text != "" {
		body["query"] = text
	}
	if !q.since.IsZero() {
		body["start"] = q.since.UTC().Format(time.RFC3339)
	}
	if !q.until.IsZero() {
		body["end"] = q.until.UTC().Format(time.RFC3339)
	}
	if len(q.levels) > 0 {
		body["severity"] = q.levels
	}
	if len(q.hosts) > 0 {
		body["hosts"] = q.hosts
	}
	if q.sort != "" {
		body["sort"] = q.sort
	}
	return body, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// logHits pulls records, and a paging cursor, out of a search response.
//
// The endpoint declares its response schema as a copy of its request schema,
// which cannot be what it returns. The shape is therefore discovered rather
// than trusted: a bare array, the usual DRF page, and a raw Elasticsearch
// envelope all appear depending on the deployment, and all mean the same thing
// here.
func logHits(raw jsonRaw) ([]jsonRaw, []any) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var arr []jsonRaw
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, nil
		}
		return unwrapHits(arr), cursorOf(arr)
	}

	var env map[string]jsonRaw
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return nil, nil
	}
	var hits []jsonRaw
	for _, key := range []string{"results", "items", "records", "logs", "hits", "data"} {
		v, ok := env[key]
		if !ok {
			continue
		}
		if vt := bytes.TrimSpace(v); len(vt) > 0 && vt[0] == '[' {
			if json.Unmarshal(vt, &hits) == nil && len(hits) > 0 {
				break
			}
			continue
		}
		// Elasticsearch nests the array one level down: {"hits": {"hits": []}}.
		var inner map[string]jsonRaw
		if json.Unmarshal(v, &inner) == nil {
			if h, ok := inner["hits"]; ok && json.Unmarshal(h, &hits) == nil && len(hits) > 0 {
				break
			}
		}
	}

	cursor := cursorOf(hits)
	if v, ok := env["search_after"]; ok {
		var explicit []any
		if json.Unmarshal(v, &explicit) == nil && len(explicit) > 0 {
			cursor = explicit
		}
	}
	return unwrapHits(hits), cursor
}

// cursorOf reads the search_after value for the next page off the last hit,
// which is where Elasticsearch puts it.
func cursorOf(hits []jsonRaw) []any {
	if len(hits) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(hits[len(hits)-1], &m) != nil {
		return nil
	}
	if s, ok := m["sort"].([]any); ok && len(s) > 0 {
		return s
	}
	return nil
}

// unwrapHits flattens an Elasticsearch hit to the document it wraps, so that
// --json emits log records rather than index bookkeeping.
func unwrapHits(hits []jsonRaw) []jsonRaw {
	out := make([]jsonRaw, 0, len(hits))
	for _, h := range hits {
		var m map[string]jsonRaw
		if json.Unmarshal(h, &m) == nil {
			if src, ok := m["_source"]; ok {
				h = src
			}
		}
		out = append(out, h)
	}
	return out
}

// fetchLogsLegacy retrieves everything the old endpoint offers and filters it
// here.
func (a *App) fetchLogsLegacy(q logQuery) (*output.Table, int, error) {
	cl, err := a.Client()
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := a.Ctx()
	defer cancel()

	list, err := cl.GetList(ctx, request("GET", logsListPath, a.infraQuery(nil)), 0)
	if err != nil {
		return nil, 0, err
	}

	f := clientFilterFor(q)
	t := newLogTable()
	fetched := len(list.Items)
	kept := 0
	for i, m := range decodeRows(list.Items) {
		if !f.Match(m) {
			continue
		}
		if q.limit > 0 && kept >= q.limit {
			t.Truncated = true
			break
		}
		kept++
		addLogRow(t, m, list.Items[i])
	}
	if list.Truncated {
		t.Truncated = true
	}
	return t, fetched, nil
}

func newLogTable() *output.Table {
	return &output.Table{
		Columns: []string{"TIME", "LEVEL", "HOST", "MESSAGE"},
		Empty:   "No log records matched.",
	}
}

func addLogRow(t *output.Table, m row, raw jsonRaw) {
	t.Add(str(m, "id", "_id"), raw,
		dash(str(m, "timestamp", "time", "datetime", "created", "@timestamp")),
		dash(str(m, "level", "severity", "priority")),
		dash(str(m, "host", "host_name", "hostname", "source")),
		truncate(dash(str(m, "message", "msg", "text", "line")), 70),
	)
}

// reportLogBackend tells the user which API answered. Two backends that print
// the same table mean different things, and the weaker one must not pass for
// the stronger.
func (a *App) reportLogBackend(res logResult, q logQuery) {
	if a.AsJSON || a.IDOnly {
		return
	}
	if res.Backend == logBackendSearch {
		return
	}
	a.Printer.Infof("this server has no %s endpoint; filtered %d fetched record(s) locally (%s)",
		logsSearchPath, res.Fetched, clientFilterFor(q).Describe())
	if len(res.Dropped) > 0 {
		a.Printer.Infof("note: %s %s no effect against %s and %s ignored",
			strings.Join(res.Dropped, ", "),
			plural(len(res.Dropped), "has", "have"),
			logsListPath,
			plural(len(res.Dropped), "was", "were"))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func newLogsSearchCmd(app *App) *cobra.Command {
	var q logQuery
	var since, until, mode string
	c := &cobra.Command{
		Use:     "search",
		Aliases: []string{"ls", "list"},
		Short:   "Search logs",
		Example: `  ups logs search --since 1h
  ups logs search --host core-sw-01 --text "link down"
  ups logs search --dataset syslog --level error --since 24h --sort asc
  ups logs search --query 'message:"link down" AND severity:error' --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			backend, err := parseLogBackend(mode)
			if err != nil {
				return err
			}
			app.logs = backend

			if q.since, err = parseLogBound("--since", since); err != nil {
				return err
			}
			if q.until, err = parseLogBound("--until", until); err != nil {
				return err
			}
			if err := validateDatasets(q.datasets); err != nil {
				return err
			}
			if q.sort != "asc" && q.sort != "desc" {
				return errs.Usage("invalid --sort %q", q.sort).WithHint("use asc or desc")
			}
			q.limit = app.Limit

			var t *output.Table
			var res logResult
			if err := app.Spin("Searching logs", func() error {
				var e error
				t, res, e = app.searchLogs(q)
				return e
			}); err != nil {
				return err
			}
			if err := app.Printer.Print(t); err != nil {
				return err
			}
			app.reportLogBackend(res, q)
			return nil
		},
	}
	c.Flags().StringArrayVar(&q.hosts, "host", nil, "match host (repeatable; substring when filtered locally)")
	c.Flags().StringVar(&q.text, "text", "", "match free text (an Elasticsearch query string server-side)")
	c.Flags().StringArrayVar(&q.levels, "level", nil, "match log level (repeatable)")
	c.Flags().StringArrayVar(&q.datasets, "dataset", nil, "limit to a dataset: flow, monitoring or syslog (repeatable)")
	c.Flags().StringVar(&q.query, "query", "", "Elasticsearch query string (server-side search only)")
	c.Flags().StringVar(&q.sort, "sort", "desc", "order by time: asc or desc (server-side search only)")
	c.Flags().StringVar(&since, "since", "", "only records newer than this duration or timestamp")
	c.Flags().StringVar(&until, "until", "", "only records older than this duration or timestamp")
	c.Flags().StringVar(&mode, "search-mode", "auto", "which log API to use: auto, server or client")
	return c
}

func validateDatasets(datasets []string) error {
	for _, d := range datasets {
		switch d {
		case "flow", "monitoring", "syslog":
		default:
			return errs.Usage("invalid --dataset %q", d).
				WithHint("use flow, monitoring or syslog")
		}
	}
	return nil
}

func newLogsFollowCmd(app *App) *cobra.Command {
	var q logQuery
	var mode string
	var interval time.Duration
	c := &cobra.Command{
		Use:   "follow",
		Short: "Poll for new log records",
		Long: `Follow logs.

There is no log stream in the API, so this polls and redraws. Ctrl-C stops it.

Which backend answers is decided once, on the first poll, and reused: probing
for an endpoint the server does not have, once per tick, doubles the request
rate to learn something that cannot have changed.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if app.AsJSON || app.IDOnly {
				return errs.Usage("follow renders a live table and cannot emit --json").
					WithHint("poll instead: watch -n5 'ups logs search --json'")
			}
			backend, err := parseLogBackend(mode)
			if err != nil {
				return err
			}
			app.logs = backend
			if err := validateDatasets(q.datasets); err != nil {
				return err
			}
			q.limit = app.Limit

			sigc := make(chan os.Signal, 1)
			signal.Notify(sigc, os.Interrupt)
			defer signal.Stop(sigc)

			theme := app.Theme()
			for {
				q.since = time.Now().Add(-interval * 2)
				t, res, err := app.searchLogs(q)
				if err != nil {
					return err
				}
				fmt.Fprint(app.Stdout, "\x1b[2J\x1b[H")
				fmt.Fprintf(app.Stdout, "%s  %s\n\n", theme.Bold.Apply("Logs"),
					theme.Dim.Apply(fmt.Sprintf("%s - %s-side filter - ctrl-c to stop",
						time.Now().Format("15:04:05"), res.Backend)))
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
	c.Flags().StringArrayVar(&q.hosts, "host", nil, "match host (repeatable)")
	c.Flags().StringVar(&q.text, "text", "", "match free text")
	c.Flags().StringArrayVar(&q.levels, "level", nil, "match log level (repeatable)")
	c.Flags().StringArrayVar(&q.datasets, "dataset", nil, "limit to a dataset: flow, monitoring or syslog (repeatable)")
	c.Flags().StringVar(&mode, "search-mode", "auto", "which log API to use: auto, server or client")
	c.Flags().DurationVar(&interval, "interval", 10*time.Second, "poll interval")
	return c
}
