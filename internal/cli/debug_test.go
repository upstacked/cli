package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/upstacked/cli/internal/history"
)

func TestDebugLogRecordsTheCommandItsExitAndItsRequests(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("GET", "/api/monitoring/modules/", 200, page(
		map[string]any{"id": 3, "name": "uptime"},
	))

	if res := e.run("monitoring", "module", "list"); res.ExitCode != 0 {
		t.Fatalf("list failed: %s", res.Stderr)
	}

	res := e.run("debug", "log", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("debug log failed: %s", res.Stderr)
	}
	var recs []history.Record
	if err := json.Unmarshal([]byte(res.Stdout), &recs); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, res.Stdout)
	}

	var found *history.Record
	for i := range recs {
		if recs[i].Command == "monitoring module list" {
			found = &recs[i]
		}
	}
	if found == nil {
		t.Fatalf("the invocation was not recorded; got %d records", len(recs))
	}
	if found.Exit != 0 {
		t.Errorf("exit = %d", found.Exit)
	}
	// The status codes are the point: "what did the server actually say" is
	// the first question a support conversation asks.
	if len(found.Requests) == 0 {
		t.Fatal("no API requests recorded")
	}
	var sawModules bool
	for _, c := range found.Requests {
		if c.Path == "/api/monitoring/modules/" && c.Status == 200 {
			sawModules = true
		}
	}
	if !sawModules {
		t.Errorf("expected the modules request with its status, got %+v", found.Requests)
	}
}

// A failed command is the one worth having in the log, and cobra's PostRun
// does not fire for it.
func TestDebugLogKeepsFailedInvocations(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("GET", "/api/monitoring/items/999/", 404, map[string]any{"detail": "Not found."})

	if res := e.run("monitoring", "item", "show", "999"); res.ExitCode == 0 {
		t.Fatal("expected the show to fail")
	}

	res := e.run("debug", "log", "--failed")
	if res.ExitCode != 0 {
		t.Fatalf("debug log failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "monitoring item show")
	contains(t, res.Stdout, "not found")
	// A successful command must not appear under --failed.
	notContains(t, res.Stdout, "debug log")
}

// Secrets are supposed to arrive on stdin, but a flag added later that takes
// one in argv would otherwise be written to this file forever.
func TestDebugLogRedactsSecretLookingFlagValues(t *testing.T) {
	got := history.RedactArgs([]string{
		"login", "--username", "tester", "--password", "hunter2",
		"credential", "create", "--community=public", "--secret-stdin",
		"--name", "core",
	})
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "hunter2") {
		t.Errorf("password value survived redaction: %s", joined)
	}
	if strings.Contains(joined, "public") {
		t.Errorf("community value survived redaction: %s", joined)
	}
	// --secret-stdin names a source, not a value, so the argument after it is
	// an ordinary flag and must not be swallowed.
	if !strings.Contains(joined, "--name core") {
		t.Errorf("a -stdin flag must not redact the next argument: %s", joined)
	}
	if !strings.Contains(joined, "tester") {
		t.Errorf("non-secret values must survive: %s", joined)
	}
}

func TestHistoryCanBeTurnedOff(t *testing.T) {
	e := newEnv(t)
	t.Setenv("UPS_NO_HISTORY", "1")
	e.login()

	res := e.run("debug", "log", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("debug log failed: %s", res.Stderr)
	}
	if strings.Contains(res.Stdout, "\"command\"") {
		t.Errorf("nothing should have been recorded: %s", res.Stdout)
	}
}

// Which server a setting came from matters more than its value: staging and
// production differ by one line of config and nothing in the prompt.
func TestDebugInfoReportsWhereTheServerCameFrom(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")

	res := e.run("debug", "info")
	if res.ExitCode != 0 {
		t.Fatalf("debug info failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "api_url")
	contains(t, res.Stdout, "api_url_source")
	contains(t, res.Stdout, "42")
	contains(t, res.Stdout, "authenticated")
}

// The bundle is meant to be pasted into a bug report, so it must carry no
// credential material.
func TestDebugBundleCarriesNoToken(t *testing.T) {
	e := newEnv(t)
	e.login()

	res := e.run("debug", "bundle")
	if res.ExitCode != 0 {
		t.Fatalf("debug bundle failed: %s", res.Stderr)
	}
	notContains(t, res.Stdout, "test-access-token")
	notContains(t, res.Stdout, "test-refresh-token")
	contains(t, res.Stdout, "\"info\"")
	contains(t, res.Stdout, "\"recent\"")
}
