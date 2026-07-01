package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"google.golang.org/grpc"

	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/export"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/health"
	"github.com/coltconsulting/click-dog/internal/leader"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/processor"
	"github.com/coltconsulting/click-dog/internal/resilience"
	"github.com/coltconsulting/click-dog/internal/selfaudit"
	"github.com/coltconsulting/click-dog/internal/webhook"
)

// version is set at build time via -ldflags="-X main.version=..."
var version = "dev"

const (
	// httpShutdownTimeout is the grace period for HTTP servers (metrics,
	// health) to flush in-flight responses during shutdown.
	httpShutdownTimeout = 3 * time.Second

	// exporterCloseTimeout is the grace period for flushing exporters on shutdown.
	exporterCloseTimeout = 5 * time.Second

	// heartbeatInterval controls how often the periodic heartbeat log is emitted.
	heartbeatInterval = 5 * time.Minute
)

// shutdownHTTPServer gracefully stops srv within httpShutdownTimeout.
// Intended to be called via `defer shutdownHTTPServer(srv)` — both the
// metrics and health listeners need the same cleanup behavior.
func shutdownHTTPServer(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func otlpMetricsSinkNames(cfg *config.Config, dryRun bool) map[string]string {
	if dryRun {
		return map[string]string{"dry_run": "dry_run"}
	}

	names := make(map[string]string, len(cfg.Exporters.OTEL)+len(cfg.Exporters.SplunkHEC)+2)
	for i, otelCfg := range cfg.Exporters.OTEL {
		stable := fmt.Sprintf("otel_%d", i)
		names[fmt.Sprintf("otel[%d]:%s", i, otelCfg.CollectorAddress)] = stable
		if i == 0 {
			names["otel"] = stable
		}
	}
	for i, splunkCfg := range cfg.Exporters.SplunkHEC {
		stable := fmt.Sprintf("splunk_hec_%d", i)
		names[fmt.Sprintf("splunk_hec[%d]:%s", i, splunkCfg.Endpoint)] = stable
		if i == 0 {
			names["splunk_hec"] = stable
		}
	}
	return names
}

// sameListenAddress reports whether two ListenAndServe addresses would bind
// to the same TCP socket. Literal string equality misses the common case
// where one address is written as ":9090" and the other as "0.0.0.0:9090"
// — both bind to every interface on port 9090. We canonicalise wildcard
// host prefixes, then compare. If the inputs differ textually but the
// canonical forms match, a warning is logged so operators notice they
// wrote inconsistent addresses.
func sameListenAddress(a, b string) bool {
	canonA := canonicalListenAddr(a)
	canonB := canonicalListenAddr(b)
	if canonA != canonB {
		return false
	}
	if a != b {
		clicklog.Warn("health.listen_address %q and metrics.listen_address %q differ textually but bind to the same socket — sharing one listener", a, b)
		// Only the IPv4↔IPv6 wildcard cross is OS-dependent: "0.0.0.0:P"
		// and "[::]:P" bind the same socket on Linux with IPV6_V6ONLY=0
		// but bind different sockets on macOS/BSD/Linux with
		// bindv6only=1.
		//
		// Warn (not Debug): on macOS/BSD the v4 and v6 wildcards bind
		// different sockets, so assuming equivalence will silently drop
		// one family of probe traffic. A developer running click-dog
		// locally with mismatched wildcard forms should see this at
		// normal log level, not have to crank verbosity to debug why
		// readiness probes hit the wrong port.
		//
		// Caveat: we don't detect Linux with net.ipv6.bindv6only=1 (some
		// hardened distros). On those hosts this Warn does NOT fire even
		// though the equivalence assumption fails. The outer Warn still
		// makes the textual mismatch visible.
		if runtime.GOOS != "linux" && isCrossWildcard(a, b) {
			clicklog.Warn("listener-sharing: v4/v6 wildcard equivalence assumed on %s — health endpoints may be missing on one family; use matching forms (both '0.0.0.0:P', or both '[::]:P'/':P') to be safe", runtime.GOOS)
		}
	}
	return true
}

// isCrossWildcard reports whether a and b mix an IPv4 wildcard with an
// IPv6 wildcard (including Go's default short form). The IPv4/IPv6
// wildcard equivalence we rely on for listener sharing is the Linux-only
// assumption; this helper narrows the diagnostic log to just that case.
//
// Go binds ":PORT" as a v6 wildcard (same socket as "[::]:PORT"), so
// those two forms are NOT cross — they're identical across OSes. The
// real cross case is "0.0.0.0:PORT" (explicit v4) paired with either
// "[::]:PORT" or the short ":PORT" (both v6). That pairing binds a
// single socket on Linux (v6 wildcard covers v4) but two separate
// sockets on macOS/BSD/Linux-with-bindv6only=1.
func isCrossWildcard(a, b string) bool {
	isV4 := func(s string) bool { return strings.HasPrefix(s, "0.0.0.0:") }
	// Explicit v6 form OR Go's short form (which also binds v6).
	// The short form is ":PORT" where PORT is numeric — we require a
	// digit after the colon so malformed inputs like "::1" or "::" are
	// not misclassified as wildcards.
	isV6 := func(s string) bool {
		if strings.HasPrefix(s, "[::]:") {
			return true
		}
		return len(s) > 1 && s[0] == ':' && s[1] >= '0' && s[1] <= '9'
	}
	return (isV4(a) && isV6(b)) || (isV6(a) && isV4(b))
}

// canonicalListenAddr rewrites "0.0.0.0:PORT" and "[::]:PORT" forms to the
// shorter ":PORT" wildcard. Other host specifications are returned as-is
// so explicit binds (e.g. "127.0.0.1:9090") still compare separately from
// wildcards.
//
// This treats IPv4 and IPv6 wildcards as equivalent. The assumption holds
// on Linux with the default IPV6_V6ONLY=0 (sysctl net.ipv6.bindv6only=0),
// where binding [::] covers both address families. It does NOT hold on:
//
//   - macOS, FreeBSD, and OpenBSD, which default to IPV6_V6ONLY=1.
//   - Linux with net.ipv6.bindv6only=1 (some hardened distros).
//
// On those targets "[::]:9090" and "0.0.0.0:9090" bind to different
// sockets, so our "same listener" decision is optimistic — it may skip a
// second listener that would actually succeed. click-dog is deployed as a
// Linux DaemonSet in production, so this caveat is mostly relevant to
// developers running on a Mac. When it matters, write both addresses in
// the same form and the string compare path takes over.
func canonicalListenAddr(addr string) string {
	// TrimPrefix over manual byte-slicing: the intent ("strip the host,
	// keep the :PORT tail") is obvious from the prefix argument and a
	// follow-up edit can't introduce an off-by-one.
	switch {
	case strings.HasPrefix(addr, "0.0.0.0:"):
		return strings.TrimPrefix(addr, "0.0.0.0")
	case strings.HasPrefix(addr, "[::]:"):
		return strings.TrimPrefix(addr, "[::]")
	default:
		return addr
	}
}

// subcommands maps each non-flag verb to its handler. Each handler returns its
// intended process exit code (no in-handler os.Exit) so dispatch() can surface
// it for the single os.Exit in main() and the handlers stay unit-testable. Kept
// as a registry so dispatch() can both route known verbs and tell an unknown
// one apart from a legitimate flag-only invocation.
var subcommands = map[string]func(args []string, out, errOut io.Writer) int{
	"init":              runInit,
	"check":             runCheck,
	"analyze":           runAnalyze,
	"test-span":         runTestSpan,
	"flush":             runFlush,
	"self-update":       runSelfUpdate,
	"create-dashboards": runCreateDashboards,
	"deploy":            runDeploy,
}

// usageBanner is the command/mode/example reference. The per-flag defaults
// table (flag.PrintDefaults) is appended only by flag.Usage for -h; the `help`
// verb prints the banner alone, which is self-contained. %s is the version.
const usageBanner = `click-dog %s — ClickHouse query monitor

Exports slow queries and OpenTelemetry spans from ClickHouse to an
OTEL collector via gRPC.

Usage:
  click-dog [flags]
  click-dog init [flags]         Generate a starter configuration file
  click-dog check [flags]        Validate config, probe the ClickHouse data plane, and test exporters
  click-dog analyze <subcommand> Run a local, read-only query analysis report
  click-dog test-span            Send a synthetic test span to verify export pipeline
  click-dog flush                Export current lookback window and exit
  click-dog self-update [flags]  Update to the latest release
  click-dog create-dashboards    Create the Datadog dashboards via API
  click-dog deploy <subcommand>  Install / inspect a click-dog deployment
  click-dog version              Print version and exit
  click-dog help                 Show this help and exit

Modes:
  (default)                       Scheduled monitoring — polls ClickHouse on an interval
  -backfill-start / -backfill-end One-shot export of a historical time range
  -validate                       Validate configuration offline and exit (no ClickHouse/exporter calls)
  -version                        Print version and exit
  --dry-run                       Read real data but discard exports; print summary

Examples:
  click-dog -config /etc/click-dog/config.yaml
  click-dog -validate -config /etc/click-dog/config.yaml
  click-dog --dry-run -config /etc/click-dog/config.yaml
  click-dog init --ch-host clickhouse.local --collector otel:4317
  click-dog check -config /etc/click-dog/config.yaml
  click-dog -backfill-start 2024-01-01T00:00:00Z -backfill-end 2024-01-02T00:00:00Z
`

// printVersion writes the canonical version line. Shared by the `version` verb
// and the `-version` flag so the two formats can never drift apart.
func printVersion(w io.Writer) {
	_, _ = fmt.Fprintln(w, "click-dog "+version)
}

// printUsage writes the command banner. Shared by the `help` verb and -h
// (flag.Usage) so they document the same surface.
func printUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, usageBanner, version)
}

// dispatch routes a leading non-flag verb (argv[1]) before flag.Parse() runs:
// a registered subcommand to its handler, or the built-in `version` / `help`
// verbs to their printers. It returns fellThrough=true only when there is no
// verb to handle — no args, or a leading flag — signalling main() to proceed
// into flag parsing and the default scheduled mode. Any other non-flag arg is
// treated as a typo (e.g. `cehck` for `check`) and reported as an error
// rather than silently falling through to start the daemon. When a verb is
// handled, the returned code is the intended process exit code.
func dispatch(args []string, out, errOut io.Writer) (exitCode int, fellThrough bool) {
	if len(args) < 2 || strings.HasPrefix(args[1], "-") {
		return 0, true
	}
	cmd := args[1]
	if handler, ok := subcommands[cmd]; ok {
		return handler(args[2:], out, errOut), false
	}
	switch cmd {
	case "version":
		printVersion(out)
		return 0, false
	case "help":
		printUsage(out)
		return 0, false
	}
	_, _ = fmt.Fprintf(errOut, "click-dog: unknown command %q\nRun 'click-dog -h' for usage.\n", cmd)
	return 2, false
}

func main() {
	// Dispatch subcommands before flag.Parse().
	if code, fellThrough := dispatch(os.Args, os.Stdout, os.Stderr); !fellThrough {
		os.Exit(code)
	}

	flag.Usage = func() {
		printUsage(os.Stderr)
		// Blank line separates the banner's Examples block from the flag
		// table; lives here (not in usageBanner) so the `help` verb's banner
		// stays tight and only -h pays for the table separator.
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Flags:")
		flag.PrintDefaults()
	}

	configPath := flag.String("config", config.DefaultConfigFlag, "Path to configuration file")
	backfillStart := flag.String("backfill-start", "", "Start time for backfill (RFC3339, e.g. 2024-01-01T00:00:00Z)")
	backfillEnd := flag.String("backfill-end", "", "End time for backfill (RFC3339, e.g. 2024-01-01T23:59:59Z)")
	showVersion := flag.Bool("version", false, "Print version and exit")
	validateOnly := flag.Bool("validate", false, "Validate configuration and exit")
	dryRun := flag.Bool("dry-run", false, "Read real data but discard exports; print summary")

	// Unknown flags fail with the standard flag package error and exit 2.
	// If a flag is removed in the future, the removal commit must add an
	// explicit deprecation handler — silent compat shims would mask typos
	// like `--cfg` for `--config`.
	flag.Parse()

	if *showVersion {
		printVersion(os.Stdout)
		os.Exit(0)
	}

	resolvedPath, err := config.ResolveConfigPath(*configPath)
	if err != nil {
		log.Fatalf("Error: %v", err)
	}
	cfg, err := config.LoadConfig(resolvedPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Log load-time warnings (always, not just validate mode)
	loadWarnings := append(append(append([]string{}, cfg.DeprecationWarnings...), cfg.EnvWarnings...), cfg.ValidationWarnings...)
	for _, w := range loadWarnings {
		log.Printf("WARN: %s", w)
	}

	if *validateOnly {
		if len(loadWarnings) > 0 {
			fmt.Println("Warnings:")
			for _, w := range loadWarnings {
				fmt.Printf("  WARN: %s\n", w)
			}
			fmt.Println()
		}
		fmt.Printf("Config %s is valid.\n", resolvedPath)
		fmt.Printf("  ClickHouse:  %s:%d\n", cfg.ClickHouse.Host, cfg.ClickHouse.Port)
		fmt.Printf("  Exporters:   %d OTEL, %d Splunk HEC\n",
			len(cfg.Exporters.OTEL), len(cfg.Exporters.SplunkHEC))
		for i, o := range cfg.Exporters.OTEL {
			fmt.Printf("    OTEL[%d]:       %s (service=%s)\n", i, o.CollectorAddress, o.ServiceName)
		}
		for i, s := range cfg.Exporters.SplunkHEC {
			fmt.Printf("    SplunkHEC[%d]:  %s\n", i, s.Endpoint)
		}
		fmt.Printf("  Monitor:     min_trace=%dms, interval=%ds\n",
			cfg.Monitor.MinTraceDurationMs, cfg.Monitor.CheckIntervalS)
		if cfg.HA.Active() {
			fmt.Printf("  HA:          leader election (keeper: %v)\n", cfg.HA.Keeper.Hosts)
		}
		if cfg.Metrics.Enabled {
			fmt.Printf("  Metrics:     %s (admin: %s)\n", cfg.Metrics.ListenAddress, cfg.Metrics.AdminListenAddress)
		}
		if cfg.Health.Enabled {
			fmt.Printf("  Health:      %s\n", cfg.Health.ListenAddress)
		}
		if cfg.Webhook.Enabled {
			fmt.Printf("  Webhook:     enabled (%d events)\n", len(cfg.Webhook.Events))
		}
		os.Exit(0)
	}

	// Catch partial backfill flags — one without the other is always a mistake
	if (*backfillStart == "") != (*backfillEnd == "") {
		log.Fatalf("Both -backfill-start and -backfill-end are required for backfill mode")
	}

	// Initialize logger
	if err := clicklog.InitLogger(cfg.LogLevel, cfg.LogFile, cfg.LogFormat, cfg.LogRotation.MaxSizeMB, cfg.LogRotation.MaxFiles); err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}
	defer clicklog.CloseLogger()

	if !cfg.ClickHouse.Secure {
		clicklog.Info("ClickHouse connection: plaintext (secure: false)")
	}
	clicklog.Info("Click-Dog starting up")
	clicklog.Info("Configuration: min_trace_duration=%dms, min_span_duration=%dms, max_trace_duration=%dms, max_span_duration=%dms",
		cfg.Monitor.MinTraceDurationMs, cfg.Monitor.MinSpanDurationMs,
		cfg.Monitor.MaxTraceDurationMs, cfg.Monitor.MaxSpanDurationMs)
	clicklog.Info("ClickHouse: %s:%d (cluster_mode=%v, max_conns=%d, query_timeout=%ds)",
		cfg.ClickHouse.Host, cfg.ClickHouse.Port, cfg.ClickHouse.UseClusterQueries,
		cfg.ClickHouse.MaxOpenConns, cfg.ClickHouse.QueryTimeoutS)
	// Log exporter configuration
	for _, o := range cfg.Exporters.OTEL {
		clicklog.Info("OTEL Collector: %s (service=%s)", o.CollectorAddress, o.ServiceName)
	}
	for _, s := range cfg.Exporters.SplunkHEC {
		clicklog.Info("Splunk HEC: %s", s.Endpoint)
	}

	// Initialize observability: metrics + webhook
	flushChan := make(chan struct{}, 1)

	// Always build the metrics collector so cycle accounting (used by /status)
	// works even when the /metrics endpoint is disabled. Cost is a single
	// struct; no goroutines or sockets are created until a server is started.
	m := metrics.NewMetrics()

	wh := webhook.NewWebhookNotifier(cfg.Webhook)

	// Initialize components
	chReader, err := clickhouse.NewClickHouseReader(cfg.ClickHouse, cfg.Filters)
	if err != nil {
		clicklog.Fatal("Failed to initialize ClickHouse reader: %v", err)
	}
	defer func() { _ = chReader.Close() }()

	// Mirror the capability probe result into a 0/1 metric so dashboards
	// can correlate "normalized query attributes missing on spans" with
	// the explicit ClickHouse capability state (#183). The reader probes
	// once at construction; we propagate the result and never re-probe.
	m.SetNormalizedQuerySupported(chReader.QueryLogNormalizedSupported())

	// Wire up HTTP listeners for metrics and health. Three possible paths:
	//   1. metrics enabled, health mounts on same mux (one listener)
	//   2. metrics enabled, health on a separate mux (two listeners)
	//   3. metrics disabled, health stands alone (one listener)
	// healthAdapter is nil when health is disabled, which collapses all three
	// paths down to just "metrics server" or "nothing".
	var healthAdapter *health.Server
	var clusterSrc *clusterSource
	if cfg.Health.Enabled {
		healthAdapter = health.NewServer(chReader, metricsCycleSource{m}, version)
		if cfg.Health.Cluster.Enabled {
			clusterSrc = newClusterSource(cfg.HA.Active(), cfg.Health.Cluster.Peers)
			healthAdapter.EnableCluster(health.ClusterConfig{
				Source:      clusterSrc,
				Self:        cfg.Health.Cluster.Self,
				PeerTimeout: time.Duration(cfg.Health.Cluster.PeerTimeoutMs) * time.Millisecond,
			})
			clicklog.Info("Cluster aggregate health enabled at /clusterz: self=%s, %d peers, peer_timeout=%dms",
				cfg.Health.Cluster.Self, len(cfg.Health.Cluster.Peers), cfg.Health.Cluster.PeerTimeoutMs)
		}
	}

	shared := cfg.Metrics.Enabled && healthAdapter != nil &&
		sameListenAddress(cfg.Health.ListenAddress, cfg.Metrics.ListenAddress)

	// Shutdown order: defers run LIFO, so each later defer below shuts down
	// before the earlier ones. We want this final teardown sequence on
	// SIGTERM:
	//   1. health  — fail probes first so K8s drops the pod from Service
	//                endpoints; later scrapes/admin calls won't reach us.
	//   2. admin   — close /flush; loopback only, nothing to drain.
	//   3. metrics — last so a final in-flight Prometheus scrape completes.
	// Source-order is therefore: metrics → admin → health, which reverses
	// to the intended LIFO sequence above. If you add another HTTP listener
	// or reorder these defers, preserve that mapping.
	//
	// Caveat: this only applies to the non-shared path (separate listeners).
	// When `shared` is true the health endpoints mount on metricsSrv behind
	// a single defer, so health and metrics come down together — K8s gets
	// no advance readiness failure before the scrape is interrupted.
	// Operators who care about drain ordering should use distinct
	// listen_addresses.
	//
	// This ordering is correctness-critical but not covered by an automated
	// test — exercising graceful shutdown sequencing in-process is awkward.
	// If you reorder these defers, the consequence is that Prometheus may
	// miss its final scrape on SIGTERM. Verify manually with a sidecar
	// rollout if you change anything here.
	//
	// TODO: integration test for shutdown ordering. A refactor (e.g. an
	// extracted shutdownGroup helper) would break the contract silently
	// today. An end-to-end test that boots all three servers, sends SIGTERM,
	// and checks health-closes-before-admin-closes-before-metrics would
	// make the invariant enforceable at CI time.
	if cfg.Metrics.Enabled {
		var extensions []metrics.MuxExtension
		if shared {
			extensions = append(extensions, healthAdapter.RegisterHandlers)
			clicklog.Info("Health endpoints share the metrics listener at %s", cfg.Metrics.ListenAddress)
		}
		metricsSrv, err := metrics.StartMetricsServer(cfg.Metrics.ListenAddress, m, extensions...)
		if err != nil {
			clicklog.Fatal("Failed to start metrics server: %v", err)
		}
		// (1/3) Scrape listener — shuts down LAST under LIFO so a final
		// Prometheus scrape can complete after readiness has already failed.
		defer shutdownHTTPServer(metricsSrv)

		adminSrv, err := metrics.StartAdminServer(cfg.Metrics.AdminListenAddress, flushChan)
		if err != nil {
			clicklog.Fatal("Failed to start admin server: %v", err)
		}
		// (2/3) Admin listener — shuts down BETWEEN health and metrics.
		// /flush has no scraper to drain, so the order between admin and
		// metrics is preference, not correctness; closing /flush before
		// /metrics avoids accepting a flush request that would race the
		// processor's own shutdown path.
		defer shutdownHTTPServer(adminSrv)
	}

	if healthAdapter != nil && !shared {
		healthSrv, err := health.Start(cfg.Health.ListenAddress, healthAdapter)
		if err != nil {
			clicklog.Fatal("Failed to start health server: %v", err)
		}
		// (3/3) Health listener — shuts down FIRST so /readyz starts failing
		// before metrics is torn down. See shutdown-ordering block above.
		defer shutdownHTTPServer(healthSrv)
	}

	// Build exporters
	var exporters []model.SpanExporter
	var exporterNames []string
	var otelExporters []*export.OTELExporter

	for i, otelCfg := range cfg.Exporters.OTEL {
		exp, otelErr := export.NewOTELExporter(otelCfg)
		if otelErr != nil {
			clicklog.Fatal("Failed to initialize OTEL exporter [%d]: %v", i, otelErr)
		}
		exporters = append(exporters, exp)
		otelExporters = append(otelExporters, exp)
		exporterNames = append(exporterNames, fmt.Sprintf("otel[%d]:%s", i, otelCfg.CollectorAddress))
	}
	for i, splunkCfg := range cfg.Exporters.SplunkHEC {
		exp, splunkErr := export.NewSplunkHECExporter(splunkCfg)
		if splunkErr != nil {
			clicklog.Fatal("Failed to initialize Splunk HEC exporter [%d]: %v", i, splunkErr)
		}
		exporters = append(exporters, exp)
		exporterNames = append(exporterNames, fmt.Sprintf("splunk_hec[%d]:%s", i, splunkCfg.Endpoint))
	}

	// Wrap in MultiExporter if multiple, or use directly if single
	var exporter model.SpanExporter
	var dryRunExp *DryRunExporter
	if *dryRun {
		dryRunExp = NewDryRunExporter()
		exporter = dryRunExp
		clicklog.Info("Dry-run mode: exports will be discarded")
	} else if len(exporters) == 1 {
		exporter = exporters[0]
	} else {
		exporter = export.NewMultiExporter(exporters, exporterNames)
		clicklog.Info("Multi-sink export enabled: %d exporters", len(exporters))
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), exporterCloseTimeout)
		defer cancel()
		if err := exporter.Close(ctx); err != nil {
			clicklog.Error("Error closing exporter(s): %v", err)
		}
	}()

	if cfg.Metrics.OTLP.Enabled {
		var metricsConn *grpc.ClientConn
		var closeMetricsConn func()
		if cfg.Metrics.OTLP.InheritOTELConnection {
			if len(otelExporters) == 0 {
				clicklog.Fatal("metrics.otlp.inherit_otel_connection=true but no OTEL exporter was initialized")
			}
			metricsConn = otelExporters[0].Conn()
		} else {
			metricsOTELCfg := config.OTELConfig{
				CollectorAddress: cfg.Metrics.OTLP.CollectorAddress,
				Secure:           cfg.Metrics.OTLP.Secure,
			}
			conn, connErr := export.NewOTELGRPCConn(metricsOTELCfg)
			if connErr != nil {
				clicklog.Fatal("Failed to initialize OTLP self-metrics connection: %v", connErr)
			}
			metricsConn = conn
			closeMetricsConn = func() {
				done := make(chan error, 1)
				go func() { done <- conn.Close() }()
				ctx, cancel := context.WithTimeout(context.Background(), exporterCloseTimeout)
				defer cancel()
				select {
				case err := <-done:
					if err != nil {
						clicklog.Error("Error closing OTLP self-metrics connection: %v", err)
					}
				case <-ctx.Done():
					clicklog.Error("Timed out closing OTLP self-metrics connection: %v", ctx.Err())
				}
			}
			defer closeMetricsConn()
		}

		metricsExporter, metricsErr := export.NewOTLPMetricsExporter(context.Background(), export.OTLPMetricsOptions{
			Metrics:        m,
			Conn:           metricsConn,
			Interval:       time.Duration(cfg.Metrics.OTLP.IntervalSeconds) * time.Second,
			ExportTimeout:  time.Duration(cfg.Monitor.ExportTimeoutS) * time.Second,
			Host:           cfg.Metrics.OTLP.Host,
			ServiceName:    cfg.Metrics.OTLP.ServiceName,
			ServiceVersion: version,
			SinkNames:      otlpMetricsSinkNames(cfg, *dryRun),
			Rename:         cfg.Metrics.OTLP.Rename,
		})
		if metricsErr != nil {
			clicklog.Fatal("Failed to initialize OTLP self-metrics exporter: %v", metricsErr)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), exporterCloseTimeout)
			defer cancel()
			if err := metricsExporter.Shutdown(ctx); err != nil {
				clicklog.Error("Error shutting down OTLP self-metrics exporter: %v", err)
			}
		}()
	}

	f, err := filter.NewQueryFilter(cfg.Filters)
	if err != nil {
		clicklog.Fatal("Failed to initialize query filter: %v", err)
	}
	if len(cfg.Filters.BlacklistOperations) > 0 {
		clicklog.Info("Operation blacklist: %d patterns active (SQL-level filtering)", len(cfg.Filters.BlacklistOperations))
	}

	ctx := context.Background()

	wh.Notify(webhook.EventStartup, fmt.Sprintf("Click-Dog starting up (host=%s:%d)", cfg.ClickHouse.Host, cfg.ClickHouse.Port))

	// Check if backfill mode
	if *backfillStart != "" && *backfillEnd != "" {
		err := runBackfillMode(ctx, chReader, exporter, f, cfg, *backfillStart, *backfillEnd, wh, m)
		// Must precede Fatal so dry-run output isn't lost on failure.
		if dryRunExp != nil {
			dryRunExp.PrintSummary()
		}
		if err != nil {
			clicklog.Fatal("%v", err)
		}
		return
	}

	// Scheduled mode
	runScheduledMode(ctx, chReader, exporter, f, cfg, m, wh, dryRunExp, flushChan, clusterSrc)
}

func runBackfillMode(
	ctx context.Context,
	chReader *clickhouse.ClickHouseReader,
	exporter model.SpanExporter,
	f *filter.QueryFilter,
	cfg *config.Config,
	startStr, endStr string,
	wh *webhook.WebhookNotifier,
	m *metrics.Metrics,
) error {
	startTime, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		return fmt.Errorf("invalid backfill-start time format (use RFC3339, e.g., 2024-01-01T00:00:00Z): %w", err)
	}

	endTime, err := time.Parse(time.RFC3339, endStr)
	if err != nil {
		return fmt.Errorf("invalid backfill-end time format (use RFC3339, e.g., 2024-01-01T23:59:59Z): %w", err)
	}

	if startTime.After(endTime) {
		return errors.New("backfill-start must be before backfill-end")
	}

	clicklog.Info("Starting backfill mode: %s to %s (min_trace_duration: %dms)",
		startTime.Format(time.RFC3339), endTime.Format(time.RFC3339), cfg.Monitor.MinTraceDurationMs)

	// Backfill is intentionally NOT leader-gated. It is a one-shot, explicitly
	// operator-invoked historical export on a chosen host, and runs no election
	// (it returns before runScheduledMode), so there is no leadership to consult.
	// Any overlap with what a cluster leader already exported for the window is
	// absorbed by downstream (trace_id, span_id) dedup — consistent with the
	// best-effort contract. The leader gate applies only to the scheduled loop.

	// Fetch queries in range
	queries, err := chReader.FetchSlowQueriesInRange(
		ctx,
		cfg.Monitor.MinTraceDurationMs,
		cfg.Monitor.MaxTraceDurationMs,
		cfg.Monitor.MaxQueryLength,
		startTime,
		endTime,
		cfg.Monitor.MaxSpansPerCycle,
	)
	if err != nil {
		return fmt.Errorf("error fetching queries: %w", err)
	}

	clicklog.Info("Found %d slow queries in range", len(queries))

	result := processor.ProcessQueriesBatch(ctx, queries, exporter, f, cfg, m)

	return processor.ReportBackfillOutcome(startStr, endStr, result, wh)
}

func runScheduledMode(
	ctx context.Context,
	chReader *clickhouse.ClickHouseReader,
	exporter model.SpanExporter,
	f *filter.QueryFilter,
	cfg *config.Config,
	m *metrics.Metrics,
	wh *webhook.WebhookNotifier,
	dryRunExp *DryRunExporter,
	flushChan chan struct{},
	clusterSrc *clusterSource,
) {
	if !cfg.Monitor.Enabled {
		clicklog.Fatal("Scheduled monitoring is disabled in config. Use -backfill-start and -backfill-end for one-time backfill, or set monitor.enabled: true")
	}

	clicklog.Info("Starting scheduled mode: min_trace_duration=%dms, interval=%ds, lookback=%ds (interval=%d + buffer=%d)",
		cfg.Monitor.MinTraceDurationMs, cfg.Monitor.CheckIntervalS, cfg.Monitor.LookbackS,
		cfg.Monitor.CheckIntervalS, cfg.Monitor.LookbackBufferS)

	// Set up graceful shutdown
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// SIGUSR1 feeds into the shared flushChan
	sigusr1 := make(chan os.Signal, 1)
	signal.Notify(sigusr1, syscall.SIGUSR1)
	go func() {
		for range sigusr1 {
			select {
			case flushChan <- struct{}{}:
			default:
			}
		}
	}()

	// Track last processed spans to avoid duplicates, keyed by the
	// composite OTLP span identity (trace_id, span_id). OTLP span IDs
	// are only unique within a trace, so keying off span_id alone would
	// silently drop sibling spans in different traces that collide on
	// the 64-bit ID.
	//
	// The LRU cache is in-memory only — it is NOT persisted across restarts.
	// After a restart the cache is empty, so spans still inside the lookback
	// window will be re-exported once.  This is acceptable because:
	//   1. OTEL collectors / backends dedup by (trace_id, span_id).
	//   2. The alternative (disk persistence) adds crash-recovery complexity
	//      that is not justified for a best-effort monitoring sidecar.
	seenSpans, err := lru.New[model.SpanKey, bool](cfg.Monitor.DedupCacheSize)
	if err != nil {
		clicklog.Fatal("Failed to create LRU cache: %v", err)
	}
	clicklog.Info("Deduplication cache size: %d (in-memory only, not persisted across restarts)", cfg.Monitor.DedupCacheSize)

	// Initialize circuit breaker to protect ClickHouse
	var circuitBreaker *resilience.CircuitBreaker
	if cfg.Monitor.CircuitBreaker.Enabled {
		circuitBreaker = resilience.NewCircuitBreaker(cfg.Monitor.CircuitBreaker)
		clicklog.Info("Circuit breaker enabled: failure_threshold=%d, reset_timeout=%ds",
			cfg.Monitor.CircuitBreaker.FailureThreshold, cfg.Monitor.CircuitBreaker.ResetTimeoutS)
	}

	// Initialize adaptive backoff
	var adaptivePoller *resilience.AdaptivePoller
	baseInterval := cfg.GetCheckInterval()
	if cfg.Monitor.Backoff.Enabled {
		adaptivePoller = resilience.NewAdaptivePoller(baseInterval, cfg.Monitor.Backoff)
		clicklog.Info("Adaptive backoff enabled: max_interval=%ds, factor=%.1f",
			cfg.Monitor.Backoff.MaxIntervalS, cfg.Monitor.Backoff.BackoffFactor)
	}

	m.SetBackoffInterval(baseInterval)

	// Initialize leader election if HA is enabled.
	// NOTE: During Keeper partitions, multiple instances may briefly act as
	// leader, producing duplicate exports. This is safe because OTEL collectors
	// and backends dedup by (trace_id, span_id). See leader.LeaderElection docs.
	//
	// Hoisted to function scope so the cluster-mode export gate (built below)
	// can consult it. Stays nil when HA is off, or when Keeper is unreachable at
	// startup (standalone fallback) — both leave a cluster reader exporting.
	var election *leader.LeaderElection
	if cfg.HA.Active() {
		clicklog.Info("Keeper hosts configured — joining leader election: %v", cfg.HA.Keeper.Hosts)
		el, electionErr := leader.NewLeaderElection(
			cfg.HA.Keeper,
			func() { clicklog.Info("Promoted to leader — taking coordination duties") },
			func() { clicklog.Info("Demoted from leader — releasing coordination duties") },
		)
		if electionErr != nil {
			clicklog.Warn("Failed to join leader election, running in standalone mode: %v", electionErr)
		} else {
			election = el
			// Hand the live election to the /clusterz adapter so IsLeader
			// flips from "not yet" to the real Keeper-driven verdict.
			if clusterSrc != nil {
				clusterSrc.SetElection(election)
			}
			// Run election in background
			electionCtx, electionCancel := context.WithCancel(ctx)
			defer electionCancel()
			go func() {
				if runErr := election.Run(electionCtx); runErr != nil {
					clicklog.Error("Leader election error: %v", runErr)
				}
			}()
			defer func() {
				if resignErr := election.Resign(); resignErr != nil {
					clicklog.Error("Error resigning from election: %v", resignErr)
				}
			}()
			clicklog.Info("HA mode active: base_path=%s, session_timeout=%ds",
				cfg.HA.Keeper.BasePath, cfg.HA.Keeper.SessionTimeout)
			// Watch for flush requests via Keeper
			go election.WatchFlush(electionCtx, flushChan)
		}
	}

	// Cluster-mode export gate. In cluster mode (use_cluster_queries) the
	// fetch/export cycle runs only on the election leader; sidecar / single
	// readers get a nil gate and always export. election is nil for a
	// Keeper-less cluster or when Keeper was unreachable at startup, which the
	// gate treats as "always export" (Option A / standalone fallback).
	var leaderHandle leadership
	if election != nil {
		leaderHandle = election
	}
	leaderGate := newLeaderGate(cfg.ClickHouse.UseClusterQueries, leaderHandle)
	if leaderGate != nil {
		clicklog.Info("Cluster mode: export is leader-gated (one active exporter, others stand by)")
	}

	// Heartbeat emits a periodic summary at Info level so operators can confirm
	// click-dog is alive without per-cycle log noise. Interval = 5 minutes.
	hb := processor.NewHeartbeat(heartbeatInterval, m.Snapshot)
	pipeline, pipelineErr := processor.NewPipeline(processor.Pipeline{
		Reader:         chReader,
		Exporter:       exporter,
		Filter:         f,
		Config:         cfg,
		SeenSpans:      seenSpans,
		CircuitBreaker: circuitBreaker,
		Heartbeat:      hb,
		Poller:         adaptivePoller,
		CanaryQuerier:  chReader,
		Metrics:        m,
		Webhook:        wh,
		LeaderGate:     leaderGate,
	})
	if pipelineErr != nil {
		clicklog.Fatal("Failed to initialize processor pipeline: %v", pipelineErr)
	}

	// Topology self-audit: an observability-only backstop that detects the
	// sidecar + use_cluster_queries anti-pattern (N× duplicate exports). Hard
	// no-op unless this instance is itself doing cluster reads and the audit is
	// enabled. Same context.WithCancel + deferred-cancel pattern as leader
	// election; never gates readiness.
	if selfaudit.ShouldAudit(cfg) {
		auditor := selfaudit.New(selfaudit.Options{
			Host:          cfg.ClickHouse.Host,
			Interval:      time.Duration(cfg.Monitor.TopologyAudit.IntervalS) * time.Second,
			Debounce:      cfg.Monitor.TopologyAudit.DebounceCount,
			Lookback:      time.Duration(cfg.Monitor.TopologyAudit.QueryLogLookbackMinutes) * time.Minute,
			CheckInterval: cfg.GetCheckInterval(),
			Reader:        chReader,
			Sink:          m,
		})
		auditCtx, auditCancel := context.WithCancel(ctx)
		defer auditCancel()
		go auditor.Run(auditCtx)
		clicklog.Info("Topology self-audit active (interval=%ds, debounce=%d) — observability-only backstop for duplicate cluster-wide reads",
			cfg.Monitor.TopologyAudit.IntervalS, cfg.Monitor.TopologyAudit.DebounceCount)
	}

	// Run one cycle eagerly at startup so the first iteration of the loop
	// produces a real success/failure signal that feeds into the poller's
	// initial backoff state. This also doubles as a smoke test that the
	// full ClickHouse → filter → OTEL pipeline is wired correctly before
	// we enter the steady-state loop.
	startupErr := pipeline.Process(ctx)
	switch {
	case errors.Is(startupErr, processor.ErrLeaderStandby):
		clicklog.Info("Startup check: standby (not leader) — export gated, no work this cycle")
	case startupErr != nil:
		clicklog.Warn("Startup check failed: %v", startupErr)
	default:
		clicklog.Info("Startup check passed")
	}

	// Main monitoring loop with adaptive timing
	ticker := time.NewTicker(baseInterval)
	defer ticker.Stop()
	processor.UpdatePollerState(adaptivePoller, startupErr, ticker, baseInterval, m)

	// In dry-run mode, run one cycle and print summary
	if dryRunExp != nil {
		dryRunExp.PrintSummary()
		return
	}

	for {
		select {
		case <-ticker.C:
			runErr := pipeline.Process(ctx)
			processor.UpdatePollerState(adaptivePoller, runErr, ticker, baseInterval, m)

		case <-flushChan:
			clicklog.Info("Flush requested — running immediate cycle")
			runErr := pipeline.Process(ctx)
			processor.UpdatePollerState(adaptivePoller, runErr, ticker, baseInterval, m)
			// Reset ticker so the next regular cycle starts from now
			ticker.Reset(baseInterval)

		case <-sigChan:
			clicklog.Info("Shutting down gracefully...")
			// Best-effort: Notify fires a goroutine that may not complete before return.
			wh.Notify(webhook.EventShutdown, "Click-Dog shutting down")
			return
		}
	}
}
