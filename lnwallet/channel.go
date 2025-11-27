package lnwallet

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/txsort"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/mempool"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// ErrChanClosing is returned when a caller attempts to close a channel
	// that has already been closed or is in the process of being closed.
	ErrChanClosing = fmt.Errorf("channel is being closed, operation disallowed")

	// ErrNoWindow is returned when revocation window is exhausted.
	ErrNoWindow = fmt.Errorf("unable to sign new commitment, the current" +
		" revocation window is exhausted")

	// ErrMaxWeightCost is returned when the cost/weight (see segwit)
	// exceeds the widely used maximum allowed policy weight limit. In this
	// case the commitment transaction can't be propagated through the
	// network.
	ErrMaxWeightCost = fmt.Errorf("commitment transaction exceed max " +
		"available cost")

	// ErrMaxHTLCNumber is returned when a proposed HTLC would exceed the
	// maximum number of allowed HTLC's if committed in a state transition
	ErrMaxHTLCNumber = fmt.Errorf("commitment transaction exceed max " +
		"htlc number")

	// ErrMaxPendingAmount is returned when a proposed HTLC would exceed
	// the overall maximum pending value of all HTLCs if committed in a
	// state transition.
	ErrMaxPendingAmount = fmt.Errorf("commitment transaction exceed max" +
		"overall pending htlc value")

	// ErrBelowChanReserve is returned when a proposed HTLC would cause
	// one of the peer's funds to dip below the channel reserve limit.
	ErrBelowChanReserve = fmt.Errorf("commitment transaction dips peer " +
		"below chan reserve")

	// ErrBelowMinHTLC is returned when a proposed HTLC has a value that
	// is below the minimum HTLC value constraint for either us or our
	// peer depending on which flags are set.
	ErrBelowMinHTLC = fmt.Errorf("proposed HTLC value is below minimum " +
		"allowed HTLC value")

	// ErrFeeBufferNotInitiator is returned when the FeeBuffer is enforced
	// although the channel was not initiated (opened) locally.
	ErrFeeBufferNotInitiator = fmt.Errorf("unable to enforce FeeBuffer, " +
		"not initiator of the channel")

	// ErrInvalidHTLCAmt signals that a proposed HTLC has a value that is
	// not positive.
	ErrInvalidHTLCAmt = fmt.Errorf("proposed HTLC value must be positive")

	// ErrCannotSyncCommitChains is returned if, upon receiving a ChanSync
	// message, the state machine deems that is unable to properly
	// synchronize states with the remote peer. In this case we should fail
	// the channel, but we won't automatically force close.
	ErrCannotSyncCommitChains = fmt.Errorf("unable to sync commit chains")

	// ErrInvalidLastCommitSecret is returned in the case that the
	// commitment secret sent by the remote party in their
	// ChannelReestablish message doesn't match the last secret we sent.
	ErrInvalidLastCommitSecret = fmt.Errorf("commit secret is incorrect")

	// ErrInvalidLocalUnrevokedCommitPoint is returned in the case that the
	// commitment point sent by the remote party in their
	// ChannelReestablish message doesn't match the last unrevoked commit
	// point they sent us.
	ErrInvalidLocalUnrevokedCommitPoint = fmt.Errorf("unrevoked commit " +
		"point is invalid")

	// ErrCommitSyncRemoteDataLoss is returned in the case that we receive
	// a ChannelReestablish message from the remote that advertises a
	// NextLocalCommitHeight that is lower than what they have already
	// ACKed, or a RemoteCommitTailHeight that is lower than our revoked
	// height. In this case we should force close the channel such that
	// both parties can retrieve their funds.
	ErrCommitSyncRemoteDataLoss = fmt.Errorf("possible remote commitment " +
		"state data loss")

	// ErrNoRevocationLogFound is returned when both the returned logs are
	// nil from querying the revocation log bucket. In theory this should
	// never happen as the query will return `ErrLogEntryNotFound`, yet
	// we'd still perform a sanity check to make sure at least one of the
	// logs is non-nil.
	ErrNoRevocationLogFound = errors.New("no revocation log found")

	// ErrOutputIndexOutOfRange is returned when an output index is greater
	// than or equal to the length of a given transaction's outputs.
	ErrOutputIndexOutOfRange = errors.New("output index is out of range")

	// ErrRevLogDataMissing is returned when a certain wanted optional field
	// in a revocation log entry is missing.
	ErrRevLogDataMissing = errors.New("revocation log data missing")

	// ErrForceCloseLocalDataLoss is returned in the case a user (or
	// another sub-system) attempts to force close when we've detected that
	// we've likely lost data ourselves.
	ErrForceCloseLocalDataLoss = errors.New("cannot force close " +
		"channel with local data loss")

	// errNoNonce is returned when a nonce is required, but none is found.
	errNoNonce = errors.New("no nonce found")

	// errNoPartialSig is returned when a partial signature is required,
	// but none is found.
	errNoPartialSig = errors.New("no partial signature found")

	// errQuit is returned when a quit signal was received, interrupting the
	// current operation.
	errQuit = errors.New("received quit signal")
)

// ErrCommitSyncLocalDataLoss is returned in the case that we receive a valid
// commit secret within the ChannelReestablish message from the remote node AND
// they advertise a RemoteCommitTailHeight higher than our current known
// height. This means we have lost some critical data, and must fail the
// channel and MUST NOT force close it. Instead we should wait for the remote
// to force close it, such that we can attempt to sweep our funds. The
// commitment point needed to sweep the remote's force close is encapsulated.
type ErrCommitSyncLocalDataLoss struct {
	// ChannelPoint is the identifier for the channel that experienced data
	// loss.
	ChannelPoint wire.OutPoint

	// CommitPoint is the last unrevoked commit point, sent to us by the
	// remote when we determined we had lost state.
	CommitPoint *btcec.PublicKey
}

// Error returns a string representation of the local data loss error.
func (e *ErrCommitSyncLocalDataLoss) Error() string {
	return fmt.Sprintf("ChannelPoint(%v) with CommitPoint(%x) had "+
		"possible local commitment state data loss", e.ChannelPoint,
		e.CommitPoint.SerializeCompressed())
}

// PaymentHash represents the sha256 of a random value. This hash is used to
// uniquely track incoming/outgoing payments within this channel, as well as
// payments requested by the wallet/daemon.
type PaymentHash [32]byte

// commitment represents a commitment to a new state within an active channel.
// New commitments can be initiated by either side. Commitments are ordered
// into a commitment chain, with one existing for both parties. Each side can
// independently extend the other side's commitment chain, up to a certain
// "revocation window", which once reached, disallows new commitments until
// the local nodes receives the revocation for the remote node's chain tail.
type commitment struct {
	// height represents the commitment height of this commitment, or the
	// update number of this commitment.
	height uint64

	// whoseCommit indicates whether this is the local or remote node's
	// version of the commitment.
	whoseCommit lntypes.ChannelParty

	// [our|their]MessageIndex are indexes into the HTLC log, up to which
	// this commitment transaction includes. These indexes allow both sides
	// to independently, and concurrent send create new commitments. Each
	// new commitment sent to the remote party includes an index in the
	// shared log which details which of their updates we're including in
	// this new commitment.
	messageIndices lntypes.Dual[uint64]

	// [our|their]HtlcIndex are the current running counters for the HTLCs
	// offered by either party. This value is incremented each time a party
	// offers a new HTLC. The log update methods that consume HTLCs will
	// reference these counters, rather than the running cumulative message
	// counters.
	ourHtlcIndex   uint64
	theirHtlcIndex uint64

	// txn is the commitment transaction generated by including any HTLC
	// updates whose index are below the two indexes listed above. If this
	// commitment is being added to the remote chain, then this txn is
	// their version of the commitment transactions. If the local commit
	// chain is being modified, the opposite is true.
	txn *wire.MsgTx

	// sig is a signature for the above commitment transaction.
	sig []byte

	// [our|their]Balance represents the settled balances at this point
	// within the commitment chain. This balance is computed by properly
	// evaluating all the add/remove/settle log entries before the listed
	// indexes.
	//
	// NOTE: This is the balance *after* subtracting any commitment fee,
	// AND anchor output values.
	ourBalance   lnwire.MilliSatoshi
	theirBalance lnwire.MilliSatoshi

	// fee is the amount that will be paid as fees for this commitment
	// transaction. The fee is recorded here so that it can be added back
	// and recalculated for each new update to the channel state.
	fee btcutil.Amount

	// feePerKw is the fee per kw used to calculate this commitment
	// transaction's fee.
	feePerKw chainfee.SatPerKWeight

	// dustLimit is the limit on the commitment transaction such that no
	// output values should be below this amount.
	dustLimit btcutil.Amount

	// outgoingHTLCs is a slice of all the outgoing HTLC's (from our PoV)
	// on this commitment transaction.
	outgoingHTLCs []paymentDescriptor

	// incomingHTLCs is a slice of all the incoming HTLC's (from our PoV)
	// on this commitment transaction.
	incomingHTLCs []paymentDescriptor

	// customBlob stores opaque bytes that may be used by custom channels
	// to store extra data for a given commitment state.
	customBlob fn.Option[tlv.Blob]

	// [outgoing|incoming]HTLCIndex is an index that maps an output index
	// on the commitment transaction to the payment descriptor that
	// represents the HTLC output.
	//
	// NOTE: that these fields are only populated if this commitment state
	// belongs to the local node. These maps are used when validating any
	// HTLC signatures which are part of the local commitment state. We use
	// this map in order to locate the details needed to validate an HTLC
	// signature while iterating of the outputs in the local commitment
	// view.
	outgoingHTLCIndex map[int32]*paymentDescriptor
	incomingHTLCIndex map[int32]*paymentDescriptor
}

// locateOutputIndex is a small helper function to locate the output index of a
// particular HTLC within the current commitment transaction. The duplicate map
// passed in is to be retained for each output within the commitment
// transition.  This ensures that we don't assign multiple HTLCs to the same
// index within the commitment transaction.
func locateOutputIndex(p *paymentDescriptor, tx *wire.MsgTx,
	whoseCommit lntypes.ChannelParty, dups map[PaymentHash][]int32,
	cltvs []uint32) (int32, error) {

	// If this is their commitment transaction, we'll be trying to locate
	// their pkScripts, otherwise we'll be looking for ours. This is
	// required as the commitment states are asymmetric in order to ascribe
	// blame in the case of a contract breach.
	pkScript := p.theirPkScript
	if whoseCommit.IsLocal() {
		pkScript = p.ourPkScript
	}

	for i, txOut := range tx.TxOut {
		cltv := cltvs[i]

		if bytes.Equal(txOut.PkScript, pkScript) &&
			txOut.Value == int64(p.Amount.ToSatoshis()) &&
			cltv == p.Timeout {

			// If this payment hash and index has already been
			// found, then we'll continue in order to avoid any
			// duplicate indexes.
			if fn.Elem(int32(i), dups[p.RHash]) {
				continue
			}

			idx := int32(i)
			dups[p.RHash] = append(dups[p.RHash], idx)
			return idx, nil
		}
	}

	return 0, fmt.Errorf("unable to find htlc: script=%x, value=%v, "+
		"cltv=%v", pkScript, p.Amount, p.Timeout)
}

// populateHtlcIndexes modifies the set of HTLCs locked-into the target view
// to have full indexing information populated. This information is required as
// we need to keep track of the indexes of each HTLC in order to properly write
// the current state to disk, and also to locate the paymentDescriptor
// corresponding to HTLC outputs in the commitment transaction.
func (c *commitment) populateHtlcIndexes(chanType channeldb.ChannelType,
	cltvs []uint32) error {

	// First, we'll set up some state to allow us to locate the output
	// index of the all the HTLCs within the commitment transaction. We
	// must keep this index so we can validate the HTLC signatures sent to
	// us.
	dups := make(map[PaymentHash][]int32)
	c.outgoingHTLCIndex = make(map[int32]*paymentDescriptor)
	c.incomingHTLCIndex = make(map[int32]*paymentDescriptor)

	// populateIndex is a helper function that populates the necessary
	// indexes within the commitment view for a particular HTLC.
	populateIndex := func(htlc *paymentDescriptor, incoming bool) error {
		isDust := HtlcIsDust(
			chanType, incoming, c.whoseCommit, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit,
		)

		var err error
		switch {

		// If this is our commitment transaction, and this is a dust
		// output then we mark it as such using a -1 index.
		case c.whoseCommit.IsLocal() && isDust:
			htlc.localOutputIndex = -1

		// If this is the commitment transaction of the remote party,
		// and this is a dust output then we mark it as such using a -1
		// index.
		case c.whoseCommit.IsRemote() && isDust:
			htlc.remoteOutputIndex = -1

		// If this is our commitment transaction, then we'll need to
		// locate the output and the index so we can verify an HTLC
		// signatures.
		case c.whoseCommit.IsLocal():
			htlc.localOutputIndex, err = locateOutputIndex(
				htlc, c.txn, c.whoseCommit, dups, cltvs,
			)
			if err != nil {
				return err
			}

			// As this is our commitment transactions, we need to
			// keep track of the locations of each output on the
			// transaction so we can verify any HTLC signatures
			// sent to us after we construct the HTLC view.
			if incoming {
				c.incomingHTLCIndex[htlc.localOutputIndex] = htlc
			} else {
				c.outgoingHTLCIndex[htlc.localOutputIndex] = htlc
			}

		// Otherwise, this is there remote party's commitment
		// transaction and we only need to populate the remote output
		// index within the HTLC index.
		case c.whoseCommit.IsRemote():
			htlc.remoteOutputIndex, err = locateOutputIndex(
				htlc, c.txn, c.whoseCommit, dups, cltvs,
			)
			if err != nil {
				return err
			}

		default:
			return fmt.Errorf("invalid commitment configuration")
		}

		return nil
	}

	// Finally, we'll need to locate the index within the commitment
	// transaction of all the HTLC outputs. This index will be required
	// later when we write the commitment state to disk, and also when
	// generating signatures for each of the HTLC transactions.
	for i := 0; i < len(c.outgoingHTLCs); i++ {
		htlc := &c.outgoingHTLCs[i]
		if err := populateIndex(htlc, false); err != nil {
			return err
		}
	}
	for i := 0; i < len(c.incomingHTLCs); i++ {
		htlc := &c.incomingHTLCs[i]
		if err := populateIndex(htlc, true); err != nil {
			return err
		}
	}

	return nil
}

// toDiskCommit converts the target commitment into a format suitable to be
// written to disk after an accepted state transition.
func (c *commitment) toDiskCommit(
	whoseCommit lntypes.ChannelParty) *channeldb.ChannelCommitment {

	numHtlcs := len(c.outgoingHTLCs) + len(c.incomingHTLCs)

	commit := &channeldb.ChannelCommitment{
		CommitHeight:    c.height,
		LocalLogIndex:   c.messageIndices.Local,
		LocalHtlcIndex:  c.ourHtlcIndex,
		RemoteLogIndex:  c.messageIndices.Remote,
		RemoteHtlcIndex: c.theirHtlcIndex,
		LocalBalance:    c.ourBalance,
		RemoteBalance:   c.theirBalance,
		CommitFee:       c.fee,
		FeePerKw:        btcutil.Amount(c.feePerKw),
		CommitTx:        c.txn,
		CommitSig:       c.sig,
		Htlcs:           make([]channeldb.HTLC, 0, numHtlcs),
		CustomBlob:      c.customBlob,
	}

	for _, htlc := range c.outgoingHTLCs {
		outputIndex := htlc.localOutputIndex
		if whoseCommit.IsRemote() {
			outputIndex = htlc.remoteOutputIndex
		}

		h := channeldb.HTLC{
			RHash:         htlc.RHash,
			Amt:           htlc.Amount,
			RefundTimeout: htlc.Timeout,
			OutputIndex:   outputIndex,
			HtlcIndex:     htlc.HtlcIndex,
			LogIndex:      htlc.LogIndex,
			Incoming:      false,
			OnionBlob:     htlc.OnionBlob,
			BlindingPoint: htlc.BlindingPoint,
			CustomRecords: htlc.CustomRecords.Copy(),
		}

		if whoseCommit.IsLocal() && htlc.sig != nil {
			h.Signature = htlc.sig.Serialize()
		}

		commit.Htlcs = append(commit.Htlcs, h)
	}

	for _, htlc := range c.incomingHTLCs {
		outputIndex := htlc.localOutputIndex
		if whoseCommit.IsRemote() {
			outputIndex = htlc.remoteOutputIndex
		}

		h := channeldb.HTLC{
			RHash:         htlc.RHash,
			Amt:           htlc.Amount,
			RefundTimeout: htlc.Timeout,
			OutputIndex:   outputIndex,
			HtlcIndex:     htlc.HtlcIndex,
			LogIndex:      htlc.LogIndex,
			Incoming:      true,
			OnionBlob:     htlc.OnionBlob,
			BlindingPoint: htlc.BlindingPoint,
			CustomRecords: htlc.CustomRecords.Copy(),
		}
		if whoseCommit.IsLocal() && htlc.sig != nil {
			h.Signature = htlc.sig.Serialize()
		}

		commit.Htlcs = append(commit.Htlcs, h)
	}

	return commit
}

// diskHtlcToPayDesc converts an HTLC previously written to disk within a
// commitment state to the form required to manipulate in memory within the
// commitment struct and updateLog. This function is used when we need to
// restore commitment state written to disk back into memory once we need to
// restart a channel session.
func (lc *LightningChannel) diskHtlcToPayDesc(feeRate chainfee.SatPerKWeight,
	htlc *channeldb.HTLC, commitKeys lntypes.Dual[*CommitmentKeyRing],
	whoseCommit lntypes.ChannelParty,
	auxLeaf input.AuxTapLeaf) (paymentDescriptor, error) {

	// The proper pkScripts for this paymentDescriptor must be
	// generated so we can easily locate them within the commitment
	// transaction in the future.
	var (
		ourP2WSH, theirP2WSH                 []byte
		ourWitnessScript, theirWitnessScript []byte
		pd                                   paymentDescriptor
		chanType                             = lc.channelState.ChanType
	)

	// If the either output is dust from the local or remote node's
	// perspective, then we don't need to generate the scripts as we only
	// generate them in order to locate the outputs within the commitment
	// transaction. As we'll mark dust with a special output index in the
	// on-disk state snapshot.
	isDustLocal := HtlcIsDust(
		chanType, htlc.Incoming, lntypes.Local, feeRate,
		htlc.Amt.ToSatoshis(), lc.channelState.LocalChanCfg.DustLimit,
	)
	localCommitKeys := commitKeys.GetForParty(lntypes.Local)
	if !isDustLocal && localCommitKeys != nil {
		scriptInfo, err := genHtlcScript(
			chanType, htlc.Incoming, lntypes.Local,
			htlc.RefundTimeout, htlc.RHash, localCommitKeys,
			auxLeaf,
		)
		if err != nil {
			return pd, err
		}
		ourP2WSH = scriptInfo.PkScript()
		ourWitnessScript = scriptInfo.WitnessScriptToSign()
	}
	isDustRemote := HtlcIsDust(
		chanType, htlc.Incoming, lntypes.Remote, feeRate,
		htlc.Amt.ToSatoshis(), lc.channelState.RemoteChanCfg.DustLimit,
	)
	remoteCommitKeys := commitKeys.GetForParty(lntypes.Remote)
	if !isDustRemote && remoteCommitKeys != nil {
		scriptInfo, err := genHtlcScript(
			chanType, htlc.Incoming, lntypes.Remote,
			htlc.RefundTimeout, htlc.RHash, remoteCommitKeys,
			auxLeaf,
		)
		if err != nil {
			return pd, err
		}
		theirP2WSH = scriptInfo.PkScript()
		theirWitnessScript = scriptInfo.WitnessScriptToSign()
	}

	// Reconstruct the proper local/remote output indexes from the HTLC's
	// persisted output index depending on whose commitment we are
	// generating.
	var (
		localOutputIndex  int32
		remoteOutputIndex int32
	)
	if whoseCommit.IsLocal() {
		localOutputIndex = htlc.OutputIndex
	} else {
		remoteOutputIndex = htlc.OutputIndex
	}

	customRecords := htlc.CustomRecords.Copy()

	entryType := lc.entryTypeForHtlc(
		customRecords, lc.channelState.ChanType,
	)

	// With the scripts reconstructed (depending on if this is our commit
	// vs theirs or a pending commit for the remote party), we can now
	// re-create the original payment descriptor.
	return paymentDescriptor{
		ChanID:             lc.ChannelID(),
		RHash:              htlc.RHash,
		Timeout:            htlc.RefundTimeout,
		Amount:             htlc.Amt,
		EntryType:          entryType,
		HtlcIndex:          htlc.HtlcIndex,
		LogIndex:           htlc.LogIndex,
		OnionBlob:          htlc.OnionBlob,
		localOutputIndex:   localOutputIndex,
		remoteOutputIndex:  remoteOutputIndex,
		ourPkScript:        ourP2WSH,
		ourWitnessScript:   ourWitnessScript,
		theirPkScript:      theirP2WSH,
		theirWitnessScript: theirWitnessScript,
		BlindingPoint:      htlc.BlindingPoint,
		CustomRecords:      customRecords,
	}, nil
}

// extractPayDescs will convert all HTLC's present within a disk commit state
// to a set of incoming and outgoing payment descriptors. Once reconstructed,
// these payment descriptors can be re-inserted into the in-memory updateLog
// for each side.
func (lc *LightningChannel) extractPayDescs(feeRate chainfee.SatPerKWeight,
	htlcs []channeldb.HTLC, commitKeys lntypes.Dual[*CommitmentKeyRing],
	whoseCommit lntypes.ChannelParty,
	auxLeaves fn.Option[CommitAuxLeaves]) ([]paymentDescriptor,
	[]paymentDescriptor, error) {

	var (
		incomingHtlcs []paymentDescriptor
		outgoingHtlcs []paymentDescriptor
	)

	// For each included HTLC within this commitment state, we'll convert
	// the disk format into our in memory paymentDescriptor format,
	// partitioning based on if we offered or received the HTLC.
	for _, htlc := range htlcs {
		// TODO(roasbeef): set isForwarded to false for all? need to
		// persist state w.r.t to if forwarded or not, or can
		// inadvertently trigger replays

		htlc := htlc

		auxLeaf := fn.FlatMapOption(
			func(l CommitAuxLeaves) input.AuxTapLeaf {
				leaves := l.OutgoingHtlcLeaves
				if htlc.Incoming {
					leaves = l.IncomingHtlcLeaves
				}

				return leaves[htlc.HtlcIndex].AuxTapLeaf
			},
		)(auxLeaves)

		payDesc, err := lc.diskHtlcToPayDesc(
			feeRate, &htlc, commitKeys, whoseCommit, auxLeaf,
		)
		if err != nil {
			return incomingHtlcs, outgoingHtlcs, err
		}

		if htlc.Incoming {
			incomingHtlcs = append(incomingHtlcs, payDesc)
		} else {
			outgoingHtlcs = append(outgoingHtlcs, payDesc)
		}
	}

	return incomingHtlcs, outgoingHtlcs, nil
}

// diskCommitToMemCommit converts the on-disk commitment format to our
// in-memory commitment format which is needed in order to properly resume
// channel operations after a restart.
func (lc *LightningChannel) diskCommitToMemCommit(
	whoseCommit lntypes.ChannelParty,
	diskCommit *channeldb.ChannelCommitment, localCommitPoint,
	remoteCommitPoint *btcec.PublicKey) (*commitment, error) {

	// First, we'll need to re-derive the commitment key ring for each
	// party used within this particular state. If this is a pending commit
	// (we extended but weren't able to complete the commitment dance
	// before shutdown), then the localCommitPoint won't be set as we
	// haven't yet received a responding commitment from the remote party.
	var commitKeys lntypes.Dual[*CommitmentKeyRing]
	if localCommitPoint != nil {
		commitKeys.SetForParty(lntypes.Local, DeriveCommitmentKeys(
			localCommitPoint, lntypes.Local,
			lc.channelState.ChanType,
			&lc.channelState.LocalChanCfg,
			&lc.channelState.RemoteChanCfg,
		))
	}
	if remoteCommitPoint != nil {
		commitKeys.SetForParty(lntypes.Remote, DeriveCommitmentKeys(
			remoteCommitPoint, lntypes.Remote,
			lc.channelState.ChanType,
			&lc.channelState.LocalChanCfg,
			&lc.channelState.RemoteChanCfg,
		))
	}

	auxResult, err := fn.MapOptionZ(
		lc.leafStore,
		func(s AuxLeafStore) fn.Result[CommitDiffAuxResult] {
			return s.FetchLeavesFromCommit(
				NewAuxChanState(lc.channelState), *diskCommit,
				*commitKeys.GetForParty(whoseCommit),
				whoseCommit,
			)
		},
	).Unpack()
	if err != nil {
		return nil, fmt.Errorf("unable to fetch aux leaves: %w", err)
	}

	// With the key rings re-created, we'll now convert all the on-disk
	// HTLC"s into paymentDescriptor's so we can re-insert them into our
	// update log.
	incomingHtlcs, outgoingHtlcs, err := lc.extractPayDescs(
		chainfee.SatPerKWeight(diskCommit.FeePerKw),
		diskCommit.Htlcs, commitKeys, whoseCommit, auxResult.AuxLeaves,
	)
	if err != nil {
		return nil, err
	}

	messageIndices := lntypes.Dual[uint64]{
		Local:  diskCommit.LocalLogIndex,
		Remote: diskCommit.RemoteLogIndex,
	}

	// With the necessary items generated, we'll now re-construct the
	// commitment state as it was originally present in memory.
	commit := &commitment{
		height:         diskCommit.CommitHeight,
		whoseCommit:    whoseCommit,
		ourBalance:     diskCommit.LocalBalance,
		theirBalance:   diskCommit.RemoteBalance,
		messageIndices: messageIndices,
		ourHtlcIndex:   diskCommit.LocalHtlcIndex,
		theirHtlcIndex: diskCommit.RemoteHtlcIndex,
		txn:            diskCommit.CommitTx,
		sig:            diskCommit.CommitSig,
		fee:            diskCommit.CommitFee,
		feePerKw:       chainfee.SatPerKWeight(diskCommit.FeePerKw),
		incomingHTLCs:  incomingHtlcs,
		outgoingHTLCs:  outgoingHtlcs,
		customBlob:     diskCommit.CustomBlob,
	}
	if whoseCommit.IsLocal() {
		commit.dustLimit = lc.channelState.LocalChanCfg.DustLimit
	} else {
		commit.dustLimit = lc.channelState.RemoteChanCfg.DustLimit
	}

	return commit, nil
}

// LightningChannel implements the state machine which corresponds to the
// current commitment protocol wire spec. The state machine implemented allows
// for asynchronous fully desynchronized, batched+pipelined updates to
// commitment transactions allowing for a high degree of non-blocking
// bi-directional payment throughput.
//
// In order to allow updates to be fully non-blocking, either side is able to
// create multiple new commitment states up to a pre-determined window size.
// This window size is encoded within InitialRevocationWindow. Before the start
// of a session, both side should send out revocation messages with nil
// preimages in order to populate their revocation window for the remote party.
//
// The state machine has for main methods:
//   - .SignNextCommitment()
//   - Called once when one wishes to sign the next commitment, either
//     initiating a new state update, or responding to a received commitment.
//   - .ReceiveNewCommitment()
//   - Called upon receipt of a new commitment from the remote party. If the
//     new commitment is valid, then a revocation should immediately be
//     generated and sent.
//   - .RevokeCurrentCommitment()
//   - Revokes the current commitment. Should be called directly after
//     receiving a new commitment.
//   - .ReceiveRevocation()
//   - Processes a revocation from the remote party. If successful creates a
//     new defacto broadcastable state.
//
// See the individual comments within the above methods for further details.
type LightningChannel struct {
	// Signer is the main signer instances that will be responsible for
	// signing any HTLC and commitment transaction generated by the state
	// machine.
	Signer input.Signer

	// leafStore is used to retrieve extra tapscript leaves for special
	// custom channel types.
	leafStore fn.Option[AuxLeafStore]

	// signDesc is the primary sign descriptor that is capable of signing
	// the commitment transaction that spends the multi-sig output.
	signDesc *input.SignDescriptor

	isClosed bool

	// sigPool is a pool of workers that are capable of signing and
	// validating signatures in parallel. This is utilized as an
	// optimization to void serially signing or validating the HTLC
	// signatures, of which there may be hundreds.
	sigPool *SigPool

	// auxSigner is a special signer used to obtain opaque signatures for
	// custom channel variants.
	auxSigner fn.Option[AuxSigner]

	// auxResolver is an optional component that can be used to modify the
	// way contracts are resolved.
	auxResolver fn.Option[AuxContractResolver]

	// Capacity is the total capacity of this channel.
	Capacity btcutil.Amount

	// currentHeight is the current height of our local commitment chain.
	// This is also the same as the number of updates to the channel we've
	// accepted.
	currentHeight uint64

	// commitChains is a Dual of the local and remote node's commitment
	// chains. Any new commitments we initiate are added to Remote chain's
	// tip. The Local portion of this field is our local commitment chain.
	// Any new commitments received are added to the tip of this chain.
	// The tail (or lowest height) in this chain is our current accepted
	// state, which we are able to broadcast safely.
	commitChains lntypes.Dual[*commitmentChain]

	channelState *channeldb.OpenChannel

	commitBuilder *CommitmentBuilder

	// [local|remote]Log is a (mostly) append-only log storing all the HTLC
	// updates to this channel. The log is walked backwards as HTLC updates
	// are applied in order to re-construct a commitment transaction from a
	// commitment. The log is compacted once a revocation is received.
	updateLogs lntypes.Dual[*updateLog]

	// log is a channel-specific logging instance.
	log btclog.Logger

	// taprootNonceProducer is used to generate a shachain tree for the
	// purpose of generating verification nonces for taproot channels.
	taprootNonceProducer shachain.Producer

	// musigSessions holds the current musig2 pair session for the channel.
	musigSessions *MusigPairSession

	// pendingVerificationNonce is the initial verification nonce generated
	// for musig2 channels when the state machine is intiated. Once we know
	// the verification nonce of the remote party, then we can start to use
	// the channel as normal.
	pendingVerificationNonce *musig2.Nonces

	// fundingOutput is the funding output (script+value).
	fundingOutput wire.TxOut

	// opts is the set of options that channel was initialized with.
	opts *channelOpts

	sync.RWMutex
}

// ChannelOpt is a functional option that lets callers modify how a new channel
// is created.
type ChannelOpt func(*channelOpts)

// channelOpts is the set of options used to create a new channel.
type channelOpts struct {
	localNonce  *musig2.Nonces
	remoteNonce *musig2.Nonces

	leafStore   fn.Option[AuxLeafStore]
	auxSigner   fn.Option[AuxSigner]
	auxResolver fn.Option[AuxContractResolver]

	skipNonceInit bool
}

// WithLocalMusigNonces is used to bind an existing verification/local nonce to
// a new channel.
func WithLocalMusigNonces(nonce *musig2.Nonces) ChannelOpt {
	return func(o *channelOpts) {
		o.localNonce = nonce
	}
}

// WithRemoteMusigNonces is used to bind the remote party's local/verification
// nonce to a new channel.
func WithRemoteMusigNonces(nonces *musig2.Nonces) ChannelOpt {
	return func(o *channelOpts) {
		o.remoteNonce = nonces
	}
}

// WithSkipNonceInit is used to modify the way nonces are handled during
// channel initialization for taproot channels. If this option is specified,
// then when we receive the chan reest message from the remote party, we won't
// modify our nonce state. This is needed if we create a channel, get a channel
// ready message, then also get the chan reest message after that.
func WithSkipNonceInit() ChannelOpt {
	return func(o *channelOpts) {
		o.skipNonceInit = true
	}
}

// WithLeafStore is used to specify a custom leaf store for the channel.
func WithLeafStore(store AuxLeafStore) ChannelOpt {
	return func(o *channelOpts) {
		o.leafStore = fn.Some[AuxLeafStore](store)
	}
}

// WithAuxSigner is used to specify a custom aux signer for the channel.
func WithAuxSigner(signer AuxSigner) ChannelOpt {
	return func(o *channelOpts) {
		o.auxSigner = fn.Some[AuxSigner](signer)
	}
}

// WithAuxResolver is used to specify a custom aux contract resolver for the
// channel.
func WithAuxResolver(resolver AuxContractResolver) ChannelOpt {
	return func(o *channelOpts) {
		o.auxResolver = fn.Some[AuxContractResolver](resolver)
	}
}

// defaultChannelOpts returns the set of default options for a new channel.
func defaultChannelOpts() *channelOpts {
	return &channelOpts{}
}

// NewLightningChannel creates a new, active payment channel given an
// implementation of the chain notifier, channel database, and the current
// settled channel state. Throughout state transitions, then channel will
// automatically persist pertinent state to the database in an efficient
// manner.
func NewLightningChannel(signer input.Signer,
	state *channeldb.OpenChannel,
	sigPool *SigPool, chanOpts ...ChannelOpt) (*LightningChannel, error) {

	opts := defaultChannelOpts()
	for _, optFunc := range chanOpts {
		optFunc(opts)
	}

	localCommit := state.LocalCommitment
	remoteCommit := state.RemoteCommitment

	// First, initialize the update logs with their current counter values
	// from the local and remote commitments.
	localUpdateLog := newUpdateLog(
		remoteCommit.LocalLogIndex, remoteCommit.LocalHtlcIndex,
	)
	remoteUpdateLog := newUpdateLog(
		localCommit.RemoteLogIndex, localCommit.RemoteHtlcIndex,
	)
	updateLogs := lntypes.Dual[*updateLog]{
		Local:  localUpdateLog,
		Remote: remoteUpdateLog,
	}

	logPrefix := fmt.Sprintf("ChannelPoint(%v):", state.FundingOutpoint)

	taprootNonceProducer, err := channeldb.DeriveMusig2Shachain(
		state.RevocationProducer,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to derive shachain: %w", err)
	}

	commitChains := lntypes.Dual[*commitmentChain]{
		Local:  newCommitmentChain(),
		Remote: newCommitmentChain(),
	}

	lc := &LightningChannel{
		Signer:        signer,
		leafStore:     opts.leafStore,
		auxSigner:     opts.auxSigner,
		auxResolver:   opts.auxResolver,
		sigPool:       sigPool,
		currentHeight: localCommit.CommitHeight,
		commitChains:  commitChains,
		channelState:  state,
		commitBuilder: NewCommitmentBuilder(
			state, opts.leafStore,
		),
		updateLogs:           updateLogs,
		Capacity:             state.Capacity,
		taprootNonceProducer: taprootNonceProducer,
		log:                  walletLog.WithPrefix(logPrefix),
		opts:                 opts,
	}

	switch {
	// At this point, we may already have nonces that were passed in, so
	// we'll check that now as this lets us skip some steps later.
	case state.ChanType.IsTaproot() && opts.localNonce != nil:
		lc.pendingVerificationNonce = opts.localNonce

	// Otherwise, we'll generate the nonces here ourselves. This ensures
	// we'll be ablve to process the chan syncmessag efrom the remote
	// party.
	case state.ChanType.IsTaproot() && opts.localNonce == nil:
		_, err := lc.GenMusigNonces()
		if err != nil {
			return nil, err
		}
	}
	if lc.pendingVerificationNonce != nil && opts.remoteNonce != nil {
		err := lc.InitRemoteMusigNonces(opts.remoteNonce)
		if err != nil {
			return nil, err
		}
	}

	// With the main channel struct reconstructed, we'll now restore the
	// commitment state in memory and also the update logs themselves.
	err = lc.restoreCommitState(&localCommit, &remoteCommit)
	if err != nil {
		return nil, err
	}

	// Create the sign descriptor which we'll be using very frequently to
	// request a signature for the 2-of-2 multi-sig from the signer in
	// order to complete channel state transitions.
	if err := lc.createSignDesc(); err != nil {
		return nil, err
	}

	return lc, nil
}

// createSignDesc derives the SignDescriptor for commitment transactions from
// other fields on the LightningChannel.
func (lc *LightningChannel) createSignDesc() error {

	var (
		fundingPkScript, multiSigScript []byte
		err                             error
	)

	chanState := lc.channelState
	localKey := chanState.LocalChanCfg.MultiSigKey.PubKey
	remoteKey := chanState.RemoteChanCfg.MultiSigKey.PubKey

	if chanState.ChanType.IsTaproot() {
		fundingPkScript, _, err = input.GenTaprootFundingScript(
			localKey, remoteKey, int64(lc.channelState.Capacity),
			chanState.TapscriptRoot,
		)
		if err != nil {
			return err
		}
	} else {
		multiSigScript, err = input.GenMultiSigScript(
			localKey.SerializeCompressed(),
			remoteKey.SerializeCompressed(),
		)
		if err != nil {
			return err
		}

		fundingPkScript, err = input.WitnessScriptHash(multiSigScript)
		if err != nil {
			return err
		}
	}

	lc.fundingOutput = wire.TxOut{
		PkScript: fundingPkScript,
		Value:    int64(lc.channelState.Capacity),
	}
	lc.signDesc = &input.SignDescriptor{
		KeyDesc:       lc.channelState.LocalChanCfg.MultiSigKey,
		WitnessScript: multiSigScript,
		Output:        &lc.fundingOutput,
		HashType:      txscript.SigHashAll,
		InputIndex:    0,
	}

	return nil
}

// ResetState resets the state of the channel back to the default state. This
// ensures that any active goroutines which need to act based on on-chain
// events do so properly.
func (lc *LightningChannel) ResetState() {
	lc.Lock()
	lc.isClosed = false
	lc.Unlock()
}

// logUpdateToPayDesc converts a LogUpdate into a matching paymentDescriptor
// entry that can be re-inserted into the update log. This method is used when
// we extended a state to the remote party, but the connection was obstructed
// before we could finish the commitment dance. In this case, we need to
// re-insert the original entries back into the update log so we can resume as
// if nothing happened.
func (lc *LightningChannel) logUpdateToPayDesc(logUpdate *channeldb.LogUpdate,
	remoteUpdateLog *updateLog, commitHeight uint64,
	feeRate chainfee.SatPerKWeight, remoteCommitKeys *CommitmentKeyRing,
	remoteDustLimit btcutil.Amount,
	auxLeaves fn.Option[CommitAuxLeaves]) (*paymentDescriptor, error) {

	// Depending on the type of update message we'll map that to a distinct
	// paymentDescriptor instance.
	var pd *paymentDescriptor

	switch wireMsg := logUpdate.UpdateMsg.(type) {

	// For offered HTLC's, we'll map that to a paymentDescriptor with the
	// type Add, ensuring we restore the necessary fields. From the PoV of
	// the commitment chain, this HTLC was included in the remote chain,
	// but not the local chain.
	case *lnwire.UpdateAddHTLC:
		// First, we'll map all the relevant fields in the
		// UpdateAddHTLC message to their corresponding fields in the
		// paymentDescriptor struct. We also set addCommitHeightRemote
		// as we've included this HTLC in our local commitment chain
		// for the remote party.
		pd = &paymentDescriptor{
			ChanID:        wireMsg.ChanID,
			RHash:         wireMsg.PaymentHash,
			Timeout:       wireMsg.Expiry,
			Amount:        wireMsg.Amount,
			EntryType:     Add,
			HtlcIndex:     wireMsg.ID,
			LogIndex:      logUpdate.LogIndex,
			OnionBlob:     wireMsg.OnionBlob,
			BlindingPoint: wireMsg.BlindingPoint,
			CustomRecords: wireMsg.CustomRecords.Copy(),
			addCommitHeights: lntypes.Dual[uint64]{
				Remote: commitHeight,
			},
		}

		pd.EntryType = lc.entryTypeForHtlc(
			pd.CustomRecords, lc.channelState.ChanType,
		)

		isDustRemote := HtlcIsDust(
			lc.channelState.ChanType, false, lntypes.Remote,
			feeRate, wireMsg.Amount.ToSatoshis(), remoteDustLimit,
		)
		if !isDustRemote {
			auxLeaf := fn.FlatMapOption(
				func(l CommitAuxLeaves) input.AuxTapLeaf {
					leaves := l.OutgoingHtlcLeaves
					return leaves[pd.HtlcIndex].AuxTapLeaf
				},
			)(auxLeaves)

			scriptInfo, err := genHtlcScript(
				lc.channelState.ChanType, false, lntypes.Remote,
				wireMsg.Expiry, wireMsg.PaymentHash,
				remoteCommitKeys, auxLeaf,
			)
			if err != nil {
				return nil, err
			}

			pd.theirPkScript = scriptInfo.PkScript()
			pd.theirWitnessScript = scriptInfo.WitnessScriptToSign()
		}

	// For HTLC's we're offered we'll fetch the original offered HTLC
	// from the remote party's update log so we can retrieve the same
	// paymentDescriptor that SettleHTLC would produce.
	case *lnwire.UpdateFulfillHTLC:
		ogHTLC := remoteUpdateLog.lookupHtlc(wireMsg.ID)

		pd = &paymentDescriptor{
			ChanID:      wireMsg.ChanID,
			Amount:      ogHTLC.Amount,
			RHash:       ogHTLC.RHash,
			RPreimage:   wireMsg.PaymentPreimage,
			LogIndex:    logUpdate.LogIndex,
			ParentIndex: ogHTLC.HtlcIndex,
			EntryType:   Settle,
			removeCommitHeights: lntypes.Dual[uint64]{
				Remote: commitHeight,
			},
		}

	// If we sent a failure for a prior incoming HTLC, then we'll consult
	// the update log of the remote party so we can retrieve the
	// information of the original HTLC we're failing. We also set the
	// removal height for the remote commitment.
	case *lnwire.UpdateFailHTLC:
		ogHTLC := remoteUpdateLog.lookupHtlc(wireMsg.ID)

		pd = &paymentDescriptor{
			ChanID:      wireMsg.ChanID,
			Amount:      ogHTLC.Amount,
			RHash:       ogHTLC.RHash,
			ParentIndex: ogHTLC.HtlcIndex,
			LogIndex:    logUpdate.LogIndex,
			EntryType:   Fail,
			FailReason:  wireMsg.Reason[:],
			removeCommitHeights: lntypes.Dual[uint64]{
				Remote: commitHeight,
			},
		}

	// HTLC fails due to malformed onion blobs are treated the exact same
	// way as regular HTLC fails.
	case *lnwire.UpdateFailMalformedHTLC:
		ogHTLC := remoteUpdateLog.lookupHtlc(wireMsg.ID)
		// TODO(roasbeef): err if nil?

		pd = &paymentDescriptor{
			ChanID:       wireMsg.ChanID,
			Amount:       ogHTLC.Amount,
			RHash:        ogHTLC.RHash,
			ParentIndex:  ogHTLC.HtlcIndex,
			LogIndex:     logUpdate.LogIndex,
			EntryType:    MalformedFail,
			FailCode:     wireMsg.FailureCode,
			ShaOnionBlob: wireMsg.ShaOnionBlob,
			removeCommitHeights: lntypes.Dual[uint64]{
				Remote: commitHeight,
			},
		}

	// For fee updates we'll create a FeeUpdate type to add to the log. We
	// reuse the amount field to hold the fee rate. Since the amount field
	// is denominated in msat we won't lose precision when storing the
	// sat/kw denominated feerate. Note that we set both the add and remove
	// height to the same value, as we consider the fee update locked in by
	// adding and removing it at the same height.
	case *lnwire.UpdateFee:
		pd = &paymentDescriptor{
			ChanID:   wireMsg.ChanID,
			LogIndex: logUpdate.LogIndex,
			Amount: lnwire.NewMSatFromSatoshis(
				btcutil.Amount(wireMsg.FeePerKw),
			),
			EntryType: FeeUpdate,
			addCommitHeights: lntypes.Dual[uint64]{
				Remote: commitHeight,
			},
			removeCommitHeights: lntypes.Dual[uint64]{
				Remote: commitHeight,
			},
		}
	}

	return pd, nil
}

// localLogUpdateToPayDesc converts a LogUpdate into a matching
// paymentDescriptor entry that can be re-inserted into the local update log.
// This method is used when we sent an update+sig, receive a revocation, but
// drop right before the counterparty can sign for the update we just sent. In
// this case, we need to re-insert the original entries back into the update
// log so we'll be expecting the peer to sign them. The height of the remote
// commitment is expected to be provided and we restore all log update entries
// with this height, even though the real height may be lower. In the way these
// fields are used elsewhere, this doesn't change anything.
func (lc *LightningChannel) localLogUpdateToPayDesc(logUpdate *channeldb.LogUpdate,
	remoteUpdateLog *updateLog, commitHeight uint64) (*paymentDescriptor,
	error) {

	// Since Add updates aren't saved to disk under this key, the update will
	// never be an Add.
	switch wireMsg := logUpdate.UpdateMsg.(type) {
	// For HTLCs that we settled, we'll fetch the original offered HTLC from
	// the remote update log so we can retrieve the same paymentDescriptor
	// that ReceiveHTLCSettle would produce.
	case *lnwire.UpdateFulfillHTLC:
		ogHTLC := remoteUpdateLog.lookupHtlc(wireMsg.ID)

		return &paymentDescriptor{
			ChanID:      wireMsg.ChanID,
			Amount:      ogHTLC.Amount,
			RHash:       ogHTLC.RHash,
			RPreimage:   wireMsg.PaymentPreimage,
			LogIndex:    logUpdate.LogIndex,
			ParentIndex: ogHTLC.HtlcIndex,
			EntryType:   Settle,
			removeCommitHeights: lntypes.Dual[uint64]{
				Remote: commitHeight,
			},
		}, nil

	// If we sent a failure for a prior incoming HTLC, then we'll consult the
	// remote update log so we can retrieve the information of the original
	// HTLC we're failing.
	case *lnwire.UpdateFailHTLC:
		ogHTLC := remoteUpdateLog.lookupHtlc(wireMsg.ID)

		return &paymentDescriptor{
			ChanID:      wireMsg.ChanID,
			Amount:      ogHTLC.Amount,
			RHash:       ogHTLC.RHash,
			ParentIndex: ogHTLC.HtlcIndex,
			LogIndex:    logUpdate.LogIndex,
			EntryType:   Fail,
			FailReason:  wireMsg.Reason[:],
			removeCommitHeights: lntypes.Dual[uint64]{
				Remote: commitHeight,
			},
		}, nil

	// HTLC fails due to malformed onion blocks are treated the exact same
	// way as regular HTLC fails.
	case *lnwire.UpdateFailMalformedHTLC:
		ogHTLC := remoteUpdateLog.lookupHtlc(wireMsg.ID)

		return &paymentDescriptor{
			ChanID:       wireMsg.ChanID,
			Amount:       ogHTLC.Amount,
			RHash:        ogHTLC.RHash,
			ParentIndex:  ogHTLC.HtlcIndex,
			LogIndex:     logUpdate.LogIndex,
			EntryType:    MalformedFail,
			FailCode:     wireMsg.FailureCode,
			ShaOnionBlob: wireMsg.ShaOnionBlob,
			removeCommitHeights: lntypes.Dual[uint64]{
				Remote: commitHeight,
			},
		}, nil

	case *lnwire.UpdateFee:
		return &paymentDescriptor{
			ChanID:   wireMsg.ChanID,
			LogIndex: logUpdate.LogIndex,
			Amount: lnwire.NewMSatFromSatoshis(
				btcutil.Amount(wireMsg.FeePerKw),
			),
			EntryType: FeeUpdate,
			addCommitHeights: lntypes.Dual[uint64]{
				Remote: commitHeight,
			},
			removeCommitHeights: lntypes.Dual[uint64]{
				Remote: commitHeight,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unknown message type: %T", wireMsg)
	}
}

// remoteLogUpdateToPayDesc converts a LogUpdate into a matching
// paymentDescriptor entry that can be re-inserted into the update log. This
// method is used when we revoked a local commitment, but the connection was
// obstructed before we could sign a remote commitment that contains these
// updates. In this case, we need to re-insert the original entries back into
// the update log so we can resume as if nothing happened. The height of the
// latest local commitment is also expected to be provided. We are restoring all
// log update entries with this height, even though the real commitment height
// may be lower. In the way these fields are used elsewhere, this doesn't change
// anything.
func (lc *LightningChannel) remoteLogUpdateToPayDesc(logUpdate *channeldb.LogUpdate,
	localUpdateLog *updateLog, commitHeight uint64) (*paymentDescriptor,
	error) {

	switch wireMsg := logUpdate.UpdateMsg.(type) {
	case *lnwire.UpdateAddHTLC:
		pd := &paymentDescriptor{
			ChanID:        wireMsg.ChanID,
			RHash:         wireMsg.PaymentHash,
			Timeout:       wireMsg.Expiry,
			Amount:        wireMsg.Amount,
			EntryType:     Add,
			HtlcIndex:     wireMsg.ID,
			LogIndex:      logUpdate.LogIndex,
			OnionBlob:     wireMsg.OnionBlob,
			BlindingPoint: wireMsg.BlindingPoint,
			CustomRecords: wireMsg.CustomRecords.Copy(),
			addCommitHeights: lntypes.Dual[uint64]{
				Local: commitHeight,
			},
		}

		pd.EntryType = lc.entryTypeForHtlc(
			pd.CustomRecords, lc.channelState.ChanType,
		)

		// We don't need to generate an htlc script yet. This will be
		// done once we sign our remote commitment.

		return pd, nil

	// For HTLCs that the remote party settled, we'll fetch the original
	// offered HTLC from the local update log so we can retrieve the same
	// paymentDescriptor that ReceiveHTLCSettle would produce.
	case *lnwire.UpdateFulfillHTLC:
		ogHTLC := localUpdateLog.lookupHtlc(wireMsg.ID)

		return &paymentDescriptor{
			ChanID:      wireMsg.ChanID,
			Amount:      ogHTLC.Amount,
			RHash:       ogHTLC.RHash,
			RPreimage:   wireMsg.PaymentPreimage,
			LogIndex:    logUpdate.LogIndex,
			ParentIndex: ogHTLC.HtlcIndex,
			EntryType:   Settle,
			removeCommitHeights: lntypes.Dual[uint64]{
				Local: commitHeight,
			},
		}, nil

	// If we received a failure for a prior outgoing HTLC, then we'll
	// consult the local update log so we can retrieve the information of
	// the original HTLC we're failing.
	case *lnwire.UpdateFailHTLC:
		ogHTLC := localUpdateLog.lookupHtlc(wireMsg.ID)

		return &paymentDescriptor{
			ChanID:      wireMsg.ChanID,
			Amount:      ogHTLC.Amount,
			RHash:       ogHTLC.RHash,
			ParentIndex: ogHTLC.HtlcIndex,
			LogIndex:    logUpdate.LogIndex,
			EntryType:   Fail,
			FailReason:  wireMsg.Reason[:],
			removeCommitHeights: lntypes.Dual[uint64]{
				Local: commitHeight,
			},
		}, nil

	// HTLC fails due to malformed onion blobs are treated the exact same
	// way as regular HTLC fails.
	case *lnwire.UpdateFailMalformedHTLC:
		ogHTLC := localUpdateLog.lookupHtlc(wireMsg.ID)

		return &paymentDescriptor{
			ChanID:       wireMsg.ChanID,
			Amount:       ogHTLC.Amount,
			RHash:        ogHTLC.RHash,
			ParentIndex:  ogHTLC.HtlcIndex,
			LogIndex:     logUpdate.LogIndex,
			EntryType:    MalformedFail,
			FailCode:     wireMsg.FailureCode,
			ShaOnionBlob: wireMsg.ShaOnionBlob,
			removeCommitHeights: lntypes.Dual[uint64]{
				Local: commitHeight,
			},
		}, nil

	// For fee updates we'll create a FeeUpdate type to add to the log. We
	// reuse the amount field to hold the fee rate. Since the amount field
	// is denominated in msat we won't lose precision when storing the
	// sat/kw denominated feerate. Note that we set both the add and remove
	// height to the same value, as we consider the fee update locked in by
	// adding and removing it at the same height.
	case *lnwire.UpdateFee:
		return &paymentDescriptor{
			ChanID:   wireMsg.ChanID,
			LogIndex: logUpdate.LogIndex,
			Amount: lnwire.NewMSatFromSatoshis(
				btcutil.Amount(wireMsg.FeePerKw),
			),
			EntryType: FeeUpdate,
			addCommitHeights: lntypes.Dual[uint64]{
				Local: commitHeight,
			},
			removeCommitHeights: lntypes.Dual[uint64]{
				Local: commitHeight,
			},
		}, nil

	default:
		return nil, errors.New("unknown message type")
	}
}

// restoreCommitState will restore the local commitment chain and updateLog
// state to a consistent in-memory representation of the passed disk commitment.
// This method is to be used upon reconnection to our channel counter party.
// Once the connection has been established, we'll prepare our in memory state
// to re-sync states with the remote party, and also verify/extend new proposed
// commitment states.
func (lc *LightningChannel) restoreCommitState(
	localCommitState, remoteCommitState *channeldb.ChannelCommitment) error {

	// In order to reconstruct the pkScripts on each of the pending HTLC
	// outputs (if any) we'll need to regenerate the current revocation for
	// this current un-revoked state as well as retrieve the current
	// revocation for the remote party.
	ourRevPreImage, err := lc.channelState.RevocationProducer.AtIndex(
		lc.currentHeight,
	)
	if err != nil {
		return err
	}
	localCommitPoint := input.ComputeCommitmentPoint(ourRevPreImage[:])
	remoteCommitPoint := lc.channelState.RemoteCurrentRevocation

	// With the revocation state reconstructed, we can now convert the disk
	// commitment into our in-memory commitment format, inserting it into
	// the local commitment chain.
	localCommit, err := lc.diskCommitToMemCommit(
		lntypes.Local, localCommitState, localCommitPoint,
		remoteCommitPoint,
	)
	if err != nil {
		return err
	}
	lc.commitChains.Local.addCommitment(localCommit)

	lc.log.Tracef("starting local commitment: %v",
		lnutils.SpewLogClosure(lc.commitChains.Local.tail()))

	// We'll also do the same for the remote commitment chain.
	remoteCommit, err := lc.diskCommitToMemCommit(
		lntypes.Remote, remoteCommitState, localCommitPoint,
		remoteCommitPoint,
	)
	if err != nil {
		return err
	}
	lc.commitChains.Remote.addCommitment(remoteCommit)

	lc.log.Tracef("starting remote commitment: %v",
		lnutils.SpewLogClosure(lc.commitChains.Remote.tail()))

	var (
		pendingRemoteCommit     *commitment
		pendingRemoteCommitDiff *channeldb.CommitDiff
		pendingRemoteKeyChain   *CommitmentKeyRing
	)

	// Next, we'll check to see if we have an un-acked commitment state we
	// extended to the remote party but which was never ACK'd.
	pendingRemoteCommitDiff, err = lc.channelState.RemoteCommitChainTip()
	if err != nil && err != channeldb.ErrNoPendingCommit {
		return err
	}

	if pendingRemoteCommitDiff != nil {
		// If we have a pending remote commitment, then we'll also
		// reconstruct the original commitment for that state,
		// inserting it into the remote party's commitment chain. We
		// don't pass our commit point as we don't have the
		// corresponding state for the local commitment chain.
		pendingCommitPoint := lc.channelState.RemoteNextRevocation
		pendingRemoteCommit, err = lc.diskCommitToMemCommit(
			lntypes.Remote, &pendingRemoteCommitDiff.Commitment,
			nil, pendingCommitPoint,
		)
		if err != nil {
			return err
		}
		lc.commitChains.Remote.addCommitment(pendingRemoteCommit)

		lc.log.Debugf("pending remote commitment: %v",
			lnutils.SpewLogClosure(lc.commitChains.Remote.tip()))

		// We'll also re-create the set of commitment keys needed to
		// fully re-derive the state.
		pendingRemoteKeyChain = DeriveCommitmentKeys(
			pendingCommitPoint, lntypes.Remote,
			lc.channelState.ChanType,
			&lc.channelState.LocalChanCfg,
			&lc.channelState.RemoteChanCfg,
		)
	}

	// Fetch remote updates that we have acked but not yet signed for.
	unsignedAckedUpdates, err := lc.channelState.UnsignedAckedUpdates()
	if err != nil {
		return err
	}

	// Fetch the local updates the peer still needs to sign for.
	remoteUnsignedLocalUpdates, err := lc.channelState.RemoteUnsignedLocalUpdates()
	if err != nil {
		return err
	}

	// Finally, with the commitment states restored, we'll now restore the
	// state logs based on the current local+remote commit, and any pending
	// remote commit that exists.
	err = lc.restoreStateLogs(
		localCommit, remoteCommit, pendingRemoteCommit,
		pendingRemoteCommitDiff, pendingRemoteKeyChain,
		unsignedAckedUpdates, remoteUnsignedLocalUpdates,
	)
	if err != nil {
		return err
	}

	return nil
}

// restoreStateLogs runs through the current locked-in HTLCs from the point of
// view of the channel and insert corresponding log entries (both local and
// remote) for each HTLC read from disk. This method is required to sync the
// in-memory state of the state machine with that read from persistent storage.
func (lc *LightningChannel) restoreStateLogs(
	localCommitment, remoteCommitment, pendingRemoteCommit *commitment,
	pendingRemoteCommitDiff *channeldb.CommitDiff,
	pendingRemoteKeys *CommitmentKeyRing,
	unsignedAckedUpdates,
	remoteUnsignedLocalUpdates []channeldb.LogUpdate) error {

	// We make a map of incoming HTLCs to the height of the remote
	// commitment they were first added, and outgoing HTLCs to the height
	// of the local commit they were first added. This will be used when we
	// restore the update logs below.
	incomingRemoteAddHeights := make(map[uint64]uint64)
	outgoingLocalAddHeights := make(map[uint64]uint64)

	// We start by setting the height of the incoming HTLCs on the pending
	// remote commitment. We set these heights first since if there are
	// duplicates, these will be overwritten by the lower height of the
	// remoteCommitment below.
	if pendingRemoteCommit != nil {
		for _, r := range pendingRemoteCommit.incomingHTLCs {
			incomingRemoteAddHeights[r.HtlcIndex] =
				pendingRemoteCommit.height
		}
	}

	// Now set the remote commit height of all incoming HTLCs found on the
	// remote commitment.
	for _, r := range remoteCommitment.incomingHTLCs {
		incomingRemoteAddHeights[r.HtlcIndex] = remoteCommitment.height
	}

	// And finally we can do the same for the outgoing HTLCs.
	for _, l := range localCommitment.outgoingHTLCs {
		outgoingLocalAddHeights[l.HtlcIndex] = localCommitment.height
	}

	// If we have any unsigned acked updates to sign for, then the add is no
	// longer on our local commitment, but is still on the remote's commitment.
	// <---fail---
	// <---sig----
	// ----rev--->
	// To ensure proper channel operation, we restore the add's addCommitHeightLocal
	// field to the height of our local commitment.
	for _, logUpdate := range unsignedAckedUpdates {
		var htlcIdx uint64
		switch wireMsg := logUpdate.UpdateMsg.(type) {
		case *lnwire.UpdateFulfillHTLC:
			htlcIdx = wireMsg.ID
		case *lnwire.UpdateFailHTLC:
			htlcIdx = wireMsg.ID
		case *lnwire.UpdateFailMalformedHTLC:
			htlcIdx = wireMsg.ID
		default:
			continue
		}

		// The htlcIdx is stored in the map with the local commitment
		// height so the related add's addCommitHeightLocal field can be
		// restored.
		outgoingLocalAddHeights[htlcIdx] = localCommitment.height
	}

	// If there are local updates that the peer needs to sign for, then the
	// corresponding add is no longer on the remote commitment, but is still on
	// our local commitment.
	// ----fail--->
	// ----sig---->
	// <---rev-----
	// To ensure proper channel operation, we restore the add's addCommitHeightRemote
	// field to the height of the remote commitment.
	for _, logUpdate := range remoteUnsignedLocalUpdates {
		var htlcIdx uint64
		switch wireMsg := logUpdate.UpdateMsg.(type) {
		case *lnwire.UpdateFulfillHTLC:
			htlcIdx = wireMsg.ID
		case *lnwire.UpdateFailHTLC:
			htlcIdx = wireMsg.ID
		case *lnwire.UpdateFailMalformedHTLC:
			htlcIdx = wireMsg.ID
		default:
			continue
		}

		// The htlcIdx is stored in the map with the remote commitment
		// height so the related add's addCommitHeightRemote field can be
		// restored.
		incomingRemoteAddHeights[htlcIdx] = remoteCommitment.height
	}

	// For each incoming HTLC within the local commitment, we add it to the
	// remote update log. Since HTLCs are added first to the receiver's
	// commitment, we don't have to restore outgoing HTLCs, as they will be
	// restored from the remote commitment below.
	for i := range localCommitment.incomingHTLCs {
		htlc := localCommitment.incomingHTLCs[i]

		// We'll need to set the add height of the HTLC. Since it is on
		// this local commit, we can use its height as local add
		// height. As remote add height we consult the incoming HTLC
		// map we created earlier. Note that if this HTLC is not in
		// incomingRemoteAddHeights, the remote add height will be set
		// to zero, which indicates that it is not added yet.
		htlc.addCommitHeights.Local = localCommitment.height
		htlc.addCommitHeights.Remote =
			incomingRemoteAddHeights[htlc.HtlcIndex]

		// Restore the htlc back to the remote log.
		lc.updateLogs.Remote.restoreHtlc(&htlc)
	}

	// Similarly, we'll do the same for the outgoing HTLCs within the
	// remote commitment, adding them to the local update log.
	for i := range remoteCommitment.outgoingHTLCs {
		htlc := remoteCommitment.outgoingHTLCs[i]

		// As for the incoming HTLCs, we'll use the current remote
		// commit height as remote add height, and consult the map
		// created above for the local add height.
		htlc.addCommitHeights.Remote = remoteCommitment.height
		htlc.addCommitHeights.Local =
			outgoingLocalAddHeights[htlc.HtlcIndex]

		// Restore the htlc back to the local log.
		lc.updateLogs.Local.restoreHtlc(&htlc)
	}

	// If we have a dangling (un-acked) commit for the remote party, then we
	// restore the updates leading up to this commit.
	if pendingRemoteCommit != nil {
		err := lc.restorePendingLocalUpdates(
			pendingRemoteCommitDiff, pendingRemoteKeys,
		)
		if err != nil {
			return err
		}
	}

	// Restore unsigned acked remote log updates so that we can include them
	// in our next signature.
	err := lc.restorePendingRemoteUpdates(
		unsignedAckedUpdates, localCommitment.height,
		pendingRemoteCommit,
	)
	if err != nil {
		return err
	}

	// Restore unsigned acked local log updates so we expect the peer to
	// sign for them.
	return lc.restorePeerLocalUpdates(
		remoteUnsignedLocalUpdates, remoteCommitment.height,
	)
}

// restorePendingRemoteUpdates restores the acked remote log updates that we
// haven't yet signed for.
func (lc *LightningChannel) restorePendingRemoteUpdates(
	unsignedAckedUpdates []channeldb.LogUpdate,
	localCommitmentHeight uint64,
	pendingRemoteCommit *commitment) error {

	lc.log.Debugf("Restoring %v dangling remote updates pending our sig",
		len(unsignedAckedUpdates))

	for _, logUpdate := range unsignedAckedUpdates {
		logUpdate := logUpdate

		payDesc, err := lc.remoteLogUpdateToPayDesc(
			&logUpdate, lc.updateLogs.Local, localCommitmentHeight,
		)
		if err != nil {
			return err
		}

		logIdx := payDesc.LogIndex

		// Sanity check that we are not restoring a remote log update
		// that we haven't received a sig for.
		if logIdx >= lc.updateLogs.Remote.logIndex {
			return fmt.Errorf("attempted to restore an "+
				"unsigned remote update: log_index=%v",
				logIdx)
		}

		// We previously restored Adds along with all the other updates,
		// but this Add restoration was a no-op as every single one of
		// these Adds was already restored since they're all incoming
		// htlcs on the local commitment.
		if payDesc.isAdd() {
			continue
		}

		var (
			height    uint64
			heightSet bool
		)

		// If we have a pending commitment for them, and this update
		// is included in that commit, then we'll use this commitment
		// height as this commitment will include these updates for
		// their new remote commitment.
		if pendingRemoteCommit != nil {
			if logIdx < pendingRemoteCommit.messageIndices.Remote {
				height = pendingRemoteCommit.height
				heightSet = true
			}
		}

		// Insert the update into the log. The log update index doesn't
		// need to be incremented (hence the restore calls), because its
		// final value was properly persisted with the last local
		// commitment update.
		switch payDesc.EntryType {
		case FeeUpdate:
			if heightSet {
				payDesc.addCommitHeights.Remote = height
				payDesc.removeCommitHeights.Remote = height
			}

			lc.updateLogs.Remote.restoreUpdate(payDesc)

		default:
			if heightSet {
				payDesc.removeCommitHeights.Remote = height
			}

			lc.updateLogs.Remote.restoreUpdate(payDesc)
			lc.updateLogs.Local.markHtlcModified(
				payDesc.ParentIndex,
			)
		}
	}

	return nil
}

// restorePeerLocalUpdates restores the acked local log updates the peer still
// needs to sign for.
func (lc *LightningChannel) restorePeerLocalUpdates(updates []channeldb.LogUpdate,
	remoteCommitmentHeight uint64) error {

	lc.log.Debugf("Restoring %v local updates that the peer should sign",
		len(updates))

	for _, logUpdate := range updates {
		logUpdate := logUpdate

		payDesc, err := lc.localLogUpdateToPayDesc(
			&logUpdate, lc.updateLogs.Remote,
			remoteCommitmentHeight,
		)
		if err != nil {
			return err
		}

		lc.updateLogs.Local.restoreUpdate(payDesc)

		// Since Add updates are not stored and FeeUpdates don't have a
		// corresponding entry in the remote update log, we only need to
		// mark the htlc as modified if the update was Settle, Fail, or
		// MalformedFail.
		if payDesc.EntryType != FeeUpdate {
			lc.updateLogs.Remote.markHtlcModified(
				payDesc.ParentIndex,
			)
		}
	}

	return nil
}

// restorePendingLocalUpdates restores the local log updates leading up to the
// given pending remote commitment.
func (lc *LightningChannel) restorePendingLocalUpdates(
	pendingRemoteCommitDiff *channeldb.CommitDiff,
	pendingRemoteKeys *CommitmentKeyRing) error {

	pendingCommit := pendingRemoteCommitDiff.Commitment
	pendingHeight := pendingCommit.CommitHeight

	lc.log.Debugf("Restoring pending remote commitment %v at commit "+
		"height %v", pendingCommit.CommitTx.TxHash(), pendingHeight)

	auxResult, err := fn.MapOptionZ(
		lc.leafStore,
		func(s AuxLeafStore) fn.Result[CommitDiffAuxResult] {
			return s.FetchLeavesFromCommit(
				NewAuxChanState(lc.channelState), pendingCommit,
				*pendingRemoteKeys, lntypes.Remote,
			)
		},
	).Unpack()
	if err != nil {
		return fmt.Errorf("unable to fetch aux leaves: %w", err)
	}

	// If we did have a dangling commit, then we'll examine which updates
	// we included in that state and re-insert them into our update log.
	for _, logUpdate := range pendingRemoteCommitDiff.LogUpdates {
		logUpdate := logUpdate

		payDesc, err := lc.logUpdateToPayDesc(
			&logUpdate, lc.updateLogs.Remote, pendingHeight,
			chainfee.SatPerKWeight(pendingCommit.FeePerKw),
			pendingRemoteKeys,
			lc.channelState.RemoteChanCfg.DustLimit,
			auxResult.AuxLeaves,
		)
		if err != nil {
			return err
		}

		// Earlier versions did not write the log index to disk for fee
		// updates, so they will be unset. To account for this we set
		// them to to current update log index.
		if payDesc.EntryType == FeeUpdate && payDesc.LogIndex == 0 &&
			lc.updateLogs.Local.logIndex > 0 {

			payDesc.LogIndex = lc.updateLogs.Local.logIndex
			lc.log.Debugf("Found FeeUpdate on "+
				"pendingRemoteCommitDiff without logIndex, "+
				"using %v", payDesc.LogIndex)
		}

		// At this point the restored update's logIndex must be equal
		// to the update log, otherwise something is horribly wrong.
		if payDesc.LogIndex != lc.updateLogs.Local.logIndex {
			panic(fmt.Sprintf("log index mismatch: "+
				"%v vs %v", payDesc.LogIndex,
				lc.updateLogs.Local.logIndex))
		}

		switch payDesc.EntryType {
		case Add, NoOpAdd:
			// The HtlcIndex of the added HTLC _must_ be equal to
			// the log's htlcCounter at this point. If it is not we
			// panic to catch this.
			// TODO(halseth): remove when cause of htlc entry bug
			// is found.
			if payDesc.HtlcIndex !=
				lc.updateLogs.Local.htlcCounter {

				panic(fmt.Sprintf("htlc index mismatch: "+
					"%v vs %v", payDesc.HtlcIndex,
					lc.updateLogs.Local.htlcCounter))
			}

			lc.updateLogs.Local.appendHtlc(payDesc)

		case FeeUpdate:
			lc.updateLogs.Local.appendUpdate(payDesc)

		default:
			lc.updateLogs.Local.appendUpdate(payDesc)

			lc.updateLogs.Remote.markHtlcModified(
				payDesc.ParentIndex,
			)
		}
	}

	return nil
}

// HtlcRetribution contains all the items necessary to seep a revoked HTLC
// transaction from a revoked commitment transaction broadcast by the remote
// party.
type HtlcRetribution struct {
	// SignDesc is a design descriptor capable of generating the necessary
	// signatures to satisfy the revocation clause of the HTLC's public key
	// script.
	SignDesc input.SignDescriptor

	// OutPoint is the target outpoint of this HTLC pointing to the
	// breached commitment transaction.
	OutPoint wire.OutPoint

	// SecondLevelWitnessScript is the witness script that will be created
	// if the second level HTLC transaction for this output is
	// broadcast/confirmed. We provide this as if the remote party attempts
	// to go to the second level to claim the HTLC then we'll need to
	// update the SignDesc above accordingly to sweep properly.
	SecondLevelWitnessScript []byte

	// SecondLevelTapTweak is the tap tweak value needed to spend the
	// second level output in case the breaching party attempts to publish
	// it.
	SecondLevelTapTweak [32]byte

	// IsIncoming is a boolean flag that indicates whether or not this
	// HTLC was accepted from the counterparty. A false value indicates that
	// this HTLC was offered by us. This flag is used determine the exact
	// witness type should be used to sweep the output.
	IsIncoming bool

	// ResolutionBlob is a blob used for aux channels that permits a
	// spender of this output to claim all funds.
	ResolutionBlob fn.Option[tlv.Blob]
}

// BreachRetribution contains all the data necessary to bring a channel
// counterparty to justice claiming ALL lingering funds within the channel in
// the scenario that they broadcast a revoked commitment transaction. A
// BreachRetribution is created by the closeObserver if it detects an
// uncooperative close of the channel which uses a revoked commitment
// transaction. The BreachRetribution is then sent over the ContractBreach
// channel in order to allow the subscriber of the channel to dispatch justice.
type BreachRetribution struct {
	// BreachTxHash is the transaction hash which breached the channel
	// contract by spending from the funding multi-sig with a revoked
	// commitment transaction.
	BreachTxHash chainhash.Hash

	// BreachHeight records the block height confirming the breach
	// transaction, used as a height hint when registering for
	// confirmations.
	BreachHeight uint32

	// ChainHash is the chain that the contract beach was identified
	// within. This is also the resident chain of the contract (the chain
	// the contract was created on).
	ChainHash chainhash.Hash

	// RevokedStateNum is the revoked state number which was broadcast.
	RevokedStateNum uint64

	// LocalOutputSignDesc is a SignDescriptor which is capable of
	// generating the signature necessary to sweep the output within the
	// breach transaction that pays directly us.
	//
	// NOTE: A nil value indicates that the local output is considered dust
	// according to the remote party's dust limit.
	LocalOutputSignDesc *input.SignDescriptor

	// LocalOutpoint is the outpoint of the output paying to us (the local
	// party) within the breach transaction.
	LocalOutpoint wire.OutPoint

	// LocalDelay is the CSV delay for the to_remote script on the breached
	// commitment.
	LocalDelay uint32

	// RemoteOutputSignDesc is a SignDescriptor which is capable of
	// generating the signature required to claim the funds as described
	// within the revocation clause of the remote party's commitment
	// output.
	//
	// NOTE: A nil value indicates that the local output is considered dust
	// according to the remote party's dust limit.
	RemoteOutputSignDesc *input.SignDescriptor

	// RemoteOutpoint is the outpoint of the output paying to the remote
	// party within the breach transaction.
	RemoteOutpoint wire.OutPoint

	// RemoteDelay specifies the CSV delay applied to to-local scripts on
	// the breaching commitment transaction.
	RemoteDelay uint32

	// HtlcRetributions is a slice of HTLC retributions for each output
	// active HTLC output within the breached commitment transaction.
	HtlcRetributions []HtlcRetribution

	// KeyRing contains the derived public keys used to construct the
	// breaching commitment transaction. This allows downstream clients to
	// have access to the public keys used in the scripts.
	KeyRing *CommitmentKeyRing

	// LocalResolutionBlob is a blob used for aux channels that permits an
	// honest party to sweep the local commitment output.
	LocalResolutionBlob fn.Option[tlv.Blob]

	// RemoteResolutionBlob is a blob used for aux channels that permits an
	// honest party to sweep the remote commitment output.
	RemoteResolutionBlob fn.Option[tlv.Blob]
}

// NewBreachRetribution creates a new fully populated BreachRetribution for the
// passed channel, at a particular revoked state number. If the spend
// transaction that the breach retribution should target is known, then it can
// be provided via the spendTx parameter. Otherwise, if the spendTx parameter is
// nil, then the revocation log will be checked to see if it contains the info
// required to construct the BreachRetribution. If the revocation log is missing
// the required fields then ErrRevLogDataMissing will be returned.
func NewBreachRetribution(chanState *channeldb.OpenChannel, stateNum uint64,
	breachHeight uint32, spendTx *wire.MsgTx,
	leafStore fn.Option[AuxLeafStore],
	auxResolver fn.Option[AuxContractResolver]) (*BreachRetribution,
	error) {

	// Query the on-disk revocation log for the snapshot which was recorded
	// at this particular state num. Based on whether a legacy revocation
	// log is returned or not, we will process them differently.
	revokedLog, revokedLogLegacy, err := chanState.FindPreviousState(
		stateNum,
	)
	if err != nil {
		return nil, err
	}

	// Sanity check that at least one of the logs is returned.
	if revokedLog == nil && revokedLogLegacy == nil {
		return nil, ErrNoRevocationLogFound
	}

	// With the state number broadcast known, we can now derive/restore the
	// proper revocation preimage necessary to sweep the remote party's
	// output.
	revocationPreimage, err := chanState.RevocationStore.LookUp(stateNum)
	if err != nil {
		return nil, err
	}
	commitmentSecret, commitmentPoint := btcec.PrivKeyFromBytes(
		revocationPreimage[:],
	)

	// With the commitment point generated, we can now generate the four
	// keys we'll need to reconstruct the commitment state,
	keyRing := DeriveCommitmentKeys(
		commitmentPoint, lntypes.Remote, chanState.ChanType,
		&chanState.LocalChanCfg, &chanState.RemoteChanCfg,
	)

	// Next, reconstruct the scripts as they were present at this state
	// number so we can have the proper witness script to sign and include
	// within the final witness.
	var leaseExpiry uint32
	if chanState.ChanType.HasLeaseExpiration() {
		leaseExpiry = chanState.ThawHeight
	}

	auxResult, err := fn.MapOptionZ(
		leafStore, func(s AuxLeafStore) fn.Result[CommitDiffAuxResult] {
			return s.FetchLeavesFromRevocation(revokedLog)
		},
	).Unpack()
	if err != nil {
		return nil, fmt.Errorf("unable to fetch aux leaves: %w", err)
	}

	// Since it is the remote breach we are reconstructing, the output
	// going to us will be a to-remote script with our local params.
	remoteAuxLeaf := fn.FlatMapOption(
		func(l CommitAuxLeaves) input.AuxTapLeaf {
			return l.RemoteAuxLeaf
		},
	)(auxResult.AuxLeaves)
	isRemoteInitiator := !chanState.IsInitiator
	ourScript, ourDelay, err := CommitScriptToRemote(
		chanState.ChanType, isRemoteInitiator, keyRing.ToRemoteKey,
		leaseExpiry, remoteAuxLeaf,
	)
	if err != nil {
		return nil, err
	}

	localAuxLeaf := fn.FlatMapOption(
		func(l CommitAuxLeaves) input.AuxTapLeaf {
			return l.LocalAuxLeaf
		},
	)(auxResult.AuxLeaves)
	theirDelay := uint32(chanState.RemoteChanCfg.CsvDelay)
	theirScript, err := CommitScriptToSelf(
		chanState.ChanType, isRemoteInitiator, keyRing.ToLocalKey,
		keyRing.RevocationKey, theirDelay, leaseExpiry, localAuxLeaf,
	)
	if err != nil {
		return nil, err
	}

	// Define an empty breach retribution that will be overwritten based on
	// different version of the revocation log found.
	var br *BreachRetribution

	// Define our and their amounts, that will be overwritten below.
	var ourAmt, theirAmt int64

	// If the returned *RevocationLog is non-nil, use it to derive the info
	// we need.
	if revokedLog != nil {
		br, ourAmt, theirAmt, err = createBreachRetribution(
			revokedLog, spendTx, chanState, keyRing,
			commitmentSecret, leaseExpiry, auxResult.AuxLeaves,
		)
		if err != nil {
			return nil, err
		}
	} else {
		// The returned revocation log is in legacy format, which is a
		// *ChannelCommitment.
		//
		// NOTE: this branch is kept for compatibility such that for
		// old nodes which refuse to migrate the legacy revocation log
		// data can still function. This branch can be deleted once we
		// are confident that no legacy format is in use.
		br, ourAmt, theirAmt, err = createBreachRetributionLegacy(
			revokedLogLegacy, chanState, keyRing, commitmentSecret,
			ourScript, theirScript, leaseExpiry,
		)
		if err != nil {
			return nil, err
		}
	}

	// Conditionally instantiate a sign descriptor for each of the
	// commitment outputs. If either is considered dust using the remote
	// party's dust limit, the respective sign descriptor will be nil.
	//
	// If our balance exceeds the remote party's dust limit, instantiate
	// the sign descriptor for our output.
	if ourAmt >= int64(chanState.RemoteChanCfg.DustLimit) {
		// As we're about to sweep our own output w/o a delay, we'll
		// obtain the witness script for the success/delay path.
		witnessScript, err := ourScript.WitnessScriptForPath(
			input.ScriptPathDelay,
		)
		if err != nil {
			return nil, err
		}

		br.LocalOutputSignDesc = &input.SignDescriptor{
			SingleTweak:   keyRing.LocalCommitKeyTweak,
			KeyDesc:       chanState.LocalChanCfg.PaymentBasePoint,
			WitnessScript: witnessScript,
			Output: &wire.TxOut{
				PkScript: ourScript.PkScript(),
				Value:    ourAmt,
			},
			HashType: sweepSigHash(chanState.ChanType),
		}

		// For taproot channels, we'll make sure to set the script path
		// spend (as our output on their revoked tx still needs the
		// delay), and set the control block.
		if scriptTree, ok := ourScript.(input.TapscriptDescriptor); ok {
			//nolint:ll
			br.LocalOutputSignDesc.SignMethod = input.TaprootScriptSpendSignMethod

			ctrlBlock, err := scriptTree.CtrlBlockForPath(
				input.ScriptPathDelay,
			)
			if err != nil {
				return nil, err
			}

			//nolint:ll
			br.LocalOutputSignDesc.ControlBlock, err = ctrlBlock.ToBytes()
			if err != nil {
				return nil, err
			}
		}

		// At this point, we'll check to see if we need any extra
		// resolution data for this output.
		//
		//nolint:ll
		resolveReq := ResolutionReq{
			ChanPoint:           chanState.FundingOutpoint,
			ChanType:            chanState.ChanType,
			ShortChanID:         chanState.ShortChanID(),
			Initiator:           chanState.IsInitiator,
			FundingBlob:         chanState.CustomBlob,
			Type:                input.TaprootRemoteCommitSpend,
			CloseType:           Breach,
			CommitTx:            spendTx,
			CommitTxBlockHeight: breachHeight,
			SignDesc:            *br.LocalOutputSignDesc,
			KeyRing:             keyRing,
			CsvDelay:            ourDelay,
			BreachCsvDelay:      fn.Some(theirDelay),
			CommitFee:           chanState.RemoteCommitment.CommitFee,
		}
		if revokedLog != nil {
			resolveReq.CommitBlob = revokedLog.CustomBlob.ValOpt()
		}

		resolveBlob := fn.MapOptionZ(
			auxResolver,
			func(a AuxContractResolver) fn.Result[tlv.Blob] {
				return a.ResolveContract(resolveReq)
			},
		)
		if err := resolveBlob.Err(); err != nil {
			return nil, fmt.Errorf("unable to aux resolve: %w", err)
		}

		br.LocalResolutionBlob = resolveBlob.OkToSome()
	}

	// Similarly, if their balance exceeds the remote party's dust limit,
	// assemble the sign descriptor for their output, which we can sweep.
	if theirAmt >= int64(chanState.RemoteChanCfg.DustLimit) {
		// As we're trying to defend the channel against a breach
		// attempt from the remote party, we want to obain the
		// revocation witness script here.
		witnessScript, err := theirScript.WitnessScriptForPath(
			input.ScriptPathRevocation,
		)
		if err != nil {
			return nil, err
		}

		br.RemoteOutputSignDesc = &input.SignDescriptor{
			KeyDesc: chanState.LocalChanCfg.
				RevocationBasePoint,
			DoubleTweak:   commitmentSecret,
			WitnessScript: witnessScript,
			Output: &wire.TxOut{
				PkScript: theirScript.PkScript(),
				Value:    theirAmt,
			},
			HashType: sweepSigHash(chanState.ChanType),
		}

		// For taproot channels, the remote output (the revoked output)
		// is spent with a script path to ensure all information 3rd
		// parties need to sweep anchors is revealed on chain.
		scriptTree, ok := theirScript.(input.TapscriptDescriptor)
		if ok {
			//nolint:ll
			br.RemoteOutputSignDesc.SignMethod = input.TaprootScriptSpendSignMethod

			ctrlBlock, err := scriptTree.CtrlBlockForPath(
				input.ScriptPathRevocation,
			)
			if err != nil {
				return nil, err
			}
			//nolint:ll
			br.RemoteOutputSignDesc.ControlBlock, err = ctrlBlock.ToBytes()
			if err != nil {
				return nil, err
			}
		}

		// At this point, we'll check to see if we need any extra
		// resolution data for this output.
		//
		//nolint:ll
		resolveReq := ResolutionReq{
			ChanPoint:           chanState.FundingOutpoint,
			ChanType:            chanState.ChanType,
			ShortChanID:         chanState.ShortChanID(),
			Initiator:           chanState.IsInitiator,
			FundingBlob:         chanState.CustomBlob,
			Type:                input.TaprootCommitmentRevoke,
			CloseType:           Breach,
			CommitTx:            spendTx,
			CommitTxBlockHeight: breachHeight,
			SignDesc:            *br.RemoteOutputSignDesc,
			KeyRing:             keyRing,
			CsvDelay:            theirDelay,
			BreachCsvDelay:      fn.Some(theirDelay),
			CommitFee:           chanState.RemoteCommitment.CommitFee,
		}
		if revokedLog != nil {
			resolveReq.CommitBlob = revokedLog.CustomBlob.ValOpt()
		}
		resolveBlob := fn.MapOptionZ(
			auxResolver,
			func(a AuxContractResolver) fn.Result[tlv.Blob] {
				return a.ResolveContract(resolveReq)
			},
		)
		if err := resolveBlob.Err(); err != nil {
			return nil, fmt.Errorf("unable to aux resolve: %w", err)
		}

		br.RemoteResolutionBlob = resolveBlob.OkToSome()
	}

	// Finally, with all the necessary data constructed, we can pad the
	// BreachRetribution struct which houses all the data necessary to
	// swiftly bring justice to the cheating remote party.
	br.BreachHeight = breachHeight
	br.RevokedStateNum = stateNum
	br.LocalDelay = ourDelay
	br.RemoteDelay = theirDelay

	return br, nil
}

// createHtlcRetribution is a helper function to construct an HtlcRetribution
// based on the passed params.
func createHtlcRetribution(chanState *channeldb.OpenChannel,
	keyRing *CommitmentKeyRing, commitHash chainhash.Hash,
	commitmentSecret *btcec.PrivateKey, leaseExpiry uint32,
	htlc *channeldb.HTLCEntry,
	auxLeaves fn.Option[CommitAuxLeaves]) (HtlcRetribution, error) {

	var emptyRetribution HtlcRetribution

	theirDelay := uint32(chanState.RemoteChanCfg.CsvDelay)
	isRemoteInitiator := !chanState.IsInitiator

	// We'll generate the original second level witness script now, as
	// we'll need it if we're revoking an HTLC output on the remote
	// commitment transaction, and *they* go to the second level.
	//nolint:ll
	secondLevelAuxLeaf := fn.FlatMapOption(
		func(l CommitAuxLeaves) fn.Option[input.AuxTapLeaf] {
			return fn.MapOption(
				func(val tlv.BigSizeT[uint64]) input.AuxTapLeaf {
					idx := val.Int()

					if htlc.Incoming.Val {
						leaves := l.IncomingHtlcLeaves[idx]
						return leaves.SecondLevelLeaf
					}

					return l.OutgoingHtlcLeaves[idx].SecondLevelLeaf
				},
			)(htlc.HtlcIndex.ValOpt())
		},
	)(auxLeaves)
	secondLevelScript, err := SecondLevelHtlcScript(
		chanState.ChanType, isRemoteInitiator,
		keyRing.RevocationKey, keyRing.ToLocalKey, theirDelay,
		leaseExpiry, fn.FlattenOption(secondLevelAuxLeaf),
	)
	if err != nil {
		return emptyRetribution, err
	}

	// If this is an incoming HTLC, then this means that they were the
	// sender of the HTLC (relative to us). So we'll re-generate the sender
	// HTLC script. Otherwise, if this was an outgoing HTLC that we sent,
	// then from the PoV of the remote commitment state, they're the
	// receiver of this HTLC.
	//nolint:ll
	htlcLeaf := fn.FlatMapOption(
		func(l CommitAuxLeaves) fn.Option[input.AuxTapLeaf] {
			return fn.MapOption(
				func(val tlv.BigSizeT[uint64]) input.AuxTapLeaf {
					idx := val.Int()

					if htlc.Incoming.Val {
						leaves := l.IncomingHtlcLeaves[idx]
						return leaves.AuxTapLeaf
					}

					return l.OutgoingHtlcLeaves[idx].AuxTapLeaf
				},
			)(htlc.HtlcIndex.ValOpt())
		},
	)(auxLeaves)
	scriptInfo, err := genHtlcScript(
		chanState.ChanType, htlc.Incoming.Val, lntypes.Remote,
		htlc.RefundTimeout.Val, htlc.RHash.Val, keyRing,
		fn.FlattenOption(htlcLeaf),
	)
	if err != nil {
		return emptyRetribution, err
	}

	signDesc := input.SignDescriptor{
		KeyDesc: chanState.LocalChanCfg.
			RevocationBasePoint,
		DoubleTweak:   commitmentSecret,
		WitnessScript: scriptInfo.WitnessScriptToSign(),
		Output: &wire.TxOut{
			PkScript: scriptInfo.PkScript(),
			Value:    int64(htlc.Amt.Val.Int()),
		},
		HashType: sweepSigHash(chanState.ChanType),
	}

	// For taproot HTLC outputs, we need to set the sign method to key
	// spend, and also set the tap tweak root needed to derive the proper
	// private key.
	if scriptTree, ok := scriptInfo.(input.TapscriptDescriptor); ok {
		signDesc.SignMethod = input.TaprootKeySpendSignMethod

		signDesc.TapTweak = scriptTree.TapTweak()
	}

	// The second level script we sign will always be the success path.
	secondLevelWitnessScript, err := secondLevelScript.WitnessScriptForPath(
		input.ScriptPathSuccess,
	)
	if err != nil {
		return emptyRetribution, err
	}

	// If this is a taproot output, we'll also need to obtain the second
	// level tap tweak as well.
	var secondLevelTapTweak [32]byte
	if scriptTree, ok := secondLevelScript.(input.TapscriptDescriptor); ok {
		copy(secondLevelTapTweak[:], scriptTree.TapTweak())
	}

	return HtlcRetribution{
		SignDesc: signDesc,
		OutPoint: wire.OutPoint{
			Hash:  commitHash,
			Index: uint32(htlc.OutputIndex.Val),
		},
		SecondLevelWitnessScript: secondLevelWitnessScript,
		IsIncoming:               htlc.Incoming.Val,
		SecondLevelTapTweak:      secondLevelTapTweak,
	}, nil
}

// createBreachRetribution creates a partially initiated BreachRetribution
// using a RevocationLog. Returns the constructed retribution, our amount,
// their amount, and a possible non-nil error. If the spendTx parameter is
// non-nil, then it will be used to glean the breach transaction's to-local and
// to-remote output amounts. Otherwise, the RevocationLog will be checked to
// see if these fields are present there. If they are not, then
// ErrRevLogDataMissing is returned.
func createBreachRetribution(revokedLog *channeldb.RevocationLog,
	spendTx *wire.MsgTx, chanState *channeldb.OpenChannel,
	keyRing *CommitmentKeyRing, commitmentSecret *btcec.PrivateKey,
	leaseExpiry uint32,
	auxLeaves fn.Option[CommitAuxLeaves]) (*BreachRetribution, int64, int64,
	error) {

	commitHash := revokedLog.CommitTxHash

	// Create the htlc retributions.
	htlcRetributions := make([]HtlcRetribution, len(revokedLog.HTLCEntries))
	for i, htlc := range revokedLog.HTLCEntries {
		hr, err := createHtlcRetribution(
			chanState, keyRing, commitHash.Val,
			commitmentSecret, leaseExpiry, htlc, auxLeaves,
		)
		if err != nil {
			return nil, 0, 0, err
		}
		htlcRetributions[i] = hr
	}

	var ourAmt, theirAmt int64

	// Construct the our outpoint.
	ourOutpoint := wire.OutPoint{
		Hash: commitHash.Val,
	}
	if revokedLog.OurOutputIndex.Val != channeldb.OutputIndexEmpty {
		ourOutpoint.Index = uint32(revokedLog.OurOutputIndex.Val)

		// If the spend transaction is provided, then we use it to get
		// the value of our output.
		if spendTx != nil {
			// Sanity check that OurOutputIndex is within range.
			if int(ourOutpoint.Index) >= len(spendTx.TxOut) {
				return nil, 0, 0, fmt.Errorf("%w: ours=%v, "+
					"len(TxOut)=%v",
					ErrOutputIndexOutOfRange,
					ourOutpoint.Index, len(spendTx.TxOut),
				)
			}
			// Read the amounts from the breach transaction.
			//
			// NOTE: ourAmt here includes commit fee and anchor
			// amount (if enabled).
			ourAmt = spendTx.TxOut[ourOutpoint.Index].Value
		} else {
			// Otherwise, we check to see if the revocation log
			// contains our output amount. Due to a previous
			// migration, this field may be empty in which case an
			// error will be returned.
			b, err := revokedLog.OurBalance.ValOpt().UnwrapOrErr(
				ErrRevLogDataMissing,
			)
			if err != nil {
				return nil, 0, 0, err
			}

			ourAmt = int64(b.Int().ToSatoshis())
		}
	}

	// Construct the their outpoint.
	theirOutpoint := wire.OutPoint{
		Hash: commitHash.Val,
	}
	if revokedLog.TheirOutputIndex.Val != channeldb.OutputIndexEmpty {
		theirOutpoint.Index = uint32(revokedLog.TheirOutputIndex.Val)

		// If the spend transaction is provided, then we use it to get
		// the value of the remote parties' output.
		if spendTx != nil {
			// Sanity check that TheirOutputIndex is within range.
			if int(revokedLog.TheirOutputIndex.Val) >=
				len(spendTx.TxOut) {

				return nil, 0, 0, fmt.Errorf("%w: theirs=%v, "+
					"len(TxOut)=%v",
					ErrOutputIndexOutOfRange,
					revokedLog.TheirOutputIndex,
					len(spendTx.TxOut),
				)
			}

			// Read the amounts from the breach transaction.
			theirAmt = spendTx.TxOut[theirOutpoint.Index].Value
		} else {
			// Otherwise, we check to see if the revocation log
			// contains remote parties' output amount. Due to a
			// previous migration, this field may be empty in which
			// case an error will be returned.
			b, err := revokedLog.TheirBalance.ValOpt().UnwrapOrErr(
				ErrRevLogDataMissing,
			)
			if err != nil {
				return nil, 0, 0, err
			}

			theirAmt = int64(b.Int().ToSatoshis())
		}
	}

	return &BreachRetribution{
		BreachTxHash:     commitHash.Val,
		ChainHash:        chanState.ChainHash,
		LocalOutpoint:    ourOutpoint,
		RemoteOutpoint:   theirOutpoint,
		HtlcRetributions: htlcRetributions,
		KeyRing:          keyRing,
	}, ourAmt, theirAmt, nil
}

// createBreachRetributionLegacy creates a partially initiated
// BreachRetribution using a ChannelCommitment. Returns the constructed
// retribution, our amount, their amount, and a possible non-nil error.
func createBreachRetributionLegacy(revokedLog *channeldb.ChannelCommitment,
	chanState *channeldb.OpenChannel, keyRing *CommitmentKeyRing,
	commitmentSecret *btcec.PrivateKey,
	ourScript, theirScript input.ScriptDescriptor,
	leaseExpiry uint32) (*BreachRetribution, int64, int64, error) {

	commitHash := revokedLog.CommitTx.TxHash()
	ourOutpoint := wire.OutPoint{
		Hash: commitHash,
	}
	theirOutpoint := wire.OutPoint{
		Hash: commitHash,
	}

	// In order to fully populate the breach retribution struct, we'll need
	// to find the exact index of the commitment outputs.
	for i, txOut := range revokedLog.CommitTx.TxOut {
		switch {
		case bytes.Equal(txOut.PkScript, ourScript.PkScript()):
			ourOutpoint.Index = uint32(i)
		case bytes.Equal(txOut.PkScript, theirScript.PkScript()):
			theirOutpoint.Index = uint32(i)
		}
	}

	// With the commitment outputs located, we'll now generate all the
	// retribution structs for each of the HTLC transactions active on the
	// remote commitment transaction.
	htlcRetributions := make([]HtlcRetribution, len(revokedLog.Htlcs))
	for i, htlc := range revokedLog.Htlcs {
		// If the HTLC is dust, then we'll skip it as it doesn't have
		// an output on the commitment transaction.
		if HtlcIsDust(
			chanState.ChanType, htlc.Incoming, lntypes.Remote,
			chainfee.SatPerKWeight(revokedLog.FeePerKw),
			htlc.Amt.ToSatoshis(),
			chanState.RemoteChanCfg.DustLimit,
		) {

			continue
		}

		entry, err := channeldb.NewHTLCEntryFromHTLC(htlc)
		if err != nil {
			return nil, 0, 0, err
		}

		hr, err := createHtlcRetribution(
			chanState, keyRing, commitHash,
			commitmentSecret, leaseExpiry, entry,
			fn.None[CommitAuxLeaves](),
		)
		if err != nil {
			return nil, 0, 0, err
		}
		htlcRetributions[i] = hr
	}

	// Compute the balances in satoshis.
	ourAmt := int64(revokedLog.LocalBalance.ToSatoshis())
	theirAmt := int64(revokedLog.RemoteBalance.ToSatoshis())

	return &BreachRetribution{
		BreachTxHash:     commitHash,
		ChainHash:        chanState.ChainHash,
		LocalOutpoint:    ourOutpoint,
		RemoteOutpoint:   theirOutpoint,
		HtlcRetributions: htlcRetributions,
		KeyRing:          keyRing,
	}, ourAmt, theirAmt, nil
}

// HtlcIsDust determines if an HTLC output is dust or not depending on two
// bits: if the HTLC is incoming and if the HTLC will be placed on our
// commitment transaction, or theirs. These two pieces of information are
// required as we currently used second-level HTLC transactions as off-chain
// covenants. Depending on the two bits, we'll either be using a timeout or
// success transaction which have different weights.
func HtlcIsDust(chanType channeldb.ChannelType,
	incoming bool, whoseCommit lntypes.ChannelParty,
	feePerKw chainfee.SatPerKWeight, htlcAmt, dustLimit btcutil.Amount,
) bool {

	// First we'll determine the fee required for this HTLC based on if this is
	// an incoming HTLC or not, and also on whose commitment transaction it
	// will be placed on.
	var htlcFee btcutil.Amount
	switch {

	// If this is an incoming HTLC on our commitment transaction, then the
	// second-level transaction will be a success transaction.
	case incoming && whoseCommit.IsLocal():
		htlcFee = HtlcSuccessFee(chanType, feePerKw)

	// If this is an incoming HTLC on their commitment transaction, then
	// we'll be using a second-level timeout transaction as they've added
	// this HTLC.
	case incoming && whoseCommit.IsRemote():
		htlcFee = HtlcTimeoutFee(chanType, feePerKw)

	// If this is an outgoing HTLC on our commitment transaction, then
	// we'll be using a timeout transaction as we're the sender of the
	// HTLC.
	case !incoming && whoseCommit.IsLocal():
		htlcFee = HtlcTimeoutFee(chanType, feePerKw)

	// If this is an outgoing HTLC on their commitment transaction, then
	// we'll be using an HTLC success transaction as they're the receiver
	// of this HTLC.
	case !incoming && whoseCommit.IsRemote():
		htlcFee = HtlcSuccessFee(chanType, feePerKw)
	}

	return (htlcAmt - htlcFee) < dustLimit
}

// HtlcView represents the "active" HTLCs at a particular point within the
// history of the HTLC update log.
type HtlcView struct {
	// NextHeight is the height of the commitment transaction that will be
	// created using this view.
	NextHeight uint64

	// Updates is a Dual of the Local and Remote HTLCs.
	Updates lntypes.Dual[[]*paymentDescriptor]

	// FeePerKw is the fee rate in sat/kw of the commitment transaction.
	FeePerKw chainfee.SatPerKWeight
}

// AuxOurUpdates returns the outgoing HTLCs as a read-only copy of
// AuxHtlcDescriptors.
func (v *HtlcView) AuxOurUpdates() []AuxHtlcDescriptor {
	return fn.Map(v.Updates.Local, newAuxHtlcDescriptor)
}

// AuxTheirUpdates returns the incoming HTLCs as a read-only copy of
// AuxHtlcDescriptors.
func (v *HtlcView) AuxTheirUpdates() []AuxHtlcDescriptor {
	return fn.Map(v.Updates.Remote, newAuxHtlcDescriptor)
}

// FetchLatestAuxHTLCView returns the latest HTLC view of the lightning channel
// as a safe copy that can be used outside the wallet code in concurrent access.
func (lc *LightningChannel) FetchLatestAuxHTLCView() AuxHtlcView {
	// This read lock is important, because we access both the local and
	// remote log indexes as well as the underlying payment descriptors of
	// the HTLCs when creating the view.
	lc.RLock()
	defer lc.RUnlock()

	return newAuxHtlcView(lc.fetchHTLCView(
		lc.updateLogs.Remote.logIndex, lc.updateLogs.Local.logIndex,
	))
}

// fetchHTLCView returns all the candidate HTLC updates which should be
// considered for inclusion within a commitment based on the passed HTLC log
// indexes.
func (lc *LightningChannel) fetchHTLCView(theirLogIndex,
	ourLogIndex uint64) *HtlcView {

	var ourHTLCs []*paymentDescriptor
	for e := lc.updateLogs.Local.Front(); e != nil; e = e.Next() {
		htlc := e.Value

		// This HTLC is active from this point-of-view iff the log
		// index of the state update is below the specified index in
		// our update log.
		if htlc.LogIndex < ourLogIndex {
			ourHTLCs = append(ourHTLCs, htlc)
		}
	}

	var theirHTLCs []*paymentDescriptor
	for e := lc.updateLogs.Remote.Front(); e != nil; e = e.Next() {
		htlc := e.Value

		// If this is an incoming HTLC, then it is only active from
		// this point-of-view if the index of the HTLC addition in
		// their log is below the specified view index.
		if htlc.LogIndex < theirLogIndex {
			theirHTLCs = append(theirHTLCs, htlc)
		}
	}

	return &HtlcView{
		Updates: lntypes.Dual[[]*paymentDescriptor]{
			Local:  ourHTLCs,
			Remote: theirHTLCs,
		},
	}
}

// fetchCommitmentView returns a populated commitment which expresses the state
// of the channel from the point of view of a local or remote chain, evaluating
// the HTLC log up to the passed indexes. This function is used to construct
// both local and remote commitment transactions in order to sign or verify new
// commitment updates. A fully populated commitment is returned which reflects
// the proper balances for both sides at this point in the commitment chain.
func (lc *LightningChannel) fetchCommitmentView(
	whoseCommitChain lntypes.ChannelParty,
	ourLogIndex, ourHtlcIndex, theirLogIndex, theirHtlcIndex uint64,
	keyRing *CommitmentKeyRing) (*commitment, error) {

	commitChain := lc.commitChains.Local
	dustLimit := lc.channelState.LocalChanCfg.DustLimit
	if whoseCommitChain.IsRemote() {
		commitChain = lc.commitChains.Remote
		dustLimit = lc.channelState.RemoteChanCfg.DustLimit
	}

	nextHeight := commitChain.tip().height + 1

	// Run through all the HTLCs that will be covered by this transaction
	// in order to update their commitment addition height, and to adjust
	// the balances on the commitment transaction accordingly. Note that
	// these balances will be *before* taking a commitment fee from the
	// initiator.
	htlcView := lc.fetchHTLCView(theirLogIndex, ourLogIndex)
	ourBalance, theirBalance, _, filteredHTLCView, err := lc.computeView(
		htlcView, whoseCommitChain, true,
		fn.None[chainfee.SatPerKWeight](),
	)
	if err != nil {
		return nil, err
	}
	feePerKw := filteredHTLCView.FeePerKw

	htlcView.NextHeight = nextHeight
	filteredHTLCView.NextHeight = nextHeight

	// Actually generate unsigned commitment transaction for this view.
	commitTx, err := lc.commitBuilder.createUnsignedCommitmentTx(
		ourBalance, theirBalance, whoseCommitChain, feePerKw,
		nextHeight, htlcView, filteredHTLCView, keyRing,
		commitChain.tip(),
	)
	if err != nil {
		return nil, err
	}

	// We'll assert that there hasn't been a mistake during fee calculation
	// leading to a fee too low.
	var totalOut btcutil.Amount
	for _, txOut := range commitTx.txn.TxOut {
		totalOut += btcutil.Amount(txOut.Value)
	}
	fee := lc.channelState.Capacity - totalOut

	var witnessWeight int64
	if lc.channelState.ChanType.IsTaproot() {
		witnessWeight = input.TaprootKeyPathWitnessSize
	} else {
		witnessWeight = input.WitnessCommitmentTxWeight
	}

	// Since the transaction is not signed yet, we use the witness weight
	// used for weight calculation.
	uTx := btcutil.NewTx(commitTx.txn)
	weight := blockchain.GetTransactionWeight(uTx) + witnessWeight

	effFeeRate := chainfee.SatPerKWeight(fee) * 1000 /
		chainfee.SatPerKWeight(weight)
	if effFeeRate < chainfee.AbsoluteFeePerKwFloor {
		return nil, fmt.Errorf("height=%v, for ChannelPoint(%v) "+
			"attempts to create commitment with feerate %v: %v",
			nextHeight, lc.channelState.FundingOutpoint,
			effFeeRate, lnutils.SpewLogClosure(commitTx))
	}

	// Given the custom blob of the past state, and this new HTLC view,
	// we'll generate a new blob for the latest commitment.
	newCommitBlob, err := fn.MapOptionZ(
		lc.leafStore,
		func(s AuxLeafStore) fn.Result[fn.Option[tlv.Blob]] {
			return updateAuxBlob(
				s, lc.channelState,
				commitChain.tip().customBlob, htlcView,
				whoseCommitChain, ourBalance, theirBalance,
				*keyRing,
			)
		},
	).Unpack()
	if err != nil {
		return nil, fmt.Errorf("unable to fetch aux leaves: %w", err)
	}

	messageIndices := lntypes.Dual[uint64]{
		Local:  ourLogIndex,
		Remote: theirLogIndex,
	}

	// With the commitment view created, store the resulting balances and
	// transaction with the other parameters for this height.
	c := &commitment{
		ourBalance:     commitTx.ourBalance,
		theirBalance:   commitTx.theirBalance,
		txn:            commitTx.txn,
		fee:            commitTx.fee,
		messageIndices: messageIndices,
		ourHtlcIndex:   ourHtlcIndex,
		theirHtlcIndex: theirHtlcIndex,
		height:         nextHeight,
		feePerKw:       feePerKw,
		dustLimit:      dustLimit,
		whoseCommit:    whoseCommitChain,
		customBlob:     newCommitBlob,
	}

	// In order to ensure _none_ of the HTLC's associated with this new
	// commitment are mutated, we'll manually copy over each HTLC to its
	// respective slice.
	c.outgoingHTLCs = make(
		[]paymentDescriptor, len(filteredHTLCView.Updates.Local),
	)
	for i, htlc := range filteredHTLCView.Updates.Local {
		c.outgoingHTLCs[i] = *htlc
	}
	c.incomingHTLCs = make(
		[]paymentDescriptor, len(filteredHTLCView.Updates.Remote),
	)
	for i, htlc := range filteredHTLCView.Updates.Remote {
		c.incomingHTLCs[i] = *htlc
	}

	// Finally, we'll populate all the HTLC indexes so we can track the
	// locations of each HTLC in the commitment state. We pass in the sorted
	// slice of CLTV deltas in order to properly locate HTLCs that otherwise
	// have the same payment hash and amount.
	err = c.populateHtlcIndexes(lc.channelState.ChanType, commitTx.cltvs)
	if err != nil {
		return nil, err
	}

	return c, nil
}

// fundingTxIn returns the funding output as a transaction input. The input
// returned by this function uses a max sequence number, so it isn't able to be
// used with RBF by default.
func fundingTxIn(chanState *channeldb.OpenChannel) wire.TxIn {
	return *wire.NewTxIn(&chanState.FundingOutpoint, nil, nil)
}

// evaluateHTLCView processes all update entries in both HTLC update logs,
// producing a final view which is the result of properly applying all adds,
// settles, timeouts and fee updates found in both logs. The resulting view
// returned reflects the current state of HTLCs within the remote or local
// commitment chain, and the current commitment fee rate.
//
// The return values of this function are as follows:
// 1. The new htlcView reflecting the current channel state.
// 2. A Dual of the updates which have not yet been committed in
// 'whoseCommitChain's commitment chain.
func (lc *LightningChannel) evaluateHTLCView(view *HtlcView,
	whoseCommitChain lntypes.ChannelParty, nextHeight uint64) (*HtlcView,
	lntypes.Dual[[]*paymentDescriptor], lntypes.Dual[int64], error) {

	// We initialize the view's fee rate to the fee rate of the unfiltered
	// view. If any fee updates are found when evaluating the view, it will
	// be updated.
	newView := &HtlcView{
		FeePerKw:   view.FeePerKw,
		NextHeight: nextHeight,
	}
	noUncommitted := lntypes.Dual[[]*paymentDescriptor]{}

	// The fee rate of our view is always the last UpdateFee message from
	// the channel's OpeningParty.
	openerUpdates := view.Updates.GetForParty(lc.channelState.Initiator())
	feeUpdates := fn.Filter(openerUpdates, func(u *paymentDescriptor) bool {
		return u.EntryType == FeeUpdate
	})
	lastFeeUpdate := fn.Last(feeUpdates)
	lastFeeUpdate.WhenSome(func(pd *paymentDescriptor) {
		newView.FeePerKw = chainfee.SatPerKWeight(
			pd.Amount.ToSatoshis(),
		)
	})

	// We use two maps, one for the local log and one for the remote log to
	// keep track of which entries we need to skip when creating the final
	// htlc view. We skip an entry whenever we find a settle or a timeout
	// modifying an entry.
	skip := lntypes.Dual[fn.Set[uint64]]{
		Local:  fn.NewSet[uint64](),
		Remote: fn.NewSet[uint64](),
	}

	balanceDeltas := lntypes.Dual[int64]{}

	parties := [2]lntypes.ChannelParty{lntypes.Local, lntypes.Remote}
	for _, party := range parties {
		// First we run through non-add entries in both logs,
		// populating the skip sets.
		resolutions := fn.Filter(
			view.Updates.GetForParty(party),
			func(pd *paymentDescriptor) bool {
				switch pd.EntryType {
				case Settle, Fail, MalformedFail:
					return true
				default:
					return false
				}
			},
		)

		for _, entry := range resolutions {
			addEntry, err := lc.fetchParent(
				entry, whoseCommitChain, party.CounterParty(),
			)
			if err != nil {
				noDeltas := lntypes.Dual[int64]{}
				return nil, noUncommitted, noDeltas, err
			}

			skipSet := skip.GetForParty(party.CounterParty())
			skipSet.Add(addEntry.HtlcIndex)

			rmvHeight := entry.removeCommitHeights.GetForParty(
				whoseCommitChain,
			)
			if rmvHeight == 0 {
				switch {
				// If this a noop add, then when we settle the
				// HTLC, we may credit the sender with the
				// amount again, thus making it a noop. Noop
				// HTLCs are only triggered by external software
				// using the AuxComponents and only for channels
				// that use the custom tapscript root. The
				// criteria about whether the noop will be
				// effective is whether the receiver is already
				// sitting above reserve.
				case entry.EntryType == Settle &&
					addEntry.EntryType == NoOpAdd:

					lc.evaluateNoOpHtlc(
						entry, party, &balanceDeltas,
					)

				// If an incoming HTLC is being settled, then
				// this means that the preimage has been
				// received by the settling party Therefore, we
				// increase the settling party's balance by the
				// HTLC amount.
				case entry.EntryType == Settle:
					delta := int64(entry.Amount)
					balanceDeltas.ModifyForParty(
						party,
						func(acc int64) int64 {
							return acc + delta
						},
					)

				// Otherwise, this HTLC is being failed out,
				// therefore the value of the HTLC should
				// return to the failing party's counterparty.
				case entry.EntryType != Settle:
					delta := int64(entry.Amount)
					balanceDeltas.ModifyForParty(
						party.CounterParty(),
						func(acc int64) int64 {
							return acc + delta
						},
					)
				}
			}
		}
	}

	// Next we take a second pass through all the log entries, skipping any
	// settled HTLCs, and debiting the chain state balance due to any newly
	// added HTLCs.
	for _, party := range parties {
		liveAdds := fn.Filter(
			view.Updates.GetForParty(party),
			func(pd *paymentDescriptor) bool {
				isAdd := pd.isAdd()
				shouldSkip := skip.GetForParty(party).
					Contains(pd.HtlcIndex)

				return isAdd && !shouldSkip
			},
		)

		for _, entry := range liveAdds {
			// Skip the entries that have already had their add
			// commit height set for this commit chain.
			addHeight := entry.addCommitHeights.GetForParty(
				whoseCommitChain,
			)
			if addHeight == 0 {
				// If this is a new incoming (un-committed)
				// HTLC, then we need to update their balance
				// accordingly by subtracting the amount of
				// the HTLC that are funds pending.
				// Similarly, we need to debit our balance if
				// this is an out going HTLC to reflect the
				// pending balance.
				balanceDeltas.ModifyForParty(
					party,
					func(acc int64) int64 {
						return acc - int64(entry.Amount)
					},
				)
			}
		}

		newView.Updates.SetForParty(party, liveAdds)
	}

	// Create a function that is capable of identifying whether or not the
	// paymentDescriptor has been committed in the commitment chain
	// corresponding to whoseCommitmentChain.
	isUncommitted := func(update *paymentDescriptor) bool {
		switch update.EntryType {
		case Add, NoOpAdd:
			return update.addCommitHeights.GetForParty(
				whoseCommitChain,
			) == 0

		case FeeUpdate:
			return update.addCommitHeights.GetForParty(
				whoseCommitChain,
			) == 0

		case Settle, Fail, MalformedFail:
			return update.removeCommitHeights.GetForParty(
				whoseCommitChain,
			) == 0

		default:
			panic("invalid paymentDescriptor EntryType")
		}
	}

	// Collect all of the updates that haven't had their commit heights set
	// for the commitment chain corresponding to whoseCommitmentChain.
	uncommittedUpdates := lntypes.MapDual(
		view.Updates,
		func(us []*paymentDescriptor) []*paymentDescriptor {
			return fn.Filter(us, isUncommitted)
		},
	)

	return newView, uncommittedUpdates, balanceDeltas, nil
}

// fetchParent is a helper that looks up update log parent entries in the
// appropriate log.
func (lc *LightningChannel) fetchParent(entry *paymentDescriptor,
	whoseCommitChain, whoseUpdateLog lntypes.ChannelParty,
) (*paymentDescriptor, error) {

	var (
		updateLog *updateLog
		logName   string
	)

	if whoseUpdateLog.IsRemote() {
		updateLog = lc.updateLogs.Remote
		logName = "remote"
	} else {
		updateLog = lc.updateLogs.Local
		logName = "local"
	}

	addEntry := updateLog.lookupHtlc(entry.ParentIndex)

	switch {
	// We check if the parent entry is not found at this point.
	// This could happen for old versions of lnd, and we return an
	// error to gracefully shut down the state machine if such an
	// entry is still in the logs.
	case addEntry == nil:
		return nil, fmt.Errorf("unable to find parent entry "+
			"%d in %v update log: %v\nUpdatelog: %v",
			entry.ParentIndex, logName,
			lnutils.SpewLogClosure(entry),
			lnutils.SpewLogClosure(updateLog))

	// The parent add height should never be zero at this point. If
	// that's the case we probably forgot to send a new commitment.
	case addEntry.addCommitHeights.GetForParty(whoseCommitChain) == 0:
		return nil, fmt.Errorf("parent entry %d for update %d "+
			"had zero %v add height", entry.ParentIndex,
			entry.LogIndex, whoseCommitChain)
	}

	return addEntry, nil
}

// balanceAboveReserve checks if the balance for the provided party is above the
// configured reserve. It also uses the balance delta for the party, to account
// for entry amounts that have been processed already.
func balanceAboveReserve(party lntypes.ChannelParty, delta int64,
	channel *channeldb.OpenChannel) bool {

	// We're going to access the channel state, so let's make sure we're
	// holding the lock.
	channel.RLock()
	defer channel.RUnlock()

	// For calculating whether a party is above reserve we are going to
	// use the channel state local/remote balance of the corresponding
	// commitment. This balance corresponds to the balance of each party
	// after the most recent revocation. That's the balance on top of which
	// we may apply the balance delta of the currently processed HTLCs. It
	// is important for the calculated balance to match between us and our
	// peer, as any disagreement over the balances here can lead to a force
	// closure.
	c := channel

	localReserve := lnwire.NewMSatFromSatoshis(c.LocalChanCfg.ChanReserve)
	remoteReserve := lnwire.NewMSatFromSatoshis(c.RemoteChanCfg.ChanReserve)

	switch {
	case party.IsLocal():
		// For the local party we'll consult the local balance of the
		// local commitment. Then we'll correctly add the delta based on
		// whether it's negative or not.
		totalLocal := c.LocalCommitment.LocalBalance
		if delta >= 0 {
			totalLocal += lnwire.MilliSatoshi(delta)
		} else {
			totalLocal -= lnwire.MilliSatoshi(-1 * delta)
		}

		return totalLocal > localReserve

	case party.IsRemote():
		// For the remote party we'll consult the remote balance of the
		// remote commitment. Then we'll correctly add the delta based
		// on whether it's negative or not.
		totalRemote := c.RemoteCommitment.RemoteBalance
		if delta >= 0 {
			totalRemote += lnwire.MilliSatoshi(delta)
		} else {
			totalRemote -= lnwire.MilliSatoshi(-1 * delta)
		}

		return totalRemote > remoteReserve
	}

	return false
}

// evaluateNoOpHtlc applies the balance delta based on whether the NoOp HTLC is
// considered effective. This depends on whether the receiver is already above
// the channel reserve.
func (lc *LightningChannel) evaluateNoOpHtlc(entry *paymentDescriptor,
	party lntypes.ChannelParty, balanceDeltas *lntypes.Dual[int64]) {

	channel := lc.channelState
	delta := balanceDeltas.GetForParty(party)

	// If the receiver has existing balance above reserve then we go ahead
	// with crediting the amount back to the sender. Otherwise we give the
	// amount to the receiver. We do this because the receiver needs some
	// above reserve balance to anchor the AuxBlob. We also pass in the so
	// far calculated delta for the party, as that's effectively part of
	// their balance within this view computation.
	if balanceAboveReserve(party, delta, channel) {
		party = party.CounterParty()

		// The noop is effective, meaning that the settlement will
		// credit the amount back to the sender. Let's mark this as it
		// may be needed later when processing the settle entry, where
		// we won't be able to perform the above check again.
		entry.noOpSettle = true
	}

	d := int64(entry.Amount)
	balanceDeltas.ModifyForParty(party, func(acc int64) int64 {
		return acc + d
	})
}

// generateRemoteHtlcSigJobs generates a series of HTLC signature jobs for the
// sig pool, along with a channel that if closed, will cancel any jobs after
// they have been submitted to the sigPool. This method is to be used when
// generating a new commitment for the remote party. The jobs generated by the
// signature can be submitted to the sigPool to generate all the signatures
// asynchronously and in parallel.
func genRemoteHtlcSigJobs(keyRing *CommitmentKeyRing,
	chanState *channeldb.OpenChannel, leaseExpiry uint32,
	remoteCommitView *commitment,
	leafStore fn.Option[AuxLeafStore]) ([]SignJob, []AuxSigJob,
	chan struct{}, error) {

	var (
		isRemoteInitiator = !chanState.IsInitiator
		localChanCfg      = chanState.LocalChanCfg
		remoteChanCfg     = chanState.RemoteChanCfg
		chanType          = chanState.ChanType
	)

	txHash := remoteCommitView.txn.TxHash()
	dustLimit := remoteChanCfg.DustLimit
	feePerKw := remoteCommitView.feePerKw
	sigHashType := HtlcSigHashType(chanType)

	// With the keys generated, we'll make a slice with enough capacity to
	// hold potentially all the HTLCs. The actual slice may be a bit
	// smaller (than its total capacity) and some HTLCs may be dust.
	numSigs := len(remoteCommitView.incomingHTLCs) +
		len(remoteCommitView.outgoingHTLCs)
	sigBatch := make([]SignJob, 0, numSigs)
	auxSigBatch := make([]AuxSigJob, 0, numSigs)

	var err error
	cancelChan := make(chan struct{})

	diskCommit := remoteCommitView.toDiskCommit(lntypes.Remote)
	auxResult, err := fn.MapOptionZ(
		leafStore, func(s AuxLeafStore) fn.Result[CommitDiffAuxResult] {
			return s.FetchLeavesFromCommit(
				NewAuxChanState(chanState), *diskCommit,
				*keyRing, lntypes.Remote,
			)
		},
	).Unpack()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unable to fetch aux leaves: "+
			"%w", err)
	}

	// For each outgoing and incoming HTLC, if the HTLC isn't considered a
	// dust output after taking into account second-level HTLC fees, then a
	// sigJob will be generated and appended to the current batch.
	for _, htlc := range remoteCommitView.incomingHTLCs {
		if HtlcIsDust(
			chanType, true, lntypes.Remote, feePerKw,
			htlc.Amount.ToSatoshis(), dustLimit,
		) {

			continue
		}

		// If the HTLC isn't dust, then we'll create an empty sign job
		// to add to the batch momentarily.
		var sigJob SignJob
		sigJob.Cancel = cancelChan
		sigJob.Resp = make(chan SignJobResp, 1)

		// As this is an incoming HTLC and we're sinning the commitment
		// transaction of the remote node, we'll need to generate an
		// HTLC timeout transaction for them. The output of the timeout
		// transaction needs to account for fees, so we'll compute the
		// required fee and output now.
		htlcFee := HtlcTimeoutFee(chanType, feePerKw)
		outputAmt := htlc.Amount.ToSatoshis() - htlcFee

		auxLeaf := fn.FlatMapOption(
			func(l CommitAuxLeaves) input.AuxTapLeaf {
				leaves := l.IncomingHtlcLeaves
				return leaves[htlc.HtlcIndex].SecondLevelLeaf
			},
		)(auxResult.AuxLeaves)

		// With the fee calculate, we can properly create the HTLC
		// timeout transaction using the HTLC amount minus the fee.
		op := wire.OutPoint{
			Hash:  txHash,
			Index: uint32(htlc.remoteOutputIndex),
		}
		sigJob.Tx, err = CreateHtlcTimeoutTx(
			chanType, isRemoteInitiator, op, outputAmt,
			htlc.Timeout, uint32(remoteChanCfg.CsvDelay),
			leaseExpiry, keyRing.RevocationKey, keyRing.ToLocalKey,
			auxLeaf,
		)
		if err != nil {
			return nil, nil, nil, err
		}

		// Construct a full hash cache as we may be signing a segwit v1
		// sighash.
		txOut := remoteCommitView.txn.TxOut[htlc.remoteOutputIndex]
		prevFetcher := txscript.NewCannedPrevOutputFetcher(
			txOut.PkScript, int64(htlc.Amount.ToSatoshis()),
		)
		hashCache := txscript.NewTxSigHashes(sigJob.Tx, prevFetcher)

		// Finally, we'll generate a sign descriptor to generate a
		// signature to give to the remote party for this commitment
		// transaction. Note we use the raw HTLC amount.
		sigJob.SignDesc = input.SignDescriptor{
			KeyDesc:           localChanCfg.HtlcBasePoint,
			SingleTweak:       keyRing.LocalHtlcKeyTweak,
			WitnessScript:     htlc.theirWitnessScript,
			Output:            txOut,
			PrevOutputFetcher: prevFetcher,
			HashType:          sigHashType,
			SigHashes:         hashCache,
			InputIndex:        0,
		}
		sigJob.OutputIndex = htlc.remoteOutputIndex

		// If this is a taproot channel, then we'll need to set the
		// method type to ensure we generate a valid signature.
		if chanType.IsTaproot() {
			//nolint:ll
			sigJob.SignDesc.SignMethod = input.TaprootScriptSpendSignMethod
		}

		sigBatch = append(sigBatch, sigJob)

		auxSigBatch = append(auxSigBatch, NewAuxSigJob(
			sigJob, *keyRing, true, newAuxHtlcDescriptor(&htlc),
			remoteCommitView.customBlob, auxLeaf, cancelChan,
		))
	}
	for _, htlc := range remoteCommitView.outgoingHTLCs {
		if HtlcIsDust(
			chanType, false, lntypes.Remote, feePerKw,
			htlc.Amount.ToSatoshis(), dustLimit,
		) {

			continue
		}

		sigJob := SignJob{}
		sigJob.Cancel = cancelChan
		sigJob.Resp = make(chan SignJobResp, 1)

		// As this is an outgoing HTLC and we're signing the commitment
		// transaction of the remote node, we'll need to generate an
		// HTLC success transaction for them. The output of the timeout
		// transaction needs to account for fees, so we'll compute the
		// required fee and output now.
		htlcFee := HtlcSuccessFee(chanType, feePerKw)
		outputAmt := htlc.Amount.ToSatoshis() - htlcFee

		auxLeaf := fn.FlatMapOption(
			func(l CommitAuxLeaves) input.AuxTapLeaf {
				leaves := l.OutgoingHtlcLeaves
				return leaves[htlc.HtlcIndex].SecondLevelLeaf
			},
		)(auxResult.AuxLeaves)

		// With the proper output amount calculated, we can now
		// generate the success transaction using the remote party's
		// CSV delay.
		op := wire.OutPoint{
			Hash:  txHash,
			Index: uint32(htlc.remoteOutputIndex),
		}
		sigJob.Tx, err = CreateHtlcSuccessTx(
			chanType, isRemoteInitiator, op, outputAmt,
			uint32(remoteChanCfg.CsvDelay), leaseExpiry,
			keyRing.RevocationKey, keyRing.ToLocalKey,
			auxLeaf,
		)
		if err != nil {
			return nil, nil, nil, err
		}

		// Construct a full hash cache as we may be signing a segwit v1
		// sighash.
		txOut := remoteCommitView.txn.TxOut[htlc.remoteOutputIndex]
		prevFetcher := txscript.NewCannedPrevOutputFetcher(
			txOut.PkScript, int64(htlc.Amount.ToSatoshis()),
		)
		hashCache := txscript.NewTxSigHashes(sigJob.Tx, prevFetcher)

		// Finally, we'll generate a sign descriptor to generate a
		// signature to give to the remote party for this commitment
		// transaction. Note we use the raw HTLC amount.
		sigJob.SignDesc = input.SignDescriptor{
			KeyDesc:           localChanCfg.HtlcBasePoint,
			SingleTweak:       keyRing.LocalHtlcKeyTweak,
			WitnessScript:     htlc.theirWitnessScript,
			Output:            txOut,
			PrevOutputFetcher: prevFetcher,
			HashType:          sigHashType,
			SigHashes:         hashCache,
			InputIndex:        0,
		}
		sigJob.OutputIndex = htlc.remoteOutputIndex

		// If this is a taproot channel, then we'll need to set the
		// method type to ensure we generate a valid signature.
		if chanType.IsTaproot() {
			//nolint:ll
			sigJob.SignDesc.SignMethod = input.TaprootScriptSpendSignMethod
		}

		sigBatch = append(sigBatch, sigJob)

		auxSigBatch = append(auxSigBatch, NewAuxSigJob(
			sigJob, *keyRing, false, newAuxHtlcDescriptor(&htlc),
			remoteCommitView.customBlob, auxLeaf, cancelChan,
		))
	}

	return sigBatch, auxSigBatch, cancelChan, nil
}

// createCommitDiff will create a commit diff given a new pending commitment
// for the remote party and the necessary signatures for the remote party to
// validate this new state. This function is called right before sending the
// new commitment to the remote party. The commit diff returned contains all
// information necessary for retransmission.
func (lc *LightningChannel) createCommitDiff(newCommit *commitment,
	commitSig lnwire.Sig, htlcSigs []lnwire.Sig,
	auxSigs []fn.Option[tlv.Blob]) (*channeldb.CommitDiff, error) {

	var (
		logUpdates        []channeldb.LogUpdate
		ackAddRefs        []channeldb.AddRef
		settleFailRefs    []channeldb.SettleFailRef
		openCircuitKeys   []models.CircuitKey
		closedCircuitKeys []models.CircuitKey
	)

	// We'll now run through our local update log to locate the items which
	// were only just committed within this pending state. This will be the
	// set of items we need to retransmit if we reconnect and find that
	// they didn't process this new state fully.
	for e := lc.updateLogs.Local.Front(); e != nil; e = e.Next() {
		pd := e.Value

		// If this entry wasn't committed at the exact height of this
		// remote commitment, then we'll skip it as it was already
		// lingering in the log.
		if pd.addCommitHeights.Remote != newCommit.height &&
			pd.removeCommitHeights.Remote != newCommit.height {

			continue
		}

		// We'll map the type of the paymentDescriptor to one of the
		// four messages that it corresponds to. With this set of
		// messages obtained, we can simply read from disk and re-send
		// them in the case of a needed channel sync.
		switch pd.EntryType {
		case Add:
			// Gather any references for circuits opened by this Add
			// HTLC.
			if pd.OpenCircuitKey != nil {
				openCircuitKeys = append(
					openCircuitKeys, *pd.OpenCircuitKey,
				)
			}

		case Settle, Fail, MalformedFail:
			// Gather the fwd pkg references from any settle or fail
			// packets, if they exist.
			if pd.SourceRef != nil {
				ackAddRefs = append(ackAddRefs, *pd.SourceRef)
			}
			if pd.DestRef != nil {
				settleFailRefs = append(
					settleFailRefs, *pd.DestRef,
				)
			}
			if pd.ClosedCircuitKey != nil {
				closedCircuitKeys = append(
					closedCircuitKeys, *pd.ClosedCircuitKey,
				)
			}

		case FeeUpdate:
			// Nothing special to do.
		}

		logUpdates = append(logUpdates, pd.toLogUpdate())
	}

	// With the set of log updates mapped into wire messages, we'll now
	// convert the in-memory commit into a format suitable for writing to
	// disk.
	diskCommit := newCommit.toDiskCommit(lntypes.Remote)

	// We prepare the commit sig message to be sent to the remote party.
	commitSigMsg := &lnwire.CommitSig{
		ChanID: lnwire.NewChanIDFromOutPoint(
			lc.channelState.FundingOutpoint,
		),
		CommitSig: commitSig,
		HtlcSigs:  htlcSigs,
	}

	// Encode and check the size of the custom records now.
	auxCustomRecords, err := fn.MapOptionZ(
		lc.auxSigner,
		func(s AuxSigner) fn.Result[lnwire.CustomRecords] {
			blobOption, err := s.PackSigs(auxSigs).Unpack()
			if err != nil {
				return fn.Err[lnwire.CustomRecords](err)
			}

			// We now serialize the commit sig message without the
			// custom records to make sure we have space for them.
			var buf bytes.Buffer
			err = commitSigMsg.Encode(&buf, 0)
			if err != nil {
				return fn.Err[lnwire.CustomRecords](err)
			}

			// The number of available bytes is the max message size
			// minus the size of the message without the custom
			// records. We also subtract 8 bytes for encoding
			// overhead of the custom records (just some safety
			// padding).
			available := lnwire.MaxMsgBody - buf.Len() - 8

			blob := blobOption.UnwrapOr(nil)
			if len(blob) > available {
				err = fmt.Errorf("aux sigs size %d exceeds "+
					"max allowed size of %d", len(blob),
					available)

				return fn.Err[lnwire.CustomRecords](err)
			}

			records, err := lnwire.ParseCustomRecords(blob)
			if err != nil {
				return fn.Err[lnwire.CustomRecords](err)
			}

			return fn.Ok(records)
		},
	).Unpack()
	if err != nil {
		return nil, fmt.Errorf("error packing aux sigs: %w", err)
	}

	commitSigMsg.CustomRecords = auxCustomRecords

	return &channeldb.CommitDiff{
		Commitment:        *diskCommit,
		CommitSig:         commitSigMsg,
		LogUpdates:        logUpdates,
		OpenedCircuitKeys: openCircuitKeys,
		ClosedCircuitKeys: closedCircuitKeys,
		AddAcks:           ackAddRefs,
		SettleFailAcks:    settleFailRefs,
	}, nil
}

// getUnsignedAckedUpdates returns all remote log updates that we haven't
// signed for yet ourselves.
func (lc *LightningChannel) getUnsignedAckedUpdates() []channeldb.LogUpdate {
	// Fetch the last remote update that we have signed for.
	lastRemoteCommitted :=
		lc.commitChains.Remote.tail().messageIndices.Remote

	// Fetch the last remote update that we have acked.
	lastLocalCommitted :=
		lc.commitChains.Local.tail().messageIndices.Remote

	// We'll now run through the remote update log to locate the items that
	// we haven't signed for yet. This will be the set of items we need to
	// restore if we reconnect in order to produce the signature that the
	// remote party expects.
	var logUpdates []channeldb.LogUpdate
	for e := lc.updateLogs.Remote.Front(); e != nil; e = e.Next() {
		pd := e.Value

		// Skip all remote updates that we have already included in our
		// commit chain.
		if pd.LogIndex < lastRemoteCommitted {
			continue
		}

		// Skip all remote updates that we haven't acked yet. At the
		// moment this function is called, there shouldn't be any, but
		// we check it anyway to make this function more generally
		// usable.
		if pd.LogIndex >= lastLocalCommitted {
			continue
		}

		logUpdates = append(logUpdates, pd.toLogUpdate())
	}

	return logUpdates
}

// CalcFeeBuffer calculates a FeeBuffer in accordance with the recommended
// amount specified in BOLT 02. It accounts for two times the current fee rate
// plus an additional htlc at this higher fee rate which allows our peer to add
// an htlc even if our channel is drained locally.
// See: https://github.com/lightning/bolts/blob/master/02-peer-protocol.md
func CalcFeeBuffer(feePerKw chainfee.SatPerKWeight,
	commitWeight lntypes.WeightUnit) lnwire.MilliSatoshi {

	// Account for a 100% in fee rate increase.
	bufferFeePerKw := 2 * feePerKw

	feeBuffer := lnwire.NewMSatFromSatoshis(
		// Account for an additional htlc at the higher fee level.
		bufferFeePerKw.FeeForWeight(commitWeight + input.HTLCWeight),
	)

	return feeBuffer
}

// BufferType is used to determine what kind of additional buffer should be left
// when evaluating the usable balance of a channel.
type BufferType uint8

const (
	// NoBuffer means no additional buffer is accounted for. This is
	// important when verifying an already locked-in commitment state.
	NoBuffer BufferType = iota

	// FeeBuffer accounts for several edge cases. One of them is where
	// a locally drained channel might become unusable due to the non-opener
	// of the channel not being able to add a non-dust htlc to the channel
	// state because we as a channel opener cannot pay the additional fees
	// an htlc would require on the commitment tx.
	// See: https://github.com/lightningnetwork/lightning-rfc/issues/728
	//
	// Moreover it mitigates the situation where htlcs are added
	// simultaneously to the commitment transaction. This cannot be avoided
	// until the feature __option_simplified_update__ is available in the
	// protocol and deployed widely in the network.
	// More information about the issue and the simplified commitment flow
	// can be found here:
	// https://github.com/lightningnetwork/lnd/issues/7657
	// https://github.com/lightning/bolts/pull/867
	//
	// The last advantage is that we can react to fee spikes (up or down)
	// by accounting for at least twice the size of the current fee rate
	// (BOLT02). It also accounts for decreases in the fee rate because
	// former dust htlcs might now become normal outputs so the overall
	// fee might increase although the fee rate decreases (this is only true
	// for non-anchor channels because htlcs have to account for their
	// fee of the second-level covenant transactions).
	FeeBuffer

	// AdditionalHtlc just accounts for an additional htlc which is helpful
	// when deciding about a fee update of the commitment transaction.
	// Leaving always room for an additional htlc makes sure that even
	// though we are the opener of a channel a new fee update will always
	// allow an htlc from our peer to be added to the commitment tx.
	AdditionalHtlc
)

// String returns a human readable name for the buffer type.
func (b BufferType) String() string {
	switch b {
	case NoBuffer:
		return "nobuffer"
	case FeeBuffer:
		return "feebuffer"
	case AdditionalHtlc:
		return "additionalhtlc"
	default:
		return "unknown"
	}
}

// applyCommitFee applies the commitFee including a buffer to the balance amount
// and verifies that it does not become negative. This function returns the new
// balance, the exact buffer amount (excluding the commitment fee) and the
// commitment fee.
func (lc *LightningChannel) applyCommitFee(
	balance lnwire.MilliSatoshi, commitWeight lntypes.WeightUnit,
	feePerKw chainfee.SatPerKWeight,
	buffer BufferType) (lnwire.MilliSatoshi, lnwire.MilliSatoshi,
	lnwire.MilliSatoshi, error) {

	commitFee := feePerKw.FeeForWeight(commitWeight)
	commitFeeMsat := lnwire.NewMSatFromSatoshis(commitFee)

	var bufferAmt lnwire.MilliSatoshi
	switch buffer {
	// The FeeBuffer is subtracted from the balance. It is of predefined
	// size add keeps room for an up to 2x increase in fees of the
	// commitment tx and an additional htlc at this fee level reserved for
	// the peer.
	case FeeBuffer:
		// Make sure that we are the initiator of the channel before we
		// apply the FeeBuffer.
		if !lc.channelState.IsInitiator {
			return 0, 0, 0, ErrFeeBufferNotInitiator
		}

		// The FeeBuffer already includes the commitFee.
		bufferAmt = CalcFeeBuffer(feePerKw, commitWeight)
		if bufferAmt < balance {
			newBalance := balance - bufferAmt

			return newBalance, bufferAmt - commitFeeMsat,
				commitFeeMsat, nil
		}

	// The AdditionalHtlc buffer type does NOT keep a FeeBuffer but solely
	// keeps space for an additional htlc on the commitment tx which our
	// peer can add.
	case AdditionalHtlc:
		additionalHtlcFee := lnwire.NewMSatFromSatoshis(
			feePerKw.FeeForWeight(input.HTLCWeight),
		)

		bufferAmt = commitFeeMsat + additionalHtlcFee
		newBalance := balance - bufferAmt
		if bufferAmt < balance {
			return newBalance, additionalHtlcFee, commitFeeMsat, nil
		}

	// The default case does not account for any buffer on the local balance
	// but just subtracts the commit fee.
	default:
		if commitFeeMsat < balance {
			newBalance := balance - commitFeeMsat
			return newBalance, 0, commitFeeMsat, nil
		}
	}

	// We still return the amount and bufferAmt here to log them at a later
	// stage.
	return balance, bufferAmt, commitFeeMsat, ErrBelowChanReserve
}

// validateCommitmentSanity is used to validate the current state of the
// commitment transaction in terms of the ChannelConstraints that we and our
// remote peer agreed upon during the funding workflow. The
// predict[Our|Their]Add should parameters should be set to a valid
// paymentDescriptor if we are validating in the state when adding a new HTLC,
// or nil otherwise.
func (lc *LightningChannel) validateCommitmentSanity(theirLogCounter,
	ourLogCounter uint64, whoseCommitChain lntypes.ChannelParty,
	buffer BufferType, predictOurAdd, predictTheirAdd *paymentDescriptor,
) error {

	// First fetch the initial balance before applying any updates.
	commitChain := lc.commitChains.Local
	if whoseCommitChain.IsRemote() {
		commitChain = lc.commitChains.Remote
	}
	ourInitialBalance := commitChain.tip().ourBalance
	theirInitialBalance := commitChain.tip().theirBalance

	// Fetch all updates not committed.
	view := lc.fetchHTLCView(theirLogCounter, ourLogCounter)

	// If we are checking if we can add a new HTLC, we add this to the
	// appropriate update log, in order to validate the sanity of the
	// commitment resulting from _actually adding_ this HTLC to the state.
	if predictOurAdd != nil {
		view.Updates.Local = append(view.Updates.Local, predictOurAdd)
	}
	if predictTheirAdd != nil {
		view.Updates.Remote = append(
			view.Updates.Remote, predictTheirAdd,
		)
	}

	ourBalance, theirBalance, commitWeight, filteredView, err := lc.computeView(
		view, whoseCommitChain, false,
		fn.None[chainfee.SatPerKWeight](),
	)
	if err != nil {
		return err
	}

	feePerKw := filteredView.FeePerKw

	// Ensure that the fee being applied is enough to be relayed across the
	// network in a reasonable time frame.
	if feePerKw < chainfee.FeePerKwFloor {
		return fmt.Errorf("commitment fee per kw %v below fee floor %v",
			feePerKw, chainfee.FeePerKwFloor)
	}

	// The channel opener has to account for the commitment fee. This
	// includes also a buffer type. Depending on whether we are the opener
	// of the channel we either want to enforce a buffer on the local
	// amount.
	var (
		bufferAmt lnwire.MilliSatoshi
		commitFee lnwire.MilliSatoshi
	)

	if lc.channelState.IsInitiator {
		ourBalance, bufferAmt, commitFee, err = lc.applyCommitFee(
			ourBalance, commitWeight, feePerKw, buffer,
		)
		if err != nil {
			lc.log.Errorf("Cannot pay for the CommitmentFee of "+
				"the ChannelState: ourBalance is negative "+
				"after applying the fee: ourBalance=%v, "+
				"commitFee=%v, feeBuffer=%v (type=%v) "+
				"local_chan_initiator", ourBalance,
				commitFee, bufferAmt, buffer)

			return err
		}
	} else {
		// No FeeBuffer is enforced when we are not the initiator of
		// the channel. We cannot do this, because if our peer does not
		// enforce the FeeBuffer (older LND software) the peer might
		// bring his balance below the FeeBuffer making the channel
		// stuck because locally we will never put another outgoing HTLC
		// on the channel state. The FeeBuffer should ONLY be enforced
		// if we locally pay for the commitment transaction.
		theirBalance, bufferAmt, commitFee, err = lc.applyCommitFee(
			theirBalance, commitWeight, feePerKw, NoBuffer,
		)
		if err != nil {
			lc.log.Errorf("Cannot pay for the CommitmentFee "+
				"of the ChannelState: theirBalance is "+
				"negative after applying the fee: "+
				"theirBalance=%v, commitFee=%v, feeBuffer=%v "+
				"(type=%v) remote_chan_initiator",
				theirBalance, commitFee, bufferAmt, buffer)

			return err
		}
	}

	// The commitment fee was accounted for successfully now make sure we
	// still do have enough left to account for the channel reserve.
	// If the added HTLCs will decrease the balance, make sure they won't
	// dip the local and remote balances below the channel reserves.
	ourReserve := lnwire.NewMSatFromSatoshis(
		lc.channelState.LocalChanCfg.ChanReserve,
	)
	theirReserve := lnwire.NewMSatFromSatoshis(
		lc.channelState.RemoteChanCfg.ChanReserve,
	)

	switch {
	// TODO(ziggie): Allow the peer dip us below the channel reserve when
	// our local balance would increase during this commitment dance or
	// allow us to dip the peer below its reserve then their balance would
	// increase during this commitment dance. This is needed for splicing
	// when e.g. a new channel (bigger capacity) has a higher required
	// reserve and the peer would need to add an additional htlc to push the
	// missing amount to our side and viceversa.
	// See: https://github.com/lightningnetwork/lnd/issues/8249
	case ourBalance < ourInitialBalance && ourBalance < ourReserve:
		lc.log.Debugf("Funds below chan reserve: ourBalance=%v, "+
			"ourReserve=%v, commitFee=%v, feeBuffer=%v "+
			"chan_initiator=%v", ourBalance, ourReserve,
			commitFee, bufferAmt, lc.channelState.IsInitiator)

		return fmt.Errorf("%w: our balance below chan reserve",
			ErrBelowChanReserve)

	case theirBalance < theirInitialBalance && theirBalance < theirReserve:
		lc.log.Debugf("Funds below chan reserve: theirBalance=%v, "+
			"theirReserve=%v", theirBalance, theirReserve)

		return fmt.Errorf("%w: their balance below chan reserve",
			ErrBelowChanReserve)
	}

	// validateUpdates take a set of updates, and validates them against
	// the passed channel constraints.
	validateUpdates := func(updates []*paymentDescriptor,
		constraints *channeldb.ChannelConfig) error {

		// We keep track of the number of HTLCs in flight for the
		// commitment, and the amount in flight.
		var numInFlight uint16
		var amtInFlight lnwire.MilliSatoshi

		// Go through all updates, checking that they don't violate the
		// channel constraints.
		for _, entry := range updates {
			if entry.isAdd() {
				// An HTLC is being added, this will add to the
				// number and amount in flight.
				amtInFlight += entry.Amount
				numInFlight++

				// Check that the HTLC amount is positive.
				if entry.Amount == 0 {
					return ErrInvalidHTLCAmt
				}

				// Check that the value of the HTLC they added
				// is above our minimum.
				if entry.Amount < constraints.MinHTLC {
					return ErrBelowMinHTLC
				}
			}
		}

		// Now that we know the total value of added HTLCs, we check
		// that this satisfy the MaxPendingAmont constraint.
		if amtInFlight > constraints.MaxPendingAmount {
			return ErrMaxPendingAmount
		}

		// In this step, we verify that the total number of active
		// HTLCs does not exceed the constraint of the maximum number
		// of HTLCs in flight.
		if numInFlight > constraints.MaxAcceptedHtlcs {
			return ErrMaxHTLCNumber
		}

		return nil
	}

	// First check that the remote updates won't violate it's channel
	// constraints.
	err = validateUpdates(
		filteredView.Updates.Remote, &lc.channelState.RemoteChanCfg,
	)
	if err != nil {
		return err
	}

	// Secondly check that our updates won't violate our channel
	// constraints.
	err = validateUpdates(
		filteredView.Updates.Local, &lc.channelState.LocalChanCfg,
	)
	if err != nil {
		return err
	}

	return nil
}

// CommitSigs holds the set of related signatures for a new commitment
// transaction state.
type CommitSigs struct {
	// CommitSig is the normal commitment signature. This will only be a
	// non-zero commitment signature for non-taproot channels.
	CommitSig lnwire.Sig

	// HtlcSigs is the set of signatures for all HTLCs in the commitment
	// transaction. Depending on the channel type, these will either be
	// ECDSA or Schnorr signatures.
	HtlcSigs []lnwire.Sig

	// PartialSig is the musig2 partial signature for taproot commitment
	// transactions.
	PartialSig lnwire.OptPartialSigWithNonceTLV

	// AuxSigBlob is the blob containing all the auxiliary signatures for
	// this new commitment state.
	AuxSigBlob tlv.Blob
}

// NewCommitState wraps the various signatures needed to properly
// propose/accept a new commitment state. This includes the signer's nonce for
// musig2 channels.
type NewCommitState struct {
	*CommitSigs

	// PendingHTLCs is the set of new/pending HTLCs produced by this
	// commitment state.
	PendingHTLCs []channeldb.HTLC
}

// SignNextCommitment signs a new commitment which includes any previous
// unsettled HTLCs, any new HTLCs, and any modifications to prior HTLCs
// committed in previous commitment updates. Signing a new commitment
// decrements the available revocation window by 1. After a successful method
// call, the remote party's commitment chain is extended by a new commitment
// which includes all updates to the HTLC log prior to this method invocation.
// The first return parameter is the signature for the commitment transaction
// itself, while the second parameter is a slice of all HTLC signatures (if
// any). The HTLC signatures are sorted according to the BIP 69 order of the
// HTLC's on the commitment transaction. Finally, the new set of pending HTLCs
// for the remote party's commitment are also returned.
//
//nolint:funlen
func (lc *LightningChannel) SignNextCommitment(
	ctx context.Context) (*NewCommitState, error) {

	lc.Lock()
	defer lc.Unlock()

	// Check for empty commit sig. This should never happen, but we don't
	// dare to fail hard here. We assume peers can deal with the empty sig
	// and continue channel operation. We log an error so that the bug
	// causing this can be tracked down.
	if !lc.oweCommitment(lntypes.Local) {
		lc.log.Errorf("sending empty commit sig")
	}

	var (
		sig        lnwire.Sig
		partialSig *lnwire.PartialSigWithNonce
		htlcSigs   []lnwire.Sig
	)

	// If we're awaiting for an ACK to a commitment signature, or if we
	// don't yet have the initial next revocation point of the remote
	// party, then we're unable to create new states. Each time we create a
	// new state, we consume a prior revocation point.
	commitPoint := lc.channelState.RemoteNextRevocation
	unacked := lc.commitChains.Remote.hasUnackedCommitment()
	if unacked || commitPoint == nil {
		lc.log.Tracef("waiting for remote ack=%v, nil "+
			"RemoteNextRevocation: %v", unacked, commitPoint == nil)
		return nil, ErrNoWindow
	}

	// Determine the last update on the remote log that has been locked in.
	remoteACKedIndex := lc.commitChains.Local.tail().messageIndices.Remote
	remoteHtlcIndex := lc.commitChains.Local.tail().theirHtlcIndex

	// Before we extend this new commitment to the remote commitment chain,
	// ensure that we aren't violating any of the constraints the remote
	// party set up when we initially set up the channel. If we are, then
	// we'll abort this state transition.
	// We do not enforce the FeeBuffer here because when we reach this
	// point all updates will have to get locked-in so we enforce the
	// minimum requirement.
	err := lc.validateCommitmentSanity(
		remoteACKedIndex, lc.updateLogs.Local.logIndex, lntypes.Remote,
		NoBuffer, nil, nil,
	)
	if err != nil {
		return nil, err
	}

	// Grab the next commitment point for the remote party. This will be
	// used within fetchCommitmentView to derive all the keys necessary to
	// construct the commitment state.
	keyRing := DeriveCommitmentKeys(
		commitPoint, lntypes.Remote, lc.channelState.ChanType,
		&lc.channelState.LocalChanCfg, &lc.channelState.RemoteChanCfg,
	)

	// Create a new commitment view which will calculate the evaluated
	// state of the remote node's new commitment including our latest added
	// HTLCs. The view includes the latest balances for both sides on the
	// remote node's chain, and also update the addition height of any new
	// HTLC log entries. When we creating a new remote view, we include
	// _all_ of our changes (pending or committed) but only the remote
	// node's changes up to the last change we've ACK'd.
	newCommitView, err := lc.fetchCommitmentView(
		lntypes.Remote, lc.updateLogs.Local.logIndex,
		lc.updateLogs.Local.htlcCounter, remoteACKedIndex,
		remoteHtlcIndex, keyRing,
	)
	if err != nil {
		return nil, err
	}

	lc.log.Tracef("extending remote chain to height %v, "+
		"local_log=%v, remote_log=%v",
		newCommitView.height,
		lc.updateLogs.Local.logIndex, remoteACKedIndex)

	lc.log.Tracef("remote chain: our_balance=%v, "+
		"their_balance=%v, commit_tx: %v",
		newCommitView.ourBalance,
		newCommitView.theirBalance,
		lnutils.SpewLogClosure(newCommitView.txn))

	// With the commitment view constructed, if there are any HTLC's, we'll
	// need to generate signatures of each of them for the remote party's
	// commitment state. We do so in two phases: first we generate and
	// submit the set of signature jobs to the worker pool.
	var leaseExpiry uint32
	if lc.channelState.ChanType.HasLeaseExpiration() {
		leaseExpiry = lc.channelState.ThawHeight
	}
	sigBatch, auxSigBatch, cancelChan, err := genRemoteHtlcSigJobs(
		keyRing, lc.channelState, leaseExpiry, newCommitView,
		lc.leafStore,
	)
	if err != nil {
		return nil, err
	}

	// We'll need to send over the signatures to the remote party in the
	// order as they appear on the commitment transaction after BIP 69
	// sorting.
	slices.SortFunc(sigBatch, func(i, j SignJob) int {
		return cmp.Compare(i.OutputIndex, j.OutputIndex)
	})
	slices.SortFunc(auxSigBatch, func(i, j AuxSigJob) int {
		return cmp.Compare(i.OutputIndex, j.OutputIndex)
	})

	lc.sigPool.SubmitSignBatch(sigBatch)

	err = fn.MapOptionZ(lc.auxSigner, func(a AuxSigner) error {
		return a.SubmitSecondLevelSigBatch(
			NewAuxChanState(lc.channelState), newCommitView.txn,
			auxSigBatch,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("error submitting second level sig "+
			"batch: %w", err)
	}

	// While the jobs are being carried out, we'll Sign their version of
	// the new commitment transaction while we're waiting for the rest of
	// the HTLC signatures to be processed.
	//
	// TODO(roasbeef): abstract into CommitSigner interface?
	if lc.channelState.ChanType.IsTaproot() {
		// In this case, we'll send out a partial signature as this is
		// a musig2 channel. The encoded normal ECDSA signature will be
		// just blank.
		remoteSession := lc.musigSessions.RemoteSession
		musig, err := remoteSession.SignCommit(
			newCommitView.txn,
		)
		if err != nil {
			close(cancelChan)
			return nil, err
		}

		partialSig = musig.ToWireSig()
	} else {
		lc.signDesc.SigHashes = input.NewTxSigHashesV0Only(
			newCommitView.txn,
		)
		rawSig, err := lc.Signer.SignOutputRaw(
			newCommitView.txn, lc.signDesc,
		)
		if err != nil {
			close(cancelChan)
			return nil, err
		}
		sig, err = lnwire.NewSigFromSignature(rawSig)
		if err != nil {
			close(cancelChan)
			return nil, err
		}
	}

	// Iterate through all the responses to gather each of the signatures
	// in the order they were submitted.
	htlcSigs = make([]lnwire.Sig, 0, len(sigBatch))
	auxSigs := make([]fn.Option[tlv.Blob], 0, len(auxSigBatch))
	for i := range sigBatch {
		htlcSigJob := sigBatch[i]
		var jobResp SignJobResp

		select {
		case jobResp = <-htlcSigJob.Resp:
		case <-ctx.Done():
			return nil, errQuit
		}

		// If an error occurred, then we'll cancel any other active
		// jobs.
		if jobResp.Err != nil {
			close(cancelChan)
			return nil, jobResp.Err
		}

		htlcSigs = append(htlcSigs, jobResp.Sig)

		if lc.auxSigner.IsNone() {
			continue
		}

		auxHtlcSigJob := auxSigBatch[i]
		var auxJobResp AuxSigJobResp

		select {
		case auxJobResp = <-auxHtlcSigJob.Resp:
		case <-ctx.Done():
			return nil, errQuit
		}

		// If an error occurred, then we'll cancel any other active
		// jobs.
		if auxJobResp.Err != nil {
			close(cancelChan)
			return nil, auxJobResp.Err
		}

		auxSigs = append(auxSigs, auxJobResp.SigBlob)
	}

	// As we're about to proposer a new commitment state for the remote
	// party, we'll write this pending state to disk before we exit, so we
	// can retransmit it if necessary.
	commitDiff, err := lc.createCommitDiff(
		newCommitView, sig, htlcSigs, auxSigs,
	)
	if err != nil {
		return nil, err
	}

	err = lc.channelState.AppendRemoteCommitChain(commitDiff)
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): check that one eclair bug
	//  * need to retransmit on first state still?
	//  * after initial reconnect

	// Extend the remote commitment chain by one with the addition of our
	// latest commitment update.
	lc.commitChains.Remote.addCommitment(newCommitView)

	auxSigBlob, err := commitDiff.CommitSig.CustomRecords.Serialize()
	if err != nil {
		return nil, fmt.Errorf("unable to serialize aux sig blob: %w",
			err)
	}

	return &NewCommitState{
		CommitSigs: &CommitSigs{
			CommitSig:  sig,
			HtlcSigs:   htlcSigs,
			PartialSig: lnwire.MaybePartialSigWithNonce(partialSig),
			AuxSigBlob: auxSigBlob,
		},
		PendingHTLCs: commitDiff.Commitment.Htlcs,
	}, nil
}

// resignMusigCommit is used to resign a commitment transaction for taproot
// channels when we need to retransmit a signature after a channel reestablish
// message. Taproot channels use musig2, which means we must use fresh nonces
// each time. After we receive the channel reestablish message, we learn the
// nonce we need to use for the remote party. As a result, we need to generate
// the partial signature again with the new nonce.
func (lc *LightningChannel) resignMusigCommit(
	commitTx *wire.MsgTx) (lnwire.OptPartialSigWithNonceTLV, error) {

	remoteSession := lc.musigSessions.RemoteSession
	musig, err := remoteSession.SignCommit(commitTx)
	if err != nil {
		var none lnwire.OptPartialSigWithNonceTLV
		return none, err
	}

	partialSig := lnwire.MaybePartialSigWithNonce(musig.ToWireSig())

	return partialSig, nil
}

// ProcessChanSyncMsg processes a ChannelReestablish message sent by the remote
// connection upon re establishment of our connection with them. This method
// will return a single message if we are currently out of sync, otherwise a
// nil lnwire.Message will be returned. If it is decided that our level of
// de-synchronization is irreconcilable, then an error indicating the issue
// will be returned. In this case that an error is returned, the channel should
// be force closed, as we cannot continue updates.
//
// One of two message sets will be returned:
//
//   - CommitSig+Updates: if we have a pending remote commit which they claim to
//     have not received
//   - RevokeAndAck: if we sent a revocation message that they claim to have
//     not received
//
// If we detect a scenario where we need to send a CommitSig+Updates, this
// method also returns two sets models.CircuitKeys identifying the circuits
// that were opened and closed, respectively, as a result of signing the
// previous commitment txn. This allows the link to clear its mailbox of those
// circuits in case they are still in memory, and ensure the switch's circuit
// map has been updated by deleting the closed circuits.
//
//nolint:funlen
func (lc *LightningChannel) ProcessChanSyncMsg(ctx context.Context,
	msg *lnwire.ChannelReestablish) ([]lnwire.Message, []models.CircuitKey,
	[]models.CircuitKey, error) {

	// Now we'll examine the state we have, vs what was contained in the
	// chain sync message. If we're de-synchronized, then we'll send a
	// batch of messages which when applied will kick start the chain
	// resync.
	var (
		updates        []lnwire.Message
		openedCircuits []models.CircuitKey
		closedCircuits []models.CircuitKey
	)

	// If the remote party included the optional fields, then we'll verify
	// their correctness first, as it will influence our decisions below.
	hasRecoveryOptions := msg.LocalUnrevokedCommitPoint != nil
	if hasRecoveryOptions && msg.RemoteCommitTailHeight != 0 {
		// We'll check that they've really sent a valid commit
		// secret from our shachain for our prior height, but only if
		// this isn't the first state.
		heightSecret, err := lc.channelState.RevocationProducer.AtIndex(
			msg.RemoteCommitTailHeight - 1,
		)
		if err != nil {
			return nil, nil, nil, err
		}
		commitSecretCorrect := bytes.Equal(
			heightSecret[:], msg.LastRemoteCommitSecret[:],
		)

		// If the commit secret they sent is incorrect then we'll fail
		// the channel as the remote node has an inconsistent state.
		if !commitSecretCorrect {
			// In this case, we'll return an error to indicate the
			// remote node sent us the wrong values. This will let
			// the caller act accordingly.
			lc.log.Errorf("sync failed: remote provided invalid " +
				"commit secret!")
			return nil, nil, nil, ErrInvalidLastCommitSecret
		}
	}

	// If this is a taproot channel, then we expect that the remote party
	// has sent the next verification nonce. If they haven't, then we'll
	// bail out, otherwise we'll init our local session then continue as
	// normal.
	switch {
	case lc.channelState.ChanType.IsTaproot() && msg.LocalNonce.IsNone():
		return nil, nil, nil, fmt.Errorf("remote verification nonce " +
			"not sent")

	case lc.channelState.ChanType.IsTaproot() && msg.LocalNonce.IsSome():
		if lc.opts.skipNonceInit {
			// Don't call InitRemoteMusigNonces if we have already
			// done so.
			break
		}

		nextNonce, err := msg.LocalNonce.UnwrapOrErrV(errNoNonce)
		if err != nil {
			return nil, nil, nil, err
		}

		err = lc.InitRemoteMusigNonces(&musig2.Nonces{
			PubNonce: nextNonce,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("unable to init "+
				"remote nonce: %w", err)
		}
	}

	// If we detect that this is is a restored channel, then we can skip a
	// portion of the verification, as we already know that we're unable to
	// proceed with any updates.
	isRestoredChan := lc.channelState.HasChanStatus(
		channeldb.ChanStatusRestored,
	)

	// Take note of our current commit chain heights before we begin adding
	// more to them.
	var (
		localTailHeight  = lc.commitChains.Local.tail().height
		remoteTailHeight = lc.commitChains.Remote.tail().height
		remoteTipHeight  = lc.commitChains.Remote.tip().height
	)

	// We'll now check that their view of our local chain is up-to-date.
	// This means checking that what their view of our local chain tail
	// height is what they believe. Note that the tail and tip height will
	// always be the same for the local chain at this stage, as we won't
	// store any received commitment to disk before it is ACKed.
	switch {

	// If their reported height for our local chain tail is ahead of our
	// view, then we're behind!
	case msg.RemoteCommitTailHeight > localTailHeight || isRestoredChan:
		lc.log.Errorf("sync failed with local data loss: remote "+
			"believes our tail height is %v, while we have %v!",
			msg.RemoteCommitTailHeight, localTailHeight)

		if isRestoredChan {
			lc.log.Warnf("detected restored triggering DLP")
		}

		// We must check that we had recovery options to ensure the
		// commitment secret matched up, and the remote is just not
		// lying about its height.
		if !hasRecoveryOptions {
			// At this point we the remote is either lying about
			// its height, or we are actually behind but the remote
			// doesn't support data loss protection. In either case
			// it is not safe for us to keep using the channel, so
			// we mark it borked and fail the channel.
			lc.log.Errorf("sync failed: local data loss, but no " +
				"recovery option.")

			return nil, nil, nil, ErrCannotSyncCommitChains
		}

		// In this case, we've likely lost data and shouldn't proceed
		// with channel updates.
		return nil, nil, nil, &ErrCommitSyncLocalDataLoss{
			ChannelPoint: lc.channelState.FundingOutpoint,
			CommitPoint:  msg.LocalUnrevokedCommitPoint,
		}

	// If the height of our commitment chain reported by the remote party
	// is behind our view of the chain, then they probably lost some state,
	// and we'll force close the channel.
	case msg.RemoteCommitTailHeight+1 < localTailHeight:
		lc.log.Errorf("sync failed: remote believes our tail height is "+
			"%v, while we have %v!",
			msg.RemoteCommitTailHeight, localTailHeight)
		return nil, nil, nil, ErrCommitSyncRemoteDataLoss

	// Their view of our commit chain is consistent with our view.
	case msg.RemoteCommitTailHeight == localTailHeight:
		// In sync, don't have to do anything.

	// We owe them a revocation if the tail of our current commitment chain
	// is one greater than what they _think_ our commitment tail is. In
	// this case we'll re-send the last revocation message that we sent.
	// This will be the revocation message for our prior chain tail.
	case msg.RemoteCommitTailHeight+1 == localTailHeight:
		lc.log.Debugf("sync: remote believes our tail height is %v, "+
			"while we have %v, we owe them a revocation",
			msg.RemoteCommitTailHeight, localTailHeight)

		heightToRetransmit := localTailHeight - 1
		revocationMsg, err := lc.generateRevocation(heightToRetransmit)
		if err != nil {
			return nil, nil, nil, err
		}

		updates = append(updates, revocationMsg)

		// Next, as a precaution, we'll check a special edge case. If
		// they initiated a state transition, we sent the revocation,
		// but died before the signature was sent. We re-transmit our
		// revocation, but also initiate a state transition to re-sync
		// them.
		if lc.OweCommitment() {
			newCommit, err := lc.SignNextCommitment(ctx)
			switch {

			// If we signed this state, then we'll accumulate
			// another update to send over.
			case err == nil:
				customRecords, err := lnwire.ParseCustomRecords(
					newCommit.AuxSigBlob,
				)
				if err != nil {
					sErr := fmt.Errorf("error parsing aux "+
						"sigs: %w", err)
					return nil, nil, nil, sErr
				}

				commitSig := &lnwire.CommitSig{
					ChanID: lnwire.NewChanIDFromOutPoint(
						lc.channelState.FundingOutpoint,
					),
					CommitSig:     newCommit.CommitSig,
					HtlcSigs:      newCommit.HtlcSigs,
					PartialSig:    newCommit.PartialSig,
					CustomRecords: customRecords,
				}

				updates = append(updates, commitSig)

			// If we get a failure due to not knowing their next
			// point, then this is fine as they'll either send
			// ChannelReady, or revoke their next state to allow
			// us to continue forwards.
			case err == ErrNoWindow:

			// Otherwise, this is an error and we'll treat it as
			// such.
			default:
				return nil, nil, nil, err
			}
		}

	// There should be no other possible states.
	default:
		lc.log.Errorf("sync failed: remote believes our tail height is "+
			"%v, while we have %v!",
			msg.RemoteCommitTailHeight, localTailHeight)
		return nil, nil, nil, ErrCannotSyncCommitChains
	}

	// Now check if our view of the remote chain is consistent with what
	// they tell us.
	switch {

	// The remote's view of what their next commit height is 2+ states
	// ahead of us, we most likely lost data, or the remote is trying to
	// trick us. Since we have no way of verifying whether they are lying
	// or not, we will fail the channel, but should not force close it
	// automatically.
	case msg.NextLocalCommitHeight > remoteTipHeight+1:
		lc.log.Errorf("sync failed: remote's next commit height is %v, "+
			"while we believe it is %v!",
			msg.NextLocalCommitHeight, remoteTipHeight+1)

		return nil, nil, nil, ErrCannotSyncCommitChains

	// They are waiting for a state they have already ACKed.
	case msg.NextLocalCommitHeight <= remoteTailHeight:
		lc.log.Errorf("sync failed: remote's next commit height is %v, "+
			"while we believe it is %v!",
			msg.NextLocalCommitHeight, remoteTipHeight+1)

		// They previously ACKed our current tail, and now they are
		// waiting for it. They probably lost state.
		return nil, nil, nil, ErrCommitSyncRemoteDataLoss

	// They have received our latest commitment, life is good.
	case msg.NextLocalCommitHeight == remoteTipHeight+1:

	// We owe them a commitment if the tip of their chain (from our Pov) is
	// equal to what they think their next commit height should be. We'll
	// re-send all the updates necessary to recreate this state, along
	// with the commit sig.
	case msg.NextLocalCommitHeight == remoteTipHeight:
		lc.log.Debugf("sync: remote's next commit height is %v, while "+
			"we believe it is %v, we owe them a commitment",
			msg.NextLocalCommitHeight, remoteTipHeight+1)

		// Grab the current remote chain tip from the database.  This
		// commit diff contains all the information required to re-sync
		// our states.
		commitDiff, err := lc.channelState.RemoteCommitChainTip()
		if err != nil {
			return nil, nil, nil, err
		}

		var commitUpdates []lnwire.Message

		// Next, we'll need to send over any updates we sent as part of
		// this new proposed commitment state.
		for _, logUpdate := range commitDiff.LogUpdates {
			commitUpdates = append(
				commitUpdates, logUpdate.UpdateMsg,
			)
		}

		// If this is a taproot channel, then we need to regenerate the
		// musig2 signature for the remote party, using their fresh
		// nonce.
		if lc.channelState.ChanType.IsTaproot() {
			partialSig, err := lc.resignMusigCommit(
				commitDiff.Commitment.CommitTx,
			)
			if err != nil {
				return nil, nil, nil, err
			}

			commitDiff.CommitSig.PartialSig = partialSig
		}

		// With the batch of updates accumulated, we'll now re-send the
		// original CommitSig message required to re-sync their remote
		// commitment chain with our local version of their chain.
		commitUpdates = append(commitUpdates, commitDiff.CommitSig)

		// NOTE: If a revocation is not owed, then updates is empty.
		if lc.channelState.LastWasRevoke {
			// If lastWasRevoke is set to true, a revocation was last and we
			// need to reorder the updates so that the revocation stored in
			// updates comes after the LogUpdates+CommitSig.
			//
			// ---logupdates--->
			// ---commitsig---->
			// ---revocation--->
			updates = append(commitUpdates, updates...)
		} else {
			// Otherwise, the revocation should come before LogUpdates
			// + CommitSig.
			//
			// ---revocation--->
			// ---logupdates--->
			// ---commitsig---->
			updates = append(updates, commitUpdates...)
		}

		openedCircuits = commitDiff.OpenedCircuitKeys
		closedCircuits = commitDiff.ClosedCircuitKeys

	// There should be no other possible states as long as the commit chain
	// can have at most two elements. If that's the case, something is
	// wrong.
	default:
		lc.log.Errorf("sync failed: remote's next commit height is %v, "+
			"while we believe it is %v!",
			msg.NextLocalCommitHeight, remoteTipHeight)
		return nil, nil, nil, ErrCannotSyncCommitChains
	}

	// If we didn't have recovery options, then the final check cannot be
	// performed, and we'll return early.
	if !hasRecoveryOptions {
		return updates, openedCircuits, closedCircuits, nil
	}

	// At this point we have determined that either the commit heights are
	// in sync, or that we are in a state we can recover from. As a final
	// check, we ensure that the commitment point sent to us by the remote
	// is valid.
	var commitPoint *btcec.PublicKey
	switch {
	// If their height is one beyond what we know their current height to
	// be, then we need to compare their current unrevoked commitment point
	// as that's what they should send.
	case msg.NextLocalCommitHeight == remoteTailHeight+1:
		commitPoint = lc.channelState.RemoteCurrentRevocation

	// Alternatively, if their height is two beyond what we know their best
	// height to be, then they're holding onto two commitments, and the
	// highest unrevoked point is their next revocation.
	//
	// TODO(roasbeef): verify this in the spec...
	case msg.NextLocalCommitHeight == remoteTailHeight+2:
		commitPoint = lc.channelState.RemoteNextRevocation
	}

	// Only if this is a tweakless channel will we attempt to verify the
	// commitment point, as otherwise it has no validity requirements.
	tweakless := lc.channelState.ChanType.IsTweakless()
	if !tweakless && commitPoint != nil &&
		!commitPoint.IsEqual(msg.LocalUnrevokedCommitPoint) {

		lc.log.Errorf("sync failed: remote sent invalid commit point "+
			"for height %v!",
			msg.NextLocalCommitHeight)
		return nil, nil, nil, ErrInvalidLocalUnrevokedCommitPoint
	}

	return updates, openedCircuits, closedCircuits, nil
}

// computeView takes the given HtlcView, and calculates the balances, filtered
// view (settling unsettled HTLCs), commitment weight and feePerKw, after
// applying the HTLCs to the latest commitment. The returned balances are the
// balances *before* subtracting the commitment fee from the initiator's
// balance. It accepts a "dry run" feerate argument to calculate a potential
// commitment transaction fee.
//
// If the updateState boolean is set true, the add and remove heights of the
// HTLCs will be set to the next commitment height.
func (lc *LightningChannel) computeView(view *HtlcView,
	whoseCommitChain lntypes.ChannelParty, updateState bool,
	dryRunFee fn.Option[chainfee.SatPerKWeight]) (lnwire.MilliSatoshi,
	lnwire.MilliSatoshi, lntypes.WeightUnit, *HtlcView, error) {

	commitChain := lc.commitChains.Local
	dustLimit := lc.channelState.LocalChanCfg.DustLimit
	if whoseCommitChain.IsRemote() {
		commitChain = lc.commitChains.Remote
		dustLimit = lc.channelState.RemoteChanCfg.DustLimit
	}

	// Since the fetched htlc view will include all updates added after the
	// last committed state, we start with the balances reflecting that
	// state.
	ourBalance := commitChain.tip().ourBalance
	theirBalance := commitChain.tip().theirBalance

	// Add the fee from the previous commitment state back to the
	// initiator's balance, so that the fee can be recalculated and
	// re-applied in case fee estimation parameters have changed or the
	// number of outstanding HTLCs has changed.
	if lc.channelState.IsInitiator {
		ourBalance += lnwire.NewMSatFromSatoshis(
			commitChain.tip().fee)
	} else if !lc.channelState.IsInitiator {
		theirBalance += lnwire.NewMSatFromSatoshis(
			commitChain.tip().fee)
	}
	nextHeight := commitChain.tip().height + 1

	// Initiate feePerKw to the last committed fee for this chain as we'll
	// need this to determine which HTLCs are dust, and also the final fee
	// rate.
	view.FeePerKw = commitChain.tip().feePerKw
	view.NextHeight = nextHeight

	// We evaluate the view at this stage, meaning settled and failed HTLCs
	// will remove their corresponding added HTLCs.  The resulting filtered
	// view will only have Add entries left, making it easy to compare the
	// channel constraints to the final commitment state. If any fee
	// updates are found in the logs, the commitment fee rate should be
	// changed, so we'll also set the feePerKw to this new value.
	filteredHTLCView, uncommitted, deltas, err := lc.evaluateHTLCView(
		view, whoseCommitChain, nextHeight,
	)
	if err != nil {
		return 0, 0, 0, nil, err
	}

	// Add the balance deltas to the balances we got from the commitment
	// state.
	if deltas.Local >= 0 {
		ourBalance += lnwire.MilliSatoshi(deltas.Local)
	} else {
		ourBalance -= lnwire.MilliSatoshi(-1 * deltas.Local)
	}
	if deltas.Remote >= 0 {
		theirBalance += lnwire.MilliSatoshi(deltas.Remote)
	} else {
		theirBalance -= lnwire.MilliSatoshi(-1 * deltas.Remote)
	}

	if updateState {
		for _, party := range lntypes.BothParties {
			for _, u := range uncommitted.GetForParty(party) {
				u.setCommitHeight(whoseCommitChain, nextHeight)

				if whoseCommitChain == lntypes.Local &&
					u.EntryType == Settle {

					// If this settle was a result of an
					// effective noop add entry, then we
					// don't need to record the amount as it
					// was never sent over to the other
					// side.
					if u.noOpSettle {
						continue
					}

					lc.recordSettlement(party, u.Amount)
				}
			}
		}
	}

	feePerKw := filteredHTLCView.FeePerKw

	// Here we override the view's fee-rate if a dry-run fee-rate was
	// passed in.
	if !updateState {
		feePerKw = dryRunFee.UnwrapOr(feePerKw)
	}

	// We need to first check ourBalance and theirBalance to be negative
	// because MilliSathoshi is a unsigned type and can underflow in the
	// code above. This should never happen for views which do not
	// include new updates (remote or local).
	if int64(ourBalance) < 0 {
		err := fmt.Errorf("%w: our balance", ErrBelowChanReserve)
		return 0, 0, 0, nil, err
	}
	if int64(theirBalance) < 0 {
		err := fmt.Errorf("%w: their balance", ErrBelowChanReserve)
		return 0, 0, 0, nil, err
	}

	// Now go through all HTLCs at this stage, to calculate the total
	// weight, needed to calculate the transaction fee.
	var totalHtlcWeight lntypes.WeightUnit
	for _, htlc := range filteredHTLCView.Updates.Local {
		if HtlcIsDust(
			lc.channelState.ChanType, false, whoseCommitChain,
			feePerKw, htlc.Amount.ToSatoshis(), dustLimit,
		) {

			continue
		}

		totalHtlcWeight += input.HTLCWeight
	}
	for _, htlc := range filteredHTLCView.Updates.Remote {
		if HtlcIsDust(
			lc.channelState.ChanType, true, whoseCommitChain,
			feePerKw, htlc.Amount.ToSatoshis(), dustLimit,
		) {

			continue
		}

		totalHtlcWeight += input.HTLCWeight
	}

	totalCommitWeight := CommitWeight(lc.channelState.ChanType) +
		totalHtlcWeight
	return ourBalance, theirBalance, totalCommitWeight, filteredHTLCView, nil
}

// recordSettlement updates the lifetime payment flow values in persistent state
// of the LightningChannel, adding amt to the total received by the redeemer.
func (lc *LightningChannel) recordSettlement(
	redeemer lntypes.ChannelParty, amt lnwire.MilliSatoshi) {

	if redeemer == lntypes.Local {
		lc.channelState.TotalMSatReceived += amt
	} else {
		lc.channelState.TotalMSatSent += amt
	}
}

// genHtlcSigValidationJobs generates a series of signatures verification jobs
// meant to verify all the signatures for HTLC's attached to a newly created
// commitment state. The jobs generated are fully populated, and can be sent
// directly into the pool of workers.
//
//nolint:funlen
func genHtlcSigValidationJobs(chanState *channeldb.OpenChannel,
	localCommitmentView *commitment, keyRing *CommitmentKeyRing,
	htlcSigs []lnwire.Sig, leaseExpiry uint32,
	leafStore fn.Option[AuxLeafStore], auxSigner fn.Option[AuxSigner],
	sigBlob fn.Option[tlv.Blob]) ([]VerifyJob, []AuxVerifyJob, error) {

	var (
		isLocalInitiator = chanState.IsInitiator
		localChanCfg     = chanState.LocalChanCfg
		chanType         = chanState.ChanType
	)

	txHash := localCommitmentView.txn.TxHash()
	feePerKw := localCommitmentView.feePerKw
	sigHashType := HtlcSigHashType(chanType)

	// With the required state generated, we'll create a slice with large
	// enough capacity to hold verification jobs for all HTLC's in this
	// view. In the case that we have some dust outputs, then the actual
	// length will be smaller than the total capacity.
	numHtlcs := len(localCommitmentView.incomingHTLCs) +
		len(localCommitmentView.outgoingHTLCs)
	verifyJobs := make([]VerifyJob, 0, numHtlcs)
	auxVerifyJobs := make([]AuxVerifyJob, 0, numHtlcs)

	diskCommit := localCommitmentView.toDiskCommit(lntypes.Local)
	auxResult, err := fn.MapOptionZ(
		leafStore, func(s AuxLeafStore) fn.Result[CommitDiffAuxResult] {
			return s.FetchLeavesFromCommit(
				NewAuxChanState(chanState), *diskCommit,
				*keyRing, lntypes.Local,
			)
		},
	).Unpack()
	if err != nil {
		return nil, nil, fmt.Errorf("unable to fetch aux leaves: %w",
			err)
	}

	// If we have a sig blob, then we'll attempt to map that to individual
	// blobs for each HTLC we might need a signature for.
	auxHtlcSigs, err := fn.MapOptionZ(
		auxSigner, func(a AuxSigner) fn.Result[[]fn.Option[tlv.Blob]] {
			return a.UnpackSigs(sigBlob)
		},
	).Unpack()
	if err != nil {
		return nil, nil, fmt.Errorf("error unpacking aux sigs: %w",
			err)
	}

	// We'll iterate through each output in the commitment transaction,
	// populating the sigHash closure function if it's detected to be an
	// HLTC output. Given the sighash, and the signing key, we'll be able
	// to validate each signature within the worker pool.
	i := 0
	for index := range localCommitmentView.txn.TxOut {
		var (
			htlcIndex uint64
			sigHash   func() ([]byte, error)
			sig       input.Signature
			htlc      *paymentDescriptor
			incoming  bool
			auxLeaf   input.AuxTapLeaf
			err       error
		)

		outputIndex := int32(index)
		switch {

		// If this output index is found within the incoming HTLC
		// index, then this means that we need to generate an HTLC
		// success transaction in order to validate the signature.
		//nolint:ll
		case localCommitmentView.incomingHTLCIndex[outputIndex] != nil:
			htlc = localCommitmentView.incomingHTLCIndex[outputIndex]

			htlcIndex = htlc.HtlcIndex
			incoming = true

			sigHash = func() ([]byte, error) {
				op := wire.OutPoint{
					Hash:  txHash,
					Index: uint32(htlc.localOutputIndex),
				}

				htlcFee := HtlcSuccessFee(chanType, feePerKw)
				outputAmt := htlc.Amount.ToSatoshis() - htlcFee

				auxLeaf := fn.FlatMapOption(func(
					l CommitAuxLeaves) input.AuxTapLeaf {

					leaves := l.IncomingHtlcLeaves
					idx := htlc.HtlcIndex
					return leaves[idx].SecondLevelLeaf
				})(auxResult.AuxLeaves)

				successTx, err := CreateHtlcSuccessTx(
					chanType, isLocalInitiator, op,
					outputAmt, uint32(localChanCfg.CsvDelay),
					leaseExpiry, keyRing.RevocationKey,
					keyRing.ToLocalKey, auxLeaf,
				)
				if err != nil {
					return nil, err
				}

				htlcAmt := int64(htlc.Amount.ToSatoshis())

				if chanType.IsTaproot() {
					// TODO(roasbeef): add abstraction in
					// front
					prevFetcher := txscript.NewCannedPrevOutputFetcher( //nolint:ll
						htlc.ourPkScript, htlcAmt,
					)
					hashCache := txscript.NewTxSigHashes(
						successTx, prevFetcher,
					)
					tapLeaf := txscript.NewBaseTapLeaf(
						htlc.ourWitnessScript,
					)

					return txscript.CalcTapscriptSignaturehash( //nolint:ll
						hashCache, sigHashType,
						successTx, 0, prevFetcher,
						tapLeaf,
					)
				}

				hashCache := input.NewTxSigHashesV0Only(successTx)
				sigHash, err := txscript.CalcWitnessSigHash(
					htlc.ourWitnessScript, hashCache,
					sigHashType, successTx, 0,
					htlcAmt,
				)
				if err != nil {
					return nil, err
				}

				return sigHash, nil
			}

			// Make sure there are more signatures left.
			if i >= len(htlcSigs) {
				return nil, nil, fmt.Errorf("not enough HTLC " +
					"signatures")
			}

			// If this is a taproot channel, then we'll convert it
			// to a schnorr signature, so we can get correct type
			// from ToSignature below.
			if chanType.IsTaproot() {
				htlcSigs[i].ForceSchnorr()
			}

			// With the sighash generated, we'll also store the
			// signature so it can be written to disk if this state
			// is valid.
			sig, err = htlcSigs[i].ToSignature()
			if err != nil {
				return nil, nil, err
			}
			htlc.sig = sig

		// Otherwise, if this is an outgoing HTLC, then we'll need to
		// generate a timeout transaction so we can verify the
		// signature presented.
		//nolint:ll
		case localCommitmentView.outgoingHTLCIndex[outputIndex] != nil:
			htlc = localCommitmentView.outgoingHTLCIndex[outputIndex]

			htlcIndex = htlc.HtlcIndex

			sigHash = func() ([]byte, error) {
				op := wire.OutPoint{
					Hash:  txHash,
					Index: uint32(htlc.localOutputIndex),
				}

				htlcFee := HtlcTimeoutFee(chanType, feePerKw)
				outputAmt := htlc.Amount.ToSatoshis() - htlcFee

				auxLeaf := fn.FlatMapOption(func(
					l CommitAuxLeaves) input.AuxTapLeaf {

					leaves := l.OutgoingHtlcLeaves
					idx := htlc.HtlcIndex
					return leaves[idx].SecondLevelLeaf
				})(auxResult.AuxLeaves)

				timeoutTx, err := CreateHtlcTimeoutTx(
					chanType, isLocalInitiator, op,
					outputAmt, htlc.Timeout,
					uint32(localChanCfg.CsvDelay),
					leaseExpiry, keyRing.RevocationKey,
					keyRing.ToLocalKey, auxLeaf,
				)
				if err != nil {
					return nil, err
				}

				htlcAmt := int64(htlc.Amount.ToSatoshis())

				if chanType.IsTaproot() {
					// TODO(roasbeef): add abstraction in
					// front
					prevFetcher := txscript.NewCannedPrevOutputFetcher( //nolint:ll
						htlc.ourPkScript, htlcAmt,
					)
					hashCache := txscript.NewTxSigHashes(
						timeoutTx, prevFetcher,
					)
					tapLeaf := txscript.NewBaseTapLeaf(
						htlc.ourWitnessScript,
					)

					return txscript.CalcTapscriptSignaturehash( //nolint:ll
						hashCache, sigHashType,
						timeoutTx, 0, prevFetcher,
						tapLeaf,
					)
				}

				hashCache := input.NewTxSigHashesV0Only(
					timeoutTx,
				)
				sigHash, err := txscript.CalcWitnessSigHash(
					htlc.ourWitnessScript, hashCache,
					sigHashType, timeoutTx, 0,
					htlcAmt,
				)
				if err != nil {
					return nil, err
				}

				return sigHash, nil
			}

			// Make sure there are more signatures left.
			if i >= len(htlcSigs) {
				return nil, nil, fmt.Errorf("not enough HTLC " +
					"signatures")
			}

			// If this is a taproot channel, then we'll convert it
			// to a schnorr signature, so we can get correct type
			// from ToSignature below.
			if chanType.IsTaproot() {
				htlcSigs[i].ForceSchnorr()
			}

			// With the sighash generated, we'll also store the
			// signature so it can be written to disk if this state
			// is valid.
			sig, err = htlcSigs[i].ToSignature()
			if err != nil {
				return nil, nil, err
			}

			htlc.sig = sig

		default:
			continue
		}

		verifyJobs = append(verifyJobs, VerifyJob{
			HtlcIndex: htlcIndex,
			PubKey:    keyRing.RemoteHtlcKey,
			Sig:       sig,
			SigHash:   sigHash,
		})

		if len(auxHtlcSigs) > i {
			auxSig := auxHtlcSigs[i]
			auxVerifyJob := NewAuxVerifyJob(
				auxSig, *keyRing, incoming,
				newAuxHtlcDescriptor(htlc),
				localCommitmentView.customBlob, auxLeaf,
			)

			if htlc.CustomRecords == nil {
				htlc.CustomRecords = make(lnwire.CustomRecords)
			}

			// As this HTLC has a custom signature associated with
			// it, store it in the custom records map so we can
			// write to disk later.
			sigType := htlcCustomSigType.TypeVal()
			htlc.CustomRecords[uint64(sigType)] = auxSig.UnwrapOr(
				nil,
			)

			auxVerifyJobs = append(auxVerifyJobs, auxVerifyJob)
		}

		i++
	}

	// If we received a number of HTLC signatures that doesn't match our
	// commitment, we'll return an error now.
	if len(htlcSigs) != i {
		return nil, nil, fmt.Errorf("number of htlc sig mismatch. "+
			"Expected %v sigs, got %v", i, len(htlcSigs))
	}

	return verifyJobs, auxVerifyJobs, nil
}

// InvalidCommitSigError is a struct that implements the error interface to
// report a failure to validate a commitment signature for a remote peer.
// We'll use the items in this struct to generate a rich error message for the
// remote peer when we receive an invalid signature from it. Doing so can
// greatly aide in debugging cross implementation issues.
type InvalidCommitSigError struct {
	commitHeight uint64

	commitSig []byte

	sigHash []byte

	commitTx []byte
}

// Error returns a detailed error string including the exact transaction that
// caused an invalid commitment signature.
func (i *InvalidCommitSigError) Error() string {
	return fmt.Sprintf("rejected commitment: commit_height=%v, "+
		"invalid_commit_sig=%x, commit_tx=%x, sig_hash=%x", i.commitHeight,
		i.commitSig[:], i.commitTx, i.sigHash[:])
}

// A compile time flag to ensure that InvalidCommitSigError implements the
// error interface.
var _ error = (*InvalidCommitSigError)(nil)

// InvalidPartialCommitSigError is used when we encounter an invalid musig2
// partial signature.
type InvalidPartialCommitSigError struct {
	InvalidCommitSigError

	*invalidPartialSigError
}

// Error returns a detailed error string including the exact transaction that
// caused an invalid partial commit sig signature.
func (i *InvalidPartialCommitSigError) Error() string {
	return fmt.Sprintf("rejected commitment: commit_height=%v, "+
		"commit_tx=%x -- %v", i.commitHeight, i.commitTx,
		i.invalidPartialSigError)
}

// InvalidHtlcSigError is a struct that implements the error interface to
// report a failure to validate an htlc signature from a remote peer. We'll use
// the items in this struct to generate a rich error message for the remote
// peer when we receive an invalid signature from it. Doing so can greatly aide
// in debugging across implementation issues.
type InvalidHtlcSigError struct {
	commitHeight uint64

	htlcSig []byte

	htlcIndex uint64

	sigHash []byte

	commitTx []byte
}

// Error returns a detailed error string including the exact transaction that
// caused an invalid htlc signature.
func (i *InvalidHtlcSigError) Error() string {
	return fmt.Sprintf("rejected commitment: commit_height=%v, "+
		"invalid_htlc_sig=%x, commit_tx=%x, sig_hash=%x", i.commitHeight,
		i.htlcSig, i.commitTx, i.sigHash[:])
}

// A compile time flag to ensure that InvalidCommitSigError implements the
// error interface.
var _ error = (*InvalidCommitSigError)(nil)

// ReceiveNewCommitment process a signature for a new commitment state sent by
// the remote party. This method should be called in response to the
// remote party initiating a new change, or when the remote party sends a
// signature fully accepting a new state we've initiated. If we are able to
// successfully validate the signature, then the generated commitment is added
// to our local commitment chain. Once we send a revocation for our prior
// state, then this newly added commitment becomes our current accepted channel
// state.
//
//nolint:funlen
func (lc *LightningChannel) ReceiveNewCommitment(commitSigs *CommitSigs) error {
	lc.Lock()
	defer lc.Unlock()

	// Check for empty commit sig. Because of a previously existing bug, it
	// is possible that we receive an empty commit sig from nodes running an
	// older version. This is a relaxation of the spec, but it is still
	// possible to handle it. To not break any channels with those older
	// nodes, we just log the event. This check is also not totally
	// reliable, because it could be that we've sent out a new sig, but the
	// remote hasn't received it yet. We could then falsely assume that they
	// should add our updates to their remote commitment tx.
	if !lc.oweCommitment(lntypes.Remote) {
		lc.log.Warnf("empty commit sig message received")
	}

	// Determine the last update on the local log that has been locked in.
	localACKedIndex := lc.commitChains.Remote.tail().messageIndices.Local
	localHtlcIndex := lc.commitChains.Remote.tail().ourHtlcIndex

	// Ensure that this new local update from the remote node respects all
	// the constraints we specified during initial channel setup. If not,
	// then we'll abort the channel as they've violated our constraints.
	//
	// We do not enforce the FeeBuffer here because when we reach this
	// point all updates will have to get locked-in (we already received
	// the UpdateAddHTLC msg from our peer prior to receiving the
	// commit-sig).
	err := lc.validateCommitmentSanity(
		lc.updateLogs.Remote.logIndex, localACKedIndex, lntypes.Local,
		NoBuffer, nil, nil,
	)
	if err != nil {
		return err
	}

	// We're receiving a new commitment which attempts to extend our local
	// commitment chain height by one, so fetch the proper commitment point
	// as this will be needed to derive the keys required to construct the
	// commitment.
	nextHeight := lc.currentHeight + 1
	commitSecret, err := lc.channelState.RevocationProducer.AtIndex(nextHeight)
	if err != nil {
		return err
	}
	commitPoint := input.ComputeCommitmentPoint(commitSecret[:])
	keyRing := DeriveCommitmentKeys(
		commitPoint, lntypes.Local, lc.channelState.ChanType,
		&lc.channelState.LocalChanCfg, &lc.channelState.RemoteChanCfg,
	)

	// With the current commitment point re-calculated, construct the new
	// commitment view which includes all the entries (pending or committed)
	// we know of in the remote node's HTLC log, but only our local changes
	// up to the last change the remote node has ACK'd.
	localCommitmentView, err := lc.fetchCommitmentView(
		lntypes.Local, localACKedIndex, localHtlcIndex,
		lc.updateLogs.Remote.logIndex, lc.updateLogs.Remote.htlcCounter,
		keyRing,
	)
	if err != nil {
		return err
	}

	lc.log.Tracef("extending local chain to height %v, "+
		"local_log=%v, remote_log=%v",
		localCommitmentView.height,
		localACKedIndex, lc.updateLogs.Remote.logIndex)

	lc.log.Tracef("local chain: our_balance=%v, "+
		"their_balance=%v, commit_tx: %v",
		localCommitmentView.ourBalance, localCommitmentView.theirBalance,
		lnutils.SpewLogClosure(localCommitmentView.txn))

	var auxSigBlob fn.Option[tlv.Blob]
	if commitSigs.AuxSigBlob != nil {
		auxSigBlob = fn.Some(commitSigs.AuxSigBlob)
	}

	// As an optimization, we'll generate a series of jobs for the worker
	// pool to verify each of the HTLC signatures presented. Once
	// generated, we'll submit these jobs to the worker pool.
	var leaseExpiry uint32
	if lc.channelState.ChanType.HasLeaseExpiration() {
		leaseExpiry = lc.channelState.ThawHeight
	}
	verifyJobs, auxVerifyJobs, err := genHtlcSigValidationJobs(
		lc.channelState, localCommitmentView, keyRing,
		commitSigs.HtlcSigs, leaseExpiry, lc.leafStore, lc.auxSigner,
		auxSigBlob,
	)
	if err != nil {
		return err
	}

	cancelChan := make(chan struct{})
	verifyResps := lc.sigPool.SubmitVerifyBatch(verifyJobs, cancelChan)

	localCommitTx := localCommitmentView.txn

	// While the HTLC verification jobs are proceeding asynchronously,
	// we'll ensure that the newly constructed commitment state has a valid
	// signature.
	//
	// To do that we'll, construct the sighash of the commitment
	// transaction corresponding to this newly proposed state update.  If
	// this is a taproot channel, then in order to validate the sighash,
	// we'll need to call into the relevant tapscript methods.
	if lc.channelState.ChanType.IsTaproot() {
		localSession := lc.musigSessions.LocalSession

		partialSig, err := commitSigs.PartialSig.UnwrapOrErrV(
			errNoPartialSig,
		)
		if err != nil {
			return err
		}

		// As we want to ensure we never write nonces to disk, we'll
		// use the shachain state to generate a nonce for our next
		// local state. Similar to generateRevocation, we do height + 2
		// (next height + 1) here, as this is for the _next_ local
		// state, and we're about to accept height + 1.
		localCtrNonce := WithLocalCounterNonce(
			nextHeight+1, lc.taprootNonceProducer,
		)
		nextVerificationNonce, err := localSession.VerifyCommitSig(
			localCommitTx, &partialSig, localCtrNonce,
		)
		if err != nil {
			close(cancelChan)

			var sigErr invalidPartialSigError
			if errors.As(err, &sigErr) {
				// If we fail to validate their commitment
				// signature, we'll generate a special error to
				// send over the protocol. We'll include the
				// exact signature and commitment we failed to
				// verify against in order to aide debugging.
				var txBytes bytes.Buffer
				_ = localCommitTx.Serialize(&txBytes)
				return &InvalidPartialCommitSigError{
					invalidPartialSigError: &sigErr,
					InvalidCommitSigError: InvalidCommitSigError{ //nolint:ll
						commitHeight: nextHeight,
						commitTx:     txBytes.Bytes(),
					},
				}
			}

			return err
		}

		// Now that we have the next verification nonce for our local
		// session, we'll refresh it to yield a new session we'll use
		// for the next incoming signature.
		newLocalSession, err := lc.musigSessions.LocalSession.Refresh(
			nextVerificationNonce,
		)
		if err != nil {
			return err
		}
		lc.musigSessions.LocalSession = newLocalSession
	} else {
		multiSigScript := lc.signDesc.WitnessScript
		prevFetcher := txscript.NewCannedPrevOutputFetcher(
			multiSigScript, int64(lc.channelState.Capacity),
		)
		hashCache := txscript.NewTxSigHashes(localCommitTx, prevFetcher)

		sigHash, err := txscript.CalcWitnessSigHash(
			multiSigScript, hashCache, txscript.SigHashAll,
			localCommitTx, 0, int64(lc.channelState.Capacity),
		)
		if err != nil {
			// TODO(roasbeef): fetchview has already mutated the
			// HTLCs...  * need to either roll-back, or make pure
			return err
		}

		verifyKey := lc.channelState.RemoteChanCfg.MultiSigKey.PubKey

		cSig, err := commitSigs.CommitSig.ToSignature()
		if err != nil {
			return err
		}
		if !cSig.Verify(sigHash, verifyKey) {
			close(cancelChan)

			// If we fail to validate their commitment signature,
			// we'll generate a special error to send over the
			// protocol. We'll include the exact signature and
			// commitment we failed to verify against in order to
			// aide debugging.
			var txBytes bytes.Buffer
			_ = localCommitTx.Serialize(&txBytes)

			return &InvalidCommitSigError{
				commitHeight: nextHeight,
				commitSig:    commitSigs.CommitSig.ToSignatureBytes(), //nolint:ll
				sigHash:      sigHash,
				commitTx:     txBytes.Bytes(),
			}
		}
	}

	// With the primary commitment transaction validated, we'll check each
	// of the HTLC validation jobs.
	for i := 0; i < len(verifyJobs); i++ {
		// In the case that a single signature is invalid, we'll exit
		// early and cancel all the outstanding verification jobs.
		htlcErr := <-verifyResps
		if htlcErr != nil {
			close(cancelChan)

			sig, err := lnwire.NewSigFromSignature(
				htlcErr.Sig,
			)
			if err != nil {
				return err
			}
			sigHash, err := htlcErr.SigHash()
			if err != nil {
				return err
			}

			var txBytes bytes.Buffer
			err = localCommitTx.Serialize(&txBytes)
			if err != nil {
				return err
			}

			return &InvalidHtlcSigError{
				commitHeight: nextHeight,
				htlcSig:      sig.ToSignatureBytes(),
				htlcIndex:    htlcErr.HtlcIndex,
				sigHash:      sigHash,
				commitTx:     txBytes.Bytes(),
			}
		}
	}

	// Now that we know all the normal sigs are valid, we'll also verify
	// the aux jobs, if any exist.
	err = fn.MapOptionZ(lc.auxSigner, func(a AuxSigner) error {
		return a.VerifySecondLevelSigs(
			NewAuxChanState(lc.channelState), localCommitTx,
			auxVerifyJobs,
		)
	})
	if err != nil {
		return fmt.Errorf("unable to validate aux sigs: %w", err)
	}

	// The signature checks out, so we can now add the new commitment to
	// our local commitment chain. For regular channels, we can just
	// serialize the ECDSA sig. For taproot channels, we'll serialize the
	// partial sig that includes the nonce that was used for signing.
	if lc.channelState.ChanType.IsTaproot() {
		partialSig, err := commitSigs.PartialSig.UnwrapOrErrV(
			errNoPartialSig,
		)
		if err != nil {
			return err
		}

		var sigBytes [lnwire.PartialSigWithNonceLen]byte
		b := bytes.NewBuffer(sigBytes[0:0])
		if err := partialSig.Encode(b); err != nil {
			return err
		}

		localCommitmentView.sig = sigBytes[:]
	} else {
		localCommitmentView.sig = commitSigs.CommitSig.ToSignatureBytes() //nolint:ll
	}

	lc.commitChains.Local.addCommitment(localCommitmentView)

	return nil
}

// IsChannelClean returns true if neither side has pending commitments, neither
// side has HTLC's, and all updates are locked in irrevocably. Internally, it
// utilizes the oweCommitment function by calling it for local and remote
// evaluation. We check if we have a pending commitment for our local state
// since this function may be called by sub-systems that are not the link (e.g.
// the rpcserver), and the ReceiveNewCommitment & RevokeCurrentCommitment calls
// are not atomic, even though link processing ensures no updates can happen in
// between.
func (lc *LightningChannel) IsChannelClean() bool {
	lc.RLock()
	defer lc.RUnlock()

	// Check whether we have a pending commitment for our local state.
	if lc.commitChains.Local.hasUnackedCommitment() {
		return false
	}

	// Check whether our counterparty has a pending commitment for their
	// state.
	if lc.commitChains.Remote.hasUnackedCommitment() {
		return false
	}

	// We call ActiveHtlcs to ensure there are no HTLCs on either
	// commitment.
	if len(lc.channelState.ActiveHtlcs()) != 0 {
		return false
	}

	// Now check that both local and remote commitments are signing the
	// same updates.
	if lc.oweCommitment(lntypes.Local) {
		return false
	}

	if lc.oweCommitment(lntypes.Remote) {
		return false
	}

	// If we reached this point, the channel has no HTLCs and both
	// commitments sign the same updates.
	return true
}

// OweCommitment returns a boolean value reflecting whether we need to send
// out a commitment signature because there are outstanding local updates and/or
// updates in the local commit tx that aren't reflected in the remote commit tx
// yet.
func (lc *LightningChannel) OweCommitment() bool {
	lc.RLock()
	defer lc.RUnlock()

	return lc.oweCommitment(lntypes.Local)
}

// NeedCommitment returns a boolean value reflecting whether we are waiting on
// a commitment signature because there are outstanding remote updates and/or
// updates in the remote commit tx that aren't reflected in the local commit tx
// yet.
func (lc *LightningChannel) NeedCommitment() bool {
	lc.RLock()
	defer lc.RUnlock()

	return lc.oweCommitment(lntypes.Remote)
}

// oweCommitment is the internal version of OweCommitment. This function expects
// to be executed with a lock held.
func (lc *LightningChannel) oweCommitment(issuer lntypes.ChannelParty) bool {
	var (
		remoteUpdatesPending, localUpdatesPending bool

		lastLocalCommit  = lc.commitChains.Local.tip()
		lastRemoteCommit = lc.commitChains.Remote.tip()

		perspective string
	)

	if issuer.IsLocal() {
		perspective = "local"

		// There are local updates pending if our local update log is
		// not in sync with our remote commitment tx.
		localUpdatesPending = lc.updateLogs.Local.logIndex !=
			lastRemoteCommit.messageIndices.Local

		// There are remote updates pending if their remote commitment
		// tx (our local commitment tx) contains updates that we don't
		// have added to our remote commitment tx yet.
		remoteUpdatesPending = lastLocalCommit.messageIndices.Remote !=
			lastRemoteCommit.messageIndices.Remote
	} else {
		perspective = "remote"

		// There are local updates pending (local updates from the
		// perspective of the remote party) if the remote party has
		// updates to their remote tx pending for which they haven't
		// signed yet.
		localUpdatesPending = lc.updateLogs.Remote.logIndex !=
			lastLocalCommit.messageIndices.Remote

		// There are remote updates pending (remote updates from the
		// perspective of the remote party) if we have updates on our
		// remote commitment tx that they haven't added to theirs yet.
		remoteUpdatesPending = lastRemoteCommit.messageIndices.Local !=
			lastLocalCommit.messageIndices.Local
	}

	// If any of the conditions above is true, we owe a commitment
	// signature.
	oweCommitment := localUpdatesPending || remoteUpdatesPending

	lc.log.Tracef("%v owes commit: %v (local updates: %v, "+
		"remote updates %v)", perspective, oweCommitment,
		localUpdatesPending, remoteUpdatesPending)

	return oweCommitment
}

// NumPendingUpdates returns the number of updates originated by whoseUpdates
// that have not been committed to the *tip* of whoseCommit's commitment chain.
func (lc *LightningChannel) NumPendingUpdates(whoseUpdates lntypes.ChannelParty,
	whoseCommit lntypes.ChannelParty) uint64 {

	lc.RLock()
	defer lc.RUnlock()

	lastCommit := lc.commitChains.GetForParty(whoseCommit).tip()
	updateIndex := lc.updateLogs.GetForParty(whoseUpdates).logIndex

	return updateIndex - lastCommit.messageIndices.GetForParty(whoseUpdates)
}

// RevokeCurrentCommitment revokes the next lowest unrevoked commitment
// transaction in the local commitment chain. As a result the edge of our
// revocation window is extended by one, and the tail of our local commitment
// chain is advanced by a single commitment. This now lowest unrevoked
// commitment becomes our currently accepted state within the channel. This
// method also returns the set of HTLC's currently active within the commitment
// transaction and the htlcs the were resolved. This return value allows callers
// to act once an HTLC has been locked into our commitment transaction.
func (lc *LightningChannel) RevokeCurrentCommitment() (*lnwire.RevokeAndAck,
	[]channeldb.HTLC, map[uint64]bool, error) {

	lc.Lock()
	defer lc.Unlock()

	revocationMsg, err := lc.generateRevocation(lc.currentHeight)
	if err != nil {
		return nil, nil, nil, err
	}

	lc.log.Tracef("revoking height=%v, now at height=%v",
		lc.commitChains.Local.tail().height,
		lc.currentHeight+1)

	// Advance our tail, as we've revoked our previous state.
	lc.commitChains.Local.advanceTail()
	lc.currentHeight++

	// Additionally, generate a channel delta for this state transition for
	// persistent storage.
	chainTail := lc.commitChains.Local.tail()
	newCommitment := chainTail.toDiskCommit(lntypes.Local)

	// Get the unsigned acked remotes updates that are currently in memory.
	// We need them after a restart to sync our remote commitment with what
	// is committed locally.
	unsignedAckedUpdates := lc.getUnsignedAckedUpdates()

	finalHtlcs, err := lc.channelState.UpdateCommitment(
		newCommitment, unsignedAckedUpdates,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	lc.log.Tracef("state transition accepted: "+
		"our_balance=%v, their_balance=%v, unsigned_acked_updates=%v",
		chainTail.ourBalance,
		chainTail.theirBalance,
		len(unsignedAckedUpdates))

	revocationMsg.ChanID = lnwire.NewChanIDFromOutPoint(
		lc.channelState.FundingOutpoint,
	)

	return revocationMsg, newCommitment.Htlcs, finalHtlcs, nil
}

// ReceiveRevocation processes a revocation sent by the remote party for the
// lowest unrevoked commitment within their commitment chain. We receive a
// revocation either during the initial session negotiation wherein revocation
// windows are extended, or in response to a state update that we initiate. If
// successful, then the remote commitment chain is advanced by a single
// commitment, and a log compaction is attempted.
//
// The returned values correspond to:
//  1. The forwarding package corresponding to the remote commitment height
//     that was revoked.
//  2. The set of HTLCs present on the current valid commitment transaction
//     for the remote party.
func (lc *LightningChannel) ReceiveRevocation(revMsg *lnwire.RevokeAndAck) (
	*channeldb.FwdPkg, []channeldb.HTLC, error) {

	lc.Lock()
	defer lc.Unlock()

	// Ensure that the new pre-image can be placed in preimage store.
	store := lc.channelState.RevocationStore
	revocation, err := chainhash.NewHash(revMsg.Revocation[:])
	if err != nil {
		return nil, nil, err
	}
	if err := store.AddNextEntry(revocation); err != nil {
		return nil, nil, err
	}

	// Verify that if we use the commitment point computed based off of the
	// revealed secret to derive a revocation key with our revocation base
	// point, then it matches the current revocation of the remote party.
	currentCommitPoint := lc.channelState.RemoteCurrentRevocation
	derivedCommitPoint := input.ComputeCommitmentPoint(revMsg.Revocation[:])
	if !derivedCommitPoint.IsEqual(currentCommitPoint) {
		return nil, nil, fmt.Errorf("revocation key mismatch")
	}

	// Now that we've verified that the prior commitment has been properly
	// revoked, we'll advance the revocation state we track for the remote
	// party: the new current revocation is what was previously the next
	// revocation, and the new next revocation is set to the key included
	// in the message.
	lc.channelState.RemoteCurrentRevocation = lc.channelState.RemoteNextRevocation
	lc.channelState.RemoteNextRevocation = revMsg.NextRevocationKey

	lc.log.Tracef("remote party accepted state transition, revoked height "+
		"%v, now at %v",
		lc.commitChains.Remote.tail().height,
		lc.commitChains.Remote.tail().height+1)

	// Add one to the remote tail since this will be height *after* we write
	// the revocation to disk, the local height will remain unchanged.
	remoteChainTail := lc.commitChains.Remote.tail().height + 1
	localChainTail := lc.commitChains.Local.tail().height

	source := lc.ShortChanID()

	// Determine the set of htlcs that can be forwarded as a result of
	// having received the revocation. We will simultaneously construct the
	// log updates and payment descriptors, allowing us to persist the log
	// updates to disk and optimistically buffer the forwarding package in
	// memory.
	var (
		addUpdatesToForward        []channeldb.LogUpdate
		settleFailUpdatesToForward []channeldb.LogUpdate
	)

	var addIndex, settleFailIndex uint16
	for e := lc.updateLogs.Remote.Front(); e != nil; e = e.Next() {
		pd := e.Value

		// Fee updates are local to this particular channel, and should
		// never be forwarded.
		if pd.EntryType == FeeUpdate {
			continue
		}

		if pd.isForwarded {
			continue
		}

		// For each type of HTLC, we will only consider forwarding it if
		// both of the remote and local heights are non-zero. If either
		// of these values is zero, it has yet to be committed in both
		// the local and remote chains.
		committedAdd := pd.addCommitHeights.Remote > 0 &&
			pd.addCommitHeights.Local > 0
		committedRmv := pd.removeCommitHeights.Remote > 0 &&
			pd.removeCommitHeights.Local > 0

		// Using the height of the remote and local commitments,
		// preemptively compute whether or not to forward this HTLC for
		// the case in which this in an Add HTLC, or if this is a
		// Settle, Fail, or MalformedFail.
		shouldFwdAdd := remoteChainTail == pd.addCommitHeights.Remote &&
			localChainTail >= pd.addCommitHeights.Local
		shouldFwdRmv := remoteChainTail ==
			pd.removeCommitHeights.Remote &&
			localChainTail >= pd.removeCommitHeights.Local

		// We'll only forward any new HTLC additions iff, it's "freshly
		// locked in". Meaning that the HTLC was only *just* considered
		// locked-in at this new state. By doing this we ensure that we
		// don't re-forward any already processed HTLC's after a
		// restart.
		switch {
		case pd.isAdd() && committedAdd && shouldFwdAdd:
			// Construct a reference specifying the location that
			// this forwarded Add will be written in the forwarding
			// package constructed at this remote height.
			pd.SourceRef = &channeldb.AddRef{
				Height: remoteChainTail,
				Index:  addIndex,
			}
			addIndex++

			pd.isForwarded = true

			// At this point we put the update into our list of
			// updates that we will eventually put into the
			// FwdPkg at this height.
			addUpdatesToForward = append(
				addUpdatesToForward, pd.toLogUpdate(),
			)

		case !pd.isAdd() && committedRmv && shouldFwdRmv:
			// Construct a reference specifying the location that
			// this forwarded Settle/Fail will be written in the
			// forwarding package constructed at this remote height.
			pd.DestRef = &channeldb.SettleFailRef{
				Source: source,
				Height: remoteChainTail,
				Index:  settleFailIndex,
			}
			settleFailIndex++

			pd.isForwarded = true

			// At this point we put the update into our list of
			// updates that we will eventually put into the
			// FwdPkg at this height.
			settleFailUpdatesToForward = append(
				settleFailUpdatesToForward, pd.toLogUpdate(),
			)

		default:
			// The update was not "freshly locked in" so we will
			// ignore it as we construct the forwarding package.
			continue
		}
	}

	// We use the remote commitment chain's tip as it will soon become the tail
	// once advanceTail is called.
	remoteMessageIndex := lc.commitChains.Remote.tip().messageIndices.Local
	localMessageIndex := lc.commitChains.Local.tail().messageIndices.Local

	localPeerUpdates := lc.unsignedLocalUpdates(
		remoteMessageIndex, localMessageIndex,
	)

	// Now that we have gathered the set of HTLCs to forward, separated by
	// type, construct a forwarding package using the height that the remote
	// commitment chain will be extended after persisting the revocation.
	fwdPkg := channeldb.NewFwdPkg(
		source, remoteChainTail, addUpdatesToForward,
		settleFailUpdatesToForward,
	)

	// We will soon be saving the current remote commitment to revocation
	// log bucket, which is `lc.channelState.RemoteCommitment`. After that,
	// the `RemoteCommitment` will be replaced with a newer version found
	// in `CommitDiff`. Thus we need to compute the output indexes here
	// before the change since the indexes are meant for the current,
	// revoked remote commitment.
	ourOutputIndex, theirOutputIndex, err := findOutputIndexesFromRemote(
		revocation, lc.channelState, lc.leafStore,
	)
	if err != nil {
		return nil, nil, err
	}

	// Now that we have a new verification nonce from them, we can refresh
	// our remote musig2 session which allows us to create another state.
	if lc.channelState.ChanType.IsTaproot() {
		localNonce, err := revMsg.LocalNonce.UnwrapOrErrV(errNoNonce)
		if err != nil {
			return nil, nil, err
		}

		session, err := lc.musigSessions.RemoteSession.Refresh(
			&musig2.Nonces{
				PubNonce: localNonce,
			},
		)
		if err != nil {
			return nil, nil, err
		}

		lc.musigSessions.RemoteSession = session
	}

	// At this point, the revocation has been accepted, and we've rotated
	// the current revocation key+hash for the remote party. Therefore we
	// sync now to ensure the revocation producer state is consistent with
	// the current commitment height and also to advance the on-disk
	// commitment chain.
	err = lc.channelState.AdvanceCommitChainTail(
		fwdPkg, localPeerUpdates,
		ourOutputIndex, theirOutputIndex,
	)
	if err != nil {
		return nil, nil, err
	}

	// Since they revoked the current lowest height in their commitment
	// chain, we can advance their chain by a single commitment.
	lc.commitChains.Remote.advanceTail()

	// As we've just completed a new state transition, attempt to see if we
	// can remove any entries from the update log which have been removed
	// from the PoV of both commitment chains.
	compactLogs(
		lc.updateLogs.Local, lc.updateLogs.Remote, localChainTail,
		remoteChainTail,
	)

	remoteHTLCs := lc.channelState.RemoteCommitment.Htlcs

	return fwdPkg, remoteHTLCs, nil
}

// LoadFwdPkgs loads any pending log updates from disk and returns the payment
// descriptors to be processed by the link.
func (lc *LightningChannel) LoadFwdPkgs() ([]*channeldb.FwdPkg, error) {
	return lc.channelState.LoadFwdPkgs()
}

// AckAddHtlcs sets a bit in the FwdFilter of a forwarding package belonging to
// this channel, that corresponds to the given AddRef. This method also succeeds
// if no forwarding package is found.
func (lc *LightningChannel) AckAddHtlcs(addRef channeldb.AddRef) error {
	return lc.channelState.AckAddHtlcs(addRef)
}

// AckSettleFails sets a bit in the SettleFailFilter of a forwarding package
// belonging to this channel, that corresponds to the given SettleFailRef. This
// method also succeeds if no forwarding package is found.
func (lc *LightningChannel) AckSettleFails(
	settleFailRefs ...channeldb.SettleFailRef) error {

	return lc.channelState.AckSettleFails(settleFailRefs...)
}

// SetFwdFilter writes the forwarding decision for a given remote commitment
// height.
func (lc *LightningChannel) SetFwdFilter(height uint64,
	fwdFilter *channeldb.PkgFilter) error {

	return lc.channelState.SetFwdFilter(height, fwdFilter)
}

// RemoveFwdPkgs permanently deletes the forwarding package at the given heights.
func (lc *LightningChannel) RemoveFwdPkgs(heights ...uint64) error {
	return lc.channelState.RemoveFwdPkgs(heights...)
}

// NextRevocationKey returns the commitment point for the _next_ commitment
// height. The pubkey returned by this function is required by the remote party
// along with their revocation base to extend our commitment chain with a
// new commitment.
func (lc *LightningChannel) NextRevocationKey() (*btcec.PublicKey, error) {
	lc.RLock()
	defer lc.RUnlock()

	nextHeight := lc.currentHeight + 1
	revocation, err := lc.channelState.RevocationProducer.AtIndex(nextHeight)
	if err != nil {
		return nil, err
	}

	return input.ComputeCommitmentPoint(revocation[:]), nil
}

// InitNextRevocation inserts the passed commitment point as the _next_
// revocation to be used when creating a new commitment state for the remote
// party. This function MUST be called before the channel can accept or propose
// any new states.
func (lc *LightningChannel) InitNextRevocation(revKey *btcec.PublicKey) error {
	lc.Lock()
	defer lc.Unlock()

	return lc.channelState.InsertNextRevocation(revKey)
}

// AddHTLC is a wrapper of the `addHTLC` function which always enforces the
// FeeBuffer on the local balance if being the initiator of the channel. This
// method should be called when preparing to send an outgoing HTLC.
//
// The additional openKey argument corresponds to the incoming CircuitKey of the
// committed circuit for this HTLC. This value should never be nil.
//
// NOTE: It is okay for sourceRef to be nil when unit testing the wallet.
func (lc *LightningChannel) AddHTLC(htlc *lnwire.UpdateAddHTLC,
	openKey *models.CircuitKey) (uint64, error) {

	return lc.addHTLC(htlc, openKey, FeeBuffer)
}

// addHTLC adds an HTLC to the state machine's local update log. It provides
// the ability to enforce a buffer on the local balance when we are the
// initiator of the channel. This is useful when checking the edge cases of a
// channel state e.g. the BOLT 03 test vectors.
//
// The additional openKey argument corresponds to the incoming CircuitKey of the
// committed circuit for this HTLC. This value should never be nil.
//
// NOTE: It is okay for sourceRef to be nil when unit testing the wallet.
func (lc *LightningChannel) addHTLC(htlc *lnwire.UpdateAddHTLC,
	openKey *models.CircuitKey, buffer BufferType) (uint64, error) {

	lc.Lock()
	defer lc.Unlock()

	pd := lc.htlcAddDescriptor(htlc, openKey)
	if err := lc.validateAddHtlc(pd, buffer); err != nil {
		return 0, err
	}

	lc.updateLogs.Local.appendHtlc(pd)

	return pd.HtlcIndex, nil
}

// GetDustSum takes in a boolean that determines which commitment to evaluate
// the dust sum on. The return value is the sum of dust on the desired
// commitment tx.
//
// NOTE: This over-estimates the dust exposure.
func (lc *LightningChannel) GetDustSum(whoseCommit lntypes.ChannelParty,
	dryRunFee fn.Option[chainfee.SatPerKWeight]) lnwire.MilliSatoshi {

	lc.RLock()
	defer lc.RUnlock()

	var dustSum lnwire.MilliSatoshi

	dustLimit := lc.channelState.LocalChanCfg.DustLimit
	commit := lc.channelState.LocalCommitment
	if whoseCommit.IsRemote() {
		// Calculate dust sum on the remote's commitment.
		dustLimit = lc.channelState.RemoteChanCfg.DustLimit
		commit = lc.channelState.RemoteCommitment
	}

	chanType := lc.channelState.ChanType
	feeRate := chainfee.SatPerKWeight(commit.FeePerKw)

	// Optionally use the dry-run fee-rate.
	feeRate = dryRunFee.UnwrapOr(feeRate)

	// Grab all of our HTLCs and evaluate against the dust limit.
	for e := lc.updateLogs.Local.Front(); e != nil; e = e.Next() {
		pd := e.Value
		if !pd.isAdd() {
			continue
		}

		amt := pd.Amount.ToSatoshis()

		// If the satoshi amount is under the dust limit, add the msat
		// amount to the dust sum.
		if HtlcIsDust(
			chanType, false, whoseCommit, feeRate, amt, dustLimit,
		) {

			dustSum += pd.Amount
		}
	}

	// Grab all of their HTLCs and evaluate against the dust limit.
	for e := lc.updateLogs.Remote.Front(); e != nil; e = e.Next() {
		pd := e.Value
		if !pd.isAdd() {
			continue
		}

		amt := pd.Amount.ToSatoshis()

		// If the satoshi amount is under the dust limit, add the msat
		// amount to the dust sum.
		if HtlcIsDust(
			chanType, true, whoseCommit, feeRate,
			amt, dustLimit,
		) {

			dustSum += pd.Amount
		}
	}

	return dustSum
}

// MayAddOutgoingHtlc validates whether we can add an outgoing htlc to this
// channel. We don't have a circuit for this htlc, because we just want to test
// that we have slots for a potential htlc so we use a "mock" htlc to validate
// a potential commitment state with one more outgoing htlc. If a zero htlc
// amount is provided, we'll attempt to add the smallest possible htlc to the
// channel (either the minimum htlc, or 1 sat).
func (lc *LightningChannel) MayAddOutgoingHtlc(amt lnwire.MilliSatoshi) error {
	lc.Lock()
	defer lc.Unlock()

	var mockHtlcAmt lnwire.MilliSatoshi
	switch {
	// If the caller specifically set an amount, we use it.
	case amt != 0:
		mockHtlcAmt = amt

	// In absence of a specific amount, we want to use minimum htlc value
	// for the channel. However certain implementations may set this value
	// to zero, so we only use this value if it is non-zero.
	case lc.channelState.LocalChanCfg.MinHTLC != 0:
		mockHtlcAmt = lc.channelState.LocalChanCfg.MinHTLC

	// As a last resort, we just add a non-zero amount.
	default:
		mockHtlcAmt++
	}

	// Create a "mock" outgoing htlc, using the smallest amount we can add
	// to the commitment so that we validate commitment slots rather than
	// available balance, since our actual htlc amount is unknown at this
	// stage.
	pd := lc.htlcAddDescriptor(
		&lnwire.UpdateAddHTLC{
			Amount: mockHtlcAmt,
		},
		&models.CircuitKey{},
	)

	// Enforce the FeeBuffer because we are evaluating whether we can add
	// another htlc to the channel state.
	if err := lc.validateAddHtlc(pd, FeeBuffer); err != nil {
		lc.log.Debugf("May add outgoing htlc rejected: %v", err)
		return err
	}

	return nil
}

// htlcAddDescriptor returns a payment descriptor for the htlc and open key
// provided to add to our local update log.
func (lc *LightningChannel) htlcAddDescriptor(htlc *lnwire.UpdateAddHTLC,
	openKey *models.CircuitKey) *paymentDescriptor {

	customRecords := htlc.CustomRecords.Copy()
	entryType := lc.entryTypeForHtlc(
		customRecords, lc.channelState.ChanType,
	)

	return &paymentDescriptor{
		ChanID:         htlc.ChanID,
		EntryType:      entryType,
		RHash:          PaymentHash(htlc.PaymentHash),
		Timeout:        htlc.Expiry,
		Amount:         htlc.Amount,
		LogIndex:       lc.updateLogs.Local.logIndex,
		HtlcIndex:      lc.updateLogs.Local.htlcCounter,
		OnionBlob:      htlc.OnionBlob,
		OpenCircuitKey: openKey,
		BlindingPoint:  htlc.BlindingPoint,
		CustomRecords:  customRecords,
	}
}

// validateAddHtlc validates the addition of an outgoing htlc to our local and
// remote commitments.
func (lc *LightningChannel) validateAddHtlc(pd *paymentDescriptor,
	buffer BufferType) error {
	// Make sure adding this HTLC won't violate any of the constraints we
	// must keep on the commitment transactions.
	remoteACKedIndex := lc.commitChains.Local.tail().messageIndices.Remote

	// First we'll check whether this HTLC can be added to the remote
	// commitment transaction without violation any of the constraints.
	err := lc.validateCommitmentSanity(
		remoteACKedIndex, lc.updateLogs.Local.logIndex, lntypes.Remote,
		buffer, pd, nil,
	)
	if err != nil {
		return err
	}

	// We must also check whether it can be added to our own commitment
	// transaction, or the remote node will refuse to sign. This is not
	// totally bullet proof, as the remote might be adding updates
	// concurrently, but if we fail this check there is for sure not
	// possible for us to add the HTLC.
	err = lc.validateCommitmentSanity(
		lc.updateLogs.Remote.logIndex, lc.updateLogs.Local.logIndex,
		lntypes.Local, buffer, pd, nil,
	)
	if err != nil {
		return err
	}

	return nil
}

// ReceiveHTLC adds an HTLC to the state machine's remote update log. This
// method should be called in response to receiving a new HTLC from the remote
// party.
func (lc *LightningChannel) ReceiveHTLC(htlc *lnwire.UpdateAddHTLC) (uint64,
	error) {

	lc.Lock()
	defer lc.Unlock()

	if htlc.ID != lc.updateLogs.Remote.htlcCounter {
		return 0, fmt.Errorf("ID %d on HTLC add does not match "+
			"expected next ID %d", htlc.ID,
			lc.updateLogs.Remote.htlcCounter)
	}

	customRecords := htlc.CustomRecords.Copy()
	entryType := lc.entryTypeForHtlc(
		customRecords, lc.channelState.ChanType,
	)

	pd := &paymentDescriptor{
		ChanID:        htlc.ChanID,
		EntryType:     entryType,
		RHash:         PaymentHash(htlc.PaymentHash),
		Timeout:       htlc.Expiry,
		Amount:        htlc.Amount,
		LogIndex:      lc.updateLogs.Remote.logIndex,
		HtlcIndex:     lc.updateLogs.Remote.htlcCounter,
		OnionBlob:     htlc.OnionBlob,
		BlindingPoint: htlc.BlindingPoint,
		CustomRecords: customRecords,
	}

	localACKedIndex := lc.commitChains.Remote.tail().messageIndices.Local

	// Clamp down on the number of HTLC's we can receive by checking the
	// commitment sanity.
	// We do not enforce the FeeBuffer here because one of the reasons it
	// was introduced is to protect against asynchronous sending of htlcs so
	// we use it here. The current lightning protocol does not allow to
	// reject ADDs already sent by the peer.
	err := lc.validateCommitmentSanity(
		lc.updateLogs.Remote.logIndex, localACKedIndex, lntypes.Local,
		NoBuffer, nil, pd,
	)
	if err != nil {
		return 0, err
	}

	lc.updateLogs.Remote.appendHtlc(pd)

	return pd.HtlcIndex, nil
}

// SettleHTLC attempts to settle an existing outstanding received HTLC. The
// remote log index of the HTLC settled is returned in order to facilitate
// creating the corresponding wire message. In the case the supplied preimage
// is invalid, an error is returned.
//
// The additional arguments correspond to:
//
//   - sourceRef: specifies the location of the Add HTLC within a forwarding
//     package that this HTLC is settling. Every Settle fails exactly one Add,
//     so this should never be empty in practice.
//
//   - destRef: specifies the location of the Settle HTLC within another
//     channel's forwarding package. This value can be nil if the corresponding
//     Add HTLC was never locked into an outgoing commitment txn, or this
//     HTLC does not originate as a response from the peer on the outgoing
//     link, e.g. on-chain resolutions.
//
//   - closeKey: identifies the circuit that should be deleted after this Settle
//     HTLC is included in a commitment txn. This value should only be nil if
//     the HTLC was settled locally before committing a circuit to the circuit
//     map.
//
// NOTE: It is okay for sourceRef, destRef, and closeKey to be nil when unit
// testing the wallet.
func (lc *LightningChannel) SettleHTLC(preimage [32]byte,
	htlcIndex uint64, sourceRef *channeldb.AddRef,
	destRef *channeldb.SettleFailRef, closeKey *models.CircuitKey) error {

	lc.Lock()
	defer lc.Unlock()

	htlc := lc.updateLogs.Remote.lookupHtlc(htlcIndex)
	if htlc == nil {
		return ErrUnknownHtlcIndex{lc.ShortChanID(), htlcIndex}
	}

	// Now that we know the HTLC exists, before checking to see if the
	// preimage matches, we'll ensure that we haven't already attempted to
	// modify the HTLC.
	if lc.updateLogs.Remote.htlcHasModification(htlcIndex) {
		return ErrHtlcIndexAlreadySettled(htlcIndex)
	}

	if htlc.RHash != sha256.Sum256(preimage[:]) {
		return ErrInvalidSettlePreimage{preimage[:], htlc.RHash[:]}
	}

	pd := &paymentDescriptor{
		ChanID:           lc.ChannelID(),
		Amount:           htlc.Amount,
		RPreimage:        preimage,
		LogIndex:         lc.updateLogs.Local.logIndex,
		ParentIndex:      htlcIndex,
		EntryType:        Settle,
		SourceRef:        sourceRef,
		DestRef:          destRef,
		ClosedCircuitKey: closeKey,
	}

	lc.updateLogs.Local.appendUpdate(pd)

	// With the settle added to our local log, we'll now mark the HTLC as
	// modified to prevent ourselves from accidentally attempting a
	// duplicate settle.
	lc.updateLogs.Remote.markHtlcModified(htlcIndex)

	return nil
}

// ReceiveHTLCSettle attempts to settle an existing outgoing HTLC indexed by an
// index into the local log. If the specified index doesn't exist within the
// log, and error is returned. Similarly if the preimage is invalid w.r.t to
// the referenced of then a distinct error is returned.
func (lc *LightningChannel) ReceiveHTLCSettle(preimage [32]byte, htlcIndex uint64) error {
	lc.Lock()
	defer lc.Unlock()

	htlc := lc.updateLogs.Local.lookupHtlc(htlcIndex)
	if htlc == nil {
		return ErrUnknownHtlcIndex{lc.ShortChanID(), htlcIndex}
	}

	// Now that we know the HTLC exists, before checking to see if the
	// preimage matches, we'll ensure that they haven't already attempted
	// to modify the HTLC.
	if lc.updateLogs.Local.htlcHasModification(htlcIndex) {
		return ErrHtlcIndexAlreadySettled(htlcIndex)
	}

	if htlc.RHash != sha256.Sum256(preimage[:]) {
		return ErrInvalidSettlePreimage{preimage[:], htlc.RHash[:]}
	}

	pd := &paymentDescriptor{
		ChanID:      lc.ChannelID(),
		Amount:      htlc.Amount,
		RPreimage:   preimage,
		ParentIndex: htlc.HtlcIndex,
		RHash:       htlc.RHash,
		LogIndex:    lc.updateLogs.Remote.logIndex,
		EntryType:   Settle,
	}

	lc.updateLogs.Remote.appendUpdate(pd)

	// With the settle added to the remote log, we'll now mark the HTLC as
	// modified to prevent the remote party from accidentally attempting a
	// duplicate settle.
	lc.updateLogs.Local.markHtlcModified(htlcIndex)

	return nil
}

// FailHTLC attempts to fail a targeted HTLC by its payment hash, inserting an
// entry which will remove the target log entry within the next commitment
// update. This method is intended to be called in order to cancel in
// _incoming_ HTLC.
//
// The additional arguments correspond to:
//
//   - sourceRef: specifies the location of the Add HTLC within a forwarding
//     package that this HTLC is failing. Every Fail fails exactly one Add, so
//     this should never be empty in practice.
//
//   - destRef: specifies the location of the Fail HTLC within another channel's
//     forwarding package. This value can be nil if the corresponding Add HTLC
//     was never locked into an outgoing commitment txn, or this HTLC does not
//     originate as a response from the peer on the outgoing link, e.g.
//     on-chain resolutions.
//
//   - closeKey: identifies the circuit that should be deleted after this Fail
//     HTLC is included in a commitment txn. This value should only be nil if
//     the HTLC was failed locally before committing a circuit to the circuit
//     map.
//
// NOTE: It is okay for sourceRef, destRef, and closeKey to be nil when unit
// testing the wallet.
func (lc *LightningChannel) FailHTLC(htlcIndex uint64, reason []byte,
	sourceRef *channeldb.AddRef, destRef *channeldb.SettleFailRef,
	closeKey *models.CircuitKey) error {

	lc.Lock()
	defer lc.Unlock()

	htlc := lc.updateLogs.Remote.lookupHtlc(htlcIndex)
	if htlc == nil {
		return ErrUnknownHtlcIndex{lc.ShortChanID(), htlcIndex}
	}

	// Now that we know the HTLC exists, we'll ensure that we haven't
	// already attempted to fail the HTLC.
	if lc.updateLogs.Remote.htlcHasModification(htlcIndex) {
		return ErrHtlcIndexAlreadyFailed(htlcIndex)
	}

	pd := &paymentDescriptor{
		ChanID:           lc.ChannelID(),
		Amount:           htlc.Amount,
		RHash:            htlc.RHash,
		ParentIndex:      htlcIndex,
		LogIndex:         lc.updateLogs.Local.logIndex,
		EntryType:        Fail,
		FailReason:       reason,
		SourceRef:        sourceRef,
		DestRef:          destRef,
		ClosedCircuitKey: closeKey,
	}

	lc.updateLogs.Local.appendUpdate(pd)

	// With the fail added to the remote log, we'll now mark the HTLC as
	// modified to prevent ourselves from accidentally attempting a
	// duplicate fail.
	lc.updateLogs.Remote.markHtlcModified(htlcIndex)

	return nil
}

// MalformedFailHTLC attempts to fail a targeted HTLC by its payment hash,
// inserting an entry which will remove the target log entry within the next
// commitment update. This method is intended to be called in order to cancel
// in _incoming_ HTLC.
//
// The additional sourceRef specifies the location of the Add HTLC within a
// forwarding package that this HTLC is failing. This value should never be
// empty.
//
// NOTE: It is okay for sourceRef to be nil when unit testing the wallet.
func (lc *LightningChannel) MalformedFailHTLC(htlcIndex uint64,
	failCode lnwire.FailCode, shaOnionBlob [sha256.Size]byte,
	sourceRef *channeldb.AddRef) error {

	lc.Lock()
	defer lc.Unlock()

	htlc := lc.updateLogs.Remote.lookupHtlc(htlcIndex)
	if htlc == nil {
		return ErrUnknownHtlcIndex{lc.ShortChanID(), htlcIndex}
	}

	// Now that we know the HTLC exists, we'll ensure that we haven't
	// already attempted to fail the HTLC.
	if lc.updateLogs.Remote.htlcHasModification(htlcIndex) {
		return ErrHtlcIndexAlreadyFailed(htlcIndex)
	}

	pd := &paymentDescriptor{
		ChanID:       lc.ChannelID(),
		Amount:       htlc.Amount,
		RHash:        htlc.RHash,
		ParentIndex:  htlcIndex,
		LogIndex:     lc.updateLogs.Local.logIndex,
		EntryType:    MalformedFail,
		FailCode:     failCode,
		ShaOnionBlob: shaOnionBlob,
		SourceRef:    sourceRef,
	}

	lc.updateLogs.Local.appendUpdate(pd)

	// With the fail added to the remote log, we'll now mark the HTLC as
	// modified to prevent ourselves from accidentally attempting a
	// duplicate fail.
	lc.updateLogs.Remote.markHtlcModified(htlcIndex)

	return nil
}

// ReceiveFailHTLC attempts to cancel a targeted HTLC by its log index,
// inserting an entry which will remove the target log entry within the next
// commitment update. This method should be called in response to the upstream
// party cancelling an outgoing HTLC.
func (lc *LightningChannel) ReceiveFailHTLC(htlcIndex uint64, reason []byte,
) error {

	lc.Lock()
	defer lc.Unlock()

	htlc := lc.updateLogs.Local.lookupHtlc(htlcIndex)
	if htlc == nil {
		return ErrUnknownHtlcIndex{lc.ShortChanID(), htlcIndex}
	}

	// Now that we know the HTLC exists, we'll ensure that they haven't
	// already attempted to fail the HTLC.
	if lc.updateLogs.Local.htlcHasModification(htlcIndex) {
		return ErrHtlcIndexAlreadyFailed(htlcIndex)
	}

	pd := &paymentDescriptor{
		ChanID:      lc.ChannelID(),
		Amount:      htlc.Amount,
		RHash:       htlc.RHash,
		ParentIndex: htlc.HtlcIndex,
		LogIndex:    lc.updateLogs.Remote.logIndex,
		EntryType:   Fail,
		FailReason:  reason,
	}

	lc.updateLogs.Remote.appendUpdate(pd)

	// With the fail added to the remote log, we'll now mark the HTLC as
	// modified to prevent ourselves from accidentally attempting a
	// duplicate fail.
	lc.updateLogs.Local.markHtlcModified(htlcIndex)

	return nil
}

// ChannelPoint returns the outpoint of the original funding transaction which
// created this active channel. This outpoint is used throughout various
// subsystems to uniquely identify an open channel.
func (lc *LightningChannel) ChannelPoint() wire.OutPoint {
	return lc.channelState.FundingOutpoint
}

// ChannelID returns the ChannelID of this LightningChannel. This is the same
// ChannelID that is used in update messages for this channel.
func (lc *LightningChannel) ChannelID() lnwire.ChannelID {
	return lnwire.NewChanIDFromOutPoint(lc.ChannelPoint())
}

// ShortChanID returns the short channel ID for the channel. The short channel
// ID encodes the exact location in the main chain that the original
// funding output can be found.
func (lc *LightningChannel) ShortChanID() lnwire.ShortChannelID {
	return lc.channelState.ShortChanID()
}

// LocalUpfrontShutdownScript returns the local upfront shutdown script for the
// channel. If it was not set, an empty byte array is returned.
func (lc *LightningChannel) LocalUpfrontShutdownScript() lnwire.DeliveryAddress {
	return lc.channelState.LocalShutdownScript
}

// RemoteUpfrontShutdownScript returns the remote upfront shutdown script for the
// channel. If it was not set, an empty byte array is returned.
func (lc *LightningChannel) RemoteUpfrontShutdownScript() lnwire.DeliveryAddress {
	return lc.channelState.RemoteShutdownScript
}

// AbsoluteThawHeight determines a frozen channel's absolute thaw height. If
// the channel is not frozen, then 0 is returned.
//
// An error is returned if the channel is pending, or is an unconfirmed zero
// conf channel.
func (lc *LightningChannel) AbsoluteThawHeight() (uint32, error) {
	return lc.channelState.AbsoluteThawHeight()
}

// SignedCommitTxInputs contains data needed to create a signed commit
// transaction using a signer. See GetSignedCommitTx.
type SignedCommitTxInputs struct {
	// CommitTx is the latest version of the commitment state, broadcast
	// able by us.
	CommitTx *wire.MsgTx

	// CommitSig is one half of the signature required to fully complete
	// the script for the commitment transaction above. This is the
	// signature signed by the remote party for our version of the
	// commitment transactions.
	CommitSig []byte

	// OurKey is our key to be used within the 2-of-2 output script
	// for the owner of this channel.
	OurKey keychain.KeyDescriptor

	// TheirKey is their key to be used within the 2-of-2 output script
	// for the owner of this channel.
	TheirKey keychain.KeyDescriptor

	// SignDesc is the primary sign descriptor that is capable of signing
	// the commitment transaction that spends the multi-sig output.
	SignDesc *input.SignDescriptor

	// Taproot holds fields needed in case of a taproot channel.
	// Iff the channel is of taproot type, this field is filled.
	Taproot fn.Option[TaprootSignedCommitTxInputs]
}

// TaprootSignedCommitTxInputs contains additional data needed to create a
// signed commit transaction using a signer, used in case of a taproot channel.
// See GetSignedCommitTx.
type TaprootSignedCommitTxInputs struct {
	// CommitHeight is the update number that this channel state represents.
	// It is the total number of commitment updates up to this point. This
	// can be viewed as sort of a "commitment height" as this number is
	// monotonically increasing. This number is used to make a signature
	// for a taproot channel, since it is used by shachain nonce producer
	// (TaprootNonceProducer).
	CommitHeight uint64

	// TaprootNonceProducer is used to generate a shachain tree for the
	// purpose of generating verification nonces for taproot channels.
	TaprootNonceProducer shachain.Producer

	// TapscriptRoot is the root of the tapscript tree that will be used to
	// create the funding output. This is an optional field that should
	// only be set for taproot channels.
	TapscriptRoot fn.Option[chainhash.Hash]
}

// GetSignedCommitTx creates the witness stack of a channel commitment
// transaction. It can handle all commitment types (taproot, legacy). It is
// exported to give outside tooling the possibility to recreate the witness.
// A key use case is generating the witness data for a commitment transaction
// from a Static Channel Backup (SCB).
func GetSignedCommitTx(inputs SignedCommitTxInputs,
	signer input.Signer) (*wire.MsgTx, error) {

	commitTx := inputs.CommitTx.Copy()

	var witness wire.TxWitness
	switch {
	// If this is a taproot channel, then we'll need to re-derive the nonce
	// we need to generate a new signature
	case inputs.Taproot.IsSome():
		// Extract Taproot from fn.Option. It is safe to call
		// UnsafeFromSome because we just checked that it is some.
		taproot := inputs.Taproot.UnsafeFromSome()

		// First, we'll need to re-derive the local nonce we sent to
		// the remote party to create this musig session. We pass in
		// the same height here as we're generating the nonce needed
		// for the _current_ state.
		localNonce, err := channeldb.NewMusigVerificationNonce(
			inputs.OurKey.PubKey, taproot.CommitHeight,
			taproot.TaprootNonceProducer,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to re-derive "+
				"verification nonce: %w", err)
		}

		tapscriptTweak := fn.MapOption(TapscriptRootToTweak)(
			taproot.TapscriptRoot,
		)

		// Now that we have the local nonce, we'll re-create the musig
		// session we had for this height.
		musigSession := NewPartialMusigSession(
			*localNonce, inputs.OurKey, inputs.TheirKey, signer,
			inputs.SignDesc.Output, LocalMusigCommit,
			tapscriptTweak,
		)

		var remoteSig lnwire.PartialSigWithNonce
		err = remoteSig.Decode(
			bytes.NewReader(inputs.CommitSig),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to decode remote "+
				"partial sig: %w", err)
		}

		// Next, we'll manually finalize the session with the signing
		// nonce we got from the remote party which is embedded in the
		// signature we have.
		err = musigSession.FinalizeSession(musig2.Nonces{
			PubNonce: remoteSig.Nonce,
		})
		if err != nil {
			return nil, fmt.Errorf("unable to finalize musig "+
				"session: %w", err)
		}

		// Now that the session has been finalized, we can generate our
		// half of the signature for the state. We don't capture the
		// sig as it's stored within the session.
		if _, err := musigSession.SignCommit(commitTx); err != nil {
			return nil, fmt.Errorf("unable to sign musig2 "+
				"commitment: %w", err)
		}

		// The final step is now to combine this signature we generated
		// above, with the remote party's signature. We only need to
		// pass the remote sig, as the local sig was already cached in
		// the session.
		var partialSig MusigPartialSig
		partialSig.FromWireSig(&remoteSig)
		finalSig, err := musigSession.CombineSigs(partialSig.sig)
		if err != nil {
			return nil, fmt.Errorf("unable to combine musig "+
				"partial sigs: %w", err)
		}

		// The witness is the single keyspend schnorr sig.
		witness = wire.TxWitness{
			finalSig.Serialize(),
		}

	// Otherwise, the final witness we generate will be a normal p2wsh
	// multi-sig spend.
	default:
		theirSig, err := ecdsa.ParseDERSignature(inputs.CommitSig)
		if err != nil {
			return nil, err
		}

		// With this, we then generate the full witness so the caller
		// can broadcast a fully signed transaction.
		inputs.SignDesc.SigHashes = input.NewTxSigHashesV0Only(commitTx)
		ourSig, err := signer.SignOutputRaw(commitTx, inputs.SignDesc)
		if err != nil {
			return nil, err
		}

		// With the final signature generated, create the witness stack
		// required to spend from the multi-sig output.
		witness = input.SpendMultiSig(
			inputs.SignDesc.WitnessScript,
			inputs.OurKey.PubKey.SerializeCompressed(), ourSig,
			inputs.TheirKey.PubKey.SerializeCompressed(), theirSig,
		)
	}

	commitTx.TxIn[0].Witness = witness

	return commitTx, nil
}

// getSignedCommitTx method takes the latest commitment transaction and
// populates it with witness data.
func (lc *LightningChannel) getSignedCommitTx() (*wire.MsgTx, error) {
	// Fetch the current commitment transaction, along with their signature
	// for the transaction.
	localCommit := lc.channelState.LocalCommitment

	inputs := SignedCommitTxInputs{
		CommitTx:  localCommit.CommitTx,
		CommitSig: localCommit.CommitSig,
		OurKey:    lc.channelState.LocalChanCfg.MultiSigKey,
		TheirKey:  lc.channelState.RemoteChanCfg.MultiSigKey,
		SignDesc:  lc.signDesc,
	}

	if lc.channelState.ChanType.IsTaproot() {
		inputs.Taproot = fn.Some(TaprootSignedCommitTxInputs{
			CommitHeight:         lc.currentHeight,
			TaprootNonceProducer: lc.taprootNonceProducer,
			TapscriptRoot:        lc.channelState.TapscriptRoot,
		})
	}

	return GetSignedCommitTx(inputs, lc.Signer)
}

// CommitOutputResolution carries the necessary information required to allow
// us to sweep our commitment output in the case that either party goes to
// chain.
type CommitOutputResolution struct {
	// SelfOutPoint is the full outpoint that points to out pay-to-self
	// output within the closing commitment transaction.
	SelfOutPoint wire.OutPoint

	// SelfOutputSignDesc is a fully populated sign descriptor capable of
	// generating a valid signature to sweep the output paying to us.
	SelfOutputSignDesc input.SignDescriptor

	// MaturityDelay is the relative time-lock, in blocks for all outputs
	// that pay to the local party within the broadcast commitment
	// transaction.
	MaturityDelay uint32

	// ResolutionBlob is a blob used for aux channels that permits a
	// spender of the output to properly resolve it in the case of a force
	// close.
	ResolutionBlob fn.Option[tlv.Blob]
}

// UnilateralCloseSummary describes the details of a detected unilateral
// channel closure. This includes the information about with which
// transactions, and block the channel was unilaterally closed, as well as
// summarization details concerning the _state_ of the channel at the point of
// channel closure. Additionally, if we had a commitment output above dust on
// the remote party's commitment transaction, the necessary a SignDescriptor
// with the material necessary to seep the output are returned. Finally, if we
// had any outgoing HTLC's within the commitment transaction, then an
// OutgoingHtlcResolution for each output will included.
type UnilateralCloseSummary struct {
	// SpendDetail is a struct that describes how and when the funding
	// output was spent.
	*chainntnfs.SpendDetail

	// ChannelCloseSummary is a struct describing the final state of the
	// channel and in which state is was closed.
	channeldb.ChannelCloseSummary

	// CommitResolution contains all the data required to sweep the output
	// to ourselves. If this is our commitment transaction, then we'll need
	// to wait a time delay before we can sweep the output.
	//
	// NOTE: If our commitment delivery output is below the dust limit,
	// then this will be nil.
	CommitResolution *CommitOutputResolution

	// HtlcResolutions contains a fully populated HtlcResolutions struct
	// which contains all the data required to sweep any outgoing HTLC's,
	// and also any incoming HTLC's that we know the pre-image to.
	HtlcResolutions *HtlcResolutions

	// RemoteCommit is the exact commitment state that the remote party
	// broadcast.
	RemoteCommit channeldb.ChannelCommitment

	// AnchorResolution contains the data required to sweep our anchor
	// output. If the channel type doesn't include anchors, the value of
	// this field will be nil.
	AnchorResolution *AnchorResolution
}

// NewUnilateralCloseSummary creates a new summary that provides the caller
// with all the information required to claim all funds on chain in the event
// that the remote party broadcasts their commitment. The commitPoint argument
// should be set to the per_commitment_point corresponding to the spending
// commitment.
//
// NOTE: The remoteCommit argument should be set to the stored commitment for
// this particular state. If we don't have the commitment stored (should only
// happen in case we have lost state) it should be set to an empty struct, in
// which case we will attempt to sweep the non-HTLC output using the passed
// commitPoint.
func NewUnilateralCloseSummary(chanState *channeldb.OpenChannel, //nolint:funlen
	signer input.Signer, commitSpend *chainntnfs.SpendDetail,
	remoteCommit channeldb.ChannelCommitment, commitPoint *btcec.PublicKey,
	leafStore fn.Option[AuxLeafStore],
	auxResolver fn.Option[AuxContractResolver]) (*UnilateralCloseSummary,
	error) {

	// First, we'll generate the commitment point and the revocation point
	// so we can re-construct the HTLC state and also our payment key.
	commitType := lntypes.Remote
	commitTxHeight := uint32(commitSpend.SpendingHeight)
	keyRing := DeriveCommitmentKeys(
		commitPoint, commitType, chanState.ChanType,
		&chanState.LocalChanCfg, &chanState.RemoteChanCfg,
	)

	auxResult, err := fn.MapOptionZ(
		leafStore, func(s AuxLeafStore) fn.Result[CommitDiffAuxResult] {
			return s.FetchLeavesFromCommit(
				NewAuxChanState(chanState), remoteCommit,
				*keyRing, lntypes.Remote,
			)
		},
	).Unpack()
	if err != nil {
		return nil, fmt.Errorf("unable to fetch aux leaves: %w", err)
	}

	// Next, we'll obtain HTLC resolutions for all the outgoing HTLC's we
	// had on their commitment transaction.
	var (
		leaseExpiry       uint32
		selfPoint         *wire.OutPoint
		localBalance      int64
		isRemoteInitiator = !chanState.IsInitiator
		commitTxBroadcast = commitSpend.SpendingTx
	)

	if chanState.ChanType.HasLeaseExpiration() {
		leaseExpiry = chanState.ThawHeight
	}
	htlcResolutions, err := extractHtlcResolutions(
		chainfee.SatPerKWeight(remoteCommit.FeePerKw), commitType,
		signer, remoteCommit.Htlcs, keyRing, &chanState.LocalChanCfg,
		&chanState.RemoteChanCfg, commitSpend.SpendingTx,
		commitTxHeight, chanState.ChanType,
		isRemoteInitiator, leaseExpiry, chanState, auxResult.AuxLeaves,
		auxResolver,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create htlc resolutions: %w",
			err)
	}

	// Before we can generate the proper sign descriptor, we'll need to
	// locate the output index of our non-delayed output on the commitment
	// transaction.
	remoteAuxLeaf := fn.FlatMapOption(
		func(l CommitAuxLeaves) input.AuxTapLeaf {
			return l.RemoteAuxLeaf
		},
	)(auxResult.AuxLeaves)
	selfScript, maturityDelay, err := CommitScriptToRemote(
		chanState.ChanType, isRemoteInitiator, keyRing.ToRemoteKey,
		leaseExpiry, remoteAuxLeaf,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create self commit "+
			"script: %w", err)
	}
	for outputIndex, txOut := range commitTxBroadcast.TxOut {
		if bytes.Equal(txOut.PkScript, selfScript.PkScript()) {
			selfPoint = &wire.OutPoint{
				Hash:  *commitSpend.SpenderTxHash,
				Index: uint32(outputIndex),
			}
			localBalance = txOut.Value
			break
		}
	}

	// With the HTLC's taken care of, we'll generate the sign descriptor
	// necessary to sweep our commitment output, but only if we had a
	// non-trimmed balance.
	var commitResolution *CommitOutputResolution
	if selfPoint != nil {
		localPayBase := chanState.LocalChanCfg.PaymentBasePoint

		// As the remote party has force closed, we just need the
		// success witness script.
		witnessScript, err := selfScript.WitnessScriptForPath(
			input.ScriptPathSuccess,
		)
		if err != nil {
			return nil, err
		}

		commitResolution = &CommitOutputResolution{
			SelfOutPoint: *selfPoint,
			SelfOutputSignDesc: input.SignDescriptor{
				KeyDesc:       localPayBase,
				SingleTweak:   keyRing.LocalCommitKeyTweak,
				WitnessScript: witnessScript,
				Output: &wire.TxOut{
					Value:    localBalance,
					PkScript: selfScript.PkScript(),
				},
				HashType: sweepSigHash(chanState.ChanType),
			},
			MaturityDelay: maturityDelay,
		}

		// For taproot channels, we'll need to set some additional
		// fields to ensure the output can be swept.
		//
		//nolint:ll
		if scriptTree, ok := selfScript.(input.TapscriptDescriptor); ok {
			commitResolution.SelfOutputSignDesc.SignMethod =
				input.TaprootScriptSpendSignMethod

			ctrlBlock, err := scriptTree.CtrlBlockForPath(
				input.ScriptPathSuccess,
			)
			if err != nil {
				return nil, err
			}
			//nolint:ll
			commitResolution.SelfOutputSignDesc.ControlBlock, err = ctrlBlock.ToBytes()
			if err != nil {
				return nil, err
			}
		}

		// At this point, we'll check to see if we need any extra
		// resolution data for this output.
		//
		//nolint:ll
		resolveReq := ResolutionReq{
			ChanPoint:           chanState.FundingOutpoint,
			ChanType:            chanState.ChanType,
			ShortChanID:         chanState.ShortChanID(),
			Initiator:           chanState.IsInitiator,
			CommitBlob:          chanState.RemoteCommitment.CustomBlob,
			FundingBlob:         chanState.CustomBlob,
			Type:                input.TaprootRemoteCommitSpend,
			CloseType:           RemoteForceClose,
			CommitTx:            commitTxBroadcast,
			CommitTxBlockHeight: commitTxHeight,
			ContractPoint:       *selfPoint,
			SignDesc:            commitResolution.SelfOutputSignDesc,
			KeyRing:             keyRing,
			CsvDelay:            maturityDelay,
			CommitFee:           chanState.RemoteCommitment.CommitFee,
		}
		resolveBlob := fn.MapOptionZ(
			auxResolver,
			func(a AuxContractResolver) fn.Result[tlv.Blob] {
				return a.ResolveContract(resolveReq)
			},
		)
		if err := resolveBlob.Err(); err != nil {
			return nil, fmt.Errorf("unable to aux resolve: %w", err)
		}

		commitResolution.ResolutionBlob = resolveBlob.OkToSome()
	}

	closeSummary := channeldb.ChannelCloseSummary{
		ChanPoint:               chanState.FundingOutpoint,
		ChainHash:               chanState.ChainHash,
		ClosingTXID:             *commitSpend.SpenderTxHash,
		CloseHeight:             uint32(commitSpend.SpendingHeight),
		RemotePub:               chanState.IdentityPub,
		Capacity:                chanState.Capacity,
		SettledBalance:          btcutil.Amount(localBalance),
		CloseType:               channeldb.RemoteForceClose,
		IsPending:               true,
		RemoteCurrentRevocation: chanState.RemoteCurrentRevocation,
		RemoteNextRevocation:    chanState.RemoteNextRevocation,
		ShortChanID:             chanState.ShortChanID(),
		LocalChanConfig:         chanState.LocalChanCfg,
	}

	// Attempt to add a channel sync message to the close summary.
	chanSync, err := chanState.ChanSyncMsg()
	if err != nil {
		walletLog.Errorf("ChannelPoint(%v): unable to create channel sync "+
			"message: %v", chanState.FundingOutpoint, err)
	} else {
		closeSummary.LastChanSyncMsg = chanSync
	}

	anchorResolution, err := NewAnchorResolution(
		chanState, commitTxBroadcast, keyRing, lntypes.Remote,
	)
	if err != nil {
		return nil, err
	}

	return &UnilateralCloseSummary{
		SpendDetail:         commitSpend,
		ChannelCloseSummary: closeSummary,
		CommitResolution:    commitResolution,
		HtlcResolutions:     htlcResolutions,
		RemoteCommit:        remoteCommit,
		AnchorResolution:    anchorResolution,
	}, nil
}

// IncomingHtlcResolution houses the information required to sweep any incoming
// HTLC's that we know the preimage to. We'll need to sweep an HTLC manually
// using this struct if we need to go on-chain for any reason, or if we detect
// that the remote party broadcasts their commitment transaction.
type IncomingHtlcResolution struct {
	// Preimage is the preimage that will be used to satisfy the contract of
	// the HTLC.
	//
	// NOTE: This field will only be populated in the incoming contest
	// resolver.
	Preimage [32]byte

	// SignedSuccessTx is the fully signed HTLC success transaction. This
	// transaction (if non-nil) can be broadcast immediately. After a csv
	// delay (included below), then the output created by this transactions
	// can be swept on-chain.
	//
	// NOTE: If this field is nil, then this indicates that we don't need
	// to go to the second level to claim this HTLC. Instead, it can be
	// claimed directly from the outpoint listed below.
	SignedSuccessTx *wire.MsgTx

	// SignDetails is non-nil if SignedSuccessTx is non-nil, and the
	// channel is of the anchor type. As the above HTLC transaction will be
	// signed by the channel peer using SINGLE|ANYONECANPAY for such
	// channels, we can use the sign details to add the input-output pair
	// of the HTLC transaction to another transaction, thereby aggregating
	// multiple HTLC transactions together, and adding fees as needed.
	SignDetails *input.SignDetails

	// CsvDelay is the relative time lock (expressed in blocks) that must
	// pass after the SignedSuccessTx is confirmed in the chain before the
	// output can be swept.
	//
	// NOTE: If SignedTimeoutTx is nil, then this field denotes the CSV
	// delay needed to spend from the commitment transaction.
	CsvDelay uint32

	// ClaimOutpoint is the final outpoint that needs to be spent in order
	// to fully sweep the HTLC. The SignDescriptor below should be used to
	// spend this outpoint. In the case of a second-level HTLC (non-nil
	// SignedTimeoutTx), then we'll be spending a new transaction.
	// Otherwise, it'll be an output in the commitment transaction.
	ClaimOutpoint wire.OutPoint

	// SweepSignDesc is a sign descriptor that has been populated with the
	// necessary items required to spend the sole output of the above
	// transaction.
	SweepSignDesc input.SignDescriptor

	// ResolutionBlob is a blob used for aux channels that permits a
	// spender of the output to properly resolve it in the case of a force
	// close.
	ResolutionBlob fn.Option[tlv.Blob]
}

// OutgoingHtlcResolution houses the information necessary to sweep any
// outgoing HTLC's after their contract has expired. This struct will be needed
// in one of two cases: the local party force closes the commitment transaction
// or the remote party unilaterally closes with their version of the commitment
// transaction.
type OutgoingHtlcResolution struct {
	// Expiry the absolute timeout of the HTLC. This value is expressed in
	// block height, meaning after this height the HLTC can be swept.
	Expiry uint32

	// SignedTimeoutTx is the fully signed HTLC timeout transaction. This
	// must be broadcast immediately after timeout has passed. Once this
	// has been confirmed, the HTLC output will transition into the
	// delay+claim state.
	//
	// NOTE: If this field is nil, then this indicates that we don't need
	// to go to the second level to claim this HTLC. Instead, it can be
	// claimed directly from the outpoint listed below.
	SignedTimeoutTx *wire.MsgTx

	// SignDetails is non-nil if SignedTimeoutTx is non-nil, and the
	// channel is of the anchor type. As the above HTLC transaction will be
	// signed by the channel peer using SINGLE|ANYONECANPAY for such
	// channels, we can use the sign details to add the input-output pair
	// of the HTLC transaction to another transaction, thereby aggregating
	// multiple HTLC transactions together, and adding fees as needed.
	SignDetails *input.SignDetails

	// CsvDelay is the relative time lock (expressed in blocks) that must
	// pass after the SignedTimeoutTx is confirmed in the chain before the
	// output can be swept.
	//
	// NOTE: If SignedTimeoutTx is nil, then this field denotes the CSV
	// delay needed to spend from the commitment transaction.
	CsvDelay uint32

	// ClaimOutpoint is the final outpoint that needs to be spent in order
	// to fully sweep the HTLC. The SignDescriptor below should be used to
	// spend this outpoint. In the case of a second-level HTLC (non-nil
	// SignedTimeoutTx), then we'll be spending a new transaction.
	// Otherwise, it'll be an output in the commitment transaction.
	ClaimOutpoint wire.OutPoint

	// SweepSignDesc is a sign descriptor that has been populated with the
	// necessary items required to spend the sole output of the above
	// transaction.
	SweepSignDesc input.SignDescriptor

	// ResolutionBlob is a blob used for aux channels that permits a
	// spender of the output to properly resolve it in the case of a force
	// close.
	ResolutionBlob fn.Option[tlv.Blob]
}

// HtlcResolutions contains the items necessary to sweep HTLC's on chain
// directly from a commitment transaction. We'll use this in case either party
// goes broadcasts a commitment transaction with live HTLC's.
type HtlcResolutions struct {
	// IncomingHTLCs contains a set of structs that can be used to sweep
	// all the incoming HTL'C that we know the preimage to.
	IncomingHTLCs []IncomingHtlcResolution

	// OutgoingHTLCs contains a set of structs that contains all the info
	// needed to sweep an outgoing HTLC we've sent to the remote party
	// after an absolute delay has expired.
	OutgoingHTLCs []OutgoingHtlcResolution
}

// newOutgoingHtlcResolution generates a new HTLC resolution capable of
// allowing the caller to sweep an outgoing HTLC present on either their, or
// the remote party's commitment transaction.
func newOutgoingHtlcResolution(signer input.Signer,
	localChanCfg *channeldb.ChannelConfig, commitTx *wire.MsgTx,
	commitTxHeight uint32, htlc *channeldb.HTLC, keyRing *CommitmentKeyRing,
	feePerKw chainfee.SatPerKWeight, csvDelay, leaseExpiry uint32,
	whoseCommit lntypes.ChannelParty, isCommitFromInitiator bool,
	chanType channeldb.ChannelType, chanState *channeldb.OpenChannel,
	auxLeaves fn.Option[CommitAuxLeaves],
	auxResolver fn.Option[AuxContractResolver],
) (*OutgoingHtlcResolution, error) {

	op := wire.OutPoint{
		Hash:  commitTx.TxHash(),
		Index: uint32(htlc.OutputIndex),
	}

	// First, we'll re-generate the script used to send the HTLC to the
	// remote party within their commitment transaction.
	auxLeaf := fn.FlatMapOption(func(l CommitAuxLeaves) input.AuxTapLeaf {
		return l.OutgoingHtlcLeaves[htlc.HtlcIndex].AuxTapLeaf
	})(auxLeaves)
	htlcScriptInfo, err := genHtlcScript(
		chanType, false, whoseCommit, htlc.RefundTimeout, htlc.RHash,
		keyRing, auxLeaf,
	)
	if err != nil {
		return nil, err
	}
	htlcPkScript := htlcScriptInfo.PkScript()

	// As this is an outgoing HTLC, we just care about the timeout path
	// here.
	scriptPath := input.ScriptPathTimeout
	htlcWitnessScript, err := htlcScriptInfo.WitnessScriptForPath(
		scriptPath,
	)
	if err != nil {
		return nil, err
	}

	htlcCsvDelay := HtlcSecondLevelInputSequence(chanType)

	// If we're spending this HTLC output from the remote node's
	// commitment, then we won't need to go to the second level as our
	// outputs don't have a CSV delay.
	if whoseCommit.IsRemote() {
		// With the script generated, we can completely populated the
		// SignDescriptor needed to sweep the output.
		prevFetcher := txscript.NewCannedPrevOutputFetcher(
			htlcPkScript, int64(htlc.Amt.ToSatoshis()),
		)
		signDesc := input.SignDescriptor{
			KeyDesc:       localChanCfg.HtlcBasePoint,
			SingleTweak:   keyRing.LocalHtlcKeyTweak,
			WitnessScript: htlcWitnessScript,
			Output: &wire.TxOut{
				PkScript: htlcPkScript,
				Value:    int64(htlc.Amt.ToSatoshis()),
			},
			HashType:          sweepSigHash(chanType),
			PrevOutputFetcher: prevFetcher,
		}

		scriptTree, ok := htlcScriptInfo.(input.TapscriptDescriptor)
		if ok {
			signDesc.SignMethod = input.TaprootScriptSpendSignMethod

			ctrlBlock, err := scriptTree.CtrlBlockForPath(
				scriptPath,
			)
			if err != nil {
				return nil, err
			}
			signDesc.ControlBlock, err = ctrlBlock.ToBytes()
			if err != nil {
				return nil, err
			}
		}

		//nolint:ll
		resReq := ResolutionReq{
			ChanPoint:           chanState.FundingOutpoint,
			ChanType:            chanType,
			ShortChanID:         chanState.ShortChanID(),
			Initiator:           chanState.IsInitiator,
			CommitBlob:          chanState.RemoteCommitment.CustomBlob,
			FundingBlob:         chanState.CustomBlob,
			Type:                input.TaprootHtlcOfferedRemoteTimeout,
			CloseType:           RemoteForceClose,
			CommitTx:            commitTx,
			CommitTxBlockHeight: commitTxHeight,
			ContractPoint:       op,
			SignDesc:            signDesc,
			KeyRing:             keyRing,
			CsvDelay:            htlcCsvDelay,
			CltvDelay:           fn.Some(htlc.RefundTimeout),
			CommitFee:           chanState.RemoteCommitment.CommitFee,
			HtlcID:              fn.Some(htlc.HtlcIndex),
			PayHash:             fn.Some(htlc.RHash),
		}
		resolveRes := fn.MapOptionZ(
			auxResolver,
			func(a AuxContractResolver) fn.Result[tlv.Blob] {
				return a.ResolveContract(resReq)
			},
		)
		if err := resolveRes.Err(); err != nil {
			return nil, fmt.Errorf("unable to aux resolve: %w", err)
		}

		resolutionBlob := resolveRes.OkToSome()

		return &OutgoingHtlcResolution{
			Expiry:         htlc.RefundTimeout,
			ClaimOutpoint:  op,
			SweepSignDesc:  signDesc,
			CsvDelay:       htlcCsvDelay,
			ResolutionBlob: resolutionBlob,
		}, nil
	}

	// Otherwise, we'll need to craft a second level HTLC transaction, as
	// well as a sign desc to sweep after the CSV delay.

	// In order to properly reconstruct the HTLC transaction, we'll need to
	// re-calculate the fee required at this state, so we can add the
	// correct output value amount to the transaction.
	htlcFee := HtlcTimeoutFee(chanType, feePerKw)
	secondLevelOutputAmt := htlc.Amt.ToSatoshis() - htlcFee

	// With the fee calculated, re-construct the second level timeout
	// transaction.
	secondLevelAuxLeaf := fn.FlatMapOption(
		func(l CommitAuxLeaves) input.AuxTapLeaf {
			leaves := l.OutgoingHtlcLeaves
			return leaves[htlc.HtlcIndex].SecondLevelLeaf
		},
	)(auxLeaves)
	timeoutTx, err := CreateHtlcTimeoutTx(
		chanType, isCommitFromInitiator, op, secondLevelOutputAmt,
		htlc.RefundTimeout, csvDelay, leaseExpiry,
		keyRing.RevocationKey, keyRing.ToLocalKey, secondLevelAuxLeaf,
	)
	if err != nil {
		return nil, err
	}

	// With the transaction created, we can generate a sign descriptor
	// that's capable of generating the signature required to spend the
	// HTLC output using the timeout transaction.
	txOut := commitTx.TxOut[htlc.OutputIndex]
	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		txOut.PkScript, txOut.Value,
	)
	hashCache := txscript.NewTxSigHashes(timeoutTx, prevFetcher)
	timeoutSignDesc := input.SignDescriptor{
		KeyDesc:           localChanCfg.HtlcBasePoint,
		SingleTweak:       keyRing.LocalHtlcKeyTweak,
		WitnessScript:     htlcWitnessScript,
		Output:            txOut,
		HashType:          sweepSigHash(chanType),
		PrevOutputFetcher: prevFetcher,
		SigHashes:         hashCache,
		InputIndex:        0,
	}

	htlcSig, err := input.ParseSignature(htlc.Signature)
	if err != nil {
		return nil, err
	}

	// With the sign desc created, we can now construct the full witness
	// for the timeout transaction, and populate it as well.
	sigHashType := HtlcSigHashType(chanType)
	var timeoutWitness wire.TxWitness
	if scriptTree, ok := htlcScriptInfo.(input.TapscriptDescriptor); ok {
		timeoutSignDesc.SignMethod = input.TaprootScriptSpendSignMethod

		timeoutWitness, err = input.SenderHTLCScriptTaprootTimeout(
			htlcSig, sigHashType, signer, &timeoutSignDesc,
			timeoutTx, keyRing.RevocationKey,
			scriptTree.TapScriptTree(),
		)
		if err != nil {
			return nil, err
		}

		// The control block is always the final element of the witness
		// stack. We set this here as eventually the sweeper will need
		// to re-sign, so it needs the isolated control block.
		//
		// TODO(roasbeef): move this into input.go?
		ctlrBlkIdx := len(timeoutWitness) - 1
		timeoutSignDesc.ControlBlock = timeoutWitness[ctlrBlkIdx]
	} else {
		timeoutWitness, err = input.SenderHtlcSpendTimeout(
			htlcSig, sigHashType, signer, &timeoutSignDesc,
			timeoutTx,
		)
	}
	if err != nil {
		return nil, err
	}

	timeoutTx.TxIn[0].Witness = timeoutWitness

	// If this is an anchor type channel, the sign details will let us
	// re-sign an aggregated tx later.
	txSignDetails := HtlcSignDetails(
		chanType, timeoutSignDesc, sigHashType, htlcSig,
	)

	// Finally, we'll generate the script output that the timeout
	// transaction creates so we can generate the signDesc required to
	// complete the claim process after a delay period.
	var (
		htlcSweepScript input.ScriptDescriptor
		signMethod      input.SignMethod
		ctrlBlock       []byte
	)
	if !chanType.IsTaproot() {
		htlcSweepScript, err = SecondLevelHtlcScript(
			chanType, isCommitFromInitiator, keyRing.RevocationKey,
			keyRing.ToLocalKey, csvDelay, leaseExpiry,
			secondLevelAuxLeaf,
		)
		if err != nil {
			return nil, err
		}
	} else {
		//nolint:ll
		secondLevelScriptTree, err := input.TaprootSecondLevelScriptTree(
			keyRing.RevocationKey, keyRing.ToLocalKey, csvDelay,
			secondLevelAuxLeaf,
		)
		if err != nil {
			return nil, err
		}

		signMethod = input.TaprootScriptSpendSignMethod

		controlBlock, err := secondLevelScriptTree.CtrlBlockForPath(
			input.ScriptPathSuccess,
		)
		if err != nil {
			return nil, err
		}
		ctrlBlock, err = controlBlock.ToBytes()
		if err != nil {
			return nil, err
		}

		htlcSweepScript = secondLevelScriptTree
	}

	// In this case, the witness script that needs to be signed will always
	// be that of the success path.
	htlcSweepWitnessScript, err := htlcSweepScript.WitnessScriptForPath(
		input.ScriptPathSuccess,
	)
	if err != nil {
		return nil, err
	}

	localDelayTweak := input.SingleTweakBytes(
		keyRing.CommitPoint, localChanCfg.DelayBasePoint.PubKey,
	)

	// In addition to the info in txSignDetails, we also need extra
	// information to sweep the second level output after confirmation.
	sweepSignDesc := input.SignDescriptor{
		KeyDesc:       localChanCfg.DelayBasePoint,
		SingleTweak:   localDelayTweak,
		WitnessScript: htlcSweepWitnessScript,
		Output: &wire.TxOut{
			PkScript: htlcSweepScript.PkScript(),
			Value:    int64(secondLevelOutputAmt),
		},
		HashType: sweepSigHash(chanType),
		PrevOutputFetcher: txscript.NewCannedPrevOutputFetcher(
			htlcSweepScript.PkScript(),
			int64(secondLevelOutputAmt),
		),
		SignMethod:   signMethod,
		ControlBlock: ctrlBlock,
	}

	// In case it is a legacy channel we return early as no aux resolution
	// is neeeded.
	if txSignDetails == nil {
		return &OutgoingHtlcResolution{
			Expiry:          htlc.RefundTimeout,
			SignedTimeoutTx: timeoutTx,
			SignDetails:     txSignDetails,
			CsvDelay:        csvDelay,
			ResolutionBlob:  fn.None[tlv.Blob](),
			ClaimOutpoint: wire.OutPoint{
				Hash:  timeoutTx.TxHash(),
				Index: 0,
			},
			SweepSignDesc: sweepSignDesc,
		}, nil
	}

	// This might be an aux channel, so we'll go ahead and attempt to
	// generate the resolution blob for the channel so we can pass along to
	// the sweeping sub-system.
	resolveRes := fn.MapOptionZ(
		auxResolver, func(a AuxContractResolver) fn.Result[tlv.Blob] {
			//nolint:ll
			resReq := ResolutionReq{
				ChanPoint:           chanState.FundingOutpoint,
				ChanType:            chanType,
				ShortChanID:         chanState.ShortChanID(),
				Initiator:           chanState.IsInitiator,
				CommitBlob:          chanState.LocalCommitment.CustomBlob,
				FundingBlob:         chanState.CustomBlob,
				Type:                input.TaprootHtlcLocalOfferedTimeout,
				CloseType:           LocalForceClose,
				CommitTx:            commitTx,
				CommitTxBlockHeight: commitTxHeight,
				ContractPoint:       op,
				SignDesc:            sweepSignDesc,
				KeyRing:             keyRing,
				CsvDelay:            htlcCsvDelay,
				HtlcAmt:             btcutil.Amount(txOut.Value),
				CommitCsvDelay:      csvDelay,
				CltvDelay:           fn.Some(htlc.RefundTimeout),
				CommitFee:           chanState.LocalCommitment.CommitFee,
				HtlcID:              fn.Some(htlc.HtlcIndex),
				PayHash:             fn.Some(htlc.RHash),
				AuxSigDesc: fn.Some(AuxSigDesc{
					SignDetails: *txSignDetails,
					AuxSig: func() []byte {
						tlvType := htlcCustomSigType.TypeVal()
						return htlc.CustomRecords[uint64(tlvType)]
					}(),
				}),
			}

			return a.ResolveContract(resReq)
		},
	)
	if err := resolveRes.Err(); err != nil {
		return nil, fmt.Errorf("unable to aux resolve: %w", err)
	}
	resolutionBlob := resolveRes.OkToSome()

	return &OutgoingHtlcResolution{
		Expiry:          htlc.RefundTimeout,
		SignedTimeoutTx: timeoutTx,
		SignDetails:     txSignDetails,
		CsvDelay:        csvDelay,
		ResolutionBlob:  resolutionBlob,
		ClaimOutpoint: wire.OutPoint{
			Hash:  timeoutTx.TxHash(),
			Index: 0,
		},
		SweepSignDesc: sweepSignDesc,
	}, nil
}

// newIncomingHtlcResolution creates a new HTLC resolution capable of allowing
// the caller to sweep an incoming HTLC. If the HTLC is on the caller's
// commitment transaction, then they'll need to broadcast a second-level
// transaction before sweeping the output (and incur a CSV delay). Otherwise,
// they can just sweep the output immediately with knowledge of the pre-image.
//
// TODO(roasbeef) consolidate code with above func
func newIncomingHtlcResolution(signer input.Signer,
	localChanCfg *channeldb.ChannelConfig, commitTx *wire.MsgTx,
	commitTxHeight uint32, htlc *channeldb.HTLC, keyRing *CommitmentKeyRing,
	feePerKw chainfee.SatPerKWeight, csvDelay, leaseExpiry uint32,
	whoseCommit lntypes.ChannelParty, isCommitFromInitiator bool,
	chanType channeldb.ChannelType, chanState *channeldb.OpenChannel,
	auxLeaves fn.Option[CommitAuxLeaves],
	auxResolver fn.Option[AuxContractResolver],
) (*IncomingHtlcResolution, error) {

	op := wire.OutPoint{
		Hash:  commitTx.TxHash(),
		Index: uint32(htlc.OutputIndex),
	}

	// First, we'll re-generate the script the remote party used to
	// send the HTLC to us in their commitment transaction.
	auxLeaf := fn.FlatMapOption(func(l CommitAuxLeaves) input.AuxTapLeaf {
		return l.IncomingHtlcLeaves[htlc.HtlcIndex].AuxTapLeaf
	})(auxLeaves)
	scriptInfo, err := genHtlcScript(
		chanType, true, whoseCommit, htlc.RefundTimeout, htlc.RHash,
		keyRing, auxLeaf,
	)
	if err != nil {
		return nil, err
	}

	htlcPkScript := scriptInfo.PkScript()

	// As this is an incoming HTLC, we're attempting to sweep with the
	// success path.
	scriptPath := input.ScriptPathSuccess
	htlcWitnessScript, err := scriptInfo.WitnessScriptForPath(
		scriptPath,
	)
	if err != nil {
		return nil, err
	}

	htlcCsvDelay := HtlcSecondLevelInputSequence(chanType)

	// If we're spending this output from the remote node's commitment,
	// then we can skip the second layer and spend the output directly.
	if whoseCommit.IsRemote() {
		// With the script generated, we can completely populated the
		// SignDescriptor needed to sweep the output.
		prevFetcher := txscript.NewCannedPrevOutputFetcher(
			htlcPkScript, int64(htlc.Amt.ToSatoshis()),
		)
		signDesc := input.SignDescriptor{
			KeyDesc:       localChanCfg.HtlcBasePoint,
			SingleTweak:   keyRing.LocalHtlcKeyTweak,
			WitnessScript: htlcWitnessScript,
			Output: &wire.TxOut{
				PkScript: htlcPkScript,
				Value:    int64(htlc.Amt.ToSatoshis()),
			},
			HashType:          sweepSigHash(chanType),
			PrevOutputFetcher: prevFetcher,
		}

		//nolint:ll
		if scriptTree, ok := scriptInfo.(input.TapscriptDescriptor); ok {
			signDesc.SignMethod = input.TaprootScriptSpendSignMethod
			ctrlBlock, err := scriptTree.CtrlBlockForPath(
				scriptPath,
			)
			if err != nil {
				return nil, err
			}
			signDesc.ControlBlock, err = ctrlBlock.ToBytes()
			if err != nil {
				return nil, err
			}
		}

		//nolint:ll
		resReq := ResolutionReq{
			ChanPoint:           chanState.FundingOutpoint,
			ChanType:            chanType,
			ShortChanID:         chanState.ShortChanID(),
			Initiator:           chanState.IsInitiator,
			CommitBlob:          chanState.RemoteCommitment.CustomBlob,
			Type:                input.TaprootHtlcAcceptedRemoteSuccess,
			FundingBlob:         chanState.CustomBlob,
			CloseType:           RemoteForceClose,
			CommitTx:            commitTx,
			CommitTxBlockHeight: commitTxHeight,
			ContractPoint:       op,
			SignDesc:            signDesc,
			KeyRing:             keyRing,
			HtlcID:              fn.Some(htlc.HtlcIndex),
			CsvDelay:            htlcCsvDelay,
			CltvDelay:           fn.Some(htlc.RefundTimeout),
			CommitFee:           chanState.RemoteCommitment.CommitFee,
			PayHash:             fn.Some(htlc.RHash),
			CommitCsvDelay:      csvDelay,
			HtlcAmt:             htlc.Amt.ToSatoshis(),
		}
		resolveRes := fn.MapOptionZ(
			auxResolver,
			func(a AuxContractResolver) fn.Result[tlv.Blob] {
				return a.ResolveContract(resReq)
			},
		)
		if err := resolveRes.Err(); err != nil {
			return nil, fmt.Errorf("unable to aux resolve: %w", err)
		}

		resolutionBlob := resolveRes.OkToSome()

		return &IncomingHtlcResolution{
			ClaimOutpoint:  op,
			SweepSignDesc:  signDesc,
			CsvDelay:       htlcCsvDelay,
			ResolutionBlob: resolutionBlob,
		}, nil
	}

	secondLevelAuxLeaf := fn.FlatMapOption(
		func(l CommitAuxLeaves) input.AuxTapLeaf {
			leaves := l.IncomingHtlcLeaves
			return leaves[htlc.HtlcIndex].SecondLevelLeaf
		},
	)(auxLeaves)

	// Otherwise, we'll need to go to the second level to sweep this HTLC.
	//
	// First, we'll reconstruct the original HTLC success transaction,
	// taking into account the fee rate used.
	htlcFee := HtlcSuccessFee(chanType, feePerKw)
	secondLevelOutputAmt := htlc.Amt.ToSatoshis() - htlcFee
	successTx, err := CreateHtlcSuccessTx(
		chanType, isCommitFromInitiator, op, secondLevelOutputAmt,
		csvDelay, leaseExpiry, keyRing.RevocationKey,
		keyRing.ToLocalKey, secondLevelAuxLeaf,
	)
	if err != nil {
		return nil, err
	}

	// Once we've created the second-level transaction, we'll generate the
	// SignDesc needed spend the HTLC output using the success transaction.
	txOut := commitTx.TxOut[htlc.OutputIndex]
	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		txOut.PkScript, txOut.Value,
	)
	hashCache := txscript.NewTxSigHashes(successTx, prevFetcher)
	successSignDesc := input.SignDescriptor{
		KeyDesc:           localChanCfg.HtlcBasePoint,
		SingleTweak:       keyRing.LocalHtlcKeyTweak,
		WitnessScript:     htlcWitnessScript,
		Output:            txOut,
		HashType:          sweepSigHash(chanType),
		PrevOutputFetcher: prevFetcher,
		SigHashes:         hashCache,
		InputIndex:        0,
	}

	htlcSig, err := input.ParseSignature(htlc.Signature)
	if err != nil {
		return nil, err
	}

	// Next, we'll construct the full witness needed to satisfy the input of
	// the success transaction. Don't specify the preimage yet. The preimage
	// will be supplied by the contract resolver, either directly or when it
	// becomes known.
	var successWitness wire.TxWitness
	sigHashType := HtlcSigHashType(chanType)
	if scriptTree, ok := scriptInfo.(input.TapscriptDescriptor); ok {
		successSignDesc.SignMethod = input.TaprootScriptSpendSignMethod

		successWitness, err = input.ReceiverHTLCScriptTaprootRedeem(
			htlcSig, sigHashType, nil, signer, &successSignDesc,
			successTx, keyRing.RevocationKey,
			scriptTree.TapScriptTree(),
		)
		if err != nil {
			return nil, err
		}

		// The control block is always the final element of the witness
		// stack. We set this here as eventually the sweeper will need
		// to re-sign, so it needs the isolated control block.
		//
		// TODO(roasbeef): move this into input.go?
		ctlrBlkIdx := len(successWitness) - 1
		successSignDesc.ControlBlock = successWitness[ctlrBlkIdx]
	} else {
		successWitness, err = input.ReceiverHtlcSpendRedeem(
			htlcSig, sigHashType, nil, signer, &successSignDesc,
			successTx,
		)
		if err != nil {
			return nil, err
		}
	}
	successTx.TxIn[0].Witness = successWitness

	// If this is an anchor type channel, the sign details will let us
	// re-sign an aggregated tx later.
	txSignDetails := HtlcSignDetails(
		chanType, successSignDesc, sigHashType, htlcSig,
	)

	// Finally, we'll generate the script that the second-level transaction
	// creates so we can generate the proper signDesc to sweep it after the
	// CSV delay has passed.
	var (
		htlcSweepScript input.ScriptDescriptor
		signMethod      input.SignMethod
		ctrlBlock       []byte
	)
	if !chanType.IsTaproot() {
		htlcSweepScript, err = SecondLevelHtlcScript(
			chanType, isCommitFromInitiator, keyRing.RevocationKey,
			keyRing.ToLocalKey, csvDelay, leaseExpiry,
			secondLevelAuxLeaf,
		)
		if err != nil {
			return nil, err
		}
	} else {
		//nolint:ll
		secondLevelScriptTree, err := input.TaprootSecondLevelScriptTree(
			keyRing.RevocationKey, keyRing.ToLocalKey, csvDelay,
			secondLevelAuxLeaf,
		)
		if err != nil {
			return nil, err
		}

		signMethod = input.TaprootScriptSpendSignMethod

		controlBlock, err := secondLevelScriptTree.CtrlBlockForPath(
			input.ScriptPathSuccess,
		)
		if err != nil {
			return nil, err
		}
		ctrlBlock, err = controlBlock.ToBytes()
		if err != nil {
			return nil, err
		}

		htlcSweepScript = secondLevelScriptTree
	}

	// In this case, the witness script that needs to be signed will always
	// be that of the success path.
	htlcSweepWitnessScript, err := htlcSweepScript.WitnessScriptForPath(
		input.ScriptPathSuccess,
	)
	if err != nil {
		return nil, err
	}

	localDelayTweak := input.SingleTweakBytes(
		keyRing.CommitPoint, localChanCfg.DelayBasePoint.PubKey,
	)

	// In addition to the info in txSignDetails, we also need extra
	// information to sweep the second level output after confirmation.
	sweepSignDesc := input.SignDescriptor{
		KeyDesc:       localChanCfg.DelayBasePoint,
		SingleTweak:   localDelayTweak,
		WitnessScript: htlcSweepWitnessScript,
		Output: &wire.TxOut{
			PkScript: htlcSweepScript.PkScript(),
			Value:    int64(secondLevelOutputAmt),
		},
		HashType: sweepSigHash(chanType),
		PrevOutputFetcher: txscript.NewCannedPrevOutputFetcher(
			htlcSweepScript.PkScript(),
			int64(secondLevelOutputAmt),
		),
		SignMethod:   signMethod,
		ControlBlock: ctrlBlock,
	}

	if txSignDetails == nil {
		return &IncomingHtlcResolution{
			SignedSuccessTx: successTx,
			SignDetails:     txSignDetails,
			CsvDelay:        csvDelay,
			ResolutionBlob:  fn.None[tlv.Blob](),
			ClaimOutpoint: wire.OutPoint{
				Hash:  successTx.TxHash(),
				Index: 0,
			},
			SweepSignDesc: sweepSignDesc,
		}, nil
	}

	resolveRes := fn.MapOptionZ(
		auxResolver, func(a AuxContractResolver) fn.Result[tlv.Blob] {
			//nolint:ll
			resReq := ResolutionReq{
				ChanPoint:           chanState.FundingOutpoint,
				ChanType:            chanType,
				ShortChanID:         chanState.ShortChanID(),
				Initiator:           chanState.IsInitiator,
				CommitBlob:          chanState.LocalCommitment.CustomBlob,
				Type:                input.TaprootHtlcAcceptedLocalSuccess,
				FundingBlob:         chanState.CustomBlob,
				CloseType:           LocalForceClose,
				CommitTx:            commitTx,
				CommitTxBlockHeight: commitTxHeight,
				ContractPoint:       op,
				SignDesc:            sweepSignDesc,
				KeyRing:             keyRing,
				HtlcID:              fn.Some(htlc.HtlcIndex),
				CsvDelay:            htlcCsvDelay,
				CommitFee:           chanState.LocalCommitment.CommitFee,
				PayHash:             fn.Some(htlc.RHash),
				AuxSigDesc: fn.Some(AuxSigDesc{
					SignDetails: *txSignDetails,
					AuxSig: func() []byte {
						tlvType := htlcCustomSigType.TypeVal()
						return htlc.CustomRecords[uint64(tlvType)]
					}(),
				}),
				CommitCsvDelay: csvDelay,
				HtlcAmt:        btcutil.Amount(txOut.Value),
				CltvDelay:      fn.Some(htlc.RefundTimeout),
			}

			return a.ResolveContract(resReq)
		},
	)
	if err := resolveRes.Err(); err != nil {
		return nil, fmt.Errorf("unable to aux resolve: %w", err)
	}

	resolutionBlob := resolveRes.OkToSome()

	return &IncomingHtlcResolution{
		SignedSuccessTx: successTx,
		SignDetails:     txSignDetails,
		CsvDelay:        csvDelay,
		ResolutionBlob:  resolutionBlob,
		ClaimOutpoint: wire.OutPoint{
			Hash:  successTx.TxHash(),
			Index: 0,
		},
		SweepSignDesc: sweepSignDesc,
	}, nil
}

// HtlcPoint returns the htlc's outpoint on the commitment tx.
func (r *IncomingHtlcResolution) HtlcPoint() wire.OutPoint {
	// If we have a success transaction, then the htlc's outpoint
	// is the transaction's only input. Otherwise, it's the claim
	// point.
	if r.SignedSuccessTx != nil {
		return r.SignedSuccessTx.TxIn[0].PreviousOutPoint
	}

	return r.ClaimOutpoint
}

// HtlcPoint returns the htlc's outpoint on the commitment tx.
func (r *OutgoingHtlcResolution) HtlcPoint() wire.OutPoint {
	// If we have a timeout transaction, then the htlc's outpoint
	// is the transaction's only input. Otherwise, it's the claim
	// point.
	if r.SignedTimeoutTx != nil {
		return r.SignedTimeoutTx.TxIn[0].PreviousOutPoint
	}

	return r.ClaimOutpoint
}

// extractHtlcResolutions creates a series of outgoing HTLC resolutions, and
// the local key used when generating the HTLC scrips. This function is to be
// used in two cases: force close, or a unilateral close.
func extractHtlcResolutions(feePerKw chainfee.SatPerKWeight,
	whoseCommit lntypes.ChannelParty, signer input.Signer,
	htlcs []channeldb.HTLC, keyRing *CommitmentKeyRing,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig,
	commitTx *wire.MsgTx, commitTxHeight uint32,
	chanType channeldb.ChannelType, isCommitFromInitiator bool,
	leaseExpiry uint32, chanState *channeldb.OpenChannel,
	auxLeaves fn.Option[CommitAuxLeaves],
	auxResolver fn.Option[AuxContractResolver]) (*HtlcResolutions, error) {

	// TODO(roasbeef): don't need to swap csv delay?
	dustLimit := remoteChanCfg.DustLimit
	csvDelay := remoteChanCfg.CsvDelay
	if whoseCommit.IsLocal() {
		dustLimit = localChanCfg.DustLimit
		csvDelay = localChanCfg.CsvDelay
	}

	incomingResolutions := make([]IncomingHtlcResolution, 0, len(htlcs))
	outgoingResolutions := make([]OutgoingHtlcResolution, 0, len(htlcs))
	for _, htlc := range htlcs {
		htlc := htlc

		// We'll skip any HTLC's which were dust on the commitment
		// transaction, as these don't have a corresponding output
		// within the commitment transaction.
		if HtlcIsDust(
			chanType, htlc.Incoming, whoseCommit, feePerKw,
			htlc.Amt.ToSatoshis(), dustLimit,
		) {

			continue
		}

		// If the HTLC is incoming, then we'll attempt to see if we
		// know the pre-image to the HTLC.
		if htlc.Incoming {
			// Otherwise, we'll create an incoming HTLC resolution
			// as we can satisfy the contract.
			ihr, err := newIncomingHtlcResolution(
				signer, localChanCfg, commitTx, commitTxHeight,
				&htlc, keyRing, feePerKw, uint32(csvDelay),
				leaseExpiry, whoseCommit, isCommitFromInitiator,
				chanType, chanState, auxLeaves, auxResolver,
			)
			if err != nil {
				return nil, fmt.Errorf("incoming resolution "+
					"failed: %v", err)
			}

			incomingResolutions = append(incomingResolutions, *ihr)
			continue
		}

		ohr, err := newOutgoingHtlcResolution(
			signer, localChanCfg, commitTx, commitTxHeight, &htlc,
			keyRing, feePerKw, uint32(csvDelay), leaseExpiry,
			whoseCommit, isCommitFromInitiator, chanType, chanState,
			auxLeaves, auxResolver,
		)
		if err != nil {
			return nil, fmt.Errorf("outgoing resolution "+
				"failed: %v", err)
		}

		outgoingResolutions = append(outgoingResolutions, *ohr)
	}

	return &HtlcResolutions{
		IncomingHTLCs: incomingResolutions,
		OutgoingHTLCs: outgoingResolutions,
	}, nil
}

// AnchorResolution holds the information necessary to spend our commitment tx
// anchor.
type AnchorResolution struct {
	// AnchorSignDescriptor is the sign descriptor for our anchor.
	AnchorSignDescriptor input.SignDescriptor

	// CommitAnchor is the anchor outpoint on the commit tx.
	CommitAnchor wire.OutPoint

	// CommitFee is the fee of the commit tx.
	CommitFee btcutil.Amount

	// CommitWeight is the weight of the commit tx.
	CommitWeight lntypes.WeightUnit
}

// LocalForceCloseSummary describes the final commitment state before the
// channel is locked-down to initiate a force closure by broadcasting the
// latest state on-chain. If we intend to broadcast this this state, the
// channel should not be used after generating this close summary.  The summary
// includes all the information required to claim all rightfully owned outputs
// when the commitment gets confirmed.
type LocalForceCloseSummary struct {
	// ChanPoint is the outpoint that created the channel which has been
	// force closed.
	ChanPoint wire.OutPoint

	// CloseTx is the transaction which can be used to close the channel
	// on-chain. When we initiate a force close, this will be our latest
	// commitment state.
	CloseTx *wire.MsgTx

	// ChanSnapshot is a snapshot of the final state of the channel at the
	// time the summary was created.
	ChanSnapshot channeldb.ChannelSnapshot

	// ContractResolutions contains all the data required for resolving the
	// different output types of a commitment transaction.
	ContractResolutions fn.Option[ContractResolutions]
}

// ContractResolutions contains all the data required for resolving the
// different output types of a commitment transaction.
type ContractResolutions struct {
	// CommitResolution contains all the data required to sweep the output
	// to ourselves. Since this is our commitment transaction, we'll need
	// to wait a time delay before we can sweep the output.
	//
	// NOTE: If our commitment delivery output is below the dust limit,
	// then this will be nil.
	CommitResolution *CommitOutputResolution

	// AnchorResolution contains the data required to sweep the anchor
	// output. If the channel type doesn't include anchors, the value of
	// this field will be nil.
	AnchorResolution *AnchorResolution

	// HtlcResolutions contains all the data required to sweep any outgoing
	// HTLC's and incoming HTLc's we know the preimage to. For each of these
	// HTLC's, we'll need to go to the second level to sweep them fully.
	HtlcResolutions *HtlcResolutions
}

// ForceCloseOpt is a functional option argument for the ForceClose method.
type ForceCloseOpt func(*forceCloseConfig)

// forceCloseConfig holds the configuration options for force closing a channel.
type forceCloseConfig struct {
	// skipResolution if true will skip creating the contract resolutions
	// when generating the force close summary.
	skipResolution bool
}

// defaultForceCloseConfig returns the default force close configuration.
func defaultForceCloseConfig() *forceCloseConfig {
	return &forceCloseConfig{}
}

// WithSkipContractResolutions creates an option to skip the contract
// resolutions from the returned summary.
func WithSkipContractResolutions() ForceCloseOpt {
	return func(cfg *forceCloseConfig) {
		cfg.skipResolution = true
	}
}

// ForceClose executes a unilateral closure of the transaction at the current
// lowest commitment height of the channel. Following a force closure, all
// state transitions, or modifications to the state update logs will be
// rejected. Additionally, this function also returns a LocalForceCloseSummary
// which includes the necessary details required to sweep all the time-locked
// outputs within the commitment transaction.
//
// TODO(roasbeef): all methods need to abort if in dispute state
func (lc *LightningChannel) ForceClose(opts ...ForceCloseOpt) (
	*LocalForceCloseSummary, error) {

	lc.Lock()
	defer lc.Unlock()

	cfg := defaultForceCloseConfig()
	for _, opt := range opts {
		opt(cfg)
	}

	// If we've detected local data loss for this channel, then we won't
	// allow a force close, as it may be the case that we have a dated
	// version of the commitment, or this is actually a channel shell.
	if lc.channelState.HasChanStatus(channeldb.ChanStatusLocalDataLoss) {
		return nil, fmt.Errorf("%w: channel_state=%v",
			ErrForceCloseLocalDataLoss,
			lc.channelState.ChanStatus())
	}

	commitTx, err := lc.getSignedCommitTx()
	if err != nil {
		return nil, err
	}

	if cfg.skipResolution {
		return &LocalForceCloseSummary{
			ChanPoint:    lc.channelState.FundingOutpoint,
			ChanSnapshot: *lc.channelState.Snapshot(),
			CloseTx:      commitTx,
		}, nil
	}

	localCommitment := lc.channelState.LocalCommitment
	summary, err := NewLocalForceCloseSummary(
		lc.channelState, lc.Signer, commitTx,
		0, localCommitment.CommitHeight, lc.leafStore,
		lc.auxResolver,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to gen force close "+
			"summary: %w", err)
	}

	// Mark the channel as closed to block future closure requests.
	lc.isClosed = true

	return summary, nil
}

// NewLocalForceCloseSummary generates a LocalForceCloseSummary from the given
// channel state. The passed commitTx must be a fully signed commitment
// transaction corresponding to localCommit.
func NewLocalForceCloseSummary(chanState *channeldb.OpenChannel,
	signer input.Signer, commitTx *wire.MsgTx, commitTxHeight uint32,
	stateNum uint64, leafStore fn.Option[AuxLeafStore],
	auxResolver fn.Option[AuxContractResolver]) (*LocalForceCloseSummary,
	error) {

	// Re-derive the original pkScript for to-self output within the
	// commitment transaction. We'll need this to find the corresponding
	// output in the commitment transaction and potentially for creating
	// the sign descriptor.
	csvTimeout := uint32(chanState.LocalChanCfg.CsvDelay)

	// We use the passed state num to derive our scripts, since in case
	// this is after recovery, our latest channels state might not be up to
	// date.
	revocation, err := chanState.RevocationProducer.AtIndex(stateNum)
	if err != nil {
		return nil, err
	}
	commitPoint := input.ComputeCommitmentPoint(revocation[:])
	keyRing := DeriveCommitmentKeys(
		commitPoint, lntypes.Local, chanState.ChanType,
		&chanState.LocalChanCfg, &chanState.RemoteChanCfg,
	)

	auxResult, err := fn.MapOptionZ(
		leafStore, func(s AuxLeafStore) fn.Result[CommitDiffAuxResult] {
			return s.FetchLeavesFromCommit(
				NewAuxChanState(chanState),
				chanState.LocalCommitment, *keyRing,
				lntypes.Local,
			)
		},
	).Unpack()
	if err != nil {
		return nil, fmt.Errorf("unable to fetch aux leaves: %w", err)
	}

	var leaseExpiry uint32
	if chanState.ChanType.HasLeaseExpiration() {
		leaseExpiry = chanState.ThawHeight
	}

	localAuxLeaf := fn.FlatMapOption(
		func(l CommitAuxLeaves) input.AuxTapLeaf {
			return l.LocalAuxLeaf
		},
	)(auxResult.AuxLeaves)
	toLocalScript, err := CommitScriptToSelf(
		chanState.ChanType, chanState.IsInitiator, keyRing.ToLocalKey,
		keyRing.RevocationKey, csvTimeout, leaseExpiry, localAuxLeaf,
	)
	if err != nil {
		return nil, err
	}

	// Locate the output index of the delayed commitment output back to us.
	// We'll return the details of this output to the caller so they can
	// sweep it once it's mature.
	var (
		delayIndex uint32
		delayOut   *wire.TxOut
	)
	for i, txOut := range commitTx.TxOut {
		if !bytes.Equal(toLocalScript.PkScript(), txOut.PkScript) {
			continue
		}

		delayIndex = uint32(i)
		delayOut = txOut
		break
	}

	// With the necessary information gathered above, create a new sign
	// descriptor which is capable of generating the signature the caller
	// needs to sweep this output. The hash cache, and input index are not
	// set as the caller will decide these values once sweeping the output.
	// If the output is non-existent (dust), have the sign descriptor be
	// nil.
	var commitResolution *CommitOutputResolution
	if delayOut != nil {
		// When attempting to sweep our own output, we only need the
		// witness script for the delay path
		scriptPath := input.ScriptPathDelay
		witnessScript, err := toLocalScript.WitnessScriptForPath(
			scriptPath,
		)
		if err != nil {
			return nil, err
		}

		localBalance := delayOut.Value
		commitResolution = &CommitOutputResolution{
			SelfOutPoint: wire.OutPoint{
				Hash:  commitTx.TxHash(),
				Index: delayIndex,
			},
			SelfOutputSignDesc: input.SignDescriptor{
				KeyDesc:       chanState.LocalChanCfg.DelayBasePoint,
				SingleTweak:   keyRing.LocalCommitKeyTweak,
				WitnessScript: witnessScript,
				Output: &wire.TxOut{
					PkScript: delayOut.PkScript,
					Value:    localBalance,
				},
				HashType: sweepSigHash(chanState.ChanType),
			},
			MaturityDelay: csvTimeout,
		}

		// For taproot channels, we'll need to set some additional
		// fields to ensure the output can be swept.
		scriptTree, ok := toLocalScript.(input.TapscriptDescriptor)
		if ok {
			commitResolution.SelfOutputSignDesc.SignMethod =
				input.TaprootScriptSpendSignMethod

			ctrlBlock, err := scriptTree.CtrlBlockForPath(
				scriptPath,
			)
			if err != nil {
				return nil, err
			}
			//nolint:ll
			commitResolution.SelfOutputSignDesc.ControlBlock, err = ctrlBlock.ToBytes()
			if err != nil {
				return nil, err
			}
		}

		// At this point, we'll check to see if we need any extra
		// resolution data for this output.
		resolveBlob := fn.MapOptionZ(
			auxResolver,
			func(a AuxContractResolver) fn.Result[tlv.Blob] {
				//nolint:ll
				return a.ResolveContract(ResolutionReq{
					ChanPoint:           chanState.FundingOutpoint,
					ChanType:            chanState.ChanType,
					ShortChanID:         chanState.ShortChanID(),
					Initiator:           chanState.IsInitiator,
					CommitBlob:          chanState.LocalCommitment.CustomBlob,
					FundingBlob:         chanState.CustomBlob,
					Type:                input.TaprootLocalCommitSpend,
					CloseType:           LocalForceClose,
					CommitTx:            commitTx,
					CommitTxBlockHeight: commitTxHeight,
					ContractPoint:       commitResolution.SelfOutPoint,
					SignDesc:            commitResolution.SelfOutputSignDesc,
					KeyRing:             keyRing,
					CsvDelay:            csvTimeout,
					CommitFee:           chanState.LocalCommitment.CommitFee,
				})
			},
		)
		if err := resolveBlob.Err(); err != nil {
			return nil, fmt.Errorf("unable to aux resolve: %w", err)
		}

		commitResolution.ResolutionBlob = resolveBlob.OkToSome()
	}

	// Once the delay output has been found (if it exists), then we'll also
	// need to create a series of sign descriptors for any lingering
	// outgoing HTLC's that we'll need to claim as well. If this is after
	// recovery there is not much we can do with HTLCs, so we'll always
	// use what we have in our latest state when extracting resolutions.
	localCommit := chanState.LocalCommitment
	htlcResolutions, err := extractHtlcResolutions(
		chainfee.SatPerKWeight(localCommit.FeePerKw), lntypes.Local,
		signer, localCommit.Htlcs, keyRing, &chanState.LocalChanCfg,
		&chanState.RemoteChanCfg, commitTx, commitTxHeight,
		chanState.ChanType, chanState.IsInitiator, leaseExpiry,
		chanState, auxResult.AuxLeaves, auxResolver,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to gen htlc resolution: %w", err)
	}

	anchorResolution, err := NewAnchorResolution(
		chanState, commitTx, keyRing, lntypes.Local,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to gen anchor "+
			"resolution: %w", err)
	}

	return &LocalForceCloseSummary{
		ChanPoint:    chanState.FundingOutpoint,
		CloseTx:      commitTx,
		ChanSnapshot: *chanState.Snapshot(),
		ContractResolutions: fn.Some(ContractResolutions{
			CommitResolution: commitResolution,
			HtlcResolutions:  htlcResolutions,
			AnchorResolution: anchorResolution,
		}),
	}, nil
}

// CloseOutput wraps a normal tx out with additional metadata that indicates if
// the output belongs to the initiator of the channel or not.
type CloseOutput struct {
	wire.TxOut

	// IsLocal indicates if the output belong to the local party.
	IsLocal bool
}

// CloseSortFunc is a function type alias for a function that sorts the closing
// transaction.
type CloseSortFunc func(*wire.MsgTx) error

// chanCloseOpt is a functional option that can be used to modify the co-op
// close process.
type chanCloseOpt struct {
	musigSession *MusigSession

	extraCloseOutputs []CloseOutput

	// customSort is a custom function that can be used to sort the
	// transaction outputs. If this isn't set, then the default BIP-69
	// sorting is used.
	customSort CloseSortFunc

	customSequence fn.Option[uint32]

	customLockTime fn.Option[uint32]

	customPayer fn.Option[lntypes.ChannelParty]
}

// ChanCloseOpt is a closure type that cen be used to modify the set of default
// options.
type ChanCloseOpt func(*chanCloseOpt)

// defaultCloseOpts is the default set of close options.
func defaultCloseOpts() *chanCloseOpt {
	return &chanCloseOpt{}
}

// WithCoopCloseMusigSession can be used to apply an existing musig2 session to
// the cooperative close process. If specified, then a musig2 co-op close
// (single sig keyspend) will be used.
func WithCoopCloseMusigSession(session *MusigSession) ChanCloseOpt {
	return func(opts *chanCloseOpt) {
		opts.musigSession = session
	}
}

// WithExtraCloseOutputs can be used to add extra outputs to the cooperative
// transaction.
func WithExtraCloseOutputs(extraOutputs []CloseOutput) ChanCloseOpt {
	return func(opts *chanCloseOpt) {
		opts.extraCloseOutputs = extraOutputs
	}
}

// WithCustomCoopSort can be used to modify the way the co-op close transaction
// is sorted.
func WithCustomCoopSort(sorter CloseSortFunc) ChanCloseOpt {
	return func(opts *chanCloseOpt) {
		opts.customSort = sorter
	}
}

// WithCustomSequence can be used to specify a custom sequence number for the
// co-op close process. Otherwise, a default non-final sequence will be used.
func WithCustomSequence(sequence uint32) ChanCloseOpt {
	return func(opts *chanCloseOpt) {
		opts.customSequence = fn.Some(sequence)
	}
}

// WithCustomLockTime can be used to specify a custom lock time for the coop
// close transaction.
func WithCustomLockTime(lockTime uint32) ChanCloseOpt {
	return func(opts *chanCloseOpt) {
		opts.customLockTime = fn.Some(lockTime)
	}
}

// WithCustomPayer can be used to specify a custom payer for the closing
// transaction. This overrides the default payer, which is the initiator of the
// channel.
func WithCustomPayer(payer lntypes.ChannelParty) ChanCloseOpt {
	return func(opts *chanCloseOpt) {
		opts.customPayer = fn.Some(payer)
	}
}

// CreateCloseProposal is used by both parties in a cooperative channel close
// workflow to generate proposed close transactions and signatures. This method
// should only be executed once all pending HTLCs (if any) on the channel have
// been cleared/removed. Upon completion, the source channel will shift into
// the "closing" state, which indicates that all incoming/outgoing HTLC
// requests should be rejected. A signature for the closing transaction is
// returned.
func (lc *LightningChannel) CreateCloseProposal(proposedFee btcutil.Amount,
	localDeliveryScript []byte, remoteDeliveryScript []byte,
	closeOpts ...ChanCloseOpt) (input.Signature, *wire.MsgTx,
	btcutil.Amount, error) {

	lc.Lock()
	defer lc.Unlock()

	opts := defaultCloseOpts()
	for _, optFunc := range closeOpts {
		optFunc(opts)
	}

	// Unless there's a custom payer (sign of the RBF flow), if we're
	// already closing the channel, then ignore this request.
	if lc.isClosed && opts.customPayer.IsNone() {
		return nil, nil, 0, ErrChanClosing
	}

	// Get the final balances after subtracting the proposed fee, taking
	// care not to persist the adjusted balance, as the feeRate may change
	// during the channel closing process.
	ourBalance, theirBalance, err := CoopCloseBalance(
		lc.channelState.ChanType, lc.channelState.IsInitiator,
		proposedFee,
		lc.channelState.LocalCommitment.LocalBalance.ToSatoshis(),
		lc.channelState.LocalCommitment.RemoteBalance.ToSatoshis(),
		lc.channelState.LocalCommitment.CommitFee,
		opts.customPayer,
	)
	if err != nil {
		return nil, nil, 0, err
	}

	var closeTxOpts []CloseTxOpt

	// If this is a taproot channel, then we use an RBF'able funding input.
	if lc.channelState.ChanType.IsTaproot() {
		closeTxOpts = append(closeTxOpts, WithRBFCloseTx())
	}

	// If we have any extra outputs to pass along, then we'll map that to
	// the co-op close option txn type.
	if opts.extraCloseOutputs != nil {
		closeTxOpts = append(closeTxOpts, WithExtraTxCloseOutputs(
			opts.extraCloseOutputs,
		))
	}
	if opts.customSort != nil {
		closeTxOpts = append(
			closeTxOpts, WithCustomTxSort(opts.customSort),
		)
	}

	opts.customSequence.WhenSome(func(sequence uint32) {
		closeTxOpts = append(closeTxOpts, WithCustomTxInSequence(
			sequence,
		))
	})

	opts.customLockTime.WhenSome(func(lockTime uint32) {
		closeTxOpts = append(closeTxOpts, WithCustomTxLockTime(
			lockTime,
		))
	})

	closeTx, err := CreateCooperativeCloseTx(
		fundingTxIn(lc.channelState), lc.channelState.LocalChanCfg.DustLimit,
		lc.channelState.RemoteChanCfg.DustLimit, ourBalance, theirBalance,
		localDeliveryScript, remoteDeliveryScript, closeTxOpts...,
	)
	if err != nil {
		return nil, nil, 0, err
	}

	// Ensure that the transaction doesn't explicitly violate any
	// consensus rules such as being too big, or having any value with a
	// negative output.
	tx := btcutil.NewTx(closeTx)
	if err := blockchain.CheckTransactionSanity(tx); err != nil {
		return nil, nil, 0, err
	}

	// If we have a co-op close musig session, then this is a taproot
	// channel, so we'll generate a _partial_ signature.
	var sig input.Signature
	if opts.musigSession != nil {
		sig, err = opts.musigSession.SignCommit(closeTx)
		if err != nil {
			return nil, nil, 0, err
		}
	} else {
		// For regular channels we'll, sign the completed cooperative
		// closure transaction. As the initiator we'll simply send our
		// signature over to the remote party, using the generated txid
		// to be notified once the closure transaction has been
		// confirmed.
		lc.signDesc.SigHashes = input.NewTxSigHashesV0Only(closeTx)
		sig, err = lc.Signer.SignOutputRaw(closeTx, lc.signDesc)
		if err != nil {
			return nil, nil, 0, err
		}
	}

	return sig, closeTx, ourBalance, nil

}

// CompleteCooperativeClose completes the cooperative closure of the target
// active lightning channel. A fully signed closure transaction as well as the
// signature itself are returned. Additionally, we also return our final
// settled balance, which reflects any fees we may have paid.
//
// NOTE: The passed local and remote sigs are expected to be fully complete
// signatures including the proper sighash byte.
func (lc *LightningChannel) CompleteCooperativeClose(
	localSig, remoteSig input.Signature,
	localDeliveryScript, remoteDeliveryScript []byte,
	proposedFee btcutil.Amount,
	closeOpts ...ChanCloseOpt) (*wire.MsgTx, btcutil.Amount, error) {

	lc.Lock()
	defer lc.Unlock()

	opts := defaultCloseOpts()
	for _, optFunc := range closeOpts {
		optFunc(opts)
	}

	// Unless there's a custom payer (sign of the RBF flow), if we're
	// already closing the channel, then ignore this request.
	if lc.isClosed && opts.customPayer.IsNone() {
		return nil, 0, ErrChanClosing
	}

	// Get the final balances after subtracting the proposed fee.
	ourBalance, theirBalance, err := CoopCloseBalance(
		lc.channelState.ChanType, lc.channelState.IsInitiator,
		proposedFee,
		lc.channelState.LocalCommitment.LocalBalance.ToSatoshis(),
		lc.channelState.LocalCommitment.RemoteBalance.ToSatoshis(),
		lc.channelState.LocalCommitment.CommitFee,
		opts.customPayer,
	)
	if err != nil {
		return nil, 0, err
	}

	var closeTxOpts []CloseTxOpt

	// If this is a taproot channel, then we use an RBF'able funding input.
	if lc.channelState.ChanType.IsTaproot() {
		closeTxOpts = append(closeTxOpts, WithRBFCloseTx())
	}

	// If we have any extra outputs to pass along, then we'll map that to
	// the co-op close option txn type.
	if opts.extraCloseOutputs != nil {
		closeTxOpts = append(closeTxOpts, WithExtraTxCloseOutputs(
			opts.extraCloseOutputs,
		))
	}
	if opts.customSort != nil {
		closeTxOpts = append(
			closeTxOpts, WithCustomTxSort(opts.customSort),
		)
	}

	opts.customSequence.WhenSome(func(sequence uint32) {
		closeTxOpts = append(closeTxOpts, WithCustomTxInSequence(
			sequence,
		))
	})

	opts.customLockTime.WhenSome(func(lockTime uint32) {
		closeTxOpts = append(closeTxOpts, WithCustomTxLockTime(
			lockTime,
		))
	})

	// Create the transaction used to return the current settled balance
	// on this active channel back to both parties. In this current model,
	// the initiator pays full fees for the cooperative close transaction.
	closeTx, err := CreateCooperativeCloseTx(
		fundingTxIn(lc.channelState), lc.channelState.LocalChanCfg.DustLimit,
		lc.channelState.RemoteChanCfg.DustLimit, ourBalance, theirBalance,
		localDeliveryScript, remoteDeliveryScript, closeTxOpts...,
	)
	if err != nil {
		return nil, 0, err
	}

	// Ensure that the transaction doesn't explicitly validate any
	// consensus rules such as being too big, or having any value with a
	// negative output.
	tx := btcutil.NewTx(closeTx)
	prevOut := lc.signDesc.Output
	if err := blockchain.CheckTransactionSanity(tx); err != nil {
		return nil, 0, err
	}

	prevOutputFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)
	hashCache := txscript.NewTxSigHashes(closeTx, prevOutputFetcher)

	// Next, we'll complete the co-op close transaction. Depending on the
	// set of options, we'll either do a regular p2wsh spend, or construct
	// the final schnorr signature from a set of partial sigs.
	if opts.musigSession != nil {
		// For taproot channels, we'll use the attached session to
		// combine the two partial signatures into a proper schnorr
		// signature.
		remotePartialSig, ok := remoteSig.(*MusigPartialSig)
		if !ok {
			return nil, 0, fmt.Errorf("expected MusigPartialSig, "+
				"got %T", remoteSig)
		}

		finalSchnorrSig, err := opts.musigSession.CombineSigs(
			remotePartialSig.sig,
		)
		if err != nil {
			return nil, 0, fmt.Errorf("unable to combine "+
				"final co-op close sig: %w", err)
		}

		// The witness for a keyspend is just the signature itself.
		closeTx.TxIn[0].Witness = wire.TxWitness{
			finalSchnorrSig.Serialize(),
		}
	} else {
		// For regular channels, we'll need to , construct the witness
		// stack minding the order of the pubkeys+sigs on the stack.
		ourKey := lc.channelState.LocalChanCfg.MultiSigKey.PubKey.
			SerializeCompressed()
		theirKey := lc.channelState.RemoteChanCfg.MultiSigKey.PubKey.
			SerializeCompressed()
		witness := input.SpendMultiSig(
			lc.signDesc.WitnessScript, ourKey, localSig, theirKey,
			remoteSig,
		)
		closeTx.TxIn[0].Witness = witness
	}

	// Validate the finalized transaction to ensure the output script is
	// properly met, and that the remote peer supplied a valid signature.
	vm, err := txscript.NewEngine(
		prevOut.PkScript, closeTx, 0, txscript.StandardVerifyFlags, nil,
		hashCache, prevOut.Value, prevOutputFetcher,
	)
	if err != nil {
		return nil, 0, err
	}
	if err := vm.Execute(); err != nil {
		return nil, 0, err
	}

	// As the transaction is sane, and the scripts are valid we'll mark the
	// channel now as closed as the closure transaction should get into the
	// chain in a timely manner and possibly be re-broadcast by the wallet.
	lc.isClosed = true

	return closeTx, ourBalance, nil
}

// AnchorResolutions is a set of anchor resolutions that's being used when
// sweeping anchors during local channel force close.
type AnchorResolutions struct {
	// Local is the anchor resolution for the local commitment tx.
	Local *AnchorResolution

	// Remote is the anchor resolution for the remote commitment tx.
	Remote *AnchorResolution

	// RemotePending is the anchor resolution for the remote pending
	// commitment tx. The value will be non-nil iff we've created a new
	// commitment tx for the remote party which they haven't ACKed yet.
	RemotePending *AnchorResolution
}

// NewAnchorResolutions returns a set of anchor resolutions wrapped in the
// struct AnchorResolutions. Because we have no view on the mempool, we can
// only blindly anchor all of these txes down. The caller needs to check the
// returned values against nil to decide whether there exists an anchor
// resolution for local/remote/pending remote commitment txes.
func (lc *LightningChannel) NewAnchorResolutions() (*AnchorResolutions,
	error) {

	lc.Lock()
	defer lc.Unlock()

	var resolutions AnchorResolutions

	// Add anchor for local commitment tx, if any.
	revocation, err := lc.channelState.RevocationProducer.AtIndex(
		lc.currentHeight,
	)
	if err != nil {
		return nil, err
	}
	localCommitPoint := input.ComputeCommitmentPoint(revocation[:])
	localKeyRing := DeriveCommitmentKeys(
		localCommitPoint, lntypes.Local, lc.channelState.ChanType,
		&lc.channelState.LocalChanCfg, &lc.channelState.RemoteChanCfg,
	)
	localRes, err := NewAnchorResolution(
		lc.channelState, lc.channelState.LocalCommitment.CommitTx,
		localKeyRing, lntypes.Local,
	)
	if err != nil {
		return nil, err
	}
	resolutions.Local = localRes

	// Add anchor for remote commitment tx, if any.
	remoteKeyRing := DeriveCommitmentKeys(
		lc.channelState.RemoteCurrentRevocation, lntypes.Remote,
		lc.channelState.ChanType, &lc.channelState.LocalChanCfg,
		&lc.channelState.RemoteChanCfg,
	)
	remoteRes, err := NewAnchorResolution(
		lc.channelState, lc.channelState.RemoteCommitment.CommitTx,
		remoteKeyRing, lntypes.Remote,
	)
	if err != nil {
		return nil, err
	}
	resolutions.Remote = remoteRes

	// Add anchor for remote pending commitment tx, if any.
	remotePendingCommit, err := lc.channelState.RemoteCommitChainTip()
	if err != nil && err != channeldb.ErrNoPendingCommit {
		return nil, err
	}

	if remotePendingCommit != nil {
		pendingRemoteKeyRing := DeriveCommitmentKeys(
			lc.channelState.RemoteNextRevocation, lntypes.Remote,
			lc.channelState.ChanType, &lc.channelState.LocalChanCfg,
			&lc.channelState.RemoteChanCfg,
		)
		remotePendingRes, err := NewAnchorResolution(
			lc.channelState,
			remotePendingCommit.Commitment.CommitTx,
			pendingRemoteKeyRing, lntypes.Remote,
		)
		if err != nil {
			return nil, err
		}
		resolutions.RemotePending = remotePendingRes
	}

	return &resolutions, nil
}

// NewAnchorResolution returns the information that is required to sweep the
// local anchor.
func NewAnchorResolution(chanState *channeldb.OpenChannel,
	commitTx *wire.MsgTx, keyRing *CommitmentKeyRing,
	whoseCommit lntypes.ChannelParty) (*AnchorResolution, error) {

	// Return nil resolution if the channel has no anchors.
	if !chanState.ChanType.HasAnchors() {
		return nil, nil
	}

	// Derive our local anchor script. For taproot channels, rather than
	// use the same multi-sig key for both commitments, the anchor script
	// will differ depending on if this is our local or remote
	// commitment.
	localAnchor, remoteAnchor, err := CommitScriptAnchors(
		chanState.ChanType, &chanState.LocalChanCfg,
		&chanState.RemoteChanCfg, keyRing,
	)
	if err != nil {
		return nil, err
	}
	if chanState.ChanType.IsTaproot() && whoseCommit.IsRemote() {
		//nolint:ineffassign
		localAnchor, remoteAnchor = remoteAnchor, localAnchor
	}

	// TODO(roasbeef): remote anchor not needed above

	// Look up the script on the commitment transaction. It may not be
	// present if there is no output paying to us.
	found, index := input.FindScriptOutputIndex(
		commitTx, localAnchor.PkScript(),
	)
	if !found {
		return nil, nil
	}

	// For anchor outputs, we'll only ever care about the success path.
	// script (sweep after 1 block csv delay).
	anchorWitnessScript, err := localAnchor.WitnessScriptForPath(
		input.ScriptPathSuccess,
	)
	if err != nil {
		return nil, err
	}

	outPoint := &wire.OutPoint{
		Hash:  commitTx.TxHash(),
		Index: index,
	}

	// Instantiate the sign descriptor that allows sweeping of the anchor.
	signDesc := &input.SignDescriptor{
		KeyDesc:       chanState.LocalChanCfg.MultiSigKey,
		WitnessScript: anchorWitnessScript,
		Output: &wire.TxOut{
			PkScript: localAnchor.PkScript(),
			Value:    int64(AnchorSize),
		},
		HashType: sweepSigHash(chanState.ChanType),
	}

	// For taproot outputs, we'll need to ensure that the proper sign
	// method is used, and the tweak as well.
	if scriptTree, ok := localAnchor.(input.TapscriptDescriptor); ok {
		signDesc.SignMethod = input.TaprootKeySpendSignMethod

		//nolint:ll
		signDesc.PrevOutputFetcher = txscript.NewCannedPrevOutputFetcher(
			localAnchor.PkScript(), int64(AnchorSize),
		)

		// For anchor outputs with taproot channels, the key desc is
		// also different: we'll just re-use our local delay base point
		// (which becomes our to local output).
		if whoseCommit.IsLocal() {
			// In addition to the sign method, we'll also need to
			// ensure that the single tweak is set, as with the
			// current formulation, we'll need to use two levels of
			// tweaks: the normal LN tweak, and the tapscript
			// tweak.
			signDesc.SingleTweak = keyRing.LocalCommitKeyTweak

			signDesc.KeyDesc = chanState.LocalChanCfg.DelayBasePoint
		} else {
			// When we're playing the force close of a remote
			// commitment, as this is a "tweakless" channel type,
			// we don't need a tweak value at all.
			//
			//nolint:ll
			signDesc.KeyDesc = chanState.LocalChanCfg.PaymentBasePoint
		}

		// Finally, as this is a keyspend method, we'll need to also
		// include the taptweak as well.
		signDesc.TapTweak = scriptTree.TapTweak()
	}

	var witnessWeight int64
	if chanState.ChanType.IsTaproot() {
		witnessWeight = input.TaprootKeyPathWitnessSize
	} else {
		witnessWeight = input.WitnessCommitmentTxWeight
	}

	// Calculate commit tx weight. This commit tx doesn't yet include the
	// witness spending the funding output, so we add the (worst case)
	// weight for that too.
	utx := btcutil.NewTx(commitTx)
	weight := blockchain.GetTransactionWeight(utx) + witnessWeight

	// Calculate commit tx fee.
	fee := chanState.Capacity
	for _, out := range commitTx.TxOut {
		fee -= btcutil.Amount(out.Value)
	}

	return &AnchorResolution{
		CommitAnchor:         *outPoint,
		AnchorSignDescriptor: *signDesc,
		CommitWeight:         lntypes.WeightUnit(weight),
		CommitFee:            fee,
	}, nil
}

// AvailableBalance returns the current balance available for sending within
// the channel. By available balance, we mean that if at this very instance a
// new commitment were to be created which evals all the log entries, what
// would our available balance for adding an additional HTLC be. It takes into
// account the fee that must be paid for adding this HTLC, that we cannot spend
// from the channel reserve and moreover the FeeBuffer when we are the
// initiator of the channel. This method is useful when deciding if a given
// channel can accept an HTLC in the multi-hop forwarding scenario.
func (lc *LightningChannel) AvailableBalance() lnwire.MilliSatoshi {
	lc.RLock()
	defer lc.RUnlock()

	bal, _ := lc.availableBalance(FeeBuffer)
	return bal
}

// availableBalance is the private, non mutexed version of AvailableBalance.
// This method is provided so methods that already hold the lock can access
// this method. Additionally, the total weight of the next to be created
// commitment is returned for accounting purposes.
func (lc *LightningChannel) availableBalance(
	buffer BufferType) (lnwire.MilliSatoshi, lntypes.WeightUnit) {

	// We'll grab the current set of log updates that the remote has
	// ACKed.
	remoteACKedIndex := lc.commitChains.Local.tip().messageIndices.Remote
	htlcView := lc.fetchHTLCView(remoteACKedIndex,
		lc.updateLogs.Local.logIndex)

	// Calculate our available balance from our local commitment.
	// TODO(halseth): could reuse parts validateCommitmentSanity to do this
	// balance calculation, as most of the logic is the same.
	//
	// NOTE: This is not always accurate, since the remote node can always
	// add updates concurrently, causing our balance to go down if we're
	// the initiator, but this is a problem on the protocol level.
	ourLocalCommitBalance, commitWeight := lc.availableCommitmentBalance(
		htlcView, lntypes.Local, buffer,
	)

	// Do the same calculation from the remote commitment point of view.
	ourRemoteCommitBalance, _ := lc.availableCommitmentBalance(
		htlcView, lntypes.Remote, buffer,
	)

	// Return which ever balance is lowest.
	if ourRemoteCommitBalance < ourLocalCommitBalance {
		return ourRemoteCommitBalance, commitWeight
	}

	return ourLocalCommitBalance, commitWeight
}

// availableCommitmentBalance attempts to calculate the balance we have
// available for HTLCs on the local/remote commitment given the HtlcView. To
// account for sending HTLCs of different sizes, it will report the balance
// available for sending non-dust HTLCs, which will be manifested on the
// commitment, increasing the commitment fee we must pay as an initiator,
// eating into our balance. It will make sure we won't violate the channel
// reserve constraints for this amount.
func (lc *LightningChannel) availableCommitmentBalance(view *HtlcView,
	whoseCommitChain lntypes.ChannelParty, buffer BufferType) (
	lnwire.MilliSatoshi, lntypes.WeightUnit) {

	// Compute the current balances for this commitment. This will take
	// into account HTLCs to determine the commit weight, which the
	// initiator must pay the fee for.
	ourBalance, theirBalance, commitWeight, filteredView, err := lc.computeView(
		view, whoseCommitChain, false,
		fn.None[chainfee.SatPerKWeight](),
	)
	if err != nil {
		lc.log.Errorf("Unable to fetch available balance: %v", err)
		return 0, 0
	}

	// We can never spend from the channel reserve, so we'll subtract it
	// from our available balance.
	ourReserve := lnwire.NewMSatFromSatoshis(
		lc.channelState.LocalChanCfg.ChanReserve,
	)
	if ourReserve <= ourBalance {
		ourBalance -= ourReserve
	} else {
		ourBalance = 0
	}

	// Calculate the commitment fee in the case where we would add another
	// HTLC to the commitment, as only the balance remaining after this fee
	// has been paid is actually available for sending.
	feePerKw := filteredView.FeePerKw
	additionalHtlcFee := lnwire.NewMSatFromSatoshis(
		feePerKw.FeeForWeight(input.HTLCWeight),
	)
	commitFee := lnwire.NewMSatFromSatoshis(
		feePerKw.FeeForWeight(commitWeight))

	if lc.channelState.IsInitiator {
		// When the buffer is of type `FeeBuffer` type we know we are
		// going to send or forward an htlc over this channel therefore
		// we account for an additional htlc output on the commitment
		// tx.
		futureCommitWeight := commitWeight
		if buffer == FeeBuffer {
			futureCommitWeight += input.HTLCWeight
		}

		// Make sure we do not overwrite `ourBalance` that's why we
		// declare bufferAmt beforehand.
		var bufferAmt lnwire.MilliSatoshi
		ourBalance, bufferAmt, commitFee, err = lc.applyCommitFee(
			ourBalance, futureCommitWeight, feePerKw, buffer,
		)
		if err != nil {
			lc.log.Debugf("No available local balance after "+
				"applying the CommitmentFee of the new "+
				"CommitmentState(%v): ourBalance would drop "+
				"below the reserve: "+
				"ourBalance(w/o reserve)=%v, reserve=%v, "+
				"current commitFee(w/o additional htlc)=%v, "+
				"feeBuffer=%v (type=%v) "+
				"local_chan_initiator=%v", whoseCommitChain,
				ourBalance, ourReserve, commitFee, bufferAmt,
				buffer, lc.channelState.IsInitiator)

			return 0, commitWeight
		}

		return ourBalance, commitWeight
	}

	// If we're not the initiator, we must check whether the remote has
	// enough balance to pay for the fee of our HTLC. We'll start by also
	// subtracting our counterparty's reserve from their balance.
	theirReserve := lnwire.NewMSatFromSatoshis(
		lc.channelState.RemoteChanCfg.ChanReserve,
	)
	if theirReserve <= theirBalance {
		theirBalance -= theirReserve
	} else {
		theirBalance = 0
	}

	// We'll use the dustlimit and htlcFee to find the largest HTLC value
	// that will be considered dust on the commitment.
	dustlimit := lnwire.NewMSatFromSatoshis(
		lc.channelState.LocalChanCfg.DustLimit,
	)

	// For an extra HTLC fee to be paid on our commitment, the HTLC must be
	// large enough to make a non-dust HTLC timeout transaction.
	htlcFee := lnwire.NewMSatFromSatoshis(
		HtlcTimeoutFee(lc.channelState.ChanType, feePerKw),
	)

	// If we are looking at the remote commitment, we must use the remote
	// dust limit and the fee for adding an HTLC success transaction.
	if whoseCommitChain.IsRemote() {
		dustlimit = lnwire.NewMSatFromSatoshis(
			lc.channelState.RemoteChanCfg.DustLimit,
		)
		htlcFee = lnwire.NewMSatFromSatoshis(
			HtlcSuccessFee(lc.channelState.ChanType, feePerKw),
		)
	}

	// The HTLC output will be manifested on the commitment if it
	// is non-dust after paying the HTLC fee.
	nonDustHtlcAmt := dustlimit + htlcFee

	// commitFeeWithHtlc is the fee our peer has to pay in case we add
	// another htlc to the commitment.
	commitFeeWithHtlc := commitFee + additionalHtlcFee

	// If they cannot pay the fee if we add another non-dust HTLC, we'll
	// report our available balance just below the non-dust amount, to
	// avoid attempting HTLCs larger than this size.
	if theirBalance < commitFeeWithHtlc && ourBalance >= nonDustHtlcAmt {
		// see https://github.com/lightning/bolts/issues/728
		ourReportedBalance := nonDustHtlcAmt - 1
		lc.log.Infof("Reducing local (reported) balance "+
			"(from %v to %v): remote side does not have enough "+
			"funds (%v < %v) to pay for non-dust HTLC in case of "+
			"unilateral close.", ourBalance, ourReportedBalance,
			theirBalance, commitFeeWithHtlc)
		ourBalance = ourReportedBalance
	}

	return ourBalance, commitWeight
}

// StateSnapshot returns a snapshot of the current fully committed state within
// the channel.
func (lc *LightningChannel) StateSnapshot() *channeldb.ChannelSnapshot {
	lc.RLock()
	defer lc.RUnlock()

	return lc.channelState.Snapshot()
}

// validateFeeRate ensures that if the passed fee is applied to the channel,
// and a new commitment is created (which evaluates this fee), then the
// initiator of the channel does not dip below their reserve.
func (lc *LightningChannel) validateFeeRate(feePerKw chainfee.SatPerKWeight) error {
	// We'll ensure that we can accommodate this new fee change, yet still
	// be above our reserve balance. Otherwise, we'll reject the fee
	// update.
	// We do not enforce the FeeBuffer here because it was exactly
	// introduced to use this buffer for potential fee rate increases.
	availableBalance, txWeight := lc.availableBalance(AdditionalHtlc)

	oldFee := lnwire.NewMSatFromSatoshis(
		lc.commitChains.Local.tip().feePerKw.FeeForWeight(txWeight),
	)

	// Our base balance is the total amount of satoshis we can commit
	// towards fees before factoring in the channel reserve.
	baseBalance := availableBalance + oldFee

	// Using the weight of the commitment transaction if we were to create
	// a commitment now, we'll compute our remaining balance if we apply
	// this new fee update.
	newFee := lnwire.NewMSatFromSatoshis(
		feePerKw.FeeForWeight(txWeight),
	)

	// If the total fee exceeds our available balance (taking into account
	// the fee from the last state), then we'll reject this update as it
	// would mean we need to trim our entire output.
	if newFee > baseBalance {
		return fmt.Errorf("cannot apply fee_update=%v sat/kw, new fee "+
			"of %v is greater than balance of %v", int64(feePerKw),
			newFee, baseBalance)
	}

	// TODO(halseth): should fail if fee update is unreasonable,
	// as specified in BOLT#2.
	//  * COMMENT(roasbeef): can cross-check with our ideal fee rate

	return nil
}

// UpdateFee initiates a fee update for this channel. Must only be called by
// the channel initiator, and must be called before sending update_fee to
// the remote.
func (lc *LightningChannel) UpdateFee(feePerKw chainfee.SatPerKWeight) error {
	lc.Lock()
	defer lc.Unlock()

	// Only initiator can send fee update, so trying to send one as
	// non-initiator will fail.
	if !lc.channelState.IsInitiator {
		return fmt.Errorf("local fee update as non-initiator")
	}

	// Ensure that the passed fee rate meets our current requirements.
	if err := lc.validateFeeRate(feePerKw); err != nil {
		return err
	}

	pd := &paymentDescriptor{
		ChanID:    lc.ChannelID(),
		LogIndex:  lc.updateLogs.Local.logIndex,
		Amount:    lnwire.NewMSatFromSatoshis(btcutil.Amount(feePerKw)),
		EntryType: FeeUpdate,
	}

	lc.updateLogs.Local.appendUpdate(pd)

	return nil
}

// CommitFeeTotalAt applies a proposed feerate to the channel and returns the
// commitment fee with this new feerate. It does not modify the underlying
// LightningChannel.
func (lc *LightningChannel) CommitFeeTotalAt(
	feePerKw chainfee.SatPerKWeight) (btcutil.Amount, btcutil.Amount,
	error) {

	lc.RLock()
	defer lc.RUnlock()

	dryRunFee := fn.Some[chainfee.SatPerKWeight](feePerKw)

	// We want to grab every update in both update logs to calculate the
	// commitment fees in the worst-case with this fee-rate.
	localIdx := lc.updateLogs.Local.logIndex
	remoteIdx := lc.updateLogs.Remote.logIndex

	localHtlcView := lc.fetchHTLCView(remoteIdx, localIdx)

	var localCommitFee, remoteCommitFee btcutil.Amount

	// Compute the local commitment's weight.
	_, _, localWeight, _, err := lc.computeView(
		localHtlcView, lntypes.Local, false, dryRunFee,
	)
	if err != nil {
		return 0, 0, err
	}

	localCommitFee = feePerKw.FeeForWeight(localWeight)

	// Create another view in case for some reason the prior one was
	// mutated.
	remoteHtlcView := lc.fetchHTLCView(remoteIdx, localIdx)

	// Compute the remote commitment's weight.
	_, _, remoteWeight, _, err := lc.computeView(
		remoteHtlcView, lntypes.Remote, false, dryRunFee,
	)
	if err != nil {
		return 0, 0, err
	}

	remoteCommitFee = feePerKw.FeeForWeight(remoteWeight)

	return localCommitFee, remoteCommitFee, err
}

// ReceiveUpdateFee handles an updated fee sent from remote. This method will
// return an error if called as channel initiator.
func (lc *LightningChannel) ReceiveUpdateFee(feePerKw chainfee.SatPerKWeight) error {
	lc.Lock()
	defer lc.Unlock()

	// Only initiator can send fee update, and we must fail if we receive
	// fee update as initiator
	if lc.channelState.IsInitiator {
		return fmt.Errorf("received fee update as initiator")
	}

	// TODO(roasbeef): or just modify to use the other balance?
	pd := &paymentDescriptor{
		ChanID:    lc.ChannelID(),
		LogIndex:  lc.updateLogs.Remote.logIndex,
		Amount:    lnwire.NewMSatFromSatoshis(btcutil.Amount(feePerKw)),
		EntryType: FeeUpdate,
	}

	lc.updateLogs.Remote.appendUpdate(pd)

	return nil
}

// generateRevocation generates the revocation message for a given height.
func (lc *LightningChannel) generateRevocation(height uint64) (*lnwire.RevokeAndAck,
	error) {

	// Now that we've accept a new state transition, we send the remote
	// party the revocation for our current commitment state.
	revocationMsg := &lnwire.RevokeAndAck{}
	commitSecret, err := lc.channelState.RevocationProducer.AtIndex(height)
	if err != nil {
		return nil, err
	}
	copy(revocationMsg.Revocation[:], commitSecret[:])

	// Along with this revocation, we'll also send the _next_ commitment
	// point that the remote party should use to create our next commitment
	// transaction. We use a +2 here as we already gave them a look ahead
	// of size one after the ChannelReady message was sent:
	//
	// 0: current revocation, 1: their "next" revocation, 2: this revocation
	//
	// We're revoking the current revocation. Once they receive this
	// message they'll set the "current" revocation for us to their stored
	// "next" revocation, and this revocation will become their new "next"
	// revocation.
	//
	// Put simply in the window slides to the left by one.
	revHeight := height + 2
	nextCommitSecret, err := lc.channelState.RevocationProducer.AtIndex(
		revHeight,
	)
	if err != nil {
		return nil, err
	}

	revocationMsg.NextRevocationKey = input.ComputeCommitmentPoint(nextCommitSecret[:])
	revocationMsg.ChanID = lnwire.NewChanIDFromOutPoint(
		lc.channelState.FundingOutpoint,
	)

	// If this is a taproot channel, then we also need to generate the
	// verification nonce for this target state.
	if lc.channelState.ChanType.IsTaproot() {
		nextVerificationNonce, err := channeldb.NewMusigVerificationNonce( //nolint:ll
			lc.channelState.LocalChanCfg.MultiSigKey.PubKey,
			revHeight, lc.taprootNonceProducer,
		)
		if err != nil {
			return nil, err
		}
		revocationMsg.LocalNonce = lnwire.SomeMusig2Nonce(
			nextVerificationNonce.PubNonce,
		)
	}

	return revocationMsg, nil
}

// closeTxOpts houses the set of options that modify how the cooperative close
// tx is to be constructed.
type closeTxOpts struct {
	// enableRBF indicates whether the cooperative close tx should signal
	// RBF or not.
	enableRBF bool

	// extraCloseOutputs is a set of additional outputs that should be
	// added the co-op close transaction.
	extraCloseOutputs []CloseOutput

	// customSort is a custom function that can be used to sort the
	// transaction outputs. If this isn't set, then the default BIP-69
	// sorting is used.
	customSort CloseSortFunc

	// customSequence is an optional custom sequence to set on the co-op
	// close transaction. This gives slightly more control compared to the
	// enableRBF option.
	customSequence fn.Option[uint32]

	customLockTime fn.Option[uint32]
}

// defaultCloseTxOpts returns a closeTxOpts struct with default values.
func defaultCloseTxOpts() closeTxOpts {
	return closeTxOpts{
		enableRBF: false,
	}
}

// CloseTxOpt is a functional option that allows us to modify how the closing
// transaction is created.
type CloseTxOpt func(*closeTxOpts)

// WithRBFCloseTx signals that the cooperative close tx should signal RBF.
func WithRBFCloseTx() CloseTxOpt {
	return func(o *closeTxOpts) {
		o.enableRBF = true
	}
}

// WithExtraTxCloseOutputs can be used to add extra outputs to the cooperative
// transaction.
func WithExtraTxCloseOutputs(extraOutputs []CloseOutput) CloseTxOpt {
	return func(o *closeTxOpts) {
		o.extraCloseOutputs = extraOutputs
	}
}

// WithCustomTxSort can be used to modify the way the close transaction is
// sorted.
func WithCustomTxSort(sorter CloseSortFunc) CloseTxOpt {
	return func(opts *closeTxOpts) {
		opts.customSort = sorter
	}
}

// WithCustomTxInSequence allows a caller to set a custom sequence on the sole
// input of the co-op close tx.
func WithCustomTxInSequence(sequence uint32) CloseTxOpt {
	return func(o *closeTxOpts) {
		o.customSequence = fn.Some(sequence)
	}
}

func WithCustomTxLockTime(lockTime uint32) CloseTxOpt {
	return func(o *closeTxOpts) {
		o.customLockTime = fn.Some(lockTime)
	}
}

// CreateCooperativeCloseTx creates a transaction which if signed by both
// parties, then broadcast cooperatively closes an active channel. The creation
// of the closure transaction is modified by a boolean indicating if the party
// constructing the channel is the initiator of the closure. Currently it is
// expected that the initiator pays the transaction fees for the closing
// transaction in full.
func CreateCooperativeCloseTx(fundingTxIn wire.TxIn,
	localDust, remoteDust, ourBalance, theirBalance btcutil.Amount,
	ourDeliveryScript, theirDeliveryScript []byte,
	closeOpts ...CloseTxOpt) (*wire.MsgTx, error) {

	opts := defaultCloseTxOpts()
	for _, optFunc := range closeOpts {
		optFunc(&opts)
	}

	// If RBF is signalled, then we'll modify the sequence to permit
	// replacement.
	if opts.enableRBF {
		fundingTxIn.Sequence = mempool.MaxRBFSequence
	}

	// Otherwise, a custom sequence might be specified.
	opts.customSequence.WhenSome(func(sequence uint32) {
		fundingTxIn.Sequence = sequence
	})

	// Construct the transaction to perform a cooperative closure of the
	// channel. In the event that one side doesn't have any settled funds
	// within the channel then a refund output for that particular side can
	// be omitted.
	closeTx := wire.NewMsgTx(2)
	closeTx.AddTxIn(&fundingTxIn)

	opts.customLockTime.WhenSome(func(lockTime uint32) {
		closeTx.LockTime = lockTime
	})

	// Create both cooperative closure outputs, properly respecting the dust
	// limits of both parties.
	var localOutputIdx fn.Option[int]
	haveLocalOutput := ourBalance >= localDust
	if haveLocalOutput {
		// If our script is an OP_RETURN, then we set our balance to
		// zero.
		if opts.customSequence.IsSome() &&
			input.ScriptIsOpReturn(ourDeliveryScript) {

			ourBalance = 0
		}

		closeTx.AddTxOut(&wire.TxOut{
			PkScript: ourDeliveryScript,
			Value:    int64(ourBalance),
		})

		localOutputIdx = fn.Some(len(closeTx.TxOut) - 1)
	}

	var remoteOutputIdx fn.Option[int]
	haveRemoteOutput := theirBalance >= remoteDust
	if haveRemoteOutput {
		// If a party's script is an OP_RETURN, then we set their
		// balance to zero.
		if opts.customSequence.IsSome() &&
			input.ScriptIsOpReturn(theirDeliveryScript) {

			theirBalance = 0
		}

		closeTx.AddTxOut(&wire.TxOut{
			PkScript: theirDeliveryScript,
			Value:    int64(theirBalance),
		})

		remoteOutputIdx = fn.Some(len(closeTx.TxOut) - 1)
	}

	// If we have extra outputs to add to the co-op close transaction, then
	// we'll examine them now. We'll deduct the output's value from the
	// owning party. In the case that a party can't pay for the output, then
	// their normal output will be omitted.
	for _, extraTxOut := range opts.extraCloseOutputs {
		switch {
		// For additional local outputs, add the output, then deduct
		// the balance from our local balance.
		case extraTxOut.IsLocal:
			// The extraCloseOutputs in the options just indicate if
			// an extra output should be added in general. But we
			// only add one if we actually _need_ one, based on the
			// balance. If we don't have enough local balance to
			// cover the extra output, then localOutputIdx is None.
			localOutputIdx.WhenSome(func(idx int) {
				// The output that currently represents the
				// local balance, which means:
				// 	txOut.Value == ourBalance.
				txOut := closeTx.TxOut[idx]

				// The extra output (if one exists) is the more
				// important one, as in custom channels it might
				// carry some additional values. The normal
				// output is just an address that sends the
				// local balance back to our wallet. The extra
				// one also goes to our wallet, but might also
				// carry other values, so it has higher
				// priority. Do we have enough balance to have
				// both the extra output with the given value
				// (which is subtracted from our balance) and
				// still an above-dust normal output? If not, we
				// skip the extra output and just overwrite the
				// existing output script with the one from the
				// extra output.
				amtAfterOutput := btcutil.Amount(
					txOut.Value - extraTxOut.Value,
				)
				if amtAfterOutput <= localDust {
					txOut.PkScript = extraTxOut.PkScript

					return
				}

				txOut.Value -= extraTxOut.Value
				closeTx.AddTxOut(&extraTxOut.TxOut)
			})

		// For extra remote outputs, we'll do the opposite.
		case !extraTxOut.IsLocal:
			// The extraCloseOutputs in the options just indicate if
			// an extra output should be added in general. But we
			// only add one if we actually _need_ one, based on the
			// balance. If we don't have enough remote balance to
			// cover the extra output, then remoteOutputIdx is None.
			remoteOutputIdx.WhenSome(func(idx int) {
				// The output that currently represents the
				// remote balance, which means:
				// 	txOut.Value == theirBalance.
				txOut := closeTx.TxOut[idx]

				// The extra output (if one exists) is the more
				// important one, as in custom channels it might
				// carry some additional values. The normal
				// output is just an address that sends the
				// remote balance back to their wallet. The
				// extra one also goes to their wallet, but
				// might also carry other values, so it has
				// higher priority. Do they have enough balance
				// to have both the extra output with the given
				// value (which is subtracted from their
				// balance) and still an above-dust normal
				// output? If not, we skip the extra output and
				// just overwrite the existing output script
				// with the one from the extra output.
				amtAfterOutput := btcutil.Amount(
					txOut.Value - extraTxOut.Value,
				)
				if amtAfterOutput <= remoteDust {
					txOut.PkScript = extraTxOut.PkScript

					return
				}

				txOut.Value -= extraTxOut.Value
				closeTx.AddTxOut(&extraTxOut.TxOut)
			})
		}
	}

	if opts.customSort != nil {
		if err := opts.customSort(closeTx); err != nil {
			return nil, err
		}
	} else {
		txsort.InPlaceSort(closeTx)
	}

	return closeTx, nil
}

// LocalBalanceDust returns true if when creating a co-op close transaction,
// the balance of the local party will be dust after accounting for any anchor
// outputs.
func (lc *LightningChannel) LocalBalanceDust() (bool, btcutil.Amount) {
	lc.RLock()
	defer lc.RUnlock()

	chanState := lc.channelState
	localBalance := chanState.LocalCommitment.LocalBalance.ToSatoshis()

	// If this is an anchor channel, and we're the initiator, then we'll
	// regain the stats allocated to the anchor outputs with the co-op
	// close transaction.
	if chanState.ChanType.HasAnchors() && chanState.IsInitiator {
		localBalance += 2 * AnchorSize
	}

	localDust := chanState.LocalChanCfg.DustLimit

	return localBalance <= localDust, localDust
}

// RemoteBalanceDust returns true if when creating a co-op close transaction,
// the balance of the remote party will be dust after accounting for any anchor
// outputs.
func (lc *LightningChannel) RemoteBalanceDust() (bool, btcutil.Amount) {
	lc.RLock()
	defer lc.RUnlock()

	chanState := lc.channelState
	remoteBalance := chanState.RemoteCommitment.RemoteBalance.ToSatoshis()

	// If this is an anchor channel, and they're the initiator, then we'll
	// regain the stats allocated to the anchor outputs with the co-op
	// close transaction.
	if chanState.ChanType.HasAnchors() && !chanState.IsInitiator {
		remoteBalance += 2 * AnchorSize
	}

	remoteDust := chanState.RemoteChanCfg.DustLimit

	return remoteBalance <= remoteDust, remoteDust
}

// CommitBalances returns the local and remote balances in the current
// commitment state.
func (lc *LightningChannel) CommitBalances() (btcutil.Amount, btcutil.Amount) {
	lc.RLock()
	defer lc.RUnlock()

	chanState := lc.channelState
	localCommit := lc.channelState.LocalCommitment

	localBalance := localCommit.LocalBalance.ToSatoshis()
	remoteBalance := localCommit.RemoteBalance.ToSatoshis()

	if chanState.ChanType.HasAnchors() {
		if chanState.IsInitiator {
			localBalance += 2 * AnchorSize
		} else {
			remoteBalance += 2 * AnchorSize
		}
	}

	return localBalance, remoteBalance
}

// CommitFee returns the commitment fee for the current commitment state.
func (lc *LightningChannel) CommitFee() btcutil.Amount {
	lc.RLock()
	defer lc.RUnlock()

	return lc.channelState.LocalCommitment.CommitFee
}

// CalcFee returns the commitment fee to use for the given fee rate
// (fee-per-kw).
func (lc *LightningChannel) CalcFee(feeRate chainfee.SatPerKWeight) btcutil.Amount {
	return feeRate.FeeForWeight(CommitWeight(lc.channelState.ChanType))
}

// MaxFeeRate returns the maximum fee rate given an allocation of the channel
// initiator's spendable balance along with the local reserve amount. This can
// be useful to determine when we should stop proposing fee updates that exceed
// our maximum allocation.
// Moreover it returns the share of the total balance in the range of [0,1]
// which can be allocated to fees. When our desired fee allocation would lead to
// a maximum fee rate below the current commitment fee rate we floor the maximum
// at the current fee rate which leads to different fee allocations than
// initially requested via `maxAllocation`.
//
// NOTE: This should only be used for channels in which the local commitment is
// the initiator.
func (lc *LightningChannel) MaxFeeRate(
	maxAllocation float64) (chainfee.SatPerKWeight, float64) {

	lc.RLock()
	defer lc.RUnlock()

	// The maximum fee depends on the available balance that can be
	// committed towards fees. It takes into account our local reserve
	// balance. We do not account for a FeeBuffer here because that is
	// exactly why it was introduced to react for sharp fee changes.
	availableBalance, weight := lc.availableBalance(AdditionalHtlc)

	currentFee := lc.commitChains.Local.tip().feePerKw.FeeForWeight(weight)

	// baseBalance is the maximum amount available for us to spend on fees.
	baseBalance := availableBalance.ToSatoshis() + currentFee

	// In case our local channel balance is drained, we make sure we do not
	// decrease the fee rate below the current fee rate. This could lead to
	// a scenario where we lower the commitment fee rate as low as the fee
	// floor although current fee rates are way higher. The maximum fee
	// we allow should not be smaller then the current fee. The decrease
	// in fee rate should happen when the mempool reports lower fee levels
	// rather than us decreasing in local balance. The max fee rate is
	// always floored by the current fee rate of the channel.
	idealMaxFee := float64(baseBalance) * maxAllocation
	maxFee := max(float64(currentFee), idealMaxFee)
	maxFeeAllocation := maxFee / float64(baseBalance)
	maxFeeRate := chainfee.SatPerKWeight(maxFee / (float64(weight) / 1000))

	return maxFeeRate, maxFeeAllocation
}

// IdealCommitFeeRate uses the current network fee, the minimum relay fee,
// maximum fee allocation and anchor channel commitment fee rate to determine
// the ideal fee to be used for the commitments of the channel.
func (lc *LightningChannel) IdealCommitFeeRate(netFeeRate, minRelayFeeRate,
	maxAnchorCommitFeeRate chainfee.SatPerKWeight,
	maxFeeAlloc float64) chainfee.SatPerKWeight {

	// Get the maximum fee rate that we can use given our max fee allocation
	// and given the local reserve balance that we must preserve.
	maxFeeRate, _ := lc.MaxFeeRate(maxFeeAlloc)

	var commitFeeRate chainfee.SatPerKWeight

	// If the channel has anchor outputs then cap the fee rate at the
	// max anchor fee rate if that maximum is less than our max fee rate.
	// Otherwise, cap the fee rate at the max fee rate.
	switch lc.channelState.ChanType.HasAnchors() &&
		maxFeeRate > maxAnchorCommitFeeRate {
	case true:
		commitFeeRate = min(netFeeRate, maxAnchorCommitFeeRate)

	case false:
		commitFeeRate = min(netFeeRate, maxFeeRate)
	}

	if commitFeeRate >= minRelayFeeRate {
		return commitFeeRate
	}

	// The commitment fee rate is below the minimum relay fee rate.
	// If the min relay fee rate is still below the maximum fee, then use
	// the minimum relay fee rate.
	if minRelayFeeRate <= maxFeeRate {
		return minRelayFeeRate
	}

	// The minimum relay fee rate is more than the ideal maximum fee rate.
	// Check if it is smaller than the absolute maximum fee rate we can
	// use. If it is, then we use the minimum relay fee rate and we log a
	// warning to indicate that the max channel fee allocation option was
	// ignored.
	absoluteMaxFee, _ := lc.MaxFeeRate(1)
	if minRelayFeeRate <= absoluteMaxFee {
		lc.log.Warn("Ignoring max channel fee allocation to " +
			"ensure that the commitment fee is above the " +
			"minimum relay fee.")

		return minRelayFeeRate
	}

	// The absolute maximum fee rate we can pay is below the minimum
	// relay fee rate. The commitment tx will not be able to propagate.
	// To give the transaction the best chance, we use the absolute
	// maximum fee we have available and we log an error.
	lc.log.Errorf("The commitment fee rate of %s is below the current "+
		"minimum relay fee rate of %s. The max fee rate of %s will be "+
		"used.", commitFeeRate, minRelayFeeRate, absoluteMaxFee)

	return absoluteMaxFee
}

// RemoteNextRevocation returns the channelState's RemoteNextRevocation. For
// musig2 channels, until a nonce pair is processed by the remote party, a nil
// public key is returned.
//
// TODO(roasbeef): revisit, maybe just make a more general method instead?
func (lc *LightningChannel) RemoteNextRevocation() *btcec.PublicKey {
	lc.RLock()
	defer lc.RUnlock()

	if !lc.channelState.ChanType.IsTaproot() {
		return lc.channelState.RemoteNextRevocation
	}

	if lc.musigSessions == nil {
		return nil
	}

	return lc.channelState.RemoteNextRevocation
}

// IsInitiator returns true if we were the ones that initiated the funding
// workflow which led to the creation of this channel. Otherwise, it returns
// false.
func (lc *LightningChannel) IsInitiator() bool {
	lc.RLock()
	defer lc.RUnlock()

	return lc.channelState.IsInitiator
}

// CommitFeeRate returns the current fee rate of the commitment transaction in
// units of sat-per-kw.
func (lc *LightningChannel) CommitFeeRate() chainfee.SatPerKWeight {
	lc.RLock()
	defer lc.RUnlock()

	return chainfee.SatPerKWeight(lc.channelState.LocalCommitment.FeePerKw)
}

// WorstCaseFeeRate returns the higher feerate from either the local commitment
// or the remote commitment.
func (lc *LightningChannel) WorstCaseFeeRate() chainfee.SatPerKWeight {
	lc.RLock()
	defer lc.RUnlock()

	localFeeRate := lc.channelState.LocalCommitment.FeePerKw
	remoteFeeRate := lc.channelState.RemoteCommitment.FeePerKw

	if localFeeRate > remoteFeeRate {
		return chainfee.SatPerKWeight(localFeeRate)
	}

	return chainfee.SatPerKWeight(remoteFeeRate)
}

// IsPending returns true if the channel's funding transaction has been fully
// confirmed, and false otherwise.
func (lc *LightningChannel) IsPending() bool {
	lc.RLock()
	defer lc.RUnlock()

	return lc.channelState.IsPending
}

// State provides access to the channel's internal state.
func (lc *LightningChannel) State() *channeldb.OpenChannel {
	return lc.channelState
}

// MarkBorked marks the event when the channel as reached an irreconcilable
// state, such as a channel breach or state desynchronization. Borked channels
// should never be added to the switch.
func (lc *LightningChannel) MarkBorked() error {
	lc.Lock()
	defer lc.Unlock()

	return lc.channelState.MarkBorked()
}

// MarkCommitmentBroadcasted marks the channel as a commitment transaction has
// been broadcast, either our own or the remote, and we should watch the chain
// for it to confirm before taking any further action. It takes a boolean which
// indicates whether we initiated the close.
func (lc *LightningChannel) MarkCommitmentBroadcasted(tx *wire.MsgTx,
	closer lntypes.ChannelParty) error {

	lc.Lock()
	defer lc.Unlock()

	return lc.channelState.MarkCommitmentBroadcasted(tx, closer)
}

// MarkCoopBroadcasted marks the channel as a cooperative close transaction has
// been broadcast, and that we should watch the chain for it to confirm before
// taking any further action. It takes a locally initiated bool which is true
// if we initiated the cooperative close.
func (lc *LightningChannel) MarkCoopBroadcasted(tx *wire.MsgTx,
	closer lntypes.ChannelParty) error {

	lc.Lock()
	defer lc.Unlock()

	return lc.channelState.MarkCoopBroadcasted(tx, closer)
}

// MarkShutdownSent persists the given ShutdownInfo. The existence of the
// ShutdownInfo represents the fact that the Shutdown message has been sent by
// us and so should be re-sent on re-establish.
func (lc *LightningChannel) MarkShutdownSent(
	info *channeldb.ShutdownInfo) error {

	lc.Lock()
	defer lc.Unlock()

	return lc.channelState.MarkShutdownSent(info)
}

// MarkDataLoss marks sets the channel status to LocalDataLoss and stores the
// passed commitPoint for use to retrieve funds in case the remote force closes
// the channel.
func (lc *LightningChannel) MarkDataLoss(commitPoint *btcec.PublicKey) error {
	lc.Lock()
	defer lc.Unlock()

	return lc.channelState.MarkDataLoss(commitPoint)
}

// ActiveHtlcs returns a slice of HTLC's which are currently active on *both*
// commitment transactions.
func (lc *LightningChannel) ActiveHtlcs() []channeldb.HTLC {
	lc.RLock()
	defer lc.RUnlock()

	return lc.channelState.ActiveHtlcs()
}

// LocalChanReserve returns our local ChanReserve requirement for the remote party.
func (lc *LightningChannel) LocalChanReserve() btcutil.Amount {
	return lc.channelState.LocalChanCfg.ChanReserve
}

// NextLocalHtlcIndex returns the next unallocated local htlc index. To ensure
// this always returns the next index that has been not been allocated, this
// will first try to examine any pending commitments, before falling back to the
// last locked-in local commitment.
func (lc *LightningChannel) NextLocalHtlcIndex() (uint64, error) {
	lc.RLock()
	defer lc.RUnlock()

	return lc.channelState.NextLocalHtlcIndex()
}

// FwdMinHtlc returns the minimum HTLC value required by the remote node, i.e.
// the minimum value HTLC we can forward on this channel.
func (lc *LightningChannel) FwdMinHtlc() lnwire.MilliSatoshi {
	return lc.channelState.LocalChanCfg.MinHTLC
}

// unsignedLocalUpdates retrieves the unsigned local updates that we should
// store upon receiving a revocation. This function is called from
// ReceiveRevocation. remoteMessageIndex is the height into the local update
// log that the remote commitment chain tip includes. localMessageIndex
// is the height into the local update log that the local commitment tail
// includes. Our local updates that are unsigned by the remote should
// have height greater than or equal to localMessageIndex (not on our commit),
// and height less than remoteMessageIndex (on the remote commit).
//
// NOTE: remoteMessageIndex is the height on the tip because this is called
// before the tail is advanced to the tip during ReceiveRevocation.
func (lc *LightningChannel) unsignedLocalUpdates(remoteMessageIndex,
	localMessageIndex uint64) []channeldb.LogUpdate {

	var localPeerUpdates []channeldb.LogUpdate
	for e := lc.updateLogs.Local.Front(); e != nil; e = e.Next() {
		pd := e.Value

		// We don't save add updates as they are restored from the
		// remote commitment in restoreStateLogs.
		if pd.isAdd() {
			continue
		}

		// This is a settle/fail that is on the remote commitment, but
		// not on the local commitment. We expect this update to be
		// covered in the next commitment signature that the remote
		// sends.
		if pd.LogIndex < remoteMessageIndex && pd.LogIndex >= localMessageIndex {
			localPeerUpdates = append(
				localPeerUpdates, pd.toLogUpdate(),
			)
		}
	}

	return localPeerUpdates
}

// GenMusigNonces generates the verification nonce to start off a new musig2
// channel session.
func (lc *LightningChannel) GenMusigNonces() (*musig2.Nonces, error) {
	lc.Lock()
	defer lc.Unlock()

	var err error

	// We pass in the current height+1 as this'll be the set of
	// verification nonces we'll send to the party to create our _next_
	// state.
	lc.pendingVerificationNonce, err = channeldb.NewMusigVerificationNonce(
		lc.channelState.LocalChanCfg.MultiSigKey.PubKey,
		lc.currentHeight+1, lc.taprootNonceProducer,
	)
	if err != nil {
		return nil, err
	}

	return lc.pendingVerificationNonce, nil
}

// HasRemoteNonces returns true if the channel has a remote nonce pair.
func (lc *LightningChannel) HasRemoteNonces() bool {
	return lc.musigSessions != nil
}

// InitRemoteMusigNonces processes the remote musig nonces sent by the remote
// party. This should be called upon connection re-establishment, after we've
// generated our own nonces. Once this method returns a nil error, then the
// channel can be used to sign commitment states.
func (lc *LightningChannel) InitRemoteMusigNonces(remoteNonce *musig2.Nonces,
) error {

	lc.Lock()
	defer lc.Unlock()

	if lc.pendingVerificationNonce == nil {
		return fmt.Errorf("pending verification nonce is not set")
	}

	// Now that we have the set of local and remote nonces, we can generate
	// a new pair of musig sessions for our local commitment and the
	// commitment of the remote party.
	localNonce := lc.pendingVerificationNonce

	localChanCfg := lc.channelState.LocalChanCfg
	remoteChanCfg := lc.channelState.RemoteChanCfg

	// TODO(roasbeef): propagate rename of signing and verification nonces

	sessionCfg := &MusigSessionCfg{
		LocalKey:       localChanCfg.MultiSigKey,
		RemoteKey:      remoteChanCfg.MultiSigKey,
		LocalNonce:     *localNonce,
		RemoteNonce:    *remoteNonce,
		Signer:         lc.Signer,
		InputTxOut:     &lc.fundingOutput,
		TapscriptTweak: lc.channelState.TapscriptRoot,
	}
	lc.musigSessions = NewMusigPairSession(
		sessionCfg,
	)

	lc.pendingVerificationNonce = nil

	lc.opts.localNonce = nil
	lc.opts.remoteNonce = nil

	return nil
}

// ChanType returns the channel type.
func (lc *LightningChannel) ChanType() channeldb.ChannelType {
	lc.RLock()
	defer lc.RUnlock()

	return lc.channelState.ChanType
}

// Initiator returns the ChannelParty that originally opened this channel.
func (lc *LightningChannel) Initiator() lntypes.ChannelParty {
	lc.RLock()
	defer lc.RUnlock()

	return lc.channelState.Initiator()
}

// FundingTxOut returns the funding output of the channel.
func (lc *LightningChannel) FundingTxOut() *wire.TxOut {
	lc.RLock()
	defer lc.RUnlock()

	return &lc.fundingOutput
}

// DeriveHeightHint derives the block height for the channel opening.
func (lc *LightningChannel) DeriveHeightHint() uint32 {
	lc.RLock()
	defer lc.RUnlock()

	return lc.channelState.DeriveHeightHint()
}

// MultiSigKeys returns the set of multi-sig keys for an channel.
func (lc *LightningChannel) MultiSigKeys() (keychain.KeyDescriptor,
	keychain.KeyDescriptor) {

	lc.RLock()
	defer lc.RUnlock()

	return lc.channelState.LocalChanCfg.MultiSigKey,
		lc.channelState.RemoteChanCfg.MultiSigKey
}

// LocalCommitmentBlob returns the custom blob of the local commitment.
func (lc *LightningChannel) LocalCommitmentBlob() fn.Option[tlv.Blob] {
	lc.RLock()
	defer lc.RUnlock()

	chanState := lc.channelState
	localBalance := chanState.LocalCommitment.CustomBlob

	return fn.MapOption(func(b tlv.Blob) tlv.Blob {
		newBlob := make([]byte, len(b))
		copy(newBlob, b)

		return newBlob
	})(localBalance)
}

// FundingBlob returns the funding custom blob.
func (lc *LightningChannel) FundingBlob() fn.Option[tlv.Blob] {
	lc.RLock()
	defer lc.RUnlock()

	return fn.MapOption(func(b tlv.Blob) tlv.Blob {
		newBlob := make([]byte, len(b))
		copy(newBlob, b)

		return newBlob
	})(lc.channelState.CustomBlob)
}

// ZeroConfRealScid returns an optional real scid for the channel. If this
// returns None, then this isn't a zero conf channel. Otherwise, the real scid
// value will be returned.
//
//nolint:ll
func (lc *LightningChannel) ZeroConfRealScid() fn.Option[lnwire.ShortChannelID] {
	if lc.channelState.IsZeroConf() {
		return fn.Some(lc.channelState.ZeroConfRealScid())
	}

	return fn.None[lnwire.ShortChannelID]()
}

// entryTypeForHtlc returns the add type that should be used for adding this
// HTLC to the channel. If the channel has a tapscript root and the HTLC carries
// the NoOp bit in the custom records then we'll convert this to a NoOp add.
func (lc *LightningChannel) entryTypeForHtlc(records lnwire.CustomRecords,
	chanType channeldb.ChannelType) updateType {

	noopTLV := uint64(NoOpHtlcTLVEntry.TypeVal())
	_, noopFlag := records[noopTLV]
	if noopFlag && chanType.HasTapscriptRoot() {
		return NoOpAdd
	}

	if noopFlag && !chanType.HasTapscriptRoot() {
		lc.log.Warnf("Received flag for noop-add over a channel that " +
			"doesn't have a tapscript root")
	}

	return Add
}
