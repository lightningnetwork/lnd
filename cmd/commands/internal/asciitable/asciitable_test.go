package asciitable

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRenderMatchesGoPrettyDefault locks in the visual format used
// by lncli's trackpayment table. The expected layout is the
// go-pretty StyleDefault: `+/-/|` borders, one space of padding on
// each side of every cell, header band separated from the body by
// a horizontal rule, and per-column alignment.
func TestRenderMatchesGoPrettyDefault(t *testing.T) {
	t.Parallel()

	w := NewWriter()
	w.AppendHeader(Row{"HTLC_STATE", "ATTEMPT_TIME", "CHAN_OUT"})
	w.SetColumnConfigs([]ColumnConfig{
		{Name: "ATTEMPT_TIME", Align: AlignRight},
		{
			Name:        "CHAN_OUT",
			Align:       AlignLeft,
			AlignHeader: AlignLeft,
		},
	})
	w.AppendRow(Row{"SUCCEEDED", "0.123", uint64(8675309)})
	w.AppendRow(Row{"FAILED", "12.000", uint64(42)})

	var buf bytes.Buffer
	w.SetOutputMirror(&buf)
	w.Render()

	want := strings.Join([]string{
		"+------------+--------------+----------+",
		"| HTLC_STATE | ATTEMPT_TIME | CHAN_OUT |",
		"+------------+--------------+----------+",
		"| SUCCEEDED  |        0.123 | 8675309  |",
		"| FAILED     |       12.000 | 42       |",
		"+------------+--------------+----------+",
		"",
	}, "\n")
	require.Equal(t, want, buf.String())
}

// TestRenderNoOutputMirrorIsNoOp asserts that calling Render before
// SetOutputMirror does not panic. go-pretty's writer is a no-op in
// the same situation; downstream code occasionally builds a writer
// conditionally and we want the same forgiving behavior.
func TestRenderNoOutputMirrorIsNoOp(t *testing.T) {
	t.Parallel()

	w := NewWriter()
	w.AppendHeader(Row{"a"})
	w.AppendRow(Row{"b"})
	require.NotPanics(t, w.Render)
}

// TestAutoAlignNumericVsString covers the auto-detection that picks
// AlignRight for numeric kinds and AlignLeft for strings when no
// explicit ColumnConfig has been set. This mirrors go-pretty's
// StyleDefault and is what the trackpayment table relies on for its
// AMT / FEE / TIMELOCK columns.
func TestAutoAlignNumericVsString(t *testing.T) {
	t.Parallel()

	w := NewWriter()
	w.AppendHeader(Row{"NAME", "AMT"})
	w.AppendRow(Row{"alice", int64(1000)})
	w.AppendRow(Row{"bob", int64(20)})

	var buf bytes.Buffer
	w.SetOutputMirror(&buf)
	w.Render()

	// "alice" and "bob" should be left-flush in column 1; 1000
	// and 20 should be right-flush in column 2.
	got := buf.String()
	require.Contains(t, got, "| alice |")
	require.Contains(t, got, "| bob   |")
	require.Contains(t, got, "| 1000 |")
	require.Contains(t, got, "|   20 |")
}

// TestRowShorterThanHeader handles the degenerate case where a body
// row has fewer cells than the header. The missing cells render as
// empty space rather than crashing.
func TestRowShorterThanHeader(t *testing.T) {
	t.Parallel()

	w := NewWriter()
	w.AppendHeader(Row{"A", "B", "C"})
	w.AppendRow(Row{"x", "y"})

	var buf bytes.Buffer
	w.SetOutputMirror(&buf)
	require.NotPanics(t, w.Render)

	require.Contains(t, buf.String(), "| x | y |   |")
}

// TestStringerValue confirms that fmt.Stringer values render via
// their String() method, the same way go-pretty's default formatter
// does. lncli passes proto enum values into the table that satisfy
// fmt.Stringer for their human-readable name.
func TestStringerValue(t *testing.T) {
	t.Parallel()

	w := NewWriter()
	w.AppendHeader(Row{"S"})
	w.AppendRow(Row{stringer{"hello"}})

	var buf bytes.Buffer
	w.SetOutputMirror(&buf)
	w.Render()

	require.Contains(t, buf.String(), "| hello |")
}

type stringer struct{ s string }

func (s stringer) String() string { return s.s }
