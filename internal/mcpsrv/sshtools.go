package mcpsrv

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/sshx"
)

type sshExecInput struct {
	VMName         string `json:"vm_name" jsonschema:"Exact name of the VM."`
	Command        string `json:"command" jsonschema:"Command to run in the guest shell."`
	Username       string `json:"username,omitempty" jsonschema:"Overrides the stored username."`
	Password       string `json:"password,omitempty" jsonschema:"Overrides the stored password."`
	PrivateKey     string `json:"private_key,omitempty" jsonschema:"PEM private key, overriding the stored one."`
	Host           string `json:"host,omitempty" jsonschema:"Address to connect to, overriding the one Hyper-V reports."`
	Port           int    `json:"port,omitempty" jsonschema:"SSH port. Defaults to the stored value, else 22."`
	Stdin          string `json:"stdin,omitempty" jsonschema:"Data to feed the command on standard input."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Default 120."`
	TrustNewKey    bool   `json:"trust_new_key,omitempty" jsonschema:"Accept and re-pin a changed host key. Needed after rebuilding or reverting the VM."`
}

type sshExecResult struct {
	Stdout             string `json:"stdout"`
	Stderr             string `json:"stderr"`
	ExitCode           int    `json:"exit_code"`
	HostKeyFingerprint string `json:"host_key_fingerprint,omitempty"`
	HostKeyFirstSeen   bool   `json:"host_key_first_seen,omitempty"`
}

type sshInfoResult struct {
	VMName        string   `json:"vm_name"`
	Address       string   `json:"address"`
	AllAddresses  []string `json:"all_addresses"`
	Port          int      `json:"port"`
	Username      string   `json:"username"`
	AuthMethods   []string `json:"auth_methods"`
	HostKeyPinned bool     `json:"host_key_pinned"`
	Fingerprint   string   `json:"fingerprint,omitempty"`
	SSHCommand    string   `json:"ssh_command"`
	ReachableFrom []string `json:"reachable_from"`
}

func registerSSHTools(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  "ssh_exec",
		Title: "Run a command over SSH",
		Description: "Run a command inside a guest over SSH and return its output and exit code. " +
			"A non-zero exit is a result, not an error.\n\n" +
			"Compared with guest_invoke_command, this works with any guest OS but needs guest " +
			"networking and a running sshd.\n\n" +
			"Credentials come from this call if given, otherwise from what is stored for the VM. " +
			"The host key is pinned on first connect; a later mismatch fails until trust_new_key is " +
			"passed, which is the expected path after rebuilding the VM or reverting a checkpoint.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sshExecInput) (*mcp.CallToolResult, *sshExecResult, error) {
		if in.Command == "" {
			return nil, nil, hverr.New(hverr.InvalidArgument, "command is required")
		}
		client, verdict, err := d.Guest.Connect(ctx, in.VMName, sshx.Credential{
			Username:   in.Username,
			Password:   in.Password,
			PrivateKey: in.PrivateKey,
			Port:       in.Port,
		}, in.Host, in.TrustNewKey)
		if err != nil {
			return nil, nil, err
		}

		res, err := sshx.Exec(ctx, client, in.Command, in.Stdin,
			time.Duration(in.TimeoutSeconds)*time.Second)
		if err != nil {
			return nil, nil, err
		}

		out := &sshExecResult{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode}
		if verdict != nil {
			out.HostKeyFingerprint = verdict.Fingerprint
			out.HostKeyFirstSeen = verdict.FirstSeen
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "ssh_info",
		Title: "SSH connection details",
		Description: "Report everything needed to reach a VM over SSH from another tool, without " +
			"running anything: the resolved address, port, stored username, which authentication " +
			"methods are available, and a ready-to-paste ssh command.\n\n" +
			"reachable_from says which vantage points can use that address as-is. The host always " +
			"can; the LAN only when the VM is on an External switch; a tailnet peer only through an " +
			"open tunnel.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in vmNameInput) (*mcp.CallToolResult, *sshInfoResult, error) {
		cred, err := d.Guest.Credential(in.Name, sshx.Credential{})
		if err != nil {
			return nil, nil, err
		}
		addr, all, err := d.VM.ResolveGuestIP(ctx, in.Name)
		if err != nil {
			return nil, nil, err
		}

		out := &sshInfoResult{
			VMName:        in.Name,
			Address:       addr,
			AllAddresses:  all,
			Port:          cred.Port,
			Username:      cred.Username,
			AuthMethods:   []string{},
			ReachableFrom: []string{"host"},
			SSHCommand:    fmt.Sprintf("ssh %s@%s", cred.Username, addr),
		}
		if cred.Port != 22 {
			out.SSHCommand = fmt.Sprintf("ssh -p %d %s@%s", cred.Port, cred.Username, addr)
		}
		if cred.PrivateKey != "" {
			out.AuthMethods = append(out.AuthMethods, "publickey")
		}
		if cred.Password != "" {
			out.AuthMethods = append(out.AuthMethods, "password")
		}
		if key, ok := d.HostKeys.Get(in.Name); ok {
			out.HostKeyPinned = true
			out.Fingerprint = key.Fingerprint
		}

		// Only an External switch puts the guest on the physical LAN; every
		// other switch type keeps it behind the host.
		if detail, err := d.VM.GetVM(ctx, in.Name); err == nil {
			for _, nic := range detail.NetworkAdapters {
				for _, sw := range switchesOfType(ctx, d, "External") {
					if strings.EqualFold(nic.SwitchName, sw) {
						out.ReachableFrom = append(out.ReachableFrom, "lan")
					}
				}
			}
		}
		for _, t := range d.Tunnels.List(in.Name) {
			if t.BindScope == "tailnet" && t.GuestPort == cred.Port {
				out.ReachableFrom = append(out.ReachableFrom, "tailnet")
				break
			}
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ssh_forget_host_key",
		Title:       "Forget a pinned SSH host key",
		Description: "Drop the pinned host key for a VM, so the next connection pins whatever it finds. Use after rebuilding a VM if you prefer an explicit reset over passing trust_new_key.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in vmNameInput) (*mcp.CallToolResult, map[string]any, error) {
		if err := d.HostKeys.Forget(in.Name); err != nil {
			return nil, nil, err
		}
		d.SSH.Drop(in.Name)
		return nil, map[string]any{"forgotten": in.Name}, nil
	})
}

// switchesOfType returns the names of switches of a given type, or nothing if
// they cannot be listed — this only refines a hint, so a failure is not fatal.
func switchesOfType(ctx context.Context, d *Deps, kind string) []string {
	switches, err := d.VM.ListSwitches(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range switches {
		if strings.EqualFold(s.SwitchType, kind) {
			out = append(out, s.Name)
		}
	}
	return out
}
