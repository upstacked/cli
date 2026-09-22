package cli

import (
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
)

const (
	intervalsPath      = "/api/monitoring/intervals/"
	credentialTagsPath = "/api/credential/tags/"
	credentialsPath    = "/api/credential/credentials/"
)

var intervalUnits = map[string]string{"m": "minutes", "h": "hours", "d": "days"}

func newMonIntervalCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:     "interval",
		Aliases: []string{"intervals"},
		Short:   "Polling intervals an item can use",
	}
	c.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the polling intervals, by duration",
		Long: `List the polling intervals the server offers.

An item's interval is one of these, not a free duration. --interval on item
create and update takes the duration shown here (5m, 1h, 1d).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runList(listOpts{
				Path:    intervalsPath,
				Columns: []string{"ID", "DURATION"},
				Empty:   "No polling intervals defined.",
				Cells: func(m row) []string {
					return []string{str(m, "id"), intervalLabel(m)}
				},
			})
		},
	})
	return c
}

func intervalLabel(m row) string {
	period := str(m, "interval_period")
	for short, long := range intervalUnits {
		if long == period {
			return str(m, "number_of_period") + short
		}
	}
	return str(m, "number_of_period") + " " + period
}

func intervalSeconds(m row) int {
	n, _ := strconv.Atoi(str(m, "number_of_period"))
	return n * map[string]int{"minutes": 60, "hours": 3600, "days": 86400}[str(m, "interval_period")]
}

// resolveInterval turns a duration like 5m into the server's interval id.
//
// The server stores an interval as a reference to one of a fixed set, so a
// duration that is not in the set is refused with the ones that are, rather
// than rounded to a neighbour: polling every hour instead of every 30 minutes
// is a different check.
func (a *App) resolveInterval(spec string) (any, error) {
	spec = strings.ToLower(strings.TrimSpace(spec))
	if isAllDigits(spec) {
		return atoiOr(spec), nil
	}
	rows, err := a.fetchRows(intervalsPath, nil)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(i, j int) bool { return intervalSeconds(rows[i]) < intervalSeconds(rows[j]) })
	var offered []string
	for _, r := range rows {
		label := intervalLabel(r)
		if label == spec {
			return atoiOr(str(r, "id")), nil
		}
		offered = append(offered, label)
	}
	return nil, errs.Usage("%q is not a polling interval the server offers", spec).
		WithHint("one of: %s", strings.Join(offered, ", "))
}

// setCredential writes a credential and its tag onto an item body.
//
// The portal requires both and they must agree: the tag is what a template
// uses to find the right credential in each infrastructure it is applied to,
// the credential is the one used against the test device. Given one, the
// other is derived.
func (a *App) setCredential(body map[string]any, credential, tag string) error {
	if credential == "" && tag == "" {
		return nil
	}
	creds, err := a.fetchRows(credentialsPath, a.infraQuery(nil))
	if err != nil {
		return err
	}

	if tag != "" {
		name, err := a.resolveCredentialTag(tag)
		if err != nil {
			return err
		}
		body["credential_tag"] = name
		tag = name
	}

	if credential != "" {
		for _, c := range creds {
			if str(c, "id") != credential {
				continue
			}
			tagName := str(objField(c, "tag"), "name")
			if tag == "" && tagName != "" {
				body["credential_tag"] = tagName
			} else if tag != "" && tagName != "" && !strings.EqualFold(tag, tagName) {
				return errs.Usage("credential %s is tagged %q, not %q", credential, tagName, tag).
					WithHint("drop one of --credential or --credential-tag, or pick a credential with that tag: ups credential list")
			}
			setDefault(body, "credential_type", str(c, "credential_type"))
			body["credential"] = atoiOr(credential)
			return nil
		}
		return errs.NotFound("no credential %s in this infrastructure", credential).
			WithHint("list them: ups credential list")
	}

	// Each credential has a system-scope twin holding the copy encrypted for
	// the agent. Users pick the organization one; the server finds the twin.
	var matches []row
	for _, c := range creds {
		if str(c, "scope") == "system" {
			continue
		}
		if strings.EqualFold(str(objField(c, "tag"), "name"), tag) {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 1:
		body["credential"] = atoiOr(str(matches[0], "id"))
		setDefault(body, "credential_type", str(matches[0], "credential_type"))
		return nil
	case 0:
		return errs.NotFound("no credential tagged %q in this infrastructure", tag).
			WithHint("the item needs one to poll its test device: ups credential list")
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, str(m, "id"))
		}
		return errs.Conflict("%d credentials are tagged %q here: %s", len(matches), tag, strings.Join(ids, ", ")).
			WithHint("name one with --credential <id>")
	}
}

// resolveCredentialTag returns a tag's canonical name, given its name or id.
func (a *App) resolveCredentialTag(spec string) (string, error) {
	rows, err := a.fetchRows(credentialTagsPath, nil)
	if err != nil {
		return "", err
	}
	var names []string
	for _, r := range rows {
		if str(r, "id") == spec || strings.EqualFold(str(r, "name"), spec) {
			return str(r, "name"), nil
		}
		names = append(names, str(r, "name"))
	}
	return "", errs.NotFound("no credential tag %q", spec).
		WithHint("one of: %s", strings.Join(names, ", "))
}

func setDefault(m map[string]any, key, value string) {
	if _, ok := m[key]; !ok && value != "" {
		m[key] = value
	}
}
