package cli

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/output"
)

const valueMappingsPath = "/api/monitoring/monitoring_value_mapping/"

// newMonValueMapCmd manages value mappings: how a raw value reads on screen.
//
// A mapping field publishes what the device sent - "1", "true", 1000000000.
// A value mapping is what turns that into "UP" in green on the host page. It
// changes nothing that is stored or alerted on, only what a person sees, which
// is why getting it wrong is quiet: the page shows "1" and nobody files a bug.
func newMonValueMapCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "value-mapping",
		Aliases: []string{"value-mappings", "vmap"},
		Short:   "Turn raw values into labels and colours on the portal",
		Long: `A value mapping says how a raw value reads in the portal: "1" as UP in
green, "2" as DOWN in red, 1000000000 as "1000 Mbps".

It is attached to one field of a schema mapping, and changes only what people
see - stored data and alert rules still read the raw value.

  ups monitoring value-mapping list                       # reuse before creating
  ups monitoring value-mapping show <id|name>
  ups monitoring value-mapping create --name ifOperStatus --rule 1=UP@green --rule 2=DOWN@red
  ups monitoring item mapping update <id> --value-mapping oper_status=ifOperStatus

Applying a template copies each value mapping's rules onto the host's items.
Editing a value mapping afterwards changes new applies, not hosts it was
already applied to: re-apply the template to pick the change up.`,
	}
	c.AddCommand(newValueMapListCmd(app), newValueMapShowCmd(app),
		newValueMapCreateCmd(app), newValueMapUpdateCmd(app), newValueMapDeleteCmd(app))
	return c
}

func newValueMapListCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List value mappings",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runList(listOpts{
				Path:    valueMappingsPath,
				Columns: []string{"ID", "NAME", "RULES", "IN USE", "ORG"},
				Empty:   "No value mappings. Create one: ups monitoring value-mapping create --name <name> --rule 1=UP@green",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "name")),
						dash(truncate(rulesSummary(valueMapRules(m)), 60)),
						yesNo(m["in_use"]), dash(str(m, "organization")),
					}
				},
			})
		},
	}
}

func newValueMapShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id|name>",
		Short: "Show a value mapping's rules and where it is used",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := app.resolveValueMapping(args[0])
			if err != nil {
				return err
			}
			m, raw, err := app.getOne(valueMappingsPath+id+"/", nil)
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
				{"Organization", dash(str(m, "organization"))},
				{"In use", yesNo(m["in_use"])},
			}); err != nil {
				return err
			}
			t := &output.Table{
				Columns: []string{"#", "WHEN", "SHOWS", "COLOUR"},
				Empty:   "No rules: every value shows as it was sent.",
			}
			for i, r := range valueMapRules(m) {
				rawRule, _ := json.Marshal(r)
				t.Add(str(r, "id"), rawRule, strconv.Itoa(i+1), ruleCondition(r),
					dash(str(r, "mapped_value")), dash(str(r, "color")))
			}
			return app.Printer.Print(t)
		},
	}
}

func newValueMapCreateCmd(app *App) *cobra.Command {
	var name, description, org string
	var rules []string

	c := &cobra.Command{
		Use:   "create",
		Short: "Create a value mapping",
		Long: `Create a value mapping from --rule, repeatably. Rules are tried in order and
the first match wins.

  VALUE=LABEL[@COLOUR]         the value equals VALUE (compared as text, any case)
  range:LOW-HIGH=LABEL         LOW <= value <= HIGH, whole numbers
  gte:N=LABEL  lte:N=LABEL     value >= N, value <= N, whole numbers
  default=LABEL                anything no earlier rule matched

COLOUR is one of green, red, yellow, orange, blue. LABEL may use {{value}},
e.g. 'default={{ value // 1000000 }} Mbps'. A value no rule matches shows as
sent, so a trailing 'default' is only needed to relabel the rest.

Reuse before creating: 'ups monitoring value-mapping list' shows the mappings
this organization already has, and the portal lists them by name - a second
"ifOperStatus" is one nobody can tell apart from the first.`,
		Example: `  ups monitoring value-mapping create --name ifOperStatus \
    --rule 1=UP@green --rule 2=DOWN@red --rule 3=TESTING@yellow
  ups monitoring value-mapping create --name "CPU load" \
    --rule range:0-79=OK@green --rule gte:80=HIGH@red`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return errs.Usage("--name is required")
			}
			parsed, err := parseValueRules(rules)
			if err != nil {
				return err
			}
			if len(parsed) == 0 {
				return errs.Usage("a value mapping with no rules changes nothing").
					WithHint("add at least one: --rule 1=UP@green")
			}
			if clash, err := app.valueMappingNamed(name); err != nil {
				return err
			} else if clash != "" {
				return errs.Usage("a value mapping called %q already exists (%s)", name, clash).
					WithHint("reuse it, or change its rules: ups monitoring value-mapping update %s --rule ...", clash)
			}
			orgID, err := app.resolveOrganization(org)
			if err != nil {
				return err
			}
			body := map[string]any{
				"name": name, "organization": atoiOr(orgID), "value_mapping_rules": parsed,
			}
			if description != "" {
				body["description"] = description
			}
			var raw jsonRaw
			if err := app.create(valueMappingsPath, body, &raw); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			var m row
			_ = jsonUnmarshal(raw, &m)
			t := app.Theme()
			fmt.Fprintf(app.Stderr, "%s Created value mapping %s (%s)\n",
				t.Green.Apply(app.Sym().OK), name, str(m, "id"))
			fmt.Fprintf(app.Stderr, "  %s ups monitoring item mapping update <mapping-id> --value-mapping <key>=%s\n",
				t.Dim.Apply("next:"), str(m, "id"))
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "value mapping name (required)")
	c.Flags().StringVar(&description, "description", "", "what the values mean")
	c.Flags().StringArrayVar(&rules, "rule", nil, "a rule, as VALUE=LABEL[@COLOUR] (repeatable, first match wins)")
	c.Flags().StringVar(&org, "org", "", "organization id (defaults to yours when you belong to exactly one)")
	return c
}

func newValueMapUpdateCmd(app *App) *cobra.Command {
	var name, description string
	var rules []string

	c := &cobra.Command{
		Use:   "update <id|name>",
		Short: "Rename a value mapping or replace its rules",
		Long: `Change a value mapping.

--rule replaces the whole rule list, in the order given: rules are ordered and
first-match-wins, so merging new ones into the old list would change which
one matches. See 'value-mapping create --help' for the rule syntax.

Hosts a template was already applied to keep the rules they were applied
with. Re-apply the template for them to pick the change up.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := app.resolveValueMapping(args[0])
			if err != nil {
				return err
			}
			current, _, err := app.getOne(valueMappingsPath+id+"/", nil)
			if err != nil {
				return err
			}
			// The API refuses a body without rules and replaces the list with
			// whatever it gets, so the current rules are resent when unchanged.
			body := map[string]any{"value_mapping_rules": stripRuleIDs(valueMapRules(current))}
			if len(rules) > 0 {
				parsed, err := parseValueRules(rules)
				if err != nil {
					return err
				}
				body["value_mapping_rules"] = parsed
			}
			if name != "" {
				body["name"] = name
			}
			if cmd.Flags().Changed("description") {
				body["description"] = description
			}
			if len(rules) == 0 && name == "" && !cmd.Flags().Changed("description") {
				return errs.Usage("nothing to change").WithHint("pass --rule, --name or --description")
			}
			if err := app.mutate("PATCH", valueMappingsPath+id+"/", body, nil); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			app.Printer.Infof("%s Updated value mapping %s", app.Sym().OK, id)
			if b, _ := current["in_use"].(bool); b && len(rules) > 0 {
				fmt.Fprintf(app.Stderr, "  %s hosts a template was already applied to keep the old rules until it is re-applied.\n",
					app.Theme().Yellow.Apply(app.Sym().Warn))
			}
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "new name")
	c.Flags().StringVar(&description, "description", "", "new description")
	c.Flags().StringArrayVar(&rules, "rule", nil, "the complete new rule list, as VALUE=LABEL[@COLOUR] (repeatable)")
	return c
}

func newValueMapDeleteCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id|name>",
		Short: "Delete a value mapping",
		Long: `Delete a value mapping.

Every field that uses it goes back to showing raw values - "1" where the page
said UP - including on hosts a template was already applied to.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := app.resolveValueMapping(args[0])
			if err != nil {
				return err
			}
			m, _, err := app.getOne(valueMappingsPath+id+"/", nil)
			if err != nil {
				return err
			}
			prompt := fmt.Sprintf("Delete value mapping %s (%s)?", id, str(m, "name"))
			if b, _ := m["in_use"].(bool); b {
				prompt = fmt.Sprintf("Delete value mapping %s (%s)? It is in use: those fields go back to showing raw values.", id, str(m, "name"))
			}
			if err := app.Confirm(prompt); err != nil {
				return err
			}
			if err := app.mutate("DELETE", valueMappingsPath+id+"/", nil, nil); err != nil {
				return err
			}
			if !app.DryRun {
				app.Printer.Infof("Deleted value mapping %s.", id)
			}
			return nil
		},
	}
}

// resolveValueMapping returns a value mapping's id, given its id or name.
func (a *App) resolveValueMapping(spec string) (string, error) {
	if _, err := strconv.Atoi(spec); err == nil {
		return spec, nil
	}
	id, err := a.valueMappingNamed(spec)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", errs.NotFound("no value mapping called %q", spec).
			WithHint("list them: ups monitoring value-mapping list")
	}
	return id, nil
}

// valueMappingNamed returns the id of the value mapping with this name, or "".
func (a *App) valueMappingNamed(name string) (string, error) {
	rows, err := a.fetchRows(valueMappingsPath, nil)
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		if strings.EqualFold(strings.TrimSpace(str(r, "name")), strings.TrimSpace(name)) {
			return str(r, "id"), nil
		}
	}
	return "", nil
}

var valueMapColours = map[string]bool{
	"none": true, "green": true, "red": true, "yellow": true, "orange": true, "blue": true,
}

var rangeValue = regexp.MustCompile(`^\s*-?\d+\s*-\s*-?\d+\s*$`)

// parseValueRules turns --rule specs into the API's rule objects.
func parseValueRules(specs []string) ([]any, error) {
	out := make([]any, 0, len(specs))
	for _, spec := range specs {
		lhs, label, ok := strings.Cut(spec, "=")
		if !ok {
			if strings.TrimSpace(spec) != "default" {
				return nil, errs.Usage("--rule takes VALUE=LABEL, got %q", spec).
					WithHint("for example: --rule 1=UP@green, --rule range:0-79=OK, --rule default=Other")
			}
			lhs = "default"
		}
		colour := "none"
		if i := strings.LastIndex(label, "@"); i >= 0 && valueMapColours[strings.ToLower(label[i+1:])] {
			colour = strings.ToLower(label[i+1:])
			label = label[:i]
		}
		r := map[string]any{"mapped_value": strings.TrimSpace(label), "color": colour}
		cond, value, hasCond := strings.Cut(lhs, ":")
		switch {
		case strings.TrimSpace(lhs) == "default":
			r["condition"], r["value"] = "default", ""
		case hasCond && cond == "range":
			if !rangeValue.MatchString(value) {
				return nil, errs.Usage("range takes whole numbers LOW-HIGH, got %q", value)
			}
			r["condition"], r["value"] = "in_range", strings.ReplaceAll(value, " ", "")
		case hasCond && (cond == "gte" || cond == "lte"):
			if _, err := strconv.Atoi(strings.TrimSpace(value)); err != nil {
				return nil, errs.Usage("%s compares whole numbers, got %q", cond, value).
					WithHint("the portal compares as integers, so %q would never match", value)
			}
			r["condition"] = map[string]string{"gte": "is_greater_than_or_equals", "lte": "is_less_than_or_equals"}[cond]
			r["value"] = strings.TrimSpace(value)
		default:
			if strings.TrimSpace(lhs) == "" {
				return nil, errs.Usage("--rule %q matches no value", spec)
			}
			r["condition"], r["value"] = "equals", strings.TrimSpace(lhs)
		}
		out = append(out, r)
	}
	return out, nil
}

func valueMapRules(m row) []row {
	raw, _ := m["value_mapping_rules"].([]any)
	out := make([]row, 0, len(raw))
	for _, v := range raw {
		if r, ok := v.(map[string]any); ok {
			out = append(out, row(r))
		}
	}
	return out
}

func stripRuleIDs(rules []row) []any {
	out := make([]any, 0, len(rules))
	for _, r := range rules {
		out = append(out, map[string]any{
			"condition": r["condition"], "value": r["value"],
			"mapped_value": r["mapped_value"], "color": r["color"],
		})
	}
	return out
}

func ruleCondition(r row) string {
	v := str(r, "value")
	switch str(r, "condition") {
	case "default":
		return "anything else"
	case "in_range":
		return "in " + v
	case "is_greater_than_or_equals":
		return ">= " + v
	case "is_less_than_or_equals":
		return "<= " + v
	}
	return "= " + v
}

func rulesSummary(rules []row) string {
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		label := str(r, "mapped_value")
		if label == "" {
			label = "(as sent)"
		}
		parts = append(parts, ruleCondition(r)+" → "+label)
	}
	return strings.Join(parts, ", ")
}
