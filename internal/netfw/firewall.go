//go:build windows

// Package netfw manages the Windows Firewall rules a tunnel needs when it binds
// anything other than loopback.
package netfw

import (
	"context"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/psrun"
)

// Firewall creates and removes inbound allow rules.
type Firewall struct {
	r *psrun.Runner
}

func New(r *psrun.Runner) *Firewall { return &Firewall{r: r} }

// Allow opens an inbound TCP port.
//
// addresses narrows the rule to specific local addresses, so a tunnel bound to
// the host's Tailscale address is not also exposed on whatever other network the
// host happens to be attached to. An empty list allows every local address.
func (f *Firewall) Allow(ctx context.Context, ruleName string, port int, addresses []string) error {
	const script = `
    # Rules are named after the tunnel, so removal can target exactly this one.
    Remove-NetFirewallRule -Name $P.rule -ErrorAction SilentlyContinue
    $args = @{
        Name        = $P.rule
        # The rule name already carries the instance prefix, so it identifies the
        # owner on its own.
        DisplayName = 'tunnel ' + $P.rule
        Direction   = 'Inbound'
        Action      = 'Allow'
        Protocol    = 'TCP'
        LocalPort   = [int]$P.port
        Profile     = 'Any'
    }
    if ($P.addresses -and @($P.addresses).Count -gt 0) { $args['LocalAddress'] = @($P.addresses) }
    New-NetFirewallRule @args | Out-Null
    $result = $true`

	_, err := f.r.RunTimeout(ctx, 60*time.Second, script, map[string]any{
		"rule":      ruleName,
		"port":      port,
		"addresses": addresses,
	})
	if err != nil {
		return hverr.Wrap(hverr.FirewallError, err, "could not create firewall rule %s", ruleName)
	}
	return nil
}

// Remove deletes a rule by name. Removing one that is already gone succeeds.
func (f *Firewall) Remove(ctx context.Context, ruleName string) error {
	const script = `
    Remove-NetFirewallRule -Name $P.rule -ErrorAction SilentlyContinue
    $result = $true`

	if _, err := f.r.RunTimeout(ctx, 60*time.Second, script, map[string]any{"rule": ruleName}); err != nil {
		return hverr.Wrap(hverr.FirewallError, err, "could not remove firewall rule %s", ruleName)
	}
	return nil
}
