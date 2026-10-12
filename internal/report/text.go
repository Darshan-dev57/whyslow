package report

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Darshan-dev57/whyslow/internal/blame"
	"github.com/Darshan-dev57/whyslow/internal/model"
)

// FormatValue renders a section total for people.
func FormatValue(unit string, v float64) string {
	if unit == "cpu_s" {
		return blame.FormatDuration(time.Duration(v * float64(time.Second)))
	}
	return model.Bytes(v)
}

// WriteText renders the report for a terminal.
func WriteText(w io.Writer, r *Report) {
	fmt.Fprintf(w, "whyslow report for %s (watched %s)\n", r.Date, blame.FormatDuration(r.Watched.Round(time.Second)))
	if r.Watched == 0 {
		fmt.Fprintln(w, "\nNo samples for this day. Was \"whyslow run\" running?")
		return
	}
	for _, s := range r.Sections {
		fmt.Fprintf(w, "\n%s\n", s.Label)
		if len(s.Apps) == 0 {
			fmt.Fprintln(w, "  (nothing recorded)")
			continue
		}
		for i, a := range s.Apps {
			fmt.Fprintf(w, "  %d. %-24s %s\n", i+1, a.App, FormatValue(s.Unit, a.Value))
		}
	}

	fmt.Fprintln(w, "\nBattery hogs (estimate)")
	if r.Battery == nil {
		fmt.Fprintln(w, "  No time on battery this day.")
	} else {
		b := r.Battery
		fmt.Fprintf(w, "  %s on battery, %.1f%% used", blame.FormatDuration(b.OnBattery.Round(time.Second)), b.DrainPct)
		if b.DrainWh > 0 {
			fmt.Fprintf(w, " (%.1f Wh)", b.DrainWh)
		}
		fmt.Fprintln(w)
		for i, h := range b.Hogs {
			fmt.Fprintf(w, "  %d. %-24s ~%.1f%%\n", i+1, h.App, h.Pct)
		}
	}

	fmt.Fprintf(w, "\nMysteries: %d", len(r.Mysteries))
	if len(r.ByMetric) > 0 {
		var parts []string
		for _, m := range model.Metrics {
			if n := r.ByMetric[m]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", lower(m.Label()), n))
			}
		}
		fmt.Fprintf(w, " (%s)", strings.Join(parts, ", "))
	}
	fmt.Fprintln(w)
	for i, m := range r.Mysteries { // newest first
		if i == 5 {
			fmt.Fprintf(w, "  ... and %d more (whyslow mysteries)\n", len(r.Mysteries)-5)
			break
		}
		fmt.Fprintf(w, "  #%d %s\n", m.ID, m.Summary)
	}

	fmt.Fprintf(w, "\nRepeat offenders (since %s)\n", r.RepeatSince.Format("Jan 02"))
	if len(r.Repeat) == 0 {
		fmt.Fprintln(w, "  None yet.")
	}
	for _, o := range r.Repeat {
		fmt.Fprintf(w, "  - %s\n", o.Text)
	}
}
