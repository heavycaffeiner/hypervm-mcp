package hyperv

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
)

// hostHeldPorts are ports Windows itself listens on. A guest service on any of
// them cannot be reached through a tunnel, which is the main reason to give a VM
// its own address on the physical LAN.
var hostHeldPorts = []int{135, 137, 139, 445, 3389, 5985, 5986}

// ListPhysicalAdapters returns the host's real network adapters, including
// whether each is already bound to an external switch.
func (c *Client) ListPhysicalAdapters(ctx context.Context) ([]PhysicalAdapter, error) {
	const script = `
    # Map each physical adapter's description to the switch bound to it, since
    # that is the field Get-VMSwitch matches on.
    $bound = @{}
    foreach ($s in @(Get-VMSwitch -SwitchType External -ErrorAction SilentlyContinue)) {
        if ($s.NetAdapterInterfaceDescription) { $bound[$s.NetAdapterInterfaceDescription] = $s.Name }
    }
    $result = @(Get-NetAdapter -Physical -ErrorAction SilentlyContinue | Sort-Object Name | ForEach-Object {
        $ips = @(Get-NetIPAddress -InterfaceIndex $_.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue |
                 ForEach-Object { $_.IPAddress })
        [ordered]@{
            name                  = $_.Name
            interface_description = [string]$_.InterfaceDescription
            status                = [string]$_.Status
            link_speed_mbps       = [int64]($_.Speed / 1000000)
            mac_address           = [string]$_.MacAddress
            # Wireless adapters bridge by proxying ARP rather than truly
            # bridging, so a guest on one often does not appear as its own host.
            is_wireless           = [bool]($_.PhysicalMediaType -match 'Native 802.11|Wireless')
            bound_to_switch       = [string]$bound[$_.InterfaceDescription]
            ip_addresses          = $ips
        }
    })`

	out := []PhysicalAdapter{}
	if err := c.r.RunInto(ctx, script, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PreflightExternalSwitch reports what binding an adapter to an External switch
// would do to this host.
//
// The dangerous cases are specific and checkable: a statically addressed adapter
// whose configuration may not migrate, the host's only uplink, a wireless
// adapter that will not truly bridge, and a firewall profile that will be
// reassessed for the new virtual adapter. Reporting which of them apply beats
// any amount of generic warning text.
func (c *Client) PreflightExternalSwitch(ctx context.Context, adapterName string) (*ExternalSwitchPreflight, error) {
	if adapterName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "net_adapter_name is required")
	}

	const script = `
    $nic = Get-NetAdapter -Name $P.adapter -ErrorAction SilentlyContinue
    if (-not $nic) { throw "HVERR:ADAPTER_NOT_FOUND|no network adapter named '$($P.adapter)'" }

    $up = @(Get-NetAdapter -Physical | Where-Object { $_.Status -eq 'Up' })
    $ipif = Get-NetIPInterface -InterfaceIndex $nic.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue
    $addrs = @(Get-NetIPAddress -InterfaceIndex $nic.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue |
               ForEach-Object { $_.IPAddress + '/' + $_.PrefixLength })
    $profile = Get-NetConnectionProfile -InterfaceIndex $nic.ifIndex -ErrorAction SilentlyContinue

    $bound = ''
    foreach ($s in @(Get-VMSwitch -SwitchType External -ErrorAction SilentlyContinue)) {
        if ($s.NetAdapterInterfaceDescription -eq $nic.InterfaceDescription) { $bound = $s.Name }
    }

    $result = [ordered]@{
        adapter_name    = $nic.Name
        adapter_status  = [string]$nic.Status
        is_wireless     = [bool]($nic.PhysicalMediaType -match 'Native 802.11|Wireless')
        is_only_uplink  = [bool]($up.Count -le 1)
        uses_dhcp       = [bool]($ipif -and $ipif.Dhcp -eq 'Enabled')
        addresses       = $addrs
        network_profile = [string]$(if ($profile) { $profile.NetworkCategory } else { '' })
        already_bound_to = $bound
        risks           = @()
    }`

	var out ExternalSwitchPreflight
	if err := c.r.RunTimeoutInto(ctx, 60*time.Second, script,
		map[string]any{"adapter": adapterName}, &out); err != nil {
		return nil, err
	}

	out.Risks = externalSwitchRisks(&out)
	return &out, nil
}

func externalSwitchRisks(p *ExternalSwitchPreflight) []string {
	risks := []string{
		"This host loses network connectivity for several seconds while Hyper-V rebinds " +
			p.AdapterName + ". Any remote session to this machine will drop, and transfers in " +
			"flight will fail.",
	}

	if !p.UsesDHCP {
		// The failure that actually strands machines: Hyper-V is supposed to move
		// a static configuration to the new virtual adapter and does not always.
		risks = append(risks, "SERIOUS: "+p.AdapterName+" is statically addressed. Hyper-V moves the "+
			"address to the new virtual adapter, but when that fails the host is left with no "+
			"working address and needs console access to recover. Write down its current "+
			"configuration first.")
	}
	if p.IsOnlyUplink {
		risks = append(risks, p.AdapterName+" is this host's only connected adapter, so there is no "+
			"second path to fix anything that goes wrong remotely. allow_management_os must stay "+
			"true.")
	}
	if p.IsWireless {
		risks = append(risks, "SERIOUS: "+p.AdapterName+" is wireless. Hyper-V bridges wireless by "+
			"proxying ARP rather than truly bridging, so guests often do not appear as independent "+
			"hosts on the LAN — which is the entire reason to make an External switch. Prefer a "+
			"wired adapter.")
	}
	if p.NetworkProfile != "" && !strings.EqualFold(p.NetworkProfile, "Public") {
		risks = append(risks, "The new virtual adapter is a network Windows has not seen before, so "+
			"it may be classified Public where "+p.AdapterName+" is currently "+p.NetworkProfile+". "+
			"Firewall rules scoped to "+p.NetworkProfile+" would stop applying until you reclassify it.")
	}
	if p.AlreadyBound != "" {
		risks = append(risks, p.AdapterName+" is already bound to the switch '"+p.AlreadyBound+
			"', so a second External switch on it cannot be created.")
	}
	risks = append(risks, "The address is re-requested over DHCP and is usually the same one, but "+
		"anything pinned to the host's current address should be checked afterwards.")

	return risks
}

// CreateSwitch creates a virtual switch.
//
// Creating an External switch rebinds a physical adapter, which drops the host's
// network for a few seconds. That is why confirmDisruption exists: the caller
// has to say it accepts losing connectivity, because a remote session would not
// survive to see an error message.
//
// NOT YET EXERCISED against a real host. The External path has been written and
// reviewed but never run, because doing so means deliberately disconnecting a
// working machine. Its guards and preflight are tested; the creation itself is
// not. Treat it as unproven until that changes.
func (c *Client) CreateSwitch(ctx context.Context, name, switchType, adapterName string, allowManagementOS *bool, confirmDisruption bool, notes string) (*VMSwitch, error) {
	if name == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	switch strings.ToLower(switchType) {
	case "external":
		switchType = "External"
		if adapterName == "" {
			return nil, hverr.New(hverr.InvalidArgument,
				"net_adapter_name is required for an External switch").
				WithDetail("Call list_physical_adapters to see the options; prefer a wired adapter.")
		}
		if !confirmDisruption {
			return nil, hverr.New(hverr.InvalidArgument,
				"creating an External switch briefly disconnects this host from the network").
				WithDetail("Hyper-V rebinds the physical adapter, so any remote session to this " +
					"machine will drop. Pass confirm_disruption to proceed.")
		}
	case "internal":
		switchType = "Internal"
	case "private":
		switchType = "Private"
	default:
		return nil, hverr.New(hverr.InvalidArgument,
			`switch_type must be "External", "Internal" or "Private", got %q`, switchType)
	}

	manage := true
	if allowManagementOS != nil {
		manage = *allowManagementOS
	}

	const script = `
    if (Get-VMSwitch -Name $P.name -ErrorAction SilentlyContinue) {
        throw "HVERR:VM_ALREADY_EXISTS|a switch named '$($P.name)' already exists"
    }
    $newArgs = @{ Name = $P.name; Notes = [string]$P.notes }
    if ($P.switch_type -eq 'External') {
        $nic = Get-NetAdapter -Name $P.adapter -ErrorAction SilentlyContinue
        if (-not $nic) { throw "HVERR:ADAPTER_NOT_FOUND|no network adapter named '$($P.adapter)'" }
        if (-not $P.manage) {
            # Taking the management OS off its only adapter would cut the host
            # off entirely, with no way back except the console.
            $physical = @(Get-NetAdapter -Physical | Where-Object { $_.Status -eq 'Up' })
            if ($physical.Count -le 1) {
                throw "HVERR:INVALID_ARGUMENT|this host has one connected adapter, so allow_management_os must stay true or the host loses all networking"
            }
        }
        $newArgs['NetAdapterName']    = $P.adapter
        $newArgs['AllowManagementOS'] = [bool]$P.manage
    } else {
        $newArgs['SwitchType'] = $P.switch_type
    }
    New-VMSwitch @newArgs | Out-Null
` + switchProjectionOne

	var out VMSwitch
	// Rebinding an adapter can take a while, and the host's own network is down
	// for part of it.
	err := c.r.RunTimeoutInto(ctx, 3*time.Minute, script, map[string]any{
		"name":        name,
		"switch_type": switchType,
		"adapter":     adapterName,
		"manage":      manage,
		"notes":       notes,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// switchProjectionOne fills $result from the switch named $P.name.
const switchProjectionOne = `
    $sw = Get-VMSwitch -Name $P.name
    $onSwitch = @(Get-VM | Get-VMNetworkAdapter | Where-Object { $_.SwitchName -eq $sw.Name } |
                  ForEach-Object { $_.VMName } | Sort-Object -Unique)
    $result = [ordered]@{
        name                = $sw.Name
        switch_type         = $sw.SwitchType.ToString()
        net_adapter_name    = [string]$sw.NetAdapterInterfaceDescription
        allow_management_os = [bool]$sw.AllowManagementOS
        connected_vms       = $onSwitch
    }`

// DeleteSwitch removes a virtual switch, reporting which VMs lose their network.
func (c *Client) DeleteSwitch(ctx context.Context, name string, confirmDisruption bool) (map[string]any, error) {
	if name == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	const script = `
    $sw = Get-VMSwitch -Name $P.name -ErrorAction SilentlyContinue
    if (-not $sw) { throw "HVERR:SWITCH_NOT_FOUND|no switch named '$($P.name)'" }

    if ($sw.SwitchType -eq 'External' -and -not $P.confirm) {
        throw "HVERR:INVALID_ARGUMENT|removing an External switch briefly disconnects this host; pass confirm_disruption to proceed"
    }
    $affected = @(Get-VM | Get-VMNetworkAdapter | Where-Object { $_.SwitchName -eq $sw.Name } |
                  ForEach-Object { $_.VMName } | Sort-Object -Unique)
    Remove-VMSwitch -VMSwitch $sw -Force -Confirm:$false | Out-Null
    $result = [ordered]@{
        deleted            = $true
        name               = $P.name
        disconnected_vms   = $affected
    }`

	var out map[string]any
	err := c.r.RunTimeoutInto(ctx, 3*time.Minute, script,
		map[string]any{"name": name, "confirm": confirmDisruption}, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetVMNetwork configures a VM's network adapter.
func (c *Client) SetVMNetwork(ctx context.Context, o SetVMNetworkOptions) (*VMDetail, error) {
	if o.VMName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "vm_name is required")
	}
	if o.StaticMAC != "" && !strings.EqualFold(o.StaticMAC, "dynamic") {
		if _, err := net.ParseMAC(normalizeMAC(o.StaticMAC)); err != nil {
			return nil, hverr.New(hverr.InvalidArgument,
				"static_mac %q is not a MAC address", o.StaticMAC)
		}
	}
	if o.VLANID != nil && (*o.VLANID < 0 || *o.VLANID > 4094) {
		return nil, hverr.New(hverr.InvalidArgument, "vlan_id must be between 0 and 4094")
	}

	args := map[string]any{
		"name":           o.VMName,
		"adapter_name":   o.AdapterName,
		"switch_name":    o.SwitchName,
		"static_mac":     o.StaticMAC,
		"create_adapter": o.CreateAdapter,
		"set_vlan":       o.VLANID != nil,
		"vlan_id":        0,
		"set_spoofing":   o.MACSpoofing != nil,
		"mac_spoofing":   false,
	}
	if o.VLANID != nil {
		args["vlan_id"] = *o.VLANID
	}
	if o.MACSpoofing != nil {
		args["mac_spoofing"] = *o.MACSpoofing
	}

	const script = requireVM + `
    $nics = @(Get-VMNetworkAdapter -VM $vm)

    if ($P.adapter_name) {
        $nic = @($nics | Where-Object { $_.Name -eq $P.adapter_name })[0]
        if (-not $nic) {
            # A named adapter that does not exist yet is a request to add one,
            # which is how a VM ends up on a second network.
            if (-not $P.create_adapter) {
                $have = @($nics | ForEach-Object { $_.Name }) -join ', '
                throw "HVERR:ADAPTER_NOT_FOUND|'$($P.name)' has no adapter named '$($P.adapter_name)' (has: $have). Pass create_adapter to add one."
            }
            Add-VMNetworkAdapter -VM $vm -Name $P.adapter_name | Out-Null
            $nic = @(Get-VMNetworkAdapter -VM $vm | Where-Object { $_.Name -eq $P.adapter_name })[0]
        }
    } elseif ($nics.Count -eq 0) {
        if (-not $P.create_adapter) {
            throw "HVERR:ADAPTER_NOT_FOUND|'$($P.name)' has no network adapter; pass create_adapter to add one"
        }
        Add-VMNetworkAdapter -VM $vm | Out-Null
        $nic = @(Get-VMNetworkAdapter -VM $vm)[0]
    } else {
        $nic = $nics[0]
    }

    if ($P.switch_name -eq '-') {
        Disconnect-VMNetworkAdapter -VMNetworkAdapter $nic | Out-Null
    } elseif ($P.switch_name) {
        if (-not (Get-VMSwitch -Name $P.switch_name -ErrorAction SilentlyContinue)) {
            throw "HVERR:SWITCH_NOT_FOUND|no switch named '$($P.switch_name)'"
        }
        Connect-VMNetworkAdapter -VMNetworkAdapter $nic -SwitchName $P.switch_name | Out-Null
    }

    if ($P.static_mac) {
        if ($P.static_mac -eq 'dynamic') {
            Set-VMNetworkAdapter -VMNetworkAdapter $nic -DynamicMacAddress | Out-Null
        } else {
            Set-VMNetworkAdapter -VMNetworkAdapter $nic -StaticMacAddress $P.static_mac | Out-Null
        }
    }
    if ($P.set_spoofing) {
        $mode = if ($P.mac_spoofing) { 'On' } else { 'Off' }
        Set-VMNetworkAdapter -VMNetworkAdapter $nic -MacAddressSpoofing $mode | Out-Null
    }
    if ($P.set_vlan) {
        if ([int]$P.vlan_id -eq 0) {
            Set-VMNetworkAdapterVlan -VMNetworkAdapter $nic -Untagged | Out-Null
        } else {
            Set-VMNetworkAdapterVlan -VMNetworkAdapter $nic -Access -VlanId ([int]$P.vlan_id) | Out-Null
        }
    }
` + detailProjection

	var out VMDetail
	if err := c.r.RunTimeoutInto(ctx, 2*time.Minute, script, args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// requireAdapter resolves $P.adapter_name into $nic, defaulting to the VM's
// first adapter. It expects $vm to be set.
const requireAdapter = `
    $nics = @(Get-VMNetworkAdapter -VM $vm)
    if ($nics.Count -eq 0) {
        throw "HVERR:ADAPTER_NOT_FOUND|'$($P.name)' has no network adapter"
    }
    if ($P.adapter_name) {
        $nic = @($nics | Where-Object { $_.Name -eq $P.adapter_name })[0]
        if (-not $nic) {
            $have = @($nics | ForEach-Object { $_.Name }) -join ', '
            throw "HVERR:ADAPTER_NOT_FOUND|'$($P.name)' has no adapter named '$($P.adapter_name)' (has: $have)"
        }
    } else {
        $nic = $nics[0]
    }
`

// SetVMNetworkAdvanced changes the per-port features of a virtual NIC.
//
// These are the settings on a Hyper-V adapter that SetVMNetwork leaves alone:
// the security guards, bandwidth reservations, offloads, port mirroring, and
// trunk-mode VLAN. They are separate because the questions differ. SetVMNetwork
// answers "which network is this VM on"; this answers "how does the switch treat
// its traffic", which is a question you only reach once the first is settled.
func (c *Client) SetVMNetworkAdvanced(ctx context.Context, o AdapterFeatureOptions) (*VMSettings, error) {
	if o.VMName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "vm_name is required")
	}

	switch strings.ToLower(o.PortMirroring) {
	case "":
	case "none":
		o.PortMirroring = "None"
	case "source":
		o.PortMirroring = "Source"
	case "destination":
		o.PortMirroring = "Destination"
	default:
		return nil, hverr.New(hverr.InvalidArgument,
			`port_mirroring must be "None", "Source" or "Destination", got %q`, o.PortMirroring)
	}

	for _, f := range []struct {
		label string
		v     *int
		max   int
	}{
		{"vmq_weight", o.VMQWeight, 100},
		{"minimum_bandwidth_weight", o.MinimumBandwidthWeight, 100},
	} {
		if f.v != nil && (*f.v < 0 || *f.v > f.max) {
			return nil, hverr.New(hverr.InvalidArgument, "%s must be between 0 and %d", f.label, f.max)
		}
	}
	for _, f := range []struct {
		label string
		v     *int
	}{
		{"ipsec_offload_max_sa", o.IPsecOffloadMaxSA},
		{"minimum_bandwidth_mbps", o.MinimumBandwidthMbps},
		{"maximum_bandwidth_mbps", o.MaximumBandwidthMbps},
	} {
		if f.v != nil && *f.v < 0 {
			return nil, hverr.New(hverr.InvalidArgument, "%s cannot be negative", f.label)
		}
	}
	// Hyper-V accepts both and then enforces neither predictably, so the choice
	// is made here instead of being discovered later.
	if o.MinimumBandwidthMbps != nil && *o.MinimumBandwidthMbps > 0 &&
		o.MinimumBandwidthWeight != nil && *o.MinimumBandwidthWeight > 0 {
		return nil, hverr.New(hverr.InvalidArgument,
			"minimum_bandwidth_mbps and minimum_bandwidth_weight are two ways to reserve the "+
				"same thing; a switch honours one or the other, so set just one")
	}
	if o.MinimumBandwidthMbps != nil && o.MaximumBandwidthMbps != nil &&
		*o.MaximumBandwidthMbps > 0 && *o.MinimumBandwidthMbps > *o.MaximumBandwidthMbps {
		return nil, hverr.New(hverr.InvalidArgument,
			"minimum_bandwidth_mbps (%d) is above maximum_bandwidth_mbps (%d)",
			*o.MinimumBandwidthMbps, *o.MaximumBandwidthMbps)
	}

	if o.TrunkNativeVLANID != nil && (*o.TrunkNativeVLANID < 0 || *o.TrunkNativeVLANID > 4094) {
		return nil, hverr.New(hverr.InvalidArgument, "trunk_native_vlan_id must be between 0 and 4094")
	}
	for _, id := range o.TrunkAllowedVLANIDs {
		if id < 1 || id > 4094 {
			return nil, hverr.New(hverr.InvalidArgument,
				"trunk_allowed_vlan_ids entry %d must be between 1 and 4094", id)
		}
	}
	if (o.TrunkNativeVLANID != nil) != (len(o.TrunkAllowedVLANIDs) > 0) {
		return nil, hverr.New(hverr.InvalidArgument,
			"trunk_native_vlan_id and trunk_allowed_vlan_ids define one trunk together; give both")
	}

	// A nil slice would reach PowerShell as null, which pipes as one element
	// rather than none.
	if o.TrunkAllowedVLANIDs == nil {
		o.TrunkAllowedVLANIDs = []int{}
	}
	args := map[string]any{
		"name":          o.VMName,
		"adapter_name":  o.AdapterName,
		"mirroring":     o.PortMirroring,
		"trunk_allowed": o.TrunkAllowedVLANIDs,
		"set_trunk":     o.TrunkNativeVLANID != nil,
		"trunk_native":  0,
	}
	if o.TrunkNativeVLANID != nil {
		args["trunk_native"] = *o.TrunkNativeVLANID
	}
	setBool(args, "dhcp_guard", o.DHCPGuard)
	setBool(args, "router_guard", o.RouterGuard)
	setBool(args, "device_naming", o.DeviceNaming)
	setBool(args, "teaming", o.AllowTeaming)
	setInt(args, "vmq", o.VMQWeight)
	setInt(args, "ipsec", o.IPsecOffloadMaxSA)
	setInt(args, "min_bw", o.MinimumBandwidthMbps)
	setInt(args, "max_bw", o.MaximumBandwidthMbps)
	setInt(args, "min_weight", o.MinimumBandwidthWeight)

	const script = requireVM + requireAdapter + `
    $a = @{ VMNetworkAdapter = $nic }
    if ($P.set_dhcp_guard)    { $a['DhcpGuard']     = $(if ($P.dhcp_guard)    { 'On' } else { 'Off' }) }
    if ($P.set_router_guard)  { $a['RouterGuard']   = $(if ($P.router_guard)  { 'On' } else { 'Off' }) }
    if ($P.set_device_naming) { $a['DeviceNaming']  = $(if ($P.device_naming) { 'On' } else { 'Off' }) }
    if ($P.set_teaming)       { $a['AllowTeaming']  = $(if ($P.teaming)       { 'On' } else { 'Off' }) }
    if ($P.mirroring)         { $a['PortMirroring'] = $P.mirroring }
    if ($P.set_vmq)           { $a['VmqWeight']     = [int]$P.vmq }
    if ($P.set_ipsec)         { $a['IPsecOffloadMaximumSecurityAssociation'] = [int]$P.ipsec }
    # Hyper-V takes these in bits per second.
    if ($P.set_min_bw)        { $a['MinimumBandwidthAbsolute'] = [int64]$P.min_bw * 1000000 }
    if ($P.set_max_bw)        { $a['MaximumBandwidth']         = [int64]$P.max_bw * 1000000 }
    if ($P.set_min_weight)    { $a['MinimumBandwidthWeight']   = [int]$P.min_weight }

    if ($a.Count -gt 1) { Set-VMNetworkAdapter @a | Out-Null }

    if ($P.set_trunk) {
        $allowed = @($P.trunk_allowed | ForEach-Object { [int]$_ })
        Set-VMNetworkAdapterVlan -VMNetworkAdapter $nic -Trunk -NativeVlanId ([int]$P.trunk_native) -AllowedVlanIdList $allowed | Out-Null
    } elseif ($a.Count -le 1) {
        throw "HVERR:INVALID_ARGUMENT|nothing to change; pass at least one adapter feature"
    }
` + settingsProjection

	var out VMSettings
	if err := c.r.RunTimeoutInto(ctx, 2*time.Minute, script, args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveVMNetworkAdapter takes a virtual NIC away from a VM.
//
// This is the counterpart to SetVMNetwork's create_adapter. Removing the last
// adapter is allowed: a VM with no network at all is a legitimate thing to want,
// and guest_invoke_command still reaches a Windows guest without one.
func (c *Client) RemoveVMNetworkAdapter(ctx context.Context, vmName, adapterName string) (*VMSettings, error) {
	if vmName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "vm_name is required")
	}
	if adapterName == "" {
		return nil, hverr.New(hverr.InvalidArgument,
			"adapter_name is required; removing an unnamed adapter would be a guess at which one")
	}

	const script = requireVM + requireAdapter + `
    Remove-VMNetworkAdapter -VMNetworkAdapter $nic | Out-Null
` + settingsProjection

	var out VMSettings
	if err := c.r.RunTimeoutInto(ctx, 2*time.Minute, script,
		map[string]any{"name": vmName, "adapter_name": adapterName}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// normalizeMAC accepts the bare 12-hex form Hyper-V reports and turns it into
// something net.ParseMAC understands.
func normalizeMAC(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 12 && !strings.ContainsAny(s, ":-") {
		var parts []string
		for i := 0; i < 12; i += 2 {
			parts = append(parts, s[i:i+2])
		}
		return strings.Join(parts, ":")
	}
	return s
}

// DiagnoseVMNetwork reports what a VM's networking can and cannot do, so the
// caller can choose between opening a tunnel and moving the VM to an External
// switch instead of finding out by trial and error.
//
// guestHost is probed when Hyper-V reports no address, which happens on any
// guest without the reporting agent installed.
func (c *Client) DiagnoseVMNetwork(ctx context.Context, vmName, guestHost string, probePorts []int) (*NetworkDiagnosis, error) {
	if vmName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "vm_name is required")
	}

	const script = requireVM + `
    $result = [ordered]@{
        vm_name = $vm.Name
        state   = $vm.State.ToString()
        adapters = @(Get-VMNetworkAdapter -VM $vm | ForEach-Object {
            $nic  = $_
            $sw   = Get-VMSwitch -Name $nic.SwitchName -ErrorAction SilentlyContinue
            $vlan = Get-VMNetworkAdapterVlan -VMNetworkAdapter $nic -ErrorAction SilentlyContinue
            [ordered]@{
                name          = $nic.Name
                switch_name   = [string]$nic.SwitchName
                switch_type   = [string]$(if ($sw) { $sw.SwitchType.ToString() } else { '' })
                mac_address   = [string]$nic.MacAddress
                vlan_id       = [int]$(if ($vlan -and $vlan.OperationMode -eq 'Access') { $vlan.AccessVlanId } else { 0 })
                mac_spoofing  = [bool]($nic.MacAddressSpoofing -eq 'On')
                ip_addresses  = @($nic.IPAddresses | Where-Object { $_ })
                # Only an External switch puts a guest on the physical network;
                # every other type keeps it behind the host.
                on_physical_lan = [bool]($sw -and $sw.SwitchType -eq 'External')
                host_can_reach  = $false
            }
        })
    }`

	var d NetworkDiagnosis
	if err := c.r.RunInto(ctx, script, map[string]any{"name": vmName}, &d); err != nil {
		return nil, err
	}

	// Probing is done from Go rather than PowerShell: it is plain TCP, and doing
	// it here keeps one dial timeout rather than a nested script deadline.
	for i := range d.Adapters {
		a := &d.Adapters[i]
		if a.OnPhysicalLAN {
			d.GuestOnPhysicalLAN = true
		}
		if len(a.IPAddresses) > 0 {
			d.AddressesReported = true
		}
		// Probe only routable addresses. A link-local one needs a zone index to
		// dial at all, and probing every address would report each port once per
		// address rather than once.
		for _, addr := range a.IPAddresses {
			if ip, err := netip.ParseAddr(addr); err != nil ||
				ip.IsLinkLocalUnicast() || ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			if probeAddress(a, addr, probePorts) {
				d.HostCanReach = true
			}
		}
	}

	// With nothing reported, fall back to the caller's address so the answer is
	// about reachability rather than about the missing agent.
	if !d.AddressesReported && guestHost != "" && len(d.Adapters) > 0 {
		d.ProbedHost = guestHost
		if probeAddress(&d.Adapters[0], guestHost, probePorts) {
			d.HostCanReach = true
		}
	}

	d.BlockedHostPorts = occupiedHostPorts()
	d.Recommendation = recommend(&d)
	return &d, nil
}

// probeAddress tests one address and records which of probePorts answered.
func probeAddress(a *AdapterDiagnosis, addr string, probePorts []int) bool {
	live := reachable(addr, 22, 2*time.Second) || reachable(addr, 80, 2*time.Second) || pingable(addr)
	for _, p := range probePorts {
		if reachable(addr, p, 2*time.Second) {
			a.ReachablePorts = append(a.ReachablePorts, p)
			live = true
		} else {
			a.UnreachablePort = append(a.UnreachablePort, p)
		}
	}
	if live {
		a.HostCanReach = true
	}
	return live
}

// reachable reports whether a TCP connection succeeds.
func reachable(host string, port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// pingable stands in for ICMP, which needs raw sockets: if anything at all
// answers a TCP handshake attempt with a refusal, the host is routable.
func pingable(host string) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "9"), time.Second)
	if err == nil {
		conn.Close()
		return true
	}
	// A refused connection still proves the address is reachable; a timeout does not.
	return strings.Contains(err.Error(), "refused")
}

// occupiedHostPorts reports which well-known ports the host holds, since those
// can never be used as a tunnel's host port.
func occupiedHostPorts() []int {
	var busy []int
	for _, p := range hostHeldPorts {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			busy = append(busy, p)
			continue
		}
		ln.Close()
	}
	return busy
}

func recommend(d *NetworkDiagnosis) string {
	if len(d.Adapters) == 0 {
		return "This VM has no network adapter. Attach one with set_vm_network, or use " +
			"guest_invoke_command, which needs no guest network at all."
	}

	// Reachability leads, because it is what the caller is usually asking about.
	var parts []string
	switch {
	case d.GuestOnPhysicalLAN:
		parts = append(parts, "The guest is on an External switch, so it has its own address on the "+
			"physical LAN. Other machines can reach it directly, and protocols a tunnel cannot carry "+
			"— SMB, Kerberos-authenticated services, anything relying on broadcast — will work.")

	case d.HostCanReach:
		parts = append(parts, "The guest is behind the host, so only this host can reach it.")

	case !d.AddressesReported:
		// Not knowing the address is a different problem from not being able to
		// reach it, and calling it unreachable would send the reader down the
		// wrong path entirely.
		parts = append(parts, "The guest is not reporting an address and no address was supplied to "+
			"probe, so its reachability is unknown — which is not the same as unreachable.")

	default:
		parts = append(parts, "The guest reports an address but does not answer on it. Check that it "+
			"has finished booting and that its firewall allows the port; on a Private switch nothing "+
			"outside the VM network can reach it at all, and guest_invoke_command is the only way in.")
	}

	// A missing agent is a separate fact from reachability, and stays relevant
	// even when the guest answers: wait_for_guest_ip will keep failing, and every
	// tool will need the address passed by hand.
	if !d.AddressesReported {
		parts = append(parts, "Hyper-V is not being told the guest's address, because it learns that "+
			"from an agent inside the guest and minimal Linux installs ship without one. "+
			"wait_for_guest_ip will keep timing out until you install hyperv-daemons and get "+
			"hypervkvpd running; until then pass the address as guest_host, which ssh_exec, "+
			"open_tunnel and this tool all accept.")
	}

	if !d.GuestOnPhysicalLAN {
		if len(d.BlockedHostPorts) > 0 {
			parts = append(parts, fmt.Sprintf(
				"Tunnels work for services whose client can be pointed at a port. Ports %v are held "+
					"by Windows itself and cannot be tunnelled — for those, and for anything "+
					"sensitive to host identity, create an External switch so the guest gets its own "+
					"LAN address.", d.BlockedHostPorts))
		} else {
			parts = append(parts, `Open a tunnel to reach a service, using mode "ssh" if it is bound `+
				`to the guest's own 127.0.0.1.`)
		}
	}

	return strings.Join(parts, " ")
}
