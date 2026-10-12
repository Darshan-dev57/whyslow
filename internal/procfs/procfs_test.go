package procfs

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

var fixture = New("testdata/proc")

func TestParseStat(t *testing.T) {
	s, err := fixture.Stat()
	if err != nil {
		t.Fatal(err)
	}
	want := CPUStat{7625765, 201, 1572274, 184970777, 431063, 0, 240343, 152370}
	if s.CPU != want {
		t.Errorf("cpu = %+v, want %+v", s.CPU, want)
	}
	if s.NumCPU != 4 {
		t.Errorf("NumCPU = %d, want 4", s.NumCPU)
	}
	if s.BootTime != 1791328918 {
		t.Errorf("BootTime = %d", s.BootTime)
	}
	if got, want := s.CPU.Total(), uint64(194992793); got != want {
		t.Errorf("Total = %d, want %d", got, want)
	}
	if got, want := s.CPU.IdleAll(), uint64(185401840); got != want {
		t.Errorf("IdleAll = %d, want %d", got, want)
	}
}

func TestParseStatErrors(t *testing.T) {
	for name, in := range map[string]string{
		"no cpu line": "btime 1\n",
		"short cpu":   "cpu 1 2 3\n",
		"bad number":  "cpu 1 2 3 4 5 6 7 x\n",
	} {
		if _, err := ParseStat(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseMemInfo(t *testing.T) {
	m, err := fixture.MemInfo()
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != 8147616*1024 || m.Available != 5700040*1024 {
		t.Errorf("got %+v", m)
	}
	if m.Used() != (8147616-5700040)*1024 {
		t.Errorf("Used = %d", m.Used())
	}
	if _, err := ParseMemInfo(strings.NewReader("MemTotal: 10 kB\n")); err == nil {
		t.Error("missing MemAvailable should fail")
	}
}

func TestParseDiskStats(t *testing.T) {
	d, err := fixture.DiskStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 6 {
		t.Fatalf("got %d disks", len(d))
	}
	vda := d[1]
	if vda.Name != "vda" || vda.SectorsRead != 559676382 || vda.SectorsWritten != 689967712 {
		t.Errorf("vda = %+v", vda)
	}
}

func TestParseNetDev(t *testing.T) {
	n, err := fixture.NetDev()
	if err != nil {
		t.Fatal(err)
	}
	want := []NetDev{
		{"lo", 5400799168, 5400799168},
		{"eth0", 23631449145, 116004496497},
		{"docker0", 249025708, 2219907724},
		{"veth720278a", 11939903, 273400374},
		{"wlp2s0", 1000, 2000},
	}
	if !reflect.DeepEqual(n, want) {
		t.Errorf("got %+v", n)
	}
}

func TestPIDs(t *testing.T) {
	pids, err := fixture.PIDs()
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]bool{}
	for _, p := range pids {
		got[p] = true
	}
	for _, p := range []int{1, 2, 777, 999, 4321} {
		if !got[p] {
			t.Errorf("pid %d missing from %v", p, pids)
		}
	}
	if len(pids) != 5 {
		t.Errorf("non-numeric entries leaked: %v", pids)
	}
}

func TestParseProcStat(t *testing.T) {
	cases := []struct {
		pid     int
		comm    string
		ppid    int
		kthread bool
		ticks   uint64
		threads int
		start   uint64
	}{
		{1, "systemd", 0, false, 2400, 1, 10},
		{2, "kthreadd", 0, true, 0, 1, 10},
		{777, "(sd-pam)", 1, false, 0, 1, 500},
		{4321, "Web Content", 4300, false, 29000, 30, 9000},
		{999, "weird ) name", 1, false, 10, 2, 20000},
	}
	for _, c := range cases {
		p, err := fixture.ProcStat(c.pid)
		if err != nil {
			t.Fatalf("pid %d: %v", c.pid, err)
		}
		if p.PID != c.pid || p.Comm != c.comm || p.PPID != c.ppid || p.IsKernelThread() != c.kthread ||
			p.CPUTicks() != c.ticks || p.NumThreads != c.threads || p.StartTime != c.start {
			t.Errorf("pid %d: got %+v", c.pid, p)
		}
	}
}

func TestParseProcStatErrors(t *testing.T) {
	for _, in := range []string{"", "12 no comm here", "x (a) S 1", "5 (a) S 1 2 3"} {
		if _, err := ParseProcStat([]byte(in)); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func TestStatm(t *testing.T) {
	n, err := fixture.Statm(4321)
	if err != nil || n != 51200 {
		t.Errorf("Statm = %d, %v", n, err)
	}
	if _, err := ParseStatm([]byte("1")); err == nil {
		t.Error("short statm should fail")
	}
}

func TestIO(t *testing.T) {
	io, err := fixture.IO(4321)
	if err != nil {
		t.Fatal(err)
	}
	if io != (IO{ReadBytes: 4096000, WriteBytes: 1228800}) {
		t.Errorf("got %+v", io)
	}
	if _, err := fixture.IO(1); err == nil {
		t.Error("truncated io should fail")
	}
	if _, err := fixture.IO(2); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing io: %v", err)
	}
}

func TestCmdline(t *testing.T) {
	c, err := fixture.Cmdline(4321)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/lib/firefox/firefox", "-contentproc", "-childID", "1"}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("got %q", c)
	}
	if c, _ := fixture.Cmdline(2); c != nil {
		t.Errorf("kernel thread cmdline = %q", c)
	}
}

func TestSocketCount(t *testing.T) {
	n, err := fixture.SocketCount(4321)
	if err != nil || n != 3 {
		t.Errorf("SocketCount = %d, %v; want 3", n, err)
	}
}

func TestUptime(t *testing.T) {
	u, err := fixture.Uptime()
	if err != nil || u != 350912.25 {
		t.Errorf("Uptime = %v, %v", u, err)
	}
	if _, err := ParseUptime([]byte(" ")); err == nil {
		t.Error("empty uptime should fail")
	}
}
