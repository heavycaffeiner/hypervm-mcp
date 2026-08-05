package mcpsrv

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hyperv"
)

// ---- Tool inputs -----------------------------------------------------------
//
// Optional settings are pointers throughout. For a percentage, a byte count or a
// flag, the zero value is a real setting a caller may want, so it cannot double
// as "leave this alone".

type setVMMemoryInput struct {
	Name           string `json:"name" jsonschema:"Exact name of the VM."`
	StartupMB      *int   `json:"startup_mb,omitempty" jsonschema:"Memory at power-on, in MB. With dynamic memory off this is simply the VM's memory. Needs the VM stopped."`
	Dynamic        *bool  `json:"dynamic,omitempty" jsonschema:"Let Hyper-V grow and shrink the VM's memory between minimum and maximum. Needs the VM stopped. Turn it off for a VM running its own hypervisor or a latency-sensitive workload."`
	MinimumMB      *int   `json:"minimum_mb,omitempty" jsonschema:"Floor Hyper-V may shrink to while the VM is idle. Only takes effect with dynamic memory on."`
	MaximumMB      *int   `json:"maximum_mb,omitempty" jsonschema:"Ceiling Hyper-V may grow to under pressure. Only takes effect with dynamic memory on."`
	BufferPercent  *int   `json:"buffer_percent,omitempty" jsonschema:"Headroom kept above what the guest is actually using, 5 to 2000. Higher absorbs sudden demand at the cost of holding host memory."`
	PriorityWeight *int   `json:"priority_weight,omitempty" jsonschema:"Who wins when the host cannot satisfy every VM, 0 to 100. Default 50."`
}

type setVMProcessorInput struct {
	Name                      string `json:"name" jsonschema:"Exact name of the VM."`
	Count                     *int   `json:"count,omitempty" jsonschema:"Number of virtual processors. Needs the VM stopped; Hyper-V has no CPU hot-add."`
	ReservePercent            *int   `json:"reserve_percent,omitempty" jsonschema:"Share of one logical processor guaranteed to this VM, 0 to 100. Reserving more than the host can honour stops other VMs from starting."`
	MaximumPercent            *int   `json:"maximum_percent,omitempty" jsonschema:"Ceiling on the share of one logical processor this VM may use, 0 to 100. Default 100."`
	RelativeWeight            *int   `json:"relative_weight,omitempty" jsonschema:"How this VM's demand is weighed against other VMs competing for CPU, 1 to 10000. Default 100. Only matters under contention."`
	HwThreadCountPerCore      *int   `json:"hw_thread_count_per_core,omitempty" jsonschema:"0 follows the host, 1 hides SMT from the guest, 2 exposes it. Needs the VM stopped."`
	CompatibilityForMigration *bool  `json:"compatibility_for_migration,omitempty" jsonschema:"Present only the CPU features common to older processors, so the VM can move to a different host. Costs the guest access to newer instruction sets. Needs the VM stopped."`
	HostResourceProtection    *bool  `json:"host_resource_protection,omitempty" jsonschema:"Throttle a guest whose activity is degrading the host."`
}

type setVMFirmwareInput struct {
	Name                  string   `json:"name" jsonschema:"Exact name of the VM."`
	BootOrder             []string `json:"boot_order,omitempty" jsonschema:"Devices to try, in order. Each entry is \"disk\", \"dvd\", \"network\", \"floppy\" or \"file\", optionally narrowed with a colon: \"disk:D:\\\\VMs\\\\os.vhdx\" or \"network:Network Adapter\". A bare class means every device of that class, in its current order. Simplest is to take the token get_vm_settings reports for each entry and send them back in the order you want. On a Generation 1 VM only the classes exist, and anything you leave out keeps its current place at the end."`
	SecureBoot            string   `json:"secure_boot,omitempty" jsonschema:"\"windows\", \"linux\" or \"off\". Generation 2 only. A Linux distribution needs \"linux\": it is signed by the third-party UEFI CA, which the Windows template does not trust, and boots to a security violation otherwise."`
	ConsoleMode           string   `json:"console_mode,omitempty" jsonschema:"Where the firmware draws: \"Default\" to the video device, \"COM1\" or \"COM2\" to a serial port, \"None\" nowhere. Generation 2 only."`
	PauseAfterBootFailure *bool    `json:"pause_after_boot_failure,omitempty" jsonschema:"Stop at the firmware screen when nothing boots instead of retrying, so capture_vm_screen can show why. Generation 2 only."`
	NumLock               *bool    `json:"num_lock,omitempty" jsonschema:"Num Lock state at power-on. Generation 1 only."`
}

type setVMOptionsInput struct {
	Name  string  `json:"name" jsonschema:"Exact name of the VM."`
	Notes *string `json:"notes,omitempty" jsonschema:"Free-form note stored with the VM. An empty string clears it."`

	AutomaticStartAction       string `json:"automatic_start_action,omitempty" jsonschema:"What the host does to this VM when it boots: \"Nothing\", \"StartIfRunning\" to restore what was running, or \"Start\" always."`
	AutomaticStartDelaySeconds *int   `json:"automatic_start_delay_seconds,omitempty" jsonschema:"Wait this long before starting, so several VMs do not contend for disk at once."`
	AutomaticStopAction        string `json:"automatic_stop_action,omitempty" jsonschema:"What the host does to this VM when it shuts down: \"Save\" its state, \"TurnOff\" abruptly, or \"ShutDown\" the guest gracefully. Needs the VM stopped to change."`

	AutomaticCriticalErrorAction         string `json:"automatic_critical_error_action,omitempty" jsonschema:"\"Pause\" the VM when its storage becomes unavailable, or \"None\" to let it fail."`
	AutomaticCriticalErrorTimeoutMinutes *int   `json:"automatic_critical_error_action_timeout_minutes,omitempty" jsonschema:"How long to stay paused waiting for storage to come back before giving up."`

	CheckpointType              string `json:"checkpoint_type,omitempty" jsonschema:"\"Production\" asks the guest to quiesce and is application-consistent; \"Standard\" saves live memory too, which captures running state but is not safe to restore for a database; \"ProductionOnly\" refuses rather than falling back; \"Disabled\" forbids checkpoints entirely."`
	AutomaticCheckpointsEnabled *bool  `json:"automatic_checkpoints_enabled,omitempty" jsonschema:"Snapshot the VM on every start. On by default on client Windows, which is rarely wanted and quietly fills disks."`

	CheckpointFileLocation string `json:"checkpoint_file_location,omitempty" jsonschema:"Directory for checkpoint files."`
	SmartPagingFilePath    string `json:"smart_paging_file_path,omitempty" jsonschema:"Directory for the smart paging file, used only when a VM restarts and its minimum memory is not available."`
	CreateParents          bool   `json:"create_parents,omitempty" jsonschema:"Create those directories if they do not exist."`

	EnhancedSessionTransportType string `json:"enhanced_session_transport_type,omitempty" jsonschema:"\"HvSocket\" carries an enhanced session over the VMBus and needs no guest network; \"VMBus\" is the older path."`
	GuestControlledCacheTypes    *bool  `json:"guest_controlled_cache_types,omitempty" jsonschema:"Let the guest choose memory cache types. Needed by some device passthrough and GPU workloads."`
	LockOnDisconnect             *bool  `json:"lock_on_disconnect,omitempty" jsonschema:"Lock the guest's desktop when a console session disconnects."`
}

type setVMIntegrationServicesInput struct {
	Name                  string `json:"name" jsonschema:"Exact name of the VM."`
	GuestServiceInterface *bool  `json:"guest_service_interface,omitempty" jsonschema:"File copy into the guest. guest_copy_file needs this on. Off by default."`
	Heartbeat             *bool  `json:"heartbeat,omitempty" jsonschema:"Report that the guest is alive and responding."`
	KeyValuePairExchange  *bool  `json:"key_value_pair_exchange,omitempty" jsonschema:"How Hyper-V learns the guest's IP addresses and OS. Turning this off blinds wait_for_guest_ip and get_vm."`
	Shutdown              *bool  `json:"shutdown,omitempty" jsonschema:"Let the host ask the guest to shut down. stop_vm without force needs this on."`
	TimeSynchronization   *bool  `json:"time_synchronization,omitempty" jsonschema:"Keep the guest clock matched to the host. Turn it off for a domain controller or anything testing clock skew."`
	VSS                   *bool  `json:"vss,omitempty" jsonschema:"Volume Shadow Copy, which is what makes a Production checkpoint application-consistent."`
}

type setVMSecurityInput struct {
	Name                            string `json:"name" jsonschema:"Exact name of the VM."`
	TPMEnabled                      *bool  `json:"tpm_enabled,omitempty" jsonschema:"Give the VM a virtual TPM. Windows 11 refuses to install without one."`
	EncryptStateAndMigrationTraffic *bool  `json:"encrypt_state_and_migration_traffic,omitempty" jsonschema:"Encrypt saved state, checkpoints and live-migration traffic."`
}

type setVMVideoInput struct {
	Name                 string `json:"name" jsonschema:"Exact name of the VM."`
	ResolutionType       string `json:"resolution_type,omitempty" jsonschema:"\"Single\" pins one resolution, \"Maximum\" caps it, \"Default\" leaves it to Hyper-V. Giving a resolution implies \"Single\"."`
	HorizontalResolution int    `json:"horizontal_resolution,omitempty" jsonschema:"Console width in pixels, e.g. 1920."`
	VerticalResolution   int    `json:"vertical_resolution,omitempty" jsonschema:"Console height in pixels, e.g. 1080."`
}

type setVMComPortInput struct {
	Name         string `json:"name" jsonschema:"Exact name of the VM."`
	Number       int    `json:"number" jsonschema:"Which serial port, 1 or 2."`
	Path         string `json:"path,omitempty" jsonschema:"Host named pipe to attach, e.g. \"\\\\\\\\.\\\\pipe\\\\myvm-com1\". Only a named pipe works."`
	Detach       bool   `json:"detach,omitempty" jsonschema:"Disconnect the port instead of attaching it."`
	DebuggerMode *bool  `json:"debugger_mode,omitempty" jsonschema:"Tune the pipe for a kernel debugger rather than a console."`
}

type setVMDiskSettingsInput struct {
	Name                          string `json:"name" jsonschema:"Exact name of the VM."`
	Path                          string `json:"path" jsonschema:"Path of the attached disk to change, exactly as get_vm_settings reports it."`
	MinimumIOPS                   *int64 `json:"minimum_iops,omitempty" jsonschema:"Reserved floor in normalized 8 KB operations per second. 0 reserves nothing."`
	MaximumIOPS                   *int64 `json:"maximum_iops,omitempty" jsonschema:"Ceiling in normalized 8 KB operations per second. 0 is unlimited. A 32 KB read costs four."`
	SupportPersistentReservations *bool  `json:"support_persistent_reservations,omitempty" jsonschema:"Allow SCSI persistent reservations, which guest clustering uses to arbitrate a shared disk."`

	ToControllerType     string `json:"to_controller_type,omitempty" jsonschema:"Move the disk to \"SCSI\" or \"IDE\"."`
	ToControllerNumber   *int   `json:"to_controller_number,omitempty" jsonschema:"Move the disk to this controller number."`
	ToControllerLocation *int   `json:"to_controller_location,omitempty" jsonschema:"Move the disk to this port on the controller. The guest names its devices by this, so it decides which disk is sdb."`
}

// ---- Registration ----------------------------------------------------------

func registerSettingsTools(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  "get_vm_settings",
		Title: "Get every VM setting",
		Description: "Read a VM's complete configuration: memory, processor, firmware or BIOS with its " +
			"boot order, the automatic start and stop actions, checkpoint policy and file locations, " +
			"virtual TPM and encryption, integration services, serial ports, console resolution, every " +
			"disk with its storage QoS, and every network adapter with its per-port features.\n\n" +
			"get_vm answers \"what is this VM\"; this answers \"how is it configured\". The vocabulary " +
			"is the same one every set_vm_* tool accepts, so what this reports can be sent straight " +
			"back — the boot order in particular reports a token per device that reorders it.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in vmNameInput) (*mcp.CallToolResult, *hyperv.VMSettings, error) {
		out, err := d.VM.GetVMSettings(ctx, in.Name)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "set_vm_memory",
		Title: "Set VM memory",
		Description: "Change how much memory a VM gets and whether Hyper-V may vary it.\n\n" +
			"Every setting you pass is applied together in one operation, which matters because they " +
			"constrain each other: raising startup memory past the current maximum is rejected on its " +
			"own but accepted when both move at once.\n\n" +
			"Dynamic memory suits idle and bursty guests and lets a host run more VMs than it has " +
			"memory for. Turn it off when the guest runs its own hypervisor, which needs its memory " +
			"really backed, or when a benchmark should not have the floor move under it. Startup " +
			"memory and the dynamic switch need the VM stopped; the rest can change while it runs.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setVMMemoryInput) (*mcp.CallToolResult, *hyperv.VMSettings, error) {
		out, err := d.VM.SetVMMemory(ctx, hyperv.MemoryOptions{
			VMName:         in.Name,
			StartupMB:      in.StartupMB,
			MinimumMB:      in.MinimumMB,
			MaximumMB:      in.MaximumMB,
			Dynamic:        in.Dynamic,
			BufferPercent:  in.BufferPercent,
			PriorityWeight: in.PriorityWeight,
		})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "set_vm_processor",
		Title: "Set VM processor",
		Description: "Change a VM's virtual processors and how much host CPU it may take.\n\n" +
			"count needs the VM stopped: Hyper-V has no CPU hot-add. The scheduling controls " +
			"(reserve, maximum, relative weight) can change while it runs, and only matter when VMs " +
			"actually compete — a reserve is a guarantee taken out of the host's total, so reserving " +
			"more than exists stops other VMs from starting.\n\n" +
			"hw_thread_count_per_core decides whether the guest sees SMT, which changes how its own " +
			"scheduler behaves and how per-core software licences count. compatibility_for_migration " +
			"hides newer instruction sets so the VM can move to an older host, at the guest's expense.\n\n" +
			"Nested virtualization has its own tool, set_vm_nested_virtualization, because enabling it " +
			"carries a memory prerequisite.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setVMProcessorInput) (*mcp.CallToolResult, *hyperv.VMSettings, error) {
		out, err := d.VM.SetVMProcessor(ctx, hyperv.ProcessorOptions{
			VMName:                    in.Name,
			Count:                     in.Count,
			ReservePercent:            in.ReservePercent,
			MaximumPercent:            in.MaximumPercent,
			RelativeWeight:            in.RelativeWeight,
			HwThreadCountPerCore:      in.HwThreadCountPerCore,
			CompatibilityForMigration: in.CompatibilityForMigration,
			HostResourceProtection:    in.HostResourceProtection,
		})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "set_vm_firmware",
		Title: "Set VM boot settings",
		Description: "Change what a VM boots from, and how its firmware behaves.\n\n" +
			"One tool serves both generations, because the question is the same either way; what " +
			"differs is what can be said. A Generation 2 VM boots from named devices, so an entry may " +
			"point at one particular disk: \"disk:D:\\\\VMs\\\\os.vhdx\". A Generation 1 BIOS knows only " +
			"four device classes and must be handed all of them, so anything you leave out is appended " +
			"in its current order — \"dvd\" alone means \"try the DVD first, then everything else as " +
			"before\".\n\n" +
			"secure_boot is the setting that most often decides whether a guest boots at all. A Linux " +
			"distribution needs \"linux\", because it is signed by the third-party UEFI CA rather than " +
			"the Windows one and otherwise stops at a security violation with no other explanation.\n\n" +
			"pause_after_boot_failure is worth setting before an unattended install: without it a VM " +
			"that finds nothing to boot retries forever, and capture_vm_screen catches a different " +
			"frame each time.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setVMFirmwareInput) (*mcp.CallToolResult, *hyperv.VMSettings, error) {
		out, err := d.VM.SetVMFirmware(ctx, hyperv.FirmwareOptions{
			VMName:                in.Name,
			BootOrder:             in.BootOrder,
			SecureBoot:            in.SecureBoot,
			ConsoleMode:           in.ConsoleMode,
			PauseAfterBootFailure: in.PauseAfterBootFailure,
			NumLock:               in.NumLock,
		})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "set_vm_options",
		Title: "Set VM management options",
		Description: "Change what the host does to a VM on its own: how it starts and stops with the " +
			"host, how it checkpoints, and where its files live.\n\n" +
			"automatic_start_action and automatic_stop_action decide what a host reboot means for this " +
			"VM. The default stop action saves state, which is fast but leaves the guest with a clock " +
			"jump and stale network state on resume; \"ShutDown\" is slower and cleaner. Changing the " +
			"stop action needs the VM stopped.\n\n" +
			"checkpoint_type is the one to get right before relying on checkpoints. \"Production\" " +
			"asks the guest to quiesce through VSS, so what is captured is application-consistent and " +
			"safe to restore. \"Standard\" also saves live memory, which captures a running process " +
			"exactly but restores a database to a state it never agreed to. automatic_checkpoints " +
			"arrives on by default on client Windows and snapshots the VM on every start, which fills " +
			"disks quietly; VMs created by this server already have it off.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setVMOptionsInput) (*mcp.CallToolResult, *hyperv.VMSettings, error) {
		out, err := d.VM.SetVMOptions(ctx, hyperv.VMOptionsUpdate{
			VMName:                               in.Name,
			Notes:                                in.Notes,
			AutomaticStartAction:                 in.AutomaticStartAction,
			AutomaticStartDelaySeconds:           in.AutomaticStartDelaySeconds,
			AutomaticStopAction:                  in.AutomaticStopAction,
			AutomaticCriticalErrorAction:         in.AutomaticCriticalErrorAction,
			AutomaticCriticalErrorTimeoutMinutes: in.AutomaticCriticalErrorTimeoutMinutes,
			CheckpointType:                       in.CheckpointType,
			AutomaticCheckpointsEnabled:          in.AutomaticCheckpointsEnabled,
			CheckpointFileLocation:               in.CheckpointFileLocation,
			SmartPagingFilePath:                  in.SmartPagingFilePath,
			CreateParents:                        in.CreateParents,
			EnhancedSessionTransportType:         in.EnhancedSessionTransportType,
			GuestControlledCacheTypes:            in.GuestControlledCacheTypes,
			LockOnDisconnect:                     in.LockOnDisconnect,
		})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "set_vm_integration_services",
		Title: "Set VM integration services",
		Description: "Turn the guest-facing VMBus services on or off.\n\n" +
			"Two of these are load-bearing for other tools here. Guest Service Interface is what " +
			"guest_copy_file travels over, and it is off by default on a new VM. Key-Value Pair " +
			"Exchange is how Hyper-V learns the guest's addresses, so turning it off blinds " +
			"wait_for_guest_ip and empties the ip_addresses get_vm reports, without breaking anything " +
			"inside the guest to explain why.\n\n" +
			"Shutdown is what stop_vm uses when force is not set. Time Synchronization is the one " +
			"worth turning off deliberately: a domain controller must not take its clock from the " +
			"host, and anything testing clock skew cannot have it corrected underneath.\n\n" +
			"A setting here only enables the host's side. The guest also needs the matching daemon, " +
			"which on Linux means hyperv-daemons.\n\n" +
			"Services are addressed by a fixed key, not by the name Hyper-V shows. Hyper-V names " +
			"them in the host's display language, so on a Korean host the shutdown service is called " +
			"\"시스템 종료\"; get_vm_settings reports both, and the key is the half that is the same " +
			"everywhere.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setVMIntegrationServicesInput) (*mcp.CallToolResult, *hyperv.VMSettings, error) {
		out, err := d.VM.SetVMIntegrationServices(ctx, hyperv.IntegrationOptions{
			VMName:                in.Name,
			GuestServiceInterface: in.GuestServiceInterface,
			Heartbeat:             in.Heartbeat,
			KeyValuePairExchange:  in.KeyValuePairExchange,
			Shutdown:              in.Shutdown,
			TimeSynchronization:   in.TimeSynchronization,
			VSS:                   in.VSS,
		})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "set_vm_security",
		Title: "Set VM TPM and encryption",
		Description: "Give a VM a virtual TPM, or encrypt its saved state.\n\n" +
			"This is what Windows 11 needs: its installer checks for a TPM and refuses to go on " +
			"without one, and the check happens early enough that an unattended install simply stops " +
			"at a dialog nothing is there to answer.\n\n" +
			"Both settings need a key protector, which a VM created by this server does not have. One " +
			"is created locally when it is missing, because Enable-VMTPM otherwise fails with an error " +
			"that does not mention key protectors at all. The VM must be Off, and this says so rather " +
			"than passing on Hyper-V's message, which names neither the VM nor its state.\n\n" +
			"Generation 2 only: a virtual TPM needs UEFI firmware.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setVMSecurityInput) (*mcp.CallToolResult, *hyperv.VMSettings, error) {
		out, err := d.VM.SetVMSecurity(ctx, hyperv.SecurityOptions{
			VMName:                          in.Name,
			TPMEnabled:                      in.TPMEnabled,
			EncryptStateAndMigrationTraffic: in.EncryptStateAndMigrationTraffic,
		})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "set_vm_video",
		Title: "Set VM console resolution",
		Description: "Pin the resolution of the VM's console framebuffer.\n\n" +
			"This is what capture_vm_screen photographs, and what a guest sees whenever it has no " +
			"display driver of its own: a Linux boot console, a Windows installer before setup " +
			"finishes, a firmware screen. Pinning it makes those captures a known size instead of " +
			"whatever the guest negotiated, which is what makes a series of screenshots comparable.\n\n" +
			"It does not follow that a guest with its own driver obeys: once one loads, the guest " +
			"chooses. Judge a running GUI by its automation tree through guest_run_in_session, not by " +
			"its pixels.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setVMVideoInput) (*mcp.CallToolResult, *hyperv.VMSettings, error) {
		out, err := d.VM.SetVMVideo(ctx, hyperv.VideoOptions{
			VMName:               in.Name,
			ResolutionType:       in.ResolutionType,
			HorizontalResolution: in.HorizontalResolution,
			VerticalResolution:   in.VerticalResolution,
		})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "set_vm_com_port",
		Title: "Attach a VM serial port",
		Description: "Back one of the VM's two serial ports with a host named pipe, or disconnect it.\n\n" +
			"A serial port is the one way into a guest that needs no network, no guest agent and no " +
			"working display. A Linux kernel booted with console=ttyS0 writes its entire boot there, " +
			"including the panic that stopped it; a Windows kernel debugger attaches over it. Pair it " +
			"with set_vm_firmware's console_mode to put the firmware's own output on the same wire.\n\n" +
			"Only a named pipe works. Hyper-V accepts a file path and then never opens it, so a path " +
			"that is not \\\\.\\pipe\\... is refused here instead of failing silently. This server does " +
			"not read the pipe for you; any named-pipe client can.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setVMComPortInput) (*mcp.CallToolResult, *hyperv.VMSettings, error) {
		out, err := d.VM.SetVMComPort(ctx, hyperv.ComPortOptions{
			VMName:       in.Name,
			Number:       in.Number,
			Path:         in.Path,
			Detach:       in.Detach,
			DebuggerMode: in.DebuggerMode,
		})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "set_vm_disk_settings",
		Title: "Set a VM disk's QoS and placement",
		Description: "Limit an attached disk's throughput, or move it to another controller port.\n\n" +
			"IOPS are counted in normalized 8 KB operations, so a 32 KB read costs four and a limit " +
			"of 500 does not mean 500 large reads. 0 is unlimited. A maximum is the honest way to " +
			"keep one VM's disk activity from starving the host; a minimum only reserves, and Hyper-V " +
			"reports rather than enforces when the reservations exceed what the storage can do.\n\n" +
			"Moving a disk changes what the guest calls it. A Linux guest enumerates SCSI targets by " +
			"controller location, so the port a disk sits on is what decides whether it is sdb or sdc " +
			"— which matters when a set of disks is meant to line up in a known order.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setVMDiskSettingsInput) (*mcp.CallToolResult, *hyperv.VMSettings, error) {
		out, err := d.VM.SetVMDiskSettings(ctx, hyperv.DiskSettingsOptions{
			VMName:                        in.Name,
			Path:                          in.Path,
			MinimumIOPS:                   in.MinimumIOPS,
			MaximumIOPS:                   in.MaximumIOPS,
			SupportPersistentReservations: in.SupportPersistentReservations,
			ToControllerType:              in.ToControllerType,
			ToControllerNumber:            in.ToControllerNumber,
			ToControllerLocation:          in.ToControllerLocation,
		})
		return nil, out, err
	})
}
