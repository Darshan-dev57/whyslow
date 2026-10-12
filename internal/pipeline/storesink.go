package pipeline

import (
	"context"
	"log/slog"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/model"
	"github.com/Darshan-dev57/whyslow/internal/store"
)

// StoreSink persists pipeline output. Frames are queued and written in one
// transaction every FlushEvery; mysteries are written at once so the live
// feed can link to them by ID. Run owns the database writes.
type StoreSink struct {
	st         *store.Store
	frames     chan *model.Snapshot
	mysteries  chan *model.Mystery
	batteries  chan *model.BatteryWindow
	FlushEvery time.Duration
	Retention  store.Retention
	log        *slog.Logger
	dropped    int
}

// NewStoreSink returns a sink writing to st.
func NewStoreSink(st *store.Store, log *slog.Logger) *StoreSink {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &StoreSink{
		st:         st,
		frames:     make(chan *model.Snapshot, 120),
		mysteries:  make(chan *model.Mystery, 16),
		batteries:  make(chan *model.BatteryWindow, 4),
		FlushEvery: 5 * time.Second,
		Retention:  store.DefaultRetention(),
		log:        log,
	}
}

// Frame queues a snapshot; if the writer is two minutes behind, it drops.
func (s *StoreSink) Frame(f *Frame) {
	select {
	case s.frames <- f.Snapshot:
	default:
		s.dropped++
	}
}

// Mystery stores a mystery and fills in its ID before other sinks see it.
// Sinks run in order, so put the store sink first.
func (s *StoreSink) Mystery(m *model.Mystery) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.st.InsertMystery(ctx, m); err != nil {
		s.log.Error("store mystery", "err", err)
	}
}

// Battery queues a battery window.
func (s *StoreSink) Battery(w *model.BatteryWindow) {
	select {
	case s.batteries <- w:
	default:
	}
}

// Run writes until ctx is cancelled, then flushes what is queued.
func (s *StoreSink) Run(ctx context.Context) error {
	flush := time.NewTicker(s.FlushEvery)
	defer flush.Stop()
	rollup := time.NewTicker(time.Minute)
	defer rollup.Stop()
	retain := time.NewTicker(time.Hour)
	defer retain.Stop()

	var batch []*model.Snapshot
	write := func(ctx context.Context) {
		if len(batch) == 0 {
			return
		}
		if err := s.st.WriteSnapshots(ctx, batch); err != nil {
			s.log.Error("store samples", "err", err, "rows", len(batch))
		}
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			// drain what is queued with a fresh deadline
			bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			for drained := false; !drained; {
				select {
				case f := <-s.frames:
					batch = append(batch, f)
				default:
					drained = true
				}
			}
			write(bg)
			cancel()
			return nil
		case f := <-s.frames:
			batch = append(batch, f)
		case w := <-s.batteries:
			if err := s.st.InsertBatteryWindow(ctx, w); err != nil {
				s.log.Error("store battery window", "err", err)
			}
		case <-flush.C:
			write(ctx)
		case <-rollup.C:
			write(ctx)
			if err := s.st.Rollup(ctx, time.Now().Add(-time.Minute)); err != nil {
				s.log.Error("rollup", "err", err)
			}
		case <-retain.C:
			if err := s.st.Retain(ctx, time.Now(), s.Retention); err != nil {
				s.log.Error("retention", "err", err)
			}
		}
	}
}
