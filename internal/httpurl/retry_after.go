package httpurl

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryAfterWait parses the unbounded header and reports whether it was valid.
func RetryAfterWait(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		const maxDurationSeconds = int64(1<<63-1) / int64(time.Second)
		if seconds > maxDurationSeconds {
			return time.Duration(1<<63 - 1), true
		}
		return time.Duration(seconds) * time.Second, true
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(at.Sub(now), 0), true
}
