package cli

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/api"
	"github.com/upstacked/cli/internal/config"
	"github.com/upstacked/cli/internal/errs"
)

func newLoginCmd(app *App) *cobra.Command {
	var username, passwordFile string
	var passwordStdin bool

	c := &cobra.Command{
		Use:   "login",
		Short: "Authenticate against an Upstacked server",
		Long: `Authenticate and store a token for the active profile.

The password is never taken from a command-line flag: argv is visible to
other processes, lands in shell history, and in CI ends up in build logs.
Use the interactive prompt, --password-stdin, or --password-file.`,
		Example: `  ups login
  ups login --username alice
  printf '%s' "$PASS" | ups login --username alice --password-stdin`,
		RunE: func(cmd *cobra.Command, args []string) error {
			baseURL, err := app.Resolved.RequireAPIURL()
			if err != nil {
				return err
			}

			if username == "" {
				if !app.Interactive() {
					return errs.Usage("no username given and stdin is not a terminal").
						WithHint("pass --username")
				}
				username, err = app.Prompt("Username", "")
				if err != nil {
					return err
				}
			}
			if username == "" {
				return errs.Usage("username is required")
			}

			password, err := readPassword(app, passwordStdin, passwordFile)
			if err != nil {
				return err
			}

			client := api.New(baseURL, app.Timeout)
			if app.Debug {
				client.Debug = app.Stderr
			}
			ctx, cancel := app.Ctx()
			defer cancel()

			var res *api.LoginResult
			err = app.Spin(fmt.Sprintf("Authenticating with %s", baseURL), func() error {
				var e error
				res, e = api.Login(ctx, client, username, password)
				return e
			})
			if err != nil {
				return err
			}

			creds, err := app.Store.LoadCredentials()
			if err != nil {
				return err
			}
			creds.Set(app.Resolved.ProfileName, &config.Credential{
				APIURL:   baseURL,
				Access:   res.AccessToken(),
				Refresh:  res.Refresh,
				Username: username,
				Obtained: time.Now().UTC(),
			})
			if err := app.Store.SaveCredentials(creds); err != nil {
				return err
			}

			// Persist the server on the profile so the next run needs no flag.
			if err := app.persistProfileURL(baseURL); err != nil {
				return err
			}

			t, sym := app.Theme(), app.Sym()
			fmt.Fprintf(app.Stderr, "%s Logged in to %s as %s (profile %q)\n",
				t.Green.Apply(sym.OK), baseURL, username, app.Resolved.ProfileName)
			if !app.Resolved.Infrastructure.IsSet() {
				fmt.Fprintf(app.Stderr, "  %s pick a default infrastructure: ups context set\n",
					t.Dim.Apply("next:"))
			}
			return nil
		},
	}
	c.Flags().StringVarP(&username, "username", "u", "", "username")
	c.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from stdin")
	c.Flags().StringVar(&passwordFile, "password-file", "", "read the password from a file")
	return c
}

// readPassword sources a secret without ever accepting it from argv.
func readPassword(app *App, fromStdin bool, file string) (string, error) {
	switch {
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", errs.Usage("cannot read password file %s", file).Wrapping(err)
		}
		return trimNewline(string(b)), nil
	case fromStdin || !app.Interactive():
		s, err := app.ReadSecret("Password")
		if err != nil {
			return "", err
		}
		if s == "" {
			return "", errs.Usage("no password received on stdin").
				WithHint("printf '%%s' \"$PASS\" | ups login --username <user> --password-stdin")
		}
		return s, nil
	default:
		return app.ReadSecret("Password")
	}
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// persistProfileURL records the server on the active profile.
func (a *App) persistProfileURL(baseURL string) error {
	name := a.Resolved.ProfileName
	if a.File.Profiles == nil {
		a.File.Profiles = map[string]*config.Profile{}
	}
	p := a.File.Profiles[name]
	if p == nil {
		p = &config.Profile{}
		a.File.Profiles[name] = p
	}
	p.APIURL = baseURL
	if a.File.CurrentProfile == "" {
		a.File.CurrentProfile = name
	}
	return a.Store.SaveConfig(a.File)
}

func newLogoutCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Discard stored credentials for the active profile",
		RunE: func(cmd *cobra.Command, args []string) error {
			creds, err := app.Store.LoadCredentials()
			if err != nil {
				return err
			}
			name := app.Resolved.ProfileName
			if _, ok := creds.Stored(name); !ok {
				app.Printer.Infof("No credentials stored for profile %q.", name)
				return nil
			}
			creds.Delete(name)
			if err := app.Store.SaveCredentials(creds); err != nil {
				return err
			}
			t, sym := app.Theme(), app.Sym()
			fmt.Fprintf(app.Stderr, "%s Logged out of profile %q\n", t.Green.Apply(sym.OK), name)
			return nil
		},
	}
}

func newWhoamiCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the authenticated user, roles and feature flags",
		Long: `Show who you are on the active server.

Use this when a command is denied: it reports the roles actually granted,
which is usually the answer to "why can't I do this".`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := app.Client()
			if err != nil {
				return err
			}
			ctx, cancel := app.Ctx()
			defer cancel()

			var raw map[string]any
			if err := c.Do(ctx, api.Request{Method: http.MethodGet, Path: "/api/user/details/v2/"}, &raw); err != nil {
				// Fall back to the v1 shape on servers that lack v2.
				if errs.CodeOf(err) == errs.CodeNotFound {
					err = c.Do(ctx, api.Request{Method: http.MethodGet, Path: "/api/user/details/"}, &raw)
				}
				if err != nil {
					return err
				}
			}
			if app.AsJSON {
				return app.Printer.JSON(raw)
			}

			// The real response nests the identity under "user", with roles and
			// organisations alongside it. Field names were confirmed against a
			// live server, not inferred from the spec.
			user := row{}
			if u, ok := raw["user"].(map[string]any); ok {
				user = row(u)
			} else {
				user = row(raw)
			}
			name := strings.TrimSpace(str(user, "first_name") + " " + str(user, "last_name"))

			fields := [][2]string{
				{"Server", app.Resolved.APIURL.Value},
				{"Profile", app.Resolved.ProfileName},
				{"User", dash(str(user, "username", "mail"))},
				{"Name", dash(name)},
				{"Email", dash(str(user, "mail", "email"))},
				{"Type", dash(str(user, "user_type"))},
				{"Active", dash(str(user, "is_active"))},
			}
			for _, line := range describeRoles(raw) {
				fields = append(fields, line)
			}
			return app.Printer.Object(nil, fields)
		},
	}
}

// describeRoles flattens user_roles and organizations into display lines. An
// agent or operator asking "why can't I do this" needs the scope of each role,
// not just its name.
func describeRoles(raw map[string]any) [][2]string {
	var out [][2]string
	roles, _ := raw["user_roles"].([]any)
	for i, r := range roles {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		scope := str(row(m), "infrastructure_name")
		if scope == "" {
			scope = str(row(m), "customer_name")
		}
		if scope == "" {
			scope = str(row(m), "organization_name")
		}
		label := "Role"
		if i > 0 {
			label = ""
		}
		out = append(out, [2]string{label,
			dash(str(row(m), "role_description")) + "  " + dash(scope)})
	}

	orgs, _ := raw["organizations"].(map[string]any)
	first := true
	for _, v := range orgs {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		label := "Organization"
		if !first {
			label = ""
		}
		first = false
		line := dash(str(row(m), "name"))
		if cs, ok := m["customers"].([]any); ok && len(cs) > 0 {
			line += fmt.Sprintf("  (%d customer(s))", len(cs))
		}
		out = append(out, [2]string{label, line})
	}
	return out
}
