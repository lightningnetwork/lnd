package walletrpc

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseDerivationPath(t *testing.T) {
	testCases := []struct {
		name           string
		path           string
		expectedErr    string
		expectedResult []uint32
	}{{
		name:        "empty path",
		path:        "",
		expectedErr: "path cannot be empty",
	}, {
		name:        "just whitespace",
		path:        " \n\t\r",
		expectedErr: "path cannot be empty",
	}, {
		name:        "incorrect prefix",
		path:        "0/0",
		expectedErr: "path must start with m/",
	}, {
		name:        "invalid number",
		path:        "m/a'/0'",
		expectedErr: "could not parse part \"a\": strconv.ParseInt",
	}, {
		name:        "double slash",
		path:        "m/0'//",
		expectedErr: "could not parse part \"\": strconv.ParseInt",
	}, {
		name:        "number too large",
		path:        "m/99999999999999",
		expectedErr: "could not parse part \"99999999999999\": strconv",
	}, {
		name:           "empty path",
		path:           "m/",
		expectedResult: []uint32{},
	}, {
		name:           "mixed path",
		path:           "m/0'/1'/2'/3/4/5/6'/7'",
		expectedResult: []uint32{0, 1, 2, 3, 4, 5, 6, 7},
	}, {
		name:           "short path",
		path:           "m/0'",
		expectedResult: []uint32{0},
	}, {
		name:           "plain path",
		path:           "m/0/1/2",
		expectedResult: []uint32{0, 1, 2},
	}}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(tt *testing.T) {
			result, err := parseDerivationPath(tc.path)

			if tc.expectedErr != "" {
				require.Error(tt, err)
				require.Contains(
					tt, err.Error(), tc.expectedErr,
				)
			} else {
				require.NoError(tt, err)
				require.Equal(tt, tc.expectedResult, result)
			}
		})
	}
}
