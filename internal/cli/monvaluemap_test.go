package cli

import (
	"testing"
)

func stubValueMappings(e *env) {
	e.stub.handleMethod("GET", valueMappingsPath, 200, page(
		map[string]any{"id": 1, "name": "ifOperStatus", "organization": 2, "in_use": true,
			"value_mapping_rules": []any{
				map[string]any{"id": 11, "condition": "equals", "value": "1", "mapped_value": "UP", "color": "green"},
				map[string]any{"id": 12, "condition": "equals", "value": "2", "mapped_value": "DOWN", "color": "red"},
			}},
	))
}

func TestValueMapRulesParseIntoTheAPIsConditions(t *testing.T) {
	got, err := parseValueRules([]string{
		"1=UP@green", "range:60-90=WARM@Yellow", "gte:91=HOT@red",
		"lte:0=OFF", "default={{ value // 1000000 }} Mbps", "a@b=c@d",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"condition": "equals", "value": "1", "mapped_value": "UP", "color": "green"},
		{"condition": "in_range", "value": "60-90", "mapped_value": "WARM", "color": "yellow"},
		{"condition": "is_greater_than_or_equals", "value": "91", "mapped_value": "HOT", "color": "red"},
		{"condition": "is_less_than_or_equals", "value": "0", "mapped_value": "OFF", "color": "none"},
		{"condition": "default", "value": "", "mapped_value": "{{ value // 1000000 }} Mbps", "color": "none"},
		// "@d" is not a colour, so it stays part of the label.
		{"condition": "equals", "value": "a@b", "mapped_value": "c@d", "color": "none"},
	}
	for i, w := range want {
		r := got[i].(map[string]any)
		for k, v := range w {
			if r[k] != v {
				t.Errorf("rule %d: %s = %v, want %v", i, k, r[k], v)
			}
		}
	}
}

// The portal compares ranges and thresholds as integers, so a rule it can
// never match is refused rather than saved.
func TestValueMapRulesRefuseWhatThePortalCannotMatch(t *testing.T) {
	for _, spec := range []string{"gte:0.5=X", "range:1-=X", "UP", "=X"} {
		if _, err := parseValueRules([]string{spec}); err == nil {
			t.Errorf("%q should be refused", spec)
		}
	}
}

func TestValueMapCreateSendsRulesInOrder(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.org("2")
	e.stub.handleMethod("GET", valueMappingsPath, 200, page())
	e.stub.handleMethod("POST", valueMappingsPath, 201, map[string]any{"id": 9, "name": "CPU load"})

	res := e.run("monitoring", "value-mapping", "create", "--name", "CPU load",
		"--rule", "range:0-79=OK@green", "--rule", "gte:80=HIGH@red")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("POST", valueMappingsPath)
	rules, _ := got[0].Body["value_mapping_rules"].([]any)
	if len(rules) != 2 || rules[0].(map[string]any)["mapped_value"] != "OK" {
		t.Fatalf("rules not sent in order: %v", got[0].Body)
	}
	if got[0].Body["organization"] != float64(2) {
		t.Errorf("organization must be sent: %v", got[0].Body)
	}
	contains(t, res.Stderr, "--value-mapping <key>=9")
}

// The portal lists value mappings by name, so a second one with the same name
// is one nobody can tell apart from the first.
func TestValueMapCreateRefusesADuplicateName(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.org("2")
	stubValueMappings(e)

	res := e.run("monitoring", "value-mapping", "create", "--name", "ifoperstatus", "--rule", "1=UP")
	if res.ExitCode == 0 {
		t.Fatal("a duplicate name must be refused")
	}
	contains(t, res.Stderr, "already exists")
	if n := len(e.stub.requestsTo("POST", valueMappingsPath)); n != 0 {
		t.Errorf("nothing should be created, got %d POSTs", n)
	}
}

// The API replaces the rule list with whatever it is sent, so a rename must
// resend the current rules or it wipes them.
func TestValueMapRenameKeepsTheRules(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubValueMappings(e)
	e.stub.handleMethod("GET", valueMappingsPath+"1/", 200, map[string]any{
		"id": 1, "name": "ifOperStatus", "in_use": true,
		"value_mapping_rules": []any{
			map[string]any{"id": 11, "condition": "equals", "value": "1", "mapped_value": "UP", "color": "green"},
		}})
	e.stub.handleMethod("PATCH", valueMappingsPath+"1/", 200, map[string]any{"id": 1})

	res := e.run("monitoring", "value-mapping", "update", "ifOperStatus", "--name", "Interface status")
	if res.ExitCode != 0 {
		t.Fatalf("update failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("PATCH", valueMappingsPath+"1/")
	rules, _ := got[0].Body["value_mapping_rules"].([]any)
	if len(rules) != 1 || rules[0].(map[string]any)["mapped_value"] != "UP" {
		t.Fatalf("a rename must resend the rules: %v", got[0].Body)
	}
	if _, ok := rules[0].(map[string]any)["id"]; ok {
		t.Error("rule ids are read-only and must not be sent")
	}
}

func TestMappingUpdateAttachesAValueMappingByName(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubMapping(e)
	stubValueMappings(e)

	res := e.run("monitoring", "item", "mapping", "update", "88",
		"--value-mapping", "in_octets=ifOperStatus", "--skip-test")
	if res.ExitCode != 0 {
		t.Fatalf("update failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("PATCH", mappingsPath+"88/")
	fields, _ := got[0].Body["field_mappings"].([]any)
	if len(fields) != 2 {
		t.Fatalf("attaching a value mapping must keep every field: %v", got[0].Body)
	}
	first := fields[0].(map[string]any)
	if first["key"] != "in_octets" || first["value_mapping"] != float64(1) {
		t.Errorf("value mapping not attached to in_octets: %v", first)
	}
}

func TestMappingUpdateRefusesAValueMappingOnAnUnmappedKey(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubMapping(e)
	stubValueMappings(e)

	res := e.run("monitoring", "item", "mapping", "update", "88",
		"--value-mapping", "oper_status=ifOperStatus", "--skip-test")
	if res.ExitCode == 0 {
		t.Fatal("a key the mapping does not fill must be refused")
	}
	contains(t, res.Stderr, "no field")
	if n := len(e.stub.requestsTo("PATCH", mappingsPath+"88/")); n != 0 {
		t.Errorf("nothing should be sent, got %d", n)
	}
}
