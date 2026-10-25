package export

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"

	"github.com/coltconsulting/click-dog/internal/config"
)

type stubResolver map[string][]string

func (r stubResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ips, ok := r[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	out := make([]net.IPAddr, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
	}
	return out, nil
}

func TestLoopbackOnlyDialer_RefusesNonLoopback(t *testing.T) {
	resolver := stubResolver{"localhost": {"10.0.0.1"}, "mixed": {"127.0.0.1", "10.0.0.2"}}
	dial := loopbackOnlyDialer("exporters.otel[2]", resolver)
	for _, addr := range []string{
		"localhost:4317", // passthrough hands over the raw name
		"mixed:4317",     // any off-host address refuses the whole name
		"10.0.0.1:4317",  // dns hands over a resolved address
		"[::ffff:10.0.0.1]:4317",
	} {
		t.Run(addr, func(t *testing.T) {
			conn, err := dial(context.Background(), addr)
			if err == nil {
				_ = conn.Close()
				t.Fatal("dial succeeded, want refusal")
			}
			if !strings.Contains(err.Error(), "exporters.otel[2]") || !strings.Contains(err.Error(), "not a loopback address") {
				t.Fatalf("error = %v, want refusal naming the exporter", err)
			}
		})
	}
}

func TestLoopbackOnlyDialer_DialsVerifiedLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	dial := loopbackOnlyDialer("exporters.otel[0]", stubResolver{"localhost": {"127.0.0.1"}})
	for _, addr := range []string{"localhost:" + port, "127.0.0.1:" + port, ":" + port} {
		conn, err := dial(context.Background(), addr)
		if err != nil {
			t.Fatalf("dial %s: %v", addr, err)
		}
		_ = conn.Close()
	}
}

// TestNewOTELGRPCConn_AutomaticPlaintextIgnoresProxy runs in a child process
// so HTTPS_PROXY is read fresh: net/http caches the proxy environment for the
// life of the process.
func TestNewOTELGRPCConn_AutomaticPlaintextIgnoresProxy(t *testing.T) {
	if os.Getenv("CLICK_DOG_PROXY_CHILD") == "1" {
		runProxyChild(t)
		return
	}
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Close() }()
	var proxied atomic.Int32
	go func() {
		for {
			c, err := proxy.Accept()
			if err != nil {
				return
			}
			proxied.Add(1)
			_ = c.Close()
		}
	}()
	cmd := exec.Command(os.Args[0], "-test.run=^TestNewOTELGRPCConn_AutomaticPlaintextIgnoresProxy$", "-test.v")
	cmd.Env = append(os.Environ(),
		"CLICK_DOG_PROXY_CHILD=1",
		"HTTPS_PROXY=http://"+proxy.Addr().String(),
		"https_proxy=http://"+proxy.Addr().String(),
		"NO_PROXY=", "no_proxy=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	if n := proxied.Load(); n != 0 {
		t.Fatalf("proxy saw %d connections; automatic plaintext must dial directly\n%s", n, out)
	}
}

func runProxyChild(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	go func() { _ = srv.Serve(ln) }()
	defer srv.Stop()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	// Go's own proxy exemption covers only the exact spelling "localhost".
	for _, host := range []string{"LOCALHOST", "localhost."} {
		addr := host + ":" + port
		cfg := config.OTELConfig{CollectorAddress: addr, Name: "exporters.otel[0]"}
		conn, err := NewOTELGRPCConn(cfg)
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn.Connect()
		for s := conn.GetState(); s != connectivity.Ready; s = conn.GetState() {
			if !conn.WaitForStateChange(ctx, s) {
				cancel()
				_ = conn.Close()
				t.Fatalf("%s: never became ready (last state %s)", addr, s)
			}
		}
		cancel()
		_ = conn.Close()
	}
}

func TestNewOTELGRPCConn_ExplicitPlaintextKeepsDefaultDialer(t *testing.T) {
	// An explicit secure: false to a remote collector is the operator's
	// choice; construction must succeed (the dial itself is lazy).
	cfg := config.OTELConfig{CollectorAddress: "otel-collector.monitoring:4317", SecureSource: config.TransportExplicit, Name: "exporters.otel[0]"}
	conn, err := NewOTELGRPCConn(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}
