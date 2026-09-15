package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeployKubernetesInitialGeneration(t *testing.T) {
	outDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	secretPath := filepath.Join(outDir, "secret.yaml")
	if err := os.WriteFile(secretPath, []byte("stale credentials\n"), 0600); err != nil {
		t.Fatalf("pre-create secret.yaml: %v", err)
	}
	if err := os.Chmod(secretPath, 0644); err != nil {
		t.Fatalf("make pre-existing secret.yaml world-readable: %v", err)
	}

	err := deployKubernetes(deployGenerateOptions{
		CollectorAddress:  "otel-collector.monitoring:4317",
		OutDir:            outDir,
		Version:           "26.05.1",
		ClickHouseHost:    "clickhouse.clickhouse.svc.cluster.local",
		ClickHouseCluster: "main",
		ClickHouseUser:    "click_dog_monitor",
		ClickHousePass:    "s3cr3t",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("deployKubernetes returned error: %v\nstderr:\n%s", err, stderr.String())
	}

	for _, name := range []string{
		"namespace.yaml",
		"serviceaccount.yaml",
		"configmap.yaml",
		"secret.yaml",
		"deployment.yaml",
		"kustomization.yaml",
		".gitignore",
	} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
			t.Fatalf("expected %s to be written: %v", name, err)
		}
	}

	assertFileContains(t, filepath.Join(outDir, "configmap.yaml"),
		`collector_address: "otel-collector.monitoring:4317"`,
		`host: "clickhouse.clickhouse.svc.cluster.local"`,
		"use_cluster_queries: true",
		`cluster: "main"`,
		"username: ${CLICKHOUSE_USERNAME}",
		"password: ${CLICKHOUSE_PASSWORD}",
		"listen_address: \":8686\"",
	)
	assertFileContains(t, secretPath,
		"CLICKHOUSE_USERNAME: Y2xpY2tfZG9nX21vbml0b3I=",
		"CLICKHOUSE_PASSWORD: czNjcjN0",
	)
	assertFileContains(t, filepath.Join(outDir, "deployment.yaml"),
		"kind: Deployment",
		"image: ghcr.io/coltconsulting/click-dog:26.05.1",
		// Health probes hit the dedicated health port, not the metrics port.
		"- name: health",
		"containerPort: 8686",
		"path: /healthz",
		"path: /readyz",
		"runAsNonRoot: true",
		"runAsUser: 65532",
		"allowPrivilegeEscalation: false",
		"readOnlyRootFilesystem: true",
		"- ALL",
		"secretKeyRef:",
		"key: CLICKHOUSE_PASSWORD",
	)
	assertFileContains(t, filepath.Join(outDir, ".gitignore"), "secret.yaml")
	secretInfo, err := os.Stat(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := secretInfo.Mode().Perm(); got != 0600 {
		t.Fatalf("secret.yaml mode = %v, want 0600", got)
	}

	if !strings.Contains(stdout.String(), "WARNING: secret.yaml contains credentials") {
		t.Fatalf("stdout missing secret warning:\n%s", stdout.String())
	}
}

func TestDeployDockerInitialGeneration(t *testing.T) {
	outDir := t.TempDir()
	var stdout, stderr bytes.Buffer

	err := deployDocker(deployGenerateOptions{
		CollectorAddress: "localhost:4317",
		OutDir:           outDir,
		Version:          "v26.05.1",
		ClickHouseUser:   "click_dog_monitor",
		ClickHousePass:   `pa\ss$word#1`,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("deployDocker returned error: %v\nstderr:\n%s", err, stderr.String())
	}

	for _, name := range []string{"docker-compose.yml", "click-dog.yaml", ".env", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
			t.Fatalf("expected %s to be written: %v", name, err)
		}
	}

	assertFileContains(t, filepath.Join(outDir, "docker-compose.yml"),
		"image: ghcr.io/coltconsulting/click-dog:26.05.1",
		"network_mode: host",
	)
	assertFileContains(t, filepath.Join(outDir, "click-dog.yaml"),
		`username: "click_dog_monitor"`,
		"password: ${CLICKHOUSE_PASSWORD}",
		`collector_address: "localhost:4317"`,
		"port: 9000",
	)
	dockerCfg := readFile(t, filepath.Join(outDir, "click-dog.yaml"))
	if strings.Contains(dockerCfg, "secure: true") {
		t.Fatalf("docker click-dog.yaml should not enable TLS against plaintext port 9000:\n%s", dockerCfg)
	}
	assertFileContains(t, filepath.Join(outDir, ".gitignore"), ".env")

	envPath := filepath.Join(outDir, ".env")
	gotEnv := readFile(t, envPath)
	wantEnv := `CLICKHOUSE_PASSWORD='pa\ss$word#1'` + "\n"
	if gotEnv != wantEnv {
		t.Fatalf(".env content = %q, want %q", gotEnv, wantEnv)
	}
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf(".env mode = %v, want 0600", got)
	}
	if !strings.Contains(stdout.String(), "WARNING: .env contains credentials") {
		t.Fatalf("stdout missing .env warning:\n%s", stdout.String())
	}
}

func TestDeployKubernetesQuotesTemplateScalars(t *testing.T) {
	outDir := t.TempDir()
	payload := "otel:4317\n        - name: injected\n          image: attacker.invalid/poc:1"
	err := deployKubernetes(deployGenerateOptions{
		CollectorAddress:  payload,
		OutDir:            outDir,
		Version:           "26.05.1",
		ClickHouseHost:    "clickhouse.example\n---\ninjected",
		ClickHouseCluster: "main: prod",
		ClickHouseUser:    "click_dog_monitor",
		ClickHousePass:    "secret",
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("deployKubernetes returned error: %v", err)
	}
	config := readFile(t, filepath.Join(outDir, "configmap.yaml"))
	for _, want := range []string{
		`collector_address: "otel:4317\n        - name: injected\n          image: attacker.invalid/poc:1"`,
		`host: "clickhouse.example\n---\ninjected"`,
		`cluster: "main: prod"`,
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("configmap missing safely quoted scalar %q:\n%s", want, config)
		}
	}
	if strings.Count(config, "apiVersion:") != 1 {
		t.Fatalf("template input created another YAML document:\n%s", config)
	}
}

func TestDeployRejectsUnsafeVersions(t *testing.T) {
	for _, version := range []string{
		"26.05",
		"latest",
		"26.05.1+meta",
		"26.05.1\n        - name: injected\n          image: attacker.invalid/poc:1",
	} {
		t.Run(strings.ReplaceAll(version, "\n", "_newline_"), func(t *testing.T) {
			if _, err := validateDeployVersion(version); err == nil {
				t.Fatalf("validateDeployVersion(%q) unexpectedly succeeded", version)
			}
			outDir := t.TempDir()
			err := deployKubernetes(deployGenerateOptions{
				CollectorAddress: "otel:4317",
				OutDir:           outDir,
				Version:          version,
				ClickHouseHost:   "clickhouse.clickhouse.svc.cluster.local",
				ClickHouseUser:   "click_dog_monitor",
				ClickHousePass:   "secret",
			}, io.Discard, io.Discard)
			if err == nil {
				t.Fatalf("deployKubernetes accepted unsafe version %q", version)
			}
			if _, statErr := os.Stat(filepath.Join(outDir, "deployment.yaml")); !os.IsNotExist(statErr) {
				t.Fatalf("deployment.yaml written for unsafe version %q: %v", version, statErr)
			}
		})
	}
}

func TestDeployKubernetesUpdateBumpsImageAndBacksUp(t *testing.T) {
	outDir := t.TempDir()
	var stdout, stderr bytes.Buffer

	err := deployKubernetes(deployGenerateOptions{
		CollectorAddress: "otel:4317",
		OutDir:           outDir,
		Version:          "26.05.1",
		ClickHouseHost:   "clickhouse.clickhouse.svc.cluster.local",
		ClickHouseUser:   "click_dog_monitor",
		ClickHousePass:   "secret",
	}, io.Discard, &stderr)
	if err != nil {
		t.Fatalf("initial deployKubernetes returned error: %v", err)
	}

	err = deployKubernetes(deployGenerateOptions{
		OutDir:  outDir,
		Version: "26.05.2",
		Update:  true,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("update deployKubernetes returned error: %v\nstderr:\n%s", err, stderr.String())
	}

	assertFileContains(t, filepath.Join(outDir, "deployment.yaml"),
		"image: ghcr.io/coltconsulting/click-dog:26.05.2",
	)
	assertFileContains(t, filepath.Join(outDir, "deployment.yaml.bak"),
		"image: ghcr.io/coltconsulting/click-dog:26.05.1",
	)
	if !strings.Contains(stdout.String(), "Updated image tag to 26.05.2 in deployment.yaml") {
		t.Fatalf("stdout missing update line:\n%s", stdout.String())
	}
}

// deploy kubernetes must reject a missing or loopback ClickHouse host: a
// standalone click-dog pod can't reach ClickHouse via localhost (#199).
func TestDeployKubernetesRejectsLocalhostHost(t *testing.T) {
	// Empty + loopback (the host can't reach ClickHouse from a separate pod),
	// plus host:port forms (the port is a separate field; host:port would render
	// localhost:9000 + port:9000 -> localhost:9000:9000 at dial time).
	for _, host := range []string{
		"", "localhost", "127.0.0.1", "::1", "[::1]",
		// All of 127.0.0.0/8 is loopback, not just 127.0.0.1.
		"127.0.0.2", "127.1.2.3",
		"localhost:9000", "127.0.0.1:9000", "[::1]:9000",
		"clickhouse.clickhouse.svc.cluster.local:9000",
	} {
		var stderr bytes.Buffer
		err := deployKubernetes(deployGenerateOptions{
			CollectorAddress: "otel:4317",
			OutDir:           t.TempDir(),
			Version:          "26.05.1",
			ClickHouseHost:   host,
			ClickHouseUser:   "click_dog_monitor",
			ClickHousePass:   "secret",
		}, io.Discard, &stderr)
		if err == nil {
			t.Errorf("expected error for ch-host %q, got nil", host)
		}
	}
}

// A pre-#199 output dir (daemonset.yaml, no deployment.yaml) can't be
// image-bumped in place — the topology changed — so --update must refuse with
// migration guidance rather than a generic not-found.
func TestDeployKubernetesUpdateRejectsLegacyDaemonSetDir(t *testing.T) {
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "daemonset.yaml"),
		[]byte("kind: DaemonSet\nspec: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	err := deployKubernetes(deployGenerateOptions{
		OutDir:  outDir,
		Version: "26.05.2",
		Update:  true,
	}, io.Discard, &stderr)
	if err == nil {
		t.Fatal("expected a migration error for a legacy daemonset.yaml dir, got nil")
	}
	if !strings.Contains(err.Error(), "kubectl delete daemonset") {
		t.Errorf("error should give migration guidance (delete the old DaemonSet), got: %v", err)
	}
}

func TestParseDeployGenerateFlagsAliases(t *testing.T) {
	opts := deployGenerateOptions{OutDir: defaultDeployDockerOutDir}
	err := parseDeployGenerateFlags(
		"deploy docker",
		[]string{"-c", "otel:4317", "-o", "/tmp/out", "-v", "v26.05.1", "-u", "reader", "-p", "secret", "--update"},
		&opts,
		io.Discard,
		func(io.Writer, *flag.FlagSet) {},
	)
	if err != nil {
		t.Fatal(err)
	}

	if opts.CollectorAddress != "otel:4317" {
		t.Fatalf("CollectorAddress = %q", opts.CollectorAddress)
	}
	if opts.OutDir != "/tmp/out" {
		t.Fatalf("OutDir = %q", opts.OutDir)
	}
	if opts.Version != "v26.05.1" {
		t.Fatalf("Version = %q", opts.Version)
	}
	if opts.ClickHouseUser != "reader" {
		t.Fatalf("ClickHouseUser = %q", opts.ClickHouseUser)
	}
	if opts.ClickHousePass != "secret" {
		t.Fatalf("ClickHousePass = %q", opts.ClickHousePass)
	}
	if !opts.PasswordFromFlag {
		t.Fatal("PasswordFromFlag = false, want true")
	}
	if !opts.Update {
		t.Fatal("Update = false, want true")
	}
}

func TestEscapeComposeEnv(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain", "secret", `'secret'`},
		{"dollar", "pa$word", `'pa$word'`},
		{"hash", "p#w", `'p#w'`},
		{"backslash", `pa\ss`, `'pa\ss'`},
		{"mixed", `pa\ss$word#1`, `'pa\ss$word#1'`},
		{"single_quote", `o'brien`, `'o'\''brien'`},
		{"empty", "", `''`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := escapeComposeEnv(c.in); got != c.want {
				t.Fatalf("escapeComposeEnv(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestDeployDockerUpdateBumpsImageAndBacksUp(t *testing.T) {
	outDir := t.TempDir()
	var stdout, stderr bytes.Buffer

	if err := deployDocker(deployGenerateOptions{
		CollectorAddress: "otel:4317",
		OutDir:           outDir,
		Version:          "26.05.1",
		ClickHouseUser:   "click_dog_monitor",
		ClickHousePass:   "secret",
	}, io.Discard, &stderr); err != nil {
		t.Fatalf("initial deployDocker returned error: %v", err)
	}

	if err := deployDocker(deployGenerateOptions{
		OutDir:  outDir,
		Version: " v26.05.2 ",
		Update:  true,
	}, &stdout, &stderr); err != nil {
		t.Fatalf("update deployDocker returned error: %v\nstderr:\n%s", err, stderr.String())
	}

	assertFileContains(t, filepath.Join(outDir, "docker-compose.yml"),
		"image: ghcr.io/coltconsulting/click-dog:26.05.2",
	)
	assertFileContains(t, filepath.Join(outDir, "docker-compose.yml.bak"),
		"image: ghcr.io/coltconsulting/click-dog:26.05.1",
	)
	if !strings.Contains(stdout.String(), "Updated image tag to 26.05.2 in docker-compose.yml") {
		t.Fatalf("stdout missing update line:\n%s", stdout.String())
	}
}

func TestDeployUpdateMissingFileErrors(t *testing.T) {
	outDir := t.TempDir()
	var stdout bytes.Buffer

	err := updateDeploymentImage(outDir, "daemonset.yaml", "26.05.2", &stdout, "Kubernetes")
	if err == nil {
		t.Fatal("expected error when target file is missing, got nil")
	}
	if !strings.Contains(err.Error(), "daemonset.yaml not found") {
		t.Fatalf("error = %v, want message about missing file", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout should be empty on error, got: %s", stdout.String())
	}
}

func TestDeployUpdateNoMatchLeavesNoBackup(t *testing.T) {
	outDir := t.TempDir()
	target := filepath.Join(outDir, "daemonset.yaml")
	if err := os.WriteFile(target, []byte("kind: DaemonSet\nspec: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	err := updateDeploymentImage(outDir, "daemonset.yaml", "26.05.2", io.Discard, "Kubernetes")
	if err == nil {
		t.Fatal("expected error when image line is absent, got nil")
	}
	if !strings.Contains(err.Error(), "could not find click-dog image tag") {
		t.Fatalf("error = %v, want message about missing image tag", err)
	}
	if _, statErr := os.Stat(target + ".bak"); !os.IsNotExist(statErr) {
		t.Fatalf("expected no .bak on no-match path, stat err = %v", statErr)
	}
}

func TestDeployUpdateMultipleMatchesErrors(t *testing.T) {
	outDir := t.TempDir()
	target := filepath.Join(outDir, "daemonset.yaml")
	body := "      image: ghcr.io/coltconsulting/click-dog:26.05.1\n" +
		"      image: ghcr.io/coltconsulting/click-dog:26.05.1\n"
	if err := os.WriteFile(target, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	err := updateDeploymentImage(outDir, "daemonset.yaml", "26.05.2", io.Discard, "Kubernetes")
	if err == nil {
		t.Fatal("expected ambiguous-match error, got nil")
	}
	if !strings.Contains(err.Error(), "found 2 click-dog image tags") {
		t.Fatalf("error = %v, want multi-match message", err)
	}
	if _, statErr := os.Stat(target + ".bak"); !os.IsNotExist(statErr) {
		t.Fatalf("expected no .bak on multi-match path, stat err = %v", statErr)
	}
}

func TestPrepareInitialDeployOptionsResolvesVersion(t *testing.T) {
	t.Setenv("CLICKHOUSE_PASSWORD", "")
	prev := deployLatestVersion
	t.Cleanup(func() { deployLatestVersion = prev })

	called := 0
	deployLatestVersion = func() (string, error) {
		called++
		return "26.05.9", nil
	}

	opts := deployGenerateOptions{
		CollectorAddress: "otel:4317",
		OutDir:           "/tmp/ignored",
		ClickHouseUser:   "click_dog_monitor",
		ClickHousePass:   "secret",
	}
	var stdout, stderr bytes.Buffer
	if err := prepareInitialDeployOptions(&opts, &stdout, &stderr); err != nil {
		t.Fatalf("prepareInitialDeployOptions returned error: %v", err)
	}
	if called != 1 {
		t.Fatalf("deployLatestVersion calls = %d, want 1", called)
	}
	if opts.Version != "26.05.9" {
		t.Fatalf("Version = %q, want %q", opts.Version, "26.05.9")
	}
	if !strings.Contains(stdout.String(), "Latest version: 26.05.9") {
		t.Fatalf("stdout missing latest-version line:\n%s", stdout.String())
	}
}

// TestPrepareInitialDeployOptions_PasswordFlagWarning pins the password-on-argv
// warning to the standardized spelling: -p stays a single-char short, but the
// long form must read --password.
func TestPrepareInitialDeployOptions_PasswordFlagWarning(t *testing.T) {
	prev := deployLatestVersion
	t.Cleanup(func() { deployLatestVersion = prev })
	deployLatestVersion = func() (string, error) { return "26.05.9", nil }

	opts := deployGenerateOptions{
		CollectorAddress: "otel:4317",
		OutDir:           "/tmp/ignored",
		ClickHouseUser:   "click_dog_monitor",
		ClickHousePass:   "secret",
		PasswordFromFlag: true,
	}
	var stdout, stderr bytes.Buffer
	if err := prepareInitialDeployOptions(&opts, &stdout, &stderr); err != nil {
		t.Fatalf("prepareInitialDeployOptions returned error: %v", err)
	}
	if !strings.Contains(stderr.String(), "-p/--password exposes the password") {
		t.Fatalf("stderr missing --password warning:\n%s", stderr.String())
	}
}

func TestPrepareInitialDeployOptionsWrapsResolverError(t *testing.T) {
	prev := deployLatestVersion
	t.Cleanup(func() { deployLatestVersion = prev })

	deployLatestVersion = func() (string, error) {
		return "", errors.New("github: 503 service unavailable")
	}

	opts := deployGenerateOptions{
		CollectorAddress: "otel:4317",
		OutDir:           "/tmp/ignored",
		ClickHouseUser:   "click_dog_monitor",
		ClickHousePass:   "secret",
	}
	err := prepareInitialDeployOptions(&opts, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected error from prepareInitialDeployOptions, got nil")
	}
	if !strings.Contains(err.Error(), "could not resolve latest release version") {
		t.Fatalf("error = %v, want wrap with 'could not resolve latest release version'", err)
	}
	if !strings.Contains(err.Error(), "github: 503 service unavailable") {
		t.Fatalf("error = %v, want underlying error to be wrapped", err)
	}
}

func TestDeployUsernameAcceptsDotAndHyphen(t *testing.T) {
	for _, name := range []string{"reader", "ch.user", "ro-user", "user_1", "_internal"} {
		if !deployUsernameRE.MatchString(name) {
			t.Errorf("expected %q to match deployUsernameRE", name)
		}
	}
	for _, name := range []string{"", "1user", "us er", "user;DROP"} {
		if deployUsernameRE.MatchString(name) {
			t.Errorf("expected %q to NOT match deployUsernameRE", name)
		}
	}
}

func assertFileContains(t *testing.T, path string, needles ...string) {
	t.Helper()
	content := readFile(t, path)
	for _, needle := range needles {
		if !strings.Contains(content, needle) {
			t.Fatalf("%s missing %q\nfull content:\n%s", path, needle, content)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}
