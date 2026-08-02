package hyperv

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/winpath"
)

// ScreenCapture is one frame of a VM's console.
//
// Width and Height are the size of the picture; GuestWidth and GuestHeight are
// what the guest is actually running. They differ whenever the capture was
// scaled, and the difference matters: send_vm_mouse has to know which of the two
// a coordinate was measured against.
type ScreenCapture struct {
	VMName      string `json:"vm_name"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	GuestWidth  int    `json:"guest_width,omitempty"`
	GuestHeight int    `json:"guest_height,omitempty"`
	PNG         int    `json:"png_bytes"`
	Path        string `json:"path,omitempty"`

	// Blank says every pixel came back the same colour. Reported rather than
	// left for the reader to notice, because a blank frame is the one result
	// that looks like a working capture of a broken VM when it is usually
	// neither.
	Blank bool   `json:"blank,omitempty"`
	Note  string `json:"note,omitempty"`

	// png is the encoded image, returned to the caller as image content rather
	// than through the JSON result.
	png []byte
}

// Image returns the encoded PNG.
func (c *ScreenCapture) Image() []byte { return c.png }

// CaptureScreen photographs a VM's console.
//
// This is the only way to see a VM that cannot yet talk. It reads the frame
// buffer from the host side, so it needs no guest agent, no network, and no
// operating system: a firmware prompt, a boot menu, a stop error and an
// installer waiting on a dialog all look like what they are. Every other way
// into a guest — PowerShell Direct, SSH, the VMBus file copy — needs something
// running inside it to answer.
//
// What comes back is a thumbnail, not a recording. Hyper-V scales the console to
// the size asked for, so this is for reading a screen, not for comparing pixels;
// judge a GUI by its automation tree instead.
func (c *Client) CaptureScreen(ctx context.Context, vmName string, width, height int, outputPath string) (*ScreenCapture, error) {
	if vmName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "vm_name is required")
	}
	// Zero means "whatever the guest is running", resolved in the script below.
	// A fixed default cannot work: Hyper-V refuses a size far from the console's
	// own, so 1024x768 fails outright on anything in a text mode — which is
	// every Generation 1 VM at its firmware screen.
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	if width > 4096 || height > 4096 {
		return nil, hverr.New(hverr.InvalidArgument, "width and height must each be 4096 or less")
	}

	var dest string
	if outputPath != "" {
		var err error
		// Create, not Write: the file is new every time. Written by the service,
		// so the LocalSystem path rules apply.
		dest, err = winpath.Validate(outputPath, winpath.Create, true)
		if err != nil {
			return nil, err
		}
	}

	// The raw frame goes to a file rather than through the result: at two bytes
	// a pixel it is megabytes, and base64 in JSON would make it larger still.
	const script = `
    $vm = @(Get-CimInstance -Namespace root\virtualization\v2 -ClassName Msvm_ComputerSystem |
            Where-Object { $_.ElementName -eq $P.name -and $_.Name -ne $env:COMPUTERNAME })[0]
    if (-not $vm) { throw "HVERR:VM_NOT_FOUND|no VM named '$($P.name)'" }
    if ([int]$vm.EnabledState -ne 2) {
        throw "HVERR:VM_WRONG_STATE|'$($P.name)' is not running; a stopped VM has no console to photograph"
    }

    # What the console is actually running. Asked first because it is also the
    # only size the capture is guaranteed to accept.
    $gw = 0; $gh = 0
    $head = @(Get-CimAssociatedInstance -InputObject $vm -ResultClassName Msvm_VideoHead |
              Where-Object { $_.CurrentHorizontalResolution -gt 0 })[0]
    if ($head) {
        $gw = [int]$head.CurrentHorizontalResolution
        $gh = [int]$head.CurrentVerticalResolution
    }

    $w = [int]$P.width; $h = [int]$P.height
    if ($w -le 0 -or $h -le 0) {
        if ($gw -le 0) {
            throw "HVERR:VM_WRONG_STATE|'$($P.name)' reports no display resolution yet, so there is no size to capture at. It may have only just started."
        }
        $w = $gw; $h = $gh
    }

    $svc = Get-CimInstance -Namespace root\virtualization\v2 -ClassName Msvm_VirtualSystemManagementService
    $r = Invoke-CimMethod -InputObject $svc -MethodName GetVirtualSystemThumbnailImage -Arguments @{
        TargetSystem = [CimInstance]$vm
        WidthPixels  = [uint16]$w
        HeightPixels = [uint16]$h
    }
    if ([int]$r.ReturnValue -ne 0 -and $gw -gt 0 -and ($w -ne $gw -or $h -ne $gh)) {
        # A requested size Hyper-V will not scale to is the common cause, and
        # the size it will always accept is the one the console is using.
        $w = $gw; $h = $gh
        $r = Invoke-CimMethod -InputObject $svc -MethodName GetVirtualSystemThumbnailImage -Arguments @{
            TargetSystem = [CimInstance]$vm
            WidthPixels  = [uint16]$w
            HeightPixels = [uint16]$h
        }
    }
    if ([int]$r.ReturnValue -ne 0) {
        throw "HVERR:INTERNAL|Hyper-V refused to capture '$($P.name)' at ${w}x${h} (returned $($r.ReturnValue)); its console reports ${gw}x${gh}."
    }
    if (-not $r.ImageData) { throw "HVERR:INTERNAL|Hyper-V returned no image data" }

    $tmp = [System.IO.Path]::Combine($env:TEMP, 'hypervm-frame-' + [guid]::NewGuid().ToString('N') + '.bin')
    [System.IO.File]::WriteAllBytes($tmp, [byte[]]$r.ImageData)

    # What the guest is actually running, which the picture may not match.
    $gw = 0; $gh = 0
    $head = @(Get-CimAssociatedInstance -InputObject $vm -ResultClassName Msvm_VideoHead |
              Where-Object { $_.CurrentHorizontalResolution -gt 0 })[0]
    if ($head) {
        $gw = [int]$head.CurrentHorizontalResolution
        $gh = [int]$head.CurrentVerticalResolution
    }

    $result = [ordered]@{
        path         = $tmp
        bytes        = [int]([byte[]]$r.ImageData).Length
        width        = $w
        height       = $h
        guest_width  = $gw
        guest_height = $gh
    }`

	var raw struct {
		Path        string `json:"path"`
		Bytes       int    `json:"bytes"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		GuestWidth  int    `json:"guest_width"`
		GuestHeight int    `json:"guest_height"`
	}
	if err := c.r.RunTimeoutInto(ctx, 90*time.Second, script, map[string]any{
		"name": vmName, "width": width, "height": height,
	}, &raw); err != nil {
		return nil, err
	}
	defer os.Remove(raw.Path)

	frame, err := os.ReadFile(raw.Path)
	if err != nil {
		return nil, hverr.Wrap(hverr.Internal, err, "could not read the captured frame")
	}

	// The size actually captured, which is not the size asked for when the
	// request was zero or had to fall back to the console's own.
	width, height = raw.Width, raw.Height

	img, err := decodeRGB565(frame, width, height)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, hverr.Wrap(hverr.Internal, err, "could not encode the capture as PNG")
	}

	out := &ScreenCapture{
		VMName:      vmName,
		Width:       width,
		Height:      height,
		GuestWidth:  raw.GuestWidth,
		GuestHeight: raw.GuestHeight,
		PNG:         buf.Len(),
		png:         buf.Bytes(),
	}
	if share := dominantShare(frame, width*height*2); share >= blankThreshold {
		out.Blank = true
		out.Note = fmt.Sprintf(
			"One colour covers %.2f%% of this frame, so there is nothing to read in it. "+
				"A blanked display is the usual cause: send_vm_key with \"0x10\" (shift) "+
				"wakes it without typing anything, then capture again. A guest that has "+
				"not reached video output yet looks the same.", share*100)
	}

	if dest != "" {
		if err := os.WriteFile(dest, buf.Bytes(), 0o600); err != nil {
			return nil, hverr.Wrap(hverr.PathNotAccessible, err, "could not write %s", dest)
		}
		out.Path = dest
	}
	return out, nil
}

// blankThreshold is how much of a frame one colour has to cover before there is
// nothing worth looking at.
//
// Not 100%: a blanked console still comes back with a stray pixel or two, and
// requiring exact uniformity let a screen that was 99.9997% black pass as
// content. The gap between a blank frame and a real one is enormous — a desktop
// puts its commonest colour around a third of the picture — so anywhere in this
// region separates them.
const blankThreshold = 0.999

// dominantShare returns how much of the frame its commonest colour covers.
func dominantShare(frame []byte, n int) float64 {
	if n < 4 || len(frame) < n {
		return 0
	}
	// One bucket per possible RGB565 value: exact, and cheaper than a map.
	var counts [1 << 16]int32
	for i := 0; i < n; i += 2 {
		counts[uint16(frame[i])|uint16(frame[i+1])<<8]++
	}
	var best int32
	for _, c := range counts {
		if c > best {
			best = c
		}
	}
	return float64(best) / float64(n/2)
}

// decodeRGB565 turns Hyper-V's frame buffer into an image.
//
// The thumbnail arrives as 16 bits per pixel, five red, six green, five blue.
// Each channel is widened by repeating its high bits into the low ones, so full
// scale stays full scale — a plain shift left would cap white at 0xF8.
func decodeRGB565(frame []byte, width, height int) (image.Image, error) {
	want := width * height * 2
	if len(frame) < want {
		return nil, hverr.New(hverr.Internal,
			"the capture is %d bytes, short of the %d a %dx%d frame needs",
			len(frame), want, width, height)
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := (y*width + x) * 2
			v := uint16(frame[i]) | uint16(frame[i+1])<<8
			r := uint8(v>>11) & 0x1F
			g := uint8(v>>5) & 0x3F
			b := uint8(v) & 0x1F
			img.Set(x, y, color.RGBA{
				R: r<<3 | r>>2,
				G: g<<2 | g>>4,
				B: b<<3 | b>>2,
				A: 0xFF,
			})
		}
	}
	return img, nil
}
