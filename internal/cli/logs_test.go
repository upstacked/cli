package cli

import (
	"bytes"
	"net/http"
	"testing"
)

// legacyLogs registers the old endpoint with two records.
func legacyLogs(e *env) {
	e.stub.handle(logsListPath, 200, page(
		map[string]any{"id": 1, "host": "core-sw-01", "message": "link down", "level": "error"},
		map[string]any{"id": 2, "host": "fw-01", "message": "ok", "level": "info"},
	))
}

// When the server has the search endpoint, the query must go to it: the whole
// point is that Elasticsearch, not this process, decides what matches.
func TestLogsSearchUsesTheServerEndpoint(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	legacyLogs(e)
	e.stub.handleMethod("POST", logsSearchPath, 200, page(
		map[string]any{"id": 7, "host": "core-sw-01", "message": "link down", "severity": "error"},
	))

	res := e.run("logs", "search", "--host", "core-sw-01", "--level", "error",
		"--dataset", "syslog", "--since", "1h", "--sort", "asc")
	if res.ExitCode != 0 {
		t.Fatalf("logs search failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "link down")

	reqs := e.stub.requestsTo("POST", logsSearchPath)
	if len(reqs) != 1 {
		t.Fatalf("expected one search request, got %d", len(reqs))
	}
	body := reqs[0].Body
	if got, ok := body["infrastructures"].([]any); !ok || len(got) != 1 || got[0] != float64(42) {
		t.Errorf("search must be scoped to the active infrastructure, got %v", body["infrastructures"])
	}
	for field, want := range map[string]string{"sort": "asc"} {
		if body[field] != want {
			t.Errorf("expected %s=%q, got %v", field, want, body[field])
		}
	}
	for _, field := range []string{"datasets", "severity", "hosts"} {
		if _, ok := body[field].([]any); !ok {
			t.Errorf("expected %s to be sent, got %v", field, body[field])
		}
	}
	if body["start"] == nil {
		t.Error("--since must become a server-side start bound")
	}
	// The old endpoint must not be touched when the new one answered.
	if n := len(e.stub.requestsTo("GET", logsListPath)); n != 0 {
		t.Errorf("expected no fallback fetch, got %d", n)
	}
}

// A server without the endpoint must still work, and must say that the answer
// came from a weaker search.
func TestLogsFallBackWhenSearchEndpointIsAbsent(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	legacyLogs(e)
	// The search endpoint is left unregistered, which the stub answers 404 -
	// exactly what a server predating it does.

	res := e.run("logs", "search", "--host", "core-sw-01")
	if res.ExitCode != 0 {
		t.Fatalf("logs search failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "link down")
	notContains(t, res.Stdout, "fw-01")
	contains(t, res.Stderr, "has no /api/logs/search/ endpoint")
	contains(t, res.Stderr, "filtered 2 fetched record(s) locally")
}

// Falling back silently drops any filter the old endpoint cannot apply. The
// user must be told which ones, or they will read the result as an answer to
// the question they asked.
func TestLogsFallbackNamesTheFiltersItCannotApply(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	legacyLogs(e)

	res := e.run("logs", "search", "--dataset", "syslog", "--sort", "asc")
	if res.ExitCode != 0 {
		t.Fatalf("logs search failed: %s", res.Stderr)
	}
	contains(t, res.Stderr, "--dataset")
	contains(t, res.Stderr, "--sort")
	contains(t, res.Stderr, "no effect")
}

// Only "this endpoint does not exist" justifies a downgrade. A failed search
// must fail, or a broken index looks like a quiet one.
func TestLogsDoNotFallBackOnAServerError(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	legacyLogs(e)
	e.stub.handleMethod("POST", logsSearchPath, 500, map[string]any{"detail": "index unavailable"})

	res := e.run("logs", "search", "--text", "link down")
	if res.ExitCode == 0 {
		t.Fatalf("a failing search must not be reported as success:\n%s", res.Stdout)
	}
	contains(t, res.Stderr, "server error 500")
	if n := len(e.stub.requestsTo("GET", logsListPath)); n != 0 {
		t.Errorf("a server error must not silently downgrade the search, got %d fallback fetches", n)
	}
}

// --search-mode server is a request for the stronger search specifically.
// Answering it with the weaker one would be a lie by omission.
func TestLogsSearchModeServerRefusesToDowngrade(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	legacyLogs(e)

	res := e.run("logs", "search", "--search-mode", "server")
	if res.ExitCode == 0 {
		t.Fatalf("expected a failure, got:\n%s", res.Stdout)
	}
	contains(t, res.Stderr, "no log search endpoint")
	if n := len(e.stub.requestsTo("GET", logsListPath)); n != 0 {
		t.Errorf("expected no fallback fetch, got %d", n)
	}
}

// --search-mode client keeps the old behaviour available on a server that has
// both, for comparing the two.
func TestLogsSearchModeClientSkipsTheSearchEndpoint(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	legacyLogs(e)
	e.stub.handleMethod("POST", logsSearchPath, 200, page())

	res := e.run("logs", "search", "--search-mode", "client", "--host", "core-sw-01")
	if res.ExitCode != 0 {
		t.Fatalf("logs search failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "link down")
	if n := len(e.stub.requestsTo("POST", logsSearchPath)); n != 0 {
		t.Errorf("expected the search endpoint to be skipped, got %d requests", n)
	}
}

// The endpoint declares its response as a copy of its request, so the real
// shape is whatever the deployment returns. A raw Elasticsearch envelope is
// one of them, and its documents live under _source.
func TestLogsSearchReadsElasticsearchEnvelopes(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	e.stub.handleMethod("POST", logsSearchPath, 200, map[string]any{
		"hits": map[string]any{
			"hits": []any{
				map[string]any{
					"_id":     "abc",
					"_source": map[string]any{"@timestamp": "2026-09-04T08:00:00Z", "host": "plc01", "message": "bus fault", "severity": "crit"},
				},
			},
		},
	})

	res := e.run("logs", "search")
	if res.ExitCode != 0 {
		t.Fatalf("logs search failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "bus fault")
	contains(t, res.Stdout, "plc01")
	contains(t, res.Stdout, "crit")
}

// --json must emit the log document, not the index bookkeeping around it.
func TestLogsSearchJSONEmitsTheDocument(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	e.stub.handleMethod("POST", logsSearchPath, 200, map[string]any{
		"hits": map[string]any{
			"hits": []any{
				map[string]any{"_index": "logs-2026", "_source": map[string]any{"message": "bus fault"}},
			},
		},
	})

	res := e.run("--json", "logs", "search")
	if res.ExitCode != 0 {
		t.Fatalf("logs search failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "bus fault")
	notContains(t, res.Stdout, "_index")
}

// The search endpoint scopes by numeric id. Sending the request without one
// would widen it to everything the caller can read.
func TestLogsSearchRequiresAnInfrastructure(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("POST", logsSearchPath, 200, page())

	res := e.run("logs", "search", "--search-mode", "server")
	if res.ExitCode == 0 {
		t.Fatalf("expected a failure, got:\n%s", res.Stdout)
	}
	contains(t, res.Stderr, "no infrastructure selected")
	if n := len(e.stub.requestsTo("POST", logsSearchPath)); n != 0 {
		t.Errorf("an unscoped search must not be sent, got %d requests", n)
	}
}

// A short page ends the traversal. Without that, a server that accepts
// search_after and ignores it would be paged until the page cap.
func TestLogsSearchStopsOnAShortPage(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	e.stub.handleFunc(logsSearchPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = jsonEncode(w, map[string]any{"results": []any{
			map[string]any{"message": "one", "sort": []any{1}},
		}})
	})

	res := e.run("logs", "search")
	if res.ExitCode != 0 {
		t.Fatalf("logs search failed: %s", res.Stderr)
	}
	if n := len(e.stub.requestsTo("POST", logsSearchPath)); n != 1 {
		t.Errorf("expected traversal to stop after one short page, got %d requests", n)
	}
}

// follow polls. Re-probing a missing endpoint every tick doubles the request
// rate to learn something that cannot have changed.
func TestLogsBackendIsProbedOnce(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	legacyLogs(e)

	var out, errOut bytes.Buffer
	app := &App{Stdin: nil, Stdout: &out, Stderr: &errOut, ConfigDir: e.dir}
	root := NewRoot(app)
	root.SetArgs([]string{"logs", "search"})
	if err := root.Execute(); err != nil {
		t.Fatalf("first search failed: %v", err)
	}
	if _, _, err := app.searchLogs(logQuery{limit: 10}); err != nil {
		t.Fatalf("second search failed: %v", err)
	}
	if n := len(e.stub.requestsTo("POST", logsSearchPath)); n != 1 {
		t.Errorf("expected the endpoint to be probed once, got %d probes", n)
	}
}

func TestLogsRejectUnknownDatasets(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")

	res := e.run("logs", "search", "--dataset", "netflow")
	if res.ExitCode == 0 {
		t.Fatal("an unknown dataset must be rejected before the request is sent")
	}
	contains(t, res.Stderr, "flow, monitoring or syslog")
}
