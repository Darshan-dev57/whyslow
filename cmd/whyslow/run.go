package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Darshan-dev57/whyslow/internal/pipeline"
	"github.com/Darshan-dev57/whyslow/internal/store"
	"github.com/Darshan-dev57/whyslow/internal/web"
)

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7777", "web UI address (the process list is private: keep it on loopback)")
	dbPath := fs.String("db", defaultDBPath(), "SQLite database path")
	useEBPF := fs.Bool("ebpf", false, "per-app network via eBPF (needs root and a BTF kernel)")
	interval := fs.Duration("interval", time.Second, "sampling interval, whole seconds")
	verbose := fs.Bool("v", false, "debug logging")
	fs.Parse(args)
	log := newLogger(*verbose)
	if err := checkInterval(*interval); err != nil {
		return err
	}

	// Plain TCP, not Go's default MPTCP: MPTCP subflows are kernel sockets
	// that the socket-owner eBPF method cannot attribute to whyslow.
	var lc net.ListenConfig
	lc.SetMultipathTCP(false)
	ln, err := lc.Listen(ctx, "tcp", *addr)
	if err != nil {
		return fmt.Errorf("web UI: %w", err)
	}
	loopback := isLoopback(ln.Addr())
	if !loopback {
		log.Warn("the web UI is reachable from other machines and shows every process on this one", "addr", ln.Addr())
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		ln.Close()
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()
	handBack(*dbPath, *dbPath+"-wal", *dbPath+"-shm")

	netSrc, netStatus := openNet(*useEBPF, log)
	if netSrc != nil {
		defer netSrc.Close()
	}
	coll, err := newCollector(netSrc, 5)
	if err != nil {
		ln.Close()
		return err
	}
	sink := pipeline.NewStoreSink(st, log)
	hub := web.NewHub()
	cfg := pipeline.DefaultConfig()
	cfg.Log = log
	// the store sink goes first so a mystery has its ID when the hub sends it
	p := pipeline.New(cfg, coll.NetPerApp(), sink, hub)
	srv := web.New(web.Options{Store: st, Hub: hub, Version: version, Interval: *interval, Net: netStatus,
		Log: log, AllowAnyHost: !loopback})
	log.Info("whyslow running", "ui", "http://"+ln.Addr().String(), "db", *dbPath, "net", netStatus.Mode, "interval", *interval)

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return sink.Run(ctx) })
	g.Go(func() error { return p.Run(ctx, coll, *interval) })
	g.Go(func() error { return srv.Serve(ctx, ln) })
	return g.Wait()
}

func isLoopback(a net.Addr) bool {
	ta, ok := a.(*net.TCPAddr)
	return ok && ta.IP.IsLoopback()
}

// checkInterval accepts whole seconds only. The database keeps one row per
// second (a faster sampler overwrote nine of every ten rows), and the
// detector's windows are counted in samples, so a sub-second interval would
// also shrink the two-minute baseline to seconds.
func checkInterval(d time.Duration) error {
	if d < time.Second || d%time.Second != 0 {
		return fmt.Errorf("--interval must be a whole number of seconds, got %s", d)
	}
	return nil
}
