//go:build windows

package fileacl

import (
	"fmt"

	"golang.org/x/sys/windows"
)

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
