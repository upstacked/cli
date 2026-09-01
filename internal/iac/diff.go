package iac

import "strings"

// MaxPlanSteps bounds a single reconciliation. A plan larger than this almost
// certainly means the local document is pointed at the wrong infrastructure,
// which is exactly the mistake that would otherwise delete a customer's
// monitoring wholesale.
const MaxPlanSteps = 2000

// Diff computes the steps that would make remote match local.
//
// Both sides are keyed by name within the infrastructure. Anything present
// remotely but absent locally is a deletion, which is why an export that
// silently missed a resource would be dangerous - see Export's cap handling.
func Diff(local, remote *Document) *Plan {
	plan := &Plan{}

	remoteHosts := map[string]*Host{}
	for i := range remote.Hosts {
		remoteHosts[remote.Hosts[i].Name] = &remote.Hosts[i]
	}
	localHosts := map[string]*Host{}
	for i := range local.Hosts {
		localHosts[local.Hosts[i].Name] = &local.Hosts[i]
	}

	// Creates and updates.
	for i := range local.Hosts {
		lh := &local.Hosts[i]
		rh, exists := remoteHosts[lh.Name]
		if !exists {
			plan.Add(Step{
				Action: ActionCreate, Kind: "host", Name: lh.Name,
				Body: hostBody(lh),
			})
			for j := range lh.Monitoring {
				plan.Add(Step{
					Action: ActionCreate, Kind: "monitoring", Name: lh.Monitoring[j].Name,
					Host: lh.Name, Body: itemBody(&lh.Monitoring[j]),
				})
			}
			continue
		}
		if fields := hostFieldDiff(lh, rh); len(fields) > 0 {
			plan.Add(Step{
				Action: ActionUpdate, Kind: "host", Name: lh.Name, ID: rh.id,
				Fields: fields, Body: hostBody(lh),
			})
		}
		plan.Steps = append(plan.Steps, diffMonitoring(lh, rh)...)
	}

	// Deletions: remote resources with no local counterpart.
	for i := range remote.Hosts {
		rh := &remote.Hosts[i]
		lh, exists := localHosts[rh.Name]
		if !exists {
			// Delete the host's items first so the intent is legible in the plan.
			for j := range rh.Monitoring {
				plan.Add(Step{
					Action: ActionDelete, Kind: "monitoring", Name: rh.Monitoring[j].Name,
					Host: rh.Name, ID: rh.Monitoring[j].id,
				})
			}
			plan.Add(Step{Action: ActionDelete, Kind: "host", Name: rh.Name, ID: rh.id})
			continue
		}
		localItems := map[string]bool{}
		for _, m := range lh.Monitoring {
			localItems[m.Name] = true
		}
		for j := range rh.Monitoring {
			if !localItems[rh.Monitoring[j].Name] {
				plan.Add(Step{
					Action: ActionDelete, Kind: "monitoring", Name: rh.Monitoring[j].Name,
					Host: rh.Name, ID: rh.Monitoring[j].id,
				})
			}
		}
	}

	plan.Sort()
	return plan
}

func diffMonitoring(local, remote *Host) []Step {
	var steps []Step
	remoteItems := map[string]*MonitoringItem{}
	for i := range remote.Monitoring {
		remoteItems[remote.Monitoring[i].Name] = &remote.Monitoring[i]
	}
	for i := range local.Monitoring {
		li := &local.Monitoring[i]
		ri, exists := remoteItems[li.Name]
		if !exists {
			steps = append(steps, Step{
				Action: ActionCreate, Kind: "monitoring", Name: li.Name,
				Host: local.Name, Body: itemBody(li),
			})
			continue
		}
		if fields := itemFieldDiff(li, ri); len(fields) > 0 {
			steps = append(steps, Step{
				Action: ActionUpdate, Kind: "monitoring", Name: li.Name,
				Host: local.Name, ID: ri.id, Fields: fields, Body: itemBody(li),
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
	if local.Interval != 0 && local.Interval != remote.Interval {
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
	put(b, "monitoring_module", m.Module)
	put(b, "parameters", m.Parameters)
	put(b, "credential_type", m.CredType)
	put(b, "credential", m.Credential)
	if m.Interval != 0 {
		b["interval"] = m.Interval
	}
	return b
}

func put(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}
