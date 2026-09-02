package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/coltconsulting/click-dog/internal/config"
)

// runValidate implements `click-dog validate`: offline config validation.
// It loads and validates the config file, prints the parsed-settings summary,
// and exits without contacting ClickHouse or any exporter. The legacy
// `-validate` flag delegates here so the two spellings cannot drift.
func runValidate(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(errOut)
	configPath := fs.String("config", config.DefaultConfigFlag, "Path to configuration file")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(errOut, `click-dog validate — validate configuration offline

Usage:
  click-dog validate [flags]

Loads and validates the config file, prints the parsed settings, and exits
without contacting ClickHouse or any exporter. For a live check that also
probes ClickHouse and exporter connectivity use `+"`click-dog check`"+`.

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
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog validate: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	cfg, resolvedPath, err := loadConfig(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "Invalid config: %v\n", err)
		return 1
	}

	warnings := append(append(append([]string{}, cfg.DeprecationWarnings...), cfg.EnvWarnings...), cfg.ValidationWarnings...)
	if len(warnings) > 0 {
		_, _ = fmt.Fprintln(out, "Warnings:")
		for _, w := range warnings {
			_, _ = fmt.Fprintf(out, "  WARN: %s\n", w)
		}
		_, _ = fmt.Fprintln(out)
	}
	_, _ = fmt.Fprintf(out, "Config %s is valid.\n", resolvedPath)
	_, _ = fmt.Fprintf(out, "  ClickHouse:  %s:%d\n", cfg.ClickHouse.Host, cfg.ClickHouse.Port)
	_, _ = fmt.Fprintf(out, "  Exporters:   %d OTEL, %d Splunk HEC\n",
		len(cfg.Exporters.OTEL), len(cfg.Exporters.SplunkHEC))
	for i, o := range cfg.Exporters.OTEL {
		_, _ = fmt.Fprintf(out, "    OTEL[%d]:       %s (service=%s)\n", i, o.CollectorAddress, o.ServiceName)
	}
	for i, s := range cfg.Exporters.SplunkHEC {
		_, _ = fmt.Fprintf(out, "    SplunkHEC[%d]:  %s\n", i, s.Endpoint)
	}
	_, _ = fmt.Fprintf(out, "  Monitor:     min_trace=%dms, interval=%ds\n",
		cfg.Monitor.MinTraceDurationMs, cfg.Monitor.CheckIntervalS)
	_, _ = fmt.Fprintf(out, "  Query text:  %s\n", cfg.Filters.EffectiveQueryTextMode())
	if cfg.HA.Active() {
		_, _ = fmt.Fprintf(out, "  HA:          leader election (keeper: %v)\n", cfg.HA.Keeper.Hosts)
	}
	if cfg.Metrics.Enabled {
		_, _ = fmt.Fprintf(out, "  Metrics:     %s (admin: %s)\n", cfg.Metrics.ListenAddress, cfg.Metrics.AdminListenAddress)
	}
	// Always print the self-metrics state, on or off. Its silence in this
	// summary is exactly why a blank Datadog Health dashboard is hard to
	// diagnose: the dashboard reads OTLP-pushed click_dog.* metrics, and
	// nothing here told the operator whether that push was configured.
	if cfg.Metrics.OTLP.Enabled {
		endpoint := cfg.Metrics.OTLP.CollectorAddress
		source := "standalone"
		if cfg.Metrics.OTLP.InheritOTELConnection {
			source = "inherit otel[0]"
			if len(cfg.Exporters.OTEL) > 0 {
				endpoint = cfg.Exporters.OTEL[0].CollectorAddress
			}
		}
		_, _ = fmt.Fprintf(out, "  Self-metrics: OTLP → %s (%s), %ds, host=%s\n",
			endpoint, source, cfg.Metrics.OTLP.IntervalSeconds, cfg.Metrics.OTLP.Host)
	} else {
		_, _ = fmt.Fprintf(out, "  Self-metrics: OTLP push off\n")
	}
	if cfg.Health.Enabled {
		_, _ = fmt.Fprintf(out, "  Health:      %s\n", cfg.Health.ListenAddress)
	}
	if cfg.Webhook.Enabled {
		_, _ = fmt.Fprintf(out, "  Webhook:     enabled (%d events)\n", len(cfg.Webhook.Events))
	}
	return 0
}
