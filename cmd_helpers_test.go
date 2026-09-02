package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

// TestWritePrivateFile_ReassertsModeOnExistingFile is the reason this helper
// exists instead of os.WriteFile: os.WriteFile's mode argument applies only
// when it creates the file, so writing over a target that already exists
// leaves it on its old, possibly world-readable, mode.
func TestWritePrivateFile_ReassertsModeOnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact.json")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	// os.WriteFile creates honoring umask; force the permissive mode so the
	// test asserts against 0644 regardless of the umask the suite runs under.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writePrivateFile(path, []byte("fresh"), 0o600); err != nil {
		t.Fatalf("writePrivateFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode after rewrite = %o, want 600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "fresh" {
		t.Errorf("content = %q, want %q", data, "fresh")
	}
}

// TestWritePrivateFile_DoesNotFollowSymlink covers the other half of the
// os.WriteFile hazard: a symlink sitting at the destination is followed, so the
// artifact lands in whatever file the link names. Removing the link first means
// the content goes to the intended path and the link's target is untouched.
func TestWritePrivateFile_DoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "artifact.json")
	if err := os.Symlink(victim, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := writePrivateFile(path, []byte("secret report"), 0o600); err != nil {
		t.Fatalf("writePrivateFile: %v", err)
	}

	if data, err := os.ReadFile(victim); err != nil {
		t.Fatal(err)
	} else if string(data) != "original" {
		t.Errorf("symlink target was overwritten: %q", data)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("destination is still a symlink; the link was written through")
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

// TestWritePrivateFile_RefusesDirectory pins the destructive case this helper
// must not inherit from os.Remove: an EMPTY directory is removable, so without
// an explicit guard an --output pointed at one would be deleted and replaced
// by a file. os.WriteFile returned EISDIR and left the directory alone; so
// must this.
func TestWritePrivateFile_RefusesDirectory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		populate bool
	}{
		{"empty directory", false},
		{"non-empty directory", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dest")
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.populate {
				if err := os.WriteFile(filepath.Join(path, "child"), []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			if err := writePrivateFile(path, []byte("report"), 0o600); err == nil {
				t.Fatal("writePrivateFile overwrote a directory; want an error")
			}

			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("directory was removed: %v", err)
			}
			if !info.IsDir() {
				t.Fatal("directory was replaced by a file")
			}
			if tc.populate {
				if _, err := os.Stat(filepath.Join(path, "child")); err != nil {
					t.Errorf("directory contents lost: %v", err)
				}
			}
		})
	}
}

// TestWritePrivateFile_RefusesNonRegularDestinations covers the rest of what
// os.Remove will happily unlink. A live Unix socket is the sharp case: os.Remove
// returns nil and takes the socket path with it, which can make a running
// service unreachable, where os.WriteFile returned ENXIO and left it alone. The
// same applies to a FIFO, and to device nodes — an --output of /dev/null would
// otherwise unlink /dev/null rather than discard the report.
func TestWritePrivateFile_RefusesNonRegularDestinations(t *testing.T) {
	t.Run("unix socket", func(t *testing.T) {
		// Bind in a short temp dir: the sockaddr_un path limit (~104 bytes) is
		// well under what t.TempDir() plus a long test name can produce.
		dir, err := os.MkdirTemp("", "sock")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })

		path := filepath.Join(dir, "s.sock")
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Skipf("unix sockets unavailable: %v", err)
		}
		defer func() { _ = ln.Close() }()

		err = writePrivateFile(path, []byte("report"), 0o600)
		if err == nil {
			t.Fatal("writePrivateFile replaced a live socket; want an error")
		}
		if !strings.Contains(err.Error(), "is a socket") {
			t.Errorf("error should name the socket, got: %v", err)
		}
		info, statErr := os.Lstat(path)
		if statErr != nil {
			t.Fatalf("socket path was unlinked: %v", statErr)
		}
		if info.Mode()&os.ModeSocket == 0 {
			t.Error("socket was replaced by another file type")
		}
	})

	t.Run("named pipe", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pipe")
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Skipf("mkfifo unavailable: %v", err)
		}

		err := writePrivateFile(path, []byte("report"), 0o600)
		if err == nil {
			t.Fatal("writePrivateFile replaced a FIFO; want an error")
		}
		if !strings.Contains(err.Error(), "is a named pipe") {
			t.Errorf("error should name the FIFO, got: %v", err)
		}
		info, statErr := os.Lstat(path)
		if statErr != nil {
			t.Fatalf("FIFO was unlinked: %v", statErr)
		}
		if info.Mode()&os.ModeNamedPipe == 0 {
			t.Error("FIFO was replaced by another file type")
		}
	})
}

// TestFileKind pins the names the rejection message uses, including the
// character-device case that /dev/null exercises in the wild — a char device
// sets both ModeDevice and ModeCharDevice, so the ordering in fileKind matters.
func TestFileKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
		want string
	}{
		{"directory", os.ModeDir | 0o755, "a directory"},
		{"socket", os.ModeSocket | 0o600, "a socket"},
		{"named pipe", os.ModeNamedPipe | 0o600, "a named pipe"},
		{"character device", os.ModeDevice | os.ModeCharDevice | 0o666, "a character device"},
		{"block device", os.ModeDevice | 0o660, "a block device"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fileKind(tc.mode); got != tc.want {
				t.Errorf("fileKind(%v) = %q, want %q", tc.mode, got, tc.want)
			}
		})
	}
}

// TestWritePrivateFile_ReplacesSymlinkToDirectory is the other side of the
// directory guard: the check uses Lstat, so a symlink is judged as a symlink
// even when it points at a directory. The link is replaced by the artifact and
// the directory it named is left untouched — a Stat-based check would instead
// refuse the write.
func TestWritePrivateFile_ReplacesSymlinkToDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real-dir")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "dest")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := writePrivateFile(path, []byte("report"), 0o600); err != nil {
		t.Fatalf("writePrivateFile: %v", err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
		t.Error("destination should be a regular file after the write")
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	if targetInfo, err := os.Stat(target); err != nil || !targetInfo.IsDir() {
		t.Errorf("symlink target directory should be untouched (err=%v)", err)
	}
}

// minimal loadable config: LoadConfig validates that at least one exporter
// and the ClickHouse connection are configured.
const helperTestConfig = `clickhouse:
  host: localhost
  port: 9000

exporters:
  otel:
    - collector_address: localhost:4317
      service_name: helper-test

monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`

func TestLoadConfig_ValidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "click-dog.yaml")
	if err := os.WriteFile(path, []byte(helperTestConfig), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, resolvedPath, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if resolvedPath != path {
		t.Errorf("resolvedPath = %q, want %q", resolvedPath, path)
	}
	if cfg == nil || cfg.ClickHouse.Host != "localhost" {
		t.Errorf("config not loaded: %+v", cfg)
	}
}

func TestLoadConfig_MissingExplicitPath(t *testing.T) {
	_, _, err := loadConfig(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected error for missing explicit config path")
	}
}

// A file that resolves but fails to load must still return the resolved path,
// so callers can name the file they rejected.
func TestLoadConfig_LoadErrorReturnsResolvedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(path, []byte(":\tnot yaml"), 0600); err != nil {
		t.Fatal(err)
	}

	_, resolvedPath, err := loadConfig(path)
	if err == nil {
		t.Fatal("expected error for unparseable config")
	}
	if resolvedPath != path {
		t.Errorf("resolvedPath = %q, want %q even on load failure", resolvedPath, path)
	}
}

func TestBuildExporters_LabelsNamesAndOrder(t *testing.T) {
	cfg := &config.Config{}
	cfg.Exporters.OTEL = []config.OTELConfig{
		{CollectorAddress: "collector:4317", ServiceName: "svc"},
	}
	cfg.Exporters.SplunkHEC = []config.SplunkHECConfig{
		{Endpoint: "https://splunk.example.com:8088", Token: "tok"},
	}

	built := buildExporters(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, b := range built {
			if b.Exporter != nil {
				_ = b.Exporter.Close(ctx)
			}
		}
	})

	if len(built) != 2 {
		t.Fatalf("built %d exporters, want 2", len(built))
	}

	tests := []struct {
		idx      int
		label    string
		name     string
		endpoint string
		wantOTEL bool
	}{
		{0, "OTEL[0]", "otel[0]:collector:4317", "collector:4317", true},
		{1, "SplunkHEC[0]", "splunk_hec[0]:https://splunk.example.com:8088", "https://splunk.example.com:8088", false},
	}
	for _, tc := range tests {
		b := built[tc.idx]
		if b.InitErr != nil {
			t.Fatalf("built[%d] InitErr = %v, want nil", tc.idx, b.InitErr)
		}
		if b.Exporter == nil {
			t.Fatalf("built[%d] Exporter is nil", tc.idx)
		}
		if b.Label != tc.label {
			t.Errorf("built[%d] Label = %q, want %q", tc.idx, b.Label, tc.label)
		}
		if b.Name != tc.name {
			t.Errorf("built[%d] Name = %q, want %q", tc.idx, b.Name, tc.name)
		}
		if b.Endpoint != tc.endpoint {
			t.Errorf("built[%d] Endpoint = %q, want %q", tc.idx, b.Endpoint, tc.endpoint)
		}
		if (b.OTEL != nil) != tc.wantOTEL {
			t.Errorf("built[%d] OTEL non-nil = %v, want %v", tc.idx, b.OTEL != nil, tc.wantOTEL)
		}
		// Both shipped exporter kinds support active probing; `check` relies
		// on the type assertion, so pin it here.
		if _, ok := b.Exporter.(model.ConnectivityChecker); !ok {
			t.Errorf("built[%d] (%s) does not implement model.ConnectivityChecker", tc.idx, b.Label)
		}
	}
}

// A constructor failure must land in InitErr for that entry — not abort the
// build — so `check` and `test-span` can report the broken exporter and keep
// probing the rest.
func TestBuildExporters_InitErrorDoesNotAbortBuild(t *testing.T) {
	cfg := &config.Config{}
	cfg.Exporters.SplunkHEC = []config.SplunkHECConfig{
		{Endpoint: "https://splunk.example.com:8088"}, // no token → constructor error
		{Endpoint: "https://splunk2.example.com:8088", Token: "tok"},
	}

	built := buildExporters(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, b := range built {
			if b.Exporter != nil {
				_ = b.Exporter.Close(ctx)
			}
		}
	})

	if len(built) != 2 {
		t.Fatalf("built %d exporters, want 2", len(built))
	}
	if built[0].InitErr == nil || built[0].Exporter != nil {
		t.Errorf("built[0] = {InitErr: %v, Exporter: %v}, want init error and nil exporter", built[0].InitErr, built[0].Exporter)
	}
	if !strings.Contains(built[0].Label, "SplunkHEC[0]") {
		t.Errorf("built[0] Label = %q, want SplunkHEC[0]", built[0].Label)
	}
	if built[1].InitErr != nil || built[1].Exporter == nil {
		t.Errorf("built[1] = {InitErr: %v}, want working exporter after a failed one", built[1].InitErr)
	}
}
