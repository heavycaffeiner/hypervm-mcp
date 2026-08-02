//go:build windows

// Package e2e drives the installed service exactly the way an MCP client does:
// it spawns `hypervm-mcp bridge` unprivileged and speaks MCP over its stdio.
//
// This is the test that proves the point of the whole design — an unprivileged
// process reaching Hyper-V with no elevation prompt. It needs the service to be
// installed, so it is opt-in:
//
//	$env:HYPERVM_E2E="C:\ProgramData\hypervm-mcp\bin\hypervm-mcp.exe"
//	go test ./internal/e2e -v
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connect(t *testing.T) (*mcp.ClientSession, context.Context) {
	t.Helper()

	exePath := os.Getenv("HYPERVM_E2E")
	if exePath == "" {
		t.Skip("set HYPERVM_E2E to the installed hypervm-mcp.exe to run these")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{
		Command: exec.Command(exePath, "bridge"),
	}, nil)
	if err != nil {
		t.Fatalf("connect through the bridge: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	return session, ctx
}

func TestToolsAreAdvertised(t *testing.T) {
	session, ctx := connect(t)

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	want := map[string]bool{
		"list_vms": false, "get_vm": false, "start_vm": false, "stop_vm": false,
		"restart_vm": false, "suspend_vm": false, "resume_vm": false,
		"wait_for_guest_ip": false,
	}
	for _, tool := range res.Tools {
		t.Logf("  %-20s %s", tool.Name, tool.Title)
		want[tool.Name] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %q was not advertised", name)
		}
	}
}

// The service runs as LocalSystem, so this call must succeed even though the
// test process itself cannot query Hyper-V.
func TestListVMsThroughBridge(t *testing.T) {
	session, ctx := connect(t)

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_vms",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool reported an error: %s", contentText(res))
	}

	// StructuredContent arrives as a decoded value, so round-trip it to reach
	// the concrete shape.
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("re-encode structured content: %v", err)
	}
	var vms []map[string]any
	if err := json.Unmarshal(raw, &vms); err != nil {
		t.Fatalf("decode structured content %s: %v", raw, err)
	}
	t.Logf("%d VM(s)", len(vms))
	for _, vm := range vms {
		t.Logf("  %-30v %-8v gen%v", vm["name"], vm["state"], vm["generation"])
	}
}

// A missing VM must come back as a coded tool error, not a protocol failure.
func TestGetVMNotFound(t *testing.T) {
	session, ctx := connect(t)

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_vm",
		Arguments: map[string]any{"name": "zzz-no-such-vm"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a tool error")
	}
	t.Logf("error content: %s", contentText(res))
}

// TestLifecycle drives a real VM through start, suspend, resume and stop.
//
// It needs a VM you are willing to have power-cycled, named explicitly so it can
// never pick one by accident:
//
//	$env:HYPERVM_E2E_VM="Test-VM"
func TestLifecycle(t *testing.T) {
	name := os.Getenv("HYPERVM_E2E_VM")
	if name == "" {
		t.Skip("set HYPERVM_E2E_VM to a disposable VM name to run this")
	}
	session, ctx := connect(t)

	call := func(tool string, args map[string]any) map[string]any {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if res.IsError {
			t.Fatalf("%s: %s", tool, contentText(res))
		}
		raw, _ := json.Marshal(res.StructuredContent)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		t.Logf("%-18s -> state=%v", tool, out["state"])
		return out
	}

	if got := call("start_vm", map[string]any{"name": name}); got["state"] != "Running" {
		t.Fatalf("after start_vm the VM is %v, want Running", got["state"])
	}
	if got := call("suspend_vm", map[string]any{"name": name, "mode": "pause"}); got["state"] != "Paused" {
		t.Fatalf("after suspend_vm the VM is %v, want Paused", got["state"])
	}
	if got := call("resume_vm", map[string]any{"name": name}); got["state"] != "Running" {
		t.Fatalf("after resume_vm the VM is %v, want Running", got["state"])
	}
	if got := call("stop_vm", map[string]any{"name": name, "force": true}); got["state"] != "Off" {
		t.Fatalf("after stop_vm the VM is %v, want Off", got["state"])
	}
}

// TestForgetHostKey clears the pinned SSH host key for the Rocky VM.
//
// This checks nothing. It is a maintenance step for when the VM has been rebuilt
// outside TestRockyProvision — which clears the pin itself — leaving every SSH
// test failing on a mismatch that is entirely expected. It is opt-in because
// discarding a pinned key is exactly what an attacker would want, so it should
// never happen as a side effect of running the suite.
func TestForgetHostKey(t *testing.T) {
	if os.Getenv("HYPERVM_E2E_FORGET") == "" {
		t.Skip("set HYPERVM_E2E_FORGET=1 to discard the pinned SSH host key")
	}
	session, ctx := connect(t)

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ssh_forget_host_key",
		Arguments: map[string]any{"name": "rocky10-test"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("%s", contentText(res))
	}
	t.Log("the pinned host key was discarded; the next connection will pin whatever it finds")
}

func contentText(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return "(no text content)"
}
