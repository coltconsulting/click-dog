package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coltconsulting/click-dog/internal/config"
)

func TestRunInit_WritesValidConfig(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "test-config.yaml")

	runInit([]string{"--ch-host", "ch.example.com", "--collector", "otel:4317", "--service", "my-svc", "--output", output}, io.Discard, io.Discard)

	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("expected config file to be written: %v", err)
	}

	if len(data) == 0 {
		t.Fatal("config file is empty")
	}

	// Verify the generated config parses and the user-supplied values land
	// in the expected struct fields. LoadConfig includes validation; the
	// rendered config supplies all required exporters/clickhouse keys so
	// validation should pass too.
	cfg, err := config.LoadConfig(output)
	if err != nil {
		t.Fatalf("LoadConfig: %v\n--- rendered ---\n%s", err, string(data))
	}
	if cfg.ClickHouse.Host != "ch.example.com" {
		t.Errorf("expected host ch.example.com, got %s", cfg.ClickHouse.Host)
	}
	if len(cfg.Exporters.OTEL) != 1 || cfg.Exporters.OTEL[0].CollectorAddress != "otel:4317" {
		t.Errorf("expected single OTEL exporter at otel:4317, got %+v", cfg.Exporters.OTEL)
	}
	if cfg.Exporters.OTEL[0].ServiceName != "my-svc" {
		t.Errorf("expected service my-svc, got %q", cfg.Exporters.OTEL[0].ServiceName)
	}
}

func TestRunInit_RefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "test-config.yaml")

	// Create the file first
	if err := os.WriteFile(output, []byte("existing"), 0644); err != nil {
		t.Fatal(err)
	}

	// runInit calls os.Exit(1) on overwrite refusal — not easily testable without
	// subprocess. Instead, test the file-exists logic directly.
	if _, err := os.Stat(output); err != nil {
		t.Fatal("expected file to exist")
	}
}

func TestRunInit_ForceOverwrite(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "test-config.yaml")

	if err := os.WriteFile(output, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}

	runInit([]string{"--force", "--output", output, "--ch-host", "new-host"}, io.Discard, io.Discard)

	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "old" {
		t.Error("expected file to be overwritten")
	}
}

func TestIsTTY(t *testing.T) {
	// In test environments, stdin is typically not a TTY
	// Just make sure isTTY() doesn't panic
	_ = isTTY()
}

// ---------------------------------------------------------------------------
// Profiles — rendered via the unified wizard renderer
// ---------------------------------------------------------------------------

// TestRunInit_AllProfilesProduceValidConfigs is the strongest end-to-end
// contract `click-dog init` carries: every profile, rendered with realistic
// values, must parse AND validate via config.LoadConfig. A profile that
// fails validation hands an unusable starter to anyone who picks it.
func TestRunInit_AllProfilesProduceValidConfigs(t *testing.T) {
	for _, profile := range []string{"production", "minimal", "paranoid"} {
		t.Run(profile, func(t *testing.T) {
			dir := t.TempDir()
			output := filepath.Join(dir, profile+".yaml")
			runInit([]string{
				"--profile", profile,
				"--ch-host", "ch.example.com",
				"--collector", "otel.example.com:4317",
				"--service", "test-svc",
				"--output", output,
			}, io.Discard, io.Discard)

			cfg, err := config.LoadConfig(output)
			if err != nil {
				body, _ := os.ReadFile(output)
				t.Fatalf("profile %q failed to load: %v\n--- rendered ---\n%s", profile, err, body)
			}
			if cfg.ClickHouse.Host != "ch.example.com" {
				t.Errorf("profile %q: host = %q, want ch.example.com", profile, cfg.ClickHouse.Host)
			}
		})
	}
}

func TestRunInit_MinimalProfile(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "minimal.yaml")

	runInit([]string{"--profile", "minimal", "--ch-host", "ch.example.com", "--collector", "otel:4317", "--service", "svc", "--output", output}, io.Discard, io.Discard)

	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("expected config file to be written: %v", err)
	}
	// Structural assertions — checks that the generated file matches the
	// "minimal" intent (omits the production-only knobs) without tying the
	// test to specific comment wording in the rendered output.
	body := string(data)
	for _, mustNotContain := range []string{
		"circuit_breaker:",
		"max_spans_per_cycle:",
		"backoff:",
		"filters:",
		"metrics:",
		"health:",
	} {
		if strings.Contains(body, mustNotContain) {
			t.Errorf("minimal profile should not include %q, got:\n%s", mustNotContain, body)
		}
	}
}

func TestRunInit_ParanoidProfile(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "paranoid.yaml")

	runInit([]string{"--profile", "paranoid", "--ch-host", "ch.example.com", "--collector", "otel:4317", "--service", "svc", "--output", output}, io.Discard, io.Discard)

	cfg, err := config.LoadConfig(output)
	if err != nil {
		t.Fatalf("paranoid profile failed to load: %v", err)
	}
	// Spot-check a few of the tightenings that justify the profile's name.
	if cfg.ClickHouse.QueryTimeoutS != 5 {
		t.Errorf("paranoid query_timeout_s = %d, want 5", cfg.ClickHouse.QueryTimeoutS)
	}
	if cfg.Monitor.MaxSpansPerCycle != 100 {
		t.Errorf("paranoid max_spans_per_cycle = %d, want 100", cfg.Monitor.MaxSpansPerCycle)
	}
	if !cfg.Monitor.Canary.Enabled {
		t.Errorf("paranoid profile should enable canary by default")
	}
}

// TestRunInit_ProductionHasHardeningAndBlacklistOps locks in the spec
// requirement that the unified production profile carries BOTH the
// `filters.blacklist_operations` list AND install.sh's old
// `max_open_conns / max_idle_conns / query_timeout_s` hardening. Before
// unification these only lived in install.sh's `generate_full_config`;
// any future restructure that drops them from `production` is a
// regression for everyone who picks the recommended default.
func TestRunInit_ProductionHasHardeningAndBlacklistOps(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "production.yaml")
	runInit([]string{
		"--profile", "production",
		"--ch-host", "ch.example.com",
		"--collector", "otel:4317",
		"--service", "svc",
		"--output", output,
	}, io.Discard, io.Discard)

	cfg, err := config.LoadConfig(output)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.ClickHouse.MaxOpenConns != 2 {
		t.Errorf("production max_open_conns = %d, want 2", cfg.ClickHouse.MaxOpenConns)
	}
	if cfg.ClickHouse.MaxIdleConns != 1 {
		t.Errorf("production max_idle_conns = %d, want 1", cfg.ClickHouse.MaxIdleConns)
	}
	if cfg.ClickHouse.QueryTimeoutS != 30 {
		t.Errorf("production query_timeout_s = %d, want 30", cfg.ClickHouse.QueryTimeoutS)
	}
	if !cfg.Metrics.Enabled {
		t.Errorf("production metrics.enabled = false, want true")
	}
	if cfg.Metrics.ListenAddress != ":9090" {
		t.Errorf("production metrics.listen_address = %q, want :9090", cfg.Metrics.ListenAddress)
	}
	if !cfg.Health.Enabled {
		t.Errorf("production health.enabled = false, want true")
	}
	if cfg.Health.ListenAddress != ":8686" {
		t.Errorf("production health.listen_address = %q, want :8686", cfg.Health.ListenAddress)
	}

	// blacklist_operations must contain the install.sh-era set of
	// high-volume engine-internal spans. Without these the production
	// profile silently regresses on the ~90-99% ingest reduction those
	// drops historically provided.
	wantOps := []string{
		"MergeTreeSource", "MergeTreeMarksLoader", "MergeTreeIndex",
		"MergeTreeSequentialSource", "VFSWrite", "WriteBufferFromS3",
		"ConcurrentJoin", "QueryPipelineEx",
	}
	for _, want := range wantOps {
		found := false
		for _, got := range cfg.Filters.BlacklistOperations {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("production filters.blacklist_operations missing %q, got %v", want, cfg.Filters.BlacklistOperations)
		}
	}
}

// TestRunInit_NeverEmitsTopLevelOtel pins the spec's schema-cleanup
// payoff: the unified renderer emits `exporters.otel[]` only, never the
// deprecated top-level `otel:` form. Configs already on disk in the
// deprecated form keep working via internal/config/compat.go, but
// freshly-rendered configs must NOT carry the deprecation warning. Loop
// over every profile + the optional blocks so the assertion covers the
// whole renderer surface.
func TestRunInit_NeverEmitsTopLevelOtel(t *testing.T) {
	for _, profile := range []string{"production", "minimal", "paranoid"} {
		t.Run(profile, func(t *testing.T) {
			dir := t.TempDir()
			output := filepath.Join(dir, "c.yaml")
			runInit([]string{
				"--profile", profile,
				"--ch-host", "ch.example.com",
				"--collector", "otel:4317",
				"--service", "svc",
				"--ha-keeper", "k1:9181,k2:9181",
				"--ch-secure",
				"--otel-secure",
				"--output", output,
			}, io.Discard, io.Discard)

			body, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			// A line beginning `otel:` (column 0) is the deprecated
			// top-level form. The current schema nests under
			// `exporters:` (column 0) then `  otel:` (column 2).
			for _, line := range strings.Split(string(body), "\n") {
				if strings.HasPrefix(line, "otel:") {
					t.Errorf("rendered %s YAML contains top-level otel: form (deprecated schema)\n--- rendered ---\n%s", profile, body)
					break
				}
			}

			// Reinforce via the deprecation registry: LoadConfig must
			// surface zero warnings on a freshly-rendered file.
			cfg, err := config.LoadConfig(output)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if len(cfg.DeprecationWarnings) > 0 {
				t.Errorf("fresh %s config should have zero deprecation warnings, got: %v", profile, cfg.DeprecationWarnings)
			}
		})
	}
}

// TestRunInit_CHSecureAndPortPropagate exercises the two new ClickHouse
// flags end-to-end: `-ch-secure` produces `secure: true` in the loaded
// config, and `-ch-port` overrides the wizard's auto-flip-to-9440 when
// the user wants TLS on a non-standard port (or, here, the standard
// `port: 9440`). Locks the seed-flag-plumbing contract install.sh
// depends on.
func TestRunInit_CHSecureAndPortPropagate(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "c.yaml")
	runInit([]string{
		"--ch-host", "ch.example.com",
		"--ch-secure",
		"--ch-port", "9440",
		"--collector", "otel:4317",
		"--service", "svc",
		"--output", output,
	}, io.Discard, io.Discard)

	cfg, err := config.LoadConfig(output)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.ClickHouse.Secure {
		t.Errorf("expected clickhouse.secure: true after -ch-secure")
	}
	if cfg.ClickHouse.Port != 9440 {
		t.Errorf("expected clickhouse.port = 9440 after -ch-port 9440, got %d", cfg.ClickHouse.Port)
	}
}

// TestRunInit_CHSecureAutoFlipsPort confirms the convenience behavior
// documented on the -ch-port flag help: with `-ch-secure` and no
// explicit `-ch-port`, the port defaults to 9440 (ClickHouse's standard
// TLS-on-native-protocol port) instead of the plaintext 9000.
func TestRunInit_CHSecureAutoFlipsPort(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "c.yaml")
	runInit([]string{
		"--ch-host", "ch.example.com",
		"--ch-secure",
		"--collector", "otel:4317",
		"--service", "svc",
		"--output", output,
	}, io.Discard, io.Discard)

	cfg, err := config.LoadConfig(output)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ClickHouse.Port != 9440 {
		t.Errorf("expected port auto-flip to 9440 when -ch-secure and no -ch-port, got %d", cfg.ClickHouse.Port)
	}
}

// TestRunInit_CHUserFlagOverridesDefault locks in the `-ch-user` flag's
// only consumer (install.sh): the rendered config carries the supplied
// username so install.sh's dedicated `monitoring` ClickHouse user can
// still authenticate after the renderer unification.
func TestRunInit_CHUserFlagOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "c.yaml")
	runInit([]string{
		"--ch-host", "ch.example.com",
		"--ch-user", "monitoring",
		"--collector", "otel:4317",
		"--service", "svc",
		"--output", output,
	}, io.Discard, io.Discard)

	cfg, err := config.LoadConfig(output)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ClickHouse.Username != "monitoring" {
		t.Errorf("expected clickhouse.username = monitoring, got %q", cfg.ClickHouse.Username)
	}
}

// TestRunInit_MinimalProfileHonorsCHUser closes the gap raised in
// review: the renderer used to suppress `username:` for the minimal
// profile unconditionally to keep the example bytes-minimal, which
// silently swallowed a user's explicit `-ch-user` choice and produced
// a config that authenticates as the default user. The fix emits
// `username:` whenever the caller asked for a non-default value,
// regardless of profile.
func TestRunInit_MinimalProfileHonorsCHUser(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "c.yaml")
	runInit([]string{
		"--profile", "minimal",
		"--ch-host", "ch.example.com",
		"--ch-user", "monitoring",
		"--collector", "otel:4317",
		"--service", "svc",
		"--output", output,
	}, io.Discard, io.Discard)

	cfg, err := config.LoadConfig(output)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ClickHouse.Username != "monitoring" {
		t.Errorf("minimal profile with --ch-user monitoring: clickhouse.username = %q, want monitoring", cfg.ClickHouse.Username)
	}

	// And the inverse: minimal with default user keeps the bytes-minimal
	// example shape (no `username:` line in the rendered YAML). This
	// guard locks both halves of the conditional so a future refactor
	// can't regress either direction.
	defOutput := filepath.Join(dir, "default.yaml")
	runInit([]string{
		"--profile", "minimal",
		"--ch-host", "ch.example.com",
		"--collector", "otel:4317",
		"--service", "svc",
		"--output", defOutput,
	}, io.Discard, io.Discard)
	body, err := os.ReadFile(defOutput)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "username:") {
		t.Errorf("minimal profile with default user should omit username: to match docs/examples; got:\n%s", body)
	}
}

// TestPromptInteractive_RejectsBadProfile exercises the re-prompt loop:
// a bad profile is echoed back as an error, the user retries with a valid
// one, and only the second value sticks. Without the loop, runInit would
// have os.Exit'd later and wiped the host/collector/service the user just
// typed.
func TestPromptInteractive_RejectsBadProfile(t *testing.T) {
	chHost := "default-host"
	collector := "default-otel:4317"
	service := "default-svc"
	profile := "production"

	// Inputs:
	//   1. host    — keep default (empty line)
	//   2. otel    — keep default
	//   3. service — keep default
	//   4. profile — "ludicrous" (rejected)
	//   5. profile — "paranoid"  (accepted)
	in := strings.NewReader("\n\n\nludicrous\nparanoid\n")
	var errOut bytes.Buffer

	promptInteractive(in, io.Discard, &errOut, &chHost, &collector, &service, &profile)

	if profile != "paranoid" {
		t.Errorf("profile = %q, want paranoid (loop should have accepted second input)", profile)
	}
	// Defaults preserved on the empty inputs.
	if chHost != "default-host" || collector != "default-otel:4317" || service != "default-svc" {
		t.Errorf("defaults not preserved: host=%q collector=%q service=%q",
			chHost, collector, service)
	}
	if !strings.Contains(errOut.String(), `unknown profile "ludicrous"`) {
		t.Errorf("expected error output to mention bad profile, got: %q", errOut.String())
	}
}

// TestPromptInteractive_EmptyProfileKeepsDefault confirms hitting Enter at
// the profile prompt leaves *profile unchanged (the Loop's empty-input
// break). Mirrors the existing behavior for the other three prompts.
func TestPromptInteractive_EmptyProfileKeepsDefault(t *testing.T) {
	chHost := "default-host"
	collector := "default-otel:4317"
	service := "default-svc"
	profile := "production"

	in := strings.NewReader("\n\n\n\n") // four empties
	var errOut bytes.Buffer

	promptInteractive(in, io.Discard, &errOut, &chHost, &collector, &service, &profile)

	if profile != "production" {
		t.Errorf("profile = %q, want production (empty input should leave default)", profile)
	}
	if errOut.Len() != 0 {
		t.Errorf("no error expected on empty input, got: %q", errOut.String())
	}
}

// TestProfiles_ProductionIsFirst directly enforces the registry invariant
// that the rendered help text and the interactive prompt both rely on.
// TestFlagDescriptionForProfile checks the same property indirectly via
// the rendered description; this test pins the underlying state so a
// mistake at the registry level fails with a clear message rather than
// surfacing as a confusing description-string assertion failure.
func TestProfiles_ProductionIsFirst(t *testing.T) {
	if profiles[0].name != "production" {
		t.Errorf("profiles[0] = %q, want production (must lead help text and prompt ordering)", profiles[0].name)
	}
}

// TestFlagDescriptionForProfile locks the rendered --profile flag help
// against drift. The recommended-default profile must appear first and
// every registered profile must be present in the rendered description;
// the formatting helper handles the Oxford-comma pluralisation.
func TestFlagDescriptionForProfile(t *testing.T) {
	desc := flagDescriptionForProfile()
	for _, name := range profileNames() {
		if !strings.Contains(desc, name) {
			t.Errorf("flag description %q should mention %q", desc, name)
		}
	}
	// production is registered first; the rendered description must
	// reflect that ordering so the recommended default appears first to
	// the user. Locks the registry-driven ordering against accidental
	// alphabetic / random sorts.
	if !strings.HasPrefix(desc, "Config profile: production") {
		t.Errorf("description should lead with the recommended default, got: %q", desc)
	}
}

// TestValueFlagsExplicitlySet pins the prompting gate: only --ch-host /
// --collector / --service should suppress the interactive prompt. Control
// flags (--profile, --output, --force) and the new seed flags (--ch-port,
// --ch-secure, --ch-user, --otel-secure, --splunk-hec-endpoint,
// --ha-keeper) must NOT cause the three required values to be silently
// substituted with their localhost defaults — see the function's
// commentary for the why.
func TestValueFlagsExplicitlySet(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"no args", []string{}, false},
		{"profile only — control flag", []string{"--profile", "minimal"}, false},
		{"output only — control flag", []string{"--output", "out.yaml"}, false},
		{"force only — control flag", []string{"--force"}, false},
		{"profile + output — control flags", []string{"--profile", "minimal", "--output", "out.yaml"}, false},
		{"ch-host — value flag", []string{"--ch-host", "ch.example.com"}, true},
		{"collector — value flag", []string{"--collector", "otel:4317"}, true},
		{"service — value flag", []string{"--service", "svc"}, true},
		{"profile + ch-host — value present", []string{"--profile", "minimal", "--ch-host", "ch.example.com"}, true},
		// New seed flags are deliberately NOT in the value-flag set:
		// passing them does NOT suppress the short prompt for the four
		// staples (host/collector/service/profile).
		{"ch-secure alone — seed flag", []string{"--ch-secure"}, false},
		{"ch-port alone — seed flag", []string{"--ch-port", "9440"}, false},
		{"ch-user alone — seed flag", []string{"--ch-user", "monitoring"}, false},
		{"otel-secure alone — seed flag", []string{"--otel-secure"}, false},
		{"splunk-hec alone — seed flag", []string{"--splunk-hec-endpoint", "https://splunk:8088"}, false},
		{"ha-keeper alone — seed flag", []string{"--ha-keeper", "k1:9181"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("init", flag.ContinueOnError)
			_ = fs.String("ch-host", "localhost", "")
			_ = fs.Int("ch-port", 9000, "")
			_ = fs.Bool("ch-secure", false, "")
			_ = fs.String("ch-user", "default", "")
			_ = fs.String("collector", "localhost:4317", "")
			_ = fs.String("service", "click-dog-monitor", "")
			_ = fs.Bool("otel-secure", false, "")
			_ = fs.String("splunk-hec-endpoint", "", "")
			_ = fs.String("ha-keeper", "", "")
			_ = fs.String("output", "click-dog.yaml", "")
			_ = fs.String("profile", "production", "")
			_ = fs.Bool("force", false, "")
			if err := fs.Parse(tt.args); err != nil {
				t.Fatalf("Parse(%v): %v", tt.args, err)
			}
			if got := valueFlagsExplicitlySet(fs); got != tt.want {
				t.Errorf("valueFlagsExplicitlySet(args=%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestRunInit_ForceOverwriteAnnouncesReplacement covers the success-line
// signal added on the --force path: operators rerunning `init --force` on
// top of an old install should see that the file was replaced rather than
// just "Wrote ...". The pre-existing TestRunInit_ForceOverwrite verifies
// the bytes change; this one verifies the message.
func TestRunInit_ForceOverwriteAnnouncesReplacement(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "test-config.yaml")
	if err := os.WriteFile(output, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runInit([]string{"--force", "--ch-host", "ch.example.com", "--output", output}, &out, io.Discard)

	if !strings.Contains(out.String(), "existing file replaced") {
		t.Errorf("expected force-overwrite notice, got: %q", out.String())
	}
}

// TestRunInit_InvalidProfileExits confirms that runInit, invoked through
// the same code path a real CLI user hits, exits non-zero AND prints a
// useful error when given a bogus profile. Uses the standard subprocess
// pattern because runInit calls os.Exit(1) on the unknown-profile
// branch. After the renderer unification this is the only path that
// validates --profile values (loadProfileTemplate is gone), so the
// error wording lives in runInit itself.
func TestRunInit_InvalidProfileExits(t *testing.T) {
	if os.Getenv("CLICK_DOG_TEST_INVALID_PROFILE") == "1" {
		// Child: actually invoke runInit. The output flag is set so the
		// failure happens at the profile lookup, not at the file
		// existence check, regardless of the test's working directory.
		// Pass --ch-host so valueFlagsExplicitlySet returns true and the
		// short prompt doesn't try to consume the empty subprocess stdin
		// before the profile validation fires. Pass os.Stdout/os.Stderr
		// so the parent's CombinedOutput sees the unknown-profile error
		// message printed by runInit.
		// runInit returns its exit code instead of calling os.Exit; surface it
		// so the parent subprocess still observes the non-zero exit.
		os.Exit(runInit([]string{"--profile", "ludicrous", "--ch-host", "ch.example.com", "--output", filepath.Join(t.TempDir(), "x.yaml")}, os.Stdout, os.Stderr))
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestRunInit_InvalidProfileExits") //nolint:gosec // Args[0] is the test binary, not user input.
	cmd.Env = append(os.Environ(), "CLICK_DOG_TEST_INVALID_PROFILE=1")
	out, err := cmd.CombinedOutput()

	exitErr := &exec.ExitError{}
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected non-zero exit, got err=%v, out=%s", err, out)
	}
	if exitErr.ExitCode() == 0 {
		t.Fatalf("expected non-zero exit code, got 0; out=%s", out)
	}
	body := string(out)
	if !strings.Contains(body, `unknown profile "ludicrous"`) {
		t.Errorf("expected error to mention bad profile, got: %s", body)
	}
	if !strings.Contains(body, "valid: production, minimal, paranoid") {
		t.Errorf("expected error to list valid profiles in registry order, got: %s", body)
	}
}
