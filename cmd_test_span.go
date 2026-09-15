package main

import (
	"fmt"
	"io"
)

func runTestSpan(args []string, out, errOut io.Writer) int {
	_, _ = fmt.Fprintln(errOut, "warning: click-dog test-span is deprecated; use 'click-dog test export'.")
	return runTestExportAs("test-span", args, out, errOut)
}
