package iac

import (
	"testing"
)

func tplDoc(templates []Template, hosts ...Host) *Document {
	d := &Document{APIVersion: APIVersion, Templates: templates, Hosts: hosts}
	d.Normalize()
	return d
}

func stepsOfKind(p *Plan, kind string) []Step {
	var out []Step
	for _, s := range p.Steps {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

func TestTemplateAndItsChecksAreCreatedTogether(t *testing.T) {
	local := tplDoc([]Template{{
		Name: "PLC", Modules: []string{"2"},
		Checks: []MonitoringItem{{Name: "ping", Module: "2"}},
	}})
	remote := tplDoc(nil)

	p := Diff(local, remote)
	if got := stepsOfKind(p, "template"); len(got) != 1 || got[0].Action != ActionCreate {
		t.Fatalf("expected one template create, got %v", p.Steps)
	}
	checks := stepsOfKind(p, "check")
	if len(checks) != 1 || checks[0].Action != ActionCreate || checks[0].Host != "PLC" {
		t.Fatalf("expected the check to be created under its template, got %v", checks)
	}
}

// A template must exist before a host can be pointed at one.
func TestTemplateStepsRunBeforeHostSteps(t *testing.T) {
	local := tplDoc(
		[]Template{{Name: "PLC", Modules: []string{"2"}, Checks: []MonitoringItem{{Name: "ping"}}}},
		Host{Name: "plc01", Template: "PLC"},
	)
	remote := tplDoc(nil)

	p := Diff(local, remote)
	order := map[string]int{}
	for i, s := range p.Steps {
		if _, seen := order[s.Kind]; !seen {
			order[s.Kind] = i
		}
	}
	if order["template"] > order["check"] || order["check"] > order["host"] {
		t.Errorf("expected template, then check, then host; got %v", p.Steps)
	}
}

// A new host with a template must not also create the items inline: the
// platform copies them from the template, so creating both would double up.
func TestNewHostWithATemplateDoesNotCreateItsItemsInline(t *testing.T) {
	local := tplDoc(nil, Host{
		Name: "plc01", Template: "PLC",
		Monitoring: []MonitoringItem{{Name: "ping"}},
	})
	remote := tplDoc(nil)

	p := Diff(local, remote)
	if got := stepsOfKind(p, "monitoring"); len(got) != 0 {
		t.Errorf("the template supplies the items; none should be created directly: %v", got)
	}
	hosts := stepsOfKind(p, "host")
	if len(hosts) != 1 || hosts[0].Body[TemplateRefKey] != "PLC" {
		t.Errorf("expected the host create to carry the template reference, got %v", hosts)
	}
}

// Assigning a template deletes every item on the host. That is an update step,
// so without this the plan would understate it as a one-field change.
func TestChangingAHostsTemplateReportsWhatItDestroys(t *testing.T) {
	local := tplDoc(nil, Host{Name: "plc01", Template: "PLC"})
	remote := tplDoc(nil, Host{
		ID: "2", Name: "plc01",
		Monitoring: []MonitoringItem{{ID: "27", Name: "ping"}, {ID: "28", Name: "hand-added"}},
	})

	p := Diff(local, remote)
	hosts := stepsOfKind(p, "host")
	if len(hosts) != 1 {
		t.Fatalf("expected one host update, got %v", p.Steps)
	}
	if len(hosts[0].Replaces) != 2 {
		t.Errorf("expected both existing items to be named as losses, got %v", hosts[0].Replaces)
	}
	if !p.Destructive() {
		t.Error("a plan that wipes a host's monitoring is destructive")
	}
	if len(p.Deletions()) != 0 {
		t.Error("the loss is a replacement, not a delete step")
	}
}

// The template becomes the source of truth for the host's items, so diffing
// the inline list as well would have the two fight over the same host.
func TestRetemplatedHostDoesNotAlsoDiffItsInlineItems(t *testing.T) {
	local := tplDoc(nil, Host{
		Name: "plc01", Template: "PLC",
		Monitoring: []MonitoringItem{{Name: "something-else"}},
	})
	remote := tplDoc(nil, Host{
		ID: "2", Name: "plc01",
		Monitoring: []MonitoringItem{{ID: "27", Name: "ping"}},
	})

	if got := stepsOfKind(Diff(local, remote), "monitoring"); len(got) != 0 {
		t.Errorf("expected no monitoring steps while the template changes, got %v", got)
	}
}

// An unchanged template assignment must not be re-sent: re-applying a template
// is how a host loses monitoring it was supposed to keep.
func TestUnchangedTemplateProducesNoStepAndNoReference(t *testing.T) {
	local := tplDoc(nil, Host{Name: "plc01", Template: "PLC", IP: "10.0.0.5"})
	remote := tplDoc(nil, Host{ID: "2", Name: "plc01", Template: "PLC", IP: "10.0.0.1"})

	p := Diff(local, remote)
	hosts := stepsOfKind(p, "host")
	if len(hosts) != 1 {
		t.Fatalf("expected the ip change, got %v", p.Steps)
	}
	if _, ok := hosts[0].Body[TemplateRefKey]; ok {
		t.Error("an unrelated edit must not re-assign the template")
	}
	if len(hosts[0].Replaces) != 0 {
		t.Error("nothing is destroyed when the template is unchanged")
	}
}

// A template is organization-wide. A document that stops mentioning one is
// saying "not managed here", and must never be read as "delete it".
func TestATemplateMissingFromTheDocumentIsNeverDeleted(t *testing.T) {
	local := tplDoc(nil, Host{Name: "plc01"})
	remote := tplDoc([]Template{{ID: "4", Name: "PLC", Checks: []MonitoringItem{{ID: "80", Name: "ping"}}}},
		Host{ID: "2", Name: "plc01"})

	p := Diff(local, remote)
	for _, s := range p.Steps {
		if s.Kind == "template" || s.Kind == "check" {
			t.Errorf("an unmentioned template must be left alone, got %v", s)
		}
	}
}

// Inside a template the document does declare, a removed check is a removal.
func TestCheckRemovedFromADeclaredTemplateIsDeleted(t *testing.T) {
	local := tplDoc([]Template{{ID: "4", Name: "PLC", Checks: []MonitoringItem{{ID: "80", Name: "ping"}}}})
	remote := tplDoc([]Template{{ID: "4", Name: "PLC", Checks: []MonitoringItem{
		{ID: "80", Name: "ping"}, {ID: "81", Name: "uptime"},
	}}})

	p := Diff(local, remote)
	dels := p.Deletions()
	if len(dels) != 1 || dels[0].Kind != "check" || dels[0].Name != "uptime" {
		t.Fatalf("expected the dropped check to be deleted, got %v", p.Steps)
	}
	if !p.TouchesTemplates() {
		t.Error("the plan changes a template and should say so")
	}
}

func TestTemplateModuleSetChangeIsAnUpdate(t *testing.T) {
	local := tplDoc([]Template{{ID: "4", Name: "PLC", Modules: []string{"2", "3"}}})
	remote := tplDoc([]Template{{ID: "4", Name: "PLC", Modules: []string{"2"}}})

	got := stepsOfKind(Diff(local, remote), "template")
	if len(got) != 1 || got[0].Action != ActionUpdate {
		t.Fatalf("expected a template update, got %v", got)
	}
	if len(got[0].Fields) != 1 || got[0].Fields[0] != "modules" {
		t.Errorf("expected the modules field to be named, got %v", got[0].Fields)
	}
}

// Module order is not meaningful, so a reordered list is not a change.
func TestReorderedModulesAreNotAChange(t *testing.T) {
	local := tplDoc([]Template{{ID: "4", Name: "PLC", Modules: []string{"3", "2"}}})
	remote := tplDoc([]Template{{ID: "4", Name: "PLC", Modules: []string{"2", "3"}}})

	if p := Diff(local, remote); !p.Empty() {
		t.Errorf("expected no steps, got %v", p.Steps)
	}
}

func TestDuplicateTemplateNamesAreRejected(t *testing.T) {
	d := &Document{Templates: []Template{{Name: "PLC"}, {Name: "PLC"}}}
	if err := d.Validate(); err == nil {
		t.Error("duplicate template names must not validate")
	}
}
