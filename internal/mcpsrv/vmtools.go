package mcpsrv

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hyperv"
)

// ---- Tool inputs -----------------------------------------------------------

type listVMsInput struct {
	Name string `json:"name,omitempty" jsonschema:"Wildcard filter on VM name, e.g. \"Dev-*\". Empty returns every VM."`
}

type vmNameInput struct {
	Name string `json:"name" jsonschema:"Exact name of the VM."`
}

type stopVMInput struct {
	Name           string `json:"name" jsonschema:"Exact name of the VM."`
	Force          bool   `json:"force,omitempty" jsonschema:"Power the VM off immediately instead of asking the guest to shut down. May corrupt the guest filesystem."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"How long to wait for a graceful shutdown. Default 120. Ignored when force is true."`
}

type suspendVMInput struct {
	Name string `json:"name" jsonschema:"Exact name of the VM."`
	Mode string `json:"mode,omitempty" jsonschema:"\"save\" writes VM state to disk and frees its memory; \"pause\" leaves it resident. Default \"save\"."`
}

type waitForGuestIPInput struct {
	Name           string `json:"name" jsonschema:"Exact name of the VM."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"How long to wait. Default 120."`
	Subnet         string `json:"subnet,omitempty" jsonschema:"Only accept an address inside this CIDR range, e.g. \"192.168.0.0/24\". Useful when the VM is attached to more than one switch."`
	AllowLinkLocal bool   `json:"allow_link_local,omitempty" jsonschema:"Accept a 169.254.x.x address. Normally that means DHCP has not finished, but on an Internal or Private switch nothing hands out addresses, so it may be all a guest ever has."`
}

// ---- Registration ----------------------------------------------------------

func registerVMTools(s *mcp.Server, d *Deps) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}

	mcp.AddTool(s, &mcp.Tool{
		Name:  "list_vms",
		Title: "List VMs",
		Description: "List the virtual machines on this Hyper-V host, sorted by name. " +
			"A filter that matches nothing returns an empty list rather than an error.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listVMsInput) (*mcp.CallToolResult, *listOf[hyperv.VMSummary], error) {
		return list(d.VM.ListVMs(ctx, in.Name))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "get_vm",
		Title: "Get VM detail",
		Description: "Full detail for one VM: state, CPU and memory settings, attached disks with their " +
			"on-disk paths, network adapters with reported guest IPs, checkpoint count, and the " +
			"storage locations Hyper-V uses for its configuration, checkpoints and paging file.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in vmNameInput) (*mcp.CallToolResult, *hyperv.VMDetail, error) {
		out, err := d.VM.GetVM(ctx, in.Name)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "start_vm",
		Title: "Start VM",
		Description: "Power on a VM, or resume it if it was saved. Starting an already-running VM succeeds " +
			"and changes nothing. This returns as soon as Hyper-V reports Running, which happens long " +
			"before the guest OS is reachable — follow it with wait_for_guest_ip.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in vmNameInput) (*mcp.CallToolResult, *hyperv.VMSummary, error) {
		out, err := d.VM.StartVM(ctx, in.Name)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "stop_vm",
		Title: "Stop VM",
		Description: "Shut a VM down. By default the guest OS is asked to shut down and the call waits for it. " +
			"Set force to cut power immediately, which is faster but can corrupt the guest filesystem. " +
			"Stopping an already-stopped VM succeeds and changes nothing.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in stopVMInput) (*mcp.CallToolResult, *hyperv.VMSummary, error) {
		out, err := d.VM.StopVM(ctx, in.Name, in.Force, in.TimeoutSeconds)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "restart_vm",
		Title:       "Restart VM",
		Description: "Stop a VM and start it again. force has the same meaning and the same risk as in stop_vm.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in stopVMInput) (*mcp.CallToolResult, *hyperv.VMSummary, error) {
		out, err := d.VM.RestartVM(ctx, in.Name, in.Force, in.TimeoutSeconds)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "suspend_vm",
		Title: "Suspend VM",
		Description: "Suspend a running VM. Mode \"save\" writes its state to disk and releases its memory; " +
			"mode \"pause\" keeps it in memory and resumes faster. Use resume_vm to bring it back.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in suspendVMInput) (*mcp.CallToolResult, *hyperv.VMSummary, error) {
		out, err := d.VM.SuspendVM(ctx, in.Name, in.Mode)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "resume_vm",
		Title:       "Resume VM",
		Description: "Return a paused or saved VM to the running state.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in vmNameInput) (*mcp.CallToolResult, *hyperv.VMSummary, error) {
		out, err := d.VM.ResumeVM(ctx, in.Name)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "wait_for_guest_ip",
		Title: "Wait for guest IP",
		Description: "Wait until the guest OS reports a usable IP address, and return it along with every " +
			"address it reported. This is the correct way to wait for a VM to become reachable after " +
			"start_vm: the Running state says nothing about whether the guest network is up. " +
			"Link-local and loopback addresses are ignored because they mean DHCP has not finished.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in waitForGuestIPInput) (*mcp.CallToolResult, *hyperv.GuestIPResult, error) {
		out, err := d.VM.WaitForGuestIP(ctx, in.Name, in.Subnet, in.AllowLinkLocal,
			time.Duration(in.TimeoutSeconds)*time.Second)
		return nil, out, err
	})
}
