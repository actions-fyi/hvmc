package fake

import (
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Bytes is a memory amount in bytes. In YAML it is a Kubernetes quantity
// ("8Gi", "512Mi" or a plain integer) and is written back in canonical
// binary-SI form.
type Bytes int64

// Common sizes.
const (
	MiB Bytes = 1 << 20
	GiB Bytes = 1 << 30
)

// alignment is Hyper-V's memory granularity: every written value must be a
// multiple of 2 MiB.
const alignment = 2 * MiB

// MarshalJSON writes b as a quantity string.
func (b Bytes) MarshalJSON() ([]byte, error) {
	return json.Marshal(resource.NewQuantity(int64(b), resource.BinarySI).String())
}

// UnmarshalJSON reads a quantity string or a JSON number.
func (b *Bytes) UnmarshalJSON(data []byte) error {
	var q resource.Quantity
	if err := q.UnmarshalJSON(data); err != nil {
		return fmt.Errorf("invalid memory quantity %s: %w", data, err)
	}
	v := q.Value()
	if resource.NewQuantity(v, resource.BinarySI).Cmp(q) != 0 {
		return fmt.Errorf("memory quantity %s is not a whole number of bytes", data)
	}
	*b = Bytes(v)
	return nil
}

// String formats b as a quantity.
func (b Bytes) String() string {
	return resource.NewQuantity(int64(b), resource.BinarySI).String()
}

// Duration is a time.Duration written as a Go duration string ("2s",
// "1m30s") in YAML.
type Duration time.Duration

// MarshalJSON writes d as a duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON reads a duration string.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"2s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// D returns d as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }
