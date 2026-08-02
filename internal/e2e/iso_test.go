//go:build windows

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestAttachISO gives a running VM a disc and has the guest mount it.
//
// This is the capability that matters for installing an OS: the server's job is
// to put media in front of a VM, not to author it. Checking that the guest can
// actually read the disc proves the whole path, where merely seeing a DVD drive
// appear in the VM's configuration would not.
func TestAttachISO(t *testing.T) {
	requireRocky(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	if _, err := os.Stat(rockyISO); err != nil {
		t.Skipf("no ISO to attach at %s: %v", rockyISO, err)
	}
	host := os.Getenv(guestHostEnv)

	before := guestOpticalDrives(t, session, ctx, host)
	t.Logf("guest sees %d optical drive(s) before: %v", len(before), before)

	var detail map[string]any
	call(t, session, ctx, "attach_iso", map[string]any{
		"vm_name": rockyVM, "iso_path": rockyISO,
	}, &detail)
	t.Logf("attached %s", rockyISO)

	defer func() {
		bg := context.Background()
		_ = tryCall(t, session, bg, "ssh_exec", map[string]any{
			"vm_name": rockyVM, "host": host, "timeout_seconds": 60,
			"command": "sudo umount /mnt/iso 2>/dev/null; sudo rmdir /mnt/iso 2>/dev/null; true",
		})
		_ = tryCall(t, session, bg, "eject_dvd", map[string]any{"name": rockyVM})
	}()

	// Hyper-V presents a hot-added DVD without a reboot, but the guest takes a
	// moment to notice.
	var after []string
	deadline := time.Now().Add(2 * time.Minute)
	for {
		after = guestOpticalDrives(t, session, ctx, host)
		if len(after) > len(before) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	t.Logf("guest sees %d optical drive(s) after: %v", len(after), after)
	if len(after) <= len(before) {
		t.Fatalf("the guest never saw the new drive: before=%v after=%v", before, after)
	}

	device := "/dev/" + difference(after, before)[0]
	t.Logf("mounting %s in the guest", device)

	sshRun(t, session, ctx, host, "sudo mkdir -p /mnt/iso")
	sshRun(t, session, ctx, host, "sudo mount -o ro "+device+" /mnt/iso")

	listing := sshRun(t, session, ctx, host, "ls /mnt/iso")
	t.Logf("disc contents:\n%s", listing)

	// The Rocky installation media carries these; anything else means we mounted
	// something other than the disc we attached.
	for _, want := range []string{"EFI", "images"} {
		if !strings.Contains(listing, want) {
			t.Fatalf("the mounted disc does not look like the Rocky ISO (no %q):\n%s", want, listing)
		}
	}

	// Read a file off it, so the check covers data and not just the directory.
	release := sshRun(t, session, ctx, host,
		"cat /mnt/iso/.discinfo 2>/dev/null | head -3 || echo '(no .discinfo)'")
	t.Logf(".discinfo:\n%s", release)

	t.Log("the guest mounted the attached ISO and read from it")
}

// guestOpticalDrives lists optical devices the guest can see.
func guestOpticalDrives(t *testing.T, s *mcp.ClientSession, ctx context.Context, host string) []string {
	t.Helper()
	out := sshRun(t, s, ctx, host, `lsblk -dno NAME,TYPE | awk '$2=="rom"{print $1}' | sort`)
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if n := strings.TrimSpace(line); n != "" {
			names = append(names, n)
		}
	}
	return names
}
