package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/store"
)

func cmdMysteries(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mysteries", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite database path")
	since := fs.Duration("since", 24*time.Hour, "how far back to look")
	limit := fs.Int("limit", 50, "maximum number to show")
	fs.Parse(args)

	st, err := openExisting(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	now := time.Now()
	ms, err := st.Mysteries(ctx, now.Add(-*since), now.Add(time.Minute), *limit)
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		fmt.Printf("No mysteries in the last %s. Either the machine behaved, or whyslow was not running.\n", *since)
		return nil
	}
	for i := len(ms) - 1; i >= 0; i-- {
		m := ms[i]
		fmt.Printf("#%-4d %s  %s\n", m.ID, m.Start.Local().Format("Jan 02"), m.Summary)
	}
	return nil
}

// openExisting opens a database for reading and gives a helpful error when
// whyslow has never run.
func openExisting(path string) (*store.Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no database at %s (start the collector with \"whyslow run\" first)", path)
	}
	return store.Open(path)
}
