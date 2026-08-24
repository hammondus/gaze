// Package ui renders a system snapshot as a full-screen terminal dashboard.
//
// The split with the metrics package is strict: metrics decides what the
// numbers are, ui decides what they look like. The only state kept here is
// display state — window size, sort order, and the short history behind the
// sparklines.
package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/hammondus/gaze/internal/metrics"
)

// historyLen is how many samples the sparklines keep. At the default one
// second refresh that is a minute of history, which is long enough to show a
// spike you just missed and short enough to stay narrow.
const historyLen = 60

// collectTimeout bounds one collection. Reading /proc cannot hang, but the
// Docker socket can, and a wedged daemon must not freeze the display.
const collectTimeout = 3 * time.Second

// viewMode is which of the three screens is showing.
//
// The container views are cycled with one key rather than bound to three,
// because they are points on a single axis: how much of the screen containers
// deserve. On a host running none, the cycle still works and the container
// screens say so.
type viewMode int

const (
	// viewDashboard is the default: containers get one panel in the band,
	// beside network and disk.
	viewDashboard viewMode = iota
	// viewSplit gives containers a full-width table above the process list.
	viewSplit
	// viewContainers replaces the process list with the container table, and
	// is the only view that shows containers which are not running.
	viewContainers
)

func (v viewMode) name() string {
	return [...]string{"dashboard", "split", "containers"}[v]
}

// next cycles to the following view.
func (v viewMode) next() viewMode { return (v + 1) % 3 }

// snapshotMsg carries a finished collection back to the update loop.
type snapshotMsg metrics.Snapshot

// tickMsg schedules the next collection.
type tickMsg struct{}

// Model is the Bubble Tea model for the dashboard.
type Model struct {
	col      *metrics.Collector
	snap     metrics.Snapshot
	interval time.Duration

	width, height int
	ready         bool

	view       viewMode
	sort       sortKey
	ctrSort    ctrSort
	hideKernel bool
	filter     string
	typing     bool // the filter prompt has the keyboard
	paused     bool
	perCore    bool
	showHelp   bool
	offset     int // first process row on screen
	cursor     int // selected process row, as an index into the filtered list
	ctrOff     int // first container row on screen
	ctrCur     int // selected container row

	cpuHist  *ring
	memHist  *ring
	loadHist *ring
	swapHist *ring
	netHist  *ring
	diskHist *ring
}

// New returns a dashboard model refreshing at the given interval.
//
// Kernel threads start hidden. They run entirely inside the kernel, hold no
// memory, and on a typical host outnumber everything you started by an order
// of magnitude, so showing them by default buries the process table. The
// context line always reports how many are hidden, and K brings them back.
func New(col *metrics.Collector, interval time.Duration) Model {
	return Model{
		col:        col,
		interval:   interval,
		sort:       sortCPU,
		hideKernel: true,
		cpuHist:    newRing(historyLen),
		memHist:    newRing(historyLen),
		loadHist:   newRing(historyLen),
		swapHist:   newRing(historyLen),
		netHist:    newRing(historyLen),
		diskHist:   newRing(historyLen),
	}
}

// Init starts the first collection straight away, so the screen fills without
// waiting out an interval.
func (m Model) Init() tea.Cmd { return m.collect() }

// collect runs one collection off the update loop and delivers the result as a
// message.
//
// Collections are chained rather than run on a fixed timer: the next tick is
// only scheduled once a snapshot arrives. That guarantees one collection at a
// time, which the collector requires, and means a machine too slow to keep up
// refreshes less often instead of queueing work it will never finish.
func (m Model) collect() tea.Cmd {
	col := m.col
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
		defer cancel()
		return snapshotMsg(col.Collect(ctx))
	}
}

// tick schedules the next collection.
func (m Model) tick() tea.Cmd {
	return tea.Tick(m.interval, func(time.Time) tea.Msg { return tickMsg{} })
}

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height, m.ready = msg.Width, msg.Height, true
		return m, nil

	case snapshotMsg:
		m.snap = metrics.Snapshot(msg)
		m.record()
		return m, m.tick()

	case tickMsg:
		if m.paused {
			return m, m.tick()
		}
		return m, m.collect()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// record pushes the newest sample into the sparkline histories.
func (m *Model) record() {
	m.cpuHist.push(m.snap.CPU.Busy)
	m.memHist.push(m.snap.Memory.Percent)
	m.loadHist.push(m.loadPercent())
	m.swapHist.push(m.snap.Swap.Percent)

	var net, disk float64
	for _, n := range m.snap.Networks {
		net += n.RxRate + n.TxRate
	}
	for _, d := range m.snap.Disks {
		disk += d.ReadRate + d.WriteRate
	}
	m.netHist.push(net)
	m.diskHist.push(disk)
}

// handleKey applies a keystroke.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// While the filter prompt is open it takes every key but escape and
	// enter, so a process name containing "q" does not quit the program.
	if m.typing {
		switch msg.Type {
		case tea.KeyEsc:
			m.typing, m.filter = false, ""
		case tea.KeyEnter:
			m.typing = false
		case tea.KeyBackspace:
			if n := len(m.filter); n > 0 {
				m.filter = m.filter[:n-1]
			}
		case tea.KeyRunes, tea.KeySpace:
			m.filter += string(msg.Runes)
		case tea.KeyCtrlC:
			return m, tea.Quit
		}
		m.cursor, m.offset = 0, 0
		return m, nil
	}

	switch msg.String() {
	case "q", "esc", "ctrl+c":
		return m, tea.Quit
	// The sort keys act on whichever table is on screen, so c always means
	// "order by CPU" wherever you are.
	case "c":
		m.sort, m.ctrSort = sortCPU, ctrCPU
	case "m":
		m.sort, m.ctrSort = sortMem, ctrMem
	case "s":
		m.sort = sortSwap
	case "t":
		m.sort, m.ctrSort = sortTime, ctrUptime
	case "i":
		m.ctrSort = ctrIO
	case "p":
		m.sort = sortPID
	case "n":
		m.sort, m.ctrSort = sortName, ctrName
	case "u":
		m.sort = sortUser
	case "v":
		m.view = m.view.next()
		m.cursor, m.offset = 0, 0
		m.ctrCur, m.ctrOff = 0, 0
	case "1":
		m.perCore = !m.perCore
	case "K":
		// Lower-case k moves the cursor, so this follows htop and takes the
		// shifted key.
		m.hideKernel = !m.hideKernel
		m.cursor, m.offset = 0, 0
	case "?", "h":
		m.showHelp = !m.showHelp
	case " ":
		m.paused = !m.paused
	case "/":
		m.typing, m.filter = true, ""
	case "+", "=":
		m.interval = clampInterval(m.interval * 2)
	case "-", "_":
		m.interval = clampInterval(m.interval / 2)
	case "up", "k":
		m.moveCursor(-1)
	case "down", "j":
		m.moveCursor(1)
	case "pgup":
		m.moveCursor(-m.layout().rows())
	case "pgdown":
		m.moveCursor(m.layout().rows())
	case "home", "g":
		if m.view == viewContainers {
			m.ctrCur = 0
		} else {
			m.cursor = 0
		}
	}
	return m, nil
}

// moveCursor moves the cursor of the list the current view puts at the bottom
// of the screen.
func (m *Model) moveCursor(delta int) {
	if m.view == viewContainers {
		m.ctrCur = max(0, m.ctrCur+delta)
		return
	}
	m.cursor = max(0, m.cursor+delta)
}

// clampInterval keeps the refresh rate between half a second and ten seconds.
// Below half a second the collection cost starts to show up in its own
// readings, which makes the numbers lie about the machine.
func clampInterval(d time.Duration) time.Duration {
	switch {
	case d < 500*time.Millisecond:
		return 500 * time.Millisecond
	case d > 10*time.Second:
		return 10 * time.Second
	default:
		return d
	}
}

// minProcRows is the smallest process table worth drawing. The middle band
// gives up its rows to protect this: a monitor whose process list is two rows
// tall has stopped being a monitor.
const minProcRows = 5

// layout is the height budget for one frame.
//
// Every region is measured before anything is drawn, because a full-screen
// program that renders one line too many scrolls its own header off the top on
// every refresh. Sizing is done once, here, and both View and the paging keys
// read the result.
type layout struct {
	header, meters, band string
	ctrRows              int // the split view's container table, zero elsewhere
	bottomRows           int // the table filling the rest of the screen
}

// rows returns the height of the list the cursor moves through.
func (l layout) rows() int { return l.bottomRows }

func (m Model) layout() layout {
	l := layout{header: m.header(), meters: m.meters()}

	// The chrome is the header, the gauges, the blank line after each, and
	// the footer. Each table supplies its own heading line.
	used := lipgloss.Height(l.header) + 1 + lipgloss.Height(l.meters) + 1 + 1 + 1

	// The band gets what is left once the bottom table has its minimum, less
	// one line for the blank that follows it.
	l.band = m.band(m.height - used - minProcRows - 1)
	if l.band != "" {
		used += lipgloss.Height(l.band) + 1
	}

	// In the split view the container table sits between the band and the
	// process list, and the two share what remains. Containers take the
	// smaller share: the split view exists to keep an eye on them, not to
	// replace the container view.
	if m.view == viewSplit {
		spare := m.height - used - minProcRows
		if n := len(filterContainers(m.snap.Containers, m.filter, m.ctrSort, false)); n > 0 && spare > 2 {
			l.ctrRows = min(n+1, min(spare-1, m.height/4))
			used += l.ctrRows + 1
		}
	}

	l.bottomRows = max(1, m.height-used)
	return l
}

// View renders the whole screen.
func (m Model) View() string {
	if !m.ready {
		return "starting…"
	}
	if m.showHelp {
		return m.helpView()
	}

	l := m.layout()
	var b strings.Builder
	b.WriteString(l.header)
	b.WriteString("\n\n")
	b.WriteString(l.meters)
	b.WriteString("\n\n")
	if l.band != "" {
		b.WriteString(l.band)
		b.WriteString("\n\n")
	}
	if l.ctrRows > 0 {
		b.WriteString(m.containerTable(l.ctrRows, false))
		b.WriteString("\n\n")
	}
	if m.view == viewContainers {
		b.WriteString(m.containerTable(l.bottomRows, true))
	} else {
		b.WriteString(m.processes(l.bottomRows))
	}
	b.WriteString("\n")
	b.WriteString(m.footer())
	return b.String()
}

// containerTable renders the container list. showStopped is set only for the
// dedicated view, which is the one with room to spare.
func (m Model) containerTable(rows int, showStopped bool) string {
	// Every one of these lines carries colour, so each is clipped by display
	// width rather than by rune count.
	//
	// Switched off and not found are different facts, and reporting one as the
	// other sends you looking for a daemon problem you do not have.
	if m.snap.ContainersDisabled {
		return clipWidth(styLabel.Render("container collection is switched off"), m.width) + "\n" +
			clipWidth(styFaint.Render("restart without -containers=false to enable it"), m.width)
	}
	if m.snap.ContainerRuntime == "" {
		return clipWidth(styLabel.Render("no container runtime reachable"), m.width) + "\n" +
			clipWidth(styFaint.Render("looked for docker.sock and podman.sock"), m.width)
	}
	cs := filterContainers(m.snap.Containers, m.filter, m.ctrSort, showStopped)
	if len(cs) == 0 {
		if m.filter != "" {
			return clipWidth(styLabel.Render("no containers match "+m.filter), m.width)
		}
		return clipWidth(styLabel.Render("no containers running on "+m.snap.ContainerRuntime), m.width)
	}

	// The split view's table is a readout, not a list you move through, so it
	// draws no cursor.
	cursor, offset := m.ctrCur, m.ctrOff
	if !showStopped {
		cursor, offset = -1, 0
	}
	lines, _ := ctrTable.render(cs, m.width, rows, int(m.ctrSort), cursor, offset)
	return strings.Join(lines, "\n")
}

// header renders the title bar.
//
// The left side is assembled from whole parts and shortened by dropping a part
// rather than by cutting a string. A kernel version chopped in the middle
// reads as a different version; an absent one reads as absent.
func (m Model) header() string {
	h := m.snap.Host
	right := styLabel.Render(fmt.Sprintf("%s  %.1fs", m.snap.Taken.Format("15:04:05"), m.interval.Seconds()))
	if m.paused {
		right = styWarn.Render("PAUSED") + "  " + right
	}

	parts := []string{
		styHeader.Render(" gaze ") + " " + styText.Render(h.Hostname),
		styLabel.Render("Linux " + h.Kernel),
		styLabel.Render("up " + uptime(h.Uptime)),
	}
	for n := len(parts); n > 1; n-- {
		left := strings.Join(parts[:n], "  ")
		if lipgloss.Width(left)+lipgloss.Width(right)+1 <= m.width {
			return spread(left, right, m.width)
		}
	}
	return spread(parts[0], right, m.width)
}

// meters renders the gauge band: overall usage on the left, memory on the
// right, with a minute of history beside each.
func (m Model) meters() string {
	if m.perCore {
		return m.perCoreMeters()
	}

	colW := m.width/2 - 1
	if colW < 24 {
		colW = m.width
	}
	sparkW := 0
	if colW >= 46 {
		sparkW = 14
	}
	meterW := colW - sparkW - 2

	line := func(label string, pct float64, th thresholds, hist *ring, peak float64) string {
		s := meter(label, pct, meterW, 5, 6, th)
		if sparkW > 0 {
			s += "  " + hist.spark(sparkW, peak, th.styleFor(pct))
		}
		return s
	}

	leftA := line("CPU", m.snap.CPU.Busy, thCPU, m.cpuHist, 100)
	leftB := line("LOAD", m.loadPercent(), thLoad, m.loadHist, 100)
	rightA := line("MEM", m.snap.Memory.Percent, thMem, m.memHist, 100)
	rightB := line("SWAP", m.snap.Swap.Percent, thSwap, m.swapHist, 100)

	if colW == m.width { // too narrow to sit side by side
		return strings.Join([]string{leftA, rightA, leftB, rightB, m.context()}, "\n")
	}
	col := lipgloss.NewStyle().Width(colW).MaxWidth(colW)
	band := lipgloss.JoinHorizontal(lipgloss.Top,
		col.Render(leftA+"\n"+leftB), "  ", col.Render(rightA+"\n"+rightB))
	return band + "\n" + m.context()
}

// loadPercent expresses the one-minute load average as a share of the
// machine's cores, so it can share a gauge with the percentages beside it. A
// load equal to the core count is 100 percent: the point at which every
// runnable task has a core and the next one waits.
func (m Model) loadPercent() float64 {
	if m.snap.Host.CPUCount == 0 {
		return 0
	}
	return m.snap.Load.One / float64(m.snap.Host.CPUCount) * 100
}

// context is the one-line summary under the gauges: the numbers that do not
// deserve a gauge but are wanted at a glance.
func (m Model) context() string {
	s := m.snap
	tasks := fmt.Sprintf("%d tasks, %d threads", s.ProcCounts.Total, s.ProcCounts.Threads)
	if m.hideKernel && s.ProcCounts.Kernel > 0 {
		// Say what is missing. A monitor that quietly drops a third of the
		// process table is worse than one that shows all of it.
		tasks += fmt.Sprintf(" (%d kernel hidden)", s.ProcCounts.Kernel)
	}
	parts := []string{
		fmt.Sprintf("%d cores", s.Host.CPUCount),
		fmt.Sprintf("load %.2f %.2f %.2f", s.Load.One, s.Load.Five, s.Load.Fifteen),
		fmt.Sprintf("%s of %s used", bytes(s.Memory.Used), bytes(s.Memory.Total)),
		tasks,
	}
	if s.ProcCounts.Zombie > 0 {
		parts = append(parts, styCrit.Render(fmt.Sprintf("%d zombie", s.ProcCounts.Zombie)))
	}
	if s.CPU.IOWait >= 5 {
		parts = append(parts, styWarn.Render(fmt.Sprintf("%s iowait", percent(s.CPU.IOWait))))
	}
	if s.CPU.Steal >= 1 {
		parts = append(parts, styWarn.Render(fmt.Sprintf("%s steal", percent(s.CPU.Steal))))
	}
	return clipWidth(styLabel.Render(strings.Join(parts, " · ")), m.width)
}

// perCoreMeters renders one small gauge per core, in as many columns as fit.
func (m Model) perCoreMeters() string {
	cores := m.snap.PerCPU
	if len(cores) == 0 {
		return styLabel.Render("no per-core data")
	}
	const cellW = 26
	cols := max(1, m.width/cellW)
	rows := (len(cores) + cols - 1) / cols

	lines := make([]string, rows)
	for i, c := range cores {
		r := i % rows
		if lines[r] != "" {
			lines[r] += " "
		}
		lines[r] += meter(strings.TrimPrefix(c.Name, "cpu"), c.Busy, cellW-1, 3, 6, thCPU)
	}
	return strings.Join(lines, "\n") + "\n" + m.context()
}

// bandMinColWidth is the narrowest a panel column may be. Below this the
// figures crowd out the name beside them.
const bandMinColWidth = 30

// bandGap is the space between panel columns.
const bandGap = 2

// band renders the middle block of panels within a height budget.
//
// Panels are built to fit rather than clipped after the fact, and the optional
// ones are dropped first when the screen is short. What survives is what a
// short screen has room to say.
func (m Model) band(budget int) string {
	if budget < 3 || m.width < bandMinColWidth {
		return ""
	}

	// Network, disk, and filesystems always appear, so their position never
	// moves. Sensors and containers only appear when the machine has them.
	required := 3
	optional := 0
	if len(m.snap.Sensors) > 0 {
		optional++
	}
	if m.bandShowsContainers() {
		optional++
	}

	for n := required + optional; n >= required; n-- {
		cols, colW := bandColumns(n, m.width)
		rows, ok := panelRows(n, cols, budget)
		if !ok {
			continue
		}
		return clip(flow(m.panels(n, colW, rows), cols, m.width, colW, bandGap), budget)
	}
	return ""
}

// bandColumns decides how many panel columns to draw and how wide each is.
//
// The count comes from the minimum width, then the terminal is shared out
// evenly between them. Fixed-width columns would leave a ragged margin on
// every terminal that is not an exact multiple of the panel width.
func bandColumns(n, width int) (cols, colWidth int) {
	cols = (width + bandGap) / (bandMinColWidth + bandGap)
	if cols < 1 {
		cols = 1
	}
	if cols > n {
		cols = n
	}
	return cols, (width - bandGap*(cols-1)) / cols
}

// bandShowsContainers reports whether the band carries the container panel.
// Only the dashboard view does; the other two give containers a table of their
// own, and showing both would report the same containers twice.
func (m Model) bandShowsContainers() bool {
	return m.view == viewDashboard && len(m.snap.Containers) > 0
}

// panels builds the first n panels in fixed order.
func (m Model) panels(n, colW, maxRows int) []panel {
	all := []panel{
		netPanel(m.snap, colW, maxRows),
		diskPanel(m.snap, colW, maxRows),
		fsPanel(m.snap, colW, maxRows),
	}
	if len(m.snap.Sensors) > 0 {
		all = append(all, sensorPanel(m.snap, colW, maxRows))
	}
	if m.bandShowsContainers() {
		all = append(all, containerPanel(m.snap, colW, maxRows))
	}
	if n > len(all) {
		n = len(all)
	}
	return all[:n]
}

// panelRows returns how many data rows each of n panels may have, given the
// column count and the band's height budget. It reports false when n panels
// cannot be shown at all.
func panelRows(n, cols, budget int) (int, bool) {
	perCol := (n + cols - 1) / cols

	// Each panel costs a title and a heading line, and stacked panels are
	// separated by a blank.
	overhead := perCol*2 + (perCol - 1)
	rows := (budget - overhead) / perCol
	if rows < 1 {
		return 0, false
	}
	return rows, true
}

// processes renders the process table into the given number of rows.
func (m Model) processes(rows int) string {
	procs := filterProcesses(m.snap.Processes, m.filter, m.sort, m.hideKernel)
	lines, _ := procTable.render(procs, m.width, rows, int(m.sort), m.cursor, m.offset)
	return strings.Join(lines, "\n")
}

// footer renders the key hints, or the filter prompt while it is open.
func (m Model) footer() string {
	if m.typing {
		return styLabel.Render("filter: ") + styText.Render(m.filter) + styTitle.Render("▏")
	}
	kthreads := "K kthreads:on"
	if m.hideKernel {
		kthreads = "K kthreads:off"
	}
	// The sort hint names the table on screen, since the keys act on it.
	sorted := "c/m/s/t/p sort:" + m.sort.name()
	if m.view == viewContainers {
		sorted = "c/m/t/i/n sort:" + m.ctrSort.name()
		kthreads = "" // no processes on screen to hide
	}
	keys := []string{"q quit", sorted, "v view:" + m.view.name()}
	if kthreads != "" {
		keys = append(keys, kthreads)
	}
	keys = append(keys, "/ filter", "␣ pause", "? help")
	line := styLabel.Render(strings.Join(keys, "  "))
	if m.filter != "" {
		line = styTitle.Render("filter:"+m.filter) + "  " + line
	}
	if n := len(m.snap.Errs); n > 0 {
		line = styCrit.Render(fmt.Sprintf("%d collector errors (?)", n)) + "  " + line
	}
	return clipWidth(line, m.width)
}

// helpView renders the help overlay.
func (m Model) helpView() string {
	var b strings.Builder
	b.WriteString(styTitle.Render("gaze") + "\n\n")
	for _, k := range [][2]string{
		{"q, esc", "quit"},
		{"v", "cycle dashboard, split, and container views"},
		{"c, m, s, t, p, n, u", "sort processes by cpu, memory, swap, time, pid, name, user"},
		{"c, m, t, i, n", "sort containers by cpu, memory, uptime, disk io, name"},
		{"1", "toggle per-core gauges"},
		{"K", "show or hide kernel threads"},
		{"/", "filter processes by name or command line"},
		{"space", "pause collection"},
		{"+, -", "halve or double the refresh interval"},
		{"↑ ↓, j k, pgup, pgdn", "move through the process list"},
		{"?, h", "close this help"},
	} {
		b.WriteString("  " + styKey.Render(pad(k[0], 22)) + styText.Render(k[1]) + "\n")
	}
	if len(m.snap.Errs) > 0 {
		b.WriteString("\n" + styTitle.Render("collector errors") + "\n")
		for _, err := range m.snap.Errs {
			b.WriteString("  " + styCrit.Render(truncate(err.Error(), m.width-2)) + "\n")
		}
	}
	return b.String()
}

// spread puts left at the start of a line and right at its end.
//
// When the two do not fit, the left side gives way. The right side of the
// header is the clock and the refresh rate, which are the same width forever;
// the left side is a hostname and a kernel version, which are not, and losing
// the end of a kernel version costs less than losing the clock.
func spread(left, right string, width int) string {
	rw := lipgloss.Width(right)
	gap := width - lipgloss.Width(left) - rw
	if gap >= 1 {
		return left + strings.Repeat(" ", gap) + right
	}
	if width-rw-1 < 8 {
		return clipWidth(left, width)
	}
	return clipWidth(left, width-rw-1) + " " + right
}

// clip truncates a block to at most n lines.
//
// The panel builders already size themselves to the budget. This is the
// backstop: one miscounted line in a full-screen program means the whole
// display scrolls, so the guarantee is enforced at the point of use rather
// than trusted to the arithmetic above.
func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

// clipWidth truncates a styled line to a display width.
//
// The plain-text helpers in format.go count runes, which is wrong for a string
// that already carries colour escapes: the escapes count as characters and the
// line is cut far too short. Anything already styled goes through here.
func clipWidth(s string, n int) string {
	if n <= 0 {
		return ""
	}
	return lipgloss.NewStyle().MaxWidth(n).Render(s)
}
