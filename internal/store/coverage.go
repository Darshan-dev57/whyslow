package store

import (
	"context"
	"time"
)

// Watched estimates how long whyslow was running in [from, to): whole
// minutes from the rollup table plus the real sample intervals of raw rows
// that are not rolled up yet. A report uses it to say how much of the day
// it actually saw.
func (s *Store) Watched(ctx context.Context, from, to time.Time) (time.Duration, error) {
	until, err := s.rollupUntil(ctx)
	if err != nil {
		return 0, err
	}
	split := min(max(until, from.Unix()), to.Unix())
	var minutes int64
	var raw float64
	err = s.r.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM sys_samples_1m WHERE ts >= ? AND ts < ?),
		(SELECT COALESCE(SUM(dt), 0) FROM sys_samples WHERE ts >= ? AND ts < ?)`,
		from.Unix(), split, split, to.Unix()).Scan(&minutes, &raw)
	if err != nil {
		return 0, err
	}
	return time.Duration(minutes)*time.Minute + time.Duration(raw*float64(time.Second)), nil
}
