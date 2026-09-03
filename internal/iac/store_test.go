package iac

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func sample() *Document {
	return &Document{
		APIVersion:     APIVersion,
		Infrastructure: InfrastructureR{ID: "6", Name: "OT Lab"},
		Hosts: []Host{
			{ID: "1", Name: "FW01", IP: "10.0.0.1", Monitoring: []MonitoringItem{
				{ID: "10", Name: "ping", Module: "2"},
			}},
			{ID: "2", Name: "PLC01", IP: "10.0.0.2"},
		},
	}
}

func TestSaveAndLoadDirectoryRoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "infra")
	if _, err := Save(sample(), dir+"/"); err != nil {
		t.Fatal(err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := sample()
	want.Normalize()
	if len(got.Hosts) != len(want.Hosts) {
		t.Fatalf("expected %d hosts, got %d", len(want.Hosts), len(got.Hosts))
	}
	if got.Infrastructure.ID != "6" || got.Infrastructure.Name != "OT Lab" {
		t.Errorf("infrastructure not round-tripped: %+v", got.Infrastructure)
	}
	if got.Hosts[0].ID != "1" || got.Hosts[0].Name != "FW01" {
		t.Errorf("host not round-tripped: %+v", got.Hosts[0])
	}
	if len(got.Hosts[0].Monitoring) != 1 || got.Hosts[0].Monitoring[0].ID != "10" {
		t.Errorf("monitoring not round-tripped: %+v", got.Hosts[0].Monitoring)
	}
}

// A file per host, named predictably, is the point of directory mode.
func TestDirectoryLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "infra")
	if _, err := Save(sample(), dir+"/"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ManifestName)); err != nil {
		t.Errorf("manifest missing: %v", err)
	}
	for _, name := range []string{"fw01.yaml", "plc01.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, "hosts", name)); err != nil {
			t.Errorf("expected hosts/%s: %v", name, err)
		}
	}
}

// Repeated exports of unchanged state must produce identical bytes, or every
// export would show up as a diff in git.
func TestDirectoryExportIsByteStable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "infra")
	if _, err := Save(sample(), dir+"/"); err != nil {
		t.Fatal(err)
	}
	first := readAll(t, dir)
	if _, err := Save(sample(), dir+"/"); err != nil {
		t.Fatal(err)
	}
	second := readAll(t, dir)

	if len(first) != len(second) {
		t.Fatalf("file set changed between exports: %d vs %d", len(first), len(second))
	}
	for path, content := range first {
		if second[path] != content {
			t.Errorf("%s differs between identical exports", path)
		}
	}
}

// A stale file would read as a host to create on the next apply.
func TestReexportPrunesStaleHostFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "infra")
	if _, err := Save(sample(), dir+"/"); err != nil {
		t.Fatal(err)
	}
	ghost := filepath.Join(dir, "hosts", "ghost.yaml")
	if err := os.WriteFile(ghost, []byte("name: ghost\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Save(sample(), dir+"/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ghost); !os.IsNotExist(err) {
		t.Error("the stale host file should have been removed")
	}
	if len(res.Removed) != 1 || !strings.HasSuffix(res.Removed[0], "ghost.yaml") {
		t.Errorf("the removal should be reported, got %v", res.Removed)
	}
}

// Pruning must not touch anything that is not a host file.
func TestPruningLeavesNonHostFilesAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "infra")
	if _, err := Save(sample(), dir+"/"); err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(dir, "hosts", "README.md")
	if err := os.WriteFile(readme, []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(sample(), dir+"/"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(readme); err != nil {
		t.Errorf("a non-YAML file should survive pruning: %v", err)
	}
}

func TestHostFileNamesAreUniqueAndStable(t *testing.T) {
	hosts := []Host{
		{ID: "1", Name: "FW 01"},
		{ID: "2", Name: "fw-01"},
		{ID: "3", Name: "!!!"},
	}
	first := hostFileNames(hosts)
	second := hostFileNames(hosts)

	seen := map[string]bool{}
	for i, n := range first {
		if seen[n] {
			t.Errorf("duplicate filename %q", n)
		}
		seen[n] = true
		if n != second[i] {
			t.Errorf("filename not stable: %q vs %q", n, second[i])
		}
	}
}

func TestLoadRejectsADirectoryThatIsNotAnExport(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(dir)
	if err == nil {
		t.Fatal("a directory without a manifest should be rejected")
	}
	if !strings.Contains(err.Error(), ManifestName) {
		t.Errorf("the error should name the missing manifest, got %v", err)
	}
}

func TestLoadRejectsHostFileWithoutName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "infra")
	if _, err := Save(sample(), dir+"/"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hosts", "broken.yaml"),
		[]byte("ip: 10.0.0.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("a host file with no name should be rejected")
	}
}

// Single-file mode must keep working exactly as before.
func TestSaveSingleFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "infra.yaml")
	res, err := Save(sample(), path)
	if err != nil {
		t.Fatal(err)
	}
	if res.Directory {
		t.Error("a .yaml path should not be treated as a directory")
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hosts) != 2 {
		t.Errorf("expected 2 hosts, got %d", len(got.Hosts))
	}
}

func TestIsDirTarget(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"./infra/", true},
		{"infra", true},
		{"infra.yaml", false},
		{"a/b/c.yml", false},
		{"-", false},
	}
	for _, tc := range cases {
		if got := IsDirTarget(tc.in); got != tc.want {
			t.Errorf("IsDirTarget(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func readAll(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	var paths []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = string(b)
	}
	return out
}

// A directory export must round-trip templates, or an export/apply cycle would
// propose recreating every template it just wrote.
func TestDirectoryRoundTripPreservesTemplates(t *testing.T) {
	dir := t.TempDir()
	in := &Document{
		APIVersion:     APIVersion,
		Infrastructure: InfrastructureR{ID: "6", Name: "OT Lab"},
		Templates: []Template{{
			ID: "4", Name: "PLC", Status: "published", Organization: "3",
			Modules: []string{"2"},
			Checks:  []MonitoringItem{{ID: "80", Name: "ping", Module: "2"}},
		}},
		Hosts: []Host{{ID: "2", Name: "plc01", Template: "PLC"}},
	}
	if _, err := Save(in, dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(out.Templates) != 1 {
		t.Fatalf("expected the template to survive, got %v", out.Templates)
	}
	if out.Templates[0].Name != "PLC" || len(out.Templates[0].Checks) != 1 {
		t.Errorf("template did not round-trip: %+v", out.Templates[0])
	}
	if out.Hosts[0].Template != "PLC" {
		t.Errorf("host lost its template reference: %+v", out.Hosts[0])
	}
	if p := Diff(out, in); !p.Empty() {
		t.Errorf("a round-trip must diff clean, got %v", p.Steps)
	}
}

// A template file left behind would read as a template to manage on the next
// apply, so an export that no longer produces it removes it and says so.
func TestReexportPrunesTemplateFiles(t *testing.T) {
	dir := t.TempDir()
	with := &Document{
		APIVersion:     APIVersion,
		Infrastructure: InfrastructureR{ID: "6"},
		Templates:      []Template{{ID: "4", Name: "PLC"}},
		Hosts:          []Host{{ID: "2", Name: "plc01"}},
	}
	if _, err := Save(with, dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	without := &Document{
		APIVersion:     APIVersion,
		Infrastructure: InfrastructureR{ID: "6"},
		Hosts:          []Host{{ID: "2", Name: "plc01"}},
	}
	res, err := Save(without, dir)
	if err != nil {
		t.Fatalf("re-save: %v", err)
	}
	if len(res.Removed) != 1 || !strings.Contains(res.Removed[0], "plc.yaml") {
		t.Errorf("expected the stale template file to be reported as removed, got %v", res.Removed)
	}
	out, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(out.Templates) != 0 {
		t.Errorf("expected no templates after the prune, got %v", out.Templates)
	}
}
