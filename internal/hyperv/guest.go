package hyperv

import (
	"context"
	"net/netip"
	"strings"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
)

// guestIPPollInterval is how often WaitForGuestIP re-asks Hyper-V. Each poll
// spawns a PowerShell process (~200-400ms), so polling faster than this mostly
// burns CPU.
const guestIPPollInterval = 2 * time.Second

// noAddressHint names the usual cause. Hyper-V does not discover guest addresses
// itself — it reads what a guest agent reports over the Data Exchange channel.
// Minimal Linux installs leave that agent out, so a perfectly healthy VM with a
// working DHCP lease reports nothing at all.
const noAddressHint = "The guest reported no addresses. Hyper-V learns them from an agent " +
	"inside the guest, so a minimal Linux install reports nothing even when its network works: " +
	"install hyperv-daemons (RHEL, Rocky, Fedora) or linux-virtual/hyperv-daemons (Debian, Ubuntu) " +
	"and enable hypervkvpd. Otherwise check that the adapter is connected to a switch. " +
	"Tools that need to reach the guest accept an explicit host, so this is not a dead end."

// GetNetworkAdapters returns a VM's virtual NICs along with whatever addresses
// the guest has reported through integration services.
func (c *Client) GetNetworkAdapters(ctx context.Context, name string) ([]NetworkAdapter, error) {
	if name == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	const script = requireVM + `
    $result = @(Get-VMNetworkAdapter -VM $vm | ` + adapterProjection + `)`

	out := []NetworkAdapter{}
	if err := c.r.RunInto(ctx, script, map[string]any{"name": name}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// WaitForGuestIP polls until the guest reports a usable address or the deadline
// passes.
//
// This exists because Hyper-V reports a VM as Running long before its guest
// network stack is up; anything that dials the guest must wait for this instead
// of for the Running state.
//
// subnet, if given as CIDR, restricts the wait to an address in that range —
// useful when a VM sits on both a NAT switch and the physical LAN.
//
// allowLinkLocal accepts a 169.254.x.x address, which is normally a sign that
// DHCP has not finished — but is the only address a guest will ever have on an
// Internal or Private switch, where nothing hands addresses out.
func (c *Client) WaitForGuestIP(ctx context.Context, name, subnet string, allowLinkLocal bool, timeout time.Duration) (*GuestIPResult, error) {
	if name == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	var prefix netip.Prefix
	if subnet != "" {
		p, err := netip.ParsePrefix(subnet)
		if err != nil {
			return nil, hverr.Wrap(hverr.InvalidArgument, err, "subnet %q is not valid CIDR", subnet)
		}
		prefix = p
	}

	started := time.Now()
	deadline := started.Add(timeout)

	var lastAll []string
	for {
		adapters, err := c.GetNetworkAdapters(ctx, name)
		if err != nil {
			return nil, err
		}
		addr, all := pickGuestIP(adapters, prefix, allowLinkLocal)
		lastAll = all
		if addr != "" {
			return &GuestIPResult{
				Address:       addr,
				AllAddresses:  all,
				WaitedSeconds: time.Since(started).Seconds(),
			}, nil
		}

		if time.Now().Add(guestIPPollInterval).After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, hverr.Wrap(hverr.OperationTimeout, ctx.Err(), "cancelled while waiting for a guest IP")
		case <-time.After(guestIPPollInterval):
		}
	}

	e := hverr.New(hverr.GuestIPUnavailable,
		"%q reported no usable IP address within %s", name, timeout)
	if len(lastAll) > 0 {
		e = e.WithDetail("addresses seen: " + strings.Join(lastAll, ", "))
	} else {
		e = e.WithDetail(noAddressHint)
	}
	return nil, e
}

// ResolveGuestIP returns the address a tunnel or SSH client should dial right
// now. It does not wait: callers that just started the VM should use
// WaitForGuestIP instead.
func (c *Client) ResolveGuestIP(ctx context.Context, name string) (string, []string, error) {
	adapters, err := c.GetNetworkAdapters(ctx, name)
	if err != nil {
		return "", nil, err
	}
	addr, all := pickGuestIP(adapters, netip.Prefix{}, false)
	if addr == "" {
		return "", all, hverr.New(hverr.GuestIPUnavailable,
			"%q is not reporting a usable IP address", name)
	}
	return addr, all, nil
}

// pickGuestIP chooses one address out of everything the guest reported, and
// returns the full list alongside it.
//
// Preference is routable IPv4, then routable IPv6, then — only when asked for —
// link-local. Loopback is never usable from the host.
//
// Link-local is last rather than merely allowed: a guest can hold one alongside
// a real address while DHCP settles, and returning the link-local one then would
// be a worse answer than waiting a moment for the other.
func pickGuestIP(adapters []NetworkAdapter, prefix netip.Prefix, allowLinkLocal bool) (string, []string) {
	var all []string
	var bestV4, bestV6, bestLinkLocal string

	for _, a := range adapters {
		for _, raw := range a.IPAddresses {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			all = append(all, raw)

			ip, err := netip.ParseAddr(raw)
			if err != nil || ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			if prefix.IsValid() && !prefix.Contains(ip) {
				continue
			}
			switch {
			case ip.IsLinkLocalUnicast():
				if allowLinkLocal && bestLinkLocal == "" && ip.Is4() {
					bestLinkLocal = raw
				}
			case ip.Is4():
				if bestV4 == "" {
					bestV4 = raw
				}
			default:
				if bestV6 == "" {
					bestV6 = raw
				}
			}
		}
	}

	switch {
	case bestV4 != "":
		return bestV4, all
	case bestV6 != "":
		return bestV6, all
	default:
		return bestLinkLocal, all
	}
}
