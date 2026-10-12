package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/model"
)

func cmdTop(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	interval := fs.Duration("interval", time.Second, "refresh interval")
	n := fs.Int("n", 15, "number of apps to show")
	once := fs.Bool("once", false, "print one frame and exit (no screen clearing)")
	useEBPF := fs.Bool("ebpf", false, "per-app network via eBPF (needs root)")
	fs.Parse(args)

	net, mode := openNet(*useEBPF, nil)
	if net != nil {
		defer net.Close()
	}
	c, err := newCollector(net, 5)
	if err != nil {
		return err
	}
	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		s, err := c.Sample()
		if err != nil {
			return err
		}
		if *once {
			renderTop(os.Stdout, s, *n, mode)
			return nil
		}
		var b strings.Builder
		b.WriteString("\033[H\033[2J")
		renderTop(&b, s, *n, mode)
		io.WriteString(os.Stdout, b.String())
	}
}

func renderTop(w io.Writer, s *model.Snapshot, n int, mode string) {
	sys := s.Sys
	fmt.Fprintf(w, "whyslow top  %s   CPU %.0f%%   Mem %s / %s   Disk r %s w %s   Net rx %s tx %s   [net: %s]\n",
		s.TS.Format("15:04:05"), sys.CPU, model.Bytes(float64(sys.MemUsed)), model.Bytes(float64(sys.MemTotal)),
		rate(sys.DiskRead), rate(sys.DiskWrite), rate(sys.NetRx), rate(sys.NetTx), mode)
	if b := sys.Battery; b != nil {
		fmt.Fprintf(w, "battery %.0f%% %s", b.Percent, b.Status)
		if b.HasPower {
			fmt.Fprintf(w, " %.1f W", b.Watts)
		}
		fmt.Fprintln(w)
	}
	if sys.TempC != nil {
		fmt.Fprintf(w, "temperature %.0f C\n", *sys.TempC)
	}
	fmt.Fprintf(w, "\n%-24s %7s %9s %10s %10s %10s %10s %6s\n", "APP", "CPU%", "MEM", "DISK R", "DISK W", "NET RX", "NET TX", "PROCS")
	for i, a := range s.Apps {
		if i == n {
			break
		}
		netRx, netTx := "-", "-"
		if a.NetActive {
			netRx, netTx = rate(a.NetRx), rate(a.NetTx)
		}
		dr, dw := rate(a.DiskRead), rate(a.DiskWrite)
		if a.IOHidden > 0 && a.IOHidden == a.Procs {
			dr, dw = "?", "?"
		}
		fmt.Fprintf(w, "%-24s %7.1f %9s %10s %10s %10s %10s %6d\n", trunc(a.Name, 24), a.CPU,
			model.Bytes(float64(a.RSS)), dr, dw, netRx, netTx, a.Procs)
	}
}

func rate(v float64) string { return model.Bytes(v) + "/s" }

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "~"
}
