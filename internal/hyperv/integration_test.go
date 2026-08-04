//go:build windows

package hyperv

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/psrun"
)

// These tests talk to the real Hyper-V on this machine, so they are opt-in:
//
//	$env:HYPERVM_INTEGRATION=1; go test ./internal/hyperv -run Integration -v
//
// They must be run from an elevated shell, since Hyper-V refuses unprivileged
// callers — which is the whole reason this project exists.
func integrationClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("HYPERVM_INTEGRATION") == "" {
		t.Skip("set HYPERVM_INTEGRATION=1 to run against real Hyper-V")
	}
	return NewClient(psrun.New(config.DefaultPowerShellPath(), 60*time.Second, 4))
}

func TestIntegrationListVMs(t *testing.T) {
	c := integrationClient(t)

	vms, err := c.ListVMs(context.Background(), "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	t.Logf("found %d VM(s)", len(vms))
	for _, vm := range vms {
		if vm.Name == "" || vm.ID == "" || vm.State == "" {
			b, _ := json.Marshal(vm)
			t.Errorf("incomplete summary: %s", b)
		}
		t.Logf("  %-30s %-8s gen%d  %d MB", vm.Name, vm.State, vm.Generation, vm.MemoryAssigned/(1024*1024))
	}
}

// A filter that matches nothing must return an empty list, not an error.
func TestIntegrationListVMsNoMatch(t *testing.T) {
	c := integrationClient(t)

	vms, err := c.ListVMs(context.Background(), "zzz-no-such-vm-*")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(vms) != 0 {
		t.Fatalf("got %d VMs, want 0", len(vms))
	}
}

func TestIntegrationGetVM(t *testing.T) {
	c := integrationClient(t)

	vms, err := c.ListVMs(context.Background(), "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(vms) == 0 {
		t.Skip("no VMs on this host")
	}

	detail, err := c.GetVM(context.Background(), vms[0].Name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	b, _ := json.MarshalIndent(detail, "", "  ")
	t.Logf("%s", b)

	if detail.ConfigurationLocation == "" {
		t.Error("configuration_location is empty")
	}
	if detail.ProcessorCount == 0 {
		t.Error("processor_count is zero")
	}
}

func TestIntegrationGetVMNotFound(t *testing.T) {
	c := integrationClient(t)

	_, err := c.GetVM(context.Background(), "zzz-no-such-vm")
	if err == nil {
		t.Fatal("expected an error")
	}
	t.Logf("error: %v", err)
}

// TestIntegrationNestedVirtualization covers the two prerequisites Hyper-V was
// measured not to enforce: it accepts the change on a running VM and ignores it,
// and it starts a VM that has both nested virtualization and dynamic memory.
// Both are this package's job, so both are checked against the real thing.
func TestIntegrationNestedVirtualization(t *testing.T) {
	c := integrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	const name = "hypervm-mcp-nested-test"
	// Dynamic memory on, so that enabling has something to turn off.
	vm, err := c.CreateVM(ctx, CreateVMOptions{
		Name: name, MemoryMB: 1024, DynamicMemory: true, CPUCount: 2,
		VHDSizeMB: 1024, CreateParents: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		// Its own context: the test's may already be spent by the time this runs.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if _, err := c.DeleteVM(ctx, name, true, true); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	if vm.NestedVirtualization {
		t.Fatal("a fresh VM should not have nested virtualization on")
	}
	if !vm.DynamicMemoryEnabled {
		t.Fatal("wanted dynamic memory on, so enabling has something to turn off")
	}

	on, err := c.SetNestedVirtualization(ctx, name, true)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !on.NestedVirtualization {
		t.Error("nested virtualization did not come on")
	}
	if on.DynamicMemoryEnabled {
		t.Error("dynamic memory is still on; a guest hypervisor needs its memory backed")
	}

	// The ordinary detail path has to agree, or get_vm would contradict the tool
	// that just reported success.
	got, err := c.GetVM(ctx, name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.NestedVirtualization {
		t.Error("get_vm does not report nested virtualization as on")
	}

	if _, err := c.StartVM(ctx, name); err != nil {
		t.Fatalf("start: %v", err)
	}
	_, err = c.SetNestedVirtualization(ctx, name, false)
	if err == nil {
		t.Fatal("changing it on a running VM was allowed; Hyper-V ignores that silently")
	}
	if !hverr.Is(err, hverr.VMWrongState) {
		t.Errorf("got %v, want %s", err, hverr.VMWrongState)
	}
	t.Logf("running VM refused: %v", err)

	// Refusing must also mean changing nothing.
	if got, err = c.GetVM(ctx, name); err != nil {
		t.Fatalf("get: %v", err)
	} else if !got.NestedVirtualization {
		t.Error("the refused call turned it off anyway")
	}

	if _, err := c.StopVM(ctx, name, true, 0); err != nil {
		t.Fatalf("stop: %v", err)
	}
	off, err := c.SetNestedVirtualization(ctx, name, false)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if off.NestedVirtualization {
		t.Error("nested virtualization did not go off")
	}
}
