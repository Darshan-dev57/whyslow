// Package detect finds spikes in a metric's time series.
//
// The baseline is the median of the last Window samples and its spread is
// the median absolute deviation (MAD). Both ignore outliers: one huge
// download moves a mean and a standard deviation a lot, but barely moves a
// median, so the detector does not go blind right after a spike the way a
// mean-based one does.
//
// A spike opens when the value stays above the threshold for Sustain samples
// in a row (one noisy second is not a spike), and closes after Release calm
// samples below ReleaseFrac × threshold (hysteresis, so a value hovering at
// the line does not open and close every second). After a spike the metric
// gets a Cooldown before a new one can open. While a spike is open its
// samples stay out of the baseline, so a long spike does not teach the
// detector that the spike is normal; a spike longer than MaxLen is closed
// and the baseline restarts, accepting the new level.
package detect

import (
	"math"
	"slices"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/model"
)

// Config tunes one detector. The threshold is
//
//	max(median + K·1.4826·MAD, Ratio·median, median + MinJump, Floor)
//
// optionally capped at Cap so a busy baseline cannot push it out of reach.
type Config struct {
	Window      int
	Warmup      int
	K           float64
	Ratio       float64
	MinJump     float64
	Floor       float64
	Cap         float64
	Sustain     int
	Release     int
	ReleaseFrac float64
	Cooldown    time.Duration
	MaxLen      time.Duration
}

// madScale makes the MAD of normally distributed data match its standard deviation.
const madScale = 1.4826

// Defaults returns the tuned configuration for a metric. memTotal (bytes)
// scales the memory jump that counts as a spike.
func Defaults(m model.Metric, memTotal uint64) Config {
	c := Config{
		Window: 120, Warmup: 60, K: 6, Sustain: 3, Release: 5, ReleaseFrac: 0.8,
		Cooldown: 30 * time.Second, MaxLen: 10 * time.Minute,
	}
	switch m {
	case model.CPU:
		// percent of all cores
		c.Ratio, c.MinJump, c.Floor, c.Cap = 2, 20, 25, 90
	case model.Mem:
		// used memory never triples; a spike is a big absolute jump
		c.MinJump = math.Max(512<<20, 0.1*float64(memTotal))
	case model.DiskRead, model.DiskWrite:
		c.Ratio, c.Floor = 3, 5e6
	case model.NetRx, model.NetTx:
		c.Ratio, c.Floor = 3, 1e6
	}
	return c
}

// Spike is an open or closed spike.
type Spike struct {
	Metric    model.Metric
	Onset     time.Time // first sample above the threshold
	End       time.Time // first calm sample; zero while open
	Baseline  float64   // median when the spike began
	Threshold float64
	Peak      float64
	Mean      float64 // over the hot samples
	Samples   int
	Truncated bool // closed at MaxLen
}

// EventKind says whether a spike opened or closed.
type EventKind int

// Event kinds.
const (
	Opened EventKind = iota + 1
	Closed
)

// Event is a spike changing state.
type Event struct {
	Kind  EventKind
	Spike Spike
}

type point struct {
	ts time.Time
	v  float64
}

// Detector tracks one metric. It is not safe for concurrent use.
type Detector struct {
	metric model.Metric
	cfg    Config

	hist   []float64 // ring buffer
	next   int
	filled int

	pending       []point // above threshold, not yet sustained
	cooldownUntil time.Time

	active  *Spike
	sum     float64
	calm    []point // calm samples inside an open spike
	scratch []float64
}

// New returns a detector for metric m.
func New(m model.Metric, cfg Config) *Detector {
	return &Detector{metric: m, cfg: cfg, hist: make([]float64, cfg.Window)}
}

// Metric returns the metric this detector watches.
func (d *Detector) Metric() model.Metric { return d.metric }

// Active returns the open spike, or nil.
func (d *Detector) Active() *Spike {
	if d.active == nil {
		return nil
	}
	s := *d.active
	return &s
}

// Threshold returns the current threshold and whether the detector is warm.
func (d *Detector) Threshold() (float64, bool) {
	if d.active != nil {
		return d.active.Threshold, true
	}
	if d.filled < d.cfg.Warmup {
		return 0, false
	}
	_, _, thr := d.baseline()
	return thr, true
}

func (d *Detector) push(v float64) {
	d.hist[d.next] = v
	d.next = (d.next + 1) % len(d.hist)
	if d.filled < len(d.hist) {
		d.filled++
	}
}

func (d *Detector) baseline() (median, sigma, threshold float64) {
	d.scratch = append(d.scratch[:0], d.hist[:d.filled]...)
	median = medianOf(d.scratch)
	for i, v := range d.scratch {
		d.scratch[i] = math.Abs(v - median)
	}
	sigma = madScale * medianOf(d.scratch)
	c := d.cfg
	threshold = max(median+c.K*sigma, c.Ratio*median, median+c.MinJump, c.Floor)
	if c.Cap > 0 && threshold > c.Cap {
		threshold = max(c.Cap, median+c.MinJump/2)
	}
	return median, sigma, threshold
}

// medianOf sorts s in place and returns its median.
func medianOf(s []float64) float64 {
	if len(s) == 0 {
		return 0
	}
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// Observe feeds one sample and returns an event when a spike opens or closes.
func (d *Detector) Observe(ts time.Time, v float64) *Event {
	if d.active != nil {
		return d.observeActive(ts, v)
	}
	if d.filled < d.cfg.Warmup || ts.Before(d.cooldownUntil) {
		d.flushPending()
		d.push(v)
		return nil
	}
	median, _, thr := d.baseline()
	if v <= thr {
		d.flushPending()
		d.push(v)
		return nil
	}
	d.pending = append(d.pending, point{ts, v})
	if len(d.pending) < d.cfg.Sustain {
		return nil
	}
	s := &Spike{Metric: d.metric, Onset: d.pending[0].ts, Baseline: median, Threshold: thr}
	d.sum = 0
	for _, p := range d.pending {
		s.Peak = max(s.Peak, p.v)
		d.sum += p.v
		s.Samples++
	}
	s.Mean = d.sum / float64(s.Samples)
	d.pending = d.pending[:0]
	d.calm = d.calm[:0]
	d.active = s
	return &Event{Kind: Opened, Spike: *s}
}

func (d *Detector) observeActive(ts time.Time, v float64) *Event {
	s := d.active
	if v < s.Threshold*d.cfg.ReleaseFrac {
		d.calm = append(d.calm, point{ts, v})
		if len(d.calm) >= d.cfg.Release {
			s.End = d.calm[0].ts
			for _, p := range d.calm {
				d.push(p.v) // calm samples are normal again
			}
			d.calm = d.calm[:0]
			return d.close(ts)
		}
	} else {
		// A dip that did not last is part of the spike.
		for _, p := range d.calm {
			d.sum += p.v
			s.Samples++
		}
		d.calm = d.calm[:0]
		d.sum += v
		s.Samples++
		s.Peak = max(s.Peak, v)
	}
	if ts.Sub(s.Onset) >= d.cfg.MaxLen && d.active != nil {
		for _, p := range d.calm {
			d.sum += p.v
			s.Samples++
		}
		d.calm = d.calm[:0]
		s.End = ts
		s.Truncated = true
		// A level that lasted this long is the new normal: start over.
		d.filled, d.next = 0, 0
		return d.close(ts)
	}
	s.Mean = d.sum / float64(s.Samples)
	return nil
}

func (d *Detector) close(ts time.Time) *Event {
	s := d.active
	s.Mean = d.sum / float64(s.Samples)
	d.active = nil
	d.cooldownUntil = ts.Add(d.cfg.Cooldown)
	return &Event{Kind: Closed, Spike: *s}
}

func (d *Detector) flushPending() {
	for _, p := range d.pending {
		d.push(p.v)
	}
	d.pending = d.pending[:0]
}
