// Package mcpsrv builds the MCP server the service exposes over the named pipe.
//
// A fresh server is constructed per connection so protocol state stays isolated
// between MCP sessions, while the dependencies it closes over (the PowerShell
// runner, and later the tunnel manager) are process-wide singletons.
package mcpsrv

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heavycaffeiner/hypervm-mcp/internal/creds"
	"github.com/heavycaffeiner/hypervm-mcp/internal/guest"
	"github.com/heavycaffeiner/hypervm-mcp/internal/hyperv"
	"github.com/heavycaffeiner/hypervm-mcp/internal/logx"
	"github.com/heavycaffeiner/hypervm-mcp/internal/sshx"
	"github.com/heavycaffeiner/hypervm-mcp/internal/tailnet"
	"github.com/heavycaffeiner/hypervm-mcp/internal/tunnel"
)

// Deps are the shared services every tool handler draws on. They are
// process-wide singletons, unlike the per-connection server that closes over them.
type Deps struct {
	VM       *hyperv.Client
	Guest    *guest.Dialer
	Creds    *creds.Store
	SSH      *sshx.Pool
	HostKeys *sshx.HostKeys
	Tunnels  *tunnel.Manager
	Tailnet  *tailnet.Client
	Log      *logx.Logger
}

const instructions = `Controls Hyper-V virtual machines on this Windows host.

Two things are worth knowing up front.

Hyper-V reports a VM as Running well before its guest OS has finished booting.
After start_vm, call wait_for_guest_ip before assuming the VM is reachable.

There are two ways to reach a service inside a VM, and they solve different
problems. A tunnel forwards a host port and works for anything whose client can
be pointed at a port; use mode "ssh" when the service is bound to the guest's own
127.0.0.1, because no direct route reaches that. An External virtual switch gives
the guest its own address on the physical LAN, which is the only option when the
host already holds the port (SMB, RDP, WinRM), when the protocol is sensitive to
host identity, or when other machines must reach the guest directly.`

// New returns a server with every tool registered, ready to run over a transport.
func New(version string, d *Deps) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:        "hypervm-mcp",
		Title:       "Hyper-V Manager",
		Description: "Manage Hyper-V virtual machines without per-session privilege elevation.",
		Version:     version,
	}, &mcp.ServerOptions{
		Instructions: instructions,
	})

	registerVMTools(s, d)
	registerCheckpointTools(s, d)
	registerProvisionTools(s, d)
	registerStorageTools(s, d)
	registerDiskTools(s, d)
	registerNetworkTools(s, d)
	registerGuestTools(s, d)
	registerSSHTools(s, d)
	registerTunnelTools(s, d)
	registerDoctorTool(s, d)
	return s
}

// ptr is a helper for the SDK's optional-bool annotation fields.
func ptr[T any](v T) *T { return &v }
