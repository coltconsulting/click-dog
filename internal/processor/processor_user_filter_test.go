package processor

import (
	"context"
	"testing"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/model"
)

// TestProcessQueriesBatch_Filters pins the filters on the backfill path,
// where each query_log row is exported on its own. A distributed query's
// secondary rows run as the interserver account from the initiating server, so
// the user and IP filters must follow the originating client, and the
// blacklist must also catch a blacklisted client behind the interserver
// account. The operation filters describe native span-log operations and must
// not drop query_log rows.
func TestProcessQueriesBatch_Filters(t *testing.T) {
	initial := model.QueryLog{QueryID: "q-initial", User: "app", InitialUser: "app", ClientAddress: "192.168.4.7", InitialAddress: "192.168.4.7", Query: "SELECT * FROM events"}
	secondary := model.QueryLog{QueryID: "q-secondary", User: "default", InitialUser: "app", ClientAddress: "10.0.0.9", InitialAddress: "192.168.4.7", Query: "SELECT * FROM events_local"}
	patient := model.QueryLog{QueryID: "q-patient", User: "patient_records", InitialUser: "patient_records", ClientAddress: "192.168.4.8", Query: "SELECT ssn FROM patients"}
	patientShard := model.QueryLog{QueryID: "q-patient-shard", User: "default", InitialUser: "patient_records", ClientAddress: "10.0.0.9", InitialAddress: "192.168.4.8", Query: "SELECT ssn FROM patients_local"}
	unknown := model.QueryLog{QueryID: "q-unknown", Query: "SELECT 3"}
	all := []model.QueryLog{initial, secondary, patient, patientShard, unknown}

	tests := []struct {
		name    string
		filters config.FiltersConfig
		want    []string
	}{
		{
			name:    "no filters export everything",
			filters: config.FiltersConfig{},
			want:    []string{"q-initial", "q-secondary", "q-patient", "q-patient-shard", "q-unknown"},
		},
		{
			name:    "user whitelist follows the originating client",
			filters: config.FiltersConfig{WhitelistUsers: []string{"app"}},
			want:    []string{"q-initial", "q-secondary"},
		},
		{
			// Whitelisting the interserver account must not admit another
			// client's secondary rows.
			name:    "whitelisting the interserver account admits no secondary rows",
			filters: config.FiltersConfig{WhitelistUsers: []string{"default"}},
			want:    nil,
		},
		{
			name:    "user blacklist catches a blacklisted client's secondary rows",
			filters: config.FiltersConfig{BlacklistUsers: []string{"patient_records"}},
			want:    []string{"q-initial", "q-secondary", "q-unknown"},
		},
		{
			name:    "blacklisting the interserver account drops every secondary row",
			filters: config.FiltersConfig{BlacklistUsers: []string{"default"}},
			want:    []string{"q-initial", "q-patient", "q-unknown"},
		},
		{
			name:    "whitelist and blacklist both apply",
			filters: config.FiltersConfig{WhitelistUsers: []string{"app", "patient_records"}, BlacklistUsers: []string{"patient_records"}},
			want:    []string{"q-initial", "q-secondary"},
		},
		{
			name:    "IP whitelist follows the originating client",
			filters: config.FiltersConfig{WhitelistIPs: []string{"192.168.4.7"}},
			want:    []string{"q-initial", "q-secondary"},
		},
		{
			name:    "IP whitelist on the server subnet admits no secondary rows",
			filters: config.FiltersConfig{WhitelistIPs: []string{"10.0.0.0/8"}},
			want:    nil,
		},
		{
			name:    "query blacklist matches each row's text",
			filters: config.FiltersConfig{BlacklistQueries: []string{"patients"}},
			want:    []string{"q-initial", "q-secondary", "q-unknown"},
		},
		{
			name:    "operation whitelist does not apply to query_log rows",
			filters: config.FiltersConfig{WhitelistOperations: []string{"query"}},
			want:    []string{"q-initial", "q-secondary", "q-patient", "q-patient-shard", "q-unknown"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			qf, err := filter.NewQueryFilter(tt.filters)
			if err != nil {
				t.Fatalf("NewQueryFilter: %v", err)
			}
			var got []string
			exp := &mockExporter{exportQueryFunc: func(_ context.Context, q model.QueryLog) error {
				got = append(got, q.QueryID)
				return nil
			}}
			result := ProcessQueriesBatch(context.Background(), all, exp, qf, &config.Config{}, nil)
			if len(got) != len(tt.want) {
				t.Fatalf("exported %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("exported %v, want %v", got, tt.want)
				}
			}
			if result.Filtered != len(all)-len(tt.want) {
				t.Errorf("Filtered = %d, want %d", result.Filtered, len(all)-len(tt.want))
			}
		})
	}
}
