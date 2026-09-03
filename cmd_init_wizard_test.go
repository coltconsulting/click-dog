package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/coltconsulting/click-dog/internal/config"
)

// TestWizard_AcceptAllDefaults walks the wizard pressing Enter at every
// prompt. The result should match defaultWizardAnswers() unchanged — this
// pins the "press-Enter-through-everything" path as a known starting point
// and protects future prompt additions from silently mutating defaults.
func TestWizard_AcceptAllDefaults(t *testing.T) {
	// 10 visible prompts (incl. sidecar/cluster topology); no follow-ups
	// triggered when Splunk HEC stays off, leader election is declined, and
	// topology defaults to sidecar.
	in := strings.NewReader(strings.Repeat("\n", 10))
	var out, errOut bytes.Buffer

	got, eof := runWizard(in, &out, &errOut, defaultWizardAnswers())
	if eof {
		t.Errorf("did not expect EOF on this input")
	}

	want := defaultWizardAnswers()
	// wizardAnswers contains a []string (KeeperHosts) so == is not valid;
	// check each field explicitly. KeeperHosts stays nil because the HA
	// follow-up is never triggered on the all-defaults path.
	if got.Profile != want.Profile || got.CHHost != want.CHHost ||
		got.CHPort != want.CHPort || got.CHSecure != want.CHSecure ||
		got.OTELCollector != want.OTELCollector || got.OTELService != want.OTELService ||
		got.OTELSecure != want.OTELSecure || got.SplunkHECEnabled != want.SplunkHECEnabled ||
		got.SplunkEndpoint != want.SplunkEndpoint ||
		got.UseClusterQueries != want.UseClusterQueries || len(got.KeeperHosts) != 0 {
		t.Errorf("all-defaults walk should leave answers unchanged\n got: %+v\nwant: %+v", got, want)
	}
	if errOut.Len() != 0 {
		t.Errorf("no error expected on all-defaults walk, got: %q", errOut.String())
	}
}

// TestWizard_FullBranchingFlow exercises every conditional path: TLS on
// both sides, Splunk HEC follow-up, HA with multiple Keeper hosts. Catches
// regressions where adding a new prompt accidentally skips a follow-up.
func TestWizard_FullBranchingFlow(t *testing.T) {
	inputs := []string{
		"paranoid",                    // profile
		"ch.example.com",              // CH host
		"y",                           // CH TLS yes → port default flips to 9440
		"",                            // port: keep flipped default 9440
		"otel.example.com:4317",       // collector
		"my-service",                  // service
		"y",                           // OTEL TLS
		"y",                           // Splunk HEC
		"https://splunk.example:8088", // Splunk endpoint
		"sidecar",                     // topology: sidecar (HA stays a separate question)
		"y",                           // HA
		"keeper1:9181, keeper2:9181, keeper3:9181", // Keeper hosts (with spaces)
	}
	in := strings.NewReader(strings.Join(inputs, "\n") + "\n")
	var out, errOut bytes.Buffer

	got, eof := runWizard(in, &out, &errOut, defaultWizardAnswers())
	if eof {
		t.Errorf("did not expect EOF on this input")
	}

	if got.Profile != "paranoid" {
		t.Errorf("Profile = %q, want paranoid", got.Profile)
	}
	if got.CHHost != "ch.example.com" {
		t.Errorf("CHHost = %q", got.CHHost)
	}
	if !got.CHSecure {
		t.Error("CHSecure should be true after y answer")
	}
	if got.CHPort != 9440 {
		t.Errorf("CHPort = %d, want 9440 (TLS default flip)", got.CHPort)
	}
	if got.OTELCollector != "otel.example.com:4317" {
		t.Errorf("OTELCollector = %q", got.OTELCollector)
	}
	if !got.OTELSecure {
		t.Error("OTELSecure should be true")
	}
	if !got.SplunkHECEnabled || got.SplunkEndpoint != "https://splunk.example:8088" {
		t.Errorf("Splunk HEC not captured: %+v", got)
	}
	if len(got.KeeperHosts) != 3 {
		t.Errorf("Keeper hosts not captured: %v", got.KeeperHosts)
	}
	for i, want := range []string{"keeper1:9181", "keeper2:9181", "keeper3:9181"} {
		if got.KeeperHosts[i] != want {
			t.Errorf("KeeperHosts[%d] = %q, want %q (spaces should be trimmed)", i, got.KeeperHosts[i], want)
		}
	}
}

// TestWizard_ClusterTopologyFlow exercises the cluster branch: choosing cluster
// collects a cluster name + Keeper hosts, sets use_cluster_queries, and derives
// HA (no separate HA question is asked). The rendered YAML must carry the
// cluster keys and the ha.keeper block, and must still parse + validate.
func TestWizard_ClusterTopologyFlow(t *testing.T) {
	inputs := []string{
		"production",                 // profile
		"ch.example.com",             // CH host
		"n",                          // CH TLS
		"9000",                       // port
		"otel.example.com:4317",      // collector
		"svc",                        // service
		"n",                          // OTEL TLS
		"n",                          // Splunk HEC
		"cluster",                    // topology: cluster
		"prod_cluster",               // cluster name
		"keeper1:9181, keeper2:9181", // Keeper hosts
		// NOTE: no HA question — cluster mode derives it.
	}
	in := strings.NewReader(strings.Join(inputs, "\n") + "\n")
	var out, errOut bytes.Buffer

	got, eof := runWizard(in, &out, &errOut, defaultWizardAnswers())
	if eof {
		t.Fatalf("did not expect EOF; errOut=%s", errOut.String())
	}
	if !got.UseClusterQueries {
		t.Error("UseClusterQueries should be true after cluster topology")
	}
	if got.Cluster != "prod_cluster" {
		t.Errorf("Cluster = %q, want prod_cluster", got.Cluster)
	}
	// Cluster mode collects Keeper hosts (their presence is what enables
	// election — there is no separate HA flag).
	if len(got.KeeperHosts) != 2 || got.KeeperHosts[0] != "keeper1:9181" {
		t.Errorf("KeeperHosts = %v, want [keeper1:9181 keeper2:9181]", got.KeeperHosts)
	}

	// The rendered YAML must carry the cluster keys + keeper block and validate.
	got.CHUser = "default"
	t.Setenv("CLICKHOUSE_PASSWORD", "x")
	yamlOut := renderWizardYAML(got)
	for _, want := range []string{"cluster: prod_cluster", "use_cluster_queries: true", "ha:", "keeper1:9181"} {
		if !strings.Contains(yamlOut, want) {
			t.Errorf("rendered cluster YAML missing %q\n---\n%s", want, yamlOut)
		}
	}
	wizPath := filepath.Join(t.TempDir(), "wiz.yaml")
	if err := os.WriteFile(wizPath, []byte(yamlOut), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(wizPath)
	if err != nil {
		t.Fatalf("cluster wizard YAML failed to load/validate: %v", err)
	}
	if !cfg.ClickHouse.UseClusterQueries || cfg.ClickHouse.Cluster != "prod_cluster" {
		t.Errorf("loaded config cluster keys wrong: use_cluster=%v cluster=%q",
			cfg.ClickHouse.UseClusterQueries, cfg.ClickHouse.Cluster)
	}
	if !cfg.HA.Active() || len(cfg.HA.Keeper.Hosts) != 2 {
		t.Errorf("loaded config HA wrong: active=%v hosts=%v", cfg.HA.Active(), cfg.HA.Keeper.Hosts)
	}
}

// TestWizard_RejectsAndReprompts confirms the re-prompt loops for the three
// validating prompts (profile, yes/no, integer). A typo on any of these must
// NOT wipe the answers the user already typed — the loop pattern is the
// same one used by the simpler promptInteractive.
func TestWizard_RejectsAndReprompts(t *testing.T) {
	inputs := []string{
		"ludicrous", "minimal", // profile: bad → good
		"ch.example.com", // host
		"maybe", "n",     // CH TLS: bad → no
		"oops", "9000", // port: bad → good
		"otel:4317", // collector
		"svc",       // service
		"n",         // OTEL TLS
		"n",         // Splunk HEC
		"sidecar",   // topology
		"n",         // HA
	}
	in := strings.NewReader(strings.Join(inputs, "\n") + "\n")
	var out, errOut bytes.Buffer

	got, eof := runWizard(in, &out, &errOut, defaultWizardAnswers())
	if eof {
		t.Errorf("did not expect EOF on this input")
	}

	if got.Profile != "minimal" {
		t.Errorf("Profile = %q, want minimal (re-prompt should have accepted 2nd input)", got.Profile)
	}
	if got.CHHost != "ch.example.com" {
		t.Errorf("CHHost not preserved across bad answers: %q", got.CHHost)
	}
	if got.CHPort != 9000 {
		t.Errorf("CHPort = %d, want 9000", got.CHPort)
	}
	// Each invalid answer should have produced a useful error message.
	body := errOut.String()
	for _, want := range []string{"ludicrous", "maybe", "oops"} {
		if !strings.Contains(body, want) {
			t.Errorf("error stream should echo bad input %q; got: %q", want, body)
		}
	}
}

// TestRenderWizardYAML_LoadsForEveryCombination is the strongest contract the
// wizard carries: whatever the user picks, the rendered config must parse
// AND validate via config.LoadConfig. Generating an invalid starter config
// is a worse failure than not having a wizard at all.
func TestRenderWizardYAML_LoadsForEveryCombination(t *testing.T) {
	base := wizardAnswers{
		CHHost:        "ch.example.com",
		CHPort:        9000,
		OTELCollector: "otel.example.com:4317",
		OTELService:   "svc",
	}
	cases := []struct {
		name string
		mut  func(*wizardAnswers)
	}{
		{"minimal/plain", func(a *wizardAnswers) { a.Profile = "minimal" }},
		{"production/plain", func(a *wizardAnswers) { a.Profile = "production" }},
		{"paranoid/plain", func(a *wizardAnswers) { a.Profile = "paranoid" }},
		{"production/ch-tls", func(a *wizardAnswers) { a.Profile = "production"; a.CHSecure = true; a.CHPort = 9440 }},
		{"production/otel-tls", func(a *wizardAnswers) { a.Profile = "production"; a.OTELSecure = true }},
		{"production/splunk-hec", func(a *wizardAnswers) {
			a.Profile = "production"
			a.SplunkHECEnabled = true
			a.SplunkEndpoint = "https://splunk:8088"
		}},
		{"paranoid/ha", func(a *wizardAnswers) {
			a.Profile = "paranoid"
			a.KeeperHosts = []string{"keeper1:9181", "keeper2:9181"}
		}},
		{"production/everything", func(a *wizardAnswers) {
			a.Profile = "production"
			a.CHSecure = true
			a.CHPort = 9440
			a.OTELSecure = true
			a.SplunkHECEnabled = true
			a.SplunkEndpoint = "https://splunk:8088"
			a.KeeperHosts = []string{"k1:9181"}
		}},
	}

	// The Splunk HEC token comes through as ${SPLUNK_HEC_TOKEN} in the
	// rendered config; LoadConfig expands env vars before validating, and
	// an empty token fails validation. Set the var here so the validation
	// path can exercise the wizard's exporter blocks end-to-end.
	t.Setenv("SPLUNK_HEC_TOKEN", "test-token")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := base
			tc.mut(&a)
			content := renderWizardYAML(a)

			path := filepath.Join(t.TempDir(), "wizard.yaml")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatalf("write rendered wizard yaml: %v", err)
			}
			cfg, err := config.LoadConfig(path)
			if err != nil {
				t.Fatalf("wizard config failed to load (%s): %v\n--- rendered ---\n%s", tc.name, err, content)
			}
			if cfg.ClickHouse.Host != a.CHHost {
				t.Errorf("CH host = %q, want %q", cfg.ClickHouse.Host, a.CHHost)
			}
			if a.CHSecure && !cfg.ClickHouse.Secure {
				t.Errorf("CH TLS not propagated to loaded config")
			}
			if a.SplunkHECEnabled && len(cfg.Exporters.SplunkHEC) != 1 {
				t.Errorf("Splunk HEC exporter missing from loaded config: %+v", cfg.Exporters.SplunkHEC)
			}
			if len(a.KeeperHosts) > 0 && !cfg.HA.Active() {
				t.Errorf("HA not active in loaded config")
			}
			if len(a.KeeperHosts) > 0 && len(cfg.HA.Keeper.Hosts) != len(a.KeeperHosts) {
				t.Errorf("Keeper hosts = %v, want %v", cfg.HA.Keeper.Hosts, a.KeeperHosts)
			}
		})
	}
}

// TestWizard_ParityWithProfileTemplates is the single regression gate
// against renderer drift, now that the wizard is the only YAML-emitting
// surface. For each profile, with the optional blocks (TLS, Splunk HEC,
// HA) NOT enabled, the parsed config from the wizard renderer must be
// reflect.DeepEqual to the parsed config from the matching tracked
// example under docs/examples/. The earlier version of this test
// hand-listed six fields; that turned out to be the exact size of the
// bug surface (paranoid's clickhouse: hardening was dropped because no
// listed field covered ClickHouse.QueryTimeoutS). DeepEqual catches the
// next divergence of this class automatically.
//
// The example values (`ch.example.com`, `otel.example.com:4317`,
// `click-dog-monitor`) match the wizardAnswers built below — same input,
// same output. If you edit the examples, mirror the change in the
// renderer (or vice versa) so this stays green.
//
// DeprecationWarnings is the one field excluded — both configs share the
// same emptiness for it, but we zero it explicitly so a future example
// change that intentionally triggers a deprecation warning doesn't fail
// this test for an unrelated reason.
func TestWizard_ParityWithProfileTemplates(t *testing.T) {
	requireInternalSource(t, "docs/examples")
	for _, profile := range []string{"production", "minimal", "paranoid"} {
		t.Run(profile, func(t *testing.T) {
			a := wizardAnswers{
				Profile:       profile,
				CHHost:        "ch.example.com",
				CHPort:        9000,
				CHUser:        "default",
				OTELCollector: "otel.example.com:4317",
				OTELService:   "click-dog-monitor",
			}
			wizPath := filepath.Join(t.TempDir(), "wiz.yaml")
			if err := os.WriteFile(wizPath, []byte(renderWizardYAML(a)), 0600); err != nil {
				t.Fatal(err)
			}
			wizCfg, err := config.LoadConfig(wizPath)
			if err != nil {
				t.Fatalf("wizard YAML failed to load: %v", err)
			}

			examplePath := filepath.Join("docs", "examples", "click-dog-"+profile+".yaml")
			profCfg, err := config.LoadConfig(examplePath)
			if err != nil {
				t.Fatalf("example %s failed to load: %v", examplePath, err)
			}

			wizCfg.DeprecationWarnings = nil
			profCfg.DeprecationWarnings = nil

			if !reflect.DeepEqual(wizCfg, profCfg) {
				t.Errorf("wizard renderer / docs/examples/ diverge for %q:\nwizard:  %+v\nexample: %+v",
					profile, wizCfg, profCfg)
			}
		})
	}
}

// blacklistOpsRe pulls each quoted operation name out of a rendered
// blacklist_operations list, whether the lines are active (`  - "X"`) or
// commented (`#     - "X"`). Anchored on the `- "` list-item prefix so the
// blacklist_queries entries above it (which are regex strings, not op names)
// don't get scooped up when the scanner is pointed at the whole filters block.
var blacklistOpsRe = regexp.MustCompile(`(?m)^\s*#?\s*-\s*"([A-Za-z][A-Za-z0-9]*)"\s*$`)

// extractBlacklistOps returns the operation names under the
// blacklist_operations: key in a rendered YAML fragment, in order. It scopes to
// that section so the production block's blacklist_queries: entries (regex
// patterns like "^SYSTEM") are excluded.
func extractBlacklistOps(block string) []string {
	_, ops, _ := strings.Cut(block, "blacklist_operations:")
	var out []string
	for _, m := range blacklistOpsRe.FindAllStringSubmatch(ops, -1) {
		out = append(out, m[1])
	}
	return out
}

// quotedListItemRe matches any quoted YAML list item; used for blacklist_queries
// patterns, which (unlike op names) carry regex metacharacters like ^ . * \.
var quotedListItemRe = regexp.MustCompile(`(?m)^\s*#?\s*-\s*"(.+)"\s*$`)

// extractBlacklistQueries returns the quoted patterns under blacklist_queries:,
// scoped to that section — it stops at blacklist_operations:, which follows it
// in every block that carries both.
func extractBlacklistQueries(block string) []string {
	_, after, found := strings.Cut(block, "blacklist_queries:")
	if !found {
		return nil
	}
	if before, _, ok := strings.Cut(after, "blacklist_operations:"); ok {
		after = before
	}
	var out []string
	for _, m := range quotedListItemRe.FindAllStringSubmatch(after, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestBlacklistOpsConsistentAcrossProfiles closes the drift gap called out in
// review: the engine-internal operation list is now hand-copied across five
// places — active in production and paranoid, commented in minimal, plus the
// matching docs/examples/ files for minimal and paranoid. The parity gate is
// blind to comment text (and doesn't diff active lists op-by-op), so without
// this a copy could silently diverge from production. Assert every source
// carries the identical list, in the same order.
func TestBlacklistOpsConsistentAcrossProfiles(t *testing.T) {
	requireInternalSource(t, "docs/examples")
	prod := extractBlacklistOps(monitorSectionForProfile("production"))
	if len(prod) == 0 {
		t.Fatal("no ops extracted from production block — the rendered format changed; update extractBlacklistOps")
	}

	readExample := func(profile string) string {
		b, err := os.ReadFile(filepath.Join("docs", "examples", "click-dog-"+profile+".yaml"))
		if err != nil {
			t.Fatalf("read %s example: %v", profile, err)
		}
		return string(b)
	}

	for _, src := range []struct{ name, block string }{
		{"minimal profile (commented)", monitorSectionForProfile("minimal")},
		{"paranoid profile (active)", monitorSectionForProfile("paranoid")},
		{"docs/examples/click-dog-minimal.yaml", readExample("minimal")},
		{"docs/examples/click-dog-paranoid.yaml", readExample("paranoid")},
	} {
		if got := extractBlacklistOps(src.block); !reflect.DeepEqual(prod, got) {
			t.Errorf("%s blacklist drifted from production:\n production=%v\n %s=%v", src.name, prod, src.name, got)
		}
	}

	// blacklist_queries is active in production and paranoid only (not minimal);
	// keep paranoid's copies identical to production's.
	prodQ := extractBlacklistQueries(monitorSectionForProfile("production"))
	if len(prodQ) == 0 {
		t.Fatal("no queries extracted from production block — the rendered format changed; update extractBlacklistQueries")
	}
	for _, src := range []struct{ name, block string }{
		{"paranoid profile (active)", monitorSectionForProfile("paranoid")},
		{"docs/examples/click-dog-paranoid.yaml", readExample("paranoid")},
	} {
		if got := extractBlacklistQueries(src.block); !reflect.DeepEqual(prodQ, got) {
			t.Errorf("%s blacklist_queries drifted from production:\n production=%v\n %s=%v", src.name, prodQ, src.name, got)
		}
	}
}

// TestRunInitWizard_RefusesOverwrite checks that the wizard respects the same
// overwrite-protection contract as runInit. A user re-running `init --wizard`
// over an existing config should see the same "use --force" message.
func TestRunInitWizard_RefusesOverwrite(t *testing.T) {
	if os.Getenv("CLICK_DOG_TEST_WIZARD_OVERWRITE") == "1" {
		dir := os.Getenv("CLICK_DOG_TEST_DIR")
		output := filepath.Join(dir, "click-dog.yaml")
		// Pre-create the file so the wizard's overwrite check trips. If the
		// WriteFile itself fails (unwritable temp dir), the subprocess would
		// otherwise proceed past the absent overwrite-check, write a fresh
		// file, and exit 0 — making the parent's "expected non-zero exit"
		// assertion fail for the wrong reason. Fail loud here instead.
		if err := os.WriteFile(output, []byte("existing"), 0600); err != nil {
			t.Fatalf("setup: pre-create existing file: %v", err)
		}
		// All-defaults walk: 9 blank lines.
		in := strings.NewReader(strings.Repeat("\n", 9))
		fs := flag.NewFlagSet("init", flag.ContinueOnError)
		os.Exit(runInitWizard(in, os.Stdout, os.Stderr, fs, "localhost", 9000, false, "default", "", "localhost:4317", "click-dog-monitor", false, "", "", "production", output, false))
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestRunInitWizard_RefusesOverwrite") //nolint:gosec // Args[0] is the test binary, not user input.
	cmd.Env = append(os.Environ(),
		"CLICK_DOG_TEST_WIZARD_OVERWRITE=1",
		"CLICK_DOG_TEST_DIR="+dir,
	)
	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected non-zero exit, got err=%v, out=%s", err, out)
	}
	body := string(out)
	if !strings.Contains(body, "already exists") {
		t.Errorf("expected overwrite refusal message, got: %s", body)
	}
}

// TestRunInitWizard_RefusesWriteOnTruncatedStdin pins the new EOF-detection
// behavior: if stdin closes before the wizard has heard back on every
// question, runInitWizard must NOT silently write a half-defaulted config.
// Subprocess pattern (matching TestRunInitWizard_RefusesOverwrite) because
// runInitWizard os.Exits on the EOF branch.
func TestRunInitWizard_RefusesWriteOnTruncatedStdin(t *testing.T) {
	if os.Getenv("CLICK_DOG_TEST_WIZARD_EOF") == "1" {
		dir := os.Getenv("CLICK_DOG_TEST_DIR")
		output := filepath.Join(dir, "click-dog.yaml")
		// Only 3 lines of input — far short of the wizard's 9-prompt baseline.
		in := strings.NewReader("\n\n\n")
		fs := flag.NewFlagSet("init", flag.ContinueOnError)
		os.Exit(runInitWizard(in, os.Stdout, os.Stderr, fs, "localhost", 9000, false, "default", "", "localhost:4317", "click-dog-monitor", false, "", "", "production", output, false))
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestRunInitWizard_RefusesWriteOnTruncatedStdin") //nolint:gosec // Args[0] is the test binary, not user input.
	cmd.Env = append(os.Environ(),
		"CLICK_DOG_TEST_WIZARD_EOF=1",
		"CLICK_DOG_TEST_DIR="+dir,
	)
	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected non-zero exit on truncated stdin, got err=%v, out=%s", err, out)
	}
	if !strings.Contains(string(out), "stdin closed before") {
		t.Errorf("expected EOF refusal message, got: %s", out)
	}
	// Most important assertion: the file must NOT have been written.
	if _, statErr := os.Stat(filepath.Join(dir, "click-dog.yaml")); statErr == nil {
		t.Errorf("config file was written despite truncated stdin — EOF guard failed")
	}
}

// TestRunInitWizard_ForceOverwrite confirms --force lets the wizard replace
// an existing file and emits the same "existing file replaced" cue as runInit.
func TestRunInitWizard_ForceOverwrite(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "click-dog.yaml")
	if err := os.WriteFile(output, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader(strings.Repeat("\n", 10))
	var out bytes.Buffer
	fs := flag.NewFlagSet("init", flag.ContinueOnError)

	runInitWizard(in, &out, io.Discard, fs, "localhost", 9000, false, "default", "", "localhost:4317", "click-dog-monitor", false, "", "", "production", output, true)

	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "old" {
		t.Error("file was not overwritten")
	}
	if !strings.Contains(out.String(), "existing file replaced") {
		t.Errorf("expected force-overwrite notice, got: %q", out.String())
	}
}

// TestSplitAndTrim pins the comma-split helper used for Keeper hosts: empty
// entries dropped, surrounding whitespace stripped.
func TestSplitAndTrim(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{"a, b ,c", []string{"a", "b", "c"}},
		{"", []string{}},
		{",,", []string{}},
		{"a,,b", []string{"a", "b"}},
	}
	for _, tt := range tests {
		got := splitAndTrim(tt.in)
		// Normalize nil to []string{} so DeepEqual treats them identically;
		// the contract callers care about is "len(result) > 0", not "nil
		// vs. empty slice".
		if got == nil {
			got = []string{}
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitAndTrim(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// TestWizard_KeeperHostsRequiredForElection exercises the empty-hosts
// re-prompt loop: once the user opts into leader election, an answer that
// parses to zero hosts (e.g. ", , ,") must NOT produce an empty ha block —
// hosts presence is what enables election, so zero hosts would silently leave
// the instance ungated. The user gets re-prompted until they supply a host.
func TestWizard_KeeperHostsRequiredForElection(t *testing.T) {
	// NOTE: empty input ("") at the Keeper-hosts prompt accepts the *default*
	// (localhost:9181) rather than producing zero hosts — that's
	// promptString's documented contract and matches every other prompt.
	// To trigger the re-prompt loop, the user has to type something that
	// parses to zero hosts (e.g. ", , ," or pure whitespace).
	inputs := []string{
		"production",     // profile
		"ch.example.com", // host
		"n",              // CH TLS
		"9000",           // port
		"otel:4317",      // collector
		"svc",            // service
		"n",              // OTEL TLS
		"n",              // Splunk HEC
		"sidecar",        // topology
		"y",              // HA enabled
		",,,",            // parses to zero hosts, should re-prompt
		" , , ",          // pure-whitespace-and-commas, also zero, should re-prompt
		"k1:9181",        // finally a real host
	}
	in := strings.NewReader(strings.Join(inputs, "\n") + "\n")
	var out, errOut bytes.Buffer

	got, eof := runWizard(in, &out, &errOut, defaultWizardAnswers())
	if eof {
		t.Errorf("did not expect EOF on this input")
	}

	if len(got.KeeperHosts) != 1 || got.KeeperHosts[0] != "k1:9181" {
		t.Errorf("KeeperHosts = %v, want [k1:9181]", got.KeeperHosts)
	}
	// The error stream should mention the required-hosts message twice
	// (one for each rejected empty answer).
	if want := "at least one Keeper host"; strings.Count(errOut.String(), want) < 2 {
		t.Errorf("expected re-prompt error %q at least twice, got: %q", want, errOut.String())
	}
}

// TestRenderWizardYAML_EscapesScalars confirms that adversarial user input
// (YAML indicator characters, leading whitespace, inline `#`) survives the
// renderer without breaking the parser. Before yamlScalar was wired in,
// values like "foo # comment" silently produced a YAML comment and dropped
// the trailing text from the parsed config.
func TestRenderWizardYAML_EscapesScalars(t *testing.T) {
	t.Setenv("SPLUNK_HEC_TOKEN", "test")
	tests := []struct {
		name    string
		field   string
		value   string
		extract func(*config.Config) string
	}{
		{"host with # ", "host", "ch.example.com # not a comment", func(c *config.Config) string { return c.ClickHouse.Host }},
		{"host with leading space", "host", " ch.example.com", func(c *config.Config) string { return c.ClickHouse.Host }},
		{"service with colon-space", "service", "svc: with colon", func(c *config.Config) string { return c.Exporters.OTEL[0].ServiceName }},
		{"service with ampersand", "service", "&anchor-looking", func(c *config.Config) string { return c.Exporters.OTEL[0].ServiceName }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := wizardAnswers{
				Profile:       "production",
				CHHost:        "ch.example.com",
				CHPort:        9000,
				OTELCollector: "otel:4317",
				OTELService:   "svc",
			}
			switch tt.field {
			case "host":
				a.CHHost = tt.value
			case "service":
				a.OTELService = tt.value
			}

			path := filepath.Join(t.TempDir(), "esc.yaml")
			if err := os.WriteFile(path, []byte(renderWizardYAML(a)), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadConfig(path)
			if err != nil {
				t.Fatalf("rendered YAML failed to parse: %v", err)
			}
			if got := tt.extract(cfg); got != tt.value {
				t.Errorf("round-trip lost data: got %q, want %q (rendered file may be unsafely concatenated)", got, tt.value)
			}
		})
	}
}

// TestMonitorSectionForProfile_PanicsOnUnknown locks in the loud-failure
// contract: extending the `profiles` registry without adding a matching
// case here would have silently handed the user a production-tuned
// starter under a different name. The panic surfaces the registry/wizard
// mismatch with a clear message pointing at the missing case.
func TestMonitorSectionForProfile_PanicsOnUnknown(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on unknown profile")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "ludicrous") {
			t.Errorf("panic message should mention the unknown profile, got: %v", r)
		}
	}()
	_ = monitorSectionForProfile("ludicrous")
}

// TestRunInitWizard_OverwriteCheckFiresBeforePrompts is the UX-fix test:
// stat-then-prompt order means a user who runs `init --wizard` over an
// existing config gets the refusal IMMEDIATELY, without typing answers
// to nine questions that are about to be thrown away. The empty stdin
// would otherwise EOF on the very first prompt — if the stat check ran
// late, we'd see the EOF error instead of the overwrite error. Both
// would exit non-zero, but the error message tells us which gate caught
// the call.
func TestRunInitWizard_OverwriteCheckFiresBeforePrompts(t *testing.T) {
	if os.Getenv("CLICK_DOG_TEST_WIZARD_FAILFAST") == "1" {
		dir := os.Getenv("CLICK_DOG_TEST_DIR")
		output := filepath.Join(dir, "click-dog.yaml")
		if err := os.WriteFile(output, []byte("pre-existing"), 0600); err != nil {
			t.Fatalf("setup: pre-create existing file: %v", err)
		}
		// Empty stdin: if the overwrite check fired AFTER the wizard,
		// the EOF-detection guard would trip first and we'd see the
		// "stdin closed" message instead of the "already exists" one.
		in := strings.NewReader("")
		fs := flag.NewFlagSet("init", flag.ContinueOnError)
		os.Exit(runInitWizard(in, os.Stdout, os.Stderr, fs, "localhost", 9000, false, "default", "", "localhost:4317", "click-dog-monitor", false, "", "", "production", output, false))
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestRunInitWizard_OverwriteCheckFiresBeforePrompts") //nolint:gosec // Args[0] is the test binary, not user input.
	cmd.Env = append(os.Environ(),
		"CLICK_DOG_TEST_WIZARD_FAILFAST=1",
		"CLICK_DOG_TEST_DIR="+dir,
	)
	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected non-zero exit, got err=%v, out=%s", err, out)
	}
	body := string(out)
	if !strings.Contains(body, "already exists") {
		t.Errorf("overwrite check must fire before prompts: expected 'already exists' but got: %s", body)
	}
	// Pre-existing contents must be untouched.
	data, _ := os.ReadFile(filepath.Join(dir, "click-dog.yaml"))
	if string(data) != "pre-existing" {
		t.Errorf("pre-existing file mutated: %q", data)
	}
}

// TestRunInitWizard_RejectsInvalidProfileFlag confirms the --profile flag
// is validated before the wizard begins. Without this, an invalid value
// would appear as the seeded default ("Profile (...) [ludicrous]:") and
// pressing Enter would let it through into renderWizardYAML, which would
// then panic at monitor-block selection — a much worse diagnostic.
func TestRunInitWizard_RejectsInvalidProfileFlag(t *testing.T) {
	if os.Getenv("CLICK_DOG_TEST_WIZARD_BAD_PROFILE") == "1" {
		dir := os.Getenv("CLICK_DOG_TEST_DIR")
		output := filepath.Join(dir, "click-dog.yaml")
		fs := flag.NewFlagSet("init", flag.ContinueOnError)
		os.Exit(runInitWizard(strings.NewReader(""), os.Stdout, os.Stderr, fs, "localhost", 9000, false, "default", "", "localhost:4317", "click-dog-monitor", false, "", "", "ludicrous", output, false))
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestRunInitWizard_RejectsInvalidProfileFlag") //nolint:gosec // Args[0] is the test binary, not user input.
	cmd.Env = append(os.Environ(),
		"CLICK_DOG_TEST_WIZARD_BAD_PROFILE=1",
		"CLICK_DOG_TEST_DIR="+dir,
	)
	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected non-zero exit, got err=%v, out=%s", err, out)
	}
	body := string(out)
	if !strings.Contains(body, `unknown profile "ludicrous"`) {
		t.Errorf("expected unknown-profile error, got: %s", body)
	}
	for _, name := range []string{"production", "minimal", "paranoid"} {
		if !strings.Contains(body, name) {
			t.Errorf("error should list valid profile %q, got: %s", name, body)
		}
	}
}

// TestRunInit_WizardDispatch covers the --wizard branch in runInit itself,
// not just runInitWizard in isolation. Previously the dispatch was only
// reached by the binary in production; this test forces a full
// runInit(["--wizard", ...]) invocation through the subprocess pattern
// and asserts the wizard's identifying banner appears in output AND the
// rendered file contains the wizard's marker comment.
func TestRunInit_WizardDispatch(t *testing.T) {
	if os.Getenv("CLICK_DOG_TEST_RUNINIT_WIZARD") == "1" {
		dir := os.Getenv("CLICK_DOG_TEST_DIR")
		output := filepath.Join(dir, "click-dog.yaml")
		// runInit reads os.Stdin directly. The subprocess gets its stdin
		// piped from the parent (set below) — 9 newlines walks the
		// wizard's prompts at all defaults.
		runInit([]string{"--wizard", "--output", output}, os.Stdout, os.Stderr)
		return
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestRunInit_WizardDispatch") //nolint:gosec // Args[0] is the test binary, not user input.
	cmd.Env = append(os.Environ(),
		"CLICK_DOG_TEST_RUNINIT_WIZARD=1",
		"CLICK_DOG_TEST_DIR="+dir,
	)
	cmd.Stdin = strings.NewReader(strings.Repeat("\n", 10))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("runInit(--wizard) exited non-zero: err=%v, out=%s", err, out)
	}
	if !strings.Contains(string(out), "click-dog configurator") {
		t.Errorf("expected wizard banner in runInit --wizard output, got: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "click-dog.yaml")); err != nil {
		t.Fatalf("wizard branch did not write the output file: %v", err)
	}
	// The wizard-branch signal is the summary block printed AFTER the file
	// is written; renderWizardYAML's file banner is now schema-only since
	// both `click-dog init` paths share the same renderer.
	if !strings.Contains(string(out), "Configured:") {
		t.Errorf("expected wizard summary block in runInit --wizard output, got: %s", out)
	}
}

// TestPromptInt_BoundsCheck pins the [min, max] enforcement: values
// outside the range re-prompt rather than slide through to write a
// config the binary will reject on first start. The port range
// [1, 65535] mirrors the LoadConfig validator at
// internal/config/config.go:532; this test guards the wizard side of
// that contract so the two stay aligned.
func TestPromptInt_BoundsCheck(t *testing.T) {
	tests := []struct {
		name   string
		inputs string
		want   int
	}{
		{"in-range single", "8080\n", 8080},
		{"above max → reject → in-range", "99999\n8080\n", 8080},
		{"below min → reject → in-range", "0\n443\n", 443},
		{"non-numeric → reject → in-range", "ninethousand\n9000\n", 9000},
		{"min boundary accepted", "1\n", 1},
		{"max boundary accepted", "65535\n", 65535},
		{"empty input keeps default", "\n", 9000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &wizPrompter{r: bufio.NewReader(strings.NewReader(tt.inputs))}
			var out, errOut bytes.Buffer
			got := promptInt(w, &out, &errOut, "port", 9000, 1, 65535)
			if got != tt.want {
				t.Errorf("promptInt(...) = %d, want %d (inputs=%q)", got, tt.want, tt.inputs)
			}
			// Re-prompt cases should mention the legal range in the error
			// stream so the user knows what to type.
			if strings.Contains(tt.name, "reject") && !strings.Contains(errOut.String(), "between 1 and 65535") {
				t.Errorf("expected range hint in error stream, got: %q", errOut.String())
			}
		})
	}
}

// TestWriteWizardSummary pins the post-write Configured: block. Pure function,
// no subprocess needed — just a writer and a struct. Each sub-case covers one
// branch (TLS flags, Splunk, HA) so a regression that breaks one row stays
// localised.
func TestWriteWizardSummary(t *testing.T) {
	tests := []struct {
		name    string
		answers wizardAnswers
		want    []string // substrings that MUST appear
		notWant []string // substrings that MUST NOT appear
	}{
		{
			name: "minimal — no TLS, no Splunk, no HA",
			answers: wizardAnswers{
				CHHost: "fsnpch201", CHPort: 9000,
				OTELCollector: "otel:4317", OTELService: "click-dog-monitor",
			},
			want: []string{
				"Configured:",
				"ClickHouse  fsnpch201:9000",
				"OTEL        otel:4317",
				`service "click-dog-monitor"`,
				"Topology    sidecar",
			},
			notWant: []string{"(TLS)", "Splunk HEC", "HA"},
		},
		{
			name: "cluster topology — leader-gated, keeper hosts",
			answers: wizardAnswers{
				CHHost: "ch", CHPort: 9000,
				OTELCollector: "otel:4317", OTELService: "click-dog-monitor",
				UseClusterQueries: true, Cluster: "main",
				KeeperHosts: []string{"k1:9181"},
			},
			want: []string{
				`Topology    cluster "main"`,
				"leader-gated",
				"HA          leader election — keeper k1:9181",
			},
		},
		{
			name: "ClickHouse + OTEL both TLS",
			answers: wizardAnswers{
				CHHost: "ch.prod", CHPort: 9440, CHSecure: true,
				OTELCollector: "otel.prod:4317", OTELService: "click-dog-monitor", OTELSecure: true,
			},
			want: []string{
				"ClickHouse  ch.prod:9440 (TLS)",
				"OTEL        otel.prod:4317 (TLS)",
			},
		},
		{
			name: "Splunk HEC branch",
			answers: wizardAnswers{
				CHHost: "ch", CHPort: 9000,
				OTELCollector: "otel:4317", OTELService: "click-dog-monitor",
				SplunkHECEnabled: true, SplunkEndpoint: "https://splunk:8088",
			},
			want: []string{"Splunk HEC  https://splunk:8088"},
		},
		{
			name: "HA branch — keeper hosts joined",
			answers: wizardAnswers{
				CHHost: "ch", CHPort: 9000,
				OTELCollector: "otel:4317", OTELService: "click-dog-monitor",
				KeeperHosts: []string{"k1:9181", "k2:9181", "k3:9181"},
			},
			want: []string{"HA          leader election — keeper k1:9181,k2:9181,k3:9181"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			writeWizardSummary(&out, tc.answers)
			body := out.String()
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("summary missing %q\n--- got ---\n%s", w, body)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(body, nw) {
					t.Errorf("summary should not contain %q in the no-branch case\n--- got ---\n%s", nw, body)
				}
			}
		})
	}
}

// TestWriteWizardNextSteps_ConfigFlagSuppression pins the -config suppression
// rule AND the step-5 remote-deploy bullet, which share the same predicate:
// when the wizard writes to a path ResolveConfigPath will find on its own
// (the bare relative default or the canonical /etc install path), the
// suggested commands omit -config and the remote-deploy bullet is hidden;
// otherwise both appear. Covers the fragile-suppression bug a reviewer
// flagged (leading "./" and the /etc path) and locks the install.sh -f
// handoff text against regressions.
func TestWriteWizardNextSteps_ConfigFlagSuppression(t *testing.T) {
	tests := []struct {
		name         string
		outputPath   string
		wantConfig   bool   // true → "-config <path>" must appear at least once AND step 5 fires
		wantPathFrag string // substring to find when wantConfig is true
	}{
		{name: "default relative path", outputPath: "click-dog.yaml", wantConfig: false},
		{name: "leading ./ on default", outputPath: "./click-dog.yaml", wantConfig: false},
		{name: "canonical /etc install path", outputPath: "/etc/click-dog/click-dog.yaml", wantConfig: false},
		{name: "non-default path", outputPath: "/tmp/cd-demo.yaml", wantConfig: true, wantPathFrag: "-config /tmp/cd-demo.yaml"},
	}

	answers := wizardAnswers{
		CHHost: "ch", CHPort: 9000,
		OTELCollector: "otel:4317", OTELService: "click-dog-monitor",
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			writeWizardNextSteps(&out, answers, tc.outputPath)
			body := out.String()
			hasConfig := strings.Contains(body, "-config ")
			if hasConfig != tc.wantConfig {
				t.Errorf("outputPath=%q: -config presence = %v, want %v\n--- got ---\n%s",
					tc.outputPath, hasConfig, tc.wantConfig, body)
			}
			if tc.wantConfig && !strings.Contains(body, tc.wantPathFrag) {
				t.Errorf("expected %q in next-steps output, got:\n%s", tc.wantPathFrag, body)
			}
			// Step 5 fires for the same set of paths -config does — it is
			// the remote-deploy handoff to `install.sh install -f`.
			hasStep5 := strings.Contains(body, "install.sh install -f")
			if hasStep5 != tc.wantConfig {
				t.Errorf("outputPath=%q: step 5 (install.sh -f handoff) presence = %v, want %v\n--- got ---\n%s",
					tc.outputPath, hasStep5, tc.wantConfig, body)
			}
			if tc.wantConfig {
				// Step 5 must reference the file the user actually wrote, by basename,
				// so a copy-paste lands a sensibly-named file in /tmp on the remote.
				wantBase := filepath.Base(tc.outputPath)
				if !strings.Contains(body, "/tmp/"+wantBase) {
					t.Errorf("step 5 should reference /tmp/%s on the remote, got:\n%s", wantBase, body)
				}
				// The remote must obtain install.sh itself (it isn't scp'd), and
				// install.sh downloads + verifies the release binary on the target —
				// so the recipe must not transfer a laptop binary, which would be the
				// wrong architecture/OS half the time. Guards the reviewer's P1.
				if !strings.Contains(body, "releases/latest/download/install.sh") {
					t.Errorf("step 5 should fetch install.sh on the target, got:\n%s", body)
				}
				if strings.Contains(body, "scp click-dog ") {
					t.Errorf("step 5 should not scp a laptop binary (cross-arch hazard), got:\n%s", body)
				}
			}
			// Splunk env var only when SplunkHECEnabled.
			if strings.Contains(body, "SPLUNK_HEC_TOKEN") {
				t.Errorf("SPLUNK_HEC_TOKEN should not appear when Splunk is disabled:\n%s", body)
			}
		})
	}
}

// TestWriteWizardNextSteps_SplunkBranch pins the Splunk-on path: the
// SPLUNK_HEC_TOKEN export appears under step 1 only when SplunkHECEnabled.
func TestWriteWizardNextSteps_SplunkBranch(t *testing.T) {
	var out bytes.Buffer
	writeWizardNextSteps(&out, wizardAnswers{
		CHHost: "ch", CHPort: 9000,
		OTELCollector: "otel:4317", OTELService: "click-dog-monitor",
		SplunkHECEnabled: true, SplunkEndpoint: "https://splunk:8088",
	}, "click-dog.yaml")
	body := out.String()
	if !strings.Contains(body, "export SPLUNK_HEC_TOKEN=...") {
		t.Errorf("Splunk-on: SPLUNK_HEC_TOKEN export must appear, got:\n%s", body)
	}
	// Step 1 must list both env vars, with the Splunk one after CLICKHOUSE_PASSWORD.
	chIdx := strings.Index(body, "CLICKHOUSE_PASSWORD")
	spIdx := strings.Index(body, "SPLUNK_HEC_TOKEN")
	if chIdx < 0 || spIdx < 0 || spIdx < chIdx {
		t.Errorf("expected CLICKHOUSE_PASSWORD before SPLUNK_HEC_TOKEN in step 1, got:\n%s", body)
	}
}

// TestWriteWizardNextSteps_PasswordFileBranch pins the file-based-secret path:
// when CHPasswordFile is set, step 1 must tell the operator to WRITE the secret
// to that file (mode 0600) rather than export CLICKHOUSE_PASSWORD — the systemd
// unit install.sh generates no longer injects the password via the environment.
func TestWriteWizardNextSteps_PasswordFileBranch(t *testing.T) {
	var out bytes.Buffer
	writeWizardNextSteps(&out, wizardAnswers{
		CHHost: "ch", CHPort: 9000,
		OTELCollector: "otel:4317", OTELService: "click-dog-monitor",
		CHPasswordFile: "/etc/click-dog/.secret",
	}, "click-dog.yaml")
	body := out.String()
	if !strings.Contains(body, "write the ClickHouse password to /etc/click-dog/.secret") {
		t.Errorf("file-secret path: expected a write-to-file instruction, got:\n%s", body)
	}
	if !strings.Contains(body, "0600") {
		t.Errorf("file-secret path: should mention mode 0600, got:\n%s", body)
	}
	if strings.Contains(body, "export CLICKHOUSE_PASSWORD") {
		t.Errorf("file-secret path: should NOT tell the user to export CLICKHOUSE_PASSWORD, got:\n%s", body)
	}
}

// TestWriteWizardNextSteps_PasswordFileRemoteDeploy pins the step-5 remote
// deploy block for the file-based path: when CHPasswordFile is set AND the
// output path is non-default (so step 5 fires), the `install -f` command must
// pass CLICKHOUSE_PASSWORD at install time — install.sh writes it to the
// password_file path, since the generated unit ships no EnvironmentFile. The
// old env-only phrasing ("CLICKHOUSE_PASSWORD matches it" with no password on
// the remote command) is wrong for this path.
func TestWriteWizardNextSteps_PasswordFileRemoteDeploy(t *testing.T) {
	var out bytes.Buffer
	writeWizardNextSteps(&out, wizardAnswers{
		CHHost: "ch", CHPort: 9000,
		OTELCollector: "otel:4317", OTELService: "click-dog-monitor",
		CHPasswordFile: "/etc/click-dog/.secret",
	}, "/tmp/cd-demo.yaml")
	body := out.String()
	// Step 5 must fire for a non-default output path.
	if !strings.Contains(body, "install.sh install -f") {
		t.Fatalf("expected the step-5 install -f handoff to fire for a non-default path, got:\n%s", body)
	}
	// The remote install -f command must carry the password so install.sh can
	// write the 0600 secret file the YAML points at.
	if !strings.Contains(body, "sudo CLICKHOUSE_PASSWORD=... bash /tmp/install.sh install -f /tmp/cd-demo.yaml --systemd") {
		t.Errorf("file-secret remote deploy: install -f command must pass CLICKHOUSE_PASSWORD, got:\n%s", body)
	}
	// And it should name the password_file the secret lands in.
	if !strings.Contains(body, "reads the password from /etc/click-dog/.secret") {
		t.Errorf("file-secret remote deploy: should reference the password_file path, got:\n%s", body)
	}
	// The env-only phrasing must not appear on this path.
	if strings.Contains(body, "CLICKHOUSE_PASSWORD matches it") {
		t.Errorf("file-secret remote deploy: stale env-only phrasing should be gone, got:\n%s", body)
	}
}
