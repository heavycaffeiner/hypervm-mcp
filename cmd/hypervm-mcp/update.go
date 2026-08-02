//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
)

const (
	// releaseRepo is where update looks for a newer build.
	releaseRepo   = "heavycaffeiner/hypervm-mcp"
	binaryAsset   = "hypervm-mcp.exe"
	checksumAsset = "hypervm-mcp.exe.sha256"

	// maxDownload bounds what a compromised or confused endpoint can make this
	// read into memory. The real binary is an order of magnitude under it.
	maxDownload = 128 << 20
)

// cmdUpdate installs the latest release over the current one.
//
// It is deliberately thin: everything that makes an install an install lives in
// `service install`, and an upgrade path that duplicated any of it would drift
// away from the one the installer uses.
func cmdUpdate() int {
	current, err := installedVersion()
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("Installed: %s\n", current)

	rel, err := latestRelease()
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("Latest:    %s\n", rel.TagName)

	if rel.TagName == current {
		fmt.Println("\nAlready up to date.")
		return 0
	}

	dir, err := os.MkdirTemp("", "hypervm-mcp-update-")
	if err != nil {
		return fail("create a download directory: %v", err)
	}
	defer os.RemoveAll(dir)

	exe := filepath.Join(dir, binaryAsset)
	if err := download(rel, exe); err != nil {
		return fail("%v", err)
	}

	// Running the downloaded binary rather than this one is the whole trick:
	// install stages whichever executable performs it, so this is what makes
	// the new version the installed one. It raises the usual UAC prompt, stops
	// the service, replaces the staged copy and starts it again. Stored
	// credentials, pinned host keys and tunnels live in the data directory,
	// which install does not touch.
	fmt.Printf("\nInstalling %s\n", rel.TagName)
	cmd := exec.Command(exe, "service", "install")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fail("install %s: %v", rel.TagName, err)
	}

	fmt.Printf("\nUpdated %s to %s\n", current, rel.TagName)
	return 0
}

// installedVersion asks the installed binary what it is.
//
// Not this process's own version: update may well be run from a build that is
// not the one the service runs, and the answer that matters is the one on disk.
func installedVersion() (string, error) {
	path := config.BinaryPath()
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("nothing is installed at %s; run: hypervm-mcp service install", path)
	}
	out, err := exec.Command(path, "version").Output()
	if err != nil {
		return "", fmt.Errorf("ask %s for its version: %w", path, err)
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("%s reported no version", path)
	}
	return v, nil
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func latestRelease() (*ghRelease, error) {
	body, err := fetch("https://api.github.com/repos/"+releaseRepo+"/releases/latest", 4<<20)
	if err != nil {
		return nil, err
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("parse GitHub's answer: %w", err)
	}
	if rel.TagName == "" {
		return nil, errors.New("GitHub's answer named no release")
	}
	return &rel, nil
}

// asset finds a published file's download URL.
func (r *ghRelease) asset(name string) (string, error) {
	for _, a := range r.Assets {
		if a.Name != name {
			continue
		}
		// This URL decides what ends up running as LocalSystem, so it is checked
		// rather than followed on trust.
		if u, err := url.Parse(a.URL); err != nil || u.Scheme != "https" {
			return "", fmt.Errorf("release %s offers %s over something other than https", r.TagName, name)
		}
		return a.URL, nil
	}
	return "", fmt.Errorf("release %s publishes no %s", r.TagName, name)
}

// download fetches the release binary and refuses to keep one whose SHA-256 is
// not what the release published. Every release publishes a checksum, so a
// missing one is a reason to stop rather than to shrug: this file is about to
// be run as LocalSystem.
func download(rel *ghRelease, dst string) error {
	binURL, err := rel.asset(binaryAsset)
	if err != nil {
		return err
	}
	sumURL, err := rel.asset(checksumAsset)
	if err != nil {
		return err
	}

	sum, err := fetch(sumURL, 4<<10)
	if err != nil {
		return err
	}

	fmt.Printf("Downloading %s\n", rel.TagName)
	body, err := fetch(binURL, maxDownload)
	if err != nil {
		return err
	}
	got, err := checkDigest(body, sum)
	if err != nil {
		return err
	}
	fmt.Printf("Checksum verified: %s\n", got)

	// 0o600 rather than the usual temp-file permissions: nobody else needs to
	// be able to swap this out between here and the install that runs it.
	if err := os.WriteFile(dst, body, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

// checkDigest reports the SHA-256 of body once it matches what the release
// published, and refuses it otherwise. sumFile is the published checksum file,
// which holds "<hex>  <filename>".
func checkDigest(body, sumFile []byte) (string, error) {
	fields := strings.Fields(string(sumFile))
	if len(fields) == 0 {
		return "", fmt.Errorf("%s is empty", checksumAsset)
	}
	want := strings.ToLower(fields[0])
	if _, err := hex.DecodeString(want); err != nil || len(want) != sha256.Size*2 {
		return "", fmt.Errorf("%s holds %q, which is not a SHA-256", checksumAsset, fields[0])
	}

	got := fmt.Sprintf("%x", sha256.Sum256(body))
	if got != want {
		return "", fmt.Errorf("checksum mismatch: the download hashes to %s, the release published %s",
			got, want)
	}
	return got, nil
}

// fetch reads a URL into memory, refusing anything past limit bytes.
func fetch(rawURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "hypervm-mcp-update")

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: GitHub answered %s", rawURL, resp.Status)
	}

	// One byte past the limit, so an oversized body is caught rather than
	// silently truncated into a checksum mismatch that explains nothing.
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rawURL, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s is larger than the %d bytes expected of it", rawURL, limit)
	}
	return body, nil
}
