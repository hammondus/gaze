package ui

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/hammondus/gaze/internal/metrics"
)

// demoSnapshot is a plausible busy machine. It exists so the layout can be
// rendered and checked without a Linux kernel underneath, which is the whole
// reason the display layer takes a Snapshot rather than reading /proc itself.
func demoSnapshot() metrics.Snapshot {
	s := metrics.Snapshot{
		Taken:    time.Date(2026, 8, 24, 14, 23, 7, 0, time.UTC),
		Interval: time.Second,
		Host: metrics.Host{
			Hostname: "aurora", Kernel: "6.8.0-45-generic",
			Uptime: 148*time.Hour + 12*time.Minute, CPUCount: 8,
		},
		CPU:    metrics.CPU{Busy: 42.3, Idle: 57.7, IOWait: 6.1, User: 30, System: 12},
		Memory: metrics.Memory{Total: 16 << 30, Used: 11 << 30, Percent: 68.7},
		Swap:   metrics.Swap{Total: 4 << 30, Used: 1 << 30, Percent: 26.5},
		Load:   metrics.Load{One: 3.4, Five: 2.9, Fifteen: 2.1},
		Networks: []metrics.Network{
			{Name: "eth0", Up: true, RxRate: 1.4 * 1024 * 1024, TxRate: 340 * 1024},
			{Name: "wg0", Up: true, RxRate: 2048, TxRate: 900},
			{Name: "lo", Up: true},
		},
		Disks: []metrics.Disk{
			{Name: "nvme0n1", ReadRate: 12 * 1024 * 1024, WriteRate: 4 * 1024 * 1024},
			{Name: "sda", WriteRate: 96 * 1024},
		},
		Mounts: []metrics.Mount{
			{Path: "/", Percent: 34.2, Free: 420 << 30},
			{Path: "/boot/efi", Percent: 9.1, Free: 480 << 20},
			{Path: "/var/lib/postgresql/data", Percent: 91.4, Free: 12 << 30},
		},
		Sensors: []metrics.Sensor{
			{Kind: metrics.SensorTemp, Label: "Package id 0", Value: 78.5, High: 84, Crit: 100},
			{Kind: metrics.SensorTemp, Label: "nvme0n1", Value: 41},
			{Kind: metrics.SensorFan, Label: "fan1", Value: 1420},
			{Kind: metrics.SensorBattery, Label: "BAT0 (discharging)", Value: 42},
		},
		ContainerRuntime: "docker",
		Containers: []metrics.Container{
			{Name: "pgdata", Image: "postgres:16", Command: "postgres -c shared_buffers=2GB",
				State: "running", Status: "Up 6 days", Uptime: 148 * time.Hour,
				CPU: 18.2, MemUsed: 2 << 30, MemLimit: 4 << 30, MemPct: 50.1, PIDs: 24,
				ReadRate: 2 << 20, WriteRate: 14 << 20, RxRate: 90 << 10, TxRate: 12 << 10},
			{Name: "edge-proxy", Image: "nginx:1.27-alpine", Command: "nginx -g 'daemon off;'",
				State: "running", Status: "Up 6 days", Uptime: 147 * time.Hour,
				CPU: 0.4, MemUsed: 48 << 20, MemLimit: 512 << 20, MemPct: 9.4, PIDs: 3,
				RxRate: 1 << 20, TxRate: 3 << 20},
			{Name: "nightly-backup", Image: "restic/restic:0.17", Command: "restic backup /data",
				State: "exited", Status: "Exited (0) 4 hours ago"},
			{Name: "migrate-once", Image: "flyway/flyway:10", Command: "flyway migrate",
				State: "exited", Status: "Exited (1) 2 days ago"},
		},
		ProcCounts: metrics.ProcCounts{Total: 412, Running: 3, Threads: 1183, Zombie: 1},
	}
	for i, p := range []struct {
		pid        int
		user, name string
		cmd        string
		cpu, mem   float64
		rss        uint64
		state      byte
		kernel     bool
		swap       uint64
	}{
		{2841, "postgres", "postgres", "postgres: writer process", 18.2, 12.4, 2 << 30, 'R', false, 0},
		{1102, "craig", "firefox", "/usr/lib/firefox/firefox --profile /home/craig/.mozilla", 11.7, 8.9, 1500 << 20, 'S', false, 340 << 20},
		{9021, "root", "kworker/2:1", "", 4.1, 0, 0, 'I', true, 0},
		{331, "root", "systemd-journald", "/usr/lib/systemd/systemd-journald", 2.2, 0.4, 68 << 20, 'D', false, 12 << 20},
		{7734, "craig", "go", "go build ./...", 1.4, 2.1, 350 << 20, 'S', false, 0},
		{2, "root", "kthreadd", "", 0, 0, 0, 'S', true, 0},
		// A zombie has no command line either, so it renders in brackets, but
		// it is not a kernel thread and must survive the K toggle.
		{412, "nobody", "defunct-thing", "", 0, 0, 0, 'Z', false, 0},
	} {
		s.Processes = append(s.Processes, metrics.Process{
			PID: p.pid, PPID: 1, User: p.user, Name: p.name, Cmdline: p.cmd,
			State: p.state, Threads: 4 + i, CPU: p.cpu, MemPct: p.mem, RSS: p.rss,
			Kernel: p.kernel, Swap: p.swap,
			CPUTime: time.Duration(i+1) * 137 * time.Second,
		})
	}
	s.ProcCounts.Kernel = 2
	return s
}

// demoModel returns a model already holding a snapshot and a little history.
func demoModel(w, h int) Model {
	m := New(nil, time.Second)
	m.width, m.height, m.ready = w, h, true
	m.snap = demoSnapshot()
	for _, v := range []float64{12, 18, 31, 55, 74, 61, 40, 33, 28, 44, 52, 42} {
		m.cpuHist.push(v)
		m.memHist.push(v/2 + 40)
	}
	m.record()
	return m
}

// TestViewFitsTerminal is the assertion that matters for a full-screen
// program: no line may exceed the width, at any size, or the terminal wraps it
// and the whole layout slides down the screen.
func TestViewFitsTerminal(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {90, 24}, {100, 30}, {120, 40}, {200, 60}, {60, 20}, {40, 15}} {
		m := demoModel(size[0], size[1])
		for _, variant := range []string{"default", "percore", "filtered", "procs", "containers", "nodocker", "nosensors"} {
			mm := m
			switch variant {
			case "procs":
				mm.view = viewProcs
			case "containers":
				mm.view = viewContainers
			case "nodocker":
				mm.view = viewContainers
				mm.snap.Containers = nil
				mm.snap.ContainerRuntime = ""
			case "nosensors":
				mm.snap.Sensors = nil
			case "percore":
				mm.perCore = true
				mm.snap.PerCPU = make([]metrics.CPU, 8)
				for i := range mm.snap.PerCPU {
					mm.snap.PerCPU[i] = metrics.CPU{Name: "cpu" + string(rune('0'+i)), Busy: float64(i * 12)}
				}
			case "filtered":
				mm.filter = "post"
			}
			for i, line := range strings.Split(mm.View(), "\n") {
				if w := lipgloss.Width(line); w > size[0] {
					t.Errorf("%dx%d %s: line %d is %d columns wide\n%q",
						size[0], size[1], variant, i, w, line)
				}
			}
		}
	}
}

// TestViewFitsHeight checks the screen does not overrun the terminal, which
// would scroll the header off the top on every refresh.
func TestViewFitsHeight(t *testing.T) {
	// The last two are smaller than the header, gauges, and footer together.
	// Nothing is readable there, but the frame must still not overrun.
	for _, size := range [][2]int{{80, 24}, {90, 24}, {120, 40}, {200, 60}, {100, 20}, {120, 14}, {40, 8}, {20, 6}} {
		for _, v := range []viewMode{viewSplit, viewContainers, viewProcs} {
			m := demoModel(size[0], size[1])
			m.view = v
			if h := lipgloss.Height(m.View()); h > size[1] {
				t.Errorf("%dx%d %s: view is %d lines tall", size[0], size[1], v.name(), h)
			}
		}
	}
}

// TestSidebarSitsBesideTheTables checks the two-column frame: the panels are on
// the left of the same lines that carry the tables, not above them. A regression
// here reads as "the layout still works" in a height test while having quietly
// gone back to stacking.
func TestSidebarSitsBesideTheTables(t *testing.T) {
	const width = 160
	sideW := sidebarWidth(width)
	lines := strings.Split(stripStyle(demoModel(width, 40).View()), "\n")

	titles := 0
	var networkLine []rune
	for _, line := range lines {
		for _, title := range []string{"NETWORK", "DISK I/O", "FILESYSTEM", "SENSORS"} {
			if !strings.HasPrefix(line, title) {
				continue
			}
			titles++
			if title == "NETWORK" {
				networkLine = []rune(line)
			}
		}
	}
	// Every panel title starts its line, so the panels are down the left.
	if titles != 4 {
		t.Errorf("found %d panel titles at the start of a line, want 4", titles)
	}
	if networkLine == nil {
		t.Fatal("no network panel on screen")
	}
	// The first panel and the first table share a line, so the sidebar is
	// beside the tables rather than above them.
	if len(networkLine) <= sideW || strings.TrimSpace(string(networkLine[sideW:])) == "" {
		t.Errorf("nothing beside the network panel; the frame is still stacked:\n%q", string(networkLine))
	}

	if sideW < 30 || sideW > width/4 {
		t.Errorf("sidebar width at %d columns = %d, want 30 to %d", width, sideW, width/4)
	}
	if w := sidebarWidth(80); w != 0 {
		t.Errorf("80 columns is too narrow to divide, but got a %d-column sidebar", w)
	}
}

// TestSidebarSharesRowsWhereTheyAreWanted checks the row split. An even share is
// the wrong answer: the rows a panel cannot fill must go to one that can.
func TestSidebarSharesRowsWhereTheyAreWanted(t *testing.T) {
	for _, c := range []struct {
		name   string
		want   []int
		budget int
		got    []int
	}{
		{"everyone fits", []int{2, 2, 3}, 12, []int{2, 2, 3}},
		{"the spare rows go to the panel that wants them", []int{1, 1, 9}, 9, []int{1, 1, 7}},
		{"an even split when every panel is greedy", []int{9, 9, 9}, 9, []int{3, 3, 3}},
		{"fewer rows than panels, top first", []int{4, 4, 4}, 2, []int{1, 1, 0}},
		{"a panel with no data takes nothing", []int{0, 5, 5}, 6, []int{0, 3, 3}},
	} {
		got := share(c.want, c.budget)
		if len(got) != len(c.got) {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.got)
		}
		total := 0
		for i := range got {
			total += got[i]
			if got[i] != c.got[i] {
				t.Errorf("%s: share(%v, %d) = %v, want %v", c.name, c.want, c.budget, got, c.got)
				break
			}
		}
		if total > c.budget {
			t.Errorf("%s: handed out %d rows of a %d budget", c.name, total, c.budget)
		}
	}
}

// TestViewCycle checks the v key and what each view puts in the main column.
//
// The three views are how much of that column containers get: a share of it,
// all of it, none of it. The sidebar is the same in each.
func TestViewCycle(t *testing.T) {
	m := demoModel(160, 40)
	if m.view != viewSplit {
		t.Fatal("the split view is the starting view")
	}

	out := stripStyle(m.View())
	// The split view shows both tables.
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "PID") {
		t.Error("the split view should show both the container and process tables")
	}

	next, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	ctr := next.(Model)
	if ctr.view != viewContainers {
		t.Fatalf("v gave %s, want containers", ctr.view.name())
	}
	out = stripStyle(ctr.View())
	if strings.Contains(out, "  PID ") {
		t.Error("the container view should replace the process table")
	}
	if !strings.Contains(out, "NETWORK") {
		t.Error("the sidebar belongs to every view")
	}

	next, _ = ctr.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	procs := next.(Model)
	if procs.view != viewProcs {
		t.Fatalf("v gave %s, want processes", procs.view.name())
	}
	out = stripStyle(procs.View())
	if !strings.Contains(out, "  PID ") {
		t.Error("the process view should show the process table")
	}
	// Every container name is gone, so the process list has the whole column.
	for _, name := range []string{"pgdata", "edge-proxy"} {
		if strings.Contains(out, name) {
			t.Errorf("the process view still shows the container %s", name)
		}
	}

	// A third press returns to the start.
	next, _ = procs.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	if next.(Model).view != viewSplit {
		t.Error("v did not cycle back to the split view")
	}
}

// TestStoppedContainersOnlyInDedicatedView checks the one behaviour that
// separates the container view from the split view.
func TestStoppedContainersOnlyInDedicatedView(t *testing.T) {
	cs := demoSnapshot().Containers

	running := filterContainers(cs, "", ctrCPU, false)
	if len(running) != 2 {
		t.Errorf("running containers = %d, want 2", len(running))
	}
	for _, c := range running {
		if c.State != "running" {
			t.Errorf("%s is %s and should be hidden", c.Name, c.State)
		}
	}

	all := filterContainers(cs, "", ctrCPU, true)
	if len(all) != 4 {
		t.Errorf("all containers = %d, want 4", len(all))
	}
	// Whatever the column, a stopped container sorts last: its rates are all
	// zero, so it would otherwise scatter through the list.
	if all[len(all)-1].State == "running" {
		t.Errorf("stopped containers did not sort last: %v", names(all))
	}
	if all[0].Name != "pgdata" {
		t.Errorf("first by cpu = %s, want pgdata", all[0].Name)
	}
}

// TestSplitViewHidesStoppedContainers checks that the container table above the
// process list applies the same rule as the panel it replaced. A row of dashes
// for something that exited last week costs a row a running container needs.
func TestSplitViewHidesStoppedContainers(t *testing.T) {
	out := stripStyle(demoModel(160, 40).View())
	for _, stopped := range []string{"nightly-backup", "migrate-once"} {
		if strings.Contains(out, stopped) {
			t.Errorf("%s is stopped and must not appear in the split view", stopped)
		}
	}
	if !strings.Contains(out, "pgdata") {
		t.Error("the busiest running container is missing")
	}
}

// TestContainerSortAndFilter covers the container table's own keys.
func TestContainerSortAndFilter(t *testing.T) {
	cs := demoSnapshot().Containers

	if got := filterContainers(cs, "", ctrIO, true); got[0].Name != "pgdata" {
		t.Errorf("first by disk io = %s, want pgdata", got[0].Name)
	}
	if got := filterContainers(cs, "", ctrName, true); got[0].Name != "edge-proxy" {
		t.Errorf("first by name = %s, want edge-proxy", got[0].Name)
	}
	// The filter matches the image and the command, not just the name.
	if got := filterContainers(cs, "nginx", ctrCPU, true); len(got) != 1 {
		t.Errorf("image filter matched %d, want 1", len(got))
	}
	if got := filterContainers(cs, "shared_buffers", ctrCPU, true); len(got) != 1 {
		t.Errorf("command filter matched %d, want 1", len(got))
	}
}

// TestContainerViewEmptyStates checks the three reasons the table can be
// empty. Each has its own message, because reporting one as another sends you
// looking for a problem you do not have.
func TestContainerViewEmptyStates(t *testing.T) {
	for _, c := range []struct {
		name     string
		set      func(m *Model)
		want     string
		unwanted string
	}{
		{
			name: "collection switched off",
			set: func(m *Model) {
				m.snap.ContainersDisabled = true
				m.snap.ContainerRuntime = ""
				m.snap.Containers = nil
			},
			want:     "container collection is switched off",
			unwanted: "no container runtime reachable",
		},
		{
			name: "no runtime found",
			set: func(m *Model) {
				m.snap.ContainerRuntime = ""
				m.snap.Containers = nil
			},
			want:     "no container runtime reachable",
			unwanted: "switched off",
		},
		{
			name: "runtime found but nothing running",
			set: func(m *Model) {
				m.snap.ContainerRuntime = "podman"
				m.snap.Containers = nil
			},
			want:     "no containers running on podman",
			unwanted: "reachable",
		},
	} {
		m := demoModel(120, 40)
		m.view = viewContainers
		c.set(&m)
		out := stripStyle(m.View())
		if !strings.Contains(out, c.want) {
			t.Errorf("%s: the view should say %q", c.name, c.want)
		}
		if strings.Contains(out, c.unwanted) {
			t.Errorf("%s: the view wrongly says %q", c.name, c.unwanted)
		}
	}
}

func names(cs []metrics.Container) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name + "/" + c.State
	}
	return out
}

// TestFilterNarrowsTheList checks the filter matches on the command line as
// well as the process name.
func TestFilterNarrowsTheList(t *testing.T) {
	procs := demoSnapshot().Processes
	if got := filterProcesses(procs, "firefox", sortCPU, false); len(got) != 1 {
		t.Errorf("name filter matched %d processes, want 1", len(got))
	}
	// "profile" appears only in Firefox's arguments, not in any name.
	if got := filterProcesses(procs, "profile", sortCPU, false); len(got) != 1 {
		t.Errorf("command-line filter matched %d processes, want 1", len(got))
	}
	if got := filterProcesses(procs, "POSTGRES", sortCPU, false); len(got) != 1 {
		t.Errorf("filter is case sensitive: matched %d", len(got))
	}
}

// TestSortIsStableAcrossTies checks that processes tied on every column keep a
// fixed order, so an idle table does not shuffle every second.
func TestSortIsStableAcrossTies(t *testing.T) {
	procs := []metrics.Process{{PID: 30}, {PID: 10}, {PID: 20}}
	got := filterProcesses(procs, "", sortCPU, false)
	if got[0].PID != 10 || got[2].PID != 30 {
		t.Errorf("ties are not broken by PID: %v", got)
	}
}

// TestSortByCPUFallsBackToMemory checks that an idle machine does not fill the
// screen with kernel threads. They all read zero percent and hold no memory,
// so the processes you actually started must sort above them.
func TestSortByCPUFallsBackToMemory(t *testing.T) {
	procs := []metrics.Process{
		{PID: 2, Name: "kthreadd"},                    // kernel thread, no RSS
		{PID: 9, Name: "kworker/0:0"},                 // kernel thread, no RSS
		{PID: 4210, Name: "firefox", RSS: 1 << 30},    // idle but real
		{PID: 3300, Name: "postgres", RSS: 200 << 20}, // idle but real
	}
	got := filterProcesses(procs, "", sortCPU, false)
	if got[0].Name != "firefox" || got[1].Name != "postgres" {
		t.Errorf("idle order = %s, %s; want firefox, postgres", got[0].Name, got[1].Name)
	}
}

// TestFilterKeyboardCapturesEverything checks that typing a name containing
// "q" into the filter does not quit the program.
func TestFilterKeyboardCapturesEverything(t *testing.T) {
	m := demoModel(100, 30)
	m.typing = true
	next, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd != nil {
		t.Fatal("typing q into the filter quit the program")
	}
	if next.(Model).filter != "q" {
		t.Errorf("filter = %q, want %q", next.(Model).filter, "q")
	}
}

// TestHideKernelThreads checks the K toggle.
//
// Kernel threads run entirely inside the kernel and hold no memory. On a
// typical host they outnumber everything you started, so hiding them is what
// makes the table readable.
func TestHideKernelThreads(t *testing.T) {
	procs := demoSnapshot().Processes

	shown := filterProcesses(procs, "", sortCPU, false)
	hidden := filterProcesses(procs, "", sortCPU, true)
	if len(shown)-len(hidden) != 2 {
		t.Errorf("hiding removed %d processes, want 2", len(shown)-len(hidden))
	}
	for _, p := range hidden {
		if p.Kernel {
			t.Errorf("PID %d (%s) is a kernel thread and should be hidden", p.PID, p.Name)
		}
	}

	// The zombie has no command line and renders in brackets, but it is not a
	// kernel thread. Bracketed and hidden are not the same set.
	var sawZombie bool
	for _, p := range hidden {
		if p.State == 'Z' {
			sawZombie = true
		}
	}
	if !sawZombie {
		t.Error("the zombie was hidden; only kernel threads should be")
	}

	// The toggle composes with the name filter.
	both := filterProcesses(procs, "k", sortCPU, true)
	for _, p := range both {
		if p.Kernel {
			t.Errorf("PID %d survived both filters", p.PID)
		}
	}
}

// TestKeyKHidesKernelThreads checks the binding, and that lower-case k still
// moves the cursor.
func TestKeyKHidesKernelThreads(t *testing.T) {
	m := demoModel(120, 40)
	m.cursor = 3

	next, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("K")})
	got := next.(Model)
	if got.hideKernel {
		t.Error("K did not reveal kernel threads, which start hidden")
	}
	// The list just got shorter, so a cursor left where it was would point
	// past the end or at a different process.
	if got.cursor != 0 {
		t.Errorf("cursor = %d, want 0 after the list changed length", got.cursor)
	}

	again, _ := got.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("K")})
	if !again.(Model).hideKernel {
		t.Error("K did not toggle back")
	}

	// Lower-case k keeps its cursor binding.
	m2 := demoModel(120, 40)
	m2.cursor = 3
	moved, _ := m2.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if moved.(Model).cursor != 2 {
		t.Errorf("lower-case k moved the cursor to %d, want 2", moved.(Model).cursor)
	}
	if !moved.(Model).hideKernel {
		t.Error("lower-case k changed the kernel thread setting")
	}
}

// TestHiddenCountIsReported checks that the context line says what is missing.
// Kernel threads are hidden by default, so the screen must say so; a monitor
// that quietly drops most of the process table is worse than one that shows
// all of it.
func TestHiddenCountIsReported(t *testing.T) {
	m := demoModel(120, 40)
	if !m.hideKernel {
		t.Fatal("kernel threads should start hidden")
	}
	if !strings.Contains(stripStyle(m.context()), "2 kernel hidden") {
		t.Errorf("context line does not report the hidden count: %q", stripStyle(m.context()))
	}
	m.hideKernel = false
	if strings.Contains(stripStyle(m.context()), "hidden") {
		t.Error("nothing is hidden, so nothing should be reported")
	}
}

// TestSortBySwap checks the s key's ordering.
func TestSortBySwap(t *testing.T) {
	got := filterProcesses(demoSnapshot().Processes, "", sortSwap, true)
	if got[0].Name != "firefox" {
		t.Errorf("first by swap = %s, want firefox", got[0].Name)
	}
	if got[1].Name != "systemd-journald" {
		t.Errorf("second by swap = %s, want systemd-journald", got[1].Name)
	}
	// Most processes hold nothing in swap, so the rest tie. Resident size
	// breaks the tie, then PID, so the order never shuffles.
	for i := 2; i < len(got); i++ {
		if got[i].Swap != 0 {
			t.Errorf("%s sorted below a zero-swap process", got[i].Name)
		}
	}
	if got[2].Name != "postgres" {
		t.Errorf("swap ties did not fall back to memory: got %s", got[2].Name)
	}
}

// TestRenderDemo prints a frame. Run it with -v to look at the layout:
//
//	go test ./internal/ui -run TestRenderDemo -v
func TestRenderDemo(t *testing.T) {
	if os.Getenv("GAZE_DEMO") == "" && !testing.Verbose() {
		t.Skip("set -v or GAZE_DEMO=1 to print a frame")
	}
	lipgloss.SetColorProfile(0) // no escape codes, so the dump is readable
	t.Log("\n" + demoModel(120, 40).View())
}

// TestAbsentFieldsRenderAsDash covers Snapshot.Absent: a field the running
// platform could not supply renders as a dash, where a zero would claim a
// measurement nobody took. The Linux collector never sets Absent, so this is
// the only place the path runs until a second platform exists.
func TestAbsentFieldsRenderAsDash(t *testing.T) {
	lipgloss.SetColorProfile(0)
	// Wide enough that the main column keeps the SWAP column beside the
	// sidebar; at 120 the column is dropped before absence could show.
	m := demoModel(200, 60)
	if !strings.Contains(m.View(), "340M") {
		t.Fatal("the demo frame no longer shows firefox's 340M of swap; update this test's expectations with it")
	}

	m.snap.Absent = []metrics.Field{metrics.FieldProcessSwap}
	frame := m.View()
	if strings.Contains(frame, "340M") {
		t.Error("the SWAP column shows a value while the field is absent")
	}
	if !strings.Contains(frame, "SWAP") {
		t.Error("the SWAP column was dropped; an absent field renders a dash, it does not vanish")
	}
}

// TestLiveFrame collects from the running kernel and renders a frame.
//
// It is the end-to-end check the fixture tests cannot be: it proves the
// parsers match a real /proc, that statfs returns sane figures, and that the
// layout survives real data. It needs a Linux kernel, so it is opt-in:
//
//	GAZE_LIVE=1 go test ./internal/ui -run TestLiveFrame -v
func TestLiveFrame(t *testing.T) {
	if os.Getenv("GAZE_LIVE") == "" {
		t.Skip("set GAZE_LIVE=1 to collect from the running kernel")
	}
	if runtime.GOOS != "linux" {
		t.Skipf("no /proc on %s", runtime.GOOS)
	}

	col := metrics.New(metrics.Options{})
	time.Sleep(500 * time.Millisecond) // let the counters move so rates are real

	m := New(col.Collect, time.Second)
	m.width, m.height, m.ready = 120, 40, true
	m.snap = metrics.Snapshot(collectOnce(t, col))
	m.record()

	if m.snap.Host.CPUCount < 1 {
		t.Error("no CPUs found")
	}
	if m.snap.Memory.Total == 0 {
		t.Error("no memory reported")
	}
	if len(m.snap.Processes) == 0 {
		t.Error("no processes found")
	}
	if len(m.snap.Mounts) == 0 {
		t.Error("no filesystems found: statfs is the only Linux-only code here")
	}
	for _, err := range m.snap.Errs {
		t.Errorf("collector error: %v", err)
	}

	// The same guarantee the synthetic test makes, against real data: real
	// process names and mount paths are far longer and stranger.
	for i, line := range strings.Split(m.View(), "\n") {
		if w := lipgloss.Width(line); w > 120 {
			t.Errorf("line %d is %d columns wide: %q", i, w, line)
		}
	}

	// Exercise both states of the K toggle against a real process table,
	// where kernel threads are most of it.
	shown := len(filterProcesses(m.snap.Processes, "", sortCPU, false))
	hidden := len(filterProcesses(m.snap.Processes, "", sortCPU, true))
	t.Logf("processes: %d total, %d after hiding %d kernel threads",
		shown, hidden, m.snap.ProcCounts.Kernel)
	if m.snap.ProcCounts.Kernel == 0 {
		t.Error("no kernel threads found; every Linux host has them")
	}
	if shown-hidden != m.snap.ProcCounts.Kernel {
		t.Errorf("hiding removed %d, but the collector counted %d kernel threads",
			shown-hidden, m.snap.ProcCounts.Kernel)
	}

	lipgloss.SetColorProfile(0)
	t.Log("\n" + m.View())
	m.hideKernel = true
	t.Log("\nwith kernel threads hidden:\n" + m.View())
}

func collectOnce(t *testing.T, col *metrics.Collector) metrics.Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()
	return col.Collect(ctx)
}
