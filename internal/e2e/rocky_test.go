//go:build windows

package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/crypto/ssh"
)

// This file drives the goal end to end: install Rocky Linux 10 unattended, put
// nginx behind the guest's own loopback, and reach its default page through an
// SSH tunnel.
//
// nginx is bound to 127.0.0.1 inside the guest deliberately. That address is
// unreachable from the host by any route, so a "direct" tunnel cannot work and
// the SSH data path is the only thing that can serve the page. If the page comes
// back, SSH forwarding is what fetched it.
//
// Opt in, and expect it to take a while:
//
//	$env:HYPERVM_E2E="C:\ProgramData\hypervm-mcp\bin\hypervm-mcp.exe"
//	$env:HYPERVM_E2E_ROCKY="1"
//	$env:HYPERVM_E2E_INSTALL="1"   # only for TestRockyProvision, which rebuilds the VM
//	go test ./internal/e2e -run Rocky -v -count=1 -timeout 90m

const (
	rockyVM       = "rocky10-test"
	rockyISO      = `D:\HyperV\ISO\Rocky-10-minimal.iso`
	rockyVMPath   = `D:\HyperV\VMs`
	rockyVHD      = `D:\HyperV\VHD\rocky10-test.vhdx`
	rockySeedVHD  = `D:\HyperV\VHD\rocky10-test-seed.vhdx`
	rockyArtifact = `D:\HyperV\test`
	rockyUser     = "dev"
	// A fixed password for a disposable test VM that exists only to be rebuilt.
	// It is deliberately in the open so a run is reproducible; do not copy this
	// kickstart into anything you keep.
	rockyPassword = "rocky-e2e-pw"
	rockySwitch   = "Default Switch"
)

func requireRocky(t *testing.T) {
	t.Helper()
	if os.Getenv("HYPERVM_E2E_ROCKY") == "" {
		t.Skip("set HYPERVM_E2E_ROCKY=1 to run the Rocky Linux provisioning tests")
	}
}

// call runs a tool and fails the test on either a protocol error or a tool error.
func call(t *testing.T, s *mcp.ClientSession, ctx context.Context, tool string, args map[string]any, out any) {
	t.Helper()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s: %s", tool, contentText(res))
	}
	if out != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("%s: re-encode result: %v", tool, err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s: decode %s: %v", tool, raw, err)
		}
	}
}

// callList runs a listing tool and decodes the items out of its result.
//
// Listing tools answer with {"items": [...], "count": N} rather than a bare
// array, because an MCP outputSchema has to describe an object and some clients
// reject a server's entire tool list otherwise. That wrapper is an artefact of
// the protocol, so it is unwrapped here instead of in every caller.
func callList(t *testing.T, s *mcp.ClientSession, ctx context.Context, tool string, args map[string]any, out any) {
	t.Helper()
	var wrapper struct {
		Items json.RawMessage `json:"items"`
		Count int             `json:"count"`
	}
	call(t, s, ctx, tool, args, &wrapper)
	if len(wrapper.Items) == 0 {
		t.Fatalf("%s returned no items field; the listing shape has changed", tool)
	}
	if err := json.Unmarshal(wrapper.Items, out); err != nil {
		t.Fatalf("%s: decode items: %v", tool, err)
	}
}

// tryCall is call for steps that are allowed to fail, such as cleaning up
// something that may not exist.
func tryCall(t *testing.T, s *mcp.ClientSession, ctx context.Context, tool string, args map[string]any) error {
	t.Helper()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("%s", contentText(res))
	}
	return nil
}

// sshKeyPair returns a stable ed25519 key pair, generating it on first use.
//
// The key is written next to the other test artifacts so the install test and
// the tunnel test can run separately without reinstalling the guest.
func sshKeyPair(t *testing.T) (privatePEM string, authorizedKey string) {
	t.Helper()
	if err := os.MkdirAll(rockyArtifact, 0o700); err != nil {
		t.Fatalf("create %s: %v", rockyArtifact, err)
	}
	keyPath := filepath.Join(rockyArtifact, "id_ed25519")
	pubPath := keyPath + ".pub"

	if priv, err := os.ReadFile(keyPath); err == nil {
		pub, err := os.ReadFile(pubPath)
		if err == nil {
			return string(priv), strings.TrimSpace(string(pub))
		}
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "hypervm-mcp e2e")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("wrap public key: %v", err)
	}

	privatePEM = string(pem.EncodeToMemory(block))
	authorizedKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))

	if err := os.WriteFile(keyPath, []byte(privatePEM), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if err := os.WriteFile(pubPath, []byte(authorizedKey+"\n"), 0o644); err != nil {
		t.Fatalf("write public key: %v", err)
	}
	return privatePEM, authorizedKey
}

// kickstart builds the answer file Anaconda picks up from the OEMDRV volume.
//
// It ends with poweroff rather than reboot so that "the VM reached the Off
// state" is an unambiguous signal that the install finished — otherwise a reboot
// looks exactly like the installer restarting.
func kickstart(authorizedKey string) string {
	return `# Unattended Rocky Linux 10 install for hypervm-mcp end-to-end testing.
text
eula --agreed
lang en_US.UTF-8
keyboard us
timezone UTC --utc

# sda is the boot disk on SCSI 0/0; sdb is the OEMDRV seed holding this file.
# Restricting the installer to sda keeps it from wiping its own answer file.
ignoredisk --only-use=sda
clearpart --all --initlabel --drives=sda
autopart --type=lvm --noswap

network --bootproto=dhcp --device=link --activate --hostname=rocky10-test

rootpw --lock
user --name=` + rockyUser + ` --groups=wheel --plaintext --password=` + rockyPassword + `
sshkey --username=` + rockyUser + ` "` + authorizedKey + `"

firewall --enabled --service=ssh
selinux --enforcing
services --enabled=sshd

%packages --exclude-weakdeps
@^minimal-environment
openssh-server
%end

%post --erroronfail
# The automation account drives configuration over SSH, so it must not be
# prompted for a password.
echo '` + rockyUser + ` ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/` + rockyUser + `
chmod 440 /etc/sudoers.d/` + rockyUser + `

# Hyper-V learns a guest's address from an agent inside it rather than
# discovering it, so without this the host never learns the VM's IP.
#
# It is fetched on first boot rather than listed in %packages because the
# minimal installation ISO does not carry it — naming a package the media does
# not have stops Anaconda at an error prompt, which in an unattended install
# looks exactly like a hang.
cat > /etc/systemd/system/hypervm-mcp-firstboot.service <<'UNIT'
[Unit]
Description=Install Hyper-V guest daemons on first boot
After=network-online.target
Wants=network-online.target
ConditionPathExists=!/var/lib/hypervm-mcp-firstboot.done

[Service]
Type=oneshot
ExecStart=/usr/bin/dnf install -y hyperv-daemons
# hypervkvpd is BindsTo= a systemd device unit for /dev/vmbus/hv_kvp. That unit
# only exists if udev saw the device with the package's rules already in place —
# which it did not, because the package arrived after boot. Starting the service
# now would fail on a dependency that cannot be satisfied, so re-run udev to
# create the device unit first.
ExecStartPost=/usr/sbin/udevadm control --reload-rules
ExecStartPost=/usr/sbin/udevadm trigger --subsystem-match=misc --action=add
ExecStartPost=/usr/bin/systemctl start hypervkvpd
ExecStartPost=/usr/bin/touch /var/lib/hypervm-mcp-firstboot.done
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
UNIT
systemctl enable hypervm-mcp-firstboot.service
%end

poweroff
`
}

// TestRockyProvision installs Rocky Linux 10 with no console interaction and
// leaves the VM running with a reachable IP.
//
// It is gated separately from the rest of the Rocky group because it destroys
// and rebuilds the VM. Every other test in this package configures the guest and
// then checks something about it; running this one in the middle of them would
// silently pull that configuration out from under whatever came next.
func TestRockyProvision(t *testing.T) {
	requireRocky(t)
	if os.Getenv("HYPERVM_E2E_INSTALL") == "" {
		t.Skip("set HYPERVM_E2E_INSTALL=1 to run; this deletes and reinstalls the VM")
	}
	if _, err := os.Stat(rockyISO); err != nil {
		t.Fatalf("Rocky ISO not found at %s: %v", rockyISO, err)
	}
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Minute)
	defer cancel()

	_, authorizedKey := sshKeyPair(t)

	// Start from nothing so a rerun is not affected by a previous attempt.
	if err := tryCall(t, session, ctx, "delete_vm",
		map[string]any{"name": rockyVM, "delete_disks": true, "force": true}); err == nil {
		t.Log("removed a previous VM")
	}
	// A rebuilt VM generates new SSH host keys, so the pin from the old one is
	// now wrong. Clearing it here is right because this is the only place that
	// knows the VM was replaced — elsewhere, a changed key should still be
	// refused rather than waved through.
	_ = tryCall(t, session, ctx, "ssh_forget_host_key", map[string]any{"name": rockyVM})

	var switches []map[string]any
	callList(t, session, ctx, "list_switches", map[string]any{}, &switches)
	found := false
	for _, sw := range switches {
		t.Logf("switch %-24v %v", sw["name"], sw["switch_type"])
		if sw["name"] == rockySwitch {
			found = true
		}
	}
	if !found {
		t.Fatalf("no switch named %q; the guest needs outbound network", rockySwitch)
	}

	var seed map[string]any
	call(t, session, ctx, "create_seed_disk", map[string]any{
		"path":  rockySeedVHD,
		"label": "OEMDRV",
		"files": []map[string]any{
			{"path": "ks.cfg", "content": kickstart(authorizedKey)},
		},
		"overwrite":      true,
		"create_parents": true,
	}, &seed)
	t.Logf("seed disk: %v (%v)", seed["path"], seed["label"])

	var vm map[string]any
	call(t, session, ctx, "create_vm", map[string]any{
		"name":           rockyVM,
		"generation":     2,
		"memory_mb":      4096,
		"cpu_count":      2,
		"vm_path":        rockyVMPath,
		"vhd_path":       rockyVHD,
		"vhd_size_mb":    32768,
		"switch_name":    rockySwitch,
		"iso_path":       rockyISO,
		"secure_boot":    "linux", // Rocky is signed by the third-party UEFI CA
		"create_parents": true,
	}, &vm)
	t.Logf("created %v at %v", vm["name"], vm["configuration_location"])

	call(t, session, ctx, "attach_vhd", map[string]any{
		"vm_name": rockyVM, "path": rockySeedVHD,
	}, nil)

	call(t, session, ctx, "start_vm", map[string]any{"name": rockyVM}, nil)
	t.Log("installing; the VM powers itself off when the kickstart finishes")

	waitForState(t, session, ctx, rockyVM, "Off", 60*time.Minute)

	// Boot from disk from now on, and take the answer file away so a later
	// accidental boot from the ISO cannot reinstall over the system.
	call(t, session, ctx, "eject_dvd", map[string]any{"name": rockyVM}, nil)
	call(t, session, ctx, "detach_vhd", map[string]any{"vm_name": rockyVM, "path": rockySeedVHD}, nil)

	call(t, session, ctx, "start_vm", map[string]any{"name": rockyVM}, nil)

	var ip map[string]any
	call(t, session, ctx, "wait_for_guest_ip", map[string]any{
		"name": rockyVM, "timeout_seconds": 300,
	}, &ip)
	t.Logf("guest is up at %v (waited %.0fs)", ip["address"], ip["waited_seconds"])
}

// waitForState polls until the VM reaches want, reporting progress so a long
// install does not look like a hang.
func waitForState(t *testing.T, s *mcp.ClientSession, ctx context.Context, name, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		var vm map[string]any
		call(t, s, ctx, "get_vm", map[string]any{"name": name}, &vm)
		state, _ := vm["state"].(string)
		if state != last {
			t.Logf("[%s] state: %s", time.Now().Format("15:04:05"), state)
			last = state
		}
		if state == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s stayed in state %s for %s, waiting for %s", name, state, timeout, want)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("cancelled while waiting for %s", want)
		case <-time.After(20 * time.Second):
		}
	}
}
