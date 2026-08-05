package mcpsrv

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heavycaffeiner/hypervm-mcp/internal/guest"
	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/hyperv"
)

type createSwitchInput struct {
	Name              string `json:"name" jsonschema:"Name for the new switch."`
	SwitchType        string `json:"switch_type" jsonschema:"\"External\" puts guests on the physical LAN, \"Internal\" reaches only the host, \"Private\" reaches only other VMs."`
	NetAdapterName    string `json:"net_adapter_name,omitempty" jsonschema:"Physical adapter to bind. Required for External. See list_physical_adapters."`
	AllowManagementOS *bool  `json:"allow_management_os,omitempty" jsonschema:"Keep the host using the same adapter. Default true; false on a single-adapter host is refused."`
	ConfirmDisruption bool   `json:"confirm_disruption,omitempty" jsonschema:"Required for External: creating one briefly disconnects this host from the network."`
	Notes             string `json:"notes,omitempty" jsonschema:"Free-form note stored with the switch."`
}

type deleteSwitchInput struct {
	Name              string `json:"name" jsonschema:"Switch to remove."`
	ConfirmDisruption bool   `json:"confirm_disruption,omitempty" jsonschema:"Required for External, which briefly disconnects this host."`
}

type setVMNetworkInput struct {
	VMName        string `json:"vm_name" jsonschema:"Exact name of the VM."`
	AdapterName   string `json:"adapter_name,omitempty" jsonschema:"Which adapter to change, by name. Defaults to the VM's first. Naming one that does not exist adds it, if create_adapter is set — that is how a VM joins a second network."`
	SwitchName    string `json:"switch_name,omitempty" jsonschema:"Switch to connect to. \"-\" disconnects. Empty leaves it unchanged."`
	StaticMAC     string `json:"static_mac,omitempty" jsonschema:"Fixed MAC address, or \"dynamic\" to go back to a generated one."`
	VLANID        *int   `json:"vlan_id,omitempty" jsonschema:"VLAN to tag with; 0 removes tagging. Omit to leave unchanged."`
	MACSpoofing   *bool  `json:"mac_spoofing,omitempty" jsonschema:"Allow frames with other MAC addresses. Needed when the guest runs nested VMs or bridged containers. Omit to leave unchanged."`
	CreateAdapter bool   `json:"create_adapter,omitempty" jsonschema:"Add an adapter if the VM has none."`
}

type setVMNetworkAdvancedInput struct {
	VMName      string `json:"vm_name" jsonschema:"Exact name of the VM."`
	AdapterName string `json:"adapter_name,omitempty" jsonschema:"Which adapter to change, by name. Defaults to the VM's first."`

	DHCPGuard     *bool  `json:"dhcp_guard,omitempty" jsonschema:"Drop DHCP offers sent by this guest, so a VM cannot hand out addresses on the network it sits on."`
	RouterGuard   *bool  `json:"router_guard,omitempty" jsonschema:"Drop router advertisements sent by this guest."`
	PortMirroring string `json:"port_mirroring,omitempty" jsonschema:"\"Source\" copies this adapter's traffic to whichever adapter on the same switch is set to \"Destination\". \"None\" turns it off."`
	DeviceNaming  *bool  `json:"device_naming,omitempty" jsonschema:"Pass the adapter's Hyper-V name through to the guest, so the guest can tell which NIC is which."`
	AllowTeaming  *bool  `json:"allow_teaming,omitempty" jsonschema:"Let the guest put this adapter in a NIC team of its own."`

	VMQWeight         *int `json:"vmq_weight,omitempty" jsonschema:"Claim on the switch's virtual machine queues, 0 to 100. 0 disables VMQ for this adapter."`
	IPsecOffloadMaxSA *int `json:"ipsec_offload_max_sa,omitempty" jsonschema:"How many IPsec security associations the adapter may offload to hardware. 0 disables offload."`

	MinimumBandwidthMbps   *int `json:"minimum_bandwidth_mbps,omitempty" jsonschema:"Bandwidth reserved for this adapter, in Mbps. 0 reserves nothing."`
	MaximumBandwidthMbps   *int `json:"maximum_bandwidth_mbps,omitempty" jsonschema:"Bandwidth cap for this adapter, in Mbps. 0 is unlimited."`
	MinimumBandwidthWeight *int `json:"minimum_bandwidth_weight,omitempty" jsonschema:"Relative claim on bandwidth under contention, 0 to 100. An alternative to minimum_bandwidth_mbps, not a companion to it."`

	TrunkNativeVLANID   *int  `json:"trunk_native_vlan_id,omitempty" jsonschema:"VLAN carried untagged on a trunk. Give this together with trunk_allowed_vlan_ids."`
	TrunkAllowedVLANIDs []int `json:"trunk_allowed_vlan_ids,omitempty" jsonschema:"VLANs carried tagged on a trunk. Give this together with trunk_native_vlan_id."`
}

type removeVMNetworkAdapterInput struct {
	VMName      string `json:"vm_name" jsonschema:"Exact name of the VM."`
	AdapterName string `json:"adapter_name" jsonschema:"Name of the adapter to remove, exactly as get_vm_settings reports it."`
}

type preflightInput struct {
	NetAdapterName string `json:"net_adapter_name" jsonschema:"Physical adapter that would be bound. See list_physical_adapters."`
}

// tailscaleRisk says what the rebind means for Tailscale, which is worth calling
// out separately: it survives on its own virtual adapter, but its underlay goes
// down with everything else.
func tailscaleRisk(ctx context.Context, d *Deps) string {
	status, err := d.Tailnet.Status(ctx)
	if err != nil || !status.Installed {
		return "Tailscale is not installed here, so nothing tailnet-related is at stake."
	}
	if status.BackendState != "Running" {
		return "Tailscale is installed but not connected, so nothing tailnet-related is at stake."
	}
	return "Tailscale runs on its own virtual adapter and is not rebound, but its underlying " +
		"connection goes down with the physical adapter and has to re-establish. Expect this host " +
		"(" + status.MagicDNSName + ") to drop off the tailnet for a few seconds; open tunnels " +
		"bound to tailnet addresses will refuse connections until it returns."
}

type diagnoseNetworkInput struct {
	VMName     string `json:"vm_name" jsonschema:"Exact name of the VM."`
	GuestHost  string `json:"guest_host,omitempty" jsonschema:"Address to probe when Hyper-V reports none, which happens on guests without the reporting agent."`
	ProbePorts []int  `json:"probe_ports,omitempty" jsonschema:"Guest ports to test for reachability from the host."`
}

type staticIPInput struct {
	VMName         string   `json:"vm_name" jsonschema:"Exact name of the VM."`
	Address        string   `json:"address" jsonschema:"IPv4 address to assign, e.g. \"192.168.0.42\"."`
	PrefixLength   int      `json:"prefix_length" jsonschema:"Subnet prefix length, e.g. 24."`
	Gateway        string   `json:"gateway,omitempty" jsonschema:"Default gateway."`
	DNSServers     []string `json:"dns_servers,omitempty" jsonschema:"DNS servers to configure."`
	InterfaceName  string   `json:"interface_name,omitempty" jsonschema:"Which guest interface to change. Defaults to the one carrying the default route."`
	AutoCheckpoint *bool    `json:"auto_checkpoint,omitempty" jsonschema:"Take a checkpoint first so a mistake is recoverable. Default true."`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"How long to wait for the new address to answer. Default 120."`
	Username       string   `json:"username,omitempty"`
	Password       string   `json:"password,omitempty"`
	Host           string   `json:"host,omitempty" jsonschema:"Current address to reach the guest at, if Hyper-V does not report one."`
}

type switchHostAddressInput struct {
	SwitchName   string `json:"switch_name" jsonschema:"Internal switch whose host adapter to configure."`
	Address      string `json:"address,omitempty" jsonschema:"IPv4 address for the host on this switch, e.g. \"10.10.0.1\"."`
	PrefixLength int    `json:"prefix_length,omitempty" jsonschema:"Subnet prefix length, e.g. 24."`
	Remove       bool   `json:"remove,omitempty" jsonschema:"Take the address away instead of setting one."`
}

type switchNATInput struct {
	SwitchName string `json:"switch_name" jsonschema:"Switch the NAT serves."`
	Prefix     string `json:"prefix,omitempty" jsonschema:"Subnet to translate, in CIDR, e.g. \"10.10.0.0/24\". Must contain the host address on this switch."`
	Enable     bool   `json:"enable,omitempty" jsonschema:"True to create it, false to remove it."`
}

func registerNetworkTools(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  "set_switch_host_address",
		Title: "Give the host an address on a switch",
		Description: "Set the host's own address on an Internal switch, which is what makes a " +
			"hand-made one usable.\n\n" +
			"Hyper-V's Default Switch arrives with an address, NAT and DHCP already arranged, which " +
			"hides how much of that is normally your job. A switch you create yourself has none of " +
			"it: its host adapter sits at a link-local address, and guests get nothing at all.\n\n" +
			"So a working private network takes three steps: create the Internal switch, give the " +
			"host an address here, and give each guest a static address in the same subnet with " +
			"set_guest_static_ip. Add set_switch_nat if the guests also need the internet.\n\n" +
			"A Private switch has no host adapter — that is what makes it private — so this is " +
			"refused for one. This touches only the virtual adapter, never a physical one.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in switchHostAddressInput) (*mcp.CallToolResult, *hyperv.SwitchNetwork, error) {
		out, err := d.VM.SetSwitchHostAddress(ctx, in.SwitchName, in.Address, in.PrefixLength, in.Remove)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "set_switch_nat",
		Title: "Give guests on a switch a route out",
		Description: "Translate a switch's subnet through the host, so guests on an Internal switch " +
			"can reach the internet without being on the physical LAN.\n\n" +
			"The host must already hold an address inside the prefix, so call " +
			"set_switch_host_address first. Windows NAT is defined by address prefix rather than by " +
			"interface, so two NATs with overlapping prefixes conflict; the error names the one in " +
			"the way.\n\n" +
			"NAT is a way out, not a way in — connections start from the guest. To reach a guest " +
			"service from elsewhere, open a tunnel. And NAT is not DHCP: guests still need static " +
			"addresses, with the host's address as their gateway.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in switchNATInput) (*mcp.CallToolResult, *hyperv.SwitchNetwork, error) {
		out, err := d.VM.SetSwitchNAT(ctx, in.SwitchName, "", in.Prefix, in.Enable)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "set_guest_static_ip",
		Title: "Set a static IP inside a guest",
		Description: "Configure a fixed address inside the guest OS.\n\n" +
			"The two guest families need opposite approaches, and this picks one automatically. A " +
			"Windows guest is configured over PowerShell Direct, which travels the VMBus and is " +
			"unaffected by the network change it makes. A Linux guest has no such channel, so the " +
			"change goes over SSH and applying it drops that very connection — the command is " +
			"detached, the session is allowed to die, and success is confirmed by connecting to the " +
			"NEW address. If that never answers, the guest is likely stranded and the error says so.\n\n" +
			"Because of that risk a checkpoint is taken first unless auto_checkpoint is turned off, " +
			"and its name is returned so a bad change can be reverted with apply_checkpoint.\n\n" +
			"Linux support covers NetworkManager and netplan. Anything else returns " +
			"UNSUPPORTED_GUEST_OS along with the exact commands to run by hand, rather than guessing " +
			"at an unfamiliar network stack.\n\n" +
			"Note that a static address only makes sense on a switch with a stable subnet. The " +
			"Default Switch renumbers itself when the host reboots, so a static address there will " +
			"stop working; a static MAC plus a DHCP reservation on your router is usually the better " +
			"way to pin an address.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in staticIPInput) (*mcp.CallToolResult, *guest.StaticIPResult, error) {
		checkpoint := true
		if in.AutoCheckpoint != nil {
			checkpoint = *in.AutoCheckpoint
		}
		out, err := d.Guest.SetStaticIP(ctx, guest.StaticIPOptions{
			VMName:         in.VMName,
			Address:        in.Address,
			PrefixLength:   in.PrefixLength,
			Gateway:        in.Gateway,
			DNSServers:     in.DNSServers,
			InterfaceName:  in.InterfaceName,
			AutoCheckpoint: checkpoint,
			TimeoutSeconds: in.TimeoutSeconds,
			Username:       in.Username,
			Password:       in.Password,
			Host:           in.Host,
		})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "list_physical_adapters",
		Title: "List the host's network adapters",
		Description: "List the host's real network adapters with link state, speed, and whether each " +
			"is already bound to an external switch. Use it to choose an adapter for create_switch. " +
			"Prefer a wired one: a wireless adapter bridges by proxying ARP rather than truly " +
			"bridging, so guests on it often do not appear as independent hosts on the LAN.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, *listOf[hyperv.PhysicalAdapter], error) {
		return list(d.VM.ListPhysicalAdapters(ctx))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "preflight_external_switch",
		Title: "Check what an External switch would do to this host",
		Description: "Report what binding a given adapter to an External switch would do to THIS " +
			"host: whether it is the only uplink, whether its address is static and so might not " +
			"survive the move, whether it is wireless and so will not truly bridge, what firewall " +
			"profile it currently carries, and whether it is already bound.\n\n" +
			"Changes nothing. Call it before create_switch, which refuses an External switch until " +
			"confirm_disruption is set and returns this same report when it does.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in preflightInput) (*mcp.CallToolResult, *hyperv.ExternalSwitchPreflight, error) {
		out, err := d.VM.PreflightExternalSwitch(ctx, in.NetAdapterName)
		if err != nil {
			return nil, nil, err
		}
		out.Risks = append(out.Risks, tailscaleRisk(ctx, d))
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "create_switch",
		Title: "Create a virtual switch",
		Description: "Create a virtual switch.\n\n" +
			"NOT YET VERIFIED for switch_type \"External\": that path has been written and reviewed " +
			"but never run against a real host, because exercising it means deliberately " +
			"disconnecting a working machine. Its refusal and preflight are tested; creation itself " +
			"is not. Treat it as unproven and have console access before using it.\n\n" +
			"DANGEROUS FOR switch_type \"External\". Hyper-V rebinds the chosen physical adapter, " +
			"which disconnects this host from the network for several seconds and can, when a static " +
			"address fails to migrate, leave it with no working address at all — recoverable only " +
			"from the console. Nothing else this server does touches host networking.\n\n" +
			"Because of that, an External switch is refused until confirm_disruption is set, and the " +
			"refusal reports exactly which risks apply to this host. Read that before retrying, or " +
			"call preflight_external_switch first. \"Internal\" and \"Private\" switches touch no " +
			"physical adapter and are safe.\n\n" +
			"An External switch gives a guest its own address on the physical LAN, assigned by the " +
			"LAN's DHCP server. It is worth the risk only when other machines must reach the guest " +
			"directly, or when the guest must see broadcast traffic. It is NOT needed to reach a " +
			"guest service from this host: an outbound connection to the guest's address binds no " +
			"host port, so even SMB on 445 works over the Default Switch — it is only a tunnel, " +
			"which must bind a host port, that cannot.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createSwitchInput) (*mcp.CallToolResult, *hyperv.VMSwitch, error) {
		// Refuse before touching anything, and make the refusal carry the reason
		// this host in particular should think twice.
		if strings.EqualFold(in.SwitchType, "External") && !in.ConfirmDisruption {
			report, err := d.VM.PreflightExternalSwitch(ctx, in.NetAdapterName)
			if err != nil {
				return nil, nil, err
			}
			report.Risks = append(report.Risks, tailscaleRisk(ctx, d))
			return nil, nil, hverr.New(hverr.InvalidArgument,
				"creating an External switch on %q disrupts this host's networking", in.NetAdapterName).
				WithDetail("Pass confirm_disruption to proceed.\n\n" + strings.Join(report.Risks, "\n\n"))
		}

		out, err := d.VM.CreateSwitch(ctx, in.Name, in.SwitchType, in.NetAdapterName,
			in.AllowManagementOS, in.ConfirmDisruption, in.Notes)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "delete_switch",
		Title: "Delete a virtual switch",
		Description: "Remove a virtual switch. VMs attached to it lose their network; the result lists " +
			"which. Removing an External switch disconnects this host again as the adapter is " +
			"unbound, and that path is unverified for the same reason create_switch's is.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deleteSwitchInput) (*mcp.CallToolResult, map[string]any, error) {
		out, err := d.VM.DeleteSwitch(ctx, in.Name, in.ConfirmDisruption)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "set_vm_network",
		Title: "Configure a VM's network adapter",
		Description: "Change which switch a VM is on, its MAC address, VLAN, or MAC spoofing, or give " +
			"it another adapter.\n\n" +
			"To put a VM on a second network, name an adapter that does not exist yet and pass " +
			"create_adapter: it is added with that name and connected to switch_name, leaving the " +
			"existing one alone.\n\n" +
			"A static MAC is the least invasive way to give a guest a stable address: pair it with a " +
			"DHCP reservation on the router and the guest keeps its IP across reinstalls without any " +
			"guest-side configuration.\n\n" +
			"mac_spoofing is needed when the guest itself runs VMs or bridged containers, whose frames " +
			"carry MAC addresses the switch has not learned for that port and are dropped otherwise.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setVMNetworkInput) (*mcp.CallToolResult, *hyperv.VMDetail, error) {
		out, err := d.VM.SetVMNetwork(ctx, hyperv.SetVMNetworkOptions{
			VMName:        in.VMName,
			AdapterName:   in.AdapterName,
			SwitchName:    in.SwitchName,
			StaticMAC:     in.StaticMAC,
			VLANID:        in.VLANID,
			MACSpoofing:   in.MACSpoofing,
			CreateAdapter: in.CreateAdapter,
		})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "set_vm_network_advanced",
		Title: "Set a VM adapter's port features",
		Description: "Change the per-port features of a virtual NIC: the security guards, bandwidth " +
			"reservations and caps, hardware offloads, port mirroring, and trunk-mode VLAN.\n\n" +
			"set_vm_network answers \"which network is this VM on\". This answers \"how does the " +
			"switch treat its traffic\", which is a question you only reach once the first is settled.\n\n" +
			"dhcp_guard and router_guard are the two worth knowing about by default: they stop a guest " +
			"from handing out addresses or advertising itself as a router on a network it shares with " +
			"real machines, which is exactly what a misconfigured test VM does to an office LAN.\n\n" +
			"A trunk is what a guest firewall or router needs — it carries several VLANs to one " +
			"adapter and lets the guest tag its own frames, where set_vm_network's vlan_id puts the " +
			"adapter in a single VLAN and hides tagging entirely. The two are mutually exclusive " +
			"modes on the same adapter.\n\n" +
			"minimum_bandwidth_mbps and minimum_bandwidth_weight are two ways to reserve the same " +
			"thing and a switch honours one or the other, so passing both is refused rather than " +
			"quietly resolved.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setVMNetworkAdvancedInput) (*mcp.CallToolResult, *hyperv.VMSettings, error) {
		out, err := d.VM.SetVMNetworkAdvanced(ctx, hyperv.AdapterFeatureOptions{
			VMName:                 in.VMName,
			AdapterName:            in.AdapterName,
			DHCPGuard:              in.DHCPGuard,
			RouterGuard:            in.RouterGuard,
			PortMirroring:          in.PortMirroring,
			DeviceNaming:           in.DeviceNaming,
			AllowTeaming:           in.AllowTeaming,
			VMQWeight:              in.VMQWeight,
			IPsecOffloadMaxSA:      in.IPsecOffloadMaxSA,
			MinimumBandwidthMbps:   in.MinimumBandwidthMbps,
			MaximumBandwidthMbps:   in.MaximumBandwidthMbps,
			MinimumBandwidthWeight: in.MinimumBandwidthWeight,
			TrunkNativeVLANID:      in.TrunkNativeVLANID,
			TrunkAllowedVLANIDs:    in.TrunkAllowedVLANIDs,
		})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "remove_vm_network_adapter",
		Title: "Remove a VM network adapter",
		Description: "Take a virtual NIC away from a VM. This is the counterpart to set_vm_network's " +
			"create_adapter.\n\n" +
			"Removing the last adapter is allowed: a VM with no network at all is a legitimate thing " +
			"to want, and guest_invoke_command still reaches a Windows guest over the VMBus without " +
			"one. A Generation 1 VM needs to be stopped first; a Generation 2 VM does not.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in removeVMNetworkAdapterInput) (*mcp.CallToolResult, *hyperv.VMSettings, error) {
		out, err := d.VM.RemoveVMNetworkAdapter(ctx, in.VMName, in.AdapterName)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "diagnose_vm_network",
		Title: "Diagnose a VM's network reachability",
		Description: "Report a VM's network topology and what it can and cannot do, so you can choose " +
			"between opening a tunnel and moving the VM to an External switch instead of finding out " +
			"by trial and error.\n\n" +
			"For each adapter it reports the switch and its type, the addresses the guest reports, " +
			"VLAN and MAC spoofing, whether the host can actually reach it, and whether the guest is " +
			"a first-class node on the physical LAN. It also lists which well-known ports Windows " +
			"itself holds, since those can never be a tunnel's host port.\n\n" +
			"A guest that reports no address is reported as exactly that, not as unreachable — the " +
			"two have completely different fixes, and minimal Linux installs routinely do the first " +
			"while working fine. Pass guest_host to probe an address you already know.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in diagnoseNetworkInput) (*mcp.CallToolResult, *hyperv.NetworkDiagnosis, error) {
		out, err := d.VM.DiagnoseVMNetwork(ctx, in.VMName, in.GuestHost, in.ProbePorts)
		return nil, out, err
	})
}
