//go:build walletrpc
// +build walletrpc

package commands

import (
	"bytes"
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli"
)

// newFlagContext builds a cli.Context with the given command flags parsed
// from args.
func newFlagContext(t *testing.T, flags []cli.Flag,
	args []string) *cli.Context {

	t.Helper()

	flagSet := flag.NewFlagSet("test", flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)
	for _, f := range flags {
		f.Apply(flagSet)
	}
	require.NoError(t, flagSet.Parse(args))

	return cli.NewContext(cli.NewApp(), flagSet, nil)
}

// TestParseLeaseOutputRequest verifies that exactly one of --expiry and
// --no_expiry selects the lease lifetime.
func TestParseLeaseOutputRequest(t *testing.T) {
	t.Parallel()

	outpoint := strings.Repeat("ab", 32) + ":1"
	lockID := strings.Repeat("01", 32)
	baseArgs := []string{"--outpoint", outpoint, "--lockid", lockID}

	testCases := []struct {
		name       string
		args       []string
		expiry     uint64
		noExpiry   bool
		errContain string
	}{
		{
			name:   "expiry",
			args:   []string{"--expiry", "60"},
			expiry: 60,
		},
		{
			name:     "no expiry",
			args:     []string{"--no_expiry"},
			noExpiry: true,
		},
		{
			name:       "both",
			args:       []string{"--expiry", "60", "--no_expiry"},
			errContain: "cannot be combined",
		},
		{
			name:       "neither",
			errContain: "expiry not specified",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			args := append(append([]string{}, baseArgs...),
				tc.args...)
			ctx := newFlagContext(t, leaseOutputCommand.Flags, args)

			req, err := parseLeaseOutputRequest(ctx)
			if tc.errContain != "" {
				require.ErrorContains(t, err, tc.errContain)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expiry, req.ExpirationSeconds)
			require.Equal(t, tc.noExpiry, req.NoExpiry)
			require.Equal(t, uint32(1), req.Outpoint.OutputIndex)
			require.Len(t, req.Id, 32)
		})
	}
}

// TestMarshallLocksNoExpiry verifies that the JSON lease output reports the
// no-expiry policy.
func TestMarshallLocksNoExpiry(t *testing.T) {
	t.Parallel()

	leases := []*walletrpc.UtxoLease{
		{
			Id: bytes.Repeat([]byte{0x03}, 32),
			Outpoint: &lnrpc.OutPoint{
				TxidBytes: make([]byte, 32),
			},
			Expiration: 100,
			NoExpiry:   true,
		},
		{
			Id: bytes.Repeat([]byte{0x04}, 32),
			Outpoint: &lnrpc.OutPoint{
				TxidBytes: make([]byte, 32),
			},
			Expiration: 200,
		},
	}

	jsonLocks := marshallLocks(leases)
	require.Len(t, jsonLocks, 2)
	require.True(t, jsonLocks[0].NoExpiry)
	require.False(t, jsonLocks[1].NoExpiry)
}
