package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestResolve checks that import-only conflicts keep the release branch's v1
// imports plus the new non-v2 imports, and that other conflicts are refused.
func TestResolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want string
		n    int
		err  error
	}{{
		name: "new import next to v2 imports",
		src: `package x

import (
	"sync"

<<<<<<< HEAD
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
=======
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/chain"
>>>>>>> bd37272af (sweep: retain conflict retry funding)
	"github.com/lightningnetwork/lnd/fn/v2"
)
`,
		want: `package x

import (
	"sync"

	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/fn/v2"
)
`,
		n: 1,
	}, {
		name: "only v2 renames",
		src: `package x

import (
<<<<<<< HEAD
	"github.com/btcsuite/btcd/wire"
=======
	"github.com/btcsuite/btcd/wire/v2"
>>>>>>> abc (x)
)
`,
		want: `package x

import (
	"github.com/btcsuite/btcd/wire"
)
`,
		n: 1,
	}, {
		name: "code in the conflict",
		src: `package x

func f() {
<<<<<<< HEAD
	tc := tc
=======
>>>>>>> abc (x)
}
`,
		err: errNotImports,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, n, err := resolve([]byte(tc.src))
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.n, n)
			require.Equal(t, tc.want, string(got))
		})
	}
}
