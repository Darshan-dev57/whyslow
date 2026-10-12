package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/model"
	"github.com/Darshan-dev57/whyslow/internal/pipeline"
	"github.com/Darshan-dev57/whyslow/internal/store"
)

var t0 = time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)

// fixture: a store with three minutes of samples in which "builder" burns
// two cores from 60 s to 90 s, and one mystery about it.
func fixture(t *testing.T) (*Server, *Hub, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var snaps []*model.Snapshot
	for i := 0; i < 180; i++ {
		s := &model.Snapshot{TS: t0.Add(time.Duration(i) * time.Second), Elapsed: time.Second,
			Sys: model.System{CPU: 5, NumCPU: 4, MemUsed: 2e9, MemTotal: 8e9}}
		s.Apps = []model.App{{Name: "editor", CPU: 10, RSS: 100e6, Procs: 1}}
		if i >= 60 && i < 90 {
			s.Sys.CPU = 55
			s.Apps = append(s.Apps, model.App{Name: "builder", CPU: 200, RSS: 50e6, DiskWrite: 1e6, Procs: 2})
		}
		snaps = append(snaps, s)
	}
	if err := st.WriteSnapshots(context.Background(), snaps); err != nil {
		t.Fatal(err)
	}
	m := &model.Mystery{Metric: model.CPU, Start: t0.Add(60 * time.Second), End: t0.Add(90 * time.Second),
		Baseline: 5, Threshold: 20, Peak: 55, Mean: 55, Ratio: 11, Summary: "CPU spiked 11x",
		Culprits:  []model.Culprit{{App: "builder", Share: 0.95, Before: 0, During: 200, Newcomer: true}},
		Newcomers: []model.Newcomer{{App: "builder", StartedAgo: 2}}}
	if err := st.InsertMystery(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	srv := New(Options{Store: st, Hub: hub, Version: "test", Interval: time.Second,
		Net: NetStatus{Mode: "proc", Reason: "not root"}, Now: func() time.Time { return t0.Add(3 * time.Minute) }})
	return srv, hub, st
}

func get(t *testing.T, h http.Handler, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", url, nil)
	req.Host = "localhost:7777"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestStatus(t *testing.T) {
	srv, hub, _ := fixture(t)
	st := decode[statusResponse](t, get(t, srv, "/api/status"))
	if st.Version != "test" || st.Net.Mode != "proc" || st.Net.Reason != "not root" || st.Sampling || st.IntervalS != 1 {
		t.Errorf("status before any frame = %+v", st)
	}
	temp := 61.0
	hub.Frame(&pipeline.Frame{Snapshot: &model.Snapshot{TS: t0, Sys: model.System{TempC: &temp, NumCPU: 4}}})
	st = decode[statusResponse](t, get(t, srv, "/api/status"))
	if !st.Sampling || !st.Thermal || st.Battery {
		t.Errorf("status after a frame = %+v", st)
	}
}

func TestTimeline(t *testing.T) {
	srv, _, _ := fixture(t)
	from, to := t0.Unix(), t0.Add(179*time.Second).Unix()
	rec := get(t, srv, "/api/timeline?metric=cpu&from="+itoa(from)+"&to="+itoa(to))
	if rec.Code != 200 {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	s := decode[series](t, rec)
	if len(s.TS) != 180 || len(s.V) != 180 || s.V[0] != 5 || s.V[70] != 55 {
		t.Errorf("system series: %d points, v[0]=%v v[70]=%v", len(s.TS), s.V[0], s.V[70])
	}
	a := decode[series](t, get(t, srv, "/api/timeline?metric=cpu&app=builder&from="+itoa(from)+"&to="+itoa(to)))
	if a.App != "builder" || a.V[0] != 0 || a.V[70] != 200 {
		t.Errorf("app series: %+v", a.V[:3])
	}
	for _, bad := range []string{"/api/timeline?metric=bogus", "/api/timeline?metric=cpu&from=x",
		"/api/timeline?metric=cpu&from=200&to=100", "/api/timeline?metric=cpu&from=0&to=99999999"} {
		if rec := get(t, srv, bad); rec.Code != 400 || !strings.Contains(rec.Body.String(), "error") {
			t.Errorf("%s: code %d", bad, rec.Code)
		}
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func TestApps(t *testing.T) {
	srv, _, _ := fixture(t)
	apps := decode[[]store.AppAvg](t, get(t, srv, "/api/apps?window=3m"))
	if len(apps) != 2 || apps[0].Name != "builder" {
		t.Fatalf("apps = %+v", apps)
	}
	if rec := get(t, srv, "/api/apps?window=48h"); rec.Code != 400 {
		t.Errorf("window over the limit: %d", rec.Code)
	}
}

func TestMysteries(t *testing.T) {
	srv, _, _ := fixture(t)
	ms := decode[[]model.Mystery](t, get(t, srv, "/api/mysteries?since=1h&limit=10"))
	if len(ms) != 1 || ms[0].ID != 1 || ms[0].Culprits[0].App != "builder" {
		t.Fatalf("mysteries = %+v", ms)
	}
	if rec := get(t, srv, "/api/mysteries?limit=0"); rec.Code != 400 {
		t.Errorf("limit 0: %d", rec.Code)
	}
	// an empty range is an empty list, not null
	if body := get(t, srv, "/api/mysteries?since=1s").Body.String(); strings.TrimSpace(body) != "[]" {
		t.Errorf("empty feed = %q", body)
	}
}

func TestMysteryDetail(t *testing.T) {
	srv, hub, _ := fixture(t)
	hub.Frame(&pipeline.Frame{Snapshot: &model.Snapshot{TS: t0, Sys: model.System{NumCPU: 4}}})
	d := decode[mysteryDetail](t, get(t, srv, "/api/mysteries/1"))
	if d.Mystery.ID != 1 || len(d.System.TS) == 0 || len(d.Apps) != 1 {
		t.Fatalf("detail = %+v", d)
	}
	// the window is the spike +-2 min, clipped to the data: 0..179 s
	if d.System.TS[0] != float64(t0.Unix()) || len(d.System.TS) != 180 {
		t.Errorf("window: first %v, %d points", d.System.TS[0], len(d.System.TS))
	}
	// builder's 200% of one core is 50% of a 4-core machine on the system axis
	if v := d.Apps[0].V[70]; v != 50 {
		t.Errorf("scaled app cpu = %v, want 50", v)
	}
	if rec := get(t, srv, "/api/mysteries/99"); rec.Code != 404 {
		t.Errorf("missing: %d", rec.Code)
	}
	if rec := get(t, srv, "/api/mysteries/abc"); rec.Code != 400 {
		t.Errorf("bad id: %d", rec.Code)
	}
}

func TestReport(t *testing.T) {
	srv, _, _ := fixture(t)
	rec := get(t, srv, "/api/report?date=2026-10-12")
	if rec.Code != 200 {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	var r struct {
		Date      string `json:"date"`
		Watched   int64  `json:"watched_ns"`
		Mysteries []any  `json:"mysteries"`
		Sections  []struct {
			Resource string `json:"resource"`
			Apps     []struct {
				App   string  `json:"app"`
				Value float64 `json:"value"`
			} `json:"apps"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Date != "2026-10-12" || r.Watched != int64(180*time.Second) || len(r.Mysteries) != 1 {
		t.Errorf("report = %+v", r)
	}
	if s := r.Sections[0]; s.Resource != "cpu" || s.Apps[0].App != "builder" || s.Apps[0].Value != 60 {
		t.Errorf("cpu section = %+v", s)
	}
	if rec := get(t, srv, "/api/report?date=12-10-2026"); rec.Code != 400 {
		t.Errorf("bad date: %d", rec.Code)
	}
}

func TestStaticAndHeaders(t *testing.T) {
	srv, _, _ := fixture(t)
	for _, path := range []string{"/", "/app.js", "/style.css", "/vendor/uPlot.iife.min.js"} {
		rec := get(t, srv, path)
		if rec.Code != 200 || rec.Body.Len() == 0 {
			t.Errorf("%s: code %d", path, rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'self'") {
			t.Errorf("%s: no CSP", path)
		}
	}
	if rec := get(t, srv, "/api/nope"); rec.Code != 404 {
		t.Errorf("unknown endpoint: %d", rec.Code)
	}
}

func TestHostCheck(t *testing.T) {
	srv, _, st := fixture(t)
	cases := map[string]int{"localhost:7777": 200, "127.0.0.1:7777": 200, "[::1]:7777": 200, "localhost": 200,
		"evil.example:7777": 403, "192.168.1.5:7777": 403, "127.0.0.1.evil.example": 403}
	for host, want := range cases {
		req := httptest.NewRequest("GET", "/api/status", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q: %d, want %d", host, rec.Code, want)
		}
	}
	open := New(Options{Store: st, AllowAnyHost: true})
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Host = "192.168.1.5:7777"
	rec := httptest.NewRecorder()
	open.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("AllowAnyHost: %d", rec.Code)
	}
}

// readEvent reads one SSE event (name and data) and skips comments.
func readEvent(t *testing.T, r *bufio.Reader) (string, string) {
	t.Helper()
	var name, data string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "" && (name != "" || data != ""):
			return name, data
		case strings.HasPrefix(line, "event: "):
			name = line[7:]
		case strings.HasPrefix(line, "data: "):
			data = line[6:]
		}
	}
}

func TestLiveStream(t *testing.T) {
	srv, hub, _ := fixture(t)
	hub.Frame(&pipeline.Frame{Snapshot: &model.Snapshot{TS: t0, Sys: model.System{CPU: 12, NumCPU: 4},
		Apps: []model.App{{Name: "editor", CPU: 10}}}})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/live", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	r := bufio.NewReader(resp.Body)

	name, data := readEvent(t, r)
	var h history
	if name != "history" || json.Unmarshal([]byte(data), &h) != nil || len(h.CPU) != 1 || h.CPU[0] != 12 {
		t.Fatalf("first event %s %s", name, data)
	}
	name, data = readEvent(t, r)
	var f liveFrame
	if name != "frame" || json.Unmarshal([]byte(data), &f) != nil || f.Apps[0].Name != "editor" || f.Open == nil {
		t.Fatalf("second event %s %s", name, data)
	}

	waitFor(t, func() bool { return hub.Clients() == 1 })
	hub.Frame(&pipeline.Frame{Snapshot: &model.Snapshot{TS: t0.Add(time.Second), Sys: model.System{CPU: 80}},
		Open: []pipeline.OpenSpike{{Metric: model.CPU, Onset: t0, Threshold: 20, Peak: 80}}})
	name, data = readEvent(t, r)
	if name != "frame" || !strings.Contains(data, `"open":[{"metric":"cpu"`) {
		t.Errorf("live frame %s %s", name, data)
	}
	hub.Mystery(&model.Mystery{ID: 7, Metric: model.CPU, Summary: "CPU spiked"})
	if name, data = readEvent(t, r); name != "mystery" || !strings.Contains(data, `"id":7`) {
		t.Errorf("mystery event %s %s", name, data)
	}
	cancel()
	waitFor(t, func() bool { return hub.Clients() == 0 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in 2 s")
}

// A client that stops reading must cost the pipeline nothing: frames are
// dropped for it, and after maxBehind drops in a row it is disconnected.
func TestSlowClientIsDropped(t *testing.T) {
	hub := NewHub()
	sub, _, _ := hub.subscribe()
	start := time.Now()
	for i := 0; i < subBuffer+maxBehind; i++ {
		hub.Frame(&pipeline.Frame{Snapshot: &model.Snapshot{TS: t0.Add(time.Duration(i) * time.Second)}})
	}
	if time.Since(start) > time.Second {
		t.Error("broadcasting to a stuck client blocked")
	}
	select {
	case <-sub.gone:
	default:
		t.Fatal("stuck client still subscribed")
	}
	if hub.Clients() != 0 || hub.dropped.Load() != maxBehind || hub.kicked.Load() != 1 {
		t.Errorf("clients %d, dropped %d, kicked %d", hub.Clients(), hub.dropped.Load(), hub.kicked.Load())
	}
	// history keeps only the newest historySize points
	for i := 0; i < historySize+10; i++ {
		hub.Frame(&pipeline.Frame{Snapshot: &model.Snapshot{TS: t0.Add(time.Duration(i) * time.Second)}})
	}
	_, hist, _ := hub.subscribe()
	var h history
	if err := json.Unmarshal(hist, &h); err != nil || len(h.TS) != historySize || h.TS[0] != float64(t0.Add(10*time.Second).Unix()) {
		t.Errorf("history: %d points, first %v", len(h.TS), h.TS[0])
	}
}
