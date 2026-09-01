package cli

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/config"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/output"
)

func newProfileCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "profile",
		Short: "Manage named server/context profiles",
		Long: `Profiles keep separate servers apart: production, staging, and
per-customer contexts. Credentials are stored per profile and bound to the
server that issued them.`,
	}
	c.AddCommand(newProfileListCmd(app), newProfileUseCmd(app), newProfileAddCmd(app), newProfileRemoveCmd(app))
	return c
}

func newProfileListCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			creds, err := app.Store.LoadCredentials()
			if err != nil {
				return err
			}
			names := make([]string, 0, len(app.File.Profiles))
			for n := range app.File.Profiles {
				names = append(names, n)
			}
			sort.Strings(names)

			t := &output.Table{
				Columns: []string{"", "PROFILE", "SERVER", "INFRASTRUCTURE", "AUTH"},
				Empty:   "No profiles yet. Run: ups init",
			}
			for _, n := range names {
				p := app.File.Profiles[n]
				marker := " "
				if n == app.Resolved.ProfileName {
					marker = "*"
				}
				auth := "not logged in"
				if cred, ok := creds.For(n, p.APIURL); ok {
					auth = "ok"
					if cred.Username != "" {
						auth = cred.Username
					}
				} else if stored, ok := creds.Stored(n); ok {
					auth = "token for " + stored.APIURL
				}
				infra := p.Infrastructure
				if p.InfrastructureName != "" {
					infra = p.Infrastructure + " " + p.InfrastructureName
				}
				t.Add(n, nil, marker, n, dash(p.APIURL), dash(infra), auth)
			}
			return app.Printer.Print(t)
		},
	}
}

func newProfileUseCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Make a profile the default",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, ok := app.File.Profiles[args[0]]; !ok {
				return errs.NotFound("no profile named %q", args[0]).
					WithHint("list them with: ups profile list")
			}
			app.File.CurrentProfile = args[0]
			if err := app.Store.SaveConfig(app.File); err != nil {
				return err
			}
			t, sym := app.Theme(), app.Sym()
			fmt.Fprintf(app.Stderr, "%s Now using profile %q (%s)\n",
				t.Green.Apply(sym.OK), args[0], app.File.Profiles[args[0]].APIURL)
			return nil
		},
	}
}

func newProfileAddCmd(app *App) *cobra.Command {
	var apiURL string
	c := &cobra.Command{
		Use:   "add <name>",
		Short: "Create a profile pointing at a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if apiURL == "" {
				return errs.Usage("--url is required when adding a profile").
					WithHint("aliases available: %s", joinAliases())
			}
			normalized, err := config.NormalizeURL(apiURL)
			if err != nil {
				return err
			}
			if app.File.Profiles == nil {
				app.File.Profiles = map[string]*config.Profile{}
			}
			if _, exists := app.File.Profiles[args[0]]; exists {
				return errs.Conflict("profile %q already exists", args[0])
			}
			app.File.Profiles[args[0]] = &config.Profile{APIURL: normalized}
			if err := app.Store.SaveConfig(app.File); err != nil {
				return err
			}
			app.Printer.Infof("Created profile %q for %s. Log in with: ups --profile %s login",
				args[0], normalized, args[0])
			return nil
		},
	}
	c.Flags().StringVar(&apiURL, "url", "", "server URL or alias ("+joinAliases()+")")
	return c
}

func newProfileRemoveCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a profile and its stored credentials",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, ok := app.File.Profiles[args[0]]; !ok {
				return errs.NotFound("no profile named %q", args[0])
			}
			if err := app.Confirm(fmt.Sprintf("Delete profile %q and its credentials?", args[0])); err != nil {
				return err
			}
			delete(app.File.Profiles, args[0])
			if app.File.CurrentProfile == args[0] {
				app.File.CurrentProfile = "default"
			}
			if err := app.Store.SaveConfig(app.File); err != nil {
				return err
			}
			creds, err := app.Store.LoadCredentials()
			if err == nil {
				creds.Delete(args[0])
				_ = app.Store.SaveCredentials(creds)
			}
			app.Printer.Infof("Removed profile %q.", args[0])
			return nil
		},
	}
}
