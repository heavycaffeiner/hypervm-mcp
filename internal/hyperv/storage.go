package hyperv

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/winpath"
)

// readOnlyFile reports whether a file carries Windows' read-only attribute,
// which Go surfaces by clearing the owner write bit.
func readOnlyFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().Perm()&0o200 == 0
}

// GetHostStoragePaths reports where Hyper-V puts things by default, and whether
// the service can actually write there.
//
// The accessibility check matters because the defaults often sit under a user
// profile or a drive that has since gone away, and the failure would otherwise
// only surface halfway through creating a VM.
func (c *Client) GetHostStoragePaths(ctx context.Context) (*HostStoragePaths, error) {
	const script = `
    $h = Get-VMHost
    $result = [ordered]@{
        virtual_machine_path  = [string]$h.VirtualMachinePath
        virtual_hard_disk_path = [string]$h.VirtualHardDiskPath
        free_space_bytes = @{}
    }
    foreach ($v in @(Get-Volume | Where-Object { $_.DriveLetter })) {
        $result.free_space_bytes[[string]$v.DriveLetter + ':'] = [int64]$v.SizeRemaining
    }`

	var out HostStoragePaths
	if err := c.r.RunInto(ctx, script, nil, &out); err != nil {
		return nil, err
	}
	out.VMPathAccessible = writable(out.VirtualMachinePath)
	out.VHDPathAccessible = writable(out.VirtualHardDiskPath)
	return &out, nil
}

func writable(path string) bool {
	if path == "" {
		return false
	}
	_, err := winpath.ValidateDir(path, false)
	return err == nil
}

// SetHostStoragePaths changes the host-wide defaults. Existing VMs stay where
// they are; only later ones are affected.
func (c *Client) SetHostStoragePaths(ctx context.Context, vmPath, vhdPath string, createParents bool) (*HostStoragePaths, error) {
	if vmPath == "" && vhdPath == "" {
		return nil, hverr.New(hverr.InvalidArgument,
			"give at least one of virtual_machine_path or virtual_hard_disk_path")
	}
	args := map[string]any{"vm_path": "", "vhd_path": ""}
	if vmPath != "" {
		p, err := winpath.ValidateDir(vmPath, createParents)
		if err != nil {
			return nil, err
		}
		args["vm_path"] = p
	}
	if vhdPath != "" {
		p, err := winpath.ValidateDir(vhdPath, createParents)
		if err != nil {
			return nil, err
		}
		args["vhd_path"] = p
	}

	const script = `
    $setArgs = @{}
    if ($P.vm_path)  { $setArgs['VirtualMachinePath']  = $P.vm_path }
    if ($P.vhd_path) { $setArgs['VirtualHardDiskPath'] = $P.vhd_path }
    Set-VMHost @setArgs | Out-Null
    $result = $true`

	if _, err := c.r.RunTimeout(ctx, 60*time.Second, script, args); err != nil {
		return nil, err
	}
	return c.GetHostStoragePaths(ctx)
}

// GetVHDInfo reports a disk's type, size and parent chain.
func (c *Client) GetVHDInfo(ctx context.Context, path string) (*VHDInfo, error) {
	p, err := winpath.Validate(path, winpath.Read, false)
	if err != nil {
		return nil, err
	}
	var out VHDInfo
	if err := c.r.RunInto(ctx, vhdProjection, map[string]any{"path": p}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ResizeVHD changes a disk's virtual size. The VM must be off.
//
// Growing the file does not grow the filesystem inside it; that is a guest-side
// step this tool deliberately leaves alone.
func (c *Client) ResizeVHD(ctx context.Context, path string, sizeMB int) (*VHDInfo, error) {
	if sizeMB <= 0 {
		return nil, hverr.New(hverr.InvalidArgument, "size_mb must be positive")
	}
	p, err := winpath.Validate(path, winpath.Read, false)
	if err != nil {
		return nil, err
	}

	const script = `
    $current = Get-VHD -Path $P.path
    if ($current.Attached) {
        throw "HVERR:VM_WRONG_STATE|'$($P.path)' is attached to a running VM; stop it first"
    }
    Resize-VHD -Path $P.path -SizeBytes ([int64]$P.size_bytes)
` + vhdProjection

	var out VHDInfo
	if err := c.r.RunTimeoutInto(ctx, 30*time.Minute, script, map[string]any{
		"path": p, "size_bytes": int64(sizeMB) * 1024 * 1024,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ConvertVHD writes a converted copy, leaving the source untouched.
func (c *Client) ConvertVHD(ctx context.Context, src, dst, format, diskType string, createParents bool) (*VHDInfo, error) {
	s, err := winpath.Validate(src, winpath.Read, false)
	if err != nil {
		return nil, err
	}
	d, err := winpath.Validate(dst, winpath.Create, createParents)
	if err != nil {
		return nil, err
	}
	switch format {
	case "", "VHDX", "vhdx":
		format = "VHDX"
	case "VHD", "vhd":
		format = "VHD"
	default:
		return nil, hverr.New(hverr.InvalidArgument, `format must be "VHD" or "VHDX", got %q`, format)
	}
	switch diskType {
	case "", "dynamic":
		diskType = "Dynamic"
	case "fixed":
		diskType = "Fixed"
	default:
		return nil, hverr.New(hverr.InvalidArgument, `disk_type must be "dynamic" or "fixed", got %q`, diskType)
	}

	const script = `
    Convert-VHD -Path $P.source -DestinationPath $P.path -VHDFormat $P.format -VHDType $P.disk_type
` + vhdProjection

	var out VHDInfo
	if err := c.r.RunTimeoutInto(ctx, 2*time.Hour, script, map[string]any{
		"source": s, "path": d, "format": format, "disk_type": diskType,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateVMFromTemplate provisions a VM in seconds by giving it a differencing
// disk over an existing image, instead of installing an OS.
//
// The clone inherits the image's machine identity — hostname, SSH host keys,
// machine-id, and on Windows the SID. Running several at once causes collisions
// on the network, which the warnings say so the caller can generalize the guest.
func (c *Client) CreateVMFromTemplate(ctx context.Context, o CreateVMOptions, parentVHD string) (*VMDetail, []string, error) {
	if o.Name == "" {
		return nil, nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	parent, err := winpath.Validate(parentVHD, winpath.Read, false)
	if err != nil {
		return nil, nil, hverr.New(hverr.ParentVHDNotFound, "%v", err)
	}

	// Default the child disk next to the parent, named after the VM.
	child := o.VHDPath
	if child == "" {
		child = filepath.Join(filepath.Dir(parent), o.Name+".vhdx")
	}
	childPath, err := winpath.Validate(child, winpath.Create, o.CreateParents)
	if err != nil {
		return nil, nil, err
	}

	var warnings []string
	info, err := c.GetVHDInfo(ctx, parent)
	if err != nil {
		return nil, nil, err
	}
	// A parent that anything can write to will eventually be written to, and
	// every child breaks the moment it is. Sharing one parent across VMs is the
	// intended use, though, so this warns rather than refuses.
	if !readOnlyFile(parent) {
		warnings = append(warnings,
			"the parent image is writable; changing it will corrupt every VM built on it. "+
				"Consider marking "+parent+" read-only.")
	}
	if info.VHDType == "Differencing" {
		warnings = append(warnings,
			"the parent is itself a differencing disk, so this VM depends on the whole chain")
	}

	if _, err := c.CreateVHD(ctx, childPath, 0, "differencing", parent, o.CreateParents); err != nil {
		return nil, nil, err
	}

	o.VHDPath = childPath
	detail, err := c.CreateVM(ctx, o)
	if err != nil {
		return nil, nil, err
	}

	warnings = append(warnings,
		"this VM inherits the image's identity: hostname, machine-id and SSH host keys. "+
			"Running clones side by side will collide on the network — regenerate them in the guest "+
			"(sysprep on Windows; ssh-keygen -A and a new /etc/machine-id on Linux).",
		// Learned the hard way: this fails at start time with a file-in-use error
		// that says nothing about differencing disks.
		"the VM holding the parent image must stay powered off while this clone runs. A running VM "+
			"opens its disk exclusively, and a differencing disk cannot read a parent held that way — "+
			"starting the clone would fail with a file-in-use error. Keep the template shut down and "+
			"run clones from it.")
	return detail, warnings, nil
}

// ExportVM writes a self-contained copy of a VM into exportPath/<name>/.
//
// This is also the only way to move a VM's configuration directory, which is
// fixed when the VM is created: export it, then import it somewhere else.
func (c *Client) ExportVM(ctx context.Context, name, exportPath string, createParents bool) (map[string]any, error) {
	if name == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	dir, err := winpath.ValidateDir(exportPath, createParents)
	if err != nil {
		return nil, err
	}

	const script = requireVM + `
    Export-VM -VM $vm -Path $P.export_path
    $dest = Join-Path $P.export_path $vm.Name
    $result = [ordered]@{
        exported    = $true
        vm_name     = $vm.Name
        path        = $dest
        size_bytes  = [int64](Get-ChildItem -LiteralPath $dest -Recurse -File |
                              Measure-Object -Property Length -Sum).Sum
    }`

	var out map[string]any
	// Exporting copies every disk, so this is bounded by disk size, not by any
	// Hyper-V operation.
	if err := c.r.RunTimeoutInto(ctx, 4*time.Hour, script,
		map[string]any{"name": name, "export_path": dir}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ImportVM registers a VM from an exported directory.
//
// copy=false registers the files where they lie, which is fast but ties the VM
// to that location. generateNewID is required when the original is still
// registered, since two VMs cannot share an identifier.
func (c *Client) ImportVM(ctx context.Context, exportedPath, newName, vmPath, vhdPath string, copyFiles, generateNewID, createParents bool) (*VMDetail, error) {
	src, err := winpath.ValidateDir(exportedPath, false)
	if err != nil {
		return nil, err
	}
	args := map[string]any{
		"source":   src,
		"new_name": newName,
		"vm_path":  "",
		"vhd_path": "",
		"copy":     copyFiles,
		"new_id":   generateNewID,
	}
	if vmPath != "" {
		p, err := winpath.ValidateDir(vmPath, createParents)
		if err != nil {
			return nil, err
		}
		args["vm_path"] = p
	}
	if vhdPath != "" {
		p, err := winpath.ValidateDir(vhdPath, createParents)
		if err != nil {
			return nil, err
		}
		args["vhd_path"] = p
	}

	const script = `
    # Export-VM writes <path>/<vm name>/Virtual Machines/<guid>.vmcx; accept
    # either the directory that holds it or the export root.
    $vmcx = @(Get-ChildItem -LiteralPath $P.source -Recurse -Filter *.vmcx -File -ErrorAction SilentlyContinue)
    if ($vmcx.Count -eq 0) {
        throw "HVERR:PATH_NOT_FOUND|no exported VM configuration (.vmcx) found under '$($P.source)'"
    }
    $importArgs = @{ Path = $vmcx[0].FullName }
    if ($P.copy) {
        $importArgs['Copy'] = $true
        if ($P.vm_path)  { $importArgs['VirtualMachinePath'] = $P.vm_path; $importArgs['SnapshotFilePath'] = $P.vm_path; $importArgs['SmartPagingFilePath'] = $P.vm_path }
        if ($P.vhd_path) { $importArgs['VhdDestinationPath'] = $P.vhd_path }
    } else {
        $importArgs['Register'] = $true
    }
    if ($P.new_id) { $importArgs['GenerateNewId'] = $true }

    $vm = Import-VM @importArgs
    if ($P.new_name -and $P.new_name -ne $vm.Name) {
        Rename-VM -VM $vm -NewName $P.new_name | Out-Null
        $vm = Get-VM -Id $vm.Id
    }
` + detailProjection

	var out VMDetail
	if err := c.r.RunTimeoutInto(ctx, 4*time.Hour, script, args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
