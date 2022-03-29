package lookout

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/txsort"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
)

var (
	// ErrOutputNotFound signals that the breached output could not be found
	// on the commitment transaction.
	ErrOutputNotFound = errors.New("unable to find output on commit tx")

	// ErrUnknownSweepAddrType signals that client provided an output that
	// was not p2wkh or p2wsh.
	ErrUnknownSweepAddrType = errors.New("sweep addr is not p2wkh or p2wsh")
)

// JusticeDescriptor contains the information required to sweep a breached
// channel on behalf of a victim. It supports the ability to create the justice
// transaction that sweeps the commitments and recover a cut of the channel for
// the watcher's eternal vigilance.
type JusticeDescriptor struct {
	// BreachedCommitTx is the commitment transaction that caused the breach
	// to be detected.
	BreachedCommitTx *wire.MsgTx

	// SessionInfo contains the contract with the watchtower client and
	// the prenegotiated terms they agreed to.
	SessionInfo *wtdb.SessionInfo

	// JusticeKit contains the decrypted blob and information required to
	// construct the transaction scripts and witnesses.
	JusticeKit *blob.JusticeKit
}

// breachedInput contains the required information to construct and spend
// breached outputs on a commitment transaction.
type breachedInput struct {
	txOut    *wire.TxOut
	outPoint wire.OutPoint
	witness  [][]byte
	sequence uint32
}

// commitToLocalInput extracts the information required to spend the commit
// to-local output.
func (p *JusticeDescriptor) commitToLocalInput() (*breachedInput, error) {
	// Retrieve the to-local witness script from the justice kit.
	toLocalScript, err := p.JusticeKit.CommitToLocalWitnessScript()
	if err != nil {
		return nil, err
	}

	// Compute the witness script hash, which will be used to locate the
	// input on the breaching commitment transaction.
	toLocalWitnessHash, err := input.WitnessScriptHash(toLocalScript)
	if err != nil {
		return nil, err
	}

	// Locate the to-local output on the breaching commitment transaction.
	toLocalIndex, toLocalTxOut, err := findTxOutByPkScript(
		p.BreachedCommitTx, toLocalWitnessHash,
	)
	if err != nil {
		return nil, err
	}

	// Construct the to-local outpoint that will be spent in the justice
	// transaction.
	toLocalOutPoint := wire.OutPoint{
		Hash:  p.BreachedCommitTx.TxHash(),
		Index: toLocalIndex,
	}

	// Retrieve to-local witness stack, which primarily includes a signature
	// under the revocation pubkey.
	witnessStack, err := p.JusticeKit.CommitToLocalRevokeWitnessStack()
	if err != nil {
		return nil, err
	}

	return &breachedInput{
		txOut:    toLocalTxOut,
		outPoint: toLocalOutPoint,
		witness:  buildWitness(witnessStack, toLocalScript),
	}, nil
}

// commitToRemoteInput extracts the information required to spend the commit
// to-remote output.
func (p *JusticeDescriptor) commitToRemoteInput() (*breachedInput, error) {
	// Retrieve the to-remote witness script from the justice kit.
	toRemoteScript, err := p.JusticeKit.CommitToRemoteWitnessScript()
	if err != nil {
		return nil, err
	}

	var (
		toRemoteScriptHash []byte
		toRemoteSequence   uint32
	)
	if p.JusticeKit.BlobType.IsAnchorChannel() {
		toRemoteScriptHash, err = input.WitnessScriptHash(
			toRemoteScript,
		)
		if err != nil {
			return nil, err
		}

		toRemoteSequence = 1
	} else {
		// Since the to-remote witness script should just be a regular p2wkh
		// output, we'll parse it to retrieve the public key.
		toRemotePubKey, err := btcec.ParsePubKey(toRemoteScript)
		if err != nil {
			return nil, err
		}

		// Compute the witness script hash from the to-remote pubkey, which will
		// be used to locate the input on the breach commitment transaction.
		toRemoteScriptHash, err = input.CommitScriptUnencumbered(
			toRemotePubKey,
		)
		if err != nil {
			return nil, err
		}
	}

	// Locate the to-remote output on the breaching commitment transaction.
	toRemoteIndex, toRemoteTxOut, err := findTxOutByPkScript(
		p.BreachedCommitTx, toRemoteScriptHash,
	)
	if err != nil {
		return nil, err
	}

	// Construct the to-remote outpoint which will be spent in the justice
	// transaction.
	toRemoteOutPoint := wire.OutPoint{
		Hash:  p.BreachedCommitTx.TxHash(),
		Index: toRemoteIndex,
	}

	// Retrieve the to-remote witness stack, which is just a signature under
	// the to-remote pubkey.
	witnessStack, err := p.JusticeKit.CommitToRemoteWitnessStack()
	if err != nil {
		return nil, err
	}

	return &breachedInput{
		txOut:    toRemoteTxOut,
		outPoint: toRemoteOutPoint,
		witness:  buildWitness(witnessStack, toRemoteScript),
		sequence: toRemoteSequence,
	}, nil
}

// assembleJusticeTxn accepts the breached inputs recovered from state update
// and attempts to construct the justice transaction that sweeps the victims
// funds to their wallet and claims the watchtower's reward.
func (p *JusticeDescriptor) assembleJusticeTxn(txWeight int64,
	inputs ...*breachedInput) (*wire.MsgTx, error) {

	justiceTxn := wire.NewMsgTx(2)

	// First, construct add the breached inputs to our justice transaction
	// and compute the total amount that will be swept.
	var totalAmt btcutil.Amount
	for _, inp := range inputs {
		totalAmt += btcutil.Amount(inp.txOut.Value)
		justiceTxn.AddTxIn(&wire.TxIn{
			PreviousOutPoint: inp.outPoint,
			Sequence:         inp.sequence,
		})
	}

	// Using the session's policy, compute the outputs that should be added
	// to the justice transaction. In the case of an altruist sweep, there
	// will be a single output paying back to the victim. Otherwise for a
	// reward sweep, there will be two outputs, one of which pays back to
	// the victim while the other gives a cut to the tower.
	outputs, err := p.SessionInfo.Policy.ComputeJusticeTxOuts(
		totalAmt, txWeight, p.JusticeKit.SweepAddress[:],
		p.SessionInfo.RewardAddress,
	)
	if err != nil {
		return nil, err
	}

	// Attach the computed txouts to the justice transaction.
	justiceTxn.TxOut = outputs

	// Apply a BIP69 sort to the resulting transaction.
	txsort.InPlaceSort(justiceTxn)

	btx := btcutil.NewTx(justiceTxn)
	if err := blockchain.CheckTransactionSanity(btx); err != nil {
		return nil, err
	}

	// Since the transaction inputs could have been reordered as a result of the
	// BIP69 sort, create an index mapping each prevout to it's new index.
	inputIndex := make(map[wire.OutPoint]int)
	for i, txIn := range justiceTxn.TxIn {
		inputIndex[txIn.PreviousOutPoint] = i
	}

	// Attach each of the provided witnesses to the transaction.
	prevOutFetcher, err := prevOutFetcher(inputs)
	if err != nil {
		return nil, fmt.Errorf("error creating previous output "+
			"fetcher: %v", err)
	}
	for _, inp := range inputs {
		// Lookup the input's new post-sort position.
		i := inputIndex[inp.outPoint]
		justiceTxn.TxIn[i].Witness = inp.witness

		// Validate the reconstructed witnesses to ensure they are valid
		// for the breached inputs.
		vm, err := txscript.NewEngine(
			inp.txOut.PkScript, justiceTxn, i,
			txscript.StandardVerifyFlags,
			nil, nil, inp.txOut.Value, prevOutFetcher,
		)
		if err != nil {
			return nil, err
		}
		if err := vm.Execute(); err != nil {
			return nil, err
		}
	}

	return justiceTxn, nil
}

// CreateJusticeTxn computes the justice transaction that sweeps a breaching
// commitment transaction. The justice transaction is constructed by assembling
// the witnesses using data provided by the client in a prior state update.
//
// NOTE: An older version of ToLocalPenaltyWitnessSize underestimated the size
// of the witness by one byte, which could cause the signature(s) to break if
// the tower is reconstructing with the newer constant because the output values
// might differ. This method retains that original behavior to not invalidate
// historical signatures.
func (p *JusticeDescriptor) CreateJusticeTxn() (*wire.MsgTx, error) {
	var (
		sweepInputs    = make([]*breachedInput, 0, 2)
		weightEstimate input.TxWeightEstimator
	)

	// Add the sweep address's contribution, depending on whether it is a
	// p2wkh or p2wsh output.
	switch len(p.JusticeKit.SweepAddress) {
	case input.P2WPKHSize:
		weightEstimate.AddP2WKHOutput()

	case input.P2WSHSize:
		weightEstimate.AddP2WSHOutput()

	default:
		return nil, ErrUnknownSweepAddrType
	}

	// Add our reward address to the weight estimate if the policy's blob
	// type specifies a reward output.
	if p.SessionInfo.Policy.BlobType.Has(blob.FlagReward) {
		weightEstimate.AddP2WKHOutput()
	}

	// Assemble the breached to-local output from the justice descriptor and
	// add it to our weight estimate.
	toLocalInput, err := p.commitToLocalInput()
	if err != nil {
		return nil, err
	}

	// An older ToLocalPenaltyWitnessSize constant used to underestimate the
	// size by one byte. The diferrence in weight can cause different output
	// values on the sweep transaction, so we mimic the original bug to
	// avoid invalidating signatures by older clients. For anchor channels
	// we correct this and use the correct witness size.
	if p.JusticeKit.BlobType.IsAnchorChannel() {
		weightEstimate.AddWitnessInput(input.ToLocalPenaltyWitnessSize)
	} else {
		weightEstimate.AddWitnessInput(input.ToLocalPenaltyWitnessSize - 1)
	}

	sweepInputs = append(sweepInputs, toLocalInput)

	// If the justice kit specifies that we have to sweep the to-remote
	// output, we'll also try to assemble the output and add it to weight
	// estimate if successful.
	if p.JusticeKit.HasCommitToRemoteOutput() {
		toRemoteInput, err := p.commitToRemoteInput()
		if err != nil {
			return nil, err
		}
		sweepInputs = append(sweepInputs, toRemoteInput)

		if p.JusticeKit.BlobType.IsAnchorChannel() {
			weightEstimate.AddWitnessInput(input.ToRemoteConfirmedWitnessSize)
		} else {
			weightEstimate.AddWitnessInput(input.P2WKHWitnessSize)
		}
	}

	// TODO(conner): sweep htlc outputs

	txWeight := int64(weightEstimate.Weight())

	return p.assembleJusticeTxn(txWeight, sweepInputs...)
}

// findTxOutByPkScript searches the given transaction for an output whose
// pkscript matches the query. If one is found, the TxOut is returned along with
// the index.
//
// NOTE: The search stops after the first match is found.
func findTxOutByPkScript(txn *wire.MsgTx,
	pkScript []byte) (uint32, *wire.TxOut, error) {

	found, index := input.FindScriptOutputIndex(txn, pkScript)
	if !found {
		return 0, nil, ErrOutputNotFound
	}

	return index, txn.TxOut[index], nil
}

// buildWitness appends the witness script to a given witness stack.
func buildWitness(witnessStack [][]byte, witnessScript []byte) [][]byte {
	witness := make([][]byte, len(witnessStack)+1)
	lastIdx := copy(witness, witnessStack)
	witness[lastIdx] = witnessScript

	return witness
}

// prevOutFetcher returns a txscript.MultiPrevOutFetcher for the given set
// of inputs.
func prevOutFetcher(inputs []*breachedInput) (*txscript.MultiPrevOutFetcher,
	error) {

	fetcher := txscript.NewMultiPrevOutFetcher(nil)
	for _, inp := range inputs {
		if inp.txOut == nil {
			return nil, fmt.Errorf("missing input utxo information")
		}

		fetcher.AddPrevOut(inp.outPoint, inp.txOut)
	}

	return fetcher, nil
}
