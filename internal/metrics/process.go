package metrics

import (
	"errors"
	"io"
	"io/fs"
	"strings"
)

// userHZ is the unit of the CPU times in /proc/<pid>/stat.
//
// The portable way to read this is sysconf(_SC_CLK_TCK), which needs cgo. It
// has been 100 on every Linux port since 2.6 and the kernel exposes no other
// source for it, so it is a constant here. Nothing important rides on it:
// CPU percentages are a ratio of two tick counts and cancel the unit out
// entirely. Only the absolute CPUTime column depends on this value.
const userHZ = 100

// pfKthread is the kernel's PF_KTHREAD task flag, which marks a task that runs
// entirely inside the kernel and has no user-space address space.
//
// This is the authoritative test. The two common alternatives are guesses that
// break: an empty command line also describes a zombie, whose address space
// the kernel has already torn down, and a parent of PID 2 misses a kernel
// thread whose parent exited. The flag is field 9 of the stat line, already
// being parsed, so the correct test costs nothing.
const pfKthread = 0x00200000

// procStat holds the fields taken from one /proc/<pid>/stat line.
type procStat struct {
	pid        int
	comm       string
	state      byte
	ppid       int
	flags      uint64
	utime      uint64
	stime      uint64
	nice       int
	numThreads int
	starttime  uint64
	vsize      uint64
	rssPages   uint64
}

// kernel reports whether the task is a kernel thread.
func (p procStat) kernel() bool { return p.flags&pfKthread != 0 }

// ticks returns the process's total CPU time in jiffies.
func (p procStat) ticks() uint64 { return p.utime + p.stime }

// errBadProcStat reports a stat line that does not match the documented
// layout, which in practice means the process exited mid-read.
var errBadProcStat = errors.New("malformed /proc/<pid>/stat")

// parseProcStat parses one /proc/<pid>/stat line.
//
// The second field is the executable name in parentheses, and the kernel does
// not escape it. A process named "(evil) 1 2 3" would break a naive split on
// whitespace, so the name is taken between the first '(' and the *last* ')',
// and fields are counted from there.
func parseProcStat(b []byte) (procStat, error) {
	var p procStat
	s := string(b)

	lp := strings.IndexByte(s, '(')
	rp := strings.LastIndexByte(s, ')')
	if lp < 0 || rp < lp {
		return p, errBadProcStat
	}
	p.pid = atoi(strings.TrimSpace(s[:lp]))
	p.comm = s[lp+1 : rp]

	// f[0] is field 3 of the documented layout, so field N is f[N-3].
	f := strings.Fields(s[rp+1:])
	if len(f) < 22 {
		return p, errBadProcStat
	}
	p.state = f[0][0]
	p.ppid = atoi(f[1])
	p.flags = atou(f[6])
	p.utime = atou(f[11])
	p.stime = atou(f[12])
	p.nice = atoi(f[16])
	p.numThreads = atoi(f[17])
	p.starttime = atou(f[19])
	p.vsize = atou(f[20])
	p.rssPages = atou(f[21])
	return p, nil
}

// procStatus holds the fields taken from /proc/<pid>/status.
type procStatus struct {
	uid    uint32
	swap   uint64 // bytes of this process's memory currently in swap
	hasUID bool
}

// parseProcStatus reads /proc/<pid>/status.
//
// This file is read for two fields that /proc/<pid>/stat does not carry: the
// owning user, and how much of the process sits in swap. Reading it replaces a
// separate stat call on the process directory that the owner used to come
// from, so the extra field costs one file read per process and no extra
// syscall beyond it.
//
// A kernel thread has no address space and so no VmSwap line, and a kernel
// built without CONFIG_SWAP omits it everywhere. Both cases leave the value at
// zero, which is the truth.
func parseProcStatus(r io.Reader) (procStatus, error) {
	var s procStatus
	err := scanLines(r, func(line string) error {
		switch {
		case strings.HasPrefix(line, "Uid:"):
			// Real, effective, saved set, and filesystem UID. The first is
			// the one every other tool shows.
			if f := strings.Fields(line[4:]); len(f) > 0 {
				s.uid, s.hasUID = uint32(atou(f[0])), true
			}
		case strings.HasPrefix(line, "VmSwap:"):
			// Every size in this file is in kibibytes, despite the kB suffix.
			if f := strings.Fields(line[7:]); len(f) > 0 {
				s.swap = atou(f[0]) << 10
			}
		}
		return nil
	})
	return s, err
}

// parseCmdline decodes /proc/<pid>/cmdline, whose arguments are separated and
// terminated by NUL bytes. A kernel thread has an empty cmdline.
func parseCmdline(b []byte) string {
	s := strings.TrimRight(string(b), "\x00")
	if s == "" {
		return ""
	}
	return strings.ReplaceAll(s, "\x00", " ")
}

// scanPIDs returns the process IDs currently in /proc.
//
// The directory is read without sorting, since the caller sorts the finished
// process list by a user-chosen column anyway.
func scanPIDs(proc fs.FS) ([]int, error) {
	f, err := proc.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rd, ok := f.(fs.ReadDirFile)
	if !ok {
		return nil, errors.New("/proc does not support directory reads")
	}
	ents, err := rd.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(ents))
	for _, e := range ents {
		// Only numeric names are processes; the rest of /proc is
		// system-wide files and named directories.
		name := e.Name()
		if name[0] < '0' || name[0] > '9' {
			continue
		}
		if pid := atoi(name); pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// countState folds one process into the state tallies.
func countState(c *ProcCounts, state byte) {
	c.Total++
	switch state {
	case 'R':
		c.Running++
	case 'S', 'D', 'I':
		c.Sleeping++
	case 'T', 't':
		c.Stopped++
	case 'Z':
		c.Zombie++
	}
}
