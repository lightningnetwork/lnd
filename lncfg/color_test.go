package lncfg

import (
	"testing"
)

func TestParseHexColor(t *testing.T) {
	var colorTestCases = []struct {
		test  string
		valid bool // If valid format
		R     byte
		G     byte
		B     byte
	}{
		{"#123", false, 0, 0, 0},
		{"#1234567", false, 0, 0, 0},
		{"$123456", false, 0, 0, 0},
		{"#12345+", false, 0, 0, 0},
		{"#fFGG00", false, 0, 0, 0},
		{"", false, 0, 0, 0},
		{"#123456", true, 0x12, 0x34, 0x56},
		{"#C0FfeE", true, 0xc0, 0xff, 0xee},
	}

	// Perform the table driven tests.
	for _, ct := range colorTestCases {
		color, err := ParseHexColor(ct.test)
		if !ct.valid && err == nil {
			t.Fatalf("Invalid color string: %s, should return "+
				"error, but did not", ct.test)
		}

		if ct.valid && err != nil {
			t.Fatalf("Color %s valid to parse: %s", ct.test, err)
		}

		// Ensure that the string to hex decoding is working properly.
		if color.R != ct.R || color.G != ct.G || color.B != ct.B {
			t.Fatalf("Color %s incorrectly parsed as %v", ct.test, color)
		}
	}
}
