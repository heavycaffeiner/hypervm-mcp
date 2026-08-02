//go:build windows

package main

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

// The published checksum decides whether a binary gets run as LocalSystem, so
// every way it can be wrong has to end in a refusal.
func TestCheckDigest(t *testing.T) {
	body := []byte("a release binary")
	digest := fmt.Sprintf("%x", sha256.Sum256(body))

	t.Run("accepts what the release published", func(t *testing.T) {
		got, err := checkDigest(body, []byte(digest+"  hypervm-mcp.exe\n"))
		if err != nil {
			t.Fatalf("rejected a matching download: %v", err)
		}
		if got != digest {
			t.Fatalf("reported %s, want %s", got, digest)
		}
	})

	t.Run("accepts an upper-case digest", func(t *testing.T) {
		if _, err := checkDigest(body, []byte(strings.ToUpper(digest))); err != nil {
			t.Fatalf("rejected a matching download: %v", err)
		}
	})

	// A digest that parses but belongs to something else is the case that
	// matters: a tampered binary served with a stale or forged checksum.
	other := fmt.Sprintf("%x", sha256.Sum256([]byte("something else")))
	for _, tc := range []struct {
		name string
		sum  string
	}{
		{"different content", other + "  hypervm-mcp.exe"},
		{"empty file", ""},
		{"whitespace only", "   \n\t "},
		{"not hex", strings.Repeat("z", 64) + "  hypervm-mcp.exe"},
		{"too short", digest[:63]},
		{"too long", digest + "ab"},
		{"a filename with no digest", "hypervm-mcp.exe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := checkDigest(body, []byte(tc.sum)); err == nil {
				t.Fatal("accepted a download it should have refused")
			}
		})
	}
}

func TestReleaseAsset(t *testing.T) {
	rel := &ghRelease{TagName: "v1.0.0"}
	rel.Assets = append(rel.Assets, struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}{Name: binaryAsset, URL: "https://example.invalid/hypervm-mcp.exe"})
	rel.Assets = append(rel.Assets, struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}{Name: "plain", URL: "http://example.invalid/hypervm-mcp.exe"})

	if _, err := rel.asset(binaryAsset); err != nil {
		t.Fatalf("rejected an https asset: %v", err)
	}
	if _, err := rel.asset("plain"); err == nil {
		t.Fatal("accepted an asset served over plain http")
	}
	if _, err := rel.asset(checksumAsset); err == nil {
		t.Fatal("invented an asset the release does not publish")
	}
}
