package clicklog

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var (
	logMu           sync.RWMutex
	currentLogLevel Level = LevelError
	logWriter       *rotatingWriter
	logFormat       string = "text" // "text" or "json"
)

// credentialPatterns matches common credential strings that may leak into log
// messages via wrapped errors from database drivers, gRPC libraries, etc.
// Each pattern replaces the sensitive portion with [REDACTED].
var credentialPatterns = []*regexp.Regexp{
	// password=secret or password='secret' or password:"secret" in connection strings / errors
	regexp.MustCompile(`(?i)(password\s*[:=]\s*['"]?)([^'"\s,;]+)(['"]?)`),
	// Bearer and Basic auth tokens
	regexp.MustCompile(`(?i)((?:Bearer|Basic)\s+)([A-Za-z0-9_.+/=-]+)`),
	// Splunk HEC token in Authorization header
	regexp.MustCompile(`(?i)(Splunk\s+)([A-Za-z0-9_-]{8,})`),
	// Generic token= or api_key= or secret= parameters
	regexp.MustCompile(`(?i)((?:token|api_key|apikey|secret|access_key|secret_key)\s*[:=]\s*['"]?)([^'"\s,;]+)(['"]?)`),
	// Space-separated key/value forms (e.g. "password 'x'", "AuthToken xyz") emitted by
	// driver / wrapper errors that don't use [:=] as a separator. Defense-in-depth — may
	// over-redact in log prose ("password expired" → "password [REDACTED]"); preferred to
	// missing a real credential.
	regexp.MustCompile(`(?i)((?:password|passwd|pwd|token|api_key|apikey|secret|access_key|secret_key|auth_token|authtoken)\s+['"]?)([^'"\s,;]+)(['"]?)`),
	// DSN-embedded credentials: scheme://user:pass@host
	regexp.MustCompile(`(://[^:]+:)([^@]+)(@)`),
}

// sanitize scrubs known credential patterns from a log message.
func sanitize(msg string) string {
	for _, re := range credentialPatterns {
		msg = re.ReplaceAllStringFunc(msg, func(match string) string {
			loc := re.FindStringSubmatchIndex(match)
			if len(loc) < 6 {
				return match
			}
			// Replace capture group 2 (the secret value) with [REDACTED]
			prefix := match[loc[2]:loc[3]]
			suffix := ""
			if len(loc) >= 8 && loc[6] >= 0 {
				suffix = match[loc[6]:loc[7]]
			}
			return prefix + "[REDACTED]" + suffix
		})
	}
	return msg
}

func writeVisibleUnicodeEscape(b *strings.Builder, r rune) {
	if r <= '\uffff' {
		_, _ = fmt.Fprintf(b, `\u%04x`, r)
		return
	}
	_, _ = fmt.Fprintf(b, `\U%08x`, r)
}

// escapeTextControls keeps an untrusted message on a single physical log line
// and prevents terminal control sequences from changing how operators see it.
func escapeTextControls(msg string) string {
	var b strings.Builder
	b.Grow(len(msg))
	for _, r := range msg {
		switch r {
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\x1b':
			b.WriteString(`\x1b`)
		default:
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
				writeVisibleUnicodeEscape(&b, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// escapeJSONFormatControls makes DEL, C1, and Unicode format controls visible
// without changing JSON's normal semantic handling of newlines and C0 controls.
// encoding/json quotes C0 controls, but intentionally preserves DEL, C1, and
// general Cf runes such as bidi overrides and zero-width characters.
func escapeJSONFormatControls(msg string) string {
	var b strings.Builder
	b.Grow(len(msg))
	for _, r := range msg {
		if r == '\u007f' || (r >= '\u0080' && r <= '\u009f') || unicode.In(r, unicode.Cf) {
			writeVisibleUnicodeEscape(&b, r)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// rotatingWriter is an io.Writer that performs size-based log rotation.
type rotatingWriter struct {
	mu          sync.Mutex
	file        *os.File
	filePath    string
	maxBytes    int64
	maxFiles    int
	currentSize int64
}

func newRotatingWriter(filePath string, maxBytes int64, maxFiles int) (*rotatingWriter, error) {
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}

	// Seed currentSize from existing file
	var currentSize int64
	if info, statErr := f.Stat(); statErr == nil {
		currentSize = info.Size()
	}

	// Clean up orphaned rotated files from a previously higher max_files setting
	for i := maxFiles + 1; ; i++ {
		path := fmt.Sprintf("%s.%d", filePath, i)
		if err := os.Remove(path); err != nil {
			break
		}
	}

	return &rotatingWriter{
		file:        f,
		filePath:    filePath,
		maxBytes:    maxBytes,
		maxFiles:    maxFiles,
		currentSize: currentSize,
	}, nil
}

func (w *rotatingWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.maxBytes > 0 && w.currentSize+int64(len(p)) > w.maxBytes {
		if rotErr := w.rotate(); rotErr != nil {
			return 0, rotErr
		}
	}

	n, err = w.file.Write(p)
	w.currentSize += int64(n)
	return n, err
}

func (w *rotatingWriter) rotate() error {
	// Close current file
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("failed to close log file for rotation: %w", err)
	}

	// Shift existing rotated files: .N -> .N+1, delete beyond maxFiles.
	// Errors are intentionally ignored: files may not exist yet, and
	// failing to shift an old log should not block the current rotation.
	for i := w.maxFiles; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.filePath, i)
		if i == w.maxFiles {
			_ = os.Remove(src)
		} else {
			dst := fmt.Sprintf("%s.%d", w.filePath, i+1)
			_ = os.Rename(src, dst)
		}
	}

	// Rename current -> .1
	if err := os.Rename(w.filePath, w.filePath+".1"); err != nil {
		// Rename failed after close — try to reopen the original path so
		// subsequent writes don't silently fail on a closed file handle.
		if f, reopenErr := os.OpenFile(w.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640); reopenErr == nil {
			w.file = f
			// currentSize is approximate but keeps rotation functional
			if info, statErr := f.Stat(); statErr == nil {
				w.currentSize = info.Size()
			}
		}
		return fmt.Errorf("failed to rotate log file: %w", err)
	}

	// Open fresh file
	f, err := os.OpenFile(w.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return fmt.Errorf("failed to open new log file after rotation: %w", err)
	}

	w.file = f
	w.currentSize = 0
	return nil
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

func InitLogger(level string, logFilePath string, format string, maxSizeMB int, maxFiles int) error {
	logMu.Lock()
	defer logMu.Unlock()

	switch strings.ToLower(level) {
	case "debug":
		currentLogLevel = LevelDebug
	case "info":
		currentLogLevel = LevelInfo
	case "warn", "warning":
		currentLogLevel = LevelWarn
	case "error":
		currentLogLevel = LevelError
	default:
		currentLogLevel = LevelError
	}

	if format == "" {
		format = "text"
	}
	logFormat = strings.ToLower(format)

	// JSON output includes its own "ts" field — suppress the stdlib log prefix
	// to avoid double timestamps like: 2024/01/01 00:00:00 {"ts":"...","msg":"..."}
	if logFormat == "json" {
		log.SetFlags(0)
	} else {
		log.SetFlags(log.LstdFlags)
	}

	// If log file path is provided, write to file only. Otherwise stderr.
	if logFilePath != "" {
		maxBytes := int64(maxSizeMB) * 1024 * 1024
		w, err := newRotatingWriter(logFilePath, maxBytes, maxFiles)
		if err != nil {
			return err
		}
		logWriter = w
		log.SetOutput(w)
	} else {
		// Default to stderr only
		log.SetOutput(os.Stderr)
	}

	return nil
}

func CloseLogger() {
	logMu.Lock()
	defer logMu.Unlock()
	if logWriter != nil {
		if err := logWriter.Close(); err != nil {
			log.Printf("[WARN] failed to close log file: %s", escapeTextControls(err.Error()))
		}
		logWriter = nil
	}
}

// readLogState returns the current log level and format under RLock.
func readLogState() (Level, string) {
	logMu.RLock()
	defer logMu.RUnlock()
	return currentLogLevel, logFormat
}

// logJSONMsg writes a pre-formatted message as a JSON log entry.
func logJSONMsg(level string, msg string) {
	entry := map[string]string{
		"ts":    time.Now().UTC().Format(time.RFC3339),
		"level": level,
		"msg":   escapeJSONFormatControls(msg),
	}
	data, _ := json.Marshal(entry) // map[string]string never fails to marshal
	log.Println(string(data))
}

func Debug(format string, v ...interface{}) {
	level, fmt_ := readLogState()
	if level <= LevelDebug {
		msg := sanitize(fmt.Sprintf(format, v...))
		if fmt_ == "json" {
			logJSONMsg("debug", msg)
		} else {
			log.Printf("[DEBUG] %s", escapeTextControls(msg))
		}
	}
}

func Info(format string, v ...interface{}) {
	level, fmt_ := readLogState()
	if level <= LevelInfo {
		msg := sanitize(fmt.Sprintf(format, v...))
		if fmt_ == "json" {
			logJSONMsg("info", msg)
		} else {
			log.Printf("[INFO] %s", escapeTextControls(msg))
		}
	}
}

func Warn(format string, v ...interface{}) {
	level, fmt_ := readLogState()
	if level <= LevelWarn {
		msg := sanitize(fmt.Sprintf(format, v...))
		if fmt_ == "json" {
			logJSONMsg("warn", msg)
		} else {
			log.Printf("[WARN] %s", escapeTextControls(msg))
		}
	}
}

func Error(format string, v ...interface{}) {
	level, fmt_ := readLogState()
	if level <= LevelError {
		msg := sanitize(fmt.Sprintf(format, v...))
		if fmt_ == "json" {
			logJSONMsg("error", msg)
		} else {
			log.Printf("[ERROR] %s", escapeTextControls(msg))
		}
	}
}

// Fatal always logs regardless of level
func Fatal(format string, v ...interface{}) {
	_, fmt_ := readLogState()
	msg := sanitize(fmt.Sprintf(format, v...))
	if fmt_ == "json" {
		entry := map[string]string{
			"ts":    time.Now().UTC().Format(time.RFC3339),
			"level": "fatal",
			"msg":   escapeJSONFormatControls(msg),
		}
		data, _ := json.Marshal(entry) // map[string]string never fails to marshal
		log.Fatalf("%s", string(data))
	} else {
		log.Fatalf("[FATAL] %s", escapeTextControls(msg))
	}
}
