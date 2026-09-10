package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/coltconsulting/click-dog/internal/config"
	"gopkg.in/yaml.v3"
)

// runInitWizard is the --wizard entry point called from runInit. It collects
// answers (seeding defaults from any flags the user passed) and writes the
// rendered YAML to outputPath. Refuses to overwrite an existing file unless
// force is set, matching runInit's non-wizard contract. Returns the process
// exit code (0 on success) so the caller (runInit) returns it directly.
//
// The long parameter list mirrors runInit's flag set so the wizard can seed
// its prompts from any flag the user supplied. Empty / zero values fall
// back to defaultWizardAnswers; non-zero values override.
func runInitWizard(in io.Reader, out, errOut io.Writer, fs *flag.FlagSet, chHost string, chPort int, chSecure bool, chUser, chPasswordFile, collector, service string, otelSecure bool, splunkHEC, haKeeper, profile, outputPath string, force bool) int {
	// Fail-fast on a pre-existing file before any prompts run. Without this
	// the user answered all nine wizard questions, then learnt the output
	// path was taken — every typed answer thrown away. Walk this check up
	// here so the refusal arrives before the keystrokes.
	replacedExisting := false
	if _, err := os.Stat(outputPath); err == nil {
		if !force {
			_, _ = fmt.Fprintf(errOut, "Error: %s already exists (use --force to overwrite)\n", outputPath)
			return 1
		}
		replacedExisting = true
	}

	// Reject an invalid --profile flag value before the wizard treats it as
	// a seeded default. promptProfile only validates new user input, so an
	// invalid flag value would be displayed as "[ludicrous]:" and silently
	// stick if the user pressed Enter. runInit's non-wizard path makes the
	// same guarantee; mirror its error wording so a shell-level typo
	// produces the same diagnostic regardless of branch.
	if !isValidProfile(profile) {
		_, _ = fmt.Fprintf(errOut, "Error: unknown profile %q; valid: %s\n", profile, strings.Join(profileNames(), ", "))
		return 1
	}

	defaults := defaultWizardAnswers()
	// Seed the wizard from any value/control flag the user supplied. Each
	// non-default flag value overrides the wizard's built-in default so a
	// power-user can pre-fill answers without losing the option to change
	// them at the prompt.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "ch-host":
			defaults.CHHost = chHost
		case "ch-port":
			defaults.CHPort = chPort
		case "ch-secure":
			defaults.CHSecure = chSecure
		case "ch-user":
			defaults.CHUser = chUser
		case "collector":
			defaults.OTELCollector = collector
		case "service":
			defaults.OTELService = service
		case "otel-secure":
			defaults.OTELSecure = otelSecure
		case "splunk-hec-endpoint":
			if splunkHEC != "" {
				defaults.SplunkHECEnabled = true
				defaults.SplunkEndpoint = splunkHEC
			}
		case "ha-keeper":
			if haKeeper != "" {
				defaults.KeeperHosts = splitAndTrim(haKeeper)
			}
		case "profile":
			defaults.Profile = profile
		}
	})

	a, eof := runWizard(in, out, errOut, defaults)
	// EOF mid-wizard means stdin closed before the user finished answering.
	// Without this check, the wizard would happily write a config full of
	// silent defaults for the unanswered tail (e.g. localhost:9181 Keeper
	// hosts the user never picked). Refuse the write and exit non-zero so
	// the user re-runs with the full input.
	if eof {
		_, _ = fmt.Fprintf(errOut, "Error: stdin closed before all wizard questions were answered; not writing %s\n", outputPath)
		return 1
	}
	// The wizard doesn't prompt for the secret file path (it's an install.sh /
	// power-user concern, not one of the nine interactive questions); seed it
	// straight from the --ch-password-file flag so renderWizardYAML emits
	// clickhouse.password_file instead of password: ${CLICKHOUSE_PASSWORD}.
	a.CHPasswordFile = chPasswordFile
	content := renderWizardYAML(a)

	// See cmd_init.go's write: the wizard's replace path has the same contract,
	// so it takes the same mode-reasserting write rather than os.WriteFile.
	if err := writePrivateFile(outputPath, []byte(content), 0600); err != nil {
		_, _ = fmt.Fprintf(errOut, "Error writing config: %v\n", err)
		return 1
	}

	suffix := ""
	if replacedExisting {
		suffix = " — existing file replaced"
	}
	_, _ = fmt.Fprintf(out, "Wrote %s (wizard, profile: %s%s)\n", outputPath, a.Profile, suffix)
	writeWizardSummary(out, a)
	writeWizardNextSteps(out, a, outputPath)
	return 0
}

// writeWizardSummary echoes the captured choices back. Nine prompts is enough
// that a typo on question two has scrolled off-screen by the time the wizard
// exits — re-printing the resolved values lets the user sanity-check before
// running anything.
func writeWizardSummary(out io.Writer, a wizardAnswers) {
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Configured:")
	chTLS := ""
	if a.CHSecure {
		chTLS = " (TLS)"
	}
	_, _ = fmt.Fprintf(out, "  ClickHouse  %s:%d%s\n", a.CHHost, a.CHPort, chTLS)
	otelTLS := ""
	if a.OTELSecure {
		otelTLS = " (TLS)"
	}
	_, _ = fmt.Fprintf(out, "  OTEL        %s%s — service %q\n", a.OTELCollector, otelTLS, a.OTELService)
	if a.SplunkHECEnabled {
		_, _ = fmt.Fprintf(out, "  Splunk HEC  %s\n", a.SplunkEndpoint)
	}
	topology := "sidecar — deploy once per ClickHouse node"
	if a.UseClusterQueries {
		topology = fmt.Sprintf("cluster %q — whole-cluster reads, leader-gated", a.Cluster)
	}
	_, _ = fmt.Fprintf(out, "  Topology    %s\n", topology)
	if len(a.KeeperHosts) > 0 {
		_, _ = fmt.Fprintf(out, "  HA          leader election — keeper %s\n", strings.Join(a.KeeperHosts, ","))
	}
}

// writeWizardNextSteps prints a copy-paste-friendly checklist for what to do
// with the freshly written config. --config is omitted from the suggested
// commands when the output path is one ResolveConfigPath will find on its own
// (the bare relative default or the canonical system install location), so the
// commands are as short as they can be for the common cases.
func writeWizardNextSteps(out io.Writer, a wizardAnswers, outputPath string) {
	cfgFlag := ""
	clean := filepath.Clean(outputPath)
	if clean != config.DefaultConfigFlag && clean != config.DefaultConfigPath {
		cfgFlag = " --config " + outputPath
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Next steps:")
	_, _ = fmt.Fprintln(out, "  1. Set credentials:")
	if a.CHPasswordFile != "" {
		// File-based secret: the config reads the password from this path, so
		// there's nothing to export — write the raw secret to the file (0600)
		// instead. Mirrors install.sh, which provisions this file for the unit.
		_, _ = fmt.Fprintf(out, "       write the ClickHouse password to %s (mode 0600)\n", a.CHPasswordFile)
	} else {
		_, _ = fmt.Fprintln(out, "       export CLICKHOUSE_PASSWORD=...")
	}
	if a.SplunkHECEnabled {
		_, _ = fmt.Fprintln(out, "       export SPLUNK_HEC_TOKEN=...")
	}
	_, _ = fmt.Fprintln(out, "  2. Validate config and connectivity:")
	_, _ = fmt.Fprintf(out, "       click-dog check%s\n", cfgFlag)
	_, _ = fmt.Fprintln(out, "  3. Test exporter delivery:")
	_, _ = fmt.Fprintf(out, "       click-dog test export%s\n", cfgFlag)
	_, _ = fmt.Fprintln(out, "  4. Test native ClickHouse trace propagation:")
	_, _ = fmt.Fprintf(out, "       click-dog test tracing%s\n", cfgFlag)
	_, _ = fmt.Fprintln(out, "  5. Start scheduled monitoring:")
	_, _ = fmt.Fprintf(out, "       click-dog%s\n", cfgFlag)

	// Step 6 only when the user wrote somewhere other than the two paths
	// the binary already searches by default — that's the signal they're
	// rendering locally to deploy elsewhere. Suppressing it for the
	// canonical paths keeps the next-steps short for the "just run it here"
	// flow.
	if clean != config.DefaultConfigFlag && clean != config.DefaultConfigPath {
		base := filepath.Base(outputPath)
		_, _ = fmt.Fprintln(out, "  6. Or deploy to a remote Linux host with systemd.")
		_, _ = fmt.Fprintln(out, "     Copy the YAML, then download the current installer on the target.")
		_, _ = fmt.Fprintln(out, "     It always checks the release archive SHA-256; with cosign installed it")
		_, _ = fmt.Fprintln(out, "     also authenticates the signed manifest. Cosign is recommended, not required.")
		if a.CHPasswordFile != "" {
			// File-based config: the systemd unit ships no EnvironmentFile, so
			// an env export on the target never reaches the service. The
			// password must be passed to install.sh at install time — it writes
			// the raw 0600 secret to the password_file path the YAML points at.
			_, _ = fmt.Fprintf(out, "     The YAML reads the password from %s; pass CLICKHOUSE_PASSWORD at\n", a.CHPasswordFile)
			_, _ = fmt.Fprintln(out, "     install time so install.sh writes that file (0600). The ClickHouse")
			_, _ = fmt.Fprintln(out, "     user named in the YAML must already exist with that password:")
			_, _ = fmt.Fprintf(out, "       scp %s HOST:/tmp/\n", outputPath)
			_, _ = fmt.Fprintf(out, "       ssh HOST 'curl -fsSL https://github.com/coltconsulting/click-dog/releases/latest/download/install.sh -o /tmp/install.sh && sudo CLICKHOUSE_PASSWORD=... bash /tmp/install.sh install -f /tmp/%s --systemd'\n", base)
		} else {
			_, _ = fmt.Fprintln(out, "     The YAML owns clickhouse.username/password, so make sure that")
			_, _ = fmt.Fprintln(out, "     ClickHouse user exists and CLICKHOUSE_PASSWORD matches it:")
			_, _ = fmt.Fprintf(out, "       scp %s HOST:/tmp/\n", outputPath)
			_, _ = fmt.Fprintf(out, "       ssh HOST 'curl -fsSL https://github.com/coltconsulting/click-dog/releases/latest/download/install.sh -o /tmp/install.sh && sudo bash /tmp/install.sh install -f /tmp/%s --systemd'\n", base)
		}
	}
}

// wizardAnswers captures the user's responses from runWizard. Defaults live
// in defaultWizardAnswers; renderWizardYAML treats the populated struct as
// the source of truth and does not consult package globals.
//
// CHUser is exposed only as a `--ch-user` flag on `click-dog init`; the
// interactive prompts (short + wizard) do not ask for it. install.sh's
// quickstart provisions a dedicated `monitoring` ClickHouse user and passes
// the name through so the rendered config authenticates with the matching
// credentials. End users overwhelmingly want `default`, which is the field's
// default.
type wizardAnswers struct {
	Profile          string
	CHHost           string
	CHPort           int
	CHSecure         bool
	CHUser           string
	OTELCollector    string
	OTELService      string
	OTELSecure       bool
	SplunkHECEnabled bool
	SplunkEndpoint   string
	// KeeperHosts are the ClickHouse Keeper endpoints. Their presence is what
	// turns on leader election — there is no separate HA toggle.
	KeeperHosts []string
	// UseClusterQueries selects the cluster topology: this reader covers the
	// whole cluster via cluster(...) and is leader-gated. False is the sidecar
	// topology (one reader per node, the default). Cluster mode implies a
	// Keeper-coordinated election, so it also collects KeeperHosts.
	UseClusterQueries bool
	// Cluster is the ClickHouse cluster name interpolated into cluster('name', …);
	// only meaningful (and only emitted) when UseClusterQueries is true.
	Cluster string
	// CHPasswordFile, when set, makes the renderer emit
	// `clickhouse.password_file: <path>` instead of the default
	// `password: ${CLICKHOUSE_PASSWORD}`. install.sh uses this so the systemd
	// service reads the secret from a 0600 file rather than its environment.
	CHPasswordFile string
}

// defaultWizardAnswers returns the baseline the wizard prompts against. The
// values mirror the non-interactive `click-dog init` defaults so a user who
// presses Enter through the entire wizard ends up with the same config as
// `click-dog init` with no flags.
func defaultWizardAnswers() wizardAnswers {
	return wizardAnswers{
		Profile:       "production",
		CHHost:        "localhost",
		CHPort:        9000,
		CHUser:        "default",
		OTELCollector: "localhost:4317",
		OTELService:   "click-dog-monitor",
	}
}

// wizPrompter wraps a bufio.Reader and tracks whether any read hit EOF.
// runWizard surfaces the flag back to the caller (via the second return
// value) so a truncated stdin doesn't silently produce a config file using
// defaults for every unanswered prompt.
type wizPrompter struct {
	r      *bufio.Reader
	sawEOF bool
}

// readLine reads one line, trimmed. Mirrors the package-level readLine's
// "EOF or read error with no buffered data → return empty" contract so
// existing tests that drain stdin cleanly still work; the difference is
// that sawEOF flips on the first such read so the caller can refuse to
// commit results.
func (w *wizPrompter) readLine() string {
	line, err := w.r.ReadString('\n')
	if err != nil {
		w.sawEOF = true
		if line == "" {
			return ""
		}
	}
	return strings.TrimSpace(line)
}

// runWizard executes the interactive configurator. The caller passes in the
// starting defaults (typically defaultWizardAnswers overlaid with any flags
// the user supplied) and gets back the populated struct plus a boolean
// indicating whether stdin closed mid-wizard. runInitWizard treats true as
// "don't write the file".
//
// Empty input keeps the default. Invalid yes/no, profile, or port responses
// re-prompt rather than exit — eight earlier answers shouldn't be wiped by a
// typo on question nine.
func runWizard(in io.Reader, out, errOut io.Writer, a wizardAnswers) (wizardAnswers, bool) {
	w := &wizPrompter{r: bufio.NewReader(in)}

	_, _ = fmt.Fprintln(out, "click-dog configurator — answer a few questions to generate a starter config.")
	_, _ = fmt.Fprintln(out, "Press Enter at any prompt to accept the [default] in brackets.")
	_, _ = fmt.Fprintln(out)

	a.Profile = promptProfile(w, out, errOut, a.Profile)
	a.CHHost = promptString(w, out, "ClickHouse host", a.CHHost)
	a.CHSecure = promptYesNo(w, out, errOut, "Use TLS to connect to ClickHouse?", a.CHSecure)
	// Default port follows the TLS choice so the common case (TLS on the
	// native protocol = port 9440) doesn't require a manual override.
	defaultPort := a.CHPort
	if a.CHSecure && defaultPort == 9000 {
		defaultPort = 9440
	}
	// Bounds mirror the LoadConfig validator (`clickhouse.port must be
	// between 0 and 65535`) but with a min of 1: a port of 0 is structurally
	// valid in the YAML but unusable for a TCP client. Catching out-of-range
	// at the prompt prevents a "wizard succeeded, click-dog won't start"
	// gap.
	a.CHPort = promptInt(w, out, errOut, "ClickHouse port", defaultPort, 1, 65535)
	a.OTELCollector = promptString(w, out, "OTEL collector address", a.OTELCollector)
	a.OTELService = promptString(w, out, "Service name for exported spans", a.OTELService)
	a.OTELSecure = promptYesNo(w, out, errOut, "Use TLS for the OTEL collector connection?", a.OTELSecure)
	a.SplunkHECEnabled = promptYesNo(w, out, errOut, "Also export to Splunk HEC?", a.SplunkHECEnabled)
	if a.SplunkHECEnabled {
		ep := a.SplunkEndpoint
		if ep == "" {
			ep = "https://splunk:8088"
		}
		a.SplunkEndpoint = promptString(w, out, "Splunk HEC endpoint", ep)
	}
	// Deployment topology. sidecar (default) reads this node's local span log
	// and is deployed once per node; cluster reads the whole cluster via
	// cluster(...) and is always leader-gated — so it implies a
	// Keeper-coordinated election rather than a separate HA toggle.
	a.UseClusterQueries = promptTopology(w, out, errOut, a.UseClusterQueries)
	if a.UseClusterQueries {
		def := a.Cluster
		if def == "" {
			def = "main"
		}
		a.Cluster = promptString(w, out, "ClickHouse cluster name", def)
		// Cluster mode is always leader-gated: collect Keeper hosts. Their
		// presence is what enables election — there is no separate HA toggle.
		a.KeeperHosts = promptKeeperHosts(w, out, errOut, a.KeeperHosts)
	} else {
		// Sidecar may still opt into Keeper-coordinated leadership (for /clusterz
		// + flush coordination); the export gate is a no-op there. Election is
		// driven by the presence of Keeper hosts, so a "no" leaves hosts empty.
		if promptYesNo(w, out, errOut, "Enable leader election via ClickHouse Keeper?", len(a.KeeperHosts) > 0) {
			a.KeeperHosts = promptKeeperHosts(w, out, errOut, a.KeeperHosts)
		} else {
			a.KeeperHosts = nil
		}
	}
	return a, w.sawEOF
}

// promptTopology asks for the deployment topology: sidecar (one reader per node,
// the default) or cluster (one reader covers the whole cluster via cluster(...),
// leader-gated). Returns true for cluster. Re-prompts on an unrecognized answer;
// bails on EOF so a truncated stdin keeps the current default.
func promptTopology(w *wizPrompter, out, errOut io.Writer, current bool) bool {
	def := "sidecar"
	if current {
		def = "cluster"
	}
	for {
		_, _ = fmt.Fprintf(out, "Deployment topology (sidecar/cluster) [%s]: ", def)
		switch strings.ToLower(w.readLine()) {
		case "":
			return current
		case "sidecar":
			return false
		case "cluster":
			return true
		default:
			_, _ = fmt.Fprintln(errOut, "please answer sidecar or cluster")
			if w.sawEOF {
				return current
			}
		}
	}
}

// promptKeeperHosts prompts for the comma-separated Keeper host list, re-prompting
// until at least one host is given. Hosts presence is what enables election, so
// an empty list here would silently leave a cluster instance ungated; collecting
// at least one keeps the freshly written config coherent. Empty input accepts the
// default (promptString's contract); a value that parses to zero hosts (", , ,")
// re-prompts. Bails on EOF, returning the prior list, so a truncated stdin doesn't
// spin forever on no input.
func promptKeeperHosts(w *wizPrompter, out, errOut io.Writer, current []string) []string {
	def := "localhost:9181"
	if len(current) > 0 {
		def = strings.Join(current, ",")
	}
	for {
		hosts := splitAndTrim(promptString(w, out, "Keeper hosts (comma-separated)", def))
		if len(hosts) > 0 {
			return hosts
		}
		if w.sawEOF {
			return current
		}
		_, _ = fmt.Fprintln(errOut, "at least one Keeper host is required")
	}
}

// promptProfile is the profile prompt with a re-prompt loop. Mirrors the
// behavior of the simple promptInteractive's trailing profile loop so a bad
// answer doesn't kick the user out of the wizard. Bails on EOF so a
// truncated stdin doesn't spin the re-prompt loop forever.
func promptProfile(w *wizPrompter, out, errOut io.Writer, current string) string {
	names := profileNames()
	for {
		_, _ = fmt.Fprintf(out, "Profile (%s) [%s]: ", strings.Join(names, "/"), current)
		line := w.readLine()
		if line == "" {
			return current
		}
		if isValidProfile(line) {
			return line
		}
		_, _ = fmt.Fprintf(errOut, "unknown profile %q; valid: %s\n", line, strings.Join(names, ", "))
		if w.sawEOF {
			return current
		}
	}
}

func promptString(w *wizPrompter, out io.Writer, label, def string) string {
	_, _ = fmt.Fprintf(out, "%s [%s]: ", label, def)
	if line := w.readLine(); line != "" {
		return line
	}
	return def
}

// promptInt prompts for an integer in the inclusive [min, max] range.
// Bounding here rather than only on the lower side catches values that
// LoadConfig would reject later — e.g. a typed `99999` for a port would
// write a config that then fails `clickhouse.port must be between 0 and
// 65535` on next start. Empty input keeps the default; out-of-range or
// non-numeric input re-prompts. The error message names the legal range
// so the user can self-correct without consulting docs.
func promptInt(w *wizPrompter, out, errOut io.Writer, label string, def, minVal, maxVal int) int {
	for {
		_, _ = fmt.Fprintf(out, "%s [%d]: ", label, def)
		line := w.readLine()
		if line == "" {
			return def
		}
		n, err := strconv.Atoi(line)
		if err != nil || n < minVal || n > maxVal {
			_, _ = fmt.Fprintf(errOut, "invalid integer %q; enter a value between %d and %d, or press Enter for %d\n", line, minVal, maxVal, def)
			if w.sawEOF {
				return def
			}
			continue
		}
		return n
	}
}

// promptYesNo accepts y/yes/n/no (case-insensitive). Empty input keeps the
// default. Anything else re-prompts. Matched against the same set of answers
// most CLIs accept so muscle memory works. Bails on EOF after surfacing the
// last bad answer so the wizard doesn't loop forever on a closed stdin.
func promptYesNo(w *wizPrompter, out, errOut io.Writer, label string, def bool) bool {
	defStr := "n"
	if def {
		defStr = "y"
	}
	for {
		_, _ = fmt.Fprintf(out, "%s (y/n) [%s]: ", label, defStr)
		line := strings.ToLower(w.readLine())
		switch line {
		case "":
			return def
		case "y", "yes":
			return true
		case "n", "no":
			return false
		default:
			_, _ = fmt.Fprintf(errOut, "please answer y or n (got %q)\n", line)
			if w.sawEOF {
				return def
			}
		}
	}
}

// splitAndTrim splits a comma-separated string and returns the non-empty,
// whitespace-trimmed parts. Used for the Keeper hosts prompt where users may
// reasonably type "host1:9181, host2:9181" with a space after the comma.
func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// renderWizardYAML emits a fully assembled YAML config from the answers. It
// is the *only* renderer in click-dog; both `click-dog init` and install.sh
// (via the binary) reach the on-disk YAML through this function. The
// invariant is enforced by TestRenderWizardYAML_NeverEmitsTopLevelOtel.
//
// The output layout matches examples/click-dog-<profile>.yaml so a user
// comparing renderer output to the tracked example sees the same shape; the
// parity test (TestWizard_ParityWithProfileTemplates) DeepEquals the parsed
// configs so the example stays a regression gate against renderer drift.
func renderWizardYAML(a wizardAnswers) string {
	var b strings.Builder

	b.WriteString("# Generated by `click-dog init`.\n")
	// Link the published reference, not a repo-relative path: this file is
	// written onto an installed host that has no docs/ directory.
	fmt.Fprintf(&b, "# Profile: %s. See https://click-dog.com/configuration/ for the full key reference.\n\n", a.Profile)

	b.WriteString("clickhouse:\n")
	fmt.Fprintf(&b, "  host: %s\n", yamlScalar(a.CHHost))
	fmt.Fprintf(&b, "  port: %d\n", a.CHPort)
	b.WriteString("  database: system\n")
	// `minimal` deliberately omits `username:` in its example; the ClickHouse
	// driver uses its default user when the field is empty. Mirror that here ONLY
	// when the caller didn't override --ch-user: if they explicitly asked
	// for a non-default username, emit it regardless of profile so the
	// rendered config authenticates as the user they asked for. Without
	// this guard, `click-dog init --profile minimal --ch-user monitoring`
	// silently produced a config that authenticates as `default`.
	if a.Profile != "minimal" || a.CHUser != "default" {
		fmt.Fprintf(&b, "  username: %s\n", yamlScalar(a.CHUser))
	}
	if a.CHPasswordFile != "" {
		// File-based secret: keep the password out of the process environment.
		// install.sh uses this so the systemd unit needs no EnvironmentFile.
		fmt.Fprintf(&b, "  password_file: %s\n", yamlScalar(a.CHPasswordFile))
	} else {
		b.WriteString("  password: ${CLICKHOUSE_PASSWORD}\n")
	}
	if a.CHSecure {
		b.WriteString("  secure: true\n")
	}
	// Cluster topology: name the cluster and wrap span reads in cluster(...).
	// Emitted only in cluster mode, so the sidecar default stays byte-identical
	// to examples/*.yaml (parity gate).
	if a.UseClusterQueries {
		fmt.Fprintf(&b, "  cluster: %s\n", yamlScalar(a.Cluster))
		b.WriteString("  use_cluster_queries: true\n")
	}
	// Profile-specific clickhouse: hardening. Production now carries the
	// connection-cap + query-timeout knobs install.sh used to emit
	// unconditionally; paranoid stacks tighter limits on top.
	b.WriteString(clickhouseHardeningForProfile(a.Profile))
	b.WriteByte('\n')

	// Emitted for every profile: the operator deciding where spans go is the
	// same one who must choose the query-text privacy posture below.
	b.WriteString("# filters.query_text_mode controls whether raw SQL crosses the export boundary.\n")
	b.WriteString("# Use normalized_only or none for privacy-sensitive production environments.\n")
	b.WriteString("# See https://click-dog.com/filtering/#query-text-export-modes.\n")
	b.WriteString("exporters:\n")
	b.WriteString("  otel:\n")
	fmt.Fprintf(&b, "    - collector_address: %s\n", yamlScalar(a.OTELCollector))
	fmt.Fprintf(&b, "      service_name: %s\n", yamlScalar(a.OTELService))
	if a.OTELSecure {
		b.WriteString("      secure: true\n")
	}
	if a.SplunkHECEnabled {
		b.WriteString("  splunk_hec:\n")
		fmt.Fprintf(&b, "    - endpoint: %s\n", yamlScalar(a.SplunkEndpoint))
		b.WriteString("      token: ${SPLUNK_HEC_TOKEN}\n")
	}
	b.WriteByte('\n')

	if len(a.KeeperHosts) > 0 {
		b.WriteString("ha:\n")
		b.WriteString("  keeper:\n")
		b.WriteString("    hosts:\n")
		for _, h := range a.KeeperHosts {
			fmt.Fprintf(&b, "      - %s\n", yamlScalar(h))
		}
		b.WriteString("    # secure: true\n")
		b.WriteString("    # auth_user: ${KEEPER_USER}\n")
		b.WriteString("    # auth_password: ${KEEPER_PASSWORD}\n")
		b.WriteByte('\n')
	}

	b.WriteString(monitorSectionForProfile(a.Profile))
	b.WriteByte('\n')

	if metrics := metricsSectionForProfile(a.Profile); metrics != "" {
		b.WriteString(metrics)
		b.WriteByte('\n')
	}

	if health := healthSectionForProfile(a.Profile); health != "" {
		b.WriteString(health)
		b.WriteByte('\n')
	}

	if a.Profile == "paranoid" {
		b.WriteString("log_level: warn\n")
	} else {
		b.WriteString("log_level: info\n")
	}
	return b.String()
}

// yamlScalar encodes s as a YAML scalar safe to drop into an emitted
// document. The wizard reads user input via bufio and previously
// concatenated it into the document with fmt.Fprintf, which is wrong for
// values containing `#`, leading whitespace, `:` followed by space, or YAML
// indicator characters (`&`, `*`, `!`, `|`, `>`, `%`, `@`). Delegating to
// yaml.Marshal hands off the quoting decision to a parser-aware encoder.
//
// yaml.Marshal always appends a trailing newline; trim it so the result
// composes cleanly with the renderer's own line breaks.
func yamlScalar(s string) string {
	b, err := yaml.Marshal(s)
	if err != nil {
		// yaml.Marshal of a string cannot fail in practice; if it ever did,
		// falling back to %q is the safest readable form for a YAML
		// double-quoted scalar (the escape syntaxes overlap enough that the
		// generated file still parses correctly for the vast majority of
		// inputs).
		return fmt.Sprintf("%q", s)
	}
	return strings.TrimRight(string(b), "\n")
}

// clickhouseHardeningForProfile returns the profile-specific clickhouse:
// hardening lines (each already indented two spaces). Returns empty for
// profiles that don't add hardening. Mirrors examples/click-dog-*.yaml.
//
// `production` carries the modest connection-cap + 30s query timeout that
// install.sh emitted unconditionally pre-unification; without these the
// monitor could exhaust pooled connections under load. `paranoid` doubles
// down (1 conn, 5s timeout, half-default memory) for tighter envelopes.
func clickhouseHardeningForProfile(profile string) string {
	switch profile {
	case "production":
		return `  max_open_conns: 2
  max_idle_conns: 1
  query_timeout_s: 30
`
	case "paranoid":
		return `  query_timeout_s: 5
  max_open_conns: 1
  max_idle_conns: 1
  max_memory_usage: 52428800
`
	}
	return ""
}

// metricsSectionForProfile returns the metrics: block (each line at column
// 0) for the named profile, or empty for profiles that opt out.
//
// `production` enables the Prometheus scrape on :9090 by default — install.sh
// emitted this block unconditionally pre-unification, and operators
// shipping a production install almost certainly want scrape coverage. It also
// turns on the OTLP self-metrics push (`otlp.enabled`), which reuses
// `exporters.otel[0]` — without it the shipped Datadog "Click-Dog: Health"
// dashboard reads `click_dog.*` metrics that were never exported and renders
// all-"No data". The line is written into the generated YAML (not defaulted in
// Go) so it's visible and one deletion away from off. `minimal` keeps the
// bare-essentials shape; `paranoid` likewise omits metrics so a
// load-constrained cluster doesn't get an extra listener — or a billable
// self-metrics stream — it didn't ask for.
func metricsSectionForProfile(profile string) string {
	if profile != "production" {
		return ""
	}
	return `metrics:
  enabled: true
  listen_address: ":9090"
  otlp:
    # Push click-dog's own health metrics over OTLP, reusing exporters.otel[0].
    # Feeds the Datadog "Click-Dog: Health" dashboard. Delete to disable.
    enabled: true
`
}

// healthSectionForProfile returns the dedicated health listener block for
// production installs. Keeping it out of minimal/paranoid avoids opening an
// extra listener in profiles that are intentionally sparse or constrained.
func healthSectionForProfile(profile string) string {
	if profile != "production" {
		return ""
	}
	return `health:
  enabled: true
  listen_address: ":8686"
`
}

// monitorSectionForProfile returns the monitor: block (and, for production,
// the trailing filters: block) tuned for the named profile. Values mirror
// examples/click-dog-<profile>.yaml — see renderWizardYAML's
// commentary on the duplication.
//
// Unknown profiles panic rather than fall back to production silently:
// registering a new profile in `profiles` without wiring it through here
// would otherwise hand the user a misconfigured starter under the name
// they asked for.
//
// The production filters: block now carries the `blacklist_operations`
// list install.sh emitted pre-unification. These drop high-volume
// engine-internal spans (MergeTree* / VFSWrite / ConcurrentJoin /
// QueryPipelineEx) at the SQL level, cutting ingest cost ~90-99%.
func monitorSectionForProfile(profile string) string {
	switch profile {
	case "production":
		return `monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
  max_spans_per_cycle: 1000
  dedup_cache_size: 10000
  circuit_breaker:
    enabled: true
    failure_threshold: 3
    success_threshold: 1
    reset_timeout_s: 60
  backoff:
    enabled: true
    max_interval_s: 300
    backoff_factor: 2.0

filters:
  query_text_mode: raw # raw | redacted | normalized_only | none
  blacklist_queries:
    - "^SYSTEM"
    - "^INSERT INTO.*\\.inner\\."
  blacklist_operations:
    - "MergeTreeSource"
    - "MergeTreeMarksLoader"
    - "MergeTreeIndex"
    - "MergeTreeSequentialSource"
    - "VFSWrite"
    - "WriteBufferFromS3"
    - "ConcurrentJoin"
    - "QueryPipelineEx"
`
	case "minimal":
		return `monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30

# Recommended before real use: drop high-volume engine-internal spans at the
# SQL level. They carry no clickhouse.query_id, so leaving them in drives the
# Health dashboard's "spans with query_id" tile toward zero and inflates ingest
# ~10-100x. The production profile enables this list; uncomment to use it here.
# filters:
#   query_text_mode: raw # raw | redacted | normalized_only | none
#   blacklist_operations:
#     - "MergeTreeSource"
#     - "MergeTreeMarksLoader"
#     - "MergeTreeIndex"
#     - "MergeTreeSequentialSource"
#     - "VFSWrite"
#     - "WriteBufferFromS3"
#     - "ConcurrentJoin"
#     - "QueryPipelineEx"
`
	case "paranoid":
		return `monitor:
  enabled: true
  min_trace_duration_ms: 5000
  check_interval_s: 60
  lookback_buffer_s: 30
  max_spans_per_cycle: 100
  batch_size: 50
  batch_delay_ms: 100
  dedup_cache_size: 5000
  max_query_length: 10000
  circuit_breaker:
    enabled: true
    failure_threshold: 2
    success_threshold: 2
    reset_timeout_s: 120
  backoff:
    enabled: true
    max_interval_s: 600
    backoff_factor: 3.0
  canary:
    enabled: true
    threshold_duration_ms: 60000

filters:
  # Never export raw SQL in the paranoid profile. If ClickHouse cannot provide
  # a normalized preview, query text is omitted rather than falling back.
  query_text_mode: normalized_only
  # Both lists are ACTIVE here (unlike minimal's commented block): paranoid's
  # max_spans_per_cycle budget would otherwise fill with engine-internal spans
  # instead of the slow query spans it exists to catch, and aggressive dropping
  # is the profile's whole point. Engine ops carry no clickhouse.query_id.
  blacklist_queries:
    - "^SYSTEM"
    - "^INSERT INTO.*\\.inner\\."
  blacklist_operations:
    - "MergeTreeSource"
    - "MergeTreeMarksLoader"
    - "MergeTreeIndex"
    - "MergeTreeSequentialSource"
    - "VFSWrite"
    - "WriteBufferFromS3"
    - "ConcurrentJoin"
    - "QueryPipelineEx"
`
	default:
		panic(fmt.Sprintf(
			"click-dog bug: wizard has no monitor block for profile %q; add a case here when extending the profiles registry",
			profile))
	}
}
