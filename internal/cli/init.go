package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/config"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/skill"
	"github.com/upstacked/cli/internal/ui"
)

func newInitCmd(app *App) *cobra.Command {
	var (
		clients     []string
		apiURL      string
		username    string
		scope       string
		skillOnly   bool
		noSkill     bool
		nonInteract bool
		force       bool
	)

	c := &cobra.Command{
		Use:   "init",
		Short: "Set up the CLI: server, login, context and agent skill",
		Long: `Get from nothing to working.

init only orchestrates; every step is also available on its own as
'ups login', 'ups context set' and 'ups skill install'. Re-running it is
safe: steps that are already done are reported and skipped.`,
		Example: `  ups init
  ups init --api-url staging
  ups init --skill-only
  ups init --api-url staging --username alice --non-interactive`,
		RunE: func(cmd *cobra.Command, args []string) error {
			t, sym := app.Theme(), app.Sym()
			interactive := app.Interactive() && !nonInteract

			if skillOnly {
				return installSkillStep(app, scope, clients, force)
			}

			fmt.Fprintf(app.Stderr, "\n%s\n", t.Bold.Apply("Setting up the Upstacked CLI"))

			// Step 1: server.
			target := apiURL
			if target == "" {
				target = app.Resolved.APIURL.Value
			}
			if target == "" {
				if !interactive {
					return errs.Usage("no server configured and this is a non-interactive run").
						WithHint("pass --api-url <url|%s>", joinAliases())
				}
				var err error
				target, err = promptServer(app)
				if err != nil {
					return err
				}
			}
			normalized, err := config.NormalizeURL(target)
			if err != nil {
				return err
			}
			if err := app.persistProfileURL(normalized); err != nil {
				return err
			}
			// Re-resolve so later steps see the stored server.
			if err := app.Load(); err != nil {
				return err
			}
			fmt.Fprintf(app.Stderr, " %s server %s\n", t.Green.Apply(sym.OK), normalized)

			// Step 2: authentication.
			if _, err := app.Client(); err != nil {
				if !interactive && username == "" {
					return errs.Auth("not logged in to %s", normalized).
						WithHint("run: ups login --username <user> --password-stdin")
				}
				fmt.Fprintf(app.Stderr, " %s not logged in\n", t.Yellow.Apply(sym.Warn))
				login := newLoginCmd(app)
				loginArgs := []string{}
				if username != "" {
					loginArgs = append(loginArgs, "--username", username)
				}
				login.SetArgs(loginArgs)
				login.SetIn(app.Stdin)
				login.SetOut(app.Stderr)
				login.SetErr(app.Stderr)
				if err := login.Execute(); err != nil {
					return err
				}
				app.client = nil
			} else {
				fmt.Fprintf(app.Stderr, " %s already authenticated\n", t.Green.Apply(sym.OK))
			}

			// Step 3: context.
			if !app.Resolved.Infrastructure.IsSet() {
				if interactive {
					id, label, err := app.pickInfrastructure()
					if err != nil {
						fmt.Fprintf(app.Stderr, " %s skipped infrastructure: %v\n", t.Yellow.Apply(sym.Warn), err)
					} else {
						p := app.File.Profiles[app.Resolved.ProfileName]
						if p == nil {
							p = &config.Profile{APIURL: normalized}
							app.File.Profiles[app.Resolved.ProfileName] = p
						}
						p.Infrastructure, p.InfrastructureName = id, label
						if err := app.Store.SaveConfig(app.File); err != nil {
							return err
						}
						fmt.Fprintf(app.Stderr, " %s infrastructure %s %s\n",
							t.Green.Apply(sym.OK), id, t.Dim.Apply(label))
					}
				} else {
					fmt.Fprintf(app.Stderr, " %s no default infrastructure (set one: ups context set)\n",
						t.Yellow.Apply(sym.Warn))
				}
			} else {
				fmt.Fprintf(app.Stderr, " %s infrastructure %s\n",
					t.Green.Apply(sym.OK), app.Resolved.Infrastructure.Value)
			}

			// Step 4: agent skill.
			if !noSkill {
				if err := installSkillStep(app, scope, clients, force); err != nil {
					// A skill conflict must not fail the whole setup.
					if errs.CodeOf(err) == errs.CodeConflict {
						fmt.Fprintf(app.Stderr, " %s %v\n", t.Yellow.Apply(sym.Warn), err)
					} else {
						return err
					}
				}
			}

			fmt.Fprintf(app.Stderr, "\n%s Run %s to verify, %s to see what you are pointed at.\n",
				t.Green.Apply(sym.OK), t.Bold.Apply("ups doctor"), t.Bold.Apply("ups context show"))
			return nil
		},
	}

	c.Flags().StringVar(&apiURL, "api-url", "", "server URL or alias ("+joinAliases()+")")
	c.Flags().StringVarP(&username, "username", "u", "", "username to log in as")
	c.Flags().BoolVar(&skillOnly, "skill-only", false, "only install the agent skill")
	c.Flags().BoolVar(&noSkill, "no-skill", false, "skip installing the agent skill")
	c.Flags().BoolVar(&nonInteract, "non-interactive", false, "never prompt; fail instead")
	c.Flags().BoolVar(&force, "force", false, "overwrite local edits to the installed skill")
	scopeFlag(c, &scope)
	clientFlag(c, &clients)
	return c
}

// installSkillStep offers the client picker during setup, so the skill lands
// wherever the user actually works rather than assuming one tool.
func installSkillStep(app *App, scope string, clients []string, force bool) error {
	sc := skill.Scope(scope)
	if len(clients) == 0 && !app.Interactive() {
		// Non-interactive setup still deserves a skill; default to the
		// broadest useful pair rather than silently installing nothing.
		clients = skill.DefaultTargets
	}
	targets, err := app.chooseTargets(clients, sc)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}
	return app.installTargets(targets, sc, force)
}

// promptServer offers the known aliases plus a free-text option.
func promptServer(app *App) (string, error) {
	choices := []ui.Choice{}
	for _, name := range config.AliasNames() {
		choices = append(choices, ui.Choice{
			Label: name, Desc: config.Aliases[name], Value: config.Aliases[name],
		})
	}
	choices = append(choices, ui.Choice{Label: "other", Desc: "enter a URL", Value: "\x00other"})

	picked, err := ui.Select(app.Stdin, app.Stderr, "Which Upstacked server?", choices)
	if err != nil {
		return app.Prompt("Server URL", "")
	}
	if picked == "\x00other" {
		return app.Prompt("Server URL", "")
	}
	return picked, nil
}
