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
