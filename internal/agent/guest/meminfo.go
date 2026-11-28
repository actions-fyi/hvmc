package guest

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// ErrMalformedMeminfo is returned for /proc/meminfo content that does not
// carry MemTotal and MemAvailable in the expected form.
var ErrMalformedMeminfo = errors.New("malformed /proc/meminfo")

// meminfoFields are the fields the guest agent reads.
var meminfoFields = []string{"MemTotal", "MemAvailable"}

// ParseMeminfo reads MemTotal and MemAvailable from /proc/meminfo content.
// Every line must have the form "Key: value"; the two fields must each
// appear once, as a non-negative integer followed by "kB". Other fields
// are ignored.
func ParseMeminfo(r io.Reader) (Reading, error) {
	values := map[string]int64{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		if err := parseMeminfoLine(sc.Text(), values); err != nil {
			return Reading{}, fmt.Errorf("%w: line %d: %w", ErrMalformedMeminfo, n, err)
		}
	}
	if err := sc.Err(); err != nil {
		return Reading{}, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	for _, f := range meminfoFields {
		if _, ok := values[f]; !ok {
			return Reading{}, fmt.Errorf("%w: %s missing", ErrMalformedMeminfo, f)
		}
	}
	return Reading{Total: values["MemTotal"], Available: values["MemAvailable"]}, nil
}

// parseMeminfoLine stores the line's value in values if it is one of
// meminfoFields.
func parseMeminfoLine(line string, values map[string]int64) error {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	key, rest, ok := strings.Cut(line, ":")
	if !ok {
		return fmt.Errorf("%q has no colon", line)
	}
	if !isMeminfoField(key) {
		return nil
	}
	if _, dup := values[key]; dup {
		return fmt.Errorf("%s repeated", key)
	}
	v, err := parseKB(strings.Fields(rest))
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	values[key] = v
	return nil
}

func isMeminfoField(key string) bool {
	for _, f := range meminfoFields {
		if key == f {
			return true
		}
	}
	return false
}

// parseKB converts ["<n>", "kB"] to bytes.
func parseKB(fields []string) (int64, error) {
	if len(fields) != 2 || fields[1] != "kB" {
		return 0, fmt.Errorf("value %q, want \"<n> kB\"", strings.Join(fields, " "))
	}
	kb, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || kb < 0 {
		return 0, fmt.Errorf("value %q is not a non-negative integer", fields[0])
	}
	if kb > math.MaxInt64/1024 {
		return 0, fmt.Errorf("value %d kB overflows", kb)
	}
	return kb * 1024, nil
}
