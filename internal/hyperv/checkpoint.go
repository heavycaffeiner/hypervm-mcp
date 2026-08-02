package hyperv

import (
	"context"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
)

// checkpointProjection maps a VMSnapshot onto Checkpoint's JSON keys.
const checkpointProjection = `ForEach-Object { [ordered]@{
        name            = $_.Name
        vm_name         = $_.VMName
        id              = $_.Id.ToString()
        parent_name     = [string]$_.ParentSnapshotName
        created_at      = $_.CreationTime.ToUniversalTime().ToString('o')
        checkpoint_type = $_.SnapshotType.ToString()
        path            = [string]$_.Path
    } }`

// requireSnapshot resolves $P.snapshot_name into $snap.
const requireSnapshot = `
    $snap = Get-VMSnapshot -VM $vm -Name $P.snapshot_name -ErrorAction SilentlyContinue
    if (-not $snap) {
        throw "HVERR:CHECKPOINT_NOT_FOUND|'$($P.name)' has no checkpoint named '$($P.snapshot_name)'"
    }
    $snap = @($snap)[0]
`

// CreateCheckpoint snapshots a VM.
//
// An empty name lets Hyper-V generate a timestamped one.
func (c *Client) CreateCheckpoint(ctx context.Context, vmName, snapshotName string) (*Checkpoint, error) {
	if vmName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "vm_name is required")
	}
	const script = requireVM + `
    if ($P.snapshot_name) {
        Checkpoint-VM -VM $vm -SnapshotName $P.snapshot_name -Confirm:$false | Out-Null
        $made = Get-VMSnapshot -VM $vm -Name $P.snapshot_name
    } else {
        Checkpoint-VM -VM $vm -Confirm:$false | Out-Null
        # Hyper-V names it after the moment it was taken; the newest is ours.
        $made = Get-VMSnapshot -VM $vm | Sort-Object CreationTime | Select-Object -Last 1
    }
    $result = $made | ` + checkpointProjection

	var out Checkpoint
	// Checkpointing a running VM saves its memory, which takes time proportional
	// to how much is assigned.
	if err := c.r.RunTimeoutInto(ctx, 10*time.Minute, script,
		map[string]any{"name": vmName, "snapshot_name": snapshotName}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListCheckpoints returns a VM's checkpoints oldest first, so the parent of each
// appears before it.
func (c *Client) ListCheckpoints(ctx context.Context, vmName string) ([]Checkpoint, error) {
	if vmName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "vm_name is required")
	}
	const script = requireVM + `
    $result = @(Get-VMSnapshot -VM $vm -ErrorAction SilentlyContinue |
        Sort-Object CreationTime | ` + checkpointProjection + `)`

	out := []Checkpoint{}
	if err := c.r.RunInto(ctx, script, map[string]any{"name": vmName}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ApplyCheckpoint reverts a VM to a checkpoint, discarding everything since.
//
// Hyper-V will not restore into a running VM, so this stops it first when
// autoStop is set and refuses otherwise rather than powering off a VM the caller
// may not expect to lose.
func (c *Client) ApplyCheckpoint(ctx context.Context, vmName, snapshotName string, autoStop bool) (*VMSummary, error) {
	if vmName == "" || snapshotName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "vm_name and snapshot_name are required")
	}
	const script = requireVM + requireSnapshot + `
    if ($vm.State -ne 'Off') {
        if (-not $P.auto_stop) {
            throw "HVERR:VM_WRONG_STATE|'$($P.name)' is $($vm.State); applying a checkpoint stops it, so pass auto_stop to allow that"
        }
        Stop-VM -VM $vm -TurnOff -Force | Out-Null
    }
    Restore-VMSnapshot -VMSnapshot $snap -Confirm:$false | Out-Null
    $result = Get-VM -Id $vm.Id | ` + summaryProjection

	return c.runSummary(ctx, 10*time.Minute, script, map[string]any{
		"name": vmName, "snapshot_name": snapshotName, "auto_stop": autoStop,
	})
}

// DeleteCheckpoint removes a checkpoint and waits for its differencing disk to
// merge back into the parent.
//
// The wait matters: Hyper-V merges asynchronously, so returning as soon as the
// checkpoint disappears would leave the VM unable to start and the disk still
// growing, with nothing to show why.
func (c *Client) DeleteCheckpoint(ctx context.Context, vmName, snapshotName string, includeChildren bool) error {
	if vmName == "" || snapshotName == "" {
		return hverr.New(hverr.InvalidArgument, "vm_name and snapshot_name are required")
	}
	const script = requireVM + requireSnapshot + `
    if ($P.include_children) {
        Remove-VMSnapshot -VMSnapshot $snap -IncludeAllChildSnapshots -Confirm:$false | Out-Null
    } else {
        Remove-VMSnapshot -VMSnapshot $snap -Confirm:$false | Out-Null
    }

    # Hyper-V reports the merge through the VM's status text.
    $deadline = (Get-Date).AddMinutes(30)
    while ((Get-VM -Id $vm.Id).Status -match 'Merging') {
        if ((Get-Date) -gt $deadline) {
            throw "HVERR:OPERATION_TIMEOUT|the disk merge for '$($P.name)' did not finish within 30 minutes"
        }
        Start-Sleep -Seconds 2
    }
    $result = $true`

	_, err := c.r.RunTimeout(ctx, 35*time.Minute, script, map[string]any{
		"name": vmName, "snapshot_name": snapshotName, "include_children": includeChildren,
	})
	return err
}
