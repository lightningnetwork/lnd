package routerrpc

import (
	"fmt"
	"time"
)

// parseDuration parses a duration string using a hybrid approach. It first
// attempts to use the standard library time.ParseDuration, which supports
// ns, us, ms, s, m, h. If that fails, it falls back to custom parsing for
// user-friendly units: d (days), w (weeks), M (months), y (years).
//
// Examples:
//   - Standard Go: "-24h", "-1.5h", "-30m"
//   - Custom units: "-1d", "-1w", "-1M", "-1y"
//
// All durations should be negative to indicate "time ago".
func parseDuration(durationStr string) (time.Duration, error) {
	// First, try the standard library parser.
	duration, err := time.ParseDuration(durationStr)
	if err == nil {
		// Enforce negative durations to prevent confusion.
		if duration >= 0 {
			return 0, fmt.Errorf("duration must be negative to " +
				"indicate time in the past (e.g., -1w, -24h)")
		}

		return duration, nil
	}

	// Fall back to custom parsing for d, w, M, y units.
	if len(durationStr) < 2 {
		return 0, fmt.Errorf("duration too short")
	}

	// Duration strings should start with a minus sign for "ago".
	if durationStr[0] != '-' {
		return 0, fmt.Errorf("duration must be " +
			"negative (e.g., -1w, -24h)")
	}

	// Strip the minus sign.
	durationStr = durationStr[1:]

	// Find where the numeric part ends. We allow digits and a single
	// decimal point so that custom units accept fractional values like
	// "-1.5d", matching the behaviour of the standard Go parser.
	var (
		numStr string
		unit   string
		hasDot bool
	)
	for i, ch := range durationStr {
		if ch == '.' && !hasDot {
			hasDot = true
			continue
		}
		if ch < '0' || ch > '9' {
			numStr = durationStr[:i]
			unit = durationStr[i:]
			break
		}
	}

	if numStr == "" {
		return 0, fmt.Errorf("no numeric value found")
	}
	if unit == "" {
		return 0, fmt.Errorf("no unit specified")
	}

	var value float64
	_, parseErr := fmt.Sscanf(numStr, "%f", &value)
	if parseErr != nil {
		return 0, fmt.Errorf("invalid numeric value: %w", parseErr)
	}

	// Calculate the duration based on the custom unit.
	var customDuration time.Duration
	switch unit {
	case "d":
		customDuration = time.Duration(value * 24 * float64(time.Hour))

	case "w":
		customDuration = time.Duration(
			value * 7 * 24 * float64(time.Hour),
		)

	case "M":
		// Average month = 30.44 days.
		customDuration = time.Duration(
			value * 30.44 * 24 * float64(time.Hour),
		)

	case "y":
		// Average year = 365.25 days.
		customDuration = time.Duration(
			value * 365.25 * 24 * float64(time.Hour),
		)

	default:
		// Not a custom unit we recognize, return the original error.
		return 0, fmt.Errorf("unknown time unit: %s (supported: ns, "+
			"us, ms, s, m, h, d, w, M, y)", unit)
	}

	// Return negative duration (going back in time).
	return -customDuration, nil
}
