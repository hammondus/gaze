package ui

import (
	"slices"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// column is one field of a table.
//
// minWidth is the terminal width below which the column is dropped. The value
// says how much the column earns its space: on a narrow terminal you want to
// know what is running and what it costs, and everything else can go.
//
// key identifies the sort column. It is an int rather than a named type
// because two tables sort by different sets, and both convert their own enum
// on the way in.
type column[T any] struct {
	label    string
	width    int
	minWidth int
	key      int
	sortable bool
	right    bool
	value    func(T) string
	style    func(T) lipgloss.Style
}

// table lays out rows as aligned columns that drop as the terminal narrows.
//
// The final column flexes to fill whatever is left, because the thing worth
// reading in full — a command line, a mount path — is always last.
type table[T any] struct {
	cols    []column[T]
	flex    column[T]
	minFlex int
}

// withAbsent returns a copy of the table whose named columns render a dash in
// every row. It is how a table honours Snapshot.Absent: the platform could not
// supply the value, and a dash says so where a zero would lie.
func (t table[T]) withAbsent(labels ...string) table[T] {
	cols := make([]column[T], len(t.cols))
	copy(cols, t.cols)
	for i := range cols {
		if slices.Contains(labels, cols[i].label) {
			cols[i].value = func(T) string { return "-" }
			cols[i].style = func(T) lipgloss.Style { return styFaint }
		}
	}
	t.cols = cols
	return t
}

// active returns the columns that fit and the width left for the flexible one.
//
// Columns are dropped from the least useful upwards until the flexible column
// has room, so the table degrades rather than wraps.
func (t table[T]) active(width int) ([]column[T], int) {
	cols := make([]column[T], 0, len(t.cols))
	for _, c := range t.cols {
		if width >= c.minWidth {
			cols = append(cols, c)
		}
	}
	for {
		used := 0
		for _, c := range cols {
			used += c.width + 1
		}
		flexW := width - used
		if flexW >= t.minFlex || len(cols) <= 1 {
			return cols, max(0, flexW)
		}
		worst := 0
		for i := range cols {
			if cols[i].minWidth > cols[worst].minWidth {
				worst = i
			}
		}
		cols = append(cols[:worst], cols[worst+1:]...)
	}
}

// header renders the column labels, marking the active sort column so the
// ordering is visible without reading the footer.
func (t table[T]) header(width, activeKey int) string {
	cols, flexW := t.active(width)
	var b strings.Builder
	for _, c := range cols {
		label, sty := c.label, styLabel
		if c.sortable && c.key == activeKey {
			// The marker eats one column of the label rather than widening
			// the field, so the numbers below stay put when the sort changes.
			label, sty = "▾"+label, styTitle
		}
		if c.right {
			b.WriteString(sty.Render(padLeft(label, c.width)))
		} else {
			b.WriteString(sty.Render(pad(label, c.width)))
		}
		b.WriteString(" ")
	}
	b.WriteString(styLabel.Render(pad(t.flex.label, flexW)))
	return b.String()
}

// row renders one row.
//
// The fields are laid out as plain text first and styled afterwards. A
// selected row is one background colour across its whole width, and mixing a
// background into strings that already carry foreground escapes leaves gaps
// wherever a colour resets.
func (t table[T]) row(v T, width int, selected bool) string {
	cols, flexW := t.active(width)

	fields := make([]string, 0, len(cols)+1)
	for _, c := range cols {
		if c.right {
			fields = append(fields, padLeft(c.value(v), c.width))
		} else {
			fields = append(fields, pad(c.value(v), c.width))
		}
	}
	fields = append(fields, pad(t.flex.value(v), flexW))

	if selected {
		return styRowSel.Render(strings.Join(fields, " "))
	}
	var b strings.Builder
	for i, c := range cols {
		b.WriteString(c.style(v).Render(fields[i]))
		b.WriteString(" ")
	}
	b.WriteString(t.flex.style(v).Render(fields[len(fields)-1]))
	return b.String()
}

// render draws the header and the visible slice of rows, keeping the cursor on
// screen. It returns the finished lines and the offset it settled on, so the
// caller can remember where the list was scrolled to.
func (t table[T]) render(rows []T, width, height, activeKey, cursor, offset int) ([]string, int) {
	if height < 1 {
		return nil, offset
	}
	body := height - 1 // the header takes one line
	cursor = min(cursor, max(0, len(rows)-1))
	if cursor < offset {
		offset = cursor
	}
	if cursor >= offset+body {
		offset = cursor - body + 1
	}
	offset = max(0, min(offset, max(0, len(rows)-body)))

	out := make([]string, 0, height)
	out = append(out, t.header(width, activeKey))
	for i := offset; i < len(rows) && i < offset+body; i++ {
		out = append(out, t.row(rows[i], width, i == cursor))
	}
	return out, offset
}
