package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/export"
)

// runTestTLS is `click-dog test tls`: one TLS handshake with an OTLP receiver,
// made by the exporter's own TLS code, and a verdict on whether export would
// accept the receiver. deploy/install.sh runs it (with --json and
// --chain-out) for every OTLP TLS decision, so the installer and the service
// cannot disagree about a certificate.
func runTestTLS(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("test tls", flag.ContinueOnError)
	fs.SetOutput(errOut)
	collector := fs.String("collector", "", "OTLP receiver address, written as exporters.otel[].collector_address")
	caCert := fs.String("ca-cert", "", "Trust only this PEM CA file, as exporters.otel[].ca_cert does (default: the system trust store)")
	clientCert := fs.String("client-cert", "", "Client certificate (PEM) to present when the receiver asks, as exporters.otel[].client_cert; needs --client-key")
	clientKey := fs.String("client-key", "", "Key for --client-cert, as exporters.otel[].client_key")
	configPath := fs.String("config", "", "Check exporters.otel[--exporter] from this config file with all its TLS settings, instead of --collector")
	exporterIdx := fs.Int("exporter", 0, "With --config, the exporters.otel entry to check, counting from 0")
	timeout := fs.Duration("timeout", 10*time.Second, "Bound for the whole check")
	asJSON := fs.Bool("json", false, "Print the result as one JSON object")
	chainOut := fs.String("chain-out", "", "Write the certificates the receiver sent to this file as PEM, leaf first")

	fs.Usage = func() {
		_, _ = fmt.Fprint(errOut, `click-dog test tls — check an OTLP receiver's TLS the way the exporter does

Dials the receiver once and completes the TLS handshake with click-dog's own
exporter TLS code: the certificate verified against the system trust store,
or against --ca-cert alone; a subjectAltName naming the dialed host (the
Common Name is never used); ALPN h2, which the gRPC exporter requires; and,
when the receiver asks, the client certificate given. Nothing is exported.

With --config, the address and every TLS setting (ca_cert, client_cert,
client_key, insecure_skip_verify) come from one exporters.otel entry, so the
check is exactly what that exporter would do.

Verdicts: ok, plaintext, no-san, untrusted, client-cert, no-alpn, unknown.

Usage:
  click-dog test tls --collector HOST:PORT [--ca-cert FILE] [--client-cert FILE --client-key FILE] [flags]
  click-dog test tls --config FILE [--exporter N] [flags]

Exit status: 0 when the exporter would accept the receiver, 1 otherwise
(including an inconclusive check), 2 for invalid usage.

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
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog test tls: unexpected argument %q\n\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintln(errOut, "click-dog test tls: --timeout must be greater than zero")
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	var cfg config.OTELConfig
	source := ""
	if *configPath != "" {
		for _, name := range []string{"collector", "ca-cert", "client-cert", "client-key"} {
			if set[name] {
				_, _ = fmt.Fprintf(errOut, "click-dog test tls: --%s cannot be combined with --config, which supplies it\n", name)
				return 2
			}
		}
		full, resolved, err := loadConfig(*configPath)
		if err != nil {
			_, _ = fmt.Fprintf(errOut, "click-dog test tls: config: %v\n", err)
			return 2
		}
		entries := full.Exporters.OTEL
		if *exporterIdx < 0 || *exporterIdx >= len(entries) {
			_, _ = fmt.Fprintf(errOut, "click-dog test tls: --exporter %d: %s has %d exporters.otel entries\n", *exporterIdx, resolved, len(entries))
			return 2
		}
		cfg = entries[*exporterIdx]
		if !cfg.Secure {
			_, _ = fmt.Fprintf(errOut, "click-dog test tls: exporters.otel[%d] in %s uses %s, so it does not use TLS.\n"+
				"  To see whether its receiver speaks TLS, run: click-dog test tls --collector %s\n", *exporterIdx, resolved,
				config.TransportSummary(cfg.CollectorAddress, cfg.Secure, cfg.SecureSource), cfg.CollectorAddress)
			return 2
		}
		source = fmt.Sprintf("exporters.otel[%d] in %s", *exporterIdx, resolved)
	} else {
		if set["exporter"] {
			_, _ = fmt.Fprintln(errOut, "click-dog test tls: --exporter needs --config")
			return 2
		}
		if strings.TrimSpace(*collector) == "" {
			_, _ = fmt.Fprintln(errOut, "click-dog test tls: --collector or --config is required")
			return 2
		}
		if (*clientCert == "") != (*clientKey == "") {
			_, _ = fmt.Fprintln(errOut, "click-dog test tls: --client-cert and --client-key go together")
			return 2
		}
		cfg = config.OTELConfig{CollectorAddress: *collector, Secure: true, CACert: *caCert, ClientCert: *clientCert, ClientKey: *clientKey}
	}

	res := export.ProbeOTELTLS(context.Background(), cfg, *timeout)

	if *chainOut != "" {
		if err := os.WriteFile(*chainOut, []byte(renderTLSChain(res)), 0o600); err != nil {
			_, _ = fmt.Fprintf(errOut, "click-dog test tls: write --chain-out: %v\n", err)
			return 1
		}
	}

	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		// Keep <, > and & literal: the object is read by people and shells,
		// not embedded in HTML.
		enc.SetEscapeHTML(false)
		if err := enc.Encode(res); err != nil {
			_, _ = fmt.Fprintf(errOut, "click-dog test tls: encode JSON: %v\n", err)
			return 1
		}
	} else {
		printTLSReport(out, res, source)
	}

	if res.Verdict == export.TLSVerdictOK {
		return 0
	}
	return 1
}

// tlsCertSummary is the labeled description of one certificate, shared by the
// text report and the --chain-out comments. install.sh shows these lines when
// it offers to trust a certificate, so the labels are part of the
// --chain-out contract.
func tlsCertSummary(c export.TLSProbeCert) []string {
	sans := strings.Join(c.SANs, ", ")
	if sans == "" {
		sans = "none"
	}
	return []string{
		"Subject:     " + c.Subject,
		"Issuer:      " + c.Issuer,
		"SANs:        " + sans,
		"Not before:  " + c.NotBefore.UTC().Format("2006-01-02 15:04:05 MST"),
		"Not after:   " + c.NotAfter.UTC().Format("2006-01-02 15:04:05 MST"),
		"SHA-256:     " + c.SHA256,
	}
}

// renderTLSChain is the --chain-out file: each certificate the receiver sent,
// leaf first, as PEM under "# Label: value" comment lines, which PEM readers
// (Go, openssl) skip. Empty when no certificate arrived.
func renderTLSChain(res export.TLSProbeResult) string {
	var b strings.Builder
	for i, c := range res.Certificates {
		fmt.Fprintf(&b, "# Certificate %d of %d sent by %s\n", i+1, len(res.Certificates), res.Address)
		for _, line := range tlsCertSummary(c) {
			b.WriteString("# " + line + "\n")
		}
		b.WriteString(c.PEM)
	}
	return b.String()
}

func printTLSReport(out io.Writer, res export.TLSProbeResult, source string) {
	_, _ = fmt.Fprintf(out, "OTLP receiver TLS check: %s\n", res.Address)
	if source != "" {
		_, _ = fmt.Fprintf(out, "  Settings:     %s\n", source)
	}
	if res.ServerName != "" {
		_, _ = fmt.Fprintf(out, "  Server name:  %s\n", res.ServerName)
	}
	switch {
	case res.InsecureSkipVerify:
		_, _ = fmt.Fprintln(out, "  Trust:        NONE: insecure_skip_verify is set, so the certificate is not verified")
	case res.CACert != "":
		_, _ = fmt.Fprintf(out, "  Trust:        %s only\n", res.CACert)
	default:
		_, _ = fmt.Fprintln(out, "  Trust:        system trust store")
	}
	if res.ClientCert != "" {
		_, _ = fmt.Fprintf(out, "  Client cert:  %s\n", res.ClientCert)
	}
	if res.TLSVersion != "" {
		alpn := res.ALPN
		if alpn == "" {
			alpn = "none"
		}
		_, _ = fmt.Fprintf(out, "  Negotiated:   %s, ALPN %s\n", res.TLSVersion, alpn)
	}

	if len(res.Certificates) > 0 {
		_, _ = fmt.Fprintln(out, "\nCertificates sent, leaf first:")
		for i, c := range res.Certificates {
			for j, line := range tlsCertSummary(c) {
				prefix := "     "
				if j == 0 {
					prefix = fmt.Sprintf("  %d. ", i+1)
				}
				_, _ = fmt.Fprintln(out, prefix+line)
			}
		}
	}

	_, _ = fmt.Fprintln(out)
	switch res.Verdict {
	case export.TLSVerdictOK:
		_, _ = fmt.Fprintf(out, "Result: PASS — the exporter would accept this receiver (%s).\n", res.Message)
		if res.InsecureSkipVerify {
			_, _ = fmt.Fprintln(out, "Warning: with insecure_skip_verify anything on the network path can impersonate the receiver. Set ca_cert instead.")
		}
		return
	case export.TLSVerdictUnknown:
		_, _ = fmt.Fprintf(out, "Result: INCONCLUSIVE — %s.\n", res.Message)
	default:
		_, _ = fmt.Fprintf(out, "Result: FAIL (%s) — %s.\n", res.Verdict, res.Message)
	}
	if fix := tlsFix(res); fix != "" {
		_, _ = fmt.Fprintf(out, "Fix: %s\n", fix)
	}
}

// tlsFix names the configuration that resolves a failed check.
func tlsFix(res export.TLSProbeResult) string {
	switch res.Verdict {
	case export.TLSVerdictPlaintext:
		if res.Reason == export.TLSReasonClosed {
			return fmt.Sprintf("if this port is plaintext, enable TLS on it or export in plaintext (secure: false); if it is a TLS proxy or load balancer, have it serve a certificate for %s.", res.ServerName)
		}
		return "the receiver does not speak TLS on this port. Enable TLS on it, or export in plaintext (secure: false)."
	case export.TLSVerdictNoSAN:
		return fmt.Sprintf("reissue the certificate with a subjectAltName for %s.", res.ServerName)
	case export.TLSVerdictClientCert:
		if res.ClientCert != "" {
			return "the receiver rejected the client certificate: use one issued by a CA it trusts, and check that it has not expired."
		}
		return "set exporters.otel[].client_cert and client_key (mTLS); check them first with --client-cert and --client-key."
	case export.TLSVerdictNoALPN:
		return "enable HTTP/2 on the receiver's TLS listener (often a load balancer or proxy setting), or point at the collector itself."
	case export.TLSVerdictUntrusted:
		switch res.Reason {
		case export.TLSReasonHostnameMismatch:
			return "use an address the certificate names, or reissue it for this one."
		case export.TLSReasonExpired, export.TLSReasonNotYetValid:
			return "renew the certificate, and check this host's clock."
		case export.TLSReasonUnknownAuthority:
			if res.CACert != "" {
				return "use the CA that issued the certificate (the certificate itself when self-signed)."
			}
			return "set exporters.otel[].ca_cert to the CA that issued the certificate (the certificate itself when self-signed)."
		}
	}
	return ""
}
