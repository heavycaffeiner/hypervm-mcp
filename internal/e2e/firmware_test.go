//go:build windows

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The console tools are meant to work with no guest at all — that is the reason
// they exist, since everything else needs something inside the VM to answer. The
// other tests all had an operating system running, so none of them checked that.
//
// This one uses a VM with no disk and no media, which gets as far as its firmware
// and stops. It is also the only test of a Generation 1 VM: Hyper-V presents
// emulated devices to those rather than synthetic ones, and a tool that only
// knows the synthetic names would fail on every legacy guest.
//
// It creates and destroys its own VM, so it needs no guest and disturbs nothing.
//
//	$env:HYPERVM_E2E="C:\ProgramData\hypervm-mcp-dev\bin\hypervm-mcp-dev.exe"
//	go test ./internal/e2e -run Firmware -v -count=1

const firmwareVM = "hypervm-firmware-probe"

func TestFirmwareConsole(t *testing.T) {
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	for _, gen := range []int{1, 2} {
		gen := gen
		t.Run(map[int]string{1: "generation1", 2: "generation2"}[gen], func(t *testing.T) {
			name := firmwareVM
			_ = tryCall(t, session, ctx, "delete_vm",
				map[string]any{"name": name, "delete_disks": true, "force": true})

			// No disk and no ISO: the firmware looks for something to boot,
			// finds nothing, and stays on screen saying so.
			var vm map[string]any
			call(t, session, ctx, "create_vm", map[string]any{
				"name": name, "generation": gen, "memory_mb": 512, "cpu_count": 1,
				"secure_boot": "off",
			}, &vm)
			defer func() {
				_ = tryCall(t, session, context.Background(), "delete_vm",
					map[string]any{"name": name, "delete_disks": true, "force": true})
			}()

			call(t, session, ctx, "start_vm", map[string]any{"name": name}, nil)
			// Firmware needs a moment to give up looking for a boot device.
			time.Sleep(20 * time.Second)

			dir := filepath.Join(rockyArtifact, "screens")
			_ = os.MkdirAll(dir, 0o755)
			shot := filepath.Join(dir, "firmware-gen"+string(rune('0'+gen))+".png")

			// No size given: the console's own is the only one guaranteed to
			// work, and a Generation 1 firmware screen is nowhere near 1024x768.
			meta := captureAt(t, session, ctx, name, shot, 0, 0)
			t.Logf("gen%d: captured %dx%d, guest reports %dx%d, %d bytes of PNG",
				gen, meta.Width, meta.Height, meta.GuestWidth, meta.GuestHeight, meta.PNG)
			if meta.Blank {
				t.Fatalf("gen%d: the firmware screen came back blank: %s", gen, meta.Note)
			}
			colours, dominant, share := colourSpread(t, mustRead(t, shot))
			t.Logf("gen%d: %d colours, %v covers %.2f%%", gen, colours, dominant, share*100)
			if share >= 0.999 {
				t.Fatalf("gen%d: nothing is drawn on the firmware screen", gen)
			}
			t.Logf("gen%d: a VM with no operating system can still be looked at", gen)

			// The keyboard is what firmware listens to, so this is the one input
			// that has to work before an OS exists.
			var keys map[string]any
			if err := tryCall(t, session, ctx, "send_vm_key", map[string]any{
				"vm_name": name, "keys": []string{"enter"}, "repeat": 2, "interval_ms": 300,
			}); err != nil {
				t.Errorf("gen%d: the firmware keyboard refused input: %v", gen, err)
			} else {
				call(t, session, ctx, "send_vm_key", map[string]any{
					"vm_name": name, "keys": []string{"esc"},
				}, &keys)
				t.Logf("gen%d: keyboard accepted %v key(s)", gen, keys["sent"])
			}

			// The pointer is the interesting one. Firmware binds no mouse, and a
			// Generation 1 VM is given an emulated one rather than the synthetic
			// device. Either way the answer has to name the cause.
			err := tryCall(t, session, ctx, "send_vm_mouse", map[string]any{
				"vm_name": name, "x": 100, "y": 100,
				"screen_width": meta.Width, "screen_height": meta.Height,
			})
			switch {
			case err == nil:
				t.Logf("gen%d: the pointer was accepted even with no guest", gen)
			case strings.Contains(err.Error(), "pointer"),
				strings.Contains(err.Error(), "resolution"):
				t.Logf("gen%d: no pointer here, refused with an explanation: %v", gen, err)
			default:
				t.Errorf("gen%d: the refusal does not explain itself: %v", gen, err)
			}
		})
	}
}
