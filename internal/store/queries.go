package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/model"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// Point is one value of a time series.
type Point struct {
	TS time.Time
	V  float64
}

// rawLimit is the longest range served from per-second rows; longer ranges
// use the minute rollups so a chart never gets hundreds of thousands of points.
const rawLimit = 2 * time.Hour

var sysCols = map[model.Metric][2]string{ // raw column, 1m column
	model.CPU: {"cpu", "cpu"}, model.Mem: {"mem_used", "mem_used"},
	model.DiskRead: {"disk_r", "disk_r"}, model.DiskWrite: {"disk_w", "disk_w"},
	model.NetRx: {"net_rx", "net_rx"}, model.NetTx: {"net_tx", "net_tx"},
}

// app 1m tables hold totals; dividing by 60 turns them back into rates.
var appCols = map[model.Metric][2]string{
	model.CPU: {"cpu", "cpu_s * 100 / 60.0"}, model.Mem: {"rss", "rss_max"},
	model.DiskRead: {"disk_r", "disk_r_b / 60.0"}, model.DiskWrite: {"disk_w", "disk_w_b / 60.0"},
	model.NetRx: {"net_rx", "net_rx_b / 60.0"}, model.NetTx: {"net_tx", "net_tx_b / 60.0"},
}

// Timeline returns a metric's series for the system (app == "") or one app.
// App series are aligned to the system's timestamps: a second in which the
// app was not stored (it was idle) reads as zero.
func (s *Store) Timeline(ctx context.Context, metric model.Metric, from, to time.Time, app string) ([]Point, error) {
	if !metric.Valid() {
		return nil, fmt.Errorf("store: unknown metric %q", metric)
	}
	raw := to.Sub(from) <= rawLimit
	var q string
	switch {
	case app == "" && raw:
		q = `SELECT ts, ` + sysCols[metric][0] + ` FROM sys_samples WHERE ts >= ? AND ts <= ? ORDER BY ts`
	case app == "":
		q = `SELECT ts, ` + sysCols[metric][1] + ` FROM sys_samples_1m WHERE ts >= ? AND ts <= ? ORDER BY ts`
	case raw:
		q = `SELECT s.ts, COALESCE(a.` + appCols[metric][0] + `, 0) FROM sys_samples s
			LEFT JOIN app_samples a ON a.ts = s.ts AND a.app_id = (SELECT id FROM apps WHERE name = ?)
			WHERE s.ts >= ? AND s.ts <= ? ORDER BY s.ts`
	default:
		q = `SELECT s.ts, COALESCE(a.` + appCols[metric][1] + `, 0) FROM sys_samples_1m s
			LEFT JOIN app_samples_1m a ON a.ts = s.ts AND a.app_id = (SELECT id FROM apps WHERE name = ?)
			WHERE s.ts >= ? AND s.ts <= ? ORDER BY s.ts`
	}
	args := []any{from.Unix(), to.Unix()}
	if app != "" {
		args = append([]any{app}, args...)
	}
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Point
	for rows.Next() {
		var ts int64
		var v sql.NullFloat64
		if err := rows.Scan(&ts, &v); err != nil {
			return nil, err
		}
		out = append(out, Point{TS: time.Unix(ts, 0), V: v.Float64})
	}
	return out, rows.Err()
}

// AppAvg is an app's average over a window.
type AppAvg struct {
	Name      string  `json:"name"`
	CPU       float64 `json:"cpu"`
	RSS       float64 `json:"rss"`
	DiskRead  float64 `json:"disk_read"`
	DiskWrite float64 `json:"disk_write"`
	NetRx     float64 `json:"net_rx"`
	NetTx     float64 `json:"net_tx"`
	Procs     float64 `json:"procs"`
}

// AppAverages returns per-app averages over [from, to], busiest CPU first.
// Seconds in which an app was not stored count as zero.
func (s *Store) AppAverages(ctx context.Context, from, to time.Time) ([]AppAvg, error) {
	var n float64
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys_samples WHERE ts >= ? AND ts <= ?`,
		from.Unix(), to.Unix()).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return []AppAvg{}, nil
	}
	rows, err := s.r.QueryContext(ctx, `SELECT p.name, SUM(a.cpu), SUM(a.rss), SUM(a.disk_r), SUM(a.disk_w),
			SUM(a.net_rx), SUM(a.net_tx), SUM(a.procs)
		FROM app_samples a JOIN apps p ON p.id = a.app_id
		WHERE a.ts >= ? AND a.ts <= ? GROUP BY p.name ORDER BY SUM(a.cpu) DESC, p.name`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AppAvg{}
	for rows.Next() {
		var a AppAvg
		if err := rows.Scan(&a.Name, &a.CPU, &a.RSS, &a.DiskRead, &a.DiskWrite, &a.NetRx, &a.NetTx, &a.Procs); err != nil {
			return nil, err
		}
		a.CPU /= n
		a.RSS /= n
		a.DiskRead /= n
		a.DiskWrite /= n
		a.NetRx /= n
		a.NetTx /= n
		a.Procs /= n
		out = append(out, a)
	}
	return out, rows.Err()
}

const mysteryCols = `id, metric, started_at, ended_at, baseline, threshold, peak, mean, ratio,
	culprits_json, newcomers_json, summary, truncated`

func scanMystery(sc interface{ Scan(...any) error }) (*model.Mystery, error) {
	var m model.Mystery
	var metric, culprits, newcomers string
	var start, end float64
	if err := sc.Scan(&m.ID, &metric, &start, &end, &m.Baseline, &m.Threshold, &m.Peak, &m.Mean, &m.Ratio,
		&culprits, &newcomers, &m.Summary, &m.Truncated); err != nil {
		return nil, err
	}
	m.Metric = model.Metric(metric)
	m.Start, m.End = fromUnixF(start), fromUnixF(end)
	if err := json.Unmarshal([]byte(culprits), &m.Culprits); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(newcomers), &m.Newcomers); err != nil {
		return nil, err
	}
	return &m, nil
}

// Mysteries returns mysteries that started in [from, to), newest first.
func (s *Store) Mysteries(ctx context.Context, from, to time.Time, limit int) ([]*model.Mystery, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.r.QueryContext(ctx, `SELECT `+mysteryCols+` FROM mysteries
		WHERE started_at >= ? AND started_at < ? ORDER BY started_at DESC LIMIT ?`, unixF(from), unixF(to), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Mystery{}
	for rows.Next() {
		m, err := scanMystery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Mystery returns one mystery by ID.
func (s *Store) Mystery(ctx context.Context, id int64) (*model.Mystery, error) {
	m, err := scanMystery(s.r.QueryRowContext(ctx, `SELECT `+mysteryCols+` FROM mysteries WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// AppTotal is an app's total use of one resource over a period.
type AppTotal struct {
	App   string  `json:"app"`
	Value float64 `json:"value"` // CPU-seconds, bytes, or peak RSS bytes
}

// Resource names what TopApps totals.
type Resource string

// Resources reported per day.
const (
	ResCPU       Resource = "cpu"        // CPU-seconds
	ResMem       Resource = "mem"        // peak RSS bytes
	ResDiskRead  Resource = "disk_read"  // bytes
	ResDiskWrite Resource = "disk_write" // bytes
	ResNetRx     Resource = "net_rx"     // bytes
	ResNetTx     Resource = "net_tx"     // bytes
)

var resCols = map[Resource][3]string{ // 1m column, raw expression, aggregate
	ResCPU:       {"cpu_s", "a.cpu / 100 * s.dt", "SUM"},
	ResMem:       {"rss_max", "a.rss", "MAX"},
	ResDiskRead:  {"disk_r_b", "a.disk_r * s.dt", "SUM"},
	ResDiskWrite: {"disk_w_b", "a.disk_w * s.dt", "SUM"},
	ResNetRx:     {"net_rx_b", "a.net_rx * s.dt", "SUM"},
	ResNetTx:     {"net_tx_b", "a.net_tx * s.dt", "SUM"},
}

// TopApps totals a resource per app over [from, to) and returns the top n.
// Rolled-up minutes come from the minute table and the rest from raw rows,
// so a report for today includes the last few seconds.
func (s *Store) TopApps(ctx context.Context, res Resource, from, to time.Time, n int) ([]AppTotal, error) {
	c, ok := resCols[res]
	if !ok {
		return nil, fmt.Errorf("store: unknown resource %q", res)
	}
	until, err := s.rollupUntil(ctx)
	if err != nil {
		return nil, err
	}
	split := min(max(until, from.Unix()), to.Unix())
	q := `SELECT p.name, ` + c[2] + `(v) AS total FROM (
			SELECT app_id, ` + c[0] + ` AS v FROM app_samples_1m WHERE ts >= ? AND ts < ?
			UNION ALL
			SELECT a.app_id, ` + c[1] + ` AS v FROM app_samples a JOIN sys_samples s ON s.ts = a.ts
			WHERE a.ts >= ? AND a.ts < ?
		) JOIN apps p ON p.id = app_id
		GROUP BY p.name HAVING total > 0 ORDER BY total DESC, p.name LIMIT ?`
	rows, err := s.r.QueryContext(ctx, q, from.Unix(), split, split, to.Unix(), n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AppTotal{}
	for rows.Next() {
		var t AppTotal
		if err := rows.Scan(&t.App, &t.Value); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) rollupUntil(ctx context.Context) (int64, error) {
	var v int64
	err := s.r.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'rollup_until'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

// BatteryWindows returns battery windows that started in [from, to).
func (s *Store) BatteryWindows(ctx context.Context, from, to time.Time) ([]*model.BatteryWindow, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT start, end, drain_wh, drain_pct, avg_w, attribution_json
		FROM battery_windows WHERE start >= ? AND start < ? ORDER BY start`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.BatteryWindow
	for rows.Next() {
		var w model.BatteryWindow
		var start, end int64
		var attr string
		if err := rows.Scan(&start, &end, &w.DrainWh, &w.DrainPct, &w.AvgWatts, &attr); err != nil {
			return nil, err
		}
		w.Start, w.End = time.Unix(start, 0), time.Unix(end, 0)
		if err := json.Unmarshal([]byte(attr), &w.Apps); err != nil {
			return nil, err
		}
		out = append(out, &w)
	}
	return out, rows.Err()
}

// Span returns the first and last sample times, or zero times when empty.
func (s *Store) Span(ctx context.Context) (first, last time.Time, err error) {
	var a, b sql.NullInt64
	err = s.r.QueryRowContext(ctx, `SELECT MIN(ts), MAX(ts) FROM sys_samples`).Scan(&a, &b)
	if err != nil || !a.Valid {
		return time.Time{}, time.Time{}, err
	}
	return time.Unix(a.Int64, 0), time.Unix(b.Int64, 0), nil
}
