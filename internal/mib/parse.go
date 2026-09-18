// Package mib caches SMIv2 MIB repositories and indexes the object
// definitions in them.
//
// The point is to answer "what OID is this, and what does it return" without
// a network round trip and without an agent reading megabytes of MIB text
// into its context. Parsing is deliberately partial: it recovers the
// name -> OID -> syntax -> description relation that a monitoring item needs
// and ignores the rest of the grammar. A MIB this parser cannot read is
// skipped rather than failing a sync, because the repositories carry plenty
// of files that no strict parser accepts either.
package mib

import (
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Object is one definition recovered from a MIB file.
type Object struct {
	Name   string
	Kind   string
	Module string
	// File is the path relative to the cache root, so the index stays valid
	// wherever the cache lives.
	File string
	// Anchor is the name the OID hangs off, empty when the definition spelled
	// out a fully numeric OID.
	Anchor string
	// Nums are the numeric components appended to the anchor.
	Nums []int
	// OID is filled in by Resolve; empty when the anchor could not be resolved.
	OID         string
	Syntax      string
	Access      string
	Description string
}

var (
	moduleRe = regexp.MustCompile(`(?m)^\s*([A-Za-z][\w-]*)\s+DEFINITIONS\s`)
	// Macro invocations: `name OBJECT-TYPE`, and the plain
	// `name OBJECT IDENTIFIER ::= { ... }` form that anchors most subtrees.
	defRe    = regexp.MustCompile(`(?m)^[ \t]*([a-zA-Z][\w-]*)[ \t]+(OBJECT-TYPE|MODULE-IDENTITY|OBJECT-IDENTITY|NOTIFICATION-TYPE|OBJECT IDENTIFIER)\b`)
	braceRe  = regexp.MustCompile(`::=\s*\{([^}]*)\}`)
	syntaxRe = regexp.MustCompile(`(?m)^[ \t]*SYNTAX[ \t]+(.+)$`)
	accessRe = regexp.MustCompile(`(?m)^[ \t]*(?:MAX-ACCESS|ACCESS)[ \t]+(.+)$`)
	descRe   = regexp.MustCompile(`(?s)DESCRIPTION\s*"([^"]*)"`)
	tokenRe  = regexp.MustCompile(`([A-Za-z][\w-]*)\s*\(\s*(\d+)\s*\)|([A-Za-z][\w-]*)|(\d+)`)
	spaceRe  = regexp.MustCompile(`\s+`)
)

// ParseFile reads one MIB file. A file that defines nothing is not an error:
// the repositories contain READMEs, licences and vendor archives alongside
// the MIBs, and refusing them would make a sync fail on irrelevant content.
func ParseFile(path, rel string) (string, []Object) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}
	return Parse(string(b), rel)
}

// Parse recovers the definitions in one MIB document.
func Parse(src, rel string) (string, []Object) {
	src = stripComments(src)

	module := ""
	if m := moduleRe.FindStringSubmatch(src); m != nil {
		module = m[1]
	}

	locs := defRe.FindAllStringSubmatchIndex(src, -1)
	out := make([]Object, 0, len(locs))
	for i, loc := range locs {
		name := src[loc[2]:loc[3]]
		kind := src[loc[4]:loc[5]]

		// The body runs to the next definition, so a missing `::=` cannot
		// swallow the rest of the file and attach the wrong OID.
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body := src[loc[1]:end]

		br := braceRe.FindStringSubmatch(body)
		if br == nil {
			continue
		}
		anchor, nums, ok := parseOIDClause(br[1])
		if !ok {
			continue
		}

		o := Object{
			Name: name, Kind: normalizeKind(kind), Module: module, File: rel,
			Anchor: anchor, Nums: nums,
		}
		if m := syntaxRe.FindStringSubmatch(body); m != nil {
			o.Syntax = collapse(m[1])
		}
		if m := accessRe.FindStringSubmatch(body); m != nil {
			o.Access = collapse(m[1])
		}
		if m := descRe.FindStringSubmatch(body); m != nil {
			o.Description = collapse(m[1])
		}
		out = append(out, o)
	}
	return module, out
}

func normalizeKind(k string) string {
	if k == "OBJECT IDENTIFIER" {
		return "node"
	}
	return strings.ToLower(k)
}

// parseOIDClause reads the `{ mib-2 1 }` half of a definition.
//
// The first token may be a name (the subtree this hangs off) or a number (a
// fully spelled-out OID). Later tokens are numbers, or the `org(3)` form that
// names a component and gives its number in the same breath.
func parseOIDClause(s string) (string, []int, bool) {
	toks := tokenRe.FindAllStringSubmatch(strings.TrimSpace(s), -1)
	if len(toks) == 0 {
		return "", nil, false
	}
	anchor := ""
	nums := []int{}
	for i, t := range toks {
		switch {
		case t[1] != "": // name(number)
			n, _ := strconv.Atoi(t[2])
			if i == 0 {
				// `{ iso(1) 3 }`: the anchor names itself and its number, so
				// the number is the start of the OID rather than an offset.
				nums = append(nums, n)
				continue
			}
			nums = append(nums, n)
		case t[3] != "": // bare name
			if i != 0 {
				// A bare name after the first position has no number to
				// contribute, so the OID cannot be reconstructed.
				return "", nil, false
			}
			anchor = t[3]
		case t[4] != "": // bare number
			n, _ := strconv.Atoi(t[4])
			nums = append(nums, n)
		}
	}
	return anchor, nums, true
}

// stripComments removes `--` comments without touching quoted text, which is
// where DESCRIPTION lives and where dashes are ordinary punctuation.
func stripComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			inString = !inString
			b.WriteByte(c)
			continue
		}
		if !inString && c == '-' && i+1 < len(s) && s[i+1] == '-' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func collapse(s string) string {
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}
