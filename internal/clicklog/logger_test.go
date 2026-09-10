package clicklog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingWriter_RotatesOnSize(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")

	// 100 bytes max — small threshold for easy testing
	w, err := newRotatingWriter(logPath, 100, 3)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Write 80 bytes — no rotation yet
	msg := strings.Repeat("A", 80) + "\n"
	if _, err := w.Write([]byte(msg)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(logPath + ".1"); !os.IsNotExist(err) {
		t.Fatal("rotated file should not exist yet")
	}

	// Write another 30 bytes — pushes past 100, triggers rotation
	msg2 := strings.Repeat("B", 29) + "\n"
	if _, err := w.Write([]byte(msg2)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// The old content should now be in .1
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("expected rotated file .1 to exist: %v", err)
	}

	// Fresh file should contain only the second message
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "BBB") {
		t.Errorf("fresh log should contain second message, got: %q", string(data))
	}

	// Rotated file should contain the first message
	data1, err := os.ReadFile(logPath + ".1")
	if err != nil {
		t.Fatalf("ReadFile .1: %v", err)
	}
	if !strings.Contains(string(data1), "AAA") {
		t.Errorf("rotated .1 should contain first message, got: %q", string(data1))
	}
}

func TestRotatingWriter_MaxFilesRespected(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")

	// maxFiles=2, 50 bytes max
	w, err := newRotatingWriter(logPath, 50, 2)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Trigger 3 rotations
	for i := 0; i < 4; i++ {
		msg := strings.Repeat(string(rune('A'+i)), 49) + "\n"
		if _, err := w.Write([]byte(msg)); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	// .1 and .2 should exist
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Errorf("expected .1 to exist: %v", err)
	}
	if _, err := os.Stat(logPath + ".2"); err != nil {
		t.Errorf("expected .2 to exist: %v", err)
	}
	// .3 should NOT exist — maxFiles=2
	if _, err := os.Stat(logPath + ".3"); !os.IsNotExist(err) {
		t.Errorf("expected .3 to not exist (maxFiles=2)")
	}
}

func TestRotatingWriter_ResumeSize(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")

	// Pre-populate file with 70 bytes
	if err := os.WriteFile(logPath, []byte(strings.Repeat("X", 70)), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// maxBytes=100 — we already have 70, so 31 more should trigger rotation
	w, err := newRotatingWriter(logPath, 100, 3)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer func() { _ = w.Close() }()

	if w.currentSize != 70 {
		t.Fatalf("currentSize = %d, want 70", w.currentSize)
	}

	// Write 20 bytes — no rotation (70+20=90 < 100)
	if _, err := w.Write([]byte(strings.Repeat("A", 20))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(logPath + ".1"); !os.IsNotExist(err) {
		t.Fatal("should not have rotated yet")
	}

	// Write 20 more — pushes past 100 (90+20=110), triggers rotation
	if _, err := w.Write([]byte(strings.Repeat("B", 20))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("expected .1 after rotation: %v", err)
	}
}

func TestRotatingWriter_CleansOrphanedFiles(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")

	// Simulate a previous run with max_files=5: create .1 through .5
	if err := os.WriteFile(logPath, []byte("current"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	for i := 1; i <= 5; i++ {
		path := fmt.Sprintf("%s.%d", logPath, i)
		if err := os.WriteFile(path, []byte(fmt.Sprintf("old-%d", i)), 0644); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}

	// Open with max_files=2 — should clean up .3, .4, .5
	w, err := newRotatingWriter(logPath, 1000, 2)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer func() { _ = w.Close() }()

	// .1 and .2 should still exist
	for i := 1; i <= 2; i++ {
		path := fmt.Sprintf("%s.%d", logPath, i)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s to still exist: %v", path, err)
		}
	}
	// .3, .4, .5 should be gone
	for i := 3; i <= 5; i++ {
		path := fmt.Sprintf("%s.%d", logPath, i)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected %s to be cleaned up", path)
		}
	}
}

func TestRotatingWriter_NoRotationStderr(t *testing.T) {
	// When logFilePath is empty, InitLogger should use stderr, not create a writer
	defer func() { logWriter = nil }() // ensure cleanup regardless of outcome
	err := InitLogger("info", "", "text", 100, 3)
	if err != nil {
		t.Fatalf("InitLogger: %v", err)
	}
	if logWriter != nil {
		t.Error("logWriter should be nil when logging to stderr")
	}
}

func TestInitLogger_TextFormat(t *testing.T) {
	if err := InitLogger("info", "", "text", 0, 0); err != nil {
		t.Fatalf("InitLogger failed: %v", err)
	}
	defer CloseLogger()

	if logFormat != "text" {
		t.Errorf("logFormat = %q, want text", logFormat)
	}
	if currentLogLevel != LevelInfo {
		t.Errorf("currentLogLevel = %d, want %d", currentLogLevel, LevelInfo)
	}
}

func TestInitLogger_JSONFormat(t *testing.T) {
	if err := InitLogger("debug", "", "json", 0, 0); err != nil {
		t.Fatalf("InitLogger failed: %v", err)
	}
	defer CloseLogger()

	if logFormat != "json" {
		t.Errorf("logFormat = %q, want json", logFormat)
	}
	if currentLogLevel != LevelDebug {
		t.Errorf("currentLogLevel = %d, want %d", currentLogLevel, LevelDebug)
	}

	// Verify InitLogger suppresses the stdlib log prefix (SetFlags(0))
	// so JSON output starts with '{' rather than a timestamp.
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	Info("json format test")
	output := buf.String()
	trimmed := strings.TrimSpace(output)
	if !strings.HasPrefix(trimmed, "{") {
		t.Errorf("JSON log output should start with '{', got: %s", trimmed)
	}
}

func TestInitLogger_DefaultFormat(t *testing.T) {
	if err := InitLogger("info", "", "", 0, 0); err != nil {
		t.Fatalf("InitLogger failed: %v", err)
	}
	defer CloseLogger()

	if logFormat != "text" {
		t.Errorf("logFormat = %q, want text (default)", logFormat)
	}
}

func TestLogJSON_Output(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	logFormat = "json"
	currentLogLevel = LevelDebug

	Info("test message %d", 42)

	output := buf.String()
	// Should contain valid JSON
	// Find the JSON object in the log output (stdlib log prepends timestamp)
	idx := strings.Index(output, "{")
	if idx < 0 {
		t.Fatalf("expected JSON in output, got: %s", output)
	}
	jsonPart := strings.TrimSpace(output[idx:])

	var entry map[string]string
	if err := json.Unmarshal([]byte(jsonPart), &entry); err != nil {
		t.Fatalf("failed to parse JSON log: %v (raw: %s)", err, jsonPart)
	}

	if entry["level"] != "info" {
		t.Errorf("level = %q, want info", entry["level"])
	}
	if entry["msg"] != "test message 42" {
		t.Errorf("msg = %q, want 'test message 42'", entry["msg"])
	}
	if entry["ts"] == "" {
		t.Error("expected ts field in JSON output")
	}
}

func TestLogText_Output(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	logFormat = "text"
	currentLogLevel = LevelDebug

	Warn("test warning %s", "here")

	output := buf.String()
	if !strings.Contains(output, "[WARN]") {
		t.Errorf("expected [WARN] prefix, got: %s", output)
	}
	if !strings.Contains(output, "test warning here") {
		t.Errorf("expected message in output, got: %s", output)
	}
}

func TestLogLevel_Filtering(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	logFormat = "text"
	currentLogLevel = LevelWarn

	Debug("should not appear")
	Info("should not appear either")
	Warn("should appear")
	Error("should also appear")

	output := buf.String()
	if strings.Contains(output, "should not appear") {
		t.Errorf("debug/info messages should be filtered at warn level, got: %s", output)
	}
	if !strings.Contains(output, "should appear") {
		t.Errorf("warn message should appear, got: %s", output)
	}
	if !strings.Contains(output, "should also appear") {
		t.Errorf("error message should appear, got: %s", output)
	}
}

func TestInitLogger_FileOutput(t *testing.T) {
	tmpFile := t.TempDir() + "/test.log"

	if err := InitLogger("info", tmpFile, "text", 0, 0); err != nil {
		t.Fatalf("InitLogger failed: %v", err)
	}

	Info("file test message")
	CloseLogger()

	data, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if !strings.Contains(string(data), "file test message") {
		t.Errorf("log file should contain message, got: %s", string(data))
	}

	// Reset logger state
	logWriter = nil
	log.SetOutput(os.Stderr)
}

// TestRotatingWriter_FileMode0640 verifies log files are created with mode
// 0640 — service user RW, group R, world none. Operators relying on a log
// shipper running under a different UID must put the shipper in the
// service user's group (see operator docs).
//
// The umask running this test must allow 0640 (typical 022 leaves it
// untouched; an 077 umask would tighten to 0600 and is still acceptable
// since owner-only is stricter, not weaker).
func TestRotatingWriter_FileMode0640(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "perm.log")

	w, err := newRotatingWriter(logPath, 50, 2)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer func() { _ = w.Close() }()

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	mode := info.Mode().Perm()
	// World bits must be clear; group read may be present.
	if mode&0o007 != 0 {
		t.Errorf("log file world-bits set, mode=%#o (want world 0)", mode)
	}
	if mode > 0o640 {
		t.Errorf("log file mode = %#o, want <= 0640", mode)
	}

	// Trigger a rotation; the freshly-opened file should also be 0640.
	for i := 0; i < 3; i++ {
		if _, err := w.Write([]byte(strings.Repeat("Z", 49) + "\n")); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	info2, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("Stat after rotate: %v", err)
	}
	mode2 := info2.Mode().Perm()
	if mode2&0o007 != 0 {
		t.Errorf("rotated-fresh log world-bits set, mode=%#o", mode2)
	}
	if mode2 > 0o640 {
		t.Errorf("rotated-fresh log mode = %#o, want <= 0640", mode2)
	}
}

// TestSanitize_SpaceSeparatedCredentials covers the driver-error-style
// forms that motivated 7c: keys followed by space (no [:=] separator).
func TestSanitize_SpaceSeparatedCredentials(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // substring that must appear after sanitize
		bad  string // substring that must NOT appear after sanitize
	}{
		{
			name: "password space-quoted",
			in:   "Authentication failed: password 'hunter2' is incorrect",
			want: "[REDACTED]",
			bad:  "hunter2",
		},
		{
			name: "password space-unquoted",
			in:   "auth error: password hunter2 invalid",
			want: "[REDACTED]",
			bad:  "hunter2",
		},
		{
			name: "AuthToken colon-space",
			in:   "request rejected: AuthToken: abc.def-ghi",
			want: "[REDACTED]",
			bad:  "abc.def-ghi",
		},
		{
			name: "AuthToken space-only",
			in:   "request rejected: AuthToken abc.def-ghi",
			want: "[REDACTED]",
			bad:  "abc.def-ghi",
		},
		{
			name: "token space-quoted double",
			in:   `gRPC error: token "tok-XYZ-123"`,
			want: "[REDACTED]",
			bad:  "tok-XYZ-123",
		},
		{
			name: "secret space-unquoted",
			in:   "exporter setup: secret s3cr3t-value applied",
			want: "[REDACTED]",
			bad:  "s3cr3t-value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitize(tt.in)
			if !strings.Contains(got, tt.want) {
				t.Errorf("sanitize(%q) = %q; want to contain %q", tt.in, got, tt.want)
			}
			if strings.Contains(got, tt.bad) {
				t.Errorf("sanitize(%q) = %q; secret %q leaked", tt.in, got, tt.bad)
			}
		})
	}
}

func TestEscapeTextControls(t *testing.T) {
	input := "query failed\n[INFO] forged\r\x1b[2J\t\u202eabc\U0001bca0"
	want := `query failed\n[INFO] forged\r\x1b[2J\t\u202eabc\U0001bca0`
	if got := escapeTextControls(input); got != want {
		t.Fatalf("escapeTextControls() = %q, want %q", got, want)
	}
}

func TestJSONLogEscapesFormatControls(t *testing.T) {
	if err := InitLogger("info", "", "json", 0, 0); err != nil {
		t.Fatalf("InitLogger failed: %v", err)
	}
	defer CloseLogger()

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	Info("request: %s", "line1\nline2\u007f\u0085\u009b\u202eabc\U0001bca0")
	output := strings.TrimSpace(buf.String())
	if strings.ContainsAny(output, "\u007f\u0085\u009b\u202e\U0001bca0") {
		t.Fatalf("JSON log contains a raw DEL, C1, or Unicode format control: %q", output)
	}

	var entry map[string]string
	if err := json.Unmarshal([]byte(output), &entry); err != nil {
		t.Fatalf("JSON log is invalid: %v\n%s", err, output)
	}
	want := "request: line1\nline2\\u007f\\u0085\\u009b\\u202eabc\\U0001bca0"
	if entry["msg"] != want {
		t.Fatalf("JSON msg = %q, want %q", entry["msg"], want)
	}
}

func TestTextLogEscapesControls(t *testing.T) {
	if err := InitLogger("info", "", "text", 0, 0); err != nil {
		t.Fatalf("InitLogger failed: %v", err)
	}
	defer CloseLogger()

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	Info("request: %s", "ok\n[ERROR] forged\x1b[2J")
	output := strings.TrimSuffix(buf.String(), "\n")
	if strings.ContainsAny(output, "\r\n\x1b") {
		t.Fatalf("text log contains a raw control character: %q", output)
	}
	if !strings.Contains(output, `ok\n[ERROR] forged\x1b[2J`) {
		t.Fatalf("text log does not contain escaped message: %q", output)
	}
}
