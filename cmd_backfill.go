package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/coltconsulting/click-dog/internal/config"
)

// rewriteBackfillArgs implements the argument surface of `click-dog backfill`.
// Backfill runs the full fetch/filter/export pipeline that main() assembles for
// scheduled mode, so the verb does not get a handler of its own: dispatch()
// translates it into the legacy mode flags (-backfill-start/-backfill-end)
// and falls through into main()'s flag parsing. Returns the translated argv
// (without the program name), or nil with the intended exit code when parsing
// failed or -h was requested.
func rewriteBackfillArgs(args []string, errOut io.Writer) (argv []string, exitCode int) {
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	fs.SetOutput(errOut)
	start := fs.String("start", "", "Start of the export range (RFC3339, e.g. 2024-01-01T00:00:00Z)")
	end := fs.String("end", "", "End of the export range (RFC3339, e.g. 2024-01-01T23:59:59Z)")
	fs.String("config", config.DefaultConfigFlag, "Path to configuration file")
	fs.Bool("dry-run", false, "Read real data but discard exports; print summary")

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
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog backfill: unexpected argument %q\n", fs.Arg(0))
		return nil, 2
	}
	if *start == "" || *end == "" {
		_, _ = fmt.Fprintln(errOut, "click-dog backfill: both --start and --end are required")
		return nil, 2
	}

	// Forward only the flags the user actually set, mapped onto the names
	// main()'s FlagSet knows, so main's own defaults stay authoritative.
	argv = []string{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "start":
			argv = append(argv, "-backfill-start", f.Value.String())
		case "end":
			argv = append(argv, "-backfill-end", f.Value.String())
		case "dry-run":
			if f.Value.String() == "true" {
				argv = append(argv, "-dry-run")
			}
		default:
			argv = append(argv, "-"+f.Name, f.Value.String())
		}
	})
	return argv, 0
}
