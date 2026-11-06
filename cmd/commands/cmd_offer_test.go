//go:build dev
// +build dev

package commands

import (
	"flag"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli"
)

// TestOfferExpiry checks how createoffer turns its expiry flags into an
// absolute expiry.
func TestOfferExpiry(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)

	tests := []struct {
		name   string
		args   []string
		expiry uint64
		err    string
	}{
		{
			name: "no expiry",
		},
		{
			name:   "absolute expiry",
			args:   []string{"--absolute_expiry", "1800000000"},
			expiry: 1_800_000_000,
		},
		{
			name:   "duration",
			args:   []string{"--expiry", "1h30m"},
			expiry: 1_700_005_400,
		},
		{
			name: "both",
			args: []string{
				"--expiry", "1h", "--absolute_expiry", "1",
			},
			err: "not both",
		},
		{
			name: "zero duration",
			args: []string{"--expiry", "0s"},
			err:  "must be positive",
		},
		{
			name: "negative duration",
			args: []string{"--expiry", "-1h"},
			err:  "must be positive",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := newOfferContext(t, tc.args)
			expiry, err := offerExpiry(ctx, now)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expiry, expiry)
		})
	}
}

// newOfferContext parses args with the flags of the createoffer command.
func newOfferContext(t *testing.T, args []string) *cli.Context {
	t.Helper()

	flagSet := flag.NewFlagSet("createoffer", flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)
	for _, f := range CreateOfferCommand.Flags {
		f.Apply(flagSet)
	}
	require.NoError(t, flagSet.Parse(args))

	return cli.NewContext(cli.NewApp(), flagSet, nil)
}
