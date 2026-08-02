package hyperv

import (
	"context"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
)

// RenameVM changes a VM's name in Hyper-V.
//
// Only the name changes. The VM keeps its identifier, and — the part that
// surprises people — its files keep the old name too: the configuration folder,
// the virtual disks and the checkpoint files are all left where they are. Moving
// those means exporting and reimporting, which is a different operation with
// different risks.
//
// Hyper-V permits two VMs to share a name, which is a trap rather than a
// feature: everything here addresses a VM by name, so a duplicate makes it
// ambiguous which one any later call means. That is refused.
func (c *Client) RenameVM(ctx context.Context, oldName, newName string) (*VMDetail, error) {
	switch {
	case oldName == "":
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	case newName == "":
		return nil, hverr.New(hverr.InvalidArgument, "new_name is required")
	case oldName == newName:
		return nil, hverr.New(hverr.InvalidArgument, "%q is already the VM's name", newName)
	}

	const script = requireVM + `
    # Hyper-V would allow a second VM with this name, and then every call that
    # names it would be ambiguous.
    $clash = @(Get-VM -Name $P.new_name -ErrorAction SilentlyContinue)
    if ($clash.Count -gt 0) {
        throw "HVERR:VM_ALREADY_EXISTS|a VM named '$($P.new_name)' already exists. Hyper-V would allow a second one, but every tool here addresses a VM by name and two would be indistinguishable."
    }

    Rename-VM -VM $vm -NewName $P.new_name
    # The projection re-reads the VM by its identifier, which the rename did not
    # change, so it reports the new name without being told it.
` + detailProjection

	var out VMDetail
	err := c.r.RunTimeoutInto(ctx, 2*time.Minute, script, map[string]any{
		"name": oldName, "new_name": newName,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}
