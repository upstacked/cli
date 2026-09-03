package cli

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/iac"
)

// exportInfra reads the live state of an infrastructure into a Document.
//
// Truncation here would be dangerous: a host missing from the export looks
// like a deletion to diff. So a capped traversal is a hard error, not a note.
func (a *App) exportInfra(infraID string) (*iac.Document, error) {
	cl, err := a.Client()
	if err != nil {
		return nil, err
	}
	ctx, cancel := a.Ctx()
	defer cancel()

	infraName := ""
	if m, _, err := a.getOne("/api/infrastructure/"+infraID+"/", nil); err == nil {
		infraName = str(m, "name", "infra_code")
	}

	q := url.Values{}
	q.Set("infrastructure", infraID)
	hosts, err := cl.GetList(ctx, request("GET", "/api/host/", q), 0)
	if err != nil {
		return nil, err
	}
	if hosts.Truncated {
		return nil, errs.General("the host list was truncated at %d records", len(hosts.Items)).
			WithHint("an incomplete export would look like deletions to 'ups diff'; this is refused rather than risked")
	}

	items, err := cl.GetList(ctx, request("GET", "/api/monitoring/items/", q), 0)
	if err != nil {
		return nil, err
	}
	if items.Truncated {
		return nil, errs.General("the monitoring item list was truncated at %d records", len(items.Items)).
			WithHint("an incomplete export would look like deletions to 'ups diff'; this is refused rather than risked")
	}

	// Index monitoring items by host id.
	byHost := map[string][]iac.MonitoringItem{}
	for _, m := range decodeRows(items.Items) {
		hostID := str(m, "host")
		byHost[hostID] = append(byHost[hostID], iac.MonitoringItem{
			ID:         str(m, "id"),
			Name:       str(m, "name"),
			Module:     str(m, "monitoring_module"),
			Interval:   str(m, "interval"),
			Parameters: str(m, "parameters"),
			CredType:   str(m, "credential_type"),
			Credential: str(m, "credential"),
		})
	}

	doc := &iac.Document{
		APIVersion:     iac.APIVersion,
		Infrastructure: iac.InfrastructureR{ID: infraID, Name: infraName},
	}
	used := map[string]bool{}
	for _, m := range decodeRows(hosts.Items) {
		id := str(m, "id")
		tpl := str(m, "monitoring_template_name")
		if tpl == "" {
			tpl = str(m, "monitoring_template")
		}
		if tpl != "" {
			used[tpl] = true
		}
		doc.Hosts = append(doc.Hosts, iac.Host{
			ID:         id,
			Name:       str(m, "name"),
			Hostname:   str(m, "i_hostname"),
			IP:         str(m, "i_ip_address"),
			MAC:        str(m, "i_mac_address"),
			Type:       str(m, "i_type"),
			Serial:     str(m, "i_serial"),
			Template:   tpl,
			Monitoring: byHost[id],
		})
	}

	// Only the templates these hosts actually use. Templates are an
	// organization-wide library; exporting the whole library into every
	// infrastructure's document would have each infrastructure's apply
	// fighting the others over resources it does not own.
	tpls, err := a.exportTemplates(used)
	if err != nil {
		return nil, err
	}
	doc.Templates = tpls

	doc.Normalize()
	return doc, nil
}

// exportTemplates reads the named templates and the host-less checks inside
// them.
func (a *App) exportTemplates(used map[string]bool) ([]iac.Template, error) {
	if len(used) == 0 {
		return nil, nil
	}
	rows, err := a.fetchRows(templatesPath, nil)
	if err != nil {
		return nil, err
	}
	var out []iac.Template
	for _, m := range rows {
		name := str(m, "name")
		if !used[name] && !used[str(m, "id")] {
			continue
		}
		id := str(m, "id")
		t := iac.Template{
			ID:           id,
			Name:         name,
			Status:       str(m, "published_status"),
			Organization: str(m, "organization"),
			Modules:      moduleIDs(m),
		}
		q := url.Values{}
		q.Set("monitoring_template", id)
		checks, err := a.fetchRows("/api/monitoring/items/", q)
		if err != nil {
			return nil, err
		}
		for _, c := range checks {
			t.Checks = append(t.Checks, iac.MonitoringItem{
				ID:         str(c, "id"),
				Name:       str(c, "name"),
				Module:     str(c, "monitoring_module"),
				Interval:   str(c, "interval"),
				Parameters: str(c, "parameters"),
				CredType:   str(c, "credential_type"),
				Credential: str(c, "credential"),
			})
		}
		out = append(out, t)
	}
	return out, nil
}

func newExportCmd(app *App) *cobra.Command {
	var out string
	c := &cobra.Command{
		Use:   "export",
		Short: "Export an infrastructure to YAML",
		Long: `Write the live state of an infrastructure to YAML you can commit.

--out takes either a file or a directory. A directory gives one file per
host and one per monitoring template, which keeps git diffs small and
reviewable on a real infrastructure:

  infra/
    infrastructure.yaml
    templates/
      cisco-ios-switch.yaml
    hosts/
      core-sw-01.yaml
      fw-01.yaml

A host carrying a template records it in a template: field, and only the
templates the hosts actually use are exported. Editing a check in the
template file is then one small diff instead of the same edit repeated on
every host.

Output is sorted, and filenames are derived from host names, so re-exporting
unchanged state produces identical bytes. If it did not, every export would
look like a diff.

Re-exporting into a directory removes host files whose resource no longer
exists on the platform, and says which ones. A leftover file would read as a
host to create on the next apply.`,
		Example: `  ups export --out ./infra/              # one file per host
  ups export --out infra.yaml            # single file
  ups export --infra 42 --out customers/acme/`,
		RunE: func(cmd *cobra.Command, args []string) error {
			infraID, err := app.Resolved.RequireInfra()
			if err != nil {
				return err
			}
			var doc *iac.Document
			if err := app.Spin("Exporting infrastructure "+infraID, func() error {
				var e error
				doc, e = app.exportInfra(infraID)
				return e
			}); err != nil {
				return err
			}
			if out == "" || out == "-" {
				b, err := iac.Marshal(doc)
				if err != nil {
					return errs.General("cannot serialize the document").Wrapping(err)
				}
				fmt.Fprint(app.Stdout, string(b))
				return nil
			}

			res, err := iac.Save(doc, out)
			if err != nil {
				return errs.General("cannot write %s", out).Wrapping(err)
			}
			t, sym := app.Theme(), app.Sym()
			if res.Directory {
				fmt.Fprintf(app.Stderr, "%s Wrote %d host(s) to %s%c\n",
					t.Green.Apply(sym.OK), len(doc.Hosts), strings.TrimRight(out, "/"), os.PathSeparator)
				for _, r := range res.Removed {
					// Say so explicitly: a silently vanishing file is
					// indistinguishable from one the user deleted themselves.
					fmt.Fprintf(app.Stderr, "  %s removed %s (no longer on the platform)\n",
						t.Yellow.Apply("-"), r)
				}
			} else {
				fmt.Fprintf(app.Stderr, "%s Wrote %d host(s) to %s\n",
					t.Green.Apply(sym.OK), len(doc.Hosts), out)
			}
			return nil
		},
	}
	c.Flags().StringVarP(&out, "out", "o", "",
		"where to write: a .yaml file, or a directory for one file per host (default stdout)")
	return c
}

// loadDocument reads and validates a local document.
func loadDocument(path string) (*iac.Document, error) {
	doc, err := iac.Load(path)
	if err != nil {
		return nil, errs.Usage("cannot read %s: %v", path, err).
			WithHint("point at a file from 'ups export --out x.yaml', or a directory from 'ups export --out ./infra/'")
	}
	if err := doc.Validate(); err != nil {
		return nil, errs.Usage("%s is not valid: %v", path, err)
	}
	return doc, nil
}

// planFor loads local state, exports remote state, and diffs them.
func (a *App) planFor(path string) (*iac.Plan, *iac.Document, string, error) {
	local, err := loadDocument(path)
	if err != nil {
		return nil, nil, "", err
	}
	infraID := local.Infrastructure.ID
	if a.Resolved.Infrastructure.Source == "flag" || infraID == "" {
		if id, err := a.Resolved.RequireInfra(); err == nil {
			infraID = id
		}
	}
	if infraID == "" {
		return nil, nil, "", errs.Usage("the document does not name an infrastructure").
			WithHint("add infrastructure.id, or pass --infra")
	}

	var remote *iac.Document
	if err := a.Spin("Reading current state", func() error {
		var e error
		remote, e = a.exportInfra(infraID)
		return e
	}); err != nil {
		return nil, nil, "", err
	}

	plan := iac.Diff(local, remote)
	if len(plan.Steps) > iac.MaxPlanSteps {
		return nil, nil, "", errs.General("the plan has %d steps, above the %d limit",
			len(plan.Steps), iac.MaxPlanSteps).
			WithHint("this usually means the document targets the wrong infrastructure. Check infrastructure.id")
	}
	return plan, local, infraID, nil
}

// renderPlan prints a plan the way a human should read it: deletions last and
// visually distinct, because those are the steps that remove monitoring.
func (a *App) renderPlan(plan *iac.Plan, infraID string) {
	t, sym := a.Theme(), a.Sym()
	if plan.Empty() {
		fmt.Fprintf(a.Stdout, "%s No changes. Infrastructure %s matches the document.\n",
			t.Green.Apply(sym.OK), infraID)
		return
	}
	fmt.Fprintf(a.Stdout, "\nPlan for infrastructure %s:\n\n", t.Bold.Apply(infraID))
	for _, s := range plan.Steps {
		switch s.Action {
		case iac.ActionCreate:
			fmt.Fprintf(a.Stdout, "  %s %s\n", t.Green.Apply("+"), s.String())
		case iac.ActionUpdate:
			fmt.Fprintf(a.Stdout, "  %s %s\n", t.Yellow.Apply("~"), s.String())
		case iac.ActionDelete:
			fmt.Fprintf(a.Stdout, "  %s %s\n", t.Red.Apply("-"), s.String())
		}
		// A template assignment reads as a one-field update but removes every
		// check on the host. Name them under the step, or the plan understates
		// what it does by an order of magnitude.
		for _, gone := range s.Replaces {
			fmt.Fprintf(a.Stdout, "      %s %s (removed by the template change)\n",
				t.Red.Apply("-"), gone)
		}
	}
	create, update, del := plan.Counts()
	fmt.Fprintf(a.Stdout, "\n%d to create, %d to update, %d to delete.\n", create, update, del)
	if repl := plan.Replacements(); len(repl) > 0 {
		n := 0
		for _, s := range repl {
			n += len(s.Replaces)
		}
		fmt.Fprintf(a.Stdout, "%s %d existing monitoring item(s) will be replaced by a template.\n",
			t.Red.Apply(sym.Warn), n)
	}
	if plan.TouchesTemplates() {
		fmt.Fprintf(a.Stdout, "\n%s %s\n", t.Yellow.Apply(sym.Warn),
			t.Bold.Apply("Templates belong to the organization, not to this infrastructure."))
		fmt.Fprintf(a.Stdout, "  Editing one here changes it everywhere it is used, including hosts this document does not list.\n")
	}
	if renames := plan.Renames(); len(renames) > 0 {
		fmt.Fprintf(a.Stdout, "\n%s %d rename(s) will keep the existing resource and its history.\n",
			t.Green.Apply(sym.OK), len(renames))
	}
	if del > 0 || len(plan.Replacements()) > 0 {
		fmt.Fprintf(a.Stdout, "\n%s %s\n", t.Red.Apply(sym.Warn),
			t.Bold.Apply("Deleting monitoring is silent: nothing alerts when a check disappears."))
		fmt.Fprintf(a.Stdout, "  Read the deletions above. If they were not intended, the document is wrong.\n")
	}
	// A create paired with a delete of a look-alike resource is almost always
	// a rename attempted on a document whose ids were stripped. Saying so is
	// far more useful than letting the user destroy and recreate the host.
	for _, pair := range plan.LikelyRenames() {
		fmt.Fprintf(a.Stdout, "\n%s %s\n", t.Yellow.Apply(sym.Warn),
			t.Bold.Apply(fmt.Sprintf("%q and %q look like the same %s renamed.",
				pair.From, pair.To, pair.Kind)))
		fmt.Fprintf(a.Stdout, "  As written this deletes one and creates the other, losing its history.\n")
		fmt.Fprintf(a.Stdout, "  To rename in place, keep the `id:` field from `ups export` and change only `name:`.\n")
	}
}

func newDiffCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "diff <path>",
		Short: "Show what apply would change",
		Long: `Compare a local document against the live infrastructure.

<path> is a file or an export directory; both forms describe the same thing.

Read this before every apply. The export covers a whole infrastructure, so a
block missing from the document is a deletion.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, _, infraID, err := app.planFor(args[0])
			if err != nil {
				return err
			}
			if app.AsJSON {
				steps := make([]map[string]any, 0, len(plan.Steps))
				for _, s := range plan.Steps {
					steps = append(steps, map[string]any{
						"action": string(s.Action), "kind": s.Kind, "name": s.Name,
						"host": s.Host, "id": s.ID, "fields": s.Fields,
					})
				}
				create, update, del := plan.Counts()
				return app.Printer.JSON(map[string]any{
					"infrastructure": infraID, "steps": steps,
					"create": create, "update": update, "delete": del,
					"destructive": plan.Destructive(),
				})
			}
			app.renderPlan(plan, infraID)
			return nil
		},
	}
	return c
}

func newApplyCmd(app *App) *cobra.Command {
	var allowDelete bool
	c := &cobra.Command{
		Use:   "apply <path>",
		Short: "Make the infrastructure match the document",
		Long: `Converge the platform to a local document.

apply is idempotent and safe to re-run. It is not safe to run unread: run
'ups diff' first. Deletions require --allow-delete, and that flag is not a
way past an error - if the plan proposes deletions you did not intend, the
document is wrong.`,
		Example: `  ups diff ./infra/
  ups apply ./infra/
  ups apply infra.yaml --allow-delete --yes`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, _, infraID, err := app.planFor(args[0])
			if err != nil {
				return err
			}
			app.renderPlan(plan, infraID)
			if plan.Empty() {
				return nil
			}

			if plan.Destructive() && !allowDelete {
				if n := len(plan.Deletions()); n > 0 {
					return errs.Conflict("the plan deletes %d resource(s)", n).
						WithHint("review them above. If they are intended, re-run with --allow-delete")
				}
				return errs.Conflict("the plan replaces the monitoring on %d host(s) with a template",
					len(plan.Replacements())).
					WithHint("the existing items are listed above and will be deleted. If that is intended, re-run with --allow-delete")
			}
			if app.DryRun {
				app.Printer.Infof("dry-run: %d step(s) not executed", len(plan.Steps))
				return nil
			}
			prompt := fmt.Sprintf("Apply %d change(s) to infrastructure %s?", len(plan.Steps), infraID)
			if plan.Destructive() {
				lost := len(plan.Deletions())
				for _, s := range plan.Replacements() {
					lost += len(s.Replaces)
				}
				prompt = fmt.Sprintf("Apply %d change(s) to infrastructure %s, REMOVING %d resource(s)?",
					len(plan.Steps), infraID, lost)
			}
			if err := app.Confirm(prompt); err != nil {
				return err
			}
			return app.executePlan(plan, infraID)
		},
	}
	c.Flags().BoolVar(&allowDelete, "allow-delete", false, "permit steps that remove resources")
	return c
}

// executePlan runs each step, stopping at the first failure so a broken run
// does not cascade.
func (a *App) executePlan(plan *iac.Plan, infraID string) error {
	t, sym := a.Theme(), a.Sym()
	// Host ids created during this run, so monitoring items can reference them.
	created := map[string]string{}
	// Template ids by name, seeded as templates are created so a host in the
	// same plan can be pointed at one that did not exist a moment ago.
	templates := map[string]string{}

	for i, s := range plan.Steps {
		var err error
		switch {
		case s.Kind == "template" && s.Action == iac.ActionCreate:
			body := copyBody(s.Body)
			if _, ok := body["organization"]; !ok {
				var org string
				if org, err = a.resolveOrganization(""); err != nil {
					return err
				}
				body["organization"] = atoiOr(org)
			}
			var raw jsonRaw
			if err = a.mutate("POST", templatesPath, body, &raw); err == nil {
				var m row
				_ = jsonUnmarshal(raw, &m)
				templates[s.Name] = str(m, "id")
			}
		case s.Kind == "template" && s.Action == iac.ActionUpdate:
			templates[s.Name] = s.ID
			err = a.mutate("PATCH", templatesPath+s.ID+"/", s.Body, nil)
		case s.Kind == "check" && s.Action == iac.ActionCreate:
			// A check is a monitoring item with no host: the template stamps
			// it onto one later, so there is nothing to test here.
			body := copyBody(s.Body)
			if _, ok := body["organization"]; !ok {
				var org string
				if org, err = a.resolveOrganization(""); err != nil {
					return err
				}
				body["organization"] = atoiOr(org)
			}
			err = a.mutate("POST", "/api/monitoring/items/", body, nil)
		case s.Kind == "check" && s.Action == iac.ActionUpdate:
			err = a.mutate("PATCH", "/api/monitoring/items/"+s.ID+"/", s.Body, nil)
		case s.Kind == "check" && s.Action == iac.ActionDelete:
			err = a.mutate("DELETE", "/api/monitoring/items/"+s.ID+"/", nil, nil)
		case s.Kind == "host" && s.Action == iac.ActionCreate:
			body := copyBody(s.Body)
			body["infrastructure"] = atoiOr(infraID)
			if err = a.bindTemplate(body, templates); err != nil {
				return err
			}
			var raw jsonRaw
			if err = a.mutate("POST", "/api/host/", body, &raw); err == nil {
				var m row
				_ = jsonUnmarshal(raw, &m)
				created[s.Name] = str(m, "id")
			}
		case s.Kind == "host" && s.Action == iac.ActionUpdate:
			body := copyBody(s.Body)
			if err = a.bindTemplate(body, templates); err != nil {
				return err
			}
			err = a.mutate("PATCH", "/api/host/"+s.ID+"/", body, nil)
		case s.Kind == "host" && s.Action == iac.ActionDelete:
			err = a.mutate("DELETE", "/api/host/"+s.ID+"/", nil, nil)
		case s.Kind == "monitoring" && s.Action == iac.ActionCreate:
			body := copyBody(s.Body)
			hostID := created[s.Host]
			if hostID == "" {
				if hostID, err = a.hostIDByName(infraID, s.Host); err != nil {
					return err
				}
			}
			body["host"] = atoiOr(hostID)
			err = a.mutate("POST", "/api/monitoring/items/", body, nil)
		case s.Kind == "monitoring" && s.Action == iac.ActionUpdate:
			err = a.mutate("PATCH", "/api/monitoring/items/"+s.ID+"/", s.Body, nil)
		case s.Kind == "monitoring" && s.Action == iac.ActionDelete:
			err = a.mutate("DELETE", "/api/monitoring/items/"+s.ID+"/", nil, nil)
		}

		if err != nil {
			fmt.Fprintf(a.Stderr, "%s step %d/%d failed: %s\n",
				t.Red.Apply(sym.Fail), i+1, len(plan.Steps), s.String())
			return errs.General("apply stopped after %d of %d step(s): %v", i, len(plan.Steps), err).
				WithHint("re-run 'ups diff' to see what remains; apply is idempotent")
		}
		fmt.Fprintf(a.Stderr, "  %s %s\n", t.Green.Apply(sym.OK), s.String())
	}
	fmt.Fprintf(a.Stderr, "\n%s Applied %d change(s).\n", t.Green.Apply(sym.OK), len(plan.Steps))
	return nil
}

// bindTemplate turns the template name the plan carries into the id the API
// expects, and removes the marker so it is never sent.
func (a *App) bindTemplate(body map[string]any, known map[string]string) error {
	name, ok := body[iac.TemplateRefKey].(string)
	delete(body, iac.TemplateRefKey)
	if !ok || name == "" {
		return nil
	}
	id, cached := known[name]
	if !cached {
		var err error
		if id, err = a.templateIDByName(name); err != nil {
			return err
		}
		known[name] = id
	}
	body["monitoring_template"] = atoiOr(id)
	return nil
}

// templateIDByName resolves a template by name. An ambiguous name is an error:
// applying the wrong template replaces a host's monitoring with someone else's.
func (a *App) templateIDByName(name string) (string, error) {
	rows, err := a.fetchRows(templatesPath, nil)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, m := range rows {
		if str(m, "name") == name {
			matches = append(matches, str(m, "id"))
		}
	}
	switch len(matches) {
	case 0:
		return "", errs.NotFound("monitoring template %q not found", name).
			WithHint("declare it under templates: in the document, or create it: ups monitoring template create --name %q", name)
	case 1:
		return matches[0], nil
	default:
		return "", errs.Conflict("monitoring template %q matches %d templates", name, len(matches))
	}
}

// hostIDByName resolves a host by name within an infrastructure. Ambiguity is
// an error: the same name can exist in other customers' infrastructures, and
// picking one arbitrarily is how a change lands on the wrong device.
func (a *App) hostIDByName(infraID, name string) (string, error) {
	cl, err := a.Client()
	if err != nil {
		return "", err
	}
	ctx, cancel := a.Ctx()
	defer cancel()
	q := url.Values{}
	q.Set("infrastructure", infraID)
	q.Set("search", name)
	list, err := cl.GetList(ctx, request("GET", "/api/host/", q), 50)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, m := range decodeRows(list.Items) {
		if str(m, "name") == name {
			matches = append(matches, str(m, "id"))
		}
	}
	switch len(matches) {
	case 0:
		return "", errs.NotFound("host %q not found in infrastructure %s", name, infraID)
	case 1:
		return matches[0], nil
	default:
		return "", errs.Conflict("host name %q matches %d hosts in infrastructure %s",
			name, len(matches), infraID).
			WithHint("names must be unique within an infrastructure for apply to be safe")
	}
}

func copyBody(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}
