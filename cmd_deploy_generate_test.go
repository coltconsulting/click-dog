package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/coltconsulting/click-dog/internal/config"
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
		OTELSecure:        boolPtr(false),
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
		"runAsGroup: 65532",
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

func TestDeployDocker_ConfigReadableByImageUser(t *testing.T) {
	// os.WriteFile keeps an existing file's mode, as a umask of 027 would
	// lower a new one's; either leaves the container's user unable to read it.
	outDir := t.TempDir()
	cfgPath := filepath.Join(outDir, "click-dog.yaml")
	if err := os.WriteFile(cfgPath, []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}

	err := deployDocker(deployGenerateOptions{
		CollectorAddress: "localhost:4317",
		OutDir:           outDir,
		Version:          "v26.05.1",
		ClickHouseUser:   "click_dog_monitor",
		ClickHousePass:   "secret",
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("deployDocker returned error: %v", err)
	}

	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0644 {
		t.Fatalf("click-dog.yaml mode = %v, want 0644", got)
	}
}

func TestDeployDocker_SymlinkedConfigKeepsTargetMode(t *testing.T) {
	// Widening the mode would follow the link to a file outside the output
	// directory, which the operator may keep private on purpose.
	outDir := t.TempDir()
	target := filepath.Join(t.TempDir(), "shared.yaml")
	if err := os.WriteFile(target, []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(outDir, "click-dog.yaml")); err != nil {
		t.Fatal(err)
	}

	err := deployDocker(deployGenerateOptions{
		CollectorAddress: "localhost:4317",
		OutDir:           outDir,
		Version:          "v26.05.1",
		ClickHouseUser:   "click_dog_monitor",
		ClickHousePass:   "secret",
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("deployDocker returned error: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("symlink target mode = %v, want 0600", got)
	}
}

func TestDeployKubernetesQuotesTemplateScalars(t *testing.T) {
	outDir := t.TempDir()
	payload := "otel:4317\n        - name: injected\n          image: attacker.invalid/poc:1"
	err := deployKubernetes(deployGenerateOptions{
		CollectorAddress:  payload,
		OTELSecure:        boolPtr(false),
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
				OTELSecure:       boolPtr(false),
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
		OTELSecure:       boolPtr(false),
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
			OTELSecure:       boolPtr(false),
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
		[]string{"-c", "otel:4317", "-o", "/tmp/out", "-v", "v26.05.1", "-u", "reader", "-p", "secret", "--update", "--otel-secure"},
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
	if opts.OTELSecure == nil || !*opts.OTELSecure {
		t.Fatalf("OTELSecure = %v, want true", opts.OTELSecure)
	}
}

// TestDeployGenerate_OTELSecure pins --otel-secure for both generators: either
// value renders exporters.otel[0].secure (the key `click-dog init` writes) and
// nothing else, and without it a loopback collector gets the rule comment
// while any other collector is refused. The rendered config is loaded through config.LoadConfig so the
// key is checked where it lands, not just that the text appears somewhere —
// clickhouse.secure must stay off because both generators target port 9000.
func TestDeployGenerate_OTELSecure(t *testing.T) {
	t.Setenv("CLICKHOUSE_USERNAME", "click_dog_monitor")
	t.Setenv("CLICKHOUSE_PASSWORD", "secret")

	generators := []struct {
		name   string
		run    func(deployGenerateOptions) error
		config func(t *testing.T, outDir string) string
	}{
		{
			name: "kubernetes",
			run: func(o deployGenerateOptions) error {
				o.ClickHouseHost = "clickhouse.clickhouse.svc.cluster.local"
				return deployKubernetes(o, io.Discard, io.Discard)
			},
			config: configMapClickDogYAML,
		},
		{
			name: "docker",
			run: func(o deployGenerateOptions) error {
				return deployDocker(o, io.Discard, io.Discard)
			},
			config: func(t *testing.T, outDir string) string {
				return readFile(t, filepath.Join(outDir, "click-dog.yaml"))
			},
		},
	}
	for _, g := range generators {
		for _, tc := range []struct {
			name      string
			collector string
			flag      *bool
			want      bool
			wantLine  string // "" means the rule comment
		}{
			{"otel-secure=true", "otel.example.com:4317", boolPtr(true), true, "secure: true"},
			{"otel-secure=false", "otel-collector.monitoring:4317", boolPtr(false), false, "secure: false"},
			{"loopback automatic", "localhost:4317", nil, false, ""},
		} {
			secure := tc.want
			t.Run(g.name+"/"+tc.name, func(t *testing.T) {
				outDir := t.TempDir()
				if err := g.run(deployGenerateOptions{
					CollectorAddress: tc.collector,
					OutDir:           outDir,
					Version:          "26.05.1",
					ClickHouseUser:   "click_dog_monitor",
					ClickHousePass:   "secret",
					OTELSecure:       tc.flag,
				}); err != nil {
					t.Fatalf("generate: %v", err)
				}

				rendered := g.config(t, outDir)
				if tc.wantLine != "" {
					if got := strings.Count(rendered, tc.wantLine); got != 1 {
						t.Fatalf("%q appears %d times, want 1:\n%s", tc.wantLine, got, rendered)
					}
				} else if strings.Contains(rendered, "secure: ") || !strings.Contains(rendered, "# secure omitted: plaintext while collector_address is loopback") {
					t.Fatalf("want the rule comment and no secure key:\n%s", rendered)
				}
				if g.name == "kubernetes" && strings.Contains(rendered, "# ca_cert:") != (tc.flag != nil && *tc.flag) {
					t.Errorf("commented TLS file lines belong with secure: true only:\n%s", rendered)
				}
				cfgPath := filepath.Join(t.TempDir(), "click-dog.yaml")
				if err := os.WriteFile(cfgPath, []byte(rendered), 0600); err != nil {
					t.Fatal(err)
				}
				cfg, err := config.LoadConfig(cfgPath)
				if err != nil {
					t.Fatalf("LoadConfig: %v\n--- rendered ---\n%s", err, rendered)
				}
				if len(cfg.Exporters.OTEL) != 1 {
					t.Fatalf("want one OTEL exporter, got %d", len(cfg.Exporters.OTEL))
				}
				if got := cfg.Exporters.OTEL[0].Secure; got != secure {
					t.Errorf("exporters.otel[0].secure = %v, want %v", got, secure)
				}
				if cfg.Exporters.OTEL[0].CACert != "" {
					t.Errorf("exporters.otel[0].ca_cert = %q, want empty (system trust store)", cfg.Exporters.OTEL[0].CACert)
				}
				if cfg.ClickHouse.Secure {
					t.Error("clickhouse.secure = true; --otel-secure must not touch the ClickHouse connection")
				}
			})
		}
	}
}

// TestDeployKubernetes_OTELTLSVolume pins the optional OTLP TLS mount every
// generated Deployment carries, matching deploy/kubernetes/deployment.yaml:
// ConfigMap click-dog-otel-ca and Secret click-dog-otel-client projected
// read-only at /etc/click-dog/tls, both optional so the pod starts without
// them, and the client key 0440 under fsGroup 65532 so only click-dog reads it.
// The pod also runs in primary group 65532, so the key is not readable through
// group 0 when fsGroup is missing.
func TestDeployKubernetes_OTELTLSVolume(t *testing.T) {
	outDir := t.TempDir()
	if err := deployKubernetes(deployGenerateOptions{
		CollectorAddress: "otel.example.com:4317",
		OTELSecure:       boolPtr(false),
		OutDir:           outDir,
		Version:          "26.05.1",
		ClickHouseHost:   "clickhouse.clickhouse.svc.cluster.local",
		ClickHouseUser:   "click_dog_monitor",
		ClickHousePass:   "secret",
	}, io.Discard, io.Discard); err != nil {
		t.Fatalf("deployKubernetes: %v", err)
	}

	type keyPath struct {
		Key  string `yaml:"key"`
		Path string `yaml:"path"`
		Mode *int   `yaml:"mode"`
	}
	type source struct {
		Name     string    `yaml:"name"`
		Optional bool      `yaml:"optional"`
		Items    []keyPath `yaml:"items"`
	}
	var d struct {
		Spec struct {
			Template struct {
				Spec struct {
					SecurityContext struct {
						RunAsUser  int `yaml:"runAsUser"`
						RunAsGroup int `yaml:"runAsGroup"`
						FSGroup    int `yaml:"fsGroup"`
					} `yaml:"securityContext"`
					Containers []struct {
						VolumeMounts []struct {
							Name      string `yaml:"name"`
							MountPath string `yaml:"mountPath"`
							ReadOnly  bool   `yaml:"readOnly"`
						} `yaml:"volumeMounts"`
					} `yaml:"containers"`
					Volumes []struct {
						Name      string `yaml:"name"`
						Projected *struct {
							Sources []struct {
								ConfigMap *source `yaml:"configMap"`
								Secret    *source `yaml:"secret"`
							} `yaml:"sources"`
						} `yaml:"projected"`
					} `yaml:"volumes"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(outDir, "deployment.yaml"))), &d); err != nil {
		t.Fatalf("parsing deployment.yaml: %v", err)
	}
	pod := d.Spec.Template.Spec
	if pod.SecurityContext.FSGroup != pod.SecurityContext.RunAsUser || pod.SecurityContext.FSGroup != 65532 {
		t.Errorf("fsGroup = %d, want 65532 (the runAsUser)", pod.SecurityContext.FSGroup)
	}
	if pod.SecurityContext.RunAsGroup != 65532 {
		t.Errorf("runAsGroup = %d, want 65532 (unset runs click-dog in group 0)", pod.SecurityContext.RunAsGroup)
	}

	mounted := false
	for _, m := range pod.Containers[0].VolumeMounts {
		if m.Name == "otel-tls" {
			mounted = m.MountPath == "/etc/click-dog/tls" && m.ReadOnly
		}
	}
	if !mounted {
		t.Errorf("otel-tls not mounted read-only at /etc/click-dog/tls: %+v", pod.Containers[0].VolumeMounts)
	}

	var ca, client *source
	for _, v := range pod.Volumes {
		if v.Name != "otel-tls" || v.Projected == nil {
			continue
		}
		for _, s := range v.Projected.Sources {
			if s.ConfigMap != nil {
				ca = s.ConfigMap
			}
			if s.Secret != nil {
				client = s.Secret
			}
		}
	}
	if ca == nil || ca.Name != "click-dog-otel-ca" || !ca.Optional ||
		len(ca.Items) != 1 || ca.Items[0] != (keyPath{Key: "ca.pem", Path: "ca.pem"}) {
		t.Errorf("CA source = %+v, want optional ConfigMap click-dog-otel-ca, ca.pem -> ca.pem", ca)
	}
	if client == nil || client.Name != "click-dog-otel-client" || !client.Optional || len(client.Items) != 2 {
		t.Fatalf("client source = %+v, want optional Secret click-dog-otel-client with two items", client)
	}
	if got := client.Items[0]; got.Key != "tls.crt" || got.Path != "client.crt" || got.Mode != nil {
		t.Errorf("client cert item = %+v, want tls.crt -> client.crt at the default mode", got)
	}
	if got := client.Items[1]; got.Key != "tls.key" || got.Path != "client.key" || got.Mode == nil || *got.Mode != 0o440 {
		t.Errorf("client key item = %+v, want tls.key -> client.key with mode 0440", got)
	}

	// Without --otel-secure the ConfigMap names none of the mounted files.
	if strings.Contains(readFile(t, filepath.Join(outDir, "configmap.yaml")), "/etc/click-dog/tls/") {
		t.Error("plaintext configmap.yaml should not mention the TLS mount")
	}
}

// TestDeployKubernetes_SecurityContextMatchesShippedManifest keeps the
// generated Deployment and deploy/kubernetes/deployment.yaml on one pod
// identity: the same pod and container securityContext, running click-dog as
// user and group 65532. make check-k8s only validates the shipped file's
// schema, so without this a drift in either copy goes unnoticed.
func TestDeployKubernetes_SecurityContextMatchesShippedManifest(t *testing.T) {
	outDir := t.TempDir()
	if err := deployKubernetes(deployGenerateOptions{
		CollectorAddress: "otel.example.com:4317",
		OTELSecure:       boolPtr(false),
		OutDir:           outDir,
		Version:          "26.05.1",
		ClickHouseHost:   "clickhouse.clickhouse.svc.cluster.local",
		ClickHouseUser:   "click_dog_monitor",
		ClickHousePass:   "secret",
	}, io.Discard, io.Discard); err != nil {
		t.Fatalf("deployKubernetes: %v", err)
	}

	shippedPod, shippedContainer := podSecurityContexts(t, shippedDeployment)
	genPod, genContainer := podSecurityContexts(t, filepath.Join(outDir, "deployment.yaml"))
	if !reflect.DeepEqual(shippedPod, genPod) {
		t.Errorf("pod securityContext differs:\n  shipped:   %+v\n  generated: %+v", shippedPod, genPod)
	}
	if !reflect.DeepEqual(shippedContainer, genContainer) {
		t.Errorf("container securityContext differs:\n  shipped:   %+v\n  generated: %+v", shippedContainer, genContainer)
	}
	if got := shippedPod["runAsGroup"]; got != 65532 {
		t.Errorf("shipped runAsGroup = %v, want 65532 (unset runs click-dog in group 0)", got)
	}
}

// shippedDeployment is the checked-in sample manifest the generator mirrors.
var shippedDeployment = filepath.Join("deploy", "kubernetes", "deployment.yaml")

// podSecurityContexts returns a Deployment manifest's pod securityContext and
// its click-dog container's securityContext.
func podSecurityContexts(t *testing.T, path string) (pod, container map[string]any) {
	t.Helper()
	var d struct {
		Spec struct {
			Template struct {
				Spec struct {
					SecurityContext map[string]any `yaml:"securityContext"`
					Containers      []struct {
						Name            string         `yaml:"name"`
						SecurityContext map[string]any `yaml:"securityContext"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, path)), &d); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	spec := d.Spec.Template.Spec
	for _, c := range spec.Containers {
		if c.Name == "click-dog" {
			return spec.SecurityContext, c.SecurityContext
		}
	}
	t.Fatalf("%s has no click-dog container", path)
	return nil, nil
}

// TestDeployUpdate_RejectsOTELSecure: --update only bumps the image tag, so
// --otel-secure there would be silently dropped, leaving a plaintext exporter
// behind a command line that asked for TLS. Both generators refuse it before
// touching the output directory.
func TestDeployUpdate_RejectsOTELSecure(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		run        func(deployGenerateOptions) error
	}{
		{"kubernetes", "configmap.yaml", func(o deployGenerateOptions) error { return deployKubernetes(o, io.Discard, io.Discard) }},
		{"docker", "click-dog.yaml", func(o deployGenerateOptions) error { return deployDocker(o, io.Discard, io.Discard) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outDir := t.TempDir()
			err := tc.run(deployGenerateOptions{OutDir: outDir, Version: "26.05.2", Update: true, OTELSecure: boolPtr(true)})
			if err == nil {
				t.Fatal("expected --update --otel-secure to be refused")
			}
			if !strings.Contains(err.Error(), "set secure under exporters.otel in "+tc.file) {
				t.Errorf("error should say where to set it by hand, got: %v", err)
			}
			entries, readErr := os.ReadDir(outDir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Errorf("refusal wrote into the output directory: %v", entries)
			}
		})
	}
}

// configMapClickDogYAML returns the click-dog.yaml document embedded in a
// generated configmap.yaml.
func configMapClickDogYAML(t *testing.T, outDir string) string {
	t.Helper()
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(outDir, "configmap.yaml"))), &cm); err != nil {
		t.Fatalf("parsing configmap.yaml: %v", err)
	}
	doc, ok := cm.Data["click-dog.yaml"]
	if !ok {
		t.Fatal("configmap.yaml has no data.click-dog.yaml")
	}
	return doc
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
		OTELSecure:       boolPtr(false),
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
		OTELSecure:       boolPtr(false),
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
		OTELSecure:       boolPtr(false),
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
		OTELSecure:       boolPtr(false),
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

// TestDeployGenerate_RemoteCollectorNeedsTransport pins the refusal: the
// generators cannot see the collector's network, so a non-loopback collector
// without --otel-secure fails before anything is written.
func TestDeployGenerate_RemoteCollectorNeedsTransport(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(deployGenerateOptions) error
	}{
		{"kubernetes", func(o deployGenerateOptions) error {
			o.ClickHouseHost = "clickhouse.clickhouse.svc.cluster.local"
			return deployKubernetes(o, io.Discard, io.Discard)
		}},
		{"docker", func(o deployGenerateOptions) error { return deployDocker(o, io.Discard, io.Discard) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outDir := filepath.Join(t.TempDir(), "out")
			err := tc.run(deployGenerateOptions{
				CollectorAddress: "otel-collector.monitoring:4317",
				OutDir:           outDir,
				Version:          "26.05.1",
				ClickHouseUser:   "click_dog_monitor",
				ClickHousePass:   "secret",
			})
			if err == nil {
				t.Fatal("want refusal")
			}
			for _, want := range []string{"--otel-secure=true", "--otel-secure=false", "usually serve plaintext"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error lacks %q: %v", want, err)
				}
			}
			if _, statErr := os.Stat(outDir); statErr == nil {
				t.Error("refusal created the output directory")
			}
		})
	}
}

func TestParseDeployGenerateFlags_StrictOTELSecure(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		want    *bool
		wantErr string
	}{
		{[]string{"-c", "x:1"}, nil, ""},
		{[]string{"--otel-secure", "-c", "x:1"}, boolPtr(true), ""},
		{[]string{"--otel-secure=false", "-c", "x:1"}, boolPtr(false), ""},
		{[]string{"--otel-secure", "false", "-c", "x:1"}, nil, "--otel-secure=false"},
		{[]string{"-c", "x:1", "stray"}, nil, `unexpected argument "stray"`},
	} {
		var opts deployGenerateOptions
		var errOut bytes.Buffer
		err := parseDeployGenerateFlags("deploy docker", tc.args, &opts, &errOut, deployDockerUsage)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(errOut.String(), tc.wantErr) {
				t.Errorf("%v: err=%v stderr=%q, want %q", tc.args, err, errOut.String(), tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: %v", tc.args, err)
			continue
		}
		if (opts.OTELSecure == nil) != (tc.want == nil) || (tc.want != nil && *opts.OTELSecure != *tc.want) {
			t.Errorf("%v: OTELSecure = %v, want %v", tc.args, opts.OTELSecure, tc.want)
		}
	}
}
