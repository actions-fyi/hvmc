package guest

import (
	"fmt"
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

// memoryStatusEx is MEMORYSTATUSEX (sysinfoapi.h).
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// golang.org/x/sys/windows has no wrapper for GlobalMemoryStatusEx, so it is
// called through the package's lazy DLL loader (DESIGN §2.1).
var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

// platformSource reads GlobalMemoryStatusEx: total = ullTotalPhys,
// available = ullAvailPhys, commit limit = ullTotalPageFile, commit
// available = ullAvailPageFile (guest.proto).
func platformSource() (Reading, error) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	//nolint:gosec // G103: the Win32 call takes a pointer to MEMORYSTATUSEX.
	ok, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if ok == 0 {
		return Reading{}, fmt.Errorf("GlobalMemoryStatusEx: %w", err)
	}
	return memoryStatusReading(m)
}

// memoryStatusReading converts MEMORYSTATUSEX to a Reading.
func memoryStatusReading(m memoryStatusEx) (Reading, error) {
	vals := [4]int64{}
	for i, v := range [4]uint64{m.TotalPhys, m.AvailPhys, m.TotalPageFile, m.AvailPageFile} {
		if v > math.MaxInt64 {
			return Reading{}, fmt.Errorf("GlobalMemoryStatusEx: value %d overflows int64", v)
		}
		vals[i] = int64(v)
	}
	return Reading{Total: vals[0], Available: vals[1], CommitLimit: &vals[2], CommitAvailable: &vals[3]}, nil
}
