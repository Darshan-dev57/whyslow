// Package procfs parses the parts of /proc that whyslow samples.
//
// Every parser works on bytes or an io.Reader so it can be tested against
// fixture files; FS methods only add the file reads. Nothing here keeps
// state between calls: turning counters into rates is the collector's job.
package procfs

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// UserHZ is the unit of the CPU time fields in /proc. The kernel always
// exports clock ticks at USER_HZ, which is 100 on every Linux ABI.
const UserHZ = 100

// FS is a /proc mount. Root is normally "/proc"; tests point it at a fixture tree.
type FS struct {
	Root string
}

// New returns an FS rooted at root.
func New(root string) FS { return FS{Root: root} }

// Default is the host's /proc.
func Default() FS { return FS{Root: "/proc"} }

func (fs FS) path(elem ...string) string {
	return filepath.Join(append([]string{fs.Root}, elem...)...)
}

// CPUStat holds the aggregate "cpu" line of /proc/stat, in clock ticks.
type CPUStat struct {
	User, Nice, System, Idle, IOWait, IRQ, SoftIRQ, Steal uint64
}

// Total is all ticks except guest time, which the kernel already counts inside user.
func (c CPUStat) Total() uint64 {
	return c.User + c.Nice + c.System + c.Idle + c.IOWait + c.IRQ + c.SoftIRQ + c.Steal
}

// IdleAll is idle plus iowait: time the CPU had nothing to run.
func (c CPUStat) IdleAll() uint64 { return c.Idle + c.IOWait }

// Stat is the subset of /proc/stat whyslow uses.
type Stat struct {
	CPU      CPUStat
	NumCPU   int
	BootTime int64 // unix seconds
}

// Stat reads /proc/stat.
func (fs FS) Stat() (Stat, error) {
	f, err := os.Open(fs.path("stat"))
	if err != nil {
		return Stat{}, err
	}
	defer f.Close()
	return ParseStat(f)
}

// ParseStat parses the format of /proc/stat.
func ParseStat(r io.Reader) (Stat, error) {
	var s Stat
	sawCPU := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024) // the intr line can be long
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "cpu "):
			f := strings.Fields(line)[1:]
			if len(f) < 8 {
				return s, fmt.Errorf("procfs: short cpu line %q", line)
			}
			var v [8]uint64
			for i := range v {
				n, err := strconv.ParseUint(f[i], 10, 64)
				if err != nil {
					return s, fmt.Errorf("procfs: cpu field %d: %w", i, err)
				}
				v[i] = n
			}
			s.CPU = CPUStat{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7]}
			sawCPU = true
		case strings.HasPrefix(line, "cpu"):
			s.NumCPU++
		case strings.HasPrefix(line, "btime "):
			n, err := strconv.ParseInt(strings.TrimSpace(line[len("btime "):]), 10, 64)
			if err != nil {
				return s, fmt.Errorf("procfs: btime: %w", err)
			}
			s.BootTime = n
		}
	}
	if err := sc.Err(); err != nil {
		return s, err
	}
	if !sawCPU {
		return s, errors.New("procfs: no aggregate cpu line in stat")
	}
	return s, nil
}

// MemInfo holds memory totals in bytes.
type MemInfo struct {
	Total     uint64
	Available uint64
}

// Used is memory not available to new allocations without swapping.
func (m MemInfo) Used() uint64 {
	if m.Available > m.Total {
		return 0
	}
	return m.Total - m.Available
}

// MemInfo reads /proc/meminfo.
func (fs FS) MemInfo() (MemInfo, error) {
	f, err := os.Open(fs.path("meminfo"))
	if err != nil {
		return MemInfo{}, err
	}
	defer f.Close()
	return ParseMemInfo(f)
}

// ParseMemInfo parses the format of /proc/meminfo. Values there are in KiB.
func ParseMemInfo(r io.Reader) (MemInfo, error) {
	var m MemInfo
	var haveTotal, haveAvail bool
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		var dst *uint64
		switch key {
		case "MemTotal":
			dst, haveTotal = &m.Total, true
		case "MemAvailable":
			dst, haveAvail = &m.Available, true
		default:
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			return m, fmt.Errorf("procfs: empty %s", key)
		}
		n, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return m, fmt.Errorf("procfs: %s: %w", key, err)
		}
		*dst = n * 1024
	}
	if err := sc.Err(); err != nil {
		return m, err
	}
	if !haveTotal || !haveAvail {
		return m, errors.New("procfs: meminfo lacks MemTotal or MemAvailable")
	}
	return m, nil
}

// SectorSize is the unit of /proc/diskstats sector counts. The kernel always
// reports 512-byte sectors there, whatever the device's real sector size.
const SectorSize = 512

// DiskStat is one line of /proc/diskstats.
type DiskStat struct {
	Name           string
	SectorsRead    uint64
	SectorsWritten uint64
}

// DiskStats reads /proc/diskstats.
func (fs FS) DiskStats() ([]DiskStat, error) {
	f, err := os.Open(fs.path("diskstats"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseDiskStats(f)
}

// ParseDiskStats parses the format of /proc/diskstats.
func ParseDiskStats(r io.Reader) ([]DiskStat, error) {
	var out []DiskStat
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		rd, err := strconv.ParseUint(f[5], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("procfs: diskstats %s sectors read: %w", f[2], err)
		}
		wr, err := strconv.ParseUint(f[9], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("procfs: diskstats %s sectors written: %w", f[2], err)
		}
		out = append(out, DiskStat{Name: f[2], SectorsRead: rd, SectorsWritten: wr})
	}
	return out, sc.Err()
}

// NetDev is one interface line of /proc/net/dev.
type NetDev struct {
	Name    string
	RxBytes uint64
	TxBytes uint64
}

// NetDev reads /proc/net/dev.
func (fs FS) NetDev() ([]NetDev, error) {
	f, err := os.Open(fs.path("net", "dev"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseNetDev(f)
}

// ParseNetDev parses the format of /proc/net/dev.
func ParseNetDev(r io.Reader) ([]NetDev, error) {
	var out []NetDev
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue // the two header lines
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			return nil, fmt.Errorf("procfs: short net/dev line for %q", name)
		}
		rx, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("procfs: net/dev rx: %w", err)
		}
		tx, err := strconv.ParseUint(f[8], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("procfs: net/dev tx: %w", err)
		}
		out = append(out, NetDev{Name: strings.TrimSpace(name), RxBytes: rx, TxBytes: tx})
	}
	return out, sc.Err()
}

// PIDs lists the numeric entries of /proc: one per process (thread group).
func (fs FS) PIDs() ([]int, error) {
	d, err := os.Open(fs.Root)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(names))
	for _, n := range names {
		if n == "" || n[0] < '0' || n[0] > '9' {
			continue
		}
		if pid, err := strconv.Atoi(n); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func readTrim(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	return bytes.TrimSpace(b), err
}
