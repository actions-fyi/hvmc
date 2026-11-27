package guest_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/matfax/hvmc/internal/agent/guest"
)

const meminfoSample = `MemTotal:        8131980 kB
MemFree:          364552 kB
MemAvailable:    5512340 kB
Buffers:          212672 kB
Cached:          4702760 kB
HugePages_Total:       0
Hugepagesize:       2048 kB
`

func TestParseMeminfo(t *testing.T) {
	r, err := guest.ParseMeminfo(strings.NewReader(meminfoSample))
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 8131980*1024 || r.Available != 5512340*1024 {
		t.Errorf("reading = %+v", r)
	}
	if r.CommitLimit != nil || r.CommitAvailable != nil {
		t.Errorf("Linux reading has commit figures: %+v", r)
	}
}

func TestParseMeminfoRejects(t *testing.T) {
	for name, in := range map[string]string{
		"empty":                "",
		"only blank lines":     "\n\n",
		"missing MemAvailable": "MemTotal: 8131980 kB\nMemFree: 1 kB\n",
		"missing MemTotal":     "MemAvailable: 5512340 kB\n",
		"non-numeric":          "MemTotal: lots kB\nMemAvailable: 1 kB\n",
		"negative":             "MemTotal: -1 kB\nMemAvailable: 1 kB\n",
		"unexpected unit":      "MemTotal: 8131980 MB\nMemAvailable: 1 kB\n",
		"no unit":              "MemTotal: 8131980\nMemAvailable: 1 kB\n",
		"extra field":          "MemTotal: 8131980 kB x\nMemAvailable: 1 kB\n",
		"truncated value":      "MemTotal: 8131980 kB\nMemAvailable:",
		"truncated key":        "MemTotal: 8131980 kB\nMemAvail",
		"repeated field":       "MemTotal: 1 kB\nMemTotal: 2 kB\nMemAvailable: 1 kB\n",
		"overflow":             "MemTotal: 9223372036854775807 kB\nMemAvailable: 1 kB\n",
	} {
		t.Run(name, func(t *testing.T) {
			r, err := guest.ParseMeminfo(strings.NewReader(in))
			if !errors.Is(err, guest.ErrMalformedMeminfo) {
				t.Fatalf("ParseMeminfo = %+v, %v; want ErrMalformedMeminfo", r, err)
			}
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

func TestParseMeminfoReadError(t *testing.T) {
	if _, err := guest.ParseMeminfo(failingReader{}); err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("err = %v", err)
	}
}
