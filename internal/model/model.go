// Package model holds the types shared by the collector, the detector, the
// blame engine, the store and the web API.
package model

import (
	"fmt"
	"time"
)

// Metric names one system-level series that whyslow watches for spikes.
type Metric string

// The watched metrics.
const (
	CPU       Metric = "cpu"
	Mem       Metric = "mem"
	DiskRead  Metric = "disk_read"
	DiskWrite Metric = "disk_write"
	NetRx     Metric = "net_rx"
	NetTx     Metric = "net_tx"
)

// Metrics lists every watched metric in display order.
var Metrics = []Metric{CPU, Mem, DiskRead, DiskWrite, NetRx, NetTx}

// Valid reports whether m is one of Metrics.
func (m Metric) Valid() bool {
	for _, x := range Metrics {
		if x == m {
			return true
		}
	}
	return false
}

// Label is the human name used in sentences and the UI.
func (m Metric) Label() string {
	switch m {
	case CPU:
		return "CPU"
	case Mem:
		return "Memory"
	case DiskRead:
		return "Disk read"
	case DiskWrite:
		return "Disk write"
	case NetRx:
		return "Network download"
	case NetTx:
		return "Network upload"
	}
	return string(m)
}

// IsNet reports whether the metric needs per-app network data to blame.
func (m Metric) IsNet() bool { return m == NetRx || m == NetTx }

// Format renders a value of this metric for people: 37%, 2.1 GB, 12.6 MB/s.
// System CPU is a percentage of all cores.
func (m Metric) Format(v float64) string {
	switch m {
	case CPU:
		return fmt.Sprintf("%.0f%%", v)
	case Mem:
		return Bytes(v)
	default:
		return Bytes(v) + "/s"
	}
}

// Bytes renders a byte count with decimal units, the way disk and network
// tools usually do.
func Bytes(v float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	switch {
	case i == 0:
		return fmt.Sprintf("%.0f B", v)
	case v >= 100:
		return fmt.Sprintf("%.0f %s", v, units[i])
	default:
		return fmt.Sprintf("%.1f %s", v, units[i])
	}
}

// Battery is the battery state at one sample.
type Battery struct {
	Percent   float64 `json:"pct"`
	Status    string  `json:"status"`
	Watts     float64 `json:"watts"`
	EnergyWh  float64 `json:"energy_wh"`
	HasPower  bool    `json:"has_power"`
	HasEnergy bool    `json:"has_energy"`
}

// Discharging reports whether the machine is running on battery.
func (b *Battery) Discharging() bool { return b != nil && b.Status == "Discharging" }

// System is the whole-machine state at one sample. Rates are bytes/second
// averaged over the interval since the previous sample.
type System struct {
	CPU       float64  `json:"cpu"` // percent of all cores, 0..100
	NumCPU    int      `json:"ncpu"`
	MemUsed   uint64   `json:"mem_used"`
	MemTotal  uint64   `json:"mem_total"`
	DiskRead  float64  `json:"disk_read"`
	DiskWrite float64  `json:"disk_write"`
	NetRx     float64  `json:"net_rx"`
	NetTx     float64  `json:"net_tx"`
	Battery   *Battery `json:"battery,omitempty"`
	TempC     *float64 `json:"temp_c,omitempty"`
}

// Value returns the system value for a metric.
func (s *System) Value(m Metric) float64 {
	switch m {
	case CPU:
		return s.CPU
	case Mem:
		return float64(s.MemUsed)
	case DiskRead:
		return s.DiskRead
	case DiskWrite:
		return s.DiskWrite
	case NetRx:
		return s.NetRx
	case NetTx:
		return s.NetTx
	}
	return 0
}

// App is one application (a group of processes) at one sample.
type App struct {
	Name      string    `json:"name"`
	CPU       float64   `json:"cpu"` // percent of one core; 250 means 2.5 cores busy
	RSS       uint64    `json:"rss"`
	DiskRead  float64   `json:"disk_read"`
	DiskWrite float64   `json:"disk_write"`
	NetRx     float64   `json:"net_rx"`
	NetTx     float64   `json:"net_tx"`
	Procs     int       `json:"procs"`
	Sockets   int       `json:"sockets"`
	Started   time.Time `json:"started"`    // start of the app's oldest live process
	IOHidden  int       `json:"io_hidden"`  // processes whose /proc/[pid]/io was unreadable
	NetActive bool      `json:"net_active"` // per-app network numbers are real (eBPF)
}

// Value returns the app's share of a metric, in the same unit family as the
// system value (CPU is per-core here; blame only compares apps with apps).
func (a *App) Value(m Metric) float64 {
	switch m {
	case CPU:
		return a.CPU
	case Mem:
		return float64(a.RSS)
	case DiskRead:
		return a.DiskRead
	case DiskWrite:
		return a.DiskWrite
	case NetRx:
		return a.NetRx
	case NetTx:
		return a.NetTx
	}
	return 0
}

// Snapshot is everything sampled at one tick.
type Snapshot struct {
	TS        time.Time     `json:"ts"`
	Elapsed   time.Duration `json:"-"` // real time since the previous sample
	Sys       System        `json:"sys"`
	Apps      []App         `json:"apps"` // sorted by CPU, busiest first
	NetPerApp bool          `json:"net_per_app"`
}

// App returns the named app, or nil.
func (s *Snapshot) App(name string) *App {
	for i := range s.Apps {
		if s.Apps[i].Name == name {
			return &s.Apps[i]
		}
	}
	return nil
}

// Culprit is an app ranked by how much it added during a spike.
type Culprit struct {
	App      string  `json:"app"`
	Share    float64 `json:"share"`  // 0..1 of the positive deltas
	Before   float64 `json:"before"` // mean in the minute before
	During   float64 `json:"during"` // mean during the spike
	Newcomer bool    `json:"newcomer"`
}

// Newcomer is an app whose first process started shortly before a spike.
type Newcomer struct {
	App        string  `json:"app"`
	StartedAgo float64 `json:"started_ago"` // seconds before onset; negative = after
}

// Mystery is one explained spike.
type Mystery struct {
	ID        int64      `json:"id"`
	Metric    Metric     `json:"metric"`
	Start     time.Time  `json:"start"`
	End       time.Time  `json:"end"`
	Baseline  float64    `json:"baseline"` // median of the window before
	Threshold float64    `json:"threshold"`
	Peak      float64    `json:"peak"`
	Mean      float64    `json:"mean"`
	Ratio     float64    `json:"ratio"` // mean / baseline; 0 when the baseline was ~0
	Culprits  []Culprit  `json:"culprits"`
	Newcomers []Newcomer `json:"newcomers"`
	Summary   string     `json:"summary"`
	Truncated bool       `json:"truncated"` // closed at the maximum length
}

// Duration is how long the spike lasted.
func (m *Mystery) Duration() time.Duration { return m.End.Sub(m.Start) }

// TopApp returns the leading culprit's name and share, or "" when nobody was blamed.
func (m *Mystery) TopApp() (string, float64) {
	if len(m.Culprits) == 0 {
		return "", 0
	}
	return m.Culprits[0].App, m.Culprits[0].Share
}
