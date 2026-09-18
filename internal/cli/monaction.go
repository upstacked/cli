package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
)

const actionsPath = "/api/monitoring/actions/"

// newMonActionCmd lists the data sources an item can be built on.
//
// The field is called action_type on the wire and data_source in the item
// list's query parameters, and the dry run reports it under a third set of
// worker names. Whatever it is called, it is the thing that decides whether a
// check speaks SNMP, HTTP or ICMP, and an item without one polls nothing.
func newMonActionCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "action",
		Aliases: []string{"actions", "data-source"},
		Short:   "Data sources a monitoring item can be built on",
		Long: `List the data sources available to a monitoring item.

Each row is a (type, name) pair: the type is the protocol - snmp, icmp,
api_data, meraki and so on - and the name is what that source does, because
several types offer more than one. SNMP has both 'get' and 'walk', and they
are not interchangeable.

'ups monitoring item create --data-source' accepts an id, a "type:name" pair,
or a bare type when that type offers exactly one action. It never picks
between two.`,
		Example: `  ups monitoring action list
  ups monitoring item create --host 12 --name CPU --module 3 --data-source snmp:walk`,
	}
	c.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the available data sources",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runList(listOpts{
				Path:    actionsPath,
				Columns: []string{"ID", "TYPE", "NAME"},
				Empty:   "No data sources available.",
				Cells: func(m row) []string {
					return []string{str(m, "id"), dash(str(m, "type")), dash(str(m, "name"))}
				},
			})
		},
	})
	return c
}

// resolveDataSource turns what a caller typed into an action id.
//
// A bare type is accepted only when it is unambiguous. Picking one of two SNMP
// actions on the caller's behalf would produce a check that polls the wrong
// way and still reports as created, so an ambiguous spec lists the candidates
// and stops - the same rule a hostname that matches two hosts gets.
func (a *App) resolveDataSource(spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", nil
	}
	if isAllDigits(spec) {
		return spec, nil
	}

	rows, err := a.fetchRows(actionsPath, nil)
	if err != nil {
		return "", err
	}

	wantType, wantName, hasName := strings.Cut(spec, ":")
	wantType = strings.ToLower(strings.TrimSpace(wantType))
	wantName = strings.ToLower(strings.TrimSpace(wantName))

	var matches []row
	for _, r := range rows {
		typ := strings.ToLower(str(r, "type"))
		name := strings.ToLower(str(r, "name"))
		if hasName {
			if typ == wantType && name == wantName {
				matches = append(matches, r)
			}
			continue
		}
		// Without a name, accept a match on either half: callers think in
		// protocols ("snmp") but the catalogue is also searchable by what the
		// action does ("ping_host").
		if typ == wantType || name == wantType {
			matches = append(matches, r)
		}
	}

	switch len(matches) {
	case 1:
		return str(matches[0], "id"), nil
	case 0:
		return "", errs.NotFound("no data source matching %q", spec).
			WithHint("list them: ups monitoring action list")
	default:
		return "", errs.Conflict("%q matches %d data sources", spec, len(matches)).
			WithHint("name one of: %s", strings.Join(describeActions(matches), ", "))
	}
}

func describeActions(rows []row) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%s:%s", str(r, "type"), str(r, "name")))
	}
	sort.Strings(out)
	return out
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
