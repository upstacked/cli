package cli

import "testing"

func TestControllerSolutionCreateSendsItsAttributes(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("POST", controllerSolutionsPath, 201,
		map[string]any{"id": 6, "name": "Cisco SD-WAN"})

	res := e.run("controller-solution", "create", "--name", "Cisco SD-WAN",
		"--required-attr", "controllerIp=vManage address",
		"--required-attr", "systemIp",
		"--attr", "siteId=site the device belongs to")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}

	got := e.stub.requestsTo("POST", controllerSolutionsPath)
	if len(got) != 1 {
		t.Fatalf("expected one create, got %d", len(got))
	}
	attrs, _ := got[0].Body["attributes"].([]any)
	if len(attrs) != 3 {
		t.Fatalf("expected three attributes, got %v", got[0].Body["attributes"])
	}
	first, _ := attrs[0].(map[string]any)
	if first["name"] != "controllerIp" || first["is_required"] != true {
		t.Errorf("required attribute not marked required: %v", first)
	}
	if first["description"] != "vManage address" {
		t.Errorf("description not carried: %v", first)
	}
	last, _ := attrs[2].(map[string]any)
	if last["name"] != "siteId" || last["is_required"] != false {
		t.Errorf("optional attribute marked required: %v", last)
	}
}

// A solution with no attributes cannot identify a device, so an item templating
// one has nothing to put in the URL.
func TestControllerSolutionCreateRefusesNoAttributes(t *testing.T) {
	e := newEnv(t)
	e.login()

	res := e.run("controller-solution", "create", "--name", "Empty")

	if res.ExitCode == 0 {
		t.Fatal("a solution with no attributes must be refused")
	}
	contains(t, res.Stderr, "identify a device")
	if got := e.stub.requestsTo("POST", controllerSolutionsPath); len(got) != 0 {
		t.Error("nothing should have been written")
	}
}

// Whether an attribute is required decides whether every future host must carry
// it, so a name given as both is refused rather than resolved.
func TestControllerSolutionRefusesAnAttributeGivenTwice(t *testing.T) {
	e := newEnv(t)
	e.login()

	res := e.run("controller-solution", "create", "--name", "Cisco SD-WAN",
		"--required-attr", "systemIp", "--attr", "systemIp")

	if res.ExitCode == 0 {
		t.Fatal("a contradictory attribute must be refused")
	}
	contains(t, res.Stderr, "twice")
}

func TestControllerSolutionListShowsTheAttributesAHostMustCarry(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("GET", controllerSolutionsPath, 200, map[string]any{
		"count": 1,
		"results": []any{map[string]any{
			"id": 6, "name": "Cisco SD-WAN", "type": "custom",
			"attributes": []any{
				map[string]any{"identifier": "controllerIp", "is_required": true},
				map[string]any{"identifier": "systemIp", "is_required": true},
			},
		}},
	})

	res := e.run("controller-solution", "list")
	if res.ExitCode != 0 {
		t.Fatalf("list failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "Cisco SD-WAN")
	contains(t, res.Stdout, "controllerIp")
}
