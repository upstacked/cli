package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
)

// credentialTypes maps the CLI's noun to the API path segment.
var credentialTypes = map[string]string{
	"snmpv2":        "snmpv2",
	"snmpv3":        "snmpv3",
	"api":           "api",
	"device":        "device",
	"oauth2":        "oauth2",
	"aci":           "aci",
	"meraki":        "meraki",
	"dnac":          "dnac",
	"fmc":           "fmc",
	"viptela":       "viptela",
	"webex":         "webex",
	"cybervision":   "cybervision",
	"cisco-service": "cisco_service",
	"cisco-support": "cisco_support",
}

func credentialTypeNames() []string {
	out := make([]string, 0, len(credentialTypes))
	for k := range credentialTypes {
		out = append(out, k)
	}
	sortStringsAsc(out)
	return out
}

func sortStringsAsc(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func newCredentialCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "credential",
		Aliases: []string{"cred"},
		Short:   "Manage credentials",
		Long: `Store credentials for devices and APIs.

Secrets are never accepted as flag values. argv is visible to other
processes via ps, lands in shell history, and in CI ends up in build logs -
and these credentials authenticate to live network equipment. Pipe the
secret in, or let the CLI prompt for it.`,
	}
	c.AddCommand(newCredListCmd(app), newCredCreateCmd(app), newCredDeleteCmd(app), newCredTagsCmd(app))
	return c
}

func newCredListCmd(app *App) *cobra.Command {
	var kind string
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List stored credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/api/credential/credentials/"
			if kind != "" {
				seg, ok := credentialTypes[kind]
				if !ok {
					return errs.Usage("unknown credential type %q", kind).
						WithHint("one of: %s", strings.Join(credentialTypeNames(), ", "))
				}
				path = "/api/credential/" + seg + "/"
			}
			return app.runList(listOpts{
				Path:    path,
				Query:   app.infraQuery(nil),
				Columns: []string{"ID", "NAME", "TYPE", "TAG", "INFRASTRUCTURE"},
				Empty:   "No credentials stored.",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "name")),
						dash(str(m, "credential_type", "type")), dash(str(m, "tag")),
						dash(str(m, "infrastructure")),
					}
				},
			})
		},
	}
	c.Flags().StringVar(&kind, "type", "", "credential type ("+strings.Join(credentialTypeNames(), ", ")+")")
	return c
}

func newCredCreateCmd(app *App) *cobra.Command {
	var kind, name, username, community, tag, infra string
	var secretStdin bool
	var secretFile string

	c := &cobra.Command{
		Use:   "create <type>",
		Short: "Store a credential, reading the secret from stdin",
		Long: `Store a credential.

The secret comes from stdin or a file, never from a flag. On a terminal the
CLI prompts for it without echoing.`,
		Example: `  printf '%s' "$PW" | ups credential create snmpv3 --name core --username admin --secret-stdin
  ups credential create api --name vendor-api --username svc`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind = args[0]
			seg, ok := credentialTypes[kind]
			if !ok {
				return errs.Usage("unknown credential type %q", kind).
					WithHint("one of: %s", strings.Join(credentialTypeNames(), ", "))
			}
			if name == "" {
				return errs.Usage("--name is required")
			}
			target := infra
			if target == "" {
				var err error
				if target, err = app.Resolved.RequireInfra(); err != nil {
					return err
				}
			}

			secret, err := readPassword(app, secretStdin, secretFile)
			if err != nil {
				return err
			}
			if secret == "" && community == "" {
				return errs.Usage("no secret provided").
					WithHint("pipe it in: printf '%%s' \"$PW\" | ups credential create %s --secret-stdin ...", kind)
			}

			body := map[string]any{"name": name, "infrastructure": atoiOr(target)}
			addIf(body, "tag", tag)
			addIf(body, "username", username)
			switch kind {
			case "snmpv2":
				body["community"] = orDefaultStr(community, secret)
			default:
				body["password"] = secret
			}

			var raw jsonRaw
			if err := app.mutate("POST", "/api/credential/"+seg+"/", body, &raw); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			var m row
			_ = jsonUnmarshal(raw, &m)
			t, sym := app.Theme(), app.Sym()
			fmt.Fprintf(app.Stderr, "%s Stored %s credential %q (%s)\n",
				t.Green.Apply(sym.OK), kind, name, str(m, "id"))
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "credential name (required)")
	c.Flags().StringVar(&username, "username", "", "username, where the type needs one")
	c.Flags().StringVar(&community, "community", "", "SNMPv2 community string (prefer stdin)")
	c.Flags().StringVar(&tag, "tag", "", "credential tag")
	c.Flags().StringVar(&infra, "infra-id", "", "infrastructure id (defaults to the active context)")
	c.Flags().BoolVar(&secretStdin, "secret-stdin", false, "read the secret from stdin")
	c.Flags().StringVar(&secretFile, "secret-file", "", "read the secret from a file")
	return c
}

func newCredDeleteCmd(app *App) *cobra.Command {
	var kind string
	c := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			seg, ok := credentialTypes[kind]
			if !ok {
				return errs.Usage("--type is required to delete a credential").
					WithHint("one of: %s", strings.Join(credentialTypeNames(), ", "))
			}
			if err := app.Confirm(fmt.Sprintf(
				"Delete %s credential %s? Monitoring items using it will stop collecting.", kind, args[0])); err != nil {
				return err
			}
			if err := app.mutate("DELETE", "/api/credential/"+seg+"/"+args[0]+"/", nil, nil); err != nil {
				return err
			}
			if !app.DryRun {
				app.Printer.Infof("Deleted credential %s.", args[0])
			}
			return nil
		},
	}
	c.Flags().StringVar(&kind, "type", "", "credential type")
	return c
}

func newCredTagsCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "tags",
		Short: "List credential tags",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runList(listOpts{
				Path:    "/api/credential/tags/",
				Columns: []string{"ID", "NAME"},
				Empty:   "No credential tags.",
				Cells:   func(m row) []string { return []string{str(m, "id"), dash(str(m, "name", "tag"))} },
			})
		},
	}
}
