// Package dur parses durations, adding a "d" (days) suffix to time.ParseDuration.
package dur

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

func Parse(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		d, err := strconv.Atoi(n)
		if err != nil || d < 0 || d > 3650 {
			return 0, errors.New("invalid day count")
		}
		return time.Duration(d) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
