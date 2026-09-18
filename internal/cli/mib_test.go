package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/upstacked/cli/internal/errs"
)

// writeMIBIndex puts a hand-built index in the test cache, so the lookup
// commands can be tested without cloning eighty megabytes of MIBs.
func writeMIBIndex(e *env, rows ...string) {
	e.t.Helper()
	body := "#ups-mib-index\tv1\n" + strings.Join(rows, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(e.mibDir, "index.tsv"), []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func mibRow(name, oid, syntax, module, desc string) string {
	return strings.Join([]string{name, oid, "object-type", syntax, "read-only", module, module, desc}, "\t")
}

func TestMIBShowReportsTheNumericOIDAndSyntax(t *testing.T) {
	e := newEnv(t)
	writeMIBIndex(e,
		mibRow("ifHCInOctets", "1.3.6.1.2.1.31.1.1.1.6", "Counter64", "IF-MIB", "Total octets received."),
	)

	res := e.run("mib", "show", "ifHCInOctets")
	if res.ExitCode != 0 {
		t.Fatalf("show failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "1.3.6.1.2.1.31.1.1.1.6")
	contains(t, res.Stdout, "Counter64")
}

func TestMIBWalkResolvesANameAndListsOnlyTheSubtree(t *testing.T) {
	e := newEnv(t)
	writeMIBIndex(e,
		mibRow("ifXTable", "1.3.6.1.2.1.31.1.1", "SEQUENCE OF IfXEntry", "IF-MIB", ""),
		mibRow("ifName", "1.3.6.1.2.1.31.1.1.1.1", "DisplayString", "IF-MIB", ""),
		// A sibling whose OID shares the prefix as a string but not as a node.
		mibRow("ifStackTable", "1.3.6.1.2.1.31.11", "SEQUENCE", "IF-MIB", ""),
	)

	res := e.run("mib", "walk", "ifXTable")
	if res.ExitCode != 0 {
		t.Fatalf("walk failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "ifName")
	notContains(t, res.Stdout, "ifStackTable")
}

func TestMIBSearchCanMatchDescriptions(t *testing.T) {
	e := newEnv(t)
	writeMIBIndex(e,
		mibRow("cpmCPUTotal5minRev", "1.3.6.1.4.1.9.9.109.1.1.1.1.8", "Gauge", "CISCO-PROCESS-MIB",
			"The overall CPU busy percentage in the last 5 minute period."),
	)

	res := e.run("mib", "search", "cpu busy", "--describe")
	if res.ExitCode != 0 {
		t.Fatalf("search failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "cpmCPUTotal5minRev")

	// Without --describe the same text must not match: a name search that
	// silently widened would report objects the caller did not ask about.
	res = e.run("mib", "search", "cpu busy")
	notContains(t, res.Stdout, "cpmCPUTotal5minRev")
}

// An empty cache and an unknown object read the same way otherwise, and they
// need different answers: one is "run sync", the other is "wrong name".
func TestMIBLookupWithNoIndexSaysToSync(t *testing.T) {
	e := newEnv(t)

	res := e.run("mib", "show", "ifName")
	if res.ExitCode != errs.CodeNotFound {
		t.Fatalf("expected not-found exit %d, got %d: %s", errs.CodeNotFound, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "ups mib sync")
}

func TestMIBSearchReportsTruncation(t *testing.T) {
	e := newEnv(t)
	writeMIBIndex(e,
		mibRow("ifA", "1.3.6.1.2.1.31.1", "Counter64", "IF-MIB", ""),
		mibRow("ifB", "1.3.6.1.2.1.31.2", "Counter64", "IF-MIB", ""),
	)

	res := e.run("mib", "search", "if", "--limit", "1")
	if res.ExitCode != 0 {
		t.Fatalf("search failed: %s", res.Stderr)
	}
	contains(t, res.Stderr, "truncated")
}
