package version

import "testing"

func TestString(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	for _, tc := range []struct{ in, want string }{
		{"v1.2.3", "v1.2.3"},
		{"", "dev"},
	} {
		Version = tc.in
		if got := String(); got != tc.want {
			t.Errorf("Version=%q: String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}
