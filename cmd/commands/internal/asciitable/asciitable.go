// Package asciitable renders a small ASCII-bordered table to an
// io.Writer. It exists so cmd/commands no longer depends on
// github.com/jedib0t/go-pretty/v6 for the single trackpayment table
// (the only place lncli rendered one). Visual output matches
// go-pretty's StyleDefault for the call surface we use: a single
// header band, per-column alignment, and `+/-/|` box borders.
//
// We intentionally drop everything go-pretty supports that we don't
// need: alternate styles (bold, double, rounded, ColoredBright,
// etc.), HTML / CSV / Markdown renderers, ANSI coloring, automatic
// text wrapping, captions, footers, page-break separators, sorting,
// and runewidth-aware width counting. The trackpayment table is
// pure ASCII (HTLC state strings, formatted decimals, integer chan
// IDs, hex pubkey aliases joined by "->"), so `utf8.RuneCountInString`
// is sufficient for column sizing.
package asciitable

import (
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

// Align controls horizontal alignment of cell contents within a
// column.
type Align int

const (
	// AlignDefault picks left-align for string values and
	// right-align for numeric values (mirroring go-pretty's
	// StyleDefault behavior on the trackpayment table).
	AlignDefault Align = iota

	// AlignLeft pads on the right so the cell content sits flush
	// against the left padding column.
	AlignLeft

	// AlignRight pads on the left so the cell content sits flush
	// against the right padding column. Used for amounts, fees,
	// timelocks, and the like.
	AlignRight
)

// ColumnConfig overrides the default formatting for a single column,
// identified by header text.
type ColumnConfig struct {
	// Name matches the header string for the column this config
	// applies to.
	Name string

	// Align is the alignment used for body rows.
	Align Align

	// AlignHeader is the alignment used for the header row. If
	// zero (AlignDefault), Align is used.
	AlignHeader Align
}

// Row is a single line of cell values. Each element may be any
// type; non-string values are formatted with `%v`.
type Row []interface{}

// Writer accumulates rows and renders them as a bordered ASCII
// table on demand.
type Writer struct {
	header  Row
	rows    []Row
	configs []ColumnConfig
	output  io.Writer
}

// NewWriter returns a Writer with no header, no rows, and no
// configured output. Render is a no-op until SetOutputMirror has
// been called.
func NewWriter() *Writer {
	return &Writer{}
}

// AppendHeader records the header row. Calling it more than once
// replaces the previous header.
func (w *Writer) AppendHeader(r Row) {
	w.header = r
}

// AppendRow adds a body row to the table.
func (w *Writer) AppendRow(r Row) {
	w.rows = append(w.rows, r)
}

// SetColumnConfigs registers per-column alignment overrides. Configs
// for column names that do not match any header entry are silently
// ignored, matching the upstream contract.
func (w *Writer) SetColumnConfigs(c []ColumnConfig) {
	w.configs = c
}

// SetOutputMirror sets the destination Render will write to.
func (w *Writer) SetOutputMirror(out io.Writer) {
	w.output = out
}

// Render writes the table to the configured output. If no output
// has been set or there are no columns to render the call is a
// no-op, matching go-pretty's behavior.
func (w *Writer) Render() {
	if w.output == nil || (len(w.header) == 0 && len(w.rows) == 0) {
		return
	}

	cols := len(w.header)
	for _, r := range w.rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return
	}

	widths := make([]int, cols)
	for i := 0; i < cols && i < len(w.header); i++ {
		widths[i] = utf8.RuneCountInString(format(w.header[i]))
	}
	for _, r := range w.rows {
		for i := 0; i < cols && i < len(r); i++ {
			if n := utf8.RuneCountInString(format(r[i])); n > widths[i] {
				widths[i] = n
			}
		}
	}

	// Build the per-column config lookup by header name. We do
	// this lazily so an empty configs slice is free.
	cfg := make([]ColumnConfig, cols)
	for i, h := range w.header {
		if i >= len(cfg) {
			break
		}
		name := format(h)
		for _, c := range w.configs {
			if c.Name == name {
				cfg[i] = c
				break
			}
		}
	}

	border := buildBorder(widths)
	fmt.Fprintln(w.output, border)
	if len(w.header) > 0 {
		fmt.Fprintln(w.output, renderRow(
			w.header, widths, cfg, true,
		))
		fmt.Fprintln(w.output, border)
	}
	for _, r := range w.rows {
		fmt.Fprintln(w.output, renderRow(r, widths, cfg, false))
	}
	fmt.Fprintln(w.output, border)
}

// buildBorder returns one `+---+---+` separator line sized to the
// per-column widths (including one space of padding on each side).
func buildBorder(widths []int) string {
	var b strings.Builder
	b.WriteByte('+')
	for _, w := range widths {
		b.WriteString(strings.Repeat("-", w+2))
		b.WriteByte('+')
	}
	return b.String()
}

// renderRow emits one `| cell | cell |` line with per-column
// alignment applied.
func renderRow(r Row, widths []int, cfg []ColumnConfig,
	isHeader bool) string {

	var b strings.Builder
	b.WriteByte('|')
	for i, w := range widths {
		var v interface{}
		if i < len(r) {
			v = r[i]
		}
		s := format(v)

		align := cfg[i].Align
		if isHeader && cfg[i].AlignHeader != AlignDefault {
			align = cfg[i].AlignHeader
		}
		if align == AlignDefault {
			align = autoAlign(v)
		}

		b.WriteByte(' ')
		b.WriteString(pad(s, w, align))
		b.WriteByte(' ')
		b.WriteByte('|')
	}
	return b.String()
}

// pad left- or right-aligns s within width w by inserting spaces on
// the opposite side. Strings already at or above the width are
// returned unchanged (we never truncate, matching upstream).
func pad(s string, w int, a Align) string {
	have := utf8.RuneCountInString(s)
	if have >= w {
		return s
	}
	gap := strings.Repeat(" ", w-have)
	if a == AlignRight {
		return gap + s
	}
	return s + gap
}

// autoAlign picks right-align for numeric kinds and left-align for
// everything else, mirroring go-pretty's StyleDefault auto-detection
// on a per-cell basis.
func autoAlign(v interface{}) Align {
	if v == nil {
		return AlignLeft
	}
	switch reflect.TypeOf(v).Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32,
		reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32,
		reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return AlignRight
	}
	return AlignLeft
}

// format coerces an arbitrary cell value to its display string,
// preferring fmt.Stringer over the default `%v` formatter when the
// type implements it.
func format(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if s, ok := v.(fmt.Stringer); ok {
		return s.String()
	}
	return fmt.Sprintf("%v", v)
}
