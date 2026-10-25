package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/coltconsulting/click-dog/internal/config"
)

const ansibleConfigTemplate = "deploy/ansible/templates/click-dog.yaml.j2"

var (
	jinjaIfRe    = regexp.MustCompile(`^\{% if (\w+) \| bool %\}$`)
	jinjaVarRe   = regexp.MustCompile(`\{\{ (\w+) \}\}`)
	jinjaElseRe  = regexp.MustCompile(`^\{% else %\}$`)
	jinjaEndifRe = regexp.MustCompile(`^\{% endif %\}$`)
)

// renderAnsibleTemplate is just enough Jinja for click-dog.yaml.j2: `{{ var }}`
// substitution and whole-line `{% if var | bool %}` / `{% else %}` /
// `{% endif %}` blocks,
// whose tag lines vanish as they do under Ansible's default trim_blocks.
// Anything else fails the test, so a template that outgrows it is noticed
// rather than half-rendered.
func renderAnsibleTemplate(t *testing.T, vars map[string]string, flags map[string]bool) string {
	t.Helper()
	src, err := os.ReadFile(ansibleConfigTemplate)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	var include []bool
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if m := jinjaIfRe.FindStringSubmatch(trimmed); m != nil {
			on, ok := flags[m[1]]
			if !ok {
				t.Fatalf("template condition %q has no test value", m[1])
			}
			include = append(include, on)
			continue
		}
		if jinjaElseRe.MatchString(trimmed) {
			if len(include) == 0 {
				t.Fatal("template has an unmatched {% else %}")
			}
			include[len(include)-1] = !include[len(include)-1]
			continue
		}
		if jinjaEndifRe.MatchString(trimmed) {
			if len(include) == 0 {
				t.Fatal("template has an unmatched {% endif %}")
			}
			include = include[:len(include)-1]
			continue
		}
		skip := false
		for _, on := range include {
			skip = skip || !on
		}
		if skip {
			continue
		}
		line = jinjaVarRe.ReplaceAllStringFunc(line, func(tag string) string {
			name := jinjaVarRe.FindStringSubmatch(tag)[1]
			v, ok := vars[name]
			if !ok {
				t.Fatalf("template variable %q has no test value", name)
			}
			return v
		})
		if strings.Contains(line, "{{") || strings.Contains(line, "{%") {
			t.Fatalf("template line uses Jinja this test cannot render: %q", line)
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if len(include) != 0 {
		t.Fatal("template has an unclosed {% if %}")
	}
	return out.String()
}

// ansibleTemplateVars returns the playbook's defaults with the config and TLS
// directories pointed at dir, which holds the secret and certificate files the
// config loader reads.
func ansibleTemplateVars(t *testing.T, dir string) map[string]string {
	t.Helper()
	tlsDir := filepath.Join(dir, "tls")
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(dir, ".secret"):       "monitor-password",
		filepath.Join(tlsDir, "ca.pem"):     "test TLS material\n",
		filepath.Join(tlsDir, "client.crt"): "test TLS material\n",
		filepath.Join(tlsDir, "client.key"): "test TLS material\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]string{
		"clickhouse_host":        "localhost",
		"clickhouse_port":        "9000",
		"clickhouse_username":    "click_dog_monitor",
		"click_dog_config_dir":   dir,
		"otel_collector_address": "otel.example.com:4317",
		"tls_cert_dir":           tlsDir,
		"keeper_hosts":           "[]",
	}
}

func loadRenderedAnsibleConfig(t *testing.T, dir, rendered string) *config.Config {
	t.Helper()
	path := filepath.Join(dir, "click-dog.yaml")
	if err := os.WriteFile(path, []byte(rendered), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("rendered Ansible config failed to load: %v\n%s", err, rendered)
	}
	return cfg
}

// TestAnsibleTemplate_MatchesProductionProfile keeps the Ansible fleet on the
// same settings as `click-dog init`'s production profile. Only the values the
// playbook owns per host may differ: the password comes from the 0600 secret
// file, and TLS and Keeper are opt-in blocks checked separately below.
func TestAnsibleTemplate_MatchesProductionProfile(t *testing.T) {
	dir := t.TempDir()
	// The profile's remote collector renders secure: true, as the playbook
	// does once the transport is settled.
	rendered := renderAnsibleTemplate(t, ansibleTemplateVars(t, dir), ansibleTemplateFlags(true, false, false, false))
	got := loadRenderedAnsibleConfig(t, dir, rendered)

	want, err := config.LoadConfig(filepath.Join("examples", "click-dog-production.yaml"))
	if err != nil {
		t.Fatalf("production profile failed to load: %v", err)
	}

	// The profile's ${CLICKHOUSE_PASSWORD} also yields an unset-variable
	// EnvWarning; the secret file replaces it, so the warning goes too.
	got.ClickHouse.Password = want.ClickHouse.Password
	got.ClickHouse.PasswordFile = want.ClickHouse.PasswordFile
	got.EnvWarnings, want.EnvWarnings = nil, nil
	got.DeprecationWarnings, want.DeprecationWarnings = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Ansible template and production profile diverge:\nansible:    %+v\nproduction: %+v", got, want)
	}
}

func TestAnsibleTemplate_OptionalBlocksLoad(t *testing.T) {
	for _, keeperHosts := range []string{
		`["keeper-01:9181", "keeper-02:9181"]`, // INI [group:vars] string
		`['keeper-01:9181', 'keeper-02:9181']`, // Jinja rendering of a YAML list var
	} {
		t.Run(keeperHosts, func(t *testing.T) {
			dir := t.TempDir()
			vars := ansibleTemplateVars(t, dir)
			vars["keeper_hosts"] = keeperHosts
			rendered := renderAnsibleTemplate(t, vars, ansibleTemplateFlags(true, true, true, true))
			cfg := loadRenderedAnsibleConfig(t, dir, rendered)

			if want := []string{"keeper-01:9181", "keeper-02:9181"}; !reflect.DeepEqual(cfg.HA.Keeper.Hosts, want) {
				t.Errorf("ha.keeper.hosts = %v, want %v", cfg.HA.Keeper.Hosts, want)
			}
		})
	}

	// The playbook derives otel_ca_enabled and otel_mtls_enabled from whether
	// otel_ca_cert and otel_client_cert are set, and copies those files to the
	// fixed names the template renders. otel_secure_rendered is false only for
	// a loopback collector left to the automatic choice; any material makes
	// the transport TLS, so it is always rendered beside secure: true.
	for _, tc := range []struct {
		name                 string
		collector            string
		rendered             bool
		value                string
		ca, mtls             bool
		wantSecure, wantCA   bool
		wantClientCertAndKey bool
		wantSource           config.TransportSource
	}{
		{name: "system roots", rendered: true, value: "true", wantSecure: true, wantSource: config.TransportExplicit},
		{name: "private CA", rendered: true, value: "true", ca: true, wantSecure: true, wantCA: true, wantSource: config.TransportExplicit},
		{name: "mTLS with system roots", rendered: true, value: "true", mtls: true, wantSecure: true, wantClientCertAndKey: true, wantSource: config.TransportExplicit},
		{name: "mTLS with private CA", rendered: true, value: "true", ca: true, mtls: true, wantSecure: true, wantCA: true, wantClientCertAndKey: true, wantSource: config.TransportExplicit},
		{name: "explicit plaintext", collector: "otel-collector.monitoring:4317", rendered: true, value: "false", wantSource: config.TransportExplicit},
		{name: "loopback left automatic", collector: "localhost:4317", wantSource: config.TransportAutomatic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			vars := ansibleTemplateVars(t, dir)
			if tc.collector != "" {
				vars["otel_collector_address"] = tc.collector
			}
			tlsDir := vars["tls_cert_dir"]
			flags := ansibleTemplateFlags(tc.rendered, tc.ca, tc.mtls, false)
			flags["otel_transport_tls"] = tc.value == "true"
			rendered := renderAnsibleTemplate(t, vars, flags)
			if !tc.rendered && (strings.Contains(rendered, "      secure:") || !strings.Contains(rendered, otelSecureRuleComment("      "))) {
				t.Fatalf("want the rule comment and no secure key:\n%s", rendered)
			}
			otel := loadRenderedAnsibleConfig(t, dir, rendered).Exporters.OTEL[0]
			if otel.SecureSource != tc.wantSource {
				t.Errorf("transport source = %s, want %s", otel.SecureSource, tc.wantSource)
			}

			want := config.OTELConfig{Secure: tc.wantSecure}
			if tc.wantCA {
				want.CACert = filepath.Join(tlsDir, "ca.pem")
			}
			if tc.wantClientCertAndKey {
				want.ClientCert = filepath.Join(tlsDir, "client.crt")
				want.ClientKey = filepath.Join(tlsDir, "client.key")
			}
			got := config.OTELConfig{Secure: otel.Secure, CACert: otel.CACert, ClientCert: otel.ClientCert, ClientKey: otel.ClientKey}
			if got != want {
				t.Errorf("OTLP TLS = %+v, want %+v", got, want)
			}
		})
	}
}

func ansibleTemplateFlags(otelSecureRendered, otelCA, otelMTLS, ha bool) map[string]bool {
	return map[string]bool{
		"otel_secure_rendered": otelSecureRendered,
		"otel_transport_tls":   otelSecureRendered,
		"otel_ca_enabled":      otelCA,
		"otel_mtls_enabled":    otelMTLS,
		"ha_enabled":           ha,
	}
}

// TestAnsiblePlaybook_WritesRenderedTLSFiles requires every TLS file the
// template points click-dog at to be a file the playbook installs. Config load
// rejects unreadable TLS files, so a path nothing writes stops the service.
func TestAnsiblePlaybook_WritesRenderedTLSFiles(t *testing.T) {
	tmpl, err := os.ReadFile(ansibleConfigTemplate)
	if err != nil {
		t.Fatal(err)
	}
	playbook, err := os.ReadFile("deploy/ansible/playbook.yaml")
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`\{\{ tls_cert_dir \}\}/[\w.-]+`).FindAllString(string(tmpl), -1)
	if len(refs) == 0 {
		t.Fatal("template renders no TLS files; update this test if that is intended")
	}
	for _, ref := range refs {
		if !strings.Contains(string(playbook), `dest: "`+ref+`"`) {
			t.Errorf("template renders %s but deploy/ansible/playbook.yaml never writes it", ref)
		}
	}
}

// TestAnsiblePlaybook_KeeperOffByDefault pins the sidecar default: a playbook
// run without keeper_hosts must not render an ha: block (ha_enabled is
// `keeper_hosts | string | length > 2`, so the default has to be "[]").
func TestAnsiblePlaybook_KeeperOffByDefault(t *testing.T) {
	playbook, err := os.ReadFile("deploy/ansible/playbook.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`keeper_hosts: "{{ keeper_hosts | default('[]') }}"`,
		`ha_enabled: "{{ keeper_hosts | string | length > 2 }}"`,
	} {
		if !strings.Contains(string(playbook), required) {
			t.Errorf("deploy/ansible/playbook.yaml must keep Keeper off by default: missing %q", required)
		}
	}
}

// TestAnsiblePlaybook_OTLPTransportDecidedOnHost pins how the playbook settles
// an unset otel_secure: the installed binary answers (no copy of the rule in
// the playbook), a TLS choice is checked by handshake, and both probes run in
// --check mode without reporting a change.
func TestAnsiblePlaybook_OTLPTransportDecidedOnHost(t *testing.T) {
	raw, err := os.ReadFile("deploy/ansible/playbook.yaml")
	if err != nil {
		t.Fatal(err)
	}
	playbook := string(raw)
	for _, required := range []string{
		`otel_secure: "{{ otel_secure | default('') }}"`,
		`'init', '--print-otel-secure'`,
		`'test', 'tls'`,
		`set otel_secure=false to export in plaintext`,
	} {
		if !strings.Contains(playbook, required) {
			t.Errorf("deploy/ansible/playbook.yaml lacks %q", required)
		}
	}
	if strings.Contains(playbook, "otel_secure | default(false)") {
		t.Error("otel_secure must be optional, not default(false)")
	}
	for _, task := range []string{"Ask click-dog which OTLP transport the collector gets", "Check the OTLP receiver's TLS handshake"} {
		i := strings.Index(playbook, "- name: "+task)
		if i < 0 {
			t.Fatalf("task %q not found", task)
		}
		body := playbook[i:]
		if j := strings.Index(body[1:], "\n    - name: "); j > 0 {
			body = body[:j+1]
		}
		for _, want := range []string{"check_mode: false", "changed_when: false"} {
			if !strings.Contains(body, want) {
				t.Errorf("task %q lacks %s", task, want)
			}
		}
	}
	// The transport is settled before the configuration is written.
	if strings.Index(playbook, "Settle the OTLP transport") > strings.Index(playbook, "Deploy click-dog configuration") {
		t.Error("the OTLP transport must be settled before the configuration task")
	}
}
