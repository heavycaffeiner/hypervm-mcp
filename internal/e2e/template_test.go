//go:build windows

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

const cloneVM = "rocky10-clone"

// TestCloneFromTemplate provisions a VM from the Rocky disk as a golden image
// and checks the two things that matter about differencing disks: the child
// really is one, and deleting the child does not take the parent with it.
//
// That second point is the dangerous one. A golden image is shared by every VM
// built on it, so deleting one clone's disks must never touch it — losing it
// would be unrecoverable.
//
// It stops the source VM, since a disk attached to a running VM cannot be a
// parent, and starts it again afterwards.
func TestCloneFromTemplate(t *testing.T) {
	requireRocky(t)
	if os.Getenv("HYPERVM_E2E_CLONE") == "" {
		t.Skip("set HYPERVM_E2E_CLONE=1 to run; this stops and restarts the source VM")
	}
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	const clonePath = `D:\HyperV\VHD\rocky10-clone.vhdx`

	// Clean up whatever a previous run left, and put the source VM back.
	defer func() {
		bg := context.Background()
		_ = tryCall(t, session, bg, "delete_vm",
			map[string]any{"name": cloneVM, "delete_disks": true, "force": true})
		_ = tryCall(t, session, bg, "start_vm", map[string]any{"name": rockyVM})
	}()
	_ = tryCall(t, session, ctx, "delete_vm",
		map[string]any{"name": cloneVM, "delete_disks": true, "force": true})

	t.Log("stopping the source VM so its disk can act as a parent")
	call(t, session, ctx, "stop_vm",
		map[string]any{"name": rockyVM, "timeout_seconds": 180}, nil)

	start := time.Now()
	var made map[string]any
	call(t, session, ctx, "create_vm_from_template", map[string]any{
		"name":            cloneVM,
		"parent_vhd_path": rockyVHD,
		"vm_path":         rockyVMPath,
		"vhd_path":        clonePath,
		"generation":      2,
		"secure_boot":     "linux",
		"memory_mb":       2048,
		"cpu_count":       2,
		"switch_name":     rockySwitch,
		"create_parents":  true,
	}, &made)
	t.Logf("cloned in %.1fs", time.Since(start).Seconds())

	warnings, _ := made["warnings"].([]any)
	if len(warnings) == 0 {
		t.Error("expected warnings about the shared parent and the inherited identity")
	}
	for _, w := range warnings {
		t.Logf("warning: %v", w)
	}
	// The clone carries the image's hostname, machine-id and SSH host keys, and
	// saying so is the difference between a useful tool and a confusing outage.
	joined := strings.ToLower(strings.Join(toStrings(warnings), " "))
	if !strings.Contains(joined, "identity") {
		t.Errorf("the warnings do not mention the inherited identity: %v", warnings)
	}

	// The child must be a differencing disk pointing at the image.
	var child map[string]any
	call(t, session, ctx, "get_vhd_info", map[string]any{"path": clonePath}, &child)
	t.Logf("child disk: type=%v size=%v file=%v parent=%v",
		child["vhd_type"], child["size_bytes"], child["file_size_bytes"], child["parent_path"])
	if child["vhd_type"] != "Differencing" {
		t.Fatalf("expected a differencing disk, got %v", child["vhd_type"])
	}
	if p, _ := child["parent_path"].(string); !strings.EqualFold(p, rockyVHD) {
		t.Fatalf("the parent is %q, want %q", p, rockyVHD)
	}
	// The whole point of a differencing disk is that it starts near-empty.
	if fs, ok := child["file_size_bytes"].(float64); ok && fs > 512*1024*1024 {
		t.Errorf("the child disk is %.0f MB, which is not a thin clone", fs/(1024*1024))
	}

	call(t, session, ctx, "start_vm", map[string]any{"name": cloneVM}, nil)
	waitForState(t, session, ctx, cloneVM, "Running", 2*time.Minute)
	t.Log("the clone boots")

	call(t, session, ctx, "stop_vm",
		map[string]any{"name": cloneVM, "force": true}, nil)

	// Deleting the clone's disks must leave the golden image alone.
	var deleted map[string]any
	call(t, session, ctx, "delete_vm", map[string]any{
		"name": cloneVM, "delete_disks": true, "force": true,
	}, &deleted)
	t.Logf("deleted: %v", deleted["disks_deleted"])
	t.Logf("kept:    %v (%v)", deleted["disks_kept"], deleted["kept_reasons"])

	var parent map[string]any
	call(t, session, ctx, "get_vhd_info", map[string]any{"path": rockyVHD}, &parent)
	t.Logf("the golden image survived: %v (%v bytes)", parent["path"], parent["size_bytes"])

	// And the clone's own disk should be gone.
	if err := tryCall(t, session, ctx, "get_vhd_info", map[string]any{"path": clonePath}); err == nil {
		t.Errorf("the clone's disk still exists at %s", clonePath)
	}
}

func toStrings(in []any) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
