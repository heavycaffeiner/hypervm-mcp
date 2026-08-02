package hyperv

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
)

// SessionResult is the outcome of running a command on a guest's desktop.
type SessionResult struct {
	Stdout     string `json:"stdout"`
	ExitCode   int    `json:"exit_code"`
	SessionID  int    `json:"session_id"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	LoggedOnAs string `json:"logged_on_as,omitempty"`
}

// GuestRunInSession runs a command on the guest's desktop.
//
// PowerShell Direct lands in session 0, the one Windows keeps for services. That
// session has no desktop: a window opened there is drawn nowhere, a screen
// capture taken there comes back blank, and UI automation finds no elements. So
// anything to do with a graphical program has to run in the session a user is
// logged on to, which is a different session entirely.
//
// The way across is a scheduled task. Registering one for the logged-on user
// with an interactive logon type puts the process in their session, and the
// highest run level gives it an unfiltered administrator token — so this is also
// the answer for a program that needs elevation and shows a window, which
// nothing else here can drive.
//
// It needs somebody logged on. On a guest built for this that means arranging
// automatic logon; without it there is no desktop, and this says so rather than
// waiting for one to appear.
func (c *Client) GuestRunInSession(ctx context.Context, vmName, command, username, password string, timeout time.Duration) (*SessionResult, error) {
	switch {
	case vmName == "":
		return nil, hverr.New(hverr.InvalidArgument, "vm_name is required")
	case command == "":
		return nil, hverr.New(hverr.InvalidArgument, "command is required")
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	// The command travels base64-encoded and is decoded to a file inside the
	// guest, so it is never spliced into script text and needs no quoting.
	payload := base64.StdEncoding.EncodeToString([]byte(command))

	outer := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'

# Who owns a desktop is answered by explorer.exe, not by quser: quser's output
# is localized and its columns shift when a session has no name, whereas a
# process owner and session id are the same everywhere.
$desktop = @(Get-CimInstance Win32_Process -Filter "Name='explorer.exe'" | ForEach-Object {
    $o = Invoke-CimMethod -InputObject $_ -MethodName GetOwner
    if ($o.ReturnValue -eq 0) {
        [pscustomobject]@{
            user    = $(if ($o.Domain) { $o.Domain + '\' + $o.User } else { $o.User })
            session = [int]$_.SessionId
        }
    }
} | Where-Object { $_.session -gt 0 } | Sort-Object session)

if (-not $desktop) {
    throw "HVERR:VM_WRONG_STATE|nobody is logged on to a desktop on this guest, so there is no interactive session to run in. Arrange automatic logon, or sign in once over RDP."
}
$who = $desktop[0]

$dir = Join-Path $env:windir 'Temp\hypervm-session'
New-Item -ItemType Directory -Force -Path $dir | Out-Null

# Sweep anything an interrupted call left behind. The task is unregistered in a
# finally, but a host-side timeout kills this script outright and the finally
# never runs — so without this, tasks would accumulate in a guest that is meant
# to have nothing installed in it.
#
# Only what is plainly stale: an hour old and not running. A tighter rule would
# delete a task belonging to a call happening right now.
Get-ScheduledTask -TaskName 'hypervm-*' -ErrorAction SilentlyContinue |
    Where-Object { $_.State -ne 'Running' } |
    ForEach-Object {
        $script = Join-Path $dir ($_.TaskName + '.ps1')
        $stale = -not (Test-Path -LiteralPath $script)
        if (-not $stale) {
            $stale = (Get-Item -LiteralPath $script).LastWriteTime -lt (Get-Date).AddHours(-1)
        }
        if ($stale) {
            Unregister-ScheduledTask -TaskName $_.TaskName -Confirm:$false -ErrorAction SilentlyContinue
            Remove-Item -LiteralPath $script -Force -ErrorAction SilentlyContinue
            Remove-Item -LiteralPath (Join-Path $dir ($_.TaskName + '.out')) -Force -ErrorAction SilentlyContinue
        }
    }
$id  = 'hypervm-' + [guid]::NewGuid().ToString('N').Substring(0, 12)
$ps1 = Join-Path $dir ($id + '.ps1')
$out = Join-Path $dir ($id + '.out')

$body = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('%s'))
$nl = [Environment]::NewLine
# Everything the command writes, errors included, goes to one transcript. The
# task itself can only report whether it started, so the transcript is the only
# way to find out what happened.
$wrapped = '$ErrorActionPreference = ''Continue''' + $nl +
           '& {' + $nl + $body + $nl + '} *> ''' + $out + '''' + $nl +
           'exit $LASTEXITCODE'
Set-Content -LiteralPath $ps1 -Value $wrapped -Encoding UTF8

$arg = '-NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File "' + $ps1 + '"'
$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument $arg
# Interactive puts the process in the logged-on user's session; Highest gives it
# the unfiltered token a program needing elevation expects.
$principal = New-ScheduledTaskPrincipal -UserId $who.user -LogonType Interactive -RunLevel Highest
Register-ScheduledTask -TaskName $id -Action $action -Principal $principal -Force | Out-Null

try {
    Start-ScheduledTask -TaskName $id
    $deadline = (Get-Date).AddSeconds(%d)
    $timedOut = $false
    while ($true) {
        if ((Get-ScheduledTask -TaskName $id).State -ne 'Running') { Start-Sleep -Milliseconds 400; break }
        if ((Get-Date) -gt $deadline) {
            $timedOut = $true
            Stop-ScheduledTask -TaskName $id
            break
        }
        Start-Sleep -Milliseconds 500
    }
    $text = ''
    if (Test-Path -LiteralPath $out) { $text = [string](Get-Content -LiteralPath $out -Raw) }

    [ordered]@{
        stdout       = $text
        exit_code    = [int](Get-ScheduledTaskInfo -TaskName $id).LastTaskResult
        session_id   = $who.session
        timed_out    = $timedOut
        logged_on_as = $who.user
    } | ConvertTo-Json -Compress -Depth 4
} finally {
    Unregister-ScheduledTask -TaskName $id -Confirm:$false -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $ps1 -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $out -Force -ErrorAction SilentlyContinue
}
`, payload, int(timeout.Seconds()))

	// Delivered over the VMBus, so this works on a guest with no network at all.
	res, err := c.GuestInvokeCommand(ctx, vmName, outer, username, password, timeout+2*time.Minute)
	if err != nil {
		return nil, err
	}

	var out SessionResult
	if err := json.Unmarshal([]byte(lastJSONLine(res.Stdout)), &out); err != nil {
		return nil, hverr.New(hverr.Internal,
			"the guest's report was not the JSON this expected").WithDetail(res.Stdout)
	}
	return &out, nil
}

// lastJSONLine picks the report out of the guest's output.
//
// The scheduled-task cmdlets are chatty on some builds, so the report is taken
// as the last line that looks like an object rather than assuming it stands
// alone.
func lastJSONLine(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}") {
			return t
		}
	}
	return ""
}
