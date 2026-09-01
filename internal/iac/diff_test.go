package iac

import (
	"strings"
	"testing"
)

func doc(hosts ...Host) *Document {
	d := &Document{APIVersion: APIVersion, Hosts: hosts}
	d.Normalize()
	return d
}

func TestDiffOfIdenticalDocumentsIsEmpty(t *testing.T) {
	a := doc(Host{Name: "sw1", IP: "10.0.0.1", Monitoring: []MonitoringItem{{Name: "CPU", Interval: "30"}}})
	b := doc(Host{Name: "sw1", IP: "10.0.0.1", Monitoring: []MonitoringItem{{Name: "CPU", Interval: "30"}}})
	if p := Diff(a, b); !p.Empty() {
		t.Errorf("expected no steps, got %v", p.Steps)
	}
}

// An empty local field means "not managed here", not "set it to empty".
// Otherwise the first apply would wipe every field the document omits.
func TestUnsetLocalFieldsAreNotTreatedAsChanges(t *testing.T) {
	local := doc(Host{Name: "sw1"})
	remote := doc(Host{Name: "sw1", IP: "10.0.0.1", Hostname: "sw1.corp", Serial: "ABC"})
	if p := Diff(local, remote); !p.Empty() {
		t.Errorf("omitted fields must not produce updates, got %v", p.Steps)
	}
}

func TestDiffDetectsEachActionKind(t *testing.T) {
	local := doc(
		Host{Name: "keep", IP: "10.0.0.9"},
		Host{Name: "new"},
	)
	remote := doc(
		Host{Name: "keep", IP: "10.0.0.1"},
		Host{Name: "gone"},
	)
	p := Diff(local, remote)
	create, update, del := p.Counts()
	if create != 1 || update != 1 || del != 1 {
		t.Fatalf("expected 1/1/1 create/update/delete, got %d/%d/%d: %v", create, update, del, p.Steps)
	}
	if !p.Destructive() {
		t.Error("a plan containing a delete must report as destructive")
	}
}

// Creates and updates must precede deletes, so an edit never removes coverage
// before its replacement exists.
func TestPlanOrdersDeletesLast(t *testing.T) {
	local := doc(Host{Name: "new"})
	remote := doc(Host{Name: "old"})
	p := Diff(local, remote)

	seenDelete := false
	for _, s := range p.Steps {
		if s.Action == ActionDelete {
			seenDelete = true
			continue
		}
		if seenDelete {
			t.Fatalf("a %s step followed a delete: %v", s.Action, p.Steps)
		}
	}
}

func TestDiffIsDeterministic(t *testing.T) {
	local := doc(Host{Name: "b"}, Host{Name: "a"}, Host{Name: "c"})
	remote := doc()
	first := Diff(local, remote)
	second := Diff(local, remote)
	if len(first.Steps) != len(second.Steps) {
		t.Fatal("step count differs between runs")
	}
	for i := range first.Steps {
		if first.Steps[i].String() != second.Steps[i].String() {
			t.Fatalf("step %d differs: %q vs %q", i, first.Steps[i], second.Steps[i])
		}
	}
}

func TestMonitoringItemsDiffWithinTheirHost(t *testing.T) {
	local := doc(Host{Name: "sw1", Monitoring: []MonitoringItem{
		{Name: "CPU", Interval: "60"}, {Name: "Disk"},
	}})
	remote := doc(Host{Name: "sw1", Monitoring: []MonitoringItem{
		{Name: "CPU", Interval: "30"}, {Name: "Memory"},
	}})
	p := Diff(local, remote)
	create, update, del := p.Counts()
	if create != 1 || update != 1 || del != 1 {
		t.Fatalf("expected 1/1/1, got %d/%d/%d: %v", create, update, del, p.Steps)
	}
	for _, s := range p.Steps {
		if s.Kind == "monitoring" && s.Host != "sw1" {
			t.Errorf("monitoring step lost its host scope: %v", s)
		}
	}
}

func TestValidateRejectsUnsafeDocuments(t *testing.T) {
	cases := []struct {
		name string
		d    *Document
	}{
		{"no host name", &Document{APIVersion: APIVersion, Hosts: []Host{{Name: ""}}}},
		{"duplicate hosts", &Document{APIVersion: APIVersion, Hosts: []Host{{Name: "a"}, {Name: "a"}}}},
		{"duplicate items", &Document{APIVersion: APIVersion, Hosts: []Host{{Name: "a", Monitoring: []MonitoringItem{{Name: "x"}, {Name: "x"}}}}}},
		{"wrong apiVersion", &Document{APIVersion: "other/v9"}},
	}
	for _, tc := range cases {
		if err := tc.d.Validate(); err == nil {
			t.Errorf("%s should have been rejected", tc.name)
		}
	}
}

func TestNormalizeSortsForStableOutput(t *testing.T) {
	d := &Document{Hosts: []Host{
		{Name: "z", Monitoring: []MonitoringItem{{Name: "b"}, {Name: "a"}}},
		{Name: "a"},
	}}
	d.Normalize()
	if d.Hosts[0].Name != "a" || d.Hosts[1].Name != "z" {
		t.Error("hosts are not sorted by name")
	}
	if d.Hosts[1].Monitoring[0].Name != "a" {
		t.Error("monitoring items are not sorted by name")
	}
	if d.APIVersion != APIVersion {
		t.Error("Normalize should fill in the apiVersion")
	}
}

// Renaming is the common edit, and it must not destroy the resource. With ids
// present the diff produces a single update, not a delete plus a create.
func TestRenameWithIDIsAnUpdate(t *testing.T) {
	local := doc(Host{ID: "1", Name: "Firewall01", IP: "10.0.0.1"})
	remote := doc(Host{ID: "1", Name: "FW01", IP: "10.0.0.1"})

	p := Diff(local, remote)
	create, update, del := p.Counts()
	if create != 0 || update != 1 || del != 0 {
		t.Fatalf("a rename should be one update, got %d/%d/%d: %v", create, update, del, p.Steps)
	}
	if p.Destructive() {
		t.Error("renaming must not produce a destructive plan")
	}
	step := p.Steps[0]
	if step.Rename != "FW01" {
		t.Errorf("the step should record the previous name, got %q", step.Rename)
	}
	if step.Body["name"] != "Firewall01" {
		t.Errorf("the update body must carry the new name, got %v", step.Body["name"])
	}
	if !strings.Contains(step.String(), `rename from "FW01"`) {
		t.Errorf("the plan should read as a rename, got %q", step.String())
	}
}

func TestRenameOfMonitoringItemWithID(t *testing.T) {
	local := doc(Host{ID: "1", Name: "sw1", Monitoring: []MonitoringItem{{ID: "10", Name: "CPU load"}}})
	remote := doc(Host{ID: "1", Name: "sw1", Monitoring: []MonitoringItem{{ID: "10", Name: "CPU"}}})

	p := Diff(local, remote)
	create, update, del := p.Counts()
	if create != 0 || update != 1 || del != 0 {
		t.Fatalf("expected a single update, got %d/%d/%d: %v", create, update, del, p.Steps)
	}
}

// Without ids a rename cannot be distinguished from a replacement, so the plan
// must at least flag it rather than silently destroying the resource.
func TestRenameWithoutIDIsDetectedAsLikely(t *testing.T) {
	local := doc(Host{Name: "Firewall01", IP: "10.0.0.1", Hostname: "fw01"})
	remote := doc(Host{Name: "FW01", IP: "10.0.0.1", Hostname: "fw01"})

	p := Diff(local, remote)
	if !p.Destructive() {
		t.Fatal("without ids this is a delete plus a create")
	}
	likely := p.LikelyRenames()
	if len(likely) != 1 {
		t.Fatalf("the pair should be flagged as a likely rename, got %v", likely)
	}
	if likely[0].From != "FW01" || likely[0].To != "Firewall01" {
		t.Errorf("unexpected pair: %+v", likely[0])
	}
}

// Two genuinely different hosts must not be mistaken for a rename.
func TestUnrelatedCreateAndDeleteAreNotAName(t *testing.T) {
	local := doc(Host{Name: "new-sw", IP: "10.0.0.9", Hostname: "new"})
	remote := doc(Host{Name: "old-sw", IP: "10.0.0.1", Hostname: "old"})
	if got := Diff(local, remote).LikelyRenames(); len(got) != 0 {
		t.Errorf("different hosts should not be reported as a rename: %v", got)
	}
}

// A stale id must fall back to name matching rather than creating a duplicate.
func TestStaleIDFallsBackToNameMatch(t *testing.T) {
	local := doc(Host{ID: "999", Name: "sw1", IP: "10.0.0.2"})
	remote := doc(Host{ID: "1", Name: "sw1", IP: "10.0.0.1"})

	p := Diff(local, remote)
	create, update, del := p.Counts()
	if create != 0 || update != 1 || del != 0 {
		t.Fatalf("expected a name-matched update, got %d/%d/%d: %v", create, update, del, p.Steps)
	}
	if p.Steps[0].ID != "1" {
		t.Errorf("the update must target the real remote id, got %q", p.Steps[0].ID)
	}
}

// Primary-key references must be sent as numbers: the API rejects a string
// where it expects a pk.
func TestReferencesAreSentAsNumbers(t *testing.T) {
	b := itemBody(&MonitoringItem{Name: "ping", Module: "2", Interval: "1", Credential: "7"})
	for _, k := range []string{"monitoring_module", "interval", "credential"} {
		if _, ok := b[k].(int); !ok {
			t.Errorf("%s should be sent as a number, got %T (%v)", k, b[k], b[k])
		}
	}
	if b["name"] != "ping" {
		t.Errorf("name should stay a string, got %v", b["name"])
	}
}
