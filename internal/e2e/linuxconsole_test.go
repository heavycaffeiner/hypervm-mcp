//go:build windows

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The console tools claim to work whatever the guest is running, because they
// drive Hyper-V's own devices rather than anything inside the VM. That is a
// claim, and until now every test of it used a Windows guest — which is exactly
// the case where a Windows-specific assumption would go unnoticed.
//
// These run the same three tools against Linux. The guest here has no graphical
// desktop at all, only a text console, which suits the purpose: if a capture of
// a Linux text console comes back correct, nothing in the path depended on
// Windows.
//
//	$env:HYPERVM_E2E="C:\ProgramData\hypervm-mcp-dev\bin\hypervm-mcp-dev.exe"
//	$env:HYPERVM_E2E_ROCKY="1"
//	go test ./internal/e2e -run RockyConsole -v -count=1 `
//	  -ldflags "-X github.com/heavycaffeiner/hypervm-mcp/internal/config.instance=dev"

// TestRockyConsoleCapture photographs a Linux guest's console.
func TestRockyConsoleCapture(t *testing.T) {
	requireRocky(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	requireRunning(t, session, ctx, rockyVM)
	wakeConsole(t, session, ctx, rockyVM)

	dir := filepath.Join(rockyArtifact, "screens")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	shot := filepath.Join(dir, "rocky-console.png")

	meta := captureTo(t, session, ctx, rockyVM, shot)
	t.Logf("captured %dx%d (guest reports %dx%d), %d bytes of PNG",
		meta.Width, meta.Height, meta.GuestWidth, meta.GuestHeight, meta.PNG)

	if meta.GuestWidth == 0 {
		t.Error("the guest reported no resolution; send_vm_mouse cannot place a " +
			"coordinate without one")
	}
	if meta.Blank {
		t.Fatalf("the console came back blank: %s", meta.Note)
	}

	colours, dominant, share := colourSpread(t, mustRead(t, shot))
	t.Logf("%d distinct colours; %v covers %.2f%% of it", colours, dominant, share*100)
	if share >= 0.999 {
		t.Fatalf("%v covers %.3f%% of the capture, so nothing was drawn", dominant, share*100)
	}
	t.Log("the frame buffer path is not Windows-specific")
}

// TestRockyConsoleKeyboard proves the synthetic keyboard reaches a Linux guest.
//
// There is no way to read a text console back over SSH — it is a different
// terminal — so the evidence is the screen itself: pressing Enter at a login
// prompt makes it redraw, and the frame changes. That is a weaker claim than
// reading typed text back, and it is the strongest one available here.
func TestRockyConsoleKeyboard(t *testing.T) {
	requireRocky(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	requireRunning(t, session, ctx, rockyVM)
	wakeConsole(t, session, ctx, rockyVM)

	dir := filepath.Join(rockyArtifact, "screens")
	_ = os.MkdirAll(dir, 0o755)
	before := filepath.Join(dir, "rocky-console-before.png")
	after := filepath.Join(dir, "rocky-console-after.png")

	captureTo(t, session, ctx, rockyVM, before)

	// Enter at a login prompt: it redraws and scrolls, and logs nobody in.
	var keys map[string]any
	call(t, session, ctx, "send_vm_key", map[string]any{
		"vm_name": rockyVM, "keys": []string{"enter"}, "repeat": 3, "interval_ms": 500,
	}, &keys)
	t.Logf("keys accepted: %v", keys["sent"])
	time.Sleep(3 * time.Second)

	captureTo(t, session, ctx, rockyVM, after)

	changed, total := pixelsDiffer(t, mustRead(t, before), mustRead(t, after))
	t.Logf("%d of %d pixels changed (%.2f%%)", changed, total,
		100*float64(changed)/float64(total))
	if changed == 0 {
		t.Fatal("the console did not change, so the keystrokes did not reach this guest")
	}
	t.Log("the synthetic keyboard is not Windows-specific")
}

// TestRockyConsoleMouse finds out whether a pointer exists on this guest.
//
// It is not a given. The mouse is a device the guest has to bind, and a Linux
// system with no graphical stack has no reason to. Either answer is fine; what
// is not fine is an obscure failure, so this checks that a guest without one
// gets told so in terms that name the cause.
func TestRockyConsoleMouse(t *testing.T) {
	requireRocky(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	requireRunning(t, session, ctx, rockyVM)

	err := tryCall(t, session, ctx, "send_vm_mouse", map[string]any{
		"vm_name": rockyVM, "x": 100, "y": 100,
		"screen_width": 1024, "screen_height": 768,
	})
	if err == nil {
		t.Log("this Linux guest exposes a pointer and accepted the move")
		return
	}
	t.Logf("no pointer on this guest: %v", err)

	// The message has to say why, or the reader is left guessing at a VM that
	// looks healthy in every other way.
	if !strings.Contains(err.Error(), "synthetic mouse") &&
		!strings.Contains(err.Error(), "resolution") {
		t.Fatalf("the refusal does not explain itself: %v", err)
	}
	t.Log("refused with an explanation, which is the right outcome for a guest with no pointer")
}

// ---- helpers ---------------------------------------------------------------

type captureMeta struct {
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	GuestWidth  int    `json:"guest_width"`
	GuestHeight int    `json:"guest_height"`
	PNG         int    `json:"png_bytes"`
	Blank       bool   `json:"blank"`
	Note        string `json:"note"`
}

func captureTo(t *testing.T, s *mcp.ClientSession, ctx context.Context, vm, path string) captureMeta {
	t.Helper()
	var meta captureMeta
	res, err := s.CallTool(ctx, &mcp.CallToolParams{
		Name: "capture_vm_screen",
		Arguments: map[string]any{
			"vm_name": vm, "width": 1024, "height": 768, "output_path": path,
		},
	})
	if err != nil {
		t.Fatalf("capture_vm_screen: %v", err)
	}
	if res.IsError {
		t.Fatalf("capture_vm_screen: %s", contentText(res))
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return meta
}

// requireRunning skips rather than fails when the VM is off: these tests say
// nothing about a stopped VM, and the rest of the suite may have left it that way.
func requireRunning(t *testing.T, s *mcp.ClientSession, ctx context.Context, vm string) {
	t.Helper()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_vm", Arguments: map[string]any{"name": vm},
	})
	if err != nil {
		t.Fatalf("get_vm: %v", err)
	}
	if res.IsError {
		// A host with no Linux guest is a normal state for this suite, not a
		// failure of anything it tests.
		t.Skipf("%s: %s", vm, contentText(res))
	}
	var detail map[string]any
	decodeResult(t, res, &detail)
	if state, _ := detail["state"].(string); state != "Running" {
		t.Skipf("%s is %s; start it to run the console tests", vm, state)
	}
}

// wakeConsole clears a blanked display before anything is judged by what is on
// it. Linux blanks its console on a timer of its own, the same as Windows does.
func wakeConsole(t *testing.T, s *mcp.ClientSession, ctx context.Context, vm string) {
	t.Helper()
	// Shift types nothing, so it cannot disturb whatever the console is showing.
	call(t, s, ctx, "send_vm_key", map[string]any{
		"vm_name": vm, "keys": []string{"0x10"}, "repeat": 3, "interval_ms": 400,
	}, nil)
	time.Sleep(3 * time.Second)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// pixelsDiffer counts how many pixels changed between two captures.
func pixelsDiffer(t *testing.T, a, b []byte) (changed, total int) {
	t.Helper()
	ia, ib := decodePNG(t, a), decodePNG(t, b)
	if ia.Bounds() != ib.Bounds() {
		t.Fatalf("captures are different sizes: %v and %v", ia.Bounds(), ib.Bounds())
	}
	r := ia.Bounds()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if ia.At(x, y) != ib.At(x, y) {
				changed++
			}
			total++
		}
	}
	return changed, total
}

func decodePNG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode capture: %v", err)
	}
	return img
}
