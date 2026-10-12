//go:build integration

// Package integration runs whyslow's real collector and pipeline on this
// machine and checks that a spike we cause is blamed on the process that
// caused it.
//
//	go test -tags integration ./test/integration
package integration

import (
	"context"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/collector"
	"github.com/Darshan-dev57/whyslow/internal/detect"
	"github.com/Darshan-dev57/whyslow/internal/model"
	"github.com/Darshan-dev57/whyslow/internal/pipeline"
)

type recorder struct {
	mu        sync.Mutex
	frames    int
	mysteries []*model.Mystery
}

func (r *recorder) Frame(*pipeline.Frame) { r.mu.Lock(); r.frames++; r.mu.Unlock() }
func (r *recorder) Mystery(m *model.Mystery) {
	r.mu.Lock()
	r.mysteries = append(r.mysteries, m)
	r.mu.Unlock()
}
func (r *recorder) Battery(*model.BatteryWindow) {}

func (r *recorder) snapshot() (int, []*model.Mystery) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.frames, append([]*model.Mystery(nil), r.mysteries...)
}

// TestBurnerIsBlamed starts CPU burners (dd copying /dev/zero to /dev/null,
// one per two cores, under timeout so they are their own app) and expects a
// CPU mystery whose top culprit is dd, flagged as a newcomer.
func TestBurnerIsBlamed(t *testing.T) {
	if testing.Short() {
		t.Skip("burns CPU for several seconds")
	}
	coll, err := collector.New(collector.Options{})
	if err != nil {
		t.Fatal(err)
	}
	const interval = 200 * time.Millisecond
	cfg := pipeline.DefaultConfig()
	// the production windows are in samples of one second; scale them to
	// the faster test interval so the test takes seconds, not minutes
	cfg.Tune = func(m model.Metric, c *detect.Config) {
		c.Window, c.Warmup, c.Sustain, c.Release = 40, 10, 3, 3
		c.Cooldown = time.Second
	}
	cfg.Blame.PreWindow = 2 * time.Second
	cfg.Blame.PreGap = 400 * time.Millisecond
	rec := &recorder{}
	p := pipeline.New(cfg, coll.NetPerApp(), rec)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, coll, interval) }()
	defer func() { cancel(); <-done }()

	// let the baseline settle: warmup samples plus the blame pre-window
	time.Sleep(4 * time.Second)

	burners := max(1, runtime.NumCPU()/2)
	var cmds []*exec.Cmd
	for i := 0; i < burners; i++ {
		c := exec.Command("timeout", "5", "dd", "if=/dev/zero", "of=/dev/null", "bs=64k")
		if err := c.Start(); err != nil {
			t.Fatalf("start burner: %v", err)
		}
		cmds = append(cmds, c)
	}
	for _, c := range cmds {
		_ = c.Wait() // timeout exits 124
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, ms := rec.snapshot()
		for _, m := range ms {
			if m.Metric != model.CPU {
				continue
			}
			app, share := m.TopApp()
			t.Logf("mystery: %s", m.Summary)
			if app != "dd" {
				continue
			}
			if share < 0.5 {
				t.Errorf("dd share = %.2f, want most of the spike", share)
			}
			newcomer := false
			for _, n := range m.Newcomers {
				newcomer = newcomer || n.App == "dd"
			}
			if !newcomer || !m.Culprits[0].Newcomer {
				t.Errorf("dd not flagged as a newcomer: %+v", m.Newcomers)
			}
			if m.Duration() < 3*time.Second || m.Duration() > 10*time.Second {
				t.Errorf("duration %v, burners ran 5 s", m.Duration())
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	frames, ms := rec.snapshot()
	t.Fatalf("no CPU mystery blamed dd after %d frames; got %d mysteries", frames, len(ms))
}
