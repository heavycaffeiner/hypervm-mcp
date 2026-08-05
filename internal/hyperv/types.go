// Package hyperv wraps the Hyper-V PowerShell module in typed Go operations.
//
// Every script here emits JSON whose keys match the struct tags below, so the
// shapes returned to MCP clients are decided in one place rather than being
// reshaped after the fact.
package hyperv

// VMSummary is the short form returned by listings and state transitions.
type VMSummary struct {
	Name           string `json:"name"`
	ID             string `json:"id"`    // VM GUID
	State          string `json:"state"` // Running | Off | Paused | Saved | Starting | Stopping
	CPUUsage       int    `json:"cpu_usage"`
	MemoryAssigned int64  `json:"memory_assigned"` // bytes
	UptimeSeconds  int64  `json:"uptime_seconds"`
	Generation     int    `json:"generation"` // 1 or 2
}

// HardDrive is one virtual disk attached to a VM.
type HardDrive struct {
	ControllerType     string `json:"controller_type"` // IDE | SCSI
	ControllerNumber   int    `json:"controller_number"`
	ControllerLocation int    `json:"controller_location"`
	Path               string `json:"path"`
}

// NetworkAdapter is one virtual NIC. IPAddresses comes from the guest via
// integration services, so it stays empty until the guest has booted far enough
// to report.
type NetworkAdapter struct {
	Name        string   `json:"name"`
	SwitchName  string   `json:"switch_name"`
	MacAddress  string   `json:"mac_address"`
	IPAddresses []string `json:"ip_addresses"`
	Connected   bool     `json:"connected"`
}

// VMDetail is the full form returned by GetVM.
type VMDetail struct {
	VMSummary

	Notes string `json:"notes,omitempty"`

	// Storage locations. ConfigurationLocation is fixed when the VM is created
	// and cannot be changed in place.
	ConfigurationLocation  string `json:"configuration_location"`
	CheckpointFileLocation string `json:"checkpoint_file_location"`
	SmartPagingFilePath    string `json:"smart_paging_file_path"`

	ProcessorCount int `json:"processor_count"`

	// NestedVirtualization reports whether the guest can run its own hypervisor.
	NestedVirtualization bool `json:"nested_virtualization"`

	MemoryStartup        int64 `json:"memory_startup"`
	DynamicMemoryEnabled bool  `json:"dynamic_memory_enabled"`
	MemoryMinimum        int64 `json:"memory_minimum"`
	MemoryMaximum        int64 `json:"memory_maximum"`
	CheckpointCount      int   `json:"checkpoint_count"`

	HardDrives      []HardDrive      `json:"hard_drives"`
	NetworkAdapters []NetworkAdapter `json:"network_adapters"`
}

// VMSwitch is a Hyper-V virtual switch.
//
// SwitchType decides what the guest can reach: Private sees only other VMs,
// Internal adds the host, Default/Internal-with-NAT adds outbound internet, and
// External puts the guest on the physical LAN with its own address.
type VMSwitch struct {
	Name              string   `json:"name"`
	SwitchType        string   `json:"switch_type"` // External | Internal | Private
	NetAdapterName    string   `json:"net_adapter_name,omitempty"`
	AllowManagementOS bool     `json:"allow_management_os"`
	ConnectedVMs      []string `json:"connected_vms"`
}

// Checkpoint is one saved VM state.
type Checkpoint struct {
	Name       string `json:"name"`
	VMName     string `json:"vm_name"`
	ID         string `json:"id"`
	ParentName string `json:"parent_name,omitempty"`
	CreatedAt  string `json:"created_at"`
	Type       string `json:"checkpoint_type"` // Standard | Production
	Path       string `json:"path,omitempty"`
}

// PhysicalAdapter is one of the host's real network adapters.
type PhysicalAdapter struct {
	Name          string   `json:"name"`
	InterfaceDesc string   `json:"interface_description"`
	Status        string   `json:"status"` // Up | Disconnected | Disabled
	LinkSpeedMbps int64    `json:"link_speed_mbps"`
	MACAddress    string   `json:"mac_address"`
	IsWireless    bool     `json:"is_wireless"`
	BoundToSwitch string   `json:"bound_to_switch,omitempty"`
	IPAddresses   []string `json:"ip_addresses"`
}

// ExternalSwitchPreflight is what creating an External switch would do to this
// particular host.
//
// A generic warning is easy to wave through. Naming the adapter that will go
// down, whether it is the only one, and whether its address will survive gives
// the reader something they can actually weigh.
type ExternalSwitchPreflight struct {
	AdapterName    string   `json:"adapter_name"`
	AdapterStatus  string   `json:"adapter_status"`
	IsWireless     bool     `json:"is_wireless"`
	IsOnlyUplink   bool     `json:"is_only_uplink"`
	UsesDHCP       bool     `json:"uses_dhcp"`
	Addresses      []string `json:"addresses"`
	NetworkProfile string   `json:"network_profile"`
	AlreadyBound   string   `json:"already_bound_to,omitempty"`
	Risks          []string `json:"risks"`
}

// AdapterDiagnosis is one VM NIC seen through the question "what can reach this".
type AdapterDiagnosis struct {
	Name            string   `json:"name"`
	SwitchName      string   `json:"switch_name"`
	SwitchType      string   `json:"switch_type"`
	MACAddress      string   `json:"mac_address"`
	VLANID          int      `json:"vlan_id"`
	MACSpoofing     bool     `json:"mac_spoofing"`
	IPAddresses     []string `json:"ip_addresses"`
	OnPhysicalLAN   bool     `json:"on_physical_lan"`
	HostCanReach    bool     `json:"host_can_reach"`
	ReachablePorts  []int    `json:"reachable_ports,omitempty"`
	UnreachablePort []int    `json:"unreachable_ports,omitempty"`
}

// NetworkDiagnosis answers whether a tunnel will do, or whether the VM needs its
// own address on the physical LAN.
type NetworkDiagnosis struct {
	VMName             string             `json:"vm_name"`
	State              string             `json:"state"`
	Adapters           []AdapterDiagnosis `json:"adapters"`
	GuestOnPhysicalLAN bool               `json:"guest_on_physical_lan"`
	// AddressesReported separates "the guest has no address" from "no agent
	// inside the guest is reporting one". Conflating the two produces a
	// confidently wrong diagnosis for any minimal Linux install.
	AddressesReported bool   `json:"addresses_reported"`
	ProbedHost        string `json:"probed_host,omitempty"`
	HostCanReach      bool   `json:"host_can_reach"`
	BlockedHostPorts  []int  `json:"blocked_host_ports"`
	Recommendation    string `json:"recommendation"`
}

// SetVMNetworkOptions are the adapter settings SetVMNetwork can change. A nil
// pointer means "leave this alone", which an ordinary zero value cannot express
// for VLAN 0 or for disabling MAC spoofing.
type SetVMNetworkOptions struct {
	VMName        string
	AdapterName   string
	SwitchName    string
	StaticMAC     string
	VLANID        *int
	MACSpoofing   *bool
	CreateAdapter bool
}

// VHDInfo describes a virtual disk file.
type VHDInfo struct {
	Path          string `json:"path"`
	Format        string `json:"format"`   // VHD | VHDX
	VHDType       string `json:"vhd_type"` // Fixed | Dynamic | Differencing
	SizeBytes     int64  `json:"size_bytes"`
	FileSizeBytes int64  `json:"file_size_bytes"`
	ParentPath    string `json:"parent_path,omitempty"`
	Attached      bool   `json:"attached"`
}

// CreateVMOptions are the settings CreateVM applies. Zero fields fall back to
// the defaults documented on the MCP tool.
type CreateVMOptions struct {
	Name            string
	Generation      int
	MemoryMB        int
	DynamicMemory   bool
	CPUCount        int
	VMPath          string
	VHDPath         string
	VHDSizeMB       int
	CheckpointPath  string
	SmartPagingPath string
	SwitchName      string
	ISOPath         string
	SecureBoot      string // windows | linux | off
	CreateParents   bool
}

// HostStoragePaths is where Hyper-V puts things by default.
type HostStoragePaths struct {
	VirtualMachinePath  string `json:"virtual_machine_path"`
	VirtualHardDiskPath string `json:"virtual_hard_disk_path"`
	// Accessibility is reported because a default under a user profile or a
	// vanished drive fails at creation time, far from its cause.
	VMPathAccessible  bool             `json:"vm_path_accessible"`
	VHDPathAccessible bool             `json:"vhd_path_accessible"`
	FreeSpaceBytes    map[string]int64 `json:"free_space_bytes"`
}

// SeedFile is one file written into a seed disk.
type SeedFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// SeedDiskResult reports what CreateSeedDisk produced.
type SeedDiskResult struct {
	Path      string   `json:"path"`
	Label     string   `json:"label"`
	SizeBytes int64    `json:"size_bytes"`
	Files     []string `json:"files"`
}

// DeleteVMResult says what was removed and, more importantly, what was not.
type DeleteVMResult struct {
	Deleted      bool     `json:"deleted"`
	DisksDeleted []string `json:"disks_deleted"`
	DisksKept    []string `json:"disks_kept"`
	KeptReasons  []string `json:"kept_reasons"`
}

// GuestIPResult is what WaitForGuestIP reports. AllAddresses is included because
// a VM attached to several switches has an address on each, and only the caller
// knows which network it cares about.
type GuestIPResult struct {
	Address       string   `json:"address"`
	AllAddresses  []string `json:"all_addresses"`
	WaitedSeconds float64  `json:"waited_seconds"`
}

// ---- Detailed settings -----------------------------------------------------
//
// VMSettings is the whole of a VM's configuration as Hyper-V Manager's property
// pages present it, and every set_vm_* tool returns one after its change. Read
// and write share this shape on purpose: what a tool reports is exactly what the
// next call can send back, so a caller never has to guess at the vocabulary.

// MemorySettings is the memory page. Minimum and Maximum only bind while dynamic
// memory is on; Hyper-V keeps their values either way.
type MemorySettings struct {
	StartupBytes   int64 `json:"startup_bytes"`
	MinimumBytes   int64 `json:"minimum_bytes"`
	MaximumBytes   int64 `json:"maximum_bytes"`
	DynamicEnabled bool  `json:"dynamic_enabled"`
	// BufferPercent is the headroom Hyper-V keeps above what the guest is using,
	// and Priority decides who wins when the host is short of memory.
	BufferPercent int `json:"buffer_percent"`
	Priority      int `json:"priority"`
}

// ProcessorSettings is the processor page. Reserve and Maximum are percentages
// of one logical processor's capacity, and RelativeWeight only matters when VMs
// compete for it.
type ProcessorSettings struct {
	Count          int `json:"count"`
	ReservePercent int `json:"reserve_percent"`
	MaximumPercent int `json:"maximum_percent"`
	RelativeWeight int `json:"relative_weight"`
	// HwThreadCountPerCore is 0 to follow the host, 1 to hide SMT from the guest,
	// 2 to expose it.
	HwThreadCountPerCore      int  `json:"hw_thread_count_per_core"`
	CompatibilityForMigration bool `json:"compatibility_for_migration"`
	HostResourceProtection    bool `json:"host_resource_protection"`
	NestedVirtualization      bool `json:"nested_virtualization"`
	MaximumCountPerNumaNode   int  `json:"maximum_count_per_numa_node"`
}

// BootEntry is one device in a Generation 2 VM's boot order. Token is the string
// that puts this same device back at a chosen position through SetVMFirmware, so
// reordering is a matter of resending the tokens in the order wanted.
type BootEntry struct {
	Kind         string `json:"kind"` // disk | dvd | network | file | other
	Description  string `json:"description,omitempty"`
	Path         string `json:"path,omitempty"`
	AdapterName  string `json:"adapter_name,omitempty"`
	FirmwarePath string `json:"firmware_path,omitempty"`
	Token        string `json:"token"`
}

// FirmwareSettings is a Generation 2 VM's UEFI configuration.
type FirmwareSettings struct {
	SecureBoot         string `json:"secure_boot"` // On | Off
	SecureBootTemplate string `json:"secure_boot_template,omitempty"`
	// ConsoleMode routes firmware output: Default draws to the video device,
	// COM1 and COM2 send it to a serial port instead.
	ConsoleMode                  string      `json:"console_mode"`
	PauseAfterBootFailure        bool        `json:"pause_after_boot_failure"`
	PreferredNetworkBootProtocol string      `json:"preferred_network_boot_protocol"`
	BootOrder                    []BootEntry `json:"boot_order"`
}

// BIOSSettings is a Generation 1 VM's BIOS configuration. Its startup order is a
// permutation of four fixed device classes rather than a list of real devices.
type BIOSSettings struct {
	NumLockEnabled bool     `json:"num_lock_enabled"`
	StartupOrder   []string `json:"startup_order"`
}

// VMOptions is everything on the VM's own management pages: what happens to it
// when the host starts and stops, how it checkpoints, and where its files live.
type VMOptions struct {
	Notes string `json:"notes"`

	AutomaticStartAction       string `json:"automatic_start_action"` // Nothing | StartIfRunning | Start
	AutomaticStartDelaySeconds int    `json:"automatic_start_delay_seconds"`
	AutomaticStopAction        string `json:"automatic_stop_action"` // TurnOff | Save | ShutDown

	AutomaticCriticalErrorAction               string `json:"automatic_critical_error_action"` // None | Pause
	AutomaticCriticalErrorActionTimeoutMinutes int    `json:"automatic_critical_error_action_timeout_minutes"`

	CheckpointType              string `json:"checkpoint_type"` // Disabled | Production | ProductionOnly | Standard
	AutomaticCheckpointsEnabled bool   `json:"automatic_checkpoints_enabled"`

	CheckpointFileLocation string `json:"checkpoint_file_location"`
	SmartPagingFilePath    string `json:"smart_paging_file_path"`
	ConfigurationLocation  string `json:"configuration_location"`

	EnhancedSessionTransportType string `json:"enhanced_session_transport_type"` // VMBus | HvSocket
	GuestControlledCacheTypes    bool   `json:"guest_controlled_cache_types"`
	LockOnDisconnect             bool   `json:"lock_on_disconnect"`
	ConfigurationVersion         string `json:"configuration_version"`
}

// SecuritySettings covers the virtual TPM and state encryption. KeyProtectorPresent
// is reported separately because both features need one and it is created
// independently of them.
type SecuritySettings struct {
	TPMEnabled                        bool `json:"tpm_enabled"`
	KeyProtectorPresent               bool `json:"key_protector_present"`
	EncryptStateAndMigrationTraffic   bool `json:"encrypt_state_and_migration_traffic"`
	Shielded                          bool `json:"shielded"`
	VirtualizationBasedSecurityOptOut bool `json:"virtualization_based_security_opt_out"`
}

// IntegrationService is one of the guest-side services Hyper-V offers over the
// VMBus. Status comes from the guest, so it stays empty until one is running.
type IntegrationService struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Status  string `json:"status,omitempty"`
}

// ComPort is one virtual serial port, backed by a host named pipe when attached.
// Number is what SetVMComPort addresses the port by; Name is what Hyper-V calls
// it, which is where the number is derived from.
type ComPort struct {
	Number       int    `json:"number"`
	Name         string `json:"name,omitempty"`
	Path         string `json:"path,omitempty"`
	DebuggerMode string `json:"debugger_mode,omitempty"`
}

// VideoSettings is the console's framebuffer. It decides what capture_vm_screen
// sees before the guest's own driver takes over.
type VideoSettings struct {
	ResolutionType       string `json:"resolution_type"` // Default | Single | Maximum
	HorizontalResolution int    `json:"horizontal_resolution"`
	VerticalResolution   int    `json:"vertical_resolution"`
}

// DiskSettings is one attached disk with its storage QoS. IOPS values are in
// normalized 8 KB operations, and 0 means unlimited.
type DiskSettings struct {
	ControllerType                string `json:"controller_type"`
	ControllerNumber              int    `json:"controller_number"`
	ControllerLocation            int    `json:"controller_location"`
	Path                          string `json:"path"`
	MinimumIOPS                   int64  `json:"minimum_iops"`
	MaximumIOPS                   int64  `json:"maximum_iops"`
	SupportPersistentReservations bool   `json:"support_persistent_reservations"`
}

// DVDDrive is one virtual optical drive, empty when no ISO is loaded.
type DVDDrive struct {
	ControllerType     string `json:"controller_type"`
	ControllerNumber   int    `json:"controller_number"`
	ControllerLocation int    `json:"controller_location"`
	Path               string `json:"path,omitempty"`
}

// AdapterSettings is one virtual NIC with every per-port feature Hyper-V offers,
// not just the few SetVMNetwork changes.
type AdapterSettings struct {
	Name        string   `json:"name"`
	SwitchName  string   `json:"switch_name"`
	MacAddress  string   `json:"mac_address"`
	DynamicMAC  bool     `json:"dynamic_mac"`
	Connected   bool     `json:"connected"`
	IPAddresses []string `json:"ip_addresses"`

	MACSpoofing   bool   `json:"mac_spoofing"`
	DHCPGuard     bool   `json:"dhcp_guard"`
	RouterGuard   bool   `json:"router_guard"`
	PortMirroring string `json:"port_mirroring"` // None | Source | Destination
	DeviceNaming  bool   `json:"device_naming"`
	AllowTeaming  bool   `json:"allow_teaming"`

	VMQWeight         int `json:"vmq_weight"`
	IPsecOffloadMaxSA int `json:"ipsec_offload_max_sa"`

	MinimumBandwidthMbps   int64 `json:"minimum_bandwidth_mbps"`
	MaximumBandwidthMbps   int64 `json:"maximum_bandwidth_mbps"`
	MinimumBandwidthWeight int   `json:"minimum_bandwidth_weight"`

	VLANMode            string `json:"vlan_mode"` // Untagged | Access | Trunk | ...
	VLANID              int    `json:"vlan_id"`
	TrunkNativeVLANID   int    `json:"trunk_native_vlan_id"`
	TrunkAllowedVLANIDs []int  `json:"trunk_allowed_vlan_ids"`
}

// VMSettings is a VM's complete configuration. Firmware and BIOS are mutually
// exclusive: which one is present says which generation the VM is.
type VMSettings struct {
	Name       string `json:"name"`
	ID         string `json:"id"`
	State      string `json:"state"`
	Generation int    `json:"generation"`

	Memory    MemorySettings    `json:"memory"`
	Processor ProcessorSettings `json:"processor"`
	Firmware  *FirmwareSettings `json:"firmware,omitempty"`
	BIOS      *BIOSSettings     `json:"bios,omitempty"`
	Options   VMOptions         `json:"options"`
	Security  SecuritySettings  `json:"security"`
	Video     *VideoSettings    `json:"video,omitempty"`

	IntegrationServices []IntegrationService `json:"integration_services"`
	ComPorts            []ComPort            `json:"com_ports"`
	SCSIControllerCount int                  `json:"scsi_controller_count"`
	HardDrives          []DiskSettings       `json:"hard_drives"`
	DVDDrives           []DVDDrive           `json:"dvd_drives"`
	NetworkAdapters     []AdapterSettings    `json:"network_adapters"`
}

// MemoryOptions are the memory settings SetVMMemory can change. Every field is a
// pointer so that "leave this alone" is distinguishable from a zero value, which
// for a percentage or a byte count is a real setting.
type MemoryOptions struct {
	VMName         string
	StartupMB      *int
	MinimumMB      *int
	MaximumMB      *int
	Dynamic        *bool
	BufferPercent  *int
	PriorityWeight *int
}

// ProcessorOptions are the processor settings SetVMProcessor can change.
type ProcessorOptions struct {
	VMName                    string
	Count                     *int
	ReservePercent            *int
	MaximumPercent            *int
	RelativeWeight            *int
	HwThreadCountPerCore      *int
	CompatibilityForMigration *bool
	HostResourceProtection    *bool
}

// FirmwareOptions are the boot settings SetVMFirmware can change. It serves both
// generations: BootOrder and NumLock apply to whichever the VM actually is, and
// asking for something the generation does not have is an error rather than a
// silent no-op.
type FirmwareOptions struct {
	VMName                string
	BootOrder             []string
	SecureBoot            string // windows | linux | off
	ConsoleMode           string // Default | COM1 | COM2 | None
	PauseAfterBootFailure *bool
	NumLock               *bool
}

// VMOptionsUpdate are the settings SetVMOptions can change.
type VMOptionsUpdate struct {
	VMName string

	Notes *string

	AutomaticStartAction       string
	AutomaticStartDelaySeconds *int
	AutomaticStopAction        string

	AutomaticCriticalErrorAction         string
	AutomaticCriticalErrorTimeoutMinutes *int

	CheckpointType              string
	AutomaticCheckpointsEnabled *bool

	CheckpointFileLocation string
	SmartPagingFilePath    string
	CreateParents          bool

	EnhancedSessionTransportType string
	GuestControlledCacheTypes    *bool
	LockOnDisconnect             *bool
}

// IntegrationOptions are the integration services SetVMIntegrationServices can
// toggle, named as Hyper-V Manager names them.
type IntegrationOptions struct {
	VMName                string
	GuestServiceInterface *bool
	Heartbeat             *bool
	KeyValuePairExchange  *bool
	Shutdown              *bool
	TimeSynchronization   *bool
	VSS                   *bool
}

// SecurityOptions are the settings SetVMSecurity can change.
type SecurityOptions struct {
	VMName                          string
	TPMEnabled                      *bool
	EncryptStateAndMigrationTraffic *bool
}

// VideoOptions are the settings SetVMVideo can change.
type VideoOptions struct {
	VMName               string
	ResolutionType       string // Default | Single | Maximum
	HorizontalResolution int
	VerticalResolution   int
}

// ComPortOptions are the settings SetVMComPort can change. Detach exists because
// an empty Path is what Hyper-V uses to disconnect a port, which an omitted
// field cannot mean.
type ComPortOptions struct {
	VMName       string
	Number       int
	Path         string
	Detach       bool
	DebuggerMode *bool
}

// DiskSettingsOptions are the per-disk settings SetVMDiskSettings can change.
// Path identifies which attached disk is meant.
type DiskSettingsOptions struct {
	VMName                        string
	Path                          string
	MinimumIOPS                   *int64
	MaximumIOPS                   *int64
	SupportPersistentReservations *bool

	ToControllerType     string
	ToControllerNumber   *int
	ToControllerLocation *int
}

// AdapterFeatureOptions are the per-port adapter features SetVMNetworkAdvanced
// can change. The switch, MAC and access VLAN belong to SetVMNetwork; this is
// everything else Hyper-V offers on a virtual NIC.
type AdapterFeatureOptions struct {
	VMName      string
	AdapterName string

	DHCPGuard     *bool
	RouterGuard   *bool
	PortMirroring string // None | Source | Destination
	DeviceNaming  *bool
	AllowTeaming  *bool

	VMQWeight         *int
	IPsecOffloadMaxSA *int

	MinimumBandwidthMbps   *int
	MaximumBandwidthMbps   *int
	MinimumBandwidthWeight *int

	TrunkNativeVLANID   *int
	TrunkAllowedVLANIDs []int
}
