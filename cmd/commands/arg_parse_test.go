package commands

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var now = time.Date(2017, 11, 10, 7, 8, 9, 1234, time.UTC)

var partTimeTests = []struct {
	in          string
	expected    uint64
	errExpected bool
}{
	{
		"12345",
		uint64(12345),
		false,
	},
	{
		"-0s",
		uint64(now.Unix()),
		false,
	},
	{
		"-1s",
		uint64(time.Date(2017, 11, 10, 7, 8, 8, 1234, time.UTC).Unix()),
		false,
	},
	{
		"-2h",
		uint64(time.Date(2017, 11, 10, 5, 8, 9, 1234, time.UTC).Unix()),
		false,
	},
	{
		"-3d",
		uint64(time.Date(2017, 11, 7, 7, 8, 9, 1234, time.UTC).Unix()),
		false,
	},
	{
		"-4w",
		uint64(time.Date(2017, 10, 13, 7, 8, 9, 1234, time.UTC).Unix()),
		false,
	},
	{
		"-5M",
		uint64(now.Unix() - 30.44*5*24*60*60),
		false,
	},
	{
		"-6y",
		uint64(now.Unix() - 365.25*6*24*60*60),
		false,
	},
	{
		"-999999999999999999s",
		uint64(now.Unix() - 999999999999999999),
		false,
	},
	{
		"-9999999999999999991s",
		0,
		true,
	},
	{
		"-7z",
		0,
		true,
	},
}

// Test that parsing absolute and relative times works.
func TestParseTime(t *testing.T) {
	for _, test := range partTimeTests {
		actual, err := parseTime(test.in, now)
		if test.errExpected == (err == nil) {
			t.Fatalf("unexpected error for %s:\n%v\n", test.in, err)
		}
		if actual != test.expected {
			t.Fatalf(
				"for %s actual and expected do not match:\n%d\n%d\n",
				test.in,
				actual,
				test.expected,
			)
		}
	}
}

var stripPrefixTests = []struct {
	in       string
	expected string
}{
	{
		"lightning:ln123",
		"ln123",
	},
	{
		"lightning: ln123",
		"ln123",
	},
	{
		"ln123",
		"ln123",
	},
}

func TestStripPrefix(t *testing.T) {
	t.Parallel()

	for _, test := range stripPrefixTests {
		actual := StripPrefix(test.in)
		require.Equal(t, test.expected, actual)
	}
}
