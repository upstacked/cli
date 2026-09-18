package mib

import (
	"sort"
	"strconv"
	"strings"
)

// roots are the OID anchors no MIB defines in terms of anything else.
var roots = map[string]string{
	"ccitt":           "0",
	"itu-t":           "0",
	"iso":             "1",
	"joint-iso-ccitt": "2",
	"joint-iso-itu-t": "2",
}

// Resolve fills in the numeric OID of every object it can.
//
// An object names its parent, so resolution is a walk up the chain across all
// the MIBs in the cache at once - which is why this runs over the whole index
// rather than per file. An object whose parent is defined in a MIB the cache
// does not carry keeps an empty OID: reporting that honestly is the point,
// since a guessed OID polls the wrong thing.
func Resolve(objs []Object) []Object {
	byName := make(map[string]int, len(objs))
	for i := range objs {
		// First definition wins. Names collide across vendor MIBs, and
		// flip-flopping on re-index would make the same query answer
		// differently on different machines.
		if _, ok := byName[objs[i].Name]; !ok {
			byName[objs[i].Name] = i
		}
	}

	memo := make(map[string]string, len(objs))
	var walk func(name string, depth int) string
	walk = func(name string, depth int) string {
		if oid, ok := roots[name]; ok {
			return oid
		}
		if oid, ok := memo[name]; ok {
			return oid
		}
		// A cycle, or a chain deep enough to be one, resolves to nothing
		// rather than recursing until the stack gives out.
		if depth > 64 {
			return ""
		}
		i, ok := byName[name]
		if !ok {
			return ""
		}
		memo[name] = "" // break cycles while this name is in flight
		oid := compose(objs[i], func(a string) string { return walk(a, depth+1) })
		memo[name] = oid
		return oid
	}

	for i := range objs {
		objs[i].OID = compose(objs[i], func(a string) string { return walk(a, 0) })
	}
	return objs
}

func compose(o Object, parent func(string) string) string {
	parts := make([]string, 0, len(o.Nums)+1)
	if o.Anchor != "" {
		p := parent(o.Anchor)
		if p == "" {
			return ""
		}
		parts = append(parts, p)
	}
	for _, n := range o.Nums {
		parts = append(parts, strconv.Itoa(n))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ".")
}

// SortIndex orders objects by OID so that neighbouring rows in the index are
// neighbouring nodes in the tree. Walking a subtree is then a range over the
// file rather than a scan of all of it.
func SortIndex(objs []Object) {
	sort.SliceStable(objs, func(i, j int) bool {
		a, b := objs[i], objs[j]
		if a.OID == b.OID {
			return a.Name < b.Name
		}
		if a.OID == "" {
			return false
		}
		if b.OID == "" {
			return true
		}
		return lessOID(a.OID, b.OID)
	})
}

func lessOID(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, _ := strconv.Atoi(as[i])
		y, _ := strconv.Atoi(bs[i])
		if x != y {
			return x < y
		}
	}
	return len(as) < len(bs)
}
