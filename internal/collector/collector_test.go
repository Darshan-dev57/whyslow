package collector

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/procfs"
	"github.com/Darshan-dev57/whyslow/internal/sysfs"
)

// fakeProc writes a minimal /proc tree that tests mutate between samples.
type fakeProc struct {
	t    *testing.T
	root string
}

type fakeTask struct {
	pid, ppid     int
	comm          string
	cmdline       string // NUL-separated
	ticks, start  uint64
	cticks        uint64 // CPU time of reaped children
	rssPages      uint64
	read, written uint64
	noIO, kernel  bool
}

func newFakeProc(t *testing.T) *fakeProc {
	f := &fakeProc{t: t, root: t.TempDir()}
	f.write("net/dev", "Inter-| hdr\n face |hdr\n")
	return f
}

func (f *fakeProc) write(name, content string) {
	p := filepath.Join(f.root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeProc) system(busy, idle uint64, uptime float64, diskR, diskW, ethRx, ethTx uint64) {
	f.write("stat", fmt.Sprintf("cpu  %d 0 0 %d 0 0 0 0 0 0\ncpu0 0 0 0 0 0 0 0 0 0 0\ncpu1 0 0 0 0 0 0 0 0 0 0\nbtime 1700000000\n", busy, idle))
	f.write("uptime", fmt.Sprintf("%.2f 0.00\n", uptime))
	f.write("meminfo", "MemTotal: 8000000 kB\nMemAvailable: 6000000 kB\n")
	f.write("diskstats", fmt.Sprintf(
		" 254 0 vda 1 0 %d 0 1 0 %d 0 0 0 0\n 254 1 vda1 1 0 %d 0 1 0 %d 0 0 0 0\n 7 0 loop0 1 0 999999 0 1 0 999999 0 0 0 0\n",
		diskR/512, diskW/512, diskR/512, diskW/512))
	f.write("net/dev", fmt.Sprintf("h1\nh2\n    lo: 777777 0 0 0 0 0 0 0 777777 0 0 0 0 0 0 0\n  eth0: %d 0 0 0 0 0 0 0 %d 0 0 0 0 0 0 0\ndocker0: 5555 0 0 0 0 0 0 0 5555 0 0 0 0 0 0 0\n", ethRx, ethTx))
}

func (f *fakeProc) task(k fakeTask) {
	dir := fmt.Sprint(k.pid)
	flags := 4194560
	if k.kernel {
		flags = 2129984
	}
	f.write(dir+"/stat", fmt.Sprintf("%d (%s) S %d %d %d 0 -1 %d 0 0 0 0 %d 0 %d 0 20 0 1 0 %d 1000 %d 0 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0\n",
		k.pid, k.comm, k.ppid, k.pid, k.pid, flags, k.ticks, k.cticks, k.start, k.rssPages))
	f.write(dir+"/statm", fmt.Sprintf("100 %d 0 0 0 0 0\n", k.rssPages))
	f.write(dir+"/cmdline", k.cmdline)
	if !k.noIO {
		f.write(dir+"/io", fmt.Sprintf("read_bytes: %d\nwrite_bytes: %d\n", k.read, k.written))
	}
}

func (f *fakeProc) remove(pid int) {
	if err := os.RemoveAll(filepath.Join(f.root, fmt.Sprint(pid))); err != nil {
		f.t.Fatal(err)
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type fakeNet struct {
	counters map[uint32]NetCounter
	forgot   []uint32
}

func (n *fakeNet) Counters() (map[uint32]NetCounter, error) {
	out := make(map[uint32]NetCounter, len(n.counters))
	for k, v := range n.counters {
		out[k] = v
	}
	return out, nil
}

func (n *fakeNet) Forget(pids []uint32) error {
	n.forgot = append(n.forgot, pids...)
	for _, p := range pids {
		delete(n.counters, p)
	}
	return nil
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestSampleRates(t *testing.T) {
	fp := newFakeProc(t)
	clk := &clock{time.Unix(1700001000, 0)}
	fp.system(1000, 9000, 1000.00, 1_000_000, 2_000_000, 10_000, 20_000)
	fp.task(fakeTask{pid: 1, comm: "systemd", cmdline: "/lib/systemd/systemd\x00", ticks: 50, start: 1, rssPages: 10})
	fp.task(fakeTask{pid: 2, comm: "kthreadd", ticks: 0, start: 1, kernel: true, noIO: true})
	fp.task(fakeTask{pid: 30, ppid: 2, comm: "kworker/0:1", ticks: 100, start: 5, kernel: true, noIO: true})
	fp.task(fakeTask{pid: 100, ppid: 1, comm: "chrome", cmdline: "/opt/google/chrome/chrome\x00", ticks: 500, start: 10000, rssPages: 1000, read: 4096, written: 0})
	fp.task(fakeTask{pid: 101, ppid: 100, comm: "chrome", cmdline: "/opt/google/chrome/chrome --type=renderer\x00", ticks: 300, start: 20000, rssPages: 2000})
	fp.task(fakeTask{pid: 200, ppid: 1, comm: "secret", cmdline: "secret\x00", ticks: 10, start: 30000, noIO: true})

	sys := sysfs.New(filepath.Join("..", "sysfs", "testdata", "vm"))
	c, err := New(Options{Proc: procfs.New(fp.root), Sys: sys, Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}

	// Two seconds later: 4 cores' worth of ticks pass, 25% of them busy.
	clk.t = clk.t.Add(2 * time.Second)
	fp.system(1100, 9300, 1002.00, 1_000_000+8_000_000, 2_000_000+512_000, 10_000+3_000, 20_000+1_000)
	fp.task(fakeTask{pid: 1, comm: "systemd", cmdline: "/lib/systemd/systemd\x00", ticks: 50, start: 1, rssPages: 10})
	fp.task(fakeTask{pid: 30, ppid: 2, comm: "kworker/0:1", ticks: 110, start: 5, kernel: true, noIO: true})
	fp.task(fakeTask{pid: 100, ppid: 1, comm: "chrome", cmdline: "/opt/google/chrome/chrome\x00", ticks: 600, start: 10000, rssPages: 1000, read: 4096 + 2_000_000})
	fp.task(fakeTask{pid: 101, ppid: 100, comm: "chrome", cmdline: "/opt/google/chrome/chrome --type=renderer\x00", ticks: 400, start: 20000, rssPages: 2000})
	// a process born after the first sample (start tick > 100000): all its work counts
	fp.task(fakeTask{pid: 300, ppid: 200, comm: "dd", cmdline: "dd\x00if=/dev/zero\x00", ticks: 40, start: 100050, written: 6_000_000})
	fp.task(fakeTask{pid: 200, ppid: 1, comm: "secret", cmdline: "secret\x00", ticks: 10, start: 30000, noIO: true})

	s, err := c.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if !near(s.Sys.CPU, 25) {
		t.Errorf("system cpu = %v, want 25", s.Sys.CPU)
	}
	if !near(s.Sys.DiskRead, 4_000_000) || !near(s.Sys.DiskWrite, 256_000) {
		t.Errorf("disk (partition and loop must not count) = %v / %v", s.Sys.DiskRead, s.Sys.DiskWrite)
	}
	if !near(s.Sys.NetRx, 1500) || !near(s.Sys.NetTx, 500) {
		t.Errorf("net (only eth0) = %v / %v", s.Sys.NetRx, s.Sys.NetTx)
	}
	if s.Sys.MemUsed != 2_000_000*1024 || s.Sys.NumCPU != 2 {
		t.Errorf("mem/ncpu = %d / %d", s.Sys.MemUsed, s.Sys.NumCPU)
	}
	if s.Sys.Battery != nil || s.Sys.TempC != nil {
		t.Error("VM fixture has no battery or sensors")
	}

	chrome := s.App("chrome")
	if chrome == nil || chrome.Procs != 2 || !near(chrome.CPU, 100) || !near(chrome.DiskRead, 1_000_000) {
		t.Fatalf("chrome = %+v", chrome)
	}
	if chrome.RSS != 3000*uint64(os.Getpagesize()) {
		t.Errorf("chrome rss = %d", chrome.RSS)
	}
	if s.Apps[0].Name != "chrome" {
		t.Errorf("apps not sorted by cpu: first is %s", s.Apps[0].Name)
	}
	// dd is a child of "secret", which is not a launcher, so it joins that app.
	sec := s.App("secret")
	if sec == nil || !near(sec.CPU, 20) || !near(sec.DiskWrite, 3_000_000) || sec.IOHidden != 1 {
		t.Errorf("secret+dd = %+v", sec)
	}
	if k := s.App("kernel"); k == nil || !near(k.CPU, 5) || k.IOHidden != 0 {
		t.Errorf("kernel = %+v", k)
	}
	if s.App("kthreadd") != nil {
		t.Error("an exited process must not appear")
	}
	wantStart := time.Unix(1700001002, 0).Add(-time.Duration((1002.0 - 100.0) * float64(time.Second)))
	if !chrome.Started.Equal(wantStart) {
		t.Errorf("chrome started %v, want %v", chrome.Started, wantStart)
	}
}

func TestPIDReuseIsNotCharged(t *testing.T) {
	fp := newFakeProc(t)
	clk := &clock{time.Unix(1700001000, 0)}
	fp.system(0, 1000, 500, 0, 0, 0, 0)
	fp.task(fakeTask{pid: 1, comm: "systemd", cmdline: "systemd\x00", start: 1})
	fp.task(fakeTask{pid: 42, ppid: 1, comm: "old", cmdline: "old\x00", ticks: 9000, start: 100, written: 9e9})
	c, err := New(Options{Proc: procfs.New(fp.root), Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	// PID 42 exits and a new process reuses it. Its start time predates the
	// previous sample (it is not "born now"), so the collector cannot know
	// how much of its total is new and charges nothing this round.
	clk.t = clk.t.Add(time.Second)
	fp.system(10, 1090, 501, 0, 0, 0, 0)
	fp.task(fakeTask{pid: 42, ppid: 1, comm: "new", cmdline: "new\x00", ticks: 5, start: 40000, written: 100})
	s, err := c.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if a := s.App("new"); a == nil || a.CPU != 0 || a.DiskWrite != 0 {
		t.Errorf("reused pid charged: %+v", a)
	}
	clk.t = clk.t.Add(time.Second)
	fp.system(20, 1180, 502, 0, 0, 0, 0)
	fp.task(fakeTask{pid: 42, ppid: 1, comm: "new", cmdline: "new\x00", ticks: 55, start: 40000, written: 1100})
	s, _ = c.Sample()
	if a := s.App("new"); a == nil || !near(a.CPU, 50) || !near(a.DiskWrite, 1000) {
		t.Errorf("second interval = %+v", a)
	}
}

// A build script spawns short-lived compilers. One is seen once and then
// exits; another starts and exits between samples. Both end up in the
// parent's cutime and io once reaped. The seen child's earlier work must not
// be counted twice, and the unseen child's work must not be lost.
func TestReapedChildrenAreCountedOnce(t *testing.T) {
	fp := newFakeProc(t)
	clk := &clock{time.Unix(1700001000, 0)}
	fp.system(0, 1000, 500, 0, 0, 0, 0)
	fp.task(fakeTask{pid: 1, comm: "systemd", cmdline: "systemd\x00", start: 1})
	fp.task(fakeTask{pid: 50, ppid: 1, comm: "make", cmdline: "make\x00", ticks: 10, start: 100, written: 1000})
	c, err := New(Options{Proc: procfs.New(fp.root), Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}

	// cc (pid 51) appears with 30 ticks and 5000 bytes written.
	clk.t = clk.t.Add(time.Second)
	fp.system(30, 1070, 501, 0, 0, 0, 0)
	fp.task(fakeTask{pid: 51, ppid: 50, comm: "cc", cmdline: "cc\x00", ticks: 30, start: 50050, written: 5000})
	if _, err := c.Sample(); err != nil {
		t.Fatal(err)
	}

	// cc ran 20 more ticks and wrote 2000 more bytes, then exited; a second
	// compiler ran 40 ticks and wrote 3000 bytes entirely between samples.
	// make itself used 5 ticks. make's cutime and io now include both children.
	clk.t = clk.t.Add(time.Second)
	fp.system(95, 1105, 502, 0, 0, 0, 0)
	fp.remove(51)
	fp.task(fakeTask{pid: 50, ppid: 1, comm: "make", cmdline: "make\x00", ticks: 15, cticks: 50 + 40, start: 100,
		written: 1000 + 7000 + 3000})
	s, err := c.Sample()
	if err != nil {
		t.Fatal(err)
	}
	a := s.App("make")
	if a == nil || !near(a.CPU, 65) || !near(a.DiskWrite, 5000) {
		t.Errorf("make = %+v, want cpu 65 (5 own + 20 seen child + 40 unseen) and 5000 B/s written", a)
	}
}

func TestExecRenamesProcess(t *testing.T) {
	fp := newFakeProc(t)
	clk := &clock{time.Unix(1700001000, 0)}
	fp.system(0, 1000, 500, 0, 0, 0, 0)
	fp.task(fakeTask{pid: 1, comm: "systemd", cmdline: "systemd\x00", start: 1})
	fp.task(fakeTask{pid: 7, ppid: 1, comm: "sh", cmdline: "sh\x00-c\x00x\x00", start: 100})
	c, _ := New(Options{Proc: procfs.New(fp.root), Now: clk.now})
	clk.t = clk.t.Add(time.Second)
	fp.system(10, 1090, 501, 0, 0, 0, 0)
	fp.task(fakeTask{pid: 7, ppid: 1, comm: "stress", cmdline: "stress\x00--cpu\x001\x00", ticks: 100, start: 100})
	s, _ := c.Sample()
	if s.App("stress") == nil || s.App("sh") != nil {
		t.Errorf("exec not followed: %+v", s.Apps)
	}
}

func TestNetSourceAttribution(t *testing.T) {
	fp := newFakeProc(t)
	clk := &clock{time.Unix(1700001000, 0)}
	fp.system(0, 1000, 500, 0, 0, 0, 0)
	fp.task(fakeTask{pid: 1, comm: "systemd", cmdline: "systemd\x00", start: 1})
	fp.task(fakeTask{pid: 10, ppid: 1, comm: "firefox", cmdline: "/usr/lib/firefox/firefox\x00", start: 100})
	fp.task(fakeTask{pid: 11, ppid: 10, comm: "Web Content", cmdline: "/usr/lib/firefox/firefox\x00-contentproc\x00", start: 200})
	net := &fakeNet{counters: map[uint32]NetCounter{11: {Tx: 100, Rx: 1000, Comm: "Web Content"}}}
	c, _ := New(Options{Proc: procfs.New(fp.root), Net: net, Now: clk.now})
	if !c.NetPerApp() {
		t.Fatal("net source not used")
	}
	clk.t = clk.t.Add(2 * time.Second)
	fp.system(10, 1190, 502, 0, 0, 0, 0)
	net.counters[11] = NetCounter{Tx: 300, Rx: 9000, Comm: "Web Content"}
	// a curl that started and exited between samples: never in /proc, still counted
	net.counters[77] = NetCounter{Rx: 4_000_000, Comm: "curl"}
	s, _ := c.Sample()
	ff := s.App("firefox")
	if ff == nil || !near(ff.NetTx, 100) || !near(ff.NetRx, 4000) || !ff.NetActive {
		t.Errorf("firefox = %+v", ff)
	}
	if cu := s.App("curl"); cu == nil || !near(cu.NetRx, 2_000_000) || cu.Procs != 0 {
		t.Errorf("exited curl = %+v", cu)
	}
	if len(net.forgot) != 1 || net.forgot[0] != 77 {
		t.Errorf("forgot %v, want [77]", net.forgot)
	}
	// firefox's renderer exits: its last bytes still go to firefox.
	clk.t = clk.t.Add(time.Second)
	fp.system(20, 1280, 503, 0, 0, 0, 0)
	fp.remove(11)
	net.counters[11] = NetCounter{Tx: 300, Rx: 10000, Comm: "Web Content"}
	s, _ = c.Sample()
	if ff := s.App("firefox"); ff == nil || !near(ff.NetRx, 1000) {
		t.Errorf("exited renderer's bytes: %+v", ff)
	}
}

func TestIsPartition(t *testing.T) {
	for name, want := range map[string]bool{
		"sda": false, "sda1": true, "vda": false, "vda3": true, "xvda1": true,
		"nvme0n1": false, "nvme0n1p2": true, "mmcblk0": false, "mmcblk0p1": true,
	} {
		if IsPartition(name) != want {
			t.Errorf("IsPartition(%q) = %v", name, !want)
		}
	}
}

func TestIsPhysicalIface(t *testing.T) {
	for name, want := range map[string]bool{
		"eth0": true, "wlp2s0": true, "enp3s0": true, "lo": false, "docker0": false,
		"veth12ab": false, "br-1f": false, "wg0": false, "tailscale0": false,
	} {
		if IsPhysicalIface(name) != want {
			t.Errorf("IsPhysicalIface(%q) = %v", name, !want)
		}
	}
}

func TestSampleOnHost(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	c, err := New(Options{SocketEvery: 1})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	s, err := c.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Apps) == 0 || s.Sys.MemTotal == 0 || s.Sys.NumCPU == 0 {
		t.Errorf("host sample looks empty: %d apps, mem %d, ncpu %d", len(s.Apps), s.Sys.MemTotal, s.Sys.NumCPU)
	}
}
