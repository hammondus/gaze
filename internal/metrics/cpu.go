package metrics

import (
	"io"
	"strings"
)

// cpuTicks holds the raw jiffy counters for one CPU line of /proc/stat. The
// units cancel when one sample is divided by another, so the value of USER_HZ
// never has to be known.
type cpuTicks struct {
	name                                  string
	user, nice, system, idle, iowait      uint64
	irq, softirq, steal, guest, guestNice uint64
}

// total returns every tick in the sample. Guest time is already counted inside
// user, and guest_nice inside nice, so adding them again would double-count.
func (t cpuTicks) total() uint64 {
	return t.user + t.nice + t.system + t.idle + t.iowait + t.irq + t.softirq + t.steal
}

// statSample is one reading of /proc/stat.
type statSample struct {
	total   cpuTicks
	perCPU  []cpuTicks
	ctxt    uint64
	forks   uint64
	running int
	blocked int
}

// parseStat reads /proc/stat.
func parseStat(r io.Reader) (statSample, error) {
	var s statSample
	err := scanLines(r, func(line string) error {
		switch {
		case strings.HasPrefix(line, "cpu"):
			t := parseCPULine(line)
			if t.name == "cpu" {
				s.total = t
			} else {
				s.perCPU = append(s.perCPU, t)
			}
		case strings.HasPrefix(line, "ctxt "):
			s.ctxt = atou(line[5:])
		case strings.HasPrefix(line, "processes "):
			s.forks = atou(line[10:])
		case strings.HasPrefix(line, "procs_running "):
			s.running = atoi(line[14:])
		case strings.HasPrefix(line, "procs_blocked "):
			s.blocked = atoi(line[14:])
		}
		return nil
	})
	return s, err
}

// parseCPULine parses one "cpuN user nice system ..." line. Fields after steal
// were added over successive kernel versions, so each is taken only if present
// rather than assuming a fixed field count.
func parseCPULine(line string) cpuTicks {
	f := strings.Fields(line)
	t := cpuTicks{name: f[0]}
	get := func(i int) uint64 {
		if i < len(f) {
			return atou(f[i])
		}
		return 0
	}
	t.user, t.nice, t.system, t.idle = get(1), get(2), get(3), get(4)
	t.iowait, t.irq, t.softirq = get(5), get(6), get(7)
	t.steal, t.guest, t.guestNice = get(8), get(9), get(10)
	return t
}

// cpuDelta turns two tick samples into the percentage of the interval spent in
// each state. A counter that moved backwards means the CPU went offline and
// came back with reset counters, which yields a zeroed reading rather than a
// nonsensical negative one.
func cpuDelta(prev, cur cpuTicks) CPU {
	c := CPU{Name: cur.name}
	total := cur.total() - prev.total()
	if cur.total() < prev.total() || total == 0 {
		return c
	}
	f := func(a, b uint64) float64 {
		if b < a {
			return 0
		}
		return float64(b-a) / float64(total) * 100
	}
	c.User = f(prev.user, cur.user)
	c.Nice = f(prev.nice, cur.nice)
	c.System = f(prev.system, cur.system)
	c.Idle = f(prev.idle, cur.idle)
	c.IOWait = f(prev.iowait, cur.iowait)
	c.IRQ = f(prev.irq, cur.irq)
	c.SoftIRQ = f(prev.softirq, cur.softirq)
	c.Steal = f(prev.steal, cur.steal)
	c.Guest = f(prev.guest, cur.guest)
	c.Busy = 100 - c.Idle
	return c
}
