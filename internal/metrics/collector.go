package metrics

import (
	"context"
	"io/fs"
	"os"
	"os/user"
	"path"
	"strconv"
	"time"
)

// Collector produces snapshots. It holds the previous reading of every
// cumulative counter, which is what lets a Snapshot carry finished rates and
// leaves the display layer with no state of its own.
//
// A Collector is not safe for concurrent use. Run one, in one goroutine.
type Collector struct {
	src      Source
	opts     Options
	pageSize uint64

	host   Host
	users  map[uint32]string
	docker *dockerClient

	prevAt   time.Time
	prevStat statSample
	prevNet  map[string]netCounters
	prevDisk map[string]diskCounters
	prevProc map[int]procSample

	// lastTotalDelta is the machine-wide CPU jiffy count accrued over the
	// interval just measured. Process percentages divide by it, so it is
	// recorded by collectCPU before prevStat is replaced.
	lastTotalDelta float64
}

// minRateInterval is the shortest gap between two readings that yields a
// meaningful rate.
//
// Every rate here is a difference divided by elapsed time. Over a millisecond
// the divisor is near zero and a single jiffy of CPU time reads as hundreds of
// percent, so the first frame after start-up would show nonsense. Below this
// gap the collector reports the absolute values it read and leaves every
// derived rate at zero, then produces real figures from the next collection.
const minRateInterval = 100 * time.Millisecond

// procSample is the previous reading for one process. starttime is kept so a
// recycled PID is recognised: the kernel reuses process IDs, and diffing a new
// process against a dead one's counters would report a huge false spike.
type procSample struct {
	ticks     uint64
	starttime uint64
}

// Options configure a Collector. The zero value collects everything.
type Options struct {
	// DisableContainers stops the collector from looking for a container
	// runtime. No socket is probed and no request is made to a daemon.
	//
	// This is worth having for two reasons. A host running many containers
	// pays two requests per container per refresh, which is the largest
	// single cost in a collection; and not everyone wants a monitor talking
	// to their Docker socket at all.
	DisableContainers bool
}

// New returns a Collector reading the running system.
//
// It takes a priming sample straight away, so the first snapshot a caller
// receives one interval later already carries real rates rather than a screen
// of zeros.
func New(opts Options) *Collector { return NewWithSource(NewSource(), opts) }

// NewWithSource returns a Collector reading from an arbitrary Source. Tests
// use it to read fixture trees, and it lets you replay a captured /proc
// directory when chasing a reading you cannot reproduce live.
func NewWithSource(src Source, opts Options) *Collector {
	c := &Collector{
		src:      src,
		opts:     opts,
		pageSize: uint64(os.Getpagesize()),
		users:    make(map[uint32]string),
	}
	c.host = c.readHost()
	if !opts.DisableContainers {
		if d, err := newDockerClient(); err == nil {
			c.docker = d
		}
	}
	c.prime()
	return c
}

// prime takes the first reading of every cumulative counter without producing
// a snapshot.
func (c *Collector) prime() {
	if f, err := c.src.Proc.Open("stat"); err == nil {
		c.prevStat, _ = parseStat(f)
		f.Close()
	}
	c.prevNet, _ = c.readNet()
	c.prevDisk, _ = c.readDisk()
	c.prevProc = make(map[int]procSample)
	if pids, err := scanPIDs(c.src.Proc); err == nil {
		for _, pid := range pids {
			if st, err := c.readProcStat(pid); err == nil {
				c.prevProc[pid] = procSample{ticks: st.ticks(), starttime: st.starttime}
			}
		}
	}
	c.prevAt = time.Now()
}

// Collect takes one reading of the whole system.
//
// A failure in any single collector is recorded in Snapshot.Errs and leaves
// that field zero. One unreadable file must not cost you the rest of the
// screen, which is exactly what a monitoring tool is for.
func (c *Collector) Collect(ctx context.Context) Snapshot {
	now := time.Now()
	s := Snapshot{
		Taken:    now,
		Interval: now.Sub(c.prevAt),
		Host:     c.host,
	}
	// rate returns zero for a non-positive interval, so passing zero seconds
	// disables every derived figure in one place.
	secs := s.Interval.Seconds()
	if s.Interval < minRateInterval {
		secs = 0
	}

	fail := func(err error) {
		if err != nil {
			s.Errs = append(s.Errs, err)
		}
	}

	fail(c.collectCPU(&s, secs > 0))
	fail(c.collectMemory(&s))
	fail(c.collectLoad(&s))
	fail(c.collectNet(&s, secs))
	fail(c.collectDisk(&s, secs))
	fail(c.collectMounts(&s))
	fail(c.collectProcesses(&s))
	if sensors, err := readSensors(c.src.Sys); err == nil {
		s.Sensors = sensors
	}
	s.ContainersDisabled = c.opts.DisableContainers
	if c.docker != nil {
		s.ContainerRuntime = c.docker.runtime
		cs, err := c.docker.collect(ctx)
		if err == nil {
			s.Containers = cs
		}
	}

	// Uptime is cheap and changes every tick, unlike the rest of Host.
	if f, err := c.src.Proc.Open("uptime"); err == nil {
		s.Host.Uptime, _ = parseUptime(f)
		f.Close()
	}

	c.prevAt = now
	return s
}

// readHost reads the values that do not change while the program runs.
func (c *Collector) readHost() Host {
	h := Host{}
	h.Hostname, _ = os.Hostname()
	if f, err := c.src.Proc.Open("sys/kernel/osrelease"); err == nil {
		h.Kernel, _ = parseKernel(f)
		f.Close()
	}
	if f, err := c.src.Proc.Open("stat"); err == nil {
		st, _ := parseStat(f)
		f.Close()
		h.CPUCount = len(st.perCPU)
	}
	if h.CPUCount == 0 {
		h.CPUCount = 1
	}
	return h
}

// collectCPU reads /proc/stat. When rates is false the sample is still stored,
// so the next collection has something to diff against, but no percentage is
// derived from it.
func (c *Collector) collectCPU(s *Snapshot, rates bool) error {
	f, err := c.src.Proc.Open("stat")
	if err != nil {
		return err
	}
	defer f.Close()
	cur, err := parseStat(f)
	if err != nil {
		return err
	}

	if !rates {
		c.lastTotalDelta = 0
		c.prevStat = cur
		s.PerCPU = make([]CPU, len(cur.perCPU))
		for i, t := range cur.perCPU {
			s.PerCPU[i] = CPU{Name: t.name}
		}
		return nil
	}

	if cur.total.total() >= c.prevStat.total.total() {
		c.lastTotalDelta = float64(cur.total.total() - c.prevStat.total.total())
	} else {
		c.lastTotalDelta = 0
	}

	s.CPU = cpuDelta(c.prevStat.total, cur.total)
	s.PerCPU = make([]CPU, 0, len(cur.perCPU))
	for i, t := range cur.perCPU {
		// Cores can go offline and come back, so match by name rather than by
		// position in the slice.
		var prev cpuTicks
		if i < len(c.prevStat.perCPU) && c.prevStat.perCPU[i].name == t.name {
			prev = c.prevStat.perCPU[i]
		} else {
			for _, p := range c.prevStat.perCPU {
				if p.name == t.name {
					prev = p
					break
				}
			}
		}
		s.PerCPU = append(s.PerCPU, cpuDelta(prev, t))
	}
	c.prevStat = cur
	return nil
}

func (c *Collector) collectMemory(s *Snapshot) error {
	f, err := c.src.Proc.Open("meminfo")
	if err != nil {
		return err
	}
	defer f.Close()
	s.Memory, s.Swap, err = parseMeminfo(f)
	return err
}

func (c *Collector) collectLoad(s *Snapshot) error {
	f, err := c.src.Proc.Open("loadavg")
	if err != nil {
		return err
	}
	defer f.Close()
	s.Load, err = parseLoadavg(f)
	return err
}

func (c *Collector) readNet() (map[string]netCounters, error) {
	f, err := c.src.Proc.Open("net/dev")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseNetDev(f)
}

func (c *Collector) collectNet(s *Snapshot, secs float64) error {
	cur, err := c.readNet()
	if err != nil {
		return err
	}
	for name, n := range cur {
		prev := c.prevNet[name]
		iface := Network{
			Name:      name,
			RxBytes:   n.rxBytes,
			TxBytes:   n.txBytes,
			RxPackets: n.rxPackets,
			TxPackets: n.txPackets,
			RxErrs:    n.rxErrs,
			TxErrs:    n.txErrs,
			RxDrops:   n.rxDrops,
			TxDrops:   n.txDrops,
			RxRate:    rate(prev.rxBytes, n.rxBytes, secs),
			TxRate:    rate(prev.txBytes, n.txBytes, secs),
		}
		state, _ := readTrimmed(c.src.Sys, path.Join("class/net", name, "operstate"))
		// The loopback interface reports "unknown" rather than "up", as do
		// several virtual interfaces, so anything not explicitly down counts
		// as up.
		iface.Up = state != "down"
		s.Networks = append(s.Networks, iface)
	}
	c.prevNet = cur
	return nil
}

func (c *Collector) readDisk() (map[string]diskCounters, error) {
	f, err := c.src.Proc.Open("diskstats")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseDiskstats(f)
}

func (c *Collector) collectDisk(s *Snapshot, secs float64) error {
	cur, err := c.readDisk()
	if err != nil {
		return err
	}
	whole := c.wholeDisks()
	for name, d := range cur {
		// /proc/diskstats lists partitions alongside their parent device, and
		// counting both reports the same I/O twice. /sys/block holds exactly
		// the whole devices, so it is the filter.
		if whole != nil && !whole[name] {
			continue
		}
		if !d.used() {
			continue
		}
		prev := c.prevDisk[name]
		s.Disks = append(s.Disks, Disk{
			Name:       name,
			ReadBytes:  d.readSectors * sectorSize,
			WriteBytes: d.writeSectors * sectorSize,
			ReadOps:    d.readOps,
			WriteOps:   d.writeOps,
			ReadRate:   rate(prev.readSectors, d.readSectors, secs) * sectorSize,
			WriteRate:  rate(prev.writeSectors, d.writeSectors, secs) * sectorSize,
			IOPSRead:   rate(prev.readOps, d.readOps, secs),
			IOPSWrite:  rate(prev.writeOps, d.writeOps, secs),
		})
	}
	c.prevDisk = cur
	return nil
}

// wholeDisks returns the set of whole block devices, or nil if /sys is not
// readable, in which case the caller shows every device rather than none.
func (c *Collector) wholeDisks() map[string]bool {
	ents, err := fs.ReadDir(c.src.Sys, "block")
	if err != nil {
		return nil
	}
	out := make(map[string]bool, len(ents))
	for _, e := range ents {
		out[e.Name()] = true
	}
	return out
}

func (c *Collector) collectMounts(s *Snapshot) error {
	f, err := c.src.Proc.Open("self/mounts")
	if err != nil {
		return err
	}
	entries, err := parseMounts(f)
	f.Close()
	if err != nil {
		return err
	}

	nodev := map[string]bool{}
	if pf, err := c.src.Proc.Open("filesystems"); err == nil {
		nodev, _ = parseFilesystems(pf)
		pf.Close()
	}

	seen := make(map[string]bool)
	for _, m := range entries {
		if !keepMount(m, nodev, seen) {
			continue
		}
		total, used, avail, iUsed, iFree, err := statfsUsage(m.path)
		if err != nil || total == 0 {
			continue
		}
		s.Mounts = append(s.Mounts, Mount{
			Device:     m.device,
			Path:       m.path,
			FSType:     m.fstype,
			Total:      total,
			Used:       used,
			Free:       avail,
			Percent:    pct(used, used+avail),
			InodesUsed: iUsed,
			InodesFree: iFree,
		})
	}
	return nil
}

// readProcStat reads and parses one process's stat file.
func (c *Collector) readProcStat(pid int) (procStat, error) {
	b, err := readFile(c.src.Proc, strconv.Itoa(pid)+"/stat")
	if err != nil {
		return procStat{}, err
	}
	return parseProcStat(b)
}

func (c *Collector) collectProcesses(s *Snapshot) error {
	pids, err := scanPIDs(c.src.Proc)
	if err != nil {
		return err
	}

	// The denominator for every process percentage is the total CPU time the
	// machine accrued over this interval, taken from the aggregate line of
	// /proc/stat. Dividing one tick count by another cancels USER_HZ, so the
	// value of that constant never enters the calculation.
	totalTicks := c.lastTotalDelta

	procs := make([]Process, 0, len(pids))
	cur := make(map[int]procSample, len(pids))
	var counts ProcCounts

	for _, pid := range pids {
		st, err := c.readProcStat(pid)
		if err != nil {
			// The process exited between the directory scan and the read.
			// That is routine, not an error worth reporting.
			continue
		}
		cur[pid] = procSample{ticks: st.ticks(), starttime: st.starttime}
		countState(&counts, st.state)
		counts.Threads += st.numThreads
		if st.kernel() {
			counts.Kernel++
		}

		p := Process{
			PID:     st.pid,
			PPID:    st.ppid,
			Name:    st.comm,
			Kernel:  st.kernel(),
			State:   st.state,
			Threads: st.numThreads,
			Nice:    st.nice,
			RSS:     st.rssPages * c.pageSize,
			VMS:     st.vsize,
			CPUTime: time.Duration(st.ticks()) * time.Second / userHZ,
		}
		p.MemPct = pct(p.RSS, s.Memory.Total)

		if prev, ok := c.prevProc[pid]; ok && prev.starttime == st.starttime && totalTicks > 0 {
			delta := float64(st.ticks() - prev.ticks)
			if st.ticks() >= prev.ticks {
				p.CPU = delta / totalTicks * float64(c.host.CPUCount) * 100
			}
		}
		// A kernel thread has no user-space address space, so status and
		// cmdline hold nothing: no command line, no VmSwap line, and no user
		// context other than root. Reading and parsing them anyway was two
		// thirds of the cost of a collection on a host where 148 of 163
		// processes were kernel threads, which is the ordinary case.
		if st.kernel() {
			p.User = c.userName(0)
			procs = append(procs, p)
			continue
		}

		if f, err := c.src.Proc.Open(strconv.Itoa(pid) + "/status"); err == nil {
			status, err := parseProcStatus(f)
			f.Close()
			if err == nil {
				p.Swap = status.swap
				if status.hasUID {
					p.User = c.userName(status.uid)
				}
			}
		}
		if b, err := readFile(c.src.Proc, strconv.Itoa(pid)+"/cmdline"); err == nil {
			p.Cmdline = parseCmdline(b)
		}
		procs = append(procs, p)
	}

	c.prevProc = cur
	s.Processes = procs
	s.ProcCounts = counts
	return nil
}

// userName returns the username for a user ID, falling back to the number.
//
// Lookups are cached because each one reads /etc/passwd or goes through NSS,
// and a machine has far fewer distinct users than processes.
func (c *Collector) userName(uid uint32) string {
	if name, ok := c.users[uid]; ok {
		return name
	}
	name := strconv.FormatUint(uint64(uid), 10)
	if u, err := user.LookupId(name); err == nil {
		name = u.Username
	}
	c.users[uid] = name
	return name
}
