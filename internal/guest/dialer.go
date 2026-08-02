//go:build windows

// Package guest ties together the three things needed to reach inside a VM:
// where it is, who to log in as, and a pooled connection.
package guest

import (
	"context"
	"net"
	"strconv"

	"golang.org/x/crypto/ssh"

	"github.com/heavycaffeiner/hypervm-mcp/internal/creds"
	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/hyperv"
	"github.com/heavycaffeiner/hypervm-mcp/internal/sshx"
)

const defaultSSHPort = 22

// Dialer resolves a VM name into a live SSH connection.
type Dialer struct {
	VM    *hyperv.Client
	Creds *creds.Store
	Pool  *sshx.Pool
}

// Credential resolves how to log into a VM.
//
// An explicit credential in the call wins over the stored one, and nothing is
// guessed: with neither available the call fails rather than trying a default
// username that would produce a confusing authentication error.
func (d *Dialer) Credential(vmName string, override sshx.Credential) (sshx.Credential, error) {
	stored, ok, err := d.Creds.Get(vmName)
	if err != nil {
		return sshx.Credential{}, err
	}

	out := override
	if ok {
		if out.Username == "" {
			out.Username = stored.Username
		}
		if out.Password == "" && out.PrivateKey == "" {
			out.Password = stored.Password
			out.PrivateKey = stored.SSHPrivateKey
			out.Passphrase = stored.SSHPassphrase
		}
		if out.Port == 0 {
			out.Port = stored.SSHPort
		}
	}
	if out.Port == 0 {
		out.Port = defaultSSHPort
	}
	if out.Username == "" {
		return sshx.Credential{}, hverr.New(hverr.CredentialNotFound,
			"no credentials for %q", vmName).
			WithDetail("Store one with `hypervm-mcp cred set --vm " + vmName +
				" --user <name>`, or pass a username and password or key in the call.")
	}
	return out, nil
}

// Address returns host:port for a VM's SSH service, resolving the guest IP.
func (d *Dialer) Address(ctx context.Context, vmName string, override sshx.Credential) (string, error) {
	cred, err := d.Credential(vmName, override)
	if err != nil {
		return "", err
	}
	ip, _, err := d.VM.ResolveGuestIP(ctx, vmName)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(ip, strconv.Itoa(cred.Port)), nil
}

// Connect returns a pooled SSH connection to a VM.
//
// host, if set, overrides the resolved guest address — useful when the VM
// answers on an address Hyper-V does not report, such as behind a router.
func (d *Dialer) Connect(ctx context.Context, vmName string, override sshx.Credential, host string, trustNewKey bool) (*ssh.Client, *sshx.Verdict, error) {
	cred, err := d.Credential(vmName, override)
	if err != nil {
		return nil, nil, err
	}
	if host == "" {
		ip, _, err := d.VM.ResolveGuestIP(ctx, vmName)
		if err != nil {
			return nil, nil, err
		}
		host = ip
	}
	addr := net.JoinHostPort(host, strconv.Itoa(cred.Port))
	return d.Pool.Get(ctx, vmName, addr, cred, trustNewKey)
}

// ConnectStored is Connect using only what is on file, optionally at a pinned
// address. Tunnels use it, since they outlive the call that created them and
// have no per-call credential.
func (d *Dialer) ConnectStored(ctx context.Context, vmName, host string) (*ssh.Client, error) {
	client, _, err := d.Connect(ctx, vmName, sshx.Credential{}, host, false)
	return client, err
}
