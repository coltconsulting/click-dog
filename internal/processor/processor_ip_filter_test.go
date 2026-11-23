package processor

import (
	"testing"

	"github.com/coltconsulting/click-dog/internal/model"
)

// TestOriginatingAddress_FallsBackToOwnAddress covers rows without a usable
// initial_address, which must keep the pre-existing single-node behavior.
func TestOriginatingAddress_FallsBackToOwnAddress(t *testing.T) {
	for _, tc := range []struct{ initial, own, want string }{
		{"192.168.4.7", "10.0.0.9", "192.168.4.7"},
		{"", "10.0.0.9", "10.0.0.9"},
		{"::", "10.0.0.9", "10.0.0.9"},
		{"0.0.0.0", "10.0.0.9", "10.0.0.9"},
	} {
		if got := OriginatingAddress(model.QueryLog{InitialAddress: tc.initial, ClientAddress: tc.own}); got != tc.want {
			t.Errorf("OriginatingAddress(initial=%q, own=%q) = %q, want %q", tc.initial, tc.own, got, tc.want)
		}
	}
}
