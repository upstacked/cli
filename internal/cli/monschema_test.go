package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/upstacked/cli/internal/errs"
)

func TestSchemaShowListsTheKeysAndMarksTheIdentifier(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("GET", schemasPath+"7/", 200, map[string]any{
		"id": 7, "name": "interface", "organization": 3,
		"fields": []any{
			map[string]any{"id": 1, "key": "if_name", "of_type": "string",
				"display_name": "Interface", "is_identifier": true},
			map[string]any{"id": 2, "key": "in_octets", "of_type": "integer",
				"display_name": "In octets", "is_identifier": false},
		},
	})

	res := e.run("monitoring", "schema", "show", "7")
	if res.ExitCode != 0 {
		t.Fatalf("show failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "in_octets")
	// Which field tells rows apart is the decision the mapping hangs on, so it
	// has to be visible rather than inferred from the field list.
	contains(t, res.Stdout, "if_name")
	contains(t, res.Stdout, "IDENTIFIER")
	contains(t, res.Stdout, "yes")
}

func TestSchemaHostReadsWhatADeviceAlreadyPublishes(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("GET", "/api/monitoring-metrics/host/12/data-schema/", 200, page(
		map[string]any{"id": 7, "name": "interface", "type": "snmpstd",
			"identifier_display_name": "Interface", "fields": []any{}},
	))

	res := e.run("monitoring", "schema", "host", "12")
	if res.ExitCode != 0 {
		t.Fatalf("host failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "interface")
	contains(t, res.Stdout, "snmpstd")
}

func TestModuleCreateSuggestsAddingItToATemplate(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.org("3")
	e.stub.handleMethod("POST", "/api/monitoring/modules/", 201, map[string]any{"id": 21, "name": "Cisco CPU"})

	res := e.run("monitoring", "module", "create", "--name", "Cisco CPU")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("POST", "/api/monitoring/modules/")
	if len(got) != 1 {
		t.Fatalf("expected one create, got %d", len(got))
	}
	if got[0].Body["organization"] != float64(3) {
		t.Errorf("organization must be sent: %v", got[0].Body)
	}
	// A module nothing holds is never applied, so the next step is the point.
	contains(t, res.Stderr, "--add-module 21")
}

func TestItemUpdateSavesTheConfigADryRunProved(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("PATCH", "/api/monitoring/items/412/", 200, map[string]any{"id": 412})

	path := filepath.Join(t.TempDir(), "config.json")
	body, _ := json.Marshal(map[string]any{
		"parameters":         `{"oids":["1.3.6.1.4.1.9.9.109.1.1.1.1.8"]}`,
		"response_root_path": "$.data",
	})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	res := e.run("monitoring", "item", "update", "412", "--from-file", path, "--skip-test")
	if res.ExitCode != 0 {
		t.Fatalf("update failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("PATCH", "/api/monitoring/items/412/")
	if len(got) != 1 {
		t.Fatalf("expected one patch, got %d", len(got))
	}
	if got[0].Body["response_root_path"] != "$.data" {
		t.Errorf("config was not saved: %v", got[0].Body)
	}
	// The saved config is no longer the one a dry run confirmed.
	contains(t, res.Stderr, "INCOMPLETE")
}

// A key the command will not write must fail rather than be dropped: a dropped
// field reads back as saved when nothing saved it.
func TestItemUpdateRefusesFieldsItWillNotWrite(t *testing.T) {
	e := newEnv(t)
	e.login()

	path := filepath.Join(t.TempDir(), "config.json")
	body, _ := json.Marshal(map[string]any{
		"parameters": "{}", "schema_mapping": []any{}, "nonsense": 1,
	})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	res := e.run("monitoring", "item", "update", "412", "--from-file", path)
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected usage exit %d, got %d: %s", errs.CodeUsage, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "nonsense")
	contains(t, res.Stderr, "schema_mapping")
	contains(t, res.Stderr, "ups monitoring item mapping")
	if len(e.stub.requestsTo("PATCH", "/api/monitoring/items/412/")) != 0 {
		t.Error("nothing may be written")
	}
}

func TestItemUpdateDryRunsAfterSaving(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("PATCH", "/api/monitoring/items/412/", 200, map[string]any{"id": 412})
	e.stub.handleMethod("POST", dryRunsPath, 201, map[string]any{"id": 17, "status": "pending"})
	e.stub.handleMethod("GET", dryRunsPath+"17/", 200, dryRunRecord("success", nil))

	res := e.run("monitoring", "item", "update", "412", "--response-root-path", "$.data")
	if res.ExitCode != 0 {
		t.Fatalf("update failed: %s", res.Stderr)
	}
	if len(e.stub.requestsTo("POST", dryRunsPath)) != 1 {
		t.Error("an edited item must be re-checked, not assumed to still work")
	}
}
