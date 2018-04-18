// +build !rpctest

package main

import "testing"

func TestParseHexColor(t *testing.T) {
	empty := ""
	color, err := parseHexColor(empty)
	if err == nil {
		t.Fatalf("Empty color string should return error, but did not")
	}

	tooLong := "#1234567"
	color, err = parseHexColor(tooLong)
	if err == nil {
		t.Fatalf("Invalid color string %s should return error, but did not",
			tooLong)
	}

	invalidFormat := "$123456"
	color, err = parseHexColor(invalidFormat)
	if err == nil {
		t.Fatalf("Invalid color string %s should return error, but did not",
			invalidFormat)
	}

	valid := "#C0FfeE"
	color, err = parseHexColor(valid)
	if err != nil {
		t.Fatalf("Color %s valid to parse: %s", valid, err)
	}
	if color.R != 0xc0 || color.G != 0xff || color.B != 0xee {
		t.Fatalf("Color %s incorrectly parsed as %v", valid, color)
	}
}
