package iac

import "testing"

func doc(hosts ...Host) *Document {
	d := &Document{APIVersion: APIVersion, Hosts: hosts}
	d.Normalize()
	return d
}

func TestDiffOfIdenticalDocumentsIsEmpty(t *testing.T) {
	a := doc(Host{Name: "sw1", IP: "10.0.0.1", Monitoring: []MonitoringItem{{Name: "CPU", Interval: 30}}})
	b := doc(Host{Name: "sw1", IP: "10.0.0.1", Monitoring: []MonitoringItem{{Name: "CPU", Interval: 30}}})
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
		{Name: "CPU", Interval: 60}, {Name: "Disk"},
	}})
	remote := doc(Host{Name: "sw1", Monitoring: []MonitoringItem{
		{Name: "CPU", Interval: 30}, {Name: "Memory"},
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
