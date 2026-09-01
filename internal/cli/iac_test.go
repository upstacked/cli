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
	if got := e.stub.requestsTo("POST", "/api/monitoring/items/"); len(got) != 1 {
		t.Errorf("expected the monitoring item to be created, saw %d", len(got))
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
