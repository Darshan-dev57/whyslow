package detect

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/model"
)

var t0 = time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)

func at(i int) time.Time { return t0.Add(time.Duration(i) * time.Second) }

// diskCfg is the disk-write default: floor 5 MB/s, ratio 3.
func diskCfg() Config { return Defaults(model.DiskWrite, 0) }

// noise returns n samples around base with ±spread uniform noise.
func noise(r *rand.Rand, n int, base, spread float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = base + (r.Float64()*2-1)*spread
	}
	return out
}

func run(d *Detector, series []float64) []Event {
	var evs []Event
	for i, v := range series {
		if e := d.Observe(at(i), v); e != nil {
			evs = append(evs, *e)
		}
	}
	return evs
}

func TestFlatNoiseNoSpike(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	d := New(model.DiskWrite, diskCfg())
	if evs := run(d, noise(r, 1000, 3e6, 1e6)); len(evs) != 0 {
		t.Errorf("noise produced %d events: %+v", len(evs), evs)
	}
}

func TestShortBlipIsNotASpike(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	s := noise(r, 200, 3e6, 0.5e6)
	s[150], s[151] = 60e6, 60e6 // two seconds is under Sustain
	if evs := run(New(model.DiskWrite, diskCfg()), s); len(evs) != 0 {
		t.Errorf("2 s blip produced events: %+v", evs)
	}
}

func TestStepProducesOneSpike(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	s := noise(r, 200, 3e6, 0.5e6)
	for i := 120; i < 130; i++ {
		s[i] = 12.6e6 + float64(i-120)*0.1e6 // 12.6 .. 13.5 MB/s
	}
	evs := run(New(model.DiskWrite, diskCfg()), s)
	if len(evs) != 2 || evs[0].Kind != Opened || evs[1].Kind != Closed {
		t.Fatalf("events = %+v", evs)
	}
	open, closed := evs[0].Spike, evs[1].Spike
	if !open.Onset.Equal(at(120)) || !closed.Onset.Equal(at(120)) {
		t.Errorf("onset %v / %v, want %v", open.Onset, closed.Onset, at(120))
	}
	if !closed.End.Equal(at(130)) {
		t.Errorf("end = %v, want first calm sample %v", closed.End, at(130))
	}
	if closed.Samples != 10 || closed.Peak != 13.5e6 {
		t.Errorf("samples %d peak %v", closed.Samples, closed.Peak)
	}
	if want := 13.05e6; closed.Mean < want-1 || closed.Mean > want+1 {
		t.Errorf("mean = %v, want %v", closed.Mean, want)
	}
	if closed.Baseline < 2.5e6 || closed.Baseline > 3.5e6 {
		t.Errorf("baseline = %v", closed.Baseline)
	}
	// floor is 5 MB/s, 3x a ~3 MB/s median is ~9 MB/s: ratio wins
	if closed.Threshold < 8e6 || closed.Threshold > 11e6 {
		t.Errorf("threshold = %v", closed.Threshold)
	}
}

func TestFlappingIsOneSpike(t *testing.T) {
	s := make([]float64, 220)
	for i := range s {
		s[i] = 1e6
	}
	// 30 s hot with dips of 1-3 calm seconds inside: hysteresis keeps one spike
	for i := 100; i < 130; i++ {
		s[i] = 40e6
	}
	for _, i := range []int{105, 112, 113, 120, 121, 122} {
		s[i] = 0.5e6
	}
	evs := run(New(model.DiskWrite, diskCfg()), s)
	if len(evs) != 2 {
		t.Fatalf("flapping produced %d events: %+v", len(evs), evs)
	}
	sp := evs[1].Spike
	if !sp.Onset.Equal(at(100)) || !sp.End.Equal(at(130)) || sp.Samples != 30 {
		t.Errorf("spike = %+v", sp)
	}
}

func TestHoveringAtTheLineStaysOpen(t *testing.T) {
	// Values just under the threshold but above ReleaseFrac of it do not close.
	s := make([]float64, 200)
	for i := range s {
		s[i] = 1e6
	}
	for i := 100; i < 140; i++ {
		s[i] = 4.5e6 // threshold is the 5 MB/s floor; 4.5 > 0.8 x 5
	}
	s[100], s[101], s[102] = 9e6, 9e6, 9e6
	evs := run(New(model.DiskWrite, diskCfg()), s)
	if len(evs) != 2 || !evs[1].Spike.End.Equal(at(140)) {
		t.Errorf("events = %+v", evs)
	}
}

func TestCooldown(t *testing.T) {
	s := make([]float64, 300)
	for i := range s {
		s[i] = 1e6
	}
	burst := func(from int) {
		for i := from; i < from+5; i++ {
			s[i] = 50e6
		}
	}
	burst(100) // closes at 109 (5 calm samples from 105)
	burst(120) // inside the 30 s cooldown: ignored
	burst(160) // after cooldown: a new spike
	evs := run(New(model.DiskWrite, diskCfg()), s)
	if len(evs) != 4 {
		t.Fatalf("events = %d %+v", len(evs), evs)
	}
	if !evs[0].Spike.Onset.Equal(at(100)) || !evs[2].Spike.Onset.Equal(at(160)) {
		t.Errorf("onsets %v, %v", evs[0].Spike.Onset, evs[2].Spike.Onset)
	}
}

func TestWarmup(t *testing.T) {
	s := make([]float64, 100)
	for i := 10; i < 20; i++ {
		s[i] = 80e6
	}
	if evs := run(New(model.DiskWrite, diskCfg()), s); len(evs) != 0 {
		t.Errorf("spike during warmup: %+v", evs)
	}
}

func TestMaxLenTruncatesAndAcceptsNewLevel(t *testing.T) {
	cfg := diskCfg()
	cfg.MaxLen = 60 * time.Second
	s := make([]float64, 400)
	for i := range s {
		s[i] = 1e6
		if i >= 100 {
			s[i] = 30e6 // a permanent new level
		}
	}
	evs := run(New(model.DiskWrite, cfg), s)
	if len(evs) != 2 || !evs[1].Spike.Truncated || !evs[1].Spike.End.Equal(at(160)) {
		t.Fatalf("events = %+v", evs)
	}
}

func TestOpenSpikeIsVisible(t *testing.T) {
	d := New(model.DiskWrite, diskCfg())
	for i := 0; i < 100; i++ {
		d.Observe(at(i), 1e6)
	}
	if thr, warm := d.Threshold(); !warm || thr != 5e6 {
		t.Errorf("threshold = %v warm=%v, want the 5 MB/s floor", thr, warm)
	}
	for i := 100; i < 104; i++ {
		d.Observe(at(i), 20e6)
	}
	a := d.Active()
	if a == nil || !a.Onset.Equal(at(100)) || a.Samples != 4 || a.Mean != 20e6 {
		t.Fatalf("active = %+v", a)
	}
	a.Peak = 0 // a copy: must not change the detector
	if d.Active().Peak != 20e6 {
		t.Error("Active leaked internal state")
	}
}

func TestThresholds(t *testing.T) {
	fill := func(cfg Config, v float64) *Detector {
		d := New(model.CPU, cfg)
		for i := 0; i < cfg.Window; i++ {
			d.Observe(at(i), v)
		}
		return d
	}
	// CPU: idle machine -> the 25-point floor
	if thr, _ := fill(Defaults(model.CPU, 0), 3).Threshold(); thr != 25 {
		t.Errorf("idle cpu threshold = %v", thr)
	}
	// CPU: busy machine -> 2x median would be 100, capped at 90
	if thr, _ := fill(Defaults(model.CPU, 0), 50).Threshold(); thr != 90 {
		t.Errorf("busy cpu threshold = %v", thr)
	}
	// CPU: nearly saturated -> the cap moves up to median + MinJump/2
	if thr, _ := fill(Defaults(model.CPU, 0), 85).Threshold(); thr != 95 {
		t.Errorf("saturated cpu threshold = %v", thr)
	}
	// memory: 8 GB machine using 3 GB -> needs +819 MB (10% of total)
	mem := Defaults(model.Mem, 8192<<20)
	if thr, _ := fill(mem, 3e9).Threshold(); thr != 3e9+0.1*float64(8192<<20) {
		t.Errorf("mem threshold = %v", thr)
	}
}

func TestMedianOf(t *testing.T) {
	if medianOf(nil) != 0 || medianOf([]float64{3, 1, 2}) != 2 || medianOf([]float64{4, 1, 3, 2}) != 2.5 {
		t.Error("medianOf")
	}
}
