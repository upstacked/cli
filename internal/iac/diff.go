package iac

import (
	"sort"
	"strings"
)

// TemplateRefKey carries a template NAME through the plan.
//
// The API needs the template's id, and only the command layer can resolve one,
// so apply translates this key into monitoring_template and drops it before
// sending. The key is set only on steps that actually change the assignment:
// re-sending it on an unrelated host edit would be a request to re-apply the
// template, and re-applying wipes the host's items.
const TemplateRefKey = "__template_name"

// MaxPlanSteps bounds a single reconciliation. A plan larger than this almost
// certainly means the local document is pointed at the wrong infrastructure,
// which is exactly the mistake that would otherwise delete a customer's
// monitoring wholesale.
const MaxPlanSteps = 2000

// Diff computes the steps that would make remote match local.
//
// Matching is by server-assigned id first, then by name. The id path is what
// makes a rename a rename: without it, changing a name reads as "delete this,
// create that", which destroys the resource and its history.
func Diff(local, remote *Document) *Plan {
	plan := &Plan{}

	plan.Steps = append(plan.Steps, diffTemplates(local, remote)...)

	byID, byName := indexHosts(remote)
	// Claimed remote hosts are tracked by pointer, not by id: a hand-authored
	// document has no ids, and keying on an empty string would mark every
	// unmatched host as claimed and silently drop all deletions.
	matched := map[*Host]bool{}

	for i := range local.Hosts {
		lh := &local.Hosts[i]
		rh := matchHost(lh, byID, byName)
		if rh == nil {
			body := hostBody(lh)
			if lh.Template != "" {
				// The template supplies this host's monitoring, so the inline
				// items are not created a second time.
				body[TemplateRefKey] = lh.Template
			}
			plan.Add(Step{
				Action: ActionCreate, Kind: "host", Name: lh.Name, Body: body,
			})
			if lh.Template == "" {
				for j := range lh.Monitoring {
					plan.Add(Step{
						Action: ActionCreate, Kind: "monitoring", Name: lh.Monitoring[j].Name,
						Host: lh.Name, Body: itemBody(&lh.Monitoring[j]),
					})
				}
			}
			continue
		}
		matched[rh] = true

		fields := hostFieldDiff(lh, rh)
		rename := ""
		if lh.Name != rh.Name {
			// Only reachable when the two were matched by id.
			rename = rh.Name
			fields = append([]string{"name"}, fields...)
		}
		// A changed template assignment is an update to the host, but the
		// platform carries it out by deleting every item on the host first.
		// Name them, and leave the host's inline items alone: the template is
		// the source of truth for them from here on, and diffing both would
		// have the two fight over the same host.
		retemplated := lh.Template != "" && lh.Template != rh.Template
		if retemplated {
			fields = append(fields, "template")
		}
		if len(fields) > 0 {
			step := Step{
				Action: ActionUpdate, Kind: "host", Name: lh.Name, ID: rh.ID,
				Fields: fields, Rename: rename, Body: hostBody(lh),
			}
			if retemplated {
				step.Body[TemplateRefKey] = lh.Template
				for _, m := range rh.Monitoring {
					step.Replaces = append(step.Replaces, m.Name)
				}
			}
			plan.Add(step)
		}
		if !retemplated {
			plan.Steps = append(plan.Steps, diffMonitoring(lh, rh)...)
		}
	}

	// Deletions: remote hosts no local entry claimed.
	for i := range remote.Hosts {
		rh := &remote.Hosts[i]
		if matched[rh] {
			continue
		}
		for j := range rh.Monitoring {
			plan.Add(Step{
				Action: ActionDelete, Kind: "monitoring", Name: rh.Monitoring[j].Name,
				Host: rh.Name, ID: rh.Monitoring[j].ID, Body: itemBody(&rh.Monitoring[j]),
			})
		}
		// The body is carried on deletes purely so a delete/create pair can be
		// recognised as a probable rename before it is executed.
		plan.Add(Step{Action: ActionDelete, Kind: "host", Name: rh.Name, ID: rh.ID, Body: hostBody(rh)})
	}

	plan.Sort()
	return plan
}

func indexHosts(d *Document) (byID, byName map[string]*Host) {
	byID, byName = map[string]*Host{}, map[string]*Host{}
	for i := range d.Hosts {
		h := &d.Hosts[i]
		if h.ID != "" {
			byID[h.ID] = h
		}
		byName[h.Name] = h
	}
	return
}

// matchHost resolves a local host to its remote counterpart. A stale id (one
// that no longer exists remotely) falls back to name matching rather than
// silently creating a duplicate.
func matchHost(lh *Host, byID, byName map[string]*Host) *Host {
	if lh.ID != "" {
		if rh, ok := byID[lh.ID]; ok {
			return rh
		}
	}
	if rh, ok := byName[lh.Name]; ok {
		return rh
	}
	return nil
}

// diffTemplates reconciles the templates the document declares.
//
// Deletion is deliberately not produced. A template belongs to an organization
// and can be used by infrastructures this document knows nothing about, so a
// template missing from the file means "not managed here", never "remove it".
// Removing one is an explicit act: ups monitoring template delete.
func diffTemplates(local, remote *Document) []Step {
	var steps []Step
	byID, byName := map[string]*Template{}, map[string]*Template{}
	for i := range remote.Templates {
		t := &remote.Templates[i]
		if t.ID != "" {
			byID[t.ID] = t
		}
		byName[t.Name] = t
	}

	for i := range local.Templates {
		lt := &local.Templates[i]
		var rt *Template
		if lt.ID != "" {
			rt = byID[lt.ID]
		}
		if rt == nil {
			rt = byName[lt.Name]
		}
		if rt == nil {
			steps = append(steps, Step{
				Action: ActionCreate, Kind: "template", Name: lt.Name, Body: templateBody(lt),
			})
			for j := range lt.Checks {
				steps = append(steps, Step{
					Action: ActionCreate, Kind: "check", Name: lt.Checks[j].Name,
					Host: lt.Name, Body: checkBody(&lt.Checks[j], lt),
				})
			}
			continue
		}

		fields := templateFieldDiff(lt, rt)
		rename := ""
		if lt.Name != rt.Name {
			rename = rt.Name
			fields = append([]string{"name"}, fields...)
		}
		if len(fields) > 0 {
			steps = append(steps, Step{
				Action: ActionUpdate, Kind: "template", Name: lt.Name, ID: rt.ID,
				Fields: fields, Rename: rename, Body: templateBody(lt),
			})
		}
		steps = append(steps, diffChecks(lt, rt)...)
	}
	return steps
}

// diffChecks compares the host-less items inside one declared template.
func diffChecks(local, remote *Template) []Step {
	var steps []Step
	byID, byName := map[string]*MonitoringItem{}, map[string]*MonitoringItem{}
	for i := range remote.Checks {
		c := &remote.Checks[i]
		if c.ID != "" {
			byID[c.ID] = c
		}
		byName[c.Name] = c
	}
	matched := map[*MonitoringItem]bool{}

	for i := range local.Checks {
		lc := &local.Checks[i]
		var rc *MonitoringItem
		if lc.ID != "" {
			rc = byID[lc.ID]
		}
		if rc == nil {
			rc = byName[lc.Name]
		}
		if rc == nil {
			steps = append(steps, Step{
				Action: ActionCreate, Kind: "check", Name: lc.Name,
				Host: local.Name, Body: checkBody(lc, local),
			})
			continue
		}
		matched[rc] = true

		fields := itemFieldDiff(lc, rc)
		rename := ""
		if lc.Name != rc.Name {
			rename = rc.Name
			fields = append([]string{"name"}, fields...)
		}
		if len(fields) > 0 {
			steps = append(steps, Step{
				Action: ActionUpdate, Kind: "check", Name: lc.Name,
				Host: local.Name, ID: rc.ID, Fields: fields, Rename: rename, Body: itemBody(lc),
			})
		}
	}

	for i := range remote.Checks {
		rc := &remote.Checks[i]
		if !matched[rc] {
			steps = append(steps, Step{
				Action: ActionDelete, Kind: "check", Name: rc.Name,
				Host: remote.Name, ID: rc.ID, Body: itemBody(rc),
			})
		}
	}
	return steps
}

// checkBody is a template check's payload: a monitoring item with no host,
// which the API only accepts when an organization says who owns it.
func checkBody(m *MonitoringItem, t *Template) map[string]any {
	b := itemBody(m)
	putRef(b, "organization", t.Organization)
	return b
}

func templateFieldDiff(local, remote *Template) []string {
	var changed []string
	if local.Status != "" && local.Status != remote.Status {
		changed = append(changed, "status")
	}
	if len(local.Modules) > 0 && !sameStrings(local.Modules, remote.Modules) {
		changed = append(changed, "modules")
	}
	return changed
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func templateBody(t *Template) map[string]any {
	b := map[string]any{"name": t.Name}
	put(b, "published_status", t.Status)
	putRef(b, "organization", t.Organization)
	if len(t.Modules) > 0 {
		mods := make([]any, 0, len(t.Modules))
		for _, m := range t.Modules {
			if n, ok := asInt(m); ok {
				mods = append(mods, n)
				continue
			}
			mods = append(mods, m)
		}
		b["monitoring_modules"] = mods
	}
	return b
}

func diffMonitoring(local, remote *Host) []Step {
	var steps []Step
	byID, byName := map[string]*MonitoringItem{}, map[string]*MonitoringItem{}
	for i := range remote.Monitoring {
		m := &remote.Monitoring[i]
		if m.ID != "" {
			byID[m.ID] = m
		}
		byName[m.Name] = m
	}
	matched := map[*MonitoringItem]bool{}

	for i := range local.Monitoring {
		li := &local.Monitoring[i]
		var ri *MonitoringItem
		if li.ID != "" {
			ri = byID[li.ID]
		}
		if ri == nil {
			ri = byName[li.Name]
		}
		if ri == nil {
			steps = append(steps, Step{
				Action: ActionCreate, Kind: "monitoring", Name: li.Name,
				Host: local.Name, Body: itemBody(li),
			})
			continue
		}
		matched[ri] = true

		fields := itemFieldDiff(li, ri)
		rename := ""
		if li.Name != ri.Name {
			rename = ri.Name
			fields = append([]string{"name"}, fields...)
		}
		if len(fields) > 0 {
			steps = append(steps, Step{
				Action: ActionUpdate, Kind: "monitoring", Name: li.Name,
				Host: local.Name, ID: ri.ID, Fields: fields, Rename: rename, Body: itemBody(li),
			})
		}
	}

	for i := range remote.Monitoring {
		ri := &remote.Monitoring[i]
		if !matched[ri] {
			steps = append(steps, Step{
				Action: ActionDelete, Kind: "monitoring", Name: ri.Name,
				Host: remote.Name, ID: ri.ID, Body: itemBody(ri),
			})
		}
	}
	return steps
}

// hostFieldDiff compares only fields the document actually declares. An empty
// local value means "not managed here", not "set it to empty" - otherwise
// every unmanaged field would be clobbered on the first apply.
func hostFieldDiff(local, remote *Host) []string {
	var changed []string
	cmp := func(name, l, r string) {
		if l != "" && strings.TrimSpace(l) != strings.TrimSpace(r) {
			changed = append(changed, name)
		}
	}
	cmp("hostname", local.Hostname, remote.Hostname)
	cmp("ip", local.IP, remote.IP)
	cmp("mac", local.MAC, remote.MAC)
	cmp("type", local.Type, remote.Type)
	cmp("serial", local.Serial, remote.Serial)
	return changed
}

func itemFieldDiff(local, remote *MonitoringItem) []string {
	var changed []string
	if local.Module != "" && local.Module != remote.Module {
		changed = append(changed, "module")
	}
	if local.Interval != "" && local.Interval != remote.Interval {
		changed = append(changed, "interval")
	}
	if local.Parameters != "" && strings.TrimSpace(local.Parameters) != strings.TrimSpace(remote.Parameters) {
		changed = append(changed, "parameters")
	}
	if local.CredType != "" && local.CredType != remote.CredType {
		changed = append(changed, "credential_type")
	}
	if local.Credential != "" && local.Credential != remote.Credential {
		changed = append(changed, "credential")
	}
	return changed
}

// hostBody builds the API payload. Numeric references are sent as numbers:
// the API rejects a string where it expects a primary key.
func hostBody(h *Host) map[string]any {
	b := map[string]any{"name": h.Name}
	put(b, "i_hostname", h.Hostname)
	put(b, "i_ip_address", h.IP)
	put(b, "i_mac_address", h.MAC)
	put(b, "i_type", h.Type)
	put(b, "i_serial", h.Serial)
	return b
}

func itemBody(m *MonitoringItem) map[string]any {
	b := map[string]any{"name": m.Name}
	putRef(b, "monitoring_module", m.Module)
	// interval is a foreign key to a monitoring interval, not a number of
	// seconds. Confirmed against a live server, which rejects a raw duration
	// with `Invalid pk "60" - object does not exist`.
	putRef(b, "interval", m.Interval)
	putRef(b, "credential", m.Credential)
	put(b, "parameters", m.Parameters)
	put(b, "credential_type", m.CredType)
	return b
}

func put(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// putRef writes a primary-key reference, as a number when it looks like one.
func putRef(m map[string]any, k, v string) {
	if v == "" {
		return
	}
	if n, ok := asInt(v); ok {
		m[k] = n
		return
	}
	m[k] = v
}

func asInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}
