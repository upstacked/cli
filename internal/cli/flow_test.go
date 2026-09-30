package cli

import (
	"net/url"
	"testing"
)

func conversationsBody(unmatched ...string) map[string]any {
	if unmatched == nil {
		unmatched = []string{}
	}
	return map[string]any{
		"window": "15m",
		"conversations": []any{
			map[string]any{
				"id": "10.10.2.80|10.20.0.45|502|tcp", "client": "10.10.2.80", "server": "10.20.0.45",
				"service_port": 502, "transport": "tcp", "service_name": "modbus",
				"request_bps": 3100, "response_bps": 9800, "asymmetric": false,
				"request_path": []any{
					map[string]any{"host": 4, "in_port": nil, "out_port": "Gi1/0/48", "observed": false, "bps": 3100},
					map[string]any{"host": 1, "in_port": "Gi1/0/2", "out_port": "Te1/1/4", "observed": true, "bps": 3100},
					map[string]any{"host": 2, "in_port": "Te1/1/4", "out_port": "Gi1/0/9", "observed": true, "bps": 3100},
				},
				"response_path": []any{},
			},
		},
		"unmatched_exporters": unmatched,
	}
}

func flowHosts(e *env) {
	e.stub.handleMethod("GET", "/api/host/", 200, page(
		map[string]any{"id": 1, "name": "core-sw-01"},
		map[string]any{"id": 2, "name": "core-sw-02"},
		map[string]any{"id": 4, "name": "acc-sw-02"},
	))
}

func flowQueryOf(t *testing.T, e *env, path string) url.Values {
	t.Helper()
	reqs := e.stub.requestsTo("GET", path)
	if len(reqs) != 1 {
		t.Fatalf("expected one request to %s, got %d", path, len(reqs))
	}
	q, err := url.ParseQuery(reqs[0].Query)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// The topology endpoints scope by "hostgroups"; "infrastructure" is silently
// the wrong parameter there, so it is pinned.
func TestFlowConversationsScopesByHostgroups(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	flowHosts(e)
	e.stub.handleMethod("GET", flowConversationsPath, 200, conversationsBody())

	res := e.run("flow", "conversations", "--window", "1h", "--query", "modbus", "--limit", "20")
	if res.ExitCode != 0 {
		t.Fatalf("flow conversations failed: %s", res.Stderr)
	}
	q := flowQueryOf(t, e, flowConversationsPath)
	for k, want := range map[string]string{"hostgroups": "6", "window": "1h", "q": "modbus", "size": "20"} {
		if q.Get(k) != want {
			t.Errorf("expected %s=%q, got %q", k, want, q.Get(k))
		}
	}
	contains(t, res.Stdout, "10.10.2.80 → 10.20.0.45:502/tcp")
	contains(t, res.Stdout, "9.8 kbps")
	// A device that exports nothing is on the path by inference, and must not
	// read as if it had reported the traffic.
	contains(t, res.Stdout, "(acc-sw-02) > core-sw-01 > core-sw-02")
}

// --through is resolved to ids within the infrastructure, because the
// endpoint filters by id and a name can exist in other customers' networks.
func TestFlowConversationsResolvesThroughNames(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	flowHosts(e)
	e.stub.handleMethod("GET", flowConversationsPath, 200, conversationsBody())

	res := e.run("flow", "conversations", "--through", "core-sw-01,2")
	if res.ExitCode != 0 {
		t.Fatalf("flow conversations failed: %s", res.Stderr)
	}
	if got := flowQueryOf(t, e, flowConversationsPath).Get("through"); got != "1,2" {
		t.Errorf("expected through=1,2, got %q", got)
	}
}

func TestFlowConversationsRejectsOneThroughHost(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")

	res := e.run("flow", "conversations", "--through", "core-sw-01")
	if res.ExitCode == 0 {
		t.Fatal("expected --through with one host to fail")
	}
	contains(t, res.Stderr, "exactly two hosts")
	if n := len(e.stub.requestsTo("GET", flowConversationsPath)); n != 0 {
		t.Errorf("expected no request, got %d", n)
	}
}

// An exporter that matches no host drops out of every path and the answer
// still looks complete. The warning is the only sign, so it survives --json.
func TestFlowWarnsAboutUnmatchedExportersEvenWithJSON(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	flowHosts(e)
	e.stub.handleMethod("GET", flowConversationsPath, 200, conversationsBody("10.99.0.1"))

	res := e.run("flow", "conversations", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("flow conversations failed: %s", res.Stderr)
	}
	contains(t, res.Stderr, "10.99.0.1 matched no host")
	contains(t, res.Stderr, "management IP")
	res.JSON(t)
}

func TestFlowConversationShowsHops(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	flowHosts(e)
	e.stub.handleMethod("GET", flowConversationsPath, 200, conversationsBody())

	res := e.run("flow", "conversation", "10.10.2.80|10.20.0.45|502|tcp")
	if res.ExitCode != 0 {
		t.Fatalf("flow conversation failed: %s", res.Stderr)
	}
	if got := flowQueryOf(t, e, flowConversationsPath).Get("conversation"); got != "10.10.2.80|10.20.0.45|502|tcp" {
		t.Errorf("expected the conversation id to be sent, got %q", got)
	}
	for _, want := range []string{"Request path", "acc-sw-02", "inferred", "Te1/1/4", "exported"} {
		contains(t, res.Stdout, want)
	}
}

func TestFlowConversationNotFound(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	e.stub.handleMethod("GET", flowConversationsPath, 200, map[string]any{
		"window": "15m", "conversations": []any{}, "unmatched_exporters": []any{},
	})

	res := e.run("flow", "conversation", "1.1.1.1|2.2.2.2|53|udp")
	if res.ExitCode == 0 {
		t.Fatal("expected a missing conversation to fail")
	}
	contains(t, res.Stderr, "--window")
}

func TestFlowLinksNamesEachEnd(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	flowHosts(e)
	e.stub.handleMethod("GET", "/api/host_link/", 200, page(
		map[string]any{"id": 7, "source_node": 1, "destination_node": 2,
			"source_port_name": "Te1/1/4", "destination_port_name": "Te1/1/3"},
	))
	e.stub.handleMethod("GET", flowLinksPath, 200, map[string]any{
		"window": "15m",
		"links": []any{map[string]any{
			"link": 7, "source_to_destination_bps": 2.3e9, "destination_to_source_bps": 1.9e9,
		}},
		"unmatched_exporters": []any{},
	})

	res := e.run("flow", "links")
	if res.ExitCode != 0 {
		t.Fatalf("flow links failed: %s", res.Stderr)
	}
	if got := flowQueryOf(t, e, flowLinksPath).Get("hostgroups"); got != "6" {
		t.Errorf("expected hostgroups=6, got %q", got)
	}
	for _, want := range []string{"core-sw-01", "Te1/1/4", "core-sw-02", "Te1/1/3", "2.3 Gbps", "1.9 Gbps"} {
		contains(t, res.Stdout, want)
	}
}

// Monitoring traffic is hidden by the server unless asked for, so a request
// that forgets the flag must not send include_monitoring at all.
func TestFlowHidesMonitoringUnlessAsked(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	flowHosts(e)
	e.stub.handleMethod("GET", flowConversationsPath, 200, conversationsBody())

	if res := e.run("flow", "conversations"); res.ExitCode != 0 {
		t.Fatalf("flow conversations failed: %s", res.Stderr)
	}
	if q := flowQueryOf(t, e, flowConversationsPath); q.Has("include_monitoring") {
		t.Errorf("expected no include_monitoring by default, got %q", q.Get("include_monitoring"))
	}
}

func TestFlowConversationsSendsEveryFilter(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	flowHosts(e)
	e.stub.handleMethod("GET", flowConversationsPath, 200, conversationsBody())

	res := e.run("flow", "conversations", "--seen-by", "core-sw-01", "--service", "https (TCP/443)",
		"--host", "10.30.100.15", "--include-monitoring",
		"--start", "2026-09-30T11:00:00Z", "--end", "2026-09-30T11:30:00Z")
	if res.ExitCode != 0 {
		t.Fatalf("flow conversations failed: %s", res.Stderr)
	}
	q := flowQueryOf(t, e, flowConversationsPath)
	for k, want := range map[string]string{
		"seen_by": "1", "service": "https (TCP/443)", "host": "10.30.100.15",
		"include_monitoring": "true", "start": "2026-09-30T11:00:00Z", "end": "2026-09-30T11:30:00Z",
	} {
		if q.Get(k) != want {
			t.Errorf("expected %s=%q, got %q", k, want, q.Get(k))
		}
	}
	// An explicit range replaces the window; sending both reads as if both applied.
	if q.Has("window") {
		t.Errorf("expected no window next to an explicit range, got %q", q.Get("window"))
	}
}

func TestFlowConversationsNamesEachEnd(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	flowHosts(e)
	body := conversationsBody()
	body["names"] = map[string]any{
		"10.10.2.80": map[string]any{"name": "historian", "source": "ipam", "host_id": nil, "org": nil},
		"10.20.0.45": map[string]any{"name": nil, "source": "asn", "host_id": nil, "org": "Example Networks"},
	}
	e.stub.handleMethod("GET", flowConversationsPath, 200, body)

	res := e.run("flow", "conversations")
	if res.ExitCode != 0 {
		t.Fatalf("flow conversations failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "historian (10.10.2.80) → Example Networks · 10.20.0.45:502/tcp")
}

// A path that stops short reads as traffic that went nowhere else; the gap
// and its fix are what tell the caller the answer is incomplete.
func TestFlowConversationExplainsGaps(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	flowHosts(e)
	body := conversationsBody()
	conv := body["conversations"].([]any)[0].(map[string]any)
	conv["request_gaps"] = []any{
		map[string]any{"reason": "no_interface_table", "host": 2, "ip": nil, "if_index": nil},
		map[string]any{"reason": "exporter_unmatched", "host": nil, "ip": "10.99.0.1", "if_index": nil},
	}
	conv["response_gaps"] = []any{
		map[string]any{"reason": "unknown_interface", "host": 1, "ip": nil, "if_index": 42},
	}
	e.stub.handleMethod("GET", flowConversationsPath, 200, body)

	res := e.run("flow", "conversation", "10.10.2.80|10.20.0.45|502|tcp")
	if res.ExitCode != 0 {
		t.Fatalf("flow conversation failed: %s", res.Stderr)
	}
	for _, want := range []string{
		"core-sw-02 has no interface table", "ups host walk 2 ifName ifDescr",
		"flow from 10.99.0.1 matched no host", "management IP",
		"core-sw-01's interface table has no ifIndex 42",
	} {
		contains(t, res.Stdout, want)
	}
}

func TestFlowLinksCanIncludeMonitoring(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	flowHosts(e)
	e.stub.handleMethod("GET", "/api/host_link/", 200, page())
	e.stub.handleMethod("GET", flowLinksPath, 200, map[string]any{
		"window": "15m", "links": []any{}, "unmatched_exporters": []any{},
	})

	if res := e.run("flow", "links", "--include-monitoring"); res.ExitCode != 0 {
		t.Fatalf("flow links failed: %s", res.Stderr)
	}
	if got := flowQueryOf(t, e, flowLinksPath).Get("include_monitoring"); got != "true" {
		t.Errorf("expected include_monitoring=true, got %q", got)
	}
}

func summaryBody() map[string]any {
	return map[string]any{
		"range":  map[string]any{"start": "2026-09-30T11:00:00Z", "end": "2026-09-30T12:00:00Z", "interval": "2m"},
		"totals": map[string]any{"bytes": 498237393, "packets": 812345, "hidden_monitoring_bytes": 262000000},
		"series": map[string]any{"services": []any{}, "buckets": []any{}},
		"services": []any{
			map[string]any{"service": "https (TCP/443)", "bytes": 403324702, "packets": 48304},
		},
		"hosts": []any{
			map[string]any{"ip": "10.30.100.9", "bytes": 400000000, "packets": 50000, "role": "server"},
		},
		"names": map[string]any{
			"10.30.100.9": map[string]any{"name": "Fusion", "source": "host", "host_id": 201, "org": nil},
		},
		"unmatched_exporters": []any{"10.99.0.1"},
	}
}

func TestFlowSummary(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("GET", flowSummaryPath, 200, summaryBody())

	res := e.run("flow", "summary")
	if res.ExitCode != 0 {
		t.Fatalf("flow summary failed: %s", res.Stderr)
	}
	q := flowQueryOf(t, e, flowSummaryPath)
	if q.Get("hostgroups") != "8" || q.Get("window") != "1h" {
		t.Errorf("expected hostgroups=8 window=1h, got %v", q)
	}
	// Leaving monitoring out makes a busy network look quiet unless the
	// amount left out is said.
	for _, want := range []string{"498 MB", "262 MB left out", "https (TCP/443)", "403 MB", "Fusion (10.30.100.9)", "server"} {
		contains(t, res.Stdout, want)
	}
	contains(t, res.Stderr, "10.99.0.1 matched no host")
}

func TestFlowSummaryJSONPassesThrough(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("GET", flowSummaryPath, 200, summaryBody())

	res := e.run("flow", "summary", "--include-monitoring", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("flow summary failed: %s", res.Stderr)
	}
	if got := flowQueryOf(t, e, flowSummaryPath).Get("include_monitoring"); got != "true" {
		t.Errorf("expected include_monitoring=true, got %q", got)
	}
	res.JSON(t)
	contains(t, res.Stdout, "hidden_monitoring_bytes")
}
