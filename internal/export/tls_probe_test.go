package export

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"

	"github.com/coltconsulting/click-dog/internal/config"
)

// probeCert is a throwaway certificate and key for one test receiver.
type probeCert struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	pemPath string
}

func (c *probeCert) tlsCert(chain ...*probeCert) tls.Certificate {
	out := tls.Certificate{Certificate: [][]byte{c.cert.Raw}, PrivateKey: c.key, Leaf: c.cert}
	for _, extra := range chain {
		out.Certificate = append(out.Certificate, extra.cert.Raw)
	}
	return out
}

type certSpec struct {
	cn        string
	dns       []string
	ips       []net.IP
	notBefore time.Time
	notAfter  time.Time
	isCA      bool
	extUsage  []x509.ExtKeyUsage
}

var testSerial int64 = 100

// issueCert makes a certificate from spec, self-signed when issuer is nil,
// and writes its PEM to the test's temp dir for use as a ca_cert.
func issueCert(t *testing.T, spec certSpec, issuer *probeCert) *probeCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if spec.notBefore.IsZero() {
		spec.notBefore = time.Now().Add(-time.Hour)
	}
	if spec.notAfter.IsZero() {
		spec.notAfter = time.Now().Add(24 * time.Hour)
	}
	if spec.extUsage == nil && !spec.isCA {
		spec.extUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	testSerial++
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(testSerial),
		Subject:               pkix.Name{CommonName: spec.cn},
		DNSNames:              spec.dns,
		IPAddresses:           spec.ips,
		NotBefore:             spec.notBefore,
		NotAfter:              spec.notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           spec.extUsage,
		BasicConstraintsValid: true,
		IsCA:                  spec.isCA,
	}
	if spec.isCA {
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	parent, signer := tmpl, key
	if issuer != nil {
		parent, signer = issuer.cert, issuer.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &probeCert{cert: cert, key: key, pemPath: path}
}

var loopbackIPs = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}

// localServerCert is a self-signed certificate valid for every loopback
// spelling the tests dial.
func localServerCert(t *testing.T) *probeCert {
	return issueCert(t, certSpec{cn: "localhost", dns: []string{"localhost"}, ips: loopbackIPs}, nil)
}

func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// serveTLS completes each handshake with cfg, then holds the connection until
// the client goes away, sending nothing.
func serveTLS(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	ln := listenLoopback(t)
	serveTLSOn(ln, cfg)
	return ln.Addr().String()
}

func serveTLSOn(ln net.Listener, cfg *tls.Config) {
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				tc := tls.Server(c, cfg)
				if err := tc.Handshake(); err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, tc)
			}(c)
		}
	}()
}

// serveGRPC runs a real gRPC server, TLS when creds is set.
func serveGRPC(t *testing.T, opts ...grpc.ServerOption) string {
	t.Helper()
	ln := listenLoopback(t)
	srv := grpc.NewServer(opts...)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}

func grpcTLS(cfg *tls.Config) grpc.ServerOption {
	return grpc.Creds(credentials.NewTLS(cfg))
}

func probe(t *testing.T, addr, caCert string) TLSProbeResult {
	t.Helper()
	cfg := config.OTELConfig{CollectorAddress: addr, Secure: true, CACert: caCert}
	return ProbeOTELTLS(context.Background(), cfg, 5*time.Second)
}

func wantVerdict(t *testing.T, res TLSProbeResult, verdict, reason string) {
	t.Helper()
	if res.Verdict != verdict || res.Reason != reason {
		t.Fatalf("verdict = %q/%q (%s), want %q/%q", res.Verdict, res.Reason, res.Message, verdict, reason)
	}
}

func TestProbeOTELTLS_GRPCReceiverIsOK(t *testing.T) {
	cert := localServerCert(t)
	addr := serveGRPC(t, grpcTLS(&tls.Config{Certificates: []tls.Certificate{cert.tlsCert()}}))

	res := probe(t, addr, cert.pemPath)
	wantVerdict(t, res, TLSVerdictOK, "")
	if res.ALPN != "h2" || res.TLSVersion != "TLS 1.3" {
		t.Errorf("negotiated %s / ALPN %q, want TLS 1.3 / h2", res.TLSVersion, res.ALPN)
	}
	if res.ServerName != "127.0.0.1" || res.Address != addr {
		t.Errorf("server name %q, address %q", res.ServerName, res.Address)
	}
	if len(res.Certificates) != 1 || res.Certificates[0].SHA256 == "" || !strings.Contains(res.Certificates[0].PEM, "BEGIN CERTIFICATE") {
		t.Fatalf("certificates = %+v", res.Certificates)
	}
	if res.ClientCertRequested {
		t.Error("client certificate reported as requested")
	}
}

func TestProbeOTELTLS_VerifiesTheNameDialed(t *testing.T) {
	cert := issueCert(t, certSpec{cn: "localhost", dns: []string{"localhost"}}, nil)
	addr := serveTLS(t, &tls.Config{Certificates: []tls.Certificate{cert.tlsCert()}, NextProtos: []string{"h2"}})
	_, port, _ := net.SplitHostPort(addr)

	res := probe(t, "localhost:"+port, cert.pemPath)
	wantVerdict(t, res, TLSVerdictOK, "")
	if res.ServerName != "localhost" {
		t.Errorf("server name = %q, want localhost", res.ServerName)
	}

	// A DNS SAN never covers an IP literal: Go checks IP SANs for those.
	res = probe(t, addr, cert.pemPath)
	wantVerdict(t, res, TLSVerdictUntrusted, TLSReasonHostnameMismatch)
	if res.Message != "hostname mismatch; certificate names DNS:localhost" {
		t.Errorf("message = %q", res.Message)
	}
}

func TestProbeOTELTLS_IPv6Loopback(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	v6 := issueCert(t, certSpec{cn: "v6", ips: []net.IP{net.ParseIP("::1")}}, nil)
	serveTLSOn(ln, &tls.Config{Certificates: []tls.Certificate{v6.tlsCert()}, NextProtos: []string{"h2"}})
	addr := ln.Addr().String()
	if !strings.HasPrefix(addr, "[::1]:") {
		t.Fatalf("listener address %q", addr)
	}

	res := probe(t, addr, v6.pemPath)
	wantVerdict(t, res, TLSVerdictOK, "")
	if res.ServerName != "::1" {
		t.Errorf("server name = %q, want ::1", res.ServerName)
	}
	// Longhand spelling of the same address still verifies.
	_, port, _ := net.SplitHostPort(addr)
	wantVerdict(t, probe(t, "[0:0:0:0:0:0:0:1]:"+port, v6.pemPath), TLSVerdictOK, "")

	v4only := issueCert(t, certSpec{cn: "v4", ips: []net.IP{net.ParseIP("127.0.0.1")}}, nil)
	ln2, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln2.Close() })
	serveTLSOn(ln2, &tls.Config{Certificates: []tls.Certificate{v4only.tlsCert()}, NextProtos: []string{"h2"}})
	res = probe(t, ln2.Addr().String(), v4only.pemPath)
	wantVerdict(t, res, TLSVerdictUntrusted, TLSReasonHostnameMismatch)
	if res.Message != "hostname mismatch; certificate names IP Address:127.0.0.1" {
		t.Errorf("message = %q", res.Message)
	}
}

// openssl's hostname check falls back to the Common Name; Go's does not. A
// certificate that names the host only in its CN must never pass.
func TestProbeOTELTLS_CommonNameOnlyIsNoSAN(t *testing.T) {
	cnOnly := issueCert(t, certSpec{cn: "127.0.0.1"}, nil)
	addr := serveTLS(t, &tls.Config{Certificates: []tls.Certificate{cnOnly.tlsCert()}, NextProtos: []string{"h2"}})

	res := probe(t, addr, cnOnly.pemPath)
	wantVerdict(t, res, TLSVerdictNoSAN, "")
	if len(res.Certificates) != 1 {
		t.Fatalf("certificates = %d, want the leaf", len(res.Certificates))
	}
	if !strings.Contains(res.Message, "ignores the Common Name") {
		t.Errorf("message = %q", res.Message)
	}
}

func TestProbeOTELTLS_Validity(t *testing.T) {
	tests := []struct {
		name       string
		notBefore  time.Time
		notAfter   time.Time
		reason     string
		messageHas string
	}{
		{"expired", time.Now().Add(-72 * time.Hour), time.Now().Add(-24 * time.Hour), TLSReasonExpired, "certificate has expired: not after "},
		{"not yet valid", time.Now().Add(24 * time.Hour), time.Now().Add(72 * time.Hour), TLSReasonNotYetValid, "certificate is not yet valid: not before "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cert := issueCert(t, certSpec{cn: "localhost", ips: loopbackIPs, notBefore: tt.notBefore, notAfter: tt.notAfter}, nil)
			addr := serveTLS(t, &tls.Config{Certificates: []tls.Certificate{cert.tlsCert()}, NextProtos: []string{"h2"}})
			res := probe(t, addr, cert.pemPath)
			wantVerdict(t, res, TLSVerdictUntrusted, tt.reason)
			if !strings.HasPrefix(res.Message, tt.messageHas) {
				t.Errorf("message = %q, want prefix %q", res.Message, tt.messageHas)
			}
		})
	}
}

func TestProbeOTELTLS_UnknownAuthority(t *testing.T) {
	cert := localServerCert(t)
	other := localServerCert(t)
	addr := serveTLS(t, &tls.Config{Certificates: []tls.Certificate{cert.tlsCert()}, NextProtos: []string{"h2"}})

	// Trusting a CA file means trusting it alone: another certificate fails.
	res := probe(t, addr, other.pemPath)
	wantVerdict(t, res, TLSVerdictUntrusted, TLSReasonUnknownAuthority)
	if res.Message != "certificate signed by unknown authority (self-signed)" {
		t.Errorf("message = %q", res.Message)
	}
	if len(res.Certificates) != 1 || res.Certificates[0].SHA256 != colonHex(sha256Of(cert.cert.Raw)) {
		t.Fatalf("certificates = %+v, want the leaf sent", res.Certificates)
	}

	// The system trust store does not know a throwaway self-signed one.
	res = probe(t, addr, "")
	if res.Verdict != TLSVerdictUntrusted {
		t.Fatalf("system store verdict = %q (%s), want untrusted", res.Verdict, res.Message)
	}
	if runtime.GOOS == "linux" && res.Reason != TLSReasonUnknownAuthority {
		t.Errorf("system store reason = %q, want %q", res.Reason, TLSReasonUnknownAuthority)
	}
}

func TestProbeOTELTLS_CertificateNotForServers(t *testing.T) {
	cert := issueCert(t, certSpec{cn: "client", ips: loopbackIPs, extUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}, nil)
	addr := serveTLS(t, &tls.Config{Certificates: []tls.Certificate{cert.tlsCert()}, NextProtos: []string{"h2"}})

	res := probe(t, addr, cert.pemPath)
	wantVerdict(t, res, TLSVerdictUntrusted, TLSReasonInvalid)
	if strings.HasPrefix(res.Message, "x509:") || res.Message == "" {
		t.Errorf("message = %q", res.Message)
	}
}

func TestProbeOTELTLS_Chains(t *testing.T) {
	root := issueCert(t, certSpec{cn: "Test Root", isCA: true}, nil)
	leaf := issueCert(t, certSpec{cn: "localhost", ips: loopbackIPs}, root)
	addr := serveTLS(t, &tls.Config{Certificates: []tls.Certificate{leaf.tlsCert(root)}, NextProtos: []string{"h2"}})

	res := probe(t, addr, root.pemPath)
	wantVerdict(t, res, TLSVerdictOK, "")
	if len(res.Certificates) != 2 || res.Certificates[1].Subject != "CN=Test Root" {
		t.Fatalf("certificates = %+v, want leaf then root", res.Certificates)
	}

	// A pinned intermediate anchors the chain on its own, as ca_cert does.
	inter := issueCert(t, certSpec{cn: "Test Intermediate", isCA: true}, root)
	leaf2 := issueCert(t, certSpec{cn: "localhost", ips: loopbackIPs}, inter)
	addr = serveTLS(t, &tls.Config{Certificates: []tls.Certificate{leaf2.tlsCert(inter)}, NextProtos: []string{"h2"}})
	wantVerdict(t, probe(t, addr, inter.pemPath), TLSVerdictOK, "")

	// Without the issuer, the leaf is not self-signed.
	res = probe(t, addr, leaf.pemPath)
	wantVerdict(t, res, TLSVerdictUntrusted, TLSReasonUnknownAuthority)
	if strings.Contains(res.Message, "self-signed") {
		t.Errorf("message = %q, the leaf is CA-issued", res.Message)
	}
}

func TestProbeOTELTLS_ClientCertificateRequired(t *testing.T) {
	cert := localServerCert(t)
	clientCA := issueCert(t, certSpec{cn: "Client CA", isCA: true}, nil)
	pool := x509.NewCertPool()
	pool.AddCert(clientCA.cert)

	t.Run("TLS 1.3 gRPC receiver", func(t *testing.T) {
		addr := serveGRPC(t, grpcTLS(&tls.Config{
			Certificates: []tls.Certificate{cert.tlsCert()},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
		}))
		res := probe(t, addr, cert.pemPath)
		wantVerdict(t, res, TLSVerdictClientCert, "")
		if !res.ClientCertRequested || len(res.Certificates) != 1 {
			t.Errorf("requested = %v, certificates = %d", res.ClientCertRequested, len(res.Certificates))
		}
	})

	t.Run("TLS 1.2 receiver", func(t *testing.T) {
		addr := serveTLS(t, &tls.Config{
			Certificates: []tls.Certificate{cert.tlsCert()},
			NextProtos:   []string{"h2"},
			MaxVersion:   tls.VersionTLS12,
			ClientAuth:   tls.RequireAnyClientCert,
		})
		res := probe(t, addr, cert.pemPath)
		wantVerdict(t, res, TLSVerdictClientCert, "")
		if res.TLSVersion != "TLS 1.2" {
			t.Errorf("TLS version = %q", res.TLSVersion)
		}
	})

	// A server that asks but does not insist accepts the handshake.
	t.Run("optional client certificate", func(t *testing.T) {
		addr := serveGRPC(t, grpcTLS(&tls.Config{
			Certificates: []tls.Certificate{cert.tlsCert()},
			ClientAuth:   tls.VerifyClientCertIfGiven,
			ClientCAs:    pool,
		}))
		res := probe(t, addr, cert.pemPath)
		wantVerdict(t, res, TLSVerdictOK, "")
		if !res.ClientCertRequested {
			t.Error("client certificate request not recorded")
		}
	})

	t.Run("optional, receiver silent after the handshake", func(t *testing.T) {
		saved := tlsRefusalWait
		tlsRefusalWait = 100 * time.Millisecond
		t.Cleanup(func() { tlsRefusalWait = saved })
		addr := serveTLS(t, &tls.Config{
			Certificates: []tls.Certificate{cert.tlsCert()},
			NextProtos:   []string{"h2"},
			ClientAuth:   tls.RequestClientCert,
		})
		wantVerdict(t, probe(t, addr, cert.pemPath), TLSVerdictOK, "")
	})
}

func TestProbeOTELTLS_NoHTTP2(t *testing.T) {
	cert := localServerCert(t)

	// A receiver that ignores ALPN completes the TLS handshake; grpc-go then
	// refuses it.
	addr := serveTLS(t, &tls.Config{Certificates: []tls.Certificate{cert.tlsCert()}})
	res := probe(t, addr, cert.pemPath)
	wantVerdict(t, res, TLSVerdictNoALPN, "")
	if len(res.Certificates) != 1 {
		t.Errorf("certificates = %d, want the verified leaf", len(res.Certificates))
	}

	// One that speaks ALPN, but only HTTP/1.1, refuses with an alert.
	addr = serveTLS(t, &tls.Config{Certificates: []tls.Certificate{cert.tlsCert()}, NextProtos: []string{"http/1.1"}})
	wantVerdict(t, probe(t, addr, cert.pemPath), TLSVerdictNoALPN, "")
}

func TestProbeOTELTLS_Plaintext(t *testing.T) {
	t.Run("plaintext gRPC receiver", func(t *testing.T) {
		addr := serveGRPC(t)
		res := probe(t, addr, "")
		wantVerdict(t, res, TLSVerdictPlaintext, TLSReasonNonTLS)
		if len(res.Certificates) != 0 {
			t.Errorf("certificates = %d", len(res.Certificates))
		}
	})

	t.Run("closed without a word", func(t *testing.T) {
		ln := listenLoopback(t)
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
		// A TLS proxy that serves no certificate for the name closes the
		// same way, so the message names both causes.
		res := probe(t, ln.Addr().String(), "")
		wantVerdict(t, res, TLSVerdictPlaintext, TLSReasonClosed)
		for _, want := range []string{"not a TLS port", "TLS proxy or load balancer that does not serve 127.0.0.1 (SNI)"} {
			if !strings.Contains(res.Message, want) {
				t.Errorf("message = %q, want %q", res.Message, want)
			}
		}
	})
}

func TestProbeOTELTLS_Inconclusive(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		ln := listenLoopback(t)
		held := make(chan net.Conn, 1)
		go func() {
			c, err := ln.Accept()
			if err == nil {
				held <- c
			}
		}()
		t.Cleanup(func() {
			select {
			case c := <-held:
				_ = c.Close()
			default:
			}
		})
		cfg := config.OTELConfig{CollectorAddress: ln.Addr().String(), Secure: true}
		res := ProbeOTELTLS(context.Background(), cfg, 300*time.Millisecond)
		wantVerdict(t, res, TLSVerdictUnknown, TLSReasonTimeout)
		if res.Message != "no TLS answer within 300ms" {
			t.Errorf("message = %q", res.Message)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		res := probe(t, addr, "")
		wantVerdict(t, res, TLSVerdictUnknown, TLSReasonUnreachable)
		if !strings.HasPrefix(res.Message, "no answer from "+addr+" (") {
			t.Errorf("message = %q", res.Message)
		}
	})

	// A TLS alert before any certificate: the receiver speaks TLS but would
	// not finish this handshake. That is not plaintext.
	t.Run("alert before a certificate", func(t *testing.T) {
		addr := serveTLS(t, &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return nil, errors.New("no service for that name")
		}})
		res := probe(t, addr, "")
		wantVerdict(t, res, TLSVerdictUnknown, TLSReasonHandshakeEnded)
		if !strings.HasPrefix(res.Message, "TLS handshake ended before a certificate (remote error: tls: ") {
			t.Errorf("message = %q", res.Message)
		}
	})

	t.Run("unreadable CA file", func(t *testing.T) {
		res := probe(t, "127.0.0.1:1", filepath.Join(t.TempDir(), "absent.pem"))
		wantVerdict(t, res, TLSVerdictUnknown, TLSReasonConfig)
		if !strings.Contains(res.Message, "failed to read OTEL CA certificate") {
			t.Errorf("message = %q", res.Message)
		}
	})
}

// The probe and a real exporter connection must agree, receiver by
// receiver: that is the whole point of probing with the exporter's code.
func TestProbeOTELTLS_AgreesWithExporterConnection(t *testing.T) {
	cert := localServerCert(t)
	other := localServerCert(t)
	cnOnly := issueCert(t, certSpec{cn: "127.0.0.1"}, nil)
	clientCA := issueCert(t, certSpec{cn: "Client CA", isCA: true}, nil)
	pool := x509.NewCertPool()
	pool.AddCert(clientCA.cert)
	goodCert, goodKey := clientPair(t, clientCA, false)
	foreignCert, foreignKey := clientPair(t, issueCert(t, certSpec{cn: "Other CA", isCA: true}, nil), false)
	mTLS := []grpc.ServerOption{grpcTLS(&tls.Config{Certificates: []tls.Certificate{cert.tlsCert()}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool})}

	tests := []struct {
		name                  string
		server                []grpc.ServerOption
		caCert                string
		clientCert, clientKey string
		want                  string
	}{
		{"trusted", []grpc.ServerOption{grpcTLS(&tls.Config{Certificates: []tls.Certificate{cert.tlsCert()}})}, cert.pemPath, "", "", TLSVerdictOK},
		{"wrong CA", []grpc.ServerOption{grpcTLS(&tls.Config{Certificates: []tls.Certificate{cert.tlsCert()}})}, other.pemPath, "", "", TLSVerdictUntrusted},
		{"CN only", []grpc.ServerOption{grpcTLS(&tls.Config{Certificates: []tls.Certificate{cnOnly.tlsCert()}})}, cnOnly.pemPath, "", "", TLSVerdictNoSAN},
		{"mTLS without a pair", mTLS, cert.pemPath, "", "", TLSVerdictClientCert},
		{"mTLS with a valid pair", mTLS, cert.pemPath, goodCert, goodKey, TLSVerdictOK},
		{"mTLS with a foreign pair", mTLS, cert.pemPath, foreignCert, foreignKey, TLSVerdictClientCert},
		{"plaintext", nil, cert.pemPath, "", "", TLSVerdictPlaintext},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := serveGRPC(t, tt.server...)
			cfg := config.OTELConfig{CollectorAddress: addr, Secure: true, CACert: tt.caCert, ClientCert: tt.clientCert, ClientKey: tt.clientKey}

			res := ProbeOTELTLS(context.Background(), cfg, 5*time.Second)
			if res.Verdict != tt.want {
				t.Fatalf("probe verdict = %q (%s), want %q", res.Verdict, res.Message, tt.want)
			}

			conn, err := NewOTELGRPCConn(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn.Connect()
			state := conn.GetState()
			for state != connectivity.Ready && state != connectivity.TransientFailure {
				if !conn.WaitForStateChange(ctx, state) {
					t.Fatalf("exporter connection stuck in %s", state)
				}
				state = conn.GetState()
			}
			if (state == connectivity.Ready) != (res.Verdict == TLSVerdictOK) {
				t.Errorf("exporter connection %s, probe verdict %q", state, res.Verdict)
			}
		})
	}
}

// Certificate text is the receiver's to choose. None of it may reach a
// terminal raw, and the message must stay one shell-safe line.
func TestProbeOTELTLS_ReceiverTextIsMadeSafe(t *testing.T) {
	evil := issueCert(t, certSpec{cn: "evil\x1b[31m\nCN", dns: []string{"a\"b\\c\nd.example.com"}}, nil)
	addr := serveTLS(t, &tls.Config{Certificates: []tls.Certificate{evil.tlsCert()}, NextProtos: []string{"h2"}})

	res := probe(t, addr, evil.pemPath)
	wantVerdict(t, res, TLSVerdictUntrusted, TLSReasonHostnameMismatch)
	if strings.ContainsAny(res.Message, "\"\\\n\x1b") {
		t.Errorf("message carries unsafe characters: %q", res.Message)
	}
	subject := res.Certificates[0].Subject
	if strings.ContainsAny(subject, "\n\x1b") {
		t.Errorf("subject carries control characters: %q", subject)
	}
	for _, san := range res.Certificates[0].SANs {
		if strings.ContainsAny(san, "\n\x1b") {
			t.Errorf("SAN carries control characters: %q", san)
		}
	}
}

// clientPair issues a client certificate from ca, expired when asked, and
// writes it and its key as PEM files, returning their paths.
func clientPair(t *testing.T, ca *probeCert, expired bool) (certPath, keyPath string) {
	t.Helper()
	spec := certSpec{cn: "click-dog", extUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if expired {
		spec.notBefore, spec.notAfter = time.Now().Add(-72*time.Hour), time.Now().Add(-24*time.Hour)
	}
	c := issueCert(t, spec, ca)
	der, err := x509.MarshalECPrivateKey(c.key)
	if err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(t.TempDir(), "client.key")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return c.pemPath, keyPath
}

// The client certificate is presented exactly as the exporter presents it:
// a receiver that requires one accepts a valid pair and refuses none.
func TestProbeOTELTLS_PresentsClientCertificate(t *testing.T) {
	cert := localServerCert(t)
	clientCA := issueCert(t, certSpec{cn: "Client CA", isCA: true}, nil)
	otherCA := issueCert(t, certSpec{cn: "Other CA", isCA: true}, nil)
	pool := x509.NewCertPool()
	pool.AddCert(clientCA.cert)
	goodCert, goodKey := clientPair(t, clientCA, false)
	expiredCert, expiredKey := clientPair(t, clientCA, true)
	foreignCert, foreignKey := clientPair(t, otherCA, false)

	receivers := map[string]string{
		"TLS 1.2": serveTLS(t, &tls.Config{
			Certificates: []tls.Certificate{cert.tlsCert()},
			NextProtos:   []string{"h2"},
			MaxVersion:   tls.VersionTLS12,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
		}),
		"TLS 1.3": serveGRPC(t, grpcTLS(&tls.Config{
			Certificates: []tls.Certificate{cert.tlsCert()},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
		})),
	}
	for name, addr := range receivers {
		t.Run(name, func(t *testing.T) {
			run := func(clientCert, clientKey string) TLSProbeResult {
				cfg := config.OTELConfig{CollectorAddress: addr, Secure: true, CACert: cert.pemPath, ClientCert: clientCert, ClientKey: clientKey}
				return ProbeOTELTLS(context.Background(), cfg, 5*time.Second)
			}

			res := run(goodCert, goodKey)
			wantVerdict(t, res, TLSVerdictOK, "")
			if !strings.HasSuffix(res.Message, "; client certificate accepted") || res.ClientCert != goodCert {
				t.Errorf("message = %q, client_cert = %q", res.Message, res.ClientCert)
			}
			if res.TLSVersion != name {
				t.Errorf("TLS version = %q, want %q", res.TLSVersion, name)
			}

			res = run("", "")
			wantVerdict(t, res, TLSVerdictClientCert, "")
			if !strings.Contains(res.Message, "without a client certificate") {
				t.Errorf("message = %q", res.Message)
			}

			// Sent, because its issuer is acceptable, and then refused.
			res = run(expiredCert, expiredKey)
			wantVerdict(t, res, TLSVerdictClientCert, "")
			if !strings.Contains(res.Message, "refused the client certificate presented") {
				t.Errorf("message = %q", res.Message)
			}

			// Not sent at all: the request names the CAs the receiver
			// trusts, and crypto/tls, for the exporter too, sends nothing
			// issued by another.
			res = run(foreignCert, foreignKey)
			wantVerdict(t, res, TLSVerdictClientCert, "")
			if !strings.Contains(res.Message, "the configured one is not issued by one, so none was sent") {
				t.Errorf("message = %q", res.Message)
			}
		})
	}
}

// insecure_skip_verify makes the exporter accept anything; the probe says
// so rather than pretending the certificate was checked.
func TestProbeOTELTLS_InsecureSkipVerifyIsReported(t *testing.T) {
	cnOnly := issueCert(t, certSpec{cn: "nobody"}, nil)
	addr := serveTLS(t, &tls.Config{Certificates: []tls.Certificate{cnOnly.tlsCert()}, NextProtos: []string{"h2"}})

	cfg := config.OTELConfig{CollectorAddress: addr, Secure: true, InsecureSkipVerify: true}
	res := ProbeOTELTLS(context.Background(), cfg, 5*time.Second)
	wantVerdict(t, res, TLSVerdictOK, "")
	if !res.InsecureSkipVerify || !strings.HasPrefix(res.Message, "certificate NOT verified (insecure_skip_verify is set)") {
		t.Errorf("insecure_skip_verify = %v, message = %q", res.InsecureSkipVerify, res.Message)
	}
}

// remoteAlert reads the alert code out of crypto/tls's unexported type. If
// a toolchain changes that type, this fails, and the text fallback below
// keeps the one verdict that depends on a code.
func TestRemoteAlert_ReflectsTheAlertCode(t *testing.T) {
	cert := localServerCert(t)
	addr := serveTLS(t, &tls.Config{Certificates: []tls.Certificate{cert.tlsCert()}, NextProtos: []string{"http/1.1"}})
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: certPool(cert), NextProtos: []string{"h2"}})
	if err == nil {
		_ = conn.Close()
		t.Fatal("handshake offering only h2 to an HTTP/1.1 receiver succeeded")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "remote error" {
		t.Fatalf("error = %v (%T), want a remote alert", err, err)
	}
	code, ok := reflectedAlertCode(opErr.Err)
	if !ok || code != alertNoApplicationProtocol {
		t.Fatalf("reflected alert code = %d (ok %v), want %d", code, ok, alertNoApplicationProtocol)
	}
	if code, text, ok := remoteAlert(err); !ok || code != alertNoApplicationProtocol || text != "remote error: tls: no application protocol" {
		t.Errorf("remoteAlert = %d %q %v", code, text, ok)
	}
}

func TestRemoteAlert_FallsBackToTheAlertText(t *testing.T) {
	for _, alert := range []error{errors.New("tls: no application protocol"), opaqueAlert(0)} {
		err := &net.OpError{Op: "remote error", Err: alert}
		code, _, ok := remoteAlert(err)
		if !ok || code != alertNoApplicationProtocol {
			t.Errorf("%T: code = %d, ok = %v, want %d", alert, code, ok, alertNoApplicationProtocol)
		}
	}
	code, _, ok := remoteAlert(&net.OpError{Op: "remote error", Err: errors.New("tls: certificate required")})
	if !ok || code == alertNoApplicationProtocol {
		t.Errorf("certificate required read as code %d", code)
	}
	if _, _, ok := remoteAlert(errors.New("EOF")); ok {
		t.Error("a plain error read as an alert")
	}
}

// opaqueAlert stands in for an alert type whose code reflection reads as 0.
type opaqueAlert uint8

func (opaqueAlert) Error() string { return "tls: no application protocol" }

func certPool(certs ...*probeCert) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, c := range certs {
		pool.AddCert(c.cert)
	}
	return pool
}

func TestOTELDialTarget_FollowsGRPC(t *testing.T) {
	tests := []struct {
		target, addr, authority string
	}{
		{"otel.example.com:4317", "otel.example.com:4317", "otel.example.com:4317"},
		// grpc's dns resolver defaults the port to 443, not 4317.
		{"otel.example.com", "otel.example.com:443", "otel.example.com"},
		{"10.0.0.5:4317", "10.0.0.5:4317", "10.0.0.5:4317"},
		{"[2001:db8::5]:4317", "[2001:db8::5]:4317", "[2001:db8::5]:4317"},
		{"[2001:db8::5]", "[2001:db8::5]:443", "[2001:db8::5]"},
		{"2001:db8::5", "[2001:db8::5]:443", "2001:db8::5"},
		{":4317", "localhost:4317", "localhost:4317"},
		{"dns:///otel.example.com:4317", "otel.example.com:4317", "otel.example.com:4317"},
		{"localhost:4317", "localhost:4317", "localhost:4317"},
	}
	for _, tt := range tests {
		addr, authority, err := otelDialTarget(tt.target)
		if err != nil {
			t.Errorf("%q: %v", tt.target, err)
			continue
		}
		if addr != tt.addr || authority != tt.authority {
			t.Errorf("%q: addr %q authority %q, want %q %q", tt.target, addr, authority, tt.addr, tt.authority)
		}
	}

	for _, bad := range []string{"", "otel.example.com:", "passthrough:///otel.example.com:4317", "unix:///run/otel.sock"} {
		if _, _, err := otelDialTarget(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func sha256Of(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
