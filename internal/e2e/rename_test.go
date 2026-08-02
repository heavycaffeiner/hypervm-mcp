//go:build windows

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
	"github.com/heavycaffeiner/hypervm-mcp/internal/ipc"
)

// Renaming a VM is easy; renaming it without stranding what this server files
// under its name is the part worth testing. Credentials, the pinned SSH host key
// and open tunnels are all keyed by name, and a rename that moves only one of
// them leaves a VM that looks half-unknown.
//
// This builds its own VM so it needs no guest and disturbs nothing.
//
//	$env:HYPERVM_E2E="C:\ProgramData\hypervm-mcp-dev\bin\hypervm-mcp-dev.exe"
//	go test ./internal/e2e -run Rename -v -count=1

const (
	renameFrom  = "hypervm-rename-before"
	renameTo    = "hypervm-rename-after"
	renameClash = "hypervm-rename-clash"
)

func TestRenameVMMovesStoredState(t *testing.T) {
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	for _, n := range []string{renameFrom, renameTo, renameClash} {
		_ = tryCall(t, session, ctx, "delete_vm",
			map[string]any{"name": n, "delete_disks": true, "force": true})
		_ = tryCall(t, session, ctx, "ssh_forget_host_key", map[string]any{"name": n})
	}
	defer func() {
		bg := context.Background()
		for _, n := range []string{renameFrom, renameTo, renameClash} {
			_ = tryCall(t, session, bg, "delete_vm",
				map[string]any{"name": n, "delete_disks": true, "force": true})
			_ = tryCall(t, session, bg, "ssh_forget_host_key", map[string]any{"name": n})
			deleteCredentials(t, n)
		}
	}()

	call(t, session, ctx, "create_vm", map[string]any{
		"name": renameFrom, "generation": 2, "memory_mb": 512, "cpu_count": 1,
		"secure_boot": "off",
	}, nil)

	// File something under the old name for each store the rename has to move.
	privateKey, _ := sshKeyPair(t)
	storeCredentialsAs(t, renameFrom, "someone", "a-password", privateKey)
	if !hasCredentials(t, renameFrom) {
		t.Fatal("the credential did not store, so this test cannot prove anything moved")
	}

	var got map[string]any
	call(t, session, ctx, "rename_vm", map[string]any{
		"name": renameFrom, "new_name": renameTo,
	}, &got)
	t.Logf("credentials moved: %v, host key moved: %v, tunnels moved: %v",
		got["credentials_moved"], got["host_key_moved"], got["tunnels_moved"])
	if w, ok := got["warnings"]; ok {
		t.Fatalf("the rename reported warnings: %v", w)
	}

	// The VM answers to the new name and not the old one.
	var detail map[string]any
	call(t, session, ctx, "get_vm", map[string]any{"name": renameTo}, &detail)
	if detail["name"] != renameTo {
		t.Fatalf("the VM reports its name as %v, want %q", detail["name"], renameTo)
	}
	if err := tryCall(t, session, ctx, "get_vm", map[string]any{"name": renameFrom}); err == nil {
		t.Fatalf("%q still resolves after the rename", renameFrom)
	}

	// The credential followed. This is the check that matters: without it the
	// VM is renamed and unusable, and nothing says why.
	if got["credentials_moved"] != true {
		t.Fatalf("the rename did not report moving the credential: %v", got)
	}
	if hasCredentials(t, renameFrom) {
		t.Fatalf("the credential is still filed under %q", renameFrom)
	}
	if !hasCredentials(t, renameTo) {
		t.Fatalf("the credential is not filed under %q", renameTo)
	}
	t.Log("credentials followed the rename")

	// Renaming onto a name already in use has to be refused: Hyper-V allows it,
	// and then every call that names either VM is ambiguous.
	//
	// A third name, because the renamed VM's disk file kept the first one — which
	// is the behaviour this tool documents, and creating another VM there would
	// collide with a disk the renamed VM still owns.
	call(t, session, ctx, "create_vm", map[string]any{
		"name": renameClash, "generation": 2, "memory_mb": 512, "cpu_count": 1,
		"secure_boot": "off",
	}, nil)
	err := tryCall(t, session, ctx, "rename_vm",
		map[string]any{"name": renameClash, "new_name": renameTo})
	if err == nil {
		t.Fatal("renaming onto an existing name was allowed, which makes both VMs ambiguous")
	}
	if !strings.Contains(err.Error(), "VM_ALREADY_EXISTS") {
		t.Fatalf("the refusal is not the coded one a caller can act on: %v", err)
	}
	t.Logf("a clashing rename is refused: %v", err)

	// Renaming to its own name is a no-op worth refusing rather than performing.
	if err := tryCall(t, session, ctx, "rename_vm",
		map[string]any{"name": renameTo, "new_name": renameTo}); err == nil {
		t.Error("renaming a VM to its current name was accepted")
	}
}

// hasCredentials asks the service whether a VM has an entry, through the same
// control channel the CLI uses.
func hasCredentials(t *testing.T, vm string) bool {
	t.Helper()
	resp, err := ipc.SendControl(context.Background(), pipePath(), map[string]any{"op": "cred.list"})
	if err != nil || !resp.OK {
		t.Fatalf("list credentials: %v %v", err, resp)
	}
	list, _ := resp.Data.([]any)
	for _, raw := range list {
		if e, ok := raw.(map[string]any); ok && e["vm_name"] == vm {
			return true
		}
	}
	return false
}

func deleteCredentials(t *testing.T, vm string) {
	t.Helper()
	_, _ = ipc.SendControl(context.Background(), pipePath(),
		map[string]any{"op": "cred.delete", "vm": vm})
}

func pipePath() string {
	if cfg, err := config.Load(); err == nil {
		return cfg.PipePath()
	}
	return `\\.\pipe\` + config.DefaultPipeName()
}
