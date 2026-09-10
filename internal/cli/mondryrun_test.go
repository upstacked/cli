package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// dryRunRecord builds a completed dry-run record. Overrides are merged in so a
// test only spells out the part it is actually about.
func dryRunRecord(status string, overrides map[string]any) map[string]any {
	rec := map[string]any{
		"id":              17,
		"monitoring_item": 412,
		"host":            88,
		"source":          "api_data",
		"status":          status,
		"error":           nil,
		"trace": map[string]any{
			"id": 412, "name": "Interface status", "host_id": 88,
			"data_schemas": []any{"interface"},
			"request_status": map[string]any{
				"status": "success",
				"details": map[string]any{
					"message": "OK", "status_code": 200,
				},
			},
			"host_mapping_status": map[string]any{
				"status":  "success",
				"details": map[string]any{"message": "Matched sw-01"},
			},
			"schema_mapping_status": map[string]any{
				"status": "success", "success": 4, "total": 4,
				"details": map[string]any{"results": []any{}, "errors": []any{}},
			},
			"alerting_rules": map[string]any{"interface": 1},
		},
		"data_points": []any{
			map[string]any{
				"@timestamp": "2026-09-10T14:22:03.117Z",
				"extra": map[string]any{
					"host_id": 88, "ip": "10.0.0.1",
					"schema_id": 2, "schema_name": "interface", "mapping": "",
					"value": map[string]any{"status": `"up"`, "errors": "0"},
				},
			},
		},
		"alerts": []any{},
	}
	for k, v := range overrides {
		rec[k] = v
	}
	return rec
}

// The point of the command: what the config would have published, without
// publishing it.
func TestDryRunQueuesAndReportsWhatWouldHaveBeenCollected(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("POST", dryRunsPath, 201, map[string]any{"id": 17, "status": "pending"})
	e.stub.handleMethod("GET", dryRunsPath+"17/", 200, dryRunRecord("success", nil))

	res := e.run("monitoring", "item", "dry-run", "412")
	if res.ExitCode != 0 {
		t.Fatalf("dry-run failed: %s\n%s", res.Stderr, res.Stdout)
	}
	got := e.stub.requestsTo("POST", dryRunsPath)
	if len(got) != 1 {
		t.Fatalf("expected one queued run, got %d", len(got))
	}
	if got[0].Body["monitoring_item"] != float64(412) {
		t.Errorf("expected the item id in the body, got %v", got[0].Body)
	}
	contains(t, res.Stderr, "Dry run 17 succeeded")
	contains(t, res.Stderr, "Nothing was published")
	contains(t, res.Stdout, "Data points (1)")
	// Engine values arrive JSON-encoded; a reader should see up, not "\"up\"".
	contains(t, res.Stdout, `status=up`)
	notContains(t, res.Stdout, `status="up"`)
	contains(t, res.Stdout, "10.0.0.1")
}

// A failure has to say which stage failed, or the operator is left guessing at
// a config that is structurally fine.
func TestDryRunNamesTheStageThatFailedAndItsCandidates(t *testing.T) {
	e := newEnv(t)
	e.login()
	rec := dryRunRecord("failed", nil)
	trace := rec["trace"].(map[string]any)
	trace["host_mapping_status"] = map[string]any{
		"status": "failed",
		"details": map[string]any{
			"message":               "No matching item found",
			"identifier_path":       "item.name",
			"operator":              "equals",
			"value":                 "sw-01",
			"candidate_identifiers": []any{"sw-02", "sw-03"},
		},
	}
	trace["schema_mapping_status"] = nil
	rec["data_points"] = []any{}
	e.stub.handleMethod("POST", dryRunsPath, 201, map[string]any{"id": 17, "status": "pending"})
	e.stub.handleMethod("GET", dryRunsPath+"17/", 200, rec)

	res := e.run("monitoring", "item", "dry-run", "412")
	if res.ExitCode == 0 {
		t.Fatal("a dry run that would collect nothing must exit non-zero")
	}
	contains(t, res.Stderr, "failed at host mapping")
	contains(t, res.Stdout, "No matching item found")
	// The useful part: what each candidate actually rendered to.
	contains(t, res.Stdout, "candidates: sw-02, sw-03")
	contains(t, res.Stdout, `identifier item.name equals "sw-01"`)
	contains(t, res.Stdout, "would publish nothing")
	contains(t, res.Stderr, "it failed at host mapping")
	contains(t, res.Stderr, "never alerts")
}

// Every stage succeeding while nothing would be published is still an item that
// never alerts, so it must not read as a pass.
func TestDryRunFailsWhenItWouldPublishNothing(t *testing.T) {
	e := newEnv(t)
	e.login()
	rec := dryRunRecord("success", map[string]any{"data_points": []any{}})
	e.stub.handleMethod("POST", dryRunsPath, 201, map[string]any{"id": 17, "status": "pending"})
	e.stub.handleMethod("GET", dryRunsPath+"17/", 200, rec)

	res := e.run("monitoring", "item", "dry-run", "412")
	if res.ExitCode == 0 {
		t.Fatal("a run that produced no data points must exit non-zero")
	}
	contains(t, res.Stderr, "collected nothing")
	contains(t, res.Stderr, "every stage ran")
}

// A run the agent never picked up is failed with a null trace. Reporting that
// as "this config would publish nothing" blames the config for an outage and
// sends someone to delete a check nobody ever tested. This record is what a
// live server actually returned.
func TestDryRunSeparatesAnAgentOutageFromABadConfig(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("GET", dryRunsPath+"1/", 200, map[string]any{
		"id": 1, "monitoring_item": 26, "host": 1, "source": "icmp",
		"status": "failed", "trace": nil, "data_points": nil, "alerts": nil,
		"error": "The monitoring agent did not report a result in time. " +
			"Check that the agent for this infrastructure is online.",
	})

	res := e.run("monitoring", "item", "dry-run", "show", "1")
	if res.ExitCode == 0 {
		t.Fatal("a check that never ran must not read as a pass")
	}
	contains(t, res.Stderr, "never ran")
	contains(t, res.Stderr, "did not execute")
	contains(t, res.Stderr, "says nothing about the item's config")
	notContains(t, res.Stderr, "collected nothing")
	notContains(t, res.Stdout, "would publish nothing")
}

// A capped preview presented as complete is the same class of failure as a
// truncated list read as "no more matches".
func TestDryRunSaysWhenTheResultWasCapped(t *testing.T) {
	e := newEnv(t)
	e.login()
	rec := dryRunRecord("success", nil)
	trace := rec["trace"].(map[string]any)
	trace["data_points_truncated"] = 512
	trace["request_status"].(map[string]any)["details"].(map[string]any)["response_truncated"] = true
	e.stub.handleMethod("POST", dryRunsPath, 201, map[string]any{"id": 17, "status": "pending"})
	e.stub.handleMethod("GET", dryRunsPath+"17/", 200, rec)

	res := e.run("monitoring", "item", "dry-run", "412")
	if res.ExitCode != 0 {
		t.Fatalf("dry-run failed: %s", res.Stderr)
	}
	contains(t, res.Stderr, "only the first 512 data points")
	contains(t, res.Stderr, "raw response in the trace was truncated")
}

func TestDryRunHostFlagIsSent(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("POST", dryRunsPath, 201, map[string]any{"id": 17, "status": "pending"})
	e.stub.handleMethod("GET", dryRunsPath+"17/", 200, dryRunRecord("success", nil))

	res := e.run("monitoring", "item", "dry-run", "412", "--host", "88")
	if res.ExitCode != 0 {
		t.Fatalf("dry-run failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("POST", dryRunsPath)
	if len(got) != 1 || got[0].Body["host"] != float64(88) {
		t.Errorf("expected --host in the body, got %v", got)
	}
}

// Overrides are the reason a config can be checked before it is committed.
func TestDryRunSendsOverridesFromFile(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("POST", dryRunsPath, 201, map[string]any{"id": 17, "status": "pending"})
	e.stub.handleMethod("GET", dryRunsPath+"17/", 200, dryRunRecord("success", nil))

	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"parameters": {"url": "https://example.test/api"}, "response_root_path": "$.data"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	res := e.run("monitoring", "item", "dry-run", "412", "--from-file", path)
	if res.ExitCode != 0 {
		t.Fatalf("dry-run failed: %s", res.Stderr)
	}
	got := e.stub.requestsTo("POST", dryRunsPath)
	if len(got) != 1 {
		t.Fatalf("expected one queued run, got %d", len(got))
	}
	if got[0].Body["response_root_path"] != "$.data" {
		t.Errorf("override was not sent: %v", got[0].Body)
	}
	params, ok := got[0].Body["parameters"].(map[string]any)
	if !ok || params["url"] != "https://example.test/api" {
		t.Errorf("parameters override was not sent: %v", got[0].Body)
	}
}

// A field the server ignores reads back as "that override was checked" when
// nothing checked it, so it is refused rather than dropped.
func TestDryRunRefusesUnknownOverrideFields(t *testing.T) {
	e := newEnv(t)
	e.login()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"paramters": {}, "name": "typo"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	res := e.run("monitoring", "item", "dry-run", "412", "--from-file", path)
	if res.ExitCode != 2 {
		t.Fatalf("expected a usage error, got %d: %s", res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "name, paramters")
	if got := e.stub.requestsTo("POST", dryRunsPath); len(got) != 0 {
		t.Error("nothing should be queued when the override file is wrong")
	}
}

func TestDryRunWithoutWaitingDoesNotPoll(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("POST", dryRunsPath, 201, map[string]any{"id": 17, "status": "pending"})

	res := e.run("monitoring", "item", "dry-run", "412", "--wait", "0")
	if res.ExitCode != 0 {
		t.Fatalf("dry-run failed: %s", res.Stderr)
	}
	if got := e.stub.requestsTo("GET", dryRunsPath+"17/"); len(got) != 0 {
		t.Error("--wait 0 must return as soon as the run is queued")
	}
	contains(t, res.Stderr, "Dry run 17 queued")
	contains(t, res.Stderr, "ups monitoring item dry-run show 17")
	// The server answers the POST with "pending" too, but nobody was waiting,
	// so the late-agent advice would be wrong here.
	notContains(t, res.Stderr, "Queue a new run")
}

// A 400 that is not about the data source must not be answered with the
// data-source hint: `item test` needs a host just as much as a dry run does.
func TestDryRunDoesNotBlameTheDataSourceForEveryRejection(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("POST", dryRunsPath, 400, map[string]any{
		"detail": "This monitoring item has no host to run against.",
	})

	res := e.run("monitoring", "item", "dry-run", "6")
	if res.ExitCode != 2 {
		t.Fatalf("expected a usage error, got %d: %s", res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "no host to run against")
	notContains(t, res.Stderr, "mapping stages to preview")
	notContains(t, res.Stderr, "ups monitoring item test 6")
}

// A run is handed to an agent exactly once, so "still pending" must not read as
// "retry by looking again".
func TestDryRunStillPendingSaysToQueueANewRun(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("POST", dryRunsPath, 201, map[string]any{"id": 17, "status": "pending"})
	e.stub.handleMethod("GET", dryRunsPath+"17/", 200, map[string]any{"id": 17, "status": "pending"})

	res := e.run("monitoring", "item", "dry-run", "412", "--wait", "1ms")
	if res.ExitCode != 0 {
		t.Fatalf("a pending run is not a failure: %s", res.Stderr)
	}
	contains(t, res.Stderr, "still queued")
	contains(t, res.Stderr, "does not re-dispatch")
	contains(t, res.Stderr, "Queue a new run")
}

func TestDryRunShowReadsBackAQueuedRun(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("GET", dryRunsPath+"17/", 200, dryRunRecord("success", nil))

	res := e.run("monitoring", "item", "dry-run", "show", "17")
	if res.ExitCode != 0 {
		t.Fatalf("dry-run show failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "Data points (1)")
	if got := e.stub.requestsTo("POST", dryRunsPath); len(got) != 0 {
		t.Error("reading a run must never queue another one")
	}
}

// A source with no mapping stages is refused; the message has to name the
// check that still applies rather than leaving a dead end.
func TestDryRunExplainsAnUnsupportedDataSource(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("POST", dryRunsPath, 400, map[string]any{
		"detail": "Dry runs are only supported for api_data, snmpstd and icmp",
	})

	res := e.run("monitoring", "item", "dry-run", "412")
	if res.ExitCode != 2 {
		t.Fatalf("expected a usage error, got %d: %s", res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "api_data, snmpstd and icmp")
	contains(t, res.Stderr, "ups monitoring item test 412")
}

func TestDryRunPartialNamesTheFieldsThatDidNotMap(t *testing.T) {
	e := newEnv(t)
	e.login()
	rec := dryRunRecord("partial", nil)
	rec["trace"].(map[string]any)["schema_mapping_status"] = map[string]any{
		"status": "partial", "success": 3, "total": 4,
		"details": map[string]any{
			"results": []any{},
			"errors": []any{map[string]any{
				"schema": "interface", "field": "errors",
				"expr": "item.err_count", "message": "no such key",
			}},
		},
	}
	e.stub.handleMethod("POST", dryRunsPath, 201, map[string]any{"id": 17, "status": "pending"})
	e.stub.handleMethod("GET", dryRunsPath+"17/", 200, rec)

	res := e.run("monitoring", "item", "dry-run", "412")
	if res.ExitCode != 0 {
		t.Fatalf("a partial run still collected something: %s", res.Stderr)
	}
	contains(t, res.Stderr, "mapped only part")
	contains(t, res.Stdout, "3/4 mapped")
	contains(t, res.Stdout, "interface.errors  item.err_count: no such key")
}

// --dry-run is global and means "show me the request". A command that also
// dry-runs on the server must still send nothing.
func TestDryRunCommandHonoursTheGlobalDryRunFlag(t *testing.T) {
	e := newEnv(t)
	e.login()

	res := e.run("monitoring", "item", "dry-run", "412", "--dry-run")
	if res.ExitCode != 0 {
		t.Fatalf("unexpected failure: %s", res.Stderr)
	}
	if got := e.stub.requestsTo("POST", dryRunsPath); len(got) != 0 {
		t.Error("--dry-run must not queue a run")
	}
	contains(t, res.Stderr, "POST "+dryRunsPath)
}

// Config status is derived from dry runs now, so it is the field that says
// whether anything has confirmed the item collects data.
func TestMonitoringItemShowsConfigStatus(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.stub.handleMethod("GET", "/api/monitoring/items/412/", 200, map[string]any{
		"id": 412, "name": "Interface status",
		"monitoring_item_config_status": "INCOMPLETE",
	})

	res := e.run("monitoring", "item", "show", "412")
	if res.ExitCode != 0 {
		t.Fatalf("show failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "Config status")
	contains(t, res.Stdout, "INCOMPLETE")
}
