package lnwallet

import (
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
)

const (
	// StateHintSize is the total number of bytes used between the sequence
	// number and locktime of the commitment transaction use to encode a hint
	// to the state number of a particular commitment transaction.
	StateHintSize = 6

	// MaxStateHint is the maximum state number we're able to encode using
	// StateHintSize bytes amongst the sequence number and locktime fields
	// of the commitment transaction.
	maxStateHint uint64 = (1 << 48) - 1
)

var (
	// TimelockShift is used to make sure the commitment transaction is
	// spendable by setting the locktime with it so that it is larger than
	// 500,000,000, thus interpreting it as Unix epoch timestamp and not
	// a block height. It is also smaller than the current timestamp which
	// has bit (1 << 30) set, so there is no risk of having the commitment
	// transaction be rejected. This way we can safely use the lower 24 bits
	// of the locktime field for part of the obscured commitment transaction
	// number.
	TimelockShift = uint32(1 << 29)
)

// CreateHtlcSuccessTx creates a transaction that spends the output on the
// commitment transaction of the peer that receives an HTLC. This transaction
// essentially acts as an off-chain covenant as it's only permitted to spend
// the designated HTLC output, and also that spend can _only_ be used as a
// state transition to create another output which actually allows redemption
// or revocation of an HTLC.
//
// In order to spend the HTLC output, the witness for the passed transaction
// should be:
//   * <0> <sender sig> <recvr sig> <preimage>
func CreateHtlcSuccessTx(chanType channeldb.ChannelType,
	htlcOutput wire.OutPoint, htlcAmt btcutil.Amount, csvDelay uint32,
	revocationKey, delayKey *btcec.PublicKey) (*wire.MsgTx, error) {

	// Create a version two transaction (as the success version of this
	// spends an output with a CSV timeout).
	successTx := wire.NewMsgTx(2)

	// The input to the transaction is the outpoint that creates the
	// original HTLC on the sender's commitment transaction. Set the
	// sequence number based on the channel type.
	txin := &wire.TxIn{
		PreviousOutPoint: htlcOutput,
		Sequence:         HtlcSecondLevelInputSequence(chanType),
	}
	successTx.AddTxIn(txin)

	// Next, we'll generate the script used as the output for all second
	// level HTLC which forces a covenant w.r.t what can be done with all
	// HTLC outputs.
	witnessScript, err := input.SecondLevelHtlcScript(revocationKey, delayKey,
		csvDelay)
	if err != nil {
		return nil, err
	}
	pkScript, err := input.WitnessScriptHash(witnessScript)
	if err != nil {
		return nil, err
	}

	// Finally, the output is simply the amount of the HTLC (minus the
	// required fees), paying to the timeout script.
	successTx.AddTxOut(&wire.TxOut{
		Value:    int64(htlcAmt),
		PkScript: pkScript,
	})

	return successTx, nil
}

// CreateHtlcTimeoutTx creates a transaction that spends the HTLC output on the
// commitment transaction of the peer that created an HTLC (the sender). This
// transaction essentially acts as an off-chain covenant as it spends a 2-of-2
// multi-sig output. This output requires a signature from both the sender and
// receiver of the HTLC. By using a distinct transaction, we're able to
// uncouple the timeout and delay clauses of the HTLC contract. This
// transaction is locked with an absolute lock-time so the sender can only
// attempt to claim the output using it after the lock time has passed.
//
// In order to spend the HTLC output, the witness for the passed transaction
// should be:
// * <0> <sender sig> <receiver sig> <0>
//
// NOTE: The passed amount for the HTLC should take into account the required
// fee rate at the time the HTLC was created. The fee should be able to
// entirely pay for this (tiny: 1-in 1-out) transaction.
func CreateHtlcTimeoutTx(chanType channeldb.ChannelType,
	htlcOutput wire.OutPoint, htlcAmt btcutil.Amount,
	cltvExpiry, csvDelay uint32,
	revocationKey, delayKey *btcec.PublicKey) (*wire.MsgTx, error) {

	// Create a version two transaction (as the success version of this
	// spends an output with a CSV timeout), and set the lock-time to the
	// specified absolute lock-time in blocks.
	timeoutTx := wire.NewMsgTx(2)
	timeoutTx.LockTime = cltvExpiry

	// The input to the transaction is the outpoint that creates the
	// original HTLC on the sender's commitment transaction. Set the
	// sequence number based on the channel type.
	txin := &wire.TxIn{
		PreviousOutPoint: htlcOutput,
		Sequence:         HtlcSecondLevelInputSequence(chanType),
	}
	timeoutTx.AddTxIn(txin)

	// Next, we'll generate the script used as the output for all second
	// level HTLC which forces a covenant w.r.t what can be done with all
	// HTLC outputs.
	witnessScript, err := input.SecondLevelHtlcScript(revocationKey, delayKey,
		csvDelay)
	if err != nil {
		return nil, err
	}
	pkScript, err := input.WitnessScriptHash(witnessScript)
	if err != nil {
		return nil, err
	}

	// Finally, the output is simply the amount of the HTLC (minus the
	// required fees), paying to the regular second level HTLC script.
	timeoutTx.AddTxOut(&wire.TxOut{
		Value:    int64(htlcAmt),
		PkScript: pkScript,
	})

	return timeoutTx, nil
}

// SetStateNumHint encodes the current state number within the passed
// commitment transaction by re-purposing the locktime and sequence fields in
// the commitment transaction to encode the obfuscated state number.  The state
// number is encoded using 48 bits. The lower 24 bits of the lock time are the
// lower 24 bits of the obfuscated state number and the lower 24 bits of the
// sequence field are the higher 24 bits. Finally before encoding, the
// obfuscator is XOR'd against the state number in order to hide the exact
// state number from the PoV of outside parties.
func SetStateNumHint(commitTx *wire.MsgTx, stateNum uint64,
	obfuscator [StateHintSize]byte) error {

	// With the current schema we are only able to encode state num
	// hints up to 2^48. Therefore if the passed height is greater than our
	// state hint ceiling, then exit early.
	if stateNum > maxStateHint {
		return fmt.Errorf("unable to encode state, %v is greater "+
			"state num that max of %v", stateNum, maxStateHint)
	}

	if len(commitTx.TxIn) != 1 {
		return fmt.Errorf("commitment tx must have exactly 1 input, "+
			"instead has %v", len(commitTx.TxIn))
	}

	// Convert the obfuscator into a uint64, then XOR that against the
	// targeted height in order to obfuscate the state number of the
	// commitment transaction in the case that either commitment
	// transaction is broadcast directly on chain.
	var obfs [8]byte
	copy(obfs[2:], obfuscator[:])
	xorInt := binary.BigEndian.Uint64(obfs[:])

	stateNum = stateNum ^ xorInt

	// Set the height bit of the sequence number in order to disable any
	// sequence locks semantics.
	commitTx.TxIn[0].Sequence = uint32(stateNum>>24) | wire.SequenceLockTimeDisabled
	commitTx.LockTime = uint32(stateNum&0xFFFFFF) | TimelockShift

	return nil
}

// GetStateNumHint recovers the current state number given a commitment
// transaction which has previously had the state number encoded within it via
// setStateNumHint and a shared obfuscator.
//
// See setStateNumHint for further details w.r.t exactly how the state-hints
// are encoded.
func GetStateNumHint(commitTx *wire.MsgTx, obfuscator [StateHintSize]byte) uint64 {
	// Convert the obfuscator into a uint64, this will be used to
	// de-obfuscate the final recovered state number.
	var obfs [8]byte
	copy(obfs[2:], obfuscator[:])
	xorInt := binary.BigEndian.Uint64(obfs[:])

	// Retrieve the state hint from the sequence number and locktime
	// of the transaction.
	stateNumXor := uint64(commitTx.TxIn[0].Sequence&0xFFFFFF) << 24
	stateNumXor |= uint64(commitTx.LockTime & 0xFFFFFF)

	// Finally, to obtain the final state number, we XOR by the obfuscator
	// value to de-obfuscate the state number.
	return stateNumXor ^ xorInt
}
