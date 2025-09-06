package lookout

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/txsort"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
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
	JusticeKit blob.JusticeKit
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
	kit := p.JusticeKit

	// Retrieve the to-local output script and witness from the justice kit.
	toLocalPkScript, witness, err := kit.ToLocalOutputSpendInfo()
	if err != nil {
		return nil, err
	}

	// Locate the to-local output on the breaching commitment transaction.
	toLocalIndex, toLocalTxOut, err := findTxOutByPkScript(
		p.BreachedCommitTx, toLocalPkScript,
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

	return &breachedInput{
		txOut:    toLocalTxOut,
		outPoint: toLocalOutPoint,
		witness:  witness,
	}, nil
}

// commitToRemoteInput extracts the information required to spend the commit
// to-remote output.
func (p *JusticeDescriptor) commitToRemoteInput() (*breachedInput, error) {
	kit := p.JusticeKit

	// Retrieve the to-remote output script, witness script and sequence
	// from the justice kit.
	toRemotePkScript, witness, seq, err := kit.ToRemoteOutputSpendInfo()
	if err != nil {
		return nil, err
	}

	// Locate the to-remote output on the breaching commitment transaction.
	toRemoteIndex, toRemoteTxOut, err := findTxOutByPkScript(
		p.BreachedCommitTx, toRemotePkScript,
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

	return &breachedInput{
		txOut:    toRemoteTxOut,
		outPoint: toRemoteOutPoint,
		witness:  witness,
		sequence: seq,
	}, nil
}

// assembleJusticeTxn accepts the breached inputs recovered from state update
// and attempts to construct the justice transaction that sweeps the victims
// funds to their wallet and claims the watchtower's reward.
func (p *JusticeDescriptor) assembleJusticeTxn(txWeight lntypes.WeightUnit,
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
		totalAmt, txWeight, p.JusticeKit.SweepAddress(),
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

	hashes := txscript.NewTxSigHashes(justiceTxn, prevOutFetcher)
	for _, inp := range inputs {
		// Lookup the input's new post-sort position.
		i := inputIndex[inp.outPoint]
		justiceTxn.TxIn[i].Witness = inp.witness

		// Validate the reconstructed witnesses to ensure they are
		// valid for the breached inputs.
		vm, err := txscript.NewEngine(
			inp.txOut.PkScript, justiceTxn, i,
			txscript.StandardVerifyFlags,
			nil, hashes, inp.txOut.Value, prevOutFetcher,
		)
		if err != nil {
			return nil, err
		}
		if err := vm.Execute(); err != nil {
			log.Debugf("Failed to validate justice transaction: %s",
				lnutils.SpewLogClosure(justiceTxn))
			return nil, fmt.Errorf("error validating TX: %w", err)
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

	commitmentType, err := p.SessionInfo.Policy.BlobType.CommitmentType(nil)
	if err != nil {
		return nil, err
	}

	// Add the sweep address's contribution, depending on whether it is a
	// p2wkh or p2wsh output.
	switch len(p.JusticeKit.SweepAddress()) {
	case input.P2WPKHSize:
		weightEstimate.AddP2WKHOutput()

	// NOTE: P2TR has the same size as P2WSH (34 bytes), their output sizes
	// are also the same (43 bytes), so here we implicitly catch the P2TR
	// output case.
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

	// Get the weight for the to-local witness and add that to the
	// estimator.
	toLocalWitnessSize, err := commitmentType.ToLocalWitnessSize()
	if err != nil {
		return nil, err
	}
	weightEstimate.AddWitnessInput(toLocalWitnessSize)

	sweepInputs = append(sweepInputs, toLocalInput)

	log.Debugf("Found to local witness output=%v, stack=%x",
		lnutils.SpewLogClosure(toLocalInput.txOut),
		toLocalInput.witness)

	// If the justice kit specifies that we have to sweep the to-remote
	// output, we'll also try to assemble the output and add it to weight
	// estimate if successful.
	if p.JusticeKit.HasCommitToRemoteOutput() {
		toRemoteInput, err := p.commitToRemoteInput()
		if err != nil {
			return nil, err
		}
		sweepInputs = append(sweepInputs, toRemoteInput)

		log.Debugf("Found to remote witness output=%v, stack=%x",
			lnutils.SpewLogClosure(toRemoteInput.txOut),
			toRemoteInput.witness)

		// Get the weight for the to-remote witness and add that to the
		// estimator.
		toRemoteWitnessSize, err := commitmentType.ToRemoteWitnessSize()
		if err != nil {
			return nil, err
		}

		weightEstimate.AddWitnessInput(toRemoteWitnessSize)
	}

	// TODO(conner): sweep htlc outputs

	txWeight := weightEstimate.Weight()

	return p.assembleJusticeTxn(txWeight, sweepInputs...)
}

// findTxOutByPkScript searches the given transaction for an output whose
// pkscript matches the query. If one is found, the TxOut is returned along with
// the index.
//
// NOTE: The search stops after the first match is found.
func findTxOutByPkScript(txn *wire.MsgTx,
	pkScript *txscript.PkScript) (uint32, *wire.TxOut, error) {

	found, index := input.FindScriptOutputIndex(txn, pkScript.Script())
	if !found {
		return 0, nil, ErrOutputNotFound
	}

	return index, txn.TxOut[index], nil
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
