package report

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/model"
	"github.com/Darshan-dev57/whyslow/internal/store"
)

var t0 = time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)

func mystery(id int64, metric model.Metric, at time.Time, top string, share float64, newcomers ...string) *model.Mystery {
	m := &model.Mystery{ID: id, Metric: metric, Start: at, End: at.Add(20 * time.Second), Summary: "spike"}
	if top != "" {
		m.Culprits = []model.Culprit{{App: top, Share: share}}
	}
	for _, n := range newcomers {
		m.Newcomers = append(m.Newcomers, model.Newcomer{App: n, StartedAgo: 4})
	}
	return m
}

func TestOffenders(t *testing.T) {
	ms := []*model.Mystery{
		mystery(1, model.DiskWrite, t0, "chrome", 0.81, "code"),
		mystery(2, model.DiskWrite, t0.Add(time.Hour), "chrome", 0.6),
		mystery(3, model.CPU, t0.Add(2*time.Hour), "chrome", 0.9, "code", "code"),
		mystery(4, model.DiskWrite, t0.Add(3*time.Hour), "chrome", 0.3), // too small a share
		mystery(5, model.CPU, t0.Add(4*time.Hour), "", 0, "slack"),      // nobody blamed, one opener
	}
	got := Offenders(ms, DefaultConfig())
	if len(got) != 2 {
		t.Fatalf("offenders = %+v", got)
	}
	// Equal counts sort by app name: chrome before code.
	c, o := got[0], got[1]
	if c.App != "chrome" || c.Kind != KindCulprit || c.Metric != model.DiskWrite || c.Count != 2 ||
		!c.Last.Equal(t0.Add(time.Hour)) || len(c.IDs) != 2 || c.IDs[0] != 1 {
		t.Errorf("culprit = %+v", c)
	}
	if c.Text != "chrome was the main cause of 2 disk write spikes." {
		t.Errorf("culprit text = %q", c.Text)
	}
	// "code" appears twice in mystery 3's newcomers but counts once per mystery.
	if o.App != "code" || o.Kind != KindOpener || o.Count != 2 || !strings.Contains(o.Text, "every time it opens") {
		t.Errorf("opener = %+v", o)
	}
}

func TestOffendersNeedRepeats(t *testing.T) {
	ms := []*model.Mystery{mystery(1, model.CPU, t0, "stress", 1)}
	if got := Offenders(ms, DefaultConfig()); len(got) != 0 {
		t.Errorf("one spike is not a pattern: %+v", got)
	}
}

func TestSumBattery(t *testing.T) {
	if SumBattery(nil, 5) != nil {
		t.Error("no windows must give nil")
	}
	ws := []*model.BatteryWindow{
		{Start: t0, End: t0.Add(10 * time.Minute), DrainWh: 4, DrainPct: 8,
			Apps: []model.AppDrain{{App: "chrome", Wh: 3, Pct: 6}, {App: "slack", Wh: 1, Pct: 2}}},
		{Start: t0.Add(time.Hour), End: t0.Add(time.Hour + 5*time.Minute), DrainWh: 2, DrainPct: 4,
			Apps: []model.AppDrain{{App: "slack", Wh: 1.5, Pct: 3}, {App: "chrome", Wh: 0.5, Pct: 1}}},
	}
	b := SumBattery(ws, 1)
	if b.Windows != 2 || b.OnBattery != 15*time.Minute || b.DrainWh != 6 || b.DrainPct != 12 {
		t.Errorf("totals = %+v", b)
	}
	if len(b.Hogs) != 1 || b.Hogs[0].App != "chrome" || b.Hogs[0].Pct != 7 || b.Hogs[0].Wh != 3.5 {
		t.Errorf("hogs = %+v", b.Hogs)
	}
}

func TestDayBoundsAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	from, to := DayBounds(time.Date(2026, 10, 25, 12, 0, 0, 0, loc)) // clocks go back this night
	if to.Sub(from) != 25*time.Hour || from.Hour() != 0 || to.Day() != 26 {
		t.Errorf("bounds = %v .. %v", from, to)
	}
}

func TestBuildFromStore(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var snaps []*model.Snapshot
	for i := 0; i < 120; i++ {
		s := &model.Snapshot{TS: t0.Add(time.Duration(i) * time.Second), Elapsed: time.Second,
			Sys: model.System{CPU: 30, MemUsed: 2e9, MemTotal: 8e9}}
		s.Apps = []model.App{{Name: "builder", CPU: 100, RSS: 500e6, DiskWrite: 1e6, Procs: 3},
			{Name: "editor", CPU: 10, RSS: 200e6, Procs: 1}}
		snaps = append(snaps, s)
	}
	if err := st.WriteSnapshots(ctx, snaps); err != nil {
		t.Fatal(err)
	}
	for i, at := range []time.Duration{10 * time.Second, 70 * time.Second} {
		m := mystery(0, model.CPU, t0.Add(at), "builder", 0.9)
		m.Summary = []string{"first", "second"}[i]
		if err := st.InsertMystery(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	// a mystery from yesterday counts toward repeat offenders but not today's list
	if err := st.InsertMystery(ctx, mystery(0, model.CPU, t0.Add(-24*time.Hour), "builder", 0.7)); err != nil {
		t.Fatal(err)
	}

	r, err := Build(ctx, st, t0, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if r.Date != "2026-10-12" || r.Watched != 120*time.Second {
		t.Errorf("date/watched = %s / %v", r.Date, r.Watched)
	}
	cpu := r.Sections[0]
	if cpu.Resource != store.ResCPU || len(cpu.Apps) != 2 || cpu.Apps[0].App != "builder" || cpu.Apps[0].Value != 120 {
		t.Errorf("cpu section = %+v", cpu)
	}
	if len(r.Mysteries) != 2 || r.Mysteries[0].Summary != "second" || r.ByMetric[model.CPU] != 2 {
		t.Errorf("mysteries = %d, by metric %v", len(r.Mysteries), r.ByMetric)
	}
	if len(r.Repeat) != 1 || r.Repeat[0].Count != 3 {
		t.Errorf("repeat = %+v", r.Repeat)
	}
	if r.Battery != nil {
		t.Error("no battery windows were stored")
	}

	var buf bytes.Buffer
	WriteText(&buf, r)
	out := buf.String()
	for _, want := range []string{"report for 2026-10-12 (watched 2 min)", "1. builder", "2 min", "No time on battery",
		"Mysteries: 2 (CPU 2)", "#2 second", "builder was the main cause of 3 CPU spikes."} {
		if !strings.Contains(out, want) {
			t.Errorf("text report lacks %q:\n%s", want, out)
		}
	}
}

func TestEmptyDayText(t *testing.T) {
	var buf bytes.Buffer
	WriteText(&buf, &Report{Date: "2026-10-11"})
	if !strings.Contains(buf.String(), "No samples for this day") {
		t.Errorf("got %q", buf.String())
	}
}
