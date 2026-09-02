package cli

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/api"
	"github.com/upstacked/cli/internal/config"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/skill"
)

// checkStatus is a diagnosis outcome.
type checkStatus string

const (
	statusPass checkStatus = "pass"
	statusWarn checkStatus = "warn"
	statusFail checkStatus = "fail"
	statusSkip checkStatus = "skip"
)

// check is one independent diagnosis. Checks never mutate anything.
type check struct {
	Name   string      `json:"name"`
	Status checkStatus `json:"status"`
	Detail string      `json:"detail"`
	Fix    string      `json:"fix,omitempty"`
}

func newDoctorCmd(app *App) *cobra.Command {
	var scope string
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the local setup",
		Long: `Check configuration, authentication, context and the agent skill.

Every check runs, so a single run reports everything that is wrong rather
than stopping at the first problem. doctor never changes anything; it exits
non-zero if any check fails, which makes it usable as a CI gate.

This checks your local setup. It is unrelated to 'ups infra healthcheck',
which starts a scan of a customer infrastructure on the platform.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			checks := app.runChecks(skill.Scope(scope))

			if app.AsJSON {
				failed := 0
				for _, c := range checks {
					if c.Status == statusFail {
						failed++
					}
				}
				if err := app.Printer.JSON(map[string]any{
					"checks": checks, "failed": failed, "ok": failed == 0,
				}); err != nil {
					return err
				}
				if failed > 0 {
					return errs.General("%d check(s) failed", failed)
				}
				return nil
			}
			return app.renderChecks(checks)
		},
	}
	scopeFlag(c, &scope)
	return c
}

// runChecks performs every diagnosis in order, independently.
func (a *App) runChecks(scope skill.Scope) []check {
	var out []check
	r := a.Resolved

	// 1. Config file.
	cfgPath := a.Store.ConfigPath()
	if _, err := os.Stat(cfgPath); err != nil {
		out = append(out, check{"config file", statusWarn,
			"no config file yet (" + cfgPath + ")", "run: ups init"})
	} else {
		out = append(out, check{"config file", statusPass, cfgPath, ""})
	}

	// 2. Credential file permissions.
	if mode, err := config.FileMode(a.Store.CredsPath()); err == nil {
		if mode&0o077 != 0 {
			out = append(out, check{"credential permissions", statusFail,
				fmt.Sprintf("%s is %o; it is readable by other users", a.Store.CredsPath(), mode),
				fmt.Sprintf("chmod 600 %s", a.Store.CredsPath())})
		} else {
			out = append(out, check{"credential permissions", statusPass, "0600", ""})
		}
	}

	// 3. Server URL.
	if !r.APIURL.IsSet() {
		out = append(out, check{"server url", statusFail, "no server configured",
			"run: ups init --api-url https://your-upstacked-host"})
	} else {
		out = append(out, check{"server url", statusPass, r.APIURL.Describe(), ""})
	}

	// 4. Credentials present and bound to this server.
	creds, cerr := a.Store.LoadCredentials()
	envToken := os.Getenv("UPSTACKED_TOKEN") != ""
	switch {
	case cerr != nil:
		out = append(out, check{"credentials", statusFail, cerr.Error(), "run: ups login"})
	case envToken:
		out = append(out, check{"credentials", statusPass, "using UPSTACKED_TOKEN from the environment", ""})
	default:
		cred, ok := creds.For(r.ProfileName, r.APIURL.Value)
		switch {
		case ok:
			who := cred.Username
			if who == "" {
				who = "token stored"
			}
			out = append(out, check{"credentials", statusPass, who, ""})
			// 5. Expiry.
			//
			// A short-lived access token is not a problem when a refresh token
			// is stored: renewal is transparent. Some servers issue tokens that
			// last minutes, and warning on every run would train the user to
			// ignore doctor. Only an expiry that the CLI cannot recover from
			// is worth reporting.
			if exp, found := api.JWTExpiry(cred.Access); found {
				d := time.Until(exp)
				switch {
				case cred.Refresh != "":
					detail := fmt.Sprintf("access token valid for %s, renews automatically", d.Round(time.Minute))
					if d <= 0 {
						detail = "access token expired; it will be renewed on next use"
					}
					out = append(out, check{"token expiry", statusPass, detail, ""})
				case d <= 0:
					out = append(out, check{"token expiry", statusFail,
						"the access token has expired and there is no refresh token", "run: ups login"})
				case d < time.Hour:
					out = append(out, check{"token expiry", statusWarn,
						fmt.Sprintf("expires in %s and there is no refresh token", d.Round(time.Minute)),
						"run: ups login"})
				default:
					out = append(out, check{"token expiry", statusPass,
						fmt.Sprintf("valid for %s", d.Round(time.Minute)), ""})
				}
			}
		default:
			if stored, exists := creds.Stored(r.ProfileName); exists {
				out = append(out, check{"credentials", statusFail,
					fmt.Sprintf("stored token was issued by %s, not %s", stored.APIURL, r.APIURL.Value),
					"this is the host-binding safeguard. Run: ups login"})
			} else {
				out = append(out, check{"credentials", statusFail,
					"not logged in to " + r.APIURL.Value, "run: ups login"})
			}
		}
	}

	// 6. Reachability and identity, in one authenticated call.
	//
	// Deliberately NOT /api/healthcheck/start/ - that starts a platform-side
	// scan of a customer infrastructure. /api/status/ is ticket statuses.
	if r.APIURL.IsSet() {
		out = append(out, a.checkReachable())
	} else {
		out = append(out, check{"server reachable", statusSkip, "no server configured", ""})
	}

	// 7. Active infrastructure resolves.
	switch {
	case !r.Infrastructure.IsSet():
		out = append(out, check{"infrastructure", statusWarn,
			"no default infrastructure selected", "run: ups context set"})
	default:
		out = append(out, a.checkInfra())
	}

	// 8-10. Agent skill.
	out = append(out, a.checkSkill(scope)...)

	return out
}

func (a *App) checkReachable() check {
	c, err := a.Client()
	if err != nil {
		return check{"server reachable", statusSkip, err.Error(), errs.HintOf(err)}
	}
	ctx, cancel := a.Ctx()
	defer cancel()
	var raw map[string]any
	err = c.Do(ctx, api.Request{Method: http.MethodGet, Path: "/api/user/details/v2/"}, &raw)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeNotFound {
			if e2 := c.Do(ctx, api.Request{Method: http.MethodGet, Path: "/api/user/details/"}, &raw); e2 == nil {
				return check{"server reachable", statusPass, "authenticated (v1 user endpoint)", ""}
			}
		}
		return check{"server reachable", statusFail, err.Error(), errs.HintOf(err)}
	}
	who := str(row(raw), "username", "email")
	if who == "" {
		who = "authenticated"
	}
	return check{"server reachable", statusPass, who, ""}
}

func (a *App) checkInfra() check {
	id := a.Resolved.Infrastructure.Value
	m, _, err := a.getOne("/api/infrastructure/"+id+"/", nil)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeNotFound {
			return check{"infrastructure", statusFail,
				fmt.Sprintf("infrastructure %s does not exist on this server", id),
				"pick another: ups context set"}
		}
		return check{"infrastructure", statusWarn, err.Error(), ""}
	}
	return check{"infrastructure", statusPass,
		fmt.Sprintf("%s %s", id, str(m, "name", "infra_code")), ""}
}

// checkSkill reports every client the skill is installed into, in either
// scope. Checking only one client would miss a stale copy in another, which is
// exactly the copy that would mislead an agent.
func (a *App) checkSkill(scope skill.Scope) []check {
	states := skill.InstalledStates("", Version)
	if len(states) == 0 {
		return []check{{"agent skill", statusFail, "not installed for any LLM client",
			"run: ups skill install"}}
	}

	var out []check
	for _, st := range states {
		name := "skill: " + st.Target.ID
		switch {
		case st.Modified:
			// A warning, never a failure: the user may have customised it on
			// purpose, and doctor must not imply their edits will be discarded.
			out = append(out, check{name, statusWarn,
				"locally edited at " + st.Path,
				"keep them, or overwrite with: ups skill install --client " + st.Target.ID + " --force"})
		case st.Outdated:
			out = append(out, check{name, statusFail,
				fmt.Sprintf("written by %s, this CLI is %s - the guidance may not match the commands",
					dash(st.Version), Version),
				"run: ups skill install --client " + st.Target.ID + " --force"})
		default:
			out = append(out, check{name, statusPass,
				string(st.Scope) + " - " + st.Path, ""})
		}
	}
	return out
}

// renderChecks prints all results, then a summary line.
func (a *App) renderChecks(checks []check) error {
	t, sym := a.Theme(), a.Sym()
	fmt.Fprintln(a.Stdout)
	var failed, warned int
	for _, c := range checks {
		var glyph string
		switch c.Status {
		case statusPass:
			glyph = t.Green.Apply(sym.OK)
		case statusWarn:
			glyph = t.Yellow.Apply(sym.Warn)
			warned++
		case statusFail:
			glyph = t.Red.Apply(sym.Fail)
			failed++
		default:
			glyph = t.Dim.Apply("-")
		}
		fmt.Fprintf(a.Stdout, " %s %-24s %s\n", glyph, c.Name, c.Detail)
		if c.Fix != "" && c.Status != statusPass {
			fmt.Fprintf(a.Stdout, "   %s %s\n", t.Dim.Apply(sym.Arrow), t.Dim.Apply(c.Fix))
		}
	}
	fmt.Fprintln(a.Stdout)

	switch {
	case failed > 0:
		return errs.General("%d check(s) failed, %d warning(s)", failed, warned)
	case warned > 0:
		fmt.Fprintf(a.Stdout, "%s %d warning(s), nothing broken.\n", t.Yellow.Apply(sym.Warn), warned)
	default:
		fmt.Fprintf(a.Stdout, "%s Everything checks out.\n", t.Green.Apply(sym.OK))
	}
	return nil
}
