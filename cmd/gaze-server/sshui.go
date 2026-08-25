package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/hammondus/gaze/internal/metrics"
	"github.com/hammondus/gaze/internal/query"
	"github.com/hammondus/gaze/internal/ui"
)

// sshInterval is how often a session re-reads the store: the host list, and
// the open host's reconstructed snapshot. Reports arrive about once a
// minute, so five seconds keeps arrivals and staleness prompt on screen
// while costing a handful of cheap queries.
const sshInterval = 5 * time.Second

// sshQueryTimeout bounds one store read, the way the TUI bounds a
// collection.
const sshQueryTimeout = 3 * time.Second

// sshRoot is the model an SSH session lands in: the fleet list, with the
// ordinary gaze dashboard nested inside it once a host is opened. The
// nested model is the same ui.Model the local binary runs; the only key
// this wrapper takes from it is q, which here means back rather than quit.
type sshRoot struct {
	q *query.Q

	width, height int
	rows          []query.Overview
	cursor        int
	offset        int
	err           error

	child     *ui.Model
	childName string
}

func newSSHRoot(q *query.Q) sshRoot { return sshRoot{q: q} }

// fleetMsg carries a refreshed host list.
type fleetMsg struct {
	rows []query.Overview
	err  error
}

// fleetTickMsg schedules the next list refresh.
type fleetTickMsg struct{}

func (m sshRoot) Init() tea.Cmd {
	return tea.Batch(m.loadFleet(), m.tick())
}

func (m sshRoot) loadFleet() tea.Cmd {
	q := m.q
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), sshQueryTimeout)
		defer cancel()
		rows, err := q.Fleet(ctx)
		return fleetMsg{rows: rows, err: err}
	}
}

func (m sshRoot) tick() tea.Cmd {
	return tea.Tick(sshInterval, func(time.Time) tea.Msg { return fleetTickMsg{} })
}

func (m sshRoot) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.child != nil {
			return m.forward(msg)
		}
		return m, nil

	case fleetMsg:
		m.rows, m.err = msg.rows, msg.err
		if m.cursor >= len(m.rows) {
			m.cursor = max(0, len(m.rows)-1)
		}
		return m, nil

	case fleetTickMsg:
		if m.child != nil {
			// Keep the timer alive while a host is open, so the list is
			// already refreshing again the moment q comes back to it.
			return m, m.tick()
		}
		return m, tea.Batch(m.loadFleet(), m.tick())

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Everything else — the dashboard's own snapshot and tick messages —
	// belongs to the open host view.
	if m.child != nil {
		return m.forward(msg)
	}
	return m, nil
}

func (m sshRoot) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.child != nil {
		// While the dashboard's filter prompt is open it owns every key;
		// q in a filter is a letter, not a command.
		if !m.child.InputCaptured() {
			switch msg.String() {
			case "q", "esc":
				m.child, m.childName = nil, ""
				return m, m.loadFleet()
			case "ctrl+c":
				return m, tea.Quit
			}
		}
		return m.forward(msg)
	}

	switch msg.String() {
	case "q", "esc", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		m.cursor = max(0, m.cursor-1)
	case "down", "j":
		m.cursor = min(max(0, len(m.rows)-1), m.cursor+1)
	case "home", "g":
		m.cursor = 0
	case "enter":
		if m.cursor < len(m.rows) {
			return m.openHost(m.rows[m.cursor])
		}
	}
	return m, nil
}

// openHost nests a dashboard for the selected host, fed by the store
// instead of a collector. The size is delivered by message, since the
// child was not there when the session's pty dimensions first arrived.
func (m sshRoot) openHost(row query.Overview) (tea.Model, tea.Cmd) {
	child := ui.New(snapshotSource(m.q, row.ID, row.Name), sshInterval)
	sized, _ := child.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
	c := sized.(ui.Model)
	m.child, m.childName = &c, row.Name
	return m, c.Init()
}

func (m sshRoot) forward(msg tea.Msg) (tea.Model, tea.Cmd) {
	updated, cmd := m.child.Update(msg)
	c := updated.(ui.Model)
	m.child = &c
	return m, cmd
}

// snapshotSource adapts the query package to the dashboard's Source. An
// error — including a host that has never reported — lands in Errs, so it
// shows in the footer instead of clearing the screen.
func snapshotSource(q *query.Q, id int64, name string) ui.Source {
	return func(ctx context.Context) metrics.Snapshot {
		snap, err := q.LatestSnapshot(ctx, id)
		if err != nil {
			return metrics.Snapshot{
				Taken: time.Now(),
				Host:  metrics.Host{Hostname: name},
				Errs:  []error{fmt.Errorf("no stored reports for %s: %w", name, err)},
			}
		}
		return snap
	}
}

// Styles for the fleet list, matching the dashboard's dark ANSI-256
// palette without reaching into the ui package's unexported theme.
var (
	sshTitle = lipgloss.NewStyle().Foreground(lipgloss.Color("51")).Bold(true)
	sshLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	sshText  = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	sshSel   = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Background(lipgloss.Color("238"))
	sshOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
	sshStale = lipgloss.NewStyle().Foreground(lipgloss.Color("221"))
)

func (m sshRoot) View() string {
	if m.child != nil {
		return m.child.View()
	}
	if m.width == 0 || m.height == 0 {
		return "starting…"
	}

	var b strings.Builder
	b.WriteString(sshTitle.Render("gaze") + sshText.Render(" — hosts") + "\n\n")

	if m.err != nil {
		b.WriteString(sshStale.Render(fmt.Sprintf("cannot read the store: %v", m.err)) + "\n")
	} else if len(m.rows) == 0 {
		b.WriteString(sshLabel.Render("no hosts are enrolled") + "\n")
	} else {
		b.WriteString(m.table())
	}

	b.WriteString("\n" + sshLabel.Render("↑↓ select  enter view  q disconnect"))
	return clipLines(b.String(), m.height)
}

// table renders the host rows, keeping the cursor on screen. The columns
// mirror the web fleet page: the three states must be as unmistakable here
// as they are there.
func (m *sshRoot) table() string {
	head := fmt.Sprintf("  %-20s %-16s %-14s %6s %6s %7s", "NAME", "STATE", "LAST SEEN", "CPU", "MEM", "PROCS")

	visible := m.height - 6 // title, blanks, heading, footer
	if visible < 1 {
		visible = 1
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+visible {
		m.offset = m.cursor - visible + 1
	}

	var b strings.Builder
	b.WriteString(sshLabel.Render(clipCols(head, m.width)) + "\n")
	for i := m.offset; i < len(m.rows) && i < m.offset+visible; i++ {
		r := m.rows[i]
		state, class := hostState(r.LastSeen)

		seen, cpu, mem, procs := "—", "—", "—", "—"
		if !r.LastSeen.IsZero() {
			seen = fmtAgo(r.LastSeen)
		}
		if r.HasReport {
			cpu = fmtPercent(r.CPU)
			if r.MemTotal > 0 {
				mem = fmtPercent(r.MemUsed / float64(r.MemTotal) * 100)
			}
			procs = fmt.Sprintf("%d", r.Procs)
		}

		line := fmt.Sprintf("  %-20s %-16s %-14s %6s %6s %7s", clipCols(r.Name, 20), state, seen, cpu, mem, procs)
		line = clipCols(line, m.width)
		switch {
		case i == m.cursor:
			line = sshSel.Render(line)
		case class == "ok":
			line = sshOK.Render(line)
		case class == "stale":
			line = sshStale.Render(line)
		default:
			line = sshLabel.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// clipLines truncates a block to at most n lines, the same backstop the
// dashboard applies to its own frame.
func clipLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

// clipCols truncates a plain (unstyled) line to a rune count.
func clipCols(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
