package commands

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// reTimeRange matches systemd.time-like short negative timeranges, e.g. "-200s".
var reTimeRange = regexp.MustCompile(`^-\d{1,18}[s|m|h|d|w|M|y]$`)

// secondsPer allows translating s(seconds), m(minutes), h(ours), d(ays),
// w(eeks), M(onths) and y(ears) into corresponding seconds.
var secondsPer = map[string]int64{
	"s": 1,
	"m": 60,
	"h": 3600,
	"d": 86400,
	"w": 604800,
	"M": 2630016,  // 30.44 days
	"y": 31557600, // 365.25 days
}

// parseTime parses UNIX timestamps or short timeranges inspired by systemd
// (when starting with "-"), e.g. "-1M" for one month (30.44 days) ago.
func parseTime(s string, base time.Time) (uint64, error) {
	if reTimeRange.MatchString(s) {
		last := len(s) - 1

		d, err := strconv.ParseInt(s[1:last], 10, 64)
		if err != nil {
			return uint64(0), err
		}

		mul := secondsPer[string(s[last])]
		return uint64(base.Unix() - d*mul), nil
	}

	return strconv.ParseUint(s, 10, 64)
}

var lightningPrefix = "lightning:"

// StripPrefix removes accidentally copied 'lightning:' prefix.
func StripPrefix(s string) string {
	return strings.TrimSpace(strings.TrimPrefix(s, lightningPrefix))
}
