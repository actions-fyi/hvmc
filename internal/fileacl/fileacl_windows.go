//go:build windows

package fileacl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// MkdirSecure creates the state directory at path with the protected DACL
// of [SecureDir] in the same call (CreateDirectory with a security
// descriptor), so it never exists, even briefly, with the ACL inherited
// from its parent. Missing parents are created with their inherited ACL;
// they hold nothing. An existing directory gets the DACL as by SecureDir.
func MkdirSecure(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(dirSDDL(ServiceSID(ServiceName)))
	if err != nil {
		return fmt.Errorf("parse security descriptor: %w", err)
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	err = windows.CreateDirectory(p, sa)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, windows.ERROR_ALREADY_EXISTS):
		return SecureDir(path)
	default:
		return fmt.Errorf("create %s: %w", path, err)
	}
}

// SecureDir sets the protected DACL of the state directory on path. The
// inheritable entries propagate to what the directory already holds, so
// re-running enrollment also fixes files written before.
func SecureDir(path string) error {
	return setDACL(path, dirSDDL(ServiceSID(ServiceName)))
}

// SecureConfig sets the protected DACL of agent.yaml on path. Writers call
// it on the temporary file before renaming it into place, so agent.yaml is
// never writable by the service.
func SecureConfig(path string) error {
	return setDACL(path, configSDDL(ServiceSID(ServiceName)))
}

func setDACL(path, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return fmt.Errorf("parse security descriptor: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("security descriptor DACL: %w", err)
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil)
	if err != nil {
		return fmt.Errorf("set ACL of %s: %w", path, err)
	}
	return nil
}
