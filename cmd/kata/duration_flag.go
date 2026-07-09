package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// durationFlag is a pflag.Value for duration flags that keeps Go's
// units-required grammar but fails with guidance instead of the stock
// parser error. The common mistake — agents especially — is a bare number
// ("--timeout 1800"); bare numbers stay rejected because they are ambiguous
// (seconds? milliseconds?), but the error suggests the seconds spelling
// instead of just naming the parse failure.
type durationFlag struct {
	d *time.Duration
}

func (f durationFlag) String() string {
	if f.d == nil {
		return "0s"
	}
	return f.d.String()
}

func (f durationFlag) Set(s string) error {
	trimmed := strings.TrimSpace(s)
	d, err := time.ParseDuration(trimmed)
	if err != nil {
		if _, numErr := strconv.ParseFloat(trimmed, 64); numErr == nil {
			return fmt.Errorf("missing unit in duration %q: did you mean %q? (units: s, m, h)",
				trimmed, trimmed+"s")
		}
		return fmt.Errorf("invalid duration %q: use a Go duration such as 30s, 5m, or 1h30m", trimmed)
	}
	*f.d = d
	return nil
}

func (f durationFlag) Type() string { return "duration" }
