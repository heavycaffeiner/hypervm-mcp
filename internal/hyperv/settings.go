package hyperv

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/winpath"
)

// settingsProjection fills $result with a VMSettings. It expects $vm to be set
// and re-reads it first, so it is equally correct after a mutation.
//
// Several reads are wrapped in try/catch rather than allowed to fail the whole
// projection. Get-VMSecurity, Get-VMKeyProtector and Get-VMVideo each depend on
// host features and VM generation, and a host that cannot answer one of them
// should still be able to report everything else.
const settingsProjection = `
    $vm  = Get-VM -Id $vm.Id
    $mem = Get-VMMemory -VM $vm
    $cpu = Get-VMProcessor -VM $vm

    $fw = $null
    $bios = $null
    if ([int]$vm.Generation -eq 2) {
        $f = Get-VMFirmware -VM $vm
        $fw = [ordered]@{
            secure_boot                     = $f.SecureBoot.ToString()
            secure_boot_template            = [string]$f.SecureBootTemplate
            console_mode                    = $f.ConsoleMode.ToString()
            pause_after_boot_failure        = [bool]($f.PauseAfterBootFailure.ToString() -eq 'On')
            preferred_network_boot_protocol = $f.PreferredNetworkBootProtocol.ToString()
            boot_order                      = @($f.BootOrder | ForEach-Object {
                $src = $_
                $dev = $src.Device
                $kind = 'other'; $path = ''; $adapter = ''
                if ($dev) {
                    # Matched on the type name with wildcards rather than exactly,
                    # so a renamed or derived Hyper-V type still classifies.
                    $tn = $dev.GetType().Name
                    if     ($tn -like '*HardDiskDrive*')   { $kind = 'disk';    $path    = [string]$dev.Path }
                    elseif ($tn -like '*DvdDrive*')        { $kind = 'dvd';     $path    = [string]$dev.Path }
                    elseif ($tn -like '*NetworkAdapter*')  { $kind = 'network'; $adapter = [string]$dev.Name }
                } elseif ($src.FirmwarePath) { $kind = 'file' }

                $token = $kind
                if (($kind -eq 'disk' -or $kind -eq 'dvd') -and $path) { $token = $kind + ':' + $path }
                if ($kind -eq 'network' -and $adapter)                 { $token = 'network:' + $adapter }

                [ordered]@{
                    kind          = $kind
                    description   = [string]$src.Description
                    path          = $path
                    adapter_name  = $adapter
                    firmware_path = [string]$src.FirmwarePath
                    token         = $token
                }
            })
        }
    } else {
        $b = Get-VMBios -VM $vm
        $bios = [ordered]@{
            num_lock_enabled = [bool]$b.NumLockEnabled
            startup_order    = @($b.StartupOrder | ForEach-Object { $_.ToString() })
        }
    }

    $secObj = $null
    try { $secObj = Get-VMSecurity -VM $vm } catch { }
    $kp = $null
    try { $kp = Get-VMKeyProtector -VM $vm } catch { }
    $sec = [ordered]@{
        tpm_enabled                           = [bool]($secObj -and $secObj.TpmEnabled)
        # A VM with no protector still answers with a short placeholder rather
        # than nothing, so length is what distinguishes the two.
        key_protector_present                 = [bool]($kp -and $kp.Length -gt 4)
        encrypt_state_and_migration_traffic   = [bool]($secObj -and $secObj.EncryptStateAndVmMigrationTraffic)
        shielded                              = [bool]($secObj -and $secObj.Shielded)
        virtualization_based_security_opt_out = [bool]($secObj -and $secObj.VirtualizationBasedSecurityOptOut)
    }

    $video = $null
    try {
        $v = Get-VMVideo -VM $vm
        if ($v) {
            $video = [ordered]@{
                resolution_type       = [string]$v.ResolutionType
                horizontal_resolution = [int]$v.HorizontalResolution
                vertical_resolution   = [int]$v.VerticalResolution
            }
        }
    } catch { }

    $svcs = @(Get-VMIntegrationService -VM $vm | ForEach-Object { [ordered]@{
        name    = [string]$_.Name
        enabled = [bool]$_.Enabled
        status  = [string]$_.PrimaryStatusDescription
    } })

    # A COM port object carries no number of its own, only a name like "COM 1",
    # while Set-VMComPort addresses ports by number. The digit in the name is
    # what bridges the two, and its position is the fallback when there is none.
    $coms = @()
    $comPosition = 0
    foreach ($cp in @(Get-VMComPort -VM $vm)) {
        $comPosition++
        $comNumber = $comPosition
        if ($cp.Name -match '(\d+)\s*$') { $comNumber = [int]$matches[1] }
        $coms += [ordered]@{
            number        = $comNumber
            name          = [string]$cp.Name
            path          = [string]$cp.Path
            debugger_mode = [string]$cp.DebuggerMode
        }
    }

    $sdisks = @(Get-VMHardDiskDrive -VM $vm | ForEach-Object { [ordered]@{
        controller_type                 = $_.ControllerType.ToString()
        controller_number               = [int]$_.ControllerNumber
        controller_location             = [int]$_.ControllerLocation
        path                            = [string]$_.Path
        minimum_iops                    = [int64]$_.MinimumIOPS
        maximum_iops                    = [int64]$_.MaximumIOPS
        support_persistent_reservations = [bool]$_.SupportPersistentReservations
    } })

    $sdvds = @(Get-VMDvdDrive -VM $vm | ForEach-Object { [ordered]@{
        controller_type     = $_.ControllerType.ToString()
        controller_number   = [int]$_.ControllerNumber
        controller_location = [int]$_.ControllerLocation
        path                = [string]$_.Path
    } })

    $snics = @(Get-VMNetworkAdapter -VM $vm | ForEach-Object {
        $nic = $_
        $vlan = $null
        try { $vlan = Get-VMNetworkAdapterVlan -VMNetworkAdapter $nic } catch { }
        $bw = $nic.BandwidthSetting
        $trunk = @()
        if ($vlan -and $vlan.OperationMode.ToString() -eq 'Trunk') {
            $trunk = @($vlan.AllowedVlanIdList | ForEach-Object { [int]$_ })
        }
        [ordered]@{
            name         = $nic.Name
            switch_name  = [string]$nic.SwitchName
            mac_address  = [string]$nic.MacAddress
            dynamic_mac  = [bool]$nic.DynamicMacAddressEnabled
            connected    = [bool]$nic.Connected
            ip_addresses = @($nic.IPAddresses | Where-Object { $_ })

            mac_spoofing   = [bool]($nic.MacAddressSpoofing.ToString() -eq 'On')
            dhcp_guard     = [bool]($nic.DhcpGuard.ToString() -eq 'On')
            router_guard   = [bool]($nic.RouterGuard.ToString() -eq 'On')
            port_mirroring = [string]$nic.PortMirroringMode
            device_naming  = [bool]($nic.DeviceNaming.ToString() -eq 'On')
            allow_teaming  = [bool]($nic.AllowTeaming.ToString() -eq 'On')

            vmq_weight           = [int]$nic.VMQWeight
            ipsec_offload_max_sa = [int]$nic.IPsecOffloadMaxSA

            # Hyper-V stores bandwidth in bits per second; megabits is what the
            # setting is actually chosen in.
            minimum_bandwidth_mbps   = [int64]$(if ($bw) { $bw.MinimumBandwidthAbsolute / 1000000 } else { 0 })
            maximum_bandwidth_mbps   = [int64]$(if ($bw) { $bw.MaximumBandwidth / 1000000 } else { 0 })
            minimum_bandwidth_weight = [int]$(if ($bw) { $bw.MinimumBandwidthWeight } else { 0 })

            vlan_mode              = [string]$(if ($vlan) { $vlan.OperationMode.ToString() } else { '' })
            vlan_id                = [int]$(if ($vlan -and $vlan.OperationMode.ToString() -eq 'Access') { $vlan.AccessVlanId } else { 0 })
            trunk_native_vlan_id   = [int]$(if ($vlan -and $vlan.OperationMode.ToString() -eq 'Trunk') { $vlan.NativeVlanId } else { 0 })
            trunk_allowed_vlan_ids = $trunk
        }
    })

    $result = [ordered]@{
        name       = $vm.Name
        id         = $vm.Id.ToString()
        state      = $vm.State.ToString()
        generation = [int]$vm.Generation

        memory = [ordered]@{
            startup_bytes   = [int64]$mem.Startup
            minimum_bytes   = [int64]$mem.Minimum
            maximum_bytes   = [int64]$mem.Maximum
            dynamic_enabled = [bool]$mem.DynamicMemoryEnabled
            buffer_percent  = [int]$mem.Buffer
            priority        = [int]$mem.Priority
        }

        processor = [ordered]@{
            count                       = [int]$cpu.Count
            reserve_percent             = [int]$cpu.Reserve
            maximum_percent             = [int]$cpu.Maximum
            relative_weight             = [int]$cpu.RelativeWeight
            hw_thread_count_per_core    = [int]$cpu.HwThreadCountPerCore
            compatibility_for_migration = [bool]$cpu.CompatibilityForMigrationEnabled
            host_resource_protection    = [bool]$cpu.EnableHostResourceProtection
            nested_virtualization       = [bool]$cpu.ExposeVirtualizationExtensions
            maximum_count_per_numa_node = [int]$cpu.MaximumCountPerNumaNode
        }

        firmware = $fw
        bios     = $bios

        options = [ordered]@{
            notes                                          = [string]$vm.Notes
            automatic_start_action                         = $vm.AutomaticStartAction.ToString()
            automatic_start_delay_seconds                  = [int]$vm.AutomaticStartDelay
            automatic_stop_action                          = $vm.AutomaticStopAction.ToString()
            automatic_critical_error_action                = $vm.AutomaticCriticalErrorAction.ToString()
            automatic_critical_error_action_timeout_minutes = [int]$vm.AutomaticCriticalErrorActionTimeout
            checkpoint_type                                = $vm.CheckpointType.ToString()
            automatic_checkpoints_enabled                  = [bool]$vm.AutomaticCheckpointsEnabled
            checkpoint_file_location                       = [string]$vm.SnapshotFileLocation
            smart_paging_file_path                         = [string]$vm.SmartPagingFilePath
            configuration_location                         = [string]$vm.ConfigurationLocation
            enhanced_session_transport_type                = [string]$vm.EnhancedSessionTransportType
            guest_controlled_cache_types                   = [bool]$vm.GuestControlledCacheTypes
            lock_on_disconnect                             = [bool]($vm.LockOnDisconnect.ToString() -eq 'On')
            configuration_version                          = [string]$vm.Version
        }

        security = $sec
        video    = $video

        integration_services  = $svcs
        com_ports             = $coms
        scsi_controller_count = [int]@(Get-VMScsiController -VM $vm).Count
        hard_drives           = $sdisks
        dvd_drives            = $sdvds
        network_adapters      = $snics
    }`

// GetVMSettings returns a VM's complete configuration.
func (c *Client) GetVMSettings(ctx context.Context, name string) (*VMSettings, error) {
	if name == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	var out VMSettings
	if err := c.r.RunTimeoutInto(ctx, 2*time.Minute, requireVM+settingsProjection,
		map[string]any{"name": name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// runSettings is the shared tail of every setter: run the script, then report
// the VM's configuration as it now stands.
func (c *Client) runSettings(ctx context.Context, script string, args map[string]any) (*VMSettings, error) {
	var out VMSettings
	if err := c.r.RunTimeoutInto(ctx, 2*time.Minute, requireVM+script+settingsProjection, args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetVMMemory changes a VM's memory configuration.
//
// Every requested change is applied in a single Set-VMMemory call, which matters
// because the settings constrain each other: raising startup memory past the
// current maximum, or lowering the maximum below the current startup, is
// rejected on its own but accepted when both move together.
func (c *Client) SetVMMemory(ctx context.Context, o MemoryOptions) (*VMSettings, error) {
	if o.VMName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	for _, f := range []struct {
		label string
		v     *int
	}{{"startup_mb", o.StartupMB}, {"minimum_mb", o.MinimumMB}, {"maximum_mb", o.MaximumMB}} {
		if f.v != nil && *f.v < 1 {
			return nil, hverr.New(hverr.InvalidArgument, "%s must be at least 1", f.label)
		}
	}
	if o.BufferPercent != nil && (*o.BufferPercent < 5 || *o.BufferPercent > 2000) {
		return nil, hverr.New(hverr.InvalidArgument, "buffer_percent must be between 5 and 2000")
	}
	if o.PriorityWeight != nil && (*o.PriorityWeight < 0 || *o.PriorityWeight > 100) {
		return nil, hverr.New(hverr.InvalidArgument, "priority_weight must be between 0 and 100")
	}
	// Caught here rather than by Hyper-V, whose message for it names raw byte
	// counts and does not say which of the three is out of order.
	if o.MinimumMB != nil && o.MaximumMB != nil && *o.MinimumMB > *o.MaximumMB {
		return nil, hverr.New(hverr.InvalidArgument,
			"minimum_mb (%d) is above maximum_mb (%d)", *o.MinimumMB, *o.MaximumMB)
	}
	if o.StartupMB != nil && o.MaximumMB != nil && *o.StartupMB > *o.MaximumMB {
		return nil, hverr.New(hverr.InvalidArgument,
			"startup_mb (%d) is above maximum_mb (%d)", *o.StartupMB, *o.MaximumMB)
	}
	if o.StartupMB != nil && o.MinimumMB != nil && *o.StartupMB < *o.MinimumMB {
		return nil, hverr.New(hverr.InvalidArgument,
			"startup_mb (%d) is below minimum_mb (%d)", *o.StartupMB, *o.MinimumMB)
	}

	args := map[string]any{"name": o.VMName}
	setInt64MB(args, "startup", o.StartupMB)
	setInt64MB(args, "minimum", o.MinimumMB)
	setInt64MB(args, "maximum", o.MaximumMB)
	setBool(args, "dynamic", o.Dynamic)
	setInt(args, "buffer", o.BufferPercent)
	setInt(args, "priority", o.PriorityWeight)

	const script = `
    $a = @{ VM = $vm }
    if ($P.set_dynamic)  { $a['DynamicMemoryEnabled'] = [bool]$P.dynamic }
    if ($P.set_startup)  { $a['StartupBytes']         = [int64]$P.startup }
    if ($P.set_minimum)  { $a['MinimumBytes']         = [int64]$P.minimum }
    if ($P.set_maximum)  { $a['MaximumBytes']         = [int64]$P.maximum }
    if ($P.set_buffer)   { $a['Buffer']               = [int]$P.buffer }
    if ($P.set_priority) { $a['Priority']             = [int]$P.priority }
    if ($a.Count -le 1) { throw "HVERR:INVALID_ARGUMENT|nothing to change; pass at least one memory setting" }
    Set-VMMemory @a | Out-Null
`
	return c.runSettings(ctx, script, args)
}

// SetVMProcessor changes a VM's processor configuration.
//
// Count and hw_thread_count_per_core need the VM stopped, and Hyper-V says so
// itself; the rest of these can be changed while it runs.
func (c *Client) SetVMProcessor(ctx context.Context, o ProcessorOptions) (*VMSettings, error) {
	if o.VMName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	if o.Count != nil && *o.Count < 1 {
		return nil, hverr.New(hverr.InvalidArgument, "count must be at least 1")
	}
	for _, f := range []struct {
		label string
		v     *int
	}{{"reserve_percent", o.ReservePercent}, {"maximum_percent", o.MaximumPercent}} {
		if f.v != nil && (*f.v < 0 || *f.v > 100) {
			return nil, hverr.New(hverr.InvalidArgument, "%s must be between 0 and 100", f.label)
		}
	}
	if o.RelativeWeight != nil && (*o.RelativeWeight < 1 || *o.RelativeWeight > 10000) {
		return nil, hverr.New(hverr.InvalidArgument, "relative_weight must be between 1 and 10000")
	}
	if o.HwThreadCountPerCore != nil && (*o.HwThreadCountPerCore < 0 || *o.HwThreadCountPerCore > 2) {
		return nil, hverr.New(hverr.InvalidArgument,
			"hw_thread_count_per_core must be 0 (follow the host), 1 (hide SMT) or 2 (expose it)")
	}
	if o.ReservePercent != nil && o.MaximumPercent != nil && *o.ReservePercent > *o.MaximumPercent {
		return nil, hverr.New(hverr.InvalidArgument,
			"reserve_percent (%d) is above maximum_percent (%d)", *o.ReservePercent, *o.MaximumPercent)
	}

	args := map[string]any{"name": o.VMName}
	setInt(args, "count", o.Count)
	setInt(args, "reserve", o.ReservePercent)
	setInt(args, "maximum", o.MaximumPercent)
	setInt(args, "weight", o.RelativeWeight)
	setInt(args, "threads", o.HwThreadCountPerCore)
	setBool(args, "compat", o.CompatibilityForMigration)
	setBool(args, "protection", o.HostResourceProtection)

	const script = `
    $a = @{ VM = $vm }
    if ($P.set_count)      { $a['Count']                           = [int]$P.count }
    if ($P.set_reserve)    { $a['Reserve']                         = [int]$P.reserve }
    if ($P.set_maximum)    { $a['Maximum']                         = [int]$P.maximum }
    if ($P.set_weight)     { $a['RelativeWeight']                  = [int]$P.weight }
    if ($P.set_threads)    { $a['HwThreadCountPerCore']            = [int]$P.threads }
    if ($P.set_compat)     { $a['CompatibilityForMigrationEnabled'] = [bool]$P.compat }
    if ($P.set_protection) { $a['EnableHostResourceProtection']     = [bool]$P.protection }
    if ($a.Count -le 1) { throw "HVERR:INVALID_ARGUMENT|nothing to change; pass at least one processor setting" }
    Set-VMProcessor @a | Out-Null
`
	return c.runSettings(ctx, script, args)
}

// bootTokenKinds are the device classes a boot_order entry may name.
var bootTokenKinds = map[string]bool{"disk": true, "dvd": true, "network": true, "floppy": true}

// SetVMFirmware changes what a VM boots from, and how.
//
// One tool covers both generations because the question a caller has is the same
// either way. What differs is what can be expressed: a Generation 2 VM boots from
// named devices and its order may point at one particular disk, while a
// Generation 1 BIOS knows only four device classes and must be given all of them.
// Entries left out of a Generation 1 order are appended in their current order,
// so a caller can say "CD first" without having to name the rest.
func (c *Client) SetVMFirmware(ctx context.Context, o FirmwareOptions) (*VMSettings, error) {
	if o.VMName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	if len(o.BootOrder) == 0 && o.SecureBoot == "" && o.ConsoleMode == "" &&
		o.PauseAfterBootFailure == nil && o.NumLock == nil {
		return nil, hverr.New(hverr.InvalidArgument,
			"nothing to change; pass at least one firmware setting")
	}

	for _, tok := range o.BootOrder {
		kind := tok
		if i := strings.Index(tok, ":"); i >= 0 {
			kind = tok[:i]
		}
		if !bootTokenKinds[strings.ToLower(strings.TrimSpace(kind))] {
			return nil, hverr.New(hverr.InvalidArgument,
				`boot_order entry %q must name "disk", "dvd", "network" or "floppy", `+
					`optionally followed by ":" and a disk path or adapter name`, tok)
		}
	}

	switch strings.ToLower(o.SecureBoot) {
	case "", "windows", "linux", "off":
		o.SecureBoot = strings.ToLower(o.SecureBoot)
	default:
		return nil, hverr.New(hverr.InvalidArgument,
			`secure_boot must be "windows", "linux" or "off", got %q`, o.SecureBoot)
	}

	switch strings.ToLower(o.ConsoleMode) {
	case "":
	case "default":
		o.ConsoleMode = "Default"
	case "com1":
		o.ConsoleMode = "COM1"
	case "com2":
		o.ConsoleMode = "COM2"
	case "none":
		o.ConsoleMode = "None"
	default:
		return nil, hverr.New(hverr.InvalidArgument,
			`console_mode must be "Default", "COM1", "COM2" or "None", got %q`, o.ConsoleMode)
	}

	// A nil slice would reach PowerShell as null, and @($null) is an array of one
	// rather than an empty one — so the script would loop over a token that is
	// not there.
	if o.BootOrder == nil {
		o.BootOrder = []string{}
	}
	args := map[string]any{
		"name":         o.VMName,
		"boot_order":   o.BootOrder,
		"secure_boot":  o.SecureBoot,
		"console_mode": o.ConsoleMode,
	}
	setBool(args, "pause", o.PauseAfterBootFailure)
	setBool(args, "numlock", o.NumLock)

	const script = `
    $order = @($P.boot_order)
    if ([int]$vm.Generation -eq 2) {
        if ($P.set_numlock) {
            throw "HVERR:INVALID_ARGUMENT|num_lock is a BIOS setting and '$($P.name)' is Generation 2, which has UEFI firmware instead"
        }
        if ($order.Count -gt 0) {
            $devices = @()
            foreach ($tok in $order) {
                $kind = [string]$tok
                $qual = ''
                $i = $kind.IndexOf(':')
                if ($i -ge 0) { $qual = $kind.Substring($i + 1); $kind = $kind.Substring(0, $i) }
                $match = @()
                switch ($kind.Trim().ToLower()) {
                    'disk' {
                        $match = @(Get-VMHardDiskDrive -VM $vm)
                        if ($qual) { $match = @($match | Where-Object { $_.Path -eq $qual }) }
                    }
                    'dvd' {
                        $match = @(Get-VMDvdDrive -VM $vm)
                        if ($qual) { $match = @($match | Where-Object { $_.Path -eq $qual }) }
                    }
                    'network' {
                        $match = @(Get-VMNetworkAdapter -VM $vm)
                        if ($qual) { $match = @($match | Where-Object { $_.Name -eq $qual }) }
                    }
                    default {
                        throw "HVERR:INVALID_ARGUMENT|boot_order entry '$tok' names a device class a Generation 2 VM does not have"
                    }
                }
                if ($match.Count -eq 0) {
                    throw "HVERR:INVALID_ARGUMENT|boot_order entry '$tok' matches nothing attached to '$($P.name)'"
                }
                foreach ($m in $match) { $devices += $m }
            }
            Set-VMFirmware -VM $vm -BootOrder $devices | Out-Null
        }
        switch ($P.secure_boot) {
            'off'     { Set-VMFirmware -VM $vm -EnableSecureBoot Off | Out-Null }
            # Linux distributions are signed by the third-party UEFI CA, not the
            # Windows one, so the default template refuses to boot them.
            'linux'   { Set-VMFirmware -VM $vm -EnableSecureBoot On -SecureBootTemplate 'MicrosoftUEFICertificateAuthority' | Out-Null }
            'windows' { Set-VMFirmware -VM $vm -EnableSecureBoot On -SecureBootTemplate 'MicrosoftWindows' | Out-Null }
        }
        if ($P.console_mode) { Set-VMFirmware -VM $vm -ConsoleMode $P.console_mode | Out-Null }
        if ($P.set_pause) {
            $mode = if ($P.pause) { 'On' } else { 'Off' }
            Set-VMFirmware -VM $vm -PauseAfterBootFailure $mode | Out-Null
        }
    } else {
        if ($P.secure_boot) {
            throw "HVERR:INVALID_ARGUMENT|secure boot needs UEFI firmware and '$($P.name)' is Generation 1"
        }
        if ($P.console_mode) {
            throw "HVERR:INVALID_ARGUMENT|console_mode needs UEFI firmware and '$($P.name)' is Generation 1"
        }
        if ($P.set_pause) {
            throw "HVERR:INVALID_ARGUMENT|pause_after_boot_failure needs UEFI firmware and '$($P.name)' is Generation 1"
        }
        if ($order.Count -gt 0) {
            $map = @{ 'disk' = 'IDE'; 'dvd' = 'CD'; 'network' = 'LegacyNetworkAdapter'; 'floppy' = 'Floppy' }
            $want = @()
            foreach ($tok in $order) {
                $kind = [string]$tok
                $i = $kind.IndexOf(':')
                if ($i -ge 0) { $kind = $kind.Substring(0, $i) }
                $class = $map[$kind.Trim().ToLower()]
                if (-not $class) {
                    throw "HVERR:INVALID_ARGUMENT|boot_order entry '$tok' names a device class a Generation 1 BIOS does not have"
                }
                if ($want -notcontains $class) { $want += $class }
            }
            # The BIOS order is a permutation, not a prefix: anything the caller
            # left out keeps its current relative position at the end.
            foreach ($cur in @(Get-VMBios -VM $vm).StartupOrder) {
                $s = $cur.ToString()
                if ($want -notcontains $s) { $want += $s }
            }
            Set-VMBios -VM $vm -StartupOrder $want | Out-Null
        }
        if ($P.set_numlock) {
            if ($P.numlock) { Set-VMBios -VM $vm -EnableNumLock | Out-Null }
            else            { Set-VMBios -VM $vm -DisableNumLock | Out-Null }
        }
    }
`
	return c.runSettings(ctx, script, args)
}

// automaticActions are the enumerations Set-VM accepts, keyed by their lowercase
// form so a caller's casing does not matter.
var (
	startActions = map[string]string{
		"nothing": "Nothing", "startifrunning": "StartIfRunning", "start": "Start",
	}
	stopActions = map[string]string{
		"turnoff": "TurnOff", "save": "Save", "shutdown": "ShutDown",
	}
	criticalActions = map[string]string{
		"none": "None", "pause": "Pause",
	}
	checkpointTypes = map[string]string{
		"disabled": "Disabled", "production": "Production",
		"productiononly": "ProductionOnly", "standard": "Standard",
	}
	transportTypes = map[string]string{
		"vmbus": "VMBus", "hvsocket": "HvSocket",
	}
)

// canonical maps value onto its Hyper-V spelling, or reports what was allowed.
func canonical(field, value string, allowed map[string]string) (string, error) {
	if value == "" {
		return "", nil
	}
	if v, ok := allowed[strings.ToLower(strings.TrimSpace(value))]; ok {
		return v, nil
	}
	names := make([]string, 0, len(allowed))
	for _, v := range allowed {
		names = append(names, `"`+v+`"`)
	}
	// Sorted, because ranging a map would list the options differently each time
	// and make the same mistake look like a different error.
	sort.Strings(names)
	return "", hverr.New(hverr.InvalidArgument,
		"%s must be one of %s, got %q", field, strings.Join(names, ", "), value)
}

// SetVMOptions changes the VM's own management settings: what the host does to
// it on start and shutdown, how it checkpoints, and where its files live.
func (c *Client) SetVMOptions(ctx context.Context, o VMOptionsUpdate) (*VMSettings, error) {
	if o.VMName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}

	var err error
	if o.AutomaticStartAction, err = canonical("automatic_start_action", o.AutomaticStartAction, startActions); err != nil {
		return nil, err
	}
	if o.AutomaticStopAction, err = canonical("automatic_stop_action", o.AutomaticStopAction, stopActions); err != nil {
		return nil, err
	}
	if o.AutomaticCriticalErrorAction, err = canonical("automatic_critical_error_action", o.AutomaticCriticalErrorAction, criticalActions); err != nil {
		return nil, err
	}
	if o.CheckpointType, err = canonical("checkpoint_type", o.CheckpointType, checkpointTypes); err != nil {
		return nil, err
	}
	if o.EnhancedSessionTransportType, err = canonical("enhanced_session_transport_type", o.EnhancedSessionTransportType, transportTypes); err != nil {
		return nil, err
	}
	if o.AutomaticStartDelaySeconds != nil && *o.AutomaticStartDelaySeconds < 0 {
		return nil, hverr.New(hverr.InvalidArgument, "automatic_start_delay_seconds cannot be negative")
	}
	if o.AutomaticCriticalErrorTimeoutMinutes != nil && *o.AutomaticCriticalErrorTimeoutMinutes < 0 {
		return nil, hverr.New(hverr.InvalidArgument, "automatic_critical_error_action_timeout_minutes cannot be negative")
	}

	args := map[string]any{
		"name":          o.VMName,
		"start_action":  o.AutomaticStartAction,
		"stop_action":   o.AutomaticStopAction,
		"critical":      o.AutomaticCriticalErrorAction,
		"checkpoint":    o.CheckpointType,
		"transport":     o.EnhancedSessionTransportType,
		"snapshot_path": "",
		"paging_path":   "",
		"set_notes":     o.Notes != nil,
		"notes":         "",
	}
	if o.Notes != nil {
		args["notes"] = *o.Notes
	}
	setInt(args, "start_delay", o.AutomaticStartDelaySeconds)
	setInt(args, "critical_timeout", o.AutomaticCriticalErrorTimeoutMinutes)
	setBool(args, "auto_checkpoints", o.AutomaticCheckpointsEnabled)
	setBool(args, "cache_types", o.GuestControlledCacheTypes)
	setBool(args, "lock", o.LockOnDisconnect)

	// Validated for this service's own access before anything is changed, so a
	// directory Hyper-V cannot use fails here rather than the next time the VM
	// tries to checkpoint or page.
	if o.CheckpointFileLocation != "" {
		p, err := winpath.ValidateDir(o.CheckpointFileLocation, o.CreateParents)
		if err != nil {
			return nil, err
		}
		args["snapshot_path"] = p
	}
	if o.SmartPagingFilePath != "" {
		p, err := winpath.ValidateDir(o.SmartPagingFilePath, o.CreateParents)
		if err != nil {
			return nil, err
		}
		args["paging_path"] = p
	}

	const script = `
    $a = @{ VM = $vm }
    if ($P.set_notes)            { $a['Notes']                                = [string]$P.notes }
    if ($P.start_action)         { $a['AutomaticStartAction']                 = $P.start_action }
    if ($P.set_start_delay)      { $a['AutomaticStartDelay']                  = [int]$P.start_delay }
    if ($P.stop_action)          { $a['AutomaticStopAction']                  = $P.stop_action }
    if ($P.critical)             { $a['AutomaticCriticalErrorAction']         = $P.critical }
    if ($P.set_critical_timeout) { $a['AutomaticCriticalErrorActionTimeout']  = [int]$P.critical_timeout }
    if ($P.checkpoint)           { $a['CheckpointType']                       = $P.checkpoint }
    if ($P.set_auto_checkpoints) { $a['AutomaticCheckpointsEnabled']          = [bool]$P.auto_checkpoints }
    if ($P.snapshot_path)        { $a['SnapshotFileLocation']                 = $P.snapshot_path }
    if ($P.paging_path)          { $a['SmartPagingFilePath']                  = $P.paging_path }
    if ($P.transport)            { $a['EnhancedSessionTransportType']         = $P.transport }
    if ($P.set_cache_types)      { $a['GuestControlledCacheTypes']            = [bool]$P.cache_types }
    if ($P.set_lock)             { $a['LockOnDisconnect']                     = $(if ($P.lock) { 'On' } else { 'Off' }) }
    if ($a.Count -le 1) { throw "HVERR:INVALID_ARGUMENT|nothing to change; pass at least one setting" }
    Set-VM @a | Out-Null
`
	return c.runSettings(ctx, script, args)
}

// integrationServiceNames maps this tool's field names onto the names Hyper-V
// files each service under. Hyper-V matches them literally, spaces and all.
var integrationServiceNames = map[string]string{
	"guest_service_interface": "Guest Service Interface",
	"heartbeat":               "Heartbeat",
	"key_value_pair_exchange": "Key-Value Pair Exchange",
	"shutdown":                "Shutdown",
	"time_synchronization":    "Time Synchronization",
	"vss":                     "VSS",
}

// SetVMIntegrationServices turns the guest-facing VMBus services on or off.
//
// Two of these are what other tools in this server depend on: Guest Service
// Interface carries guest_copy_file, and Key-Value Pair Exchange is how Hyper-V
// learns the addresses wait_for_guest_ip waits for. Turning either off breaks
// the corresponding tool for that VM, which is why they are reported back.
func (c *Client) SetVMIntegrationServices(ctx context.Context, o IntegrationOptions) (*VMSettings, error) {
	if o.VMName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}

	type change struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	var changes []change
	for _, f := range []struct {
		key string
		v   *bool
	}{
		{"guest_service_interface", o.GuestServiceInterface},
		{"heartbeat", o.Heartbeat},
		{"key_value_pair_exchange", o.KeyValuePairExchange},
		{"shutdown", o.Shutdown},
		{"time_synchronization", o.TimeSynchronization},
		{"vss", o.VSS},
	} {
		if f.v != nil {
			changes = append(changes, change{Name: integrationServiceNames[f.key], Enabled: *f.v})
		}
	}
	if len(changes) == 0 {
		return nil, hverr.New(hverr.InvalidArgument,
			"nothing to change; pass at least one integration service")
	}

	const script = `
    foreach ($c in @($P.changes)) {
        # -Name filters by wildcard, so an unknown name yields nothing rather
        # than an error, and would otherwise pass silently.
        $svc = @(Get-VMIntegrationService -VM $vm -Name $c.name)
        if ($svc.Count -eq 0) {
            throw "HVERR:INVALID_ARGUMENT|'$($P.name)' has no integration service named '$($c.name)'"
        }
        if ($c.enabled) { Enable-VMIntegrationService  -VMIntegrationService $svc[0] | Out-Null }
        else            { Disable-VMIntegrationService -VMIntegrationService $svc[0] | Out-Null }
    }
`
	return c.runSettings(ctx, script, map[string]any{"name": o.VMName, "changes": changes})
}

// SetVMSecurity turns a VM's virtual TPM and state encryption on or off.
//
// Both need a key protector, which a VM created by this server does not have.
// One is created locally when it is missing, rather than leaving the caller to
// discover that Enable-VMTPM fails on its own for a reason it does not explain.
//
// The VM must be Off, which is checked here because the error Hyper-V returns
// otherwise does not name the VM or its state.
func (c *Client) SetVMSecurity(ctx context.Context, o SecurityOptions) (*VMSettings, error) {
	if o.VMName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	if o.TPMEnabled == nil && o.EncryptStateAndMigrationTraffic == nil {
		return nil, hverr.New(hverr.InvalidArgument,
			"nothing to change; pass tpm_enabled or encrypt_state_and_migration_traffic")
	}

	args := map[string]any{"name": o.VMName}
	setBool(args, "tpm", o.TPMEnabled)
	setBool(args, "encrypt", o.EncryptStateAndMigrationTraffic)

	const script = `
    if ([int]$vm.Generation -ne 2) {
        throw "HVERR:INVALID_ARGUMENT|a virtual TPM and state encryption need UEFI firmware, and '$($P.name)' is Generation 1"
    }
    if ($vm.State -ne 'Off') {
        throw "HVERR:VM_WRONG_STATE|'$($P.name)' is $($vm.State); its security settings can only be changed while it is Off"
    }

    $wants = ($P.set_tpm -and $P.tpm) -or ($P.set_encrypt -and $P.encrypt)
    if ($wants) {
        $kp = $null
        try { $kp = Get-VMKeyProtector -VM $vm } catch { }
        if (-not $kp -or $kp.Length -le 4) {
            Set-VMKeyProtector -VM $vm -NewLocalKeyProtector | Out-Null
        }
    }
    if ($P.set_encrypt) {
        Set-VMSecurity -VM $vm -EncryptStateAndVmMigrationTraffic ([bool]$P.encrypt) | Out-Null
    }
    if ($P.set_tpm) {
        if ($P.tpm) { Enable-VMTPM  -VM $vm | Out-Null }
        else        { Disable-VMTPM -VM $vm | Out-Null }
    }
`
	return c.runSettings(ctx, script, args)
}

// SetVMVideo fixes the console's framebuffer size.
//
// This is what capture_vm_screen sees and what a guest with no display driver of
// its own gets, which is every Linux guest at its boot console and every Windows
// guest before setup finishes.
func (c *Client) SetVMVideo(ctx context.Context, o VideoOptions) (*VMSettings, error) {
	if o.VMName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	switch strings.ToLower(o.ResolutionType) {
	case "":
	case "default":
		o.ResolutionType = "Default"
	case "single":
		o.ResolutionType = "Single"
	case "maximum":
		o.ResolutionType = "Maximum"
	default:
		return nil, hverr.New(hverr.InvalidArgument,
			`resolution_type must be "Default", "Single" or "Maximum", got %q`, o.ResolutionType)
	}
	if (o.HorizontalResolution > 0) != (o.VerticalResolution > 0) {
		return nil, hverr.New(hverr.InvalidArgument,
			"horizontal_resolution and vertical_resolution go together; give both or neither")
	}
	if o.HorizontalResolution > 0 && o.ResolutionType == "" {
		// A size without Single is stored and ignored, which looks like success.
		o.ResolutionType = "Single"
	}
	if o.ResolutionType == "" {
		return nil, hverr.New(hverr.InvalidArgument,
			"nothing to change; pass resolution_type, or a resolution")
	}
	if o.ResolutionType != "Default" && o.HorizontalResolution == 0 {
		return nil, hverr.New(hverr.InvalidArgument,
			"resolution_type %q needs horizontal_resolution and vertical_resolution; "+
				`only "Default" stands alone`, o.ResolutionType)
	}

	const script = `
    $a = @{ VM = $vm; ResolutionType = $P.resolution_type }
    if ([int]$P.horizontal -gt 0) {
        $a['HorizontalResolution'] = [int]$P.horizontal
        $a['VerticalResolution']   = [int]$P.vertical
    }
    Set-VMVideo @a | Out-Null
`
	return c.runSettings(ctx, script, map[string]any{
		"name":            o.VMName,
		"resolution_type": o.ResolutionType,
		"horizontal":      o.HorizontalResolution,
		"vertical":        o.VerticalResolution,
	})
}

// SetVMComPort attaches a virtual serial port to a host named pipe, or takes it
// away.
//
// A serial port is the one channel into a guest that needs no network, no guest
// agent and no working display: a Linux kernel with console=ttyS0 writes its
// whole boot there, and a Windows kernel debugger attaches over it.
func (c *Client) SetVMComPort(ctx context.Context, o ComPortOptions) (*VMSettings, error) {
	if o.VMName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	if o.Number != 1 && o.Number != 2 {
		return nil, hverr.New(hverr.InvalidArgument, "number must be 1 or 2, got %d", o.Number)
	}
	if o.Detach {
		o.Path = ""
	} else if o.Path != "" {
		// Hyper-V backs a COM port with a named pipe and nothing else; a file
		// path is accepted at the API and then never opened.
		if !strings.HasPrefix(strings.ToLower(o.Path), `\\.\pipe\`) {
			return nil, hverr.New(hverr.InvalidArgument,
				`path must be a named pipe such as "\\.\pipe\%s-com%d", got %q`,
				o.VMName, o.Number, o.Path)
		}
	}
	if o.Path == "" && !o.Detach && o.DebuggerMode == nil {
		return nil, hverr.New(hverr.InvalidArgument,
			"nothing to change; pass path, detach or debugger_mode")
	}

	args := map[string]any{
		"name":     o.VMName,
		"number":   o.Number,
		"path":     o.Path,
		"set_path": o.Path != "" || o.Detach,
	}
	setBool(args, "debugger", o.DebuggerMode)

	const script = `
    $a = @{ VM = $vm; Number = [int]$P.number }
    if ($P.set_path)     { $a['Path']         = [string]$P.path }
    if ($P.set_debugger) { $a['DebuggerMode'] = $(if ($P.debugger) { 'On' } else { 'Off' }) }
    Set-VMComPort @a | Out-Null
`
	return c.runSettings(ctx, script, args)
}

// SetVMDiskSettings changes an attached disk's storage QoS or moves it to
// another controller port.
//
// IOPS limits are counted in normalized 8 KB operations, so a 32 KB read costs
// four. Zero means unlimited, which is why both bounds are pointers: 0 is a real
// value here rather than "unset".
func (c *Client) SetVMDiskSettings(ctx context.Context, o DiskSettingsOptions) (*VMSettings, error) {
	if o.VMName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "name is required")
	}
	if o.Path == "" {
		return nil, hverr.New(hverr.InvalidArgument, "path is required to say which disk is meant")
	}
	for _, f := range []struct {
		label string
		v     *int64
	}{{"minimum_iops", o.MinimumIOPS}, {"maximum_iops", o.MaximumIOPS}} {
		if f.v != nil && *f.v < 0 {
			return nil, hverr.New(hverr.InvalidArgument, "%s cannot be negative", f.label)
		}
	}
	if o.MinimumIOPS != nil && o.MaximumIOPS != nil &&
		*o.MaximumIOPS > 0 && *o.MinimumIOPS > *o.MaximumIOPS {
		return nil, hverr.New(hverr.InvalidArgument,
			"minimum_iops (%d) is above maximum_iops (%d)", *o.MinimumIOPS, *o.MaximumIOPS)
	}
	switch strings.ToUpper(o.ToControllerType) {
	case "":
	case "SCSI":
		o.ToControllerType = "SCSI"
	case "IDE":
		o.ToControllerType = "IDE"
	default:
		return nil, hverr.New(hverr.InvalidArgument,
			`to_controller_type must be "SCSI" or "IDE", got %q`, o.ToControllerType)
	}
	if o.ToControllerNumber != nil && *o.ToControllerNumber < 0 {
		return nil, hverr.New(hverr.InvalidArgument, "to_controller_number cannot be negative")
	}
	if o.ToControllerLocation != nil && *o.ToControllerLocation < 0 {
		return nil, hverr.New(hverr.InvalidArgument, "to_controller_location cannot be negative")
	}

	args := map[string]any{
		"name":    o.VMName,
		"path":    o.Path,
		"to_type": o.ToControllerType,
	}
	setInt64(args, "min_iops", o.MinimumIOPS)
	setInt64(args, "max_iops", o.MaximumIOPS)
	setBool(args, "reservations", o.SupportPersistentReservations)
	setInt(args, "to_number", o.ToControllerNumber)
	setInt(args, "to_location", o.ToControllerLocation)

	const script = `
    $drive = @(Get-VMHardDiskDrive -VM $vm | Where-Object { $_.Path -eq $P.path })
    if ($drive.Count -eq 0) {
        $have = @(Get-VMHardDiskDrive -VM $vm | ForEach-Object { $_.Path }) -join ', '
        throw "HVERR:PATH_NOT_FOUND|'$($P.path)' is not attached to '$($P.name)' (attached: $have)"
    }
    $a = @{ VMHardDiskDrive = $drive[0] }
    if ($P.set_min_iops)     { $a['MinimumIOPS']                   = [int64]$P.min_iops }
    if ($P.set_max_iops)     { $a['MaximumIOPS']                   = [int64]$P.max_iops }
    if ($P.set_reservations) { $a['SupportPersistentReservations'] = [bool]$P.reservations }
    if ($P.to_type)          { $a['ToControllerType']              = $P.to_type }
    if ($P.set_to_number)    { $a['ToControllerNumber']            = [int]$P.to_number }
    if ($P.set_to_location)  { $a['ToControllerLocation']          = [int]$P.to_location }
    if ($a.Count -le 1) { throw "HVERR:INVALID_ARGUMENT|nothing to change; pass at least one disk setting" }
    Set-VMHardDiskDrive @a | Out-Null
`
	return c.runSettings(ctx, script, args)
}

// ---- argument helpers ------------------------------------------------------
//
// Optional settings travel as a pair: a set_<name> flag and the value. That
// keeps the PowerShell side free of null checks, which $P's JSON decoding makes
// awkward, and lets a zero value mean zero rather than "not given".

func setBool(args map[string]any, name string, v *bool) {
	args["set_"+name] = v != nil
	args[name] = v != nil && *v
}

func setInt(args map[string]any, name string, v *int) {
	args["set_"+name] = v != nil
	if v != nil {
		args[name] = *v
	} else {
		args[name] = 0
	}
}

func setInt64(args map[string]any, name string, v *int64) {
	args["set_"+name] = v != nil
	if v != nil {
		args[name] = *v
	} else {
		args[name] = int64(0)
	}
}

// setInt64MB records a megabyte figure as the byte count Hyper-V expects.
func setInt64MB(args map[string]any, name string, v *int) {
	args["set_"+name] = v != nil
	if v != nil {
		args[name] = int64(*v) * 1024 * 1024
	} else {
		args[name] = int64(0)
	}
}
