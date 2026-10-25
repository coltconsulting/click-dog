package main

import (
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/term"

	"github.com/coltconsulting/click-dog/internal/config"
	deploytemplate "github.com/coltconsulting/click-dog/internal/deploy/template"
	"github.com/coltconsulting/click-dog/internal/updater"
)

const (
	defaultDeployKubernetesOutDir = "./click-dog-k8s"
	defaultDeployDockerOutDir     = "./click-dog-docker"
	defaultDeployUsername         = defaultMonitorUser
	// defaultDeployClickHouseHost is the Docker default (network_mode: host, so
	// localhost reaches ClickHouse on the same host). Kubernetes rejects it —
	// a standalone click-dog pod cannot reach the ClickHouse pod via localhost,
	// so `deploy kubernetes` requires a reachable Service address (see #199).
	defaultDeployClickHouseHost = "localhost"
)

var (
	deployLatestVersion = defaultDeployLatestVersion
	clickDogImageRE     = regexp.MustCompile(`(?m)^(\s*image:\s*ghcr\.io/coltconsulting/click-dog:).*$`)
	deployUsernameRE    = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]*$`)
	deployVersionRE     = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$`)
)

type deployGenerateOptions struct {
	CollectorAddress  string
	OutDir            string
	Version           string
	Update            bool
	ClickHouseHost    string
	ClickHouseCluster string
	ClickHouseUser    string
	ClickHousePass    string
	PasswordFromFlag  bool
	// OTELSecure is --otel-secure=true|false, rendered as
	// exporters.otel[0].secure, the key `click-dog init` writes. nil (flag
	// not given) is allowed only for a loopback collector, which gets the
	// rule comment: the collector address resolves inside the cluster or
	// container, so the generator cannot probe it or guess its transport.
	OTELSecure *bool
}

type deployTemplateData struct {
	CollectorAddress   string
	ClickHouseHost     string
	ClickHouseCluster  string
	ClickHouseUsername string
	B64Username        string
	B64Password        string
	ImageTag           string
	// OTELSecureSet renders secure: OTELSecure; unset renders the rule comment.
	OTELSecureSet bool
	OTELSecure    bool
}

// otelSecureTemplateFields maps an optional --otel-secure onto the template's
// set/value pair.
func otelSecureTemplateFields(v *bool) (set, value bool) {
	if v == nil {
		return false, false
	}
	return true, *v
}

func runDeployKubernetes(args []string, out, errOut io.Writer) int {
	opts := deployGenerateOptions{OutDir: defaultDeployKubernetesOutDir}
	if err := parseDeployGenerateFlags("deploy kubernetes", args, &opts, errOut, deployKubernetesUsage); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if err := deployKubernetes(opts, out, errOut); err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: %v\n", err)
		return 1
	}
	return 0
}

func runDeployDocker(args []string, out, errOut io.Writer) int {
	opts := deployGenerateOptions{OutDir: defaultDeployDockerOutDir}
	if err := parseDeployGenerateFlags("deploy docker", args, &opts, errOut, deployDockerUsage); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if err := deployDocker(opts, out, errOut); err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: %v\n", err)
		return 1
	}
	return 0
}

func deployKubernetesUsage(errOut io.Writer, fs *flag.FlagSet) {
	_, _ = fmt.Fprintf(errOut, `click-dog deploy kubernetes — generate Kubernetes manifests

Usage:
  click-dog deploy kubernetes -c COLLECTOR --ch-host CH_SERVICE [--cluster NAME] [--otel-secure=true|false] [-o DIR] [-v VERSION]

Generates a click-dog Deployment that reaches ClickHouse over
--ch-host — the ClickHouse Service DNS (e.g. clickhouse.clickhouse.svc.cluster.local),
not localhost. Pass --cluster <name> to run it in cluster mode. cluster() reads
one replica per shard, so a replicated cluster gets partial coverage; for full
coverage run a sidecar in every ClickHouse pod instead.

--otel-secure=true|false is required unless the collector is loopback (a
collector container in the same pod): the generator cannot see the cluster's
network, and in-cluster collectors usually serve plaintext.

Flags:
`)
	printFlagDefaults(errOut, fs)
}

func deployDockerUsage(errOut io.Writer, fs *flag.FlagSet) {
	_, _ = fmt.Fprintf(errOut, `click-dog deploy docker — generate Docker Compose files

Usage:
  click-dog deploy docker -c HOST [--otel-secure=true|false] [--out-dir DIR] [--update] [--version VERSION]

--otel-secure=true|false is required unless the collector is loopback (on the
host's network): Compose collectors usually serve plaintext.

Flags:
`)
	printFlagDefaults(errOut, fs)
}

func parseDeployGenerateFlags(name string, args []string, opts *deployGenerateOptions, errOut io.Writer, usage func(io.Writer, *flag.FlagSet)) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	opts.ClickHouseUser = defaultDeployUsername
	opts.ClickHousePass = os.Getenv("CLICKHOUSE_PASSWORD")
	opts.ClickHouseHost = defaultDeployClickHouseHost

	fs.StringVar(&opts.CollectorAddress, "collector", "", "OTEL collector address")
	fs.StringVar(&opts.CollectorAddress, "c", "", "OTEL collector address (shorthand)")
	fs.StringVar(&opts.OutDir, "out-dir", opts.OutDir, "Output directory")
	fs.StringVar(&opts.OutDir, "o", opts.OutDir, "Output directory (shorthand)")
	fs.StringVar(&opts.Version, "version", "", "Version to generate")
	fs.StringVar(&opts.Version, "v", "", "Version to generate (shorthand)")
	fs.BoolVar(&opts.Update, "update", false, "Update image tag in an existing output directory")
	fs.StringVar(&opts.ClickHouseHost, "ch-host", opts.ClickHouseHost, "ClickHouse host (kubernetes: the ClickHouse Service DNS, not localhost)")
	fs.StringVar(&opts.ClickHouseCluster, "cluster", "", "ClickHouse cluster name (kubernetes only); runs the Deployment in cluster mode (cluster() reads one replica per shard)")
	fs.StringVar(&opts.ClickHouseUser, "username", defaultDeployUsername, "ClickHouse username")
	fs.StringVar(&opts.ClickHouseUser, "u", defaultDeployUsername, "ClickHouse username (shorthand)")
	fs.StringVar(&opts.ClickHousePass, "password", opts.ClickHousePass, "ClickHouse password")
	fs.StringVar(&opts.ClickHousePass, "p", opts.ClickHousePass, "ClickHouse password (shorthand)")
	otelSecure := fs.Bool("otel-secure", false, "OTEL collector transport: --otel-secure=true for TLS (system trust store), --otel-secure=false for plaintext; required unless the collector is loopback")
	fs.Usage = func() { usage(errOut, fs) }

	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := rejectLeftoverArgs("click-dog "+name, fs); err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return err
	}

	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "p", "password":
			opts.PasswordFromFlag = true
		case "otel-secure":
			opts.OTELSecure = otelSecure
		}
	})
	return nil
}

// requireDeployOTELTransport refuses a non-loopback collector without
// --otel-secure. The generator builds files for a network it cannot reach,
// so it does not guess; a loopback collector keeps the automatic choice.
func requireDeployOTELTransport(opts deployGenerateOptions) error {
	if opts.OTELSecure != nil || config.IsLoopbackOTLPTarget(opts.CollectorAddress) {
		return nil
	}
	return fmt.Errorf("collector %s is not on loopback, and the generator cannot see its network to choose a transport.\n"+
		"  Pass --otel-secure=true for a TLS receiver, or --otel-secure=false for plaintext.\n"+
		"  In-cluster and Compose collectors (an OpenTelemetry Collector or Datadog Agent on 4317) usually serve plaintext", opts.CollectorAddress)
}

func deployKubernetes(opts deployGenerateOptions, out, errOut io.Writer) error {
	if opts.Update {
		if err := rejectOTELSecureOnUpdate(opts, "configmap.yaml"); err != nil {
			return err
		}
		version, err := requireDeployUpdateVersion(opts.Version, "kubernetes")
		if err != nil {
			return err
		}
		// Pre-#199 output dirs hold daemonset.yaml, not deployment.yaml. The
		// topology changed (DaemonSet -> Deployment), so an in-place
		// image bump is wrong — regenerate, and delete the old workload (which
		// `kubectl apply -k` does NOT prune, so it would keep running and
		// double-export alongside the new Deployment).
		if isLegacyDaemonSetOutDir(opts.OutDir) {
			return fmt.Errorf("%s has daemonset.yaml but no deployment.yaml — it predates the DaemonSet->Deployment migration (#199); an image-tag bump won't migrate it.\n"+
				"Regenerate:\n"+
				"  click-dog deploy kubernetes -c <collector> --ch-host <ch-service> [--cluster <name>]\n"+
				"then delete the old workload (apply -k won't remove it):\n"+
				"  kubectl delete daemonset click-dog -n click-dog", opts.OutDir)
		}
		return updateDeploymentImage(opts.OutDir, "deployment.yaml", version, out, "Kubernetes")
	}

	// A standalone click-dog pod has its own network namespace, so it can only
	// reach ClickHouse through a routable address — never localhost (that would
	// be the click-dog container itself). Require a real host so the generated
	// Deployment can actually connect (#199).
	if err := validateClickHouseHostForK8s(opts.ClickHouseHost); err != nil {
		return err
	}

	// Without a cluster name, use_cluster_queries stays off and the single
	// Deployment reads only whichever pod the Service routes to — correct for a
	// single-node ClickHouse, wrong (silent partial data) for a multi-shard
	// cluster. Make that implication explicit rather than silently shipping it.
	if strings.TrimSpace(opts.ClickHouseCluster) == "" {
		_, _ = fmt.Fprintln(errOut, "WARNING: no --cluster given — the Deployment will read only the single ClickHouse")
		_, _ = fmt.Fprintln(errOut, "         pod the Service routes to. Correct for single-node ClickHouse; for a")
		_, _ = fmt.Fprintln(errOut, "         multi-node cluster run a sidecar in every ClickHouse pod, or pass")
		_, _ = fmt.Fprintln(errOut, "         --cluster <name> for cluster mode (one replica per shard).")
	}

	if err := prepareInitialDeployOptions(&opts, out, errOut); err != nil {
		return err
	}
	if err := os.MkdirAll(opts.OutDir, 0755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	data := deployTemplateData{
		CollectorAddress:   opts.CollectorAddress,
		ClickHouseHost:     strings.TrimSpace(opts.ClickHouseHost),
		ClickHouseCluster:  strings.TrimSpace(opts.ClickHouseCluster),
		ClickHouseUsername: opts.ClickHouseUser,
		B64Username:        base64.StdEncoding.EncodeToString([]byte(opts.ClickHouseUser)),
		B64Password:        base64.StdEncoding.EncodeToString([]byte(opts.ClickHousePass)),
		ImageTag:           imageTag(opts.Version),
	}
	data.OTELSecureSet, data.OTELSecure = otelSecureTemplateFields(opts.OTELSecure)
	files := []struct {
		template string
		output   string
		mode     os.FileMode
		private  bool
	}{
		{"k8s/namespace.yaml.tmpl", "namespace.yaml", 0644, false},
		{"k8s/serviceaccount.yaml.tmpl", "serviceaccount.yaml", 0644, false},
		{"k8s/configmap.yaml.tmpl", "configmap.yaml", 0644, false},
		{"k8s/secret.yaml.tmpl", "secret.yaml", 0600, true},
		{"k8s/deployment.yaml.tmpl", "deployment.yaml", 0644, false},
		{"k8s/kustomization.yaml.tmpl", "kustomization.yaml", 0644, false},
		{"k8s/gitignore.tmpl", ".gitignore", 0644, false},
	}
	for _, f := range files {
		if err := writeDeployTemplate(opts.OutDir, f.output, f.template, data, f.mode, f.private); err != nil {
			return err
		}
	}

	_, _ = fmt.Fprintf(out, "Kubernetes manifests generated in %s\n", displayDir(opts.OutDir))
	_, _ = fmt.Fprintln(out, "  WARNING: secret.yaml contains credentials - do not commit to version control")
	_, _ = fmt.Fprintf(out, "  Apply with: kubectl apply -k %s\n", displayDir(opts.OutDir))
	return nil
}

func deployDocker(opts deployGenerateOptions, out, errOut io.Writer) error {
	if opts.Update {
		if err := rejectOTELSecureOnUpdate(opts, "click-dog.yaml"); err != nil {
			return err
		}
		version, err := requireDeployUpdateVersion(opts.Version, "docker")
		if err != nil {
			return err
		}
		return updateDeploymentImage(opts.OutDir, "docker-compose.yml", version, out, "Docker")
	}

	if err := prepareInitialDeployOptions(&opts, out, errOut); err != nil {
		return err
	}
	if err := os.MkdirAll(opts.OutDir, 0755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	data := deployTemplateData{
		CollectorAddress:   opts.CollectorAddress,
		ClickHouseUsername: opts.ClickHouseUser,
		ImageTag:           imageTag(opts.Version),
	}
	data.OTELSecureSet, data.OTELSecure = otelSecureTemplateFields(opts.OTELSecure)
	files := []struct {
		template string
		output   string
		mode     os.FileMode
		private  bool
	}{
		{"docker/docker-compose.yml.tmpl", "docker-compose.yml", 0644, false},
		{"docker/click-dog.yaml.tmpl", "click-dog.yaml", 0644, false},
		{"docker/gitignore.tmpl", ".gitignore", 0644, false},
	}
	for _, f := range files {
		if err := writeDeployTemplate(opts.OutDir, f.output, f.template, data, f.mode, f.private); err != nil {
			return err
		}
	}
	// The container reads click-dog.yaml as the image's own user, not the
	// owner, so it must be 0644 whatever the umask or a previous file left.
	// A symlink is the operator's own arrangement: its target is left alone.
	cfgPath := filepath.Join(opts.OutDir, "click-dog.yaml")
	if info, err := os.Lstat(cfgPath); err == nil && info.Mode().IsRegular() {
		if err := os.Chmod(cfgPath, 0644); err != nil {
			return fmt.Errorf("setting click-dog.yaml mode: %w", err)
		}
	}

	envPath := filepath.Join(opts.OutDir, ".env")
	if err := writePrivateFile(envPath, []byte("CLICKHOUSE_PASSWORD="+escapeComposeEnv(opts.ClickHousePass)+"\n"), 0600); err != nil {
		return fmt.Errorf("writing .env: %w", err)
	}

	_, _ = fmt.Fprintf(out, "Docker files generated in %s\n", displayDir(opts.OutDir))
	_, _ = fmt.Fprintln(out, "  WARNING: .env contains credentials - do not commit to version control")
	_, _ = fmt.Fprintf(out, "  Start with: cd %s && docker compose up -d\n", displayDir(opts.OutDir))
	return nil
}

func prepareInitialDeployOptions(opts *deployGenerateOptions, out, errOut io.Writer) error {
	if strings.TrimSpace(opts.CollectorAddress) == "" {
		return errors.New("-c collector address is required")
	}
	if err := requireDeployOTELTransport(*opts); err != nil {
		return err
	}
	if strings.TrimSpace(opts.OutDir) == "" {
		return errors.New("--out-dir cannot be empty")
	}
	if !deployUsernameRE.MatchString(opts.ClickHouseUser) {
		return fmt.Errorf("username must start with a letter or underscore and contain only letters, digits, underscore, dot, or hyphen (got %q)", opts.ClickHouseUser)
	}
	if opts.PasswordFromFlag {
		_, _ = fmt.Fprintln(errOut, "WARNING: -p/--password exposes the password in process listings and shell history.")
		_, _ = fmt.Fprintln(errOut, "         Prefer: export CLICKHOUSE_PASSWORD=...")
	}
	if opts.ClickHousePass == "" {
		if isTTY() {
			_, _ = fmt.Fprint(errOut, "ClickHouse password: ")
			pw, err := term.ReadPassword(int(os.Stdin.Fd()))
			_, _ = fmt.Fprintln(errOut)
			if err != nil && !errors.Is(err, io.EOF) {
				return fmt.Errorf("reading password: %w", err)
			}
			opts.ClickHousePass = string(pw)
		}
		if opts.ClickHousePass == "" {
			return errors.New("ClickHouse password required. Set CLICKHOUSE_PASSWORD env var or use -p")
		}
	}
	resolvedLatest := false
	if opts.Version == "" {
		resolved, err := deployLatestVersion()
		if err != nil {
			return fmt.Errorf("could not resolve latest release version from GitHub API: %w", err)
		}
		opts.Version = resolved
		resolvedLatest = true
	}
	version, err := validateDeployVersion(opts.Version)
	if err != nil {
		return err
	}
	opts.Version = version
	if resolvedLatest {
		_, _ = fmt.Fprintf(out, "Latest version: %s\n", opts.Version)
	}
	return nil
}

func requireDeployUpdateVersion(version, mode string) (string, error) {
	if strings.TrimSpace(version) == "" {
		return "", fmt.Errorf("-v VERSION is required for %s update", mode)
	}
	cleaned, err := validateDeployVersion(version)
	if err != nil {
		return "", err
	}
	return cleaned, nil
}

// rejectOTELSecureOnUpdate refuses --otel-secure with --update. The update
// path only bumps the image tag and never rewrites the config, so accepting
// the flag would leave the old transport while looking like the asked-for
// one was applied.
func rejectOTELSecureOnUpdate(opts deployGenerateOptions, configFile string) error {
	if opts.OTELSecure == nil {
		return nil
	}
	return fmt.Errorf("--otel-secure applies when generating, not with --update (which only bumps the image tag); set secure under exporters.otel in %s", configFile)
}

// isLegacyDaemonSetOutDir reports whether an output dir was generated before
// the DaemonSet->Deployment migration (#199): it has daemonset.yaml and no
// deployment.yaml. Such dirs can't be image-bumped in place — the topology
// changed — so the update path refuses with migration guidance.
func isLegacyDaemonSetOutDir(outDir string) bool {
	if _, err := os.Stat(filepath.Join(outDir, "deployment.yaml")); err == nil {
		return false
	}
	_, err := os.Stat(filepath.Join(outDir, "daemonset.yaml"))
	return err == nil
}

// validateClickHouseHostForK8s rejects an empty or loopback host. A standalone
// click-dog Deployment runs in its own pod network namespace, so localhost
// resolves to the click-dog container itself, not ClickHouse — it must point at
// a routable address, typically the ClickHouse Service DNS
// (e.g. clickhouse.clickhouse.svc.cluster.local). See #199.
func validateClickHouseHostForK8s(host string) error {
	h := strings.TrimSpace(host)
	if h == "" {
		return errors.New("--ch-host is required for kubernetes: set it to the ClickHouse Service DNS (e.g. clickhouse.clickhouse.svc.cluster.local)")
	}
	// Reject host:port — the port is a separate config field (fixed at 9000). A
	// bare host:port here renders `host: localhost:9000` alongside `port: 9000`,
	// which the reader concatenates into localhost:9000:9000 at dial time.
	// SplitHostPort errors when there's no port (including a bracketless IPv6
	// literal), which is exactly the input we want to allow through.
	if _, _, err := net.SplitHostPort(h); err == nil {
		return fmt.Errorf("--ch-host %q must not include a port — pass just the host (the ClickHouse port is fixed at 9000)", host)
	}
	// Reject loopback. "localhost" is a name (ParseIP won't catch it); the rest
	// is any IP in 127.0.0.0/8 or ::1 — exact 127.0.0.1 isn't special, the whole
	// /8 is loopback. A standalone pod reaches none of these.
	ip := net.ParseIP(strings.Trim(h, "[]")) // unwrap a bracketed IPv6 literal
	if strings.EqualFold(h, "localhost") || (ip != nil && ip.IsLoopback()) {
		return fmt.Errorf("--ch-host %q is unreachable from a standalone click-dog Deployment (separate pod network namespace); use the ClickHouse Service DNS, e.g. clickhouse.clickhouse.svc.cluster.local", host)
	}
	return nil
}

func writeDeployTemplate(outDir, output, templatePath string, data deployTemplateData, mode os.FileMode, private bool) error {
	rendered, err := deploytemplate.Render(templatePath, data)
	if err != nil {
		return fmt.Errorf("rendering %s: %w", templatePath, err)
	}
	path := filepath.Join(outDir, output)
	if private {
		if err := writePrivateFile(path, rendered, mode); err != nil {
			return fmt.Errorf("writing %s: %w", output, err)
		}
		return nil
	}
	if err := os.WriteFile(path, rendered, mode); err != nil {
		return fmt.Errorf("writing %s: %w", output, err)
	}
	return nil
}

func updateDeploymentImage(outDir, filename, version string, out io.Writer, label string) error {
	if strings.TrimSpace(outDir) == "" {
		return errors.New("--out-dir cannot be empty")
	}
	if info, err := os.Stat(outDir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("output directory %q not found. Run %q first", outDir, strings.ToLower(label))
		}
		return fmt.Errorf("checking output directory: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("output path %q is not a directory", outDir)
	}

	path := filepath.Join(outDir, filename)
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s not found in %s. Run %q without --update first", filename, outDir, strings.ToLower(label))
		}
		return fmt.Errorf("reading %s: %w", filename, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("checking %s: %w", filename, err)
	}

	matches := clickDogImageRE.FindAllSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return fmt.Errorf("could not find click-dog image tag in %s", filename)
	}
	if len(matches) > 1 {
		return fmt.Errorf("found %d click-dog image tags in %s; refusing to update ambiguously - bump them manually", len(matches), filename)
	}

	updated := clickDogImageRE.ReplaceAllFunc(content, func(line []byte) []byte {
		sub := clickDogImageRE.FindSubmatch(line)
		if len(sub) != 2 {
			return line
		}
		replacement := append([]byte{}, sub[1]...)
		replacement = append(replacement, imageTag(version)...)
		return replacement
	})

	if err := os.WriteFile(path+".bak", content, info.Mode().Perm()); err != nil {
		return fmt.Errorf("writing %s.bak: %w", filename, err)
	}
	if err := os.WriteFile(path, updated, info.Mode().Perm()); err != nil {
		return fmt.Errorf("writing %s: %w", filename, err)
	}

	_, _ = fmt.Fprintf(out, "Updated image tag to %s in %s\n", imageTag(version), filename)
	if label == "Kubernetes" {
		_, _ = fmt.Fprintf(out, "Kubernetes update complete. Review changes and apply with: kubectl apply -k %s\n", displayDir(outDir))
		_, _ = fmt.Fprintln(out, "  Note: configmap.yaml merge is left to the later deploy update phase.")
	} else {
		_, _ = fmt.Fprintf(out, "Docker update complete. Apply with: cd %s && docker compose up -d\n", displayDir(outDir))
		_, _ = fmt.Fprintln(out, "  Note: click-dog.yaml merge is left to the later deploy update phase.")
	}
	return nil
}

func defaultDeployLatestVersion() (string, error) {
	release, err := updater.NewGitHubClient().LatestRelease()
	if err != nil {
		return "", err
	}
	return cleanVersion(release.TagName), nil
}

func cleanVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

func validateDeployVersion(v string) (string, error) {
	cleaned := cleanVersion(v)
	if !deployVersionRE.MatchString(cleaned) {
		return "", fmt.Errorf("--version %q must be X.Y.Z with an optional alphanumeric prerelease suffix", v)
	}
	return cleaned, nil
}

// imageTag is the container image tag pinned for a version. GoReleaser
// publishes the image under the BARE {{ .Version }} (the git tag with its
// leading "v" stripped, e.g. 26.05.1) — a "v"-prefixed tag is not published
// and would fail with ErrImagePull (#232). cleanVersion already strips any
// leading "v" from operator input, so the pin is bare regardless of how the
// version was supplied.
func imageTag(v string) string {
	return cleanVersion(v)
}

// escapeComposeEnv wraps a value for safe inclusion in a Docker Compose .env
// file. compose-spec's dotenv parser treats single-quoted values as fully
// literal (no $-interpolation, no #-comments, no backslash escape
// processing), so single-quoting is the simplest robust form. Embedded
// single quotes are escaped with the shell-standard '\” sequence.
func escapeComposeEnv(value string) string {
	escaped := strings.ReplaceAll(value, `'`, `'\''`)
	return "'" + escaped + "'"
}

func displayDir(dir string) string {
	trimmed := strings.TrimRight(dir, string(os.PathSeparator))
	return trimmed + string(os.PathSeparator)
}
