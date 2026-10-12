package pipeline

import (
	"time"

	"github.com/Darshan-dev57/whyslow/internal/model"
)

// Ring keeps the most recent snapshots so blame can look back to before a
// spike started without asking the database.
type Ring struct {
	buf  []*model.Snapshot
	next int
	n    int
}

// NewRing returns a ring that holds size snapshots.
func NewRing(size int) *Ring { return &Ring{buf: make([]*model.Snapshot, size)} }

// Push adds a snapshot, dropping the oldest when full.
func (r *Ring) Push(s *model.Snapshot) {
	r.buf[r.next] = s
	r.next = (r.next + 1) % len(r.buf)
	if r.n < len(r.buf) {
		r.n++
	}
}

// Len is the number of snapshots held.
func (r *Ring) Len() int { return r.n }

// Range returns the snapshots with from <= TS < to, oldest first.
func (r *Ring) Range(from, to time.Time) []*model.Snapshot {
	var out []*model.Snapshot
	start := (r.next - r.n + len(r.buf)) % len(r.buf)
	for i := 0; i < r.n; i++ {
		s := r.buf[(start+i)%len(r.buf)]
		if !s.TS.Before(from) && s.TS.Before(to) {
			out = append(out, s)
		}
	}
	return out
}

// Last returns the newest snapshot, or nil.
func (r *Ring) Last() *model.Snapshot {
	if r.n == 0 {
		return nil
	}
	return r.buf[(r.next-1+len(r.buf))%len(r.buf)]
}
