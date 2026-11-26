package guest

import (
	"fmt"
	"os"
)

// meminfoPath is the Linux memory source.
const meminfoPath = "/proc/meminfo"

// platformSource reads MemTotal and MemAvailable from /proc/meminfo.
func platformSource() (Reading, error) {
	f, err := os.Open(meminfoPath)
	if err != nil {
		return Reading{}, fmt.Errorf("open %s: %w", meminfoPath, err)
	}
	defer func() { _ = f.Close() }()
	return ParseMeminfo(f)
}
