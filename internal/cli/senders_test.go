package cli

import (
	"net/url"
	"testing"
)

func sendersBody() map[string]any {
	infra := map[string]any{"id": 6, "name": "Oslo", "customer": "Acme"}
	return map[string]any{
		"generated":       "2026-10-23T10:00:00Z",
		"infrastructures": []any{infra},
		"receivers": []any{map[string]any{
			"id": 3, "name": "agent-01", "running": true, "address": "10.0.0.5",
			"infrastructure": infra,
			"listeners": []any{
				map[string]any{"protocol": "syslog/udp", "address": "0.0.0.0:514"},
				map[string]any{"protocol": "netflow", "address": "0.0.0.0:2055"},
			},
		}},
		"legacy_logging_agents": []any{},
		"devices": []any{
			map[string]any{
				"key": "10.20.0.1", "address": "10.20.0.1", "name": "core-sw-01",
				"host":           map[string]any{"id": 11, "name": "core-sw-01", "sends_syslog": true, "sends_flow": nil},
				"infrastructure": infra, "platform": "cisco_ios", "status": "bad",
				"signals": map[string]any{
					"syslog": map[string]any{"kind": "syslog", "status": "bad", "alert": true, "expected": true,
						"proposed": false, "state": "stopped", "window_minutes": 60, "receivers": []any{3}},
					"flow": map[string]any{"kind": "flow", "status": "ok", "alert": nil,
						"proposed": true, "state": "arriving", "window_minutes": 5, "receivers": []any{3}},
				},
				"checks": []any{map[string]any{"title": "Syslog stopped", "status": "bad", "detail": "Last record 3 hours ago."}},
				"fix":    map[string]any{"title": "Point the device at the receiver", "code": "logging host 10.0.0.5"},
			},
			map[string]any{
				"key": "10.20.0.9", "address": "10.20.0.9", "name": "10.20.0.9", "host": nil,
				"infrastructure": infra, "platform": "generic", "status": "info",
				"signals": map[string]any{
					"syslog": map[string]any{"kind": "syslog", "status": "ok", "alert": nil,
						"proposed": false, "state": "arriving", "window_minutes": 60},
				},
				"checks": []any{}, "fix": nil,
			},
		},
		"proposals": []any{11},
	}
}

func sendersEnv(t *testing.T) *env {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	e.stub.handleMethod("GET", trafficPath, 200, sendersBody())
	return e
}

// Without an infrastructure the server widens to everything the caller can
// read; that must be asked for, not fallen into.
func TestSendersListIsScopedToTheActiveInfrastructure(t *testing.T) {
	e := sendersEnv(t)
	res := e.run("senders", "list")
	if res.ExitCode != 0 {
		t.Fatalf("senders list failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "core-sw-01")
	contains(t, res.Stdout, "stopped, alert")
	contains(t, res.Stdout, "arriving, offered")
	contains(t, res.Stderr, "ups senders accept")
	q, _ := url.ParseQuery(e.stub.requestsTo("GET", trafficPath)[0].Query)
	if q.Get("infrastructure") != "6" {
		t.Errorf("expected infrastructure=6, got %q", q.Get("infrastructure"))
	}

	unscoped := newEnv(t)
	unscoped.login()
	res = unscoped.run("senders", "list")
	if res.ExitCode == 0 {
		t.Fatal("an unscoped list must be refused")
	}
	contains(t, res.Stderr, "--all-infras")
}

func TestSendersWarnsAboutLegacyLoggingAgents(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	body := sendersBody()
	body["devices"] = []any{}
	body["legacy_logging_agents"] = []any{map[string]any{"id": 6, "name": "Oslo", "customer": "Acme"}}
	e.stub.handleMethod("GET", trafficPath, 200, body)

	res := e.run("senders", "list", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("senders list failed: %s", res.Stderr)
	}
	contains(t, res.Stderr, "legacy logging agent")
}

func TestSendersShowNamesTheReceiverAndFix(t *testing.T) {
	e := sendersEnv(t)
	res := e.run("senders", "show", "core-sw-01")
	if res.ExitCode != 0 {
		t.Fatalf("senders show failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "agent-01 at 10.0.0.5")
	contains(t, res.Stdout, "0.0.0.0:514")
	contains(t, res.Stdout, "Syslog stopped")
	contains(t, res.Stdout, "logging host 10.0.0.5")
}

// Turning off an alert that is on is silent loss of coverage: it must not
// happen without a terminal to confirm on, and --yes must still name it.
func TestSendersAlertOffNeedsConfirmation(t *testing.T) {
	e := sendersEnv(t)
	e.stub.handleMethod("POST", trafficExpectPath, 200, map[string]any{"changed": 1})

	res := e.run("senders", "alert", "core-sw-01", "--syslog", "off")
	if res.ExitCode == 0 {
		t.Fatal("turning off an alert that is on must ask first")
	}
	contains(t, res.Stderr, "Nothing will page anyone")
	if n := len(e.stub.requestsTo("POST", trafficExpectPath)); n != 0 {
		t.Fatalf("nothing may be written before confirmation, got %d writes", n)
	}

	res = e.run("senders", "alert", "core-sw-01", "--syslog", "off", "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("alert --yes failed: %s", res.Stderr)
	}
	contains(t, res.Stderr, "Nothing will page anyone")
	reqs := e.stub.requestsTo("POST", trafficExpectPath)
	if len(reqs) != 1 {
		t.Fatalf("expected one write, got %d", len(reqs))
	}
	b := reqs[0].Body
	if b["host"] != float64(11) || b["infrastructure"] != float64(6) || b["syslog"] != false {
		t.Errorf("unexpected body %v", b)
	}
}

// Off on an alert nobody turned on loses nothing, so it does not prompt.
func TestSendersAlertOffOnUndecidedDoesNotPrompt(t *testing.T) {
	e := sendersEnv(t)
	e.stub.handleMethod("POST", trafficExpectPath, 200, map[string]any{"changed": 1})

	res := e.run("senders", "alert", "11", "--flow", "off", "--flow-quiet", "15m")
	if res.ExitCode != 0 {
		t.Fatalf("alert failed: %s", res.Stderr)
	}
	b := e.stub.requestsTo("POST", trafficExpectPath)[0].Body
	if b["flow"] != false || b["flow_silence_minutes"] != float64(15) {
		t.Errorf("unexpected body %v", b)
	}
}

func TestSendersAlertRefusesAnAddressWithNoHost(t *testing.T) {
	e := sendersEnv(t)
	res := e.run("senders", "alert", "10.20.0.9", "--syslog", "on")
	if res.ExitCode == 0 {
		t.Fatal("an address with no inventory host has nothing to set an alert on")
	}
	contains(t, res.Stderr, "not an inventory host")
}

func TestSendersAlertRejectsBadQuietWindows(t *testing.T) {
	e := sendersEnv(t)
	for _, v := range []string{"30s", "90s", "200h", "soon"} {
		res := e.run("senders", "alert", "core-sw-01", "--syslog-quiet", v)
		if res.ExitCode == 0 {
			t.Errorf("--syslog-quiet %s must be refused", v)
		}
	}
	e.stub.handleMethod("POST", trafficExpectPath, 200, map[string]any{"changed": 1})
	res := e.run("senders", "alert", "core-sw-01", "--syslog-quiet", "default")
	if res.ExitCode != 0 {
		t.Fatalf("default failed: %s", res.Stderr)
	}
	b := e.stub.requestsTo("POST", trafficExpectPath)[0].Body
	if v, ok := b["syslog_silence_minutes"]; !ok || v != nil {
		t.Errorf("default must send null, got %v", b)
	}
}

// Ignore settles offers in bulk, so it names every host before writing.
func TestSendersIgnoreListsHostsAndConfirms(t *testing.T) {
	e := sendersEnv(t)
	e.stub.handleMethod("POST", trafficExpectPath, 200, map[string]any{"changed": 1})

	res := e.run("senders", "ignore")
	if res.ExitCode == 0 {
		t.Fatal("ignore must ask first")
	}
	contains(t, res.Stderr, "core-sw-01 (10.20.0.1): flow")
	if n := len(e.stub.requestsTo("POST", trafficExpectPath)); n != 0 {
		t.Fatalf("nothing may be written before confirmation, got %d", n)
	}

	res = e.run("senders", "ignore", "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("ignore --yes failed: %s", res.Stderr)
	}
	contains(t, res.Stderr, "1 host(s) changed")
	b := e.stub.requestsTo("POST", trafficExpectPath)[0].Body
	infras, _ := b["infrastructures"].([]any)
	if b["seen"] != true || b["ignore"] != true || len(infras) != 1 || infras[0] != float64(6) {
		t.Errorf("unexpected body %v", b)
	}
}
