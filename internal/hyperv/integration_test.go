//go:build windows

package hyperv

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
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
