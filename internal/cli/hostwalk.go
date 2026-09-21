package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/mib"
	"github.com/upstacked/cli/internal/output"
)

// snmpDataSource is the SNMP data source's id: the item-level pipeline, which walks.
const snmpDataSource = 2

func newHostWalkCmd(app *App) *cobra.Command {
	var credential string
	var wait time.Duration

	c := &cobra.Command{
		Use:   "walk <host-id> <name|oid>...",
		Short: "Walk a device over SNMP and show what it actually returns",
		Long: `Ask a device what it answers for one or more OIDs, before any item exists.

'ups mib walk' reads MIB files: it says what a subtree could contain. This asks
the device, through the infrastructure's monitoring agent, what it does
contain - which rows exist, what they are indexed by, and what the values look
like. A MIB object the device does not implement comes back empty here.

Names are resolved through the local MIB cache (ups mib sync); numeric OIDs are
sent as given. A table, an entry or single columns all work. Walking a whole
table on a large switch returns a lot: prefer the columns you mean to map.

It runs as a dry run with no item behind it. Nothing is saved or published.

The output ends with what to put on an item to poll the same columns, and the
paths a schema mapping uses to read each one.`,
		Example: `  ups host walk 205 ifName ifHCInOctets --credential 1
  ups host walk 205 ifXTable --credential 1
  ups host walk 205 1.3.6.1.4.1.9.9.109.1.1.1.1.8 --credential 1 --json`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if credential == "" {
				return errs.Usage("--credential is required").
					WithHint("the SNMP credential for this device: ups credential list")
			}
			bases, names, err := resolveWalkOIDs(args[1:])
			if err != nil {
				return err
			}

			body := map[string]any{
				"host":        atoiOr(args[0]),
				"data_source": snmpDataSource,
				"credential":  atoiOr(credential),
				"parameters":  map[string]any{"oid": bases},
			}
			var dispatch jsonRaw
			err = app.Spin("Queueing a walk of host "+args[0], func() error {
				return app.mutate("POST", dryRunsPath, body, &dispatch)
			})
			if err != nil {
				if errs.StatusOf(err) == http.StatusBadRequest && strings.Contains(err.Error(), "monitoring_item") {
					return errs.General("this server cannot walk a device without an item").
						WithHint("it predates device walks. Dry-run an SNMP item with --from-file '{\"parameters\": {\"oid\": [...]}}' instead")
				}
				return err
			}
			if app.DryRun {
				return nil
			}
			var m row
			_ = jsonUnmarshal(dispatch, &m)
			if wait > dryRunServerTimeout {
				wait = dryRunServerTimeout
			}
			m, _, err = app.awaitDryRun(str(m, "id"), wait, m, dispatch)
			if err != nil {
				return err
			}
			return app.reportWalk(m, bases, names)
		},
	}
	c.Flags().StringVar(&credential, "credential", "", "SNMP credential id (required)")
	c.Flags().DurationVar(&wait, "wait", dryRunWait, "how long to wait for the agent")
	return c
}

// resolveWalkOIDs turns names into numeric OIDs, keeping the name each was given as.
func resolveWalkOIDs(specs []string) ([]string, map[string]string, error) {
	var store *mib.Store
	oids := make([]string, 0, len(specs))
	names := map[string]string{}
	for _, spec := range specs {
		spec = strings.Trim(strings.TrimSpace(spec), ".")
		if isNumericOID(spec) {
			oids = append(oids, spec)
			continue
		}
		if store == nil {
			s, err := mibStore()
			if err != nil {
				return nil, nil, err
			}
			store = s
		}
		o, err := store.Lookup(spec)
		if err != nil {
			return nil, nil, mibLookupError(err, spec)
		}
		if o.OID == "" {
			return nil, nil, errs.General("%s has no resolvable OID in the cache", spec).
				WithHint("pass the numeric OID instead, or sync the MIB that defines its parent: ups mib sync")
		}
		oids = append(oids, o.OID)
		names[o.OID] = o.Name
	}
	return oids, names, nil
}

type walkRow struct {
	Column string `json:"column"`
	Name   string `json:"name,omitempty"`
	Index  string `json:"index"`
	Value  string `json:"value"`
	OID    string `json:"oid"`
	Path   string `json:"path"`
}

func (a *App) reportWalk(m row, bases []string, names map[string]string) error {
	t, sym := a.Theme(), a.Sym()
	runID := dash(str(m, "id"))
	trace := objField(m, "trace")
	fetch := objField(trace, "request_status")

	if str(m, "status") == dryRunPending {
		return errs.General("the agent has not reported walk %s yet", runID).
			WithHint("read it later: ups monitoring item dry-run show %s", runID)
	}
	if fetch == nil {
		msg := str(m, "error")
		if msg == "" {
			msg = "no result"
		}
		return errs.General("walk %s never ran: %s", runID, msg).
			WithHint("confirm the infrastructure's monitoring agent is online, then try again")
	}
	details := objField(fetch, "details")
	if str(fetch, "status") != "success" {
		return errs.General("the device did not answer the walk: %s", dash(str(details, "message"))).
			WithHint("check the credential and that the host's IP answers SNMP from the agent")
	}

	rows := flattenWalk(details["response"], bases, names)
	tbl := &output.Table{
		Columns:   []string{"COLUMN", "INDEX", "VALUE"},
		Truncated: str(details, "response_truncated") == "true",
		Empty:     "The device returned nothing for those OIDs. It may not implement them.",
	}
	for _, r := range rows {
		raw, _ := json.Marshal(r)
		label := r.Name
		if label == "" {
			label = r.Column
		}
		tbl.Add(r.OID, raw, label, r.Index, truncate(r.Value, 60))
	}
	if err := a.Printer.Print(tbl); err != nil {
		return err
	}
	if a.AsJSON || len(rows) == 0 {
		return nil
	}

	fmt.Fprintf(a.Stderr, "\n%s To poll these columns, the item takes:\n", t.Dim.Apply(sym.OK))
	cols := walkColumns(rows)
	params, _ := json.Marshal(map[string]any{"oid": cols})
	fmt.Fprintf(a.Stderr, "    --data-source snmp --params '%s'\n", params)
	fmt.Fprintf(a.Stderr, "  and a --multi-valued mapping reads each row with:\n")
	for _, c := range cols {
		label := names[c]
		if label == "" {
			label = firstName(rows, c)
		}
		fmt.Fprintf(a.Stderr, "    %-24s item['$.%s'].value   (row index: .key)\n", dash(label), c)
	}
	return nil
}

// flattenWalk turns the engine's response - nested one OID arc per level - into
// rows. Each row is attributed to the deepest MIB object above it, so a table
// walk splits into its columns.
func flattenWalk(resp any, bases []string, names map[string]string) []walkRow {
	var leaves []walkRow
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, child)
			}
		default:
			leaves = append(leaves, walkRow{OID: prefix, Value: plain(x)})
		}
	}
	walk("", resp)

	namer := newColumnNamer(names)
	for i := range leaves {
		col, name := namer.column(leaves[i].OID, bases)
		leaves[i].Column, leaves[i].Name = col, name
		leaves[i].Index = strings.TrimPrefix(strings.TrimPrefix(leaves[i].OID, col), ".")
		leaves[i].Path = fmt.Sprintf("item['$.%s'].value", col)
	}
	sort.Slice(leaves, func(i, j int) bool {
		if leaves[i].Column != leaves[j].Column {
			return compareOID(leaves[i].Column, leaves[j].Column) < 0
		}
		return compareOID(leaves[i].Index, leaves[j].Index) < 0
	})
	return leaves
}

// columnNamer finds the MIB object a returned OID belongs to, from the cache
// when there is one. Without a cache the requested OID is the column.
type columnNamer struct {
	store *mib.Store
	names map[string]string
	seen  map[string]string
}

func newColumnNamer(names map[string]string) *columnNamer {
	s, _ := mibStore()
	return &columnNamer{store: s, names: names, seen: map[string]string{}}
}

func (n *columnNamer) column(oid string, bases []string) (string, string) {
	base := ""
	for _, b := range bases {
		if (oid == b || strings.HasPrefix(oid, b+".")) && len(b) > len(base) {
			base = b
		}
	}
	if base == "" {
		return oid, ""
	}
	arcs := strings.Split(oid, ".")
	for end := len(arcs) - 1; end >= len(strings.Split(base, ".")); end-- {
		prefix := strings.Join(arcs[:end], ".")
		if name, ok := n.lookup(prefix); ok {
			return prefix, name
		}
	}
	return base, n.names[base]
}

func (n *columnNamer) lookup(oid string) (string, bool) {
	if name, ok := n.seen[oid]; ok {
		return name, name != ""
	}
	name := ""
	if n.store != nil {
		if o, err := n.store.Lookup(oid); err == nil && o.OID == oid {
			name = o.Name
		}
	}
	n.seen[oid] = name
	return name, name != ""
}

func walkColumns(rows []walkRow) []string {
	var cols []string
	seen := map[string]bool{}
	for _, r := range rows {
		if !seen[r.Column] {
			seen[r.Column] = true
			cols = append(cols, r.Column)
		}
	}
	return cols
}

func firstName(rows []walkRow, col string) string {
	for _, r := range rows {
		if r.Column == col {
			return r.Name
		}
	}
	return ""
}

// compareOID orders dotted OIDs numerically, arc by arc.
func compareOID(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, errX := strconv.Atoi(as[i])
		y, errY := strconv.Atoi(bs[i])
		if errX != nil || errY != nil {
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
			continue
		}
		if x != y {
			return x - y
		}
	}
	return len(as) - len(bs)
}
