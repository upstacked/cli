package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/api"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/output"
)

var placeForm = regexp.MustCompile(`^(location|subnet|host):\d+$`)

// resolvePlace turns what the caller typed into a place the server takes.
// Ids, IPs and ranges are sent as given; anything else is looked up by name,
// and a name matching more than one place is refused rather than guessed,
// because the wrong place gives a confident answer about the wrong traffic.
func (a *App) resolvePlace(infra, spec string) (string, error) {
	switch {
	case spec == "":
		return "", errs.Usage("--between has an empty place").
			WithHint("ups flow summary --between 'NO00 - Oslo',internet")
	case spec == "internet" || placeForm.MatchString(spec) || strings.HasPrefix(spec, "cidr:"):
		return spec, nil
	case net.ParseIP(spec) != nil:
		return "cidr:" + spec, nil
	}
	if _, _, err := net.ParseCIDR(spec); err == nil {
		return "cidr:" + spec, nil
	}

	rows, err := a.fetchRows(flowPlacesPath, url.Values{"hostgroups": {infra}, "q": {spec}})
	if err != nil {
		return "", err
	}
	var exact []row
	for _, r := range rows {
		if strings.EqualFold(str(r, "label"), spec) {
			exact = append(exact, r)
		}
	}
	switch {
	case len(exact) == 1:
		return str(exact[0], "place"), nil
	case len(exact) == 0 && len(rows) == 1:
		return str(rows[0], "place"), nil
	case len(rows) == 0:
		return "", errs.NotFound("no place matches %q", spec).
			WithHint("list places with ups flow places --query <text>")
	}
	candidates := exact
	if len(candidates) == 0 {
		candidates = rows
	}
	listed := make([]string, 0, len(candidates))
	for _, r := range candidates {
		listed = append(listed, fmt.Sprintf("%s (%s)", str(r, "place"), str(r, "label")))
	}
	return "", errs.Usage("%q matches %d places: %s", spec, len(candidates), strings.Join(listed, ", ")).
		WithHint("pass the place id instead, e.g. --between %s", str(candidates[0], "place"))
}

// resolveFlowView finds a saved view by id or name among those the caller can
// see in the infrastructure.
func (a *App) resolveFlowView(infra, spec string) (row, error) {
	views, err := a.fetchRows(flowViewsPath, url.Values{"hostgroups": {infra}})
	if err != nil {
		return nil, err
	}
	var named []row
	for _, v := range views {
		if str(v, "id") == spec {
			return v, nil
		}
		if strings.EqualFold(str(v, "name"), spec) {
			named = append(named, v)
		}
	}
	switch len(named) {
	case 1:
		return named[0], nil
	case 0:
		return nil, errs.NotFound("no saved view %q in this infrastructure", spec).
			WithHint("list them with ups flow view list")
	}
	ids := make([]string, 0, len(named))
	for _, v := range named {
		ids = append(ids, fmt.Sprintf("%s (owner %s)", str(v, "id"), dash(str(v, "owner"))))
	}
	return nil, errs.Usage("%d saved views are named %q: %s", len(named), spec, strings.Join(ids, ", ")).
		WithHint("pass the view id instead")
}

// applyFlowView fills every filter the caller did not give from the saved
// view, so a view is a starting point that flags can still narrow or change.
func (a *App) applyFlowView(cmd *cobra.Command, f *flowQuery) error {
	if f.view == "" {
		return nil
	}
	infra, err := a.Resolved.RequireInfra()
	if err != nil {
		return err
	}
	view, err := a.resolveFlowView(infra, f.view)
	if err != nil {
		return err
	}
	query, _ := view["query"].(map[string]any)
	get := func(key string) string {
		v, _ := query[key].(string)
		return v
	}
	changed := cmd.Flags().Changed
	fill := func(flag, key string, dst *string) {
		if !changed(flag) && get(key) != "" {
			*dst = get(key)
		}
	}

	fill("window", "window", &f.window)
	// A window given on the command line replaces the view's range, not just
	// its window; otherwise the view's fixed range would silently win.
	if !changed("window") || changed("start") || changed("end") {
		fill("start", "start", &f.start)
		fill("end", "end", &f.end)
	}
	fill("seen-by", "seen_by", &f.seenBy)
	fill("query", "q", &f.query)
	fill("service", "service", &f.service)
	fill("host", "host", &f.host)
	if !changed("between") {
		f.between = nil
		for _, key := range []string{"a", "b"} {
			if p := get(key); p != "" {
				f.between = append(f.between, p)
			}
		}
	}
	if !changed("include-monitoring") && get("include_monitoring") == "true" {
		f.includeMonitoring = true
	}
	return nil
}

func newFlowPlacesCmd(app *App) *cobra.Command {
	var query string
	c := &cobra.Command{
		Use:   "places",
		Short: "List the places --between accepts",
		Long: `List the places --between accepts in the active infrastructure: its
customer's locations and subnets, its devices, and the internet.

PLACE is what to pass to --between. A location covers every subnet IPAM puts
there and the devices tied to it; a location with neither covers nothing and
is refused. Any IP or CIDR is also accepted as it is.`,
		Example: `  ups flow places
  ups flow places --query oslo`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			infra, err := app.Resolved.RequireInfra()
			if err != nil {
				return err
			}
			q := url.Values{"hostgroups": {infra}}
			setIf(q, "q", query)
			client, err := app.Client()
			if err != nil {
				return err
			}
			ctx, cancel := app.Ctx()
			defer cancel()
			list, err := client.GetList(ctx, api.Request{Method: "GET", Path: flowPlacesPath, Query: q}, app.Limit)
			if err != nil {
				return err
			}
			t := &output.Table{
				Columns:   []string{"PLACE", "KIND", "LABEL", "DETAIL"},
				Truncated: list.Truncated,
				Total:     list.Count,
				Empty:     "No places match. Any IP or CIDR can be passed to --between as it is.",
			}
			for i, m := range decodeRows(list.Items) {
				t.Add(str(m, "place"), list.Items[i],
					str(m, "place"), dash(str(m, "kind")), dash(str(m, "label")), dash(str(m, "detail")))
			}
			return app.Printer.Print(t)
		},
	}
	c.Flags().StringVar(&query, "query", "", "match a name, range or IP")
	return c
}

func newFlowViewCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "view",
		Short: "Save flow searches and open them again",
		Long: `Save a flow search under a name and open it again with --view.

A view saved with --window stays relative: "1h" means the hour before it is
opened, every time. A view saved with --start/--end always shows that fixed
range. --shared lets everyone who can see the infrastructure use it; only its
owner can change or delete it.`,
	}
	c.AddCommand(newFlowViewListCmd(app), newFlowViewSaveCmd(app), newFlowViewDeleteCmd(app))
	return c
}

func newFlowViewListCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List saved flow views",
		Long:    `List your saved flow views and the ones others shared, in the active infrastructure.`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			infra, err := app.Resolved.RequireInfra()
			if err != nil {
				return err
			}
			return app.runList(listOpts{
				Path:    flowViewsPath,
				Query:   url.Values{"hostgroups": {infra}},
				Columns: []string{"ID", "NAME", "SEARCH", "SHARED", "OWNER"},
				Empty:   "No saved views. Save one with ups flow view save <name> [filters].",
				Cells: func(m row) []string {
					return []string{str(m, "id"), str(m, "name"), viewQueryLabel(m["query"]),
						yesNo(m["shared"]), dash(str(m, "owner"))}
				},
			})
		},
	}
}

func viewQueryLabel(v any) string {
	query, _ := v.(map[string]any)
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, query[k]))
	}
	return dash(strings.Join(parts, " "))
}

func newFlowViewSaveCmd(app *App) *cobra.Command {
	f := flowQuery{window: "1h"}
	var shared bool
	c := &cobra.Command{
		Use:   "save <name>",
		Short: "Save the given filters as a named view",
		Long: `Save the given filters as a named view in the active infrastructure.

Places given by name are resolved now and saved by id, so the view keeps
meaning the same place if another one is later given the same name.`,
		Example: `  ups flow view save 'Oslo to internet' --between 'NO00 - Oslo',internet
  ups flow view save 'OT web, last day' --between subnet:3 --service 'https (TCP/443)' --window 24h --shared`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q, err := app.flowQueryValues(f)
			if err != nil {
				return err
			}
			infra, err := strconv.Atoi(q.Get("hostgroups"))
			if err != nil {
				return errs.Usage("the active infrastructure %q is not an id", q.Get("hostgroups"))
			}
			q.Del("hostgroups")
			query := map[string]string{}
			for k := range q {
				query[k] = q.Get(k)
			}
			var raw jsonRaw
			body := map[string]any{"name": args[0], "infrastructure": infra, "query": query, "shared": shared}
			if err := app.create(flowViewsPath, body, &raw); err != nil {
				return err
			}
			if !app.AsJSON && !app.IDOnly && !app.DryRun {
				var m row
				_ = json.Unmarshal(raw, &m)
				app.Printer.Infof("Saved view %s (%s). Open it with ups flow summary --view %s.",
					str(m, "id"), args[0], str(m, "id"))
			}
			return nil
		},
	}
	addFlowFlags(c, &f)
	addFlowFilterFlags(c, &f)
	c.Flags().BoolVar(&shared, "shared", false, "let everyone who can see the infrastructure use it")
	return c
}

func newFlowViewDeleteCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id|name>",
		Short: "Delete a saved flow view",
		Long:  `Delete a saved flow view. A shared view disappears for everyone who used it.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			infra, err := app.Resolved.RequireInfra()
			if err != nil {
				return err
			}
			view, err := app.resolveFlowView(infra, args[0])
			if err != nil {
				return err
			}
			id := str(view, "id")
			prompt := fmt.Sprintf("Delete saved view %s (%s)?", id, str(view, "name"))
			if b, _ := view["shared"].(bool); b {
				prompt = fmt.Sprintf("Delete saved view %s (%s)? It is shared: it disappears for everyone.", id, str(view, "name"))
			}
			if err := app.Confirm(prompt); err != nil {
				return err
			}
			if err := app.mutate("DELETE", flowViewsPath+id+"/", nil, nil); err != nil {
				return err
			}
			if !app.DryRun {
				app.Printer.Infof("Deleted saved view %s.", id)
			}
			return nil
		},
	}
}
