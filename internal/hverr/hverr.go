// Package hverr defines the stable error codes every MCP tool returns.
//
// Callers (including the model driving the MCP client) are expected to branch on
// Code, so codes must stay stable once shipped. Error() renders as
// "CODE: message (detail)" so the code survives being flattened into text.
package hverr

import (
	"errors"
	"fmt"
)

// Code identifies a failure class. Values match the spec proposal's error table.
type Code string

const (
	VMNotFound      Code = "VM_NOT_FOUND"
	VMAlreadyExists Code = "VM_ALREADY_EXISTS"
	VMWrongState    Code = "VM_WRONG_STATE"

	CheckpointNotFound Code = "CHECKPOINT_NOT_FOUND"
	AdapterNotFound    Code = "ADAPTER_NOT_FOUND"
	ParentVHDNotFound  Code = "PARENT_VHD_NOT_FOUND"
	VHDInUse           Code = "VHD_IN_USE"
	PathImmutable      Code = "PATH_IMMUTABLE"
	SwitchNotFound     Code = "SWITCH_NOT_FOUND"
	PathNotFound       Code = "PATH_NOT_FOUND"
	PathNotAccessible  Code = "PATH_NOT_ACCESSIBLE"

	CredentialNotFound      Code = "CREDENTIAL_NOT_FOUND"
	GuestAuthFailed         Code = "GUEST_AUTH_FAILED"
	GuestServiceUnavailable Code = "GUEST_SERVICE_UNAVAILABLE"
	GuestIPUnavailable      Code = "GUEST_IP_UNAVAILABLE"
	UnsupportedGuestOS      Code = "UNSUPPORTED_GUEST_OS"

	SSHAuthFailed      Code = "SSH_AUTH_FAILED"
	SSHHostKeyMismatch Code = "SSH_HOST_KEY_MISMATCH"
	SSHUnreachable     Code = "SSH_UNREACHABLE"

	TunnelNotFound       Code = "TUNNEL_NOT_FOUND"
	PortInUse            Code = "PORT_IN_USE"
	TailscaleUnavailable Code = "TAILSCALE_UNAVAILABLE"
	TailscaleNotRunning  Code = "TAILSCALE_NOT_RUNNING"
	FirewallError        Code = "FIREWALL_ERROR"

	OperationTimeout  Code = "OPERATION_TIMEOUT"
	HyperVUnavailable Code = "HYPERV_UNAVAILABLE"
	PowerShellError   Code = "POWERSHELL_ERROR"
	InvalidArgument   Code = "INVALID_ARGUMENT"
	AccessDenied      Code = "ACCESS_DENIED"
	Internal          Code = "INTERNAL"
)

// Error is a coded failure. Detail carries the raw underlying message so a
// failed classification never loses information.
type Error struct {
	Code    Code
	Message string
	Detail  string

	cause error
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// New builds a coded error with a formatted message.
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap builds a coded error that keeps cause reachable via errors.Is/As.
func Wrap(code Code, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), cause: cause}
}

// WithDetail attaches the raw underlying text. Returns e for chaining.
func (e *Error) WithDetail(detail string) *Error {
	e.Detail = detail
	return e
}

// CodeOf reports the code carried by err, or INTERNAL if it carries none.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return Internal
}

// Is reports whether err carries the given code.
func Is(err error, code Code) bool { return CodeOf(err) == code }
