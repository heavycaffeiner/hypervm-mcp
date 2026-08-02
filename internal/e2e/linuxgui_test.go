//go:build windows

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A text console proves the pointer was accepted, not that it arrived: there is
// nothing on a text console for a pointer to move. So this puts a graphical
// session on the Linux guest and checks what the guest actually receives.
//
// Two things about this platform shaped the approach.
//
// Rocky 10 ships no X server at all — RHEL 10 dropped it — so the desktop here
// is Wayland: cage, a kiosk compositor small enough to be reasonable on a guest
// that exists to be rebuilt, running one terminal.
//
// And Wayland deliberately refuses to tell a client where the pointer is, which
// is what xdotool would have asked. So the position is read from the kernel
// input device instead, through libinput. That is a better measurement anyway:
// it is what the guest received, before any display server had an opinion about
// it, and it does not change if the distribution changes its compositor.
//
//	$env:HYPERVM_E2E="C:\ProgramData\hypervm-mcp-dev\bin\hypervm-mcp-dev.exe"
//	$env:HYPERVM_E2E_ROCKY="1"
//	go test ./internal/e2e -run RockyGUI -v -count=1 -timeout 40m `
//	  -ldflags "-X github.com/heavycaffeiner/hypervm-mcp/internal/config.instance=dev"

// linuxDesktopScript runs the compositor with a terminal in it.
//
// cage takes the console, so this needs no display manager, no session manager
// and no window manager.
//
// The terminal's background is deliberately a strong colour. A capture of this
// session has to be distinguishable from a capture of the text console it
// replaced, and both are otherwise black — a test that cannot tell them apart
// passes whether or not the compositor ever started.
const linuxDesktopScript = `#!/bin/sh
exec /usr/bin/cage -d -- /usr/bin/xterm -bg blue4 -fg white -fa Monospace -fs 14
`

// TestRockyGUIMouse checks that a pointer sent to the console arrives in the
// guest at the coordinates it was aimed at.
func TestRockyGUIMouse(t *testing.T) {
	requireRocky(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	requireRunning(t, session, ctx, rockyVM)
	host := ensureLinuxDesktop(t, session, ctx)
	device := hyperVMouseDevice(t, session, ctx, host)
	t.Logf("the guest's Hyper-V mouse is %s", device)

	// The capture says what resolution the coordinates are scaled against.
	dir := filepath.Join(rockyArtifact, "screens")
	_ = os.MkdirAll(dir, 0o755)
	meta := captureTo(t, session, ctx, rockyVM, filepath.Join(dir, "rocky-desktop.png"))
	t.Logf("guest display is %dx%d", meta.GuestWidth, meta.GuestHeight)

	// Points spread across the screen, including corners: a test that only ever
	// aims at the middle passes even when the scale is wrong.
	targets := []struct{ x, y int }{{100, 100}, {900, 700}, {512, 384}, {950, 50}}

	rec := startInputRecorder(t, session, ctx, host, device, 40)
	// The recorder needs a moment after it reports itself started.
	time.Sleep(2 * time.Second)

	type expectation struct{ x, y int }
	var sent []expectation
	for _, target := range targets {
		// Park somewhere else first. Setting the pointer to where it already is
		// produces no motion event at all, so without this a rerun that repeats
		// a coordinate sees nothing and looks like a broken pointer.
		call(t, session, ctx, "send_vm_mouse", map[string]any{
			"vm_name": rockyVM, "x": 5, "y": 5,
			"screen_width": meta.Width, "screen_height": meta.Height,
		}, nil)
		time.Sleep(700 * time.Millisecond)

		var mouse map[string]any
		call(t, session, ctx, "send_vm_mouse", map[string]any{
			"vm_name": rockyVM, "x": target.x, "y": target.y,
			"screen_width": meta.Width, "screen_height": meta.Height,
		}, &mouse)
		sent = append(sent, expectation{toInt(mouse["x"]), toInt(mouse["y"])})
		time.Sleep(1200 * time.Millisecond)
	}
	log := rec(t)

	got := absoluteMotions(log)
	t.Logf("the guest reported %d absolute motions", len(got))
	if len(got) == 0 {
		t.Fatalf("the guest received no pointer motion at all:\n%.2000s", log)
	}

	// libinput reports an absolute position as a percentage of the screen, not
	// in pixels, so the comparison is done in its terms rather than converting
	// its numbers into pixels they never were.
	gw, gh := meta.GuestWidth, meta.GuestHeight
	if gw == 0 || gh == 0 {
		t.Fatal("the guest reported no resolution, so a percentage cannot be checked against pixels")
	}
	for i, want := range sent {
		wantX := 100 * float64(want.x) / float64(gw)
		wantY := 100 * float64(want.y) / float64(gh)
		// A fifth of a percent is about two pixels across this screen. Tight on
		// purpose: a loose tolerance would hide a wrong scale factor, which is
		// the failure worth catching.
		if !seenNear(got, wantX, wantY, 0.2) {
			t.Fatalf("target %d went to guest pixel (%d,%d) = %.2f%%/%.2f%%, "+
				"but the guest never saw it; it received %v", i+1, want.x, want.y, wantX, wantY, got)
		}
		t.Logf("guest received (%d,%d) as %.2f%%/%.2f%%, as sent", want.x, want.y, wantX, wantY)
	}
	t.Log("the synthetic pointer reaches a Linux guest and lands where it is aimed")
}

// TestRockyGUIClick checks that a button press arrives, not just a movement.
//
// Position and buttons are separate operations, so one working says nothing
// about the other. A press without a release would leave the button held down,
// which is why both are required.
func TestRockyGUIClick(t *testing.T) {
	requireRocky(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	requireRunning(t, session, ctx, rockyVM)
	host := ensureLinuxDesktop(t, session, ctx)
	device := hyperVMouseDevice(t, session, ctx, host)

	rec := startInputRecorder(t, session, ctx, host, device, 15)
	call(t, session, ctx, "send_vm_mouse", map[string]any{
		"vm_name": rockyVM, "x": 300, "y": 300,
		"screen_width": 1024, "screen_height": 768, "button": "left",
	}, nil)
	log := rec(t)

	if !strings.Contains(log, "pressed") {
		t.Fatalf("the guest saw no button press:\n%.2000s", log)
	}
	if !strings.Contains(log, "released") {
		t.Errorf("the guest saw a press but no release, which leaves the button held:\n%.2000s", log)
	}
	t.Log("the click arrives in the guest as a press and a release")
}

// TestRockyGUICapture confirms the capture shows the graphical session, not the
// text console it replaced.
func TestRockyGUICapture(t *testing.T) {
	requireRocky(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	requireRunning(t, session, ctx, rockyVM)
	ensureLinuxDesktop(t, session, ctx)

	dir := filepath.Join(rockyArtifact, "screens")
	_ = os.MkdirAll(dir, 0o755)
	shot := filepath.Join(dir, "rocky-desktop.png")

	meta := captureTo(t, session, ctx, rockyVM, shot)
	if meta.Blank {
		t.Fatalf("the console is blank with a compositor running: %s", meta.Note)
	}
	colours, dominant, share := colourSpread(t, mustRead(t, shot))
	t.Logf("%d distinct colours; %v covers %.2f%% of it", colours, dominant, share*100)

	// The terminal in this session has a strong blue background, and a Linux
	// text console is black. Checking for the blue is what makes this a test of
	// the compositor rather than of the console it was meant to replace: an
	// earlier version passed happily on a login prompt.
	r, g, b, _ := dominant.RGBA()
	r8, g8, b8 := uint8(r>>8), uint8(g>>8), uint8(b>>8)
	blue := b8 > 100 && r8 < 60 && g8 < 60
	if !blue || share < 0.5 {
		t.Fatalf("the largest area is %v at %.1f%%, not the terminal's blue background; "+
			"the compositor is probably not drawing and this is the text console (see %s)",
			dominant, share*100, shot)
	}
	t.Logf("the Wayland session is on screen and captures the same way a Windows desktop does (%s)", shot)
}

// ---- helpers ---------------------------------------------------------------

// ensureLinuxDesktop puts a minimal Wayland session on the guest if one is not
// already running, and returns the address to reach the guest at.
//
// Idempotent on purpose: the install costs minutes and the check costs a second.
func ensureLinuxDesktop(t *testing.T, s *mcp.ClientSession, ctx context.Context) string {
	t.Helper()

	privateKey, _ := sshKeyPair(t)
	storeCredentials(t, privateKey)

	host := os.Getenv(guestHostEnv)
	if host == "" {
		var ip map[string]any
		call(t, s, ctx, "wait_for_guest_ip",
			map[string]any{"name": rockyVM, "timeout_seconds": 300}, &ip)
		host, _ = ip["address"].(string)
	}
	if host == "" {
		t.Fatal("the guest reported no address")
	}

	// Compared exactly, not with a substring: systemctl answers "inactive" for a
	// unit that does not exist, and "inactive" contains "active".
	if out := sshRun(t, s, ctx, host,
		"systemctl is-active hypervm-desktop 2>/dev/null || true"); strings.TrimSpace(out) == "active" {
		t.Logf("the desktop session is already running on %s", host)
		return host
	}

	t.Log("installing a minimal Wayland session in the guest (cage plus one terminal)")
	sshRun(t, s, ctx, host, "sudo dnf install -y epel-release >/dev/null 2>&1 || true")
	// One package at a time: dnf abandons the whole transaction when any name in
	// a list is unavailable, which silently installs nothing.
	for _, pkg := range []string{"cage", "xterm", "xorg-x11-server-Xwayland", "libinput-utils"} {
		out := sshRun(t, s, ctx, host,
			fmt.Sprintf("sudo dnf install -y %s >/dev/null 2>&1 && echo ok || echo missing", pkg))
		t.Logf("  %-28s %s", pkg, strings.TrimSpace(out))
	}
	for _, need := range []string{"cage", "libinput"} {
		if out := sshRun(t, s, ctx, host,
			"command -v "+need+" >/dev/null && echo yes || echo no"); out != "yes" {
			t.Skipf("%s is unavailable in this guest's repositories, so a graphical "+
				"session cannot be measured here", need)
		}
	}

	sshRun(t, s, ctx, host, "sudo tee /usr/local/bin/hypervm-desktop.sh >/dev/null <<'HVEOF'\n"+
		linuxDesktopScript+"HVEOF\nsudo chmod +x /usr/local/bin/hypervm-desktop.sh")

	// getty owns tty1 by default and the console shows whichever VT is active,
	// so the compositor has to take that one rather than hide on another.
	sshRun(t, s, ctx, host, "sudo systemctl disable --now getty@tty1 2>&1 | tail -2 || true")

	// PAMName=login is what makes this work at all. wlroots opens the graphics
	// device through libseat, which needs a logind session attached to a seat;
	// a plain service has none, so the compositor exits immediately with
	// "No backend was able to open a seat". Opening a PAM login session for a
	// real user on tty1 gives it seat0, and logind then supplies
	// XDG_RUNTIME_DIR too.
	unit := `[Unit]
Description=hypervm-mcp minimal Wayland session for console tests
After=systemd-user-sessions.service

[Service]
Type=simple
User=` + rockyUser + `
PAMName=login
TTYPath=/dev/tty1
StandardInput=tty
StandardOutput=journal
StandardError=journal
TTYReset=yes
TTYVHangup=yes
ExecStart=/usr/local/bin/hypervm-desktop.sh
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
`
	sshRun(t, s, ctx, host, "sudo tee /etc/systemd/system/hypervm-desktop.service >/dev/null <<'HVEOF'\n"+
		unit+"HVEOF")
	sshRun(t, s, ctx, host,
		"sudo systemctl daemon-reload && sudo systemctl enable --now hypervm-desktop")

	deadline := time.Now().Add(3 * time.Minute)
	for {
		state := strings.TrimSpace(sshRun(t, s, ctx, host,
			"systemctl is-active hypervm-desktop 2>/dev/null || true"))
		if state == "active" {
			// Being active is not being drawn; give the compositor a moment.
			time.Sleep(5 * time.Second)
			t.Logf("the desktop session is running on %s", host)
			return host
		}
		if time.Now().After(deadline) {
			logs := sshRun(t, s, ctx, host,
				"systemctl status hypervm-desktop --no-pager -l 2>&1 | tail -20; "+
					"sudo journalctl -u hypervm-desktop --no-pager -n 40 2>&1 || true")
			t.Fatalf("the compositor never started (%s):\n%s", state, logs)
		}
		time.Sleep(5 * time.Second)
	}
}

// hyperVMouseDevice finds the event device the synthetic mouse arrives on.
//
// Found by the device's name rather than a fixed event number, which changes
// with what else the guest has attached.
func hyperVMouseDevice(t *testing.T, s *mcp.ClientSession, ctx context.Context, host string) string {
	t.Helper()
	out := sshRun(t, s, ctx, host,
		`awk '/^N: Name=.*[Mm]ouse/{f=1} f&&/^H: Handlers=/{for(i=1;i<=NF;i++) if($i ~ /^event/){print $i; exit}}' /proc/bus/input/devices`)
	dev := strings.TrimSpace(out)
	if dev == "" {
		t.Skipf("the guest lists no mouse input device, so it never bound the synthetic pointer")
	}
	return "/dev/input/" + dev
}

// startInputRecorder captures kernel input events in the background and returns
// a function that waits for it and yields what it saw.
func startInputRecorder(t *testing.T, s *mcp.ClientSession, ctx context.Context, host, device string, seconds int) func(*testing.T) string {
	t.Helper()
	const logPath = "/tmp/hypervm-input.log"
	sshRun(t, s, ctx, host, fmt.Sprintf(
		"sudo rm -f %s; sudo nohup sh -c 'timeout %d libinput debug-events --device %s > %s 2>&1' "+
			">/dev/null 2>&1 & sleep 2; echo started",
		logPath, seconds, device, logPath))

	started := time.Now()
	return func(t *testing.T) string {
		t.Helper()
		// Let the recorder reach its own timeout rather than racing it.
		if wait := time.Duration(seconds+2)*time.Second - time.Since(started); wait > 0 {
			time.Sleep(wait)
		}
		log := sshRun(t, s, ctx, host, "sudo cat "+logPath+" 2>/dev/null || true")
		sshRun(t, s, ctx, host, "sudo rm -f "+logPath)
		return log
	}
}

// absoluteMotionRE matches libinput's absolute pointer lines.
//
// The coordinates are a percentage of the screen, printed as "x/y" — right
// aligned, so there is whitespace around the slash, and preceded by an event
// number that is absent on the first line. Matching loosely up to the pair is
// what survives all three.
var absoluteMotionRE = regexp.MustCompile(
	`POINTER_MOTION_ABSOLUTE.*?([0-9]+\.[0-9]+)\s*/\s*([0-9]+\.[0-9]+)`)

// point is a position as libinput reports it: a percentage of the screen.
type point struct{ x, y float64 }

func (p point) String() string { return fmt.Sprintf("%.2f%%/%.2f%%", p.x, p.y) }

func absoluteMotions(log string) []point {
	var out []point
	for _, m := range absoluteMotionRE.FindAllStringSubmatch(log, -1) {
		x, errX := strconv.ParseFloat(m[1], 64)
		y, errY := strconv.ParseFloat(m[2], 64)
		if errX == nil && errY == nil {
			out = append(out, point{x, y})
		}
	}
	return out
}

// seenNear reports whether any observed position is within tolerance of one sent.
func seenNear(got []point, x, y, tolerance float64) bool {
	for _, p := range got {
		if absF(p.x-x) <= tolerance && absF(p.y-y) <= tolerance {
			return true
		}
	}
	return false
}

func absF(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	i, _ := strconv.Atoi(fmt.Sprintf("%v", v))
	return i
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
