//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
	"github.com/heavycaffeiner/hypervm-mcp/internal/svcmgr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/winsec"
)

func cmdService(args []string) int {
	if len(args) == 0 {
		return fail("service needs a subcommand: install, uninstall, start, stop, status, run")
	}

	switch args[0] {
	case "install":
		return cmdInstall(args[1:])
	case "uninstall":
		return cmdUninstall(args[1:])
	case "start":
		return elevatedOr(args, func() error { return svcmgr.Start() },
			"Service started.")
	case "stop":
		return elevatedOr(args, func() error { return svcmgr.Stop() },
			"Service stopped.")
	case "status":
		return cmdStatus()
	case "run":
		if err := svcmgr.Run(version); err != nil {
			return fail("%v", err)
		}
		return 0
	default:
		return fail("unknown service subcommand %q", args[0])
	}
}

func cmdInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	allowedSID := fs.String("allowed-sid", "", "SID permitted to open the pipe (defaults to the current user)")
	errFile := fs.String("errfile", "", "internal: where the elevated pass records a failure")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *allowedSID == "" {
		sid, err := winsec.CurrentUserSID()
		if err != nil {
			return fail("could not determine the current user's SID: %v", err)
		}
		*allowedSID = sid
	}

	if !winsec.IsElevated() {
		// Resolve the SID before elevating: if the user elevates with a different
		// administrator account, the elevated token's SID is not theirs.
		return relaunch([]string{"service", "install", "--allowed-sid", *allowedSID}, func() {
			printInstallSuccess(*allowedSID)
		})
	}

	if err := svcmgr.Install(*allowedSID); err != nil {
		recordError(*errFile, err)
		return fail("%v", err)
	}
	return 0
}

func cmdUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also delete configuration, logs and stored credentials")
	errFile := fs.String("errfile", "", "internal: where the elevated pass records a failure")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if !winsec.IsElevated() {
		relaunchArgs := []string{"service", "uninstall"}
		if *purge {
			relaunchArgs = append(relaunchArgs, "--purge")
		}
		return relaunch(relaunchArgs, func() {
			fmt.Println("Service removed.")
			if !*purge {
				return
			}
			// The elevated run did the work on a console nobody sees, so report
			// what is actually there rather than what was asked for. When this
			// command came from the copy on PATH, that copy is still running and
			// Windows will not let it delete itself.
			if _, err := os.Stat(config.DataDir()); err != nil {
				fmt.Printf("Deleted %s\n", config.DataDir())
				return
			}
			fmt.Printf("Deleted everything in %s except the program you just ran.\n",
				config.DataDir())
			fmt.Println("Nothing sensitive is left. Remove the folder once this " +
				"process exits, or leave it — installing again reuses the file.")
		})
	}

	if err := svcmgr.Uninstall(*purge); err != nil {
		// Not being able to delete the program currently running is expected
		// when the purge is invoked through the copy on PATH, and everything
		// that matters is already gone.
		if errors.Is(err, svcmgr.ErrPurgedExceptSelf) {
			fmt.Println(err)
			fmt.Println("Nothing sensitive is left. Delete the folder once this " +
				"process exits, or leave it — installing again reuses the file.")
			return 0
		}
		recordError(*errFile, err)
		return fail("%v", err)
	}
	return 0
}

func cmdStatus() int {
	info, err := svcmgr.Status(context.Background())
	if err != nil {
		return fail("%v", err)
	}

	fmt.Printf("Service:        %s\n", yesNo(info.Installed, "installed", "not installed"))
	if info.Installed {
		fmt.Printf("State:          %s", info.State)
		if info.PID != 0 {
			fmt.Printf(" (pid %d)", info.PID)
		}
		fmt.Println()
		fmt.Printf("Binary:         %s\n", info.BinaryPath)
	}
	fmt.Printf("Config:         %s\n", yesNo(info.ConfigPresent, config.ConfigPath(), "missing"))
	fmt.Printf("Pipe:           %s\n", info.PipePath)
	fmt.Printf("Reachable:      %s\n", yesNo(info.PipeReachable, "yes", "no"))
	if !info.PipeReachable && info.PipeError != "" {
		fmt.Printf("                %s\n", info.PipeError)
	}
	fmt.Printf("Current user:   %s\n", info.CurrentSID)
	if info.AllowedSID != "" {
		fmt.Printf("Allowed user:   %s\n", info.AllowedSID)
		if info.AllowedSID != info.CurrentSID {
			fmt.Println("\nThis user is not the one the service was installed for, so the pipe " +
				"will refuse the connection.\nRe-run `hypervm-mcp service install` as this user.")
		}
	}

	if !info.Installed {
		fmt.Println("\nRun `hypervm-mcp service install` to set it up.")
		return 1
	}
	if !info.PipeReachable {
		return 1
	}
	return 0
}

// ---- elevation -------------------------------------------------------------

// elevatedOr runs op directly when already elevated, and otherwise re-launches
// this executable through UAC to do the same work.
func elevatedOr(args []string, op func() error, success string) int {
	args, errFile := splitErrFile(args)

	if !winsec.IsElevated() {
		return relaunch(append([]string{"service"}, args...), func() {
			fmt.Println(success)
		})
	}
	if err := op(); err != nil {
		recordError(errFile, err)
		return fail("%v", err)
	}
	fmt.Println(success)
	return 0
}

// splitErrFile removes the internal --errfile flag from args and returns its
// value, so the elevated pass can report failures back to its hidden parent.
func splitErrFile(args []string) ([]string, string) {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--errfile" {
			rest := append(append([]string{}, args[:i]...), args[i+2:]...)
			return rest, args[i+1]
		}
	}
	return args, ""
}

// relaunch re-runs this executable elevated and waits for it.
//
// The elevated process has its own hidden console, so it cannot print to this
// terminal. It writes any failure to a temp file instead, which we surface here.
func relaunch(args []string, onSuccess func()) int {
	exe, err := os.Executable()
	if err != nil {
		return fail("could not locate this executable: %v", err)
	}

	errFile := filepath.Join(os.TempDir(), "hypervm-mcp-elevated-error.txt")
	_ = os.Remove(errFile)
	args = append(args, "--errfile", errFile)

	code, err := winsec.RunElevated(exe, args)
	if err != nil {
		return fail("%v", err)
	}
	if code != 0 {
		if msg, readErr := os.ReadFile(errFile); readErr == nil && len(msg) > 0 {
			return fail("%s", strings.TrimSpace(string(msg)))
		}
		return fail("the elevated step exited with code %d; see %s", code, config.LogPath())
	}

	_ = os.Remove(errFile)
	onSuccess()
	return 0
}

// recordError leaves a message the unelevated parent can read back.
func recordError(path string, err error) {
	if path == "" {
		return
	}
	_ = os.WriteFile(path, []byte(err.Error()), 0o600)
}

func printInstallSuccess(sid string) {
	fmt.Printf(`Service installed and started.

  Pipe:         \\.\pipe\%s
  Allowed user: %s
  Data:         %s

Add this to a workspace's .mcp.json to use it:

  {
    "mcpServers": {
      %q: { "command": %q, "args": ["bridge"] }
    }
  }

Verify with: %s service status
`, config.DefaultPipeName(), sid, config.DataDir(),
		config.ServiceName(), config.BinaryPath(), config.BinaryPath())
}

func yesNo(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}
