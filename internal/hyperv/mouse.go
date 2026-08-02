package hyperv

import (
	"context"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
)

// MouseResult reports where the pointer ended up, in the guest's own pixels
// rather than the caller's — which is what makes a misplaced click obvious.
type MouseResult struct {
	VMName       string `json:"vm_name"`
	X            int    `json:"x"`
	Y            int    `json:"y"`
	ScreenWidth  int    `json:"screen_width"`
	ScreenHeight int    `json:"screen_height"`
	Button       string `json:"button,omitempty"`
	Scroll       int    `json:"scroll,omitempty"`
	Moved        bool   `json:"moved"`
}

// mouseButtons maps names to the indexes Msvm_SyntheticMouse uses.
var mouseButtons = map[string]int{"left": 1, "right": 2, "middle": 3}

// The synthetic mouse takes positions in the guest's own pixels — the method's
// parameters are declared in units of pixels — not in a normalized range. A
// position outside the current resolution is rejected as an invalid parameter,
// so the caller's coordinates have to be scaled from the image they read them
// off to whatever the guest is actually running.

// SendMouse moves the pointer, and optionally clicks or scrolls.
//
// Positions are given as pixels together with the size of the image they were
// read from — normally a capture from CaptureScreen. They are converted to the
// fractions Hyper-V expects, so a click read off an 800x600 thumbnail lands in
// the right place on a 1920x1080 desktop.
//
// The synthetic mouse belongs to the console, so this drives whatever is on
// screen without asking the guest to cooperate. It does need the guest's
// integration services to have bound the device, which a Windows guest does
// during boot; before that, the pointer has nowhere to go.
func (c *Client) SendMouse(ctx context.Context, vmName string, x, y, screenWidth, screenHeight int, button string, scroll int) (*MouseResult, error) {
	if vmName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "vm_name is required")
	}
	if screenWidth <= 0 || screenHeight <= 0 {
		return nil, hverr.New(hverr.InvalidArgument,
			"screen_width and screen_height are required: they say what x and y are measured against")
	}
	if x < 0 || y < 0 || x >= screenWidth || y >= screenHeight {
		return nil, hverr.New(hverr.InvalidArgument,
			"(%d,%d) is outside a %dx%d screen", x, y, screenWidth, screenHeight)
	}
	buttonIndex := 0
	if button != "" {
		idx, ok := mouseButtons[button]
		if !ok {
			return nil, hverr.New(hverr.InvalidArgument,
				"button %q is not one of \"left\", \"right\" or \"middle\"", button)
		}
		buttonIndex = idx
	}

	const script = `
    $vm = @(Get-CimInstance -Namespace root\virtualization\v2 -ClassName Msvm_ComputerSystem |
            Where-Object { $_.ElementName -eq $P.name -and $_.Name -ne $env:COMPUTERNAME })[0]
    if (-not $vm) { throw "HVERR:VM_NOT_FOUND|no VM named '$($P.name)'" }
    if ([int]$vm.EnabledState -ne 2) {
        throw "HVERR:VM_WRONG_STATE|'$($P.name)' is not running; there is no pointer to move"
    }

    $mouse = @(Get-CimAssociatedInstance -InputObject $vm -ResultClassName Msvm_SyntheticMouse)[0]
    if (-not $mouse) {
        throw "HVERR:INTERNAL|'$($P.name)' exposes no synthetic mouse. The guest binds it through its integration services, so a guest still in firmware has none."
    }

    # Positions are in the guest's own pixels, so the caller's coordinates have
    # to be rescaled from the image they were read off to the live resolution.
    $head = @(Get-CimAssociatedInstance -InputObject $vm -ResultClassName Msvm_VideoHead |
              Where-Object { $_.CurrentHorizontalResolution -gt 0 })[0]
    if (-not $head) {
        throw "HVERR:INTERNAL|'$($P.name)' reports no display resolution, so a position cannot be placed on it"
    }
    $rw = [int]$head.CurrentHorizontalResolution
    $rh = [int]$head.CurrentVerticalResolution

    $px = [int][math]::Round(([double]$P.x / [double]$P.sw) * ($rw - 1))
    $py = [int][math]::Round(([double]$P.y / [double]$P.sh) * ($rh - 1))
    if ($px -lt 0) { $px = 0 } elseif ($px -gt $rw - 1) { $px = $rw - 1 }
    if ($py -lt 0) { $py = 0 } elseif ($py -gt $rh - 1) { $py = $rh - 1 }

    $r = Invoke-CimMethod -InputObject $mouse -MethodName SetAbsolutePosition -Arguments @{
        HorizontalPosition = [int]$px
        VerticalPosition   = [int]$py
    }
    if ([int]$r.ReturnValue -ne 0) {
        throw "HVERR:INTERNAL|the mouse refused the move to ($px,$py) on a ${rw}x${rh} screen (returned $($r.ReturnValue))"
    }

    if ([int]$P.button -gt 0) {
        # A click is delivered as one call so press and release cannot be split
        # by a timeout and leave the button stuck down.
        $r = Invoke-CimMethod -InputObject $mouse -MethodName ClickButton -Arguments @{
            ButtonIndex = [uint32]$P.button
        }
        if ([int]$r.ReturnValue -ne 0) {
            throw "HVERR:INTERNAL|the mouse refused the click (returned $($r.ReturnValue))"
        }
    }

    if ([int]$P.scroll -ne 0) {
        $r = Invoke-CimMethod -InputObject $mouse -MethodName SetScrollPosition -Arguments @{
            ScrollPositionDelta = [int]$P.scroll
        }
        if ([int]$r.ReturnValue -ne 0) {
            throw "HVERR:INTERNAL|the mouse refused the scroll (returned $($r.ReturnValue))"
        }
    }

    $result = [ordered]@{
        moved         = $true
        screen_width  = $rw
        screen_height = $rh
        guest_x       = $px
        guest_y       = $py
    }`

	var out struct {
		Moved  bool `json:"moved"`
		RW     int  `json:"screen_width"`
		RH     int  `json:"screen_height"`
		GuestX int  `json:"guest_x"`
		GuestY int  `json:"guest_y"`
	}
	if err := c.r.RunTimeoutInto(ctx, 60*time.Second, script, map[string]any{
		"name": vmName, "x": x, "y": y, "sw": screenWidth, "sh": screenHeight,
		"button": buttonIndex, "scroll": scroll,
	}, &out); err != nil {
		return nil, err
	}

	return &MouseResult{
		VMName: vmName, X: out.GuestX, Y: out.GuestY,
		ScreenWidth: out.RW, ScreenHeight: out.RH,
		Button: button, Scroll: scroll, Moved: out.Moved,
	}, nil
}
