// Package iac represents an infrastructure as YAML and reconciles it against
// the platform.
//
// Identity is by name within an infrastructure, never by server-assigned id.
// Ids are allocated by the server, so a document keyed on them could not be
// authored by hand or meaningfully diffed across environments. The cost of
// that choice is that renaming a resource reads as delete-plus-create, which
// the CLI warns about rather than hiding.
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
	Hosts          []Host          `yaml:"hosts"`
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
	Name       string           `yaml:"name"`
	Hostname   string           `yaml:"hostname,omitempty"`
	IP         string           `yaml:"ip,omitempty"`
	MAC        string           `yaml:"mac,omitempty"`
	Type       string           `yaml:"type,omitempty"`
	Serial     string           `yaml:"serial,omitempty"`
	Monitoring []MonitoringItem `yaml:"monitoring,omitempty"`

	// id is the server-side identity, populated on export and by diff. It is
	// deliberately not serialised: the document must stay portable.
	id string `yaml:"-"`
}

// MonitoringItem is one check bound to a host.
type MonitoringItem struct {
	Name       string `yaml:"name"`
	Module     string `yaml:"module,omitempty"`
	Interval   int    `yaml:"interval,omitempty"`
	Parameters string `yaml:"parameters,omitempty"`
	CredType   string `yaml:"credential_type,omitempty"`
	Credential string `yaml:"credential,omitempty"`

	id string `yaml:"-"`
}

func (h *Host) SetID(id string)           { h.id = id }
func (h *Host) ID() string                { return h.id }
func (m *MonitoringItem) SetID(id string) { m.id = id }
func (m *MonitoringItem) ID() string      { return m.id }

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
}

// Validate rejects documents that cannot be applied safely.
func (d *Document) Validate() error {
	if d.APIVersion != "" && d.APIVersion != APIVersion {
		return fmt.Errorf("unsupported apiVersion %q (expected %s)", d.APIVersion, APIVersion)
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
	// Host scopes monitoring items to their device.
	Host string
	// ID is the server-side id, set for update and delete.
	ID string
	// Fields lists what differs, for update steps.
	Fields []string
	// Body is the payload to send.
	Body map[string]any
}

func (s Step) String() string {
	label := s.Kind + " " + s.Name
	if s.Host != "" {
		label = s.Kind + " " + s.Host + "/" + s.Name
	}
	if s.Action == ActionUpdate && len(s.Fields) > 0 {
		return fmt.Sprintf("%s %s (%s)", s.Action, label, strings.Join(s.Fields, ", "))
	}
	return fmt.Sprintf("%s %s", s.Action, label)
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
		if s.Action == ActionDelete {
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
	sort.SliceStable(p.Steps, func(i, j int) bool {
		a, b := p.Steps[i], p.Steps[j]
		if rank[a.Action] != rank[b.Action] {
			return rank[a.Action] < rank[b.Action]
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
