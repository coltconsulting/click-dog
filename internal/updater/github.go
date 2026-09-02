package updater

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultOwner = "coltconsulting"
	defaultRepo  = "click-dog"

	// defaultAPIBase is the public GitHub API host. Single source of truth so
	// the constructor default and the apiBase() fallback can't drift.
	defaultAPIBase = "https://api.github.com"

	// releaseListPageSize bounds the /releases listing used by the prerelease
	// channel. GitHub's max is 100; a prerelease older than the newest 100
	// releases is intentionally not offered.
	releaseListPageSize = 100

	// httpTimeout is the timeout for GitHub API and download requests.
	httpTimeout = 30 * time.Second

	// maxResponseSize is the maximum size for API responses (10 MiB).
	maxResponseSize = 10 << 20

	// maxDownloadSize is the maximum size for release asset downloads (200 MiB).
	maxDownloadSize = 200 << 20

	// maxSmallAssetSize bounds checksums.txt and the cosign signature pair
	// (1 MiB). Real artifacts are well under 1 KiB; the tighter limit means
	// a malicious release substituting a giant blob fails fast.
	maxSmallAssetSize = 1 << 20

	// cosignIdentityRegexp pins the signer identity to the click-dog release
	// workflow run on a release tag. Hardcoded — never read from config — so
	// a compromised config or release asset cannot weaken it. The same regex
	// appears in docs/install.md's manual-verification snippet; keep them
	// in sync if this ever changes.
	cosignIdentityRegexp = `^https://github\.com/coltconsulting/click-dog/\.github/workflows/release\.yml@refs/tags/v.+$`
	cosignOIDCIssuer     = `https://token.actions.githubusercontent.com`

	// cosignVerifyTimeout caps cosign verify-blob, which makes outbound
	// calls to Rekor (and potentially Fulcio). Without this, a slow or
	// blocked transparency-log endpoint would hang self-update indefinitely.
	cosignVerifyTimeout = 60 * time.Second
)

// Release represents a GitHub release.
type Release struct {
	TagName string  `json:"tag_name"`
	Draft   bool    `json:"draft"`
	Assets  []Asset `json:"assets"`
}

// Asset represents a downloadable file attached to a release.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	APIURL             string `json:"url"` // API URL — required for private repo downloads
}

// GitHubClient fetches release information from the GitHub API.
type GitHubClient struct {
	HTTPClient     *http.Client // Short timeout for API calls and release asset metadata
	DownloadClient *http.Client // No timeout — large downloads are bounded by LimitReader instead
	Owner          string
	Repo           string
	Token          string // Optional GitHub token for private repos (from GITHUB_TOKEN env)
	baseURL        string // GitHub API base; overridable in tests. Defaults to https://api.github.com
}

// NewGitHubClient creates a client for the default click-dog repository.
// If GITHUB_TOKEN is set, it is used for authentication (required for private repos).
func NewGitHubClient() *GitHubClient {
	return &GitHubClient{
		HTTPClient: &http.Client{Timeout: httpTimeout},
		// No overall Timeout on DownloadClient — large assets can take
		// minutes to stream. Connection-level deadlines below still guard
		// against stalled dials/handshakes/headers; body size is bounded
		// by maxDownloadSize via LimitReader.
		DownloadClient: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout: httpTimeout,
				}).DialContext,
				TLSHandshakeTimeout:   httpTimeout,
				ResponseHeaderTimeout: 60 * time.Second,
			},
		},
		Owner:   defaultOwner,
		Repo:    defaultRepo,
		Token:   os.Getenv("GITHUB_TOKEN"),
		baseURL: defaultAPIBase,
	}
}

// apiBase returns the configured API base, falling back to the public GitHub
// API so directly-constructed clients (e.g. in tests) without baseURL still
// reach a sane default for endpoints that don't carry their own URL.
func (g *GitHubClient) apiBase() string {
	if g.baseURL == "" {
		return defaultAPIBase
	}
	return g.baseURL
}

// getJSON issues an authenticated GET to url with the GitHub Accept header and
// decodes the size-limited JSON body into dst. Shared by the release-fetch
// methods so a change to API request handling lands in one place.
func (g *GitHubClient) getJSON(url string, dst any) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	g.setAuth(req)

	resp, err := g.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("requesting %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
		return fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize)).Decode(dst); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// setAuth adds the Authorization header if a token is configured.
func (g *GitHubClient) setAuth(req *http.Request) {
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
}

// LatestRelease fetches the latest stable release from GitHub. GitHub's
// /releases/latest endpoint deliberately excludes prereleases and drafts, so
// this only ever returns a published, non-prerelease version. Use
// LatestReleaseIncludingPrerelease for the opt-in prerelease channel.
func (g *GitHubClient) LatestRelease() (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", g.apiBase(), g.Owner, g.Repo)
	var release Release
	if err := g.getJSON(url, &release); err != nil {
		return nil, err
	}
	return &release, nil
}

// LatestReleaseIncludingPrerelease fetches the most recent release counting
// prereleases. GitHub's /releases/latest endpoint hides prereleases, so to let
// opt-in testers update onto an alpha/beta we list /releases and select the
// highest version ourselves. The list is page-bounded (releaseListPageSize); a
// release older than that window is intentionally not offered.
func (g *GitHubClient) LatestReleaseIncludingPrerelease() (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=%d", g.apiBase(), g.Owner, g.Repo, releaseListPageSize)
	var releases []Release
	if err := g.getJSON(url, &releases); err != nil {
		return nil, err
	}

	// Prefer releases that ship a binary for this platform, so a newest tag
	// with no matching asset (or one still mid-publish) isn't selected and then
	// fail at download while an installable older release exists.
	installable := make([]Release, 0, len(releases))
	for i := range releases {
		if _, err := releases[i].FindAsset(); err == nil {
			installable = append(installable, releases[i])
		}
	}

	// If nothing ships an asset for this platform (e.g. self-update invoked on
	// an OS/arch the release pipeline doesn't build), fall back to the full set
	// so the caller's FindAsset reports the precise "no asset found for
	// <os>/<arch>" error rather than a generic "no usable release found".
	if len(installable) == 0 {
		installable = releases
	}

	return selectNewest(installable)
}

// selectNewest returns the highest-version release, skipping drafts and any
// release whose tag does not parse. Ordering (including prerelease precedence,
// where a stable release outranks a prerelease of the same YY.MM.idx) is
// delegated to CalVer.Less, so it does not matter what order GitHub lists them
// in. Returns an error if no usable release exists.
func selectNewest(releases []Release) (*Release, error) {
	var best *Release
	var bestVer CalVer
	for i := range releases {
		r := &releases[i]
		if r.Draft {
			continue
		}
		v, err := ParseVersion(r.TagName)
		if err != nil {
			continue
		}
		if best == nil || bestVer.Less(v) {
			best, bestVer = r, v
		}
	}
	if best == nil {
		return nil, errors.New("no usable release found")
	}
	return best, nil
}

// FindAsset finds the archive asset matching the current OS/arch.
// Archive naming convention: click-dog_VERSION_linux_amd64.tar.gz
func (r *Release) FindAsset() (*Asset, error) {
	goos := runtime.GOOS
	goarch := runtime.GOARCH

	// GoReleaser strips the "v" prefix in filenames
	version := strings.TrimPrefix(r.TagName, "v")
	expected := fmt.Sprintf("click-dog_%s_%s_%s.tar.gz", version, goos, goarch)

	for i := range r.Assets {
		if r.Assets[i].Name == expected {
			return &r.Assets[i], nil
		}
	}

	return nil, fmt.Errorf("no asset found for %s/%s (expected %s) in release %s", goos, goarch, expected, r.TagName)
}

// DownloadAsset downloads a release asset and returns a size-limited reader.
// Uses DownloadClient (no timeout) since large binaries can take minutes to
// download; the response body is bounded by maxDownloadSize via LimitReader.
func (g *GitHubClient) DownloadAsset(asset *Asset) (io.ReadCloser, error) {
	// For private repos, use the API URL with Accept: application/octet-stream.
	// BrowserDownloadURL redirects to S3 which strips the auth token.
	downloadURL := asset.BrowserDownloadURL
	if g.Token != "" && asset.APIURL != "" {
		downloadURL = asset.APIURL
	}

	req, err := http.NewRequest("GET", downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	g.setAuth(req)

	resp, err := g.DownloadClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", asset.Name, err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download returned %d for %s", resp.StatusCode, asset.Name)
	}

	return struct {
		io.Reader
		io.Closer
	}{
		Reader: io.LimitReader(resp.Body, maxDownloadSize),
		Closer: resp.Body,
	}, nil
}

// fetchSmallAsset downloads a named release asset and returns its bytes.
// Intended for small text artifacts (checksums.txt, signatures, certificates);
// the body is bounded by maxSmallAssetSize and an oversize response is
// reported as an error rather than silently truncated. Returns an error if
// the asset is not present in the release.
func (g *GitHubClient) fetchSmallAsset(release *Release, name string) ([]byte, error) {
	var asset *Asset
	for i := range release.Assets {
		if release.Assets[i].Name == name {
			asset = &release.Assets[i]
			break
		}
	}
	if asset == nil {
		return nil, fmt.Errorf("%s not found in release %s", name, release.TagName)
	}

	downloadURL := asset.BrowserDownloadURL
	if g.Token != "" && asset.APIURL != "" {
		downloadURL = asset.APIURL
	}

	req, err := http.NewRequest("GET", downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request for %s: %w", name, err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	g.setAuth(req)

	resp, err := g.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download of %s returned %d", name, resp.StatusCode)
	}

	// Read one byte past the limit so we can detect an oversize body and
	// fail loudly instead of returning a truncated artifact downstream.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSmallAssetSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	if int64(len(data)) > maxSmallAssetSize {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, maxSmallAssetSize)
	}
	return data, nil
}

// ReleaseVerificationMode records how the checksum manifest was trusted.
// The archive's exact SHA-256 is verified separately by the caller in every
// mode; these values describe publisher authentication of the manifest.
type ReleaseVerificationMode string

const (
	ReleaseVerificationSigned        ReleaseVerificationMode = "signed"
	ReleaseVerificationNoCosign      ReleaseVerificationMode = "checksum-no-cosign"
	ReleaseVerificationCosignIgnored ReleaseVerificationMode = "checksum-cosign-ignored"
)

// ReleaseChecksums is the parsed release manifest plus its authentication mode.
type ReleaseChecksums struct {
	Entries map[string]string
	Mode    ReleaseVerificationMode
}

// ErrCosignVerification marks a signed-verification attempt that failed. It
// lets the CLI explain that it did not silently downgrade to checksum-only and
// name the explicit dangerous override an operator may choose on a later run.
var ErrCosignVerification = errors.New("cosign verification failed")

// FetchReleaseChecksums downloads and parses checksums.txt. When Cosign is on
// PATH, it also downloads the keyless signature pair and authenticates the
// manifest against the click-dog release workflow identity. A failed Cosign
// attempt is fatal and never downgrades automatically. If Cosign is absent, or
// dangerouslyIgnoreCosign is explicitly true, the checksum manifest is
// returned with a mode that callers must report prominently.
func (g *GitHubClient) FetchReleaseChecksums(release *Release, dangerouslyIgnoreCosign bool) (ReleaseChecksums, error) {
	var result ReleaseChecksums
	checksumsBytes, err := g.fetchSmallAsset(release, "checksums.txt")
	if err != nil {
		return result, fmt.Errorf("release %s missing checksums: %w", release.TagName, err)
	}

	cosignPath := ""
	if dangerouslyIgnoreCosign {
		result.Mode = ReleaseVerificationCosignIgnored
	} else {
		path, lookErr := exec.LookPath("cosign")
		switch {
		case lookErr == nil:
			cosignPath = path
			result.Mode = ReleaseVerificationSigned
		case errors.Is(lookErr, exec.ErrNotFound):
			result.Mode = ReleaseVerificationNoCosign
		default:
			return ReleaseChecksums{}, fmt.Errorf("locating cosign: %w", lookErr)
		}
	}

	if cosignPath != "" {
		if err := g.authenticateChecksumManifest(release, checksumsBytes, cosignPath); err != nil {
			return ReleaseChecksums{}, fmt.Errorf("%w: %v", ErrCosignVerification, err)
		}
	}

	checksums, err := parseChecksumLines(checksumsBytes)
	if err != nil {
		return ReleaseChecksums{}, err
	}
	if len(checksums) == 0 {
		return ReleaseChecksums{}, fmt.Errorf("checksums.txt for release %s is empty", release.TagName)
	}
	result.Entries = checksums
	return result, nil
}

func (g *GitHubClient) authenticateChecksumManifest(release *Release, checksumsBytes []byte, cosignPath string) error {
	sigBytes, err := g.fetchSmallAsset(release, "checksums.txt.sig")
	if err != nil {
		return fmt.Errorf("release %s missing cosign signature: %w", release.TagName, err)
	}
	certBytes, err := g.fetchSmallAsset(release, "checksums.txt.pem")
	if err != nil {
		return fmt.Errorf("release %s missing cosign certificate: %w", release.TagName, err)
	}

	tmpDir, err := os.MkdirTemp("", "click-dog-cosign-*")
	if err != nil {
		return fmt.Errorf("creating temp dir for verification: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	checksumsPath := filepath.Join(tmpDir, "checksums.txt")
	sigPath := filepath.Join(tmpDir, "checksums.txt.sig")
	certPath := filepath.Join(tmpDir, "checksums.txt.pem")
	for _, e := range []struct {
		path string
		data []byte
	}{
		{checksumsPath, checksumsBytes},
		{sigPath, sigBytes},
		{certPath, certBytes},
	} {
		if err := os.WriteFile(e.path, e.data, 0600); err != nil {
			return fmt.Errorf("writing %s: %w", e.path, err)
		}
	}

	return verifyCosignBlob(cosignPath, checksumsPath, sigPath, certPath, cosignVerifyTimeout)
}

// verifyCosignBlob runs `cosign verify-blob` with the identity pinned to
// the click-dog release workflow on a release tag. Returns nil only on
// successful verification. Cosign v2 makes keyless verification the default,
// so no COSIGN_EXPERIMENTAL toggle is needed. Timeout is a parameter (not a
// global) so tests can shorten it without mutating package state.
func verifyCosignBlob(cosignPath, checksumsPath, sigPath, certPath string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, cosignPath, // #nosec G204 — argv-only, paths are temp files we just wrote
		"verify-blob",
		"--certificate", certPath,
		"--signature", sigPath,
		"--certificate-identity-regexp", cosignIdentityRegexp,
		"--certificate-oidc-issuer", cosignOIDCIssuer,
		checksumsPath,
	)
	// If the deadline fires while a child of cosign still holds stdout/stderr
	// open, CombinedOutput would block on the pipe. WaitDelay caps that wait.
	cmd.WaitDelay = 2 * time.Second
	output, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("cosign verify-blob timed out after %s (Rekor/Fulcio unreachable?): %s", timeout, strings.TrimSpace(string(output)))
		}
		return fmt.Errorf("cosign verify-blob failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// parseChecksumLines parses checksums.txt content into a filename → hex-SHA256
// map. Format per line: "<sha256>  <filename>". Malformed lines are skipped.
// The hash field must decode as exactly 32 bytes of hex (a SHA256 digest) —
// validating the bytes, not just the 64-char length, so a garbled non-hex line
// is dropped here rather than surfacing later as a misleading "checksum
// mismatch". Returns the scanner error (e.g. bufio.ErrTooLong on a pathological
// line) rather than a partial map.
func parseChecksumLines(b []byte) (map[string]string, error) {
	checksums := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(b))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		if decoded, err := hex.DecodeString(fields[0]); err != nil || len(decoded) != sha256.Size {
			continue
		}
		checksums[fields[1]] = fields[0]
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parsing checksums: %w", err)
	}
	return checksums, nil
}

// VerifyChecksum verifies that the file at path matches the expected SHA256 hex digest.
func VerifyChecksum(path, expectedHex string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s for checksum: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hashing %s: %w", path, err)
	}

	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expectedHex {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", path, expectedHex, actual)
	}
	return nil
}
