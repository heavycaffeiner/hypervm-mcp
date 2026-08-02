package psrun

import (
	"strings"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
)

// throwPrefix lets a script pick its own error code:
//
//	throw "HVERR:OPERATION_TIMEOUT|the VM never reached the Off state"
//
// Use it wherever the script itself detects the failure, so the outcome does not
// depend on matching English text from Hyper-V.
const throwPrefix = "HVERR:"

// notElevatedHint explains the failure the service is built to prevent. Seeing
// it means PowerShell ran without Hyper-V rights, so the caller is not the
// LocalSystem service.
const notElevatedHint = "Hyper-V refused the caller. The service must run as LocalSystem; " +
	"check `hypervm-mcp service status`."

// fcopyDeviceHint explains a failure that looks like a misconfiguration but is
// not one. Linux 6.10 removed the /dev/vmbus/hv_fcopy device the classic
// hypervfcopyd attaches to, so on a recent kernel the daemon reports itself as
// running while Copy-VMFile has nothing to talk to.
const fcopyDeviceHint = "The guest could not accept the file. Linux 6.10 and later no longer " +
	"provide the /dev/vmbus/hv_fcopy device that hypervfcopyd attaches to, so Copy-VMFile cannot " +
	"reach guests on those kernels even though the daemon reports itself active — move files with " +
	"ssh_exec or through a tunnel instead. On Windows guests, check that the VM has finished " +
	"booting and that Integration Services are up to date."

// rule maps a PowerShell error to a code, by a substring of its message or of
// its fully qualified error id. Order matters: the first match wins, so specific
// patterns come before general ones. hint, if set, replaces the raw error id in
// the error's Detail.
//
// Prefer fqid where it identifies the failing cmdlet: unlike the message, it is
// not localized, and unlike an HRESULT it does not change between causes of the
// same underlying problem.
type rule struct {
	needle string
	fqid   string
	code   hverr.Code
	hint   string
}

var rules = []rule{
	{needle: "unable to find a virtual machine with name", code: hverr.VMNotFound},
	{needle: "unable to find virtual machine with name", code: hverr.VMNotFound},
	{needle: "unable to find a virtual switch with name", code: hverr.SwitchNotFound},
	{needle: "unable to find a snapshot matching", code: hverr.CheckpointNotFound},
	{needle: "unable to find the virtual machine snapshot", code: hverr.CheckpointNotFound},
	{needle: "already exists", code: hverr.VMAlreadyExists},
	{needle: "current state", code: hverr.VMWrongState},
	{needle: "is not in a valid state", code: hverr.VMWrongState},
	{needle: "is not recognized as the name of a cmdlet", code: hverr.HyperVUnavailable},
	{needle: "hyper-v is not installed", code: hverr.HyperVUnavailable},
	{needle: "the virtual machine management service", code: hverr.HyperVUnavailable},
	{needle: "failed to connect to the virtual machine", code: hverr.GuestServiceUnavailable},
	// Every way a guest file copy can fail lands here, and they all have the
	// same handful of causes; the HRESULT varies between them, so match the
	// cmdlet instead.
	{fqid: "CopyVMFile", code: hverr.GuestServiceUnavailable, hint: fcopyDeviceHint},
	{needle: "the credentials supplied", code: hverr.GuestAuthFailed},
	{needle: "logon failure", code: hverr.GuestAuthFailed},
	{needle: "you do not have the required permission", code: hverr.AccessDenied, hint: notElevatedHint},
	{needle: "authorization policy", code: hverr.AccessDenied, hint: notElevatedHint},
	{needle: "access is denied", code: hverr.AccessDenied},
	{needle: "access denied", code: hverr.AccessDenied},
	{needle: "could not find file", code: hverr.PathNotFound},
	{needle: "could not find a part of the path", code: hverr.PathNotFound},
	{needle: "cannot find path", code: hverr.PathNotFound},
	{needle: "timed out", code: hverr.OperationTimeout},
}

// classify turns a PowerShell failure into a coded error, preserving the
// original message as Detail so an unmatched pattern still tells the caller what
// actually went wrong.
func classify(message, category, fqid string) error {
	message = strings.TrimSpace(message)

	// A script that named its own code wins outright.
	if strings.HasPrefix(message, throwPrefix) {
		body := strings.TrimPrefix(message, throwPrefix)
		code, text, found := strings.Cut(body, "|")
		if found && code != "" {
			return hverr.New(hverr.Code(code), "%s", text)
		}
	}

	lower := strings.ToLower(message)
	lowerFQID := strings.ToLower(fqid)
	for _, r := range rules {
		matched := r.needle != "" && strings.Contains(lower, r.needle)
		if !matched && r.fqid != "" {
			matched = strings.Contains(lowerFQID, strings.ToLower(r.fqid))
		}
		if matched {
			// Keep the error id when there is nothing more useful to say; it is
			// the only locale-independent handle on the failure.
			detail := r.hint
			if detail == "" {
				detail = fqid
			}
			return hverr.New(r.code, "%s", message).WithDetail(detail)
		}
	}

	switch category {
	case "InvalidArgument", "InvalidData", "InvalidType":
		return hverr.New(hverr.InvalidArgument, "%s", message)
	case "ObjectNotFound":
		return hverr.New(hverr.PathNotFound, "%s", message)
	case "PermissionDenied", "SecurityError":
		return hverr.New(hverr.AccessDenied, "%s", message)
	case "InvalidOperation":
		return hverr.New(hverr.VMWrongState, "%s", message)
	}

	return hverr.New(hverr.PowerShellError, "%s", message).WithDetail(fqid)
}
