// Package collector turns /proc and /sys counters into per-second rates for
// the whole system and for every application.
//
// The kernel exposes cumulative counters (CPU ticks, bytes read, bytes
// written). A rate is the difference between two readings divided by the
// real time between them, so the collector keeps the previous reading of
// every process, keyed by PID *and* start time: PIDs get reused, and a new
// process that inherits an old PID must not be charged the old one's total.
package collector

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/apps"
	"github.com/Darshan-dev57/whyslow/internal/model"
	"github.com/Darshan-dev57/whyslow/internal/procfs"
	"github.com/Darshan-dev57/whyslow/internal/sysfs"
)

// NetCounter is the cumulative traffic of one process as seen by a NetSource.
type NetCounter struct {
	Tx, Rx uint64
	Comm   string // the process name when the bytes moved; lets exited processes be named
}

// NetSource reports per-process network bytes. The eBPF source implements it;
// without one the collector reports system-level network only.
type NetSource interface {
	// Counters returns cumulative bytes per process (thread group id).
	Counters() (map[uint32]NetCounter, error)
	// Forget drops counters of processes that have exited.
	Forget(pids []uint32) error
}

// Options configures a Collector. Zero values pick the host's /proc and /sys.
type Options struct {
	Proc procfs.FS
	Sys  sysfs.FS
	Net  NetSource
	// SocketEvery counts each process's sockets every N samples (0 disables).
	// Reading every fd link of every process is the most expensive part of a
	// sample, and socket counts change slowly.
	SocketEvery int
	Now         func() time.Time
}

type procState struct {
	start     uint64 // clock ticks since boot; with the PID, identifies a process
	comm      string
	cmdline   []string
	ppid      int
	kernel    bool
	ticks     uint64 // own CPU time
	cticks    uint64 // CPU time of reaped children
	io        procfs.IO
	ioOK      bool
	sockets   int
	seen      uint64 // sample number that last saw this process
	startedAt time.Time
}

// Collector samples the machine. It is not safe for concurrent use; one
// goroutine owns it.
type Collector struct {
	opts       Options
	pageSize   uint64
	numCPU     int
	wholeDisks map[string]bool

	n          uint64
	prevTS     time.Time
	prevUp     float64 // uptime at the previous sample, seconds
	prevCPU    procfs.CPUStat
	prevDisk   [2]uint64
	prevNet    [2]uint64
	procs      map[int]*procState
	prevAppNet map[uint32]NetCounter
	appOf      map[uint32]string // last app name of each PID, for counters of exited processes
}

// New creates a collector and takes the first reading, so the first call to
// Sample already returns rates.
func New(opts Options) (*Collector, error) {
	if opts.Proc.Root == "" {
		opts.Proc = procfs.Default()
	}
	if opts.Sys.Root == "" {
		opts.Sys = sysfs.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	c := &Collector{
		opts:       opts,
		pageSize:   uint64(os.Getpagesize()),
		procs:      make(map[int]*procState),
		prevAppNet: make(map[uint32]NetCounter),
		appOf:      make(map[uint32]string),
	}
	if blocks, err := opts.Sys.BlockDevices(); err == nil {
		c.wholeDisks = blocks
	}
	if _, err := c.sample(); err != nil {
		return nil, err
	}
	return c, nil
}

// NetPerApp reports whether per-app network numbers are available.
func (c *Collector) NetPerApp() bool { return c.opts.Net != nil }

// Sample reads every counter and returns rates since the previous call.
func (c *Collector) Sample() (*model.Snapshot, error) { return c.sample() }

type appAcc struct {
	model.App
	ticks uint64
}

func (c *Collector) sample() (*model.Snapshot, error) {
	now := c.opts.Now()
	stat, err := c.opts.Proc.Stat()
	if err != nil {
		return nil, fmt.Errorf("collector: %w", err)
	}
	up, err := c.opts.Proc.Uptime()
	if err != nil {
		return nil, fmt.Errorf("collector: %w", err)
	}
	mem, err := c.opts.Proc.MemInfo()
	if err != nil {
		return nil, fmt.Errorf("collector: %w", err)
	}
	diskR, diskW := c.diskTotals()
	netRx, netTx := c.netTotals()

	first := c.n == 0
	c.n++
	elapsed := now.Sub(c.prevTS)
	secs := elapsed.Seconds()
	if first || secs <= 0 {
		secs = 0
	}
	rate := func(cur, prev uint64) float64 {
		if secs == 0 || cur < prev {
			return 0
		}
		return float64(cur-prev) / secs
	}

	snap := &model.Snapshot{TS: now, Elapsed: elapsed, NetPerApp: c.NetPerApp()}
	snap.Sys = model.System{
		NumCPU:    stat.NumCPU,
		MemUsed:   mem.Used(),
		MemTotal:  mem.Total,
		DiskRead:  rate(diskR, c.prevDisk[0]),
		DiskWrite: rate(diskW, c.prevDisk[1]),
		NetRx:     rate(netRx, c.prevNet[0]),
		NetTx:     rate(netTx, c.prevNet[1]),
	}
	if dt := stat.CPU.Total() - c.prevCPU.Total(); !first && stat.CPU.Total() > c.prevCPU.Total() {
		idle := stat.CPU.IdleAll() - c.prevCPU.IdleAll()
		if idle > dt {
			idle = dt
		}
		snap.Sys.CPU = 100 * float64(dt-idle) / float64(dt)
	}
	if b := c.opts.Sys.Battery(); b != nil {
		snap.Sys.Battery = &model.Battery{Percent: b.Percent, Status: b.Status, Watts: b.PowerWatts,
			EnergyWh: b.EnergyWh, HasPower: b.HasPower, HasEnergy: b.HasEnergy}
	}
	if th := c.opts.Sys.Temperature(); th != nil {
		t := th.Celsius
		snap.Sys.TempC = &t
	}

	accs, err := c.sampleProcs(now, up, secs, first)
	if err != nil {
		return nil, err
	}
	snap.Apps = make([]model.App, 0, len(accs))
	for _, a := range accs {
		snap.Apps = append(snap.Apps, a.App)
	}
	sort.Slice(snap.Apps, func(i, j int) bool {
		if snap.Apps[i].CPU != snap.Apps[j].CPU {
			return snap.Apps[i].CPU > snap.Apps[j].CPU
		}
		return snap.Apps[i].Name < snap.Apps[j].Name
	})

	c.prevTS, c.prevUp, c.prevCPU = now, up, stat.CPU
	c.prevDisk = [2]uint64{diskR, diskW}
	c.prevNet = [2]uint64{netRx, netTx}
	c.numCPU = stat.NumCPU
	return snap, nil
}

// reading is one process's counters at this sample.
type reading struct {
	st    procfs.ProcStat
	ps    *procState
	fresh bool // first time this PID+start is seen
	born  bool // started after the previous sample
	io    procfs.IO
	ioOK  bool
}

func (c *Collector) sampleProcs(now time.Time, up, secs float64, first bool) (map[string]*appAcc, error) {
	pids, err := c.opts.Proc.PIDs()
	if err != nil {
		return nil, fmt.Errorf("collector: %w", err)
	}
	// A process whose start time is after the previous sample was born in
	// this interval: everything it has done so far happened in the interval.
	// The uptime clock and process start times share a timebase.
	bornAfter := uint64(c.prevUp*procfs.UserHZ) - procfs.UserHZ/10
	countSockets := c.opts.SocketEvery > 0 && (c.n-1)%uint64(c.opts.SocketEvery) == 0

	reads := make(map[int]*reading, len(pids))
	for _, pid := range pids {
		st, err := c.opts.Proc.ProcStat(pid)
		if err != nil {
			continue // exited between listing and reading
		}
		ps, ok := c.procs[pid]
		fresh := !ok || ps.start != st.StartTime
		if fresh {
			ps = &procState{start: st.StartTime}
			ps.startedAt = now.Add(-time.Duration((up - float64(st.StartTime)/procfs.UserHZ) * float64(time.Second)))
			c.procs[pid] = ps
		}
		r := &reading{st: st, ps: ps, fresh: fresh, born: fresh && !first && st.StartTime >= bornAfter}
		// exec changes comm but keeps the PID, so re-read the command line then.
		if fresh || ps.comm != st.Comm {
			ps.cmdline, _ = c.opts.Proc.Cmdline(pid)
			ps.comm = st.Comm
		}
		ps.kernel, ps.seen = st.IsKernelThread(), c.n
		if !ps.kernel {
			r.io, err = c.opts.Proc.IO(pid)
			r.ioOK = err == nil
			if countSockets {
				if n, err := c.opts.Proc.SocketCount(pid); err == nil {
					ps.sockets = n
				}
			}
		}
		reads[pid] = r
	}

	// When a parent waits for an exited child, the kernel adds the child's
	// CPU time to the parent's cutime/cstime and the child's I/O to the
	// parent's /proc/[pid]/io. Children that lived and died between two
	// samples are only visible that way. To avoid counting children we did
	// see twice, their last-seen totals are credited back to the parent.
	// A chain can die in one interval (make -> sh -> cc): the parent's jump
	// then includes the whole chain, so each dead process passes on its own
	// last-seen totals plus those of its dead children.
	credits := c.reapCredits()

	type delta struct{ ticks, read, wrote uint64 }
	deltas := make(map[int]delta, len(reads))
	tree := make(map[int]*apps.Proc, len(reads))
	for pid, r := range reads {
		ps, st := r.ps, r.st
		cr := credits[pid]
		var d delta
		ticks, cticks := st.CPUTicks(), st.ChildTicks()
		switch {
		case r.born:
			d.ticks = ticks + cticks
		case !r.fresh:
			d.ticks = sub(ticks, ps.ticks) + sub(sub(cticks, ps.cticks), cr.ticks)
		}
		ps.ticks, ps.cticks = ticks, cticks
		if r.ioOK {
			switch {
			case r.born:
				d.read, d.wrote = r.io.ReadBytes, r.io.WriteBytes
			case !r.fresh && ps.ioOK:
				d.read = sub(sub(r.io.ReadBytes, ps.io.ReadBytes), cr.read)
				d.wrote = sub(sub(r.io.WriteBytes, ps.io.WriteBytes), cr.wrote)
			}
			ps.io = r.io
		}
		ps.ioOK = r.ioOK
		ps.ppid = st.PPID
		deltas[pid] = d
		tree[pid] = &apps.Proc{PID: pid, PPID: st.PPID, Comm: st.Comm, Cmdline: ps.cmdline, Kernel: ps.kernel}
	}

	names := apps.Group(tree)
	accs := make(map[string]*appAcc)
	get := func(name string) *appAcc {
		a := accs[name]
		if a == nil {
			a = &appAcc{App: model.App{Name: name, NetActive: c.NetPerApp()}}
			accs[name] = a
		}
		return a
	}
	for pid, name := range names {
		ps := c.procs[pid]
		d := deltas[pid]
		a := get(name)
		a.Procs++
		a.ticks += d.ticks
		a.DiskRead += float64(d.read)
		a.DiskWrite += float64(d.wrote)
		a.Sockets += ps.sockets
		if !ps.ioOK && !ps.kernel {
			a.IOHidden++
		}
		if rss, err := c.opts.Proc.Statm(pid); err == nil {
			a.RSS += rss * c.pageSize
		}
		if a.Started.IsZero() || ps.startedAt.Before(a.Started) {
			a.Started = ps.startedAt
		}
		c.appOf[uint32(pid)] = name
	}

	if c.opts.Net != nil {
		c.addNet(get, secs)
	}

	for _, a := range accs {
		if secs > 0 {
			a.CPU = 100 * float64(a.ticks) / procfs.UserHZ / secs
			a.DiskRead /= secs
			a.DiskWrite /= secs
		} else {
			a.CPU, a.DiskRead, a.DiskWrite = 0, 0, 0
		}
	}
	return accs, nil
}

type credit struct{ ticks, read, wrote uint64 }

func (cr *credit) add(o credit) {
	cr.ticks += o.ticks
	cr.read += o.read
	cr.wrote += o.wrote
}

// reapCredits removes processes that were not seen in this sample and
// returns, per live parent PID, the totals already counted on its dead
// descendants.
func (c *Collector) reapCredits() map[int]credit {
	dead := map[int]*procState{}
	for pid, ps := range c.procs {
		if ps.seen != c.n {
			dead[pid] = ps
			delete(c.procs, pid)
		}
	}
	kids := map[int][]int{}
	for pid, ps := range dead {
		kids[ps.ppid] = append(kids[ps.ppid], pid)
	}
	var total func(pid, depth int) credit
	total = func(pid, depth int) credit {
		ps := dead[pid]
		cr := credit{ticks: ps.ticks + ps.cticks}
		if ps.ioOK {
			cr.read, cr.wrote = ps.io.ReadBytes, ps.io.WriteBytes
		}
		if depth < 64 { // PIDs are reused, so guard against a cycle
			for _, k := range kids[pid] {
				cr.add(total(k, depth+1))
			}
		}
		return cr
	}
	credits := map[int]credit{}
	for pid, ps := range dead {
		if _, parentDead := dead[ps.ppid]; parentDead {
			continue // counted in its parent's total
		}
		cr := credits[ps.ppid]
		cr.add(total(pid, 0))
		credits[ps.ppid] = cr
	}
	return credits
}

// sub is a - b, floored at zero: counters that went backwards mean nothing.
func sub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// addNet charges per-process network deltas to apps. Counters of processes
// that already exited are charged to the app they belonged to when last
// seen, or named from the comm the kernel recorded with the bytes.
func (c *Collector) addNet(get func(string) *appAcc, secs float64) {
	counters, err := c.opts.Net.Counters()
	if err != nil {
		return
	}
	var gone []uint32
	for pid, cur := range counters {
		prev := c.prevAppNet[pid]
		var tx, rx uint64
		if cur.Tx >= prev.Tx {
			tx = cur.Tx - prev.Tx
		} else {
			tx = cur.Tx // the entry was evicted and recreated
		}
		if cur.Rx >= prev.Rx {
			rx = cur.Rx - prev.Rx
		} else {
			rx = cur.Rx
		}
		c.prevAppNet[pid] = cur
		_, alive := c.procs[int(pid)]
		if !alive {
			gone = append(gone, pid)
		}
		if tx == 0 && rx == 0 {
			continue
		}
		name, ok := c.appOf[pid]
		if !ok {
			name = apps.OwnName(&apps.Proc{Comm: cur.Comm})
		}
		a := get(name)
		if secs > 0 {
			a.NetTx += float64(tx) / secs
			a.NetRx += float64(rx) / secs
		}
	}
	if len(gone) > 0 {
		_ = c.opts.Net.Forget(gone)
		for _, pid := range gone {
			delete(c.prevAppNet, pid)
		}
	}
	for pid := range c.appOf {
		if _, alive := c.procs[int(pid)]; !alive {
			if _, pending := counters[pid]; !pending {
				delete(c.appOf, pid)
			}
		}
	}
}

// diskTotals sums sectors over whole physical disks. Partitions, device
// mapper volumes and loop devices would count the same bytes twice.
func (c *Collector) diskTotals() (read, written uint64) {
	disks, err := c.opts.Proc.DiskStats()
	if err != nil {
		return 0, 0
	}
	for _, d := range disks {
		if !c.countDisk(d.Name) {
			continue
		}
		read += d.SectorsRead * procfs.SectorSize
		written += d.SectorsWritten * procfs.SectorSize
	}
	return read, written
}

var virtualDiskPrefixes = []string{"loop", "ram", "zram", "dm-", "md", "sr", "fd", "nbd"}

func (c *Collector) countDisk(name string) bool {
	for _, p := range virtualDiskPrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	if c.wholeDisks != nil {
		return c.wholeDisks[name]
	}
	return !IsPartition(name)
}

// IsPartition guesses from the name alone whether a block device is a
// partition, for systems where /sys/block cannot be read.
func IsPartition(name string) bool {
	switch {
	case strings.HasPrefix(name, "nvme"), strings.HasPrefix(name, "mmcblk"):
		// nvme0n1p2, mmcblk0p1: a 'p' and digits after the device number
		i := strings.LastIndexByte(name, 'p')
		return i > 0 && i < len(name)-1 && isDigits(name[i+1:]) && isDigit(name[i-1])
	default:
		// sda1, vda2, xvda3: trailing digits
		return len(name) > 0 && isDigit(name[len(name)-1])
	}
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return s != ""
}

var virtualIfacePrefixes = []string{"lo", "veth", "docker", "br-", "virbr", "cni", "flannel",
	"cali", "vnet", "tun", "tap", "wg", "tailscale", "zt", "dummy", "kube", "cilium", "lxc", "ifb"}

// IsPhysicalIface reports whether traffic on an interface should count as the
// machine's own traffic. Loopback, container bridges and VPN tunnels carry
// bytes that also cross (or never leave) a real interface.
func IsPhysicalIface(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

func (c *Collector) netTotals() (rx, tx uint64) {
	devs, err := c.opts.Proc.NetDev()
	if err != nil {
		return 0, 0
	}
	for _, d := range devs {
		if IsPhysicalIface(d.Name) {
			rx += d.RxBytes
			tx += d.TxBytes
		}
	}
	return rx, tx
}
