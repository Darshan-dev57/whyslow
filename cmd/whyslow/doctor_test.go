package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cilium/ebpf/btf"

	"github.com/Darshan-dev57/whyslow/internal/procfs"
	"github.com/Darshan-dev57/whyslow/internal/sysfs"
)

func fixtureEnv(t *testing.T) doctorEnv {
	return doctorEnv{
		proc:    procfs.New(filepath.Join("..", "..", "internal", "procfs", "testdata", "proc")),
		sys:     sysfs.New(filepath.Join("..", "..", "internal", "sysfs", "testdata", "laptop")),
		euid:    1000,
		jitPath: filepath.Join(t.TempDir(), "absent"),
		btfPath: filepath.Join(t.TempDir(), "absent"),
		mounts:  "cgroup2 /sys/fs/cgroup cgroup2 rw 0 0\n",
		kernel:  func() (*btf.Spec, error) { return nil, errors.New("unused") },
		dbPath:  filepath.Join(t.TempDir(), "w.db"),
		release: "6.1.0",
	}
}

func TestDoctorUnprivileged(t *testing.T) {
	env := fixtureEnv(t)
	loaded := false
	env.loadBPF = func() (string, []string, error, error) { loaded = true; return "", nil, nil, nil }
	var buf bytes.Buffer
	failed := printChecks(&buf, runChecks(env))
	out := buf.String()
	if failed {
		t.Errorf("nothing here should fail:\n%s", out)
	}
	if loaded {
		t.Error("tried to load eBPF without privileges")
	}
	for _, want := range []string{"[ ok ] /proc", "[ ok ] battery", "[warn] privileges", "[warn] kernel BTF",
		"not built into this kernel", "[ ok ] cgroup v2", "does not exist yet"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestDoctorReportsFallbackAndFailure(t *testing.T) {
	env := fixtureEnv(t)
	env.euid = 0
	env.loadBPF = func() (string, []string, error, error) {
		return "socket-owner", []string{"sock_create"}, errors.New("no JIT"), nil
	}
	var buf bytes.Buffer
	printChecks(&buf, runChecks(env))
	if !strings.Contains(buf.String(), "works in socket-owner mode (sock_create); fentry was not possible: no JIT") {
		t.Errorf("fallback not reported:\n%s", buf.String())
	}
	env.loadBPF = func() (string, []string, error, error) { return "", nil, nil, errors.New("verifier said no") }
	buf.Reset()
	if !printChecks(&buf, runChecks(env)) || !strings.Contains(buf.String(), "[FAIL] eBPF load") {
		t.Errorf("load failure not reported:\n%s", buf.String())
	}
}

func TestCapEff(t *testing.T) {
	status := "Name:\tx\nCapInh:\t0000000000000000\nCapEff:\t000000c000001000\n"
	c := capEffFrom(status)
	env := doctorEnv{euid: 1000, capEff: c}
	if !env.has(capBPF) || !env.has(capPerfmon) || !env.has(capNetAdmin) || env.has(capSysAdmin) {
		t.Errorf("caps %x parsed wrong", c)
	}
}
