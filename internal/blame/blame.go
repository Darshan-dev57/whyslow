// Package blame explains a spike: which apps added the load, and which apps
// started just before it.
//
// For each app it compares the app's mean during the spike with its mean in
// the minute before (ending a few seconds before onset, so the ramp-up does
// not count as "before"). Only increases count; each app's share is its
// increase over the sum of all increases. This is correlation in time, not
// proof of cause, which is why newcomers are reported separately: "chrome
// started 6 s earlier" is often the more useful clue.
package blame

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/model"
)

// Config tunes the blame engine.
type Config struct {
	PreWindow   time.Duration // length of the "before" window (60 s)
	PreGap      time.Duration // gap between the before window and onset (5 s)
	NewcomerAge time.Duration // an app started this long before onset is a newcomer (15 s)
	MinShare    float64       // culprits below this share are dropped (0.05)
	MaxCulprits int           // (5)
	SpreadShare float64       // top share below this reads as "spread" (0.30)
	Location    *time.Location
}

// DefaultConfig returns the tuned defaults.
func DefaultConfig() Config {
	return Config{
		PreWindow: 60 * time.Second, PreGap: 5 * time.Second, NewcomerAge: 15 * time.Second,
		MinShare: 0.05, MaxCulprits: 5, SpreadShare: 0.30, Location: time.Local,
	}
}

// Spike is the part of a detected spike that blame needs.
type Spike struct {
	Metric    model.Metric
	Onset     time.Time
	End       time.Time
	Baseline  float64
	Threshold float64
	Peak      float64
	Mean      float64
	Truncated bool
}

// Explain builds a Mystery from a spike and the snapshots around it. snaps
// must be in time order and may extend beyond the spike on either side.
// netPerApp says whether apps carry real network numbers.
func Explain(sp Spike, snaps []*model.Snapshot, netPerApp bool, cfg Config) *model.Mystery {
	m := &model.Mystery{
		Metric: sp.Metric, Start: sp.Onset, End: sp.End, Baseline: sp.Baseline,
		Threshold: sp.Threshold, Peak: sp.Peak, Mean: sp.Mean, Truncated: sp.Truncated,
	}
	if sp.Metric != model.CPU && sp.Metric != model.Mem && sp.Baseline > minBaseline(sp.Metric) {
		m.Ratio = sp.Mean / sp.Baseline
	}

	preFrom, preTo := sp.Onset.Add(-cfg.PreGap-cfg.PreWindow), sp.Onset.Add(-cfg.PreGap)
	var pre, during []*model.Snapshot
	for _, s := range snaps {
		switch {
		case !s.TS.Before(preFrom) && s.TS.Before(preTo):
			pre = append(pre, s)
		case !s.TS.Before(sp.Onset) && s.TS.Before(sp.End):
			during = append(during, s)
		}
	}
	if len(during) == 0 { // a spike that closed at once still has its onset sample
		for _, s := range snaps {
			if s.TS.Equal(sp.Onset) {
				during = append(during, s)
			}
		}
	}

	m.Newcomers = newcomers(sp.Onset, pre, during, cfg)
	isNew := map[string]bool{}
	for _, n := range m.Newcomers {
		isNew[n.App] = true
	}
	if !sp.Metric.IsNet() || netPerApp {
		m.Culprits = Rank(sp.Metric, pre, during, cfg)
		for i := range m.Culprits {
			m.Culprits[i].Newcomer = isNew[m.Culprits[i].App]
		}
	}
	m.Summary = Sentence(m, netPerApp, cfg)
	return m
}

// minBaseline is the baseline below which "N times normal" is meaningless.
func minBaseline(m model.Metric) float64 {
	switch m {
	case model.DiskRead, model.DiskWrite:
		return 250e3
	case model.NetRx, model.NetTx:
		return 50e3
	}
	return 0
}

// Rank orders apps by how much they added during the spike.
func Rank(metric model.Metric, pre, during []*model.Snapshot, cfg Config) []model.Culprit {
	before := means(metric, pre)
	now := means(metric, during)
	type cand struct {
		model.Culprit
		delta float64
	}
	var cands []cand
	var total float64
	for app, d := range now {
		delta := d - before[app]
		if delta <= 0 {
			continue
		}
		total += delta
		cands = append(cands, cand{model.Culprit{App: app, Before: before[app], During: d}, delta})
	}
	if total == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].delta != cands[j].delta {
			return cands[i].delta > cands[j].delta
		}
		return cands[i].App < cands[j].App
	})
	var out []model.Culprit
	for _, c := range cands {
		c.Share = c.delta / total
		if c.Share < cfg.MinShare || len(out) == cfg.MaxCulprits {
			break
		}
		out = append(out, c.Culprit)
	}
	return out
}

// means returns each app's mean over the snapshots; an app missing from a
// snapshot counts as zero there.
func means(metric model.Metric, snaps []*model.Snapshot) map[string]float64 {
	out := map[string]float64{}
	if len(snaps) == 0 {
		return out
	}
	for _, s := range snaps {
		for i := range s.Apps {
			out[s.Apps[i].Name] += s.Apps[i].Value(metric)
		}
	}
	for k := range out {
		out[k] /= float64(len(snaps))
	}
	return out
}

func newcomers(onset time.Time, pre, during []*model.Snapshot, cfg Config) []model.Newcomer {
	seen := map[string]time.Time{}
	look := during
	if len(pre) > 0 {
		look = append([]*model.Snapshot{pre[len(pre)-1]}, during...)
	}
	for _, s := range look {
		for i := range s.Apps {
			a := &s.Apps[i]
			if a.Started.IsZero() || a.Name == "kernel" {
				continue
			}
			if t, ok := seen[a.Name]; !ok || a.Started.Before(t) {
				seen[a.Name] = a.Started
			}
		}
	}
	// An app that existed long before onset (its oldest process is old) is
	// not new, even if one of its processes is.
	var out []model.Newcomer
	for app, started := range seen {
		ago := onset.Sub(started)
		if ago <= cfg.NewcomerAge && started.Before(onset.Add(3*time.Second)) {
			out = append(out, model.Newcomer{App: app, StartedAgo: math.Round(ago.Seconds()*10) / 10})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAgo != out[j].StartedAgo {
			return out[i].StartedAgo < out[j].StartedAgo
		}
		return out[i].App < out[j].App
	})
	return out
}

// Sentence writes the one-line explanation shown in the feed.
func Sentence(m *model.Mystery, netPerApp bool, cfg Config) string {
	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}
	var b strings.Builder
	label := m.Metric.Label()
	when := m.Start.In(loc).Format("15:04:05")
	dur := FormatDuration(m.Duration())
	if m.Truncated {
		dur = "over " + dur
	}
	switch {
	case m.Metric == model.Mem:
		fmt.Fprintf(&b, "Memory use rose by %s at %s for %s (to %s)", model.Bytes(m.Mean-m.Baseline), when, dur, model.Bytes(m.Mean))
	case m.Ratio > 0:
		fmt.Fprintf(&b, "%s spiked %.1fx at %s for %s", label, m.Ratio, when, dur)
	default:
		fmt.Fprintf(&b, "%s jumped to %s at %s for %s (normally %s)", label,
			m.Metric.Format(m.Mean), when, dur, m.Metric.Format(m.Baseline))
	}

	started := map[string]float64{}
	for _, n := range m.Newcomers {
		started[n.App] = n.StartedAgo
	}
	var parts []string
	named := map[string]bool{}
	for i, c := range m.Culprits {
		if i == 2 {
			break
		}
		p := fmt.Sprintf("%.0f%% from %s", math.Round(c.Share*100), c.App)
		if c.Newcomer {
			p += " (" + startedPhrase(started[c.App]) + ")"
		}
		parts = append(parts, p)
		named[c.App] = true
	}
	switch {
	case m.Metric.IsNet() && !netPerApp:
		b.WriteString(" — per-app network needs eBPF (run with --ebpf)")
	case len(parts) == 0:
		b.WriteString(" — no app's own usage rose; likely the kernel or another machine-wide cause")
	case m.Culprits[0].Share < cfg.SpreadShare:
		b.WriteString(" — spread across several apps; biggest: " + strings.Join(parts, ", "))
	default:
		b.WriteString(" — " + strings.Join(parts, ", "))
	}
	var others []string
	for _, n := range m.Newcomers {
		if !named[n.App] {
			others = append(others, fmt.Sprintf("%s (%s)", n.App, startedPhrase(n.StartedAgo)))
		}
		if len(others) == 3 {
			break
		}
	}
	if len(others) > 0 {
		b.WriteString(". Just started: " + strings.Join(others, ", "))
	}
	b.WriteString(".")
	return b.String()
}

func startedPhrase(ago float64) string {
	switch s := math.Round(ago); {
	case s <= 0:
		return "started as it began"
	case s == 1:
		return "started 1 s earlier"
	default:
		return fmt.Sprintf("started %.0f s earlier", s)
	}
}

// FormatDuration renders 38 s, 2 min 5 s, 1 h 3 min.
func FormatDuration(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%d s", s)
	case s < 3600:
		if s%60 == 0 {
			return fmt.Sprintf("%d min", s/60)
		}
		return fmt.Sprintf("%d min %d s", s/60, s%60)
	default:
		return fmt.Sprintf("%d h %d min", s/3600, (s%3600)/60)
	}
}
