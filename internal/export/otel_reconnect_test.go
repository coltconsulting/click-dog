package export

import (
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/connectivity"

	"github.com/coltconsulting/click-dog/internal/config"
)

func TestOTLPConnectParams_CapReconnectBackoff(t *testing.T) {
	if got := otlpConnectParams.Backoff.MaxDelay; got <= 0 || got > 5*time.Second {
		t.Fatalf("reconnect MaxDelay = %v, want a cap of at most 5s", got)
	}
	if got := otlpConnectParams.Backoff.BaseDelay; got <= 0 || got > time.Second {
		t.Fatalf("reconnect BaseDelay = %v, want at most 1s", got)
	}
	if got := otlpConnectParams.MinConnectTimeout; got != 20*time.Second {
		t.Fatalf("MinConnectTimeout = %v, want gRPC's default 20s", got)
	}
}

// TestNewOTELGRPCConn_ReconnectsPromptlyAfterCollectorReturns keeps the
// collector down long enough for gRPC's default backoff (1s base, x1.6) to
// leave its next attempt more than 0.9s away, then brings it back. With the
// capped backoff the client is ready within the cap; without it this fails.
func TestNewOTELGRPCConn_ReconnectsPromptlyAfterCollectorReturns(t *testing.T) {
	saved := otlpConnectParams
	t.Cleanup(func() { otlpConnectParams = saved })
	otlpConnectParams = grpc.ConnectParams{
		Backoff:           backoff.Config{BaseDelay: 50 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 200 * time.Millisecond},
		MinConnectTimeout: time.Second,
	}

	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := reserve.Addr().String()
	_ = reserve.Close()

	conn, err := NewOTELGRPCConn(config.OTELConfig{CollectorAddress: addr})
	if err != nil {
		t.Fatalf("NewOTELGRPCConn: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// The collector is down: let the client fail and back off.
	deadline := time.Now().Add(3200 * time.Millisecond)
	for time.Now().Before(deadline) {
		conn.Connect()
		time.Sleep(50 * time.Millisecond)
	}
	if st := conn.GetState(); st == connectivity.Ready {
		t.Fatalf("client is Ready with no collector listening on %s", addr)
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("port %s was taken while the collector was down: %v", addr, err)
	}
	srv := grpc.NewServer()
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	start := time.Now()
	for time.Since(start) < 700*time.Millisecond {
		conn.Connect()
		if conn.GetState() == connectivity.Ready {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("client not Ready %v after the collector returned on %s (state %v); want a reconnect within the capped backoff", time.Since(start).Round(time.Millisecond), addr, conn.GetState())
}
