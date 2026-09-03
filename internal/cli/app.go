// Package cli wires commands to the config, transport and output layers.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/upstacked/cli/internal/api"
	"github.com/upstacked/cli/internal/config"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/output"
	"github.com/upstacked/cli/internal/ui"
)

// Version is set at build time.
var Version = "dev"

// App is the per-invocation environment shared by all commands.
type App struct {
	Store    *config.Store
	File     *config.File
	Resolved *config.Resolved
	Printer  *output.Printer

	// Stdin stays a file because reading a secret without echo needs a fd.
	Stdin  *os.File
	Stdout io.Writer
	Stderr io.Writer

	// Global flags.
	ConfigDir  string
	ProfileArg string
	APIURLArg  string
	InfraArg   string
	CustArg    string
	AsJSON     bool
	IDOnly     bool
	Timeout    time.Duration
	Limit      int
	AssumeYes  bool
	DryRun     bool
	Debug      bool

	client *api.Client
	theme  *ui.Theme
	sym    ui.Symbols
	// logs remembers which log API this server has, so a polling command does
	// not re-probe a missing endpoint on every tick.
	logs logBackend
}

// Theme returns the colour theme for stderr-facing UI.
func (a *App) Theme() *ui.Theme {
	if a.theme == nil {
		a.theme = ui.NewTheme(a.Stderr)
	}
	return a.theme
}

// Sym returns status glyphs appropriate to the terminal.
func (a *App) Sym() ui.Symbols {
	if a.sym.OK == "" {
		a.sym = ui.NewSymbols(ui.UnicodeOK())
	}
	return a.sym
}

// Spin runs fn behind a spinner on stderr. The spinner is silent when stderr
// is not a terminal, so piped and CI output stay clean.
func (a *App) Spin(message string, fn func() error) error {
	if a.AsJSON || a.IDOnly {
		return fn()
	}
	sp := ui.NewSpinner(a.Stderr, message)
	sp.Start()
	err := fn()
	sp.Stop()
	return err
}

// Load reads config and resolves the effective settings.
func (a *App) Load() error {
	store, err := config.NewStore(a.ConfigDir)
	if err != nil {
		return err
	}
	a.Store = store

	f, err := store.LoadConfig()
	if err != nil {
		return err
	}
	a.File = f

	r, err := config.Resolve(f, config.Overrides{
		Profile:      a.ProfileArg,
		APIURLFlag:   a.APIURLArg,
		APIURLEnv:    os.Getenv("UPSTACKED_API_URL"),
		InfraFlag:    a.InfraArg,
		InfraEnv:     os.Getenv("UPSTACKED_INFRASTRUCTURE"),
		CustomerFlag: a.CustArg,
		CustomerEnv:  os.Getenv("UPSTACKED_CUSTOMER"),
	})
	if err != nil {
		return err
	}
	a.Resolved = r

	format := output.FormatTable
	switch {
	case a.IDOnly:
		format = output.FormatIDOnly
	case a.AsJSON:
		format = output.FormatJSON
	}
	a.Printer = output.New(a.Stdout, a.Stderr, format)
	return nil
}

// Client builds an authenticated API client for the active profile.
func (a *App) Client() (*api.Client, error) {
	if a.client != nil {
		return a.client, nil
	}
	baseURL, err := a.Resolved.RequireAPIURL()
	if err != nil {
		return nil, err
	}
	c := api.New(baseURL, a.Timeout)
	if a.Debug {
		c.Debug = a.Stderr
	}

	// A token from the environment is used as-is; CI has no config file (A2).
	if tok := os.Getenv("UPSTACKED_TOKEN"); tok != "" {
		c.Tokens = &api.StaticTokens{AccessToken: tok}
		a.client = c
		return c, nil
	}

	creds, err := a.Store.LoadCredentials()
	if err != nil {
		return nil, err
	}
	profile := a.Resolved.ProfileName
	cred, ok := creds.For(profile, baseURL)
	if !ok {
		if stored, exists := creds.Stored(profile); exists {
			// The safeguard, reported as such rather than as a generic failure.
			return nil, errs.Auth("stored credentials for profile %q were issued by %s, not %s",
				profile, stored.APIURL, baseURL).
				WithHint("this is deliberate: tokens are never sent to a different server. Run: ups login")
		}
		return nil, errs.Auth("not logged in to %s", baseURL).
			WithHint("run: ups login   (or set UPSTACKED_TOKEN for non-interactive use)")
	}

	c.Tokens = &api.StaticTokens{
		AccessToken:  cred.Access,
		RefreshToken: cred.Refresh,
		OnRefresh: func(access string) error {
			cs, err := a.Store.LoadCredentials()
			if err != nil {
				return err
			}
			if existing, ok := cs.Stored(profile); ok {
				existing.Access = access
				cs.Set(profile, existing)
				return a.Store.SaveCredentials(cs)
			}
			return nil
		},
	}
	a.client = c
	return c, nil
}

// Ctx returns a context bounded by the timeout flag.
func (a *App) Ctx() (context.Context, context.CancelFunc) {
	t := a.Timeout
	if t <= 0 {
		t = api.DefaultTimeout
	}
	return context.WithTimeout(context.Background(), t)
}

// Interactive reports whether prompting is possible. Non-TTY stdin must never
// be prompted; the command fails with a clear message instead (X4).
func (a *App) Interactive() bool {
	if a.Stdin == nil {
		return false
	}
	return ui.IsTerminalFd(int(a.Stdin.Fd()))
}

// Confirm asks before a destructive action.
func (a *App) Confirm(prompt string) error {
	if a.AssumeYes {
		return nil
	}
	if !a.Interactive() {
		return errs.Usage("%s requires confirmation, but stdin is not a terminal", prompt).
			WithHint("pass --yes to proceed non-interactively")
	}
	fmt.Fprintf(a.Stderr, "%s [y/N]: ", prompt)
	r := bufio.NewReader(a.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return errs.General("cannot read confirmation").Wrapping(err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return errs.General("aborted")
}

// ReadSecret takes a secret from stdin, never from argv (X7).
func (a *App) ReadSecret(prompt string) (string, error) {
	if !a.Interactive() {
		r := bufio.NewReader(a.Stdin)
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return "", errs.Usage("no value on stdin for %s", prompt).
				WithHint("pipe it in, e.g.: printf '%%s' \"$SECRET\" | ups ...")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprintf(a.Stderr, "%s: ", prompt)
	secret, err := readPasswordNoEcho(int(a.Stdin.Fd()))
	fmt.Fprintln(a.Stderr)
	if err != nil {
		return "", errs.General("cannot read %s", prompt).Wrapping(err)
	}
	return secret, nil
}

// Prompt reads a visible line of input.
func (a *App) Prompt(label, def string) (string, error) {
	if !a.Interactive() {
		return def, nil
	}
	if def != "" {
		fmt.Fprintf(a.Stderr, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(a.Stderr, "%s: ", label)
	}
	line, err := bufio.NewReader(a.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errs.General("cannot read input").Wrapping(err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

// --- generic JSON row helpers -------------------------------------------
//
// Responses are handled as generic JSON rather than 40 hand-written structs.
// Several endpoints in the spec declare no response schema, so a permissive
// representation is both simpler and more honest than types that pretend to
// know the shape.

type row map[string]any

// jsonRaw aliases the transport's raw message type for the command layer.
type jsonRaw = json.RawMessage

// request builds an api.Request without importing api at every call site.
func request(method, path string, q url.Values) api.Request {
	return api.Request{Method: method, Path: path, Query: q}
}

func decodeRows(items []json.RawMessage) []row {
	out := make([]row, 0, len(items))
	for _, it := range items {
		var m row
		if err := json.Unmarshal(it, &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

func str(m row, keys ...string) string {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case string:
			if t != "" {
				return t
			}
		case float64:
			if t == float64(int64(t)) {
				return strconv.FormatInt(int64(t), 10)
			}
			return strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			return strconv.FormatBool(t)
		case map[string]any:
			if n, ok := t["name"].(string); ok && n != "" {
				return n
			}
			if n, ok := t["title"].(string); ok && n != "" {
				return n
			}
		case []any:
			if len(t) > 0 {
				return fmt.Sprintf("%d", len(t))
			}
		}
	}
	return ""
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// listOpts describes a generic list command.
type listOpts struct {
	Path    string
	Query   url.Values
	Columns []string
	// Cells maps a record to display cells; must return len(Columns) values.
	Cells func(row) []string
	Empty string
}

// runList fetches, renders and reports truncation for a list endpoint.
func (a *App) runList(o listOpts) error {
	c, err := a.Client()
	if err != nil {
		return err
	}
	ctx, cancel := a.Ctx()
	defer cancel()

	list, err := c.GetList(ctx, api.Request{Method: "GET", Path: o.Path, Query: o.Query}, a.Limit)
	if err != nil {
		return err
	}
	t := &output.Table{Columns: o.Columns, Truncated: list.Truncated, Total: list.Count, Empty: o.Empty}
	for i, m := range decodeRows(list.Items) {
		t.Add(str(m, "id"), list.Items[i], o.Cells(m)...)
	}
	return a.Printer.Print(t)
}

// getOne fetches a single record.
func (a *App) getOne(path string, q url.Values) (row, json.RawMessage, error) {
	c, err := a.Client()
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := a.Ctx()
	defer cancel()
	var raw json.RawMessage
	if err := c.Do(ctx, api.Request{Method: "GET", Path: path, Query: q}, &raw); err != nil {
		return nil, nil, err
	}
	var m row
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, raw, errs.General("unexpected response from %s", path).Wrapping(err)
	}
	return m, raw, nil
}

// mutate performs a write, honouring --dry-run.
func (a *App) mutate(method, path string, body any, out any) error {
	if a.DryRun {
		b, _ := json.MarshalIndent(body, "", "  ")
		a.Printer.Infof("dry-run: %s %s", method, path)
		if body != nil {
			a.Printer.Infof("%s", string(b))
		}
		return nil
	}
	c, err := a.Client()
	if err != nil {
		return err
	}
	ctx, cancel := a.Ctx()
	defer cancel()
	return c.Do(ctx, api.Request{Method: method, Path: path, Body: body}, out)
}

// infraQuery adds the active infrastructure to a query when set.
func (a *App) infraQuery(extra url.Values) url.Values {
	q := url.Values{}
	for k, v := range extra {
		q[k] = v
	}
	if a.Resolved.Infrastructure.IsSet() {
		q.Set("infrastructure", a.Resolved.Infrastructure.Value)
	}
	return q
}

// jsonUnmarshal is a thin wrapper so command files need not import encoding/json.
func jsonUnmarshal(raw json.RawMessage, out any) error { return json.Unmarshal(raw, out) }

// fetchRows retrieves a list endpoint as decoded rows for commands that need to
// reason about the records rather than print them.
func (a *App) fetchRows(path string, q url.Values) ([]row, error) {
	c, err := a.Client()
	if err != nil {
		return nil, err
	}
	ctx, cancel := a.Ctx()
	defer cancel()
	list, err := c.GetList(ctx, api.Request{Method: "GET", Path: path, Query: q}, a.Limit)
	if err != nil {
		return nil, err
	}
	return decodeRows(list.Items), nil
}
