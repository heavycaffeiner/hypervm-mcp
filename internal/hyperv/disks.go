package hyperv

import (
	"context"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/winpath"
)

// AttachOptions places a disk on a specific controller port.
//
// Placement is exposed because the guest enumerates SCSI targets by controller
// location: attaching disks at chosen, consecutive locations is what makes their
// device names predictable, which matters when several are meant to form one
// array.
type AttachOptions struct {
	VMName             string
	Path               string
	ControllerType     string // SCSI (default) | IDE
	ControllerNumber   int
	ControllerLocation *int // nil picks the next free port
}

// AddSCSIController adds a SCSI controller to a VM.
//
// A VM starts with one, addressing 64 disks. A second is needed either to go
// beyond that or to test how a guest behaves with disks spread across
// controllers. Four is the maximum, and the VM must be off.
func (c *Client) AddSCSIController(ctx context.Context, vmName string) (*VMDetail, error) {
	if vmName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "vm_name is required")
	}
	const script = requireVM + `
    if ($vm.State -ne 'Off') {
        throw "HVERR:VM_WRONG_STATE|'$($P.name)' is $($vm.State); adding a controller needs it stopped"
    }
    if (@(Get-VMScsiController -VM $vm).Count -ge 4) {
        throw "HVERR:INVALID_ARGUMENT|'$($P.name)' already has the maximum of 4 SCSI controllers"
    }
    Add-VMScsiController -VM $vm | Out-Null
` + detailProjection

	var out VMDetail
	if err := c.r.RunTimeoutInto(ctx, 2*time.Minute, script,
		map[string]any{"name": vmName}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AttachISO adds a DVD drive holding an ISO.
//
// This is how installation media reaches a VM: create the VM, attach the
// installer's ISO, boot from it. Authoring that ISO is somebody else's job.
func (c *Client) AttachISO(ctx context.Context, vmName, isoPath string, firstBoot bool) (*VMDetail, error) {
	if vmName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "vm_name is required")
	}
	iso, err := winpath.Validate(isoPath, winpath.Read, false)
	if err != nil {
		return nil, err
	}

	const script = requireVM + `
    $dvd = Add-VMDvdDrive -VM $vm -Path $P.iso -Passthru
    if ($P.first_boot) {
        if ([int]$vm.Generation -eq 2) {
            Set-VMFirmware -VM $vm -FirstBootDevice $dvd
        } else {
            Set-VMBios -VM $vm -StartupOrder @('CD','IDE','LegacyNetworkAdapter','Floppy')
        }
    }
` + detailProjection

	var out VMDetail
	if err := c.r.RunTimeoutInto(ctx, 2*time.Minute, script, map[string]any{
		"name": vmName, "iso": iso, "first_boot": firstBoot,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
