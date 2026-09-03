// Package iac represents an infrastructure as YAML and reconciles it against
// the platform.
//
// Identity is resolved by server-assigned id when the document carries one,
// and by name within the infrastructure otherwise.
//
// Export records ids so that renaming works: with only a name to go on, a
// rename is indistinguishable from "delete this and create that", which
// destroys monitoring history and is almost never what was meant. A
// hand-authored document may omit ids entirely and is matched by name, which
// keeps it portable between environments.
package iac

import (
	"fmt"
	"sort"
	"strings"
)

// APIVersion identifies the document schema.
const APIVersion = "upstacked/v1"

// Document is one infrastructure's desired state.
type Document struct {
	APIVersion     string          `yaml:"apiVersion"`
	Infrastructure InfrastructureR `yaml:"infrastructure"`
	// Templates are the monitoring templates the hosts below refer to.
	// They belong to an organization rather than to this infrastructure, so
	// the document creates and updates them but never deletes one: a template
	// dropped from this file is almost certainly still in use somewhere the
	// file cannot see.
	Templates []Template `yaml:"templates,omitempty"`
	Hosts     []Host     `yaml:"hosts"`
}

// Template is a reusable set of checks a kind of device gets.
//
// Checks are the host-less monitoring items the template stamps onto a device.
// They reach a host through Modules: an item belongs to the template because
// its module is in the set.
type Template struct {
	ID           string           `yaml:"id,omitempty"`
	Name         string           `yaml:"name"`
	Status       string           `yaml:"status,omitempty"`
	Organization string           `yaml:"organization,omitempty"`
	Modules      []string         `yaml:"modules,omitempty"`
	Checks       []MonitoringItem `yaml:"checks,omitempty"`
}

// InfrastructureR identifies the target. The id is recorded for convenience
// but the name is what a human reads; apply always uses the id from context or
// from this field.
type InfrastructureR struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name,omitempty"`
}

// Host is a monitored device.
type Host struct {
	// ID is the server-assigned identity. Present in exported documents so a
	// rename is a rename; omit it in hand-authored documents to match by name.
	ID       string `yaml:"id,omitempty"`
	Name     string `yaml:"name"`
	Hostname string `yaml:"hostname,omitempty"`
	IP       string `yaml:"ip,omitempty"`
	MAC      string `yaml:"mac,omitempty"`
	Type     string `yaml:"type,omitempty"`
	Serial   string `yaml:"serial,omitempty"`
	// Template names the monitoring template applied to this host. Setting it
	// to a different value REPLACES the host's monitoring: the platform
	// deletes every existing item before copying the template's in. An empty
	// value means "not managed here", never "unassign".
	Template   string           `yaml:"template,omitempty"`
	Monitoring []MonitoringItem `yaml:"monitoring,omitempty"`
}

// MonitoringItem is one check bound to a host.
type MonitoringItem struct {
	ID     string `yaml:"id,omitempty"`
	Name   string `yaml:"name"`
	Module string `yaml:"module,omitempty"`
	// Interval is a reference to a monitoring interval object, not seconds.
	Interval   string `yaml:"interval,omitempty"`
	Parameters string `yaml:"parameters,omitempty"`
	CredType   string `yaml:"credential_type,omitempty"`
	Credential string `yaml:"credential,omitempty"`
}

// Normalize sorts the document so that export output is byte-stable: the same
// remote state must always produce the same file, or every export would show
// as a diff.
func (d *Document) Normalize() {
	if d.APIVersion == "" {
		d.APIVersion = APIVersion
	}
	sort.Slice(d.Hosts, func(i, j int) bool { return d.Hosts[i].Name < d.Hosts[j].Name })
	for i := range d.Hosts {
		items := d.Hosts[i].Monitoring
		sort.Slice(items, func(a, b int) bool { return items[a].Name < items[b].Name })
	}
	sort.Slice(d.Templates, func(i, j int) bool { return d.Templates[i].Name < d.Templates[j].Name })
	for i := range d.Templates {
		checks := d.Templates[i].Checks
		sort.Slice(checks, func(a, b int) bool { return checks[a].Name < checks[b].Name })
		sort.Strings(d.Templates[i].Modules)
	}
}

// Validate rejects documents that cannot be applied safely.
func (d *Document) Validate() error {
	if d.APIVersion != "" && d.APIVersion != APIVersion {
		return fmt.Errorf("unsupported apiVersion %q (expected %s)", d.APIVersion, APIVersion)
	}
	tplSeen := map[string]bool{}
	for _, t := range d.Templates {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			return fmt.Errorf("every template needs a name: identity is by name, not by id")
		}
		if tplSeen[name] {
			return fmt.Errorf("duplicate template name %q", name)
		}
		tplSeen[name] = true

		checkSeen := map[string]bool{}
		for _, c := range t.Checks {
			if strings.TrimSpace(c.Name) == "" {
				return fmt.Errorf("template %q has a check with no name", name)
			}
			if checkSeen[c.Name] {
				return fmt.Errorf("template %q has duplicate check %q", name, c.Name)
			}
			checkSeen[c.Name] = true
		}
	}

	seen := map[string]bool{}
	for _, h := range d.Hosts {
		name := strings.TrimSpace(h.Name)
		if name == "" {
			return fmt.Errorf("every host needs a name: identity is by name, not by id")
		}
		if seen[name] {
			return fmt.Errorf("duplicate host name %q: names must be unique within an infrastructure", name)
		}
		seen[name] = true

		itemSeen := map[string]bool{}
		for _, m := range h.Monitoring {
			if strings.TrimSpace(m.Name) == "" {
				return fmt.Errorf("host %q has a monitoring item with no name", name)
			}
			if itemSeen[m.Name] {
				return fmt.Errorf("host %q has duplicate monitoring item %q", name, m.Name)
			}
			itemSeen[m.Name] = true
		}
	}
	return nil
}

// Action is the kind of change a plan step performs.
type Action string

const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
)

// Step is one reconciliation operation.
type Step struct {
	Action Action
	Kind   string
	Name   string
	// Host scopes a step to its parent: the device for a monitoring item, the
	// template for a check.
	Host string
	// ID is the server-side id, set for update and delete.
	ID string
	// Fields lists what differs, for update steps.
	Fields []string
	// Rename records the previous name when an update changes it, so the plan
	// can say so plainly instead of showing an opaque "name" field change.
	Rename string
	// Body is the payload to send.
	Body map[string]any
	// Replaces names the monitoring items this step destroys without being a
	// delete step. Assigning a template to a host is an update to the host,
	// but the platform wipes the host's existing items to carry it out, and
	// that loss is otherwise invisible in the plan.
	Replaces []string
}

func (s Step) String() string {
	label := s.Kind + " " + s.Name
	if s.Host != "" {
		label = s.Kind + " " + s.Host + "/" + s.Name
	}
	if s.Rename != "" {
		other := strings.Join(without(s.Fields, "name"), ", ")
		if other != "" {
			other = ", " + other
		}
		return fmt.Sprintf("%s %s %s rename from %q%s", s.Action, s.Kind, s.Name, s.Rename, other)
	}
	if s.Action == ActionUpdate && len(s.Fields) > 0 {
		return fmt.Sprintf("%s %s (%s)", s.Action, label, strings.Join(s.Fields, ", "))
	}
	return fmt.Sprintf("%s %s", s.Action, label)
}

func without(list []string, drop string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// Renames returns the steps that change a resource's name.
func (p *Plan) Renames() []Step {
	var out []Step
	for _, s := range p.Steps {
		if s.Rename != "" {
			out = append(out, s)
		}
	}
	return out
}

// Plan is an ordered set of steps.
//
// Order matters: creates and updates run before deletes so that a rename-like
// edit does not remove coverage before its replacement exists.
type Plan struct {
	Steps []Step
}

func (p *Plan) Add(s Step) { p.Steps = append(p.Steps, s) }

// Destructive reports whether the plan removes anything.
func (p *Plan) Destructive() bool {
	for _, s := range p.Steps {
		if s.Action == ActionDelete || len(s.Replaces) > 0 {
			return true
		}
	}
	return false
}

// Replacements returns the steps that destroy monitoring without being
// deletions, so the confirmation prompt can count them alongside the deletes.
func (p *Plan) Replacements() []Step {
	var out []Step
	for _, s := range p.Steps {
		if s.Action != ActionDelete && len(s.Replaces) > 0 {
			out = append(out, s)
		}
	}
	return out
}

// TouchesTemplates reports whether the plan changes a template or its checks.
// Templates are shared beyond this infrastructure, so that is worth saying.
func (p *Plan) TouchesTemplates() bool {
	for _, s := range p.Steps {
		if s.Kind == "template" || s.Kind == "check" {
			return true
		}
	}
	return false
}

// Deletions returns just the removal steps, for the confirmation prompt.
func (p *Plan) Deletions() []Step {
	var out []Step
	for _, s := range p.Steps {
		if s.Action == ActionDelete {
			out = append(out, s)
		}
	}
	return out
}

// Counts summarises a plan.
func (p *Plan) Counts() (create, update, del int) {
	for _, s := range p.Steps {
		switch s.Action {
		case ActionCreate:
			create++
		case ActionUpdate:
			update++
		case ActionDelete:
			del++
		}
	}
	return
}

func (p *Plan) Empty() bool { return len(p.Steps) == 0 }

// Sort orders steps so creates and updates precede deletes, and so output is
// deterministic within each group.
func (p *Plan) Sort() {
	rank := map[Action]int{ActionCreate: 0, ActionUpdate: 1, ActionDelete: 2}
	// Templates and their checks must exist before a host can be pointed at
	// one, so kind order is explicit rather than alphabetical.
	kindRank := map[string]int{"template": 0, "check": 1, "host": 2, "monitoring": 3}
	sort.SliceStable(p.Steps, func(i, j int) bool {
		a, b := p.Steps[i], p.Steps[j]
		if rank[a.Action] != rank[b.Action] {
			return rank[a.Action] < rank[b.Action]
		}
		if kindRank[a.Kind] != kindRank[b.Kind] {
			return kindRank[a.Kind] < kindRank[b.Kind]
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		return a.Name < b.Name
	})
}

// RenamePair is a create/delete pair that looks like one resource renamed.
type RenamePair struct {
	Kind string
	From string
	To   string
}

// LikelyRenames spots a delete and a create that describe the same thing.
//
// This happens when a document is hand-authored, or exported and then stripped
// of its ids: with only names to match on, a rename is indistinguishable from
// a replacement. Detecting it lets the CLI say so instead of quietly
// destroying a resource and its history.
func (p *Plan) LikelyRenames() []RenamePair {
	var out []RenamePair
	for _, del := range p.Steps {
		if del.Action != ActionDelete {
			continue
		}
		for _, add := range p.Steps {
			if add.Action != ActionCreate || add.Kind != del.Kind || add.Name == del.Name {
				continue
			}
			if sameResource(del, add) {
				out = append(out, RenamePair{Kind: del.Kind, From: del.Name, To: add.Name})
			}
		}
	}
	return out
}

// sameResource reports whether two steps describe the same underlying thing,
// judged on the identifying fields the document carries.
func sameResource(a, b Step) bool {
	keys := []string{"i_ip_address", "i_hostname", "i_serial", "i_mac_address"}
	if a.Kind == "monitoring" {
		if a.Host != b.Host {
			return false
		}
		keys = []string{"monitoring_module", "parameters"}
	}
	shared := 0
	for _, k := range keys {
		av, aok := a.Body[k]
		bv, bok := b.Body[k]
		if !aok || !bok {
			continue
		}
		if av != bv {
			return false
		}
		shared++
	}
	return shared > 0
}
