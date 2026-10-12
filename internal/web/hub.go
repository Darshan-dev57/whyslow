package web

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/Darshan-dev57/whyslow/internal/model"
	"github.com/Darshan-dev57/whyslow/internal/pipeline"
)

// event is one Server-Sent Event, encoded once and shared by every client.
type event struct {
	name string
	data []byte
}

// subscriber is one connected browser. Its channel is small on purpose: a
// client that cannot keep up loses frames instead of slowing the pipeline,
// and one that stays behind is disconnected (EventSource reconnects).
type subscriber struct {
	ch     chan event
	behind int // frames dropped in a row
	gone   chan struct{}
}

const (
	subBuffer   = 8
	maxBehind   = 30 // about 30 s of frames at the default interval
	historySize = 300
)

// liveFrame is what the Live page receives every tick.
type liveFrame struct {
	TS        float64              `json:"ts"` // unix seconds
	Sys       model.System         `json:"sys"`
	Apps      []model.App          `json:"apps"`
	Open      []pipeline.OpenSpike `json:"open"`
	NetPerApp bool                 `json:"net_per_app"`
}

// history holds recent system values in columns, the shape uPlot wants.
type history struct {
	TS        []float64 `json:"ts"`
	CPU       []float64 `json:"cpu"`
	Mem       []float64 `json:"mem"`
	DiskRead  []float64 `json:"disk_read"`
	DiskWrite []float64 `json:"disk_write"`
	NetRx     []float64 `json:"net_rx"`
	NetTx     []float64 `json:"net_tx"`
}

func (h *history) push(ts float64, s *model.System) {
	cols := []*[]float64{&h.TS, &h.CPU, &h.Mem, &h.DiskRead, &h.DiskWrite, &h.NetRx, &h.NetTx}
	vals := []float64{ts, s.CPU, float64(s.MemUsed), s.DiskRead, s.DiskWrite, s.NetRx, s.NetTx}
	for i, c := range cols {
		*c = append(*c, vals[i])
		if len(*c) > historySize {
			*c = append((*c)[:0], (*c)[len(*c)-historySize:]...)
		}
	}
}

// Hub fans pipeline output out to live clients. It implements
// pipeline.Sink: Frame and Mystery are called from the pipeline goroutine,
// so they encode once, never block, and return.
type Hub struct {
	// MaxApps caps how many apps each frame carries (the busiest by CPU).
	MaxApps int

	mu      sync.Mutex
	subs    map[*subscriber]struct{}
	last    *event
	hist    history
	lastSys *model.System
	netApp  bool

	dropped atomic.Int64
	kicked  atomic.Int64
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{MaxApps: 40, subs: map[*subscriber]struct{}{}}
}

// Frame broadcasts one tick.
func (h *Hub) Frame(f *pipeline.Frame) {
	s := f.Snapshot
	apps := s.Apps
	if len(apps) > h.MaxApps {
		apps = apps[:h.MaxApps]
	}
	open := f.Open
	if open == nil {
		open = []pipeline.OpenSpike{}
	}
	ts := float64(s.TS.UnixMilli()) / 1000
	data, err := json.Marshal(liveFrame{TS: ts, Sys: s.Sys, Apps: apps, Open: open, NetPerApp: s.NetPerApp})
	if err != nil {
		return
	}
	ev := event{name: "frame", data: data}
	sys := s.Sys

	h.mu.Lock()
	defer h.mu.Unlock()
	h.last = &ev
	h.lastSys = &sys
	h.netApp = s.NetPerApp
	h.hist.push(ts, &s.Sys)
	h.broadcast(ev, true)
}

// Mystery tells clients a spike was explained. The store sink runs first,
// so the mystery already has its ID.
func (h *Hub) Mystery(m *model.Mystery) {
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broadcast(event{name: "mystery", data: data}, false)
}

// Battery is part of pipeline.Sink; battery windows appear on the report.
func (h *Hub) Battery(*model.BatteryWindow) {}

// broadcast must be called with mu held.
func (h *Hub) broadcast(ev event, droppable bool) {
	for s := range h.subs {
		select {
		case s.ch <- ev:
			if droppable {
				s.behind = 0
			}
		default:
			h.dropped.Add(1)
			s.behind++
			if !droppable || s.behind >= maxBehind {
				// a lost mystery event or a client this far behind: hang up
				// and let it reconnect to a fresh stream
				h.removeLocked(s)
				h.kicked.Add(1)
			}
		}
	}
}

// subscribe registers a client and returns the history and the latest
// frame so the page draws at once instead of waiting a tick.
func (h *Hub) subscribe() (*subscriber, []byte, *event) {
	s := &subscriber{ch: make(chan event, subBuffer), gone: make(chan struct{})}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[s] = struct{}{}
	hist, _ := json.Marshal(h.hist)
	return s, hist, h.last
}

func (h *Hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeLocked(s)
}

func (h *Hub) removeLocked(s *subscriber) {
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.gone)
	}
}

// Clients returns the number of connected live clients.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// lastSystem returns the latest system state, or nil before the first tick.
func (h *Hub) lastSystem() (*model.System, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastSys, h.netApp
}
