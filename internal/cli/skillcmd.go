package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/output"
	"github.com/upstacked/cli/internal/skill"
	"github.com/upstacked/cli/internal/ui"
)

func newSkillCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "skill",
		Short: "Install the Upstacked agent skill into your LLM clients",
		Long: `The agent skill teaches an LLM how to operate this CLI, and more
importantly why the workflows are shaped as they are.

It installs into whichever clients you use. Claude Code gets a real skill;
everything else gets the same guidance in that tool's own convention. Files
shared with your own instructions - AGENTS.md, GEMINI.md, Copilot
instructions - are edited in place: only a marked block is managed, and
anything you wrote around it is left alone.

A skill describing a different command surface than the installed binary is
worse than no skill, so every install records its version and 'ups doctor'
reports drift.`,
	}
	c.AddCommand(
		newSkillInstallCmd(app), newSkillStatusCmd(app),
		newSkillShowCmd(app), newSkillUninstallCmd(app), newSkillClientsCmd(app),
	)
	return c
}

func scopeFlag(cmd *cobra.Command, target *string) {
	cmd.Flags().StringVar(target, "scope", "user",
		"user (your machine) or project (committed with the repo)")
	_ = cmd.RegisterFlagCompletionFunc("scope", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"user", "project"}, cobra.ShellCompDirectiveNoFileComp
	})
}

func clientFlag(cmd *cobra.Command, target *[]string) {
	cmd.Flags().StringSliceVar(target, "client", nil,
		"clients to install into: "+strings.Join(skill.TargetIDs(), ", ")+", or all")
	_ = cmd.RegisterFlagCompletionFunc("client", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return append(skill.TargetIDs(), "all"), cobra.ShellCompDirectiveNoFileComp
	})
}

func newSkillInstallCmd(app *App) *cobra.Command {
	var scopeArg string
	var clients []string
	var force bool

	c := &cobra.Command{
		Use:   "install",
		Short: "Install or update the skill in one or more clients",
		Long: `Install the agent skill.

With no --client and a terminal, this shows a picker. Without a terminal it
needs --client, because a prompt nobody can answer is a hang.`,
		Example: `  ups skill install
  ups skill install --client claude,agents
  ups skill install --client all --scope project
  ups skill install --client cursor --force`,
		RunE: func(cmd *cobra.Command, args []string) error {
			scope := skill.Scope(scopeArg)
			targets, err := app.chooseTargets(clients, scope)
			if err != nil {
				return err
			}
			if len(targets) == 0 {
				app.Printer.Infof("Nothing selected.")
				return nil
			}
			return app.installTargets(targets, scope, force)
		},
	}
	scopeFlag(c, &scopeArg)
	clientFlag(c, &clients)
	c.Flags().BoolVar(&force, "force", false, "overwrite local edits to managed content")
	return c
}

// chooseTargets resolves --client, or prompts when a terminal is available.
func (a *App) chooseTargets(clients []string, scope skill.Scope) ([]skill.Target, error) {
	if len(clients) > 0 {
		targets, err := skill.ResolveTargets(clients, scope)
		if err != nil {
			return nil, errs.Usage("%v", err)
		}
		return targets, nil
	}
	if !a.Interactive() {
		return nil, errs.Usage("no --client given and stdin is not a terminal").
			WithHint("pass --client %s, or --client all", strings.Join(skill.DefaultTargets, ","))
	}

	var choices []ui.MultiChoice
	for _, t := range skill.Targets {
		ch := ui.MultiChoice{Label: t.Name, Value: t.ID}
		if !t.Supports(scope) {
			ch.Disabled = true
			ch.Reason = fmt.Sprintf("(no %s scope)", scope)
			choices = append(choices, ch)
			continue
		}
		desc := t.Where(scope)
		if t.Note != "" {
			desc += "  - " + t.Note
		}
		ch.Desc = desc
		for _, d := range skill.DefaultTargets {
			if d == t.ID {
				ch.Selected = true
			}
		}
		choices = append(choices, ch)
	}

	picked, err := ui.MultiSelect(a.Stdin, a.Stderr,
		fmt.Sprintf("Install the Upstacked skill into which clients?  (%s scope)", scope), choices)
	if err != nil {
		return nil, errs.General("selection cancelled")
	}
	return skill.ResolveTargets(picked, scope)
}

// installTargets writes each selected client, continuing past a client that
// refuses so one conflict does not block the rest.
func (a *App) installTargets(targets []skill.Target, scope skill.Scope, force bool) error {
	t, sym := a.Theme(), a.Sym()
	var failed int
	for _, target := range targets {
		st, err := skill.Install(target, scope, "", Version, force)
		if err != nil {
			failed++
			fmt.Fprintf(a.Stderr, " %s %-22s %v\n", t.Yellow.Apply(sym.Warn), target.Name, err)
			fmt.Fprintf(a.Stderr, "   %s keep them, or overwrite with --force\n", t.Dim.Apply(sym.Arrow))
			continue
		}
		fmt.Fprintf(a.Stderr, " %s %-22s %s\n", t.Green.Apply(sym.OK), target.Name, st.Path)
	}
	if failed > 0 {
		return errs.Conflict("%d client(s) had local edits and were left alone", failed)
	}
	return nil
}

func newSkillStatusCmd(app *App) *cobra.Command {
	var scopeArg string
	var all bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Report where the skill is installed and whether it is current",
		RunE: func(cmd *cobra.Command, args []string) error {
			scopes := []skill.Scope{skill.Scope(scopeArg)}
			if all {
				scopes = []skill.Scope{skill.ScopeUser, skill.ScopeProject}
			}

			tbl := &output.Table{
				Columns: []string{"CLIENT", "SCOPE", "STATUS", "PATH"},
				Empty:   "The skill is not installed anywhere. Run: ups skill install",
			}
			for _, scope := range scopes {
				for _, target := range skill.Targets {
					if !target.Supports(scope) {
						continue
					}
					st, err := skill.Inspect(target, scope, "", Version)
					if err != nil {
						continue
					}
					if !st.Installed && !all {
						continue
					}
					if !st.Installed {
						continue
					}
					tbl.Add(target.ID, nil, target.Name, string(scope), st.Summary(), st.Path)
				}
			}
			return app.Printer.Print(tbl)
		},
	}
	scopeFlag(c, &scopeArg)
	c.Flags().BoolVar(&all, "all-scopes", false, "check both user and project scope")
	return c
}

func newSkillClientsCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "clients",
		Short: "List the LLM clients this skill can install into",
		RunE: func(cmd *cobra.Command, args []string) error {
			tbl := &output.Table{Columns: []string{"ID", "CLIENT", "SCOPES", "WHERE", "NOTE"}}
			for _, t := range skill.Targets {
				var scopes []string
				where := ""
				for _, s := range []skill.Scope{skill.ScopeUser, skill.ScopeProject} {
					if t.Supports(s) {
						scopes = append(scopes, string(s))
						if where == "" {
							where = t.Where(s)
						}
					}
				}
				tbl.Add(t.ID, nil, t.ID, t.Name, strings.Join(scopes, ","), where, dash(t.Note))
			}
			return app.Printer.Print(tbl)
		},
	}
}

func newSkillShowCmd(app *App) *cobra.Command {
	var body bool
	c := &cobra.Command{
		Use:   "show",
		Short: "Print the skill this binary ships",
		RunE: func(cmd *cobra.Command, args []string) error {
			if body {
				fmt.Fprint(app.Stdout, skill.Body())
				return nil
			}
			fmt.Fprint(app.Stdout, skill.Content)
			return nil
		},
	}
	c.Flags().BoolVar(&body, "body", false, "omit the Claude Code frontmatter")
	return c
}

func newSkillUninstallCmd(app *App) *cobra.Command {
	var scopeArg string
	var clients []string
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the skill from one or more clients",
		Long: `Remove the managed block.

In a file you share with your own instructions, only the managed block is
removed; the rest of the file is left exactly as it was.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			scope := skill.Scope(scopeArg)
			targets, err := app.chooseTargets(clients, scope)
			if err != nil {
				return err
			}
			if len(targets) == 0 {
				return nil
			}
			names := make([]string, 0, len(targets))
			for _, t := range targets {
				names = append(names, t.Name)
			}
			if err := app.Confirm(fmt.Sprintf("Remove the skill from %s?", strings.Join(names, ", "))); err != nil {
				return err
			}
			th, sym := app.Theme(), app.Sym()
			for _, t := range targets {
				path, err := skill.Uninstall(t, scope, "")
				if err != nil {
					fmt.Fprintf(app.Stderr, " %s %-22s %v\n", th.Dim.Apply("-"), t.Name, err)
					continue
				}
				fmt.Fprintf(app.Stderr, " %s %-22s %s\n", th.Green.Apply(sym.OK), t.Name, path)
			}
			return nil
		},
	}
	scopeFlag(c, &scopeArg)
	clientFlag(c, &clients)
	return c
}
