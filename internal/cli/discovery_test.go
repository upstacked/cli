package cli

import (
	"strings"
	"testing"
)

func topologyBody() map[string]any {
	return map[string]any{
		"topology": map[string]any{"id": 6, "name": "OT Lab"},
		"topology_nodes": []any{
			map[string]any{"id": 1, "name": "Firewall01"},
			map[string]any{"id": 2, "name": "PLC01"},
		},
		"topology_links": []any{
			map[string]any{
				"id": 1, "name": "PLC01 - FW01",
				"source_node": 2, "destination_node": 1,
				"source_port_name": "eth0", "destination_port_name": "vtnet1",
				"link_status": "up",
			},
			map[string]any{
				"id": 5, "name": "OT-SENSOR - FW01",
				"source_node": 6, "destination_node": 1,
				"source_port_name": "eth0", "destination_port_name": "vtnet3",
				"link_status": "unknown",
			},
		},
	}
}

// The endpoint scopes by "hostgroups". Sending "infrastructure" gets a 200
// carrying the bare string "Wrong input", which surfaces as a malformed
// response rather than a bad request - so the wrong parameter is invisible
// unless something pins it.
func TestDiscoveryTopologyScopesByHostgroups(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	e.stub.handleMethod("GET", "/api/topology/get/", 200, topologyBody())

	res := e.run("discovery", "topology")
	if res.ExitCode != 0 {
		t.Fatalf("topology failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("GET", "/api/topology/get/")
	if len(got) != 1 {
		t.Fatalf("expected one request, got %d", len(got))
	}
	if !strings.Contains(got[0].Query, "hostgroups=6") {
		t.Errorf("expected hostgroups=6, got query %q", got[0].Query)
	}
	if strings.Contains(got[0].Query, "infrastructure=") {
		t.Errorf("infrastructure= is the wrong parameter here, got %q", got[0].Query)
	}
}

func TestDiscoveryTopologyRendersLinks(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	e.stub.handleMethod("GET", "/api/topology/get/", 200, topologyBody())

	res := e.run("discovery", "topology")
	if res.ExitCode != 0 {
		t.Fatalf("topology failed: %s", res.Stderr)
	}
	for _, want := range []string{"PLC01 - FW01", "Firewall01", "eth0", "vtnet1", "up"} {
		contains(t, res.Stdout, want)
	}
}

// topology_nodes holds only hosts that are in monitoring, so a link can point
// at a host with no name here. Printing the bare number would read as a name.
func TestDiscoveryTopologyMarksUnnamedNodesAsIDs(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	e.stub.handleMethod("GET", "/api/topology/get/", 200, topologyBody())

	res := e.run("discovery", "topology")
	if res.ExitCode != 0 {
		t.Fatalf("topology failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "#6")
}

// Discovery records links only for neighbours it read over SSH, so an empty
// result is common and usually means missing device credentials rather than a
// flat network. Saying so beats printing nothing.
func TestDiscoveryTopologyExplainsAnEmptyResult(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("6")
	e.stub.handleMethod("GET", "/api/topology/get/", 200, map[string]any{
		"topology": map[string]any{"id": 6}, "topology_nodes": []any{}, "topology_links": []any{},
	})

	res := e.run("discovery", "topology")
	if res.ExitCode != 0 {
		t.Fatalf("topology failed: %s", res.Stderr)
	}
	contains(t, res.Stdout+res.Stderr, "SSH")
}
