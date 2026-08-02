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

	ProcessorCount       int   `json:"processor_count"`
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
