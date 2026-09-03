package cli

import (
	"testing"

	"github.com/upstacked/cli/internal/errs"
)

// stubTemplate registers the template endpoints every apply test needs.
func stubTemplate(e *env, status string) {
	e.stub.handleMethod("GET", "/api/monitoring/templates/7/", 200, map[string]any{
		"id": 7, "name": "Cisco IOS switch", "published_status": status,
		"organization":       3,
		"monitoring_modules": []any{map[string]any{"id": 3, "name": "uptime"}},
	})
}

// Applying a template deletes the host's existing items before creating the
// template's. That loss is invisible afterwards, so it must be visible before.
func TestTemplateApplyListsWhatItWillRemoveAndConfirms(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	stubTemplate(e, "published")
	e.stub.handleMethod("GET", "/api/monitoring/templates/infra-system-credential/", 200, map[string]any{})
	e.stub.handleMethod("GET", "/api/monitoring/items/", 200, page(
		map[string]any{"id": 31, "name": "ping"},
		map[string]any{"id": 32, "name": "hand-added interface check"},
	))

	res := e.run("monitoring", "template", "apply", "7", "--host", "12")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected a refusal to prompt non-interactively, got %d: %s", res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "hand-added interface check")
	contains(t, res.Stderr, "removes 2 existing monitoring item(s)")
	if got := e.stub.requestsTo("PATCH", "/api/host/12/"); len(got) != 0 {
		t.Error("nothing may be written before the removal is confirmed")
	}
}

func TestTemplateApplyWritesEveryHostOnceConfirmed(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	stubTemplate(e, "published")
	e.stub.handleMethod("GET", "/api/monitoring/templates/infra-system-credential/", 200, map[string]any{})
	e.stub.handleMethod("GET", "/api/monitoring/items/", 200, page())
	e.stub.handleMethod("PATCH", "/api/host/12/", 200, map[string]any{"id": 12})
	e.stub.handleMethod("PATCH", "/api/host/14/", 200, map[string]any{"id": 14})

	res := e.run("monitoring", "template", "apply", "7", "--host", "12,14", "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("apply failed: %s", res.Stderr)
	}
	for _, h := range []string{"/api/host/12/", "/api/host/14/"} {
		got := e.stub.requestsTo("PATCH", h)
		if len(got) != 1 {
			t.Fatalf("expected one PATCH to %s, got %d", h, len(got))
		}
		if got[0].Body["monitoring_template"] != float64(7) {
			t.Errorf("%s: expected monitoring_template 7, got %v", h, got[0].Body)
		}
	}
}

// A check with no credential collects nothing and never alerts, so a template
// missing one must not reach a host by accident.
func TestTemplateApplyRefusesWhenCredentialsAreMissing(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	stubTemplate(e, "published")
	e.stub.handleMethod("GET", "/api/monitoring/templates/infra-system-credential/", 200, map[string]any{
		"42": []any{map[string]any{"name": "core-snmp", "tag": "ro", "scope": "system", "type": "credential_snmpv2"}},
	})

	res := e.run("monitoring", "template", "apply", "7", "--host", "12", "--yes")
	if res.ExitCode != errs.CodeConflict {
		t.Fatalf("expected conflict exit %d, got %d: %s", errs.CodeConflict, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "core-snmp")
	if got := e.stub.requestsTo("PATCH", "/api/host/12/"); len(got) != 0 {
		t.Error("a template missing credentials must not be applied")
	}
}

func TestTemplatePreflightReportsReadiness(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	e.stub.handleMethod("GET", "/api/monitoring/templates/infra-system-credential/", 200, map[string]any{})

	res := e.run("monitoring", "template", "preflight", "7")
	if res.ExitCode != 0 {
		t.Fatalf("preflight failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "every credential it needs")
}

// The preflight endpoint answers for every infrastructure the caller can see.
// Only the pinned one is relevant to the apply in front of them.
func TestTemplatePreflightIgnoresOtherInfrastructures(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("42")
	e.stub.handleMethod("GET", "/api/monitoring/templates/infra-system-credential/", 200, map[string]any{
		"99": []any{map[string]any{"name": "someone-elses-snmp", "tag": "ro"}},
	})

	res := e.run("monitoring", "template", "preflight", "7")
	if res.ExitCode != 0 {
		t.Fatalf("preflight failed: %s", res.Stderr)
	}
	notContains(t, res.Stdout, "someone-elses-snmp")
}

func TestTemplateCreateInfersASingleOrganization(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("GET", "/api/user/details/v2/", 200, map[string]any{
		"user":          map[string]any{"username": "tester"},
		"organizations": map[string]any{"3": map[string]any{"id": 3, "name": "Acme"}},
	})
	e.stub.handleMethod("POST", "/api/monitoring/templates/", 201, map[string]any{"id": 7, "name": "PLC"})

	res := e.run("monitoring", "template", "create", "--name", "PLC", "--module", "2,3")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("POST", "/api/monitoring/templates/")
	if len(got) != 1 {
		t.Fatalf("expected one create, got %d", len(got))
	}
	if got[0].Body["organization"] != float64(3) {
		t.Errorf("expected the sole organization to be inferred, got %v", got[0].Body["organization"])
	}
	mods, _ := got[0].Body["monitoring_modules"].([]any)
	if len(mods) != 2 {
		t.Errorf("expected 2 modules, got %v", got[0].Body["monitoring_modules"])
	}
}

// Guessing between organizations would file the template under the wrong one,
// which is not something the operator would notice.
func TestTemplateCreateRefusesToGuessAmongOrganizations(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("GET", "/api/user/details/v2/", 200, map[string]any{
		"organizations": map[string]any{
			"3": map[string]any{"id": 3, "name": "Acme"},
			"4": map[string]any{"id": 4, "name": "Globex"},
		},
	})

	res := e.run("monitoring", "template", "create", "--name", "PLC")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected a usage error, got %d: %s", res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "Globex")
	if got := e.stub.requestsTo("POST", "/api/monitoring/templates/"); len(got) != 0 {
		t.Error("no template may be created while the organization is ambiguous")
	}
}

// PATCH replaces the module set rather than merging, so --add-module has to
// send what is already there too.
func TestTemplateAddModuleKeepsExistingModules(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubTemplate(e, "draft")
	e.stub.handleMethod("PATCH", "/api/monitoring/templates/7/", 200, map[string]any{"id": 7})

	res := e.run("monitoring", "template", "update", "7", "--add-module", "9")
	if res.ExitCode != 0 {
		t.Fatalf("update failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("PATCH", "/api/monitoring/templates/7/")
	if len(got) != 1 {
		t.Fatalf("expected one update, got %d", len(got))
	}
	mods, _ := got[0].Body["monitoring_modules"].([]any)
	if len(mods) != 2 {
		t.Fatalf("expected the existing module to survive the add, got %v", got[0].Body["monitoring_modules"])
	}
}

func TestTemplateSetModuleConflictsWithAddModule(t *testing.T) {
	e := newEnv(t)
	e.login()

	res := e.run("monitoring", "template", "update", "7", "--module", "3", "--add-module", "9")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected a usage error, got %d: %s", res.ExitCode, res.Stderr)
	}
}

// A template item is bound to its template through its module. An item whose
// module is outside the set is created and then never applied, which reads as
// working right up until it matters.
func TestTemplateItemRequiresAModuleInTheTemplate(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubTemplate(e, "draft")

	res := e.run("monitoring", "item", "create", "--template", "7", "--name", "CPU", "--module", "9")
	if res.ExitCode != errs.CodeConflict {
		t.Fatalf("expected conflict exit %d, got %d: %s", errs.CodeConflict, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "--add-module 9")
	if got := e.stub.requestsTo("POST", "/api/monitoring/items/"); len(got) != 0 {
		t.Error("an item outside the template's module set must not be created")
	}
}

func TestTemplateItemIsCreatedWithoutAHostAndIsNotTested(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubTemplate(e, "draft")
	e.stub.handleMethod("GET", "/api/monitoring/templates/", 200, page(
		map[string]any{"id": 7, "name": "Cisco IOS switch",
			"monitoring_modules": []any{map[string]any{"id": 3, "name": "uptime"}}},
	))
	e.stub.handleMethod("POST", "/api/monitoring/items/", 201, map[string]any{"id": 80, "name": "uptime"})

	res := e.run("monitoring", "item", "create", "--template", "7", "--name", "uptime", "--module", "3")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("POST", "/api/monitoring/items/")
	if len(got) != 1 {
		t.Fatalf("expected one create, got %d", len(got))
	}
	if _, ok := got[0].Body["host"]; ok {
		t.Error("a template item must be created without a host")
	}
	if got[0].Body["organization"] != float64(3) {
		t.Errorf("expected the template's organization, got %v", got[0].Body["organization"])
	}
	if reqs := e.stub.requestsTo("GET", "/api/monitoring/item/80/test"); len(reqs) != 0 {
		t.Error("a host-less item cannot be tested; it must not be attempted")
	}
	contains(t, res.Stderr, "cannot be tested until it is applied")
}

// Templates share items through their modules: an item added to one template
// silently joins every other template holding that module.
func TestTemplateItemWarnsWhenTheModuleIsSharedWithAnotherTemplate(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubTemplate(e, "draft")
	e.stub.handleMethod("GET", "/api/monitoring/templates/", 200, page(
		map[string]any{"id": 7, "name": "Cisco IOS switch",
			"monitoring_modules": []any{map[string]any{"id": 3, "name": "uptime"}}},
		map[string]any{"id": 8, "name": "PLC",
			"monitoring_modules": []any{map[string]any{"id": 3, "name": "uptime"}}},
	))

	res := e.run("monitoring", "item", "create", "--template", "7", "--name", "uptime", "--module", "3")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected the shared-module warning to require confirmation, got %d: %s", res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "PLC (8)")
	if got := e.stub.requestsTo("POST", "/api/monitoring/items/"); len(got) != 0 {
		t.Error("the item must not be created before the shared module is acknowledged")
	}
}

func TestItemCreateRejectsBothHostAndTemplate(t *testing.T) {
	e := newEnv(t)
	e.login()

	res := e.run("monitoring", "item", "create", "--host", "12", "--template", "7", "--name", "CPU")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected a usage error, got %d: %s", res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "exactly one of --host or --template")
}

func TestTemplateDeleteExplainsThatHostsKeepTheirItems(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubTemplate(e, "published")

	res := e.run("monitoring", "template", "delete", "7")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected a refusal to prompt non-interactively, got %d: %s", res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "Hosts keep the items it already created")
}
