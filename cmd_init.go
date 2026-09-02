package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// profileEntry pairs a profile's name with the one-line description shown in
// help text. Keep `production` first so it remains the recommended default
// in help text, error messages, and interactive prompts (all of which derive
// from this slice). Adding a profile = add a case to monitorSectionForProfile
// / clickhouseHardeningForProfile / metricsSectionForProfile and append a
// row here.
type profileEntry struct {
	name        string
	description string // shown in `click-dog init --help` Profiles: block
}

var profiles = []profileEntry{
	{"production", "Recommended defaults: tuned circuit breaker, basic filters (default)"},
	{"minimal", "Quickstart: only the keys needed to run; everything else defaulted"},
	{"paranoid", "Maximum caution, minimum throughput; tight limits on every knob"},
}

// assertProfileRegistry checks the two invariants the rest of this file
// relies on: the registry is non-empty (flagDescriptionForProfile calls
// names[len(names)-1] unconditionally) and `production` is first (it is
// the recommended default and must lead help text, prompts, and error
// messages). Failing fast here turns a bad merge into a startup panic
// with a clear message instead of a confusing index-out-of-range later.
//
// Wired through Go's package-level init() so the file's literal `func
// init()` reads as the registry self-check, not as the CLI subcommand —
// the subcommand is runInit.
func assertProfileRegistry() {
	if len(profiles) == 0 {
		panic("click-dog bug: profiles registry is empty; click-dog init has nothing to offer")
	}
	if profiles[0].name != "production" {
		panic(fmt.Sprintf(
			"click-dog bug: profiles[0] = %q, must be %q so the recommended default leads help text and prompts",
			profiles[0].name, "production"))
	}
}

func init() { assertProfileRegistry() }

func profileNames() []string {
	out := make([]string, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, p.name)
	}
	return out
}

func isValidProfile(name string) bool {
	for _, p := range profiles {
		if p.name == name {
			return true
		}
	}
	return false
}

// formatProfilesHelp renders the Profiles: block of `click-dog init --help`.
// The name-column width is derived from the registry so a future profile
// name longer than the current 10-char max stays aligned automatically.
func formatProfilesHelp() string {
	maxLen := 0
	for _, p := range profiles {
		if n := len(p.name); n > maxLen {
			maxLen = n
		}
	}
	var b strings.Builder
	for _, p := range profiles {
		fmt.Fprintf(&b, "  %-*s  %s\n", maxLen, p.name, p.description)
	}
	return b.String()
}

// flagDescriptionForProfile renders the --profile flag's one-line help
// using the registry as the single source of truth, so a new profile
// shows up in `--help` without a second edit. The names[len-1] index is
// safe because assertProfileRegistry (run from init()) guarantees
// len(profiles) >= 1.
func flagDescriptionForProfile() string {
	names := profileNames()
	return fmt.Sprintf("Config profile: %s, or %s",
		strings.Join(names[:len(names)-1], ", "), names[len(names)-1])
}

// valueFlagsExplicitlySet reports whether the user passed any of the three
// "config value" flags (--ch-host, --collector, --service). When false, the
// interactive prompt should still run even though `--profile` or `--output`
// were set: those are control flags, not values. Suppressing the prompt on
// a control-flag-only invocation would silently substitute localhost
// defaults for the three values the user is most likely to want to set.
//
// The new seed flags added for install.sh's benefit (--ch-port, --ch-secure,
// --ch-user, --otel-secure, --splunk-hec-endpoint, --ha-keeper) are
// intentionally NOT in this set. They pre-fill defaults but do not suppress
// the short prompt — a TTY invocation of `click-dog init --ch-secure` still
// asks for host / collector / service / profile so the four most-edited
// values don't silently take their localhost defaults. install.sh always
// pipes a non-TTY stdin so prompt suppression is moot for the scripted path.
func valueFlagsExplicitlySet(fs *flag.FlagSet) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "ch-host", "collector", "service":
			set = true
		}
	})
	return set
}

// flagWasSet reports whether the named flag was passed on the command line
// (vs. left at its default). Used to decide whether the wizard's port
// auto-flip (9000 → 9440 when --ch-secure) should fire: skip the flip if the
// user explicitly set --ch-port, so they get exactly what they asked for.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func runInit(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(errOut)
	chHost := fs.String("ch-host", "localhost", "ClickHouse host")
	chPort := fs.Int("ch-port", 9000, "ClickHouse port (default 9000, or 9440 if --ch-secure and --ch-port is not given)")
	chSecure := fs.Bool("ch-secure", false, "Use TLS for the ClickHouse connection")
	chUser := fs.String("ch-user", "default", "ClickHouse username (install.sh quickstart overrides to its dedicated monitoring user)")
	chPasswordFile := fs.String("ch-password-file", "", "Emit clickhouse.password_file: <path> instead of password: ${CLICKHOUSE_PASSWORD} (file-based secret)")
	collector := fs.String("collector", "localhost:4317", "OTEL collector address")
	service := fs.String("service", "click-dog-monitor", "Service name for OTEL spans")
	otelSecure := fs.Bool("otel-secure", false, "Use TLS for the OTEL collector connection")
	splunkHEC := fs.String("splunk-hec-endpoint", "", "Splunk HEC endpoint (empty = Splunk exporter disabled)")
	haKeeper := fs.String("ha-keeper", "", "Comma-separated Keeper hosts (empty = HA disabled)")
	output := fs.String("output", "click-dog.yaml", "Output config file path")
	fs.StringVar(output, "o", "click-dog.yaml", "Output config file path (shorthand)")
	profile := fs.String("profile", "production", flagDescriptionForProfile())
	force := fs.Bool("force", false, "Overwrite existing config file")
	wizard := fs.Bool("wizard", false, "Run the interactive configurator (asks ~9 questions, covers TLS / Splunk HEC / HA)")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(errOut, `click-dog init — generate a starter configuration file

Usage:
  click-dog init [flags]

Profiles:
%s
If no flags are provided and stdin is a TTY, you will be prompted
for the four most common values (host / collector / service / profile).
Pass --wizard for a longer, branching configurator that also covers TLS,
Splunk HEC, and HA.

Flags:
`, formatProfilesHelp())
		printFlagDefaults(errOut, fs)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	// --wizard overrides the short prompt. It assembles its own YAML
	// (assembled in renderWizardYAML), so the flag-driven build below is
	// skipped entirely on this branch. Any seed flags the user also passed
	// pre-fill the wizard's defaults so power-users can answer-by-Enter.
	if *wizard {
		return runInitWizard(os.Stdin, out, errOut, fs, *chHost, *chPort, *chSecure, *chUser, *chPasswordFile, *collector, *service, *otelSecure, *splunkHEC, *haKeeper, *profile, *output, *force)
	}

	// Prompt interactively when the user has not explicitly set any of the
	// three "config value" flags AND stdin is a TTY. The new seed flags
	// (--ch-port / --ch-secure / --ch-user / --otel-secure /
	// --splunk-hec-endpoint / --ha-keeper) intentionally do NOT suppress
	// the short prompt — see valueFlagsExplicitlySet's comment.
	if !valueFlagsExplicitlySet(fs) && isTTY() {
		promptInteractive(os.Stdin, out, errOut, chHost, collector, service, profile)
	}

	// Validate profile after prompting (the prompt loop re-validates user
	// input, but the seeded flag value comes through here). Match the
	// formerly-loadProfileTemplate error wording for a familiar diagnostic.
	if !isValidProfile(*profile) {
		_, _ = fmt.Fprintf(errOut, "Error: unknown profile %q; valid: %s\n", *profile, strings.Join(profileNames(), ", "))
		return 1
	}

	// Check if output file exists. Track replacement separately from
	// `force` so the success message can call out a destructive overwrite.
	replacedExisting := false
	if _, err := os.Stat(*output); err == nil {
		if !*force {
			_, _ = fmt.Fprintf(errOut, "Error: %s already exists (use --force to overwrite)\n", *output)
			return 1
		}
		replacedExisting = true
	}

	a := buildAnswersFromFlags(fs, *profile, *chHost, *chPort, *chSecure, *chUser, *collector, *service, *otelSecure, *splunkHEC, *haKeeper)
	a.CHPasswordFile = *chPasswordFile
	content := renderWizardYAML(a)

	// writePrivateFile, not os.WriteFile: --force overwrites an existing config,
	// and os.WriteFile would leave a pre-existing file on its current (possibly
	// world-readable) mode. This is the file operators go on to fill with
	// credentials — install.sh chmods it 640 and the Ansible playbook writes
	// 0600 — so a rewrite must reassert the mode rather than inherit one.
	if err := writePrivateFile(*output, []byte(content), 0600); err != nil {
		_, _ = fmt.Fprintf(errOut, "Error writing config: %v\n", err)
		return 1
	}

	if replacedExisting {
		// renderWizardYAML output is intentionally verbose and
		// defaults-compatible: an operator overwriting an old config via
		// --force should be told the file changed shape (extra commentary,
		// explicit blocks for values that were defaulted) without changing
		// runtime semantics. Without the suffix the user has to diff to
		// know a destructive overwrite happened.
		_, _ = fmt.Fprintf(out, "Wrote %s (profile: %s; existing file replaced — values are defaults-compatible)\n",
			*output, *profile)
	} else {
		_, _ = fmt.Fprintf(out, "Wrote %s (profile: %s)\n", *output, *profile)
	}
	if *chPasswordFile != "" {
		_, _ = fmt.Fprintf(out, "Write the ClickHouse password to %s (mode 0600) before running click-dog.\n", *chPasswordFile)
	} else {
		_, _ = fmt.Fprintln(out, "Set CLICKHOUSE_PASSWORD in your environment before running click-dog.")
	}
	return 0
}

// buildAnswersFromFlags converts the parsed flag values into a wizardAnswers
// struct ready for renderWizardYAML. The port auto-flip (9000 → 9440 when
// --ch-secure is set and --ch-port wasn't explicit) matches the wizard's
// interactive default-flip — both surfaces present the same "TLS implies
// 9440" assumption, with --ch-port available as the override.
func buildAnswersFromFlags(fs *flag.FlagSet, profile, chHost string, chPort int, chSecure bool, chUser, collector, service string, otelSecure bool, splunkHEC, haKeeper string) wizardAnswers {
	port := chPort
	if chSecure && !flagWasSet(fs, "ch-port") {
		port = 9440
	}
	a := wizardAnswers{
		Profile:       profile,
		CHHost:        chHost,
		CHPort:        port,
		CHSecure:      chSecure,
		CHUser:        chUser,
		OTELCollector: collector,
		OTELService:   service,
		OTELSecure:    otelSecure,
	}
	if splunkHEC != "" {
		a.SplunkHECEnabled = true
		a.SplunkEndpoint = splunkHEC
	}
	if haKeeper != "" {
		a.KeeperHosts = splitAndTrim(haKeeper)
	}
	return a
}

func isTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// promptInteractive accepts io.Reader/io.Writer rather than reading os.Stdin
// directly so the profile-prompt loop can be exercised by tests without
// stubbing the global stdin.
func promptInteractive(in io.Reader, out, errOut io.Writer, chHost, collector, service, profile *string) {
	reader := bufio.NewReader(in)

	_, _ = fmt.Fprintf(out, "ClickHouse host [%s]: ", *chHost)
	if line := readLine(reader); line != "" {
		*chHost = line
	}

	_, _ = fmt.Fprintf(out, "OTEL collector address [%s]: ", *collector)
	if line := readLine(reader); line != "" {
		*collector = line
	}

	_, _ = fmt.Fprintf(out, "Service name [%s]: ", *service)
	if line := readLine(reader); line != "" {
		*service = line
	}

	// Re-prompt on a bad profile rather than os.Exit'ing (which would wipe
	// the three values the user just typed). Empty input keeps the current
	// default; EOF likewise breaks the loop.
	names := profileNames()
	for {
		_, _ = fmt.Fprintf(out, "Profile (%s) [%s]: ", strings.Join(names, "/"), *profile)
		line := readLine(reader)
		if line == "" {
			return
		}
		if isValidProfile(line) {
			*profile = line
			return
		}
		_, _ = fmt.Fprintf(errOut, "unknown profile %q; valid: %s\n", line, strings.Join(names, ", "))
	}
}

func readLine(reader *bufio.Reader) string {
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		// EOF or read error with no buffered data — surface as "no input"
		// so promptInteractive's loop can break cleanly.
		return ""
	}
	// Intentional: when err != nil but line != "" (a partial read cut by
	// EOF or stdin error), keep the partial input. Stdin is the only
	// caller; the alternatives are (a) discard what the user already
	// typed, or (b) propagate an error the prompt loop has no useful
	// recovery for. Prefer the user's last keystroke.
	return strings.TrimSpace(line)
}
