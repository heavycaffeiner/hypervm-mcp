//go:build windows

package guest

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/sshx"
)

// StaticIPOptions describes the address to configure inside a guest.
type StaticIPOptions struct {
	VMName         string
	Address        string
	PrefixLength   int
	Gateway        string
	DNSServers     []string
	InterfaceName  string
	AutoCheckpoint bool
	TimeoutSeconds int

	// Credential overrides; the stored credential is used otherwise.
	Username string
	Password string
	Host     string
}

// StaticIPResult reports what was done and whether it took effect.
type StaticIPResult struct {
	Applied         bool     `json:"applied"`
	VerifiedAddress string   `json:"verified_address,omitempty"`
	CheckpointName  string   `json:"checkpoint_name,omitempty"`
	Method          string   `json:"method"` // powershell-direct | nmcli | netplan
	ManualCommands  []string `json:"manual_commands,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
}

// ifaceNamePattern bounds an interface name to what a real one can contain.
//
// Unlike the PowerShell path, which passes values as data, the Linux path builds
// shell command text — so every value that reaches it is validated first. That
// validation is the whole defence against injection here.
var ifaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// SetStaticIP configures a fixed address inside a guest.
//
// The two guest families need opposite approaches. A Windows guest is configured
// over PowerShell Direct, which travels the VMBus and so is unaffected by the
// network change it is making. A Linux guest has no such channel, so the change
// must go over SSH — and applying it drops that very connection. The Linux path
// therefore detaches the command, lets the session die, and then confirms by
// connecting to the new address.
//
// Because a mistake here can leave a guest unreachable, a checkpoint is taken
// first unless the caller opts out.
func (d *Dialer) SetStaticIP(ctx context.Context, o StaticIPOptions) (*StaticIPResult, error) {
	addr, err := netip.ParseAddr(o.Address)
	if err != nil || !addr.Is4() {
		return nil, hverr.New(hverr.InvalidArgument, "address %q is not an IPv4 address", o.Address)
	}
	if o.PrefixLength < 1 || o.PrefixLength > 32 {
		return nil, hverr.New(hverr.InvalidArgument, "prefix_length must be between 1 and 32")
	}
	if o.Gateway != "" {
		if g, err := netip.ParseAddr(o.Gateway); err != nil || !g.Is4() {
			return nil, hverr.New(hverr.InvalidArgument, "gateway %q is not an IPv4 address", o.Gateway)
		}
	}
	for _, s := range o.DNSServers {
		if _, err := netip.ParseAddr(s); err != nil {
			return nil, hverr.New(hverr.InvalidArgument, "dns server %q is not an IP address", s)
		}
	}
	if o.InterfaceName != "" && !ifaceNamePattern.MatchString(o.InterfaceName) {
		return nil, hverr.New(hverr.InvalidArgument, "interface_name %q is not a valid interface name", o.InterfaceName)
	}
	if o.TimeoutSeconds <= 0 {
		o.TimeoutSeconds = 120
	}

	res := &StaticIPResult{}

	if o.AutoCheckpoint {
		name := "before-static-ip-" + strings.ReplaceAll(o.Address, ".", "-")
		cp, err := d.VM.CreateCheckpoint(ctx, o.VMName, name)
		if err != nil {
			return nil, hverr.Wrap(hverr.Internal, err,
				"could not take a checkpoint before changing the guest's network")
		}
		res.CheckpointName = cp.Name
	}

	if isWindows, _ := d.guestIsWindows(ctx, o); isWindows {
		res.Method = "powershell-direct"
		if err := d.setStaticIPWindows(ctx, o, res); err != nil {
			return res, err
		}
		return res, nil
	}
	if err := d.setStaticIPLinux(ctx, o, res); err != nil {
		// Return the partial result even on failure: it names the checkpoint to
		// revert to, which is the most useful thing to know at that moment.
		return res, err
	}
	return res, nil
}

// guestIsWindows probes for a PowerShell Direct endpoint, which only a Windows
// guest has.
func (d *Dialer) guestIsWindows(ctx context.Context, o StaticIPOptions) (bool, error) {
	user, pass := o.Username, o.Password
	if user == "" || pass == "" {
		stored, ok, err := d.Creds.Get(o.VMName)
		if err != nil || !ok {
			return false, err
		}
		if user == "" {
			user = stored.Username
		}
		if pass == "" {
			pass = stored.Password
		}
	}
	if user == "" || pass == "" {
		return false, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	_, err := d.VM.GuestInvokeCommand(probeCtx, o.VMName, "$true", user, pass, 30*time.Second)
	return err == nil, err
}

func (d *Dialer) setStaticIPWindows(ctx context.Context, o StaticIPOptions, res *StaticIPResult) error {
	// Over the VMBus, so the guest losing its address mid-command is harmless.
	script := `
$ErrorActionPreference = 'Stop'
$nic = if ($env:HVM_IFACE) { Get-NetAdapter -Name $env:HVM_IFACE }
       else { Get-NetAdapter | Where-Object { $_.Status -eq 'Up' } | Select-Object -First 1 }
if (-not $nic) { throw 'no usable network adapter' }
Remove-NetIPAddress -InterfaceIndex $nic.ifIndex -AddressFamily IPv4 -Confirm:$false -ErrorAction SilentlyContinue
Remove-NetRoute -InterfaceIndex $nic.ifIndex -DestinationPrefix '0.0.0.0/0' -Confirm:$false -ErrorAction SilentlyContinue
`
	newAddr := fmt.Sprintf(
		"New-NetIPAddress -InterfaceIndex $nic.ifIndex -IPAddress '%s' -PrefixLength %d",
		o.Address, o.PrefixLength)
	if o.Gateway != "" {
		newAddr += fmt.Sprintf(" -DefaultGateway '%s'", o.Gateway)
	}
	script += newAddr + " | Out-Null\n"
	if len(o.DNSServers) > 0 {
		script += fmt.Sprintf(
			"Set-DnsClientServerAddress -InterfaceIndex $nic.ifIndex -ServerAddresses %s\n",
			"'"+strings.Join(o.DNSServers, "','")+"'")
	}
	if o.InterfaceName != "" {
		script = "$env:HVM_IFACE = '" + o.InterfaceName + "'\n" + script
	}

	user, pass := o.Username, o.Password
	if user == "" || pass == "" {
		stored, _, _ := d.Creds.Get(o.VMName)
		if user == "" {
			user = stored.Username
		}
		if pass == "" {
			pass = stored.Password
		}
	}

	if _, err := d.VM.GuestInvokeCommand(ctx, o.VMName, script, user, pass,
		time.Duration(o.TimeoutSeconds)*time.Second); err != nil {
		return err
	}
	res.Applied = true
	res.VerifiedAddress = o.Address
	return nil
}

func (d *Dialer) setStaticIPLinux(ctx context.Context, o StaticIPOptions, res *StaticIPResult) error {
	client, _, err := d.Connect(ctx, o.VMName, sshx.Credential{
		Username: o.Username, Password: o.Password,
	}, o.Host, false)
	if err != nil {
		return err
	}

	run := func(cmd string) (string, error) {
		out, err := sshx.Exec(ctx, client, cmd, "", 60*time.Second)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(out.Stdout), nil
	}

	iface := o.InterfaceName
	if iface == "" {
		// The interface carrying the default route is the one worth changing.
		iface, err = run(`ip -o route show default | awk '{print $5}' | head -1`)
		if err != nil {
			return err
		}
		if !ifaceNamePattern.MatchString(iface) {
			return hverr.New(hverr.InvalidArgument,
				"could not determine the guest's primary interface (got %q); pass interface_name", iface)
		}
	}

	cidr := o.Address + "/" + strconv.Itoa(o.PrefixLength)
	dns := strings.Join(o.DNSServers, ",")

	hasNmcli, _ := run("command -v nmcli >/dev/null && echo yes || echo no")
	hasNetplan, _ := run("test -d /etc/netplan && echo yes || echo no")

	var apply string
	switch {
	case hasNmcli == "yes":
		res.Method = "nmcli"
		conn, err := run(fmt.Sprintf(
			`nmcli -t -f NAME,DEVICE connection show --active | awk -F: '$2=="%s"{print $1; exit}'`, iface))
		if err != nil || conn == "" {
			return hverr.New(hverr.UnsupportedGuestOS,
				"no active NetworkManager connection found for %s", iface)
		}
		mod := fmt.Sprintf(
			`sudo nmcli connection modify %q ipv4.method manual ipv4.addresses %s`,
			conn, cidr)
		if o.Gateway != "" {
			mod += " ipv4.gateway " + o.Gateway
		}
		if dns != "" {
			mod += " ipv4.dns " + dns
		}
		if _, err := run(mod); err != nil {
			return err
		}
		// Reactivating drops this SSH session, so detach it and let the shell go.
		apply = fmt.Sprintf(
			`sudo nohup sh -c 'sleep 2; nmcli connection up %q' >/dev/null 2>&1 & echo detached`, conn)

	case hasNetplan == "yes":
		res.Method = "netplan"
		yaml := buildNetplan(iface, cidr, o.Gateway, o.DNSServers)
		if _, err := run("sudo tee /etc/netplan/99-hypervm-mcp.yaml >/dev/null <<'HVMEOF'\n" + yaml + "HVMEOF"); err != nil {
			return err
		}
		if _, err := run("sudo chmod 600 /etc/netplan/99-hypervm-mcp.yaml"); err != nil {
			return err
		}
		apply = `sudo nohup sh -c 'sleep 2; netplan apply' >/dev/null 2>&1 & echo detached`

	default:
		// Guessing at an unknown distribution's network stack would be worse than
		// handing back the commands and letting a person run them.
		res.Method = "unsupported"
		res.ManualCommands = manualCommands(iface, cidr, o.Gateway, o.DNSServers)
		return hverr.New(hverr.UnsupportedGuestOS,
			"this guest has neither NetworkManager nor netplan").
			WithDetail("Run the commands in manual_commands, or pass interface_name if the guest uses something else.")
	}

	if _, err := run(apply); err != nil {
		return err
	}
	// The connection we just used is about to die; drop it so the pool does not
	// hand the corpse to the next caller.
	d.Pool.Drop(o.VMName)

	if err := d.waitForAddress(ctx, o, res); err != nil {
		return err
	}
	res.Applied = true
	res.VerifiedAddress = o.Address
	return nil
}

// waitForAddress confirms the change by connecting to the new address, since the
// command that made it could not report its own success.
func (d *Dialer) waitForAddress(ctx context.Context, o StaticIPOptions, res *StaticIPResult) error {
	deadline := time.Now().Add(time.Duration(o.TimeoutSeconds) * time.Second)
	var lastErr error
	for {
		client, _, err := d.Connect(ctx, o.VMName, sshx.Credential{
			Username: o.Username, Password: o.Password,
		}, o.Address, false)
		if err == nil {
			if _, err := sshx.Exec(ctx, client, "true", "", 20*time.Second); err == nil {
				return nil
			}
		}
		lastErr = err
		d.Pool.Drop(o.VMName)

		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return hverr.Wrap(hverr.OperationTimeout, ctx.Err(), "cancelled while confirming the new address")
		case <-time.After(5 * time.Second):
		}
	}

	detail := "The guest may now be unreachable."
	if res.CheckpointName != "" {
		detail += " Revert with apply_checkpoint using " + res.CheckpointName + "."
	} else {
		detail += " No checkpoint was taken, so recovery needs console access."
	}
	return hverr.Wrap(hverr.OperationTimeout, lastErr,
		"the address was applied but %s never answered", o.Address).WithDetail(detail)
}

func buildNetplan(iface, cidr, gateway string, dns []string) string {
	var b strings.Builder
	b.WriteString("network:\n  version: 2\n  ethernets:\n    " + iface + ":\n")
	b.WriteString("      dhcp4: false\n      addresses: [" + cidr + "]\n")
	if gateway != "" {
		b.WriteString("      routes:\n        - to: default\n          via: " + gateway + "\n")
	}
	if len(dns) > 0 {
		b.WriteString("      nameservers:\n        addresses: [" + strings.Join(dns, ", ") + "]\n")
	}
	return b.String()
}

func manualCommands(iface, cidr, gateway string, dns []string) []string {
	cmds := []string{
		fmt.Sprintf("sudo ip address flush dev %s", iface),
		fmt.Sprintf("sudo ip address add %s dev %s", cidr, iface),
		fmt.Sprintf("sudo ip link set %s up", iface),
	}
	if gateway != "" {
		cmds = append(cmds, fmt.Sprintf("sudo ip route replace default via %s dev %s", gateway, iface))
	}
	if len(dns) > 0 {
		cmds = append(cmds,
			fmt.Sprintf("printf 'nameserver %s\\n' | sudo tee /etc/resolv.conf", strings.Join(dns, "\\nnameserver ")))
	}
	cmds = append(cmds, "# these last only until reboot; write them into the distribution's own configuration to persist")
	return cmds
}
