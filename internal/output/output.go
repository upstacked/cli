// Package output renders results for humans or for machines.
//
// The table form is explicitly not a stable interface; --json is. Commands
// build a Table and let this package decide how to present it, so that the
// TTY/non-TTY rules (X1) live in exactly one place.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

type Format string

const (
	FormatTable  Format = "table"
	FormatJSON   Format = "json"
	FormatIDOnly Format = "id-only"
)

// Printer writes command results.
type Printer struct {
	Out    io.Writer
	Err    io.Writer
	Format Format
	// Color is disabled automatically when Out is not a terminal.
	Color bool
}

func New(out, errw io.Writer, format Format) *Printer {
	return &Printer{Out: out, Err: errw, Format: format, Color: format == FormatTable && IsTerminal(out)}
}

// IsTerminal reports whether w is an interactive terminal.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// Table is a renderable result set. Rows carry their source object so that
// --json emits the server's representation rather than the flattened view.
type Table struct {
	Columns []string
	Rows    [][]string
	// Raw is the underlying JSON for each row, used by --json.
	Raw []json.RawMessage
	// IDs backs --id-only.
	IDs []string
	// Truncated marks a capped traversal. Never rendered silently (X1, L-series).
	Truncated bool
	// Total is the server's reported count, when known.
	Total int
	// Empty is shown when there are no rows.
	Empty string
}

func (t *Table) Add(id string, raw json.RawMessage, cells ...string) {
	t.Rows = append(t.Rows, cells)
	t.Raw = append(t.Raw, raw)
	t.IDs = append(t.IDs, id)
}

func (t *Table) Len() int { return len(t.Rows) }

// Print renders a table in the printer's format.
func (p *Printer) Print(t *Table) error {
	switch p.Format {
	case FormatJSON:
		return p.printJSON(t)
	case FormatIDOnly:
		for _, id := range t.IDs {
			fmt.Fprintln(p.Out, id)
		}
		p.warnTruncated(t)
		return nil
	default:
		return p.printTable(t)
	}
}

func (p *Printer) printJSON(t *Table) error {
	items := t.Raw
	if items == nil {
		items = []json.RawMessage{}
	}
	doc := map[string]any{
		"items":     items,
		"count":     len(items),
		"truncated": t.Truncated,
	}
	if t.Total > 0 {
		doc["total"] = t.Total
	}
	enc := json.NewEncoder(p.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func (p *Printer) printTable(t *Table) error {
	if t.Len() == 0 {
		msg := t.Empty
		if msg == "" {
			msg = "No results."
		}
		fmt.Fprintln(p.Out, msg)
		p.warnTruncated(t)
		return nil
	}
	tw := tabwriter.NewWriter(p.Out, 0, 0, 2, ' ', 0)
	if len(t.Columns) > 0 {
		fmt.Fprintln(tw, strings.Join(t.Columns, "\t"))
	}
	for _, row := range t.Rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	p.warnTruncated(t)
	return nil
}

// warnTruncated makes a capped result set impossible to mistake for a complete
// one. A truncated set is not "no more matches".
func (p *Printer) warnTruncated(t *Table) {
	if !t.Truncated {
		return
	}
	if t.Total > 0 {
		fmt.Fprintf(p.Err, "\nnote: showing %d of %d results. This is not the complete set - it was truncated. Raise --limit to see more.\n", t.Len(), t.Total)
		return
	}
	fmt.Fprintf(p.Err, "\nnote: results were truncated at %d. This is not the complete set. Raise --limit to see more.\n", t.Len())
}

// Object prints a single record.
func (p *Printer) Object(raw json.RawMessage, fields [][2]string) error {
	if p.Format == FormatJSON {
		var buf any
		if err := json.Unmarshal(raw, &buf); err != nil {
			fmt.Fprintln(p.Out, string(raw))
			return nil
		}
		enc := json.NewEncoder(p.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(buf)
	}
	tw := tabwriter.NewWriter(p.Out, 0, 0, 2, ' ', 0)
	for _, f := range fields {
		fmt.Fprintf(tw, "%s\t%s\n", f[0], f[1])
	}
	return tw.Flush()
}

// JSON emits an arbitrary document (used by doctor --json).
func (p *Printer) JSON(v any) error {
	enc := json.NewEncoder(p.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Infof writes progress to stderr, keeping stdout clean for data (X2).
func (p *Printer) Infof(format string, args ...any) {
	fmt.Fprintf(p.Err, format+"\n", args...)
}

// Printf writes to stdout.
func (p *Printer) Printf(format string, args ...any) {
	fmt.Fprintf(p.Out, format+"\n", args...)
}
