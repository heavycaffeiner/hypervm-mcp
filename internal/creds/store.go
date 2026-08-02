//go:build windows

// Package creds stores guest OS credentials so they never have to travel through
// a conversation or sit in a config file.
//
// The store lives in the service's data directory, encrypted with machine-scope
// DPAPI, and is written and read only by the service. The CLI hands credentials
// over the named pipe rather than writing the file itself.
package creds

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
)

const currentVersion = 1

// Entry is everything known about how to log into one VM's guest OS.
type Entry struct {
	Username      string `json:"username"`
	Password      string `json:"password,omitempty"`
	SSHPort       int    `json:"ssh_port,omitempty"`
	SSHPrivateKey string `json:"ssh_private_key,omitempty"`
	SSHPassphrase string `json:"ssh_key_passphrase,omitempty"`
}

// Info is the secret-free view returned to callers who only need to know what
// is on file.
type Info struct {
	VMName      string `json:"vm_name"`
	Username    string `json:"username"`
	SSHPort     int    `json:"ssh_port"`
	HasPassword bool   `json:"has_password"`
	HasKey      bool   `json:"has_key"`
}

type file struct {
	Version int              `json:"version"`
	Entries map[string]Entry `json:"entries"`
}

// Store is the process-wide credential store.
type Store struct {
	mu   sync.RWMutex
	path string
}

// NewStore returns a store backed by the service's credentials file.
func NewStore() *Store { return &Store{path: config.CredentialsPath()} }

// Get returns the entry for a VM, if one is stored.
func (s *Store) Get(vmName string) (Entry, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	f, err := s.load()
	if err != nil {
		return Entry{}, false, err
	}
	e, ok := f.Entries[vmName]
	return e, ok, nil
}

// Set stores or replaces the entry for a VM, merging with what is already there
// so that adding an SSH key does not drop an existing password.
func (s *Store) Set(vmName string, e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.load()
	if err != nil {
		return err
	}
	existing := f.Entries[vmName]
	if e.Username == "" {
		e.Username = existing.Username
	}
	if e.Password == "" {
		e.Password = existing.Password
	}
	if e.SSHPort == 0 {
		e.SSHPort = existing.SSHPort
	}
	if e.SSHPrivateKey == "" {
		e.SSHPrivateKey = existing.SSHPrivateKey
		e.SSHPassphrase = existing.SSHPassphrase
	}
	f.Entries[vmName] = e
	return s.save(f)
}

// Delete removes a VM's entry. Removing something that is not there succeeds.
func (s *Store) Delete(vmName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.load()
	if err != nil {
		return err
	}
	delete(f.Entries, vmName)
	return s.save(f)
}

// List reports which VMs have credentials, without revealing them.
// Rename moves a VM's credentials to a new name.
//
// Credentials are filed by VM name, so a rename without this leaves them
// orphaned and every later call reports no credentials for a VM that plainly
// has some. Reports whether there was an entry to move.
func (s *Store) Rename(oldName, newName string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.load()
	if err != nil {
		return false, err
	}
	e, ok := f.Entries[oldName]
	if !ok {
		return false, nil
	}
	f.Entries[newName] = e
	delete(f.Entries, oldName)
	return true, s.save(f)
}

func (s *Store) List() ([]Info, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	f, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(f.Entries))
	for name, e := range f.Entries {
		out = append(out, Info{
			VMName:      name,
			Username:    e.Username,
			SSHPort:     e.SSHPort,
			HasPassword: e.Password != "",
			HasKey:      e.SSHPrivateKey != "",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VMName < out[j].VMName })
	return out, nil
}

// load reads and decrypts the file. A missing file is an empty store, not an
// error: nothing has been stored yet.
func (s *Store) load() (*file, error) {
	empty := &file{Version: currentVersion, Entries: map[string]Entry{}}

	cipher, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return empty, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}
	if len(cipher) == 0 {
		return empty, nil
	}

	plain, err := unprotect(cipher)
	if err != nil {
		return nil, err
	}
	defer zero(plain)

	var f file
	if err := json.Unmarshal(plain, &f); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	if f.Entries == nil {
		f.Entries = map[string]Entry{}
	}
	return &f, nil
}

func (s *Store) save(f *file) error {
	f.Version = currentVersion

	plain, err := json.Marshal(f)
	if err != nil {
		return err
	}
	defer zero(plain)

	cipher, err := protect(plain)
	if err != nil {
		return err
	}
	// Atomic replace: a crash mid-write must not leave a truncated blob that
	// would take every stored credential with it.
	return config.WriteFileAtomic(s.path, cipher, 0o600)
}
