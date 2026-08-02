package hyperv

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
)

// KeyResult reports what was typed.
//
// Rejected is not a warning on its own: the keyboard refuses presses until the
// guest attaches to it, so a run that covers a boot prompt normally starts with
// a few refusals and then succeeds.
type KeyResult struct {
	VMName   string   `json:"vm_name"`
	Keys     []string `json:"keys"`
	Repeated int      `json:"repeated"`
	Sent     int      `json:"sent"`
	Rejected int      `json:"rejected,omitempty"`
}

// namedKeys maps the keys worth pressing at a firmware or boot-loader prompt to
// their Windows virtual-key codes. Anything not listed can still be given as a
// number, so this is a convenience rather than a limit.
var namedKeys = map[string]int{
	"space": 0x20, "enter": 0x0D, "return": 0x0D, "esc": 0x1B, "escape": 0x1B,
	"tab": 0x09, "backspace": 0x08, "delete": 0x2E, "insert": 0x2D,
	"up": 0x26, "down": 0x28, "left": 0x25, "right": 0x27,
	"home": 0x24, "end": 0x23, "pageup": 0x21, "pagedown": 0x22,
	"f1": 0x70, "f2": 0x71, "f3": 0x72, "f4": 0x73, "f5": 0x74, "f6": 0x75,
	"f7": 0x76, "f8": 0x77, "f9": 0x78, "f10": 0x79, "f11": 0x7A, "f12": 0x7B,
}

// ParseKey turns a key name or a numeric virtual-key code into a code.
func ParseKey(s string) (int, error) {
	k := strings.ToLower(strings.TrimSpace(s))
	if code, ok := namedKeys[k]; ok {
		return code, nil
	}
	// "0x20" or "32", so any key can be reached without waiting for a name to
	// be added here.
	base, digits := 10, k
	if strings.HasPrefix(k, "0x") {
		base, digits = 16, k[2:]
	}
	code, err := strconv.ParseInt(digits, base, 32)
	if err != nil || code <= 0 || code > 0xFF {
		return 0, hverr.New(hverr.InvalidArgument,
			"%q is not a key name or a virtual-key code; try \"space\", \"enter\", \"f8\" or \"0x20\"", s)
	}
	return int(code), nil
}

// SendKeys types keys into a VM's synthetic keyboard.
//
// This exists for the one thing that cannot be done any other way: answering a
// prompt that appears before the guest has an operating system. Microsoft's
// installation media asks "Press any key to boot from CD or DVD" and gives up
// after a few seconds, so an unattended install from a stock ISO never starts
// unless something presses a key — and nothing in the guest is running yet to do
// it. PowerShell Direct, SSH and the VMBus file copy all need a booted guest.
//
// repeat and interval exist because the prompt is a race: the window opens a few
// seconds after power-on and closes again, so the usual approach is to press the
// key steadily across the period it might appear rather than try to time it.
//
// The keyboard is reached through WMI rather than the Hyper-V module, which has
// no cmdlet for it. That path needs administrator rights, which is exactly why it
// belongs on this side of the pipe.
func (c *Client) SendKeys(ctx context.Context, vmName string, keys []int, repeat int, interval time.Duration) (*KeyResult, error) {
	if vmName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "vm_name is required")
	}
	if len(keys) == 0 {
		return nil, hverr.New(hverr.InvalidArgument, "at least one key is required")
	}
	if repeat <= 0 {
		repeat = 1
	}
	if interval <= 0 {
		interval = time.Second
	}

	// Budget the whole run, plus room for the WMI lookups at either end.
	total := time.Duration(repeat)*interval + 60*time.Second

	const script = `
    # Msvm_ComputerSystem covers the host as well as its VMs. The host's Name is
    # the computer name while a VM's is its GUID, which distinguishes them
    # without depending on a localized Caption.
    $vm = @(Get-CimInstance -Namespace root\virtualization\v2 -ClassName Msvm_ComputerSystem |
            Where-Object { $_.ElementName -eq $P.name -and $_.Name -ne $env:COMPUTERNAME })[0]
    if (-not $vm) { throw "HVERR:VM_NOT_FOUND|no VM named '$($P.name)'" }

    # EnabledState 2 is Enabled, i.e. running. A keyboard on a stopped VM
    # accepts keys and discards them, which would look like success.
    if ([int]$vm.EnabledState -ne 2) {
        throw "HVERR:VM_WRONG_STATE|'$($P.name)' is not running; there is no keyboard to type on"
    }

    $kb = @(Get-CimAssociatedInstance -InputObject $vm -ResultClassName Msvm_Keyboard)[0]
    if (-not $kb) {
        throw "HVERR:INTERNAL|'$($P.name)' exposes no synthetic keyboard"
    }

    # A press is refused until the guest attaches to the keyboard, which happens
    # some way into firmware start-up — so early rejections are the normal case
    # when covering a boot prompt, not an error. Keep going and report both
    # counts; only a run where nothing at all landed is a failure.
    $sent = 0; $rejected = 0; $lastCode = 0
    for ($i = 0; $i -lt [int]$P.repeat; $i++) {
        foreach ($code in @($P.keys)) {
            $r = Invoke-CimMethod -InputObject $kb -MethodName TypeKey -Arguments @{ keyCode = [uint16]$code }
            if ([int]$r.ReturnValue -eq 0) { $sent++ }
            else { $rejected++; $lastCode = [int]$r.ReturnValue }
        }
        if ($i -lt [int]$P.repeat - 1) { Start-Sleep -Milliseconds ([int]$P.interval_ms) }
    }
    if ($sent -eq 0) {
        # 32775 is Hyper-V's "invalid state for this operation".
        $why = if ($lastCode -eq 32775) {
            "the VM never attached to its keyboard. It may have powered off, or been started only moments ago."
        } else { "TypeKey returned $lastCode every time." }
        throw "HVERR:VM_WRONG_STATE|'$($P.name)' accepted none of $rejected key press(es): $why"
    }

    $result = [ordered]@{
        vm_name  = [string]$P.name
        repeated = [int]$P.repeat
        sent     = [int]$sent
        rejected = [int]$rejected
    }`

	var out KeyResult
	err := c.r.RunTimeoutInto(ctx, total, script, map[string]any{
		"name":        vmName,
		"keys":        keys,
		"repeat":      repeat,
		"interval_ms": int(interval / time.Millisecond),
	}, &out)
	if err != nil {
		return nil, err
	}

	out.Keys = make([]string, 0, len(keys))
	for _, k := range keys {
		out.Keys = append(out.Keys, fmt.Sprintf("0x%02X", k))
	}
	return &out, nil
}
