package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func publicDocFiles(t *testing.T) []string {
	t.Helper()

	files := []string{"README.md", "CONTRIBUTING.md"}
	for _, root := range []string{"docs", "overrides"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if path == filepath.Join("docs", "development") {
					return filepath.SkipDir
				}
				return nil
			}
			switch filepath.Ext(path) {
			case ".md", ".html", ".txt":
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return files
}

func deliveryContractFiles(t *testing.T) []string {
	t.Helper()

	files := append([]string{}, publicDocFiles(t)...)
	files = append(files,
		"CLAUDE.md",
		"config.yaml.example",
		"export_gate.go",
		"internal/leader/election.go",
		"main.go",
	)
	err := filepath.WalkDir("deploy/kubernetes", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		switch filepath.Ext(path) {
		case ".md", ".yaml", ".yml":
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk deploy/kubernetes: %v", err)
	}
	return files
}

func TestPublicDocs_DoNotLinkExportIgnoredDevelopmentPaths(t *testing.T) {
	for _, path := range publicDocFiles(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(data)
		for _, forbidden := range []string{
			"docs/development/",
			"../development/",
			"github.com/coltconsulting/click-dog/issues/18",
			"github.com/coltconsulting/click-dog/issues/19",
		} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains public link/reference to unavailable internal target %q", path, forbidden)
			}
		}
	}
}

func TestDeliveryContract_DoesNotPromiseDownstreamDeduplication(t *testing.T) {
	for _, path := range deliveryContractFiles(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := strings.ToLower(string(data))
		for _, forbidden := range []string{
			"effectively-once",
			"backends dedup it away",
			"collectors and backends handle duplicate",
			"collector dedups by",
			"deduplicated downstream by",
			"downstream collectors dedup",
		} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains unsupported delivery guarantee %q", path, forbidden)
			}
		}
	}
}

func TestKeeperStartupDocs_QualifyFailurePaths(t *testing.T) {
	for _, path := range deliveryContractFiles(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := strings.ToLower(string(data))
		for _, forbidden := range []string{
			"falls back to standalone mode if keeper is unreachable",
			"if keeper is unreachable at startup, the instance runs in standalone mode",
			"keeper unreachable at startup (standalone fallback)",
			"when keeper was unreachable at startup",
		} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s collapses distinct Keeper startup failures into %q", path, forbidden)
			}
		}
	}
}

func TestFailoverDocs_KeepLookbackRecoveryConditional(t *testing.T) {
	files := []string{
		"docs/configuration.md",
		"docs/operating.md",
		"docs/development/specs/deployment-topology.md",
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := strings.ToLower(string(data))
		for _, forbidden := range []string{
			"session ttl (≤10s)",
			"prevents a failover gap",
			"never a failover gap",
			"no failover gap",
		} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains unconditional failover claim %q", path, forbidden)
			}
		}
	}
}

func TestInstallDocs_DoNotPinCommandExamplesToOneRelease(t *testing.T) {
	data, err := os.ReadFile("docs/install.md")
	if err != nil {
		t.Fatal(err)
	}

	pinned := regexp.MustCompile(`(?m)(?:VERSION=|-v[ \t]+|click_dog_version=)v?[0-9]{2}\.[0-9]{2}\.[0-9]+`)
	if match := pinned.Find(data); match != nil {
		t.Fatalf("docs/install.md pins a command example to %q; resolve latest or use a VERSION variable", match)
	}
}

func TestKubernetesExample_DefinesReadonly2Profile(t *testing.T) {
	data, err := os.ReadFile("deploy/kubernetes/clickhouse-operator-spanlog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{
		"click_dog_readonly/readonly: 2",
		"click_dog_monitor/profile: click_dog_readonly",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("Kubernetes ClickHouse example is missing %q", required)
		}
	}
	if strings.Contains(text, "click_dog_monitor/profile: readonly") {
		t.Error("Kubernetes ClickHouse example uses the built-in readonly=1 profile")
	}
}

func TestAnalyzeQueriesDocs_ListEveryFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runAnalyzeQueries([]string{"-help"}, &out, &errOut); code != 0 {
		t.Fatalf("analyze queries -help exit code = %d, want 0", code)
	}

	docs, err := os.ReadFile("docs/query-analysis.md")
	if err != nil {
		t.Fatal(err)
	}
	helpFlag := regexp.MustCompile(`(?m)^  -([a-z][a-z-]*)`)
	for _, match := range helpFlag.FindAllStringSubmatch(errOut.String(), -1) {
		name := match[1]
		if !bytes.Contains(docs, []byte("-"+name+"`")) {
			t.Errorf("docs/query-analysis.md does not list analyze queries flag -%s", name)
		}
	}
}

func TestResilienceDocs_UseCurrentBackoffLogFormat(t *testing.T) {
	for _, path := range []string{"docs/resilience.md", "docs/troubleshooting.md"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(data), "Backoff increased to") {
			t.Errorf("%s documents the pre-format-change backoff log message", path)
		}
	}
}
