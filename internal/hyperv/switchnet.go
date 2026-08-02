package hyperv

import (
	"context"
	"net/netip"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
)

// SwitchNetwork is the host's side of a virtual switch.
type SwitchNetwork struct {
	SwitchName  string   `json:"switch_name"`
	SwitchType  string   `json:"switch_type"`
	AdapterName string   `json:"adapter_name,omitempty"`
	Addresses   []string `json:"addresses"`
	NATName     string   `json:"nat_name,omitempty"`
	NATPrefix   string   `json:"nat_prefix,omitempty"`
	Notes       []string `json:"notes,omitempty"`
}

// SetSwitchHostAddress gives the host an address on an Internal switch.
//
// This is the piece that makes a hand-made Internal switch usable. Hyper-V's
// Default Switch comes with an address, NAT and DHCP already arranged, which
// hides how much of that is normally your job: a switch you create yourself
// arrives with none of it, and its host adapter sits at a link-local address
// until told otherwise.
//
// A Private switch has no host adapter at all — that is what makes it private —
// so this is refused for one rather than failing later with something obscure.
func (c *Client) SetSwitchHostAddress(ctx context.Context, switchName, address string, prefixLength int, remove bool) (*SwitchNetwork, error) {
	if switchName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "switch_name is required")
	}
	if !remove {
		addr, err := netip.ParseAddr(address)
		if err != nil || !addr.Is4() {
			return nil, hverr.New(hverr.InvalidArgument, "address %q is not an IPv4 address", address)
		}
		if prefixLength < 1 || prefixLength > 32 {
			return nil, hverr.New(hverr.InvalidArgument, "prefix_length must be between 1 and 32")
		}
	}

	const script = `
    $sw = Get-VMSwitch -Name $P.switch -ErrorAction SilentlyContinue
    if (-not $sw) { throw "HVERR:SWITCH_NOT_FOUND|no switch named '$($P.switch)'" }
    if ($sw.SwitchType -eq 'Private') {
        throw "HVERR:INVALID_ARGUMENT|'$($P.switch)' is a Private switch, which by definition has no host adapter. Use an Internal switch if the host should take part."
    }

    $alias = 'vEthernet (' + $sw.Name + ')'
    $nic = Get-NetAdapter -Name $alias -ErrorAction SilentlyContinue
    if (-not $nic) {
        throw "HVERR:ADAPTER_NOT_FOUND|the host has no adapter '$alias'; the switch may have only just been created"
    }

    # Replace rather than add: two addresses on one adapter is rarely what
    # anyone means, and leaves routing ambiguous.
    Remove-NetIPAddress -InterfaceIndex $nic.ifIndex -AddressFamily IPv4 -Confirm:$false -ErrorAction SilentlyContinue
    Remove-NetRoute -InterfaceIndex $nic.ifIndex -AddressFamily IPv4 -Confirm:$false -ErrorAction SilentlyContinue
    if (-not $P.remove) {
        New-NetIPAddress -InterfaceIndex $nic.ifIndex -IPAddress $P.address -PrefixLength ([int]$P.prefix) | Out-Null
    }
` + switchNetworkProjection

	var out SwitchNetwork
	err := c.r.RunTimeoutInto(ctx, 2*time.Minute, script, map[string]any{
		"switch": switchName, "address": address, "prefix": prefixLength, "remove": remove,
		"nat_prefix": config.ResourcePrefix(),
	}, &out)
	if err != nil {
		return nil, err
	}

	if !remove {
		out.Notes = append(out.Notes,
			"Guests on this switch still need addresses of their own: an Internal switch has no DHCP "+
				"server. Set them with set_guest_static_ip, or run a DHCP server on one of the guests.")
		if out.NATName == "" {
			out.Notes = append(out.Notes,
				"Guests can reach the host and each other, but not the internet. Add that with "+
					"set_switch_nat if they need it.")
		}
	}
	return &out, nil
}

// switchNetworkProjection fills $result from $P.switch.
const switchNetworkProjection = `
    $sw = Get-VMSwitch -Name $P.switch
    $alias = 'vEthernet (' + $sw.Name + ')'
    $nic = Get-NetAdapter -Name $alias -ErrorAction SilentlyContinue
    $addrs = @()
    if ($nic) {
        $addrs = @(Get-NetIPAddress -InterfaceIndex $nic.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue |
                   ForEach-Object { $_.IPAddress + '/' + $_.PrefixLength })
    }

    # A NAT is defined by prefix and holds no reference to a switch, so there is
    # no reliable way to ask Windows which switch one belongs to. Report the one
    # this server manages for this switch, found by its name.
    $natName = ''; $natPrefix = ''
    $managed = Get-NetNat -Name ($P.nat_prefix + '-' + $sw.Name) -ErrorAction SilentlyContinue
    if ($managed) {
        $natName = $managed.Name
        $natPrefix = [string]$managed.InternalIPInterfaceAddressPrefix
    }

    $result = [ordered]@{
        switch_name  = $sw.Name
        switch_type  = $sw.SwitchType.ToString()
        adapter_name = [string]$(if ($nic) { $nic.Name } else { '' })
        addresses    = $addrs
        nat_name     = $natName
        nat_prefix   = $natPrefix
    }`

// SetSwitchNAT gives guests on an Internal switch a route out through the host.
//
// Windows NAT is defined by an address prefix rather than by an interface, so
// two NATs with overlapping prefixes conflict — the error says which one is in
// the way rather than leaving you to find it.
func (c *Client) SetSwitchNAT(ctx context.Context, switchName, natName, prefix string, enable bool) (*SwitchNetwork, error) {
	if switchName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "switch_name is required")
	}
	if natName == "" {
		natName = config.ResourcePrefix() + "-" + switchName
	}
	if enable {
		p, err := netip.ParsePrefix(prefix)
		if err != nil {
			return nil, hverr.New(hverr.InvalidArgument, "prefix %q is not valid CIDR", prefix)
		}
		if !p.Addr().Is4() {
			return nil, hverr.New(hverr.InvalidArgument, "prefix must be IPv4")
		}
	}

	const script = `
    $sw = Get-VMSwitch -Name $P.switch -ErrorAction SilentlyContinue
    if (-not $sw) { throw "HVERR:SWITCH_NOT_FOUND|no switch named '$($P.switch)'" }

    $existing = Get-NetNat -Name $P.nat_name -ErrorAction SilentlyContinue
    if ($existing) { Remove-NetNat -Name $P.nat_name -Confirm:$false }

    if ($P.enable) {
        # NAT needs the host to already hold an address inside the prefix; without
        # one there is no interface for it to translate on.
        $alias = 'vEthernet (' + $sw.Name + ')'
        $nic = Get-NetAdapter -Name $alias -ErrorAction SilentlyContinue
        if (-not $nic) {
            throw "HVERR:ADAPTER_NOT_FOUND|the host has no adapter '$alias'"
        }
        if (-not (Get-NetIPAddress -InterfaceIndex $nic.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue |
                  Where-Object { $_.PrefixOrigin -ne 'WellKnown' })) {
            throw "HVERR:INVALID_ARGUMENT|'$alias' has no address yet. Call set_switch_host_address first, with an address inside $($P.prefix)."
        }
        try {
            New-NetNat -Name $P.nat_name -InternalIPInterfaceAddressPrefix $P.prefix -ErrorAction Stop | Out-Null
        } catch {
            $others = @(Get-NetNat | ForEach-Object { $_.Name + ' (' + $_.InternalIPInterfaceAddressPrefix + ')' }) -join ', '
            throw "HVERR:INVALID_ARGUMENT|could not create the NAT: $($_.Exception.Message). Existing NATs: $others"
        }
    }
` + switchNetworkProjection

	var out SwitchNetwork
	err := c.r.RunTimeoutInto(ctx, 2*time.Minute, script, map[string]any{
		"switch": switchName, "nat_name": natName, "prefix": prefix, "enable": enable,
		"nat_prefix": config.ResourcePrefix(),
	}, &out)
	if err != nil {
		return nil, err
	}

	if enable {
		out.Notes = append(out.Notes,
			"NAT gives guests a way out, not a way in: connections start from the guest. To reach a "+
				"guest service from elsewhere, open a tunnel.",
			"There is still no DHCP on this switch. Guests need a static address inside "+prefix+
				", with the host's address as their gateway.")
	}
	return &out, nil
}
