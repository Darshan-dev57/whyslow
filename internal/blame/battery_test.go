package blame

import (
	"testing"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/model"
)

func batt(status string, pct, wh float64) *model.Battery {
	return &model.Battery{Status: status, Percent: pct, EnergyWh: wh, HasEnergy: true, Watts: 10, HasPower: true}
}

func TestBatteryWindowAttribution(t *testing.T) {
	tr := NewBatteryTracker(DefaultBatteryConfig())
	var got []*BatteryWindow
	// 6 minutes on battery: draining 2% and 1 Wh per minute.
	for i := 0; i <= 360; i++ {
		s := &model.Snapshot{TS: onset.Add(time.Duration(i) * time.Second),
			Sys: model.System{Battery: batt("Discharging", 80-float64(i)/30, 40-float64(i)/60)},
			Apps: []model.App{
				{Name: "chrome", CPU: 60},
				{Name: "spotify", CPU: 15},
				{Name: "rsync", CPU: 5, DiskRead: 50e6}, // 0.05 + 0.5 per second
				{Name: "idle", CPU: 0.1},
			}}
		if w := tr.Observe(s); w != nil {
			got = append(got, w)
		}
	}
	// back on AC after 6 min: the second, 1-minute window closes too
	if w := tr.Observe(&model.Snapshot{TS: onset.Add(361 * time.Second), Sys: model.System{Battery: batt("Charging", 68, 34)}}); w != nil {
		got = append(got, w)
	}
	if len(got) != 2 {
		t.Fatalf("windows = %d", len(got))
	}
	w := got[0]
	if w.End.Sub(w.Start) != 5*time.Minute || !approx(w.DrainPct, 10, 1e-9) || !approx(w.DrainWh, 5, 1e-9) || w.AvgWatts != 10 {
		t.Errorf("window = %+v", w)
	}
	// scores per second: chrome 0.6, rsync 0.55, spotify 0.15, idle 0.001 (< 1%, dropped)
	if len(w.Apps) != 3 || w.Apps[0].App != "chrome" || w.Apps[1].App != "rsync" || w.Apps[2].App != "spotify" {
		t.Fatalf("apps = %+v", w.Apps)
	}
	if !approx(w.Apps[0].Share, 0.6/1.301, 1e-3) || !approx(w.Apps[0].Wh, 5*0.6/1.301, 1e-2) {
		t.Errorf("chrome = %+v", w.Apps[0])
	}
	if got[1].End.Sub(got[1].Start) != time.Minute {
		t.Errorf("second window = %v", got[1].End.Sub(got[1].Start))
	}
}

func TestBatteryShortWindowDropped(t *testing.T) {
	tr := NewBatteryTracker(DefaultBatteryConfig())
	for i := 0; i < 30; i++ {
		tr.Observe(&model.Snapshot{TS: onset.Add(time.Duration(i) * time.Second), Sys: model.System{Battery: batt("Discharging", 50, 20)}})
	}
	if w := tr.Observe(&model.Snapshot{TS: onset.Add(31 * time.Second), Sys: model.System{Battery: batt("Full", 50, 20)}}); w != nil {
		t.Errorf("30 s on battery should not make a window: %+v", w)
	}
	if w := tr.Observe(&model.Snapshot{TS: onset}); w != nil {
		t.Error("no battery: no window")
	}
}
