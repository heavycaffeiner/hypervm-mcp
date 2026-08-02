//go:build windows

package winsec

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shell32          = windows.NewLazySystemDLL("shell32.dll")
	procShellExecute = shell32.NewProc("ShellExecuteExW")
)

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	swHide                = 0
)

// shellExecuteInfo mirrors SHELLEXECUTEINFOW. Field order and types must match
// the C struct exactly; Go's alignment rules produce the same 112-byte layout on
// amd64.
type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         windows.Handle
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     windows.Handle
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    windows.Handle
	dwHotKey     uint32
	hIconMonitor windows.Handle
	hProcess     windows.Handle
}

// RunElevated re-launches exePath with the "runas" verb, raising one UAC prompt,
// and waits for it to finish. It returns the child's exit code.
//
// The service installer uses this so that elevation is required exactly once, at
// install time, instead of on every MCP session.
func RunElevated(exePath string, args []string) (int, error) {
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return -1, err
	}
	file, err := windows.UTF16PtrFromString(exePath)
	if err != nil {
		return -1, err
	}
	params, err := windows.UTF16PtrFromString(quoteArgs(args))
	if err != nil {
		return -1, err
	}

	info := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync,
		lpVerb:       verb,
		lpFile:       file,
		lpParameters: params,
		nShow:        swHide,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))

	ret, _, callErr := procShellExecute.Call(uintptr(unsafe.Pointer(&info)))
	if ret == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && errno == windows.ERROR_CANCELLED {
			return -1, fmt.Errorf("elevation was declined")
		}
		return -1, fmt.Errorf("ShellExecuteEx failed: %w", callErr)
	}
	if info.hProcess == 0 {
		return -1, fmt.Errorf("elevated process started but no handle was returned")
	}
	defer windows.CloseHandle(info.hProcess)

	if _, err := windows.WaitForSingleObject(info.hProcess, windows.INFINITE); err != nil {
		return -1, fmt.Errorf("wait for elevated process: %w", err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.hProcess, &code); err != nil {
		return -1, fmt.Errorf("read elevated exit code: %w", err)
	}
	return int(code), nil
}

// quoteArgs joins args into a command line, quoting any that contain spaces.
// Arguments here are our own flags and a SID, so full CommandLineToArgvW
// escaping is not needed.
func quoteArgs(args []string) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		if strings.ContainsAny(a, " \t\"") {
			a = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}
