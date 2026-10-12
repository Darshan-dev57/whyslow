// Package web serves the dashboard and its JSON API from the same binary.
//
// The live page uses Server-Sent Events rather than WebSockets: data only
// flows from server to browser, EventSource reconnects on its own, and SSE
// is plain HTTP, so it needs no extra library.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/model"
	"github.com/Darshan-dev57/whyslow/internal/report"
	"github.com/Darshan-dev57/whyslow/internal/store"
)

//go:embed static
var staticFiles embed.FS

// NetStatus says where per-app network numbers come from.
type NetStatus struct {
	Mode   string `json:"mode"`             // "ebpf" or "proc"
	Reason string `json:"reason,omitempty"` // why eBPF is off, when it is
}

// Options configures a Server.
type Options struct {
	Store    *store.Store
	Hub      *Hub
	Version  string
	Interval time.Duration
	Net      NetStatus
	Log      *slog.Logger
	Now      func() time.Time
	// AllowAnyHost turns off the Host header check. The check stops other
	// websites from reading the API through DNS rebinding; it is only
	// needed while the server listens on loopback.
	AllowAnyHost bool
}

// Server is the HTTP side of whyslow.
type Server struct {
	opts    Options
	started time.Time
	mux     *http.ServeMux
}

// New builds the server and its routes.
func New(opts Options) *Server {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Hub == nil {
		opts.Hub = NewHub()
	}
	s := &Server{opts: opts, started: opts.Now(), mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/live", s.live)
	s.mux.HandleFunc("GET /api/status", s.status)
	s.mux.HandleFunc("GET /api/timeline", s.timeline)
	s.mux.HandleFunc("GET /api/apps", s.apps)
	s.mux.HandleFunc("GET /api/mysteries", s.mysteries)
	s.mux.HandleFunc("GET /api/mysteries/{id}", s.mystery)
	s.mux.HandleFunc("GET /api/report", s.report)
	s.mux.HandleFunc("GET /api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err) // the embed path is fixed at compile time
	}
	s.mux.Handle("GET /", http.FileServerFS(sub))
	return s
}

// ServeHTTP adds security headers and the Host check, then routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
	if !s.opts.AllowAnyHost && !localHost(r.Host) {
		writeError(w, http.StatusForbidden, "host not allowed; whyslow only answers to localhost")
		return
	}
	s.mux.ServeHTTP(w, r)
}

func localHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Serve listens on addr until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: the live stream stays open. Other handlers are
		// short database reads bounded by their request contexts.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := srv.Shutdown(shut)
		if errors.Is(err, context.DeadlineExceeded) {
			err = srv.Close()
		}
		return err
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// headers are gone; nothing useful to tell the client
		return
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Context().Err() != nil {
		return // the client went away
	}
	s.opts.Log.Error("api", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// live streams frames as Server-Sent Events.
func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")

	sub, hist, last := s.opts.Hub.subscribe()
	defer s.opts.Hub.unsubscribe(sub)

	send := func(name string, data []byte) error {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
			return err
		}
		return rc.Flush()
	}
	if _, err := fmt.Fprint(w, "retry: 2000\n\n"); err != nil {
		return
	}
	if send("history", hist) != nil {
		return
	}
	if last != nil && send(last.name, last.data) != nil {
		return
	}
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.gone:
			return
		case ev := <-sub.ch:
			if send(ev.name, ev.data) != nil {
				return
			}
		case <-beat.C:
			// a comment keeps proxies and idle timers from closing the stream
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

type statusResponse struct {
	Version   string    `json:"version"`
	Started   time.Time `json:"started"`
	UptimeS   float64   `json:"uptime_s"`
	IntervalS float64   `json:"interval_s"`
	Net       NetStatus `json:"net"`
	Battery   bool      `json:"battery"`
	Thermal   bool      `json:"thermal"`
	Sampling  bool      `json:"sampling"` // at least one frame has arrived
	Clients   int       `json:"clients"`
	Dropped   int64     `json:"dropped_frames"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	sys, _ := s.opts.Hub.lastSystem()
	resp := statusResponse{
		Version: s.opts.Version, Started: s.started, UptimeS: s.opts.Now().Sub(s.started).Seconds(),
		IntervalS: s.opts.Interval.Seconds(), Net: s.opts.Net, Sampling: sys != nil,
		Clients: s.opts.Hub.Clients(), Dropped: s.opts.Hub.dropped.Load(),
	}
	if sys != nil {
		resp.Battery = sys.Battery != nil
		resp.Thermal = sys.TempC != nil
	}
	writeJSON(w, resp)
}

// parseTime accepts unix seconds or RFC 3339.
func parseTime(v string, def time.Time) (time.Time, error) {
	if v == "" {
		return def, nil
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return time.UnixMilli(int64(f * 1000)), nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad time %q: use unix seconds or RFC 3339", v)
	}
	return t, nil
}

const maxRange = 31 * 24 * time.Hour

type series struct {
	Metric model.Metric `json:"metric"`
	App    string       `json:"app,omitempty"`
	TS     []float64    `json:"ts"`
	V      []float64    `json:"v"`
}

func toSeries(m model.Metric, app string, pts []store.Point) series {
	out := series{Metric: m, App: app, TS: make([]float64, len(pts)), V: make([]float64, len(pts))}
	for i, p := range pts {
		out.TS[i] = float64(p.TS.Unix())
		out.V[i] = p.V
	}
	return out
}

func (s *Server) timeline(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	metric := model.Metric(q.Get("metric"))
	if !metric.Valid() {
		writeError(w, http.StatusBadRequest, "metric must be one of cpu, mem, disk_read, disk_write, net_rx, net_tx")
		return
	}
	now := s.opts.Now()
	to, err := parseTime(q.Get("to"), now)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	from, err := parseTime(q.Get("from"), to.Add(-5*time.Minute))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !from.Before(to) || to.Sub(from) > maxRange {
		writeError(w, http.StatusBadRequest, "from must be before to, at most 31 days apart")
		return
	}
	app := q.Get("app")
	pts, err := s.opts.Store.Timeline(r.Context(), metric, from, to, app)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, toSeries(metric, app, pts))
}

func parseWindow(v string, def, limit time.Duration) (time.Duration, error) {
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 || d > limit {
		return 0, fmt.Errorf("bad duration %q: use e.g. 5m or 24h, at most %s", v, limit)
	}
	return d, nil
}

func (s *Server) apps(w http.ResponseWriter, r *http.Request) {
	win, err := parseWindow(r.URL.Query().Get("window"), 5*time.Minute, 24*time.Hour)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := s.opts.Now()
	avgs, err := s.opts.Store.AppAverages(r.Context(), now.Add(-win), now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, avgs)
}

func (s *Server) mysteries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, err := parseWindow(q.Get("since"), 24*time.Hour, maxRange)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := 100
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 1000 {
			writeError(w, http.StatusBadRequest, "limit must be 1..1000")
			return
		}
	}
	now := s.opts.Now()
	ms, err := s.opts.Store.Mysteries(r.Context(), now.Add(-since), now.Add(time.Minute), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if ms == nil {
		ms = []*model.Mystery{}
	}
	writeJSON(w, ms)
}

// detailPad is how much context the detail chart shows around a spike.
const detailPad = 2 * time.Minute

type mysteryDetail struct {
	Mystery *model.Mystery `json:"mystery"`
	System  series         `json:"system"`
	Apps    []series       `json:"apps"` // the top culprits, aligned to System.TS
}

func (s *Server) mystery(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "bad mystery id")
		return
	}
	ctx := r.Context()
	m, err := s.opts.Store.Mystery(ctx, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	from, to := m.Start.Add(-detailPad), m.End.Add(detailPad)
	pts, err := s.opts.Store.Timeline(ctx, m.Metric, from, to, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := mysteryDetail{Mystery: m, System: toSeries(m.Metric, "", pts), Apps: []series{}}
	for i, c := range m.Culprits {
		if i == 3 {
			break
		}
		ap, err := s.opts.Store.Timeline(ctx, m.Metric, from, to, c.App)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		as := toSeries(m.Metric, c.App, ap)
		if m.Metric == model.CPU {
			// App CPU is per core (200 = two cores); the system's is a share
			// of all cores. Scale so both fit one axis.
			n := float64(s.numCPU())
			for i := range as.V {
				as.V[i] /= n
			}
		}
		d.Apps = append(d.Apps, as)
	}
	writeJSON(w, d)
}

func (s *Server) numCPU() int {
	if sys, _ := s.opts.Hub.lastSystem(); sys != nil && sys.NumCPU > 0 {
		return sys.NumCPU
	}
	return runtime.NumCPU()
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	day := s.opts.Now()
	if v := r.URL.Query().Get("date"); v != "" {
		d, err := time.ParseInLocation("2006-01-02", v, day.Location())
		if err != nil {
			writeError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
			return
		}
		day = d
	}
	rep, err := report.Build(r.Context(), s.opts.Store, day, report.DefaultConfig())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, rep)
}
