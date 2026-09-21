package cli

import (
	"encoding/json"
	"testing"

	"github.com/upstacked/cli/internal/errs"
)

const (
	ifName       = "1.3.6.1.2.1.31.1.1.1.1"
	ifHCInOctets = "1.3.6.1.2.1.31.1.1.1.6"
)

// nested builds the engine's walk response: one OID arc per level.
func nested(values map[string]string) map[string]any {
	root := map[string]any{}
	for oid, v := range values {
		node := root
		arcs := splitArcs(oid)
		for _, a := range arcs[:len(arcs)-1] {
			next, ok := node[a].(map[string]any)
			if !ok {
				next = map[string]any{}
				node[a] = next
			}
			node = next
		}
		node[arcs[len(arcs)-1]] = v
	}
	return root
}

func splitArcs(oid string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(oid); i++ {
		if i == len(oid) || oid[i] == '.' {
			out = append(out, oid[start:i])
			start = i + 1
		}
	}
	return out
}

func stubWalk(e *env, status string, response any) {
	e.stub.handleMethod("POST", dryRunsPath, 201, map[string]any{"id": 30, "status": "pending"})
	e.stub.handleMethod("GET", dryRunsPath+"30/", 200, map[string]any{
		"id": 30, "status": "success",
		"trace": map[string]any{
			"request_status": map[string]any{
				"status":  status,
				"details": map[string]any{"message": "SNMP error: timeout", "response": response},
			},
		},
	})
}

func TestHostWalkProbesTheDeviceWithoutAnItem(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubWalk(e, "success", nested(map[string]string{
		ifName + ".2":        "Nu0",
		ifName + ".10":       "Gi1/0/3",
		ifHCInOctets + ".10": "12891219",
	}))

	res := e.run("host", "walk", "205", ifName, ifHCInOctets, "--credential", "1")
	if res.ExitCode != 0 {
		t.Fatalf("walk failed: %s", res.Stderr)
	}

	body := e.stub.requestsTo("POST", dryRunsPath)[0].Body
	if _, ok := body["monitoring_item"]; ok {
		t.Error("a walk has no item behind it")
	}
	if body["data_source"] != float64(2) || body["host"] != float64(205) || body["credential"] != float64(1) {
		t.Errorf("probe not addressed to SNMP on host 205 with credential 1: %v", body)
	}
	oids, _ := body["parameters"].(map[string]any)["oid"].([]any)
	if len(oids) != 2 || oids[0] != ifName {
		t.Errorf("the SNMP pipeline reads parameters.oid, got %v", body["parameters"])
	}

	// Numeric order, not string order: index 2 before 10.
	contains(t, res.Stdout, "Nu0")
	if idx2, idx10 := indexOf(res.Stdout, "Nu0"), indexOf(res.Stdout, "Gi1/0/3"); idx2 > idx10 {
		t.Error("rows must be in numeric index order")
	}
	contains(t, res.Stderr, `"oid":"`+ifName+`,`+ifHCInOctets+`"`)
	contains(t, res.Stderr, "item['$."+ifName+"'].value")
}

func TestHostWalkJSONCarriesTheMappingPath(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubWalk(e, "success", nested(map[string]string{ifName + ".10": "Gi1/0/3"}))

	res := e.run("host", "walk", "205", ifName, "--credential", "1", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("walk failed: %s", res.Stderr)
	}
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, res.Stdout)
	}
	rows := out.Items
	if len(rows) != 1 || rows[0]["index"] != "10" || rows[0]["path"] != "item['$."+ifName+"'].value" {
		t.Errorf("unexpected row: %v", rows)
	}
}

// A device that refuses the walk is not "the device has no such rows".
func TestHostWalkSaysWhenTheDeviceDidNotAnswer(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubWalk(e, "failed", nil)

	res := e.run("host", "walk", "205", ifName, "--credential", "1")
	if res.ExitCode == 0 {
		t.Fatal("a failed walk must fail the command")
	}
	contains(t, res.Stderr, "did not answer")
	contains(t, res.Stderr, "SNMP error: timeout")
}

func TestHostWalkNeedsACredential(t *testing.T) {
	e := newEnv(t)
	e.login()

	res := e.run("host", "walk", "205", ifName)
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected usage exit, got %d: %s", res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "ups credential list")
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
