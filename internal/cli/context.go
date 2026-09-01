package cli

import (
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/config"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/ui"
)

func newContextCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "context",
		Short: "Show or set the active server and infrastructure",
	}
	c.AddCommand(newContextShowCmd(app), newContextSetCmd(app), newContextClearCmd(app))
	return c
}

func newContextShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the active context and where each value came from",
		Long: `Show the active context.

Each value reports the layer that supplied it, because staging and
production differ by one line of config and nothing in the prompt.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			r := app.Resolved
			if app.AsJSON {
				return app.Printer.JSON(map[string]any{
					"profile": r.ProfileName,
					"api_url": map[string]string{"value": r.APIURL.Value, "source": string(r.APIURL.Source)},
					"infrastructure": map[string]string{
						"value": r.Infrastructure.Value, "source": string(r.Infrastructure.Source),
						"name": r.Profile.InfrastructureName,
					},
					"customer": map[string]string{
						"value": r.Customer.Value, "source": string(r.Customer.Source),
						"name": r.Profile.CustomerName,
					},
					"config_file": app.Store.ConfigPath(),
				})
			}
			infra := r.Infrastructure.Describe()
			if r.Infrastructure.IsSet() && r.Profile.InfrastructureName != "" {
				infra = fmt.Sprintf("%s %s (from %s)", r.Infrastructure.Value,
					r.Profile.InfrastructureName, r.Infrastructure.Source)
			}
			cust := r.Customer.Describe()
			if r.Customer.IsSet() && r.Profile.CustomerName != "" {
				cust = fmt.Sprintf("%s %s (from %s)", r.Customer.Value,
					r.Profile.CustomerName, r.Customer.Source)
			}
			return app.Printer.Object(nil, [][2]string{
				{"Profile", r.ProfileName},
				{"Server", r.APIURL.Describe()},
				{"Infrastructure", infra},
				{"Customer", cust},
				{"Config", app.Store.ConfigPath()},
			})
		},
	}
}

func newContextSetCmd(app *App) *cobra.Command {
	var infra, customer, apiURL string
	c := &cobra.Command{
		Use:   "set",
		Short: "Set the default infrastructure, customer or server for this profile",
		Long: `Set defaults for the active profile.

With no flags on a terminal, this lists the infrastructures you can see
and lets you pick one.`,
		Example: `  ups context set
  ups context set --infra 42
  ups context set --api-url staging`,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := app.Resolved.ProfileName
			if app.File.Profiles == nil {
				app.File.Profiles = map[string]*config.Profile{}
			}
			p := app.File.Profiles[name]
			if p == nil {
				p = &config.Profile{APIURL: app.Resolved.APIURL.Value}
				app.File.Profiles[name] = p
			}

			if apiURL != "" {
				normalized, err := config.NormalizeURL(apiURL)
				if err != nil {
					return err
				}
				p.APIURL = normalized
			}
			if customer != "" {
				p.Customer, p.CustomerName = customer, ""
			}

			switch {
			case infra != "":
				p.Infrastructure, p.InfrastructureName = infra, ""
			case apiURL == "" && customer == "":
				// Nothing specified: offer a picker.
				id, label, err := app.pickInfrastructure()
				if err != nil {
					return err
				}
				p.Infrastructure, p.InfrastructureName = id, label
			}

			// Resolve the display name for whatever id we ended up with.
			if p.Infrastructure != "" && p.InfrastructureName == "" {
				if name, err := app.lookupInfraName(p.Infrastructure); err == nil {
					p.InfrastructureName = name
				}
			}
			if app.File.CurrentProfile == "" {
				app.File.CurrentProfile = name
			}
			if err := app.Store.SaveConfig(app.File); err != nil {
				return err
			}

			t, sym := app.Theme(), app.Sym()
			fmt.Fprintf(app.Stderr, "%s Context updated for profile %q\n", t.Green.Apply(sym.OK), name)
			if p.Infrastructure != "" {
				fmt.Fprintf(app.Stderr, "  infrastructure %s %s\n", p.Infrastructure,
					t.Dim.Apply(p.InfrastructureName))
			}
			return nil
		},
	}
	c.Flags().StringVar(&infra, "infra-id", "", "infrastructure id to make default")
	c.Flags().StringVar(&customer, "customer-id", "", "customer id to make default")
	c.Flags().StringVar(&apiURL, "server", "", "server URL to store on this profile")
	return c
}

func newContextClearCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "clear",
		Short: "Remove the default infrastructure and customer",
		RunE: func(cmd *cobra.Command, args []string) error {
			p := app.File.Profiles[app.Resolved.ProfileName]
			if p == nil {
				return nil
			}
			p.Infrastructure, p.InfrastructureName = "", ""
			p.Customer, p.CustomerName = "", ""
			if err := app.Store.SaveConfig(app.File); err != nil {
				return err
			}
			app.Printer.Infof("Context cleared.")
			return nil
		},
	}
}

// pickInfrastructure lists infrastructures and prompts for one.
func (a *App) pickInfrastructure() (id, label string, err error) {
	c, err := a.Client()
	if err != nil {
		return "", "", err
	}
	ctx, cancel := a.Ctx()
	defer cancel()

	var list *listResult
	err = a.Spin("Loading infrastructures", func() error {
		l, e := c.GetList(ctx, request("GET", "/api/infrastructure/", url.Values{}), 200)
		if e != nil {
			return e
		}
		list = &listResult{l.Items, l.Truncated}
		return nil
	})
	if err != nil {
		return "", "", err
	}

	rows := decodeRows(list.Items)
	if len(rows) == 0 {
		return "", "", errs.NotFound("no infrastructures are visible to this account").
			WithHint("check you are on the right server: ups context show")
	}
	choices := make([]ui.Choice, 0, len(rows))
	for _, m := range rows {
		name := str(m, "name", "infra_code")
		choices = append(choices, ui.Choice{
			Label: name,
			Desc:  "#" + str(m, "id"),
			Value: str(m, "id") + "\x00" + name,
		})
	}
	if !a.Interactive() {
		return "", "", errs.Usage("no infrastructure specified and stdin is not a terminal").
			WithHint("pass --infra-id <id>; list them with: ups infra list")
	}
	picked, err := ui.Select(a.Stdin, a.Stderr, "Select an infrastructure", choices)
	if err != nil {
		return "", "", errs.General("selection cancelled")
	}
	for i, ch := range choices {
		if ch.Value == picked {
			return str(rows[i], "id"), str(rows[i], "name", "infra_code"), nil
		}
	}
	return "", "", errs.General("selection failed")
}

func (a *App) lookupInfraName(id string) (string, error) {
	m, _, err := a.getOne("/api/infrastructure/"+id+"/", nil)
	if err != nil {
		return "", err
	}
	return str(m, "name", "infra_code"), nil
}

type listResult struct {
	Items     []jsonRaw
	Truncated bool
}
