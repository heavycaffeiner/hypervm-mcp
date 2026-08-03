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
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

	cfg, err := installConfig(allowedSID)
	if err != nil {
		return err
	}
	// Only overwrite a path that was found: an absent Tailscale does not mean a
	// hand-configured one is wrong.
	if ts := detectTailscale(); ts != "" {
		cfg.TailscalePath = ts
	}
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("write configuration: %w", err)
	}

	// Best-effort: the service still logs to a file if event log registration
	// fails (for example when a stale registration is left over).
	_ = eventlog.InstallAsEventCreate(config.ServiceName(),
		eventlog.Error|eventlog.Warning|eventlog.Info)

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service control manager: %w", err)
	}
	defer m.Disconnect()

	// Upgrading in place: the running service holds its own binary open, and has
	// to stop before the file can be replaced and to pick up the new one.
	if s, err := m.OpenService(config.ServiceName()); err == nil {
		defer s.Close()
		if err := stopAndWait(s); err != nil {
			return err
		}
		if err := stageBinary(); err != nil {
			// The old binary is still intact and the service is stopped because
			// we stopped it. An upgrade that could not go ahead must not leave
			// Hyper-V unreachable, so put the previous version back in service.
			if startErr := startAndWait(s); startErr != nil {
				return fmt.Errorf("%w (and the previous version would not restart: %v)", err, startErr)
			}
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

	s, err := m.CreateService(config.ServiceName(), config.BinaryPath(), mgr.Config{
		DisplayName:      config.ServiceDisplayName(),
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

	s, err := m.OpenService(config.ServiceName())
	if err == nil {
		defer s.Close()
		if err := stopAndWait(s); err != nil {
			return err
		}
		if err := s.Delete(); err != nil {
			return fmt.Errorf("deregister the service: %w", err)
		}
	}

	_ = eventlog.Remove(config.ServiceName())

	if purge {
		return purgeDataDir()
	}
	return nil
}

// purgeDataDir deletes the data directory, including the program doing it.
//
// Installing puts the staged binary on PATH, so `service uninstall --purge`
// normally runs that very binary. Windows will not unlink a running image —
// but it will happily rename one. So move it out of the way first: the
// directory then deletes like any other, and what is left is a stray file
// outside the product's own folder, which a small helper removes as soon as
// this process exits.
//
// Copying to TEMP and re-running from there does not work, for the record: the
// original stays alive waiting on the copy, still holding the file.
func purgeDataDir() error {
	root := config.DataDir()
	if err := os.RemoveAll(root); err == nil {
		return nil
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("remove %s: %w", root, err)
	}
	self = filepath.Clean(self)
	if rel, err := filepath.Rel(root, self); err != nil || strings.HasPrefix(rel, "..") {
		// Something else is holding a file; renaming ours would not help.
		return fmt.Errorf("remove %s: %w", root, err)
	}

	// Renamed into the parent directory, so it is certainly the same volume —
	// a rename across volumes is a copy and would be refused for a running
	// image. The leading dot keeps it out of the way if anyone looks.
	stray := filepath.Join(filepath.Dir(root),
		fmt.Sprintf(".%s-old-%d.exe", filepath.Base(root), os.Getpid()))
	if err := os.Rename(self, stray); err != nil {
		return fmt.Errorf("move the running executable out of %s: %w", root, err)
	}

	if err := os.RemoveAll(root); err != nil {
		// Put it back rather than leave the install half-dismantled.
		_ = os.Rename(stray, self)
		return fmt.Errorf("remove %s: %w", root, err)
	}

	scheduleDelete(stray)
	return nil
}

// markForDeleteAtReboot has Windows unlink a file at the next restart.
//
// This is all that can be done about a file some other process still has open,
// and it is the only disposal an upgrade needs: the stale image it leaves is
// held by an MCP client's bridge, which can outlive any wait worth making.
// Best effort, and nothing depends on it: the file holds nothing by now.
func markForDeleteAtReboot(path string) {
	_ = windows.MoveFileEx(windows.StringToUTF16Ptr(path), nil,
		windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}

// scheduleDelete removes a file once this process lets go of it.
//
// For the one case where waiting pays: uninstalling renames the running program
// out of the way, and Windows releases that image within moments of this process
// exiting. A background cmd retries for about a minute and stops the moment the
// file goes, with the reboot backstop covering the helper never running at all.
func scheduleDelete(path string) {
	markForDeleteAtReboot(path)

	// Written to a file rather than passed as one long argument, because cmd
	// re-parses quotes in a way that is painful to get right through exec.
	script := filepath.Join(os.TempDir(), fmt.Sprintf("hypervm-mcp-cleanup-%d.cmd", os.Getpid()))
	body := "@echo off\r\n" +
		"for /l %%i in (1,1,30) do (\r\n" +
		"  if not exist \"" + path + "\" goto done\r\n" +
		"  del /f /q \"" + path + "\" >nul 2>&1\r\n" +
		"  ping -n 2 127.0.0.1 >nul\r\n" +
		")\r\n" +
		":done\r\n" +
		"del /f /q \"%~f0\" >nul 2>&1\r\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		return
	}

	// No wait: this outlives the caller on purpose.
	_ = cleanupCommand(script).Start()
}

// cleanupCommand builds the helper process, and the flags are the point of it.
//
// A hidden console, and deliberately not DETACHED_PROCESS. Detaching leaves the
// helper with no console at all, and Windows then hands every console program it
// starts a brand new one, window and all: HideWindow applies to the process
// created here and never to its children. `ping`, which the loop sleeps with,
// therefore put a console window on the user's desktop once per iteration. A
// hidden console is inherited, so nothing shows.
//
// /d for the same reason one step removed: without it the helper also runs
// whatever the user has in cmd's AutoRun, which is free to start a console
// program of its own.
func cleanupCommand(script string) *exec.Cmd {
	cmd := exec.Command("cmd", "/d", "/c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
	return cmd
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
		info.PipePath = `\\.\pipe\` + config.DefaultPipeName()
	}

	// Status must work for the ordinary user this whole design serves, so it
	// asks the SCM only for query rights rather than mgr.Connect's full access.
	if m, err := connectQuery(); err == nil {
		defer m.Disconnect()
		if s, err := openQuery(m, config.ServiceName()); err == nil {
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
	s, err := m.OpenService(config.ServiceName())
	if err != nil {
		m.Disconnect()
		return nil, nil, fmt.Errorf("%s is not installed: %w", config.ServiceName(), err)
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

// installConfig returns the configuration to write for this install.
//
// Re-running the installer is how this product upgrades, so it must not quietly
// revert settings someone edited. Only the allowed SID is taken from the install
// itself: it identifies whoever is running it, and reinstalling as another user
// is the documented way to hand the pipe over.
func installConfig(allowedSID string) (*config.Config, error) {
	cfg, err := config.Load()
	switch {
	case err == nil:
		cfg.AllowedSID = allowedSID
		return cfg, nil
	case errors.Is(err, os.ErrNotExist):
		return config.New(allowedSID), nil
	}

	// Unusable, so it has to be replaced. Keep it anyway rather than overwrite
	// the only copy of whatever was in there.
	bak := config.ConfigPath() + ".bak"
	if renErr := os.Rename(config.ConfigPath(), bak); renErr != nil {
		return nil, fmt.Errorf("%s is unusable (%v) and could not be moved aside: %w",
			config.ConfigPath(), err, renErr)
	}
	return config.New(allowedSID), nil
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

	sweepStaleBinaries()

	// Written beside the target and renamed over it, so a write that fails
	// partway leaves a working service behind rather than a truncated image.
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, data, 0o700); err != nil {
		return fmt.Errorf("stage the binary at %s: %w", tmp, err)
	}
	defer os.Remove(tmp) // no-op once a rename has moved it

	if err := os.Rename(tmp, dst); err == nil {
		return nil
	}

	// The caller stopped the service, but it is not the only thing running this
	// file: every MCP client holds a `bridge` process open on it, and so does
	// `update` itself. Windows will not replace a mapped executable image, but
	// it will rename one. Move the old file aside and put the new one in its
	// place; whatever is still running carries on from the renamed image until
	// it exits, and the next upgrade sweeps it up.
	stray, err := renameAside(dst)
	if err != nil {
		return fmt.Errorf("stage the binary at %s: %w", dst, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Rename(stray, dst) // put the working binary back
		return fmt.Errorf("stage the binary at %s: %w", dst, err)
	}
	// No waiting for this one. Whatever holds the old image is a bridge process
	// that lives as long as its MCP client does, so there is nothing to wait
	// for: sweepStaleBinaries collects it at the next upgrade, and the reboot
	// takes it if that never comes.
	markForDeleteAtReboot(stray)
	return nil
}

// stalePrefix marks a binary an upgrade displaced but could not yet delete.
const stalePrefix = ".old-"

// renameAside moves a file to a free name in its own directory, which keeps the
// rename on one volume: a cross-volume rename is a copy, and Windows refuses
// that for a running image.
func renameAside(path string) (string, error) {
	dir, base := filepath.Split(path)
	for i := 0; i < 100; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s%d-%s", stalePrefix, i, base))
		if _, err := os.Stat(candidate); err == nil {
			continue // an earlier upgrade's leftover is still there
		}
		if err := os.Rename(path, candidate); err != nil {
			return "", err
		}
		return candidate, nil
	}
	return "", fmt.Errorf("found no free name to move %s aside", path)
}

// sweepStaleBinaries deletes what earlier upgrades displaced, now that the
// processes holding those images have most likely exited. Best effort by
// design: anything still running stays, and scheduleDelete already arranged for
// the reboot to take it if nothing else does.
func sweepStaleBinaries() {
	matches, err := filepath.Glob(filepath.Join(config.BinDir(), stalePrefix+"*"))
	if err != nil {
		return
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
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
	c.DisplayName = config.ServiceDisplayName()
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
