package hyperv

import (
	"context"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/winpath"
)

// guestServiceInterfaceID is the VMBus identifier of the Guest Service
// Interface component, which Copy-VMFile needs. Hyper-V localizes the display
// names of integration services, so this id is the only stable handle on it.
const guestServiceInterfaceID = "6C09BB55-D683-4DA0-8931-C9BF705F6480"

// GuestResult is the outcome of running a command inside a guest.
type GuestResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// GuestInvokeCommand runs a command in a Windows guest over PowerShell Direct.
//
// This path uses the VMBus rather than the network, so it works on a VM with no
// address, no switch, or a Private switch — which makes it the way to bootstrap
// a guest before it can be reached any other way, including configuring the very
// network settings that would cut an SSH session.
//
// It is Windows-only. Linux guests have no PowerShell Direct endpoint; use
// SSHExec for those.
func (c *Client) GuestInvokeCommand(ctx context.Context, vmName, command, username, password string, timeout time.Duration) (*GuestResult, error) {
	switch {
	case vmName == "":
		return nil, hverr.New(hverr.InvalidArgument, "vm_name is required")
	case command == "":
		return nil, hverr.New(hverr.InvalidArgument, "command is required")
	case username == "" || password == "":
		return nil, hverr.New(hverr.CredentialNotFound,
			"PowerShell Direct needs a username and password for %q", vmName).
			WithDetail("Store them with `hypervm-mcp cred set`, or pass them in the call. " +
				"A key is not enough: PowerShell Direct authenticates with a password.")
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	// The command crosses two boundaries as data: into this script as $P, and
	// into the guest as a scriptblock argument. It is never concatenated into
	// script text on either side.
	const script = requireVM + `
    if ($vm.State -ne 'Running') {
        throw "HVERR:VM_WRONG_STATE|'$($P.name)' is $($vm.State); PowerShell Direct needs a running VM"
    }
    $sec  = ConvertTo-SecureString $P.password -AsPlainText -Force
    $cred = New-Object System.Management.Automation.PSCredential($P.username, $sec)

    $res = Invoke-Command -VMId $vm.Id -Credential $cred -ScriptBlock {
        param([string]$command)
        # Continue rather than Stop: a failing command should come back as
        # output and an exit code, not as a thrown error from the transport.
        $ErrorActionPreference = 'Continue'
        $global:LASTEXITCODE = 0
        $out = & ([ScriptBlock]::Create($command)) 2>&1 | Out-String
        [pscustomobject]@{ output = [string]$out; code = [int]$LASTEXITCODE }
    } -ArgumentList $P.command

    $result = [ordered]@{
        stdout    = [string]$res.output
        stderr    = ''
        exit_code = [int]$res.code
    }`

	var out GuestResult
	err := c.r.RunTimeoutInto(ctx, timeout+30*time.Second, script, map[string]any{
		"name":     vmName,
		"command":  command,
		"username": username,
		"password": password,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GuestCopyFile copies a file from the host into a guest.
//
// This goes over the VMBus too, so it needs no guest network — but it does need
// the Guest Service Interface integration component, which this enables if it is
// switched off. On Linux guests that component is the hypervfcopyd daemon from
// the hyperv-daemons package.
//
// Only host to guest is supported; Hyper-V offers no reverse. For the other
// direction use SSHExec, or open a tunnel.
func (c *Client) GuestCopyFile(ctx context.Context, vmName, source, destination string, createFullPath, overwrite bool) error {
	if vmName == "" || destination == "" {
		return hverr.New(hverr.InvalidArgument, "vm_name and destination_path are required")
	}
	// Read by the service, so the LocalSystem path rules apply.
	src, err := winpath.Validate(source, winpath.Read, false)
	if err != nil {
		return err
	}

	const script = requireVM + `
    if ($vm.State -ne 'Running') {
        throw "HVERR:VM_WRONG_STATE|'$($P.name)' is $($vm.State); copying into a guest needs it running"
    }

    # Integration services are matched by their VMBus id, not their name: the
    # name is localized, so 'Guest Service Interface' finds nothing on a Windows
    # installed in another language. The id is the same everywhere.
    $svc = @(Get-VMIntegrationService -VM $vm |
             Where-Object { $_.Id -like '*` + guestServiceInterfaceID + `' })[0]
    if (-not $svc) {
        $have = @(Get-VMIntegrationService -VM $vm | ForEach-Object { $_.Name }) -join ', '
        throw "HVERR:GUEST_SERVICE_UNAVAILABLE|'$($P.name)' offers no Guest Service Interface (has: $have). On Linux install hyperv-daemons and start hypervfcopyd."
    }
    if (-not $svc.Enabled) { Enable-VMIntegrationService -VMIntegrationService $svc | Out-Null }

    $copyArgs = @{
        VM                = $vm
        SourcePath        = $P.source
        DestinationPath   = $P.destination
        FileSource        = 'Host'
    }
    if ($P.create_full_path) { $copyArgs['CreateFullPath'] = $true }
    if ($P.overwrite)        { $copyArgs['Force'] = $true }
    Copy-VMFile @copyArgs
    $result = $true`

	_, err = c.r.RunTimeout(ctx, 30*time.Minute, script, map[string]any{
		"name":             vmName,
		"source":           src,
		"destination":      destination,
		"create_full_path": createFullPath,
		"overwrite":        overwrite,
	})
	return err
}
