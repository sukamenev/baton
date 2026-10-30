package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	latestVersionURL  = "https://raw.githubusercontent.com/sukamenev/baton/main/latest.txt"
	githubReleaseBase = "https://github.com/sukamenev/baton/releases/download"
	goPackageSpec     = "github.com/sukamenev/baton@latest"
)

type updateState struct {
	LastCheckUnix   int64  `json:"last_check_unix"`
	LatestAvailable string `json:"latest_available,omitempty"`
	Version         string `json:"version"`
}

func batonStateDir() string {
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "baton")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "baton")
}

func batonConfigDir() string {
	if env := os.Getenv("BATON_CONFIG_DIR"); env != "" {
		return env
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "baton")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "baton")
}

// maybeTriggerQuietUpdate checks cached update state on normal CLI invocations.
// If a newer version was discovered by a previous check, it prints a one-line
// notice to stderr. If 24 hours have passed since the last check, it spawns a
// short-lived detached process (`baton update --check-quiet`) that checks
// latest.txt and exits silently if offline.
func maybeTriggerQuietUpdate() {
	if os.Getenv("BATON_NO_AUTO_UPDATE") == "1" {
		return
	}
	stateDir := batonStateDir()
	_ = os.MkdirAll(stateDir, 0755)
	stateFile := filepath.Join(stateDir, "update_state.json")

	var st updateState
	if data, err := os.ReadFile(stateFile); err == nil {
		_ = json.Unmarshal(data, &st)
	}

	if st.Version != version {
		_ = syncSkillsSilent()
		st.Version = version
	}

	if st.LatestAvailable != "" && isNewerVersion(st.LatestAvailable, version) {
		fmt.Fprintf(os.Stderr, "[baton] Update available: %s -> %s. Run 'baton update' to upgrade.\n", version, st.LatestAvailable)
	}

	now := time.Now().Unix()
	if now-st.LastCheckUnix < 24*3600 {
		return
	}
	st.LastCheckUnix = now
	saveUpdateState(stateFile, st)

	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "update", "--check-quiet")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	_ = cmd.Start()
}

// runQuietCheckAndExit fetches latest.txt with a 3-second timeout and records
// the latest version in update_state.json. It only performs an automatic
// background upgrade if the user explicitly opted in via BATON_AUTO_UPDATE=1.
// If offline or already up to date, it exits silently with zero output.
func runQuietCheckAndExit() {
	remoteVer, err := fetchLatestVersion()
	if err != nil || remoteVer == "" {
		return
	}

	stateDir := batonStateDir()
	_ = os.MkdirAll(stateDir, 0755)
	stateFile := filepath.Join(stateDir, "update_state.json")

	var st updateState
	if data, err := os.ReadFile(stateFile); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	st.LatestAvailable = remoteVer
	st.LastCheckUnix = time.Now().Unix()
	saveUpdateState(stateFile, st)

	if os.Getenv("BATON_AUTO_UPDATE") == "1" && isNewerVersion(remoteVer, version) {
		_, _ = runSelfUpdate()
	}
}

func saveUpdateState(path string, st updateState) {
	if b, err := json.MarshalIndent(st, "", "  "); err == nil {
		_ = os.WriteFile(path, b, 0644)
	}
}

func fetchLatestVersion() (string, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest("GET", latestVersionURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "baton-updater/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(strings.TrimSpace(string(body)), "v"), nil
}

// isNewerVersion returns true if remote (e.g. "0.3.0") is strictly greater
// than local (e.g. "0.2.0").
func isNewerVersion(remote, local string) bool {
	rParts := parseSemverParts(remote)
	lParts := parseSemverParts(local)
	if len(rParts) == 0 || len(lParts) == 0 {
		return false
	}
	for i := 0; i < 3; i++ {
		if rParts[i] > lParts[i] {
			return true
		}
		if rParts[i] < lParts[i] {
			return false
		}
	}
	return false
}

func parseSemverParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		v = v[:idx]
	}
	fields := strings.Split(v, ".")
	if len(fields) == 0 {
		return nil
	}
	res := []int{0, 0, 0}
	for i := 0; i < len(fields) && i < 3; i++ {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			return nil
		}
		res[i] = n
	}
	return res
}

// releaseAssetName returns the platform-specific binary asset name in GitHub Releases
// for the current OS and CPU architecture (e.g. baton_linux_amd64, baton_darwin_arm64,
// baton_windows_amd64.exe).
func releaseAssetName(goos, goarch string) string {
	name := fmt.Sprintf("baton_%s_%s", goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// runSelfUpdate performs a two-stage update of the running executable:
//  1. If the Go compiler is available, it builds from source via `go install`
//     (verified against Go's checksum transparency database sum.golang.org),
//     with GOBIN pointed at the directory of the running binary so the new
//     build replaces it instead of landing in ~/go/bin.
//  2. Otherwise (or if stage 1 did not produce the expected version), it
//     downloads the prebuilt binary for runtime.GOOS/runtime.GOARCH from GitHub
//     Releases, verifies its SHA-256 against checksums.txt, and atomically
//     replaces the executable.
//
// Each stage is verified by running `<binary> version`. Finally, the NEW binary
// runs `baton install`, because SKILL.md and /baton are embedded into the binary
// and this old process only carries the old texts.
func runSelfUpdate() (string, error) {
	var sb strings.Builder
	remoteVer, err := fetchLatestVersion()
	if err != nil || remoteVer == "" {
		return "", fmt.Errorf("cannot check the latest version at %s: %v", latestVersionURL, err)
	}
	if !isNewerVersion(remoteVer, version) {
		_ = syncSkillsSilent()
		return fmt.Sprintf("baton %s is already up to date (latest.txt: %s). Refreshed SKILL.md across agents.\n", version, remoteVer), nil
	}
	sb.WriteString(fmt.Sprintf("Updating baton: %s -> %s (%s/%s)\n", version, remoteVer, runtime.GOOS, runtime.GOARCH))

	exePath := resolveBinaryPath()
	updatedBinary := false

	// Stage 1: Prefer compiling from auditable source via `go install` if Go is installed.
	// `go install` names the binary after the module ("baton"), so it can only replace
	// the running executable if that is what it is called.
	if _, err := exec.LookPath("go"); err == nil && isBatonExecutableName(exePath) {
		cmd := exec.Command("go", "install", goPackageSpec)
		cmd.Env = append(os.Environ(), "GOBIN="+filepath.Dir(exePath))
		if out, err := cmd.CombinedOutput(); err != nil {
			sb.WriteString(fmt.Sprintf("[INFO] `go install` failed, trying the release binary (%s)\n", strings.TrimSpace(string(out))))
		} else if got := installedBinaryVersion(exePath); got != remoteVer {
			// proxy.golang.org may lag behind a fresh tag for a few minutes
			sb.WriteString(fmt.Sprintf("[INFO] `go install` produced version %q instead of %s, trying the release binary\n", got, remoteVer))
		} else {
			sb.WriteString(fmt.Sprintf("[OK] Rebuilt %s from source via `go install %s` (verified via sum.golang.org)\n", exePath, goPackageSpec))
			updatedBinary = true
		}
	}

	// Stage 2: Download prebuilt OS/Arch binary from GitHub Releases and verify SHA-256
	if !updatedBinary {
		if err := downloadAndVerifyReleaseBinary(remoteVer); err != nil {
			sb.WriteString(fmt.Sprintf("[FAIL] Release binary download failed (%v)\n", err))
		} else if got := installedBinaryVersion(exePath); got != remoteVer {
			sb.WriteString(fmt.Sprintf("[FAIL] Installed release binary reports version %q instead of %s\n", got, remoteVer))
		} else {
			sb.WriteString(fmt.Sprintf("[OK] Downloaded and SHA-256 verified release binary `%s` (v%s) into %s\n",
				releaseAssetName(runtime.GOOS, runtime.GOARCH), remoteVer, exePath))
			updatedBinary = true
		}
	}

	if !updatedBinary {
		return sb.String(), fmt.Errorf("baton was NOT updated and is still %s (see the reasons above; if the release was tagged just now, retry in a few minutes)", version)
	}

	// The new binary carries the new embedded SKILL.md and /baton texts.
	cmd := exec.Command(exePath, "install")
	cmd.Env = append(os.Environ(), "BATON_NO_AUTO_UPDATE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		return sb.String(), fmt.Errorf("baton %s installed, but refreshing SKILL.md failed: %v (%s)", remoteVer, err, strings.TrimSpace(string(out)))
	}
	sb.WriteString(fmt.Sprintf("[OK] baton %s refreshed SKILL.md and /baton slash command across all installed agents.\n", remoteVer))
	return sb.String(), nil
}

func isBatonExecutableName(exePath string) bool {
	base := strings.ToLower(filepath.Base(exePath))
	return base == "baton" || base == "baton.exe"
}

// installedBinaryVersion runs `<exePath> version` and returns the reported
// version ("0.3.5"), or "" if the binary cannot be executed.
func installedBinaryVersion(exePath string) string {
	cmd := exec.Command(exePath, "version")
	cmd.Env = append(os.Environ(), "BATON_NO_AUTO_UPDATE=1")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return ""
	}
	return strings.TrimPrefix(fields[1], "v")
}

func downloadAndVerifyReleaseBinary(ver string) error {
	ver = strings.TrimPrefix(strings.TrimSpace(ver), "v")
	asset := releaseAssetName(runtime.GOOS, runtime.GOARCH)
	baseURL := fmt.Sprintf("%s/v%s", githubReleaseBase, ver)

	checksumsRaw, err := downloadBytesWithLimit(baseURL+"/checksums.txt", 64*1024)
	if err != nil {
		return fmt.Errorf("fetch checksums.txt: %w", err)
	}

	expectedSHA := parseExpectedSHA256(string(checksumsRaw), asset)
	if expectedSHA == "" {
		return fmt.Errorf("asset %s not found in checksums.txt", asset)
	}

	binData, err := downloadBytesWithLimit(baseURL+"/"+asset, 32*1024*1024)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", asset, err)
	}

	actualSum := sha256.Sum256(binData)
	actualSHA := hex.EncodeToString(actualSum[:])
	if !strings.EqualFold(actualSHA, expectedSHA) {
		return fmt.Errorf("SHA-256 mismatch for %s: expected %s, got %s", asset, expectedSHA, actualSHA)
	}

	exePath := resolveBinaryPath()
	tmpPath := exePath + ".new"
	if err := os.WriteFile(tmpPath, binData, 0755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		oldPath := exePath + ".old"
		_ = os.Remove(oldPath)
		_ = os.Rename(exePath, oldPath)
	}
	return os.Rename(tmpPath, exePath)
}

func parseExpectedSHA256(checksumsText, assetName string) string {
	for _, line := range strings.Split(checksumsText, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 {
			fileField := strings.TrimPrefix(fields[1], "*")
			if filepath.Base(fileField) == assetName {
				return strings.ToLower(fields[0])
			}
		}
	}
	return ""
}

func downloadBytesWithLimit(url string, maxBytes int64) ([]byte, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBytes))
}
