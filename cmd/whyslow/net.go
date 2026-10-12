package main

import (
	"log/slog"

	"github.com/Darshan-dev57/whyslow/internal/collector"
	"github.com/Darshan-dev57/whyslow/internal/web"
)

type netSource interface {
	collector.NetSource
	Close() error
}

// openNet returns the per-app network source and where network numbers
// come from, for the UI and logs.
func openNet(useEBPF bool, log *slog.Logger) (netSource, web.NetStatus) {
	return nil, web.NetStatus{Mode: "proc", Reason: "eBPF not enabled"}
}
