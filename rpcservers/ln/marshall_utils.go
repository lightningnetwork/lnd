package ln

import (
	"encoding/hex"
	"errors"
	fmt "fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrSatMsatMutualExclusive is returned when both a sat and an msat
	// amount are set.
	ErrSatMsatMutualExclusive = errors.New(
		"sat and msat arguments are mutually exclusive",
	)
)

// CalculateFeeLimit returns the fee limit in millisatoshis. If a percentage
// based fee limit has been requested, we'll factor in the ratio provided with
// the amount of the payment.
func CalculateFeeLimit(feeLimit *lnrpc.FeeLimit,
	amount lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	switch feeLimit.GetLimit().(type) {
	case *lnrpc.FeeLimit_Fixed:
		return lnwire.NewMSatFromSatoshis(
			btcutil.Amount(feeLimit.GetFixed()),
		)

	case *lnrpc.FeeLimit_FixedMsat:
		return lnwire.MilliSatoshi(feeLimit.GetFixedMsat())

	case *lnrpc.FeeLimit_Percent:
		return amount * lnwire.MilliSatoshi(feeLimit.GetPercent()) / 100

	default:
		// Fall back to a sane default value that is based on the amount
		// itself.
		return lnwallet.DefaultRoutingFeeLimitForAmount(amount)
	}
}

// UnmarshallAmt returns a strong msat type for a sat/msat pair of rpc fields.
func UnmarshallAmt(amtSat, amtMsat int64) (lnwire.MilliSatoshi, error) {
	if amtSat != 0 && amtMsat != 0 {
		return 0, ErrSatMsatMutualExclusive
	}

	if amtSat != 0 {
		return lnwire.NewMSatFromSatoshis(btcutil.Amount(amtSat)), nil
	}

	return lnwire.MilliSatoshi(amtMsat), nil
}

// ParseConfs validates the minimum and maximum confirmation arguments of a
// ListUnspent request.
func ParseConfs(min, max int32) (int32, int32, error) {
	switch {
	// Ensure that the user didn't attempt to specify a negative number of
	// confirmations, as that isn't possible.
	case min < 0:
		return 0, 0, fmt.Errorf("min confirmations must be >= 0")

	// We'll also ensure that the min number of confs is strictly less than
	// or equal to the max number of confs for sanity.
	case min > max:
		return 0, 0, fmt.Errorf("max confirmations must be >= min " +
			"confirmations")

	default:
		return min, max, nil
	}
}

// MarshalUtxos translates a []*lnwallet.Utxo into a []*lnrpc.Utxo.
func MarshalUtxos(utxos []*lnwallet.Utxo, activeNetParams *chaincfg.Params) (
	[]*lnrpc.Utxo, error) {

	res := make([]*lnrpc.Utxo, 0, len(utxos))
	for _, utxo := range utxos {
		// Translate lnwallet address type to the proper gRPC proto
		// address type.
		var addrType lnrpc.AddressType
		switch utxo.AddressType {
		case lnwallet.WitnessPubKey:
			addrType = lnrpc.AddressType_WITNESS_PUBKEY_HASH

		case lnwallet.NestedWitnessPubKey:
			addrType = lnrpc.AddressType_NESTED_PUBKEY_HASH

		case lnwallet.TaprootPubkey:
			addrType = lnrpc.AddressType_TAPROOT_PUBKEY

		case lnwallet.UnknownAddressType:
			continue

		default:
			return nil, fmt.Errorf("invalid utxo address type")
		}

		// Now that we know we have a proper mapping to an address,
		// we'll convert the regular outpoint to an lnrpc variant.
		outpoint := &lnrpc.OutPoint{
			TxidBytes:   utxo.OutPoint.Hash[:],
			TxidStr:     utxo.OutPoint.Hash.String(),
			OutputIndex: utxo.OutPoint.Index,
		}

		utxoResp := lnrpc.Utxo{
			AddressType:   addrType,
			AmountSat:     int64(utxo.Value),
			PkScript:      hex.EncodeToString(utxo.PkScript),
			Outpoint:      outpoint,
			Confirmations: utxo.Confirmations,
		}

		// Finally, we'll attempt to extract the raw address from the
		// script so we can display a human friendly address to the end
		// user.
		_, outAddresses, _, err := txscript.ExtractPkScriptAddrs(
			utxo.PkScript, activeNetParams,
		)
		if err != nil {
			return nil, err
		}

		// If we can't properly locate a single address, then this was
		// an error in our mapping, and we'll return an error back to
		// the user.
		if len(outAddresses) != 1 {
			return nil, fmt.Errorf("an output was unexpectedly " +
				"multisig")
		}
		utxoResp.Address = outAddresses[0].String()

		res = append(res, &utxoResp)
	}

	return res, nil
}

// MarshallOutputType translates a txscript.ScriptClass into a
// lnrpc.OutputScriptType.
func MarshallOutputType(o txscript.ScriptClass) lnrpc.OutputScriptType {
	// Translate txscript ScriptClass type to the proper gRPC proto
	// output script type.
	switch o {
	case txscript.ScriptHashTy:
		return lnrpc.OutputScriptType_SCRIPT_TYPE_SCRIPT_HASH
	case txscript.WitnessV0PubKeyHashTy:
		return lnrpc.OutputScriptType_SCRIPT_TYPE_WITNESS_V0_PUBKEY_HASH
	case txscript.WitnessV0ScriptHashTy:
		return lnrpc.OutputScriptType_SCRIPT_TYPE_WITNESS_V0_SCRIPT_HASH
	case txscript.PubKeyTy:
		return lnrpc.OutputScriptType_SCRIPT_TYPE_PUBKEY
	case txscript.MultiSigTy:
		return lnrpc.OutputScriptType_SCRIPT_TYPE_MULTISIG
	case txscript.NullDataTy:
		return lnrpc.OutputScriptType_SCRIPT_TYPE_NULLDATA
	case txscript.NonStandardTy:
		return lnrpc.OutputScriptType_SCRIPT_TYPE_NON_STANDARD
	case txscript.WitnessUnknownTy:
		return lnrpc.OutputScriptType_SCRIPT_TYPE_WITNESS_UNKNOWN
	default:
		return lnrpc.OutputScriptType_SCRIPT_TYPE_PUBKEY_HASH
	}
}
