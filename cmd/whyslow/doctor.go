package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/cilium/ebpf/btf"
	"golang.org/x/sys/unix"

	"github.com/Darshan-dev57/whyslow/internal/netbpf"
	"github.com/Darshan-dev57/whyslow/internal/procfs"
	"github.com/Darshan-dev57/whyslow/internal/sysfs"
)

type level int

const (
	lvOK level = iota
	lvInfo
	lvWarn
	lvFail
)

func (l level) String() string {
	return [...]string{" ok ", "info", "warn", "FAIL"}[l]
}

type check struct {
	lv     level
	name   string
	detail string
}

// doctorEnv is what the checks look at; tests swap in fixtures.
type doctorEnv struct {
	proc    procfs.FS
	sys     sysfs.FS
	euid    int
	capEff  uint64 // effective capabilities
	jitPath string // /proc/sys/net/core/bpf_jit_enable
	btfPath string
	mounts  string // /proc/self/mounts
	kernel  func() (*btf.Spec, error)
	loadBPF func() (mode string, hooks []string, fallback, err error)
	dbPath  string
	release string
}

const (
	capNetAdmin = 12
	capSysAdmin = 21
	capPerfmon  = 38
	capBPF      = 39
)

func hostDoctorEnv() doctorEnv {
	var uts unix.Utsname
	_ = unix.Uname(&uts)
	env := doctorEnv{
		proc: procfs.Default(), sys: sysfs.Default(), euid: os.Geteuid(),
		jitPath: "/proc/sys/net/core/bpf_jit_enable", btfPath: "/sys/kernel/btf/vmlinux",
		kernel: btf.LoadKernelSpec, dbPath: defaultDBPath(), release: unix.ByteSliceToString(uts.Release[:]),
	}
	if b, err := os.ReadFile("/proc/self/mounts"); err == nil {
		env.mounts = string(b)
	}
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		env.capEff = capEffFrom(string(b))
	}
	env.loadBPF = func() (string, []string, error, error) {
		src, err := netbpf.Load()
		if err != nil {
			return "", nil, nil, err
		}
		defer src.Close()
		return string(src.Mode()), src.Attached(), src.Fallback(), nil
	}
	return env
}

func capEffFrom(status string) uint64 {
	for _, line := range strings.Split(status, "\n") {
		if v, ok := strings.CutPrefix(line, "CapEff:"); ok {
			n, _ := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			return n
		}
	}
	return 0
}

func (e doctorEnv) has(c uint) bool { return e.euid == 0 || e.capEff&(1<<c) != 0 }

func runChecks(e doctorEnv) []check {
	var out []check
	add := func(lv level, name, format string, a ...any) {
		out = append(out, check{lv, name, fmt.Sprintf(format, a...)})
	}
	add(lvInfo, "system", "Linux %s, %s, %d CPUs", e.release, runtime.GOARCH, runtime.NumCPU())

	// /proc
	pids, err := e.proc.PIDs()
	if err != nil {
		add(lvFail, "/proc", "cannot list processes: %v", err)
	} else {
		add(lvOK, "/proc", "%d processes visible", len(pids))
		hidden := 0
		for _, p := range pids {
			if _, err := e.proc.IO(p); errors.Is(err, os.ErrPermission) {
				hidden++
			}
		}
		if hidden > 0 {
			add(lvWarn, "disk I/O per app", "%d of %d processes hide /proc/[pid]/io (other users' processes); run as root to see their disk I/O", hidden, len(pids))
		} else {
			add(lvOK, "disk I/O per app", "every process's /proc/[pid]/io is readable")
		}
	}

	// battery and sensors
	if b := e.sys.Battery(); b != nil {
		power := "no power reading (drain is estimated from percentage)"
		if b.HasPower {
			power = fmt.Sprintf("%.1f W", b.PowerWatts)
		}
		add(lvOK, "battery", "%.0f%%, %s, %s", b.Percent, b.Status, power)
	} else {
		add(lvInfo, "battery", "none found (desktop or VM); battery hogs stay empty")
	}
	if t := e.sys.Temperature(); t != nil {
		add(lvOK, "temperature", "%.0f C from %s", t.Celsius, t.Sensor)
	} else {
		add(lvInfo, "temperature", "no sensors exposed (normal in VMs)")
	}

	// eBPF prerequisites
	privileged := e.has(capBPF) && e.has(capPerfmon) || e.has(capSysAdmin)
	if privileged {
		add(lvOK, "privileges", "can load eBPF (root or CAP_BPF+CAP_PERFMON)")
	} else {
		add(lvWarn, "privileges", "not root: --ebpf will fall back to system-level network (try sudo whyslow doctor)")
	}
	if _, err := os.Stat(e.btfPath); err != nil {
		add(lvWarn, "kernel BTF", "missing (%s); fentry/fexit needs it", e.btfPath)
	} else {
		add(lvOK, "kernel BTF", "present")
		if spec, err := e.kernel(); err != nil {
			add(lvWarn, "hooks", "cannot read BTF: %v", err)
		} else {
			var missing []string
			for _, h := range netbpf.Hooks {
				if _, err := h.Slot(spec); err != nil {
					missing = append(missing, h.Func)
				}
			}
			if len(missing) == 0 {
				add(lvOK, "hooks", "all %d send/receive functions found with the expected signatures", len(netbpf.Hooks))
			} else {
				add(lvWarn, "hooks", "not usable: %s", strings.Join(missing, ", "))
			}
		}
	}
	if b, err := os.ReadFile(e.jitPath); err != nil {
		add(lvWarn, "BPF JIT", "not built into this kernel: fentry/fexit cannot run; whyslow will use socket-owner mode")
	} else if strings.TrimSpace(string(b)) == "0" {
		add(lvWarn, "BPF JIT", "disabled (net.core.bpf_jit_enable=0): fentry/fexit cannot run")
	} else {
		add(lvOK, "BPF JIT", "enabled")
	}
	if strings.Contains(e.mounts, " cgroup2 ") {
		add(lvOK, "cgroup v2", "mounted (needed by the socket-owner fallback)")
	} else {
		add(lvInfo, "cgroup v2", "not mounted: no fallback if fentry/fexit fails")
	}
	if privileged && e.loadBPF != nil {
		mode, hooks, fallback, err := e.loadBPF()
		switch {
		case err != nil:
			add(lvFail, "eBPF load", "%v", err)
		case fallback != nil:
			add(lvOK, "eBPF load", "works in %s mode (%s); fentry was not possible: %v", mode, strings.Join(hooks, ", "), fallback)
		default:
			add(lvOK, "eBPF load", "works in %s mode (%s)", mode, strings.Join(hooks, ", "))
		}
	}

	// database
	if st, err := os.Stat(e.dbPath); err == nil {
		add(lvOK, "database", "%s (%.1f MB)", e.dbPath, float64(st.Size())/1e6)
	} else if errors.Is(err, os.ErrNotExist) {
		add(lvInfo, "database", "%s does not exist yet; whyslow run creates it", e.dbPath)
	} else {
		add(lvWarn, "database", "%s: %v", e.dbPath, err)
	}
	return out
}

func printChecks(w io.Writer, cs []check) (failed bool) {
	for _, c := range cs {
		fmt.Fprintf(w, "[%s] %-17s %s\n", c.lv, c.name, c.detail)
		failed = failed || c.lv == lvFail
	}
	return failed
}

func cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite database path to check")
	fs.Parse(args)
	env := hostDoctorEnv()
	env.dbPath = *dbPath
	if printChecks(os.Stdout, runChecks(env)) {
		return errors.New("some checks failed")
	}
	return nil
}
