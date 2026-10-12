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

// pfKthread is PF_KTHREAD from include/linux/sched.h, exposed in the flags
// field of /proc/[pid]/stat.
const pfKthread = 0x00200000

// ProcStat is the subset of /proc/[pid]/stat whyslow uses.
type ProcStat struct {
	PID        int
	Comm       string
	State      byte
	PPID       int
	Flags      uint64
	UTime      uint64 // clock ticks
	STime      uint64 // clock ticks
	CUTime     uint64 // user time of reaped children, clock ticks
	CSTime     uint64 // system time of reaped children, clock ticks
	NumThreads int
	StartTime  uint64 // clock ticks since boot
}

// CPUTicks is user plus system time.
func (p ProcStat) CPUTicks() uint64 { return p.UTime + p.STime }

// ChildTicks is the CPU time of children this process has waited for.
func (p ProcStat) ChildTicks() uint64 { return p.CUTime + p.CSTime }

// IsKernelThread reports whether the task is a kernel thread.
func (p ProcStat) IsKernelThread() bool { return p.Flags&pfKthread != 0 }

// ProcStat reads /proc/[pid]/stat.
func (fs FS) ProcStat(pid int) (ProcStat, error) {
	b, err := os.ReadFile(fs.path(strconv.Itoa(pid), "stat"))
	if err != nil {
		return ProcStat{}, err
	}
	return ParseProcStat(b)
}

// ParseProcStat parses /proc/[pid]/stat. The comm field is wrapped in
// parentheses and may itself contain spaces and parentheses, for example
// "(Web Content)" or "((sd-pam))", so it is cut at the first '(' and the
// last ')' rather than split on spaces.
func ParseProcStat(b []byte) (ProcStat, error) {
	open := bytes.IndexByte(b, '(')
	end := bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return ProcStat{}, errors.New("procfs: stat has no comm")
	}
	pid, err := strconv.Atoi(string(bytes.TrimSpace(b[:open])))
	if err != nil {
		return ProcStat{}, fmt.Errorf("procfs: stat pid: %w", err)
	}
	p := ProcStat{PID: pid, Comm: string(b[open+1 : end])}
	// Fields after comm, numbered from 3 as in proc(5): state is field 3.
	f := strings.Fields(string(b[end+1:]))
	if len(f) < 20 {
		return ProcStat{}, fmt.Errorf("procfs: stat for pid %d has %d fields after comm", pid, len(f))
	}
	field := func(n int) string { return f[n-3] }
	if s := field(3); len(s) > 0 {
		p.State = s[0]
	}
	nums := []struct {
		n   int
		dst *uint64
	}{{9, &p.Flags}, {14, &p.UTime}, {15, &p.STime}, {22, &p.StartTime}}
	for _, x := range []struct {
		n   int
		dst *uint64
	}{{16, &p.CUTime}, {17, &p.CSTime}} {
		// cutime and cstime are signed in the kernel (long); never negative in practice.
		v, err := strconv.ParseInt(field(x.n), 10, 64)
		if err != nil {
			return ProcStat{}, fmt.Errorf("procfs: stat field %d: %w", x.n, err)
		}
		*x.dst = uint64(max(v, 0))
	}
	for _, x := range nums {
		v, err := strconv.ParseUint(field(x.n), 10, 64)
		if err != nil {
			return ProcStat{}, fmt.Errorf("procfs: stat field %d: %w", x.n, err)
		}
		*x.dst = v
	}
	if p.PPID, err = strconv.Atoi(field(4)); err != nil {
		return ProcStat{}, fmt.Errorf("procfs: stat ppid: %w", err)
	}
	if p.NumThreads, err = strconv.Atoi(field(20)); err != nil {
		return ProcStat{}, fmt.Errorf("procfs: stat threads: %w", err)
	}
	return p, nil
}

// Statm reads the resident set size, in pages, from /proc/[pid]/statm.
func (fs FS) Statm(pid int) (residentPages uint64, err error) {
	b, err := readTrim(fs.path(strconv.Itoa(pid), "statm"))
	if err != nil {
		return 0, err
	}
	return ParseStatm(b)
}

// ParseStatm returns the resident field (pages) of /proc/[pid]/statm.
func ParseStatm(b []byte) (uint64, error) {
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0, errors.New("procfs: short statm")
	}
	return strconv.ParseUint(f[1], 10, 64)
}

// IO is the storage-level I/O of a process from /proc/[pid]/io.
//
// ReadBytes counts bytes fetched from storage (page cache hits are free) and
// WriteBytes counts bytes dirtied in the page cache, which is when the
// kernel charges a write to a process, not when it reaches the disk.
type IO struct {
	ReadBytes  uint64
	WriteBytes uint64
}

// IO reads /proc/[pid]/io. For another user's process this needs root (it is
// gated by ptrace access), so callers should treat os.ErrPermission as
// "unknown" rather than zero.
func (fs FS) IO(pid int) (IO, error) {
	f, err := os.Open(fs.path(strconv.Itoa(pid), "io"))
	if err != nil {
		return IO{}, err
	}
	defer f.Close()
	return ParseIO(f)
}

// ParseIO parses the format of /proc/[pid]/io.
func ParseIO(r io.Reader) (IO, error) {
	var out IO
	var seen uint8 // bit 0: read_bytes, bit 1: write_bytes
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		var dst *uint64
		var bit uint8
		switch k {
		case "read_bytes":
			dst, bit = &out.ReadBytes, 1
		case "write_bytes":
			dst, bit = &out.WriteBytes, 2
		default:
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return IO{}, fmt.Errorf("procfs: io %s: %w", k, err)
		}
		*dst = n
		seen |= bit
	}
	if err := sc.Err(); err != nil {
		return IO{}, err
	}
	if seen != 3 {
		return IO{}, errors.New("procfs: io lacks read_bytes or write_bytes")
	}
	return out, nil
}

// Cmdline returns the NUL-separated argument vector of /proc/[pid]/cmdline.
// It is empty for kernel threads and zombies.
func (fs FS) Cmdline(pid int) ([]string, error) {
	b, err := os.ReadFile(fs.path(strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	return ParseCmdline(b), nil
}

// ParseCmdline splits a raw cmdline buffer.
func ParseCmdline(b []byte) []string {
	b = bytes.TrimRight(b, "\x00")
	if len(b) == 0 {
		return nil
	}
	parts := bytes.Split(b, []byte{0})
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out
}

// SocketCount counts the open file descriptors of a process that are sockets.
// It is the no-root fallback signal for "which apps talk to the network".
func (fs FS) SocketCount(pid int) (int, error) {
	dir := fs.path(strconv.Itoa(pid), "fd")
	d, err := os.Open(dir)
	if err != nil {
		return 0, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, name := range names {
		target, err := os.Readlink(filepath.Join(dir, name))
		if err != nil {
			continue // fd closed while we looked
		}
		if strings.HasPrefix(target, "socket:[") {
			n++
		}
	}
	return n, nil
}
