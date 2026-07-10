package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/minio/selfupdate"
	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"
)

// githubRepo is the release source for upgrade checks.
const githubRepo = "YorrickBao/kiro-anthropic"

// upgradeMaxBytes caps how much we'll buffer for a release archive (the
// binaries are a few MB; this is a generous ceiling).
const upgradeMaxBytes = 256 << 20

// maxBinaryBytes caps the decompressed binary size, guarding against a
// decompression bomb; a real Go binary is well under this.
const maxBinaryBytes = 512 << 20

// upgradeOptions configures the upgrade command.
type upgradeOptions struct {
	proxy           string // resolved (raw) --proxy value, same semantics as serve
	check           bool   // only report whether an update is available
	yes             bool   // skip the confirmation prompt
	version         string // specific tag to install (empty = latest)
	allowUnverified bool   // install even without a verified checksum (escape hatch)
}

// newUpgradeCmd builds the "upgrade" subcommand.
func newUpgradeCmd() *cobra.Command {
	opts := &upgradeOptions{}
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Download and install the latest release from GitHub",
		Long: "Check GitHub for a newer release of kiro-anthropic and replace the\n" +
			"current binary in place. Verifies the download against the release's\n" +
			"checksums.txt before installing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpgrade(cmd.Context(), opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.proxy, "proxy", "", "outbound HTTP proxy for GitHub calls; precedence: this flag > http(s)_proxy env > default "+defaultProxyURL+"; use 'none' to connect directly")
	f.BoolVar(&opts.check, "check", false, "only check whether an update is available; do not download or install")
	f.BoolVarP(&opts.yes, "yes", "y", false, "skip the confirmation prompt before installing")
	f.StringVar(&opts.version, "version", "", "install a specific release tag (e.g. v0.2.0); default: latest")
	f.BoolVar(&opts.allowUnverified, "allow-unverified", false, "install even if checksums.txt is missing or does not list the asset (NOT recommended)")
	return cmd
}

// runUpgrade is the top-level flow: configure proxy, fetch the target release,
// compare versions, then (unless --check) download, verify and replace.
func runUpgrade(ctx context.Context, opts *upgradeOptions) error {
	cfg := &Config{ProxyURL: opts.proxy}
	if err := configureProxy(cfg); err != nil {
		return err
	}
	client := newHTTPClient(cfg.ProxyURL)

	rel, err := fetchRelease(ctx, client, opts.version)
	if err != nil {
		return fmt.Errorf("lookup release: %w", err)
	}

	current := canonicalSemver(version)
	latest := canonicalSemver(rel.TagName)
	switch cmp := compareVersions(current, latest); {
	case !semver.IsValid(current):
		// A dev/unstamped build; can't tell if newer, so just report and ask.
		fmt.Printf("current version is %q (development build); latest release is %s\n", version, rel.TagName)
	case cmp >= 0 && opts.version == "":
		fmt.Printf("already up to date (%s)\n", version)
		return nil
	case cmp >= 0:
		fmt.Printf("version %s is already installed (latest requested: %s)\n", current, rel.TagName)
		return nil
	default:
		fmt.Printf("update available: %s -> %s\n", current, rel.TagName)
	}

	if opts.check {
		return nil
	}

	asset, err := pickAsset(rel.Assets, runtime.GOOS, runtime.GOARCH, rel.TagName)
	if err != nil {
		return err
	}

	if !opts.yes {
		fmt.Printf("Downloading %s and replacing %s\n", asset.Name, selfExeLabel())
		fmt.Print("Proceed? [y/N] ")
		if !confirm() {
			fmt.Println("aborted")
			return nil
		}
	}

	fmt.Printf("downloading %s ...\n", asset.Name)
	archiveBytes, err := download(ctx, client, asset.URL)
	if err != nil {
		return fmt.Errorf("download %s: %w", asset.Name, err)
	}

	// Verify the download against the release's checksums.txt before we ever
	// write it over the running binary. build.sh always ships checksums.txt, so
	// a missing/unlisted checksum is anomalous and aborts the upgrade unless the
	// operator explicitly opts out.
	if opts.allowUnverified {
		fmt.Fprintln(os.Stderr, "warning: --allow-unverified set; installing without checksum verification")
	} else {
		curl := rel.checksumURL()
		if curl == "" {
			return fmt.Errorf("release %s has no checksums.txt (use --allow-unverified to override)", rel.TagName)
		}
		sum, err := download(ctx, client, curl)
		if err != nil {
			return fmt.Errorf("fetch checksums.txt (use --allow-unverified to override): %w", err)
		}
		want, ok := parseChecksums(sum)[asset.Name]
		if !ok {
			return fmt.Errorf("%s not listed in checksums.txt (use --allow-unverified to override)", asset.Name)
		}
		if err := verifySHA256(archiveBytes, want); err != nil {
			return err
		}
	}

	bin, err := extractBinary(archiveBytes, asset.Name)
	if err != nil {
		return fmt.Errorf("extract binary: %w", err)
	}

	// Pre-flight: surface a clear message if we can't write the target.
	var so selfupdate.Options
	if err := so.CheckPermissions(); err != nil {
		return fmt.Errorf("%w\n(the binary may be in a privileged location — try running with sudo)", err)
	}

	fmt.Println("installing ...")
	if err := selfupdate.Apply(bytes.NewReader(bin), so); err != nil {
		if rerr := selfupdate.RollbackError(err); rerr != nil {
			return fmt.Errorf("update failed and rollback also failed: %v (original: %v) — recover manually", rerr, err)
		}
		return fmt.Errorf("update failed: %w", err)
	}

	fmt.Printf("upgraded to %s. Restart any running kiro-anthropic to use it.\n", rel.TagName)
	return nil
}

// --- GitHub release discovery ---------------------------------------------

type githubAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int    `json:"size"`
}

type githubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Body        string        `json:"body"`         // markdown release notes
	HTMLURL     string        `json:"html_url"`     // release page
	PublishedAt string        `json:"published_at"` // RFC3339
	Assets      []githubAsset `json:"assets"`
}

// checksumURL returns the browser_download_url for checksums.txt, or "" if the
// release doesn't carry one.
func (r githubRelease) checksumURL() string {
	for _, a := range r.Assets {
		if a.Name == "checksums.txt" {
			return a.URL
		}
	}
	return ""
}

// fetchRelease fetches the latest release (or the one named by tag if non-empty).
func fetchRelease(ctx context.Context, client *http.Client, tag string) (githubRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)
	if tag != "" {
		// Tags are created as vX.Y.Z (see AGENTS.md), and GitHub's tags endpoint
		// needs the exact tag_name. Canonicalize so both "0.2.0" and "v0.2.0"
		// resolve to the v-prefixed tag.
		url = fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", githubRepo, canonicalSemver(tag))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return githubRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "kiro-anthropic/"+version)
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := client.Do(req)
	if err != nil {
		return githubRelease{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return githubRelease{}, fmt.Errorf("github api: %s: %s", resp.Status, readSnippet(resp.Body))
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return githubRelease{}, fmt.Errorf("decode release: %w", err)
	}
	if rel.TagName == "" {
		return githubRelease{}, errors.New("release has no tag_name")
	}
	return rel, nil
}

// listReleases fetches the repo's releases (newest first, up to 100). It uses
// the injected client, so it honors the process's configured HTTP proxy just
// like fetchRelease. Used by the admin update check to aggregate the notes of
// every release newer than the running version.
func listReleases(ctx context.Context, client *http.Client) ([]githubRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=100", githubRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "kiro-anthropic/"+version)
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github api: %s: %s", resp.Status, readSnippet(resp.Body))
	}

	var rels []githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, fmt.Errorf("decode releases: %w", err)
	}
	return rels, nil
}

// --- pure helpers (unit-tested) -------------------------------------------

// assetNameFor returns the release asset name for the given platform, matching
// build.sh's naming: kiro-anthropic_<tag>_<os>_<arch>.{tar.gz|zip}. tag should
// include its leading "v".
func assetNameFor(goos, goarch, tag string) string {
	bin := fmt.Sprintf("kiro-anthropic_%s_%s_%s", tag, goos, goarch)
	if goos == "windows" {
		return bin + ".zip"
	}
	return bin + ".tar.gz"
}

// pickAsset finds the asset matching this platform in the release. It prefers
// the exact build.sh name for the release tag, falling back to a platform
// suffix match if the tag differs from what we expect.
func pickAsset(assets []githubAsset, goos, goarch, tag string) (githubAsset, error) {
	if tag != "" {
		want := assetNameFor(goos, goarch, tag)
		for _, a := range assets {
			if a.Name == want {
				return a, nil
			}
		}
	}
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	suffix := fmt.Sprintf("_%s_%s%s", goos, goarch, ext)
	for _, a := range assets {
		if strings.HasSuffix(a.Name, suffix) {
			return a, nil
		}
	}
	return githubAsset{}, fmt.Errorf("no release asset for %s/%s (looked for %q)", goos, goarch, suffix)
}

// parseChecksums parses a `sha256sum`-style checksums.txt into name->hexsum.
func parseChecksums(data []byte) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// Binary-mode output (sha256sum -b / shasum -b) prefixes the name with
		// '*'; strip it so lookups by asset name match.
		name := strings.TrimPrefix(fields[1], "*")
		out[name] = fields[0]
	}
	return out
}

// verifySHA256 reports whether data hashes to the given hex digest.
func verifySHA256(data []byte, hexSum string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, hexSum) {
		return fmt.Errorf("archive checksum mismatch: expected %s, got %s", hexSum, got)
	}
	return nil
}

// extractBinary decompresses a release archive and returns the kiro-anthropic
// binary inside. Supports .tar.gz (unix) and .zip (windows).
func extractBinary(archive []byte, assetName string) ([]byte, error) {
	switch {
	case strings.HasSuffix(assetName, ".zip"):
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, fmt.Errorf("open zip: %w", err)
		}
		for _, f := range zr.File {
			if baseName(f.Name) == exeName() && !f.FileInfo().IsDir() {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return readCapped(rc)
			}
		}
		return nil, fmt.Errorf("kiro-anthropic binary not found in zip")
	default: // .tar.gz
		gr, err := gzip.NewReader(bytes.NewReader(archive))
		if err != nil {
			return nil, fmt.Errorf("open gzip: %w", err)
		}
		defer gr.Close()
		tr := tar.NewReader(gr)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			if h.Typeflag != tar.TypeReg {
				continue
			}
			if baseName(h.Name) == exeName() {
				return readCapped(tr)
			}
		}
		return nil, fmt.Errorf("kiro-anthropic binary not found in tarball")
	}
}

// readCapped reads all of r but errors past maxBinaryBytes, so a crafted
// archive can't expand into an OOM.
func readCapped(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBinaryBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBinaryBytes {
		return nil, fmt.Errorf("extracted binary exceeds %d bytes", maxBinaryBytes)
	}
	return b, nil
}

// download fetches a URL fully into memory (capped at upgradeMaxBytes).
func download(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "kiro-anthropic/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, upgradeMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > upgradeMaxBytes {
		return nil, fmt.Errorf("%s: response exceeds %d bytes", url, upgradeMaxBytes)
	}
	return body, nil
}

// --- small format/version helpers -----------------------------------------

func canonicalSemver(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

// compareVersions compares two version strings using semver. Inputs that are
// not canonical semver (e.g. "dev", "0.1.0-3-gabcdef") are treated as less than
// any release. Returns -1, 0, +1.
func compareVersions(a, b string) int {
	ca, cb := canonicalSemver(a), canonicalSemver(b)
	switch {
	case semver.IsValid(ca) && semver.IsValid(cb):
		return semver.Compare(ca, cb)
	case semver.IsValid(ca):
		return 1
	case semver.IsValid(cb):
		return -1
	default:
		return 0
	}
}

// newerReleases returns the releases strictly newer than current, sorted newest
// first. Non-semver releases (and everything when current itself is not semver,
// e.g. a dev build) are excluded, so a dev build never reports an update. Pure
// and side-effect free for unit testing.
func newerReleases(all []githubRelease, current string) []githubRelease {
	cur := canonicalSemver(current)
	if !semver.IsValid(cur) {
		return nil
	}
	out := make([]githubRelease, 0, len(all))
	for _, r := range all {
		tag := canonicalSemver(r.TagName)
		if semver.IsValid(tag) && semver.Compare(tag, cur) > 0 {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return compareVersions(out[i].TagName, out[j].TagName) > 0
	})
	return out
}

// selfExeLabel is a short, friendly label for the running binary path.
func selfExeLabel() string {
	exe, err := os.Executable()
	if err != nil {
		return "this binary"
	}
	return exe
}

func exeName() string {
	if runtime.GOOS == "windows" {
		return "kiro-anthropic.exe"
	}
	return "kiro-anthropic"
}

func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		path = path[i+1:]
	}
	if i := strings.LastIndexByte(path, '\\'); i >= 0 {
		path = path[i+1:]
	}
	return path
}

// confirm reads a y/yes answer from stdin.
func confirm() bool {
	var resp string
	if _, err := fmt.Fscanln(os.Stdin, &resp); err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(resp)) {
	case "y", "yes":
		return true
	}
	return false
}
