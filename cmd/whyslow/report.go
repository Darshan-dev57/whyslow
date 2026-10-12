package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/report"
)

func cmdReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite database path")
	fs.Bool("today", true, "report on today (the default)")
	date := fs.String("date", "", "report on this local date, YYYY-MM-DD")
	asJSON := fs.Bool("json", false, "print JSON instead of text")
	fs.Parse(args)

	day := time.Now()
	if *date != "" {
		d, err := time.ParseInLocation("2006-01-02", *date, time.Local)
		if err != nil {
			return fmt.Errorf("--date: want YYYY-MM-DD, got %q", *date)
		}
		day = d
	}
	st, err := openExisting(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	r, err := report.Build(ctx, st, day, report.DefaultConfig())
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	report.WriteText(os.Stdout, r)
	return nil
}
