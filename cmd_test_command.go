package main

import (
	"fmt"
	"io"
)

const testUsage = `click-dog test <subcommand>

Subcommands:
  export     Send a synthetic span through each configured exporter
  tracing    Run a traced ClickHouse query, recover its native spans, and export them

Usage:
  click-dog test <subcommand> [flags]
`

func printTestUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, testUsage)
}

func runTest(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		printTestUsage(out)
		return 0
	}

	switch args[0] {
	case "export":
		return runTestExport(args[1:], out, errOut)
	case "tracing":
		return runTestTracing(args[1:], out, errOut)
	case "help", "-h", "--help":
		printTestUsage(out)
		return 0
	default:
		_, _ = fmt.Fprintf(errOut, "click-dog test: unknown command %q\n\n", args[0])
		printTestUsage(errOut)
		return 2
	}
}
