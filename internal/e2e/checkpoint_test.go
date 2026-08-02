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

// TestNetworkDiagnosis reports what the VM's networking can do. It changes
// nothing, so it is safe to run against a VM in use.
func TestNetworkDiagnosis(t *testing.T) {
	requireRocky(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var adapters []map[string]any
	callList(t, session, ctx, "list_physical_adapters", map[string]any{}, &adapters)
	for _, a := range adapters {
		t.Logf("host adapter %-28v %-13v %v Mbps  wireless=%v  switch=%v",
			a["name"], a["status"], a["link_speed_mbps"], a["is_wireless"], a["bound_to_switch"])
	}

	var d map[string]any
	call(t, session, ctx, "diagnose_vm_network", map[string]any{
		"vm_name":     rockyVM,
		"guest_host":  os.Getenv(guestHostEnv),
		"probe_ports": []int{22, 80},
	}, &d)

	t.Logf("state: %v, on physical LAN: %v, addresses reported: %v, host can reach: %v",
		d["state"], d["guest_on_physical_lan"], d["addresses_reported"], d["host_can_reach"])
	t.Logf("host-held ports (never tunnellable): %v", d["blocked_host_ports"])
	t.Logf("recommendation: %v", d["recommendation"])

	nics, _ := d["adapters"].([]any)
	if len(nics) == 0 {
		t.Fatal("the VM reported no adapters")
	}
	for _, raw := range nics {
		n, _ := raw.(map[string]any)
		t.Logf("  %v on %v (%v): ips=%v reachable=%v open=%v closed=%v",
			n["name"], n["switch_name"], n["switch_type"], n["ip_addresses"],
			n["host_can_reach"], n["reachable_ports"], n["unreachable_ports"])
	}

	// 445 is the reason External switches exist in this design; if the host did
	// not hold it, the advice the tools give would be wrong.
	blocked, _ := d["blocked_host_ports"].([]any)
	found445 := false
	for _, p := range blocked {
		if n, ok := p.(float64); ok && int(n) == 445 {
			found445 = true
		}
	}
	if !found445 {
		t.Errorf("expected the host to hold port 445; got %v", blocked)
	}

	// Reporting no address must not be mistaken for being unreachable: the two
	// have different fixes, and a guest without the Hyper-V agent does the first
	// while working perfectly.
	if os.Getenv(guestHostEnv) != "" {
		if d["host_can_reach"] != true {
			t.Errorf("a known-good address was probed but reported unreachable: %v", d["recommendation"])
		}
		if d["addresses_reported"] == false && !strings.Contains(d["recommendation"].(string), "agent") {
			t.Errorf("the recommendation should explain the missing agent, got: %v", d["recommendation"])
		}
	}
}

// TestCheckpointCycle takes a checkpoint, changes the guest, reverts, and
// confirms the change is gone — then merges the checkpoint away.
//
// It power-cycles the VM, so it is opt-in beyond the usual Rocky guard.
func TestCheckpointCycle(t *testing.T) {
	requireRocky(t)
	if os.Getenv("HYPERVM_E2E_CHECKPOINT") == "" {
		t.Skip("set HYPERVM_E2E_CHECKPOINT=1 to run; this reverts and restarts the VM")
	}
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	host := os.Getenv(guestHostEnv)
	const snapshot = "e2e-checkpoint"
	const marker = "/var/tmp/hypervm-mcp-marker"

	// Leave nothing behind even if an assertion fails part way through.
	defer func() {
		_ = tryCall(t, session, context.Background(), "delete_checkpoint",
			map[string]any{"vm_name": rockyVM, "snapshot_name": snapshot, "include_children": true})
	}()

	var made map[string]any
	call(t, session, ctx, "create_checkpoint", map[string]any{
		"vm_name": rockyVM, "snapshot_name": snapshot,
	}, &made)
	t.Logf("checkpoint %v (%v) taken at %v", made["name"], made["checkpoint_type"], made["created_at"])

	var list []map[string]any
	callList(t, session, ctx, "list_checkpoints", map[string]any{"vm_name": rockyVM}, &list)
	if !containsCheckpoint(list, snapshot) {
		t.Fatalf("the checkpoint is not in the list: %v", list)
	}

	// Make a change that must not survive the revert.
	sshRun(t, session, ctx, host, "echo written-after-checkpoint | sudo tee "+marker)
	if out := sshRun(t, session, ctx, host, "cat "+marker); !strings.Contains(out, "written-after-checkpoint") {
		t.Fatalf("the marker was not written: %q", out)
	}

	call(t, session, ctx, "apply_checkpoint", map[string]any{
		"vm_name": rockyVM, "snapshot_name": snapshot, "auto_stop": true,
	}, nil)
	call(t, session, ctx, "start_vm", map[string]any{"name": rockyVM}, nil)

	waitForSSH(t, session, ctx, host, 5*time.Minute)

	// The file was created after the checkpoint, so reverting must have removed it.
	out := sshRun(t, session, ctx, host, "test -e "+marker+" && echo PRESENT || echo ABSENT")
	if !strings.Contains(out, "ABSENT") {
		t.Fatalf("the revert did not discard the change made after the checkpoint: %q", out)
	}
	t.Log("the post-checkpoint change is gone, so the revert worked")

	call(t, session, ctx, "delete_checkpoint", map[string]any{
		"vm_name": rockyVM, "snapshot_name": snapshot,
	}, nil)

	callList(t, session, ctx, "list_checkpoints", map[string]any{"vm_name": rockyVM}, &list)
	if containsCheckpoint(list, snapshot) {
		t.Fatalf("the checkpoint is still listed after deletion: %v", list)
	}
	t.Log("checkpoint deleted and its disk merged")
}

func containsCheckpoint(list []map[string]any, name string) bool {
	for _, c := range list {
		if c["name"] == name {
			return true
		}
	}
	return false
}

// sshRun runs a command on the primary VM and returns stdout.
//
// vm_name is not decoration: it selects the stored credential and the pinned
// host key. Running against another machine under this name would use the wrong
// credential and file the wrong key, so anything that is not the primary VM must
// use sshRunVM.
func sshRun(t *testing.T, s *mcp.ClientSession, ctx context.Context, host, command string) string {
	t.Helper()
	return sshRunVM(t, s, ctx, rockyVM, host, command)
}

// sshRunVM runs a command on a named VM and returns stdout, failing on any error.
func sshRunVM(t *testing.T, s *mcp.ClientSession, ctx context.Context, vm, host, command string) string {
	t.Helper()
	var res struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
	args := map[string]any{"vm_name": vm, "command": command, "timeout_seconds": 120}
	if host != "" {
		args["host"] = host
	}
	call(t, s, ctx, "ssh_exec", args, &res)
	if res.ExitCode != 0 {
		t.Fatalf("%s command failed (exit %d): %s\n%s", vm, res.ExitCode, command, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

// waitForSSH polls until the primary VM answers again.
//
// This does not use wait_for_guest_ip, because what matters after a revert is
// that the guest is serving SSH — which is later than having an address, and is
// the thing the next step actually needs.
func waitForSSH(t *testing.T, s *mcp.ClientSession, ctx context.Context, host string, timeout time.Duration) {
	t.Helper()
	waitForSSHVM(t, s, ctx, rockyVM, host, timeout)
}

// waitForSSHVM is waitForSSH for any other VM.
//
// Credentials and pinned host keys are filed per VM, so probing a clone or a
// different guest under the primary VM's name authenticates with the wrong key
// and can never succeed — it just spends the whole timeout failing.
func waitForSSHVM(t *testing.T, s *mcp.ClientSession, ctx context.Context, vm, host string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	// "exit 0" rather than "true": this also probes Windows guests, where sshd
	// hands the command to cmd.exe and there is no "true".
	args := map[string]any{"vm_name": vm, "command": "exit 0", "timeout_seconds": 20}
	if host != "" {
		args["host"] = host
	}
	for {
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "ssh_exec", Arguments: args})
		if err == nil && !res.IsError {
			t.Logf("guest is answering SSH again")
			return
		}
		if time.Now().After(deadline) {
			detail := "call failed"
			if res != nil {
				detail = contentText(res)
			}
			t.Fatalf("the guest did not answer SSH within %s: %s", timeout, detail)
		}
		select {
		case <-ctx.Done():
			t.Fatal("cancelled while waiting for SSH")
		case <-time.After(10 * time.Second):
		}
	}
}
