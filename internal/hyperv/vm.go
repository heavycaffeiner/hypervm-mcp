package hyperv

import (
	"context"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/psrun"
)

// summaryProjection maps a VM object onto VMSummary's JSON keys.
//
// Projections here build ordered hashtables rather than using Select-Object with
// calculated properties, because Select-Object flattens the pipeline output of
// its expressions: an array of one becomes a scalar and an empty array becomes
// {}, neither of which decodes as a list. Hashtable values serialize faithfully.
//
// Each field is also cast or stringified deliberately, since PowerShell renders
// enums as integers and TimeSpan as an object.
const summaryProjection = `ForEach-Object { [ordered]@{
        name            = $_.Name
        id              = $_.Id.ToString()
        state           = $_.State.ToString()
        cpu_usage       = [int]$_.CPUUsage
        memory_assigned = [int64]$_.MemoryAssigned
        uptime_seconds  = [int64]$_.Uptime.TotalSeconds
        generation      = [int]$_.Generation
    } }`

// requireVM resolves $P.name into $vm, or throws.
//
// It distinguishes "no such VM" from a real failure by error category rather
// than by message text: Hyper-V reports a missing VM as InvalidArgument, and
// localizes every message it produces, so matching English strings would break
// on a non-English Windows. Anything that is not InvalidArgument — a permission
// failure, a stopped management service — is rethrown untouched so it keeps its
// own classification.
const requireVM = `
    $vm = Get-VM -Name $P.name -ErrorAction SilentlyContinue -ErrorVariable __getErr
    if (-not $vm) {
        if ($__getErr -and $__getErr.Count -gt 0 -and
            $__getErr[0].CategoryInfo.Category -ne 'InvalidArgument') { throw $__getErr[0] }
        throw "HVERR:VM_NOT_FOUND|no VM named '$($P.name)' on this host"
    }
`

// Client runs Hyper-V operations through a PowerShell runner.
type Client struct {
	r *psrun.Runner
}

func NewClient(r *psrun.Runner) *Client { return &Client{r: r} }

// ListVMs returns every VM, or those matching a wildcard name pattern.
// An empty result is not an error: a filter that matches nothing returns nothing.
func (c *Client) ListVMs(ctx context.Context, pattern string) ([]VMSummary, error) {
	// Filtering in the pipeline rather than with Get-VM -Name keeps real
	// failures visible. Suppressing errors on Get-VM would make a permission
	// problem look identical to "nothing matched".
	const script = `
    $vms = @(Get-VM)
    if ($P.name) { $vms = @($vms | Where-Object { $_.Name -like $P.name }) }
    $result = @($vms | Sort-Object Name | ` + summaryProjection + `)`

	out := []VMSummary{}
	if err := c.r.RunInto(ctx, script, map[string]any{"name": pattern}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// adapterProjection maps virtual NICs, including the addresses the guest reports
// through integration services.
const adapterProjection = `ForEach-Object { [ordered]@{
        name         = $_.Name
        switch_name  = [string]$_.SwitchName
        mac_address  = [string]$_.MacAddress
        ip_addresses = @($_.IPAddresses | Where-Object { $_ })
        connected    = [bool]$_.Connected
    } }`

// detailProjection fills $result with a VMDetail. It expects $vm to be set and
// re-reads it first, so it is equally correct after a mutation.
const detailProjection = `
    $vm = Get-VM -Id $vm.Id
    $mem = Get-VMMemory -VM $vm
    $cpu = Get-VMProcessor -VM $vm
    $drives = @(Get-VMHardDiskDrive -VM $vm | ForEach-Object { [ordered]@{
        controller_type     = $_.ControllerType.ToString()
        controller_number   = [int]$_.ControllerNumber
        controller_location = [int]$_.ControllerLocation
        path                = [string]$_.Path
    } })
    $nics = @(Get-VMNetworkAdapter -VM $vm | ` + adapterProjection + `)
    $snaps = @(Get-VMSnapshot -VM $vm -ErrorAction SilentlyContinue)
    $result = [ordered]@{
        name                     = $vm.Name
        id                       = $vm.Id.ToString()
        state                    = $vm.State.ToString()
        cpu_usage                = [int]$vm.CPUUsage
        memory_assigned          = [int64]$vm.MemoryAssigned
        uptime_seconds           = [int64]$vm.Uptime.TotalSeconds
        generation               = [int]$vm.Generation
        notes                    = [string]$vm.Notes
        configuration_location   = [string]$vm.ConfigurationLocation
        checkpoint_file_location = [string]$vm.SnapshotFileLocation
        smart_paging_file_path   = [string]$vm.SmartPagingFilePath
        processor_count          = [int]$cpu.Count
        nested_virtualization    = [bool]$cpu.ExposeVirtualizationExtensions
        memory_startup           = [int64]$mem.Startup
        dynamic_memory_enabled   = [bool]$mem.DynamicMemoryEnabled
        memory_minimum           = [int64]$mem.Minimum
        memory_maximum           = [int64]$mem.Maximum
        checkpoint_count         = [int]$snaps.Count
        hard_drives              = $drives
        network_adapters         = $nics
    }`

// GetVM returns full detail for one VM.
func (c *Client) GetVM(ctx context.Context, name string) (*VMDetail, error) {
	if name == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	var out VMDetail
	if err := c.r.RunInto(ctx, requireVM+detailProjection, map[string]any{"name": name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StartVM powers on a VM. Already-running is a no-op that succeeds, and a saved
// VM is resumed. This returns as soon as Hyper-V reports Running, which is well
// before the guest OS has finished booting; use WaitForGuestIP for that.
func (c *Client) StartVM(ctx context.Context, name string) (*VMSummary, error) {
	if name == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	const script = requireVM + `
    if ($vm.State -ne 'Running') { Start-VM -VM $vm | Out-Null }
    $result = Get-VM -Name $P.name | ` + summaryProjection

	return c.runSummary(ctx, 0, script, map[string]any{"name": name})
}

// StopVM shuts a VM down.
//
// force powers the VM off immediately, which can corrupt the guest filesystem.
// Otherwise an ACPI shutdown request is sent and the script polls until the VM
// reaches Off or timeoutSec elapses.
func (c *Client) StopVM(ctx context.Context, name string, force bool, timeoutSec int) (*VMSummary, error) {
	if name == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	if timeoutSec <= 0 {
		timeoutSec = 120
	}
	const script = requireVM + `
    if ($vm.State -ne 'Off') {
        if ($P.force) {
            Stop-VM -VM $vm -TurnOff -Force | Out-Null
        } else {
            # -Force here only suppresses the confirmation prompt; Stop-VM still
            # performs a graceful ACPI shutdown.
            Stop-VM -VM $vm -Force | Out-Null
            $deadline = (Get-Date).AddSeconds([int]$P.timeout)
            while ((Get-VM -Name $P.name).State -ne 'Off') {
                if ((Get-Date) -gt $deadline) {
                    throw "HVERR:OPERATION_TIMEOUT|'$($P.name)' did not shut down within $($P.timeout)s"
                }
                Start-Sleep -Milliseconds 500
            }
        }
    }
    $result = Get-VM -Name $P.name | ` + summaryProjection

	// Give the interpreter headroom past the in-script deadline so the script's
	// own timeout message wins over a process kill.
	budget := time.Duration(timeoutSec+30) * time.Second
	return c.runSummary(ctx, budget, script,
		map[string]any{"name": name, "force": force, "timeout": timeoutSec})
}

// RestartVM stops then starts a VM. force has the same meaning as in StopVM.
func (c *Client) RestartVM(ctx context.Context, name string, force bool, timeoutSec int) (*VMSummary, error) {
	if _, err := c.StopVM(ctx, name, force, timeoutSec); err != nil {
		return nil, err
	}
	return c.StartVM(ctx, name)
}

// SuspendVM pauses a running VM. mode "save" writes VM state to disk and frees
// its memory; mode "pause" leaves it resident.
func (c *Client) SuspendVM(ctx context.Context, name, mode string) (*VMSummary, error) {
	if name == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	switch mode {
	case "save", "pause":
	case "":
		mode = "save"
	default:
		return nil, hverr.New(hverr.InvalidArgument, "mode must be \"save\" or \"pause\", got %q", mode)
	}
	const script = requireVM + `
    if ($vm.State -ne 'Running') {
        throw "HVERR:VM_WRONG_STATE|'$($P.name)' is $($vm.State); only a Running VM can be suspended"
    }
    if ($P.mode -eq 'save') { Save-VM -VM $vm | Out-Null }
    else                    { Suspend-VM -VM $vm | Out-Null }
    $result = Get-VM -Name $P.name | ` + summaryProjection

	return c.runSummary(ctx, 0, script, map[string]any{"name": name, "mode": mode})
}

// ResumeVM returns a paused or saved VM to Running.
func (c *Client) ResumeVM(ctx context.Context, name string) (*VMSummary, error) {
	if name == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	const script = requireVM + `
    switch ($vm.State) {
        'Paused'  { Resume-VM -VM $vm | Out-Null }
        'Saved'   { Start-VM  -VM $vm | Out-Null }
        'Running' { }
        default   {
            throw "HVERR:VM_WRONG_STATE|'$($P.name)' is $($vm.State); only a Paused or Saved VM can be resumed"
        }
    }
    $result = Get-VM -Name $P.name | ` + summaryProjection

	return c.runSummary(ctx, 0, script, map[string]any{"name": name})
}

// SetNestedVirtualization exposes the host's virtualization extensions to a
// guest, so the guest can run a hypervisor of its own.
//
// Two prerequisites are enforced here rather than by Hyper-V, which was measured
// to enforce neither:
//
// The VM must be Off. Set-VMProcessor accepts the change on a running VM,
// reports success, and does not apply it: the setting reads back unchanged, then
// still unchanged once the VM stops. Passing that through would mean telling a
// caller nested virtualization is on when nothing happened.
//
// Dynamic memory is turned off when enabling. A guest hypervisor needs its
// memory backed for real, and Hyper-V will start the VM with both settings on
// and leave the breakage to appear inside the guest. Turning it off here is the
// only way through this server, which has no tool for memory, and the returned
// detail reports the new value.
func (c *Client) SetNestedVirtualization(ctx context.Context, name string, enabled bool) (*VMDetail, error) {
	if name == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}

	const script = requireVM + `
    if ($vm.State -ne 'Off') {
        throw "HVERR:VM_WRONG_STATE|'$($P.name)' is $($vm.State); nested virtualization can only be changed while the VM is Off. Hyper-V accepts the change on a running VM and then silently ignores it."
    }
    if ($P.enabled -and (Get-VMMemory -VM $vm).DynamicMemoryEnabled) {
        Set-VMMemory -VM $vm -DynamicMemoryEnabled $false | Out-Null
    }
    Set-VMProcessor -VM $vm -ExposeVirtualizationExtensions ([bool]$P.enabled) | Out-Null
` + detailProjection

	var out VMDetail
	if err := c.r.RunTimeoutInto(ctx, 2*time.Minute, script,
		map[string]any{"name": name, "enabled": enabled}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// runSummary is the shared tail of every state transition: run, decode, and turn
// an empty result into VM_NOT_FOUND.
func (c *Client) runSummary(ctx context.Context, timeout time.Duration, script string, args map[string]any) (*VMSummary, error) {
	var out VMSummary
	if err := c.r.RunTimeoutInto(ctx, timeout, script, args, &out); err != nil {
		return nil, err
	}
	if out.Name == "" {
		return nil, hverr.New(hverr.VMNotFound, "no VM named %q on this host", args["name"])
	}
	return &out, nil
}
