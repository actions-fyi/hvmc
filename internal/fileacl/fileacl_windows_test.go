//go:build windows

package fileacl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// daclOf returns the DACL of path in SDDL.
func daclOf(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s) = %v", path, err)
	}
	return sd.String()
}

// restoreAccess gives the current user full control of dir again when the
// test ends, so t.TempDir can remove it whatever token the test runs with.
func restoreAccess(t *testing.T, dir string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser() = %v", err)
	}
	t.Cleanup(func() {
		if err := setDACL(dir, "D:(A;OICI;FA;;;"+user.User.Sid.String()+")"); err != nil {
			t.Logf("restoring access to %s: %v", dir, err)
		}
	})
}

func TestSecureDirAndConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hvmc")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	restoreAccess(t, dir)
	// A file written before the directory is secured gets the new ACL too.
	early := filepath.Join(dir, "serving.pem")
	if err := os.WriteFile(early, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SecureDir(dir); err != nil {
		t.Fatalf("SecureDir() = %v", err)
	}
	config := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(config, []byte("role: host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SecureConfig(config); err != nil {
		t.Fatalf("SecureConfig() = %v", err)
	}
	late := filepath.Join(dir, "audit.log")
	if err := os.WriteFile(late, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	sid := ServiceSID(ServiceName)
	for _, tc := range []struct {
		path, prefix, service string
	}{
		{dir, "D:P", ";" + sid + ")"},
		// Inherited from the directory, whatever happened before.
		{early, "D:", "(A;ID;" + modifyAccess + ";;;" + sid + ")"},
		{late, "D:", "(A;ID;" + modifyAccess + ";;;" + sid + ")"},
		{config, "D:P", "(A;;FR;;;" + sid + ")"},
	} {
		checkDACL(t, tc.path, tc.prefix, tc.service)
	}
	if dacl := daclOf(t, config); strings.Contains(dacl, modifyAccess) || strings.Contains(dacl, "ID;") {
		t.Errorf("agent.yaml: DACL %s inherits the directory's entries", dacl)
	}
}

// checkDACL fails t unless the DACL of path starts with prefix, holds the
// service entry and grants nothing to broad groups.
func checkDACL(t *testing.T, path, prefix, service string) {
	t.Helper()
	dacl := daclOf(t, path)
	if !strings.HasPrefix(dacl, prefix) {
		t.Errorf("%s: DACL %s, want prefix %s", filepath.Base(path), dacl, prefix)
	}
	if !strings.Contains(dacl, service) {
		t.Errorf("%s: DACL %s lacks the service entry %s", filepath.Base(path), dacl, service)
	}
	for _, broad := range []string{";;;BU)", ";;;WD)", ";;;AU)", ";;;IU)"} {
		if strings.Contains(dacl, broad) {
			t.Errorf("%s: DACL %s grants %s", filepath.Base(path), dacl, broad)
		}
	}
}

func TestMkdirSecure(t *testing.T) {
	sid := ServiceSID(ServiceName)
	dir := filepath.Join(t.TempDir(), "parent", "hvmc")
	restoreAccess(t, dir)
	if err := MkdirSecure(dir); err != nil {
		t.Fatalf("MkdirSecure() = %v", err)
	}
	// Created with the protected DACL, not the parent's.
	checkDACL(t, dir, "D:P", "(A;OICI;"+modifyAccess+";;;"+sid+")")
	f := filepath.Join(dir, "serving.pem")
	if err := os.WriteFile(f, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkDACL(t, f, "D:", "(A;ID;"+modifyAccess+";;;"+sid+")")

	// An existing directory with the inherited ACL is secured.
	existing := filepath.Join(t.TempDir(), "hvmc")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	restoreAccess(t, existing)
	if err := MkdirSecure(existing); err != nil {
		t.Fatalf("MkdirSecure(existing) = %v", err)
	}
	checkDACL(t, existing, "D:P", ";"+sid+")")
}
