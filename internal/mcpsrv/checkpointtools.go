package mcpsrv

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hyperv"
)

type createCheckpointInput struct {
	VMName       string `json:"vm_name" jsonschema:"Exact name of the VM."`
	SnapshotName string `json:"snapshot_name,omitempty" jsonschema:"Name for the checkpoint. Empty lets Hyper-V generate a timestamped one."`
}

type checkpointInput struct {
	VMName       string `json:"vm_name" jsonschema:"Exact name of the VM."`
	SnapshotName string `json:"snapshot_name" jsonschema:"Name of the checkpoint."`
}

type applyCheckpointInput struct {
	VMName       string `json:"vm_name" jsonschema:"Exact name of the VM."`
	SnapshotName string `json:"snapshot_name" jsonschema:"Checkpoint to revert to."`
	AutoStop     bool   `json:"auto_stop,omitempty" jsonschema:"Power the VM off first if it is running. Without this a running VM is refused, since applying a checkpoint always stops it."`
}

type deleteCheckpointInput struct {
	VMName          string `json:"vm_name" jsonschema:"Exact name of the VM."`
	SnapshotName    string `json:"snapshot_name" jsonschema:"Checkpoint to remove."`
	IncludeChildren bool   `json:"include_children,omitempty" jsonschema:"Also remove every checkpoint descended from it."`
}

func registerCheckpointTools(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  "create_checkpoint",
		Title: "Create a checkpoint",
		Description: "Snapshot a VM so you can return to this exact state. Works on a running VM, " +
			"where it also saves memory and so takes time proportional to how much is assigned.\n\n" +
			"Take one before anything hard to undo — reconfiguring the guest network, changing " +
			"virtual switches, or an in-place upgrade.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createCheckpointInput) (*mcp.CallToolResult, *hyperv.Checkpoint, error) {
		out, err := d.VM.CreateCheckpoint(ctx, in.VMName, in.SnapshotName)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_checkpoints",
		Title:       "List checkpoints",
		Description: "List a VM's checkpoints oldest first, so each one's parent appears before it.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in vmNameOnlyInput) (*mcp.CallToolResult, *listOf[hyperv.Checkpoint], error) {
		return list(d.VM.ListCheckpoints(ctx, in.VMName))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "apply_checkpoint",
		Title: "Revert to a checkpoint",
		Description: "Return a VM to a checkpoint, DISCARDING every change made since it was taken.\n\n" +
			"Tunnels to this VM survive: they re-resolve its address on the next connection. SSH may " +
			"not — reverting past the point where the guest generated its host keys changes them, and " +
			"ssh_exec will report SSH_HOST_KEY_MISMATCH until you pass trust_new_key.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in applyCheckpointInput) (*mcp.CallToolResult, *hyperv.VMSummary, error) {
		out, err := d.VM.ApplyCheckpoint(ctx, in.VMName, in.SnapshotName, in.AutoStop)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "delete_checkpoint",
		Title: "Delete a checkpoint",
		Description: "Remove a checkpoint and merge its differencing disk back into the parent. " +
			"This waits for the merge, which Hyper-V performs asynchronously — returning earlier " +
			"would leave the VM unable to start with nothing to explain why.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deleteCheckpointInput) (*mcp.CallToolResult, map[string]any, error) {
		if err := d.VM.DeleteCheckpoint(ctx, in.VMName, in.SnapshotName, in.IncludeChildren); err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"deleted": in.SnapshotName, "vm_name": in.VMName}, nil
	})
}

// vmNameOnlyInput is for tools that key on vm_name rather than name.
type vmNameOnlyInput struct {
	VMName string `json:"vm_name" jsonschema:"Exact name of the VM."`
}
