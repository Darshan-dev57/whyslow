package blame

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/model"
)

var (
	onset = time.Date(2026, 10, 12, 15, 12, 4, 0, time.UTC)
	cfg   = func() Config { c := DefaultConfig(); c.Location = time.UTC; return c }()
)

// series builds one snapshot per second from onset-70 s to onset+dur+5 s.
// app(ts) returns the apps present at that second.
func series(dur time.Duration, app func(ts time.Time) []model.App) []*model.Snapshot {
	var out []*model.Snapshot
	for ts := onset.Add(-70 * time.Second); ts.Before(onset.Add(dur + 5*time.Second)); ts = ts.Add(time.Second) {
		out = append(out, &model.Snapshot{TS: ts, Apps: app(ts)})
	}
	return out
}

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// The worked example from the README: disk write baseline 3.0 MB/s, 12.6 MB/s
// for 38 s from 15:12:04. chrome's first process starts at 15:11:58 and
// writes its profile (30 MB in its first second); dockerd and journald were
// already running.
func TestWorkedExample(t *testing.T) {
	const MB = 1e6
	chromeStart := onset.Add(-6 * time.Second)
	old := onset.Add(-time.Hour)
	snaps := series(38*time.Second, func(ts time.Time) []model.App {
		in := !ts.Before(onset) && ts.Before(onset.Add(38*time.Second))
		apps := []model.App{
			{Name: "dockerd", DiskWrite: pick(in, 2.6*MB, 1.0*MB), Started: old},
			{Name: "systemd-journald", DiskWrite: pick(in, 0.5*MB, 0.3*MB), Started: old},
			{Name: "kernel", DiskWrite: 0},
		}
		switch {
		case ts.Equal(chromeStart):
			apps = append(apps, model.App{Name: "chrome", DiskWrite: 30 * MB, Started: chromeStart})
		case ts.After(chromeStart) && !in:
			apps = append(apps, model.App{Name: "chrome", DiskWrite: 0, Started: chromeStart})
		case in:
			apps = append(apps, model.App{Name: "chrome", DiskWrite: 8.3 * MB, Started: chromeStart})
		}
		return apps
	})
	sp := Spike{Metric: model.DiskWrite, Onset: onset, End: onset.Add(38 * time.Second),
		Baseline: 3.0 * MB, Threshold: 9.0 * MB, Peak: 14 * MB, Mean: 12.6 * MB}
	m := Explain(sp, snaps, false, cfg)

	if len(m.Culprits) != 2 {
		t.Fatalf("culprits = %+v (journald's 2%% must be dropped)", m.Culprits)
	}
	c0, c1 := m.Culprits[0], m.Culprits[1]
	if c0.App != "chrome" || !approx(c0.Share, 0.8125, 0.001) || !c0.Newcomer ||
		!approx(c0.Before, 0.5*MB, 1) || !approx(c0.During, 8.3*MB, 1) {
		t.Errorf("first culprit = %+v", c0)
	}
	if c1.App != "dockerd" || !approx(c1.Share, 0.1667, 0.001) || c1.Newcomer {
		t.Errorf("second culprit = %+v", c1)
	}
	if !approx(m.Ratio, 4.2, 1e-9) {
		t.Errorf("ratio = %v", m.Ratio)
	}
	if len(m.Newcomers) != 1 || m.Newcomers[0].App != "chrome" || m.Newcomers[0].StartedAgo != 6 {
		t.Errorf("newcomers = %+v", m.Newcomers)
	}
	want := "Disk write spiked 4.2x at 15:12:04 for 38 s — 81% from chrome (started 6 s earlier), 17% from dockerd."
	if m.Summary != want {
		t.Errorf("summary\n got: %s\nwant: %s", m.Summary, want)
	}
}

func pick(cond bool, a, b float64) float64 {
	if cond {
		return a
	}
	return b
}

func TestSpreadLoad(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "f"}
	snaps := series(20*time.Second, func(ts time.Time) []model.App {
		in := !ts.Before(onset)
		var apps []model.App
		for i, n := range names {
			apps = append(apps, model.App{Name: n, CPU: pick(in, 30+float64(i), 5), Started: onset.Add(-time.Hour)})
		}
		return apps
	})
	m := Explain(Spike{Metric: model.CPU, Onset: onset, End: onset.Add(20 * time.Second), Baseline: 8, Mean: 60}, snaps, false, cfg)
	if len(m.Culprits) != 5 {
		t.Errorf("want MaxCulprits=5, got %d", len(m.Culprits))
	}
	if !strings.Contains(m.Summary, "spread across several apps; biggest: 18% from f, 18% from e") {
		t.Errorf("summary = %s", m.Summary)
	}
	if !strings.HasPrefix(m.Summary, "CPU jumped to 60% at 15:12:04 for 20 s (normally 8%)") {
		t.Errorf("cpu wording = %s", m.Summary)
	}
}

func TestAbsentBeforeAndJumpWording(t *testing.T) {
	// dd did not exist before the spike: its "before" is zero. The baseline
	// was ~0, so the sentence gives the level instead of a ratio.
	snaps := series(12*time.Second, func(ts time.Time) []model.App {
		apps := []model.App{{Name: "systemd", Started: onset.Add(-time.Hour)}}
		if !ts.Before(onset.Add(-2 * time.Second)) {
			apps = append(apps, model.App{Name: "dd", DiskWrite: pick(ts.Before(onset), 0, 400e6), Started: onset.Add(-2 * time.Second)})
		}
		return apps
	})
	m := Explain(Spike{Metric: model.DiskWrite, Onset: onset, End: onset.Add(12 * time.Second), Baseline: 20e3, Mean: 400e6}, snaps, false, cfg)
	if m.Ratio != 0 {
		t.Errorf("ratio against a ~0 baseline should be 0, got %v", m.Ratio)
	}
	want := "Disk write jumped to 400 MB/s at 15:12:04 for 12 s (normally 20.0 KB/s) — 100% from dd (started 2 s earlier)."
	if m.Summary != want {
		t.Errorf("summary\n got: %s\nwant: %s", m.Summary, want)
	}
}

func TestNetworkWithoutEBPF(t *testing.T) {
	snaps := series(10*time.Second, func(ts time.Time) []model.App {
		return []model.App{{Name: "curl", Started: onset.Add(-1 * time.Second), Sockets: 1}}
	})
	m := Explain(Spike{Metric: model.NetRx, Onset: onset, End: onset.Add(10 * time.Second), Baseline: 2e3, Mean: 9e6}, snaps, false, cfg)
	if len(m.Culprits) != 0 {
		t.Errorf("no per-app net data, but culprits = %+v", m.Culprits)
	}
	want := "Network download jumped to 9.0 MB/s at 15:12:04 for 10 s (normally 2.0 KB/s) — per-app network needs eBPF (run with --ebpf). Just started: curl (started 1 s earlier)."
	if m.Summary != want {
		t.Errorf("summary\n got: %s\nwant: %s", m.Summary, want)
	}
}

func TestNetworkWithEBPF(t *testing.T) {
	snaps := series(10*time.Second, func(ts time.Time) []model.App {
		in := !ts.Before(onset)
		return []model.App{
			{Name: "curl", NetRx: pick(in, 9e6, 0), Started: onset.Add(-time.Second)},
			{Name: "firefox", NetRx: 100e3, Started: onset.Add(-time.Hour)},
		}
	})
	m := Explain(Spike{Metric: model.NetRx, Onset: onset, End: onset.Add(10 * time.Second), Baseline: 100e3, Mean: 9.1e6}, snaps, true, cfg)
	if len(m.Culprits) != 1 || m.Culprits[0].App != "curl" || m.Culprits[0].Share != 1 {
		t.Errorf("culprits = %+v", m.Culprits)
	}
	if !strings.HasPrefix(m.Summary, "Network download spiked 91.0x") {
		t.Errorf("summary = %s", m.Summary)
	}
}

func TestMemoryAndNoCulprit(t *testing.T) {
	snaps := series(30*time.Second, func(ts time.Time) []model.App {
		return []model.App{{Name: "postgres", RSS: 500e6, Started: onset.Add(-time.Hour)}}
	})
	m := Explain(Spike{Metric: model.Mem, Onset: onset, End: onset.Add(30 * time.Second), Baseline: 3e9, Mean: 5.1e9, Truncated: true}, snaps, false, cfg)
	want := "Memory use rose by 2.1 GB at 15:12:04 for over 30 s (to 5.1 GB) — no app's own usage rose; likely the kernel or another machine-wide cause."
	if m.Summary != want {
		t.Errorf("summary\n got: %s\nwant: %s", m.Summary, want)
	}
}

func TestOldAppWithNewProcessIsNotNewcomer(t *testing.T) {
	snaps := series(10*time.Second, func(ts time.Time) []model.App {
		return []model.App{{Name: "chrome", CPU: pick(!ts.Before(onset), 90, 5), Started: onset.Add(-2 * time.Hour)}}
	})
	m := Explain(Spike{Metric: model.CPU, Onset: onset, End: onset.Add(10 * time.Second), Baseline: 5, Mean: 30}, snaps, false, cfg)
	if len(m.Newcomers) != 0 || m.Culprits[0].Newcomer {
		t.Errorf("chrome is not new: %+v", m.Newcomers)
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		38 * time.Second:                "38 s",
		2*time.Minute + 5*time.Second:   "2 min 5 s",
		10 * time.Minute:                "10 min",
		time.Hour + 3*time.Minute + 9e9: "1 h 3 min",
	} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
