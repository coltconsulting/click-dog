package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/export"
	"github.com/coltconsulting/click-dog/internal/model"
)

// writePrivateFile replaces path with a freshly-created file carrying exactly
// mode. Use it for every artifact whose permissions are part of its contract —
// rendered configs, deployment secrets, and analysis reports all carry material
// that must not become world-readable.
//
// os.WriteFile is not a substitute: its mode argument applies only when the
// file is created, so an existing target silently keeps whatever mode it
// already had, and a symlink sitting at the path is followed and its target
// overwritten. Removing first and creating with O_EXCL makes a symlink planted
// between the two steps fail closed rather than be followed, and the explicit
// Chmod corrects both a permissive pre-existing mode and any owner bits the
// caller's umask would have masked off at create time.
//
// This is a replace, not an atomic swap: any previous file is removed up front
// and a failure after that removes the partial too, so a failed write leaves no
// file at the path rather than a truncated or half-permissioned one. Losing the
// previous version on a failed rewrite is the accepted trade — it is the
// behavior the generated deployment secrets have always had, and a truncated
// artifact is worse than an absent one. Callers that must keep the old version
// readable until the new one is complete want analysis.WriteBaselineAtomic's
// temp-file-and-rename instead.
func writePrivateFile(path string, content []byte, mode os.FileMode) error {
	// Decide what is at the path BEFORE removing anything, and allow only the
	// three cases this helper is defined for: nothing, a regular file, or a
	// symlink. os.Remove unlinks anything else just as happily — an empty
	// directory, a live Unix socket, a FIFO, a device node — so an --output or
	// --force --output naming one of those would destroy it and leave a plain
	// file behind. os.WriteFile refused a directory (EISDIR) and a socket
	// (ENXIO), and wrote *through* a device node, so none of those were
	// destructive before.
	//
	// Lstat, not Stat: a symlink is judged as a symlink whatever it points at,
	// so replacing the link rather than following it — the behavior this helper
	// exists for — keeps working, while a symlink to a directory is not
	// mistaken for a directory.
	info, err := os.Lstat(path)
	switch {
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("checking %s: %w", path, err)
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		// The one non-regular type deliberately replaced.
	case err == nil && !info.Mode().IsRegular():
		return fmt.Errorf("%s is %s, not a regular file", path, fileKind(info.Mode()))
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing pre-existing private file: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	removeOnFailure := true
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		if removeOnFailure {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	closed = true
	removeOnFailure = false
	return nil
}

// fileKind names the filesystem object at a rejected destination so the error
// tells an operator what is actually in the way. Ordered most-specific first:
// a character device sets both ModeDevice and ModeCharDevice.
func fileKind(mode os.FileMode) string {
	switch {
	case mode.IsDir():
		return "a directory"
	case mode&os.ModeSocket != 0:
		return "a socket"
	case mode&os.ModeNamedPipe != 0:
		return "a named pipe"
	case mode&os.ModeCharDevice != 0:
		return "a character device"
	case mode&os.ModeDevice != 0:
		return "a block device"
	default:
		return "an irregular file"
	}
}

// printFlagDefaults writes fs's flag table in flag.PrintDefaults' layout but
// spells long options --flag, matching the documented option style (the parser
// accepts both). Single-character shorthands keep one dash. Flags registered
// with an empty usage string are hidden/internal (check's --quick) and are
// omitted, and defaults that are the type's zero value are elided like
// PrintDefaults does.
func printFlagDefaults(w io.Writer, fs *flag.FlagSet) {
	fs.VisitAll(func(f *flag.Flag) {
		if f.Usage == "" {
			return
		}
		dash := "--"
		if len(f.Name) == 1 {
			dash = "-"
		}
		name, usage := flag.UnquoteUsage(f)
		line := "  " + dash + f.Name
		if name != "" {
			line += " " + name
		}
		line += "\n    \t" + strings.ReplaceAll(usage, "\n", "\n    \t")
		switch f.DefValue {
		case "", "false", "0", "0s":
			// zero value — no default clause
		default:
			if name == "string" {
				line += fmt.Sprintf(" (default %q)", f.DefValue)
			} else {
				line += fmt.Sprintf(" (default %v)", f.DefValue)
			}
		}
		_, _ = fmt.Fprintln(w, line)
	})
}

// loadConfig resolves a -config flag value and loads the file it names. It
// returns the resolved path alongside the config so callers can report which
// file was used without re-resolving; on a load failure the resolved path is
// still returned. Error presentation stays with the caller — each command has
// its own output contract.
func loadConfig(flagPath string) (*config.Config, string, error) {
	resolvedPath, err := config.ResolveConfigPath(flagPath)
	if err != nil {
		return nil, "", err
	}
	cfg, err := config.LoadConfig(resolvedPath)
	if err != nil {
		return nil, resolvedPath, err
	}
	return cfg, resolvedPath, nil
}

// builtExporter pairs a constructed span exporter with the identity strings
// commands print for it and the MultiExporter names it by.
type builtExporter struct {
	// Exporter is nil when InitErr is non-nil.
	Exporter model.SpanExporter
	// OTEL holds the concrete exporter for exporters.otel entries; the daemon
	// reuses its gRPC connection for the OTLP self-metrics push. Nil for
	// other exporter kinds.
	OTEL *export.OTELExporter
	// Label is the human-facing name used in command output: "OTEL[0]",
	// "SplunkHEC[1]".
	Label string
	// Name is the machine-facing name used by MultiExporter and metrics:
	// "otel[0]:collector:4317", "splunk_hec[1]:https://…".
	Name string
	// Endpoint is the collector address or HEC endpoint for status lines.
	Endpoint string
	// InitErr records a constructor failure for this entry.
	InitErr error
}

// buildExporters constructs every exporter configured under exporters.*, in
// OTEL-then-Splunk-HEC order — the single place exporter construction and
// naming live. Constructor failures are reported per entry via InitErr rather
// than aborting the build, because callers disagree on severity: the daemon
// treats any failure as fatal, while `check` and the test commands report the
// failed exporter and keep going.
func buildExporters(cfg *config.Config) []builtExporter {
	built := make([]builtExporter, 0, len(cfg.Exporters.OTEL)+len(cfg.Exporters.SplunkHEC))
	for i, otelCfg := range cfg.Exporters.OTEL {
		b := builtExporter{
			Label:    fmt.Sprintf("OTEL[%d]", i),
			Name:     fmt.Sprintf("otel[%d]:%s", i, otelCfg.CollectorAddress),
			Endpoint: otelCfg.CollectorAddress,
		}
		if exp, err := export.NewOTELExporter(otelCfg); err != nil {
			b.InitErr = err
		} else {
			b.Exporter = exp
			b.OTEL = exp
		}
		built = append(built, b)
	}
	for i, splunkCfg := range cfg.Exporters.SplunkHEC {
		b := builtExporter{
			Label:    fmt.Sprintf("SplunkHEC[%d]", i),
			Name:     fmt.Sprintf("splunk_hec[%d]:%s", i, splunkCfg.Endpoint),
			Endpoint: splunkCfg.Endpoint,
		}
		if exp, err := export.NewSplunkHECExporter(splunkCfg); err != nil {
			b.InitErr = err
		} else {
			b.Exporter = exp
		}
		built = append(built, b)
	}
	return built
}
