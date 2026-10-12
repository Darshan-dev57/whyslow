package main

import (
	"log/slog"
	"strings"

	"github.com/Darshan-dev57/whyslow/internal/collector"
	"github.com/Darshan-dev57/whyslow/internal/netbpf"
	"github.com/Darshan-dev57/whyslow/internal/web"
)

type netSource interface {
	collector.NetSource
	Close() error
}

// openNet loads the eBPF network counter when asked to. If it cannot load,
// whyslow keeps running with system-level network numbers and per-app
// socket counts from /proc, and says why.
func openNet(useEBPF bool, log *slog.Logger) (netSource, web.NetStatus) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if !useEBPF {
		return nil, web.NetStatus{Mode: "proc", Reason: "started without --ebpf"}
	}
	src, err := netbpf.Load()
	if err != nil {
		log.Warn("eBPF network accounting unavailable; per-app network is off", "err", err)
		return nil, web.NetStatus{Mode: "proc", Reason: err.Error()}
	}
	st := web.NetStatus{Mode: "ebpf", Method: string(src.Mode())}
	log.Info("eBPF network accounting on", "method", src.Mode(), "hooks", strings.Join(src.Attached(), ","))
	if err := src.Fallback(); err != nil {
		st.Reason = err.Error()
		log.Info("fentry/fexit unavailable, counting packets per socket owner instead", "why", err)
	}
	for _, s := range src.Skipped() {
		log.Warn("optional eBPF hook skipped", "hook", s)
	}
	return src, st
}
