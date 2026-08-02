//go:build windows

// Package sshx is the service's SSH client: a pooled dialer for guest VMs with
// trust-on-first-use host key pinning.
package sshx

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
)

const currentVersion = 1

// HostKey is one pinned key.
type HostKey struct {
	KeyType     string `json:"key_type"`
	Fingerprint string `json:"fingerprint_sha256"`
	PublicKey   string `json:"public_key"` // base64 of the wire format
	FirstSeen   string `json:"first_seen"`
}

type hostKeyFile struct {
	Version int                `json:"version"`
	Hosts   map[string]HostKey `json:"hosts"`
}

// HostKeys pins one key per VM name.
//
// Keying on the VM name rather than the address is deliberate. A guest's IP
// changes on almost every reboot under NAT, so an address-keyed store would see
// a new host every time and pin nothing. The VM name is stable for as long as
// the VM exists, which is exactly the identity worth pinning.
type HostKeys struct {
	mu   sync.RWMutex
	path string
}

func NewHostKeys() *HostKeys {
	return &HostKeys{path: filepath.Join(config.DataDir(), "known_hosts.json")}
}

// Verdict reports what pinning decided about a connection.
type Verdict struct {
	Fingerprint string
	FirstSeen   bool // the key was recorded by this connection
	Replaced    bool // a previously pinned key was overwritten
}

// Check compares a presented key against the pinned one.
//
// A mismatch is refused unless trustNew is set. That escape hatch exists because
// the mismatch is usually legitimate — the VM was rebuilt from an image, or
// reverted to a checkpoint taken before its keys were generated — and without it
// the only recovery would be editing a file by hand.
func (h *HostKeys) Check(vmName string, key ssh.PublicKey, trustNew bool) (*Verdict, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	f, err := h.load()
	if err != nil {
		return nil, err
	}

	presented := HostKey{
		KeyType:     key.Type(),
		Fingerprint: ssh.FingerprintSHA256(key),
		PublicKey:   base64.StdEncoding.EncodeToString(key.Marshal()),
		FirstSeen:   time.Now().UTC().Format(time.RFC3339),
	}

	pinned, ok := f.Hosts[vmName]
	switch {
	case !ok:
		f.Hosts[vmName] = presented
		if err := h.save(f); err != nil {
			return nil, err
		}
		return &Verdict{Fingerprint: presented.Fingerprint, FirstSeen: true}, nil

	case pinned.PublicKey == presented.PublicKey:
		return &Verdict{Fingerprint: presented.Fingerprint}, nil

	case trustNew:
		f.Hosts[vmName] = presented
		if err := h.save(f); err != nil {
			return nil, err
		}
		return &Verdict{Fingerprint: presented.Fingerprint, Replaced: true}, nil

	default:
		return nil, fmt.Errorf(
			"host key for %q changed: pinned %s, now %s. "+
				"If you rebuilt or reverted this VM, pass trust_new_key to accept the new key",
			vmName, pinned.Fingerprint, presented.Fingerprint)
	}
}

// Forget drops the pinned key for a VM.
func (h *HostKeys) Forget(vmName string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	f, err := h.load()
	if err != nil {
		return err
	}
	delete(f.Hosts, vmName)
	return h.save(f)
}

// Get returns the pinned key for a VM, if any.
func (h *HostKeys) Get(vmName string) (HostKey, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	f, err := h.load()
	if err != nil {
		return HostKey{}, false
	}
	k, ok := f.Hosts[vmName]
	return k, ok
}

func (h *HostKeys) load() (*hostKeyFile, error) {
	empty := &hostKeyFile{Version: currentVersion, Hosts: map[string]HostKey{}}

	b, err := os.ReadFile(h.path)
	if os.IsNotExist(err) {
		return empty, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", h.path, err)
	}
	var f hostKeyFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", h.path, err)
	}
	if f.Hosts == nil {
		f.Hosts = map[string]HostKey{}
	}
	return &f, nil
}

func (h *HostKeys) save(f *hostKeyFile) error {
	f.Version = currentVersion
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(h.path, append(b, '\n'), 0o600)
}
