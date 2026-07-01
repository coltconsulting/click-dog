package main

import "testing"

// fakeLeader is a stand-in for *leader.LeaderElection that lets the gate
// decision table run without a live Keeper.
type fakeLeader struct {
	joined bool
	leader bool
}

func (f fakeLeader) Joined() bool   { return f.joined }
func (f fakeLeader) IsLeader() bool { return f.leader }

// TestNewLeaderGate is the canonical coverage of the deployment-topology gate
// rule shouldExport = !clusterMode || !electionActive || IsLeader(). Each row
// maps to one of the required behaviors in deployment-topology.md.
func TestNewLeaderGate(t *testing.T) {
	tests := []struct {
		name        string
		clusterMode bool
		el          leadership // nil → no election
		wantNilGate bool       // sidecar/single: gate absent, always export
		wantExport  bool       // when gate present, its verdict
	}{
		{
			name:        "sidecar/single always exports (no-op gate)",
			clusterMode: false,
			el:          fakeLeader{joined: true, leader: false},
			wantNilGate: true,
		},
		{
			// Both "no Keeper configured (Option A)" and "Keeper unreachable at
			// startup (standalone fallback)" reach the gate as a nil election —
			// NewLeaderElection is never called in the first, and returns an
			// error leaving election nil in the second. Same path, one case.
			name:        "cluster, nil election (no Keeper / unreachable at startup) exports",
			clusterMode: true,
			el:          nil,
			wantExport:  true,
		},
		{
			name:        "cluster, dial/join window (not yet joined) exports",
			clusterMode: true,
			el:          fakeLeader{joined: false, leader: false},
			wantExport:  true,
		},
		{
			name:        "cluster, joined leader exports",
			clusterMode: true,
			el:          fakeLeader{joined: true, leader: true},
			wantExport:  true,
		},
		{
			name:        "cluster, joined follower (standby) skips",
			clusterMode: true,
			el:          fakeLeader{joined: true, leader: false},
			wantExport:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A nil concrete *fakeLeader must be passed as a genuinely nil
			// interface, mirroring main.go's typed-nil guard.
			var lp leadership
			if tt.el != nil {
				lp = tt.el
			}
			gate := newLeaderGate(tt.clusterMode, lp)
			if tt.wantNilGate {
				if gate != nil {
					t.Fatalf("expected nil gate (always export) for sidecar/single, got non-nil")
				}
				return
			}
			if gate == nil {
				t.Fatalf("expected a gate in cluster mode, got nil")
			}
			if got := gate(); got != tt.wantExport {
				t.Errorf("gate() = %v, want %v", got, tt.wantExport)
			}
		})
	}
}
