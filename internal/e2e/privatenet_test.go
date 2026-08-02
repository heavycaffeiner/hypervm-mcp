//go:build windows

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A private lab network: the host at .1, and two guests either side of it.
const (
	labSwitch = "hypervm-mcp-lab"
	labPrefix = "10.77.0.0/24"
	labHostIP = "10.77.0.1"
	labBits   = 24
)

// labGuests are two clones of the Rocky VM, each with an address on the lab
// network.
//
// They are both clones, rather than the original plus one clone, because a
// differencing disk cannot read a parent that a running VM holds open: the
// template has to stay powered off while anything built on it runs. That is the
// shape this pattern is meant to take anyway — a template that never runs, and
// clones from it — so the test follows it.
var labGuests = []struct {
	vm   string
	lab  string
	disk string
}{
	{"rocky10-lab1", "10.77.0.11", `D:\HyperV\VHD\rocky10-lab1.vhdx`},
	{"rocky10-lab2", "10.77.0.12", `D:\HyperV\VHD\rocky10-lab2.vhdx`},
}

// TestInternalSwitchThreeWay builds a private network on an Internal switch and
// checks that all three parties reach each other: the host to each guest, and
// the guests to one another.
//
// This is the case the Default Switch hides. That one arrives with an address,
// NAT and DHCP already arranged; a switch you create yourself has none of them,
// so the test also exercises the pieces that supply them —
// set_switch_host_address for the host side and set_guest_static_ip for each
// guest, since nothing on this network hands addresses out.
func TestInternalSwitchThreeWay(t *testing.T) {
	requireRocky(t)
	if os.Getenv("HYPERVM_E2E_LAB") == "" {
		t.Skip("set HYPERVM_E2E_LAB=1 to run; this clones the VM twice and builds a private network")
	}
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	privateKey, _ := sshKeyPair(t)

	defer func() {
		bg := context.Background()
		for _, g := range labGuests {
			_ = tryCall(t, session, bg, "delete_vm",
				map[string]any{"name": g.vm, "delete_disks": true, "force": true})
			_ = tryCall(t, session, bg, "ssh_forget_host_key", map[string]any{"name": g.vm})
		}
		_ = tryCall(t, session, bg, "set_switch_nat",
			map[string]any{"switch_name": labSwitch, "enable": false})
		_ = tryCall(t, session, bg, "delete_switch", map[string]any{"name": labSwitch})
		// Put the template back the way the rest of the suite expects it.
		_ = tryCall(t, session, bg, "start_vm", map[string]any{"name": rockyVM})
	}()

	// Start from nothing. Another test may have built this switch and left it,
	// and creating one that exists is an error — so an interrupted run used to
	// poison every run after it.
	if err := tryCall(t, session, ctx, "delete_switch",
		map[string]any{"name": labSwitch}); err == nil {
		t.Logf("removed a leftover %s", labSwitch)
	}

	// An Internal switch touches no physical adapter, so no confirmation and no
	// disruption to the host's own networking.
	var sw map[string]any
	call(t, session, ctx, "create_switch", map[string]any{
		"name": labSwitch, "switch_type": "Internal",
		"notes": "hypervm-mcp end-to-end lab network",
	}, &sw)
	t.Logf("created %v (%v)", sw["name"], sw["switch_type"])

	var hostNet map[string]any
	call(t, session, ctx, "set_switch_host_address", map[string]any{
		"switch_name": labSwitch, "address": labHostIP, "prefix_length": labBits,
	}, &hostNet)
	t.Logf("host adapter %v at %v", hostNet["adapter_name"], hostNet["addresses"])
	if !hasAddress(hostNet, labHostIP) {
		t.Fatalf("the host did not take %s: %v", labHostIP, hostNet["addresses"])
	}

	// The template must be off for its disk to act as a parent, and must stay
	// off for the clones to run.
	t.Log("stopping the template")
	call(t, session, ctx, "stop_vm", map[string]any{"name": rockyVM, "timeout_seconds": 180}, nil)

	for _, g := range labGuests {
		_ = tryCall(t, session, ctx, "delete_vm",
			map[string]any{"name": g.vm, "delete_disks": true, "force": true})

		call(t, session, ctx, "create_vm_from_template", map[string]any{
			"name":            g.vm,
			"parent_vhd_path": rockyVHD,
			"vm_path":         rockyVMPath,
			"vhd_path":        g.disk,
			"generation":      2,
			"secure_boot":     "linux",
			"memory_mb":       2048,
			"cpu_count":       2,
			"switch_name":     rockySwitch, // for reaching it while we configure
			"create_parents":  true,
		}, nil)

		// A second adapter on the lab switch, alongside the one that keeps the
		// clone reachable during setup.
		call(t, session, ctx, "set_vm_network", map[string]any{
			"vm_name": g.vm, "adapter_name": labSwitch,
			"switch_name": labSwitch, "create_adapter": true,
		}, nil)

		// Clones carry the image's account and key, but the store is keyed by VM
		// name, so each needs its own entry.
		storeCredentialsFor(t, g.vm, privateKey)
		call(t, session, ctx, "start_vm", map[string]any{"name": g.vm}, nil)
		t.Logf("%s cloned and started", g.vm)
	}

	// Each clone carries the template's host key, so a pin is established per VM
	// name as we first reach it.
	addrs := make(map[string]string, len(labGuests))
	for _, g := range labGuests {
		addr := waitForCloneAddress(t, session, ctx, g.vm)
		addrs[g.vm] = addr
		t.Logf("%s reachable at %s", g.vm, addr)
	}

	for _, g := range labGuests {
		host := addrs[g.vm]
		iface := labInterface(t, session, ctx, g.vm, host)
		t.Logf("%s: lab interface is %s", g.vm, iface)

		var res map[string]any
		call(t, session, ctx, "set_guest_static_ip", map[string]any{
			"vm_name":         g.vm,
			"host":            host,
			"address":         g.lab,
			"prefix_length":   labBits,
			"interface_name":  iface,
			"auto_checkpoint": false, // these clones are disposable
			"timeout_seconds": 240,
		}, &res)
		t.Logf("%s: %s via %v (verified %v)", g.vm, g.lab, res["method"], res["verified_address"])
	}

	// Host to each guest, over the lab network.
	for _, g := range labGuests {
		if !reachableFromHost(g.lab, 22, 60*time.Second) {
			t.Fatalf("the host cannot reach %s at %s on the lab switch", g.vm, g.lab)
		}
		t.Logf("host -> %s (%s) ok", g.vm, g.lab)
	}

	// Guest to guest, both directions, forced out of the lab address so the
	// Default Switch cannot be the one carrying it.
	for i, g := range labGuests {
		peer := labGuests[(i+1)%len(labGuests)]
		out := sshRunVM(t, session, ctx, g.vm, addrs[g.vm],
			fmt.Sprintf("ping -c 3 -W 2 -I %s %s 2>&1 | tail -3", g.lab, peer.lab))
		t.Logf("%s -> %s:\n%s", g.vm, peer.lab, out)
		if !strings.Contains(out, "0% packet loss") {
			t.Fatalf("%s could not reach %s over the lab switch", g.vm, peer.lab)
		}
	}

	// And each guest to the host.
	for _, g := range labGuests {
		out := sshRunVM(t, session, ctx, g.vm, addrs[g.vm],
			fmt.Sprintf("ping -c 3 -W 2 -I %s %s 2>&1 | tail -3", g.lab, labHostIP))
		if !strings.Contains(out, "0% packet loss") {
			t.Fatalf("%s could not reach the host at %s:\n%s", g.vm, labHostIP, out)
		}
		t.Logf("%s -> host (%s) ok", g.vm, labHostIP)
	}

	t.Log("all three parties reach each other on the Internal switch")

	// NAT is the optional extra: a way out for guests with no other route.
	var nat map[string]any
	call(t, session, ctx, "set_switch_nat", map[string]any{
		"switch_name": labSwitch, "prefix": labPrefix, "enable": true,
	}, &nat)
	t.Logf("NAT %v on %v", nat["nat_name"], nat["nat_prefix"])
	if nat["nat_name"] == "" {
		t.Errorf("the NAT was not created: %v", nat)
	}
}

// reachableFromHost dials a guest from the host, retrying while its new address
// settles.
func reachableFromHost(addr string, port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(addr, strconv.Itoa(port)), 3*time.Second)
		if err == nil {
			conn.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}

func hasAddress(result map[string]any, want string) bool {
	addrs, _ := result["addresses"].([]any)
	for _, a := range addrs {
		if strings.HasPrefix(fmt.Sprint(a), want+"/") {
			return true
		}
	}
	return false
}

// labInterface finds the guest interface attached to the lab switch, by
// elimination: it is the one without an address, since the other holds the
// Default Switch lease.
func labInterface(t *testing.T, s *mcp.ClientSession, ctx context.Context, vm, host string) string {
	t.Helper()
	out := sshRunVM(t, s, ctx, vm, host,
		`ip -o -4 addr show | awk '{print $2}' | sort -u > /tmp/with; `+
			`ip -o link show | awk -F': ' '$2 !~ /^lo/ {print $2}' | sort -u > /tmp/all; `+
			`comm -23 /tmp/all /tmp/with | head -1`)
	if out == "" {
		t.Fatalf("%s: could not identify the lab interface", vm)
	}
	return out
}

// waitForCloneAddress finds a clone on the Default Switch, which is the only
// network handing out addresses at this point.
func waitForCloneAddress(t *testing.T, s *mcp.ClientSession, ctx context.Context, vm string) string {
	t.Helper()
	deadline := time.Now().Add(8 * time.Minute)
	for {
		// Restrict the wait to the Default Switch subnet: the lab adapter has no
		// address yet and would otherwise offer a link-local one leading nowhere.
		res, err := s.CallTool(ctx, &mcp.CallToolParams{
			Name: "wait_for_guest_ip",
			Arguments: map[string]any{
				"name": vm, "timeout_seconds": 30, "subnet": "172.16.0.0/12",
			},
		})
		if err == nil && !res.IsError {
			var ip map[string]any
			decodeResult(t, res, &ip)
			if addr, _ := ip["address"].(string); addr != "" {
				return addr
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never reported an address; it may not have booted", vm)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("cancelled while waiting for %s", vm)
		case <-time.After(15 * time.Second):
		}
	}
}

// decodeResult unpacks a tool result into out.
func decodeResult(t *testing.T, res *mcp.CallToolResult, out any) {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("re-encode result: %v", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}
