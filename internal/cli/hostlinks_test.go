package cli

import (
	"encoding/json"
	"testing"

	"github.com/upstacked/cli/internal/errs"
)

func TestHostLinksCreateSendsBothEndpointsAndPorts(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("POST", "/api/host_link/", 201, map[string]any{"id": 55, "name": "Gi1/0/1"})

	res := e.run("host", "links", "create",
		"--from", "12", "--to", "19", "--from-port", "Gi1/0/1", "--to-port", "Gi1/0/24")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("POST", "/api/host_link/")
	if len(got) != 1 {
		t.Fatalf("expected one create, got %d", len(got))
	}
	b := got[0].Body
	if b["source_node"] != float64(12) || b["destination_node"] != float64(19) {
		t.Errorf("wrong endpoints: %v -> %v", b["source_node"], b["destination_node"])
	}
	if b["source_port_name"] != "Gi1/0/1" || b["destination_port_name"] != "Gi1/0/24" {
		t.Errorf("wrong ports: %v", b)
	}
	if b["infrastructure"] != float64(8) {
		t.Errorf("expected the active infrastructure, got %v", b["infrastructure"])
	}
	// The name is what labels the edge on the portal; defaulting it to the
	// source port beats creating an unlabelled link.
	if b["name"] != "Gi1/0/1" {
		t.Errorf("expected the name to default to the source port, got %v", b["name"])
	}
}

// The server stamps the infrastructure's current topology revision. A link
// carrying any other value exists in the database but is never drawn, so
// sending one from here would produce a create that looks like it did nothing.
func TestHostLinksCreateNeverSendsARevisionNumber(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("POST", "/api/host_link/", 201, map[string]any{"id": 55})

	if res := e.run("host", "links", "create", "--from", "1", "--to", "2", "--name", "x"); res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	b := e.stub.requestsTo("POST", "/api/host_link/")[0].Body
	if _, ok := b["revison_number"]; ok {
		t.Errorf("revision must be left to the server, body was %v", b)
	}
}

func TestHostLinksCreateDefaultsToLayerOne(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("POST", "/api/host_link/", 201, map[string]any{"id": 1})

	if res := e.run("host", "links", "create", "--from", "1", "--to", "2", "--name", "x"); res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	b := e.stub.requestsTo("POST", "/api/host_link/")[0].Body
	if b["layer_one"] != true || b["layer_two"] != false || b["layer_three"] != false {
		t.Errorf("expected L1 only, got %v", b)
	}
}

func TestHostLinksCreateAcceptsSeveralLayers(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("POST", "/api/host_link/", 201, map[string]any{"id": 1})

	if res := e.run("host", "links", "create",
		"--from", "1", "--to", "2", "--name", "x", "--layer", "L2,3"); res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	b := e.stub.requestsTo("POST", "/api/host_link/")[0].Body
	if b["layer_one"] != false || b["layer_two"] != true || b["layer_three"] != true {
		t.Errorf("expected L2 and L3, got %v", b)
	}
}

// The API validates none of this, so a typo would otherwise create a link on
// no layer at all: stored, and drawn nowhere.
func TestHostLinksCreateRejectsAnUnknownLayer(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")

	res := e.run("host", "links", "create", "--from", "1", "--to", "2", "--name", "x", "--layer", "7")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected a usage error, got %d: %s", res.ExitCode, res.Stderr)
	}
	if len(e.stub.requestsTo("POST", "/api/host_link/")) != 0 {
		t.Error("nothing should have been sent")
	}
}

func TestHostLinksCreateRequiresBothEnds(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")

	res := e.run("host", "links", "create", "--from", "12")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected a usage error, got %d: %s", res.ExitCode, res.Stderr)
	}
	if len(e.stub.requestsTo("POST", "/api/host_link/")) != 0 {
		t.Error("nothing should have been sent")
	}
}

// A self-link is always a mistake, and the API stores it happily.
func TestHostLinksCreateRefusesToLinkAHostToItself(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")

	res := e.run("host", "links", "create", "--from", "12", "--to", "12", "--name", "x")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected a usage error, got %d: %s", res.ExitCode, res.Stderr)
	}
	if len(e.stub.requestsTo("POST", "/api/host_link/")) != 0 {
		t.Error("nothing should have been sent")
	}
}

// Without a port to borrow a name from there is nothing sensible to default to,
// and the API rejects a blank name with a 400 that does not explain itself.
func TestHostLinksCreateRequiresANameWhenThereIsNoSourcePort(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")

	res := e.run("host", "links", "create", "--from", "1", "--to", "2")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected a usage error, got %d: %s", res.ExitCode, res.Stderr)
	}
}

func TestHostLinksListShowsTheRealFields(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("GET", "/api/host_link/", 200, page(map[string]any{
		"id": 55, "name": "uplink", "infrastructure": 8,
		"source_node": 12, "destination_node": 19,
		"source_port_name": "Gi1/0/1", "destination_port_name": "Gi1/0/24",
		"layer_one": true, "layer_two": true, "layer_three": false,
		"revison_number": 3,
	}))

	res := e.run("host", "links")
	if res.ExitCode != 0 {
		t.Fatalf("list failed: %s", res.Stderr)
	}
	for _, want := range []string{"uplink", "12", "19", "Gi1/0/1", "Gi1/0/24", "L1,L2", "3"} {
		contains(t, res.Stdout, want)
	}
}

func TestHostLinksDeleteSendsIDsAndNeedsConfirmation(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("DELETE", "/api/host_link_bulk_delete/", 200, map[string]any{"deleted": 2})

	// Without a terminal and without --yes, a delete must fail rather than run.
	res := e.run("host", "links", "delete", "55", "56")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected a confirmation failure, got %d: %s", res.ExitCode, res.Stderr)
	}
	if len(e.stub.requestsTo("DELETE", "/api/host_link_bulk_delete/")) != 0 {
		t.Fatal("nothing should have been deleted")
	}

	res = e.run("host", "links", "delete", "55", "56", "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("delete failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("DELETE", "/api/host_link_bulk_delete/")
	if len(got) != 1 {
		t.Fatalf("expected one delete, got %d", len(got))
	}
	var body struct {
		IDs []int `json:"ids"`
	}
	if err := json.Unmarshal(got[0].Raw, &body); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if len(body.IDs) != 2 || body.IDs[0] != 55 || body.IDs[1] != 56 {
		t.Errorf("expected ids [55 56], got %v", body.IDs)
	}
}
