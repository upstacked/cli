package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/upstacked/cli/internal/errs"
)

// stubMapping registers one existing mapping and the item it belongs to.
func stubMapping(e *env) {
	e.t.Helper()
	e.stub.handleMethod("GET", mappingsPath+"88/", 200, map[string]any{
		"id": 88, "monitoring_item": 412, "schema": 7, "schema_name": "interface",
		"is_multi_valued": true, "identifier": "$.ifName",
		"field_mappings": []any{
			map[string]any{"key": "in_octets", "path": "$.ifInOctets",
				"filter_rules": []any{map[string]any{"condition": "is_not_equal_to", "value": "0"}}},
			map[string]any{"key": "out_octets", "path": "$.ifOutOctets"},
		},
	})
	e.stub.handleMethod("PATCH", mappingsPath+"88/", 200, map[string]any{"id": 88})
}

func TestMappingCreateSendsKeysPathsAndTheIdentifier(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("POST", mappingsPath, 201, map[string]any{"id": 91})

	res := e.run("monitoring", "item", "mapping", "create",
		"--item", "412", "--schema", "7",
		"--field", "in_octets=$.ifHCInOctets", "--field", "out_octets=$.ifHCOutOctets",
		"--identifier", "$.ifName", "--multi-valued", "--skip-test")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("POST", mappingsPath)
	if len(got) != 1 {
		t.Fatalf("expected one create, got %d", len(got))
	}
	body := got[0].Body
	if body["monitoring_item"] != float64(412) || body["schema"] != float64(7) {
		t.Errorf("item/schema not sent: %v", body)
	}
	if body["identifier"] != "$.ifName" || body["is_multi_valued"] != true {
		t.Errorf("identifier/multi-valued not sent: %v", body)
	}
	fields, _ := body["field_mappings"].([]any)
	if len(fields) != 2 {
		t.Fatalf("expected 2 field mappings, got %v", body["field_mappings"])
	}
	first, _ := fields[0].(map[string]any)
	if first["key"] != "in_octets" || first["path"] != "$.ifHCInOctets" {
		t.Errorf("field not sent as key/path: %v", first)
	}
	// Both are required by the API and refusing a body without them is not a
	// useful failure to hand a caller who never mentioned alert rules.
	if _, ok := body["alert_rule_config"]; !ok {
		t.Error("alert_rule_config must be defaulted")
	}
	if _, ok := body["selected_json_path"]; !ok {
		t.Error("selected_json_path must be defaulted")
	}
}

// A mapping with no fields is a check that publishes nothing, which looks
// exactly like a working one from the outside.
func TestMappingCreateRefusesAnEmptyFieldSet(t *testing.T) {
	e := newEnv(t)
	e.login()

	res := e.run("monitoring", "item", "mapping", "create", "--item", "412", "--schema", "7")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected usage exit %d, got %d: %s", errs.CodeUsage, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "publishes nothing")
	if len(e.stub.requestsTo("POST", mappingsPath)) != 0 {
		t.Error("nothing may be written")
	}
}

func TestMappingCreateWarnsWhenMultiValuedHasNoIdentifier(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("POST", mappingsPath, 201, map[string]any{"id": 91})

	res := e.run("monitoring", "item", "mapping", "create",
		"--item", "412", "--schema", "7", "--field", "in_octets=$.x",
		"--multi-valued", "--skip-test")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	contains(t, res.Stderr, "collapses every row onto one series")
}

// Sending the field set wholesale to change one path is how the other fields
// quietly stop being collected, so --field merges instead.
func TestMappingUpdateMergesByKeyAndKeepsFilterRules(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubMapping(e)

	res := e.run("monitoring", "item", "mapping", "update", "88",
		"--field", "in_octets=$.ifHCInOctets", "--skip-test")
	if res.ExitCode != 0 {
		t.Fatalf("update failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("PATCH", mappingsPath+"88/")
	if len(got) != 1 {
		t.Fatalf("expected one patch, got %d", len(got))
	}
	fields, _ := got[0].Body["field_mappings"].([]any)
	if len(fields) != 2 {
		t.Fatalf("out_octets was dropped by a single-field update: %v", got[0].Body["field_mappings"])
	}
	first, _ := fields[0].(map[string]any)
	if first["path"] != "$.ifHCInOctets" {
		t.Errorf("path not updated: %v", first)
	}
	if _, ok := first["filter_rules"]; !ok {
		t.Error("filter_rules are not expressible as flags, so they must survive a path change")
	}
}

func TestMappingUpdateResendsTheCurrentSchema(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubMapping(e)

	res := e.run("monitoring", "item", "mapping", "update", "88",
		"--field", "in_octets=$.ifHCInOctets", "--skip-test")
	if res.ExitCode != 0 {
		t.Fatalf("update failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("PATCH", mappingsPath+"88/")
	if len(got) != 1 || got[0].Body["schema"] != float64(7) {
		t.Fatalf("the mapping's schema must be sent even without --schema: %v", got)
	}
}

func TestMappingUpdateConfirmsBeforeDroppingAField(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubMapping(e)

	res := e.run("monitoring", "item", "mapping", "update", "88", "--remove-field", "out_octets")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected a refusal to prompt non-interactively, got %d: %s", res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "Stop collecting out_octets")
	if len(e.stub.requestsTo("PATCH", mappingsPath+"88/")) != 0 {
		t.Error("nothing may be written before the loss is confirmed")
	}
}

func TestMappingUpdateReplaceFieldsConfirmsTheLoss(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubMapping(e)

	res := e.run("monitoring", "item", "mapping", "update", "88",
		"--replace-fields", "--field", "in_octets=$.x")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected confirmation, got %d: %s", res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "out_octets")
}

func TestMappingDeleteNamesTheFieldsItStopsCollecting(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubMapping(e)

	res := e.run("monitoring", "item", "mapping", "delete", "88")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected confirmation, got %d: %s", res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "in_octets, out_octets")
	contains(t, res.Stderr, "silently")
}

// The whole point of a mapping is that it may be valid and still resolve to
// nothing, so writing one re-runs the item's dry run.
func TestMappingCreateDryRunsTheItemAfterwards(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("POST", mappingsPath, 201, map[string]any{"id": 91})
	e.stub.handleMethod("POST", dryRunsPath, 201, map[string]any{"id": 17, "status": "pending"})
	e.stub.handleMethod("GET", dryRunsPath+"17/", 200, dryRunRecord("success", nil))

	res := e.run("monitoring", "item", "mapping", "create",
		"--item", "412", "--schema", "7", "--field", "in_octets=$.x")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	if len(e.stub.requestsTo("POST", dryRunsPath)) != 1 {
		t.Error("a new mapping must be dry-run, not assumed to work")
	}
	contains(t, res.Stderr, "Nothing was published")
}

// --from-file carries what the flags cannot: per-field filter rules and value
// mappings.
func TestMappingCreateAcceptsAFullBodyFromFile(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("POST", mappingsPath, 201, map[string]any{"id": 91})

	path := filepath.Join(t.TempDir(), "mapping.json")
	body, _ := json.Marshal(map[string]any{
		"schema": 7,
		"field_mappings": []any{
			map[string]any{"key": "status", "path": "$.ifOperStatus", "value_mapping": 3},
		},
	})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	res := e.run("monitoring", "item", "mapping", "create",
		"--item", "412", "--from-file", path, "--skip-test")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("POST", mappingsPath)[0].Body
	fields, _ := got["field_mappings"].([]any)
	first, _ := fields[0].(map[string]any)
	if first["value_mapping"] != float64(3) {
		t.Errorf("value_mapping from the file was dropped: %v", first)
	}
}

// A multi-valued mapping joins rows across selected_json_path; left empty the
// engine itemizes nothing, so the columns the fields read are sent for it.
func TestMappingCreateDerivesTheRowColumnsOfAMultiValuedMapping(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("POST", mappingsPath, 201, map[string]any{"id": 91})

	name := "item['$.1.3.6.1.2.1.31.1.1.1.1'].value"
	res := e.run("monitoring", "item", "mapping", "create",
		"--item", "412", "--schema", "7", "--multi-valued",
		"--field", "name="+name,
		"--field", "in_octets=item['$.1.3.6.1.2.1.31.1.1.1.6'].value",
		"--identifier", name, "--skip-test")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	got, _ := e.stub.requestsTo("POST", mappingsPath)[0].Body["selected_json_path"].([]any)
	if len(got) != 2 || got[0] != "$.1.3.6.1.2.1.31.1.1.1.1" || got[1] != "$.1.3.6.1.2.1.31.1.1.1.6" {
		t.Errorf("expected both columns, in field order, got %v", got)
	}
}

func TestMappingCreateLeavesASingleValuedMappingAlone(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("POST", mappingsPath, 201, map[string]any{"id": 91})

	res := e.run("monitoring", "item", "mapping", "create",
		"--item", "412", "--schema", "7", "--field", "status=item['$.1.3.6.1.2.1.1.3'].value", "--skip-test")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	if got, _ := e.stub.requestsTo("POST", mappingsPath)[0].Body["selected_json_path"].(map[string]any); len(got) != 0 {
		t.Errorf("a single-valued mapping has no row columns, got %v", got)
	}
}
