package hyperv

import (
	"context"
	"path/filepath"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/winpath"
)

// ListSwitches returns the host's virtual switches and which VMs are on each.
func (c *Client) ListSwitches(ctx context.Context) ([]VMSwitch, error) {
	const script = `
    $byName = @{}
    foreach ($a in @(Get-VM | Get-VMNetworkAdapter)) {
        if ($a.SwitchName) {
            if (-not $byName.ContainsKey($a.SwitchName)) { $byName[$a.SwitchName] = @() }
            $byName[$a.SwitchName] += $a.VMName
        }
    }
    $result = @(Get-VMSwitch | Sort-Object Name | ForEach-Object {
        [ordered]@{
            name                = $_.Name
            switch_type         = $_.SwitchType.ToString()
            net_adapter_name    = [string]$_.NetAdapterInterfaceDescription
            allow_management_os = [bool]$_.AllowManagementOS
            connected_vms       = @($byName[$_.Name] | Sort-Object -Unique)
        }
    })`

	out := []VMSwitch{}
	if err := c.r.RunInto(ctx, script, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateVM provisions a new virtual machine.
//
// Every path is validated for the service's own access before anything is
// created, so a bad path fails cleanly instead of leaving a half-built VM.
func (c *Client) CreateVM(ctx context.Context, o CreateVMOptions) (*VMDetail, error) {
	if o.Name == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	if o.Generation == 0 {
		o.Generation = 2
	}
	if o.Generation != 1 && o.Generation != 2 {
		return nil, hverr.New(hverr.InvalidArgument, "generation must be 1 or 2, got %d", o.Generation)
	}
	if o.MemoryMB <= 0 {
		o.MemoryMB = 4096
	}
	if o.CPUCount <= 0 {
		o.CPUCount = 2
	}
	if o.VHDSizeMB <= 0 {
		o.VHDSizeMB = 65536 // 64 GB
	}
	switch o.SecureBoot {
	case "", "windows", "linux", "off":
	default:
		return nil, hverr.New(hverr.InvalidArgument,
			`secure_boot must be "windows", "linux" or "off", got %q`, o.SecureBoot)
	}

	args := map[string]any{
		"name":            o.Name,
		"generation":      o.Generation,
		"memory_bytes":    int64(o.MemoryMB) * 1024 * 1024,
		"cpu_count":       o.CPUCount,
		"dynamic_memory":  o.DynamicMemory,
		"vhd_bytes":       int64(o.VHDSizeMB) * 1024 * 1024,
		"switch_name":     o.SwitchName,
		"secure_boot":     o.SecureBoot,
		"vm_path":         "",
		"vhd_path":        "",
		"iso_path":        "",
		"checkpoint_path": "",
		"paging_path":     "",
	}

	if o.VMPath != "" {
		p, err := winpath.ValidateDir(o.VMPath, o.CreateParents)
		if err != nil {
			return nil, err
		}
		args["vm_path"] = p
	}
	if o.VHDPath != "" {
		// Create mode: the file itself may or may not exist yet, but its
		// directory must be writable either way.
		p, err := winpath.Validate(o.VHDPath, winpath.Create, o.CreateParents)
		if err != nil {
			return nil, err
		}
		args["vhd_path"] = p
	}
	if o.ISOPath != "" {
		p, err := winpath.Validate(o.ISOPath, winpath.Read, false)
		if err != nil {
			return nil, err
		}
		args["iso_path"] = p
	}
	if o.CheckpointPath != "" {
		p, err := winpath.ValidateDir(o.CheckpointPath, o.CreateParents)
		if err != nil {
			return nil, err
		}
		args["checkpoint_path"] = p
	}
	if o.SmartPagingPath != "" {
		p, err := winpath.ValidateDir(o.SmartPagingPath, o.CreateParents)
		if err != nil {
			return nil, err
		}
		args["paging_path"] = p
	}

	const script = `
    # New-VM will happily create a second VM with an existing name, which makes
    # every later lookup by name ambiguous.
    if (Get-VM -Name $P.name -ErrorAction SilentlyContinue) {
        throw "HVERR:VM_ALREADY_EXISTS|a VM named '$($P.name)' already exists"
    }

    $newArgs = @{
        Name               = $P.name
        MemoryStartupBytes = [int64]$P.memory_bytes
        Generation         = [int]$P.generation
    }
    if ($P.vm_path)     { $newArgs['Path'] = $P.vm_path }
    if ($P.switch_name) { $newArgs['SwitchName'] = $P.switch_name }
    if ($P.vhd_path) {
        if (Test-Path -LiteralPath $P.vhd_path) {
            $newArgs['VHDPath'] = $P.vhd_path
        } else {
            $newArgs['NewVHDPath']      = $P.vhd_path
            $newArgs['NewVHDSizeBytes'] = [int64]$P.vhd_bytes
        }
    } else {
        $newArgs['NewVHDPath']      = (Join-Path (Get-VMHost).VirtualHardDiskPath ($P.name + '.vhdx'))
        $newArgs['NewVHDSizeBytes'] = [int64]$P.vhd_bytes
    }

    $vm = New-VM @newArgs
    Set-VMProcessor -VM $vm -Count ([int]$P.cpu_count)
    Set-VMMemory -VM $vm -DynamicMemoryEnabled ([bool]$P.dynamic_memory)
    # Client Windows enables automatic checkpoints, which silently snapshots the
    # VM on every start. That is rarely wanted and surprises people when disks fill.
    Set-VM -VM $vm -AutomaticCheckpointsEnabled $false

    if ($P.checkpoint_path) { Set-VM -VM $vm -SnapshotFileLocation $P.checkpoint_path }
    if ($P.paging_path)     { Set-VM -VM $vm -SmartPagingFilePath $P.paging_path }
    if ($P.iso_path)        { Add-VMDvdDrive -VM $vm -Path $P.iso_path }

    if ([int]$P.generation -eq 2) {
        switch ($P.secure_boot) {
            'off'   { Set-VMFirmware -VM $vm -EnableSecureBoot Off }
            # Linux distributions are signed by the third-party UEFI CA, not the
            # Windows one, so the default template refuses to boot them.
            'linux' { Set-VMFirmware -VM $vm -EnableSecureBoot On -SecureBootTemplate 'MicrosoftUEFICertificateAuthority' }
            default { Set-VMFirmware -VM $vm -EnableSecureBoot On -SecureBootTemplate 'MicrosoftWindows' }
        }
        if ($P.iso_path) {
            $dvd = Get-VMDvdDrive -VM $vm | Select-Object -First 1
            Set-VMFirmware -VM $vm -FirstBootDevice $dvd
        }
    }
` + detailProjection

	var out VMDetail
	if err := c.r.RunTimeoutInto(ctx, 120*time.Second, script, args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteVM removes a VM and, optionally, its disks.
//
// A disk is kept when another VM still uses it, or when it is the parent of
// another disk. The result lists what survived and why, because silently
// deleting a shared golden image would be unrecoverable.
func (c *Client) DeleteVM(ctx context.Context, name string, deleteDisks, force bool) (*DeleteVMResult, error) {
	if name == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	const script = requireVM + `
    if ($vm.State -ne 'Off') {
        if (-not $P.force) {
            throw "HVERR:VM_WRONG_STATE|'$($P.name)' is $($vm.State); stop it first or pass force"
        }
        Stop-VM -VM $vm -TurnOff -Force | Out-Null
    }

    $disks = @(Get-VMHardDiskDrive -VM $vm | ForEach-Object { $_.Path })
    Remove-VM -VM $vm -Force | Out-Null

    $deleted = @(); $kept = @(); $reasons = @()
    if ($P.delete_disks) {
        # Snapshot what every surviving VM uses, and what their disks descend
        # from, before deleting anything.
        $inUse   = @(Get-VM | Get-VMHardDiskDrive | ForEach-Object { $_.Path })
        $parents = @()
        foreach ($u in $inUse) {
            $info = Get-VHD -Path $u -ErrorAction SilentlyContinue
            if ($info -and $info.ParentPath) { $parents += $info.ParentPath }
        }
        foreach ($d in $disks) {
            if ($inUse -contains $d) {
                $kept += $d; $reasons += "$d is still attached to another VM"; continue
            }
            if ($parents -contains $d) {
                $kept += $d; $reasons += "$d is the parent of another VM's differencing disk"; continue
            }
            try {
                Remove-Item -LiteralPath $d -Force -ErrorAction Stop
                $deleted += $d
            } catch {
                $kept += $d; $reasons += "$d could not be deleted: $($_.Exception.Message)"
            }
        }
    } else {
        $kept = $disks
        if ($disks.Count -gt 0) { $reasons += 'delete_disks was not set' }
    }

    $result = [ordered]@{
        deleted       = $true
        disks_deleted = @($deleted)
        disks_kept    = @($kept)
        kept_reasons  = @($reasons)
    }`

	var out DeleteVMResult
	err := c.r.RunTimeoutInto(ctx, 120*time.Second, script,
		map[string]any{"name": name, "delete_disks": deleteDisks, "force": force}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateVHD creates a virtual disk file without attaching it.
//
// Sizes are in megabytes throughout, so a caller testing behaviour at a
// particular capacity can name it exactly rather than round to a gigabyte.
func (c *Client) CreateVHD(ctx context.Context, path string, sizeMB int, diskType, parentPath string, createParents bool) (*VHDInfo, error) {
	p, err := winpath.Validate(path, winpath.Create, createParents)
	if err != nil {
		return nil, err
	}
	switch diskType {
	case "", "dynamic":
		diskType = "dynamic"
	case "fixed":
	case "differencing":
		if parentPath == "" {
			return nil, hverr.New(hverr.InvalidArgument, "parent_path is required for a differencing disk")
		}
		if parentPath, err = winpath.Validate(parentPath, winpath.Read, false); err != nil {
			return nil, err
		}
	default:
		return nil, hverr.New(hverr.InvalidArgument,
			`disk_type must be "dynamic", "fixed" or "differencing", got %q`, diskType)
	}
	if diskType != "differencing" {
		if sizeMB <= 0 {
			sizeMB = 65536
		}
		// Hyper-V rounds a VHDX up to a whole megabyte anyway; rejecting three
		// bytes here is better than silently storing something else.
		if sizeMB < 3 {
			return nil, hverr.New(hverr.InvalidArgument,
				"size_mb must be at least 3; Hyper-V cannot create a smaller VHDX")
		}
	}

	const script = `
    switch ($P.disk_type) {
        'fixed'        { New-VHD -Path $P.path -SizeBytes ([int64]$P.size_bytes) -Fixed | Out-Null }
        'differencing' { New-VHD -Path $P.path -ParentPath $P.parent_path -Differencing | Out-Null }
        default        { New-VHD -Path $P.path -SizeBytes ([int64]$P.size_bytes) -Dynamic | Out-Null }
    }
` + vhdProjection

	var out VHDInfo
	// A fixed disk is written out in full, so allow for tens of gigabytes.
	err = c.r.RunTimeoutInto(ctx, 30*time.Minute, script, map[string]any{
		"path":        p,
		"size_bytes":  int64(sizeMB) * 1024 * 1024,
		"disk_type":   diskType,
		"parent_path": parentPath,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// vhdProjection fills $result from $P.path.
const vhdProjection = `
    $h = Get-VHD -Path $P.path
    $result = [ordered]@{
        path            = [string]$h.Path
        format          = $h.VhdFormat.ToString()
        vhd_type        = $h.VhdType.ToString()
        size_bytes      = [int64]$h.Size
        file_size_bytes = [int64]$h.FileSize
        parent_path     = [string]$h.ParentPath
        attached        = [bool]$h.Attached
    }`

// AttachVHD adds an existing disk to a VM.
//
// Controller placement is the caller's to choose, because the guest names its
// devices by controller location: attaching a set at consecutive locations is
// what makes /dev/sdb, sdc and so on line up with what you attached.
func (c *Client) AttachVHD(ctx context.Context, o AttachOptions) (*VMDetail, error) {
	if o.VMName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "vm_name is required")
	}
	p, err := winpath.Validate(o.Path, winpath.Read, false)
	if err != nil {
		return nil, err
	}
	switch o.ControllerType {
	case "", "SCSI", "scsi":
		o.ControllerType = "SCSI"
	case "IDE", "ide":
		o.ControllerType = "IDE"
	default:
		return nil, hverr.New(hverr.InvalidArgument,
			`controller_type must be "SCSI" or "IDE", got %q`, o.ControllerType)
	}

	location := -1
	if o.ControllerLocation != nil {
		location = *o.ControllerLocation
		if location < 0 {
			return nil, hverr.New(hverr.InvalidArgument, "controller_location cannot be negative")
		}
	}

	const script = requireVM + `
    $addArgs = @{
        VM               = $vm
        Path             = $P.path
        ControllerType   = $P.controller_type
        ControllerNumber = [int]$P.controller_number
    }
    # A negative location means "wherever is free", which is Add-VMHardDiskDrive's
    # behaviour when the parameter is left off entirely.
    if ([int]$P.controller_location -ge 0) {
        $addArgs['ControllerLocation'] = [int]$P.controller_location
    }
    Add-VMHardDiskDrive @addArgs | Out-Null
` + detailProjection

	var out VMDetail
	err = c.r.RunTimeoutInto(ctx, 2*time.Minute, script, map[string]any{
		"name":                o.VMName,
		"path":                p,
		"controller_type":     o.ControllerType,
		"controller_number":   o.ControllerNumber,
		"controller_location": location,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DetachVHD removes a disk from a VM without deleting the file.
func (c *Client) DetachVHD(ctx context.Context, vmName, path string) (*VMDetail, error) {
	const script = requireVM + `
    $drive = Get-VMHardDiskDrive -VM $vm | Where-Object { $_.Path -eq $P.path } | Select-Object -First 1
    if (-not $drive) { throw "HVERR:PATH_NOT_FOUND|'$($P.path)' is not attached to '$($P.name)'" }
    Remove-VMHardDiskDrive -VMHardDiskDrive $drive | Out-Null
` + detailProjection

	var out VMDetail
	if err := c.r.RunInto(ctx, script, map[string]any{"name": vmName, "path": filepath.Clean(path)}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateSeedDisk builds a small disk containing answer files for an unattended
// installer, and leaves it detached and ready to attach.
//
// Unattended installers look for their answer file by volume label rather than
// by path: Anaconda picks up /ks.cfg from a volume labelled OEMDRV without any
// boot parameter, and cloud-init reads user-data and meta-data from CIDATA. That
// makes this the one piece needed to install a guest with no console input and
// without rebuilding the installation ISO.
func (c *Client) CreateSeedDisk(ctx context.Context, path, label string, sizeMB int, files []SeedFile, overwrite, createParents bool) (*SeedDiskResult, error) {
	p, err := winpath.Validate(path, winpath.Create, createParents)
	if err != nil {
		return nil, err
	}
	if label == "" {
		return nil, hverr.New(hverr.InvalidArgument, "label is required")
	}
	if len(label) > 11 {
		// FAT volume labels are capped at 11 characters, and a silently
		// truncated label would not be found by the installer.
		return nil, hverr.New(hverr.InvalidArgument,
			"label %q is longer than the 11 characters a FAT volume allows", label)
	}
	if len(files) == 0 {
		return nil, hverr.New(hverr.InvalidArgument, "at least one file is required")
	}
	if sizeMB <= 0 {
		sizeMB = 256 // comfortably above FAT32's minimum volume size
	}

	const script = `
    if (Test-Path -LiteralPath $P.path) {
        if (-not $P.overwrite) { throw "HVERR:VM_ALREADY_EXISTS|'$($P.path)' already exists" }
        Dismount-VHD -Path $P.path -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $P.path -Force
    }

    New-VHD -Path $P.path -SizeBytes ([int64]$P.size_bytes) -Dynamic | Out-Null

    $mountDir = $null
    $mounted  = $false
    try {
        $disk = Mount-VHD -Path $P.path -Passthru | Get-Disk
        $mounted = $true
        Initialize-Disk -Number $disk.Number -PartitionStyle GPT -ErrorAction SilentlyContinue | Out-Null
        $part = New-Partition -DiskNumber $disk.Number -UseMaximumSize
        Format-Volume -Partition $part -FileSystem FAT32 -NewFileSystemLabel $P.label -Force -Confirm:$false | Out-Null

        # Mount at a folder instead of a drive letter: letters are a global,
        # finite resource and this volume exists only for a moment.
        $mountDir = Join-Path $env:TEMP ('seed-' + [guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Path $mountDir -Force | Out-Null
        Add-PartitionAccessPath -DiskNumber $disk.Number -PartitionNumber $part.PartitionNumber -AccessPath $mountDir

        $enc = New-Object System.Text.UTF8Encoding($false)
        foreach ($f in $P.files) {
            $dest = Join-Path $mountDir $f.path
            $destDir = Split-Path -Parent $dest
            if (-not (Test-Path -LiteralPath $destDir)) { New-Item -ItemType Directory -Path $destDir -Force | Out-Null }
            # Answer files are parsed by Linux tooling: no BOM, LF line endings.
            [System.IO.File]::WriteAllText($dest, ($f.content -replace "` + "`r`n" + `", "` + "`n" + `"), $enc)
        }

        Remove-PartitionAccessPath -DiskNumber $disk.Number -PartitionNumber $part.PartitionNumber -AccessPath $mountDir
    } finally {
        if ($mountDir -and (Test-Path -LiteralPath $mountDir)) {
            Remove-Item -LiteralPath $mountDir -Force -Recurse -ErrorAction SilentlyContinue
        }
        if ($mounted) { Dismount-VHD -Path $P.path -ErrorAction SilentlyContinue }
    }

    $result = [ordered]@{
        path       = $P.path
        label      = $P.label
        size_bytes = [int64]$P.size_bytes
        files      = @($P.files | ForEach-Object { $_.path })
    }`

	var out SeedDiskResult
	err = c.r.RunTimeoutInto(ctx, 5*time.Minute, script, map[string]any{
		"path":       p,
		"label":      label,
		"size_bytes": int64(sizeMB) * 1024 * 1024,
		"files":      files,
		"overwrite":  overwrite,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// EjectDVD removes every DVD drive from a VM, which is how you stop a finished
// installer from booting its ISO again.
func (c *Client) EjectDVD(ctx context.Context, vmName string) (*VMDetail, error) {
	const script = requireVM + `
    foreach ($d in @(Get-VMDvdDrive -VM $vm)) { Remove-VMDvdDrive -VMDvdDrive $d }
` + detailProjection

	var out VMDetail
	if err := c.r.RunInto(ctx, script, map[string]any{"name": vmName}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
