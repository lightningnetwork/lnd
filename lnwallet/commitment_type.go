package lnwallet

import (
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/commitmenttx"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ScriptIinfo holds a reedeem script and hash.
type ScriptInfo struct {
	// PkScript is the outputs' PkScript.
	PkScript []byte

	// WitnessScript is the full script required to properly redeem the
	// output with PkSript. This field will only be populated if a PkScript
	// is p2wsh or p2sh.
	WitnessScript []byte
}

// CommitmentType is a type that wraps the type of channel we are dealine with,
// and abstracts the various ways of constructing commitment transactions. It
// can be used to derive keys, scripts and outputs depending on the channel's
// commitment type.
type CommitmentType struct {
	chanType channeldb.ChannelType

	localChanCfg, remoteChanCfg *channeldb.ChannelConfig

	isInitiator bool

	anchorSize btcutil.Amount

	fundingTxIn         wire.TxIn
	stateHintObfuscator [StateHintSize]byte
}

// NewCommitmentType creates a new CommitmentType based on the ChannelType.
func NewCommitmentType(chanType channeldb.ChannelType) CommitmentType {
	c := CommitmentType{
		chanType: chanType,
	}

	// The anchor chennel type MUST be tweakless.
	if chanType.HasAnchors() && !chanType.IsTweakless() {
		panic("invalid channel type combination")
	}

	return c
}

// DeriveCommitmentKeys generates a new commitment key set using the base
// points and commitment point for the this commitment type.
func (c CommitmentType) DeriveCommitmentKeys(commitPoint *btcec.PublicKey,
	isOurCommit bool, localChanCfg,
	remoteChanCfg *channeldb.ChannelConfig) *commitmenttx.KeyRing {

	// Return commitment keys with tweaklessCommit set according to channel
	// type.
	return commitmenttx.DeriveCommitmentKeys(
		commitPoint, isOurCommit, c.chanType.IsTweakless(),
		localChanCfg, remoteChanCfg,
	)
}

// CommitScriptToRemote creates the script that will pay to the non-owner of
// the commitment transaction, adding a delay to the script based on the
// commitment type.
func (c CommitmentType) CommitScriptToRemote(csvTimeout uint32,
	key *btcec.PublicKey) (*ScriptInfo, error) {

	// If this channel type has anchors, we derive the delayed to_remote
	// script.
	if c.chanType.HasAnchors() {
		script, err := input.CommitScriptToRemote(csvTimeout, key)
		if err != nil {
			return nil, err
		}

		p2wsh, err := input.WitnessScriptHash(script)
		if err != nil {
			return nil, err
		}

		return &ScriptInfo{
			PkScript:      p2wsh,
			WitnessScript: script,
		}, nil
	}

	// Otherwise the te_remote will be a simple p2wkh.
	p2wkh, err := input.CommitScriptUnencumbered(key)
	if err != nil {
		return nil, err
	}

	// Since this is a regular P2WKH, the WitnessScipt doesn't have to be
	// set.
	return &ScriptInfo{
		PkScript: p2wkh,
	}, nil
}

// CommitScriptAnchors return the scripts to use for the local and remote
// anchor. The returned values can be nil to indicate the ouputs shouldn't be
// added.
func (c CommitmentType) CommitScriptAnchors(localChanCfg,
	remoteChanCfg *channeldb.ChannelConfig) (*ScriptInfo, *ScriptInfo, error) {

	// If this channel type has no anchors we can return immediately.
	if c.chanType.HasAnchors() {
		return nil, nil, nil
	}

	// Then the anchor output spendable by the local node.
	localAnchorScript, err := input.CommitScriptAnchor(
		localChanCfg.MultiSigKey.PubKey,
	)
	if err != nil {
		return nil, nil, err
	}

	localAnchorScriptHash, err := input.WitnessScriptHash(localAnchorScript)
	if err != nil {
		return nil, nil, err
	}

	// And the anchor spemdable by the remote.
	remoteAnchorScript, err := input.CommitScriptAnchor(
		remoteChanCfg.MultiSigKey.PubKey,
	)
	if err != nil {
		return nil, nil, err
	}

	remoteAnchorScriptHash, err := input.WitnessScriptHash(
		remoteAnchorScript,
	)
	if err != nil {
		return nil, nil, err
	}

	return &ScriptInfo{
			PkScript:      localAnchorScript,
			WitnessScript: localAnchorScriptHash,
		},
		&ScriptInfo{
			PkScript:      remoteAnchorScript,
			WitnessScript: remoteAnchorScriptHash,
		}, nil
}

// CommitWeight returns the base commitment weight before adding HTLCs.
func (c CommitmentType) CommitWeight() int64 {
	// If this commitment has anchors, it will be slightly heavier.
	if c.chanType.HasAnchors() {
		return input.AnchorCommitWeight
	}

	return input.CommitWeight
}

// createCommitmentTx generates the unsigned commitment transaction for a
// commitment view and assigns to txn field.
func (ct CommitmentType) createCommitmentTx(c *commitment,
	filteredHTLCView *htlcView, keyRing *commitmenttx.KeyRing) error {

	ourBalance := c.ourBalance
	theirBalance := c.theirBalance

	numHTLCs := int64(0)
	for _, htlc := range filteredHTLCView.ourUpdates {
		if htlcIsDust(false, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit) {

			continue
		}

		numHTLCs++
	}
	for _, htlc := range filteredHTLCView.theirUpdates {
		if htlcIsDust(true, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit) {

			continue
		}

		numHTLCs++
	}

	// Next, we'll calculate the fee for the commitment transaction based
	// on its total weight. Once we have the total weight, we'll multiply
	// by the current fee-per-kw, then divide by 1000 to get the proper
	// fee.
	totalCommitWeight := ct.CommitWeight() +
		input.HTLCWeight*numHTLCs

	// With the weight known, we can now calculate the commitment fee,
	// ensuring that we account for any dust outputs trimmed above.
	commitFee := c.feePerKw.FeeForWeight(totalCommitWeight)

	// If the current commitment type has anchors (AnchorSize is non-zero)
	// it will also be paid by the initiator.
	initiatorFee := commitFee + 2*ct.anchorSize
	initiatorFeeMSat := lnwire.NewMSatFromSatoshis(initiatorFee)

	// The commitment has one to_local_anchor output, one timelocked
	// to_local output, and one to_remote output.  The commitment will have
	// a minimum fee, and this fee is always paid by the initiator. This
	// fee is subracted from the node's output, resulting in it being set
	// to 0 if below the dust limit.
	switch {
	case ct.isInitiator && initiatorFee > ourBalance.ToSatoshis():
		ourBalance = 0

	case ct.isInitiator:
		ourBalance -= initiatorFeeMSat

	case !ct.isInitiator && initiatorFee > theirBalance.ToSatoshis():
		theirBalance = 0

	case !ct.isInitiator:
		theirBalance -= initiatorFeeMSat
	}

	var (
		localCfg, remoteCfg         *channeldb.ChannelConfig
		localBalance, remoteBalance btcutil.Amount
	)
	if c.isOurs {
		localCfg = ct.localChanCfg
		remoteCfg = ct.remoteChanCfg
		localBalance = ourBalance.ToSatoshis()
		remoteBalance = theirBalance.ToSatoshis()
	} else {
		localCfg = ct.remoteChanCfg
		remoteCfg = ct.localChanCfg
		localBalance = theirBalance.ToSatoshis()
		remoteBalance = ourBalance.ToSatoshis()
	}

	// Check whether there are any htlcs left in the view. We need this to
	// determing whether to add anchor outputs.
	hasHtlcs := false
	for _, htlc := range filteredHTLCView.ourUpdates {
		if htlcIsDust(false, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), localCfg.DustLimit) {
			continue
		}

		hasHtlcs = true
		break
	}
	for _, htlc := range filteredHTLCView.theirUpdates {
		if htlcIsDust(true, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), localCfg.DustLimit) {
			continue
		}

		hasHtlcs = true
		break
	}

	// Generate a new commitment transaction with all the latest
	// unsettled/un-timed out HTLCs.
	commitTx, err := CreateCommitTx(
		ct, ct.fundingTxIn, keyRing, localCfg, remoteCfg,
		localBalance, remoteBalance, ct.anchorSize, hasHtlcs,
	)
	if err != nil {
		return err
	}

	// We'll now add all the HTLC outputs to the commitment transaction.
	// Each output includes an off-chain 2-of-2 covenant clause, so we'll
	// need the objective local/remote keys for this particular commitment
	// as well. For any non-dust HTLCs that are manifested on the commitment
	// transaction, we'll also record its CLTV which is required to sort the
	// commitment transaction below. The slice is initially sized to the
	// number of existing outputs, since any outputs already added are
	// commitment outputs and should correspond to zero values for the
	// purposes of sorting.
	cltvs := make([]uint32, len(commitTx.TxOut))
	for _, htlc := range filteredHTLCView.ourUpdates {
		if htlcIsDust(false, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit) {
			continue
		}

		err := addHTLC(commitTx, c.isOurs, false, htlc, keyRing)
		if err != nil {
			return err
		}
		cltvs = append(cltvs, htlc.Timeout)
	}
	for _, htlc := range filteredHTLCView.theirUpdates {
		if htlcIsDust(true, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit) {
			continue
		}

		err := addHTLC(commitTx, c.isOurs, true, htlc, keyRing)
		if err != nil {
			return err
		}
		cltvs = append(cltvs, htlc.Timeout)
	}

	// Set the state hint of the commitment transaction to facilitate
	// quickly recovering the necessary penalty state in the case of an
	// uncooperative broadcast.
	err = SetStateNumHint(commitTx, c.height, ct.stateHintObfuscator)
	if err != nil {
		return err
	}

	// Sort the transactions according to the agreed upon canonical
	// ordering. This lets us skip sending the entire transaction over,
	// instead we'll just send signatures.
	InPlaceCommitSort(commitTx, cltvs)

	// Next, we'll ensure that we don't accidentally create a commitment
	// transaction which would be invalid by consensus.
	uTx := btcutil.NewTx(commitTx)
	if err := blockchain.CheckTransactionSanity(uTx); err != nil {
		return err
	}

	// Finally, we'll assert that were not attempting to draw more out of
	// the channel that was originally placed within it.
	var totalOut btcutil.Amount
	for _, txOut := range commitTx.TxOut {
		totalOut += btcutil.Amount(txOut.Value)
	}
	//	if totalOut > ct.channelState.Capacity {
	//		return fmt.Errorf("height=%v, for ChannelPoint(%v) attempts "+
	//			"to consume %v while channel capacity is %v",
	//			c.height, ct..FundingOutpoint,
	//			totalOut, lc.channelState.Capacity)
	//	}

	c.txn = commitTx
	c.fee = commitFee
	c.ourBalance = ourBalance
	c.theirBalance = theirBalance
	return nil
}

// CreateCommitTx creates a commitment transaction, spending from specified
// funding output. The commitment transaction contains two outputs: one local
// output paying to the "owner" of the commitment transaction which can be
// spent after a relative block delay or revocation event, and a remote output
// paying the counterparty within the channel, which can be spent immediately
// or after a delay depending on the commitment type..
func CreateCommitTx(commitType CommitmentType,
	fundingOutput wire.TxIn, keyRing *commitmenttx.KeyRing,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig,
	amountToLocal, amountToRemote, anchorSize btcutil.Amount,
	hasHtlcs bool) (*wire.MsgTx, error) {

	// First, we create the script for the delayed "pay-to-self" output.
	// This output has 2 main redemption clauses: either we can redeem the
	// output after a relative block delay, or the remote node can claim
	// the funds with the revocation key if we broadcast a revoked
	// commitment transaction.
	toLocalRedeemScript, err := input.CommitScriptToSelf(
		uint32(localChanCfg.CsvDelay), keyRing.LocalKey,
		keyRing.RevocationKey,
	)
	if err != nil {
		return nil, err
	}
	toLocalScriptHash, err := input.WitnessScriptHash(
		toLocalRedeemScript,
	)
	if err != nil {
		return nil, err
	}

	// Next, we create the script paying to the remote.
	toRemoteScript, err := commitType.CommitScriptToRemote(
		uint32(remoteChanCfg.CsvDelay), keyRing.RemoteKey,
	)
	if err != nil {
		return nil, err
	}

	// Get the anchor outputs if any.
	localAnchor, remoteAnchor, err := commitType.CommitScriptAnchors(
		localChanCfg, remoteChanCfg,
	)
	if err != nil {
		return nil, err
	}

	// Now that both output scripts have been created, we can finally create
	// the transaction itself. We use a transaction version of 2 since CSV
	// will fail unless the tx version is >= 2.
	commitTx := wire.NewMsgTx(2)
	commitTx.AddTxIn(&fundingOutput)

	// Avoid creating dust outputs within the commitment transaction.
	if amountToLocal >= localChanCfg.DustLimit {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: toLocalScriptHash,
			Value:    int64(amountToLocal),
		})
	}

	if amountToRemote >= localChanCfg.DustLimit {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: toRemoteScript.PkScript,
			Value:    int64(amountToRemote),
		})
	}

	// Add local anchor output only if we have a commitment output
	// or there are HTLCs.
	localStake := amountToLocal >= localChanCfg.DustLimit || hasHtlcs

	// Some commitment types don't have anchors, so check if it is non-nil.
	if localAnchor != nil && localStake {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: localAnchor.PkScript,
			Value:    int64(anchorSize),
		})
	}

	// Add anchor output to remote only if they have a commitment output or
	// there are HTLCs.
	remoteStake := amountToRemote >= localChanCfg.DustLimit || hasHtlcs
	if remoteAnchor != nil && remoteStake {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: remoteAnchor.PkScript,
			Value:    int64(anchorSize),
		})
	}

	return commitTx, nil
}
