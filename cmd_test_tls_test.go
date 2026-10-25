package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/export"
)

// selfSignedTLSReceiver serves TLS (ALPN h2) on loopback with a self-signed
// certificate carrying dnsNames and the 127.0.0.1 IP SAN, and returns its
// address, the certificate's DER, and a PEM file trusting it.
func selfSignedTLSReceiver(t *testing.T, dnsNames ...string) (addr string, der []byte, caPath string) {
	t.Helper()
	return selfSignedTLSReceiverWith(t, nil, dnsNames...)
}

// selfSignedTLSReceiverWith is selfSignedTLSReceiver with its server config
// adjusted by mod first.
func selfSignedTLSReceiverWith(t *testing.T, mod func(*tls.Config), dnsNames ...string) (addr string, der []byte, caPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test receiver"},
		DNSNames:     dnsNames,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err = x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caPath = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	cfg := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{"h2"},
	}
	if mod != nil {
		mod(cfg)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				tc := tls.Server(c, cfg)
				if tc.Handshake() == nil {
					_, _ = io.Copy(io.Discard, tc)
				}
			}(c)
		}
	}()
	return ln.Addr().String(), der, caPath
}

func TestRunTestTLS_InvalidUsage(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    int
		wantErr string
	}{
		{"help", []string{"-h"}, 0, "click-dog test tls --collector HOST:PORT"},
		{"no collector", nil, 2, "--collector or --config is required"},
		{"half a client pair", []string{"--collector", "x:1", "--client-cert", "c.pem"}, 2, "--client-cert and --client-key go together"},
		{"exporter without config", []string{"--collector", "x:1", "--exporter", "1"}, 2, "--exporter needs --config"},
		{"config with collector", []string{"--config", "c.yaml", "--collector", "x:1"}, 2, "--collector cannot be combined with --config"},
		{"config with client cert", []string{"--config", "c.yaml", "--client-cert", "c.pem"}, 2, "--client-cert cannot be combined with --config"},
		{"zero timeout", []string{"--collector", "x:1", "--timeout", "0s"}, 2, "--timeout must be greater than zero"},
		{"stray argument", []string{"--collector", "x:1", "extra"}, 2, `unexpected argument "extra"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := runTestTLS(tt.args, &out, &errOut); code != tt.want {
				t.Fatalf("exit = %d, want %d; err=%q", code, tt.want, errOut.String())
			}
			if !strings.Contains(errOut.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want %q", errOut.String(), tt.wantErr)
			}
		})
	}
}

func TestRunTest_ListsTLS(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runTest([]string{"-h"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "tls        Check an OTLP receiver's TLS") {
		t.Errorf("test help does not list tls:\n%s", out.String())
	}
	var tlsOut, tlsErr bytes.Buffer
	if code := runTest([]string{"tls"}, &tlsOut, &tlsErr); code != 2 {
		t.Errorf("test tls without --collector exit = %d, want 2", code)
	}
}

// install.sh reads verdict, reason and message with sed, not a JSON parser.
// Pin the shape it relies on: one top-level field per line at a two-space
// indent, and a message that is one line with no quotes or backslashes, even
// when the receiver's certificate names contain them.
func TestRunTestTLS_JSONLinesInstallShReads(t *testing.T) {
	addr, _, caPath := selfSignedTLSReceiver(t, `evil"name\x.example.com`)
	// Dial by name so the hostile SAN lands in the mismatch message.
	_, port, _ := net.SplitHostPort(addr)

	var out, errOut bytes.Buffer
	code := runTestTLS([]string{"--collector", "localhost:" + port, "--ca-cert", caPath, "--json"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; err=%q", code, errOut.String())
	}

	var res export.TLSProbeResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if res.Verdict != export.TLSVerdictUntrusted || res.Reason != export.TLSReasonHostnameMismatch {
		t.Fatalf("verdict = %q/%q (%s)", res.Verdict, res.Reason, res.Message)
	}

	// The same expressions otel_json_field uses in deploy/install.sh.
	for field, want := range map[string]string{"verdict": res.Verdict, "reason": res.Reason, "message": res.Message} {
		re := regexp.MustCompile(`(?m)^  "` + field + `": "([^"\\]*)",?$`)
		m := re.FindStringSubmatch(out.String())
		if m == nil {
			t.Fatalf("no shell-readable %q line in:\n%s", field, out.String())
		}
		if m[1] != want {
			t.Errorf("%s read by the shell = %q, decoded = %q", field, m[1], want)
		}
	}
	if !strings.Contains(res.Message, "evil'name?x.example.com") {
		t.Errorf("message = %q, want the SAN made shell-safe", res.Message)
	}
}

func TestRunTestTLS_ChainOut(t *testing.T) {
	addr, der, _ := selfSignedTLSReceiver(t)
	chain := filepath.Join(t.TempDir(), "chain")

	var out, errOut bytes.Buffer
	// No CA file: the system store rejects it, and the chain still arrives.
	if code := runTestTLS([]string{"--collector", addr, "--chain-out", chain}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d; out=%q err=%q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "Result: FAIL (untrusted)") || !strings.Contains(out.String(), "Fix: set exporters.otel[].ca_cert") {
		t.Errorf("report:\n%s", out.String())
	}

	data, err := os.ReadFile(chain)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, label := range []string{"# Certificate 1 of 1 sent by " + addr, "# Subject:     CN=test receiver", "# Issuer:      CN=test receiver",
		"# SANs:        IP Address:127.0.0.1", "# Not before:  ", "# Not after:   ", "# SHA-256:     "} {
		if !strings.Contains(text, label) {
			t.Errorf("chain file lacks %q:\n%s", label, text)
		}
	}
	// The comment lines are invisible to PEM readers: the block is the
	// certificate the receiver sent, byte for byte.
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || !bytes.Equal(block.Bytes, der) {
		t.Fatal("chain file does not decode to the receiver's certificate")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		t.Errorf("unexpected trailing content: %q", rest)
	}

	// Nothing sent, nothing written: a stale chain must not survive.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()
	if code := runTestTLS([]string{"--collector", closed, "--chain-out", chain}, io.Discard, io.Discard); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if data, _ := os.ReadFile(chain); len(data) != 0 {
		t.Errorf("chain file kept %d bytes from the earlier run", len(data))
	}
}

func TestRunTestTLS_PassExitsZero(t *testing.T) {
	addr, _, caPath := selfSignedTLSReceiver(t)
	var out bytes.Buffer
	if code := runTestTLS([]string{"--collector", addr, "--ca-cert", caPath}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	for _, want := range []string{"Trust:        " + caPath + " only", "Negotiated:   TLS 1.3, ALPN h2", "Result: PASS"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

// writeClientPair writes a throwaway self-signed client certificate and key.
func writeClientPair(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "click-dog client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath = filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// --client-cert and --client-key reach the exporter's TLS config: a receiver
// that demands a certificate accepts the pair and refuses the bare handshake.
func TestRunTestTLS_ClientCertificateFlags(t *testing.T) {
	addr, _, caPath := selfSignedTLSReceiverWith(t, func(c *tls.Config) {
		c.MaxVersion = tls.VersionTLS12
		c.ClientAuth = tls.RequireAnyClientCert
	})
	certPath, keyPath := writeClientPair(t)

	var out bytes.Buffer
	args := []string{"--collector", addr, "--ca-cert", caPath, "--client-cert", certPath, "--client-key", keyPath}
	if code := runTestTLS(args, &out, io.Discard); code != 0 {
		t.Fatalf("with the pair: exit = %d:\n%s", code, out.String())
	}
	for _, want := range []string{"Client cert:  " + certPath, "client certificate accepted", "Result: PASS"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if code := runTestTLS([]string{"--collector", addr, "--ca-cert", caPath, "--json"}, &out, io.Discard); code != 1 {
		t.Fatalf("without the pair: exit = %d:\n%s", code, out.String())
	}
	var res export.TLSProbeResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Verdict != export.TLSVerdictClientCert || !res.ClientCertRequested || res.ClientCert != "" {
		t.Errorf("verdict %q, requested %v, client_cert %q", res.Verdict, res.ClientCertRequested, res.ClientCert)
	}
}

// --config checks one exporters.otel entry with every TLS setting it has.
func TestRunTestTLS_Config(t *testing.T) {
	addr, _, caPath := selfSignedTLSReceiver(t)
	configPath := filepath.Join(t.TempDir(), "click-dog.yaml")
	body := fmt.Sprintf(`clickhouse:
  host: localhost
  port: 9000
exporters:
  otel:
    - collector_address: %[1]s
      service_name: verified
      secure: true
      ca_cert: %[2]s
    - collector_address: %[1]s
      service_name: plaintext
    - collector_address: %[1]s
      service_name: unverified
      secure: true
      insecure_skip_verify: true
monitor:
  enabled: true
  min_trace_duration_ms: 1000
`, addr, caPath)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := runTestTLS([]string{"--config", configPath}, &out, &errOut); code != 0 {
		t.Fatalf("exporters.otel[0]: exit = %d:\n%s%s", code, out.String(), errOut.String())
	}
	for _, want := range []string{"Settings:     exporters.otel[0] in " + configPath, "Trust:        " + caPath + " only", "Result: PASS"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	errOut.Reset()
	if code := runTestTLS([]string{"--config", configPath, "--exporter", "1"}, &out, &errOut); code != 2 {
		t.Fatalf("plaintext entry: exit = %d", code)
	}
	if !strings.Contains(errOut.String(), "uses plaintext (automatic), so it does not use TLS") || !strings.Contains(errOut.String(), "click-dog test tls --collector "+addr) {
		t.Errorf("stderr = %q", errOut.String())
	}

	// insecure_skip_verify passes anything and must say so.
	out.Reset()
	if code := runTestTLS([]string{"--config", configPath, "--exporter", "2", "--json"}, &out, io.Discard); code != 0 {
		t.Fatalf("unverified entry: exit = %d:\n%s", code, out.String())
	}
	var res export.TLSProbeResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.InsecureSkipVerify || !strings.Contains(res.Message, "NOT verified") {
		t.Errorf("insecure_skip_verify = %v, message = %q", res.InsecureSkipVerify, res.Message)
	}
	out.Reset()
	_ = runTestTLS([]string{"--config", configPath, "--exporter", "2"}, &out, io.Discard)
	for _, want := range []string{"Trust:        NONE: insecure_skip_verify is set", "Warning: with insecure_skip_verify"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}

	errOut.Reset()
	if code := runTestTLS([]string{"--config", configPath, "--exporter", "3"}, io.Discard, &errOut); code != 2 {
		t.Fatalf("out of range: exit = %d", code)
	}
	if !strings.Contains(errOut.String(), "has 3 exporters.otel entries") {
		t.Errorf("stderr = %q", errOut.String())
	}
}
