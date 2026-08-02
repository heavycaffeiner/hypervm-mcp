package mcpsrv

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hyperv"
)

type attachISOInput struct {
	VMName    string `json:"vm_name" jsonschema:"Exact name of the VM."`
	ISOPath   string `json:"iso_path" jsonschema:"ISO to attach in a new DVD drive."`
	FirstBoot bool   `json:"first_boot,omitempty" jsonschema:"Make the VM boot from this drive first."`
}

func registerDiskTools(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  "add_scsi_controller",
		Title: "Add a SCSI controller",
		Description: "Add a SCSI controller to a stopped VM. A VM starts with one, addressing 64 " +
			"disks; a second is for going beyond that, or for testing how a guest handles disks " +
			"spread across controllers. Four is the maximum.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in vmNameOnlyInput) (*mcp.CallToolResult, *hyperv.VMDetail, error) {
		out, err := d.VM.AddSCSIController(ctx, in.VMName)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "attach_iso",
		Title: "Attach an ISO in a new DVD drive",
		Description: "Add a DVD drive holding an ISO, so a VM can be installed from it or read it. " +
			"Use it more than once to give a VM several discs — an installer on one, an answer file " +
			"on another.\n\n" +
			"Set first_boot to boot from it, which is what a fresh install needs. Once the install " +
			"is done, eject_dvd stops the VM booting the installer again." + pathRules,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in attachISOInput) (*mcp.CallToolResult, *hyperv.VMDetail, error) {
		out, err := d.VM.AttachISO(ctx, in.VMName, in.ISOPath, in.FirstBoot)
		return nil, out, err
	})
}
