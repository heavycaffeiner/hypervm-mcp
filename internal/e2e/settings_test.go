//go:build windows

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The settings tools are checked by writing a value and reading it back through
// the same vocabulary, because that round trip is the whole promise: what
// get_vm_settings reports is what a set_vm_* tool accepts.
//
// It builds and destroys its own VMs, so it needs no guest and disturbs nothing.
//
//	$env:HYPERVM_E2E="C:\ProgramData\hypervm-mcp-dev\bin\hypervm-mcp-dev.exe"
//	go test ./internal/e2e -run Settings -v -count=1

const settingsVM = "hypervm-settings-probe"

// vmSettings mirrors the tools' output. Only the fields this test asserts on are
// named; anything else the server adds is ignored.
type vmSettings struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	Generation int    `json:"generation"`

	Memory struct {
		StartupBytes   int64 `json:"startup_bytes"`
		MinimumBytes   int64 `json:"minimum_bytes"`
		MaximumBytes   int64 `json:"maximum_bytes"`
		DynamicEnabled bool  `json:"dynamic_enabled"`
		BufferPercent  int   `json:"buffer_percent"`
		Priority       int   `json:"priority"`
	} `json:"memory"`

	Processor struct {
		Count                int `json:"count"`
		ReservePercent       int `json:"reserve_percent"`
		MaximumPercent       int `json:"maximum_percent"`
		RelativeWeight       int `json:"relative_weight"`
		HwThreadCountPerCore int `json:"hw_thread_count_per_core"`
	} `json:"processor"`

	Firmware *struct {
		SecureBoot            string `json:"secure_boot"`
		SecureBootTemplate    string `json:"secure_boot_template"`
		ConsoleMode           string `json:"console_mode"`
		PauseAfterBootFailure bool   `json:"pause_after_boot_failure"`
		BootOrder             []struct {
			Kind  string `json:"kind"`
			Token string `json:"token"`
		} `json:"boot_order"`
	} `json:"firmware"`

	BIOS *struct {
		NumLockEnabled bool     `json:"num_lock_enabled"`
		StartupOrder   []string `json:"startup_order"`
	} `json:"bios"`

	Options struct {
		Notes                       string `json:"notes"`
		AutomaticStartAction        string `json:"automatic_start_action"`
		AutomaticStartDelaySeconds  int    `json:"automatic_start_delay_seconds"`
		AutomaticStopAction         string `json:"automatic_stop_action"`
		CheckpointType              string `json:"checkpoint_type"`
		AutomaticCheckpointsEnabled bool   `json:"automatic_checkpoints_enabled"`
		LockOnDisconnect            bool   `json:"lock_on_disconnect"`
	} `json:"options"`

	Security struct {
		TPMEnabled          bool `json:"tpm_enabled"`
		KeyProtectorPresent bool `json:"key_protector_present"`
	} `json:"security"`

	Video *struct {
		ResolutionType       string `json:"resolution_type"`
		HorizontalResolution int    `json:"horizontal_resolution"`
		VerticalResolution   int    `json:"vertical_resolution"`
	} `json:"video"`

	IntegrationServices []struct {
		Key     string `json:"key"`
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	} `json:"integration_services"`

	ComPorts []struct {
		Number int    `json:"number"`
		Path   string `json:"path"`
	} `json:"com_ports"`

	HardDrives []struct {
		Path               string `json:"path"`
		ControllerLocation int    `json:"controller_location"`
		MaximumIOPS        int64  `json:"maximum_iops"`
	} `json:"hard_drives"`

	NetworkAdapters []struct {
		Name                 string `json:"name"`
		DHCPGuard            bool   `json:"dhcp_guard"`
		RouterGuard          bool   `json:"router_guard"`
		DeviceNaming         bool   `json:"device_naming"`
		MaximumBandwidthMbps int64  `json:"maximum_bandwidth_mbps"`
		VLANMode             string `json:"vlan_mode"`
		TrunkNativeVLANID    int    `json:"trunk_native_vlan_id"`
	} `json:"network_adapters"`
}

// service looks a component up by its stable key, never by its name: Hyper-V
// names these in the host's display language, so on a Korean host the name is
// "시간 동기화" and matching "Time Synchronization" finds nothing.
func (s *vmSettings) service(key string) (bool, bool) {
	for _, svc := range s.IntegrationServices {
		if svc.Key == key {
			return svc.Enabled, true
		}
	}
	return false, false
}

const mib = 1024 * 1024

// settingsOf runs a tool and decodes its result into a fresh value.
//
// Decoding into a reused struct would be wrong in a way that is easy to miss:
// encoding/json merges into whatever is already there, so a field the server
// omits keeps the value the previous response left behind. That turns a
// successful change into an apparent failure.
func settingsOf(t *testing.T, s *mcp.ClientSession, ctx context.Context, tool string, args map[string]any) vmSettings {
	t.Helper()
	var out vmSettings
	call(t, s, ctx, tool, args, &out)
	return out
}

func TestSettingsGeneration2(t *testing.T) {
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	_ = tryCall(t, session, ctx, "delete_vm",
		map[string]any{"name": settingsVM, "delete_disks": true, "force": true})
	call(t, session, ctx, "create_vm", map[string]any{
		"name": settingsVM, "generation": 2, "memory_mb": 1024, "cpu_count": 1,
		"vhd_size_mb": 1024, "secure_boot": "off",
	}, nil)
	defer func() {
		_ = tryCall(t, session, context.Background(), "delete_vm",
			map[string]any{"name": settingsVM, "delete_disks": true, "force": true})
	}()

	s := settingsOf(t, session, ctx, "get_vm_settings", map[string]any{"name": settingsVM})
	if s.Firmware == nil {
		t.Fatal("a Generation 2 VM must report firmware")
	}
	if s.BIOS != nil {
		t.Error("a Generation 2 VM must not report a BIOS")
	}

	t.Run("memory", func(t *testing.T) {
		s = settingsOf(t, session, ctx, "set_vm_memory", map[string]any{
			"name": settingsVM, "dynamic": true, "startup_mb": 1024,
			"minimum_mb": 512, "maximum_mb": 2048,
			"buffer_percent": 25, "priority_weight": 60,
		})
		if !s.Memory.DynamicEnabled {
			t.Error("dynamic memory did not turn on")
		}
		for _, c := range []struct {
			label string
			got   int64
			want  int64
		}{
			{"startup", s.Memory.StartupBytes, 1024 * mib},
			{"minimum", s.Memory.MinimumBytes, 512 * mib},
			{"maximum", s.Memory.MaximumBytes, 2048 * mib},
		} {
			if c.got != c.want {
				t.Errorf("%s memory is %d bytes, want %d", c.label, c.got, c.want)
			}
		}
		if s.Memory.BufferPercent != 25 || s.Memory.Priority != 60 {
			t.Errorf("buffer=%d priority=%d, want 25 and 60", s.Memory.BufferPercent, s.Memory.Priority)
		}

		// The three bounds constrain each other, so moving startup past the old
		// maximum only works because they are applied together.
		s = settingsOf(t, session, ctx, "set_vm_memory", map[string]any{
			"name": settingsVM, "startup_mb": 3072, "maximum_mb": 4096,
		})
		if s.Memory.StartupBytes != 3072*mib || s.Memory.MaximumBytes != 4096*mib {
			t.Errorf("startup=%d maximum=%d after a combined raise", s.Memory.StartupBytes, s.Memory.MaximumBytes)
		}
	})

	t.Run("processor", func(t *testing.T) {
		s = settingsOf(t, session, ctx, "set_vm_processor", map[string]any{
			"name": settingsVM, "count": 2, "reserve_percent": 10,
			"maximum_percent": 90, "relative_weight": 200, "hw_thread_count_per_core": 1,
		})
		if s.Processor.Count != 2 || s.Processor.ReservePercent != 10 ||
			s.Processor.MaximumPercent != 90 || s.Processor.RelativeWeight != 200 ||
			s.Processor.HwThreadCountPerCore != 1 {
			t.Errorf("processor read back as %+v", s.Processor)
		}
	})

	t.Run("firmware", func(t *testing.T) {
		s = settingsOf(t, session, ctx, "set_vm_firmware", map[string]any{
			"name": settingsVM, "secure_boot": "linux",
			"boot_order": []string{"network", "disk"}, "pause_after_boot_failure": true,
		})
		if s.Firmware.SecureBoot != "On" {
			t.Errorf("secure boot is %q, want On", s.Firmware.SecureBoot)
		}
		if s.Firmware.SecureBootTemplate != "MicrosoftUEFICertificateAuthority" {
			t.Errorf("secure boot template is %q", s.Firmware.SecureBootTemplate)
		}
		if !s.Firmware.PauseAfterBootFailure {
			t.Error("pause_after_boot_failure did not take")
		}
		if len(s.Firmware.BootOrder) < 2 ||
			s.Firmware.BootOrder[0].Kind != "network" || s.Firmware.BootOrder[1].Kind != "disk" {
			t.Errorf("boot order is %+v, want network then disk", s.Firmware.BootOrder)
		}

		// A token reported by the read must put that same device back.
		if len(s.Firmware.BootOrder) >= 2 {
			tokens := []string{s.Firmware.BootOrder[1].Token, s.Firmware.BootOrder[0].Token}
			s = settingsOf(t, session, ctx, "set_vm_firmware", map[string]any{"name": settingsVM, "boot_order": tokens})
			if s.Firmware.BootOrder[0].Kind != "disk" {
				t.Errorf("resending tokens did not reorder: %+v", s.Firmware.BootOrder)
			}
		}
	})

	t.Run("options", func(t *testing.T) {
		s = settingsOf(t, session, ctx, "set_vm_options", map[string]any{
			"name": settingsVM, "notes": "written by the e2e suite",
			"automatic_start_action": "Nothing", "automatic_start_delay_seconds": 30,
			"automatic_stop_action": "ShutDown", "checkpoint_type": "Standard",
			"automatic_checkpoints_enabled": false, "lock_on_disconnect": true,
		})
		o := s.Options
		if o.Notes != "written by the e2e suite" || o.AutomaticStartAction != "Nothing" ||
			o.AutomaticStartDelaySeconds != 30 || o.AutomaticStopAction != "ShutDown" ||
			o.CheckpointType != "Standard" || o.AutomaticCheckpointsEnabled || !o.LockOnDisconnect {
			t.Errorf("options read back as %+v", o)
		}
	})

	t.Run("integration services", func(t *testing.T) {
		s = settingsOf(t, session, ctx, "set_vm_integration_services", map[string]any{
			"name": settingsVM, "guest_service_interface": true, "time_synchronization": false,
		})
		if on, ok := s.service("guest_service_interface"); !ok || !on {
			t.Error("Guest Service Interface did not turn on")
		}
		if on, ok := s.service("time_synchronization"); !ok || on {
			t.Error("Time Synchronization did not turn off")
		}
		// Every component must resolve to a key, or a service this server cannot
		// name is a service it cannot set.
		for _, svc := range s.IntegrationServices {
			if svc.Key == "" {
				t.Errorf("no key for integration service %q; its component GUID is unmapped", svc.Name)
			}
		}
	})

	t.Run("security", func(t *testing.T) {
		s = settingsOf(t, session, ctx, "set_vm_security", map[string]any{"name": settingsVM, "tpm_enabled": true})
		if !s.Security.TPMEnabled {
			t.Error("the virtual TPM did not turn on")
		}
		// A key protector is what makes the TPM possible, and this server creates
		// one when it is missing rather than letting Enable-VMTPM fail obscurely.
		if !s.Security.KeyProtectorPresent {
			t.Error("no key protector was created")
		}
	})

	t.Run("video", func(t *testing.T) {
		s = settingsOf(t, session, ctx, "set_vm_video", map[string]any{
			"name": settingsVM, "horizontal_resolution": 1280, "vertical_resolution": 800,
		})
		if s.Video == nil {
			t.Fatal("no video settings reported")
		}
		if s.Video.ResolutionType != "Single" ||
			s.Video.HorizontalResolution != 1280 || s.Video.VerticalResolution != 800 {
			t.Errorf("video read back as %+v", *s.Video)
		}
	})

	t.Run("com port", func(t *testing.T) {
		const pipe = `\\.\pipe\hypervm-settings-probe-com1`
		s = settingsOf(t, session, ctx, "set_vm_com_port", map[string]any{"name": settingsVM, "number": 1, "path": pipe})
		var found bool
		for _, c := range s.ComPorts {
			if c.Number == 1 && c.Path == pipe {
				found = true
			}
		}
		if !found {
			t.Errorf("com ports read back as %+v, want port 1 on %s", s.ComPorts, pipe)
		}

		// A file path is accepted by Hyper-V and then never opened, so it has to
		// be refused here rather than looking like success.
		if err := tryCall(t, session, ctx, "set_vm_com_port",
			map[string]any{"name": settingsVM, "number": 1, "path": `D:\not-a-pipe.txt`}); err == nil {
			t.Error("a file path was accepted as a COM port backing")
		}

		s = settingsOf(t, session, ctx, "set_vm_com_port", map[string]any{"name": settingsVM, "number": 1, "detach": true})
		for _, c := range s.ComPorts {
			if c.Number == 1 && c.Path != "" {
				t.Errorf("com port 1 is still attached to %q", c.Path)
			}
		}
	})

	t.Run("disk", func(t *testing.T) {
		if len(s.HardDrives) == 0 {
			t.Skip("the probe VM has no disk")
		}
		path := s.HardDrives[0].Path
		s = settingsOf(t, session, ctx, "set_vm_disk_settings", map[string]any{
			"name": settingsVM, "path": path, "maximum_iops": 500,
			"to_controller_location": 3,
		})
		if s.HardDrives[0].MaximumIOPS != 500 {
			t.Errorf("maximum IOPS is %d, want 500", s.HardDrives[0].MaximumIOPS)
		}
		if s.HardDrives[0].ControllerLocation != 3 {
			t.Errorf("the disk sits at location %d, want 3", s.HardDrives[0].ControllerLocation)
		}
	})

	t.Run("adapter features", func(t *testing.T) {
		if len(s.NetworkAdapters) == 0 {
			t.Skip("the probe VM has no adapter")
		}
		s = settingsOf(t, session, ctx, "set_vm_network_advanced", map[string]any{
			"vm_name": settingsVM, "dhcp_guard": true, "router_guard": true,
			"device_naming": true, "maximum_bandwidth_mbps": 100,
		})
		a := s.NetworkAdapters[0]
		if !a.DHCPGuard || !a.RouterGuard || !a.DeviceNaming || a.MaximumBandwidthMbps != 100 {
			t.Errorf("adapter features read back as %+v", a)
		}

		s = settingsOf(t, session, ctx, "set_vm_network_advanced", map[string]any{
			"vm_name": settingsVM, "trunk_native_vlan_id": 1,
			"trunk_allowed_vlan_ids": []int{10, 20},
		})
		if s.NetworkAdapters[0].VLANMode != "Trunk" || s.NetworkAdapters[0].TrunkNativeVLANID != 1 {
			t.Errorf("trunk read back as mode=%q native=%d",
				s.NetworkAdapters[0].VLANMode, s.NetworkAdapters[0].TrunkNativeVLANID)
		}

		// Two ways to reserve the same bandwidth: a switch honours one or the
		// other, so asking for both has to be refused rather than resolved.
		if err := tryCall(t, session, ctx, "set_vm_network_advanced", map[string]any{
			"vm_name": settingsVM, "minimum_bandwidth_mbps": 50, "minimum_bandwidth_weight": 20,
		}); err == nil {
			t.Error("both bandwidth reservations were accepted at once")
		}

		name := s.NetworkAdapters[0].Name
		s = settingsOf(t, session, ctx, "remove_vm_network_adapter", map[string]any{"vm_name": settingsVM, "adapter_name": name})
		if len(s.NetworkAdapters) != 0 {
			t.Errorf("the adapter survived removal: %+v", s.NetworkAdapters)
		}
	})
}

// A Generation 1 VM has a BIOS rather than UEFI firmware, so its boot order is a
// permutation of four device classes and nothing else can be expressed. The
// tools have to route to Set-VMBios by themselves.
func TestSettingsGeneration1(t *testing.T) {
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	name := settingsVM + "-gen1"
	_ = tryCall(t, session, ctx, "delete_vm",
		map[string]any{"name": name, "delete_disks": true, "force": true})
	call(t, session, ctx, "create_vm", map[string]any{
		"name": name, "generation": 1, "memory_mb": 512, "cpu_count": 1, "vhd_size_mb": 1024,
	}, nil)
	defer func() {
		_ = tryCall(t, session, context.Background(), "delete_vm",
			map[string]any{"name": name, "delete_disks": true, "force": true})
	}()

	s := settingsOf(t, session, ctx, "get_vm_settings", map[string]any{"name": name})
	if s.BIOS == nil {
		t.Fatal("a Generation 1 VM must report a BIOS")
	}
	if s.Firmware != nil {
		t.Error("a Generation 1 VM must not report UEFI firmware")
	}

	// Naming one class is enough: the rest keep their current order behind it,
	// because the BIOS wants a full permutation and a caller should not have to.
	s = settingsOf(t, session, ctx, "set_vm_firmware", map[string]any{"name": name, "boot_order": []string{"dvd"}, "num_lock": true})
	if len(s.BIOS.StartupOrder) != 4 {
		t.Errorf("the startup order has %d entries, want all 4: %v",
			len(s.BIOS.StartupOrder), s.BIOS.StartupOrder)
	}
	if len(s.BIOS.StartupOrder) == 0 || s.BIOS.StartupOrder[0] != "CD" {
		t.Errorf("the startup order is %v, want CD first", s.BIOS.StartupOrder)
	}
	if !s.BIOS.NumLockEnabled {
		t.Error("num lock did not turn on")
	}

	// Secure boot needs UEFI, and saying so beats letting Hyper-V fail obscurely.
	if err := tryCall(t, session, ctx, "set_vm_firmware",
		map[string]any{"name": name, "secure_boot": "windows"}); err == nil {
		t.Error("secure boot was accepted on a Generation 1 VM")
	}
	if err := tryCall(t, session, ctx, "set_vm_security",
		map[string]any{"name": name, "tpm_enabled": true}); err == nil {
		t.Error("a virtual TPM was accepted on a Generation 1 VM")
	}
}
