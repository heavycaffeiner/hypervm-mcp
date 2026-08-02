//go:build windows

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestDisksForArray creates disks one at a time at chosen controller ports and
// has the guest build a RAID array on them.
//
// Creating and attaching each disk separately is the point: sizes are given in
// megabytes so a test can pick an exact capacity, and controller locations are
// given explicitly so the guest's device order is decided rather than inferred.
func TestDisksForArray(t *testing.T) {
	requireRocky(t)
	if os.Getenv("HYPERVM_E2E_DISKS") == "" {
		t.Skip("set HYPERVM_E2E_DISKS=1 to run; this attaches disks to the VM and builds an array")
	}
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	host := os.Getenv(guestHostEnv)
	const (
		arrayDisks = 4
		diskSizeMB = 512
		firstPort  = 8 // well clear of the boot disk at location 0
	)

	before := guestDisks(t, session, ctx, host)
	t.Logf("guest sees %d disk(s) before: %v", len(before), before)

	var paths []string
	defer func() {
		bg := context.Background()
		_ = tryCall(t, session, bg, "ssh_exec", map[string]any{
			"vm_name": rockyVM, "host": host, "timeout_seconds": 120,
			"command": "sudo mdadm --stop /dev/md0 2>/dev/null; sudo mdadm --zero-superblock /dev/sd[b-z] 2>/dev/null; true",
		})
		for _, p := range paths {
			_ = tryCall(t, session, bg, "detach_vhd", map[string]any{"vm_name": rockyVM, "path": p})
			_ = os.Remove(p)
		}
	}()

	for i := 0; i < arrayDisks; i++ {
		path := fmt.Sprintf(`D:\HyperV\VHD\raid-%02d.vhdx`, i+1)
		_ = os.Remove(path)

		var made map[string]any
		call(t, session, ctx, "create_vhd", map[string]any{
			"path":           path,
			"size_mb":        diskSizeMB,
			"disk_type":      "fixed", // fixed, so the array behaves like real storage
			"create_parents": true,
		}, &made)
		paths = append(paths, path)

		// Hyper-V rounds a VHDX up to a whole megabyte, so check the size landed
		// where it was asked to rather than at some default.
		if size, ok := made["size_bytes"].(float64); !ok || int64(size) != int64(diskSizeMB)*1024*1024 {
			t.Fatalf("disk %d is %v bytes, want %d", i+1, made["size_bytes"], int64(diskSizeMB)*1024*1024)
		}

		location := firstPort + i
		var attached map[string]any
		call(t, session, ctx, "attach_vhd", map[string]any{
			"vm_name":             rockyVM,
			"path":                path,
			"controller_type":     "SCSI",
			"controller_number":   0,
			"controller_location": location,
		}, &attached)
		t.Logf("disk %d: %s at SCSI 0:%d", i+1, path, location)

		// The disk must be where it was told to go, or the guest's device order
		// is not the one this test relies on.
		if !diskAt(attached, path, 0, location) {
			t.Fatalf("%s is not at SCSI 0:%d: %v", path, location, attached["hard_drives"])
		}
	}

	// Hyper-V surfaces hot-added SCSI disks without a reboot, but the guest needs
	// a moment to notice them.
	var after []string
	deadline := time.Now().Add(2 * time.Minute)
	for {
		after = guestDisks(t, session, ctx, host)
		if len(after) >= len(before)+arrayDisks || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	t.Logf("guest sees %d disk(s) after: %v", len(after), after)
	if len(after) < len(before)+arrayDisks {
		t.Fatalf("the guest sees %d disks, expected at least %d", len(after), len(before)+arrayDisks)
	}

	fresh := difference(after, before)
	if len(fresh) < arrayDisks {
		t.Fatalf("could not identify the new disks: before=%v after=%v", before, after)
	}
	// Consecutive controller locations should have produced consecutive device
	// names; sorting them makes the array members deterministic either way.
	members := "/dev/" + strings.Join(fresh[:arrayDisks], " /dev/")
	t.Logf("building RAID5 across %s", members)

	sshRun(t, session, ctx, host, "sudo dnf install -y mdadm")
	sshRun(t, session, ctx, host, fmt.Sprintf(
		"sudo mdadm --create /dev/md0 --run --level=5 --raid-devices=%d %s", arrayDisks, members))

	detail := sshRun(t, session, ctx, host, "sudo mdadm --detail /dev/md0 | head -20")
	t.Logf("array:\n%s", detail)
	if !strings.Contains(detail, "raid5") {
		t.Fatalf("the array is not RAID5:\n%s", detail)
	}
	if !strings.Contains(detail, fmt.Sprintf("Raid Devices : %d", arrayDisks)) {
		t.Errorf("the array does not have %d devices:\n%s", arrayDisks, detail)
	}
	t.Log("the guest assembled a RAID5 array across the disks, at the ports they were attached to")
}

// diskAt reports whether the VM detail shows path at the given controller port.
func diskAt(detail map[string]any, path string, controller, location int) bool {
	drives, _ := detail["hard_drives"].([]any)
	for _, raw := range drives {
		d, _ := raw.(map[string]any)
		if !strings.EqualFold(fmt.Sprint(d["path"]), path) {
			continue
		}
		n, _ := d["controller_number"].(float64)
		l, _ := d["controller_location"].(float64)
		return int(n) == controller && int(l) == location
	}
	return false
}

// guestDisks lists whole block devices the guest can see.
func guestDisks(t *testing.T, s *mcp.ClientSession, ctx context.Context, host string) []string {
	t.Helper()
	out := sshRun(t, s, ctx, host, `lsblk -dno NAME,TYPE | awk '$2=="disk"{print $1}' | sort`)
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if n := strings.TrimSpace(line); n != "" {
			names = append(names, n)
		}
	}
	return names
}

func difference(after, before []string) []string {
	seen := make(map[string]bool, len(before))
	for _, b := range before {
		seen[b] = true
	}
	var out []string
	for _, a := range after {
		if !seen[a] {
			out = append(out, a)
		}
	}
	return out
}
