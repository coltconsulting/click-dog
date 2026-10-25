package export

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/coltconsulting/click-dog/internal/clicklog"
)

// clientCertReloader serves the OTLP mTLS client certificate through
// tls.Config.GetClientCertificate and re-reads the pair from disk on each
// handshake, so a renewed certificate (cert-manager rewriting a mounted
// Secret, or the Ansible playbook replacing the files) is presented on the
// next connection without a restart.
//
// A handshake happens only when gRPC opens a new connection, so the two files
// are read once per connection, not per export. An established connection
// keeps the certificate it was opened with; TLS servers check the client
// certificate at handshake only, so that connection keeps working past the old
// certificate's expiry, and the next one presents the renewed pair.
//
// Changes are detected by content rather than file metadata, so an in-place
// rewrite is seen however coarse the filesystem's timestamps are, and the
// bytes that are parsed are exactly the bytes that were compared.
//
// A pair that fails to load — one file replaced and the other not yet, a
// half-written file, a key that does not match the certificate, a file missing
// mid-swap — leaves the last good pair in place. Each distinct failure is
// logged once; the next change to either file retries.
//
// So does a pair whose certificate is outside its validity window while the
// one in use is inside its own: a pre-staged certificate that is not valid
// yet, or an expired one restored from a backup, would get every handshake
// refused. That rejection is also revisited when the new certificate becomes
// valid or the current one expires, since either ends it without the files
// changing. When neither certificate is valid the new one is used.
type clientCertReloader struct {
	certPath, keyPath string
	// read is readPair and now is time.Now; tests replace them to stage a
	// swap between the two file reads and to move the clock.
	read func(certPath, keyPath string) (certPEM, keyPEM []byte, d pairDigest, err error)
	now  func() time.Time

	mu sync.Mutex
	// cert is the last pair that loaded. It is never nil: construction fails
	// rather than starting without one.
	cert *tls.Certificate
	// loaded identifies the file contents cert was parsed from; failed
	// identifies the last contents (or read error) that did not load. A
	// handshake that sees either presents cert without parsing anything.
	loaded, failed pairDigest
	// recheck, when set, is when the failed contents are evaluated again
	// although they have not changed. Only a validity-window rejection sets
	// it.
	recheck time.Time
}

// pairDigest identifies one read of the two files. Only digests are kept, so
// the reloader holds no copy of the private key's PEM beyond the parsed pair.
type pairDigest [sha256.Size]byte

func digestPair(certPEM, keyPEM []byte) pairDigest {
	h := sha256.New()
	// Length-prefix the certificate so moving bytes between the two files
	// cannot produce the same digest.
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(certPEM)))
	h.Write(n[:])
	h.Write(certPEM)
	h.Write(keyPEM)
	var d pairDigest
	h.Sum(d[:0])
	return d
}

// readPair reads both files and digests what it read. On a read error the
// digest identifies the error instead, so a file that stays missing is
// reported once rather than on every handshake.
func readPair(certPath, keyPath string) (certPEM, keyPEM []byte, d pairDigest, err error) {
	certPEM, err = os.ReadFile(certPath)
	if err == nil {
		keyPEM, err = os.ReadFile(keyPath)
	}
	if err != nil {
		return nil, nil, sha256.Sum256([]byte("read error\x00" + err.Error())), err
	}
	return certPEM, keyPEM, digestPair(certPEM, keyPEM), nil
}

// parsePair is tls.X509KeyPair with the leaf always parsed, so the validity
// check and log lines never depend on the x509keypairleaf GODEBUG setting.
func parsePair(certPEM, keyPEM []byte) (*tls.Certificate, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if cert.Leaf == nil {
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return nil, err
		}
	}
	return &cert, nil
}

// newClientCertReloader loads the pair once and fails with the same errors as
// tls.LoadX509KeyPair, so an unusable pair still stops the exporter from being
// constructed. Its validity window is not checked here: with nothing to fall
// back to, an expired certificate is presented and the collector's refusal
// says why.
func newClientCertReloader(certPath, keyPath string) (*clientCertReloader, error) {
	certPEM, keyPEM, d, err := readPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	cert, err := parsePair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &clientCertReloader{
		certPath: certPath,
		keyPath:  keyPath,
		read:     readPair,
		now:      time.Now,
		cert:     cert,
		loaded:   d,
	}, nil
}

// GetClientCertificate is the tls.Config hook. When the server's request
// cannot be satisfied by the pair it returns an empty certificate, matching
// crypto/tls's own selection from Config.Certificates, which this replaces.
func (r *clientCertReloader) GetClientCertificate(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
	cert := r.current()
	if err := cri.SupportsCertificate(cert); err != nil {
		return new(tls.Certificate), nil
	}
	return cert, nil
}

// current returns the pair to present, reloading it first if either file's
// contents differ from the last load or the last failed attempt, or a
// validity-window rejection has reached its recheck time.
func (r *clientCertReloader) current() *tls.Certificate {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	// Two reads at most. The files are read one after the other, so a
	// rotation that swaps them in between (a Secret volume's directory swap,
	// or Ansible renaming the second file into place) yields a torn pair
	// that a second read resolves; only a failure that survives it is
	// reported.
	var failed pairDigest
	var recheck time.Time
	var loadErr error
	for attempt := 0; attempt < 2; attempt++ {
		certPEM, keyPEM, d, err := r.read(r.certPath, r.keyPath)
		if d == r.loaded {
			// Back to the pair in use: a later return to a failed state is a
			// new failure and is reported again.
			r.failed, r.recheck = pairDigest{}, time.Time{}
			return r.cert
		}
		if d == r.failed && (r.recheck.IsZero() || now.Before(r.recheck)) {
			return r.cert
		}
		if attempt > 0 && d == failed {
			break // unchanged since the first read: the failure is real
		}
		var cert *tls.Certificate
		recheck = time.Time{}
		if err == nil {
			cert, err = parsePair(certPEM, keyPEM)
		}
		if err == nil {
			recheck, err = validityConflict(r.cert.Leaf, cert.Leaf, now)
		}
		if err == nil {
			r.cert = cert
			r.loaded = d
			r.failed, r.recheck = pairDigest{}, time.Time{}
			clicklog.Info("Reloaded OTEL client certificate from %s (%s)", r.certPath, describeCert(r.cert))
			return r.cert
		}
		failed, loadErr = d, err
	}

	r.failed, r.recheck = failed, recheck
	retry := "until either file changes again"
	if !recheck.IsZero() {
		retry = "until either file changes or " + recheck.UTC().Format(time.RFC3339)
	}
	clicklog.Warn("OTEL client certificate %s / %s changed but was not loaded (%v); still presenting the previous certificate (%s) %s",
		r.certPath, r.keyPath, loadErr, describeCert(r.cert), retry)
	return r.cert
}

// validityConflict reports a new certificate that is outside its validity
// window while the current one is inside its own, and when that stops being
// true without the files changing: the new certificate's start, or the
// current one's expiry, whichever comes first. A valid new certificate, or
// one replacing a certificate that is no longer valid either, is no conflict.
func validityConflict(current, next *x509.Certificate, now time.Time) (recheck time.Time, err error) {
	if withinValidity(next, now) || !withinValidity(current, now) {
		return time.Time{}, nil
	}
	// NotAfter is the last valid instant, so the conflict ends just after it.
	recheck = current.NotAfter.Add(time.Nanosecond)
	if now.Before(next.NotBefore) {
		if next.NotBefore.Before(recheck) {
			recheck = next.NotBefore
		}
		return recheck, fmt.Errorf("its certificate is not valid until %s", next.NotBefore.UTC().Format(time.RFC3339))
	}
	return recheck, fmt.Errorf("its certificate expired at %s", next.NotAfter.UTC().Format(time.RFC3339))
}

func withinValidity(c *x509.Certificate, now time.Time) bool {
	return !now.Before(c.NotBefore) && !now.After(c.NotAfter)
}

// describeCert names a pair in log lines by subject and expiry, the two things
// an operator checks after a renewal.
func describeCert(cert *tls.Certificate) string {
	return "subject " + cert.Leaf.Subject.String() + ", expires " + cert.Leaf.NotAfter.UTC().Format(time.RFC3339)
}
