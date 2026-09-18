package mib

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrNoIndex means the cache has never been built. Callers turn this into
// advice rather than a stack trace, because it is the expected state on a
// machine that has not run a sync yet.
var ErrNoIndex = errors.New("no MIB index")

// Query selects rows from the index.
type Query struct {
	// Text matches the object name, and its description when Describe is set.
	Text string
	// OID restricts results to a subtree, given as a numeric prefix.
	OID string
	// Module restricts results to one MIB module.
	Module string
	// Describe widens Text to search descriptions too.
	Describe bool
	Limit    int
}

// Search scans the index. Rows are returned in index order, which is OID
// order, so a subtree query reads out as a walk of that subtree.
func (s *Store) Search(q Query) ([]Object, bool, error) {
	f, err := os.Open(s.IndexPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, ErrNoIndex
		}
		return nil, false, err
	}
	defer f.Close()

	text := strings.ToLower(q.Text)
	module := strings.ToLower(q.Module)
	prefix := strings.TrimSuffix(q.OID, ".")

	var out []Object
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		o, ok := parseRow(line)
		if !ok {
			continue
		}
		if prefix != "" && !inSubtree(o.OID, prefix) {
			continue
		}
		if module != "" && !strings.Contains(strings.ToLower(o.Module), module) {
			continue
		}
		if text != "" {
			hit := strings.Contains(strings.ToLower(o.Name), text)
			if !hit && q.Describe {
				hit = strings.Contains(strings.ToLower(o.Description), text)
			}
			if !hit {
				continue
			}
		}
		out = append(out, o)
		// One row past the limit is enough to know the answer was cut short,
		// which the caller must say rather than presenting a capped list as
		// the complete one.
		if q.Limit > 0 && len(out) > q.Limit {
			return out[:q.Limit], true, nil
		}
	}
	return out, false, sc.Err()
}

// Lookup finds one object by name (case-insensitive) or by exact OID.
func (s *Store) Lookup(nameOrOID string) (Object, error) {
	q := Query{Text: nameOrOID}
	if looksNumeric(nameOrOID) {
		q = Query{OID: nameOrOID}
	}
	found, _, err := s.Search(q)
	if err != nil {
		return Object{}, err
	}
	want := strings.ToLower(nameOrOID)
	for _, o := range found {
		if strings.ToLower(o.Name) == want || o.OID == nameOrOID {
			return o, nil
		}
	}
	return Object{}, fmt.Errorf("no MIB object named %q in the cache", nameOrOID)
}

func parseRow(line string) (Object, bool) {
	p := strings.Split(line, "\t")
	if len(p) < 8 {
		return Object{}, false
	}
	return Object{
		Name: p[0], OID: p[1], Kind: p[2], Syntax: p[3],
		Access: p[4], Module: p[5], File: p[6], Description: p[7],
	}, true
}

// inSubtree reports whether oid is prefix or below it. String prefixes are
// not enough: 1.3.6.1.2.1.11 would otherwise match under 1.3.6.1.2.1.1.
func inSubtree(oid, prefix string) bool {
	if oid == prefix {
		return true
	}
	return strings.HasPrefix(oid, prefix+".")
}

func looksNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r != '.' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
