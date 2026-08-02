//go:build windows

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// smbConf is a minimal Samba server exporting one authenticated share.
//
// Guest access is deliberately off: Windows 10 and later refuse insecure guest
// authentication by default, so an anonymous share would fail on the client side
// and tell us nothing about the server.
const smbConf = `[global]
    workgroup = WORKGROUP
    server string = hypervm-mcp e2e
    security = user
    map to guest = never
    server min protocol = SMB2

[e2eshare]
    path = /srv/e2eshare
    valid users = ` + rockyUser + `
    read only = no
    browseable = yes
`

const smbShareFile = "hello-from-the-guest.txt"

// TestSMBFromHost proves the claim that guest SMB is reachable from the host
// without an External switch, and pairs it with the tunnel attempt that cannot
// work — the two together are the whole argument.
//
// A tunnel needs to bind a host port, and Windows already holds 445. An outbound
// connection to the guest's own address binds nothing, so the host's listener is
// irrelevant. An External switch is only needed to let *other* machines reach the
// guest, which is a different requirement.
func TestSMBFromHost(t *testing.T) {
	requireRocky(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	host := os.Getenv(guestHostEnv)
	if host == "" {
		var ip map[string]any
		call(t, session, ctx, "wait_for_guest_ip",
			map[string]any{"name": rockyVM, "timeout_seconds": 300}, &ip)
		host, _ = ip["address"].(string)
	}
	t.Logf("guest address: %s", host)

	// A tunnel to 445 must fail, and must say why rather than just reporting a
	// bind error.
	err := tryCall(t, session, ctx, "open_tunnel", map[string]any{
		"vm_name": rockyVM, "guest_port": 445, "host_port": 445,
		"mode": "direct", "guest_host": host, "auto_restore": false,
	})
	if err == nil {
		t.Fatal("a tunnel bound host port 445, which Windows should already hold")
	}
	if !strings.Contains(err.Error(), "PORT_IN_USE") {
		t.Fatalf("expected PORT_IN_USE, got: %v", err)
	}
	if !strings.Contains(err.Error(), "External switch") {
		t.Errorf("the error should point at the alternative that works: %v", err)
	}
	t.Logf("tunnel refused as expected: %v", err)

	t.Log("installing and configuring Samba in the guest")
	// policycoreutils-python-utils provides semanage, which a minimal install
	// lacks; without it the labelling below would silently do nothing.
	sshRun(t, session, ctx, host,
		"sudo dnf install -y samba samba-client policycoreutils-python-utils")

	sshRun(t, session, ctx, host, "sudo mkdir -p /srv/e2eshare")
	sshRun(t, session, ctx, host,
		"echo 'served over SMB from the guest' | sudo tee /srv/e2eshare/"+smbShareFile)
	sshRun(t, session, ctx, host, "sudo chown -R "+rockyUser+": /srv/e2eshare")

	// SELinux is enforcing, so Samba may only serve paths labelled for it.
	// Getting this wrong produces a share that mounts and authenticates but
	// lists nothing, which looks like an empty directory rather than a denial.
	sshRun(t, session, ctx, host, "sudo setsebool -P samba_export_all_rw on")
	sshRun(t, session, ctx, host,
		"sudo semanage fcontext -a -t samba_share_t '/srv/e2eshare(/.*)?'")
	sshRun(t, session, ctx, host, "sudo restorecon -R /srv/e2eshare")

	// Check the setup rather than assuming it took, so a labelling failure is
	// reported here instead of surfacing as a puzzling empty share.
	label := sshRun(t, session, ctx, host, "ls -dZ /srv/e2eshare")
	t.Logf("share label: %s", label)
	if !strings.Contains(label, "samba_share_t") {
		t.Fatalf("the share is not labelled for Samba, so it would serve nothing: %s", label)
	}

	sshRun(t, session, ctx, host,
		"sudo tee /etc/samba/smb.conf >/dev/null <<'HYPERVMEOF'\n"+smbConf+"HYPERVMEOF")
	// Samba keeps its own password database, separate from the system one.
	sshRun(t, session, ctx, host,
		"printf '%s\\n%s\\n' '"+rockyPassword+"' '"+rockyPassword+"' | sudo smbpasswd -s -a "+rockyUser)
	sshRun(t, session, ctx, host, "sudo testparm -s >/dev/null")
	sshRun(t, session, ctx, host, "sudo systemctl enable --now smb")
	sshRun(t, session, ctx, host, "sudo systemctl restart smb")

	// The kickstart left firewalld allowing only SSH.
	sshRun(t, session, ctx, host, "sudo firewall-cmd --add-service=samba --permanent")
	sshRun(t, session, ctx, host, "sudo firewall-cmd --reload")

	listening := sshRun(t, session, ctx, host, "sudo ss -ltnH 'sport = :445' || true")
	t.Logf("guest listeners on 445:\n%s", listening)
	if !strings.Contains(listening, ":445") {
		t.Fatalf("smbd is not listening on 445 in the guest")
	}

	// Now the point of the whole test: connect from the host, as the ordinary
	// unprivileged user this test runs as.
	share := `\\` + host + `\e2eshare`
	if out, err := netUse(share, rockyUser, rockyPassword); err != nil {
		t.Fatalf("net use %s: %v\n%s", share, err, out)
	}
	defer netUseDelete(share)
	t.Logf("mounted %s", share)

	entries, err := os.ReadDir(share)
	if err != nil {
		t.Fatalf("list %s: %v", share, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	t.Logf("share contents: %v", names)
	if len(names) == 0 {
		// The session was established, so this is the guest refusing to serve the
		// contents rather than a networking problem. Say which.
		t.Logf("guest view:\n%s", sshRun(t, session, ctx, host, "sudo ls -laZ /srv/e2eshare"))
		t.Logf("selinux denials:\n%s", sshRun(t, session, ctx, host,
			"sudo ausearch -m avc -ts recent 2>/dev/null | tail -20 || echo '(none recorded)'"))
		t.Fatal("the share mounted and authenticated but served nothing, which points at the guest's SELinux labelling or file permissions rather than the network")
	}

	body, err := os.ReadFile(filepath.Join(share, smbShareFile))
	if err != nil {
		t.Fatalf("read %s: %v", smbShareFile, err)
	}
	if !strings.Contains(string(body), "served over SMB from the guest") {
		t.Fatalf("unexpected file contents: %q", body)
	}
	t.Logf("read %q over SMB: %s", smbShareFile, strings.TrimSpace(string(body)))

	// Writing proves it is a real share rather than a read-only accident.
	probe := filepath.Join(share, "written-by-the-host.txt")
	if err := os.WriteFile(probe, []byte("round trip"), 0o644); err != nil {
		t.Fatalf("write to the share: %v", err)
	}
	back := sshRun(t, session, ctx, host, "cat /srv/e2eshare/written-by-the-host.txt")
	if back != "round trip" {
		t.Fatalf("the guest sees %q, want %q", back, "round trip")
	}
	t.Log("wrote a file from the host and the guest sees it")
}

// netUse establishes credentials for a UNC path. Windows has no per-process way
// to do this without a helper, and the mapping is torn down again afterwards.
func netUse(share, user, password string) (string, error) {
	out, err := exec.Command("net.exe", "use", share, password, "/user:"+user, "/persistent:no").CombinedOutput()
	return string(out), err
}

func netUseDelete(share string) {
	_ = exec.Command("net.exe", "use", share, "/delete", "/y").Run()
}
