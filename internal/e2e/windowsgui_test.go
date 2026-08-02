//go:build windows

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file covers the things a graphical program needs, which are exactly the
// things every other tool here cannot reach: the console's own frame buffer and
// pointer, and the interactive session a window is drawn in.
//
//	$env:HYPERVM_E2E="C:\ProgramData\hypervm-mcp-dev\bin\hypervm-mcp-dev.exe"
//	$env:HYPERVM_E2E_WINDOWS="1"
//	go test ./internal/e2e -run WindowsGUI -v -count=1 -timeout 30m `
//	  -ldflags "-X github.com/heavycaffeiner/hypervm-mcp/internal/config.instance=dev"

// TestWindowsGUISessionIsNotSessionZero states the problem the rest of this file
// exists to solve, by measuring it.
//
// The same query answered two ways must disagree: over PowerShell Direct it runs
// in session 0, which has no desktop, and through the session bridge it runs
// where the user is logged on. If these ever agree, one of the two paths has
// stopped doing what it claims and every GUI result built on it is worthless.
func TestWindowsGUISessionIsNotSessionZero(t *testing.T) {
	requireWindowsGuest(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	const probe = `(Get-Process -Id $PID).SessionId`

	direct := strings.TrimSpace(winExec(t, session, ctx, probe, 120))
	t.Logf("guest_invoke_command runs in session %s", direct)
	if direct != "0" {
		t.Fatalf("PowerShell Direct reported session %q; this suite's premise is that it is 0", direct)
	}

	var res map[string]any
	call(t, session, ctx, "guest_run_in_session", map[string]any{
		"vm_name": winVMName, "command": probe, "timeout_seconds": 180,
	}, &res)
	t.Logf("guest_run_in_session ran as %v in session %v", res["logged_on_as"], res["session_id"])

	interactive := strings.TrimSpace(fmt.Sprintf("%v", res["stdout"]))
	if interactive == "0" || interactive == "" {
		t.Fatalf("the session bridge reported %q; it is meant to land outside session 0", interactive)
	}
	if interactive == direct {
		t.Fatalf("both paths reported session %s, so the bridge is not bridging anything", direct)
	}
}

// TestWindowsGUIElevatedInSession checks the other half of what the bridge
// promises: not just a desktop, but an unfiltered administrator token.
//
// A filtered token is the quiet failure here. It looks like a member of the
// Administrators group right up until something actually needs the privilege.
func TestWindowsGUIElevatedInSession(t *testing.T) {
	requireWindowsGuest(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Reading the token's elevation type is the direct question; writing to a
	// machine-wide key is the one that matters in practice, so ask both.
	const probe = `
$id = [Security.Principal.WindowsIdentity]::GetCurrent()
'elevated=' + ([Security.Principal.WindowsPrincipal]$id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
$k = 'HKLM:\SOFTWARE\hypervm-mcp-elevation-probe'
New-Item -Path $k -Force | Out-Null
Set-ItemProperty -Path $k -Name probe -Value 'ok'
'wrote=' + (Get-ItemProperty -Path $k).probe
Remove-Item -Path $k -Recurse -Force
`
	var res map[string]any
	call(t, session, ctx, "guest_run_in_session", map[string]any{
		"vm_name": winVMName, "command": probe, "timeout_seconds": 180,
	}, &res)

	out := fmt.Sprintf("%v", res["stdout"])
	t.Logf("session bridge reported: %s", strings.TrimSpace(out))
	if !strings.Contains(out, "elevated=True") {
		t.Fatalf("the task did not get an administrator token: %s", out)
	}
	if !strings.Contains(out, "wrote=ok") {
		t.Fatalf("the task could not write under HKLM, so its token is filtered: %s", out)
	}
}

// TestWindowsGUICaptureScreen photographs the console.
//
// The check that matters is not that bytes came back but that they are a picture
// of something: a blank capture is what session 0 produces and what a sleeping
// display produces, and both would otherwise pass silently.
func TestWindowsGUICaptureScreen(t *testing.T) {
	requireWindowsGuest(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	keepDisplayAwake(t, session, ctx)

	dir := filepath.Join(rockyArtifact, "screens")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	shot := filepath.Join(dir, "win2022-desktop.png")

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "capture_vm_screen",
		Arguments: map[string]any{
			"vm_name": winVMName, "width": 1024, "height": 768, "output_path": shot,
		},
	})
	if err != nil {
		t.Fatalf("capture_vm_screen: %v", err)
	}
	if res.IsError {
		t.Fatalf("capture_vm_screen: %s", contentText(res))
	}

	var meta struct {
		Width  int    `json:"width"`
		Height int    `json:"height"`
		PNG    int    `json:"png_bytes"`
		Path   string `json:"path"`
		Blank  bool   `json:"blank"`
		Note   string `json:"note"`
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	t.Logf("captured %dx%d, %d bytes of PNG, saved to %s",
		meta.Width, meta.Height, meta.PNG, meta.Path)

	// The image also comes back inline, which is what lets a client look at it
	// rather than open a file.
	var inline []byte
	for _, c := range res.Content {
		if img, ok := c.(*mcp.ImageContent); ok {
			if img.MIMEType != "image/png" {
				t.Fatalf("inline image is %q, want image/png", img.MIMEType)
			}
			inline = img.Data
		}
	}
	if len(inline) == 0 {
		t.Fatal("no image content came back, so a client would have nothing to look at")
	}

	onDisk, err := os.ReadFile(shot)
	if err != nil {
		t.Fatalf("read %s: %v", shot, err)
	}
	// The SDK carries image data base64-encoded on the wire; accept either form
	// so this does not depend on where the decoding happens.
	if decoded, err := base64.StdEncoding.DecodeString(string(inline)); err == nil {
		inline = decoded
	}
	if len(inline) != len(onDisk) {
		t.Fatalf("the inline image is %d bytes but the file is %d; they should be the same picture",
			len(inline), len(onDisk))
	}
	if string(onDisk[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatal("the saved file is not a PNG")
	}

	// A blank capture is the failure that matters, because it is what a sleeping
	// display and a session-0 capture both produce and neither says so. Judge it
	// by what is in the image, not by how well it compressed.
	colours, dominant, share := colourSpread(t, onDisk)
	t.Logf("the capture holds %d distinct colours; %v covers %.3f%% of it",
		colours, dominant, share*100)
	// Judged by coverage, not by the number of colours: a blanked console still
	// returns a stray pixel or two, so counting distinct colours calls a screen
	// that is 99.9997% black "content".
	if share >= 0.999 {
		t.Fatalf("%v covers %.3f%% of the capture, so the guest is showing nothing. "+
			"A slept display does this; so does capturing from a session with no desktop.",
			dominant, share*100)
	}

	// The server reaches the same verdict and says so, which is what a client
	// that cannot look at the picture has to rely on.
	if meta.Blank {
		t.Fatalf("the server reported the frame blank: %s", meta.Note)
	}
}

// colourSpread reports how varied a capture is, and what covers most of it.
func colourSpread(t *testing.T, data []byte) (distinct int, dominant color.Color, share float64) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode the capture: %v", err)
	}
	b := img.Bounds()
	counts := map[color.RGBA]int{}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			counts[color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), 0xFF}]++
		}
	}
	total := b.Dx() * b.Dy()
	var best color.RGBA
	var bestN int
	for c, n := range counts {
		if n > bestN {
			best, bestN = c, n
		}
	}
	return len(counts), best, float64(bestN) / float64(total)
}

// keepDisplayAwake stops the guest blanking its screen.
//
// A slept display captures as black and takes no clicks, which looks exactly
// like a broken tool. The answer file arranges this for a guest built here, but
// a guest built earlier — or one left running long enough — still needs it, so
// the tests do not depend on how this one came to exist.
func keepDisplayAwake(t *testing.T, s *mcp.ClientSession, ctx context.Context) {
	t.Helper()

	// Machine-wide settings; session 0 is fine for these.
	winExec(t, s, ctx, `
        powercfg /change monitor-timeout-ac 0
        powercfg /change standby-timeout-ac 0
        'power settings applied'`, 180)

	// The screen saver lives in the user's own hive, so it has to be set in the
	// session that owns it.
	var res map[string]any
	if err := tryCall(t, s, ctx, "guest_run_in_session", map[string]any{
		"vm_name": winVMName, "timeout_seconds": 180,
		"command": `
            Set-ItemProperty 'HKCU:\Control Panel\Desktop' -Name ScreenSaveActive -Value '0'
            Set-ItemProperty 'HKCU:\Control Panel\Desktop' -Name ScreenSaveTimeOut -Value '0'
            'screen saver disabled'`,
	}); err != nil {
		t.Logf("could not disable the screen saver: %v", err)
	}
	_ = res

	// Input is what actually wakes a display that has already blanked. Shift is
	// the conventional choice: it types nothing.
	call(t, s, ctx, "send_vm_key", map[string]any{
		"vm_name": winVMName, "keys": []string{"0x10"}, "repeat": 3, "interval_ms": 400,
	}, nil)
	// Waking is not instant, and capturing during the fade gives a dim frame.
	time.Sleep(3 * time.Second)
}

// TestWindowsGUIMouseAndCapture drives the pointer and reads the result off the
// screen, which is the whole loop a GUI test runs in.
//
// It opens Notepad in the interactive session, types into it through the console
// keyboard, and confirms from inside the guest that the text arrived. That last
// step is deliberate: judging it from the picture would be judging pixels, and
// pixels move with resolution, theme and DPI.
func TestWindowsGUIMouseAndCapture(t *testing.T) {
	requireWindowsGuest(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	defer func() {
		bg := context.Background()
		_ = tryCall(t, session, bg, "guest_invoke_command", map[string]any{
			"vm_name":         winVMName,
			"command":         `Get-Process notepad -ErrorAction SilentlyContinue | Stop-Process -Force; 'cleaned'`,
			"username":        winAdmin,
			"password":        winPassword,
			"timeout_seconds": 120,
		})
	}()

	// Start from none. A Notepad left behind by an interrupted run already holds
	// the text this asserts on, which would pass without anything being typed.
	winExec(t, session, ctx,
		`Get-Process notepad -ErrorAction SilentlyContinue | Stop-Process -Force; 'cleared'`, 120)

	// Start Notepad on the desktop and return immediately: waiting for it would
	// wait for the window to be closed.
	var res map[string]any
	call(t, session, ctx, "guest_run_in_session", map[string]any{
		"vm_name":         winVMName,
		"command":         `Start-Process notepad; Start-Sleep -Seconds 3; 'started'`,
		"timeout_seconds": 180,
	}, &res)
	t.Logf("notepad start reported: %s", strings.TrimSpace(fmt.Sprintf("%v", res["stdout"])))

	waitForGuestProcess(t, session, ctx, "notepad", 2*time.Minute)

	// Click the middle of the screen to put focus in the text area, then type.
	// Both go through the console, so neither needs cooperation from the guest.
	var mouse map[string]any
	call(t, session, ctx, "send_vm_mouse", map[string]any{
		"vm_name": winVMName, "x": 512, "y": 384,
		"screen_width": 1024, "screen_height": 768, "button": "left",
	}, &mouse)
	t.Logf("clicked at guest pixel (%v,%v) on a %vx%v screen",
		mouse["x"], mouse["y"], mouse["screen_width"], mouse["screen_height"])

	// Focus follows the click through a message queue, not instantly.
	time.Sleep(2 * time.Second)

	// "HI" — two keys is enough to prove the path, and letter keys need no
	// modifier handling.
	var keys map[string]any
	call(t, session, ctx, "send_vm_key", map[string]any{
		"vm_name": winVMName, "keys": []string{"0x48", "0x49"},
		"repeat": 1, "interval_ms": 200,
	}, &keys)
	t.Logf("keys accepted: %v", keys["sent"])
	time.Sleep(2 * time.Second)

	// Read the text out of the control itself rather than off the picture.
	// Which element holds the text, and which pattern exposes it, varies by
	// build: this one presents Notepad's text area as a Pane carrying the
	// content in its Name, where another gives a Document with a TextPattern.
	// Pinning either would make the test a statement about one Windows build,
	// so gather text from the whole window however each element offers it.
	const readback = `
Add-Type -AssemblyName UIAutomationClient, UIAutomationTypes
$A = [System.Windows.Automation.AutomationElement]
$cond = New-Object System.Windows.Automation.PropertyCondition($A::ClassNameProperty, 'Notepad')
$win = $A::RootElement.FindFirst([System.Windows.Automation.TreeScope]::Descendants, $cond)
if (-not $win) { 'NOWINDOW'; exit 1 }

$all = $win.FindAll([System.Windows.Automation.TreeScope]::Descendants,
    [System.Windows.Automation.Condition]::TrueCondition)

$found = @()
foreach ($e in $all) {
    try {
        $p = $e.GetCurrentPattern([System.Windows.Automation.TextPattern]::Pattern)
        $found += $p.DocumentRange.GetText(-1)
        continue
    } catch {}
    try {
        $p = $e.GetCurrentPattern([System.Windows.Automation.ValuePattern]::Pattern)
        $found += $p.Current.Value
        continue
    } catch {}
    $found += $e.Current.Name
}
'TREE:'
foreach ($e in $all) { '  ' + $e.Current.ControlType.ProgrammaticName + ' "' + $e.Current.Name + '"' }
'TEXT:' + (($found -join '') -replace '\s', '')
`
	var read map[string]any
	call(t, session, ctx, "guest_run_in_session", map[string]any{
		"vm_name": winVMName, "command": readback, "timeout_seconds": 180,
	}, &read)
	got := fmt.Sprintf("%v", read["stdout"])
	t.Logf("automation tree reported: %s", strings.TrimSpace(got))

	// Capture either way: on success it is a record, on failure it is the only
	// thing that says what the guest was actually showing.
	shot := filepath.Join(rockyArtifact, "screens", "win2022-notepad.png")
	_ = tryCall(t, session, ctx, "capture_vm_screen", map[string]any{
		"vm_name": winVMName, "width": 1024, "height": 768, "output_path": shot,
	})
	t.Logf("screen saved to %s", shot)

	if !strings.Contains(strings.ToUpper(got), "TEXT:HI") {
		t.Fatalf("the keystrokes did not reach the window; automation read %q", strings.TrimSpace(got))
	}
	t.Log("console keyboard and pointer reached a window on the desktop")
}

// waitForGuestProcess polls until a named process is running in the guest.
func waitForGuestProcess(t *testing.T, s *mcp.ClientSession, ctx context.Context, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	cmd := fmt.Sprintf(`@(Get-Process %s -ErrorAction SilentlyContinue).Count`, name)
	for {
		if out := strings.TrimSpace(winExec(t, s, ctx, cmd, 60)); out != "" && out != "0" {
			t.Logf("%s is running (%s instance(s))", name, out)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared in the guest within %s", name, timeout)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("cancelled while waiting for %s", name)
		case <-time.After(5 * time.Second):
		}
	}
}
