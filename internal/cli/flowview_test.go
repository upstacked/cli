package cli

import (
	"testing"
)

func placesBody() []any {
	return []any{
		map[string]any{"place": "location:42", "kind": "location", "label": "NO00 - Oslo", "detail": "2 subnets, 3 devices"},
		map[string]any{"place": "location:64", "kind": "location", "label": "Old BT Oslo", "detail": "1 subnet"},
	}
}

func betweenSummaryBody(between map[string]any) map[string]any {
	body := map[string]any{
		"range":               map[string]any{"start": "2026-09-30T11:00:00Z", "end": "2026-09-30T12:00:00Z", "interval": "1m"},
		"totals":              map[string]any{"bytes": 46200, "packets": 40, "hidden_monitoring_bytes": 0},
		"series":              map[string]any{"services": []any{}, "buckets": []any{}},
		"services":            []any{},
		"hosts":               []any{},
		"names":               map[string]any{},
		"unmatched_exporters": []any{},
	}
	if between != nil {
		body["between"] = between
	}
	return body
}

// IPs and ranges are sent as cidr: places without a lookup, and the split by
// sender is shown so "between" does not read as one undirected total.
func TestFlowSummaryBetweenSendsPlacesAndShowsTheSplit(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("GET", flowSummaryPath, 200, betweenSummaryBody(map[string]any{
		"a":            map[string]any{"place": "cidr:10.20.10.0/24", "label": "10.20.10.0/24"},
		"b":            map[string]any{"place": "internet", "label": "Internet"},
		"a_to_b_bytes": 1200, "b_to_a_bytes": 45000,
	}))

	res := e.run("flow", "summary", "--between", "10.20.10.0/24,internet")
	if res.ExitCode != 0 {
		t.Fatalf("flow summary failed: %s", res.Stderr)
	}
	q := flowQueryOf(t, e, flowSummaryPath)
	if q.Get("a") != "cidr:10.20.10.0/24" || q.Get("b") != "internet" {
		t.Errorf("expected a=cidr:10.20.10.0/24 b=internet, got a=%q b=%q", q.Get("a"), q.Get("b"))
	}
	if len(e.stub.requestsTo("GET", flowPlacesPath)) != 0 {
		t.Error("an IP range was looked up as a name")
	}
	contains(t, res.Stdout, "10.20.10.0/24 → Internet")
	contains(t, res.Stdout, "1.2 kB")
	contains(t, res.Stdout, "Internet → 10.20.10.0/24")
	contains(t, res.Stdout, "45 kB")
}

func TestFlowBetweenResolvesAPlaceByItsExactName(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("GET", flowPlacesPath, 200, placesBody())
	e.stub.handleMethod("GET", flowConversationsPath, 200, conversationsBody())

	res := e.run("flow", "conversations", "--between", "no00 - oslo")
	if res.ExitCode != 0 {
		t.Fatalf("flow conversations failed: %s", res.Stderr)
	}
	if got := flowQueryOf(t, e, flowConversationsPath).Get("a"); got != "location:42" {
		t.Errorf("expected a=location:42, got %q", got)
	}
}

// Guessing between two places would answer confidently about the wrong
// traffic, so an ambiguous name stops before any flow is read.
func TestFlowBetweenRefusesAnAmbiguousName(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("GET", flowPlacesPath, 200, placesBody())

	res := e.run("flow", "summary", "--between", "Oslo,internet")
	if res.ExitCode == 0 {
		t.Fatal("an ambiguous place was accepted")
	}
	contains(t, res.Stderr, "location:42 (NO00 - Oslo)")
	contains(t, res.Stderr, "location:64 (Old BT Oslo)")
	if len(e.stub.requestsTo("GET", flowSummaryPath)) != 0 {
		t.Error("flow was read for an ambiguous place")
	}
}

func TestFlowBetweenTakesAtMostTwoPlaces(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")

	res := e.run("flow", "summary", "--between", "10.0.0.1,10.0.0.2,internet")
	if res.ExitCode == 0 {
		t.Fatal("three places were accepted")
	}
	contains(t, res.Stderr, "one or two places")
}

func TestFlowPlacesListsPlaceIDs(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("GET", flowPlacesPath, 200, placesBody())

	res := e.run("flow", "places", "--query", "oslo")
	if res.ExitCode != 0 {
		t.Fatalf("flow places failed: %s", res.Stderr)
	}
	q := flowQueryOf(t, e, flowPlacesPath)
	if q.Get("hostgroups") != "8" || q.Get("q") != "oslo" {
		t.Errorf("expected hostgroups=8 q=oslo, got %v", q)
	}
	contains(t, res.Stdout, "location:42")
	contains(t, res.Stdout, "NO00 - Oslo")
}

func savedViews() map[string]any {
	return page(map[string]any{
		"id": 7, "name": "Oslo to internet", "infrastructure": 8, "shared": true, "owner": "simenandre",
		"query": map[string]any{
			"a": "location:42", "b": "internet", "window": "1h", "service": "https (TCP/443)",
		},
	})
}

// A view is a starting point: what it saved applies, and a flag on the
// command line wins over it.
func TestFlowViewAppliesItsQueryUnlessAFlagOverrides(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("GET", flowViewsPath, 200, savedViews())
	e.stub.handleMethod("GET", flowConversationsPath, 200, conversationsBody())

	res := e.run("flow", "conversations", "--view", "Oslo to internet", "--window", "15m")
	if res.ExitCode != 0 {
		t.Fatalf("flow conversations --view failed: %s", res.Stderr)
	}
	q := flowQueryOf(t, e, flowConversationsPath)
	for k, want := range map[string]string{
		"a": "location:42", "b": "internet", "service": "https (TCP/443)", "window": "15m",
	} {
		if q.Get(k) != want {
			t.Errorf("expected %s=%q, got %q", k, want, q.Get(k))
		}
	}
	if len(e.stub.requestsTo("GET", flowPlacesPath)) != 0 {
		t.Error("the view's place ids were looked up as names")
	}
}

func TestFlowViewSavePostsTheFilters(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("POST", flowViewsPath, 201, map[string]any{"id": 9, "name": "OT web"})

	res := e.run("flow", "view", "save", "OT web", "--between", "subnet:3,internet",
		"--service", "https (TCP/443)", "--window", "24h", "--shared")
	if res.ExitCode != 0 {
		t.Fatalf("flow view save failed: %s", res.Stderr)
	}
	reqs := e.stub.requestsTo("POST", flowViewsPath)
	if len(reqs) != 1 {
		t.Fatalf("expected one POST, got %d", len(reqs))
	}
	body := reqs[0].Body
	if body["name"] != "OT web" || body["infrastructure"] != float64(8) || body["shared"] != true {
		t.Errorf("unexpected view body: %v", body)
	}
	query, _ := body["query"].(map[string]any)
	for k, want := range map[string]string{
		"a": "subnet:3", "b": "internet", "service": "https (TCP/443)", "window": "24h",
	} {
		if query[k] != want {
			t.Errorf("expected query %s=%q, got %v", k, want, query[k])
		}
	}
	if _, ok := query["hostgroups"]; ok {
		t.Error("the infrastructure was saved inside the query as well")
	}
	contains(t, res.Stderr, "Saved view 9")
}

// A shared view disappears for everyone, so deleting one is confirmed, and
// without a terminal that needs --yes.
func TestFlowViewDeleteNeedsConfirmation(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setInfra("8")
	e.stub.handleMethod("GET", flowViewsPath, 200, savedViews())
	e.stub.handleMethod("DELETE", flowViewsPath+"7/", 204, nil)

	res := e.run("flow", "view", "delete", "Oslo to internet")
	if res.ExitCode == 0 {
		t.Fatal("deleted without confirmation")
	}
	if len(e.stub.requestsTo("DELETE", flowViewsPath+"7/")) != 0 {
		t.Fatal("DELETE sent without confirmation")
	}

	res = e.run("flow", "view", "delete", "7", "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("flow view delete --yes failed: %s", res.Stderr)
	}
	if len(e.stub.requestsTo("DELETE", flowViewsPath+"7/")) != 1 {
		t.Error("expected one DELETE of view 7")
	}
}
