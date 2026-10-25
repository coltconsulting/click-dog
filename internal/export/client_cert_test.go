package export

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
)

// testCA issues certificates for in-memory TLS handshakes.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, cn string) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a PEM certificate and key for cn, valid for the hour either
// side of now. A DNS name makes it a server certificate; without one it is a
// client certificate.
func (ca testCA) issue(t *testing.T, cn string, dnsNames ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	return ca.issueWindow(t, cn, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), dnsNames...)
}

// issueWindow is issue with an explicit validity window.
func (ca testCA) issueWindow(t *testing.T, cn string, notBefore, notAfter time.Time, dnsNames ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	usage := x509.ExtKeyUsageClientAuth
	if len(dnsNames) > 0 {
		usage = x509.ExtKeyUsageServerAuth
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     dnsNames,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// mtlsFixture is a collector that requires client certificates from clientCA,
// and click-dog's TLS material on disk at the paths an exporter config names.
type mtlsFixture struct {
	clientCA testCA
	server   *tls.Config
	cfg      config.OTELConfig
}

const testCollectorName = "collector.test"

func newMTLSFixture(t *testing.T) *mtlsFixture {
	t.Helper()
	serverCA := newTestCA(t, "collector CA")
	clientCA := newTestCA(t, "click-dog client CA")

	serverCertPEM, serverKeyPEM := serverCA.issue(t, testCollectorName, testCollectorName)
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(clientCA.cert)

	dir := t.TempDir()
	f := &mtlsFixture{
		clientCA: clientCA,
		server: &tls.Config{
			Certificates: []tls.Certificate{serverCert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    clientCAs,
		},
		cfg: config.OTELConfig{
			CollectorAddress: testCollectorName + ":4317",
			Secure:           true,
			CACert:           filepath.Join(dir, "ca.pem"),
			ClientCert:       filepath.Join(dir, "client.crt"),
			ClientKey:        filepath.Join(dir, "client.key"),
		},
	}
	writeTestFile(t, f.cfg.CACert, serverCA.pem)
	f.writePair(t, "click-dog-1", writeTestFile)
	return f
}

// writePair issues a client certificate for cn and writes it with write.
func (f *mtlsFixture) writePair(t *testing.T, cn string, write func(*testing.T, string, []byte)) {
	t.Helper()
	certPEM, keyPEM := f.clientCA.issue(t, cn)
	write(t, f.cfg.ClientCert, certPEM)
	write(t, f.cfg.ClientKey, keyPEM)
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// renameTestFile replaces path atomically, as Ansible's copy module and a
// Kubernetes Secret volume's directory swap both do.
func renameTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	tmp := path + ".tmp"
	writeTestFile(t, tmp, data)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// handshake runs one TLS handshake between client and the fixture's
// collector over an in-memory pipe, and returns the common name of the client
// certificate the collector verified.
func (f *mtlsFixture) handshake(t *testing.T, client *tls.Config) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()

	cConn, sConn := net.Pipe()
	_ = cConn.SetDeadline(deadline)
	_ = sConn.SetDeadline(deadline)

	type result struct {
		cn  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		srv := tls.Server(sConn, f.server)
		defer func() { _ = srv.Close() }()
		if err := srv.HandshakeContext(ctx); err != nil {
			done <- result{err: err}
			return
		}
		done <- result{cn: srv.ConnectionState().PeerCertificates[0].Subject.CommonName}
	}()

	// gRPC sets ServerName from the dial target's host on its own clone.
	client = client.Clone()
	client.ServerName = testCollectorName
	cli := tls.Client(cConn, client)
	defer func() { _ = cli.Close() }()
	clientErr := cli.HandshakeContext(ctx)
	if clientErr == nil {
		// Under TLS 1.3 the client finishes before the collector has checked
		// its certificate; read until the collector closes or alerts.
		if _, err := cli.Read(make([]byte, 1)); err != nil && !errors.Is(err, io.EOF) {
			clientErr = err
		}
	}

	res := <-done
	if res.err != nil {
		return "", res.err
	}
	return res.cn, clientErr
}

func (f *mtlsFixture) mustPresent(t *testing.T, client *tls.Config, wantCN string) {
	t.Helper()
	cn, err := f.handshake(t, client)
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	if cn != wantCN {
		t.Fatalf("collector saw client certificate %q, want %q", cn, wantCN)
	}
}

// TestOTELTLSConfig_PresentsRenewedClientCertificate pins the rotation
// contract: a pair replaced on disk is presented on the next handshake by the
// same TLS config, so a long-running exporter needs no restart after a
// renewal.
func TestOTELTLSConfig_PresentsRenewedClientCertificate(t *testing.T) {
	tests := []struct {
		name  string
		write func(*testing.T, string, []byte)
	}{
		{name: "atomic replace", write: renameTestFile},
		{name: "in-place rewrite", write: writeTestFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newMTLSFixture(t)
			client, err := newOTELTLSConfig(f.cfg)
			if err != nil {
				t.Fatalf("newOTELTLSConfig() error = %v", err)
			}
			f.mustPresent(t, client, "click-dog-1")

			f.writePair(t, "click-dog-2", tt.write)
			f.mustPresent(t, client, "click-dog-2")
			f.mustPresent(t, client, "click-dog-2")
		})
	}
}

// TestOTELTLSConfig_KeepsPreviousPairWhenReloadFails covers the states a
// rotation passes through, or stops in: whatever is on disk, the exporter
// keeps presenting the last pair that loaded, logs the failure once rather
// than on every handshake, and picks up the new pair once both files are
// consistent.
func TestOTELTLSConfig_KeepsPreviousPairWhenReloadFails(t *testing.T) {
	tests := []struct {
		name   string
		change func(t *testing.T, f *mtlsFixture)
		// logged is a fragment of the warning the failure produces.
		logged string
	}{
		{
			name: "certificate replaced before key",
			change: func(t *testing.T, f *mtlsFixture) {
				certPEM, _ := f.clientCA.issue(t, "click-dog-2")
				renameTestFile(t, f.cfg.ClientCert, certPEM)
			},
			logged: "private key does not match public key",
		},
		{
			name: "half-written certificate",
			change: func(t *testing.T, f *mtlsFixture) {
				certPEM, _ := f.clientCA.issue(t, "click-dog-2")
				writeTestFile(t, f.cfg.ClientCert, certPEM[:len(certPEM)/2])
			},
			logged: "failed to find any PEM data in certificate input",
		},
		{
			name: "empty key",
			change: func(t *testing.T, f *mtlsFixture) {
				writeTestFile(t, f.cfg.ClientKey, nil)
			},
			logged: "failed to find any PEM data in key input",
		},
		{
			name: "key missing mid-swap",
			change: func(t *testing.T, f *mtlsFixture) {
				if err := os.Remove(f.cfg.ClientKey); err != nil {
					t.Fatal(err)
				}
			},
			logged: "no such file or directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newMTLSFixture(t)
			client, err := newOTELTLSConfig(f.cfg)
			if err != nil {
				t.Fatalf("newOTELTLSConfig() error = %v", err)
			}
			logs := captureClicklog(t)

			tt.change(t, f)
			f.mustPresent(t, client, "click-dog-1")
			f.mustPresent(t, client, "click-dog-1")

			out := logs.String()
			if got := strings.Count(out, "was not loaded"); got != 1 {
				t.Fatalf("failed reload logged %d times across two handshakes, want 1:\n%s", got, out)
			}
			for _, want := range []string{tt.logged, "still presenting the previous certificate", "CN=click-dog-1"} {
				if !strings.Contains(out, want) {
					t.Fatalf("failed-reload log missing %q:\n%s", want, out)
				}
			}

			f.writePair(t, "click-dog-2", renameTestFile)
			f.mustPresent(t, client, "click-dog-2")
			if !strings.Contains(logs.String(), "Reloaded OTEL client certificate from "+f.cfg.ClientCert+" (subject CN=click-dog-2") {
				t.Fatalf("reload not logged:\n%s", logs.String())
			}
		})
	}
}

// TestClientCertReloader_TornReadIsNotAFailure stages a rotation that lands
// between the certificate and key reads: the first read sees the new
// certificate with the old key. The reloader must read again and load the
// consistent pair instead of warning about a failure that never happened on
// disk.
func TestClientCertReloader_TornReadIsNotAFailure(t *testing.T) {
	f := newMTLSFixture(t)
	r, err := newClientCertReloader(f.cfg.ClientCert, f.cfg.ClientKey)
	if err != nil {
		t.Fatal(err)
	}
	oldKey, err := os.ReadFile(f.cfg.ClientKey)
	if err != nil {
		t.Fatal(err)
	}
	f.writePair(t, "click-dog-2", renameTestFile)

	reads := 0
	r.read = func(certPath, keyPath string) ([]byte, []byte, pairDigest, error) {
		reads++
		certPEM, keyPEM, d, err := readPair(certPath, keyPath)
		if reads == 1 {
			keyPEM, d = oldKey, digestPair(certPEM, oldKey)
		}
		return certPEM, keyPEM, d, err
	}
	logs := captureClicklog(t)

	if got := r.current().Leaf.Subject.CommonName; got != "click-dog-2" {
		t.Fatalf("current() = %q after a torn read, want the consistent new pair click-dog-2", got)
	}
	if reads != 2 {
		t.Fatalf("read the pair %d times, want 2", reads)
	}
	if strings.Contains(logs.String(), "was not loaded") {
		t.Fatalf("torn read logged as a failure:\n%s", logs.String())
	}
}

// TestClientCertReloader_RepeatedFailureIsReportedAgain pins that returning to
// the pair in use clears the remembered failure: the same broken state
// reached by a later rotation is a new failure and is logged again.
func TestClientCertReloader_RepeatedFailureIsReportedAgain(t *testing.T) {
	f := newMTLSFixture(t)
	client, err := newOTELTLSConfig(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	goodKey, err := os.ReadFile(f.cfg.ClientKey)
	if err != nil {
		t.Fatal(err)
	}
	logs := captureClicklog(t)

	for i := 0; i < 2; i++ {
		writeTestFile(t, f.cfg.ClientKey, nil)
		f.mustPresent(t, client, "click-dog-1")
		writeTestFile(t, f.cfg.ClientKey, goodKey)
		f.mustPresent(t, client, "click-dog-1")
	}
	if got := strings.Count(logs.String(), "was not loaded"); got != 2 {
		t.Fatalf("two separate failed rotations logged %d warnings, want 2:\n%s", got, logs.String())
	}
}

// TestClientCertReloader_ValidityWindow pins that a new pair outside its
// validity window does not displace one inside its own, that the rejection
// ends without a file change once the new certificate becomes valid or the
// current one expires, and that between two invalid certificates the new one
// wins. The current pair, click-dog-1, is valid for an hour either side of
// base.
func TestClientCertReloader_ValidityWindow(t *testing.T) {
	type step struct {
		at       time.Duration // clock offset from base
		want     string        // common name presented
		warnings int           // cumulative "was not loaded" warnings
	}
	tests := []struct {
		name           string
		newFrom, newTo time.Duration // new certificate's window, from base
		warning        string
		steps          []step
	}{
		{
			name:    "not yet valid waits for its start",
			newFrom: 30 * time.Minute, newTo: 3 * time.Hour,
			warning: "not valid until",
			steps: []step{
				{at: 0, want: "click-dog-1", warnings: 1},
				{at: 10 * time.Minute, want: "click-dog-1", warnings: 1},
				{at: 31 * time.Minute, want: "click-dog-2", warnings: 1},
			},
		},
		{
			name:    "not yet valid replaces an expired current certificate",
			newFrom: 2 * time.Hour, newTo: 3 * time.Hour,
			warning: "not valid until",
			steps: []step{
				{at: 0, want: "click-dog-1", warnings: 1},
				{at: 61 * time.Minute, want: "click-dog-2", warnings: 1},
			},
		},
		{
			name:    "expired does not displace a valid certificate",
			newFrom: -3 * time.Hour, newTo: -2 * time.Hour,
			warning: "expired at",
			steps: []step{
				{at: 0, want: "click-dog-1", warnings: 1},
				{at: 30 * time.Minute, want: "click-dog-1", warnings: 1},
				{at: 61 * time.Minute, want: "click-dog-2", warnings: 1},
			},
		},
		{
			name:    "both invalid prefers the new certificate",
			newFrom: -3 * time.Hour, newTo: -2 * time.Hour,
			steps: []step{
				{at: 61 * time.Minute, want: "click-dog-2", warnings: 0},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := time.Now()
			f := newMTLSFixture(t)
			r, err := newClientCertReloader(f.cfg.ClientCert, f.cfg.ClientKey)
			if err != nil {
				t.Fatal(err)
			}
			clock := base
			r.now = func() time.Time { return clock }
			certPEM, keyPEM := f.clientCA.issueWindow(t, "click-dog-2", base.Add(tt.newFrom), base.Add(tt.newTo))
			renameTestFile(t, f.cfg.ClientCert, certPEM)
			renameTestFile(t, f.cfg.ClientKey, keyPEM)
			logs := captureClicklog(t)

			for _, st := range tt.steps {
				clock = base.Add(st.at)
				if got := r.current().Leaf.Subject.CommonName; got != st.want {
					t.Fatalf("at base%+v: presenting %q, want %q\n%s", st.at, got, st.want, logs.String())
				}
				if got := strings.Count(logs.String(), "was not loaded"); got != st.warnings {
					t.Fatalf("at base%+v: %d warnings, want %d:\n%s", st.at, got, st.warnings, logs.String())
				}
			}
			if tt.warning != "" && !strings.Contains(logs.String(), tt.warning) {
				t.Fatalf("warning missing %q:\n%s", tt.warning, logs.String())
			}
		})
	}
}

// TestOTELTLSConfig_StartupRequiresLoadablePair pins that reloading does not
// relax startup: a pair that cannot be loaded still fails exporter
// construction instead of starting with no client certificate. Config load
// rejects an unreadable path even earlier
// (TestLoadConfig_RejectsUnreadableActiveTLSFiles).
func TestOTELTLSConfig_StartupRequiresLoadablePair(t *testing.T) {
	tests := []struct {
		name   string
		change func(t *testing.T, f *mtlsFixture)
		want   string
	}{
		{
			name: "missing certificate",
			change: func(t *testing.T, f *mtlsFixture) {
				f.cfg.ClientCert = filepath.Join(t.TempDir(), "absent.crt")
			},
			want: "no such file or directory",
		},
		{
			name: "unreadable key",
			change: func(t *testing.T, f *mtlsFixture) {
				if os.Geteuid() == 0 {
					t.Skip("root reads files regardless of mode")
				}
				if err := os.Chmod(f.cfg.ClientKey, 0); err != nil {
					t.Fatal(err)
				}
			},
			want: "permission denied",
		},
		{
			name: "mismatched pair",
			change: func(t *testing.T, f *mtlsFixture) {
				certPEM, _ := f.clientCA.issue(t, "click-dog-2")
				writeTestFile(t, f.cfg.ClientCert, certPEM)
			},
			want: "private key does not match public key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newMTLSFixture(t)
			tt.change(t, f)

			if _, err := newOTELTLSConfig(f.cfg); err == nil ||
				!strings.Contains(err.Error(), "failed to load OTEL client certificate") ||
				!strings.Contains(err.Error(), tt.want) {
				t.Fatalf("newOTELTLSConfig() error = %v, want client certificate load failure containing %q", err, tt.want)
			}
			if conn, err := NewOTELGRPCConn(f.cfg); err == nil {
				_ = conn.Close()
				t.Fatal("NewOTELGRPCConn() succeeded with an unloadable client certificate")
			}
		})
	}
}

// TestClientCertReloader_UnacceptableIssuerSendsNoCertificate pins that the
// hook keeps crypto/tls's selection rule from Config.Certificates, which it
// replaced: a server that names acceptable CAs the pair does not chain to gets
// an empty certificate, not one it will reject.
func TestClientCertReloader_UnacceptableIssuerSendsNoCertificate(t *testing.T) {
	f := newMTLSFixture(t)
	r, err := newClientCertReloader(f.cfg.ClientCert, f.cfg.ClientKey)
	if err != nil {
		t.Fatal(err)
	}
	other := newTestCA(t, "some other CA")

	cri := &tls.CertificateRequestInfo{
		SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
		Version:          tls.VersionTLS13,
	}
	cri.AcceptableCAs = [][]byte{f.clientCA.cert.RawSubject}
	if got, _ := r.GetClientCertificate(cri); len(got.Certificate) == 0 {
		t.Fatal("GetClientCertificate() sent no certificate to a server that accepts its issuer")
	}
	cri.AcceptableCAs = [][]byte{other.cert.RawSubject}
	if got, _ := r.GetClientCertificate(cri); len(got.Certificate) != 0 {
		t.Fatal("GetClientCertificate() sent a certificate the server's acceptable CAs exclude")
	}
}
