package cli

import "testing"

// A device reached through an SD-WAN controller needs the controller and the
// attributes that identify it there. Without them an item cannot template a URL
// per host, which is the whole point of a controller template.
func TestHostCreateSendsTheControllerSolution(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("POST", "/api/host/", 201, map[string]any{"id": 300, "name": "SE02-RO02"})

	res := e.run("host", "create", "--name", "SE02-RO02",
		"--asset-type", "3", "--serial", "FGL2716MMVG", "--ip", "10.255.46.22",
		"--controller-solution", "6",
		"--controller-attr", "systemIp=10.255.46.22",
		"--controller-attr", "siteId=10461203")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}

	got := e.stub.requestsTo("POST", "/api/host/")
	if len(got) != 1 {
		t.Fatalf("expected one create, got %d", len(got))
	}
	if got[0].Body["controller_solution"] != float64(6) {
		t.Errorf("controller_solution not sent as an id: %v", got[0].Body["controller_solution"])
	}
	attrs, _ := got[0].Body["controller_solution_attributes"].(map[string]any)
	if attrs["systemIp"] != "10.255.46.22" || attrs["siteId"] != "10461203" {
		t.Errorf("attributes not sent whole: %v", attrs)
	}
}

func TestHostUpdateSendsTheControllerSolution(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("PATCH", "/api/host/242/", 200, map[string]any{"id": 242})

	res := e.run("host", "update", "242", "--controller-attr", "systemIp=10.255.46.21")
	if res.ExitCode != 0 {
		t.Fatalf("update failed: %s", res.Stderr)
	}

	got := e.stub.requestsTo("PATCH", "/api/host/242/")
	if len(got) != 1 {
		t.Fatalf("expected one patch, got %d", len(got))
	}
	attrs, _ := got[0].Body["controller_solution_attributes"].(map[string]any)
	if attrs["systemIp"] != "10.255.46.21" {
		t.Errorf("attributes not sent: %v", got[0].Body)
	}
}

// The attributes are stored as one object, so a partial write drops the keys
// left out. Saying so in the error beats a silent half-write.
func TestControllerAttrRejectsSomethingThatIsNotKeyValue(t *testing.T) {
	e := newEnv(t)
	e.login()

	res := e.run("host", "update", "242", "--controller-attr", "systemIp")
	if res.ExitCode == 0 {
		t.Fatal("a malformed attribute must not be sent")
	}
	contains(t, res.Stderr, "key=value")
	if got := e.stub.requestsTo("PATCH", "/api/host/242/"); len(got) != 0 {
		t.Error("nothing should have been written")
	}
}

func TestControllerSolutionMustBeAnId(t *testing.T) {
	e := newEnv(t)
	e.login()

	res := e.run("host", "update", "242", "--controller-solution", "vmanage-718536")
	if res.ExitCode == 0 {
		t.Fatal("a name is not an id and must be refused")
	}
	contains(t, res.Stderr, "id")
}

// An update with only the flags this adds must still be a change, not the
// "nothing to change" refusal.
func TestControllerAttrAloneIsAChange(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("PATCH", "/api/host/242/", 200, map[string]any{"id": 242})

	res := e.run("host", "update", "242", "--controller-attr", "deviceId=10.255.46.21")
	if res.ExitCode != 0 {
		t.Fatalf("refused a real change: %s", res.Stderr)
	}
}

// A device missing what identifies or reaches it is refused before anything is
// written, and every missing flag is named at once: finding them one at a time
// costs a round trip each.
func TestHostCreateNamesEveryMissingRequiredFlag(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")

	res := e.run("host", "create", "--name", "SE02-RO03")

	if res.ExitCode == 0 {
		t.Fatal("an incomplete device must not be created")
	}
	for _, flag := range []string{"--asset-type", "--serial", "--ip"} {
		contains(t, res.Stderr, flag)
	}
	if got := e.stub.requestsTo("POST", "/api/host/"); len(got) != 0 {
		t.Error("nothing should have been written")
	}
}

func TestHostCreateSendsTheAssetTypeAsAnId(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("POST", "/api/host/", 201, map[string]any{"id": 301})

	res := e.run("host", "create", "--name", "SE02-RO04",
		"--asset-type", "3", "--serial", "FGL2716MMVH", "--ip", "10.255.46.24")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}

	got := e.stub.requestsTo("POST", "/api/host/")
	if got[0].Body["asset_type"] != float64(3) {
		t.Errorf("asset_type not sent as an id: %v", got[0].Body["asset_type"])
	}
	if got[0].Body["i_serial"] != "FGL2716MMVH" {
		t.Errorf("serial not sent: %v", got[0].Body)
	}
}
