//go:build windows

// Package svcmgr installs, controls and hosts the Windows service.
//
// Installing is the one and only moment this system needs elevation. Everything
// afterwards is authorised by the named pipe's DACL, so no MCP session ever
// raises a UAC prompt.
package svcmgr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
	"github.com/heavycaffeiner/hypervm-mcp/internal/ipc"
	"github.com/heavycaffeiner/hypervm-mcp/internal/winsec"
)

const (
	serviceDescription = "Hosts the hypervm-mcp MCP server so Hyper-V can be managed without per-session elevation."

	// vmmsDependency makes the SCM start Hyper-V's management service first.
	// Without it, an auto-start race leaves every early query failing.
	vmmsDependency = "vmms"

	startStopTimeout = 30 * time.Second
)

// Install lays down the data directory, copies the binary somewhere only
// administrators can write, writes the configuration, and registers and starts
// the service. Running it again on an existing install updates it in place.
func Install(allowedSID string) error {
	if !winsec.IsElevated() {
		return errors.New("install requires an elevated process")
	}
	if err := winsec.ValidateSID(allowedSID); err != nil {
		return err
	}

	if err := prepareDataDir(allowedSID); err != nil {
		return err
	}

	cfg := config.New(allowedSID)
	cfg.TailscalePath = detectTailscale()
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("write configuration: %w", err)
	}

	// Best-effort: the service still logs to a file if event log registration
	// fails (for example when a stale registration is left over).
	_ = eventlog.InstallAsEventCreate(config.ServiceName,
		eventlog.Error|eventlog.Warning|eventlog.Info)

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service control manager: %w", err)
	}
	defer m.Disconnect()

	// Upgrading in place: the running service holds a lock on its own binary, so
	// it has to stop before the file can be replaced.
	if s, err := m.OpenService(config.ServiceName); err == nil {
		defer s.Close()
		if err := stopAndWait(s); err != nil {
			return err
		}
		if err := stageBinary(); err != nil {
			return err
		}
		if err := updateExisting(s); err != nil {
			return err
		}
		return startAndWait(s)
	}

	if err := stageBinary(); err != nil {
		return err
	}

	s, err := m.CreateService(config.ServiceName, config.BinaryPath(), mgr.Config{
		DisplayName:      config.ServiceDisplayName,
		Description:      serviceDescription,
		StartType:        mgr.StartAutomatic,
		ServiceStartName: "LocalSystem",
		Dependencies:     []string{vmmsDependency},
	}, "service", "run")
	if err != nil {
		return fmt.Errorf("register the service: %w", err)
	}
	defer s.Close()

	if err := setRecovery(s); err != nil {
		return err
	}
	return startAndWait(s)
}

// Uninstall stops and deregisters the service. purge also deletes the data
// directory, which holds stored credentials.
func Uninstall(purge bool) error {
	if !winsec.IsElevated() {
		return errors.New("uninstall requires an elevated process")
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service control manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(config.ServiceName)
	if err == nil {
		defer s.Close()
		if err := stopAndWait(s); err != nil {
			return err
		}
		if err := s.Delete(); err != nil {
			return fmt.Errorf("deregister the service: %w", err)
		}
	}

	_ = eventlog.Remove(config.ServiceName)

	if purge {
		if err := os.RemoveAll(config.DataDir()); err != nil {
			return fmt.Errorf("remove %s: %w", config.DataDir(), err)
		}
	}
	return nil
}

// Start starts an installed service.
func Start() error {
	s, m, err := open()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	return startAndWait(s)
}

// Stop stops a running service.
func Stop() error {
	s, m, err := open()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	return stopAndWait(s)
}

// StatusInfo is what the status command reports.
type StatusInfo struct {
	Installed     bool
	State         string
	PID           uint32
	BinaryPath    string
	ConfigPresent bool
	AllowedSID    string
	CurrentSID    string
	PipePath      string
	PipeReachable bool
	PipeError     string
}

// Status reports whether the service is installed and running, and — more
// usefully — whether this user can actually open its pipe.
func Status(ctx context.Context) (*StatusInfo, error) {
	info := &StatusInfo{}
	info.CurrentSID, _ = winsec.CurrentUserSID()

	if cfg, err := config.Load(); err == nil {
		info.ConfigPresent = true
		info.AllowedSID = cfg.AllowedSID
		info.PipePath = cfg.PipePath()
	} else {
		info.PipePath = `\\.\pipe\` + config.DefaultPipeName
	}

	// Status must work for the ordinary user this whole design serves, so it
	// asks the SCM only for query rights rather than mgr.Connect's full access.
	if m, err := connectQuery(); err == nil {
		defer m.Disconnect()
		if s, err := openQuery(m, config.ServiceName); err == nil {
			defer s.Close()
			info.Installed = true
			if st, err := s.Query(); err == nil {
				info.State = stateName(st.State)
				info.PID = st.ProcessId
			}
			if c, err := s.Config(); err == nil {
				info.BinaryPath = c.BinaryPathName
			}
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if conn, err := ipc.DialOnce(probeCtx, info.PipePath); err == nil {
		conn.Close()
		info.PipeReachable = true
	} else {
		info.PipeError = err.Error()
	}

	return info, nil
}

// ---- helpers ---------------------------------------------------------------

// connectQuery opens the SCM with just enough access to read service state.
func connectQuery() (*mgr.Mgr, error) {
	h, err := windows.OpenSCManager(nil, nil,
		windows.SC_MANAGER_CONNECT|windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return nil, err
	}
	return &mgr.Mgr{Handle: h}, nil
}

// openQuery opens a service read-only.
func openQuery(m *mgr.Mgr, name string) (*mgr.Service, error) {
	h, err := windows.OpenService(m.Handle, windows.StringToUTF16Ptr(name),
		windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return nil, err
	}
	return &mgr.Service{Name: name, Handle: h}, nil
}

func open() (*mgr.Service, *mgr.Mgr, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, nil, fmt.Errorf("connect to the service control manager: %w", err)
	}
	s, err := m.OpenService(config.ServiceName)
	if err != nil {
		m.Disconnect()
		return nil, nil, fmt.Errorf("%s is not installed: %w", config.ServiceName, err)
	}
	return s, m, nil
}

func prepareDataDir(allowedSID string) error {
	for _, dir := range []string{config.DataDir(), config.BinDir(), config.LogDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	// Applied to the root only; the ACEs are inheritable, so children follow.
	return winsec.SecureDataDir(config.DataDir(), allowedSID)
}

// stageBinary copies this executable into the locked-down data directory.
//
// Pointing the service's ImagePath at wherever the user happened to build the
// binary would let anyone who can write to that directory replace the code
// LocalSystem executes.
func stageBinary() error {
	src, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the running executable: %w", err)
	}
	dst := config.BinaryPath()

	srcPath, _ := filepath.Abs(src)
	if filepath.Clean(srcPath) == filepath.Clean(dst) {
		return nil // already installed and running from the staged copy
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	// A running service holds a lock on the old file; the caller stops it first.
	if err := os.WriteFile(dst, data, 0o700); err != nil {
		return fmt.Errorf("stage the binary at %s: %w", dst, err)
	}
	return nil
}

func updateExisting(s *mgr.Service) error {
	c, err := s.Config()
	if err != nil {
		return fmt.Errorf("read the current service configuration: %w", err)
	}
	c.BinaryPathName = fmt.Sprintf(`"%s" service run`, config.BinaryPath())
	c.StartType = mgr.StartAutomatic
	c.ServiceStartName = "LocalSystem"
	c.Dependencies = []string{vmmsDependency}
	c.DisplayName = config.ServiceDisplayName
	c.Description = serviceDescription
	if err := s.UpdateConfig(c); err != nil {
		return fmt.Errorf("update the service configuration: %w", err)
	}
	return setRecovery(s)
}

// setRecovery asks the SCM to restart the service if it dies: quickly the first
// couple of times, then slowly so a persistent fault does not spin.
func setRecovery(s *mgr.Service) error {
	actions := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}
	if err := s.SetRecoveryActions(actions, uint32((24 * time.Hour).Seconds())); err != nil {
		return fmt.Errorf("set recovery actions: %w", err)
	}
	return nil
}

func startAndWait(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("query the service: %w", err)
	}
	if st.State == svc.Running {
		return nil
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("start the service: %w", err)
	}
	return waitFor(s, svc.Running)
}

func stopAndWait(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("query the service: %w", err)
	}
	if st.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("stop the service: %w", err)
	}
	return waitFor(s, svc.Stopped)
}

func waitFor(s *mgr.Service, want svc.State) error {
	deadline := time.Now().Add(startStopTimeout)
	for {
		st, err := s.Query()
		if err != nil {
			return fmt.Errorf("query the service: %w", err)
		}
		if st.State == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the service stayed in state %s for %s (wanted %s); "+
				"check %s", stateName(st.State), startStopTimeout, stateName(want), config.LogPath())
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func stateName(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "Stopped"
	case svc.StartPending:
		return "StartPending"
	case svc.StopPending:
		return "StopPending"
	case svc.Running:
		return "Running"
	case svc.ContinuePending:
		return "ContinuePending"
	case svc.PausePending:
		return "PausePending"
	case svc.Paused:
		return "Paused"
	default:
		return "Unknown"
	}
}

// detectTailscale records where tailscale.exe lives, for later phases. An empty
// result simply means the tunnel tools will look again at runtime.
func detectTailscale() string {
	for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramW6432")} {
		if root == "" {
			continue
		}
		p := filepath.Join(root, "Tailscale", "tailscale.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
