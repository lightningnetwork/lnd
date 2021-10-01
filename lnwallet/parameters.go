package lnwallet

import (
	"github.com/btcsuite/btcd/mempool"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/input"
)

// DustLimitForSize retrieves the dust limit for a given pkscript size. Given
// the size, it automatically determines whether the script is a witness script
// or not. It calls btcd's GetDustThreshold method under the hood. It must be
// called with a proper size parameter or else a panic occurs.
func DustLimitForSize(scriptSize int) btcutil.Amount {
	var (
		dustlimit btcutil.Amount
		pkscript  []byte
	)

	// With the size of the script, determine which type of pkscript to
	// create. This will be used in the call to GetDustThreshold. We pass
	// in an empty byte slice since the contents of the script itself don't
	// matter.
	switch scriptSize {
	case input.P2WPKHSize:
		pkscript, _ = input.WitnessPubKeyHash([]byte{})

	case input.P2WSHSize:
		pkscript, _ = input.WitnessScriptHash([]byte{})

	case input.P2SHSize:
		pkscript, _ = input.GenerateP2SH([]byte{})

	case input.P2PKHSize:
		pkscript, _ = input.GenerateP2PKH([]byte{})

	case input.UnknownWitnessSize:
		pkscript, _ = input.GenerateUnknownWitness()

	default:
		panic("invalid script size")
	}

	// Call GetDustThreshold with a TxOut containing the generated
	// pkscript.
	txout := &wire.TxOut{PkScript: pkscript}
	dustlimit = btcutil.Amount(mempool.GetDustThreshold(txout))

	return dustlimit
}
