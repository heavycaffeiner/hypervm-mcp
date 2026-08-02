package sshx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A pin that does not follow a rename is worse than no pin at all: the next
// connection looks like a first sighting, so a host key that should have been
// refused is accepted without a word. This is the store-level guarantee that
// rename_vm depends on.
func TestHostKeysRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts.json")
	h := &HostKeys{path: path}

	seed := hostKeyFile{
		Version: currentVersion,
		Hosts: map[string]HostKey{
			"old-vm":   {Fingerprint: "SHA256:aaa", KeyType: "ssh-ed25519"},
			"other-vm": {Fingerprint: "SHA256:bbb", KeyType: "ssh-ed25519"},
		},
	}
	b, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	moved, err := h.Rename("old-vm", "new-vm")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if !moved {
		t.Fatal("the rename reported nothing to move, but a key was pinned")
	}

	if _, ok := h.Get("old-vm"); ok {
		t.Error("the key is still pinned under the old name")
	}
	got, ok := h.Get("new-vm")
	if !ok {
		t.Fatal("the key is not pinned under the new name")
	}
	if got.Fingerprint != "SHA256:aaa" {
		t.Errorf("the moved key is %s, want SHA256:aaa", got.Fingerprint)
	}

	// Moving one VM's pin must not disturb another's.
	if other, ok := h.Get("other-vm"); !ok || other.Fingerprint != "SHA256:bbb" {
		t.Errorf("another VM's pin changed: %v %v", other, ok)
	}

	// A VM with no pin is not an error; there is simply nothing to carry.
	moved, err = h.Rename("never-seen", "somewhere")
	if err != nil {
		t.Fatalf("rename with nothing to move: %v", err)
	}
	if moved {
		t.Error("the rename claimed to move a key that was never pinned")
	}
}
