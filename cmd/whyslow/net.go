package main

import (
	"log/slog"

	"github.com/Darshan-dev57/whyslow/internal/collector"
)

type netSource interface {
	collector.NetSource
	Close() error
}

// openNet returns the per-app network source and a short description of
// the network mode for the UI and logs.
func openNet(useEBPF bool, log *slog.Logger) (netSource, string) {
	return nil, "proc"
}
