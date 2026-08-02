package mcpsrv

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/tailnet"
	"github.com/heavycaffeiner/hypervm-mcp/internal/tunnel"
)

type openTunnelInput struct {
	VMName      string `json:"vm_name" jsonschema:"Exact name of the VM."`
	GuestPort   int    `json:"guest_port" jsonschema:"Port inside the guest to forward to."`
	HostPort    int    `json:"host_port,omitempty" jsonschema:"Port to listen on. 0 lets the OS pick a free one, which is returned."`
	Mode        string `json:"mode,omitempty" jsonschema:"\"direct\" (default) dials the guest's address; \"ssh\" forwards through the guest's sshd and is the only way to reach a service bound to the guest's own 127.0.0.1."`
	BindScope   string `json:"bind_scope,omitempty" jsonschema:"\"loopback\" (default) for this host only, \"tailnet\" for tailnet peers, \"all\" for every network the host is on, or an IP address."`
	GuestHost   string `json:"guest_host,omitempty" jsonschema:"Address to reach the VM at, instead of asking Hyper-V. Needed when the guest has no Hyper-V agent reporting its IP, which is common on minimal Linux installs."`
	AutoRestore *bool  `json:"auto_restore,omitempty" jsonschema:"Reopen this tunnel when the service restarts. Default true."`
	Label       string `json:"label,omitempty" jsonschema:"Free-form note shown in list_tunnels."`
}

type closeTunnelInput struct {
	ID string `json:"id" jsonschema:"Tunnel id from open_tunnel or list_tunnels."`
}

type listTunnelsInput struct {
	VMName string `json:"vm_name,omitempty" jsonschema:"Only tunnels for this VM. Empty returns all."`
}

func registerTunnelTools(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  "open_tunnel",
		Title: "Open a tunnel into a VM",
		Description: "Forward a host port to a port inside a VM. The tunnel lives in the service, so " +
			"it outlives this session and is reopened when the service restarts.\n\n" +
			"Pick the mode by where the guest service is bound. \"direct\" dials the guest's own " +
			"address and needs the service listening on 0.0.0.0 or the guest's LAN address. \"ssh\" " +
			"opens a channel through the guest's sshd, which makes the guest's 127.0.0.1 reachable — " +
			"nothing else can do that.\n\n" +
			"Pick bind_scope by who should reach it: loopback for this host, tailnet for peers on " +
			"your Tailscale network (a firewall rule scoped to those addresses is created and removed " +
			"with the tunnel), all for every network the host is attached to.\n\n" +
			"NOT EVERY SERVICE CAN BE TUNNELLED. A tunnel needs a free host port, and the client must " +
			"be able to be pointed at it. SMB (445), RDP (3389), WinRM (5985) and NetBIOS are held by " +
			"Windows itself, and an SMB client cannot even express a non-default port in a UNC path. " +
			"For those, put the VM on an External switch so it gets its own LAN address.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in openTunnelInput) (*mcp.CallToolResult, *tunnel.Tunnel, error) {
		autoRestore := true
		if in.AutoRestore != nil {
			autoRestore = *in.AutoRestore
		}
		out, err := d.Tunnels.Open(ctx, tunnel.Spec{
			VMName:      in.VMName,
			GuestPort:   in.GuestPort,
			HostPort:    in.HostPort,
			Mode:        in.Mode,
			BindScope:   in.BindScope,
			GuestHost:   in.GuestHost,
			AutoRestore: autoRestore,
			Label:       in.Label,
		})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "list_tunnels",
		Title: "List tunnels",
		Description: "List the tunnels the service currently holds, including ones opened by another " +
			"session or restored at startup, with their traffic counters and the last error each hit.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listTunnelsInput) (*mcp.CallToolResult, []tunnel.Tunnel, error) {
		return nil, d.Tunnels.List(in.VMName), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "close_tunnel",
		Title: "Close a tunnel",
		Description: "Stop a tunnel, drop its in-flight connections immediately, remove its firewall " +
			"rule, and forget it so it is not reopened on restart.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in closeTunnelInput) (*mcp.CallToolResult, map[string]any, error) {
		if err := d.Tunnels.Close(ctx, in.ID); err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"closed": in.ID}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "tailscale_serve",
		Title: "Put Tailscale HTTPS in front of a tunnel",
		Description: "Serve an existing loopback tunnel over HTTPS at " +
			"https://<host>.<tailnet>.ts.net<path>, with a certificate Tailscale issues and renews. " +
			"This is for HTTP services; for arbitrary TCP use a tunnel with bind_scope \"tailnet\" " +
			"instead.\n\n" +
			"The tunnel must be bound to loopback, since that is what Tailscale forwards to. A tunnel " +
			"already bound to the tailnet would be reachable twice — once in plain TCP and once over " +
			"HTTPS — which the result warns about rather than refusing.\n\n" +
			"This never enables Funnel, so nothing is published to the public internet.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in tailscaleServeInput) (*mcp.CallToolResult, map[string]any, error) {
		found := findTunnel(d, in.TunnelID)
		if found == nil {
			return nil, nil, hverr.New(hverr.TunnelNotFound, "no tunnel with id %q", in.TunnelID)
		}

		if in.Off {
			if err := d.Tailnet.ServeOff(ctx, in.HTTPSPort, in.Path); err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"tunnel_id": in.TunnelID, "serving": false}, nil
		}

		url, err := d.Tailnet.Serve(ctx, in.HTTPSPort, in.Path, found.HostPort)
		if err != nil {
			return nil, nil, err
		}
		out := map[string]any{
			"tunnel_id": in.TunnelID,
			"serving":   true,
			"url":       url,
			"backend":   fmt.Sprintf("http://127.0.0.1:%d", found.HostPort),
		}
		if found.BindScope != "loopback" {
			out["warnings"] = []string{
				"this tunnel is bound to " + found.BindScope + " as well, so the service is now " +
					"reachable both in plain TCP on that address and over HTTPS here",
			}
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "tailnet_status",
		Title: "Tailscale status",
		Description: "Report this host's Tailscale state: whether the CLI is present, whether the " +
			"backend is logged in, the host's tailnet addresses and MagicDNS name, and which tunnels " +
			"are currently bound to those addresses. Check this before opening a tunnel with " +
			"bind_scope \"tailnet\".",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, *tailnet.Status, error) {
		status, err := d.Tailnet.Status(ctx)
		if err != nil {
			// A missing or logged-out Tailscale is a state worth reporting, not a
			// failure that should hide the rest of the answer.
			if status != nil {
				status.ExposedTunnels = exposedTunnels(d)
				return nil, status, nil
			}
			return nil, nil, err
		}
		status.ExposedTunnels = exposedTunnels(d)
		return nil, status, nil
	})
}

type tailscaleServeInput struct {
	TunnelID  string `json:"tunnel_id" jsonschema:"Tunnel to put behind HTTPS. It must be bound to loopback."`
	Path      string `json:"path,omitempty" jsonschema:"URL path to serve at. Default \"/\"."`
	HTTPSPort int    `json:"https_port,omitempty" jsonschema:"HTTPS port. Default 443."`
	Off       bool   `json:"off,omitempty" jsonschema:"Remove the mapping instead of creating it."`
}

func findTunnel(d *Deps, id string) *tunnel.Tunnel {
	for _, t := range d.Tunnels.List("") {
		if t.ID == id {
			return &t
		}
	}
	return nil
}

func exposedTunnels(d *Deps) []string {
	out := []string{}
	for _, t := range d.Tunnels.List("") {
		if t.BindScope == "tailnet" {
			out = append(out, t.ID)
		}
	}
	return out
}
