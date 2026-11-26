//go:build !linux && !windows

package guest

// platformSource fails: guest agents run on Linux and Windows only.
func platformSource() (Reading, error) { return Reading{}, ErrUnsupported }
