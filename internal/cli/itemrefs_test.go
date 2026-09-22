package cli

import (
	"testing"

	"github.com/upstacked/cli/internal/errs"
)

func stubItemRefs(e *env) {
	e.t.Helper()
	e.stub.handleMethod("GET", intervalsPath, 200, page(
		map[string]any{"id": 1, "number_of_period": 1, "interval_period": "minutes"},
		map[string]any{"id": 2, "number_of_period": 5, "interval_period": "minutes"},
		map[string]any{"id": 5, "number_of_period": 1, "interval_period": "hours"},
	))
	e.stub.handleMethod("GET", credentialTagsPath, 200, page(
		map[string]any{"id": 2, "name": "SNMPv2"},
	))
	tag := map[string]any{"id": 2, "name": "SNMPv2"}
	e.stub.handleMethod("GET", credentialsPath, 200, page(
		map[string]any{"id": 1, "name": "snmpv2", "scope": "organization", "tag": tag, "credential_type": "snmpv2"},
		map[string]any{"id": 2, "name": "snmpv2", "scope": "system", "tag": tag, "credential_type": "snmpv2"},
	))
	e.stub.handleMethod("POST", "/api/monitoring/items/", 201, map[string]any{"id": 55})
}

func createItem(e *env, extra ...string) result {
	args := append([]string{"monitoring", "item", "create", "--host", "205", "--module", "40",
		"--name", "Interfaces", "--data-source", "snmp", "--skip-test"}, extra...)
	return e.run(args...)
}

// An item with no interval is never scheduled, and nothing says so.
func TestItemCreateRefusesWithoutAnInterval(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.org("2")
	stubItemRefs(e)

	res := createItem(e)
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected usage exit, got %d: %s", res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "ups monitoring interval list")
	if len(e.stub.requestsTo("POST", "/api/monitoring/items/")) != 0 {
		t.Error("nothing may be written without an interval")
	}
}

func TestItemCreateTakesAnIntervalAsADuration(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.org("2")
	stubItemRefs(e)

	res := createItem(e, "--interval", "5m", "--timeout", "10")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	body := e.stub.requestsTo("POST", "/api/monitoring/items/")[0].Body
	if body["interval"] != float64(2) || body["timeout"] != float64(10) {
		t.Errorf("expected interval 2 (5m) and timeout 10, got %v / %v", body["interval"], body["timeout"])
	}
}

// Rounding to a neighbour would poll on a different schedule than asked.
func TestItemCreateRefusesAnIntervalTheServerDoesNotOffer(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.org("2")
	stubItemRefs(e)

	res := createItem(e, "--interval", "10m")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected usage exit, got %d: %s", res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "1m, 5m, 1h")
}

// The portal requires both the tag and the credential; given the tag, the
// organization-scope credential with it is picked, never its system twin.
func TestItemCreateResolvesACredentialByTag(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.org("2")
	stubItemRefs(e)

	res := createItem(e, "--interval", "5m", "--credential-tag", "snmpv2")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	body := e.stub.requestsTo("POST", "/api/monitoring/items/")[0].Body
	if body["credential_tag"] != "SNMPv2" || body["credential"] != float64(1) || body["credential_type"] != "snmpv2" {
		t.Errorf("expected tag SNMPv2, credential 1, type snmpv2: %v", body)
	}
}

func TestItemCreateDerivesTheTagFromTheCredential(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.org("2")
	stubItemRefs(e)

	res := createItem(e, "--interval", "5m", "--credential", "1")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	if got := e.stub.requestsTo("POST", "/api/monitoring/items/")[0].Body["credential_tag"]; got != "SNMPv2" {
		t.Errorf("expected the credential's tag to be set, got %v", got)
	}
}
