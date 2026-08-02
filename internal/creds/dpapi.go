//go:build windows

package creds

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Credentials are encrypted with DPAPI in machine scope rather than user scope.
//
// The two ends of this are different accounts: the CLI that stores a credential
// runs as the logged-in user, while the service that reads it runs as
// LocalSystem. User-scope DPAPI would leave the service unable to decrypt what
// the CLI wrote.
//
// Machine scope means anything running as an administrator on this host can
// decrypt the file. That is already true of a service running as LocalSystem, so
// it widens nothing: the file's ACL is what keeps ordinary users out.
const dpapiFlags = windows.CRYPTPROTECT_LOCAL_MACHINE | windows.CRYPTPROTECT_UI_FORBIDDEN

func protect(plain []byte) ([]byte, error) {
	in := windows.DataBlob{Size: uint32(len(plain))}
	if len(plain) > 0 {
		in.Data = &plain[0]
	}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, dpapiFlags, &out); err != nil {
		return nil, fmt.Errorf("encrypt credentials: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

func unprotect(cipher []byte) ([]byte, error) {
	in := windows.DataBlob{Size: uint32(len(cipher))}
	if len(cipher) > 0 {
		in.Data = &cipher[0]
	}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, dpapiFlags, &out); err != nil {
		return nil, fmt.Errorf("decrypt credentials: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

// zero overwrites a secret buffer once it is no longer needed. It cannot undo
// copies the runtime made, but it shortens how long the value sits in memory.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
