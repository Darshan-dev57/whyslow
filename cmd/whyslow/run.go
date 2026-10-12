package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Darshan-dev57/whyslow/internal/pipeline"
	"github.com/Darshan-dev57/whyslow/internal/store"
)

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite database path")
	useEBPF := fs.Bool("ebpf", false, "per-app network via eBPF (needs root and a BTF kernel)")
	interval := fs.Duration("interval", time.Second, "sampling interval")
	verbose := fs.Bool("v", false, "debug logging")
	fs.Parse(args)
	log := newLogger(*verbose)

	st, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()
	handBack(*dbPath, *dbPath+"-wal", *dbPath+"-shm")

	net, mode := openNet(*useEBPF, log)
	if net != nil {
		defer net.Close()
	}
	coll, err := newCollector(net, 5)
	if err != nil {
		return err
	}
	sink := pipeline.NewStoreSink(st, log)
	cfg := pipeline.DefaultConfig()
	cfg.Log = log
	p := pipeline.New(cfg, coll.NetPerApp(), sink)
	log.Info("whyslow running", "db", *dbPath, "net", mode, "interval", *interval)

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return sink.Run(ctx) })
	g.Go(func() error { return p.Run(ctx, coll, *interval) })
	return g.Wait()
}
