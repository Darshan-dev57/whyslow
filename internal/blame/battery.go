package blame

import (
	"math"
	"sort"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/model"
)

// Most laptops have no per-app energy counter, so battery drain can only be
// estimated: each window's drain is split across apps by an activity score
// in which CPU time dominates and disk and network bytes add a little. The
// screen, the Wi-Fi radio's idle draw and the GPU belong to no app and are
// spread by the same score, which is why every number is labelled an estimate.
const (
	bytesPerCPUSecondDisk = 100e6 // 100 MB of disk I/O scores like 1 CPU-second
	bytesPerCPUSecondNet  = 50e6  // 50 MB of network scores like 1 CPU-second
)

// BatteryConfig tunes the battery window.
type BatteryConfig struct {
	Window    time.Duration // close a window after this long (5 min)
	MinWindow time.Duration // windows shorter than this are dropped (1 min)
	MinShare  float64       // apps below this share are left out (0.01)
}

// DefaultBatteryConfig returns the defaults.
func DefaultBatteryConfig() BatteryConfig {
	return BatteryConfig{Window: 5 * time.Minute, MinWindow: time.Minute, MinShare: 0.01}
}

// BatteryTracker accumulates activity while discharging. Feed it every
// snapshot; it returns a window when one closes.
type BatteryTracker struct {
	cfg      BatteryConfig
	open     bool
	start    time.Time
	last     time.Time
	startPct float64
	startWh  float64
	endPct   float64
	endWh    float64
	hasWh    bool
	wattSum  float64
	wattN    int
	score    map[string]float64
}

// NewBatteryTracker returns a tracker.
func NewBatteryTracker(cfg BatteryConfig) *BatteryTracker {
	return &BatteryTracker{cfg: cfg}
}

// Observe feeds one snapshot.
func (t *BatteryTracker) Observe(s *model.Snapshot) *model.BatteryWindow {
	b := s.Sys.Battery
	if !b.Discharging() {
		return t.close()
	}
	if !t.open {
		t.begin(s.TS, b)
		return nil
	}
	secs := s.TS.Sub(t.last).Seconds()
	t.last = s.TS
	t.endPct, t.endWh = b.Percent, b.EnergyWh
	t.hasWh = t.hasWh && b.HasEnergy
	if b.HasPower {
		t.wattSum += b.Watts
		t.wattN++
	}
	for i := range s.Apps {
		a := &s.Apps[i]
		sc := a.CPU/100*secs + (a.DiskRead+a.DiskWrite)*secs/bytesPerCPUSecondDisk +
			(a.NetRx+a.NetTx)*secs/bytesPerCPUSecondNet
		if sc > 0 {
			t.score[a.Name] += sc
		}
	}
	if t.last.Sub(t.start) >= t.cfg.Window {
		w := t.close()
		t.begin(s.TS, b) // the next window starts at this sample
		return w
	}
	return nil
}

func (t *BatteryTracker) begin(ts time.Time, b *model.Battery) {
	t.open = true
	t.start, t.last = ts, ts
	t.startPct, t.endPct = b.Percent, b.Percent
	t.startWh, t.endWh, t.hasWh = b.EnergyWh, b.EnergyWh, b.HasEnergy
	t.wattSum, t.wattN = 0, 0
	t.score = map[string]float64{}
}

func (t *BatteryTracker) close() *model.BatteryWindow {
	if !t.open {
		return nil
	}
	t.open = false
	if t.last.Sub(t.start) < t.cfg.MinWindow {
		return nil
	}
	w := &model.BatteryWindow{Start: t.start, End: t.last, DrainPct: math.Max(0, t.startPct-t.endPct)}
	if t.hasWh {
		w.DrainWh = math.Max(0, t.startWh-t.endWh)
	}
	if t.wattN > 0 {
		w.AvgWatts = t.wattSum / float64(t.wattN)
	}
	var total float64
	for _, v := range t.score {
		total += v
	}
	if total == 0 {
		return w
	}
	for app, v := range t.score {
		share := v / total
		if share < t.cfg.MinShare {
			continue
		}
		w.Apps = append(w.Apps, model.AppDrain{App: app, Share: share, Wh: share * w.DrainWh, Pct: share * w.DrainPct})
	}
	sort.Slice(w.Apps, func(i, j int) bool {
		if w.Apps[i].Share != w.Apps[j].Share {
			return w.Apps[i].Share > w.Apps[j].Share
		}
		return w.Apps[i].App < w.Apps[j].App
	})
	return w
}
