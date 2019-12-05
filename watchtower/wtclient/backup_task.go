package wtclient

import (
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/txsort"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
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
	id         wtdb.BackupID
	breachInfo *lnwallet.BreachRetribution

	// state-dependent variables

	toLocalInput  input.Input
	toRemoteInput input.Input
	totalAmt      btcutil.Amount
	sweepPkScript []byte

	// session-dependent variables

	blobType blob.Type
	outputs  []*wire.TxOut
}

// newBackupTask initializes a new backupTask and populates all state-dependent
// variables.
func newBackupTask(chanID *lnwire.ChannelID,
	breachInfo *lnwallet.BreachRetribution,
	sweepPkScript []byte, isTweakless bool) *backupTask {

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
		toLocalInput = input.NewBaseInput(
			&breachInfo.RemoteOutpoint,
			input.CommitmentRevoke,
			breachInfo.RemoteOutputSignDesc,
			0,
		)
		totalAmt += breachInfo.RemoteOutputSignDesc.Output.Value
	}
	if breachInfo.LocalOutputSignDesc != nil {
		witnessType := input.CommitmentNoDelay
		if isTweakless {
			witnessType = input.CommitSpendNoDelayTweakless
		}

		toRemoteInput = input.NewBaseInput(
			&breachInfo.LocalOutpoint,
			witnessType,
			breachInfo.LocalOutputSignDesc,
			0,
		)

		totalAmt += breachInfo.LocalOutputSignDesc.Output.Value
	}

	return &backupTask{
		id: wtdb.BackupID{
			ChanID:       *chanID,
			CommitHeight: breachInfo.RevokedStateNum,
		},
		breachInfo:    breachInfo,
		toLocalInput:  toLocalInput,
		toRemoteInput: toRemoteInput,
		totalAmt:      btcutil.Amount(totalAmt),
		sweepPkScript: sweepPkScript,
	}
}

// inputs returns all non-dust inputs that we will attempt to spend from.
//
// NOTE: Ordering of the inputs is not critical as we sort the transaction with
// BIP69.
func (t *backupTask) inputs() map[wire.OutPoint]input.Input {
	inputs := make(map[wire.OutPoint]input.Input)
	if t.toLocalInput != nil {
		inputs[*t.toLocalInput.OutPoint()] = t.toLocalInput
	}
	if t.toRemoteInput != nil {
		inputs[*t.toRemoteInput.OutPoint()] = t.toRemoteInput
	}
	return inputs
}

// bindSession determines if the backupTask is compatible with the passed
// SessionInfo's policy. If no error is returned, the task has been bound to the
// session and can be queued to upload to the tower. Otherwise, the bind failed
// and should be rescheduled with a different session.
func (t *backupTask) bindSession(session *wtdb.ClientSessionBody) error {
	// First we'll begin by deriving a weight estimate for the justice
	// transaction. The final weight can be different depending on whether
	// the watchtower is taking a reward.
	var weightEstimate input.TxWeightEstimator

	// Next, add the contribution from the inputs that are present on this
	// breach transaction.
	if t.toLocalInput != nil {
		weightEstimate.AddWitnessInput(input.ToLocalPenaltyWitnessSize)
	}
	if t.toRemoteInput != nil {
		weightEstimate.AddWitnessInput(input.P2WKHWitnessSize)
	}

	// All justice transactions have a p2wkh output paying to the victim.
	weightEstimate.AddP2WKHOutput()

	// If the justice transaction has a reward output, add the output's
	// contribution to the weight estimate.
	if session.Policy.BlobType.Has(blob.FlagReward) {
		weightEstimate.AddP2WKHOutput()
	}

	// Now, compute the output values depending on whether FlagReward is set
	// in the current session's policy.
	outputs, err := session.Policy.ComputeJusticeTxOuts(
		t.totalAmt, int64(weightEstimate.Weight()),
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

	// First, copy over the sweep pkscript, the pubkeys used to derive the
	// to-local script, and the remote CSV delay.
	keyRing := t.breachInfo.KeyRing
	justiceKit := &blob.JusticeKit{
		SweepAddress:     t.sweepPkScript,
		RevocationPubKey: toBlobPubKey(keyRing.RevocationKey),
		LocalDelayPubKey: toBlobPubKey(keyRing.LocalKey),
		CSVDelay:         t.breachInfo.RemoteDelay,
	}

	// If this commitment has an output that pays to us, copy the to-remote
	// pubkey into the justice kit. This serves as the indicator to the
	// tower that we expect the breaching transaction to have a non-dust
	// output to spend from.
	if t.toRemoteInput != nil {
		justiceKit.CommitToRemotePubKey = toBlobPubKey(
			keyRing.RemoteKey,
		)
	}

	// Now, begin construction of the justice transaction. We'll start with
	// a version 2 transaction.
	justiceTxn := wire.NewMsgTx(2)

	// Next, add the non-dust inputs that were derived from the breach
	// information. This will either be contain both the to-local and
	// to-remote outputs, or only be the to-local output.
	inputs := t.inputs()
	for prevOutPoint := range inputs {
		justiceTxn.AddTxIn(&wire.TxIn{
			PreviousOutPoint: prevOutPoint,
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
	hashCache := txscript.NewTxSigHashes(justiceTxn)

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
	for _, inp := range inputs {
		// Lookup the input's new post-sort position.
		i := inputIndex[*inp.OutPoint()]

		// Construct the full witness required to spend this input.
		inputScript, err := inp.CraftInputScript(
			signer, justiceTxn, hashCache, i,
		)
		if err != nil {
			return hint, nil, err
		}

		// Parse the DER-encoded signature from the first position of
		// the resulting witness. We trim an extra byte to remove the
		// sighash flag.
		witness := inputScript.Witness
		rawSignature := witness[0][:len(witness[0])-1]

		// Reencode the DER signature into a fixed-size 64 byte
		// signature.
		signature, err := lnwire.NewSigFromRawSignature(rawSignature)
		if err != nil {
			return hint, nil, err
		}

		// Finally, copy the serialized signature into the justice kit,
		// using the input's witness type to select the appropriate
		// field.
		switch inp.WitnessType() {
		case input.CommitmentRevoke:
			copy(justiceKit.CommitToLocalSig[:], signature[:])

		case input.CommitSpendNoDelayTweakless:
			fallthrough
		case input.CommitmentNoDelay:
			copy(justiceKit.CommitToRemoteSig[:], signature[:])
		}
	}

	breachTxID := t.breachInfo.BreachTransaction.TxHash()

	// Compute the breach key as SHA256(txid).
	hint, key := blob.NewBreachHintAndKeyFromHash(&breachTxID)

	// Then, we'll encrypt the computed justice kit using the full breach
	// transaction id, which will allow the tower to recover the contents
	// after the transaction is seen in the chain or mempool.
	encBlob, err := justiceKit.Encrypt(key, t.blobType)
	if err != nil {
		return hint, nil, err
	}

	return hint, encBlob, nil
}

// toBlobPubKey serializes the given pubkey into a blob.PubKey that can be set
// as a field on a blob.JusticeKit.
func toBlobPubKey(pubKey *btcec.PublicKey) blob.PubKey {
	var blobPubKey blob.PubKey
	copy(blobPubKey[:], pubKey.SerializeCompressed())
	return blobPubKey
}
