package cli

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
)

const templatesPath = "/api/monitoring/templates/"

func newMonTemplateCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "template",
		Aliases: []string{"tpl"},
		Short:   "Monitoring templates",
		Long: `A monitoring template is the set of checks a kind of device gets.

A template holds modules; the checks themselves are monitoring items with no
host, waiting to be stamped onto one. Create those with:

  ups monitoring item create --template <id> --module <id> --name "CPU"

Applying a template REPLACES a host's monitoring: every existing item on the
host is deleted first, then the template's items are created in their place.
Nothing pages anyone when a check disappears, so 'apply' lists what it will
remove and confirms before writing.`,
	}
	c.AddCommand(
		newMonTemplateListCmd(app), newMonTemplateShowCmd(app),
		newMonTemplateItemsCmd(app), newMonTemplateCreateCmd(app),
		newMonTemplateUpdateCmd(app), newMonTemplateDeleteCmd(app),
		newMonTemplatePreflightCmd(app), newMonTemplateApplyCmd(app),
	)
	return c
}

func newMonTemplateListCmd(app *App) *cobra.Command {
	var name, status, org string
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List monitoring templates",
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if name != "" {
				q.Set("name", name)
			}
			if status != "" {
				q.Set("status", status)
			}
			if org != "" {
				q.Set("organization", org)
			}
			return app.runList(listOpts{
				// The list serializer has no description field, so the columns
				// report what a template actually is: how many checks and
				// whether it is safe to apply.
				Path:    templatesPath,
				Query:   q,
				Columns: []string{"ID", "NAME", "MODULES", "STATUS", "RULE"},
				Empty:   "No monitoring templates.",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "name")),
						fmt.Sprintf("%d", len(moduleNames(m))),
						dash(str(m, "published_status")), dash(str(m, "rule")),
					}
				},
			})
		},
	}
	c.Flags().StringVar(&name, "name", "", "filter by name (substring match)")
	c.Flags().StringVar(&status, "status", "", "filter by publish status (draft, published)")
	c.Flags().StringVar(&org, "org", "", "filter by organization id")
	return c
}

func newMonTemplateShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one monitoring template and its modules",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, raw, err := app.getOne(templatesPath+args[0]+"/", nil)
			if err != nil {
				return err
			}
			if app.AsJSON {
				return app.Printer.Object(raw, nil)
			}
			mods := moduleNames(m)
			fields := [][2]string{
				{"ID", str(m, "id")},
				{"Name", dash(str(m, "name"))},
				{"Status", dash(str(m, "published_status"))},
				{"Organization", dash(str(m, "organization"))},
				{"Rule", dash(str(m, "rule"))},
				{"Modules", dash(strings.Join(mods, ", "))},
			}
			if err := app.Printer.Object(raw, fields); err != nil {
				return err
			}
			fmt.Fprintf(app.Stderr, "  %s see the checks: ups monitoring template items %s\n",
				app.Theme().Dim.Apply("next:"), args[0])
			return nil
		},
	}
}

// newMonTemplateItemsCmd lists the host-less items a template will stamp onto a
// host. The template record itself only names modules, so this is the only way
// to see what actually gets checked.
func newMonTemplateItemsCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "items <id>",
		Short: "List the checks a template will create",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			q.Set("monitoring_template", args[0])
			return app.runList(listOpts{
				Path:    "/api/monitoring/items/",
				Query:   q,
				Columns: []string{"ID", "NAME", "MODULE", "CREDENTIAL", "READY", "PARAMETERS"},
				Empty:   "This template has no checks. It would remove a host's monitoring and add nothing.",
				Cells: func(m row) []string {
					return []string{
						str(m, "id"), dash(str(m, "name")),
						dash(str(m, "monitoring_module_name", "monitoring_module")),
						dash(str(m, "credential_type")),
						readyLabel(m),
						dash(truncate(str(m, "parameters"), 40)),
					}
				},
			})
		},
	}
}

func newMonTemplateCreateCmd(app *App) *cobra.Command {
	var name, org, rule, parent string
	var modules []string

	c := &cobra.Command{
		Use:   "create",
		Short: "Create a monitoring template",
		Long: `Create a monitoring template from a set of modules.

The template starts empty of checks: modules say what kind of check belongs
here, and the checks themselves are added with

  ups monitoring item create --template <id> --module <id> --name "CPU"`,
		Example: `  ups monitoring template create --name "Cisco IOS switch" --module 1,2,3
  ups monitoring template create --name "PLC" --module 2 --org 4`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return errs.Usage("--name is required")
			}
			orgID, err := app.resolveOrganization(org)
			if err != nil {
				return err
			}
			body := map[string]any{"name": name, "organization": atoiOr(orgID)}
			if mods := splitIDs(modules); len(mods) > 0 {
				body["monitoring_modules"] = mods
			}
			if rule != "" {
				body["rule"] = atoiOr(rule)
			}
			if parent != "" {
				body["parent_monitoring_template"] = atoiOr(parent)
			}

			var raw jsonRaw
			if err := app.mutate("POST", templatesPath, body, &raw); err != nil {
				return err
			}
			if app.DryRun {
				return nil
			}
			var m row
			_ = jsonUnmarshal(raw, &m)
			id := str(m, "id")
			t, sym := app.Theme(), app.Sym()
			fmt.Fprintf(app.Stderr, "%s Created monitoring template %s (%s)\n",
				t.Green.Apply(sym.OK), name, id)
			fmt.Fprintf(app.Stderr, "  %s add a check: ups monitoring item create --template %s --module <id> --name \"CPU\"\n",
				t.Dim.Apply("next:"), id)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "template name (required)")
	c.Flags().StringSliceVar(&modules, "module", nil, "monitoring module ids (repeatable or comma-separated)")
	c.Flags().StringVar(&org, "org", "", "organization id (defaults to yours when you belong to exactly one)")
	c.Flags().StringVar(&rule, "rule", "", "template rule id used to match devices")
	c.Flags().StringVar(&parent, "parent", "", "parent template id")
	return c
}

func newMonTemplateUpdateCmd(app *App) *cobra.Command {
	var name, rule string
	var setModules, addModules, removeModules []string
	var publish, unpublish bool

	c := &cobra.Command{
		Use:   "update <id>",
		Short: "Rename a template, change its modules, or publish it",
		Long: `Update a monitoring template.

--module replaces the module set; --add-module and --remove-module adjust it
relative to what is already there.

--publish marks the template ready to apply. A template whose checks have never
been confirmed to collect data is the silent-failure case this whole tool exists
to prevent, so publishing an incomplete template asks first.`,
		Example: `  ups monitoring template update 7 --name "Cisco IOS switch"
  ups monitoring template update 7 --add-module 9
  ups monitoring template update 7 --publish`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if publish && unpublish {
				return errs.Usage("--publish and --unpublish are mutually exclusive")
			}
			body := map[string]any{}
			addIf(body, "name", name)
			if rule != "" {
				body["rule"] = atoiOr(rule)
			}
			switch {
			case publish:
				if err := app.confirmPublishable(args[0]); err != nil {
					return err
				}
				body["published_status"] = "published"
			case unpublish:
				body["published_status"] = "draft"
			}

			if mods, err := app.resolveModuleSet(args[0], setModules, addModules, removeModules); err != nil {
				return err
			} else if mods != nil {
				body["monitoring_modules"] = mods
			}

			if len(body) == 0 {
				return errs.Usage("nothing to update").
					WithHint("pass --name, --module, --add-module, --remove-module, --rule, --publish or --unpublish")
			}
			if err := app.mutate("PATCH", templatesPath+args[0]+"/", body, nil); err != nil {
				return err
			}
			if !app.DryRun {
				app.Printer.Infof("Updated monitoring template %s.", args[0])
			}
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "new name")
	c.Flags().StringSliceVar(&setModules, "module", nil, "replace the module set with these ids")
	c.Flags().StringSliceVar(&addModules, "add-module", nil, "add these module ids")
	c.Flags().StringSliceVar(&removeModules, "remove-module", nil, "remove these module ids")
	c.Flags().StringVar(&rule, "rule", "", "template rule id used to match devices")
	c.Flags().BoolVar(&publish, "publish", false, "mark the template published")
	c.Flags().BoolVar(&unpublish, "unpublish", false, "return the template to draft")
	return c
}

func newMonTemplateDeleteCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a monitoring template",
		Long: `Delete a monitoring template.

Hosts already carrying its items keep them: the items were copied at apply
time, not linked. Deleting the template removes the ability to reapply it, not
the monitoring it produced.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, _, err := app.getOne(templatesPath+args[0]+"/", nil)
			if err != nil {
				return err
			}
			if err := app.Confirm(fmt.Sprintf(
				"Delete monitoring template %s (%s)? Hosts keep the items it already created.",
				args[0], dash(str(m, "name")))); err != nil {
				return err
			}
			if err := app.mutate("DELETE", templatesPath+args[0]+"/", nil, nil); err != nil {
				return err
			}
			if !app.DryRun {
				app.Printer.Infof("Deleted monitoring template %s.", args[0])
			}
			return nil
		},
	}
}

// newMonTemplatePreflightCmd reports the system credentials a template needs but
// cannot find in an infrastructure. Applying without them produces items that
// authenticate against nothing and collect nothing.
func newMonTemplatePreflightCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "preflight <id>",
		Short: "Check a template for missing credentials before applying it",
		Long: `Report credentials the template's checks need but the infrastructure lacks.

A check with no credential does not fail loudly. It collects nothing, and the
gap only surfaces during the incident it should have caught.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			missing, err := app.templateMissingCredentials(args[0])
			if err != nil {
				return err
			}
			t, sym := app.Theme(), app.Sym()
			if app.AsJSON {
				return app.Printer.JSON(map[string]any{
					"template": args[0], "missing": missing, "ready": len(missing) == 0,
				})
			}
			if len(missing) == 0 {
				fmt.Fprintf(app.Stdout, "%s Template %s has every credential it needs.\n",
					t.Green.Apply(sym.OK), args[0])
				return nil
			}
			fmt.Fprintf(app.Stdout, "%s Template %s is missing credentials:\n",
				t.Red.Apply(sym.Fail), args[0])
			for _, infra := range sortedKeys(missing) {
				fmt.Fprintf(app.Stdout, "  infrastructure %s:\n", infra)
				for _, c := range missing[infra] {
					fmt.Fprintf(app.Stdout, "    %s %s\n", sym.Bullet, c)
				}
			}
			return errs.Conflict("template %s is not ready to apply", args[0]).
				WithHint("store the missing system credentials: ups credential create <type> --name ...")
		},
	}
}

func newMonTemplateApplyCmd(app *App) *cobra.Command {
	var hosts []string
	var skipPreflight bool

	c := &cobra.Command{
		Use:   "apply <id> --host <host-id>",
		Short: "Apply a template to hosts, replacing their monitoring",
		Long: `Apply a monitoring template to one or more hosts.

This is a replacement, not a merge. Every monitoring item currently on the host
is deleted and the template's items are created in their place, so any check
added by hand is lost. The items that would be removed are listed before
anything is written.

Preflight runs first unless --skip-preflight is given.`,
		Example: `  ups monitoring template apply 7 --host 12
  ups monitoring template apply 7 --host 12,14,15`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := splitStrings(hosts)
			if len(target) == 0 {
				return errs.Usage("--host is required")
			}
			tpl, _, err := app.getOne(templatesPath+args[0]+"/", nil)
			if err != nil {
				return err
			}
			t, sym := app.Theme(), app.Sym()

			if str(tpl, "published_status") == "draft" {
				fmt.Fprintf(app.Stderr, "%s Template %s is a draft; its checks may never have collected data.\n",
					t.Yellow.Apply(sym.Warn), args[0])
			}

			if !skipPreflight {
				missing, err := app.templateMissingCredentials(args[0])
				if err != nil {
					return err
				}
				if len(missing) > 0 {
					for _, infra := range sortedKeys(missing) {
						for _, c := range missing[infra] {
							fmt.Fprintf(app.Stderr, "  %s infrastructure %s: %s\n", sym.Bullet, infra, c)
						}
					}
					return errs.Conflict("template %s is missing credentials", args[0]).
						WithHint("run: ups monitoring template preflight %s", args[0])
				}
			}

			// What the apply will destroy, counted per host before any write.
			// The API deletes silently; the operator should not have to guess.
			doomed := map[string][]string{}
			total := 0
			for _, h := range target {
				q := url.Values{}
				q.Set("host", h)
				items, err := app.fetchRows("/api/monitoring/items/", q)
				if err != nil {
					return err
				}
				for _, it := range items {
					doomed[h] = append(doomed[h], dash(str(it, "name")))
				}
				total += len(items)
			}

			if total > 0 {
				fmt.Fprintf(app.Stderr, "%s Applying this template removes %d existing monitoring item(s):\n",
					t.Yellow.Apply(sym.Warn), total)
				for _, h := range target {
					if len(doomed[h]) == 0 {
						continue
					}
					fmt.Fprintf(app.Stderr, "  host %s: %s\n", h, strings.Join(doomed[h], ", "))
				}
			}
			if err := app.Confirm(fmt.Sprintf(
				"Apply template %s (%s) to %d host(s), replacing %d existing item(s)?",
				args[0], dash(str(tpl, "name")), len(target), total)); err != nil {
				return err
			}

			body := map[string]any{"monitoring_template": atoiOr(args[0])}
			for _, h := range target {
				if err := app.mutate("PATCH", "/api/host/"+h+"/", body, nil); err != nil {
					return errs.General("applying to host %s failed", h).Wrapping(err)
				}
				if !app.DryRun {
					fmt.Fprintf(app.Stderr, "%s Applied to host %s\n", t.Green.Apply(sym.OK), h)
				}
			}
			if app.DryRun {
				return nil
			}
			fmt.Fprintf(app.Stderr, "  %s the new items are untested. Confirm they collect data:\n", t.Dim.Apply("next:"))
			fmt.Fprintf(app.Stderr, "        ups monitoring item list --host %s\n", target[0])
			return nil
		},
	}
	c.Flags().StringSliceVar(&hosts, "host", nil, "host ids to apply to (repeatable or comma-separated)")
	c.Flags().BoolVar(&skipPreflight, "skip-preflight", false, "apply without checking for missing credentials")
	return c
}

// --- helpers -------------------------------------------------------------

// templateMissingCredentials maps infrastructure id to descriptions of the
// system credentials the template needs and the infrastructure does not have.
func (a *App) templateMissingCredentials(id string) (map[string][]string, error) {
	q := url.Values{}
	q.Set("monitoring_template_id", id)
	m, _, err := a.getOne("/api/monitoring/templates/infra-system-credential/", q)
	if err != nil {
		return nil, err
	}

	active := ""
	if a.Resolved != nil && a.Resolved.Infrastructure.IsSet() {
		active = a.Resolved.Infrastructure.Value
	}

	out := map[string][]string{}
	for infra, v := range m {
		// The endpoint reports every infrastructure the caller can see. When
		// one is pinned, the others are noise for this apply.
		if active != "" && infra != active {
			continue
		}
		list, ok := v.([]any)
		if !ok {
			continue
		}
		for _, e := range list {
			c, ok := e.(map[string]any)
			if !ok {
				continue
			}
			desc := dash(str(row(c), "name"))
			if tag := str(row(c), "tag"); tag != "" {
				desc += " (tag " + tag + ")"
			}
			if kind := str(row(c), "type"); kind != "" {
				desc += " [" + kind + "]"
			}
			out[infra] = append(out[infra], desc)
		}
		if len(out[infra]) == 0 {
			delete(out, infra)
		}
	}
	return out, nil
}

// resolveModuleSet turns --module/--add-module/--remove-module into the full
// list the API expects, because PATCH replaces the set rather than merging it.
func (a *App) resolveModuleSet(id string, set, add, remove []string) ([]any, error) {
	setIDs, addIDs, removeIDs := splitIDs(set), splitIDs(add), splitIDs(remove)
	if len(setIDs) == 0 && len(addIDs) == 0 && len(removeIDs) == 0 {
		return nil, nil
	}
	if len(setIDs) > 0 && (len(addIDs) > 0 || len(removeIDs) > 0) {
		return nil, errs.Usage("--module replaces the whole set; use it alone, not with --add-module or --remove-module")
	}
	if len(setIDs) > 0 {
		return setIDs, nil
	}

	current, _, err := a.getOne(templatesPath+id+"/", nil)
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, existing := range moduleIDs(current) {
		keep[existing] = true
	}
	for _, m := range addIDs {
		keep[fmt.Sprintf("%v", m)] = true
	}
	for _, m := range removeIDs {
		delete(keep, fmt.Sprintf("%v", m))
	}
	out := make([]any, 0, len(keep))
	for _, k := range sortedBoolKeys(keep) {
		out = append(out, atoiOr(k))
	}
	return out, nil
}

// confirmPublishable warns when a template's checks have not been confirmed to
// collect data. Publishing says "safe to apply everywhere"; that claim should
// not be made silently.
func (a *App) confirmPublishable(id string) error {
	q := url.Values{}
	q.Set("monitoring_template", id)
	items, err := a.fetchRows("/api/monitoring/items/", q)
	if err != nil {
		return err
	}
	t, sym := a.Theme(), a.Sym()
	if len(items) == 0 {
		return a.Confirm(fmt.Sprintf(
			"Template %s has no checks. Publishing it means applying it removes a host's monitoring and adds nothing. Continue?", id))
	}
	var incomplete []string
	for _, it := range items {
		if strings.EqualFold(str(it, "monitoring_item_config_status"), "INCOMPLETE") {
			incomplete = append(incomplete, dash(str(it, "name")))
		}
	}
	if len(incomplete) == 0 {
		return nil
	}
	fmt.Fprintf(a.Stderr, "%s %d check(s) in this template are incomplete: %s\n",
		t.Yellow.Apply(sym.Warn), len(incomplete), strings.Join(incomplete, ", "))
	return a.Confirm(fmt.Sprintf(
		"Publish template %s anyway? An incomplete check collects nothing and never alerts.", id))
}

// resolveOrganization returns the organization to write to, inferring it when
// the caller belongs to exactly one rather than guessing among several.
func (a *App) resolveOrganization(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	c, err := a.Client()
	if err != nil {
		return "", err
	}
	ctx, cancel := a.Ctx()
	defer cancel()
	var raw map[string]any
	if err := c.Do(ctx, request("GET", "/api/user/details/v2/", nil), &raw); err != nil {
		if errs.CodeOf(err) != errs.CodeNotFound {
			return "", err
		}
		if err := c.Do(ctx, request("GET", "/api/user/details/", nil), &raw); err != nil {
			return "", err
		}
	}
	orgs, _ := raw["organizations"].(map[string]any)
	var ids, labels []string
	for key, v := range orgs {
		id := key
		label := key
		if m, ok := v.(map[string]any); ok {
			if got := str(row(m), "id"); got != "" {
				id = got
			}
			if name := str(row(m), "name"); name != "" {
				label = name + " (" + id + ")"
			}
		}
		ids = append(ids, id)
		labels = append(labels, label)
	}
	sort.Strings(ids)
	sort.Strings(labels)
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return "", errs.Usage("--org is required: no organization could be determined for you").
			WithHint("run: ups whoami")
	default:
		return "", errs.Usage("--org is required: you belong to %d organizations (%s)",
			len(ids), strings.Join(labels, ", "))
	}
}

// moduleNames reads the nested module objects the GET serializer returns.
func moduleNames(m row) []string {
	var out []string
	for _, mod := range nestedModules(m) {
		if n := str(mod, "name"); n != "" {
			out = append(out, n)
		}
	}
	return out
}

func moduleIDs(m row) []string {
	var out []string
	for _, mod := range nestedModules(m) {
		if id := str(mod, "id"); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// nestedModules copes with both shapes the API returns: nested objects on GET,
// bare ids after a write.
func nestedModules(m row) []row {
	list, ok := m["monitoring_modules"].([]any)
	if !ok {
		return nil
	}
	out := make([]row, 0, len(list))
	for _, e := range list {
		switch v := e.(type) {
		case map[string]any:
			out = append(out, row(v))
		case float64, string:
			out = append(out, row{"id": v, "name": fmt.Sprintf("%v", v)})
		}
	}
	return out
}

func readyLabel(m row) string {
	if strings.EqualFold(str(m, "monitoring_item_config_status"), "COMPLETE") {
		return "yes"
	}
	return "no"
}

// splitStrings flattens comma-separated and repeated flag values.
func splitStrings(in []string) []string {
	var out []string
	for _, v := range in {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func splitIDs(in []string) []any {
	parts := splitStrings(in)
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		out = append(out, atoiOr(p))
	}
	return out
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// templateItemOrg validates that a new item will actually land in the template
// the caller named, and returns the organization to file it under.
//
// Template membership is indirect: an item belongs to a template because its
// module is in that template's module set. Two consequences are invisible from
// the command line and both are checked here — an item whose module is not in
// the set is created but never applied, and an item whose module is shared with
// another template silently joins that one too.
func (a *App) templateItemOrg(template, module, explicitOrg string) (string, error) {
	if module == "" {
		return "", errs.Usage("--module is required for a template item").
			WithHint("a template item is bound to its template through its module: ups monitoring module list")
	}
	tpl, _, err := a.getOne(templatesPath+template+"/", nil)
	if err != nil {
		return "", err
	}

	inSet := false
	for _, id := range moduleIDs(tpl) {
		if id == module {
			inSet = true
			break
		}
	}
	if !inSet {
		return "", errs.Conflict("module %s is not part of template %s, so the item would never be applied", module, template).
			WithHint("add it first: ups monitoring template update %s --add-module %s", template, module)
	}

	if err := a.warnSharedModule(template, module); err != nil {
		return "", err
	}

	org := explicitOrg
	if org == "" {
		org = str(tpl, "organization")
	}
	return a.resolveOrganization(org)
}

// warnSharedModule reports the other templates that will inherit this item.
func (a *App) warnSharedModule(template, module string) error {
	others, err := a.fetchRows(templatesPath, nil)
	if err != nil {
		// A warning is not worth failing the create over.
		return nil
	}
	var sharing []string
	for _, t := range others {
		id := str(t, "id")
		if id == template {
			continue
		}
		for _, m := range moduleIDs(t) {
			if m == module {
				sharing = append(sharing, fmt.Sprintf("%s (%s)", dash(str(t, "name")), id))
				break
			}
		}
	}
	if len(sharing) == 0 {
		return nil
	}
	th, sym := a.Theme(), a.Sym()
	fmt.Fprintf(a.Stderr, "%s Module %s is also in %s. This check joins those templates too.\n",
		th.Yellow.Apply(sym.Warn), module, strings.Join(sharing, ", "))
	return a.Confirm("Add the check anyway?")
}
