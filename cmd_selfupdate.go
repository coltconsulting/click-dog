package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/coltconsulting/click-dog/internal/updater"
)

func runSelfUpdate(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("self-update", flag.ContinueOnError)
	fs.SetOutput(errOut)
	checkOnly := fs.Bool("check", false, "Check for updates without downloading")
	prerelease := fs.Bool("prerelease", false, "Include prereleases (alpha/beta) when checking for updates")
	dangerouslyIgnoreCosign := fs.Bool("dangerously-ignore-cosign", false, "Deliberately skip cosign publisher authentication (SHA-256 still required)")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(errOut, `click-dog self-update — update to the latest release

Usage:
  click-dog self-update [flags]

Flags:
`)
		printFlagDefaults(errOut, fs)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if version == "dev" {
		_, _ = fmt.Fprintln(errOut, "Error: self-update is not available for dev builds")
		return 1
	}

	currentVer, err := updater.ParseVersion(version)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: cannot parse current version %q: %v\n", version, err)
		return 1
	}

	// Fetch latest release
	gh := updater.NewGitHubClient()
	_, _ = fmt.Fprintln(out, "Checking for updates...")
	var release *updater.Release
	if *prerelease {
		release, err = gh.LatestReleaseIncludingPrerelease()
	} else {
		release, err = gh.LatestRelease()
	}
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: %v\n", err)
		return 1
	}

	latestVer, err := updater.ParseVersion(release.TagName)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: cannot parse latest version %q: %v\n", release.TagName, err)
		return 1
	}

	if !currentVer.Less(latestVer) {
		_, _ = fmt.Fprintf(out, "Already up to date (%s)\n", version)
		return 0
	}

	// Print the raw stamped version and release tag (not CalVer.String) so the
	// exact build identifiers show — consistent with the "Already up to date"
	// line above.
	_, _ = fmt.Fprintf(out, "Update available: %s -> %s\n", version, release.TagName)

	if *checkOnly {
		return 0
	}

	// Find matching asset
	asset, err := release.FindAsset()
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: %v\n", err)
		return 1
	}

	// Resolve the canonical path of the running binary
	binaryPath, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: cannot determine binary path: %v\n", err)
		return 1
	}
	binaryPath, err = filepath.EvalSymlinks(binaryPath)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: cannot resolve symlinks: %v\n", err)
		return 1
	}

	// Stage the new binary in a private, unpredictably-named directory created
	// inside the install dir (mode 0700, owned by the updating user). Keeping it
	// inside binDir means the final rename onto the canonical path stays on the
	// same filesystem and so remains atomic — os.MkdirTemp("") could land on a
	// different filesystem and fail the rename with EXDEV.
	//
	// EnsureSecureBinDir is the primary defense: it refuses to update when binDir
	// is group/other-writable (without the sticky bit), because such a directory
	// lets an unprivileged user replace the staged file — or the canonical binary
	// itself — before this root-run update executes it. The 0700 staging dir and
	// O_EXCL extraction (see updater.ExtractBinary) are defense-in-depth on top.
	//
	// The archive is staged in the OS temp dir so the install directory isn't
	// used for a large (up to 200 MiB) transient file. On success both are
	// removed; on error each exit path cleans up explicitly. On interrupt
	// (SIGINT/SIGTERM) temp files may be left on disk — harmless since the
	// original binary is untouched until the final atomic Replace call.
	binDir := filepath.Dir(binaryPath)
	if err := updater.EnsureSecureBinDir(binDir); err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: %v\n", err)
		return 1
	}
	stagingDir, err := os.MkdirTemp(binDir, ".click-dog-update-*")
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: creating staging dir: %v\n", err)
		return 1
	}
	tmpPath := filepath.Join(stagingDir, "click-dog")
	archiveFile, err := os.CreateTemp("", "click-dog-*.tar.gz")
	if err != nil {
		_ = os.RemoveAll(stagingDir)
		_, _ = fmt.Fprintf(errOut, "Error: creating temp archive: %v\n", err)
		return 1
	}
	archivePath := archiveFile.Name()

	// Always fetch the release manifest. If cosign is installed, authenticate it
	// against the pinned release workflow identity before trusting any hash. A
	// failed cosign attempt is fatal; checksum-only mode is selected only when
	// cosign is absent or the operator explicitly passes the dangerous override.
	_, _ = fmt.Fprintln(out, "Fetching release checksums...")
	releaseChecksums, err := gh.FetchReleaseChecksums(release, *dangerouslyIgnoreCosign)
	if err != nil {
		_ = archiveFile.Close()
		_ = os.Remove(archivePath)
		_ = os.RemoveAll(stagingDir)
		_, _ = fmt.Fprintf(errOut, "Error: %v\n", err)
		if errors.Is(err, updater.ErrCosignVerification) {
			_, _ = fmt.Fprintln(errOut, "cosign verification was attempted and failed; checksum-only fallback was not used.")
			_, _ = fmt.Fprintln(errOut, "Retry with --dangerously-ignore-cosign only if you deliberately accept checksum-only verification.")
		}
		return 1
	}
	writeSelfUpdateVerificationMode(out, errOut, releaseChecksums.Mode, false)

	// Download archive to a temp file for checksum verification
	_, _ = fmt.Fprintf(out, "Downloading %s...\n", asset.Name)
	body, err := gh.DownloadAsset(asset)
	if err != nil {
		_ = archiveFile.Close()
		_ = os.Remove(archivePath)
		_ = os.RemoveAll(stagingDir)
		_, _ = fmt.Fprintf(errOut, "Error: %v\n", err)
		return 1
	}

	if _, err := io.Copy(archiveFile, body); err != nil {
		_ = archiveFile.Close()
		_ = body.Close()
		_ = os.Remove(archivePath)
		_ = os.RemoveAll(stagingDir)
		_, _ = fmt.Fprintf(errOut, "Error: downloading: %v\n", err)
		return 1
	}
	_ = archiveFile.Close()
	_ = body.Close()

	// Verify checksum
	expectedHash, ok := releaseChecksums.Entries[asset.Name]
	if !ok {
		_ = os.Remove(archivePath)
		_ = os.RemoveAll(stagingDir)
		_, _ = fmt.Fprintf(errOut, "Error: no checksum found for %s in checksums.txt\n", asset.Name)
		return 1
	}
	_, _ = fmt.Fprintln(out, "Verifying checksum...")
	if err := updater.VerifyChecksum(archivePath, expectedHash); err != nil {
		_ = os.Remove(archivePath)
		_ = os.RemoveAll(stagingDir)
		_, _ = fmt.Fprintf(errOut, "Error: %v\n", err)
		return 1
	}

	// Extract to temp
	archiveReader, err := os.Open(archivePath)
	if err != nil {
		_ = os.Remove(archivePath)
		_ = os.RemoveAll(stagingDir)
		_, _ = fmt.Fprintf(errOut, "Error: %v\n", err)
		return 1
	}
	if err := updater.ExtractBinary(archiveReader, tmpPath); err != nil {
		_ = archiveReader.Close()
		_ = os.Remove(archivePath)
		_ = os.RemoveAll(stagingDir)
		_, _ = fmt.Fprintf(errOut, "Error: %v\n", err)
		return 1
	}
	_ = archiveReader.Close()
	_ = os.Remove(archivePath)

	// Smoke-test
	_, _ = fmt.Fprintln(out, "Verifying new binary...")
	newVer, err := updater.SmokeTest(tmpPath)
	if err != nil {
		_ = os.RemoveAll(stagingDir)
		_, _ = fmt.Fprintf(errOut, "Error: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "New binary version: %s\n", newVer)

	// Atomic replace. On success tmpPath has been renamed onto binaryPath, so
	// only the now-empty staging dir remains to clean up.
	if err := updater.Replace(binaryPath, tmpPath); err != nil {
		_ = os.RemoveAll(stagingDir)
		_, _ = fmt.Fprintf(errOut, "Error: %v\n", err)
		return 1
	}
	_ = os.RemoveAll(stagingDir)

	_, _ = fmt.Fprintf(out, "Updated click-dog from %s to %s\n", version, release.TagName)
	writeSelfUpdateVerificationMode(out, errOut, releaseChecksums.Mode, true)

	// Restart the service if it's running
	if err := exec.Command("systemctl", "is-active", "click-dog").Run(); err == nil {
		_, _ = fmt.Fprintln(out, "Restarting click-dog service...")
		if err := exec.Command("systemctl", "restart", "click-dog").Run(); err != nil {
			_, _ = fmt.Fprintf(errOut, "Warning: failed to restart service: %v\n", err)
			_, _ = fmt.Fprintln(errOut, "Run manually: systemctl restart click-dog")
		} else {
			_, _ = fmt.Fprintln(out, "Service restarted.")
		}
	} else {
		_, _ = fmt.Fprintln(out, "Service not running — start with: systemctl start click-dog")
	}

	return 0
}

func writeSelfUpdateVerificationMode(out, errOut io.Writer, mode updater.ReleaseVerificationMode, archiveVerified bool) {
	archiveSuffix := ""
	if archiveVerified {
		archiveSuffix = " Archive SHA-256 matched."
	}
	switch mode {
	case updater.ReleaseVerificationSigned:
		_, _ = fmt.Fprintf(out, "Publisher signature: verified with cosign.%s\n", archiveSuffix)
	case updater.ReleaseVerificationNoCosign:
		_, _ = fmt.Fprintf(errOut, "WARNING: cosign is not installed; publisher signature was not verified.%s\n", archiveSuffix)
		if !archiveVerified {
			_, _ = fmt.Fprintln(errOut, "Official Cosign installation docs: https://docs.sigstore.dev/cosign/system_config/installation/")
		}
	case updater.ReleaseVerificationCosignIgnored:
		_, _ = fmt.Fprintf(errOut, "DANGER: publisher signature deliberately skipped by --dangerously-ignore-cosign.%s\n", archiveSuffix)
	}
}
