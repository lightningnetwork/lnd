package wtclient

import (
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/txsort"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
)

// backupTask is an internal struct for computing the justice transaction for a
// particular revoked state. A backupTask functions as a scratch pad for storing
// computing values of the transaction itself, such as the final split in
// balance if the justice transaction will give a reward to the tower. The
// backup task has three primary phases:
//  1. Init: Determines which inputs from the breach transaction will be spent,
//     and the total amount contained in the inputs.
//  2. Bind: Asserts that the revoked state is eligible under a given session's
//     parameters. Certain states may be ineligible due to fee rates, too little
//     input amount, etc. Backup of these states can be deferred to a later time
//     or session with more favorable parameters. If the session is bound
//     successfully, the final session-dependent values to the justice
//     transaction are solidified.
//  3. Send: Once the task is bound, it will be queued to send to a specific
//     tower corresponding to the session in which it was bound. The justice
//     transaction will be assembled by examining the parameters left as a
//     result of the binding. After the justice transaction is signed, the
//     necessary components are stripped out and encrypted before being sent to
//     the tower in a StateUpdate.
type backupTask struct {
	id             wtdb.BackupID
	breachInfo     *lnwallet.BreachRetribution
	commitmentType blob.CommitmentType

	// state-dependent variables

	toLocalInput  input.Input
	toRemoteInput input.Input
	totalAmt      btcutil.Amount
	sweepPkScript []byte

	// session-dependent variables

	blobType blob.Type
	outputs  []*wire.TxOut
}

// newBackupTask initializes a new backupTask.
func newBackupTask(id wtdb.BackupID, sweepPkScript []byte) *backupTask {
	return &backupTask{
		id:            id,
		sweepPkScript: sweepPkScript,
	}
}

// inputs returns all non-dust inputs that we will attempt to spend from.
//
// NOTE: Ordering of the inputs is not critical as we sort the transaction with
// BIP69 in a later stage.
func (t *backupTask) inputs() map[wire.OutPoint]input.Input {
	inputs := make(map[wire.OutPoint]input.Input)
	if t.toLocalInput != nil {
		inputs[t.toLocalInput.OutPoint()] = t.toLocalInput
	}
	if t.toRemoteInput != nil {
		inputs[t.toRemoteInput.OutPoint()] = t.toRemoteInput
	}

	return inputs
}

// addrType returns the type of an address after parsing it and matching it to
// the set of known script templates.
func addrType(pkScript []byte) txscript.ScriptClass {
	// We pass in a set of dummy chain params here as they're only needed
	// to make the address struct, which we're ignoring anyway (scripts are
	// always the same, it's addresses that change across chains).
	scriptClass, _, _, _ := txscript.ExtractPkScriptAddrs(
		pkScript, &chaincfg.MainNetParams,
	)

	return scriptClass
}

// addScriptWeight parses the passed pkScript and adds the computed weight cost
// were the script to be added to the justice transaction.
func addScriptWeight(weightEstimate *input.TxWeightEstimator,
	pkScript []byte) error {

	switch addrType(pkScript) {
	case txscript.WitnessV0PubKeyHashTy:
		weightEstimate.AddP2WKHOutput()

	case txscript.WitnessV0ScriptHashTy:
		weightEstimate.AddP2WSHOutput()

	case txscript.WitnessV1TaprootTy:
		weightEstimate.AddP2TROutput()

	default:
		return fmt.Errorf("invalid addr type: %v", addrType(pkScript))
	}

	return nil
}

// bindSession first populates all state-dependent variables of the task. Then
// it determines if the backupTask is compatible with the passed SessionInfo's
// policy. If no error is returned, the task has been bound to the session and
// can be queued to upload to the tower. Otherwise, the bind failed and should
// be rescheduled with a different session.
func (t *backupTask) bindSession(session *wtdb.ClientSessionBody,
	newBreachRetribution BreachRetributionBuilder) error {

	breachInfo, chanType, err := newBreachRetribution(
		t.id.ChanID, t.id.CommitHeight,
	)
	if err != nil {
		return err
	}

	commitType, err := session.Policy.BlobType.CommitmentType(&chanType)
	if err != nil {
		return err
	}

	// Parse the non-dust outputs from the breach transaction,
	// simultaneously computing the total amount contained in the inputs
	// present. We can't compute the exact output values at this time
	// since the task has not been assigned to a session, at which point
	// parameters such as fee rate, number of outputs, and reward rate will
	// be finalized.
	var (
		totalAmt      int64
		toLocalInput  input.Input
		toRemoteInput input.Input
	)

	// Add the sign descriptors and outputs corresponding to the to-local
	// and to-remote outputs, respectively, if either input amount is
	// non-dust. Note that the naming here seems reversed, but both are
	// correct. For example, the to-remote output on the remote party's
	// commitment is an output that pays to us. Hence the retribution refers
	// to that output as local, though relative to their commitment, it is
	// paying to-the-remote party (which is us).
	if breachInfo.RemoteOutputSignDesc != nil {
		toLocalInput, err = commitType.ToLocalInput(breachInfo)
		if err != nil {
			return err
		}

		totalAmt += breachInfo.RemoteOutputSignDesc.Output.Value
	}
	if breachInfo.LocalOutputSignDesc != nil {
		toRemoteInput, err = commitType.ToRemoteInput(breachInfo)
		if err != nil {
			return err
		}

		totalAmt += breachInfo.LocalOutputSignDesc.Output.Value
	}

	t.commitmentType = commitType
	t.breachInfo = breachInfo
	t.toLocalInput = toLocalInput
	t.toRemoteInput = toRemoteInput
	t.totalAmt = btcutil.Amount(totalAmt)

	// First we'll begin by deriving a weight estimate for the justice
	// transaction. The final weight can be different depending on whether
	// the watchtower is taking a reward.
	var weightEstimate input.TxWeightEstimator

	// Next, add the contribution from the inputs that are present on this
	// breach transaction.
	if t.toLocalInput != nil {
		toLocalWitnessSize, err := commitType.ToLocalWitnessSize()
		if err != nil {
			return err
		}

		weightEstimate.AddWitnessInput(toLocalWitnessSize)
	}
	if t.toRemoteInput != nil {
		toRemoteWitnessSize, err := commitType.ToRemoteWitnessSize()
		if err != nil {
			return err
		}

		weightEstimate.AddWitnessInput(toRemoteWitnessSize)
	}

	// All justice transactions will either use segwit v0 (p2wkh + p2wsh)
	// or segwit v1 (p2tr).
	err = addScriptWeight(&weightEstimate, t.sweepPkScript)
	if err != nil {
		return err
	}

	// If the justice transaction has a reward output, add the output's
	// contribution to the weight estimate.
	if session.Policy.BlobType.Has(blob.FlagReward) {
		err := addScriptWeight(&weightEstimate, session.RewardPkScript)
		if err != nil {
			return err
		}
	}

	// Now, compute the output values depending on whether FlagReward is set
	// in the current session's policy.
	outputs, err := session.Policy.ComputeJusticeTxOuts(
		t.totalAmt, weightEstimate.Weight(),
		t.sweepPkScript, session.RewardPkScript,
	)
	if err != nil {
		return err
	}

	t.blobType = session.Policy.BlobType
	t.outputs = outputs

	return nil
}

// craftSessionPayload is the final stage for a backupTask, and generates the
// encrypted payload and breach hint that should be sent to the tower. This
// method computes the final justice transaction using the bound
// session-dependent variables, and signs the resulting transaction. The
// required pieces from signatures, witness scripts, etc are then packaged into
// a JusticeKit and encrypted using the breach transaction's key.
func (t *backupTask) craftSessionPayload(
	signer input.Signer) (blob.BreachHint, []byte, error) {

	var hint blob.BreachHint

	justiceKit, err := t.commitmentType.NewJusticeKit(
		t.sweepPkScript, t.breachInfo, t.toRemoteInput != nil,
	)
	if err != nil {
		return hint, nil, err
	}

	// Now, begin construction of the justice transaction. We'll start with
	// a version 2 transaction.
	justiceTxn := wire.NewMsgTx(2)

	// Next, add the non-dust inputs that were derived from the breach
	// information. This will either be contain both the to-local and
	// to-remote outputs, or only be the to-local output.
	inputs := t.inputs()
	prevOutputFetcher := txscript.NewMultiPrevOutFetcher(nil)
	for prevOutPoint, inp := range inputs {
		prevOutputFetcher.AddPrevOut(
			prevOutPoint, inp.SignDesc().Output,
		)
		justiceTxn.AddTxIn(&wire.TxIn{
			PreviousOutPoint: prevOutPoint,
			Sequence:         inp.BlocksToMaturity(),
		})
	}

	// Add the sweep output paying directly to the user and possibly a
	// reward output, using the outputs computed when the task was bound.
	justiceTxn.TxOut = t.outputs

	// Sort the justice transaction according to BIP69.
	txsort.InPlaceSort(justiceTxn)

	// Check that the justice transaction meets basic validity requirements
	// before attempting to attach the witnesses.
	btx := btcutil.NewTx(justiceTxn)
	if err := blockchain.CheckTransactionSanity(btx); err != nil {
		return hint, nil, err
	}

	// Construct a sighash cache to improve signing performance.
	hashCache := txscript.NewTxSigHashes(justiceTxn, prevOutputFetcher)

	// Since the transaction inputs could have been reordered as a result of
	// the BIP69 sort, create an index mapping each prevout to it's new
	// index.
	inputIndex := make(map[wire.OutPoint]int)
	for i, txIn := range justiceTxn.TxIn {
		inputIndex[txIn.PreviousOutPoint] = i
	}

	// Now, iterate through the list of inputs that were initially added to
	// the transaction and store the computed witness within the justice
	// kit.
	commitType := t.commitmentType
	for _, inp := range inputs {
		// Lookup the input's new post-sort position.
		i := inputIndex[inp.OutPoint()]

		// Construct the full witness required to spend this input.
		inputScript, err := inp.CraftInputScript(
			signer, justiceTxn, hashCache, prevOutputFetcher, i,
		)
		if err != nil {
			return hint, nil, err
		}

		signature, err := commitType.ParseRawSig(inputScript.Witness)
		if err != nil {
			return hint, nil, err
		}

		toLocalWitnessType, err := commitType.ToLocalWitnessType()
		if err != nil {
			return hint, nil, err
		}

		toRemoteWitnessType, err := commitType.ToRemoteWitnessType()
		if err != nil {
			return hint, nil, err
		}

		// Finally, copy the serialized signature into the justice kit,
		// using the input's witness type to select the appropriate
		// field
		switch inp.WitnessType() {
		case toLocalWitnessType:
			justiceKit.AddToLocalSig(signature)

		case toRemoteWitnessType:
			justiceKit.AddToRemoteSig(signature)
		default:
			return hint, nil, fmt.Errorf("invalid witness type: %v",
				inp.WitnessType())
		}
	}

	breachTxID := t.breachInfo.BreachTxHash

	// Compute the breach hint as SHA256(txid)[:16] and breach key as
	// SHA256(txid || txid).
	hint, key := blob.NewBreachHintAndKeyFromHash(&breachTxID)

	// Then, we'll encrypt the computed justice kit using the full breach
	// transaction id, which will allow the tower to recover the contents
	// after the transaction is seen in the chain or mempool.
	encBlob, err := blob.Encrypt(justiceKit, key)
	if err != nil {
		return hint, nil, err
	}

	return hint, encBlob, nil
}
