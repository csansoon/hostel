package config

import (
	"fmt"
	"time"
)

// Duration is a time.Duration that TOML files write as a string, for example
// "10m". It is a struct so that a TOML integer is a type error, not nanoseconds.
type Duration struct{ d time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("negative duration %q", b)
	}
	d.d = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.d.String()), nil
}

// D returns the value as a time.Duration; 0 for nil.
func (d *Duration) D() time.Duration {
	if d == nil {
		return 0
	}
	return d.d
}

func dur(v time.Duration) *Duration { return &Duration{d: v} }
