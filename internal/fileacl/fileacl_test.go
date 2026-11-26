package fileacl

import (
	"strings"
	"testing"
)

func TestServiceSID(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		// Well-known virtual account, as printed by `sc showsid TrustedInstaller`.
		{"TrustedInstaller", "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"},
		// The SID ignores case.
		{"trustedinstaller", "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"},
		{ServiceName, "S-1-5-80-539124836-3259048300-2771617370-1812360420-3122952333"},
	} {
		if got := ServiceSID(tc.name); got != tc.want {
			t.Errorf("ServiceSID(%q) = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestSDDL(t *testing.T) {
	sid := ServiceSID(ServiceName)
	for name, sddl := range map[string]string{"dir": dirSDDL(sid), "config": configSDDL(sid)} {
		if !strings.HasPrefix(sddl, "D:P(") {
			t.Errorf("%s: %s is not a protected DACL", name, sddl)
		}
		for _, broad := range []string{";;;BU)", ";;;WD)", ";;;AU)", ";;;IU)"} {
			if strings.Contains(sddl, broad) {
				t.Errorf("%s: %s grants %s", name, sddl, broad)
			}
		}
	}
	if want := "(A;;FR;;;" + sid + ")"; !strings.Contains(configSDDL(sid), want) {
		t.Errorf("config DACL %s does not give the service read-only access %s", configSDDL(sid), want)
	}
}
