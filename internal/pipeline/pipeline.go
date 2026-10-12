// Package pipeline wires the collector to the detectors, the blame engine
// and the sinks (the store and the live web hub).
//
// One goroutine owns everything here: it samples, feeds the detectors and
// runs blame in order, so none of this state needs a lock. Sinks must not
// block it; the store sink queues writes and the hub drops frames for slow
// clients.
package pipeline

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/blame"
	"github.com/Darshan-dev57/whyslow/internal/detect"
	"github.com/Darshan-dev57/whyslow/internal/model"
)

// OpenSpike is a spike in progress, for shading live charts.
type OpenSpike struct {
	Metric    model.Metric `json:"metric"`
	Onset     time.Time    `json:"onset"`
	Threshold float64      `json:"threshold"`
	Peak      float64      `json:"peak"`
}

// Frame is what one tick produced.
type Frame struct {
	Snapshot *model.Snapshot
	Open     []OpenSpike
}

// Sink receives the pipeline's output. Methods are called from the pipeline
// goroutine and must return quickly.
type Sink interface {
	Frame(f *Frame)
	Mystery(m *model.Mystery)
	Battery(w *model.BatteryWindow)
}

// Sampler produces snapshots; *collector.Collector implements it.
type Sampler interface {
	Sample() (*model.Snapshot, error)
	NetPerApp() bool
}

// Config tunes the pipeline. Detector configs are derived per metric from
// detect.Defaults; Tune can adjust them (tests use shorter windows).
type Config struct {
	Blame   blame.Config
	Battery blame.BatteryConfig
	Tune    func(model.Metric, *detect.Config)
	// RingSize must cover the longest spike plus the blame pre-window.
	RingSize int
	Log      *slog.Logger
}

// DefaultConfig returns production settings.
func DefaultConfig() Config {
	return Config{Blame: blame.DefaultConfig(), Battery: blame.DefaultBatteryConfig(), RingSize: 12 * 60}
}

// Pipeline turns snapshots into frames, mysteries and battery windows.
type Pipeline struct {
	cfg       Config
	ring      *Ring
	detectors []*detect.Detector
	battery   *blame.BatteryTracker
	sinks     []Sink
	netPerApp bool
	log       *slog.Logger
}

// New returns a pipeline that reports to sinks.
func New(cfg Config, netPerApp bool, sinks ...Sink) *Pipeline {
	if cfg.RingSize == 0 {
		cfg.RingSize = 12 * 60
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Pipeline{cfg: cfg, ring: NewRing(cfg.RingSize), battery: blame.NewBatteryTracker(cfg.Battery),
		sinks: sinks, netPerApp: netPerApp, log: log}
}

func (p *Pipeline) init(s *model.Snapshot) {
	for _, m := range model.Metrics {
		c := detect.Defaults(m, s.Sys.MemTotal)
		if p.cfg.Tune != nil {
			p.cfg.Tune(m, &c)
		}
		p.detectors = append(p.detectors, detect.New(m, c))
	}
}

// Process handles one snapshot and returns the mysteries it closed.
func (p *Pipeline) Process(s *model.Snapshot) []*model.Mystery {
	if p.detectors == nil {
		p.init(s)
	}
	p.ring.Push(s)
	var closed []*model.Mystery
	var open []OpenSpike
	for _, d := range p.detectors {
		ev := d.Observe(s.TS, s.Sys.Value(d.Metric()))
		if ev != nil && ev.Kind == detect.Closed {
			closed = append(closed, p.explain(ev.Spike))
		}
		if a := d.Active(); a != nil {
			open = append(open, OpenSpike{Metric: a.Metric, Onset: a.Onset, Threshold: a.Threshold, Peak: a.Peak})
		}
		if ev != nil && ev.Kind == detect.Opened {
			p.log.Info("spike opened", "metric", ev.Spike.Metric, "value", ev.Spike.Metric.Format(ev.Spike.Peak),
				"threshold", ev.Spike.Metric.Format(ev.Spike.Threshold))
		}
	}
	f := &Frame{Snapshot: s, Open: open}
	for _, sk := range p.sinks {
		sk.Frame(f)
	}
	for _, m := range closed {
		p.log.Info("mystery", "summary", m.Summary)
		for _, sk := range p.sinks {
			sk.Mystery(m)
		}
	}
	if w := p.battery.Observe(s); w != nil {
		for _, sk := range p.sinks {
			sk.Battery(w)
		}
	}
	return closed
}

func (p *Pipeline) explain(sp detect.Spike) *model.Mystery {
	snaps := p.ring.Range(sp.Onset.Add(-p.cfg.Blame.PreWindow-p.cfg.Blame.PreGap), sp.End.Add(time.Second))
	return blame.Explain(blame.Spike{
		Metric: sp.Metric, Onset: sp.Onset, End: sp.End, Baseline: sp.Baseline, Threshold: sp.Threshold,
		Peak: sp.Peak, Mean: sp.Mean, Truncated: sp.Truncated,
	}, snaps, p.netPerApp, p.cfg.Blame)
}

// tick takes and processes one sample. A panic is recovered and returned
// with its stack.
func (p *Pipeline) tick(src Sampler) (recovered any, stack string) {
	defer func() {
		if v := recover(); v != nil {
			recovered, stack = v, string(debug.Stack())
		}
	}()
	s, err := src.Sample()
	if err != nil {
		p.log.Warn("sample failed", "err", err)
		return nil, ""
	}
	p.Process(s)
	return nil, ""
}

// Run samples every interval until ctx is cancelled. Ticks are aligned to
// the middle of a wall-clock second so that one-second samples never share
// a timestamp in the database.
func (p *Pipeline) Run(ctx context.Context, src Sampler, interval time.Duration) error {
	now := time.Now()
	first := now.Truncate(time.Second).Add(time.Second + 500*time.Millisecond)
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(first.Sub(now)):
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	panics := 0
	for {
		if v, stack := p.tick(src); v != nil {
			// One bad tick (a parser surprised by something in /proc) must
			// not take the whole daemon down. The stack is logged once.
			panics++
			if panics == 1 {
				p.log.Error("sampling tick panicked; skipping it", "panic", v, "stack", stack)
			} else {
				p.log.Error("sampling tick panicked; skipping it", "panic", v, "count", panics)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
