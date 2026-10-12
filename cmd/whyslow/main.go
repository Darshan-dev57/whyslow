// Command whyslow watches a Linux machine and explains its slowdowns.
//
//	whyslow run         collect, detect spikes and serve the web UI
//	whyslow top         live terminal view
//	whyslow mysteries   recent explained spikes
//	whyslow report      daily culprits
//	whyslow doctor      what this machine supports
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Darshan-dev57/whyslow/internal/collector"
)

var version = "dev"

const usage = `whyslow: find out why your machine is slow

Usage:
  whyslow run [--addr 127.0.0.1:7777] [--db PATH] [--ebpf] [--interval 1s]
  whyslow top [--ebpf] [--n 15] [--once]
  whyslow mysteries [--since 24h] [--db PATH]
  whyslow report [--today | --date YYYY-MM-DD] [--db PATH]
  whyslow doctor
  whyslow version

Run "whyslow <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "run":
		err = cmdRun(ctx, args)
	case "top":
		err = cmdTop(ctx, args)
	case "mysteries":
		err = cmdMysteries(ctx, args)
	case "version", "--version":
		fmt.Println("whyslow", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "whyslow: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "whyslow:", err)
		os.Exit(1)
	}
}

func newCollector(net collector.NetSource, socketEvery int) (*collector.Collector, error) {
	return collector.New(collector.Options{Net: net, SocketEvery: socketEvery})
}

func newLogger(verbose bool) *slog.Logger {
	lvl := slog.LevelInfo
	if verbose {
		lvl = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
