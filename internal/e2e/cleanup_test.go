//go:build windows

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCleanup removes everything the suite created.
//
// It is opt-in and deliberately separate: a cleanup that ran automatically would
// destroy the VM another test was about to use, and the tests here are meant to
// be runnable one at a time against a guest that persists between them.
//
// The downloaded ISO is left alone. It is an input rather than an artefact, and
// costs two gigabytes to fetch again.
func TestCleanup(t *testing.T) {
	if os.Getenv("HYPERVM_E2E_CLEANUP") == "" {
		t.Skip("set HYPERVM_E2E_CLEANUP=1 to remove the test VMs, disks and keys")
	}
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	for _, vm := range []string{rockyVM, "rocky10-lab1", "rocky10-lab2", cloneVM} {
		if err := tryCall(t, session, ctx, "delete_vm", map[string]any{
			"name": vm, "delete_disks": true, "force": true,
		}); err == nil {
			t.Logf("removed %s and its disks", vm)
		}
		_ = tryCall(t, session, ctx, "ssh_forget_host_key", map[string]any{"name": vm})
	}

	// The lab network, if a run was interrupted before its own cleanup.
	_ = tryCall(t, session, ctx, "set_switch_nat",
		map[string]any{"switch_name": labSwitch, "enable": false})
	_ = tryCall(t, session, ctx, "delete_switch", map[string]any{"name": labSwitch})

	// Disks that were never attached to a VM, so delete_vm would not have caught
	// them.
	loose, _ := filepath.Glob(`D:\HyperV\VHD\*.vhdx`)
	for _, p := range loose {
		if err := os.Remove(p); err == nil {
			t.Logf("removed %s", p)
		}
	}

	// The generated SSH key pair. It only ever reached a disposable guest, but it
	// is still a private key sitting on disk.
	if err := os.RemoveAll(rockyArtifact); err == nil {
		t.Logf("removed %s", rockyArtifact)
	}

	var vms []map[string]any
	callList(t, session, ctx, "list_vms", map[string]any{"name": "rocky10-*"}, &vms)
	if len(vms) != 0 {
		t.Errorf("VMs are still present: %v", vms)
	}
	t.Log("cleanup complete; the downloaded ISO was left in place")
}
