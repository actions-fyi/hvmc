// Package version holds build information injected at link time.
package version

// Version is the release version of hvmc. It is set at build time with
// -ldflags "-X github.com/matfax/hvmc/internal/version.Version=<version>".
var Version = "dev"

// String returns the version for display.
func String() string {
	if Version == "" {
		return "dev"
	}
	return Version
}
