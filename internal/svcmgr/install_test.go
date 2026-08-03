//go:build windows

package svcmgr

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// The cleanup helper's creation flags are load-bearing, and getting them wrong
// is not a crash but a console window flashing on the user's desktop once per
// retry, which is why they are pinned here rather than left to a comment.
func TestCleanupCommandKeepsItsConsoleHidden(t *testing.T) {
	cmd := cleanupCommand(`C:\Windows\Temp\cleanup.cmd`)

	if cmd.SysProcAttr.CreationFlags&windows.DETACHED_PROCESS != 0 {
		t.Error("DETACHED_PROCESS leaves the helper without a console, so every console " +
			"program it starts is given a visible one of its own")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Error("without CREATE_NO_WINDOW the helper's own console is visible")
	}
	if !slices.Contains(cmd.Args, "/d") {
		t.Error("without /d the helper runs the user's cmd AutoRun, which may start a " +
			"console program this has no say over")
	}
}

// The upgrade path rests on one Windows behaviour: a running executable image
// cannot be overwritten, but it can be renamed. Every MCP client keeps a
// `bridge` process open on the staged binary, so without this an upgrade fails
// whenever a client is running, and fails with the service already stopped.
func TestRenameAsideFreesARunningImage(t *testing.T) {
	src := filepath.Join(os.Getenv("SystemRoot"), "System32", "ping.exe")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("no %s to copy: %v", src, err)
	}

	dir := t.TempDir()
	exe := filepath.Join(dir, "runner.exe")
	if err := os.WriteFile(exe, data, 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(exe, "-n", "60", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the copy: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	if err := os.WriteFile(exe, []byte("replacement"), 0o700); err == nil {
		t.Fatal("overwrote a running image, so the premise of the fallback no longer holds")
	}

	stray, err := renameAside(exe)
	if err != nil {
		t.Fatalf("move a running image aside: %v", err)
	}
	if got := filepath.Dir(stray); got != dir {
		t.Fatalf("moved it to %s; a rename off the volume is a copy, which Windows refuses here", got)
	}
	if !strings.HasPrefix(filepath.Base(stray), stalePrefix) {
		t.Fatalf("%s is not named so that sweepStaleBinaries finds it", stray)
	}
	if _, err := os.Stat(exe); err == nil {
		t.Fatal("the old image is still in place")
	}
	if err := os.WriteFile(exe, []byte("replacement"), 0o700); err != nil {
		t.Fatalf("write the replacement where the running image was: %v", err)
	}

	// A second upgrade while the first leftover is still held must not collide
	// with it, or it would either fail or destroy an image in use.
	if err := os.WriteFile(exe, data, 0o700); err != nil {
		t.Fatal(err)
	}
	second, err := renameAside(exe)
	if err != nil {
		t.Fatalf("move a second image aside: %v", err)
	}
	if second == stray {
		t.Fatalf("reused %s, which is still held by the running process", stray)
	}
}
