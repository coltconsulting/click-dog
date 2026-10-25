package config

import (
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// TransportSource records how an OTLP connection's transport was chosen.
// The zero value means automatic, so a directly constructed OTELConfig (a
// test fixture, or a connection built in code) behaves as an omitted secure.
type TransportSource string

const (
	// TransportAutomatic: secure was omitted and no TLS material was set, so
	// the collector address decided (plaintext only for a loopback target).
	TransportAutomatic TransportSource = "automatic"
	// TransportExplicit: secure was written, or TLS material selected TLS.
	TransportExplicit TransportSource = "explicit"
)

// IsExplicit reports whether the transport was set rather than derived.
func (s TransportSource) IsExplicit() bool {
	return s == TransportExplicit
}

// String renders the source for validate and the startup log.
func (s TransportSource) String() string {
	if s.IsExplicit() {
		return string(TransportExplicit)
	}
	return string(TransportAutomatic)
}

// OTLPTarget is a collector address parsed the way the gRPC exporter dials it.
type OTLPTarget struct {
	// Scheme is the gRPC resolver: "dns" (the default for a bare address),
	// "passthrough", "unix", or "unix-abstract".
	Scheme string
	// Host is the dialed host, lowercased with any trailing dot removed. Empty
	// for a socket target and for an address like ":4317", which grpc dials on
	// the local host.
	Host string
	// Port is the dialed port; a bare host defaults to 443, as grpc's DNS
	// resolver does. Empty for a socket target.
	Port string
}

// Socket reports whether the target is a unix or unix-abstract socket.
func (t OTLPTarget) Socket() bool {
	return t.Scheme == "unix" || t.Scheme == "unix-abstract"
}

// Loopback reports whether the target is on this host: a socket, an empty
// host, localhost, or an IP literal in 127.0.0.0/8 or ::1 (including the
// IPv4-mapped form). Names other than localhost are never resolved.
func (t OTLPTarget) Loopback() bool {
	if t.Socket() || t.Host == "localhost" {
		return true
	}
	if t.Host == "" {
		// ":4317" dials the local host; an empty endpoint ("dns:///")
		// dials nothing and is not treated as local.
		return t.Port != ""
	}
	addr, ok := parseHostIP(t.Host)
	return ok && addr.IsLoopback()
}

// Unspecified reports whether the host is 0.0.0.0 or ::, a listen address
// rather than a collector address.
func (t OTLPTarget) Unspecified() bool {
	addr, ok := parseHostIP(t.Host)
	return ok && addr.IsUnspecified()
}

func parseHostIP(host string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap().WithZone(""), true
}

// ParseOTLPTarget parses a collector address as the exporter's gRPC client
// does: a registered resolver scheme (dns:, passthrough:, unix:,
// unix-abstract:) is honored, and anything else is a dns target. It never
// resolves names.
func ParseOTLPTarget(address string) OTLPTarget {
	endpoint := address
	scheme := "dns"
	if u, err := url.Parse(address); err == nil {
		switch strings.ToLower(u.Scheme) {
		case "dns", "passthrough":
			scheme = strings.ToLower(u.Scheme)
			endpoint = u.Path
			if endpoint == "" {
				endpoint = u.Opaque
			}
			endpoint = strings.TrimPrefix(endpoint, "/")
		case "unix", "unix-abstract":
			return OTLPTarget{Scheme: strings.ToLower(u.Scheme)}
		}
	}
	host, port := splitOTLPEndpoint(endpoint)
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return OTLPTarget{Scheme: scheme, Host: host, Port: port}
}

// splitOTLPEndpoint mirrors grpc's DNS resolver: a bare IP literal or a host
// without a port uses 443; an empty host stays empty (grpc dials localhost).
func splitOTLPEndpoint(endpoint string) (host, port string) {
	if endpoint == "" {
		return "", ""
	}
	if _, err := netip.ParseAddr(endpoint); err == nil {
		return endpoint, "443"
	}
	if h, p, err := net.SplitHostPort(endpoint); err == nil {
		return h, p
	}
	if h, p, err := net.SplitHostPort(endpoint + ":443"); err == nil {
		return h, p
	}
	// Unparseable: keep the whole string as the host so it is never mistaken
	// for loopback. Dialing it will fail with grpc's own error.
	return endpoint, ""
}

// IsLoopbackOTLPTarget reports whether a collector address is a loopback
// target, the one case in which an omitted secure means plaintext.
func IsLoopbackOTLPTarget(address string) bool {
	return ParseOTLPTarget(address).Loopback()
}

// ResolveOTLPTransport decides whether an OTLP connection uses TLS. An
// explicit secure wins; TLS material implies TLS; otherwise the transport is
// plaintext only for a loopback target.
func ResolveOTLPTransport(address string, secure *bool, hasTLSMaterial bool) (bool, TransportSource) {
	if secure != nil {
		return *secure, TransportExplicit
	}
	if hasTLSMaterial {
		return true, TransportExplicit
	}
	return !IsLoopbackOTLPTarget(address), TransportAutomatic
}

// HasTLSMaterial reports whether any setting that only means something over
// TLS is present.
func (o OTELConfig) HasTLSMaterial() bool {
	return len(o.tlsMaterialFields()) > 0
}

func (o OTELConfig) tlsMaterialFields() []string {
	var fields []string
	if o.CACert != "" {
		fields = append(fields, "ca_cert")
	}
	if o.ClientCert != "" {
		fields = append(fields, "client_cert")
	}
	if o.ClientKey != "" {
		fields = append(fields, "client_key")
	}
	if o.InsecureSkipVerify {
		fields = append(fields, "insecure_skip_verify")
	}
	return fields
}

// resolveTransport sets Secure and SecureSource from the YAML secure key.
func (o *OTELConfig) resolveTransport() {
	o.Secure, o.SecureSource = ResolveOTLPTransport(o.CollectorAddress, o.SecureSetting, o.HasTLSMaterial())
}

// resolveTransport sets Secure and SecureSource from the YAML secure key.
func (m *OTLPMetricsConfig) resolveTransport() {
	m.Secure, m.SecureSource = ResolveOTLPTransport(m.CollectorAddress, m.SecureSetting, false)
}

// TransportSummary describes a resolved transport for validate and the
// startup log, e.g. "TLS (automatic)" or "plaintext (explicit, not loopback)".
func TransportSummary(address string, secure bool, source TransportSource) string {
	if secure {
		return "TLS (" + source.String() + ")"
	}
	if source.IsExplicit() && !IsLoopbackOTLPTarget(address) {
		return "plaintext (explicit, collector is not loopback)"
	}
	return "plaintext (" + source.String() + ")"
}
