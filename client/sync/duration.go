package sync

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// ParseDuration parses duration strings with support for days and weeks
// Supports formats like: 3s, 10m, 1h30m, 2h, 1h45m, 3d, 1w, 4w
// Does not support months as they have variable length
func ParseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration string")
	}

	// Pattern to match duration components: number followed by unit
	// Supported units: s (seconds), m (minutes), h (hours), d (days), w (weeks)
	pattern := regexp.MustCompile(`(\d+)([smhdw])`)
	matches := pattern.FindAllStringSubmatch(s, -1)

	if len(matches) == 0 {
		return 0, fmt.Errorf("invalid duration format: %s", s)
	}

	var total time.Duration

	for _, match := range matches {
		if len(match) != 3 {
			continue
		}

		value, err := strconv.Atoi(match[1])
		if err != nil {
			return 0, fmt.Errorf("invalid number in duration: %s", match[1])
		}

		unit := match[2]
		switch unit {
		case "s":
			total += time.Duration(value) * time.Second
		case "m":
			total += time.Duration(value) * time.Minute
		case "h":
			total += time.Duration(value) * time.Hour
		case "d":
			total += time.Duration(value) * 24 * time.Hour
		case "w":
			total += time.Duration(value) * 7 * 24 * time.Hour
		default:
			return 0, fmt.Errorf("unsupported duration unit: %s", unit)
		}
	}

	if total == 0 {
		return 0, fmt.Errorf("duration must be greater than 0")
	}

	return total, nil
}

// ValidateWaitIdle validates that waitIdle is within acceptable range
// Min: 10 seconds, Max: 24 days
func ValidateWaitIdle(d time.Duration) error {
	const maxWaitIdle = 24 * 24 * time.Hour // 24 days
	const minWaitIdle = 10 * time.Second

	if d < minWaitIdle {
		return fmt.Errorf("waitIdle must be at least %v", minWaitIdle)
	}

	if d > maxWaitIdle {
		return fmt.Errorf("waitIdle cannot exceed %v (24 days) due to SQLite busy_timeout limitations", maxWaitIdle)
	}

	return nil
}
