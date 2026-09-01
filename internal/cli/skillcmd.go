package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/output"
	"github.com/upstacked/cli/internal/skill"
)

func newSkillCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "skill",
		Short: "Install and verify the Upstacked agent skill",
		Long: `The agent skill teaches an LLM how to operate this CLI, and more
importantly why the workflows are shaped as they are.

A skill describing a different command surface than the installed binary is
worse than no skill, so installation records a checksum and 'ups doctor'
reports drift.`,
	}
	c.AddCommand(newSkillInstallCmd(app), newSkillStatusCmd(app), newSkillShowCmd(app), newSkillUninstallCmd(app))
	return c
}

func scopeFlag(cmd *cobra.Command, target *string) {
	cmd.Flags().StringVar(target, "scope", "user",
		"where to install: user (~/.<agent>/skills) or project (./.<agent>/skills)")
	_ = cmd.RegisterFlagCompletionFunc("scope", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"user", "project"}, cobra.ShellCompDirectiveNoFileComp
	})
}

func newSkillInstallCmd(app *App) *cobra.Command {
	var scope string
	var force bool
	var agentsRaw string
	c := &cobra.Command{
		Use:   "install",
		Short: "Install or update the agent skill",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedRaw, err := resolveAgentsRaw(agentsRaw, cmd.Flags().Changed("agent"), app.Interactive(), app.Prompt)
			if err != nil {
				return err
			}
			agents, err := skill.ParseAgents(resolvedRaw)
			if err != nil {
				return err
			}
			installed, err := skill.InstallMany(skill.Scope(scope), "", Version, force, agents)
			if err != nil {
				return err
			}
			t, sym := app.Theme(), app.Sym()
			for _, st := range installed {
				fmt.Fprintf(app.Stderr, "%s %s skill installed at %s\n",
					t.Green.Apply(sym.OK), st.Agent, st.Path)
			}
			return nil
		},
	}
	scopeFlag(c, &scope)
	c.Flags().StringVar(&agentsRaw, "agent", "popular",
		"install target: popular|all|claude|cursor|codex|gemini or comma-separated list (prompted on TTY when omitted)")
	c.Flags().BoolVar(&force, "force", false, "overwrite local edits to the installed skill")
	_ = c.RegisterFlagCompletionFunc("agent", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"popular", "all", "claude", "cursor", "codex", "gemini"}, cobra.ShellCompDirectiveNoFileComp
	})
	return c
}

func resolveAgentsRaw(current string, changed, canPrompt bool, prompt func(string, string) (string, error)) (string, error) {
	if changed || !canPrompt {
		return current, nil
	}
	v, err := prompt("Install skill for agents", current)
	if err != nil {
		return "", err
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return current, nil
	}
	return v, nil
}

func newSkillStatusCmd(app *App) *cobra.Command {
	var scope string
	c := &cobra.Command{
		Use:   "status",
		Short: "Report whether the installed skill is current",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := skill.Inspect(skill.Scope(scope), "", Version)
			if err != nil {
				return err
			}
			if app.AsJSON {
				return app.Printer.JSON(map[string]any{
					"path": st.Path, "installed": st.Installed, "current": st.Current,
					"modified": st.Modified, "outdated": st.Outdated,
					"installed_version": st.InstalledVer, "cli_version": Version,
				})
			}
			return app.Printer.Object(nil, [][2]string{
				{"Path", st.Path},
				{"Installed", yesNo(st.Installed)},
				{"Up to date", yesNo(st.Current)},
				{"Locally edited", yesNo(st.Modified)},
				{"Installed by", dash(st.InstalledVer)},
				{"This CLI", Version},
			})
		},
	}
	scopeFlag(c, &scope)
	return c
}

func newSkillShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the skill this binary ships",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprint(app.Stdout, skill.Content)
			return nil
		},
	}
}

func newSkillUninstallCmd(app *App) *cobra.Command {
	var scope string
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the installed skill",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := skill.Dir(skill.Scope(scope), "")
			if err != nil {
				return err
			}
			if err := app.Confirm(fmt.Sprintf("Remove the skill at %s?", dir)); err != nil {
				return err
			}
			removed, err := skill.Uninstall(skill.Scope(scope), "")
			if err != nil {
				return err
			}
			app.Printer.Infof("Removed %s.", removed)
			return nil
		},
	}
	scopeFlag(c, &scope)
	return c
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

var _ = output.FormatTable
var _ = errs.CodeOK
