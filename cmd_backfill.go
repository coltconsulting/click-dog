package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/coltconsulting/click-dog/internal/config"
)

// runOptions carries what the export pipeline in main() takes from the command
// line. backfillStart and backfillEnd are set only by the backfill verb; when
// they are empty the pipeline runs in scheduled mode.
type runOptions struct {
	configPath    string
	dryRun        bool
	backfillStart string
	backfillEnd   string
}

// backfill reports whether these options select a one-shot backfill run.
func (o *runOptions) backfill() bool {
	return o.backfillStart != "" && o.backfillEnd != ""
}

// parseBackfillArgs implements the argument surface of `click-dog backfill`.
// Backfill runs the full fetch/filter/export pipeline that main() assembles for
// scheduled mode, so the verb does not get a handler of its own: dispatch()
// returns these options and main() runs the pipeline with them. Returns nil
// with the intended exit code when parsing failed or -h was requested.
func parseBackfillArgs(args []string, errOut io.Writer) (*runOptions, int) {
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	fs.SetOutput(errOut)
	start := fs.String("start", "", "Start of the export range (RFC3339, e.g. 2024-01-01T00:00:00Z)")
	end := fs.String("end", "", "End of the export range (RFC3339, e.g. 2024-01-01T23:59:59Z)")
	configPath := fs.String("config", config.DefaultConfigFlag, "Path to configuration file")
	dryRun := fs.Bool("dry-run", false, "Read real data but discard exports; print summary")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(errOut, `click-dog backfill — one-shot export of a historical time range

Usage:
  click-dog backfill --start <RFC3339> --end <RFC3339> [flags]

Exports spans between the two timestamps through the normal filter/export
pipeline, then exits.

Flags:
`)
		printFlagDefaults(errOut, fs)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, 0
		}
		return nil, 2
	}
	if err := rejectLeftoverArgs("click-dog backfill", fs, args); err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return nil, 2
	}
	if *start == "" || *end == "" {
		_, _ = fmt.Fprintln(errOut, "click-dog backfill: both --start and --end are required")
		return nil, 2
	}
	return &runOptions{
		configPath:    *configPath,
		dryRun:        *dryRun,
		backfillStart: *start,
		backfillEnd:   *end,
	}, 0
}
