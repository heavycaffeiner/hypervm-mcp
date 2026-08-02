// Package psrun runs PowerShell scripts and returns their results as JSON.
//
// Every Hyper-V operation in this service goes through Runner.Run. Two rules make
// that safe and predictable:
//
//   - Arguments are never concatenated into script text. They are sent as JSON on
//     stdin and read back as the $P variable, so a VM name containing PowerShell
//     syntax is just a string, never code.
//   - Scripts are wrapped in an envelope that reports success or failure as a
//     structured field. PowerShell's exit code alone is not trustworthy: a
//     non-terminating error leaves it at 0.
package psrun

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
)

// envelope wraps a script so that success and failure are both reported on
// stdout as JSON, in UTF-8 regardless of the console code page.
//
// Scripts assign their output to $result. They must not use `return`, which
// would exit before the envelope is written.
const envelope = `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
# Hyper-V and .NET localize their messages to the host's display language.
# Forcing en-US keeps error text stable, so classification and the messages
# clients see do not depend on which Windows language pack is installed.
[System.Threading.Thread]::CurrentThread.CurrentUICulture = [System.Globalization.CultureInfo]::GetCultureInfo('en-US')
$__enc = [System.Text.UTF8Encoding]::new($false)
$__reader = [System.IO.StreamReader]::new([Console]::OpenStandardInput(), $__enc)
$__raw = $__reader.ReadToEnd()
$P = if ($__raw) { $__raw | ConvertFrom-Json } else { $null }
$result = $null
try {
%s
    $__envelope = @{ ok = $true; data = $result }
} catch {
    $__envelope = @{
        ok       = $false
        error    = $_.Exception.Message
        category = $_.CategoryInfo.Category.ToString()
        fqid     = [string]$_.FullyQualifiedErrorId
    }
}
$__json = $__envelope | ConvertTo-Json -Depth 8 -Compress
$__writer = [System.IO.StreamWriter]::new([Console]::OpenStandardOutput(), $__enc)
$__writer.Write($__json)
$__writer.Flush()
`

// result is the parsed envelope.
type result struct {
	OK       bool            `json:"ok"`
	Data     json.RawMessage `json:"data"`
	Error    string          `json:"error"`
	Category string          `json:"category"`
	FQID     string          `json:"fqid"`
}

// Runner executes scripts against a fixed PowerShell interpreter.
type Runner struct {
	exe     string
	timeout time.Duration
	sem     chan struct{} // caps concurrent interpreter processes
}

// New returns a Runner. maxConcurrent bounds how many powershell.exe processes
// may run at once; without a bound, several MCP sessions issuing bulk queries can
// spawn processes faster than they finish.
func New(exePath string, timeout time.Duration, maxConcurrent int) *Runner {
	if maxConcurrent <= 0 {
		maxConcurrent = 8
	}
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	return &Runner{
		exe:     exePath,
		timeout: timeout,
		sem:     make(chan struct{}, maxConcurrent),
	}
}

// Run executes script with the Runner's default timeout.
func (r *Runner) Run(ctx context.Context, script string, args any) (json.RawMessage, error) {
	return r.RunTimeout(ctx, r.timeout, script, args)
}

// RunTimeout executes script with an explicit timeout. Use it when the script
// itself waits on something (a guest shutdown, say) for longer than the default.
func (r *Runner) RunTimeout(ctx context.Context, timeout time.Duration, script string, args any) (json.RawMessage, error) {
	if timeout <= 0 {
		timeout = r.timeout
	}

	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, hverr.Wrap(hverr.OperationTimeout, ctx.Err(), "cancelled while waiting for a PowerShell slot")
	}

	var stdin []byte
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return nil, hverr.Wrap(hverr.Internal, err, "encode script arguments")
		}
		stdin = b
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, r.exe,
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encodeCommand(fmt.Sprintf(envelope, script)))
	cmd.Stdin = bytes.NewReader(stdin)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	// The timeout kills the process, so check it before blaming the output.
	if runCtx.Err() != nil && ctx.Err() == nil {
		return nil, hverr.New(hverr.OperationTimeout,
			"PowerShell did not finish within %s", timeout)
	}
	if ctx.Err() != nil {
		return nil, hverr.Wrap(hverr.OperationTimeout, ctx.Err(), "cancelled")
	}

	var res result
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res); err != nil {
		// No envelope means PowerShell failed before our script ran: a bad
		// interpreter path, a crash, or output we did not expect.
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = truncate(stdout.String(), 4096)
		}
		if runErr != nil {
			return nil, hverr.Wrap(hverr.PowerShellError, runErr,
				"PowerShell produced no usable output").WithDetail(truncate(detail, 4096))
		}
		return nil, hverr.Wrap(hverr.Internal, err,
			"could not parse the PowerShell result envelope").WithDetail(truncate(detail, 4096))
	}

	if !res.OK {
		return nil, classify(res.Error, res.Category, res.FQID)
	}
	return res.Data, nil
}

// RunInto executes script and unmarshals its data into out.
func (r *Runner) RunInto(ctx context.Context, script string, args any, out any) error {
	return r.RunTimeoutInto(ctx, r.timeout, script, args, out)
}

// RunTimeoutInto is RunTimeout followed by unmarshalling into out.
func (r *Runner) RunTimeoutInto(ctx context.Context, timeout time.Duration, script string, args any, out any) error {
	data, err := r.RunTimeout(ctx, timeout, script, args)
	if err != nil {
		return err
	}
	if len(data) == 0 || string(data) == "null" {
		return nil // leave out at its zero value; callers decide if that is an error
	}
	if err := json.Unmarshal(data, out); err != nil {
		return hverr.Wrap(hverr.Internal, err, "decode PowerShell result").
			WithDetail(truncate(string(data), 2048))
	}
	return nil
}

// encodeCommand renders script as base64 of UTF-16LE, the format -EncodedCommand
// expects. Passing scripts this way avoids two layers of quoting rules that
// -Command would otherwise apply.
func encodeCommand(script string) string {
	units := utf16.Encode([]rune(script))
	buf := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(buf[i*2:], u)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "... (truncated)"
}
