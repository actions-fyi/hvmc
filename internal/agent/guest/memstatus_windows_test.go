package guest

import (
	"math"
	"testing"
)

// TestGlobalMemoryStatusEx reads the real memory status of the Windows CI
// runner.
func TestGlobalMemoryStatusEx(t *testing.T) {
	r, err := platformSource()
	if err != nil {
		t.Fatal(err)
	}
	if r.CommitLimit == nil || r.CommitAvailable == nil {
		t.Fatalf("commit figures missing: %+v", r)
	}
	limit, avail := *r.CommitLimit, *r.CommitAvailable
	switch {
	case r.Total <= 0:
		t.Errorf("total = %d, want > 0", r.Total)
	case r.Available <= 0 || r.Available > r.Total:
		t.Errorf("available = %d, want in (0, total %d]", r.Available, r.Total)
	case limit < r.Total:
		t.Errorf("commit limit %d < total physical %d", limit, r.Total)
	case avail < 0 || avail > limit:
		t.Errorf("commit available = %d, want in [0, limit %d]", avail, limit)
	}
}

func TestMemoryStatusReadingOverflow(t *testing.T) {
	if _, err := memoryStatusReading(memoryStatusEx{TotalPhys: math.MaxInt64 + 1}); err == nil {
		t.Fatal("overflow accepted")
	}
	r, err := memoryStatusReading(memoryStatusEx{TotalPhys: 4, AvailPhys: 3, TotalPageFile: 6, AvailPageFile: 5})
	if err != nil || r.Total != 4 || r.Available != 3 || *r.CommitLimit != 6 || *r.CommitAvailable != 5 {
		t.Fatalf("reading = %+v, %v", r, err)
	}
}
