package pipeline

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/detect"
	"github.com/Darshan-dev57/whyslow/internal/model"
	"github.com/Darshan-dev57/whyslow/internal/store"
)

var t0 = time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)

type recorder struct {
	frames    []*Frame
	mysteries []*model.Mystery
	batteries []*model.BatteryWindow
}

func (r *recorder) Frame(f *Frame)                 { r.frames = append(r.frames, f) }
func (r *recorder) Mystery(m *model.Mystery)       { r.mysteries = append(r.mysteries, m) }
func (r *recorder) Battery(w *model.BatteryWindow) { r.batteries = append(r.batteries, w) }

// diskSpike makes 200 one-second snapshots: an editor writes 1 MB/s all
// along; dd starts at second 150 and writes 80 MB/s for 20 s.
func diskSpike() []*model.Snapshot {
	var out []*model.Snapshot
	for i := 0; i < 200; i++ {
		ts := t0.Add(time.Duration(i) * time.Second)
		s := &model.Snapshot{TS: ts, Elapsed: time.Second, Sys: model.System{MemTotal: 8e9, MemUsed: 3e9, DiskWrite: 1e6, CPU: 5},
			Apps: []model.App{{Name: "editor", DiskWrite: 1e6, CPU: 5, Started: t0.Add(-time.Hour)}}}
		if i >= 150 && i < 170 {
			s.Sys.DiskWrite += 80e6
			s.Apps = append(s.Apps, model.App{Name: "dd", DiskWrite: 80e6, Started: t0.Add(149 * time.Second)})
		}
		out = append(out, s)
	}
	return out
}

func TestPipelineExplainsSpike(t *testing.T) {
	rec := &recorder{}
	cfg := DefaultConfig()
	cfg.Blame.Location = time.UTC
	p := New(cfg, false, rec)
	var closed []*model.Mystery
	sawOpen := false
	for _, s := range diskSpike() {
		closed = append(closed, p.Process(s)...)
		if f := rec.frames[len(rec.frames)-1]; len(f.Open) == 1 && f.Open[0].Metric == model.DiskWrite {
			sawOpen = true
		}
	}
	if !sawOpen {
		t.Error("open spike never shown in a frame")
	}
	if len(closed) != 1 || len(rec.mysteries) != 1 {
		t.Fatalf("mysteries = %d", len(closed))
	}
	m := closed[0]
	if m.Metric != model.DiskWrite || !m.Start.Equal(t0.Add(150*time.Second)) || !m.End.Equal(t0.Add(170*time.Second)) {
		t.Errorf("mystery window = %s %v..%v", m.Metric, m.Start, m.End)
	}
	if top, share := m.TopApp(); top != "dd" || share != 1 || !m.Culprits[0].Newcomer {
		t.Errorf("culprits = %+v", m.Culprits)
	}
	want := "Disk write spiked 81.0x at 15:02:30 for 20 s — 100% from dd (started 1 s earlier)."
	if m.Summary != want {
		t.Errorf("summary\n got: %s\nwant: %s", m.Summary, want)
	}
	if len(rec.frames) != 200 {
		t.Errorf("frames = %d", len(rec.frames))
	}
}

func TestTune(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tune = func(m model.Metric, c *detect.Config) { c.Warmup = 1000 }
	p := New(cfg, false)
	for _, s := range diskSpike() {
		if got := p.Process(s); len(got) != 0 {
			t.Fatal("Tune not applied: detector warmed up early")
		}
	}
}

func TestRing(t *testing.T) {
	r := NewRing(3)
	if r.Last() != nil || len(r.Range(t0, t0.Add(time.Hour))) != 0 {
		t.Error("empty ring")
	}
	for i := 0; i < 5; i++ {
		r.Push(&model.Snapshot{TS: t0.Add(time.Duration(i) * time.Second)})
	}
	got := r.Range(t0, t0.Add(time.Hour))
	if r.Len() != 3 || len(got) != 3 || !got[0].TS.Equal(t0.Add(2*time.Second)) || !r.Last().TS.Equal(t0.Add(4*time.Second)) {
		t.Errorf("ring = %d items, first %v", len(got), got[0].TS)
	}
	if got := r.Range(t0.Add(3*time.Second), t0.Add(4*time.Second)); len(got) != 1 {
		t.Errorf("half-open range = %d", len(got))
	}
}

func TestStoreSink(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sink := NewStoreSink(st, nil)
	sink.FlushEvery = 10 * time.Millisecond
	// the test feeds 200 frames at once; in production they come one per
	// second and the 120-frame queue is two minutes of slack
	sink.frames = make(chan *model.Snapshot, 256)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- sink.Run(ctx) }()

	cfg := DefaultConfig()
	cfg.Blame.Location = time.UTC
	rec := &recorder{}
	p := New(cfg, false, sink, rec)
	for _, s := range diskSpike() {
		p.Process(s)
	}
	if len(rec.mysteries) != 1 || rec.mysteries[0].ID != 1 {
		t.Fatalf("the store sink must set the ID before later sinks: %+v", rec.mysteries)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if sink.dropped != 0 {
		t.Errorf("dropped %d frames", sink.dropped)
	}
	pts, err := st.Timeline(context.Background(), model.DiskWrite, t0, t0.Add(time.Hour), "dd")
	if err != nil || len(pts) != 200 || pts[160].V != 80e6 {
		t.Errorf("stored timeline = %d points, %v", len(pts), err)
	}
	ms, _ := st.Mysteries(context.Background(), t0, t0.Add(time.Hour), 10)
	if len(ms) != 1 || ms[0].Culprits[0].App != "dd" {
		t.Errorf("stored mysteries = %+v", ms)
	}
}

// panicky panics on its first sample, then replays snaps and cancels.
type panicky struct {
	snaps  []*model.Snapshot
	calls  int
	cancel context.CancelFunc
}

func (s *panicky) NetPerApp() bool { return false }

func (s *panicky) Sample() (*model.Snapshot, error) {
	s.calls++
	if s.calls == 1 {
		var argv []string
		_ = argv[0] // the shape of the whitespace-title crash
	}
	if len(s.snaps) == 0 {
		s.cancel()
		return nil, errors.New("done")
	}
	x := s.snaps[0]
	s.snaps = s.snaps[1:]
	return x, nil
}

func TestRunSurvivesPanickingTick(t *testing.T) {
	rec := &recorder{}
	p := New(DefaultConfig(), false, rec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &panicky{snaps: diskSpike()[:5], cancel: cancel}
	if err := p.Run(ctx, src, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if len(rec.frames) != 5 {
		t.Errorf("frames after a panicking tick = %d, want 5", len(rec.frames))
	}
}
