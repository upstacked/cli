package cli

import (
	"net/http"
	"strings"
	"testing"

	"github.com/upstacked/cli/internal/errs"
)

func TestLoginStoresCredentialBoundToServer(t *testing.T) {
	e := newEnv(t)
	e.run("profile", "add", "default", "--url", e.stub.URL)
	e.stub.handle("/api/token/", 200, map[string]any{
		"access": "abc", "refresh": "def", "username": "tester",
	})

	res := e.runStdin("hunter2\n", "login", "--username", "tester", "--password-stdin")
	if res.ExitCode != 0 {
		t.Fatalf("login failed (%d): %s", res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "Logged in")

	// The password must never appear in the recorded request as anything but
	// the JSON body field, and never in output.
	notContains(t, res.Stdout, "hunter2")
	notContains(t, res.Stderr, "hunter2")

	reqs := e.stub.requestsTo("POST", "/api/token/")
	if len(reqs) != 1 {
		t.Fatalf("expected one token request, got %d", len(reqs))
	}
	if reqs[0].Body["username"] != "tester" || reqs[0].Body["password"] != "hunter2" {
		t.Errorf("unexpected login body: %v", reqs[0].Body)
	}
}

// A token issued by one server must never be sent to another. This is the
// safeguard described in the skill, so it needs a test that would catch its
// removal.
func TestTokenIsNotSentToADifferentServer(t *testing.T) {
	e := newEnv(t)
	e.login()

	other := newStub(t)
	other.handle("/api/host/", 200, page())

	res := e.run("--api-url", other.URL, "host", "list")
	if res.ExitCode != errs.CodeAuth {
		t.Fatalf("expected auth exit code %d, got %d: %s", errs.CodeAuth, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "were issued by")

	if len(other.requests) != 0 {
		t.Errorf("the CLI contacted the other server with a foreign token: %v", other.requests)
	}
}

func TestHostListRendersTableAndJSON(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	e.stub.handle("/api/host/", 200, page(
		map[string]any{"id": 1, "name": "core-sw-01", "i_hostname": "sw01.acme", "i_ip_address": "10.0.0.1"},
		map[string]any{"id": 2, "name": "fw-01", "i_hostname": "fw01.acme", "i_ip_address": "10.0.0.2"},
	))

	res := e.run("host", "list")
	if res.ExitCode != 0 {
		t.Fatalf("host list failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "core-sw-01")
	contains(t, res.Stdout, "10.0.0.2")

	// The infrastructure from context must scope the request.
	reqs := e.stub.requestsTo("GET", "/api/host/")
	if len(reqs) == 0 || !strings.Contains(reqs[len(reqs)-1].Query, "infrastructure=42") {
		t.Errorf("expected the request to be scoped to infrastructure 42, got %q", reqs[len(reqs)-1].Query)
	}

	jsonRes := e.run("host", "list", "--json")
	doc := jsonRes.JSON(t)
	items, ok := doc["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("expected 2 items in JSON output, got %v", doc["items"])
	}

	idRes := e.run("host", "list", "--id-only")
	if got := strings.TrimSpace(idRes.Stdout); got != "1\n2" {
		t.Errorf("--id-only should emit bare ids, got %q", got)
	}
}

// Truncation must never look like "no more results".
func TestTruncationIsReported(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	items := make([]any, 0, 5)
	for i := 1; i <= 5; i++ {
		items = append(items, map[string]any{"id": i, "name": "host-" + string(rune('0'+i))})
	}
	e.stub.handle("/api/host/", 200, page(items...))

	res := e.run("--limit", "2", "host", "list")
	if res.ExitCode != 0 {
		t.Fatalf("host list failed: %s", res.Stderr)
	}
	contains(t, res.Stderr, "truncated")
	contains(t, res.Stderr, "not the complete set")
}

func TestErrorsCarryExitCodesAndHints(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handle("/api/host/999/", 404, map[string]any{"detail": "Not found."})

	res := e.run("host", "show", "999")
	if res.ExitCode != errs.CodeNotFound {
		t.Errorf("expected exit %d for not-found, got %d", errs.CodeNotFound, res.ExitCode)
	}
	contains(t, res.Stderr, "not found")
	contains(t, res.Stderr, "hint:")
}

func TestNotLoggedInIsAnAuthError(t *testing.T) {
	e := newEnv(t)
	e.run("profile", "add", "default", "--url", e.stub.URL)

	res := e.run("host", "list")
	if res.ExitCode != errs.CodeAuth {
		t.Errorf("expected exit %d, got %d: %s", errs.CodeAuth, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "ups login")
}

func TestNoServerConfiguredExplainsHowToFixIt(t *testing.T) {
	e := newEnv(t)
	res := e.run("host", "list")
	if res.ExitCode != errs.CodeUsage {
		t.Errorf("expected usage exit %d, got %d", errs.CodeUsage, res.ExitCode)
	}
	contains(t, res.Stderr, "ups init --api-url")
}

// Destructive commands must not prompt when stdin is not a terminal; they must
// fail with instructions instead.
func TestDestructiveCommandRefusesToPromptNonInteractively(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handle("/api/host/7/", 200, map[string]any{"id": 7, "name": "core-sw-01"})

	res := e.run("host", "delete", "7")
	if res.ExitCode != errs.CodeUsage {
		t.Errorf("expected usage exit %d, got %d: %s", errs.CodeUsage, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "--yes")

	if got := e.stub.requestsTo("DELETE", "/api/host/7/"); len(got) != 0 {
		t.Error("nothing should have been deleted without confirmation")
	}
}

func TestDryRunPerformsNoWrite(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")

	res := e.run("--dry-run", "host", "create", "--name", "new-sw")
	if res.ExitCode != 0 {
		t.Fatalf("dry run failed: %s", res.Stderr)
	}
	contains(t, res.Stderr, "dry-run")
	if got := e.stub.requestsTo("POST", "/api/host/"); len(got) != 0 {
		t.Error("dry-run must not send a write request")
	}
}

// A monitoring item that silently collects nothing is the worst failure mode,
// so creation tests the item unless explicitly told not to.
func TestMonitoringItemCreateTestsTheNewItem(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handle("/api/monitoring/items/", 201, map[string]any{"id": 55, "name": "CPU"})
	e.stub.handle("/api/monitoring/item/55/test", 200, map[string]any{"value": 42, "ok": true})

	res := e.run("monitoring", "item", "create", "--host", "7", "--name", "CPU", "--module", "3")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	contains(t, res.Stderr, "Test returned data")
	if got := e.stub.requestsTo("GET", "/api/monitoring/item/55/test"); len(got) != 1 {
		t.Error("a newly created monitoring item should be tested automatically")
	}
}

func TestMonitoringItemCreateSkipTest(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handle("/api/monitoring/items/", 201, map[string]any{"id": 55, "name": "CPU"})

	res := e.run("monitoring", "item", "create", "--host", "7", "--name", "CPU", "--skip-test")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	if got := e.stub.requestsTo("GET", "/api/monitoring/item/55/test"); len(got) != 0 {
		t.Error("--skip-test must suppress the automatic test")
	}
}

// A runbook must not start when a credential is missing: a half-run runbook
// can leave a device partially configured.
func TestRunbookRefusesToRunWithMissingCredentials(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	e.stub.handle("/api/orchestration/runbooks/5/missing-credentials/", 200,
		[]any{map[string]any{"name": "core-snmp"}})

	res := e.run("runbook", "run", "5", "--yes")
	if res.ExitCode != errs.CodeConflict {
		t.Errorf("expected conflict exit %d, got %d: %s", errs.CodeConflict, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "core-snmp")
	if got := e.stub.requestsTo("POST", "/api/orchestration/runbook/5/start/42/"); len(got) != 0 {
		t.Error("the runbook must not start when credentials are missing")
	}
}

func TestRunbookRunsWhenPreflightPasses(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	e.stub.handle("/api/orchestration/runbooks/5/missing-credentials/", 200, []any{})
	e.stub.handle("/api/orchestration/runbook/5/start/42/", 200, map[string]any{"id": 900})

	res := e.run("runbook", "run", "5", "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("run failed: %s", res.Stderr)
	}
	contains(t, res.Stderr, "preflight passed")
	if got := e.stub.requestsTo("POST", "/api/orchestration/runbook/5/start/42/"); len(got) != 1 {
		t.Error("the runbook should have started")
	}
}

func TestCredentialSecretComesFromStdinNotArgv(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	e.stub.handle("/api/credential/snmpv3/", 201, map[string]any{"id": 3, "name": "core"})

	res := e.runStdin("s3cret\n", "credential", "create", "snmpv3",
		"--name", "core", "--username", "admin", "--secret-stdin")
	if res.ExitCode != 0 {
		t.Fatalf("credential create failed: %s", res.Stderr)
	}
	notContains(t, res.Stdout, "s3cret")
	notContains(t, res.Stderr, "s3cret")

	reqs := e.stub.requestsTo("POST", "/api/credential/snmpv3/")
	if len(reqs) != 1 || reqs[0].Body["password"] != "s3cret" {
		t.Errorf("secret did not reach the API correctly: %v", reqs)
	}
}

func TestIPAMNextDoesNotClaimByDefault(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handle("/api/ipam/subnet/7/next/", 200, map[string]any{"ip_address": "10.0.0.42"})

	res := e.run("ipam", "next", "--subnet", "7")
	if res.ExitCode != 0 {
		t.Fatalf("ipam next failed: %s", res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "10.0.0.42" {
		t.Errorf("expected the bare address on stdout, got %q", res.Stdout)
	}
	if got := e.stub.requestsTo("POST", "/api/ipam/ip_address/"); len(got) != 0 {
		t.Error("without --claim nothing should be reserved")
	}
}

func TestIPAMNextClaims(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handle("/api/ipam/subnet/7/next/", 200, map[string]any{"ip_address": "10.0.0.42"})
	e.stub.handle("/api/ipam/ip_address/", 201, map[string]any{"id": 12, "ip_address": "10.0.0.42"})

	res := e.run("ipam", "next", "--subnet", "7", "--claim")
	if res.ExitCode != 0 {
		t.Fatalf("claim failed: %s", res.Stderr)
	}
	reqs := e.stub.requestsTo("POST", "/api/ipam/ip_address/")
	if len(reqs) != 1 || reqs[0].Body["ip_address"] != "10.0.0.42" {
		t.Errorf("expected the address to be claimed, got %v", reqs)
	}
}

// Logs are filtered client-side today; the user must be told so, and told when
// results were capped.
func TestLogsReportClientSideFiltering(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	e.stub.handle("/api/logs/", 200, page(
		map[string]any{"id": 1, "host": "core-sw-01", "message": "link down", "level": "error"},
		map[string]any{"id": 2, "host": "fw-01", "message": "ok", "level": "info"},
	))

	res := e.run("logs", "search", "--host", "core-sw-01")
	if res.ExitCode != 0 {
		t.Fatalf("logs search failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "link down")
	notContains(t, res.Stdout, "fw-01")
	contains(t, res.Stderr, "filtered 2 fetched record(s) locally")
	contains(t, res.Stderr, "does not filter yet")
}

func TestContextShowReportsProvenance(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")

	res := e.run("context", "show")
	if res.ExitCode != 0 {
		t.Fatalf("context show failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "from profile")

	// A flag must win over the stored profile, and say so.
	res = e.run("--infra", "99", "context", "show")
	contains(t, res.Stdout, "99")
	contains(t, res.Stdout, "from flag")
}

func TestEnvironmentTokenWorksWithoutLogin(t *testing.T) {
	e := newEnv(t)
	e.run("profile", "add", "default", "--url", e.stub.URL)
	t.Setenv("UPSTACKED_TOKEN", "ci-token")
	e.stub.handle("/api/host/", 200, page())

	res := e.run("host", "list")
	if res.ExitCode != 0 {
		t.Fatalf("expected env token to authenticate: %s", res.Stderr)
	}
	reqs := e.stub.requestsTo("GET", "/api/host/")
	if len(reqs) == 0 || reqs[0].Auth != "Bearer ci-token" {
		t.Errorf("expected the env token in the Authorization header, got %q", reqs[0].Auth)
	}
}

// An expired access token should be refreshed transparently.
func TestExpiredTokenIsRefreshed(t *testing.T) {
	e := newEnv(t)
	e.login()

	calls := 0
	e.stub.handleFunc("/api/host/", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") == "Bearer test-access-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = writeJSON(w, page())
	})
	e.stub.handle("/api/token/refresh/", 200, map[string]any{"access": "fresh-token"})

	res := e.run("host", "list")
	if res.ExitCode != 0 {
		t.Fatalf("expected transparent refresh, got: %s", res.Stderr)
	}
	if len(e.stub.requestsTo("POST", "/api/token/refresh/")) != 1 {
		t.Error("expected exactly one refresh attempt")
	}
	if calls != 2 {
		t.Errorf("expected the request to be retried after refresh, saw %d calls", calls)
	}
}

func TestDoctorReportsEveryProblemAtOnce(t *testing.T) {
	e := newEnv(t)
	res := e.run("doctor")
	if res.ExitCode == 0 {
		t.Error("doctor should fail on an unconfigured setup")
	}
	// Not just the first problem: server and skill are both reported.
	contains(t, res.Stdout, "server url")
	contains(t, res.Stdout, "agent skill")
}

func TestDoctorPassesOnAHealthySetup(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	e.stub.handle("/api/user/details/v2/", 200, map[string]any{"username": "tester"})
	e.stub.handle("/api/infrastructure/42/", 200, map[string]any{"id": 42, "name": "Acme"})

	if res := e.run("skill", "install", "--client", "claude"); res.ExitCode != 0 {
		t.Fatalf("skill install failed: %s", res.Stderr)
	}
	res := e.run("doctor")
	if res.ExitCode != 0 {
		t.Fatalf("doctor should pass, got:\n%s\n%s", res.Stdout, res.Stderr)
	}
	contains(t, res.Stdout, "Everything checks out")
}

func TestDoctorJSONIsMachineReadable(t *testing.T) {
	e := newEnv(t)
	res := e.run("doctor", "--json")
	doc := res.JSON(t)
	if _, ok := doc["checks"].([]any); !ok {
		t.Fatalf("expected a checks array, got %v", doc)
	}
	if doc["ok"] != false {
		t.Error("an unconfigured setup should report ok=false")
	}
}

func writeJSON(w http.ResponseWriter, v any) error {
	return jsonEncode(w, v)
}
