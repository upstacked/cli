package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/api"
	"github.com/upstacked/cli/internal/errs"
)

func newAssetCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "asset",
		Short: "Manage assets and bulk imports",
		Long: `Assets are procurement and ownership records.

An asset is not a host: a host is the thing that gets monitored. They can
be linked, but deleting one does not delete the other.`,
	}
	c.AddCommand(newAssetListCmd(app), newAssetShowCmd(app), newAssetImportCmd(app), newAssetLinkCmd(app))
	return c
}

func newAssetListCmd(app *App) *cobra.Command {
	var search string
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List assets",
		RunE: func(cmd *cobra.Command, args []string) error {
			q := app.infraQuery(nil)
			if search != "" {
				q.Set("search", search)
			}
			return app.runList(listOpts{
				Path:    "/api/assets/",
				Query:   q,
				Columns: []string{"ID", "NAME", "TYPE", "SERIAL", "ASSIGNEE"},
				Empty:   "No assets.",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "name")), dash(str(m, "asset_type", "type")),
						dash(str(m, "serial_number", "serial")), dash(str(m, "assignee", "asset_assignee")),
					}
				},
			})
		},
	}
	c.Flags().StringVarP(&search, "search", "s", "", "free-text search")
	return c
}

func newAssetShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one asset",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, raw, err := app.getOne("/api/asset/"+args[0]+"/details/", nil)
			if err != nil {
				return err
			}
			return app.Printer.Object(raw, nil)
		},
	}
}

func newAssetLinkCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "sync-host <asset-id>",
		Short: "Sync an asset with its linked host",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := app.mutate("POST", "/api/asset/"+args[0]+"/sync-host/", nil, nil); err != nil {
				return err
			}
			if !app.DryRun {
				app.Printer.Infof("Synced asset %s with its host.", args[0])
			}
			return nil
		},
	}
}

// newAssetImportCmd exposes the server's validate-then-apply split as exactly
// that. The API separates them deliberately; the import is not trivially
// reversible, so the CLI refuses to apply without a successful validation.
func newAssetImportCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "import",
		Short: "Bulk-import assets from CSV",
		Long: `Import assets from a CSV file.

Validation is a separate step because the import is not trivially
reversible. 'validate' reports what would fail; 'apply' performs the write.`,
	}
	c.AddCommand(
		&cobra.Command{
			Use:   "sample",
			Short: "Download a sample CSV",
			RunE: func(cmd *cobra.Command, args []string) error {
				cl, err := app.Client()
				if err != nil {
					return err
				}
				ctx, cancel := app.Ctx()
				defer cancel()
				var raw jsonRaw
				if err := cl.Do(ctx, request("GET", "/api/assets/import/download-sample/", nil), &raw); err != nil {
					return err
				}
				fmt.Fprintln(app.Stdout, string(raw))
				return nil
			},
		},
		newAssetImportRunCmd(app, true),
		newAssetImportRunCmd(app, false),
		&cobra.Command{
			Use:   "status <id>",
			Short: "Show an import's row-level results",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return app.runList(listOpts{
					Path:    "/api/assets/import/" + args[0] + "/rows/",
					Columns: []string{"ROW", "STATUS", "MESSAGE"},
					Empty:   "No rows recorded for this import.",
					Cells: func(m row) []string {
						return []string{
							dash(str(m, "row", "line", "index")),
							dash(str(m, "status", "state")),
							truncate(dash(str(m, "message", "error", "detail")), 60),
						}
					},
				})
			},
		},
	)
	return c
}

func newAssetImportRunCmd(app *App, validateOnly bool) *cobra.Command {
	use, short := "apply <file.csv>", "Import assets from a validated CSV"
	path := "/api/assets/import/"
	if validateOnly {
		use, short = "validate <file.csv>", "Check a CSV without importing it"
		path = "/api/assets/import/validate/"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			content, err := os.ReadFile(args[0])
			if err != nil {
				return errs.Usage("cannot read %s", args[0]).Wrapping(err)
			}
			if !validateOnly {
				if err := app.Confirm(fmt.Sprintf(
					"Import %s? Validate it first if you have not.", args[0])); err != nil {
					return err
				}
			}
			body := map[string]any{"file": string(content), "filename": args[0]}
			if app.DryRun {
				app.Printer.Infof("dry-run: POST %s (%d bytes)", path, len(content))
				return nil
			}
			cl, err := app.Client()
			if err != nil {
				return err
			}
			ctx, cancel := app.Ctx()
			defer cancel()
			var raw jsonRaw
			verb := "Validating"
			if !validateOnly {
				verb = "Importing"
			}
			if err := app.Spin(verb+" "+args[0], func() error {
				return cl.Do(ctx, api.Request{Method: "POST", Path: path, Body: body}, &raw)
			}); err != nil {
				return err
			}
			t, sym := app.Theme(), app.Sym()
			if validateOnly {
				fmt.Fprintf(app.Stderr, "%s Validation finished. Review the result, then: ups asset import apply %s\n",
					t.Green.Apply(sym.OK), args[0])
			} else {
				fmt.Fprintf(app.Stderr, "%s Import submitted.\n", t.Green.Apply(sym.OK))
			}
			return app.Printer.Object(raw, nil)
		},
	}
}
