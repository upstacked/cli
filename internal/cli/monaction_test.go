package cli

import (
	"testing"

	"github.com/upstacked/cli/internal/errs"
)

// stubActions registers a catalogue where one type offers two actions, which
// is the case the resolver must not resolve on its own.
func stubActions(e *env) {
	e.t.Helper()
	e.stub.handleMethod("GET", actionsPath, 200, page(
		map[string]any{"id": 1, "type": "snmp", "name": "get"},
		map[string]any{"id": 2, "type": "snmp", "name": "walk"},
		map[string]any{"id": 3, "type": "icmp", "name": "ping_host"},
		map[string]any{"id": 4, "type": "api_data", "name": "host_data"},
	))
}

// An item with no data source does not know which protocol to speak, so it
// polls nothing - and nothing reports that later.
func TestItemCreateRefusesWithoutADataSource(t *testing.T) {
	e := newEnv(t)
	e.login()

	res := e.run("monitoring", "item", "create", "--host", "7", "--name", "CPU", "--module", "3")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected usage exit %d, got %d: %s", errs.CodeUsage, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "ups monitoring action list")
	if len(e.stub.requestsTo("POST", "/api/monitoring/items/")) != 0 {
		t.Error("nothing may be written without a data source")
	}
}

func TestDataSourceResolvesATypeAndNamePair(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.org("3")
	stubActions(e)
	e.stub.handleMethod("POST", "/api/monitoring/items/", 201, map[string]any{"id": 55})

	res := e.run("monitoring", "item", "create", "--host", "7", "--name", "CPU",
		"--module", "3", "--data-source", "snmp:walk", "--skip-test")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("POST", "/api/monitoring/items/")
	if len(got) != 1 {
		t.Fatalf("expected one create, got %d", len(got))
	}
	if got[0].Body["action_type"] != float64(2) {
		t.Errorf("expected action_type 2 (snmp:walk), got %v", got[0].Body["action_type"])
	}
}

// SNMP offers both get and walk and they are not interchangeable. Picking one
// would produce a check that polls the wrong way and still reports as created.
func TestDataSourceRefusesAnAmbiguousType(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubActions(e)

	res := e.run("monitoring", "item", "create", "--host", "7", "--name", "CPU",
		"--module", "3", "--data-source", "snmp", "--skip-test")
	if res.ExitCode != errs.CodeConflict {
		t.Fatalf("expected conflict exit %d, got %d: %s", errs.CodeConflict, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "snmp:get")
	contains(t, res.Stderr, "snmp:walk")
	if len(e.stub.requestsTo("POST", "/api/monitoring/items/")) != 0 {
		t.Error("nothing may be written while the source is ambiguous")
	}
}

func TestDataSourceAcceptsAnUnambiguousType(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.org("3")
	stubActions(e)
	e.stub.handleMethod("POST", "/api/monitoring/items/", 201, map[string]any{"id": 55})

	res := e.run("monitoring", "item", "create", "--host", "7", "--name", "ping",
		"--data-source", "icmp", "--skip-test")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	if got := e.stub.requestsTo("POST", "/api/monitoring/items/"); got[0].Body["action_type"] != float64(3) {
		t.Errorf("expected action_type 3 (icmp), got %v", got[0].Body["action_type"])
	}
}

// A numeric id is taken as given: it costs a round trip to confirm and the
// server rejects a bad one anyway.
func TestDataSourceAcceptsAnIdWithoutLookup(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.org("3")
	e.stub.handleMethod("POST", "/api/monitoring/items/", 201, map[string]any{"id": 55})

	res := e.run("monitoring", "item", "create", "--host", "7", "--name", "CPU",
		"--data-source", "4", "--skip-test")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	if len(e.stub.requestsTo("GET", actionsPath)) != 0 {
		t.Error("an id needs no catalogue lookup")
	}
}

func TestDataSourceNamesTheCommandThatListsThem(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubActions(e)

	res := e.run("monitoring", "item", "create", "--host", "7", "--name", "CPU",
		"--data-source", "carrier-pigeon", "--skip-test")
	if res.ExitCode != errs.CodeNotFound {
		t.Fatalf("expected not-found exit %d, got %d: %s", errs.CodeNotFound, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "ups monitoring action list")
}

func TestItemUpdateCanChangeTheDataSource(t *testing.T) {
	e := newEnv(t)
	e.login()
	stubActions(e)
	e.stub.handleMethod("PATCH", "/api/monitoring/items/412/", 200, map[string]any{"id": 412})

	res := e.run("monitoring", "item", "update", "412", "--data-source", "api_data", "--skip-test")
	if res.ExitCode != 0 {
		t.Fatalf("update failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("PATCH", "/api/monitoring/items/412/")
	if got[0].Body["action_type"] != float64(4) {
		t.Errorf("expected action_type 4, got %v", got[0].Body["action_type"])
	}
}

func TestSchemaCreateSendsTypedFieldsAndTheIdentifier(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.org("3")
	e.stub.handleMethod("POST", schemasPath, 201, map[string]any{"id": 9, "name": "interface"})

	res := e.run("monitoring", "schema", "create", "--name", "interface",
		"--field", "if_name:STRING", "--field", "in_octets:INTEGER",
		"--identifier", "if_name")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("POST", schemasPath)
	if len(got) != 1 {
		t.Fatalf("expected one create, got %d", len(got))
	}
	fields, _ := got[0].Body["fields"].([]any)
	if len(fields) != 2 {
		t.Fatalf("expected 2 fields, got %v", got[0].Body["fields"])
	}
	first, _ := fields[0].(map[string]any)
	if first["key"] != "if_name" || first["of_type"] != "STRING" || first["is_identifier"] != true {
		t.Errorf("identifier field not sent correctly: %v", first)
	}
	second, _ := fields[1].(map[string]any)
	if second["is_identifier"] != false {
		t.Errorf("only the named key is the identifier: %v", second)
	}
}

func TestSchemaCreateRejectsAnUnknownFieldType(t *testing.T) {
	e := newEnv(t)
	e.login()

	res := e.run("monitoring", "schema", "create", "--name", "x", "--field", "k:DECIMAL")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected usage exit %d, got %d: %s", errs.CodeUsage, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "STRING, INTEGER, FLOAT, BOOLEAN")
}

// An identifier that is not one of the fields would silently not be set.
func TestSchemaCreateRejectsAnIdentifierThatIsNotAField(t *testing.T) {
	e := newEnv(t)
	e.login()

	res := e.run("monitoring", "schema", "create", "--name", "x",
		"--field", "a:STRING", "--identifier", "b")
	if res.ExitCode != errs.CodeUsage {
		t.Fatalf("expected usage exit %d, got %d: %s", errs.CodeUsage, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "not one of the fields")
}

func TestSchemaCreateWarnsWhenThereIsNoIdentifier(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.org("3")
	e.stub.handleMethod("POST", schemasPath, 201, map[string]any{"id": 9})

	res := e.run("monitoring", "schema", "create", "--name", "x", "--field", "a:STRING")
	if res.ExitCode != 0 {
		t.Fatalf("create failed: %s", res.Stderr)
	}
	contains(t, res.Stderr, "collapses every row onto one series")
}

func TestSchemaAddKeyPostsToTheKeyEndpoint(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("GET", schemasPath+"7/", 200, map[string]any{
		"id": 7, "name": "interface", "organization": 3, "fields": []any{},
	})
	e.stub.handleMethod("POST", schemasPath+"7/create-data-schema-key/", 201, map[string]any{"id": 7})

	res := e.run("monitoring", "schema", "add-key", "7", "--field", "errors:INTEGER")
	if res.ExitCode != 0 {
		t.Fatalf("add-key failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("POST", schemasPath+"7/create-data-schema-key/")
	if len(got) != 1 {
		t.Fatalf("expected one post, got %d", len(got))
	}
	fields, _ := got[0].Body["fields"].([]any)
	first, _ := fields[0].(map[string]any)
	if first["key"] != "errors" || first["of_type"] != "INTEGER" {
		t.Errorf("field not sent: %v", first)
	}
}
