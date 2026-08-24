// Package metrics collects system metrics from the Linux /proc and /sys
// filesystems. It has no dependencies outside the standard library.
//
// Parsing and collection are deliberately separated. Every parse function
// takes an io.Reader or an fs.FS, so it carries no build constraint and can be
// tested on any platform against the fixtures in testdata. Only the syscall
// layer, in fs_linux.go, is Linux-only.
package metrics

import "time"

// Snapshot is one complete observation of the system. Every rate it contains
// is already computed against the previous observation, so a consumer renders
// the values as they are and holds no history of its own.
type Snapshot struct {
	Taken    time.Time
	Interval time.Duration // elapsed time since the previous snapshot

	Host       Host
	CPU        CPU
	PerCPU     []CPU
	Memory     Memory
	Swap       Swap
	Load       Load
	Networks   []Network
	Disks      []Disk
	Mounts     []Mount
	Processes  []Process
	ProcCounts ProcCounts
	Sensors    []Sensor
	Containers []Container

	// ContainerRuntime names the daemon the containers came from, "docker" or
	// "podman", and is empty when no runtime is reachable. The display needs
	// to tell "no containers" apart from "nothing to ask".
	ContainerRuntime string

	// Errs holds the failure of any collector that could not run. One
	// unreadable source must not cost you the rest of the screen, so a
	// collector records its error here and leaves its field zero.
	Errs []error
}

// Host identifies the machine and how long it has been up.
type Host struct {
	Hostname string
	Kernel   string
	Uptime   time.Duration
	CPUCount int
}

// CPU holds the share of an interval spent in each scheduler state, as a
// percentage of that interval. Fields sum to roughly 100 for a single core.
type CPU struct {
	Name    string // "cpu" for the total, "cpu0" and up for individual cores
	User    float64
	System  float64
	Nice    float64
	Idle    float64
	IOWait  float64
	IRQ     float64
	SoftIRQ float64
	Steal   float64
	Guest   float64

	// Busy is 100 minus Idle, the figure most monitors show as "CPU".
	Busy float64
}

// Memory holds physical memory in bytes.
//
// Used is total minus available rather than total minus free. The kernel hands
// back cache and reclaimable slab on demand, so free alone reads as alarmingly
// low on a healthy machine. MemAvailable is the kernel's own estimate of what
// a new allocation could actually get.
type Memory struct {
	Total     uint64
	Available uint64
	Used      uint64
	Free      uint64
	Buffers   uint64
	Cached    uint64
	Percent   float64
}

// Swap holds swap usage in bytes.
type Swap struct {
	Total   uint64
	Used    uint64
	Free    uint64
	Percent float64
}

// Load holds the run-queue averages and the process counts from /proc/loadavg.
type Load struct {
	One, Five, Fifteen float64
	Runnable, Total    int
}

// Network holds one interface's counters and the rates derived from them.
type Network struct {
	Name             string
	RxBytes, TxBytes uint64 // cumulative since boot
	RxPackets        uint64
	TxPackets        uint64
	RxErrs, TxErrs   uint64
	RxDrops, TxDrops uint64
	RxRate, TxRate   float64 // bytes per second over the last interval
	Up               bool
}

// Disk holds one block device's I/O counters and the rates derived from them.
type Disk struct {
	Name                  string
	ReadBytes, WriteBytes uint64 // cumulative since boot
	ReadOps, WriteOps     uint64
	ReadRate, WriteRate   float64 // bytes per second over the last interval
	IOPSRead, IOPSWrite   float64 // operations per second over the last interval
}

// Mount holds usage for one mounted filesystem, in bytes.
//
// Percent is computed against the space a non-root user can actually claim,
// which is what df reports: used / (used + available). The blocks the kernel
// reserves for root are neither used nor available, so including them would
// under-report a full disk.
type Mount struct {
	Device     string
	Path       string
	FSType     string
	Total      uint64
	Used       uint64
	Free       uint64 // available to an unprivileged user
	Percent    float64
	InodesUsed uint64
	InodesFree uint64
}

// Process holds one process and its share of the last interval.
type Process struct {
	PID     int
	PPID    int
	User    string
	Name    string
	Cmdline string
	State   byte
	Threads int
	Nice    int
	Kernel  bool          // a kernel thread, with no user-space address space
	CPU     float64       // percent of one core over the last interval
	MemPct  float64       // resident set as a percentage of total memory
	RSS     uint64        // resident set size in bytes
	VMS     uint64        // virtual memory size in bytes
	Swap    uint64        // bytes of this process's memory currently in swap
	CPUTime time.Duration // cumulative user + system time
}

// ProcCounts summarises the process table by state.
type ProcCounts struct {
	Total    int
	Running  int
	Sleeping int
	Stopped  int
	Zombie   int
	Threads  int
	Kernel   int // kernel threads, which are also counted in Total
}

// SensorKind distinguishes the readings that share the hwmon interface, since
// they need different units and different thresholds.
type SensorKind int

const (
	SensorTemp SensorKind = iota
	SensorFan
	SensorBattery
)

// Sensor is one hardware reading. Value is degrees Celsius for SensorTemp,
// RPM for SensorFan, and percent charge for SensorBattery.
type Sensor struct {
	Kind  SensorKind
	Chip  string
	Label string
	Value float64
	High  float64 // manufacturer's high threshold, zero if not published
	Crit  float64 // manufacturer's critical threshold, zero if not published
}

// Container holds one container's resource use, read from a Docker or Podman
// daemon. Every rate is zero for a container that is not running.
type Container struct {
	ID      string
	Name    string
	Image   string
	Command string
	State   string // "running", "exited", "paused", and so on
	Status  string // the daemon's own summary, such as "Up 3 hours"

	Created time.Time     // when the container was made
	Started time.Time     // when it last started, zero if it is not running
	Uptime  time.Duration // since Started, zero if it is not running

	CPU      float64 // percent of one core
	MemUsed  uint64
	MemLimit uint64
	MemPct   float64 // of MemLimit
	PIDs     int

	RxRate    float64 // bytes per second over the last interval
	TxRate    float64
	ReadRate  float64 // block I/O, bytes per second over the last interval
	WriteRate float64
}
