package main

import (
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

// certHostnameHint avoids err.Error() on the typed path: x509.HostnameError.Error() dereferences Certificate.
func certHostnameHint(err error, host string) string {
	var he x509.HostnameError
	mismatch := errors.As(err, &he)
	if !mismatch {
		msg := err.Error()
		mismatch = strings.Contains(msg, "x509:") && strings.Contains(msg, "certificate is valid for")
	}
	if !mismatch {
		return ""
	}
	return fmt.Sprintf("fix: the server's TLS certificate is not valid for %q — set "+
		"clickhouse.host to a name the certificate lists (shown above, usually the "+
		"fully-qualified domain name), or set clickhouse.insecure_skip_verify: true "+
		"to skip verification (insecure).", host)
}

const perCheckTimeout = 5 * time.Second

// readinessTimeout bounds the full ClickHouse readiness pass. Individual
// queries are additionally bounded by the reader's query timeout.
const readinessTimeout = 60 * time.Second

func runCheck(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(errOut)
	configPath := fs.String("config", config.DefaultConfigFlag, "Path to configuration file")
	lookback := fs.Duration("lookback", 24*time.Hour, "Lookback window for recent-data readiness checks (e.g. 24h, 6h)")
	// --quick skips the ClickHouse data-plane readiness probes, leaving only the
	// config parse, ClickHouse ping, and exporter connectivity. It exists for
	// tests and emergency scripting and is intentionally absent from the usage
	// text so operators get full readiness by default. Automation that only
	// needs config safety should use `click-dog validate` (offline) instead of
	// depending on this flag.
	quick := fs.Bool("quick", false, "")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(errOut, `click-dog check — validate config and verify ClickHouse + exporters

Usage:
  click-dog check [flags]

Validates the config, pings ClickHouse, tests exporter connectivity, and probes
the ClickHouse data plane the Datadog dashboards depend on: span/query log
readability and grants, recent spans, duration thresholds, clickhouse.query_id
presence, query_log enrichment join, and normalized_query_hash support. Data
gaps warn; missing required tables/grants fail. For an offline, config-only
check use `+"`click-dog validate`"+` instead.

Flags:
`)
		printFlagDefaults(errOut, fs)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	allOK := true
	sawWarnings := false

	// 1. Load config
	cfg, resolvedPath, err := loadConfig(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(out, "Config:     FAIL (%v)\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "Config:     ok (%s)\n", resolvedPath)
	// Surface unset ${VAR} references (e.g. a forgotten CLICKHOUSE_PASSWORD) up
	// front — otherwise the empty expansion only shows as an opaque ClickHouse
	// auth error further down. The daemon path warns the same way (main.go).
	for _, w := range cfg.EnvWarnings {
		_, _ = fmt.Fprintf(out, "            warning: %s\n", w)
	}

	// 2. ClickHouse connectivity
	chCtx, chCancel := context.WithTimeout(context.Background(), perCheckTimeout)
	chReader, err := clickhouse.NewClickHouseReader(cfg.ClickHouse, cfg.Filters)
	if err != nil {
		_, _ = fmt.Fprintf(out, "ClickHouse: FAIL (%v)\n", err)
		if hint := certHostnameHint(err, cfg.ClickHouse.Host); hint != "" {
			_, _ = fmt.Fprintf(out, "            %s\n", hint)
		}
		allOK = false
	} else {
		healthy := chReader.IsHealthy(chCtx)
		if healthy {
			_, _ = fmt.Fprintf(out, "ClickHouse: ok (%s:%d)\n", cfg.ClickHouse.Host, cfg.ClickHouse.Port)
		} else {
			_, _ = fmt.Fprintf(out, "ClickHouse: FAIL (ping failed: %s:%d)\n", cfg.ClickHouse.Host, cfg.ClickHouse.Port)
			allOK = false
		}
		// Readiness checks run only once the basic ping succeeds — a dead
		// connection would just report every prerequisite as unreadable. --quick
		// opts out of the data-plane probes entirely.
		if healthy && !*quick {
			ok, warned := runReadinessReport(out, chReader, cfg, *lookback)
			if !ok {
				allOK = false
			}
			if warned {
				sawWarnings = true
			}
		}
		_ = chReader.Close()
	}
	chCancel()

	// 3. Exporter connectivity — one line per configured exporter, in
	// config order. Exporters that implement model.ConnectivityChecker are
	// actively probed; any other kind passes construction only. The label
	// column pads to the report's 12-column layout ("OTEL[0]:    ok",
	// "SplunkHEC[0]: ok"), matching the Config/ClickHouse lines above.
	for _, b := range buildExporters(cfg) {
		label := b.Label + ":"
		if b.InitErr != nil {
			_, _ = fmt.Fprintf(out, "%-11s FAIL (init: %v)\n", label, b.InitErr)
			allOK = false
			continue
		}
		if checker, canProbe := b.Exporter.(model.ConnectivityChecker); canProbe {
			checkCtx, checkCancel := context.WithTimeout(context.Background(), perCheckTimeout)
			if connErr := checker.CheckConnectivity(checkCtx); connErr != nil {
				_, _ = fmt.Fprintf(out, "%-11s FAIL (%s: %v)\n", label, b.Endpoint, connErr)
				allOK = false
			} else {
				_, _ = fmt.Fprintf(out, "%-11s ok (%s)\n", label, b.Endpoint)
			}
			checkCancel()
		} else {
			_, _ = fmt.Fprintf(out, "%-11s ok (constructed; exporter has no connectivity probe)\n", label)
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = b.Exporter.Close(closeCtx)
		closeCancel()
	}

	if !allOK {
		_, _ = fmt.Fprintln(errOut, "One or more checks failed")
		return 1
	}
	// WARN lines (empty/recent-data gaps, optional capabilities) don't fail the
	// command, but the closing line must not claim "All checks passed" when the
	// report flagged gaps — that would recreate the false-confidence UX.
	if sawWarnings {
		_, _ = fmt.Fprintln(out, "\nNo failing checks, but some checks reported warnings (see above).")
	} else {
		_, _ = fmt.Fprintln(out, "\nAll checks passed.")
	}
	return 0
}

// runReadinessReport prints the ClickHouse data-plane readiness report. It
// returns ok=false if any check failed (a missing required table/grant) and
// warned=true if any check warned (empty/recent-data gaps, optional
// capabilities) — warnings are reported but do not fail the command.
func runReadinessReport(out io.Writer, chReader *clickhouse.ClickHouseReader, cfg *config.Config, lookback time.Duration) (ok, warned bool) {
	_, _ = fmt.Fprintf(out, "\nClickHouse readiness (lookback %s):\n", lookback)

	// Connection / TLS mode — informational context for the data-plane checks.
	tlsMode := "plaintext"
	if cfg.ClickHouse.Secure {
		tlsMode = "TLS"
		if cfg.ClickHouse.InsecureSkipVerify {
			tlsMode = "TLS (insecure_skip_verify — certificate not verified)"
		}
	}
	_, _ = fmt.Fprintf(out, "  [PASS] connection: %s:%d (%s)\n",
		cfg.ClickHouse.Host, cfg.ClickHouse.Port, tlsMode)

	enrichmentNeeded := cfg.Monitor.ShouldEnrichFromQueryLog() ||
		len(cfg.Filters.WhitelistUsers) > 0 || len(cfg.Filters.BlacklistUsers) > 0

	ctx, cancel := context.WithTimeout(context.Background(), readinessTimeout)
	defer cancel()
	checks := chReader.RunReadinessChecks(ctx, clickhouse.ReadinessOptions{
		Lookback:           lookback,
		MinTraceDurationMs: cfg.Monitor.MinTraceDurationMs,
		EnrichmentNeeded:   enrichmentNeeded,
	})

	return printReadinessChecks(out, checks)
}

// printReadinessChecks renders the readiness report. It returns ok=false if any
// check failed and warned=true if any check warned. Remedies print only for
// warn/fail lines.
func printReadinessChecks(out io.Writer, checks []clickhouse.ReadinessCheck) (ok, warned bool) {
	ok = true
	for _, ck := range checks {
		_, _ = fmt.Fprintf(out, "  [%-4s] %s: %s\n", ck.Status, ck.Name, ck.Detail)
		if ck.Remedy != "" && (ck.Status == clickhouse.ReadinessWarn || ck.Status == clickhouse.ReadinessFail) {
			_, _ = fmt.Fprintf(out, "         fix: %s\n", ck.Remedy)
		}
		switch ck.Status {
		case clickhouse.ReadinessFail:
			ok = false
		case clickhouse.ReadinessWarn:
			warned = true
		}
	}
	return ok, warned
}
