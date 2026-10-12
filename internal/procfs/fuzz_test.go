package procfs

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fuzz targets check that no parser panics on arbitrary input and that
// a successful parse is consistent with its input. Seeds are the fixtures.

func seed(f *testing.F, rel ...string) {
	f.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"testdata", "proc"}, rel...)...))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(b)
}

func FuzzParseProcStat(f *testing.F) {
	for _, pid := range []string{"1", "2", "4321", "777", "999"} {
		if b, err := os.ReadFile(filepath.Join("testdata", "proc", pid, "stat")); err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte("12 (a) b () c) S 1 1 1 0 -1 4194560 0 0 0 0 5 6 0 0 20 0 1 0 100 0 0"))
	f.Add([]byte("12 (new\nline) S 1 1 1 0 -1 0 0 0 0 0 5 6 -1 0 20 0 1 0 100 0 0"))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := ParseProcStat(b)
		if err != nil {
			return
		}
		if !bytes.Contains(b, []byte(p.Comm)) {
			t.Fatalf("comm %q not in input", p.Comm)
		}
	})
}

func FuzzParseStatm(f *testing.F) {
	f.Add([]byte("1000 250 100 10 0 300 0"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, b []byte) { ParseStatm(b) })
}

func FuzzParseIO(f *testing.F) {
	f.Add([]byte("rchar: 1\nwchar: 2\nsyscr: 3\nsyscw: 4\nread_bytes: 4096\nwrite_bytes: 8192\ncancelled_write_bytes: 0\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if _, err := ParseIO(bytes.NewReader(b)); err == nil && !bytes.Contains(b, []byte("write_bytes")) {
			t.Fatal("parsed io without write_bytes")
		}
	})
}

func FuzzParseCmdline(f *testing.F) {
	f.Add([]byte("/usr/bin/python3\x00-m\x00http.server\x00"))
	f.Add([]byte("\x00\x00"))
	f.Add([]byte("nginx: worker process"))
	f.Fuzz(func(t *testing.T, b []byte) {
		args := ParseCmdline(b)
		if got, want := strings.Join(args, "\x00"), string(bytes.TrimRight(b, "\x00")); got != want {
			t.Fatalf("round trip: %q != %q", got, want)
		}
	})
}

func FuzzParseStat(f *testing.F) {
	seed(f, "stat")
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := ParseStat(bytes.NewReader(b))
		if err == nil && s.CPU.Total() < s.CPU.User && s.CPU.User < math.MaxUint64/8 {
			t.Fatalf("total %d < user %d", s.CPU.Total(), s.CPU.User)
		}
	})
}

func FuzzParseMemInfo(f *testing.F) {
	seed(f, "meminfo")
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := ParseMemInfo(bytes.NewReader(b))
		if err == nil && m.Used() > m.Total {
			t.Fatalf("used %d > total %d", m.Used(), m.Total)
		}
	})
}

func FuzzParseDiskStats(f *testing.F) {
	seed(f, "diskstats")
	f.Fuzz(func(t *testing.T, b []byte) { ParseDiskStats(bytes.NewReader(b)) })
}

func FuzzParseNetDev(f *testing.F) {
	seed(f, "net", "dev")
	f.Fuzz(func(t *testing.T, b []byte) { ParseNetDev(bytes.NewReader(b)) })
}

func FuzzParseUptime(f *testing.F) {
	seed(f, "uptime")
	f.Fuzz(func(t *testing.T, b []byte) { ParseUptime(b) })
}
