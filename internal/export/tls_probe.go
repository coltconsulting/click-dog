package export

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"google.golang.org/grpc/credentials"

	"github.com/coltconsulting/click-dog/internal/config"
)

// TLS probe verdicts. The set is a contract shared by `click-dog test tls
// --json` and deploy/install.sh: add to it only with a matching installer
// change.
const (
	// TLSVerdictOK: the exporter would complete this handshake.
	TLSVerdictOK = "ok"
	// TLSVerdictPlaintext: the receiver answered the TLS handshake with
	// bytes that are not TLS (Reason non-tls), or closed the connection
	// without a word (Reason closed), which a TLS proxy that does not serve
	// the name asked for can also do.
	TLSVerdictPlaintext = "plaintext"
	// TLSVerdictNoSAN: the certificate carries no subjectAltName at all,
	// and Go never falls back to the Common Name.
	TLSVerdictNoSAN = "no-san"
	// TLSVerdictUntrusted: certificate verification failed; Reason says how.
	TLSVerdictUntrusted = "untrusted"
	// TLSVerdictClientCert: the receiver asked for a client certificate and
	// refused the handshake: none was configured, or it rejected the one
	// presented (mTLS).
	TLSVerdictClientCert = "client-cert"
	// TLSVerdictNoALPN: the receiver did not select HTTP/2 (ALPN h2), which
	// grpc-go requires.
	TLSVerdictNoALPN = "no-alpn"
	// TLSVerdictUnknown: nothing definite about TLS was learned; Reason says
	// why.
	TLSVerdictUnknown = "unknown"
)

// TLS probe reasons, refining plaintext, untrusted and unknown verdicts.
// Callers decide on these codes; Message is for people and may be reworded.
const (
	// TLSReasonNonTLS: the first bytes back were another protocol.
	TLSReasonNonTLS = "non-tls"
	// TLSReasonClosed: the receiver closed the connection without answering:
	// not a TLS port, or a TLS proxy or load balancer that does not serve
	// the server name (SNI).
	TLSReasonClosed = "closed"

	TLSReasonHostnameMismatch = "hostname-mismatch"
	TLSReasonExpired          = "expired"
	TLSReasonNotYetValid      = "not-yet-valid"
	TLSReasonUnknownAuthority = "unknown-authority"
	// TLSReasonInvalid is any other verification failure, such as a
	// certificate not issued for server authentication.
	TLSReasonInvalid = "invalid"

	TLSReasonUnreachable    = "unreachable"
	TLSReasonTimeout        = "timeout"
	TLSReasonHandshakeEnded = "handshake-ended"
	// TLSReasonConfig: the address or CA file could not be used, so nothing
	// was dialed.
	TLSReasonConfig = "config"
	TLSReasonOther  = "other"
)

// TLSProbeResult is one probe's outcome. Message is a single line of
// printable text with no double quotes or backslashes, so a shell can lift it
// out of the JSON without a parser.
type TLSProbeResult struct {
	// Address is the host:port dialed, after grpc's defaulting.
	Address string `json:"address"`
	// ServerName is the name the certificate must carry: the host grpc
	// derives from the address, never the Common Name.
	ServerName string `json:"server_name"`
	// CACert is the only CA trusted, or empty for the system trust store.
	CACert string `json:"ca_cert,omitempty"`
	// ClientCert is the client certificate file offered when the receiver
	// asks for one, or empty when none is configured.
	ClientCert string `json:"client_cert,omitempty"`
	// InsecureSkipVerify reports that the certificate was not verified at
	// all, as exporters.otel[].insecure_skip_verify makes the exporter do.
	InsecureSkipVerify  bool           `json:"insecure_skip_verify"`
	Verdict             string         `json:"verdict"`
	Reason              string         `json:"reason,omitempty"`
	Message             string         `json:"message"`
	TLSVersion          string         `json:"tls_version,omitempty"`
	ALPN                string         `json:"alpn,omitempty"`
	ClientCertRequested bool           `json:"client_cert_requested"`
	Certificates        []TLSProbeCert `json:"certificates"`
}

// TLSProbeCert describes one certificate the receiver sent, leaf first. The
// text fields are made printable; PEM carries the exact bytes.
type TLSProbeCert struct {
	Subject   string    `json:"subject"`
	Issuer    string    `json:"issuer"`
	SANs      []string  `json:"sans"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
	// SHA256 is the fingerprint of the DER certificate, as colon-separated
	// uppercase hex, the form `openssl x509 -fingerprint -sha256` prints.
	SHA256 string `json:"sha256"`
	PEM    string `json:"pem"`
}

// tlsRefusalWait bounds the read that catches a TLS 1.3 receiver refusing a
// missing client certificate after the handshake. A variable so tests of a
// receiver that never refuses need not wait it out.
var tlsRefusalWait = 2 * time.Second

// ProbeOTELTLS dials cfg.CollectorAddress once and completes the TLS
// handshake the exporter would, then says whether export would accept the
// receiver. The handshake is the exporter's own: the tls.Config from
// newOTELTLSConfig, wrapped by grpc's credentials.NewTLS (which offers ALPN
// h2 and refuses a receiver that does not select it), with the server name
// grpc derives from the address. Nothing is sent after the handshake. cfg is
// treated as secure; timeout bounds the whole check.
func ProbeOTELTLS(ctx context.Context, cfg config.OTELConfig, timeout time.Duration) TLSProbeResult {
	res := TLSProbeResult{
		Address:            cfg.CollectorAddress,
		CACert:             cfg.CACert,
		ClientCert:         cfg.ClientCert,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
		Certificates:       []TLSProbeCert{},
	}

	addr, authority, err := otelDialTarget(cfg.CollectorAddress)
	if err != nil {
		res.set(TLSVerdictUnknown, TLSReasonConfig, err.Error())
		return res
	}
	res.Address = addr
	res.ServerName = authority
	if host, _, splitErr := net.SplitHostPort(authority); splitErr == nil {
		res.ServerName = host
	}

	tlsConfig, err := newOTELTLSConfig(cfg)
	if err != nil {
		res.set(TLSVerdictUnknown, TLSReasonConfig, err.Error())
		return res
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var dialer net.Dialer
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		if isTimeout(err) {
			res.set(TLSVerdictUnknown, TLSReasonTimeout, fmt.Sprintf("no answer from %s within %s", addr, timeout))
		} else {
			res.set(TLSVerdictUnknown, TLSReasonUnreachable, fmt.Sprintf("no answer from %s (%s)", addr, dialCause(err)))
		}
		return res
	}

	counted := &countingConn{Conn: raw}
	rec := &tlsProbeRecorder{}
	conn, info, err := credentials.NewTLS(rec.instrument(tlsConfig)).ClientHandshake(ctx, authority, counted)
	res.ClientCertRequested = rec.clientCertRequested()
	if err != nil {
		_ = raw.Close()
		res.classifyHandshakeError(err, rec, counted.bytesRead(), timeout)
		return res
	}
	defer func() { _ = conn.Close() }()

	tlsInfo, ok := info.(credentials.TLSInfo)
	if !ok {
		res.set(TLSVerdictUnknown, TLSReasonOther, fmt.Sprintf("unexpected handshake result %T", info))
		return res
	}
	res.setState(tlsInfo.State)

	// A TLS 1.3 receiver checks the client's certificate after the client
	// considers the handshake done, so a refusal arrives as an alert on the
	// first read. Only a receiver that asked for a certificate can refuse.
	if res.ClientCertRequested && tlsInfo.State.Version >= tls.VersionTLS13 {
		if refusal := awaitRefusal(ctx, conn); refusal != "" {
			res.set(TLSVerdictClientCert, "", rec.refusalMessage(refusal))
			return res
		}
	}

	verified := "certificate verified for " + res.ServerName
	if cfg.InsecureSkipVerify {
		verified = "certificate NOT verified (insecure_skip_verify is set)"
	}
	msg := fmt.Sprintf("%s; %s, ALPN %s", verified, res.TLSVersion, res.ALPN)
	if rec.clientCertSent() {
		msg += "; client certificate accepted"
	}
	res.set(TLSVerdictOK, "", msg)
	return res
}

// classifyHandshakeError turns a failed handshake into a verdict. read is how
// many bytes the receiver sent before it failed.
func (res *TLSProbeResult) classifyHandshakeError(err error, rec *tlsProbeRecorder, read int64, timeout time.Duration) {
	state := rec.verifiedState()
	if state != nil {
		res.setState(*state)
	}

	var verifyErr *tls.CertificateVerificationError
	if errors.As(err, &verifyErr) {
		res.setCertificates(verifyErr.UnverifiedCertificates)
		res.classifyVerification(verifyErr.Err, verifyErr.UnverifiedCertificates)
		return
	}

	if isTimeout(err) {
		res.set(TLSVerdictUnknown, TLSReasonTimeout, fmt.Sprintf("no TLS answer within %s", timeout))
		return
	}

	// Conn is set only when the very first record did not look like TLS:
	// the receiver answered in another protocol, such as plaintext gRPC.
	var headerErr tls.RecordHeaderError
	if errors.As(err, &headerErr) && headerErr.Conn != nil {
		res.set(TLSVerdictPlaintext, TLSReasonNonTLS, fmt.Sprintf("%s answered the TLS handshake with non-TLS bytes", res.Address))
		return
	}

	if code, text, ok := remoteAlert(err); ok {
		switch {
		case code == alertNoApplicationProtocol:
			res.set(TLSVerdictNoALPN, "", "the receiver does not speak HTTP/2 over TLS ("+text+")")
		case rec.clientCertRequested():
			res.set(TLSVerdictClientCert, "", rec.refusalMessage(text))
		case state != nil:
			res.set(TLSVerdictUnknown, TLSReasonHandshakeEnded, "the receiver ended the TLS handshake after its certificate ("+text+")")
		default:
			res.set(TLSVerdictUnknown, TLSReasonHandshakeEnded, "TLS handshake ended before a certificate ("+text+")")
		}
		return
	}

	if isConnectionClosed(err) {
		switch {
		case read == 0:
			// Plaintext gRPC closes this way, and so does a TLS proxy or load
			// balancer that serves no certificate for the name asked for.
			res.set(TLSVerdictPlaintext, TLSReasonClosed, fmt.Sprintf(
				"%s closed the connection without a TLS answer: not a TLS port, or a TLS proxy or load balancer that does not serve %s (SNI)",
				res.Address, res.ServerName))
		case rec.clientCertRequested():
			res.set(TLSVerdictClientCert, "", rec.refusalMessage("connection closed"))
		case state != nil:
			res.set(TLSVerdictUnknown, TLSReasonHandshakeEnded, "the receiver closed the connection after its certificate")
		default:
			res.set(TLSVerdictUnknown, TLSReasonHandshakeEnded, "TLS handshake ended before a certificate (connection closed)")
		}
		return
	}

	// grpc-go finishes the TLS handshake and then refuses a receiver that
	// selected no protocol, with an untyped error whose wording is grpc's to
	// change. The verified state shows the cause: ALPN is settled before the
	// certificate, and every other failure after it is classified above.
	if state != nil && state.NegotiatedProtocol == "" {
		res.set(TLSVerdictNoALPN, "", "the receiver did not select HTTP/2 (ALPN h2)")
		return
	}

	res.set(TLSVerdictUnknown, TLSReasonOther, "TLS handshake failed: "+err.Error())
}

// classifyVerification explains a certificate the verifier rejected. Go
// checks the leaf's dates, then its name, then the chain, so the first
// failure in that order is the one reported.
func (res *TLSProbeResult) classifyVerification(err error, certs []*x509.Certificate) {
	var hostErr x509.HostnameError
	var invalidErr x509.CertificateInvalidError
	var authorityErr x509.UnknownAuthorityError
	switch {
	case errors.As(err, &hostErr):
		if !hasSANExtension(hostErr.Certificate) {
			res.set(TLSVerdictNoSAN, "", "the certificate has no subjectAltName; click-dog ignores the Common Name")
			return
		}
		names := certSANs(hostErr.Certificate)
		if len(names) == 0 {
			res.set(TLSVerdictUntrusted, TLSReasonHostnameMismatch, "hostname mismatch; certificate names no DNS name or IP address")
			return
		}
		res.set(TLSVerdictUntrusted, TLSReasonHostnameMismatch, "hostname mismatch; certificate names "+strings.Join(names, ", "))
	case errors.As(err, &invalidErr) && invalidErr.Reason == x509.Expired:
		cert := invalidErr.Cert
		if cert == nil && len(certs) > 0 {
			cert = certs[0]
		}
		switch {
		case cert != nil && time.Now().Before(cert.NotBefore):
			res.set(TLSVerdictUntrusted, TLSReasonNotYetValid, "certificate is not yet valid: not before "+formatCertTime(cert.NotBefore))
		case cert != nil:
			res.set(TLSVerdictUntrusted, TLSReasonExpired, "certificate has expired: not after "+formatCertTime(cert.NotAfter))
		default:
			res.set(TLSVerdictUntrusted, TLSReasonExpired, "certificate has expired")
		}
	case errors.As(err, &authorityErr):
		msg := "certificate signed by unknown authority"
		if len(certs) > 0 && bytes.Equal(certs[0].RawIssuer, certs[0].RawSubject) {
			msg += " (self-signed)"
		}
		res.set(TLSVerdictUntrusted, TLSReasonUnknownAuthority, msg)
	default:
		res.set(TLSVerdictUntrusted, TLSReasonInvalid, strings.TrimPrefix(err.Error(), "x509: "))
	}
}

func (res *TLSProbeResult) set(verdict, reason, message string) {
	res.Verdict = verdict
	res.Reason = reason
	res.Message = messageText(message)
}

func (res *TLSProbeResult) setState(state tls.ConnectionState) {
	res.TLSVersion = tls.VersionName(state.Version)
	res.ALPN = state.NegotiatedProtocol
	res.setCertificates(state.PeerCertificates)
}

func (res *TLSProbeResult) setCertificates(certs []*x509.Certificate) {
	res.Certificates = make([]TLSProbeCert, 0, len(certs))
	for _, c := range certs {
		res.Certificates = append(res.Certificates, newTLSProbeCert(c))
	}
}

// tlsProbeRecorder watches a handshake without changing it: it notes the
// state once the receiver's certificate verified, and whether the receiver
// asked for a client certificate.
type tlsProbeRecorder struct {
	mu        sync.Mutex
	state     *tls.ConnectionState
	certAsked bool
	certSent  bool
	// certConfigured: a client certificate is configured, whether or not
	// the receiver's request lets crypto/tls send it.
	certConfigured bool
}

// instrument returns a clone of base with two observing callbacks. Neither
// alters the handshake: VerifyConnection runs only after crypto/tls has
// verified the certificate and then defers to any existing callback, and
// GetClientCertificate returns what crypto/tls itself would choose.
func (r *tlsProbeRecorder) instrument(base *tls.Config) *tls.Config {
	cfg := base.Clone()

	verify := cfg.VerifyConnection
	cfg.VerifyConnection = func(state tls.ConnectionState) error {
		r.mu.Lock()
		r.state = &state
		r.mu.Unlock()
		if verify != nil {
			return verify(state)
		}
		return nil
	}

	getCert := cfg.GetClientCertificate
	certs := cfg.Certificates
	r.certConfigured = getCert != nil || len(certs) > 0
	cfg.GetClientCertificate = func(req *tls.CertificateRequestInfo) (*tls.Certificate, error) {
		chosen, err := chooseClientCertificate(req, getCert, certs)
		r.mu.Lock()
		r.certAsked = true
		r.certSent = err == nil && chosen != nil && len(chosen.Certificate) > 0
		r.mu.Unlock()
		return chosen, err
	}
	return cfg
}

// chooseClientCertificate is crypto/tls's own choice: the configured
// callback, else the first configured chain the request supports, else no
// certificate.
func chooseClientCertificate(req *tls.CertificateRequestInfo, getCert func(*tls.CertificateRequestInfo) (*tls.Certificate, error), certs []tls.Certificate) (*tls.Certificate, error) {
	if getCert != nil {
		return getCert(req)
	}
	for i := range certs {
		if req.SupportsCertificate(&certs[i]) == nil {
			return &certs[i], nil
		}
	}
	return new(tls.Certificate), nil
}

func (r *tlsProbeRecorder) verifiedState() *tls.ConnectionState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *tlsProbeRecorder) clientCertRequested() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.certAsked
}

func (r *tlsProbeRecorder) clientCertSent() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.certSent
}

// refusalMessage explains a receiver's refusal after it asked for a client
// certificate: of the one presented, of a configured one that the request's
// list of acceptable CAs ruled out (crypto/tls then sends none), or of the
// handshake without one.
func (r *tlsProbeRecorder) refusalMessage(cause string) string {
	r.mu.Lock()
	sent, configured := r.certSent, r.certConfigured
	r.mu.Unlock()
	switch {
	case sent:
		return "the receiver refused the client certificate presented (" + cause + ")"
	case configured:
		return "the receiver asked for a client certificate from a CA it trusts; the configured one is not issued by one, so none was sent (" + cause + ")"
	default:
		return "the receiver refused the handshake without a client certificate (" + cause + ")"
	}
}

// countingConn counts the bytes the receiver sent, so a connection closed
// before any reply can be told from one closed mid-handshake.
type countingConn struct {
	net.Conn
	read atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

func (c *countingConn) bytesRead() int64 { return c.read.Load() }

// awaitRefusal reads once, briefly, after a handshake in which the receiver
// asked for a client certificate, and returns the refusal it finds: a TLS
// alert or a closed connection. Data, or nothing before the wait ends, means
// the receiver accepted.
func awaitRefusal(ctx context.Context, conn net.Conn) string {
	deadline := time.Now().Add(tlsRefusalWait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	var b [1]byte
	_, err := conn.Read(b[:])
	if err == nil || isTimeout(err) {
		return ""
	}
	if _, text, ok := remoteAlert(err); ok {
		return text
	}
	if isConnectionClosed(err) {
		return "connection closed"
	}
	return ""
}

// alertNoApplicationProtocol is TLS alert 120 (RFC 7301): the receiver
// speaks ALPN but none of the protocols offered, here only h2.
const alertNoApplicationProtocol = 120

// remoteAlert reports a TLS alert the receiver sent. crypto/tls returns one
// as a *net.OpError with Op "remote error" around its unexported alert type,
// a uint8 holding the RFC 8446 code. Should that type ever change, the one
// code a verdict depends on is recovered from the alert's text instead.
func remoteAlert(err error) (code uint8, text string, ok bool) {
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "remote error" || opErr.Err == nil {
		return 0, "", false
	}
	text = opErr.Err.Error()
	code, ok = reflectedAlertCode(opErr.Err)
	// No received alert is code 0: crypto/tls turns close_notify into EOF.
	if (!ok || code == 0) && strings.Contains(text, "no application protocol") {
		code = alertNoApplicationProtocol
	}
	return code, "remote error: " + text, true
}

// reflectedAlertCode reads the code out of crypto/tls's alert type.
func reflectedAlertCode(alert error) (uint8, bool) {
	if v := reflect.ValueOf(alert); v.Kind() == reflect.Uint8 {
		return uint8(v.Uint()), true
	}
	return 0, false
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func isConnectionClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, net.ErrClosed)
}

// dialCause is the useful tail of a dial error: "connect: connection
// refused", or the DNS failure, without the repeated address.
func dialCause(err error) string {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return opErr.Err.Error()
	}
	return err.Error()
}

// otelDialTarget resolves a collector_address as grpc.NewClient does with
// its default dns resolver, so the probe dials the port export would and
// verifies the same name. The endpoint is the address itself, or the path of
// a dns: target; the authority, whose host is the TLS server name, is that
// endpoint (grpc's initAuthority, with encodeAuthority's escaping); and the
// port defaults to 443 (the dns resolver's parseTarget).
func otelDialTarget(target string) (addr, authority string, err error) {
	endpoint := target
	if u, parseErr := url.Parse(target); parseErr == nil {
		switch u.Scheme {
		case "dns":
			endpoint = u.Path
			if endpoint == "" {
				endpoint = u.Opaque
			}
			endpoint = strings.TrimPrefix(endpoint, "/")
		case "passthrough", "unix", "unix-abstract":
			return "", "", fmt.Errorf("collector address %q uses grpc's %s resolver; only host:port addresses can be checked", target, u.Scheme)
		}
	}

	host, port, err := grpcDNSHostPort(endpoint)
	if err != nil {
		return "", "", err
	}
	authority = encodeAuthority(endpoint)
	if strings.HasPrefix(endpoint, ":") {
		authority = "localhost" + authority
	}
	return net.JoinHostPort(host, port), authority, nil
}

// grpcDNSHostPort mirrors parseTarget in grpc's internal/resolver/dns with
// its default port, 443.
func grpcDNSHostPort(target string) (host, port string, err error) {
	const defaultPort = "443"
	if target == "" {
		return "", "", errors.New("collector address is empty")
	}
	if _, err := netip.ParseAddr(target); err == nil {
		return target, defaultPort, nil
	}
	if host, port, err = net.SplitHostPort(target); err == nil {
		if port == "" {
			return "", "", fmt.Errorf("collector address %q ends with a colon", target)
		}
		if host == "" {
			host = "localhost"
		}
		return host, port, nil
	}
	if host, port, err = net.SplitHostPort(target + ":" + defaultPort); err == nil {
		return host, port, nil
	}
	return "", "", fmt.Errorf("invalid collector address %q: %v", target, err)
}

// encodeAuthority mirrors grpc's: escape what RFC 3986 does not allow in an
// authority. A valid host name or IP address passes through unchanged.
func encodeAuthority(authority string) string {
	const upperhex = "0123456789ABCDEF"
	shouldEscape := func(c byte) bool {
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' {
			return false
		}
		switch c {
		case '-', '_', '.', '~', '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', ':', '[', ']', '@':
			return false
		}
		return true
	}
	var b strings.Builder
	for i := 0; i < len(authority); i++ {
		c := authority[i]
		if shouldEscape(c) {
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&15])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

var oidExtensionSubjectAltName = asn1.ObjectIdentifier{2, 5, 29, 17}

func hasSANExtension(c *x509.Certificate) bool {
	if c == nil {
		return false
	}
	for _, ext := range c.Extensions {
		if ext.Id.Equal(oidExtensionSubjectAltName) {
			return true
		}
	}
	return false
}

// certSANs lists a certificate's DNS and IP subjectAltNames in the form
// openssl prints them, made printable.
func certSANs(c *x509.Certificate) []string {
	names := make([]string, 0, len(c.DNSNames)+len(c.IPAddresses))
	for _, name := range c.DNSNames {
		names = append(names, "DNS:"+printable(name))
	}
	for _, ip := range c.IPAddresses {
		names = append(names, "IP Address:"+ip.String())
	}
	return names
}

func newTLSProbeCert(c *x509.Certificate) TLSProbeCert {
	sum := sha256.Sum256(c.Raw)
	return TLSProbeCert{
		Subject:   printable(c.Subject.String()),
		Issuer:    printable(c.Issuer.String()),
		SANs:      certSANs(c),
		NotBefore: c.NotBefore.UTC(),
		NotAfter:  c.NotAfter.UTC(),
		SHA256:    colonHex(sum[:]),
		PEM:       string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})),
	}
}

func colonHex(b []byte) string {
	h := strings.ToUpper(hex.EncodeToString(b))
	parts := make([]string, 0, len(b))
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

func formatCertTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05 MST")
}

// printable replaces runes a terminal could act on (control characters,
// line breaks, escape sequences) with '?'. Certificate text comes from the
// receiver, so nothing it supplies reaches an operator's screen raw.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return '?'
		}
		return r
	}, s)
}

// messageText is printable text with no double quotes or backslashes, the
// promise TLSProbeResult.Message makes to shell readers.
func messageText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '"':
			return '\''
		case r == '\\' || !unicode.IsPrint(r):
			return '?'
		}
		return r
	}, s)
}
