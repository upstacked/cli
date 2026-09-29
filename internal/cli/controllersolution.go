package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/output"
)

const controllerSolutionsPath = "/api/core/controller_solutions/"

func newControllerSolutionCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "controller-solution",
		Aliases: []string{"controller", "cs"},
		Short:   "Manage the controllers devices are reached through",
		Long: `A controller solution is how a fleet is reached: vManage for Viptela, and
anything else modelled the same way.

It names the attributes a device carries on that controller - its system IP,
site id, device id - and a host records its own values for them. An item then
templates those values into its URL, so one item serves every device in the
fabric:

  {{host.controller_solution_attributes.systemIp}}

Which attributes a solution needs comes from the vendor's API documentation:
they are the fields its monitoring calls take. Declare them here once, set them
per host with 'ups host create --controller-attr', and an item can address any
device in the fleet.`,
	}
	c.AddCommand(
		newControllerSolutionListCmd(app),
		newControllerSolutionShowCmd(app),
		newControllerSolutionCreateCmd(app),
	)
	return c
}

func newControllerSolutionListCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the controller solutions this organization can use",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runList(listOpts{
				Path:    controllerSolutionsPath,
				Columns: []string{"ID", "NAME", "TYPE", "ATTRIBUTES"},
				Empty: "No controller solutions. Devices reached through a controller " +
					"need one: ups controller-solution create --name <name> --attr <field>",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "name")), dash(str(m, "type")),
						strings.Join(attributeNames(m), ", "),
					}
				},
			})
		},
	}
}

func newControllerSolutionShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one controller solution and the attributes a host must carry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, raw, err := app.getOne(controllerSolutionsPath+args[0]+"/", nil)
			if err != nil {
				return err
			}
			if app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			if err := app.Printer.Object(raw, [][2]string{
				{"ID", str(m, "id")},
				{"Name", dash(str(m, "name"))},
				{"Description", dash(str(m, "description"))},
				{"Type", dash(str(m, "type"))},
			}); err != nil {
				return err
			}

			tbl := &output.Table{
				Columns: []string{"ATTRIBUTE", "REQUIRED", "DESCRIPTION"},
				Empty: "This solution declares no attributes, so a host has nothing to " +
					"carry and an item cannot template a device.",
			}
			for _, a := range listField(m, "attributes") {
				raw, _ := json.Marshal(a)
				name := dash(str(a, "identifier", "name"))
				tbl.Add(name, raw, name, yesNo(a["is_required"]), dash(str(a, "description")))
			}
			return app.Printer.Print(tbl)
		},
	}
}

func newControllerSolutionCreateCmd(app *App) *cobra.Command {
	var name, description string
	var attrs, requiredAttrs []string

	c := &cobra.Command{
		Use:   "create",
		Short: "Declare a controller solution and the attributes its devices carry",
		Long: `Declare a controller solution.

The attributes are the fields the vendor's monitoring API takes to identify one
device. Read them off that API's documentation rather than guessing: an
attribute a host does not carry renders as nothing, and the item polls the
wrong device or none at all.`,
		Example: `  ups controller-solution create --name "Cisco SD-WAN" \
    --required-attr controllerIp="vManage address" \
    --required-attr systemIp="device system IP" \
    --attr siteId="site the device belongs to"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return errs.Usage("--name is required")
			}

			declared, err := controllerAttributes(attrs, requiredAttrs)
			if err != nil {
				return err
			}
			if len(declared) == 0 {
				return errs.Usage("a controller solution with no attributes cannot identify a device").
					WithHint("pass --required-attr or --attr for each field the vendor's " +
						"monitoring API takes, e.g. --required-attr systemIp")
			}

			body := map[string]any{"name": name, "attributes": declared}
			addIf(body, "description", description)

			var raw jsonRaw
			if err := app.create(controllerSolutionsPath, body, &raw); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			var m row
			_ = jsonUnmarshal(raw, &m)
			t, sym := app.Theme(), app.Sym()
			fmt.Fprintf(app.Stderr, "%s Created controller solution %s (%s)\n",
				t.Green.Apply(sym.OK), dash(str(m, "name")), str(m, "id"))
			fmt.Fprintf(app.Stderr, "  %s ups host create --controller-solution %s --controller-attr <field>=<value>\n",
				t.Dim.Apply("next:"), str(m, "id"))
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "name of the controller solution (required)")
	c.Flags().StringVar(&description, "description", "", "what this controller is")
	c.Flags().StringArrayVar(&attrs, "attr", nil,
		"optional attribute as name[=description], repeatable")
	c.Flags().StringArrayVar(&requiredAttrs, "required-attr", nil,
		"attribute every host on this controller must carry, as name[=description], repeatable")
	return c
}

// controllerAttributes turns the two attribute flags into the payload the API
// takes. A name repeated across both is refused rather than resolved: which one
// the caller meant is not knowable, and guessing decides whether every future
// host must carry it.
func controllerAttributes(optional, required []string) ([]map[string]any, error) {
	var out []map[string]any
	seen := map[string]bool{}

	add := func(spec string, isRequired bool) error {
		name, description, _ := strings.Cut(spec, "=")
		name = strings.TrimSpace(name)
		if name == "" {
			return errs.Usage("an attribute needs a name, got %q", spec)
		}
		if seen[name] {
			return errs.Usage("attribute %q given twice", name).
				WithHint("an attribute is either required or not; pass it once")
		}
		seen[name] = true
		out = append(out, map[string]any{
			"name":        name,
			"description": description,
			"is_required": isRequired,
		})
		return nil
	}

	for _, spec := range required {
		if err := add(spec, true); err != nil {
			return nil, err
		}
	}
	for _, spec := range optional {
		if err := add(spec, false); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func attributeNames(m row) []string {
	var names []string
	for _, a := range listField(m, "attributes") {
		names = append(names, dash(str(a, "identifier", "name")))
	}
	return names
}
