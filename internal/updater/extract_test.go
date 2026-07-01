package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func createTestArchive(t *testing.T, files map[string][]byte) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Size: int64(len(content)),
			Mode: 0755,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}

	_ = tw.Close()
	_ = gw.Close()
	return &buf
}

func createOversizedBinaryArchive(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	size := int64(maxBinarySize) + 1
	hdr := &tar.Header{
		Name: "click-dog",
		Size: size,
		Mode: 0755,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(tw, zeroReader{}, size); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func TestExtractBinary_Found(t *testing.T) {
	archive := createTestArchive(t, map[string][]byte{
		"click-dog": []byte("binary-content"),
		"README.md": []byte("readme"),
	})

	dest := filepath.Join(t.TempDir(), "click-dog")
	if err := ExtractBinary(archive, dest); err != nil {
		t.Fatalf("ExtractBinary failed: %v", err)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading extracted binary: %v", err)
	}
	if string(data) != "binary-content" {
		t.Errorf("got %q, want %q", string(data), "binary-content")
	}
}

func TestExtractBinary_InSubdir(t *testing.T) {
	archive := createTestArchive(t, map[string][]byte{
		"click-dog_25.04.0_linux_amd64/click-dog": []byte("nested-binary"),
	})

	dest := filepath.Join(t.TempDir(), "click-dog")
	if err := ExtractBinary(archive, dest); err != nil {
		t.Fatalf("ExtractBinary failed: %v", err)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "nested-binary" {
		t.Errorf("got %q, want %q", string(data), "nested-binary")
	}
}

func TestExtractBinary_NotFound(t *testing.T) {
	archive := createTestArchive(t, map[string][]byte{
		"other-binary": []byte("nope"),
	})

	dest := filepath.Join(t.TempDir(), "click-dog")
	err := ExtractBinary(archive, dest)
	if err == nil {
		t.Fatal("expected error when binary not found in archive")
	}
}

func TestExtractBinary_RejectsOversizedBinary(t *testing.T) {
	archive := createOversizedBinaryArchive(t)
	dest := filepath.Join(t.TempDir(), "click-dog")

	err := ExtractBinary(archive, dest)
	if err == nil {
		t.Fatal("expected error for oversized binary")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("error = %v, want size-limit error", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("oversized extraction left destination behind: stat err = %v", statErr)
	}
}

func TestExtractBinary_RefusesExistingFile(t *testing.T) {
	archive := createTestArchive(t, map[string][]byte{
		"click-dog": []byte("binary-content"),
	})

	dest := filepath.Join(t.TempDir(), "click-dog")
	// Pre-plant a file at the destination — O_EXCL must refuse to clobber it.
	if err := os.WriteFile(dest, []byte("planted"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ExtractBinary(archive, dest); err == nil {
		t.Fatal("expected error when destination already exists (O_EXCL)")
	}

	// The planted file must be left untouched.
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "planted" {
		t.Errorf("planted file was modified: got %q", string(data))
	}
}

func TestExtractBinary_Mode0700(t *testing.T) {
	archive := createTestArchive(t, map[string][]byte{
		"click-dog": []byte("binary-content"),
	})

	dest := filepath.Join(t.TempDir(), "click-dog")
	if err := ExtractBinary(archive, dest); err != nil {
		t.Fatalf("ExtractBinary failed: %v", err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0700 {
		t.Errorf("extracted binary mode = %o, want 0700", got)
	}
}
