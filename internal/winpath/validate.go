//go:build windows

// Package winpath validates paths from the perspective of the LocalSystem
// service, which is not the same as the user's perspective.
//
// The differences bite in practice: a mapped drive letter belongs to a logon
// session the service does not have, and a UNC path authenticates as the machine
// account rather than the user. Both fail in ways that look like a typo unless
// they are checked for explicitly.
package winpath

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
)

// Drive types returned by GetDriveType. x/sys/windows does not export these.
const (
	driveNoRootDir = 1
	driveRemote    = 4
)

// Mode says what the caller intends to do with the path.
type Mode int

const (
	// Read requires the path to exist and be readable.
	Read Mode = iota
	// Write requires the path to exist and be writable.
	Write
	// Create requires the parent directory to exist and be writable.
	Create
)

// Validate checks that a path is usable from the service and returns it cleaned.
//
// createParents applies to Create only, and makes missing parent directories.
func Validate(path string, mode Mode, createParents bool) (string, error) {
	if path == "" {
		return "", hverr.New(hverr.InvalidArgument, "path is required")
	}
	if !filepath.IsAbs(path) {
		// A relative path resolves against the service's working directory,
		// which is %SystemRoot%\System32 — never what the caller meant.
		return "", hverr.New(hverr.InvalidArgument,
			"%q must be an absolute path", path)
	}

	clean := filepath.Clean(path)

	if err := checkVolume(clean); err != nil {
		return "", err
	}

	switch mode {
	case Read:
		if _, err := os.Stat(clean); err != nil {
			return "", hverr.Wrap(hverr.PathNotFound, err, "%s is not readable by the service", clean)
		}
	case Write:
		if _, err := os.Stat(clean); err != nil {
			return "", hverr.Wrap(hverr.PathNotFound, err, "%s does not exist", clean)
		}
		if err := checkWritable(filepath.Dir(clean)); err != nil {
			return "", err
		}
	case Create:
		parent := filepath.Dir(clean)
		if _, err := os.Stat(parent); err != nil {
			if !createParents {
				return "", hverr.New(hverr.PathNotFound,
					"%s does not exist", parent).
					WithDetail("pass create_parents to create it")
			}
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return "", hverr.Wrap(hverr.PathNotAccessible, err, "could not create %s", parent)
			}
		}
		if err := checkWritable(parent); err != nil {
			return "", err
		}
	}

	return clean, nil
}

// ValidateDir is Validate for a directory the caller wants to write into.
func ValidateDir(path string, createParents bool) (string, error) {
	if path == "" {
		return "", hverr.New(hverr.InvalidArgument, "path is required")
	}
	if !filepath.IsAbs(path) {
		return "", hverr.New(hverr.InvalidArgument, "%q must be an absolute path", path)
	}
	clean := filepath.Clean(path)

	if err := checkVolume(clean); err != nil {
		return "", err
	}
	if _, err := os.Stat(clean); err != nil {
		if !createParents {
			return "", hverr.New(hverr.PathNotFound, "%s does not exist", clean).
				WithDetail("pass create_parents to create it")
		}
		if err := os.MkdirAll(clean, 0o755); err != nil {
			return "", hverr.Wrap(hverr.PathNotAccessible, err, "could not create %s", clean)
		}
	}
	if err := checkWritable(clean); err != nil {
		return "", err
	}
	return clean, nil
}

// checkVolume rejects the two path shapes that fail for reasons specific to
// running as a service, with an explanation of what to use instead.
func checkVolume(path string) error {
	if strings.HasPrefix(path, `\\`) {
		// Reaching a share at all proves the machine account has access; a
		// permission failure here is the whole point of checking.
		root := uncShareRoot(path)
		if _, err := os.Stat(root); err != nil {
			return hverr.Wrap(hverr.PathNotAccessible, err,
				"the service cannot reach %s", root).
				WithDetail("A service running as LocalSystem authenticates to network " +
					"shares as the computer account. Grant this machine's account " +
					"(HOSTNAME$) access to the share.")
		}
		return nil
	}

	if len(path) >= 2 && path[1] == ':' {
		root := path[:2] + `\`
		ptr, err := windows.UTF16PtrFromString(root)
		if err != nil {
			return hverr.Wrap(hverr.InvalidArgument, err, "invalid path %q", path)
		}
		switch windows.GetDriveType(ptr) {
		case driveRemote:
			return hverr.New(hverr.PathNotAccessible,
				"%s is a mapped network drive, which the service cannot see", root).
				WithDetail("Drive mappings belong to a logon session, and the service " +
					"has its own. Use the UNC path (\\\\server\\share\\...) instead.")
		case driveNoRootDir:
			return hverr.New(hverr.PathNotFound, "drive %s does not exist", root)
		}
	}
	return nil
}

// checkWritable proves write access rather than inferring it from an ACL, which
// is both simpler and accounts for read-only media and full volumes.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".hypervm-mcp-write-check-*")
	if err != nil {
		return hverr.Wrap(hverr.PathNotAccessible, err, "the service cannot write to %s", dir)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}

// uncShareRoot trims \\server\share\a\b down to \\server\share.
func uncShareRoot(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, `\\`), `\`)
	if len(parts) >= 2 {
		return `\\` + parts[0] + `\` + parts[1]
	}
	return path
}
