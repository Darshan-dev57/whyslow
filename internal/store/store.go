// Package store keeps whyslow's timeline in SQLite.
//
// It uses the pure-Go modernc.org/sqlite driver (no CGO), WAL journaling so
// readers never block the writer, one dedicated writer connection, and a
// small pool of read-only connections for the web API and the CLI. Samples
// are written in batches inside a transaction: one fsync per batch instead
// of one per row.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/Darshan-dev57/whyslow/internal/model"
)

// Store is an open database.
type Store struct {
	path string
	w    *sql.DB // single writer connection
	r    *sql.DB // read-only pool

	appIDs map[string]int64 // owned by the writer
}

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
	}
	return "file:" + path + "?" + q.Encode()
}

// Open opens (and creates or migrates) the database at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if _, err := w.Exec(schema); err != nil {
		w.Close()
		return nil, fmt.Errorf("store: create schema: %w", err)
	}
	if err := migrate(w); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	return &Store{path: path, w: w, r: r, appIDs: map[string]int64{}}, nil
}

func migrate(db *sql.DB) error {
	var v string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = db.Exec(`INSERT INTO meta(key, value) VALUES ('schema_version', ?)`, strconv.Itoa(schemaVersion))
		return err
	case err != nil:
		return err
	}
	if n, _ := strconv.Atoi(v); n > schemaVersion {
		return fmt.Errorf("store: database schema %d is newer than this binary (%d)", n, schemaVersion)
	}
	return nil
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Close closes both pools.
func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

func nullF(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// appSampleLimit bounds how many apps are stored per second. Busy apps are
// always stored; idle ones are only kept while they are among the largest
// by memory, so a daily memory report still sees them.
const (
	appsByCPU = 20
	appsByRSS = 10
)

func keepApps(s *model.Snapshot) []*model.App {
	byRSS := make([]*model.App, len(s.Apps))
	for i := range s.Apps {
		byRSS[i] = &s.Apps[i]
	}
	sort.Slice(byRSS, func(i, j int) bool { return byRSS[i].RSS > byRSS[j].RSS })
	keep := map[*model.App]bool{}
	for i := 0; i < len(byRSS) && i < appsByRSS; i++ {
		keep[byRSS[i]] = true
	}
	var out []*model.App
	for i := range s.Apps { // Apps is sorted by CPU
		a := &s.Apps[i]
		active := (i < appsByCPU && a.CPU > 0) || a.DiskRead > 0 || a.DiskWrite > 0 || a.NetRx > 0 || a.NetTx > 0
		if active || keep[a] {
			out = append(out, a)
		}
	}
	return out
}

// WriteSnapshots stores a batch of snapshots in one transaction.
func (s *Store) WriteSnapshots(ctx context.Context, snaps []*model.Snapshot) error {
	if len(snaps) == 0 {
		return nil
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	sysStmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO sys_samples
		(ts, dt, cpu, mem_used, mem_total, disk_r, disk_w, net_rx, net_tx, batt_pct, batt_w, batt_status, temp_c)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	appStmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO app_samples
		(ts, app_id, cpu, rss, disk_r, disk_w, net_rx, net_tx, procs, sockets)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	lastSeen := map[int64]int64{}
	for _, snap := range snaps {
		ts := snap.TS.Unix()
		sys := snap.Sys
		dt := snap.Elapsed.Seconds()
		if dt <= 0 {
			dt = 1
		}
		var bp, bw any
		var bs any
		if b := sys.Battery; b != nil {
			bp, bs = b.Percent, b.Status
			if b.HasPower {
				bw = b.Watts
			}
		}
		if _, err := sysStmt.ExecContext(ctx, ts, dt, sys.CPU, sys.MemUsed, sys.MemTotal, sys.DiskRead, sys.DiskWrite,
			sys.NetRx, sys.NetTx, bp, bw, bs, nullF(sys.TempC)); err != nil {
			return err
		}
		for _, a := range keepApps(snap) {
			id, err := s.appID(ctx, tx, a.Name, ts)
			if err != nil {
				return err
			}
			lastSeen[id] = ts
			if _, err := appStmt.ExecContext(ctx, ts, id, a.CPU, a.RSS, a.DiskRead, a.DiskWrite, a.NetRx, a.NetTx,
				a.Procs, a.Sockets); err != nil {
				return err
			}
		}
	}
	for id, ts := range lastSeen {
		if _, err := tx.ExecContext(ctx, `UPDATE apps SET last_seen = ? WHERE id = ?`, ts, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		// A failed transaction may have inserted app rows that were rolled back.
		s.appIDs = map[string]int64{}
		return err
	}
	return nil
}

func (s *Store) appID(ctx context.Context, tx *sql.Tx, name string, ts int64) (int64, error) {
	if id, ok := s.appIDs[name]; ok {
		return id, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO apps(name, first_seen, last_seen) VALUES (?, ?, ?)`, name, ts, ts); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM apps WHERE name = ?`, name).Scan(&id); err != nil {
		return 0, err
	}
	s.appIDs[name] = id
	return id, nil
}

func unixF(t time.Time) float64 { return float64(t.UnixMilli()) / 1000 }

func fromUnixF(f float64) time.Time { return time.UnixMilli(int64(math.Round(f * 1000))) }

// InsertMystery stores a mystery and sets its ID.
func (s *Store) InsertMystery(ctx context.Context, m *model.Mystery) error {
	culprits, err := json.Marshal(nonNil(m.Culprits))
	if err != nil {
		return err
	}
	newcomers, err := json.Marshal(nonNil(m.Newcomers))
	if err != nil {
		return err
	}
	top, share := m.TopApp()
	var topApp, topShare any
	if top != "" {
		topApp, topShare = top, share
	}
	res, err := s.w.ExecContext(ctx, `INSERT INTO mysteries
		(metric, started_at, ended_at, baseline, threshold, peak, mean, ratio, top_app, top_share,
		 culprits_json, newcomers_json, summary, truncated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(m.Metric), unixF(m.Start), unixF(m.End), m.Baseline, m.Threshold, m.Peak, m.Mean, m.Ratio,
		topApp, topShare, string(culprits), string(newcomers), m.Summary, m.Truncated)
	if err != nil {
		return err
	}
	m.ID, err = res.LastInsertId()
	return err
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// InsertBatteryWindow stores a battery window.
func (s *Store) InsertBatteryWindow(ctx context.Context, w *model.BatteryWindow) error {
	attr, err := json.Marshal(nonNil(w.Apps))
	if err != nil {
		return err
	}
	_, err = s.w.ExecContext(ctx, `INSERT OR REPLACE INTO battery_windows
		(start, end, drain_wh, drain_pct, avg_w, attribution_json) VALUES (?, ?, ?, ?, ?, ?)`,
		w.Start.Unix(), w.End.Unix(), w.DrainWh, w.DrainPct, w.AvgWatts, string(attr))
	return err
}

// Rollup aggregates every complete minute before now that is not yet in
// the minute tables. It is idempotent and cheap to call every minute.
func (s *Store) Rollup(ctx context.Context, now time.Time) error {
	var fromS string
	err := s.w.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'rollup_until'`).Scan(&fromS)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	from, _ := strconv.ParseInt(fromS, 10, 64)
	if from == 0 {
		if err := s.w.QueryRowContext(ctx, `SELECT COALESCE(MIN(ts), 0) FROM sys_samples`).Scan(&from); err != nil {
			return err
		}
		from = from / 60 * 60
	}
	to := now.Unix() / 60 * 60
	if from == 0 || to <= from {
		return nil
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO sys_samples_1m
		SELECT ts / 60 * 60, COUNT(*), AVG(cpu), MAX(cpu), AVG(mem_used),
		       AVG(disk_r), MAX(disk_r), AVG(disk_w), MAX(disk_w),
		       AVG(net_rx), MAX(net_rx), AVG(net_tx), MAX(net_tx), AVG(batt_pct), AVG(temp_c)
		FROM sys_samples WHERE ts >= ? AND ts < ? GROUP BY ts / 60`, from, to); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO app_samples_1m
		SELECT a.ts / 60 * 60, a.app_id, SUM(a.cpu / 100 * s.dt), MAX(a.rss),
		       SUM(a.disk_r * s.dt), SUM(a.disk_w * s.dt), SUM(a.net_rx * s.dt), SUM(a.net_tx * s.dt)
		FROM app_samples a JOIN sys_samples s ON s.ts = a.ts
		WHERE a.ts >= ? AND a.ts < ? GROUP BY a.ts / 60, a.app_id`, from, to); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES ('rollup_until', ?)`,
		strconv.FormatInt(to, 10)); err != nil {
		return err
	}
	return tx.Commit()
}

// Retention says how long each table keeps rows.
type Retention struct {
	SysRaw  time.Duration // 24 h
	AppRaw  time.Duration // 6 h
	Minutes time.Duration // 30 days
}

// DefaultRetention returns the defaults.
func DefaultRetention() Retention {
	return Retention{SysRaw: 24 * time.Hour, AppRaw: 6 * time.Hour, Minutes: 30 * 24 * time.Hour}
}

// Retain deletes rows older than the retention. Raw rows are only deleted
// once they are rolled up.
func (s *Store) Retain(ctx context.Context, now time.Time, r Retention) error {
	var untilS string
	if err := s.w.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'rollup_until'`).Scan(&untilS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	until, _ := strconv.ParseInt(untilS, 10, 64)
	stmts := []struct {
		q   string
		arg int64
	}{
		{`DELETE FROM app_samples WHERE ts < ?`, min(now.Add(-r.AppRaw).Unix(), until)},
		{`DELETE FROM sys_samples WHERE ts < ?`, min(now.Add(-r.SysRaw).Unix(), until)},
		{`DELETE FROM app_samples_1m WHERE ts < ?`, now.Add(-r.Minutes).Unix()},
		{`DELETE FROM sys_samples_1m WHERE ts < ?`, now.Add(-r.Minutes).Unix()},
	}
	for _, st := range stmts {
		if _, err := s.w.ExecContext(ctx, st.q, st.arg); err != nil {
			return err
		}
	}
	return nil
}
