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

// TestRenameCarriesPinnedHostKey is the check the diskless VM cannot make.
//
// A VM that has never been connected to has no pinned key, so a rename moves
// nothing and reports nothing — which looks identical to a rename that silently
// failed to move one. This uses a guest that has actually been connected to, and
// compares the fingerprint on both sides of the rename.
//
// The loss is deliberately hard to notice: an absent pin is not an error. The
// next connection is a first sighting, so a key that should have been refused is
// recorded as if it were expected. Comparing fingerprints is the only way to
// tell a carried pin from a re-pinned one.
//
// It renames the shared guest and renames it back, including on failure.
func TestRenameCarriesPinnedHostKey(t *testing.T) {
	requireRocky(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	const renamed = "rocky10-test-renamed"

	var before map[string]any
	call(t, session, ctx, "ssh_info", map[string]any{"name": rockyVM}, &before)
	if before["host_key_pinned"] != true {
		t.Skipf("%s has no pinned host key yet; run an SSH test first", rockyVM)
	}
	fingerprint, _ := before["fingerprint"].(string)
	t.Logf("%s is pinned at %s", rockyVM, fingerprint)

	var res map[string]any
	call(t, session, ctx, "rename_vm",
		map[string]any{"name": rockyVM, "new_name": renamed}, &res)
	// Put the name back whatever happens next; every other test addresses this
	// guest by its usual name.
	defer func() {
		_ = tryCall(t, session, context.Background(), "rename_vm",
			map[string]any{"name": renamed, "new_name": rockyVM})
	}()

	t.Logf("credentials moved: %v, host key moved: %v", res["credentials_moved"], res["host_key_moved"])
	if res["host_key_moved"] != true {
		t.Fatalf("the rename did not carry the pinned host key: %v", res)
	}
	if res["credentials_moved"] != true {
		t.Fatalf("the rename did not carry the credentials: %v", res)
	}

	var after map[string]any
	call(t, session, ctx, "ssh_info", map[string]any{"name": renamed}, &after)
	if after["host_key_pinned"] != true {
		t.Fatalf("%s has no pinned key after the rename", renamed)
	}
	if got, _ := after["fingerprint"].(string); got != fingerprint {
		t.Fatalf("the pin under the new name is %s, want the one it had: %s", got, fingerprint)
	}
	if after["username"] != before["username"] {
		t.Fatalf("the stored username changed across the rename: %v then %v",
			before["username"], after["username"])
	}
	t.Log("the same fingerprint and account are pinned under the new name")

	// And the credential still works, which is what the caller actually cares
	// about — a moved entry that no longer authenticates would pass every check
	// above.
	host, _ := after["address"].(string)
	out := sshRunVM(t, session, ctx, renamed, host, "echo renamed-and-still-authenticated")
	if !strings.Contains(out, "renamed-and-still-authenticated") {
		t.Fatalf("SSH under the new name returned %q", out)
	}
	t.Log("SSH under the new name authenticates with the carried credential and pin")
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
