package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/upstacked/cli/internal/errs"
)

// remoteState registers the endpoints export reads.
func remoteState(e *env, hosts []any, items []any) {
	e.stub.handle("/api/infrastructure/42/", 200, map[string]any{"id": 42, "name": "Acme"})
	e.stub.handleMethod("GET", "/api/host/", 200, page(hosts...))
	e.stub.handleMethod("GET", "/api/monitoring/items/", 200, page(items...))
}

func TestExportIsStableAcrossRuns(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	// Deliberately unsorted, to prove the export normalises order.
	remoteState(e,
		[]any{
			map[string]any{"id": 2, "name": "fw-01", "i_ip_address": "10.0.0.2"},
			map[string]any{"id": 1, "name": "core-sw-01", "i_ip_address": "10.0.0.1"},
		},
		[]any{
			map[string]any{"id": 20, "host": 1, "name": "Memory", "interval": 60},
			map[string]any{"id": 10, "host": 1, "name": "CPU", "interval": 30},
		},
	)

	first := e.run("export")
	if first.ExitCode != 0 {
		t.Fatalf("export failed: %s", first.Stderr)
	}
	second := e.run("export")
	if first.Stdout != second.Stdout {
		t.Errorf("export is not deterministic:\n--- first ---\n%s\n--- second ---\n%s",
			first.Stdout, second.Stdout)
	}
	// Sorted by name, so core-sw-01 precedes fw-01 and CPU precedes Memory.
	if strings.Index(first.Stdout, "core-sw-01") > strings.Index(first.Stdout, "fw-01") {
		t.Error("hosts should be sorted by name")
	}
	if strings.Index(first.Stdout, "CPU") > strings.Index(first.Stdout, "Memory") {
		t.Error("monitoring items should be sorted by name")
	}
}

// The property that makes infrastructure-as-code trustworthy: export, apply,
// re-export must not drift.
func TestRoundTripProducesNoDiff(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	remoteState(e,
		[]any{map[string]any{"id": 1, "name": "core-sw-01", "i_ip_address": "10.0.0.1", "i_hostname": "sw01"}},
		[]any{map[string]any{"id": 10, "host": 1, "name": "CPU", "interval": 30, "monitoring_module": 3}},
	)

	path := filepath.Join(t.TempDir(), "infra.yaml")
	if res := e.run("export", "--out", path); res.ExitCode != 0 {
		t.Fatalf("export failed: %s", res.Stderr)
	}

	res := e.run("diff", path)
	if res.ExitCode != 0 {
		t.Fatalf("diff failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "No changes")
}

func TestDiffDetectsCreateUpdateAndDelete(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	remoteState(e,
		[]any{
			map[string]any{"id": 1, "name": "core-sw-01", "i_ip_address": "10.0.0.1"},
			map[string]any{"id": 2, "name": "old-sw", "i_ip_address": "10.0.0.9"},
		},
		[]any{map[string]any{"id": 10, "host": 1, "name": "CPU", "interval": 30}},
	)

	doc := `apiVersion: upstacked/v1
infrastructure:
  id: "42"
hosts:
  - name: core-sw-01
    ip: 10.0.0.5
    monitoring:
      - name: CPU
        interval: 30
      - name: Memory
        interval: 60
  - name: brand-new
    ip: 10.0.0.7
`
	path := filepath.Join(t.TempDir(), "infra.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	res := e.run("diff", path)
	if res.ExitCode != 0 {
		t.Fatalf("diff failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "create host brand-new")
	contains(t, res.Stdout, "create monitoring core-sw-01/Memory")
	contains(t, res.Stdout, "update host core-sw-01 (ip)")
	contains(t, res.Stdout, "delete host old-sw")
	contains(t, res.Stdout, "Deleting monitoring is silent")
}

// apply must refuse a destructive plan unless the deletion is opted into.
func TestApplyRefusesDeletionsWithoutAllowDelete(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	remoteState(e,
		[]any{map[string]any{"id": 2, "name": "old-sw"}},
		[]any{},
	)

	doc := "apiVersion: upstacked/v1\ninfrastructure:\n  id: \"42\"\nhosts: []\n"
	path := filepath.Join(t.TempDir(), "infra.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	res := e.run("apply", path, "--yes")
	if res.ExitCode != errs.CodeConflict {
		t.Errorf("expected conflict exit %d, got %d: %s", errs.CodeConflict, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "--allow-delete")
	if got := e.stub.requestsTo("DELETE", "/api/host/2/"); len(got) != 0 {
		t.Error("nothing should be deleted without --allow-delete")
	}
}

func TestApplyCreatesAndUpdates(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	remoteState(e,
		[]any{map[string]any{"id": 1, "name": "core-sw-01", "i_ip_address": "10.0.0.1"}},
		[]any{},
	)
	e.org("3")
	e.stub.handleMethod("PATCH", "/api/host/1/", 200, map[string]any{"id": 1})
	e.stub.handleMethod("POST", "/api/monitoring/items/", 201, map[string]any{"id": 99})

	doc := `apiVersion: upstacked/v1
infrastructure:
  id: "42"
hosts:
  - name: core-sw-01
    ip: 10.0.0.5
    monitoring:
      - name: CPU
        interval: 30
`
	path := filepath.Join(t.TempDir(), "infra.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	res := e.run("apply", path, "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("apply failed: %s\n%s", res.Stderr, res.Stdout)
	}
	if got := e.stub.requestsTo("PATCH", "/api/host/1/"); len(got) != 1 {
		t.Errorf("expected the host to be updated, saw %d PATCH requests", len(got))
	}
	got := e.stub.requestsTo("POST", "/api/monitoring/items/")
	if len(got) != 1 {
		t.Fatalf("expected the monitoring item to be created, saw %d", len(got))
	}
	// A create with no organization is refused by the API outright.
	if got[0].Body["organization"] != float64(3) {
		t.Errorf("expected the create to carry an organization, got %v", got[0].Body)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	remoteState(e,
		[]any{map[string]any{"id": 1, "name": "core-sw-01", "i_ip_address": "10.0.0.1"}},
		[]any{map[string]any{"id": 10, "host": 1, "name": "CPU", "interval": 30}},
	)

	path := filepath.Join(t.TempDir(), "infra.yaml")
	if res := e.run("export", "--out", path); res.ExitCode != 0 {
		t.Fatalf("export failed: %s", res.Stderr)
	}
	res := e.run("apply", path, "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("apply failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "No changes")
	if len(e.stub.requestsTo("PATCH", "/api/host/1/")) != 0 {
		t.Error("a no-op apply must not write")
	}
}

func TestDocumentWithDuplicateNamesIsRejected(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")

	doc := `apiVersion: upstacked/v1
infrastructure:
  id: "42"
hosts:
  - name: dup
  - name: dup
`
	path := filepath.Join(t.TempDir(), "infra.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	res := e.run("diff", path)
	if res.ExitCode != errs.CodeUsage {
		t.Errorf("expected usage exit %d, got %d", errs.CodeUsage, res.ExitCode)
	}
	contains(t, res.Stderr, "duplicate host name")
}

// An incomplete export would read as deletions, so a capped traversal must be
// an error rather than a quiet partial result.
func TestExportRefusesTruncatedResults(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	e.stub.handle("/api/infrastructure/42/", 200, map[string]any{"id": 42, "name": "Acme"})
	// Always advertise another page, so traversal hits the cap.
	e.stub.handleFunc("/api/host/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = jsonEncode(w, map[string]any{
			"count": 100000, "next": r.URL.String(), "previous": nil,
			"results": []any{map[string]any{"id": 1, "name": "h1"}},
		})
	})

	res := e.run("export")
	if res.ExitCode == 0 {
		t.Error("export should refuse to emit a truncated document")
	}
	contains(t, res.Stderr, "truncated")
	contains(t, res.Stderr, "look like deletions")
}

// Directory export is the documented default in the skill, so it gets an
// end-to-end test through the real command surface.
func TestExportToDirectoryAndDiffFromIt(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	remoteState(e,
		[]any{
			map[string]any{"id": 1, "name": "core-sw-01", "i_ip_address": "10.0.0.1"},
			map[string]any{"id": 2, "name": "fw-01", "i_ip_address": "10.0.0.2"},
		},
		[]any{map[string]any{"id": 10, "host": 1, "name": "CPU"}},
	)

	dir := filepath.Join(t.TempDir(), "infra")
	if res := e.run("export", "--out", dir+"/"); res.ExitCode != 0 {
		t.Fatalf("export failed: %s", res.Stderr)
	}
	for _, p := range []string{"infrastructure.yaml", "hosts/core-sw-01.yaml", "hosts/fw-01.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("expected %s: %v", p, err)
		}
	}

	// The directory must diff exactly like the single-file form.
	res := e.run("diff", dir)
	if res.ExitCode != 0 {
		t.Fatalf("diff failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "No changes")
}

func TestApplyFromDirectory(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	remoteState(e,
		[]any{map[string]any{"id": 1, "name": "core-sw-01", "i_ip_address": "10.0.0.1"}},
		[]any{},
	)
	e.stub.handleMethod("PATCH", "/api/host/1/", 200, map[string]any{"id": 1})

	dir := filepath.Join(t.TempDir(), "infra")
	if res := e.run("export", "--out", dir+"/"); res.ExitCode != 0 {
		t.Fatalf("export failed: %s", res.Stderr)
	}
	// Edit the exported host file the way a user would.
	hostFile := filepath.Join(dir, "hosts", "core-sw-01.yaml")
	b, err := os.ReadFile(hostFile)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(b), "10.0.0.1", "10.0.0.5", 1)
	if err := os.WriteFile(hostFile, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	res := e.run("apply", dir, "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("apply failed: %s\n%s", res.Stderr, res.Stdout)
	}
	if got := e.stub.requestsTo("PATCH", "/api/host/1/"); len(got) != 1 {
		t.Fatalf("expected one update, saw %d", len(got))
	}
}

// Re-exporting must clear out files for hosts that no longer exist, or the
// next apply would propose re-creating them.
func TestReexportRemovesFilesForDeletedHosts(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	remoteState(e,
		[]any{
			map[string]any{"id": 1, "name": "core-sw-01"},
			map[string]any{"id": 2, "name": "fw-01"},
		},
		[]any{},
	)
	dir := filepath.Join(t.TempDir(), "infra")
	if res := e.run("export", "--out", dir+"/"); res.ExitCode != 0 {
		t.Fatalf("export failed: %s", res.Stderr)
	}

	// fw-01 disappears from the platform.
	e.stub.handleMethod("GET", "/api/host/", 200, page(
		map[string]any{"id": 1, "name": "core-sw-01"},
	))
	res := e.run("export", "--out", dir+"/")
	if res.ExitCode != 0 {
		t.Fatalf("re-export failed: %s", res.Stderr)
	}
	contains(t, res.Stderr, "removed")
	if _, err := os.Stat(filepath.Join(dir, "hosts", "fw-01.yaml")); !os.IsNotExist(err) {
		t.Error("the file for the deleted host should be gone")
	}

	// And the result must diff clean, not propose re-creating fw-01.
	if res := e.run("diff", dir); !strings.Contains(res.Stdout, "No changes") {
		t.Errorf("expected a clean diff after re-export, got:\n%s", res.Stdout)
	}
}

// remoteStateWithTemplate registers an infrastructure whose host carries a
// template, plus the template library the export reads.
func remoteStateWithTemplate(e *env, hosts []any, hostItems []any, checks []any) {
	e.stub.handle("/api/infrastructure/42/", 200, map[string]any{"id": 42, "name": "Acme"})
	e.stub.handleMethod("GET", "/api/host/", 200, page(hosts...))
	e.stub.handleMethod("GET", "/api/monitoring/templates/", 200, page(
		map[string]any{"id": 4, "name": "PLC", "published_status": "published",
			"organization":       3,
			"monitoring_modules": []any{map[string]any{"id": 2, "name": "ping"}}},
	))
	// The same endpoint serves a host's items and a template's checks; only
	// the query says which.
	e.stub.handleFunc("/api/monitoring/items/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("monitoring_template") != "" {
			_ = jsonEncode(w, page(checks...))
			return
		}
		_ = jsonEncode(w, page(hostItems...))
	})
}

func TestExportRecordsTemplatesTheHostsUse(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	remoteStateWithTemplate(e,
		[]any{map[string]any{"id": 2, "name": "plc01", "monitoring_template": 4,
			"monitoring_template_name": "PLC"}},
		[]any{map[string]any{"id": 27, "host": 2, "name": "ping"}},
		[]any{map[string]any{"id": 80, "name": "ping", "monitoring_module": 2}},
	)

	res := e.run("export")
	if res.ExitCode != 0 {
		t.Fatalf("export failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "templates:")
	contains(t, res.Stdout, "template: PLC")
	contains(t, res.Stdout, "checks:")
}

func TestTemplateExportRoundTripsWithoutADiff(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	remoteStateWithTemplate(e,
		[]any{map[string]any{"id": 2, "name": "plc01", "monitoring_template_name": "PLC"}},
		[]any{map[string]any{"id": 27, "host": 2, "name": "ping"}},
		[]any{map[string]any{"id": 80, "name": "ping", "monitoring_module": 2}},
	)
	path := filepath.Join(t.TempDir(), "infra.yaml")
	if res := e.run("export", "--out", path); res.ExitCode != 0 {
		t.Fatalf("export failed: %s", res.Stderr)
	}

	res := e.run("diff", path)
	if res.ExitCode != 0 {
		t.Fatalf("diff failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "No changes")
}

// Pointing a host at a template deletes every item it already has. That must
// be spelled out and gated, not slipped through as a one-field host update.
func TestApplyRefusesToReplaceHostMonitoringWithoutAllowDelete(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	remoteStateWithTemplate(e,
		[]any{map[string]any{"id": 2, "name": "plc01"}},
		[]any{
			map[string]any{"id": 27, "host": 2, "name": "ping"},
			map[string]any{"id": 28, "host": 2, "name": "hand-added"},
		},
		[]any{map[string]any{"id": 80, "name": "ping", "monitoring_module": 2}},
	)

	path := filepath.Join(t.TempDir(), "infra.yaml")
	doc := `apiVersion: upstacked/v1
infrastructure:
  id: "42"
hosts:
  - id: "2"
    name: plc01
    template: PLC
`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	res := e.run("apply", path, "--yes")
	if res.ExitCode != errs.CodeConflict {
		t.Fatalf("expected conflict exit %d, got %d: %s", errs.CodeConflict, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stdout, "hand-added (removed by the template change)")
	contains(t, res.Stdout, "2 existing monitoring item(s) will be replaced")
	if got := e.stub.requestsTo("PATCH", "/api/host/2/"); len(got) != 0 {
		t.Error("nothing may be written before the replacement is allowed")
	}
}

func TestApplyBindsTheTemplateByNameToItsID(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	remoteStateWithTemplate(e,
		[]any{map[string]any{"id": 2, "name": "plc01"}},
		[]any{},
		[]any{map[string]any{"id": 80, "name": "ping", "monitoring_module": 2}},
	)
	e.stub.handleMethod("PATCH", "/api/host/2/", 200, map[string]any{"id": 2})

	path := filepath.Join(t.TempDir(), "infra.yaml")
	doc := `apiVersion: upstacked/v1
infrastructure:
  id: "42"
hosts:
  - id: "2"
    name: plc01
    template: PLC
`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	res := e.run("apply", path, "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("apply failed: %s\n%s", res.Stderr, res.Stdout)
	}
	got := e.stub.requestsTo("PATCH", "/api/host/2/")
	if len(got) != 1 {
		t.Fatalf("expected one host patch, got %d", len(got))
	}
	if got[0].Body["monitoring_template"] != float64(4) {
		t.Errorf("expected the template name to resolve to id 4, got %v", got[0].Body)
	}
	if _, leaked := got[0].Body["__template_name"]; leaked {
		t.Error("the internal template marker must never be sent to the API")
	}
}

// Templates are organization-wide, so a plan that edits one says so.
func TestPlanWarnsThatTemplatesAreSharedBeyondTheInfrastructure(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	remoteStateWithTemplate(e,
		[]any{map[string]any{"id": 2, "name": "plc01", "monitoring_template_name": "PLC"}},
		[]any{},
		[]any{map[string]any{"id": 80, "name": "ping", "monitoring_module": 2}},
	)

	path := filepath.Join(t.TempDir(), "infra.yaml")
	doc := `apiVersion: upstacked/v1
infrastructure:
  id: "42"
templates:
  - id: "4"
    name: PLC
    checks:
      - id: "80"
        name: ping
        module: "2"
      - name: uptime
        module: "3"
hosts:
  - id: "2"
    name: plc01
    template: PLC
`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	res := e.run("diff", path)
	if res.ExitCode != 0 {
		t.Fatalf("diff failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "create check PLC/uptime")
	contains(t, res.Stdout, "Templates belong to the organization")
}
