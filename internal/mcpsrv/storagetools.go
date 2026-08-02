package mcpsrv

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hyperv"
)

type setHostPathsInput struct {
	VirtualMachinePath  string `json:"virtual_machine_path,omitempty" jsonschema:"New default directory for VM configuration files."`
	VirtualHardDiskPath string `json:"virtual_hard_disk_path,omitempty" jsonschema:"New default directory for virtual hard disks."`
	CreateParents       bool   `json:"create_parents,omitempty" jsonschema:"Create the directories if they are missing."`
}

type vhdPathInput struct {
	Path string `json:"path" jsonschema:"Full path of the .vhdx file."`
}

type resizeVHDInput struct {
	Path   string `json:"path" jsonschema:"Full path of the .vhdx file."`
	SizeMB int    `json:"size_mb" jsonschema:"New virtual size in MB."`
}

type convertVHDInput struct {
	SourcePath      string `json:"source_path" jsonschema:"Disk to convert. It is left untouched."`
	DestinationPath string `json:"destination_path" jsonschema:"Where to write the converted copy."`
	Format          string `json:"format,omitempty" jsonschema:"\"VHDX\" (default) or \"VHD\"."`
	DiskType        string `json:"disk_type,omitempty" jsonschema:"\"dynamic\" (default) or \"fixed\"."`
	CreateParents   bool   `json:"create_parents,omitempty"`
}

type templateVMInput struct {
	Name          string `json:"name" jsonschema:"Name for the new VM."`
	ParentVHDPath string `json:"parent_vhd_path" jsonschema:"Golden image the new disk will be based on."`
	VMPath        string `json:"vm_path,omitempty" jsonschema:"Directory for the VM configuration. Fixed at creation."`
	VHDPath       string `json:"vhd_path,omitempty" jsonschema:"Where to put the differencing disk. Defaults to beside the parent."`
	Generation    int    `json:"generation,omitempty" jsonschema:"Must match the image. Default 2."`
	MemoryMB      int    `json:"memory_mb,omitempty"`
	CPUCount      int    `json:"cpu_count,omitempty"`
	SwitchName    string `json:"switch_name,omitempty"`
	StaticMAC     string `json:"static_mac,omitempty" jsonschema:"Fixed MAC, so a router DHCP reservation can give the clone a stable address."`
	SecureBoot    string `json:"secure_boot,omitempty" jsonschema:"\"windows\" (default), \"linux\", or \"off\". Must match the image."`
	CreateParents bool   `json:"create_parents,omitempty"`
}

type templateVMResult struct {
	VM       *hyperv.VMDetail `json:"vm"`
	Warnings []string         `json:"warnings,omitempty"`
}

type exportVMInput struct {
	Name          string `json:"name" jsonschema:"VM to export."`
	ExportPath    string `json:"export_path" jsonschema:"Directory to export into; a subdirectory named after the VM is created."`
	CreateParents bool   `json:"create_parents,omitempty"`
}

type importVMInput struct {
	ExportedPath  string `json:"exported_path" jsonschema:"Directory holding an exported VM."`
	NewName       string `json:"new_name,omitempty" jsonschema:"Rename the imported VM."`
	VMPath        string `json:"vm_path,omitempty" jsonschema:"Where to place the configuration. Only used with copy."`
	VHDPath       string `json:"vhd_path,omitempty" jsonschema:"Where to place the disks. Only used with copy."`
	Copy          bool   `json:"copy,omitempty" jsonschema:"Copy the files before registering, instead of using them where they lie."`
	GenerateNewID bool   `json:"generate_new_id,omitempty" jsonschema:"Assign a new VM id. Required when the original is still registered."`
	CreateParents bool   `json:"create_parents,omitempty"`
}

func registerStorageTools(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  "get_host_storage_paths",
		Title: "Host default storage paths",
		Description: "Report where Hyper-V puts VM configurations and disks by default, whether the " +
			"service can actually write there, and how much room each drive has left. Worth checking " +
			"before creating a VM without explicit paths, so you know where the files will land.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, *hyperv.HostStoragePaths, error) {
		out, err := d.VM.GetHostStoragePaths(ctx)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "set_host_storage_paths",
		Title:       "Set host default storage paths",
		Description: "Change the host-wide defaults. Existing VMs stay where they are; only later ones are affected." + pathRules,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setHostPathsInput) (*mcp.CallToolResult, *hyperv.HostStoragePaths, error) {
		out, err := d.VM.SetHostStoragePaths(ctx, in.VirtualMachinePath, in.VirtualHardDiskPath, in.CreateParents)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_vhd_info",
		Title:       "Virtual disk detail",
		Description: "Report a disk's format, type, virtual and on-disk size, and its parent if it is a differencing disk.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in vhdPathInput) (*mcp.CallToolResult, *hyperv.VHDInfo, error) {
		out, err := d.VM.GetVHDInfo(ctx, in.Path)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "resize_vhd",
		Title: "Resize a virtual disk",
		Description: "Change a disk's virtual size. The disk must not be attached to a running VM.\n\n" +
			"This grows the container, not the filesystem inside it. Extending the partition and " +
			"filesystem is a guest-side step this tool deliberately leaves to you.\n\n" +
			"Shrinking discards whatever lies beyond the new size, so back the disk up first if " +
			"the guest has ever written near the end of it.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in resizeVHDInput) (*mcp.CallToolResult, *hyperv.VHDInfo, error) {
		out, err := d.VM.ResizeVHD(ctx, in.Path, in.SizeMB)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "convert_vhd",
		Title:       "Convert a virtual disk",
		Description: "Write a converted copy of a disk, between VHD and VHDX and between dynamic and fixed. The source is left untouched." + pathRules,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in convertVHDInput) (*mcp.CallToolResult, *hyperv.VHDInfo, error) {
		out, err := d.VM.ConvertVHD(ctx, in.SourcePath, in.DestinationPath, in.Format, in.DiskType, in.CreateParents)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "create_vm_from_template",
		Title: "Clone a VM from a golden image",
		Description: "Provision a VM in seconds by giving it a differencing disk over an existing " +
			"image, instead of installing an operating system.\n\n" +
			"Three things to know.\n\n" +
			"The parent image must not change while any clone exists — writing to it corrupts every " +
			"child — so keep it read-only.\n\n" +
			"The VM holding the parent must stay powered off while clones run. A running VM opens its " +
			"disk exclusively and a differencing disk cannot read a parent held that way, so the " +
			"clone fails to start with a file-in-use error that says nothing about the real cause. " +
			"The intended shape is a template that never runs, with clones started from it.\n\n" +
			"The clone inherits the image's identity: hostname, machine-id, SSH host keys, and on " +
			"Windows the machine SID. Running clones side by side collides on the network until you " +
			"regenerate those in the guest.\n\n" +
			"generation and secure_boot must match the image, or it will not boot.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in templateVMInput) (*mcp.CallToolResult, *templateVMResult, error) {
		vm, warnings, err := d.VM.CreateVMFromTemplate(ctx, hyperv.CreateVMOptions{
			Name:          in.Name,
			Generation:    in.Generation,
			MemoryMB:      in.MemoryMB,
			CPUCount:      in.CPUCount,
			VMPath:        in.VMPath,
			VHDPath:       in.VHDPath,
			SwitchName:    in.SwitchName,
			SecureBoot:    in.SecureBoot,
			CreateParents: in.CreateParents,
		}, in.ParentVHDPath)
		if err != nil {
			return nil, nil, err
		}
		if in.StaticMAC != "" {
			if _, err := d.VM.SetVMNetwork(ctx, hyperv.SetVMNetworkOptions{
				VMName: in.Name, StaticMAC: in.StaticMAC,
			}); err != nil {
				warnings = append(warnings, "the VM was created but its MAC could not be set: "+err.Error())
			}
		}
		return nil, &templateVMResult{VM: vm, Warnings: warnings}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "export_vm",
		Title: "Export a VM",
		Description: "Write a self-contained copy of a VM — configuration and every disk — into a " +
			"subdirectory named after it. This is how you produce a golden image, and the only way " +
			"to move a VM's configuration directory, which is fixed when the VM is created.\n\n" +
			"The VM may be running; Hyper-V exports a consistent point-in-time copy." + pathRules,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in exportVMInput) (*mcp.CallToolResult, map[string]any, error) {
		out, err := d.VM.ExportVM(ctx, in.Name, in.ExportPath, in.CreateParents)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "import_vm",
		Title: "Import an exported VM",
		Description: "Register a VM from an exported directory.\n\n" +
			"Without copy the files are used where they lie, which is fast but ties the VM to that " +
			"location. With copy they are placed at vm_path and vhd_path first.\n\n" +
			"generate_new_id is required when the original is still registered, since two VMs cannot " +
			"share an identifier." + pathRules,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in importVMInput) (*mcp.CallToolResult, *hyperv.VMDetail, error) {
		out, err := d.VM.ImportVM(ctx, in.ExportedPath, in.NewName, in.VMPath, in.VHDPath,
			in.Copy, in.GenerateNewID, in.CreateParents)
		return nil, out, err
	})
}
