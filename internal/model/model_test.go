package model

import "testing"

func TestBytes(t *testing.T) {
	cases := map[float64]string{
		0:          "0 B",
		999:        "999 B",
		1000:       "1.0 KB",
		12_600_000: "12.6 MB",
		450e6:      "450 MB",
		2.1e9:      "2.1 GB",
	}
	for in, want := range cases {
		if got := Bytes(in); got != want {
			t.Errorf("Bytes(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestMetricFormat(t *testing.T) {
	if got := CPU.Format(37.4); got != "37%" {
		t.Errorf("cpu: %q", got)
	}
	if got := DiskWrite.Format(12.6e6); got != "12.6 MB/s" {
		t.Errorf("disk: %q", got)
	}
	if got := Mem.Format(2.1e9); got != "2.1 GB" {
		t.Errorf("mem: %q", got)
	}
	if !NetRx.IsNet() || CPU.IsNet() || !DiskRead.Valid() || Metric("x").Valid() {
		t.Error("metric predicates")
	}
}

func TestValues(t *testing.T) {
	s := Snapshot{Sys: System{CPU: 10, MemUsed: 5, DiskRead: 1, DiskWrite: 2, NetRx: 3, NetTx: 4},
		Apps: []App{{Name: "a", CPU: 7, RSS: 9, DiskRead: 1, DiskWrite: 2, NetRx: 3, NetTx: 4}}}
	want := []float64{10, 5, 1, 2, 3, 4}
	appWant := []float64{7, 9, 1, 2, 3, 4}
	for i, m := range Metrics {
		if s.Sys.Value(m) != want[i] || s.App("a").Value(m) != appWant[i] {
			t.Errorf("%s values", m)
		}
	}
	if s.App("missing") != nil {
		t.Error("missing app should be nil")
	}
}
