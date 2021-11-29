package table

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jedib0t/go-pretty/text"
)

// Render renders the Table in a human-readable "pretty" format. Example:
//  ┌─────┬────────────┬───────────┬────────┬─────────────────────────────┐
//  │   # │ FIRST NAME │ LAST NAME │ SALARY │                             │
//  ├─────┼────────────┼───────────┼────────┼─────────────────────────────┤
//  │   1 │ Arya       │ Stark     │   3000 │                             │
//  │  20 │ Jon        │ Snow      │   2000 │ You know nothing, Jon Snow! │
//  │ 300 │ Tyrion     │ Lannister │   5000 │                             │
//  ├─────┼────────────┼───────────┼────────┼─────────────────────────────┤
//  │     │            │ TOTAL     │  10000 │                             │
//  └─────┴────────────┴───────────┴────────┴─────────────────────────────┘
func (t *Table) Render() string {
	t.initForRender()

	var out strings.Builder
	if t.numColumns > 0 {
		t.renderTitle(&out)

		// top-most border
		t.renderRowsBorderTop(&out)

		// header rows
		t.renderRowsHeader(&out)

		// (data) rows
		t.renderRows(&out, t.rows, renderHint{})

		// footer rows
		t.renderRowsFooter(&out)

		// bottom-most border
		t.renderRowsBorderBottom(&out)

		// caption
		if t.caption != "" {
			out.WriteRune('\n')
			out.WriteString(t.caption)
		}
	}
	return t.render(&out)
}

func (t *Table) renderColumn(out *strings.Builder, row rowStr, colIdx int, maxColumnLength int, hint renderHint) {
	// when working on the first column, and autoIndex is true, insert a new
	// column with the row number on it.
	if colIdx == 0 && t.autoIndex {
		t.renderColumnAutoIndex(out, hint)
	}

	// when working on column number 2 or more, render the column separator
	if colIdx > 0 {
		t.renderColumnSeparator(out, hint)
	}

	// extract the text, convert-case if not-empty and align horizontally
	var colStr string
	if colIdx < len(row) {
		colStr = t.getFormat(hint).Apply(row[colIdx])
	}
	colStr = t.getAlign(colIdx, hint).Apply(colStr, maxColumnLength)

	// pad both sides of the column (when not a separator row)
	if !hint.isSeparatorRow {
		colStr = t.style.Box.PaddingLeft + colStr + t.style.Box.PaddingRight
	}

	t.renderColumnColorized(out, colIdx, colStr, hint)
}

func (t *Table) renderColumnAutoIndex(out *strings.Builder, hint renderHint) {
	var outAutoIndex strings.Builder
	outAutoIndex.Grow(t.maxColumnLengths[0])

	if hint.isSeparatorRow {
		numChars := t.autoIndexVIndexMaxLength + utf8.RuneCountInString(t.style.Box.PaddingLeft) +
			utf8.RuneCountInString(t.style.Box.PaddingRight)
		outAutoIndex.WriteString(text.RepeatAndTrim(t.style.Box.MiddleHorizontal, numChars))
	} else {
		outAutoIndex.WriteString(t.style.Box.PaddingLeft)
		rowNumStr := fmt.Sprint(hint.rowNumber)
		if hint.isHeaderRow || hint.isFooterRow || hint.rowLineNumber > 1 {
			rowNumStr = strings.Repeat(" ", t.autoIndexVIndexMaxLength)
		}
		outAutoIndex.WriteString(text.AlignRight.Apply(rowNumStr, t.autoIndexVIndexMaxLength))
		outAutoIndex.WriteString(t.style.Box.PaddingRight)
	}

	if t.style.Color.IndexColumn != nil {
		colors := t.style.Color.IndexColumn
		if hint.isFooterRow {
			colors = t.style.Color.Footer
		}
		out.WriteString(colors.Sprint(outAutoIndex.String()))
	} else {
		out.WriteString(outAutoIndex.String())
	}
	hint.isAutoIndexColumn = true
	t.renderColumnSeparator(out, hint)
}

func (t *Table) renderColumnColorized(out *strings.Builder, colIdx int, colStr string, hint renderHint) {
	colors := t.getColumnColors(colIdx, hint)
	if colors != nil {
		out.WriteString(colors.Sprint(colStr))
	} else if hint.isHeaderRow && t.style.Color.Header != nil {
		out.WriteString(t.style.Color.Header.Sprint(colStr))
	} else if hint.isFooterRow && t.style.Color.Footer != nil {
		out.WriteString(t.style.Color.Footer.Sprint(colStr))
	} else if hint.isRegularRow() {
		if colIdx == t.indexColumn-1 && t.style.Color.IndexColumn != nil {
			out.WriteString(t.style.Color.IndexColumn.Sprint(colStr))
		} else if hint.rowNumber%2 == 0 && t.style.Color.RowAlternate != nil {
			out.WriteString(t.style.Color.RowAlternate.Sprint(colStr))
		} else if t.style.Color.Row != nil {
			out.WriteString(t.style.Color.Row.Sprint(colStr))
		} else {
			out.WriteString(colStr)
		}
	} else {
		out.WriteString(colStr)
	}
}

func (t *Table) renderColumnSeparator(out *strings.Builder, hint renderHint) {
	if t.style.Options.SeparateColumns {
		separator := t.style.Box.MiddleVertical
		if hint.isSeparatorRow {
			if hint.isBorderTop {
				separator = t.style.Box.TopSeparator
			} else if hint.isBorderBottom {
				separator = t.style.Box.BottomSeparator
			} else {
				separator = t.style.Box.MiddleSeparator
			}
		}

		colors := t.getSeparatorColors(hint)
		if colors.EscapeSeq() != "" {
			out.WriteString(colors.Sprint(separator))
		} else {
			out.WriteString(separator)
		}
	}
}

func (t *Table) renderLine(out *strings.Builder, row rowStr, hint renderHint) {
	// if the output has content, it means that this call is working on line
	// number 2 or more; separate them with a newline
	if out.Len() > 0 {
		out.WriteRune('\n')
	}

	// use a brand new strings.Builder if a row length limit has been set
	var outLine *strings.Builder
	if t.allowedRowLength > 0 {
		outLine = &strings.Builder{}
	} else {
		outLine = out
	}
	// grow the strings.Builder to the maximum possible row length
	outLine.Grow(t.maxRowLength)

	t.renderMarginLeft(outLine, hint)
	for colIdx, maxColumnLength := range t.maxColumnLengths {
		t.renderColumn(outLine, row, colIdx, maxColumnLength, hint)
	}
	t.renderMarginRight(outLine, hint)

	// merge the strings.Builder objects if a new one was created earlier
	if outLine != out {
		outLineStr := outLine.String()
		if text.RuneCount(outLineStr) > t.allowedRowLength {
			trimLength := t.allowedRowLength - utf8.RuneCountInString(t.style.Box.UnfinishedRow)
			if trimLength > 0 {
				out.WriteString(text.Trim(outLineStr, trimLength))
				out.WriteString(t.style.Box.UnfinishedRow)
			}
		} else {
			out.WriteString(outLineStr)
		}
	}

	// if a page size has been set, and said number of lines has already
	// been rendered, and the header is not being rendered right now, render
	// the header all over again with a spacing line
	if hint.isRegularRow() {
		t.numLinesRendered++
		if t.pageSize > 0 && t.numLinesRendered%t.pageSize == 0 && !hint.isLastLineOfLastRow() {
			t.renderRowsFooter(out)
			t.renderRowsBorderBottom(out)
			out.WriteString(t.style.Box.PageSeparator)
			t.renderRowsBorderTop(out)
			t.renderRowsHeader(out)
		}
	}
}

func (t *Table) renderMarginLeft(out *strings.Builder, hint renderHint) {
	if t.style.Options.DrawBorder {
		border := t.style.Box.Left
		if hint.isBorderTop {
			if t.title != "" {
				border = t.style.Box.LeftSeparator
			} else {
				border = t.style.Box.TopLeft
			}
		} else if hint.isBorderBottom {
			border = t.style.Box.BottomLeft
		} else if hint.isSeparatorRow {
			border = t.style.Box.LeftSeparator
		}

		colors := t.getBorderColors(hint)
		if colors.EscapeSeq() != "" {
			out.WriteString(colors.Sprint(border))
		} else {
			out.WriteString(border)
		}
	}
}

func (t *Table) renderMarginRight(out *strings.Builder, hint renderHint) {
	if t.style.Options.DrawBorder {
		border := t.style.Box.Right
		if hint.isBorderTop {
			if t.title != "" {
				border = t.style.Box.RightSeparator
			} else {
				border = t.style.Box.TopRight
			}
		} else if hint.isBorderBottom {
			border = t.style.Box.BottomRight
		} else if hint.isSeparatorRow {
			border = t.style.Box.RightSeparator
		}

		colors := t.getBorderColors(hint)
		if colors.EscapeSeq() != "" {
			out.WriteString(colors.Sprint(border))
		} else {
			out.WriteString(border)
		}
	}
}

func (t *Table) renderRow(out *strings.Builder, rowNum int, row rowStr, hint renderHint) {
	if len(row) > 0 {
		// fit every column into the allowedColumnLength/maxColumnLength limit
		// and in the process find the max. number of lines in any column in
		// this row
		colMaxLines := 0
		rowWrapped := make(rowStr, len(row))
		for colIdx, colStr := range row {
			rowWrapped[colIdx] = text.WrapText(colStr, t.maxColumnLengths[colIdx])
			colNumLines := strings.Count(rowWrapped[colIdx], "\n") + 1
			if colNumLines > colMaxLines {
				colMaxLines = colNumLines
			}
		}

		// if there is just 1 line in all columns, add the row as such; else
		// split each column into individual lines and render them one-by-one
		if colMaxLines == 1 {
			hint.isLastLineOfRow = true
			t.renderLine(out, row, hint)
		} else {
			// convert one row into N # of rows based on colMaxLines
			rowLines := make([]rowStr, len(row))
			for colIdx, colStr := range rowWrapped {
				rowLines[colIdx] = t.getVAlign(colIdx, hint).ApplyStr(colStr, colMaxLines)
			}
			for colLineIdx := 0; colLineIdx < colMaxLines; colLineIdx++ {
				rowLine := make(rowStr, len(rowLines))
				for colIdx, colLines := range rowLines {
					rowLine[colIdx] = colLines[colLineIdx]
				}
				hint.isLastLineOfRow = bool(colLineIdx == colMaxLines-1)
				hint.rowLineNumber = colLineIdx + 1
				t.renderLine(out, rowLine, hint)
			}
		}
	}
}

func (t *Table) renderRowSeparator(out *strings.Builder, hint renderHint) {
	if hint.isBorderTop || hint.isBorderBottom {
		if !t.style.Options.DrawBorder {
			return
		}
	} else if hint.isHeaderRow && !t.style.Options.SeparateHeader {
		return
	} else if hint.isFooterRow && !t.style.Options.SeparateFooter {
		return
	}
	hint.isSeparatorRow = true
	hint.rowNumber = -1
	t.renderLine(out, t.rowSeparator, hint)
}

func (t *Table) renderRows(out *strings.Builder, rows []rowStr, hint renderHint) {
	hintSeparator := hint
	hintSeparator.isSeparatorRow = true

	for idx, row := range rows {
		hint.isFirstRow = bool(idx == 0)
		hint.isLastRow = bool(idx == len(rows)-1)
		hint.rowNumber = idx + 1

		t.renderRow(out, idx+1, row, hint)
		if t.style.Options.SeparateRows && idx < len(rows)-1 {
			t.renderRowSeparator(out, hintSeparator)
		}
	}
}

func (t *Table) renderRowsBorderBottom(out *strings.Builder) {
	t.renderRowSeparator(out, renderHint{isBorderBottom: true, isFooterRow: true})
}

func (t *Table) renderRowsBorderTop(out *strings.Builder) {
	t.renderRowSeparator(out, renderHint{isBorderTop: true, isHeaderRow: true})
}

func (t *Table) renderRowsFooter(out *strings.Builder) {
	if len(t.rowsFooter) > 0 {
		t.renderRowSeparator(out, renderHint{isFooterRow: true, isSeparatorRow: true})
		t.renderRows(out, t.rowsFooter, renderHint{isFooterRow: true})
	}
}

func (t *Table) renderRowsHeader(out *strings.Builder) {
	// header rows or auto-index row
	if len(t.rowsHeader) > 0 || t.autoIndex {
		if len(t.rowsHeader) > 0 {
			t.renderRows(out, t.rowsHeader, renderHint{isHeaderRow: true})
		} else if t.autoIndex {
			t.renderRow(out, 0, t.getAutoIndexColumnIDs(), renderHint{isHeaderRow: true})
		}
		t.renderRowSeparator(out, renderHint{isHeaderRow: true, isSeparatorRow: true})
	}
}

func (t *Table) renderTitle(out *strings.Builder) {
	if t.title != "" {
		if t.style.Options.DrawBorder {
			lenBorder := t.maxRowLength - text.RuneCount(t.style.Box.TopLeft+t.style.Box.TopRight)
			out.WriteString(t.style.Box.TopLeft)
			out.WriteString(text.RepeatAndTrim(t.style.Box.MiddleHorizontal, lenBorder))
			out.WriteString(t.style.Box.TopRight)
		}

		lenText := t.maxRowLength - text.RuneCount(t.style.Box.PaddingLeft+t.style.Box.PaddingRight)
		if t.style.Options.DrawBorder {
			lenText -= text.RuneCount(t.style.Box.Left + t.style.Box.Right)
		}
		titleText := text.WrapText(t.title, lenText)
		for _, titleLine := range strings.Split(titleText, "\n") {
			titleLine = strings.TrimSpace(titleLine)
			titleLine = t.style.Title.Format.Apply(titleLine)
			titleLine = t.style.Title.Align.Apply(titleLine, lenText)
			titleLine = t.style.Box.PaddingLeft + titleLine + t.style.Box.PaddingRight
			titleLine = t.style.Title.Colors.Sprint(titleLine)

			if out.Len() > 0 {
				out.WriteRune('\n')
			}
			if t.style.Options.DrawBorder {
				out.WriteString(t.style.Box.Left)
			}
			out.WriteString(titleLine)
			if t.style.Options.DrawBorder {
				out.WriteString(t.style.Box.Right)
			}
		}
	}
}
