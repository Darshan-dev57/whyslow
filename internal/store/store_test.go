package store

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/model"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sub", "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// threeMinutes writes 180 one-second snapshots starting at t0: "builder"
// uses 50% CPU and writes 2 MB/s for the first minute only; "editor" uses
// 10% CPU all the time and 300 MB of memory; "idle" is big and never busy.
func threeMinutes(t *testing.T, s *Store) {
	t.Helper()
	var batch []*model.Snapshot
	for i := 0; i < 180; i++ {
		snap := &model.Snapshot{TS: t0.Add(time.Duration(i) * time.Second), Elapsed: time.Second,
			Sys: model.System{CPU: 20, MemUsed: 4e9, MemTotal: 8e9, DiskWrite: 2e6, NetRx: 1000}}
		if i < 60 {
			snap.Apps = append(snap.Apps, model.App{Name: "builder", CPU: 50, RSS: 100e6, DiskWrite: 2e6, Procs: 4})
		}
		snap.Apps = append(snap.Apps,
			model.App{Name: "editor", CPU: 10, RSS: 300e6, NetRx: 1000, Procs: 2},
			model.App{Name: "idle", CPU: 0, RSS: 900e6, Procs: 1})
		batch = append(batch, snap)
		if len(batch) == 50 {
			if err := s.WriteSnapshots(ctx, batch); err != nil {
				t.Fatal(err)
			}
			batch = nil
		}
	}
	if err := s.WriteSnapshots(ctx, batch); err != nil {
		t.Fatal(err)
	}
}

func TestTimelineAndAverages(t *testing.T) {
	s := open(t)
	threeMinutes(t, s)

	pts, err := s.Timeline(ctx, model.CPU, t0, t0.Add(179*time.Second), "")
	if err != nil || len(pts) != 180 || pts[0].V != 20 {
		t.Fatalf("system timeline: %d points, %v", len(pts), err)
	}
	pts, err = s.Timeline(ctx, model.DiskWrite, t0, t0.Add(179*time.Second), "builder")
	if err != nil || len(pts) != 180 {
		t.Fatalf("app timeline: %d points, %v", len(pts), err)
	}
	if pts[59].V != 2e6 || pts[60].V != 0 {
		t.Errorf("builder must read 0 after it stopped: %v, %v", pts[59].V, pts[60].V)
	}
	if _, err := s.Timeline(ctx, "bogus", t0, t0, ""); err == nil {
		t.Error("unknown metric accepted")
	}

	avg, err := s.AppAverages(ctx, t0, t0.Add(179*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(avg) != 3 || avg[0].Name != "builder" || math.Abs(avg[0].CPU-50.0/3) > 1e-9 || avg[1].Name != "editor" || avg[1].CPU != 10 {
		t.Errorf("averages = %+v", avg)
	}
}

func TestRollupMatchesRaw(t *testing.T) {
	s := open(t)
	threeMinutes(t, s)
	end := t0.Add(3 * time.Minute)

	before, err := s.TopApps(ctx, ResCPU, t0, end, 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Rollup(ctx, end); err != nil {
		t.Fatal(err)
	}
	after, err := s.TopApps(ctx, ResCPU, t0, end, 5)
	if err != nil {
		t.Fatal(err)
	}
	// builder: 50% for 60 s = 30 CPU-s; editor 10% for 180 s = 18 CPU-s; idle has none
	want := []AppTotal{{"builder", 30}, {"editor", 18}}
	for _, got := range [][]AppTotal{before, after} {
		if len(got) != 2 || got[0].App != want[0].App || math.Abs(got[0].Value-30) > 1e-6 ||
			got[1].App != "editor" || math.Abs(got[1].Value-18) > 1e-6 {
			t.Errorf("TopApps cpu = %+v, want %+v", got, want)
		}
	}
	mem, _ := s.TopApps(ctx, ResMem, t0, end, 1)
	if len(mem) != 1 || mem[0].App != "idle" || mem[0].Value != 900e6 {
		t.Errorf("idle but large apps must be kept for the memory report: %+v", mem)
	}
	disk, _ := s.TopApps(ctx, ResDiskWrite, t0, end, 5)
	if len(disk) != 1 || math.Abs(disk[0].Value-120e6) > 1 {
		t.Errorf("disk total = %+v", disk)
	}

	// a range longer than the raw limit reads the minute table
	pts, err := s.Timeline(ctx, model.CPU, t0.Add(-3*time.Hour), end, "builder")
	if err != nil || len(pts) != 3 || math.Abs(pts[0].V-50) > 1e-9 || pts[1].V != 0 {
		t.Errorf("minute timeline = %+v, %v", pts, err)
	}
	// rollup is idempotent
	if err := s.Rollup(ctx, end); err != nil {
		t.Fatal(err)
	}
	again, _ := s.TopApps(ctx, ResCPU, t0, end, 5)
	if math.Abs(again[0].Value-30) > 1e-6 {
		t.Errorf("second rollup changed totals: %+v", again)
	}
}

func TestRetention(t *testing.T) {
	s := open(t)
	threeMinutes(t, s)
	r := Retention{SysRaw: time.Minute, AppRaw: time.Minute, Minutes: time.Hour}
	now := t0.Add(3 * time.Minute)
	// nothing is rolled up yet, so nothing raw may be deleted
	if err := s.Retain(ctx, now, r); err != nil {
		t.Fatal(err)
	}
	if pts, _ := s.Timeline(ctx, model.CPU, t0, now, ""); len(pts) != 180 {
		t.Fatalf("raw rows deleted before rollup: %d left", len(pts))
	}
	if err := s.Rollup(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Retain(ctx, now, r); err != nil {
		t.Fatal(err)
	}
	first, last, err := s.Span(ctx)
	if err != nil || !first.Equal(t0.Add(2*time.Minute)) || !last.Equal(t0.Add(179*time.Second)) {
		t.Errorf("span after retention = %v..%v, %v", first, last, err)
	}
	// totals survive in the minute table
	if top, _ := s.TopApps(ctx, ResCPU, t0, now, 1); len(top) != 1 || math.Abs(top[0].Value-30) > 1e-6 {
		t.Errorf("totals after retention = %+v", top)
	}
	if err := s.Retain(ctx, now.Add(2*time.Hour), r); err != nil {
		t.Fatal(err)
	}
	if top, _ := s.TopApps(ctx, ResCPU, t0, now, 1); len(top) != 0 {
		t.Errorf("minute rows past retention remain: %+v", top)
	}
}

func TestMysteries(t *testing.T) {
	s := open(t)
	for i, app := range []string{"chrome", "dd", ""} {
		m := &model.Mystery{Metric: model.DiskWrite, Start: t0.Add(time.Duration(i) * time.Minute).Add(250 * time.Millisecond),
			End: t0.Add(time.Duration(i)*time.Minute + 38*time.Second), Baseline: 3e6, Threshold: 9e6, Peak: 14e6,
			Mean: 12.6e6, Ratio: 4.2, Summary: "s", Newcomers: []model.Newcomer{{App: "chrome", StartedAgo: 6}}}
		if app != "" {
			m.Culprits = []model.Culprit{{App: app, Share: 0.81, Before: 0.5e6, During: 8.3e6, Newcomer: true}}
		}
		if err := s.InsertMystery(ctx, m); err != nil {
			t.Fatal(err)
		}
		if m.ID != int64(i+1) {
			t.Errorf("id = %d", m.ID)
		}
	}
	list, err := s.Mysteries(ctx, t0, t0.Add(time.Hour), 10)
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %d, %v", len(list), err)
	}
	if list[0].ID != 3 || list[2].ID != 1 {
		t.Errorf("not newest first: %d..%d", list[0].ID, list[2].ID)
	}
	m, err := s.Mystery(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Start.Equal(t0.Add(250*time.Millisecond)) || m.Culprits[0].App != "chrome" || !m.Culprits[0].Newcomer ||
		m.Newcomers[0].StartedAgo != 6 || m.Ratio != 4.2 {
		t.Errorf("round trip = %+v", m)
	}
	if m3, _ := s.Mystery(ctx, 3); m3.Culprits == nil || len(m3.Culprits) != 0 {
		t.Errorf("no culprits must read back as an empty list: %#v", m3.Culprits)
	}
	if _, err := s.Mystery(ctx, 99); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing id: %v", err)
	}
	if l, _ := s.Mysteries(ctx, t0, t0.Add(time.Hour), 1); len(l) != 1 {
		t.Error("limit ignored")
	}
}

func TestBatteryWindows(t *testing.T) {
	s := open(t)
	w := &model.BatteryWindow{Start: t0, End: t0.Add(5 * time.Minute), DrainWh: 5, DrainPct: 10, AvgWatts: 9.5,
		Apps: []model.AppDrain{{App: "chrome", Share: 0.6, Wh: 3, Pct: 6}}}
	if err := s.InsertBatteryWindow(ctx, w); err != nil {
		t.Fatal(err)
	}
	got, err := s.BatteryWindows(ctx, t0, t0.Add(time.Hour))
	if err != nil || len(got) != 1 || got[0].Apps[0].App != "chrome" || got[0].DrainWh != 5 || !got[0].End.Equal(w.End) {
		t.Errorf("battery windows = %+v, %v", got, err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	threeMinutes(t, s)
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// the app id cache is empty after reopening; names must map to the same ids
	if err := s.WriteSnapshots(ctx, []*model.Snapshot{{TS: t0.Add(200 * time.Second), Elapsed: time.Second,
		Apps: []model.App{{Name: "editor", CPU: 10}}}}); err != nil {
		t.Fatal(err)
	}
	avg, _ := s.AppAverages(ctx, t0, t0.Add(time.Hour))
	if len(avg) != 3 {
		t.Errorf("apps after reopen = %+v", avg)
	}
	if _, err := s.w.Exec(`UPDATE meta SET value = '99' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Error("a newer schema must be refused")
	}
}
