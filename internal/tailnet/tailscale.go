//go:build windows

// Package tailnet reads the host's Tailscale state so tunnels can bind the
// addresses tailnet peers can reach.
package tailnet

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
)

const statusTimeout = 10 * time.Second

// Status is what the CLI reports about this host.
type Status struct {
	Installed      bool     `json:"installed"`
	BackendState   string   `json:"backend_state"` // Running | NeedsLogin | Stopped | ...
	Addresses      []string `json:"addresses"`
	MagicDNSName   string   `json:"magic_dns_name,omitempty"`
	TailnetName    string   `json:"tailnet_name,omitempty"`
	ExposedTunnels []string `json:"exposed_tunnels"`
}

// Client reads Tailscale state through its CLI.
//
// The CLI is used rather than enumerating interfaces for 100.64.0.0/10, because
// that range is shared with other CGNAT users and, more importantly, an
// interface address says nothing about whether the backend is logged in.
type Client struct {
	path string
}

// New locates tailscale.exe, preferring a configured path.
func New(configured string) *Client { return &Client{path: locate(configured)} }

func locate(configured string) string {
	if configured != "" {
		if _, err := os.Stat(configured); err == nil {
			return configured
		}
	}
	for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramW6432")} {
		if root == "" {
			continue
		}
		p := filepath.Join(root, "Tailscale", "tailscale.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("tailscale.exe"); err == nil {
		return p
	}
	return ""
}

type rawStatus struct {
	BackendState   string `json:"BackendState"`
	MagicDNSSuffix string `json:"MagicDNSSuffix"`
	Self           struct {
		DNSName      string   `json:"DNSName"`
		TailscaleIPs []string `json:"TailscaleIPs"`
	} `json:"Self"`
}

// Status reports the host's Tailscale state.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	if c.path == "" {
		return &Status{ExposedTunnels: []string{}}, hverr.New(hverr.TailscaleUnavailable,
			"tailscale.exe was not found").
			WithDetail(`Install Tailscale, or set "tailscale_path" in the service configuration.`)
	}

	runCtx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()

	out, err := exec.CommandContext(runCtx, c.path, "status", "--json").Output()
	if err != nil {
		return nil, hverr.Wrap(hverr.TailscaleUnavailable, err, "could not run tailscale status")
	}

	var raw rawStatus
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, hverr.Wrap(hverr.TailscaleUnavailable, err, "could not parse tailscale status")
	}

	s := &Status{
		Installed:      true,
		BackendState:   raw.BackendState,
		Addresses:      raw.Self.TailscaleIPs,
		MagicDNSName:   strings.TrimSuffix(raw.Self.DNSName, "."),
		TailnetName:    raw.MagicDNSSuffix,
		ExposedTunnels: []string{},
	}
	if s.Addresses == nil {
		s.Addresses = []string{}
	}
	return s, nil
}

// Serve puts Tailscale's HTTPS front end in front of a local port, so tailnet
// peers reach it at https://<host>.<tailnet>.ts.net<path> with a certificate
// Tailscale issues and renews.
//
// This is tailnet-internal only. Funnel, which would publish to the whole
// internet, is deliberately not wired up.
func (c *Client) Serve(ctx context.Context, httpsPort int, path string, localPort int) (string, error) {
	if c.path == "" {
		return "", hverr.New(hverr.TailscaleUnavailable, "tailscale.exe was not found")
	}
	status, err := c.Status(ctx)
	if err != nil {
		return "", err
	}
	if status.BackendState != "Running" {
		return "", hverr.New(hverr.TailscaleNotRunning,
			"Tailscale is not connected (state %q)", status.BackendState)
	}
	if path == "" {
		path = "/"
	}
	if httpsPort == 0 {
		httpsPort = 443
	}

	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	args := []string{"serve", "--bg",
		"--https=" + strconv.Itoa(httpsPort),
		"--set-path=" + path,
		"http://127.0.0.1:" + strconv.Itoa(localPort)}

	out, err := exec.CommandContext(runCtx, c.path, args...).CombinedOutput()
	if err != nil {
		return "", hverr.Wrap(hverr.TailscaleUnavailable, err, "tailscale serve failed").
			WithDetail(strings.TrimSpace(string(out)))
	}

	url := "https://" + status.MagicDNSName
	if httpsPort != 443 {
		url += ":" + strconv.Itoa(httpsPort)
	}
	return url + strings.TrimSuffix(path, "/"), nil
}

// ServeOff removes a serve mapping.
func (c *Client) ServeOff(ctx context.Context, httpsPort int, path string) error {
	if c.path == "" {
		return hverr.New(hverr.TailscaleUnavailable, "tailscale.exe was not found")
	}
	if path == "" {
		path = "/"
	}
	if httpsPort == 0 {
		httpsPort = 443
	}

	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(runCtx, c.path, "serve",
		"--https="+strconv.Itoa(httpsPort), "--set-path="+path, "off").CombinedOutput()
	if err != nil {
		return hverr.Wrap(hverr.TailscaleUnavailable, err, "tailscale serve off failed").
			WithDetail(strings.TrimSpace(string(out)))
	}
	return nil
}

// Addrs returns the addresses a tunnel should bind for tailnet reachability.
func (c *Client) Addrs(ctx context.Context) ([]string, error) {
	s, err := c.Status(ctx)
	if err != nil {
		return nil, err
	}
	if s.BackendState != "Running" {
		return nil, hverr.New(hverr.TailscaleNotRunning,
			"Tailscale is not connected (state %q)", s.BackendState).
			WithDetail("Run `tailscale up` and try again.")
	}
	return s.Addresses, nil
}
