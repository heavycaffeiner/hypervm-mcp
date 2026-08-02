//go:build windows

package psrun

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
)

func newTestRunner(t *testing.T) *Runner {
	t.Helper()
	return New(config.DefaultPowerShellPath(), 30*time.Second, 4)
}

func TestRunReturnsScalar(t *testing.T) {
	r := newTestRunner(t)
	var got string
	if err := r.RunInto(context.Background(), `$result = 'hello'`, nil, &got); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

// Arguments must reach the script as data. If they were pasted into the script
// text, this input would delete a file instead of coming back as a string.
func TestArgumentsAreNotExecuted(t *testing.T) {
	r := newTestRunner(t)
	hostile := `Dev-VM"; Remove-Item C:\nothing -Recurse; #`

	var got string
	err := r.RunInto(context.Background(), `$result = $P.name`,
		map[string]any{"name": hostile}, &got)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != hostile {
		t.Fatalf("argument was altered:\n got  %q\n want %q", got, hostile)
	}
}

// Output must be UTF-8 regardless of the console code page, which on this host
// is not Latin-1.
func TestNonASCIIRoundTrip(t *testing.T) {
	r := newTestRunner(t)
	const want = "가상머신 テスト ✓"

	var got string
	if err := r.RunInto(context.Background(), `$result = $P.text`,
		map[string]any{"text": want}, &got); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A one-element array must stay an array, or every listing tool would return an
// object when exactly one VM matched.
func TestSingleElementArrayStaysArray(t *testing.T) {
	r := newTestRunner(t)
	var got []int
	if err := r.RunInto(context.Background(), `$result = @(42)`, nil, &got); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(got) != 1 || got[0] != 42 {
		t.Fatalf("got %v, want [42]", got)
	}
}

func TestEmptyArray(t *testing.T) {
	r := newTestRunner(t)
	got := []string{"stale"}
	if err := r.RunInto(context.Background(), `$result = @()`, nil, &got); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

// A script that names its own code must win over message matching.
func TestScriptChosenErrorCode(t *testing.T) {
	r := newTestRunner(t)
	_, err := r.Run(context.Background(),
		`throw "HVERR:VM_WRONG_STATE|the VM is Off"`, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := hverr.CodeOf(err); got != hverr.VMWrongState {
		t.Fatalf("got code %s, want %s (%v)", got, hverr.VMWrongState, err)
	}
}

// A non-terminating error would leave the exit code at 0; the envelope must
// still report failure.
func TestNonTerminatingErrorIsReported(t *testing.T) {
	r := newTestRunner(t)
	_, err := r.Run(context.Background(),
		`$result = Get-Item 'C:\definitely\not\here\at\all.txt'`, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := hverr.CodeOf(err); got != hverr.PathNotFound {
		t.Fatalf("got code %s, want %s (%v)", got, hverr.PathNotFound, err)
	}
}

// Hyper-V ships localized messages, so the envelope pins the UI culture. Without
// this, error classification would depend on the host's display language.
func TestUICultureIsPinnedToEnglish(t *testing.T) {
	r := newTestRunner(t)
	var got string
	err := r.RunInto(context.Background(),
		`$result = [System.Threading.Thread]::CurrentThread.CurrentUICulture.Name`, nil, &got)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != "en-US" {
		t.Fatalf("UI culture is %q, want en-US", got)
	}
}

func TestTimeout(t *testing.T) {
	r := newTestRunner(t)
	_, err := r.RunTimeout(context.Background(), 1500*time.Millisecond,
		`Start-Sleep -Seconds 20`, nil)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if got := hverr.CodeOf(err); got != hverr.OperationTimeout {
		t.Fatalf("got code %s, want %s (%v)", got, hverr.OperationTimeout, err)
	}
}

// Arrays nested inside an object must stay arrays at every length.
//
// This pins down why projections build hashtables: Select-Object calculated
// properties flatten their expression's output, turning a one-element array into
// a scalar and an empty one into {}. Both decode wrongly, and the one-element
// case is the common one — a VM usually has exactly one IP address.
func TestArraysInsideObjectsRoundTrip(t *testing.T) {
	r := newTestRunner(t)
	var got struct {
		Empty []string `json:"empty"`
		One   []string `json:"one"`
		Many  []string `json:"many"`
	}
	err := r.RunInto(context.Background(), `
    $result = [ordered]@{
        empty = @()
        one   = @('a')
        many  = @('a','b')
    }`, nil, &got)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(got.Empty) != 0 {
		t.Errorf("empty = %v, want []", got.Empty)
	}
	if len(got.One) != 1 || got.One[0] != "a" {
		t.Errorf("one = %v, want [a]", got.One)
	}
	if len(got.Many) != 2 {
		t.Errorf("many = %v, want [a b]", got.Many)
	}
}

func TestNestedStructureSurvives(t *testing.T) {
	r := newTestRunner(t)
	var got map[string]json.RawMessage
	err := r.RunInto(context.Background(), `
    $result = [ordered]@{ name = 'a'; items = @(1,2,3); nested = @{ k = 'v' } }`, nil, &got)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, key := range []string{"name", "items", "nested"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing key %q in %v", key, got)
		}
	}
	if string(got["items"]) != "[1,2,3]" {
		t.Fatalf("items = %s, want [1,2,3]", got["items"])
	}
}
