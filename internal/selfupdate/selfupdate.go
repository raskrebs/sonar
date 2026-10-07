package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const repo = "raskrebs/sonar"

// Set via -ldflags at build time.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type githubRelease struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

// VersionString returns the formatted version string.
func VersionString() string {
	return fmt.Sprintf("sonar %s (%s/%s)", Version, runtime.GOOS, runtime.GOARCH)
}

// LatestTag is the tag of the newest published release, fetched under the
// caller's context so a diagnosis with a deadline (`sonar doctor`) can give up
// rather than hang on a network that is not there.
func LatestTag(ctx context.Context) (string, error) {
	release, err := fetchLatestRelease(ctx)
	if err != nil {
		return "", err
	}
	return release.TagName, nil
}

// FetchLatestRelease queries GitHub for the latest release.
func FetchLatestRelease() (*githubRelease, error) { return fetchLatestRelease(context.Background()) }

func fetchLatestRelease(ctx context.Context) (*githubRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to check for updates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %s", resp.Status)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("failed to parse release info: %w", err)
	}
	return &release, nil
}

// IsNewer returns true if the remote version is newer than the current one.
// Both versions are expected to be semver strings like "v0.1.3".
func IsNewer(current, remote string) bool {
	current = strings.TrimPrefix(current, "v")
	remote = strings.TrimPrefix(remote, "v")
	if current == "dev" || current == "" {
		return false
	}
	return remote != current && compareSemver(remote, current) > 0
}

// compareSemver returns >0 if a > b, <0 if a < b, 0 if equal.
func compareSemver(a, b string) int {
	pa := parseSemver(a)
	pb := parseSemver(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] - pb[i]
		}
	}
	return 0
}

func parseSemver(s string) [3]int {
	s = strings.TrimPrefix(s, "v")
	var parts [3]int
	fmt.Sscanf(s, "%d.%d.%d", &parts[0], &parts[1], &parts[2])
	return parts
}

// FindAssetURL returns the download URL for the current platform.
func FindAssetURL(release *githubRelease) (string, error) {
	platform := fmt.Sprintf("%s_%s", runtime.GOOS, runtime.GOARCH)
	suffix := ".tar.gz"
	if runtime.GOOS == "windows" {
		suffix = ".zip"
	}
	for _, asset := range release.Assets {
		if strings.Contains(asset.Name, platform) && strings.HasSuffix(asset.Name, suffix) {
			return asset.BrowserDownloadURL, nil
		}
	}
	return "", fmt.Errorf("no binary found for %s in release %s", platform, release.TagName)
}

// DownloadAndReplace downloads the tarball and replaces the current binary.
func DownloadAndReplace(downloadURL string) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine binary path: %w", err)
	}

	// Check the binary is writable
	if err := checkWritable(execPath); err != nil {
		return err
	}

	// Download the tarball
	resp, err := http.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %s", resp.Status)
	}

	// Write to a temp file first
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	tmpFile, err := os.CreateTemp("", "sonar-update-*"+ext)
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		tmpFile.Close()
		return fmt.Errorf("download failed: %w", err)
	}
	tmpFile.Close()

	// Extract the binary from the tarball
	binary, err := extractBinary(tmpPath)
	if err != nil {
		return err
	}
	defer os.Remove(binary)

	// Replace the current binary
	if err := replaceBinary(execPath, binary); err != nil {
		return err
	}

	return nil
}

// checkWritable reports whether the update can replace the binary at path.
//
// The replacement is a rename inside the binary's directory (replaceBinary), so
// the directory is what has to be writable — not the file. Opening the file for
// writing is the wrong question as well as an unnecessary one: Linux refuses it
// with "text file busy" for any executable that is running, and the binary
// being updated is the one asking.
func checkWritable(path string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".sonar-write-test-*")
	if err != nil {
		return fmt.Errorf("binary directory %s is not writable: %w", dir, err)
	}
	tmp.Close()
	os.Remove(tmp.Name())
	return nil
}

// IsHomebrew returns true if the binary appears to be installed via Homebrew.
func IsHomebrew() bool {
	execPath, err := os.Executable()
	if err != nil {
		return false
	}
	return strings.Contains(execPath, "Cellar") || strings.Contains(execPath, "homebrew")
}
