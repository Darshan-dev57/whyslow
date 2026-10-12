// Package report builds the daily summary: which apps used the most of each
// resource, which drained the battery, and which keep causing spikes.
package report

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/model"
	"github.com/Darshan-dev57/whyslow/internal/store"
)

// Section is the top apps for one resource.
type Section struct {
	Resource store.Resource   `json:"resource"`
	Label    string           `json:"label"`
	Unit     string           `json:"unit"` // "cpu_s" or "bytes"
	Apps     []store.AppTotal `json:"apps"`
}

// Hog is one app's estimated share of the day's battery drain.
type Hog struct {
	App string  `json:"app"`
	Wh  float64 `json:"wh"`
	Pct float64 `json:"pct"`
}

// Battery sums the day's battery windows.
type Battery struct {
	Windows   int           `json:"windows"`
	OnBattery time.Duration `json:"on_battery_ns"`
	DrainWh   float64       `json:"drain_wh"`
	DrainPct  float64       `json:"drain_pct"`
	Hogs      []Hog         `json:"hogs"`
}

// Offender is an app that keeps showing up in mysteries.
type Offender struct {
	App    string       `json:"app"`
	Kind   string       `json:"kind"` // KindCulprit or KindOpener
	Metric model.Metric `json:"metric,omitempty"`
	Count  int          `json:"count"`
	Last   time.Time    `json:"last"`
	IDs    []int64      `json:"ids"`
	Text   string       `json:"text"`
}

// Offender kinds.
const (
	// KindCulprit: the app was the main culprit of several spikes of one metric.
	KindCulprit = "culprit"
	// KindOpener: the app had just started before several spikes. This is the
	// "my machine chokes every time X opens" pattern.
	KindOpener = "opener"
)

// Report is one day's summary.
type Report struct {
	Date      string               `json:"date"`
	From      time.Time            `json:"from"`
	To        time.Time            `json:"to"`
	Watched   time.Duration        `json:"watched_ns"`
	Sections  []Section            `json:"sections"`
	Battery   *Battery             `json:"battery"` // nil when the day had no time on battery
	Mysteries []*model.Mystery     `json:"mysteries"`
	ByMetric  map[model.Metric]int `json:"by_metric"`
	Repeat    []Offender           `json:"repeat"`
	// RepeatSince is the start of the window repeat offenders are drawn from.
	RepeatSince time.Time `json:"repeat_since"`
}

// Config tunes a report.
type Config struct {
	TopN int // apps per resource (5)
	// RepeatWindow is how far back repeat offenders look (7 days): one day
	// rarely has enough spikes to show a pattern.
	RepeatWindow time.Duration
	MinRepeats   int     // (2)
	MinShare     float64 // a culprit counts when its share is at least this (0.5)
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{TopN: 5, RepeatWindow: 7 * 24 * time.Hour, MinRepeats: 2, MinShare: 0.5}
}

var sections = []struct {
	res   store.Resource
	label string
	unit  string
}{
	{store.ResCPU, "CPU time", "cpu_s"},
	{store.ResMem, "Peak memory", "bytes"},
	{store.ResDiskRead, "Disk read", "bytes"},
	{store.ResDiskWrite, "Disk write", "bytes"},
	{store.ResNetRx, "Network download", "bytes"},
	{store.ResNetTx, "Network upload", "bytes"},
}

// DayBounds returns local midnight to midnight for the day containing t.
// It uses AddDate rather than 24 h so days with a DST change are right.
func DayBounds(t time.Time) (time.Time, time.Time) {
	from := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	return from, from.AddDate(0, 0, 1)
}

// Build assembles the report for the local day containing day.
func Build(ctx context.Context, st *store.Store, day time.Time, cfg Config) (*Report, error) {
	from, to := DayBounds(day)
	r := &Report{Date: from.Format("2006-01-02"), From: from, To: to, ByMetric: map[model.Metric]int{}}
	var err error
	if r.Watched, err = st.Watched(ctx, from, to); err != nil {
		return nil, err
	}
	for _, s := range sections {
		apps, err := st.TopApps(ctx, s.res, from, to, cfg.TopN)
		if err != nil {
			return nil, err
		}
		r.Sections = append(r.Sections, Section{Resource: s.res, Label: s.label, Unit: s.unit, Apps: apps})
	}
	ws, err := st.BatteryWindows(ctx, from, to)
	if err != nil {
		return nil, err
	}
	r.Battery = SumBattery(ws, cfg.TopN)

	if r.Mysteries, err = st.Mysteries(ctx, from, to, 1000); err != nil {
		return nil, err
	}
	for _, m := range r.Mysteries {
		r.ByMetric[m.Metric]++
	}
	r.RepeatSince = to.Add(-cfg.RepeatWindow)
	recent, err := st.Mysteries(ctx, r.RepeatSince, to, 5000)
	if err != nil {
		return nil, err
	}
	r.Repeat = Offenders(recent, cfg)
	return r, nil
}

// SumBattery adds up battery windows and their per-app estimates. It
// returns nil when there are none.
func SumBattery(ws []*model.BatteryWindow, topN int) *Battery {
	if len(ws) == 0 {
		return nil
	}
	b := &Battery{Windows: len(ws)}
	per := map[string]*Hog{}
	for _, w := range ws {
		b.OnBattery += w.End.Sub(w.Start)
		b.DrainWh += w.DrainWh
		b.DrainPct += w.DrainPct
		for _, a := range w.Apps {
			h := per[a.App]
			if h == nil {
				h = &Hog{App: a.App}
				per[a.App] = h
			}
			h.Wh += a.Wh
			h.Pct += a.Pct
		}
	}
	for _, h := range per {
		b.Hogs = append(b.Hogs, *h)
	}
	sort.Slice(b.Hogs, func(i, j int) bool {
		if b.Hogs[i].Pct != b.Hogs[j].Pct {
			return b.Hogs[i].Pct > b.Hogs[j].Pct
		}
		return b.Hogs[i].App < b.Hogs[j].App
	})
	if len(b.Hogs) > topN {
		b.Hogs = b.Hogs[:topN]
	}
	return b
}

// Offenders finds apps that keep causing spikes (the main culprit, with at
// least MinShare, of MinRepeats or more spikes of the same metric) and apps
// that keep starting right before spikes. Mysteries may be in any order.
func Offenders(ms []*model.Mystery, cfg Config) []Offender {
	type key struct {
		app, kind string
		metric    model.Metric
	}
	groups := map[key]*Offender{}
	add := func(k key, m *model.Mystery) {
		o := groups[k]
		if o == nil {
			o = &Offender{App: k.app, Kind: k.kind, Metric: k.metric}
			groups[k] = o
		}
		o.Count++
		o.IDs = append(o.IDs, m.ID)
		if m.Start.After(o.Last) {
			o.Last = m.Start
		}
	}
	for _, m := range ms {
		if app, share := m.TopApp(); app != "" && share >= cfg.MinShare {
			add(key{app, KindCulprit, m.Metric}, m)
		}
		seen := map[string]bool{}
		for _, n := range m.Newcomers {
			if !seen[n.App] {
				seen[n.App] = true
				add(key{n.App, KindOpener, ""}, m)
			}
		}
	}
	var out []Offender
	for _, o := range groups {
		if o.Count < cfg.MinRepeats {
			continue
		}
		sort.Slice(o.IDs, func(i, j int) bool { return o.IDs[i] < o.IDs[j] })
		o.Text = o.sentence()
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

func (o *Offender) sentence() string {
	if o.Kind == KindOpener {
		return fmt.Sprintf("%s had just started before %d spikes. The machine may choke every time it opens.", o.App, o.Count)
	}
	return fmt.Sprintf("%s was the main cause of %d %s spikes.", o.App, o.Count, lower(o.Metric.Label()))
}

func lower(s string) string {
	if s == "" || s == "CPU" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'A' && b[0] <= 'Z' {
		b[0] += 'a' - 'A'
	}
	return string(b)
}
