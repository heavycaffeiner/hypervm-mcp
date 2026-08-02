package mcpsrv

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/hyperv"
)

type guestInvokeInput struct {
	VMName         string `json:"vm_name" jsonschema:"Exact name of the VM."`
	Command        string `json:"command" jsonschema:"PowerShell command to run inside the guest."`
	Username       string `json:"username,omitempty" jsonschema:"Overrides the stored username."`
	Password       string `json:"password,omitempty" jsonschema:"Overrides the stored password."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Default 120."`
}

type sendKeyInput struct {
	VMName     string   `json:"vm_name" jsonschema:"Exact name of the VM. It must be running."`
	Keys       []string `json:"keys" jsonschema:"Keys to press in order: names such as \"space\", \"enter\", \"esc\", \"f8\", \"up\", or virtual-key codes such as \"0x20\"."`
	Repeat     int      `json:"repeat,omitempty" jsonschema:"How many times to send the sequence. Default 1. Use ~20 to cover a boot prompt whose timing is unknown."`
	IntervalMS int      `json:"interval_ms,omitempty" jsonschema:"Milliseconds between repeats. Default 1000."`
}

type guestCopyInput struct {
	VMName          string `json:"vm_name" jsonschema:"Exact name of the VM."`
	SourcePath      string `json:"source_path" jsonschema:"File on the host to copy."`
	DestinationPath string `json:"destination_path" jsonschema:"Where to put it inside the guest."`
	CreateFullPath  bool   `json:"create_full_path,omitempty" jsonschema:"Create missing directories in the guest."`
	Overwrite       bool   `json:"overwrite,omitempty" jsonschema:"Replace an existing file."`
}

func registerGuestTools(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  "guest_invoke_command",
		Title: "Run a command in a Windows guest without networking",
		Description: "Run a PowerShell command inside a Windows guest over PowerShell Direct.\n\n" +
			"This travels over the VMBus rather than the network, so it works on a VM with no " +
			"address, no switch, or a Private switch. That makes it the way to bootstrap a guest " +
			"before anything else can reach it — installing sshd, or changing network settings that " +
			"would cut an SSH session mid-command.\n\n" +
			"Windows guests only; Linux has no PowerShell Direct endpoint, so use ssh_exec there. " +
			"It also needs a password: a key is not enough for this transport.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in guestInvokeInput) (*mcp.CallToolResult, *hyperv.GuestResult, error) {
		user, pass := in.Username, in.Password
		if user == "" || pass == "" {
			stored, ok, err := d.Creds.Get(in.VMName)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				return nil, nil, hverr.New(hverr.CredentialNotFound,
					"no credentials stored for %q", in.VMName).
					WithDetail("Run: hypervm-mcp cred set --vm " + in.VMName + " --user <name>")
			}
			if user == "" {
				user = stored.Username
			}
			if pass == "" {
				pass = stored.Password
			}
		}
		out, err := d.VM.GuestInvokeCommand(ctx, in.VMName, in.Command, user, pass,
			time.Duration(in.TimeoutSeconds)*time.Second)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "send_vm_key",
		Title: "Press keys at a VM's console",
		Description: "Type into a VM's console keyboard, before it has an operating system.\n\n" +
			"This is for prompts nothing inside the guest can answer. Microsoft's installation " +
			"media asks \"Press any key to boot from CD or DVD\" and gives up after a few seconds, " +
			"so an unattended install from a stock ISO never starts unless something presses a " +
			"key — and guest_invoke_command, ssh_exec and guest_copy_file all need a booted " +
			"guest, which is exactly what does not exist yet.\n\n" +
			"The prompt is a race: it opens a few seconds after power-on and closes again. Rather " +
			"than time it, set repeat and interval_ms to press steadily across the window — " +
			"20 presses a second apart covers a normal boot.\n\n" +
			"Keys are names (\"space\", \"enter\", \"esc\", \"f8\", arrows) or virtual-key codes " +
			"(\"0x20\"). This types single keys; it is not a way to drive an installer by hand.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sendKeyInput) (*mcp.CallToolResult, *hyperv.KeyResult, error) {
		if len(in.Keys) == 0 {
			return nil, nil, hverr.New(hverr.InvalidArgument, "keys is required")
		}
		codes := make([]int, 0, len(in.Keys))
		for _, k := range in.Keys {
			code, err := hyperv.ParseKey(k)
			if err != nil {
				return nil, nil, err
			}
			codes = append(codes, code)
		}
		out, err := d.VM.SendKeys(ctx, in.VMName, codes, in.Repeat,
			time.Duration(in.IntervalMS)*time.Millisecond)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "guest_copy_file",
		Title: "Copy a file into a guest without networking",
		Description: "Copy a file from the host into a running guest over the VMBus, so it needs no " +
			"guest network.\n\n" +
			"It does need the Guest Service Interface integration component, which this enables if it " +
			"is switched off — on Linux that component is hypervfcopyd from the hyperv-daemons " +
			"package.\n\n" +
			"Only host to guest; Hyper-V offers nothing for the reverse. To bring a file back, use " +
			"ssh_exec or a tunnel." + pathRules,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in guestCopyInput) (*mcp.CallToolResult, map[string]any, error) {
		err := d.VM.GuestCopyFile(ctx, in.VMName, in.SourcePath, in.DestinationPath,
			in.CreateFullPath, in.Overwrite)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{
			"copied":      true,
			"vm_name":     in.VMName,
			"destination": in.DestinationPath,
		}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "get_guest_network",
		Title: "Guest network adapters",
		Description: "Report a VM's virtual network adapters with the switch each is on and the " +
			"addresses the guest reports. An empty address list is normal shortly after boot, and " +
			"permanent on a guest without the reporting agent — it does not mean the guest is " +
			"unreachable. Use diagnose_vm_network to tell those apart.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in vmNameOnlyInput) (*mcp.CallToolResult, *listOf[hyperv.NetworkAdapter], error) {
		return list(d.VM.GetNetworkAdapters(ctx, in.VMName))
	})
}
