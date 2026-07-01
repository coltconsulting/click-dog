package updater

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// maxBinarySize is the maximum allowed size for the extracted binary (100 MiB).
// Protects against decompression bombs.
const maxBinarySize = 100 << 20

// ExtractBinary reads a tar.gz archive from r and extracts the "click-dog"
// binary to destPath. Returns an error if the binary is not found in the archive
// or exceeds maxBinarySize.
//
// destPath must not already exist: it is opened with O_EXCL so extraction never
// writes into a pre-existing (potentially attacker-planted) file. The file is
// created mode 0700 — owner-only — so the staged binary is never world-readable
// or world-executable during the smoke-test window before the atomic swap.
func ExtractBinary(r io.Reader, destPath string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("opening gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}

		// GoReleaser may place the binary at the archive root or in a subdirectory
		name := filepath.Base(hdr.Name)
		if name != "click-dog" {
			continue
		}

		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Size > int64(maxBinarySize) {
			return fmt.Errorf("click-dog binary exceeds maximum size: %d bytes > %d bytes", hdr.Size, maxBinarySize)
		}

		f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0700)
		if err != nil {
			return fmt.Errorf("creating %s: %w", destPath, err)
		}
		n, err := io.Copy(f, io.LimitReader(tr, maxBinarySize))
		if err != nil {
			_ = f.Close()
			_ = os.Remove(destPath)
			return fmt.Errorf("extracting binary: %w", err)
		}
		if n != hdr.Size {
			_ = f.Close()
			_ = os.Remove(destPath)
			return fmt.Errorf("archive member size mismatch: header claimed %d bytes, read %d bytes", hdr.Size, n)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("closing %s: %w", destPath, err)
		}
		return nil
	}

	return fmt.Errorf("click-dog binary not found in archive")
}
