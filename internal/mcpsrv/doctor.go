package mcpsrv

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
)

// Check is one diagnostic result.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"
)

// Diagnose runs every service-side check.
//
// It lives here so the MCP tool and the CLI's doctor command report exactly the
// same thing; a diagnostic that differs depending on how you asked is worse than
// none. Checks are ordered so that a failure explains the ones below it — no
// Hyper-V means every later check is meaningless.
func Diagnose(ctx context.Context, d *Deps) []Check {
	checks := []Check{
		checkHyperV(ctx, d),
		checkStoragePaths(ctx, d),
		checkSwitches(ctx, d),
		checkCredentials(d),
		checkTailnet(ctx, d),
	}
	return append(checks, checkTunnels(ctx, d)...)
}

func checkHyperV(ctx context.Context, d *Deps) Check {
	vms, err := d.VM.ListVMs(ctx, "")
	if err != nil {
		return Check{
			Name:   "Hyper-V",
			Status: statusFail,
			Detail: err.Error(),
			Hint: "Enable the Hyper-V Windows feature, and check that the Virtual Machine " +
				"Management service (vmms) is running.",
		}
	}
	running := 0
	for _, vm := range vms {
		if vm.State == "Running" {
			running++
		}
	}
	return Check{
		Name:   "Hyper-V",
		Status: statusOK,
		Detail: fmt.Sprintf("%d VM(s), %d running", len(vms), running),
	}
}

func checkStoragePaths(ctx context.Context, d *Deps) Check {
	paths, err := d.VM.GetHostStoragePaths(ctx)
	if err != nil {
		return Check{Name: "Storage paths", Status: statusWarn, Detail: err.Error()}
	}

	var problems []string
	if !paths.VMPathAccessible {
		problems = append(problems, "VM path "+paths.VirtualMachinePath)
	}
	if !paths.VHDPathAccessible {
		problems = append(problems, "disk path "+paths.VirtualHardDiskPath)
	}
	if len(problems) > 0 {
		return Check{
			Name:   "Storage paths",
			Status: statusFail,
			Detail: "not writable by the service: " + strings.Join(problems, ", "),
			Hint: "The service runs as LocalSystem, which cannot see mapped drive letters and " +
				"reaches network shares as the computer account. Point these at a local path, or " +
				"a UNC path the machine account can write to.",
		}
	}
	return Check{
		Name:   "Storage paths",
		Status: statusOK,
		Detail: fmt.Sprintf("VMs in %s, disks in %s", paths.VirtualMachinePath, paths.VirtualHardDiskPath),
	}
}

func checkSwitches(ctx context.Context, d *Deps) Check {
	switches, err := d.VM.ListSwitches(ctx)
	if err != nil {
		return Check{Name: "Virtual switches", Status: statusWarn, Detail: err.Error()}
	}
	if len(switches) == 0 {
		return Check{
			Name:   "Virtual switches",
			Status: statusWarn,
			Detail: "none defined",
			Hint:   "VMs will have no network. Create one with create_switch.",
		}
	}

	byType := map[string]int{}
	external := false
	for _, s := range switches {
		byType[s.SwitchType]++
		if s.SwitchType == "External" {
			external = true
		}
	}
	var parts []string
	for kind, n := range byType {
		parts = append(parts, fmt.Sprintf("%d %s", n, kind))
	}
	c := Check{Name: "Virtual switches", Status: statusOK, Detail: strings.Join(parts, ", ")}
	if !external {
		// Worth saying, because it is the answer to a whole class of problems
		// tunnels cannot solve.
		c.Hint = "No External switch, so no guest has its own address on the physical LAN. " +
			"Services the host already occupies — SMB, RDP, WinRM — cannot be reached through a " +
			"tunnel, and would need one."
	}
	return c
}

func checkCredentials(d *Deps) Check {
	list, err := d.Creds.List()
	if err != nil {
		return Check{Name: "Credentials", Status: statusFail, Detail: err.Error()}
	}
	if len(list) == 0 {
		return Check{
			Name:   "Credentials",
			Status: statusOK,
			Detail: "none stored",
			Hint:   "ssh_exec and ssh-mode tunnels need one: hypervm-mcp cred set --vm NAME --user NAME",
		}
	}
	var names []string
	for _, e := range list {
		names = append(names, e.VMName)
	}
	return Check{
		Name:   "Credentials",
		Status: statusOK,
		Detail: fmt.Sprintf("%d stored: %s", len(list), strings.Join(names, ", ")),
	}
}

func checkTailnet(ctx context.Context, d *Deps) Check {
	status, err := d.Tailnet.Status(ctx)
	if err != nil {
		// Not having Tailscale is a perfectly normal state, not a fault.
		return Check{
			Name:   "Tailscale",
			Status: statusOK,
			Detail: "not installed",
			Hint:   `Only needed for tunnels with bind_scope "tailnet".`,
		}
	}
	if status.BackendState != "Running" {
		return Check{
			Name:   "Tailscale",
			Status: statusWarn,
			Detail: "installed but not connected (state " + status.BackendState + ")",
			Hint:   "Run `tailscale up` before opening a tunnel with bind_scope \"tailnet\".",
		}
	}
	detail := strings.Join(status.Addresses, ", ")
	if status.MagicDNSName != "" {
		detail += " (" + status.MagicDNSName + ")"
	}
	return Check{Name: "Tailscale", Status: statusOK, Detail: detail}
}

// checkTunnels reports each tunnel separately, since one broken tunnel among
// several is exactly the thing a summary would hide.
func checkTunnels(ctx context.Context, d *Deps) []Check {
	tunnels := d.Tunnels.List("")
	if len(tunnels) == 0 {
		return []Check{{Name: "Tunnels", Status: statusOK, Detail: "none open"}}
	}

	out := make([]Check, 0, len(tunnels))
	for _, t := range tunnels {
		name := fmt.Sprintf("Tunnel %s", t.ID)
		detail := fmt.Sprintf("%s -> %s:%d (%s), %d conns",
			strings.Join(t.ListenAddrs, ","), t.VMName, t.GuestPort, t.Mode, t.TotalConns)

		switch {
		case t.LastError != "":
			out = append(out, Check{
				Name: name, Status: statusWarn,
				Detail: detail + "; last error: " + t.LastError,
				Hint: "If the guest service is bound to the guest's own 127.0.0.1, only mode " +
					"\"ssh\" can reach it.",
			})
		case len(t.Warnings) > 0:
			out = append(out, Check{
				Name: name, Status: statusWarn,
				Detail: detail + "; " + strings.Join(t.Warnings, "; "),
			})
		default:
			out = append(out, Check{Name: name, Status: statusOK, Detail: detail})
		}
	}
	return out
}

func registerDoctorTool(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  "doctor",
		Title: "Diagnose the setup",
		Description: "Check everything this server depends on and report what is wrong and how to fix " +
			"it: Hyper-V availability, whether the service can write to the host's storage paths, " +
			"which virtual switches exist, stored credentials, Tailscale state, and the health of " +
			"every open tunnel. Run it first when something is not working.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
		runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()

		checks := Diagnose(runCtx, d)
		worst := statusOK
		for _, c := range checks {
			if c.Status == statusFail {
				worst = statusFail
				break
			}
			if c.Status == statusWarn {
				worst = statusWarn
			}
		}
		return nil, map[string]any{
			"status":    worst,
			"checks":    checks,
			"data_dir":  config.DataDir(),
			"pipe_path": `\\.\pipe\` + config.DefaultPipeName(),
		}, nil
	})
}
