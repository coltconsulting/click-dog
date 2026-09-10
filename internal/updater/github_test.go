package updater

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/testutil"
)

func TestParseChecksumLines(t *testing.T) {
	hashAmd64 := strings.Repeat("a", 64)
	hashArm64 := strings.Repeat("b", 64)
	shortHash := "abc123" // not 64 chars — must be dropped, not silently kept
	input := []byte(hashAmd64 + "  click-dog_26.04.1_linux_amd64.tar.gz\n" +
		hashArm64 + "  click-dog_26.04.1_linux_arm64.tar.gz\n" +
		shortHash + "  click-dog_26.04.1_linux_short.tar.gz\n" +
		"malformed-line-with-only-one-field\n" +
		"trailing  whitespace  three-fields\n")

	got, err := parseChecksumLines(input)
	if err != nil {
		t.Fatalf("parseChecksumLines() error = %v", err)
	}
	want := map[string]string{
		"click-dog_26.04.1_linux_amd64.tar.gz": hashAmd64,
		"click-dog_26.04.1_linux_arm64.tar.gz": hashArm64,
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d entries, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("checksums[%q] = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["click-dog_26.04.1_linux_short.tar.gz"]; ok {
		t.Errorf("short-hash entry was kept; SHA256 length check is not enforced")
	}
}

// fakeCosign writes a shell script to dir/cosign that exits with the given
// status, recording its invocation args to dir/args.log. Returns dir.
func fakeCosign(t *testing.T, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake cosign uses /bin/sh — Linux/macOS only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "cosign")
	logPath := filepath.Join(dir, "args.log")
	script := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do echo \"$a\" >> %q; done\nexit %d\n", logPath, exitCode)
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// hangingCosign writes a shell script that sleeps for sleepSeconds before
// exiting 0. Uses `exec sleep` so the sleep replaces the shell process and
// inherits as cmd.Process — otherwise exec.CommandContext would only kill
// the shell, leaving the sleep child holding stdout open and CombinedOutput
// blocked until the sleep naturally finishes.
func hangingCosign(t *testing.T, sleepSeconds int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake cosign uses /bin/sh — Linux/macOS only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "cosign")
	script := fmt.Sprintf("#!/bin/sh\nexec sleep %d\n", sleepSeconds)
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeBlobTriple writes empty placeholder checksums/sig/cert files into a
// fresh temp dir and returns their paths. Used to drive verifyCosignBlob
// without needing valid signing material.
func writeBlobTriple(t *testing.T) (checksums, sig, cert string) {
	t.Helper()
	tmp := t.TempDir()
	checksums = filepath.Join(tmp, "checksums.txt")
	sig = filepath.Join(tmp, "checksums.txt.sig")
	cert = filepath.Join(tmp, "checksums.txt.pem")
	for _, p := range []string{checksums, sig, cert} {
		if err := os.WriteFile(p, []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return checksums, sig, cert
}

func TestVerifyCosignBlob_Success(t *testing.T) {
	dir := fakeCosign(t, 0)
	cosign := filepath.Join(dir, "cosign")
	checksums, sig, cert := writeBlobTriple(t)

	if err := verifyCosignBlob(cosign, checksums, sig, cert, 5*time.Second); err != nil {
		t.Fatalf("verifyCosignBlob() = %v, want nil", err)
	}

	// Confirm the identity pinning args were passed through unchanged.
	logged, err := os.ReadFile(filepath.Join(dir, "args.log"))
	if err != nil {
		t.Fatal(err)
	}
	args := string(logged)
	for _, want := range []string{
		"verify-blob",
		"--certificate", cert,
		"--signature", sig,
		"--certificate-identity-regexp",
		cosignIdentityRegexp,
		"--certificate-oidc-issuer",
		cosignOIDCIssuer,
		checksums,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("cosign args missing %q. Got:\n%s", want, args)
		}
	}
}

func TestVerifyCosignBlob_FailureFailsClosed(t *testing.T) {
	dir := fakeCosign(t, 1)
	cosign := filepath.Join(dir, "cosign")
	checksums, sig, cert := writeBlobTriple(t)

	err := verifyCosignBlob(cosign, checksums, sig, cert, 5*time.Second)
	if err == nil {
		t.Fatal("verifyCosignBlob() = nil, want non-nil for cosign exit 1")
	}
	if !strings.Contains(err.Error(), "cosign verify-blob failed") {
		t.Errorf("error = %q, want it to mention cosign verify-blob failure", err)
	}
}

func TestVerifyCosignBlob_TimesOut(t *testing.T) {
	// A blocked Rekor/Fulcio endpoint must not hang self-update. Run a
	// fake cosign that sleeps longer than the (passed-in) deadline and
	// confirm verifyCosignBlob aborts with a timeout-flagged error.
	dir := hangingCosign(t, 5)
	cosign := filepath.Join(dir, "cosign")
	checksums, sig, cert := writeBlobTriple(t)

	start := time.Now()
	err := verifyCosignBlob(cosign, checksums, sig, cert, 100*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("verifyCosignBlob() = nil, want timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want timeout message", err)
	}
	// 3s gives WaitDelay (2s) plenty of slack while still proving we
	// don't ride out the full 5s sleep.
	if elapsed > 3*time.Second {
		t.Errorf("verifyCosignBlob took %v, want <3s — deadline not enforced", elapsed)
	}
}

func TestFetchReleaseChecksums_HappyPath(t *testing.T) {
	dir := fakeCosign(t, 0)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	hash := strings.Repeat("a", 64)
	checksumsBody := hash + "  click-dog_26.04.1_linux_amd64.tar.gz\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/checksums.txt":
			_, _ = w.Write([]byte(checksumsBody))
		case "/checksums.txt.sig":
			_, _ = w.Write([]byte("fake-signature"))
		case "/checksums.txt.pem":
			_, _ = w.Write([]byte("fake-cert"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	g := &GitHubClient{HTTPClient: srv.Client()}
	release := &Release{
		TagName: "v26.04.1",
		Assets: []Asset{
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/checksums.txt"},
			{Name: "checksums.txt.sig", BrowserDownloadURL: srv.URL + "/checksums.txt.sig"},
			{Name: "checksums.txt.pem", BrowserDownloadURL: srv.URL + "/checksums.txt.pem"},
		},
	}

	got, err := g.FetchReleaseChecksums(release, false)
	if err != nil {
		t.Fatalf("FetchReleaseChecksums() = %v, want nil", err)
	}
	if got.Mode != ReleaseVerificationSigned {
		t.Errorf("mode = %q, want %q", got.Mode, ReleaseVerificationSigned)
	}
	if got.Entries["click-dog_26.04.1_linux_amd64.tar.gz"] != hash {
		t.Errorf("checksums = %v, want %s for amd64 archive", got.Entries, hash)
	}
}

func TestFetchReleaseChecksums_MissingSignature(t *testing.T) {
	dir := fakeCosign(t, 0)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The HTTP server is load-bearing: checksums.txt IS in Assets, so the
	// fetch is actually issued and must succeed before we reach the
	// signature lookup that's the actual subject of this test.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	g := &GitHubClient{HTTPClient: srv.Client()}
	// Release lists checksums.txt but not the signature pair.
	release := &Release{
		TagName: "v26.04.1",
		Assets: []Asset{
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/checksums.txt"},
		},
	}

	_, err := g.FetchReleaseChecksums(release, false)
	if err == nil {
		t.Fatal("FetchReleaseChecksums() = nil, want failure for missing signature asset")
	}
	if !errors.Is(err, ErrCosignVerification) {
		t.Errorf("error = %v, want ErrCosignVerification", err)
	}
	if !strings.Contains(err.Error(), "missing cosign signature") {
		t.Errorf("error = %q, want it to mention missing cosign signature", err)
	}
}

func TestFetchReleaseChecksums_MissingCertificate(t *testing.T) {
	dir := fakeCosign(t, 0)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	g := &GitHubClient{HTTPClient: srv.Client()}
	// Release lists checksums.txt and .sig but not the .pem certificate.
	release := &Release{
		TagName: "v26.04.1",
		Assets: []Asset{
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/checksums.txt"},
			{Name: "checksums.txt.sig", BrowserDownloadURL: srv.URL + "/checksums.txt.sig"},
		},
	}

	_, err := g.FetchReleaseChecksums(release, false)
	if err == nil {
		t.Fatal("FetchReleaseChecksums() = nil, want failure for missing certificate asset")
	}
	if !errors.Is(err, ErrCosignVerification) {
		t.Errorf("error = %v, want ErrCosignVerification", err)
	}
	if !strings.Contains(err.Error(), "missing cosign certificate") {
		t.Errorf("error = %q, want it to mention missing cosign certificate", err)
	}
}

func TestFetchReleaseChecksums_EmptyChecksums(t *testing.T) {
	// A signed-but-empty checksums.txt would otherwise leak through to
	// cmd_selfupdate.go as a "no checksum found for archive" error,
	// which is misleading. Surface the real cause early.
	dir := fakeCosign(t, 0)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// All three assets respond, but checksums.txt is empty.
		_, _ = w.Write([]byte(""))
	}))
	defer srv.Close()

	g := &GitHubClient{HTTPClient: srv.Client()}
	release := &Release{
		TagName: "v26.04.1",
		Assets: []Asset{
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/checksums.txt"},
			{Name: "checksums.txt.sig", BrowserDownloadURL: srv.URL + "/checksums.txt.sig"},
			{Name: "checksums.txt.pem", BrowserDownloadURL: srv.URL + "/checksums.txt.pem"},
		},
	}

	_, err := g.FetchReleaseChecksums(release, false)
	if err == nil {
		t.Fatal("FetchReleaseChecksums() = nil, want failure for empty checksums.txt")
	}
	if !strings.Contains(err.Error(), "is empty") {
		t.Errorf("error = %q, want it to mention empty checksums", err)
	}
}

func TestFetchReleaseChecksums_BadSignatureFailsClosed(t *testing.T) {
	dir := fakeCosign(t, 1) // cosign rejects the signature
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	g := &GitHubClient{HTTPClient: srv.Client()}
	release := &Release{
		TagName: "v26.04.1",
		Assets: []Asset{
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/checksums.txt"},
			{Name: "checksums.txt.sig", BrowserDownloadURL: srv.URL + "/checksums.txt.sig"},
			{Name: "checksums.txt.pem", BrowserDownloadURL: srv.URL + "/checksums.txt.pem"},
		},
	}

	got, err := g.FetchReleaseChecksums(release, false)
	if err == nil {
		t.Fatal("FetchReleaseChecksums() = nil, want failure on bad signature")
	}
	if !errors.Is(err, ErrCosignVerification) {
		t.Errorf("error = %v, want ErrCosignVerification", err)
	}
	if got.Entries != nil || got.Mode != "" {
		t.Errorf("got = %+v, want zero result on signature failure (must not return unverified checksums)", got)
	}
}

func TestFetchReleaseChecksums_DangerouslyIgnoreCosign(t *testing.T) {
	dir := fakeCosign(t, 1)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	hash := strings.Repeat("a", 64)
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		_, _ = fmt.Fprintf(w, "%s  click-dog_26.04.1_linux_amd64.tar.gz\n", hash)
	}))
	defer srv.Close()

	g := &GitHubClient{HTTPClient: srv.Client()}
	release := &Release{
		TagName: "v26.04.1",
		Assets: []Asset{
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/checksums.txt"},
			{Name: "checksums.txt.sig", BrowserDownloadURL: srv.URL + "/checksums.txt.sig"},
			{Name: "checksums.txt.pem", BrowserDownloadURL: srv.URL + "/checksums.txt.pem"},
		},
	}

	got, err := g.FetchReleaseChecksums(release, true)
	if err != nil {
		t.Fatalf("FetchReleaseChecksums() = %v, want explicit checksum-only success", err)
	}
	if got.Mode != ReleaseVerificationCosignIgnored {
		t.Errorf("mode = %q, want %q", got.Mode, ReleaseVerificationCosignIgnored)
	}
	if got.Entries["click-dog_26.04.1_linux_amd64.tar.gz"] != hash {
		t.Errorf("checksums = %v, want archive hash", got.Entries)
	}
	if len(requested) != 1 || requested[0] != "/checksums.txt" {
		t.Errorf("requested assets = %v, want checksums.txt only", requested)
	}
	if _, err := os.Stat(filepath.Join(dir, "args.log")); !os.IsNotExist(err) {
		t.Errorf("Cosign was invoked despite dangerous override; stat error = %v", err)
	}
}

func TestFetchSmallAsset_OversizeRejected(t *testing.T) {
	// Serve more than maxSmallAssetSize bytes. fetchSmallAsset must
	// reject the response rather than silently truncate, otherwise a
	// malicious release could substitute a giant blob and we'd hand
	// downstream a truncated file.
	oversize := make([]byte, maxSmallAssetSize+128)
	for i := range oversize {
		oversize[i] = 'a'
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(oversize)
	}))
	defer srv.Close()

	g := &GitHubClient{HTTPClient: srv.Client()}
	release := &Release{
		TagName: "v26.04.1",
		Assets: []Asset{
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/big"},
		},
	}

	_, err := g.fetchSmallAsset(release, "checksums.txt")
	if err == nil {
		t.Fatal("fetchSmallAsset() = nil, want failure for oversize body")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %q, want it to mention size limit", err)
	}
}

func TestCosignIdentityRegexpMatchesDocs(t *testing.T) {
	// docs/verify-releases.md publishes the cosign verify-blob invocation that
	// operators are told to run when downloading binaries manually. If
	// the constant in this file drifts from the docs snippet, manual
	// verifiers would be checking a different signer identity than
	// self-update — false assurance. Lock the two together.
	docsRoot := filepath.Join("..", "..", "docs")
	info, err := os.Stat(docsRoot)
	if err == nil && !info.IsDir() {
		t.Fatalf("%s exists but is not a directory", docsRoot)
	}
	if os.IsNotExist(err) {
		public, publicErr := testutil.IsPublicSourceTree(filepath.Join("..", ".."))
		if publicErr != nil {
			t.Fatalf("identify public source tree: %v", publicErr)
		}
		if !public {
			t.Fatalf("%s is missing from the internal repository", docsRoot)
		}
		t.Skip("docs are internal-only (export-ignored); signer identity parity is enforced in the internal repository")
	}
	if err != nil {
		t.Fatalf("stat %s: %v", docsRoot, err)
	}

	docsPath := filepath.Join("..", "..", "docs", "verify-releases.md")
	docs, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatalf("read %s: %v", docsPath, err)
	}
	if !bytes.Contains(docs, []byte(cosignIdentityRegexp)) {
		t.Errorf("cosignIdentityRegexp = %q not present verbatim in %s — docs and code have drifted", cosignIdentityRegexp, docsPath)
	}
}

func TestFetchReleaseChecksums_CosignNotInstalled(t *testing.T) {
	// Empty PATH so exec.LookPath("cosign") fails.
	t.Setenv("PATH", "")

	hash := strings.Repeat("a", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  click-dog_26.04.1_linux_amd64.tar.gz\n", hash)
	}))
	defer srv.Close()
	g := &GitHubClient{HTTPClient: srv.Client()}
	release := &Release{
		TagName: "v26.04.1",
		Assets:  []Asset{{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/checksums.txt"}},
	}

	got, err := g.FetchReleaseChecksums(release, false)
	if err != nil {
		t.Fatalf("FetchReleaseChecksums() = %v, want checksum-only success", err)
	}
	if got.Mode != ReleaseVerificationNoCosign {
		t.Errorf("mode = %q, want %q", got.Mode, ReleaseVerificationNoCosign)
	}
	if got.Entries["click-dog_26.04.1_linux_amd64.tar.gz"] != hash {
		t.Errorf("checksums = %v, want archive hash", got.Entries)
	}
}

func TestSelectNewest(t *testing.T) {
	tests := []struct {
		name     string
		releases []Release
		wantTag  string
		wantErr  bool
	}{
		{
			name: "prerelease newer than stable is selected",
			releases: []Release{
				{TagName: "v26.05.1-alpha"},
				{TagName: "v26.04.6"},
			},
			wantTag: "v26.05.1-alpha",
		},
		{
			name: "stable newer than prerelease is selected",
			releases: []Release{
				{TagName: "v26.06.1"},
				{TagName: "v26.05.1-alpha"},
			},
			wantTag: "v26.06.1",
		},
		{
			name: "drafts are skipped",
			releases: []Release{
				{TagName: "v26.07.1", Draft: true},
				{TagName: "v26.05.1-alpha"},
			},
			wantTag: "v26.05.1-alpha",
		},
		{
			name: "unparseable tags are skipped",
			releases: []Release{
				{TagName: "nightly"},
				{TagName: "v26.04.6"},
			},
			wantTag: "v26.04.6",
		},
		{
			name: "tie prefers stable when prerelease is listed first",
			releases: []Release{
				{TagName: "v26.05.1-alpha"},
				{TagName: "v26.05.1"},
			},
			wantTag: "v26.05.1",
		},
		{
			name: "tie prefers stable when stable is listed first",
			releases: []Release{
				{TagName: "v26.05.1"},
				{TagName: "v26.05.1-alpha"},
			},
			wantTag: "v26.05.1",
		},
		{
			name:     "no usable release is an error",
			releases: []Release{{TagName: "nightly"}, {TagName: "v1", Draft: true}},
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectNewest(tt.releases)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("selectNewest() = %v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectNewest() error = %v", err)
			}
			if got.TagName != tt.wantTag {
				t.Errorf("selectNewest() tag = %q, want %q", got.TagName, tt.wantTag)
			}
		})
	}
}

func TestLatestReleaseIncludingPrerelease_PicksHighestInstallable(t *testing.T) {
	// Asset names must match FindAsset's expectation for the running platform.
	asset := func(ver string) string {
		return fmt.Sprintf("click-dog_%s_%s_%s.tar.gz", ver, runtime.GOOS, runtime.GOARCH)
	}
	var gotPath, gotPerPage string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotPerPage = r.URL.Query().Get("per_page")
		// v26.06.1 is the highest version but ships no asset for this platform,
		// so it must be skipped in favor of the newest installable release.
		_, _ = fmt.Fprintf(w, `[
			{"tag_name":"v26.06.1","draft":false,"assets":[]},
			{"tag_name":"v26.05.1-alpha","draft":false,"assets":[{"name":%q}]},
			{"tag_name":"v26.04.6","draft":false,"assets":[{"name":%q}]}
		]`, asset("26.05.1-alpha"), asset("26.04.6"))
	}))
	defer srv.Close()

	g := &GitHubClient{HTTPClient: srv.Client(), baseURL: srv.URL, Owner: "o", Repo: "r"}
	rel, err := g.LatestReleaseIncludingPrerelease()
	if err != nil {
		t.Fatalf("LatestReleaseIncludingPrerelease() error = %v", err)
	}
	// v26.06.1 has no platform asset, so the newest installable release wins.
	if rel.TagName != "v26.05.1-alpha" {
		t.Errorf("tag = %q, want v26.05.1-alpha", rel.TagName)
	}
	// Must hit the list endpoint, not /releases/latest (which hides prereleases).
	if want := "/repos/o/r/releases"; gotPath != want {
		t.Errorf("request path = %q, want %q", gotPath, want)
	}
	// Pin the documented page bound so an accidental change is caught.
	if want := "100"; gotPerPage != want {
		t.Errorf("per_page = %q, want %q", gotPerPage, want)
	}
}

// When no release ships an asset for the running platform, the filter must not
// swallow the result as "no usable release found"; it falls back to the full
// set so the caller's FindAsset can report the precise per-platform error.
func TestLatestReleaseIncludingPrerelease_FallsBackWhenNoPlatformAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assets exist, but for a different platform than the test runner.
		_, _ = fmt.Fprint(w, `[
			{"tag_name":"v26.06.1","draft":false,"assets":[{"name":"click-dog_26.06.1_plan9_mips.tar.gz"}]},
			{"tag_name":"v26.05.1-alpha","draft":false,"assets":[{"name":"click-dog_26.05.1-alpha_plan9_mips.tar.gz"}]}
		]`)
	}))
	defer srv.Close()

	g := &GitHubClient{HTTPClient: srv.Client(), baseURL: srv.URL, Owner: "o", Repo: "r"}
	rel, err := g.LatestReleaseIncludingPrerelease()
	if err != nil {
		t.Fatalf("LatestReleaseIncludingPrerelease() error = %v", err)
	}
	// Falls back to the full set and returns the highest version; the caller
	// then surfaces the real "no asset for this platform" error.
	if rel.TagName != "v26.06.1" {
		t.Errorf("tag = %q, want v26.06.1 (fallback to highest)", rel.TagName)
	}
}

func TestLatestRelease_UsesLatestEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = fmt.Fprint(w, `{"tag_name":"v26.04.6","prerelease":false}`)
	}))
	defer srv.Close()

	g := &GitHubClient{HTTPClient: srv.Client(), baseURL: srv.URL, Owner: "o", Repo: "r"}
	rel, err := g.LatestRelease()
	if err != nil {
		t.Fatalf("LatestRelease() error = %v", err)
	}
	if rel.TagName != "v26.04.6" {
		t.Errorf("tag = %q, want v26.04.6", rel.TagName)
	}
	if want := "/repos/o/r/releases/latest"; gotPath != want {
		t.Errorf("request path = %q, want %q", gotPath, want)
	}
}
