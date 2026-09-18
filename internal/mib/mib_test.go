package mib

import "testing"

const sampleMIB = `
IF-MIB DEFINITIONS ::= BEGIN

-- a comment mentioning -- dashes
internet     OBJECT IDENTIFIER ::= { iso org(3) dod(6) 1 }
mgmt         OBJECT IDENTIFIER ::= { internet 2 }
mib-2        OBJECT IDENTIFIER ::= { mgmt 1 }
ifMIB        MODULE-IDENTITY
    LAST-UPDATED "200006140000Z"
    DESCRIPTION  "The MIB module to describe generic objects."
    ::= { mib-2 31 }

ifXTable OBJECT-TYPE
    SYNTAX      SEQUENCE OF IfXEntry
    MAX-ACCESS  not-accessible
    STATUS      current
    DESCRIPTION "A list of interface entries -- one per interface."
    ::= { ifMIB 1 1 }

ifName OBJECT-TYPE
    SYNTAX      DisplayString
    MAX-ACCESS  read-only
    STATUS      current
    DESCRIPTION "The textual name of the interface."
    ::= { ifXTable 1 1 }

END
`

func TestParseRecoversNameSyntaxAndDescription(t *testing.T) {
	module, objs := Parse(sampleMIB, "vendor/IF-MIB")
	if module != "IF-MIB" {
		t.Errorf("module = %q, want IF-MIB", module)
	}
	byName := map[string]Object{}
	for _, o := range objs {
		byName[o.Name] = o
	}
	ifName, ok := byName["ifName"]
	if !ok {
		t.Fatalf("ifName was not parsed; got %v", byName)
	}
	if ifName.Syntax != "DisplayString" {
		t.Errorf("syntax = %q", ifName.Syntax)
	}
	if ifName.Access != "read-only" {
		t.Errorf("access = %q", ifName.Access)
	}
	if ifName.Description != "The textual name of the interface." {
		t.Errorf("description = %q", ifName.Description)
	}
	if ifName.File != "vendor/IF-MIB" {
		t.Errorf("file = %q", ifName.File)
	}
}

// A description may contain the comment marker. Stripping comments inside
// quoted text would truncate exactly the field an agent reads to pick an OID.
func TestParseKeepsDashesInsideDescriptions(t *testing.T) {
	_, objs := Parse(sampleMIB, "x")
	for _, o := range objs {
		if o.Name == "ifXTable" {
			if o.Description != "A list of interface entries -- one per interface." {
				t.Errorf("description was mangled: %q", o.Description)
			}
			return
		}
	}
	t.Fatal("ifXTable not parsed")
}

func TestResolveWalksTheParentChainToNumericOIDs(t *testing.T) {
	_, objs := Parse(sampleMIB, "x")
	objs = Resolve(objs)

	want := map[string]string{
		"internet": "1.3.6.1",
		"mib-2":    "1.3.6.1.2.1",
		"ifMIB":    "1.3.6.1.2.1.31",
		"ifXTable": "1.3.6.1.2.1.31.1.1",
		"ifName":   "1.3.6.1.2.1.31.1.1.1.1",
	}
	got := map[string]string{}
	for _, o := range objs {
		got[o.Name] = o.OID
	}
	for name, oid := range want {
		if got[name] != oid {
			t.Errorf("%s = %q, want %q", name, got[name], oid)
		}
	}
}

// A parent defined in a MIB the cache does not carry leaves a hole. Filling it
// with a guess would point a monitoring item at the wrong subtree, so the OID
// stays empty and the caller is told.
func TestResolveLeavesUnknownParentsEmptyRatherThanGuessing(t *testing.T) {
	_, objs := Parse(`X DEFINITIONS ::= BEGIN
orphan OBJECT-TYPE SYNTAX Integer32 ::= { someVendorRoot 4 }
END`, "x")
	objs = Resolve(objs)
	if len(objs) != 1 {
		t.Fatalf("expected one object, got %d", len(objs))
	}
	if objs[0].OID != "" {
		t.Errorf("OID = %q, want empty for an unresolvable parent", objs[0].OID)
	}
}

func TestResolveSurvivesACycle(t *testing.T) {
	_, objs := Parse(`X DEFINITIONS ::= BEGIN
a OBJECT-TYPE SYNTAX Integer32 ::= { b 1 }
b OBJECT-TYPE SYNTAX Integer32 ::= { a 1 }
END`, "x")
	objs = Resolve(objs) // must terminate
	for _, o := range objs {
		if o.OID != "" {
			t.Errorf("%s resolved to %q through a cycle", o.Name, o.OID)
		}
	}
}

func TestSubtreePrefixDoesNotMatchSiblings(t *testing.T) {
	if inSubtree("1.3.6.1.2.1.11", "1.3.6.1.2.1.1") {
		t.Error("1.3.6.1.2.1.11 is a sibling of 1.3.6.1.2.1.1, not a child")
	}
	if !inSubtree("1.3.6.1.2.1.1.3", "1.3.6.1.2.1.1") {
		t.Error("1.3.6.1.2.1.1.3 is under 1.3.6.1.2.1.1")
	}
}

// The repositories carry the standard MIBs once per vendor directory, so the
// same object is parsed many times. Listing a subtree must not repeat it.
func TestDedupeCollapsesRepeatedCopies(t *testing.T) {
	objs := []Object{
		{Name: "ifName", OID: "1.3.6.1.2.1.31.1.1.1.1", File: "a/IF-MIB"},
		{Name: "ifName", OID: "1.3.6.1.2.1.31.1.1.1.1", File: "b/IF-MIB"},
		{Name: "ifName", OID: "1.3.6.1.4.1.9.1", File: "c/VENDOR-MIB"},
	}
	got := dedupe(objs)
	if len(got) != 2 {
		t.Fatalf("expected 2 objects (one per distinct OID), got %d", len(got))
	}
	if got[0].File != "a/IF-MIB" {
		t.Errorf("the first copy should win, got %q", got[0].File)
	}
}

// Cisco ships an auto-converted SMIv1 tree beside the real MIBs, where the
// conversion gave up on some types. Walk order puts v1 first, so the first
// copy winning would report Counter64 objects as having no syntax at all.
func TestDedupePrefersTheCopyThatSurvivedConversion(t *testing.T) {
	objs := []Object{
		{Name: "x", OID: "1.2.3", File: "cisco/v1/X-V1SMI.my", Access: "read-only"},
		{Name: "x", OID: "1.2.3", File: "cisco/v2/X.my", Access: "read-only",
			Syntax: "Counter64", Description: "Octets."},
	}
	got := dedupe(objs)
	if len(got) != 1 {
		t.Fatalf("expected one object, got %d", len(got))
	}
	if got[0].Syntax != "Counter64" || got[0].File != "cisco/v2/X.my" {
		t.Errorf("kept the degraded copy: %+v", got[0])
	}
}

// Equal copies must resolve the same way on every machine, or the same query
// answers differently depending on directory walk order.
func TestDedupeKeepsTheFirstOfEquallyCompleteCopies(t *testing.T) {
	objs := []Object{
		{Name: "x", OID: "1.2.3", File: "a/X.my", Syntax: "Counter64"},
		{Name: "x", OID: "1.2.3", File: "b/X.my", Syntax: "Counter64"},
	}
	got := dedupe(objs)
	if got[0].File != "a/X.my" {
		t.Errorf("first copy should win a tie, got %q", got[0].File)
	}
}
