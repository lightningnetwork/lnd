package tor

import "testing"

// TestParseTorVersion is a series of tests for different version strings that
// check the correctness of determining whether they support creating v3 onion
// services through Tor control's port.
func TestParseTorVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		version string
		valid   bool
	}{
		{
			version: "0.3.3.6",
			valid:   true,
		},
		{
			version: "0.3.3.7",
			valid:   true,
		},
		{
			version: "0.3.4.6",
			valid:   true,
		},
		{
			version: "0.4.3.6",
			valid:   true,
		},
		{
			version: "1.3.3.6",
			valid:   true,
		},
		{
			version: "0.3.3.6-rc",
			valid:   true,
		},
		{
			version: "0.3.3.7-rc",
			valid:   true,
		},
		{
			version: "0.3.3.5-rc",
			valid:   false,
		},
		{
			version: "0.3.3.5",
			valid:   false,
		},
		{
			version: "0.3.2.6",
			valid:   false,
		},
		{
			version: "0.1.3.6",
			valid:   false,
		},
	}

	for i, test := range tests {
		err := supportsV3(test.version)
		if test.valid != (err == nil) {
			t.Fatalf("test %d with version string %v failed: %v", i,
				test.version, err)
		}
	}
}
