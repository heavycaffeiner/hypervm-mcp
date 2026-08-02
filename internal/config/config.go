// Package config loads and persists the service-wide configuration that lives
// under %ProgramData%\hypervm-mcp. The file is written once at install time by
// an elevated process and read at startup by the LocalSystem service; nothing
// in the user session is expected to be able to modify it.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// baseName is the product name every other identity is derived from.
const baseName = "hypervm-mcp"

// instance is set at build time to run a second copy alongside an installed one:
//
//	go build -ldflags "-X github.com/heavycaffeiner/hypervm-mcp/internal/config.instance=dev"
//
// It suffixes the service, the pipe, the data directory, the event log source
// and the firewall rules it creates, so a development build cannot restart the
// installed service, read its credentials, or answer on its pipe.
//
// It does NOT isolate Hyper-V. Both instances manage the same hypervisor and see
// the same virtual machines, so keeping their VM names apart is still a matter
// of discipline.
var instance string

// Suffix is "" for a release build and "-<instance>" otherwise.
func Suffix() string {
	if instance == "" {
		return ""
	}
	return "-" + instance
}

// Instance reports the build's instance name, empty for a release build.
func Instance() string { return instance }

// ServiceName is the Windows service name registered with the SCM.
func ServiceName() string { return baseName + Suffix() }

// ServiceDisplayName is what shows up in services.msc.
func ServiceDisplayName() string {
	if instance == "" {
		return baseName
	}
	return baseName + " (" + instance + ")"
}

// DefaultPipeName is the pipe basename; the full path is \\.\pipe\<name>.
func DefaultPipeName() string { return baseName + Suffix() }

// ResourcePrefix names things this instance creates outside its own directory —
// firewall rules and NAT entries — so a development build never removes one the
// installed service owns.
func ResourcePrefix() string { return baseName + Suffix() }

// currentVersion is bumped whenever the on-disk shape changes incompatibly.
const currentVersion = 1

// Config is the on-disk service configuration.
type Config struct {
	Version                  int    `json:"version"`
	PipeName                 string `json:"pipe_name"`
	AllowedSID               string `json:"allowed_sid"`
	PowerShellPath           string `json:"powershell_path"`
	PowerShellTimeoutSeconds int    `json:"powershell_timeout_seconds"`
	MaxConcurrentPowerShell  int    `json:"max_concurrent_powershell"`
	// TailscalePath and ImageLibraryPath are unused until later phases, but are
	// written from the start so the file shape does not change under an installed
	// service.
	TailscalePath    string `json:"tailscale_path"`
	ImageLibraryPath string `json:"image_library_path"`
	LogLevel         string `json:"log_level"`
}

// DataDir returns %ProgramData%\hypervm-mcp, the root of all persisted state.
// A development build gets its own directory, so it cannot read or overwrite an
// installed instance's credentials, pinned host keys or tunnel definitions.
func DataDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, baseName+Suffix())
}

// BinDir holds the copy of the executable that the SCM launches. It must live
// somewhere only administrators can write to; see ConfigPath's callers.
func BinDir() string { return filepath.Join(DataDir(), "bin") }

// BinaryPath is the ImagePath target for the registered service. The instance
// suffix is kept in the filename so the two show up apart in a process list.
func BinaryPath() string { return filepath.Join(BinDir(), baseName+Suffix()+".exe") }

// ConfigPath is the location of config.json.
func ConfigPath() string { return filepath.Join(DataDir(), "config.json") }

// CredentialsPath is the location of the DPAPI-encrypted credential blob.
func CredentialsPath() string { return filepath.Join(DataDir(), "credentials.dat") }

// LogDir holds rotating service logs.
func LogDir() string { return filepath.Join(DataDir(), "logs") }

// LogPath is the service's log file.
func LogPath() string { return filepath.Join(LogDir(), "service.log") }

// DefaultPowerShellPath returns the Windows PowerShell 5.1 interpreter. The
// Hyper-V module ships in-box for 5.1; PowerShell 7 loads it through the
// WinPSCompatSession shim, which adds a remoting hop and changes serialization
// fidelity. Pinning 5.1 keeps behaviour predictable.
func DefaultPowerShellPath() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, `System32\WindowsPowerShell\v1.0\powershell.exe`)
}

// New returns a Config populated with defaults for the given user SID.
func New(allowedSID string) *Config {
	return &Config{
		Version:                  currentVersion,
		PipeName:                 DefaultPipeName(),
		AllowedSID:               allowedSID,
		PowerShellPath:           DefaultPowerShellPath(),
		PowerShellTimeoutSeconds: 300,
		MaxConcurrentPowerShell:  8,
		LogLevel:                 "info",
	}
}

// PipePath renders the full \\.\pipe\... path for this configuration.
func (c *Config) PipePath() string { return `\\.\pipe\` + c.PipeName }

// applyDefaults fills in zero-valued fields so that a hand-edited or older
// config file still yields a usable configuration rather than, say, a zero
// timeout that fails every call instantly.
func (c *Config) applyDefaults() {
	if c.PipeName == "" {
		c.PipeName = DefaultPipeName()
	}
	if c.PowerShellPath == "" {
		c.PowerShellPath = DefaultPowerShellPath()
	}
	if c.PowerShellTimeoutSeconds <= 0 {
		c.PowerShellTimeoutSeconds = 300
	}
	if c.MaxConcurrentPowerShell <= 0 {
		c.MaxConcurrentPowerShell = 8
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
}

// Load reads config.json. A missing file is reported distinctly via
// os.ErrNotExist so callers can tell "not installed" from "corrupt install".
func Load() (*Config, error) {
	b, err := os.ReadFile(ConfigPath())
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ConfigPath(), err)
	}
	if c.AllowedSID == "" {
		return nil, fmt.Errorf("%s: allowed_sid is empty; reinstall the service", ConfigPath())
	}
	c.applyDefaults()
	return &c, nil
}

// Save writes config.json atomically. The caller is responsible for having
// created and ACL'd DataDir beforehand.
func Save(c *Config) error {
	c.Version = currentVersion
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(ConfigPath(), append(b, '\n'), 0o600)
}

// writeFileAtomic writes to a sibling temp file and renames over the target, so
// a crash mid-write cannot leave a truncated config behind.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeds

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	// os.Rename maps to MoveFileEx(MOVEFILE_REPLACE_EXISTING) on Windows.
	return os.Rename(tmp, path)
}

// WriteFileAtomic is exported for other packages that persist state in DataDir.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(path, data, perm)
}
