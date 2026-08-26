package ui

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/hammondus/gaze/internal/devices"
	"github.com/hammondus/gaze/internal/metrics"
)

// panel is one titled block of network, disk, filesystem, or sensor readings.
//
// Panels are built independently of where they end up. A wide terminal stacks
// them down the sidebar; a narrow one flows them across the band above the
// tables. Neither arrangement is the panel's business.
type panel struct {
	title string
	note  string   // optional aside, drawn muted at the end of the title line
	head  string   // optional column headings, drawn muted under the title
	rows  []string // already styled and padded to the panel's width
}

// render draws a panel: a coloured title, then the rows.
//
// The note shares the title's line, right-aligned, and is dropped rather than
// crowded when the panel is too narrow to carry both. It says what the panel is
// not showing, so it belongs beside the title and not in a row: a row spent
// saying "12 hidden" is a row not spent on a device.
func (p panel) render(width int) string {
	var b strings.Builder
	if n := utf8.RuneCountInString(p.note); n > 0 && utf8.RuneCountInString(p.title)+2+n <= width {
		b.WriteString(styTitle.Render(pad(p.title, width-n)) + styLabel.Render(p.note))
	} else {
		b.WriteString(styTitle.Render(pad(p.title, width)))
	}
	if p.head != "" {
		b.WriteString("\n" + styLabel.Render(pad(p.head, width)))
	}
	for _, r := range p.rows {
		b.WriteString("\n" + r)
	}
	return b.String()
}

// flow arranges panels into columns and returns the assembled band.
//
// Panels are dealt across the columns in order rather than balanced by height.
// A stable position matters more than an even block: you learn where the disk
// panel is and stop reading titles.
//
// Panels sharing a stack position are padded to a common height, so the second
// row of panels starts on one line across the whole band. Without that, a
// column whose first panel happens to have three rows pushes its second panel
// out of step with its neighbours and the band reads as a jumble.
func flow(panels []panel, cols, width, colWidth, gap int) string {
	if len(panels) == 0 || cols < 1 || width < colWidth {
		return ""
	}
	if cols > len(panels) {
		cols = len(panels)
	}

	// Height of the tallest panel in each stack row.
	stackHeight := make([]int, (len(panels)+cols-1)/cols)
	for i, p := range panels {
		if h := len(p.rows); h > stackHeight[i/cols] {
			stackHeight[i/cols] = h
		}
	}

	blocks := make([]string, cols)
	for i, p := range panels {
		c := i % cols
		if blocks[c] != "" {
			blocks[c] += "\n\n"
		}
		blocks[c] += p.render(colWidth)
		for pad := len(p.rows); pad < stackHeight[i/cols]; pad++ {
			blocks[c] += "\n"
		}
	}

	sty := lipgloss.NewStyle().Width(colWidth).MaxWidth(colWidth)
	joined := make([]string, 0, cols*2)
	for i, b := range blocks {
		if i > 0 {
			joined = append(joined, strings.Repeat(" ", gap))
		}
		joined = append(joined, sty.Render(b))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, joined...)
}

// netPanel lists interfaces by how busy they are.
//
// Interfaces that are down and idle are dropped: a laptop carries a dozen
// virtual interfaces that never move a byte, and listing them buries the one
// you care about. Unless showVirtual is set, so are the bridges and veths a
// container runtime builds, whatever they are carrying — see internal/devices.
// The heading's blank space carries a sparkline of the summed rate across all
// devices, because the instantaneous figures alone mislead: buffered disk
// writeback arrives as a sub-second burst every dirty-expiry interval, so the
// write column reads 0 for almost every refresh of a transfer that is in fact
// keeping up. The history is aggregate rather than per device — a device row
// has no width to spare in a thirty-column sidebar, and a second line per
// device would halve the panel's capacity.
func netPanel(s metrics.Snapshot, hist *ring, w, maxRows int, showVirtual bool) panel {
	type row struct {
		n     metrics.Network
		total float64
	}
	var rows []row
	hidden := 0
	for _, n := range s.Networks {
		if !n.Up && n.RxRate == 0 && n.TxRate == 0 {
			continue
		}
		// Counted after the idle test, so the tally is what V would bring
		// back, not every virtual name the kernel knows about.
		if !showVirtual && devices.VirtualNet(n.Name) {
			hidden++
			continue
		}
		rows = append(rows, row{n, n.RxRate + n.TxRate})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].total != rows[j].total {
			return rows[i].total > rows[j].total
		}
		return rows[i].n.Name < rows[j].n.Name
	})

	nameW := w - 18
	p := panel{title: "NETWORK", note: hiddenNote(hidden),
		head: pad(hist.sparkRunes(nameW-1, 0), nameW) + padLeft("rx", 8) + padLeft("tx", 9)}
	for i, r := range rows {
		if i >= maxRows {
			break
		}
		p.rows = append(p.rows, styText.Render(pad(r.n.Name, nameW))+
			styText.Render(padLeft(rate(r.n.RxRate), 8))+
			styText.Render(padLeft(rate(r.n.TxRate), 9)))
	}
	return p
}

// diskPanel lists block devices by how busy they are. Unless showVirtual is
// set, the loop and ram devices are left out — see internal/devices.
func diskPanel(s metrics.Snapshot, hist *ring, w, maxRows int, showVirtual bool) panel {
	disks := make([]metrics.Disk, 0, len(s.Disks))
	hidden := 0
	for _, d := range s.Disks {
		if !showVirtual && devices.VirtualDisk(d.Name) {
			hidden++
			continue
		}
		disks = append(disks, d)
	}
	sort.Slice(disks, func(i, j int) bool {
		a := disks[i].ReadRate + disks[i].WriteRate
		b := disks[j].ReadRate + disks[j].WriteRate
		if a != b {
			return a > b
		}
		return disks[i].Name < disks[j].Name
	})

	nameW := w - 18
	p := panel{title: "DISK I/O", note: hiddenNote(hidden),
		head: pad(hist.sparkRunes(nameW-1, 0), nameW) + padLeft("read", 8) + padLeft("write", 9)}
	for i, d := range disks {
		if i >= maxRows {
			break
		}
		p.rows = append(p.rows, styText.Render(pad(d.Name, nameW))+
			styText.Render(padLeft(rate(d.ReadRate), 8))+
			styText.Render(padLeft(rate(d.WriteRate), 9)))
	}
	return p
}

// fsPanel lists mounted filesystems, fullest first, since a filesystem is only
// interesting as it fills.
func fsPanel(s metrics.Snapshot, w, maxRows int) panel {
	mounts := append([]metrics.Mount(nil), s.Mounts...)
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].Percent > mounts[j].Percent })

	// A percentage is never wider than "100%" and a byte count never wider than
	// "1023B", so the two figures are given what they can use and the rest goes
	// to the path. In a sidebar barely thirty columns wide, every column the
	// numbers do not need is one that decides whether a mount is identifiable.
	pathW := w - 14
	p := panel{title: "FILESYSTEM", head: pad("", pathW) + padLeft("used", 6) + padLeft("free", 7)}
	for i, m := range mounts {
		if i >= maxRows {
			break
		}
		p.rows = append(p.rows, styText.Render(pad(elide(m.Path, pathW), pathW))+
			thDisk.styleFor(m.Percent).Render(padLeft(percent(m.Percent), 6))+
			styText.Render(padLeft(bytes(m.Free), 7)))
	}
	return p
}

// sensorPanel lists hardware readings.
//
// A chip that publishes its own high and critical points is judged against
// those rather than against a guess: the safe temperature of a CPU package and
// of an NVMe drive are not the same number.
func sensorPanel(s metrics.Snapshot, w, maxRows int) panel {
	labelW := w - 9
	p := panel{title: "SENSORS", head: pad("", labelW) + padLeft("value", 9)}
	for i, sen := range s.Sensors {
		if i >= maxRows {
			break
		}
		var val string
		var sty = styText
		switch sen.Kind {
		case metrics.SensorTemp:
			val = percentless(sen.Value) + "°C"
			sty = tempThresholds(sen).styleFor(sen.Value)
		case metrics.SensorFan:
			val = count(sen.Value) + "r"
		case metrics.SensorBattery:
			val = percent(sen.Value)
			// A battery is the one reading where low is the problem, so its
			// scale is inverted before the shared threshold test.
			sty = thresholds{60, 85}.styleFor(100 - sen.Value)
		}
		p.rows = append(p.rows, styText.Render(pad(sen.Label, labelW))+sty.Render(padLeft(val, 9)))
	}
	return p
}

// tempThresholds prefers the chip's published limits over the built-in guess.
func tempThresholds(s metrics.Sensor) thresholds {
	t := thTemp
	if s.High > 0 {
		t.warn = s.High
	}
	if s.Crit > 0 {
		t.crit = s.Crit
	}
	if t.warn >= t.crit {
		t.warn = t.crit * 0.85
	}
	return t
}

// percentless renders a bare number to one decimal, for units that are not
// percentages.
func percentless(v float64) string {
	return trimZero(v)
}

// hiddenNote is the aside a panel carries when it has left devices out. A
// monitor that quietly drops rows is worse than one that shows them all, so the
// count and the key that brings them back are on screen wherever it happens.
func hiddenNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d hidden (V)", n)
}
