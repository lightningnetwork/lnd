package chanstate

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/lightningnetwork/lnd/tlv"
)

// ChannelShell is a shell of a channel that is meant to be used for channel
// recovery purposes. It contains a minimal OpenChannel instance along with
// addresses for that target node.
type ChannelShell struct {
	// NodeAddrs the set of addresses that this node has known to be
	// reachable at in the past.
	NodeAddrs []net.Addr

	// Chan is a shell of an OpenChannel, it contains only the items
	// required to restore the channel on disk.
	Chan *OpenChannel
}

// OpenChannel encapsulates the persistent and dynamic state of an open channel
// with a remote node. An open channel supports several options for on-disk
// serialization depending on the exact context. Full (upon channel creation)
// state commitments, and partial (due to a commitment update) writes are
// supported. Each partial write due to a state update appends the new update
// to an on-disk log, which can then subsequently be queried in order to
// "time-travel" to a prior state.
type OpenChannel struct {
	// ChanType denotes which type of channel this is.
	ChanType ChannelType

	// ChainHash is a hash which represents the blockchain that this
	// channel will be opened within. This value is typically the genesis
	// hash. In the case that the original chain went through a contentious
	// hard-fork, then this value will be tweaked using the unique fork
	// point on each branch.
	ChainHash chainhash.Hash

	// FundingOutpoint is the outpoint of the final funding transaction.
	// This value uniquely and globally identifies the channel within the
	// target blockchain as specified by the chain hash parameter.
	FundingOutpoint wire.OutPoint

	// ShortChannelID encodes the exact location in the chain in which the
	// channel was initially confirmed. This includes: the block height,
	// transaction index, and the output within the target transaction.
	//
	// If IsZeroConf(), then this will the "base" (very first) ALIAS scid
	// and the confirmed SCID will be stored in ConfirmedScid.
	ShortChannelID lnwire.ShortChannelID

	// IsPending indicates whether a channel's funding transaction has been
	// confirmed.
	IsPending bool

	// IsInitiator is a bool which indicates if we were the original
	// initiator for the channel. This value may affect how higher levels
	// negotiate fees, or close the channel.
	IsInitiator bool

	// chanStatus is the current status of this channel. If it is not in
	// the state Default, it should not be used for forwarding payments.
	chanStatus ChannelStatus

	// FundingBroadcastHeight is the height in which the funding
	// transaction was broadcast. This value can be used by higher level
	// sub-systems to determine if a channel is stale and/or should have
	// been confirmed before a certain height.
	FundingBroadcastHeight uint32

	// ConfirmationHeight records the block height at which the funding
	// transaction was first confirmed.
	ConfirmationHeight uint32

	// CloseConfirmationHeight records the block height at which the closing
	// transaction was first confirmed. This is used to track remaining
	// confirmations until the channel is considered fully closed. It is
	// None if the closing transaction has not yet been confirmed, or if
	// this data was not available (e.g. channels closed before this
	// field was introduced).
	CloseConfirmationHeight fn.Option[uint32]

	// NumConfsRequired is the number of confirmations a channel's funding
	// transaction must have received in order to be considered available
	// for normal transactional use.
	NumConfsRequired uint16

	// ChannelFlags holds the flags that were sent as part of the
	// open_channel message.
	ChannelFlags lnwire.FundingFlag

	// IdentityPub is the identity public key of the remote node this
	// channel has been established with.
	IdentityPub *btcec.PublicKey

	// Capacity is the total capacity of this channel.
	Capacity btcutil.Amount

	// TotalMSatSent is the total number of milli-satoshis we've sent
	// within this channel.
	TotalMSatSent lnwire.MilliSatoshi

	// TotalMSatReceived is the total number of milli-satoshis we've
	// received within this channel.
	TotalMSatReceived lnwire.MilliSatoshi

	// InitialLocalBalance is the balance we have during the channel
	// opening. When we are not the initiator, this value represents the
	// push amount.
	InitialLocalBalance lnwire.MilliSatoshi

	// InitialRemoteBalance is the balance they have during the channel
	// opening.
	InitialRemoteBalance lnwire.MilliSatoshi

	// LocalChanCfg is the channel configuration for the local node.
	LocalChanCfg ChannelConfig

	// RemoteChanCfg is the channel configuration for the remote node.
	RemoteChanCfg ChannelConfig

	// LocalCommitment is the current local commitment state for the local
	// party. This is stored distinct from the state of the remote party
	// as there are certain asymmetric parameters which affect the
	// structure of each commitment.
	LocalCommitment ChannelCommitment

	// RemoteCommitment is the current remote commitment state for the
	// remote party. This is stored distinct from the state of the local
	// party as there are certain asymmetric parameters which affect the
	// structure of each commitment.
	RemoteCommitment ChannelCommitment

	// RemoteCurrentRevocation is the current revocation for their
	// commitment transaction. However, since this the derived public key,
	// we don't yet have the private key so we aren't yet able to verify
	// that it's actually in the hash chain.
	RemoteCurrentRevocation *btcec.PublicKey

	// RemoteNextRevocation is the revocation key to be used for the *next*
	// commitment transaction we create for the local node. Within the
	// specification, this value is referred to as the
	// per-commitment-point.
	RemoteNextRevocation *btcec.PublicKey

	// RevocationProducer is used to generate the revocation in such a way
	// that remote side might store it efficiently and have the ability to
	// restore the revocation by index if needed. Current implementation of
	// secret producer is shachain producer.
	RevocationProducer shachain.Producer

	// RevocationStore is used to efficiently store the revocations for
	// previous channels states sent to us by remote side. Current
	// implementation of secret store is shachain store.
	RevocationStore shachain.Store

	// FundingTxn is the transaction containing this channel's funding
	// outpoint. Upon restarts, this txn will be rebroadcast if the channel
	// is found to be pending.
	//
	// NOTE: This value will only be populated for single-funder channels
	// for which we are the initiator, and that we also have the funding
	// transaction for. One can check this by using the HasFundingTx()
	// method on the ChanType field.
	FundingTxn *wire.MsgTx

	// LocalShutdownScript is set to a pre-set script if the channel was
	// opened by the local node with option_upfront_shutdown_script set. If
	// the option was not set, the field is empty.
	LocalShutdownScript lnwire.DeliveryAddress

	// RemoteShutdownScript is set to a pre-set script if the channel was
	// opened by the remote node with option_upfront_shutdown_script set. If
	// the option was not set, the field is empty.
	RemoteShutdownScript lnwire.DeliveryAddress

	// ThawHeight is the height when a frozen channel once again becomes a
	// normal channel. If this is zero, then there're no restrictions on
	// this channel. If the value is lower than 500,000, then it's
	// interpreted as a relative height, or an absolute height otherwise.
	ThawHeight uint32

	// LastWasRevoke is a boolean that determines if the last update we sent
	// was a revocation (true) or a commitment signature (false).
	LastWasRevoke bool

	// RevocationKeyLocator stores the KeyLocator information that we will
	// need to derive the shachain root for this channel. This allows us to
	// have private key isolation from lnd.
	RevocationKeyLocator keychain.KeyLocator

	// confirmedScid is the confirmed ShortChannelID for a zero-conf
	// channel. If the channel is unconfirmed, then this will be the
	// default ShortChannelID. This is only set for zero-conf channels.
	confirmedScid lnwire.ShortChannelID

	// Memo is any arbitrary information we wish to store locally about the
	// channel that will be useful to our future selves.
	Memo []byte

	// TapscriptRoot is an optional tapscript root used to derive the MuSig2
	// funding output.
	TapscriptRoot fn.Option[chainhash.Hash]

	// CustomBlob is an optional blob that can be used to store information
	// specific to a custom channel type. This information is only created
	// at channel funding time, and after wards is to be considered
	// immutable.
	CustomBlob fn.Option[tlv.Blob]

	// Db persists channel state through the Store contract. This field
	// intentionally keeps the existing name while callers still construct
	// channels through the channeldb compatibility alias. The store
	// interface keeps receiver methods backend independent while the KV
	// implementation is moved into chanstate.
	Db Store

	// TODO(roasbeef): just need to store local and remote HTLC's?

	sync.RWMutex
}

// String returns a string representation of the channel.
func (c *OpenChannel) String() string {
	indexStr := "height=%v, local_htlc_index=%v, local_log_index=%v, " +
		"remote_htlc_index=%v, remote_log_index=%v"

	commit := c.LocalCommitment
	local := fmt.Sprintf(indexStr, commit.CommitHeight,
		commit.LocalHtlcIndex, commit.LocalLogIndex,
		commit.RemoteHtlcIndex, commit.RemoteLogIndex,
	)

	commit = c.RemoteCommitment
	remote := fmt.Sprintf(indexStr, commit.CommitHeight,
		commit.LocalHtlcIndex, commit.LocalLogIndex,
		commit.RemoteHtlcIndex, commit.RemoteLogIndex,
	)

	return fmt.Sprintf("SCID=%v, status=%v, initiator=%v, pending=%v, "+
		"local commitment has %s, remote commitment has %s",
		c.ShortChannelID, c.chanStatus, c.IsInitiator, c.IsPending,
		local, remote,
	)
}

// Initiator returns the ChannelParty that originally opened this channel.
func (c *OpenChannel) Initiator() lntypes.ChannelParty {
	c.RLock()
	defer c.RUnlock()

	if c.IsInitiator {
		return lntypes.Local
	}

	return lntypes.Remote
}

// ShortChanID returns the current ShortChannelID of this channel.
func (c *OpenChannel) ShortChanID() lnwire.ShortChannelID {
	c.RLock()
	defer c.RUnlock()

	return c.ShortChannelID
}

// ZeroConfRealScid returns the zero-conf channel's confirmed scid. This should
// only be called if IsZeroConf returns true.
func (c *OpenChannel) ZeroConfRealScid() lnwire.ShortChannelID {
	c.RLock()
	defer c.RUnlock()

	return c.confirmedScid
}

// ZeroConfConfirmed returns whether the zero-conf channel has confirmed. This
// should only be called if IsZeroConf returns true.
func (c *OpenChannel) ZeroConfConfirmed() bool {
	c.RLock()
	defer c.RUnlock()

	return c.confirmedScid != hop.Source
}

// IsZeroConf returns whether the option_zeroconf channel type was negotiated.
func (c *OpenChannel) IsZeroConf() bool {
	c.RLock()
	defer c.RUnlock()

	return c.ChanType.HasZeroConf()
}

// IsOptionScidAlias returns whether the option_scid_alias channel type was
// negotiated.
func (c *OpenChannel) IsOptionScidAlias() bool {
	c.RLock()
	defer c.RUnlock()

	return c.ChanType.HasScidAliasChan()
}

// NegotiatedAliasFeature returns whether the option-scid-alias feature bit was
// negotiated.
func (c *OpenChannel) NegotiatedAliasFeature() bool {
	c.RLock()
	defer c.RUnlock()

	return c.ChanType.HasScidAliasFeature()
}

// ChanStatus returns the current ChannelStatus of this channel.
func (c *OpenChannel) ChanStatus() ChannelStatus {
	c.RLock()
	defer c.RUnlock()

	return c.chanStatus
}

// ChannelStatusForStore returns the in-memory channel status without taking
// the channel mutex.
//
// NOTE: This is a preliminary migration hook for KV-backed store code while
// OpenChannel persistence moves behind backend-owned stores. Callers are
// responsible for synchronization. Normal callers should use ChanStatus.
func (c *OpenChannel) ChannelStatusForStore() ChannelStatus {
	return c.chanStatus
}

// SetChannelStatusForStore updates the in-memory channel status without taking
// the channel mutex.
//
// NOTE: This is a preliminary migration hook for KV-backed store code while
// OpenChannel persistence moves behind backend-owned stores. Callers are
// responsible for synchronization. Normal callers should use ApplyChanStatus
// or ClearChanStatus when the status change must be persisted.
func (c *OpenChannel) SetChannelStatusForStore(status ChannelStatus) {
	c.chanStatus = status
}

// ApplyChanStatus allows the caller to modify the internal channel state in a
// thead-safe manner.
func (c *OpenChannel) ApplyChanStatus(status ChannelStatus) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.ApplyChannelStatus(c, status)
}

// ClearChanStatus allows the caller to clear a particular channel status from
// the primary channel status bit field. After this method returns, a call to
// HasChanStatus(status) should return false.
func (c *OpenChannel) ClearChanStatus(status ChannelStatus) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.ClearChannelStatus(c, status)
}

// HasChanStatus returns true if the internal bitfield channel status of the
// target channel has the specified status bit set.
func (c *OpenChannel) HasChanStatus(status ChannelStatus) bool {
	c.RLock()
	defer c.RUnlock()

	return c.hasChanStatus(status)
}

func (c *OpenChannel) hasChanStatus(status ChannelStatus) bool {
	// Special case ChanStatusDefualt since it isn't actually flag, but a
	// particular combination (or lack-there-of) of flags.
	if status == ChanStatusDefault {
		return c.chanStatus == ChanStatusDefault
	}

	return c.chanStatus&status == status
}

// HasChanStatusForStore returns true if the internal bitfield channel status
// has the specified status bit set, without taking the channel mutex.
//
// NOTE: This is a preliminary migration hook for KV-backed store code while
// OpenChannel persistence moves behind backend-owned stores. Callers are
// responsible for synchronization. Normal callers should use HasChanStatus.
func (c *OpenChannel) HasChanStatusForStore(status ChannelStatus) bool {
	return c.hasChanStatus(status)
}

// ConfirmedScidForStore returns the in-memory confirmed SCID without taking
// the channel mutex.
//
// NOTE: This is a preliminary migration hook for KV-backed store code while
// OpenChannel persistence moves behind backend-owned stores. Callers are
// responsible for synchronization. Normal callers should use ZeroConfRealScid.
func (c *OpenChannel) ConfirmedScidForStore() lnwire.ShortChannelID {
	return c.confirmedScid
}

// SetConfirmedScidForStore updates the in-memory confirmed SCID without taking
// the channel mutex.
//
// NOTE: This is a preliminary migration hook for KV-backed store code while
// OpenChannel persistence moves behind backend-owned stores. Callers are
// responsible for synchronization.
func (c *OpenChannel) SetConfirmedScidForStore(scid lnwire.ShortChannelID) {
	c.confirmedScid = scid
}

// BroadcastHeight returns the height at which the funding tx was broadcast.
func (c *OpenChannel) BroadcastHeight() uint32 {
	c.RLock()
	defer c.RUnlock()

	return c.FundingBroadcastHeight
}

// FundingTxPresent returns true if expect the funding transcation to be found
// on disk or already populated within the passed open channel struct.
func (c *OpenChannel) FundingTxPresent() bool {
	chanType := c.ChanType

	return chanType.IsSingleFunder() && chanType.HasFundingTx() &&
		c.IsInitiator &&
		!c.HasChanStatusForStore(ChanStatusRestored)
}

// SetBroadcastHeight sets the FundingBroadcastHeight.
func (c *OpenChannel) SetBroadcastHeight(height uint32) {
	c.Lock()
	defer c.Unlock()

	c.FundingBroadcastHeight = height
}

// Refresh updates the in-memory channel state using the latest state observed
// on disk.
func (c *OpenChannel) Refresh() error {
	c.Lock()
	defer c.Unlock()

	return c.Db.RefreshChannel(c)
}

// MarkConfirmationHeight updates the channel's confirmation height once the
// channel opening transaction receives one confirmation.
func (c *OpenChannel) MarkConfirmationHeight(height uint32) error {
	c.Lock()
	defer c.Unlock()

	if err := c.Db.MarkChannelConfirmationHeight(c, height); err != nil {
		return err
	}

	c.ConfirmationHeight = height

	return nil
}

// ResetCloseConfirmationHeight clears the channel's close confirmation height
// when the spending transaction is reorged out.
func (c *OpenChannel) ResetCloseConfirmationHeight() error {
	return c.MarkCloseConfirmationHeight(fn.None[uint32]())
}

// MarkCloseConfirmationHeight updates the channel's close confirmation height
// when the closing transaction is first detected in a block (spend height).
func (c *OpenChannel) MarkCloseConfirmationHeight(
	height fn.Option[uint32]) error {

	c.Lock()
	defer c.Unlock()

	err := c.Db.MarkChannelCloseConfirmationHeight(c, height)
	if err != nil {
		return err
	}

	c.CloseConfirmationHeight = height

	return nil
}

// MarkAsOpen marks a channel as fully open given a locator that uniquely
// describes its location within the chain.
func (c *OpenChannel) MarkAsOpen(openLoc lnwire.ShortChannelID) error {
	c.Lock()
	defer c.Unlock()

	if err := c.Db.MarkChannelOpen(c, openLoc); err != nil {
		return err
	}

	c.IsPending = false
	c.ShortChannelID = openLoc

	return nil
}

// MarkRealScid marks the zero-conf channel's confirmed ShortChannelID. This
// should only be done if IsZeroConf returns true.
func (c *OpenChannel) MarkRealScid(realScid lnwire.ShortChannelID) error {
	c.Lock()
	defer c.Unlock()

	if err := c.Db.MarkChannelRealScid(c, realScid); err != nil {
		return err
	}

	c.confirmedScid = realScid

	return nil
}

// MarkScidAliasNegotiated adds ScidAliasFeatureBit to ChanType in-memory and
// in the database.
func (c *OpenChannel) MarkScidAliasNegotiated() error {
	c.Lock()
	defer c.Unlock()

	if err := c.Db.MarkChannelScidAliasNegotiated(c); err != nil {
		return err
	}

	c.ChanType |= ScidAliasFeatureBit

	return nil
}

// MarkDataLoss marks sets the channel status to LocalDataLoss and stores the
// passed commitPoint for use to retrieve funds in case the remote force closes
// the channel.
func (c *OpenChannel) MarkDataLoss(commitPoint *btcec.PublicKey) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.MarkChannelDataLoss(c, commitPoint)
}

// DataLossCommitPoint retrieves the stored commit point set during
// MarkDataLoss. If not found ErrNoCommitPoint is returned.
func (c *OpenChannel) DataLossCommitPoint() (*btcec.PublicKey, error) {
	return c.Db.FetchChannelDataLossCommitPoint(c)
}

// MarkBorked marks the event when the channel as reached an irreconcilable
// state, such as a channel breach or state desynchronization. Borked channels
// should never be added to the switch.
func (c *OpenChannel) MarkBorked() error {
	c.Lock()
	defer c.Unlock()

	return c.Db.MarkChannelBorked(c)
}

// SecondCommitmentPoint returns the second per-commitment-point for use in the
// channel_ready message.
func (c *OpenChannel) SecondCommitmentPoint() (*btcec.PublicKey, error) {
	c.RLock()
	defer c.RUnlock()

	// Since we start at commitment height = 0, the second per commitment
	// point is actually at the 1st index.
	revocation, err := c.RevocationProducer.AtIndex(1)
	if err != nil {
		return nil, err
	}

	return input.ComputeCommitmentPoint(revocation[:]), nil
}

// ChanSyncMsg returns the ChannelReestablish message that should be sent upon
// reconnection with the remote peer that we're maintaining this channel with.
// The information contained within this message is necessary to re-sync our
// commitment chains in the case of a last or only partially processed message.
// When the remote party receives this message one of three things may happen:
//
//  1. We're fully synced and no messages need to be sent.
//  2. We didn't get the last CommitSig message they sent, so they'll re-send
//     it.
//  3. We didn't get the last RevokeAndAck message they sent, so they'll
//     re-send it.
//
// If this is a restored channel, having status ChanStatusRestored, then we'll
// modify our typical chan sync message to ensure they force close even if
// we're on the very first state.
func (c *OpenChannel) ChanSyncMsg() (*lnwire.ChannelReestablish, error) {
	c.Lock()
	defer c.Unlock()

	// The remote commitment height that we'll send in the
	// ChannelReestablish message is our current commitment height plus
	// one. If the receiver thinks that our commitment height is actually
	// *equal* to this value, then they'll re-send the last commitment that
	// they sent but we never fully processed.
	localHeight := c.LocalCommitment.CommitHeight
	nextLocalCommitHeight := localHeight + 1

	// The second value we'll send is the height of the remote commitment
	// from our PoV. If the receiver thinks that their height is actually
	// *one plus* this value, then they'll re-send their last revocation.
	remoteChainTipHeight := c.RemoteCommitment.CommitHeight

	// If this channel has undergone a commitment update, then in order to
	// prove to the remote party our knowledge of their prior commitment
	// state, we'll also send over the last commitment secret that the
	// remote party sent.
	var lastCommitSecret [32]byte
	if remoteChainTipHeight != 0 {
		remoteSecret, err := c.RevocationStore.LookUp(
			remoteChainTipHeight - 1,
		)
		if err != nil {
			return nil, err
		}
		lastCommitSecret = [32]byte(*remoteSecret)
	}

	// Additionally, we'll send over the current unrevoked commitment on
	// our local commitment transaction.
	currentCommitSecret, err := c.RevocationProducer.AtIndex(
		localHeight,
	)
	if err != nil {
		return nil, err
	}

	// If we've restored this channel, then we'll purposefully give them an
	// invalid LocalUnrevokedCommitPoint so they'll force close the channel
	// allowing us to sweep our funds.
	if c.hasChanStatus(ChanStatusRestored) {
		currentCommitSecret[0] ^= 1

		// If this is a tweakless channel, then we'll purposefully send
		// a next local height taht's invalid to trigger a force close
		// on their end. We do this as tweakless channels don't require
		// that the commitment point is valid, only that it's present.
		if c.ChanType.IsTweakless() {
			nextLocalCommitHeight = 0
		}
	}

	// If this is a taproot channel, then we'll need to generate our next
	// verification nonce to send to the remote party. They'll use this to
	// sign the next update to our commitment transaction.
	var (
		nextTaprootNonce lnwire.OptMusig2NonceTLV
		nextLocalNonces  lnwire.OptLocalNonces
	)
	if c.ChanType.IsTaproot() {
		taprootRevProducer, err := DeriveMusig2Shachain(
			c.RevocationProducer,
		)
		if err != nil {
			return nil, err
		}

		nextNonce, err := NewMusigVerificationNonce(
			c.LocalChanCfg.MultiSigKey.PubKey,
			nextLocalCommitHeight, taprootRevProducer,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to gen next "+
				"nonce: %w", err)
		}

		fundingTxid := c.FundingOutpoint.Hash
		nonce := nextNonce.PubNonce

		// Final taproot channels use the map-based LocalNonces
		// field keyed by funding TXID. Staging channels use the
		// legacy single LocalNonce field.
		if c.ChanType.IsTaprootFinal() {
			noncesMap := make(map[chainhash.Hash]lnwire.Musig2Nonce)
			noncesMap[fundingTxid] = nonce
			nextLocalNonces = lnwire.SomeLocalNonces(
				lnwire.LocalNoncesData{NoncesMap: noncesMap},
			)
		} else {
			nextTaprootNonce = lnwire.SomeMusig2Nonce(nonce)
		}
	}

	return &lnwire.ChannelReestablish{
		ChanID: lnwire.NewChanIDFromOutPoint(
			c.FundingOutpoint,
		),
		NextLocalCommitHeight:  nextLocalCommitHeight,
		RemoteCommitTailHeight: remoteChainTipHeight,
		LastRemoteCommitSecret: lastCommitSecret,
		LocalUnrevokedCommitPoint: input.ComputeCommitmentPoint(
			currentCommitSecret[:],
		),
		LocalNonce:  nextTaprootNonce,
		LocalNonces: nextLocalNonces,
	}, nil
}

// MarkShutdownSent serialises and persist the given ShutdownInfo for this
// channel. Persisting this info represents the fact that we have sent the
// Shutdown message to the remote side and hence that we should re-transmit the
// same Shutdown message on re-establish.
func (c *OpenChannel) MarkShutdownSent(info *ShutdownInfo) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.StoreChannelShutdownInfo(c, info)
}

// ShutdownInfo decodes the shutdown info stored for this channel and returns
// the result. If no shutdown info has been persisted for this channel then the
// ErrNoShutdownInfo error is returned.
func (c *OpenChannel) ShutdownInfo() (fn.Option[ShutdownInfo], error) {
	c.RLock()
	defer c.RUnlock()

	return c.Db.FetchChannelShutdownInfo(c)
}

// MarkCommitmentBroadcasted marks the channel as a commitment transaction has
// been broadcast, either our own or the remote, and we should watch the chain
// for it to confirm before taking any further action. It takes as argument the
// closing tx _we believe_ will appear in the chain. This is only used to
// republish this tx at startup to ensure propagation, and we should still
// handle the case where a different tx actually hits the chain.
func (c *OpenChannel) MarkCommitmentBroadcasted(closeTx *wire.MsgTx,
	closer lntypes.ChannelParty) error {

	return c.Db.MarkChannelCommitmentBroadcasted(c, closeTx, closer)
}

// MarkCoopBroadcasted marks the channel to indicate that a cooperative close
// transaction has been broadcast, either our own or the remote, and that we
// should watch the chain for it to confirm before taking further action. It
// takes as argument a cooperative close tx that could appear on chain, and
// should be rebroadcast upon startup. This is only used to republish and
// ensure propagation, and we should still handle the case where a different tx
// actually hits the chain.
func (c *OpenChannel) MarkCoopBroadcasted(closeTx *wire.MsgTx,
	closer lntypes.ChannelParty) error {

	return c.Db.MarkChannelCoopBroadcasted(c, closeTx, closer)
}

// BroadcastedCommitment retrieves the stored unilateral closing tx set during
// MarkCommitmentBroadcasted. If not found ErrNoCloseTx is returned.
func (c *OpenChannel) BroadcastedCommitment() (*wire.MsgTx, error) {
	return c.Db.FetchChannelBroadcastedCommitment(c)
}

// BroadcastedCooperative retrieves the stored cooperative closing tx set during
// MarkCoopBroadcasted. If not found ErrNoCloseTx is returned.
func (c *OpenChannel) BroadcastedCooperative() (*wire.MsgTx, error) {
	return c.Db.FetchChannelBroadcastedCooperative(c)
}

// UpdateCommitment updates the local commitment state. It locks in the pending
// local updates that were received by us from the remote party. The commitment
// state completely describes the balance state at this point in the commitment
// chain. In addition to that, it persists all the remote log updates that we
// have acked, but not signed a remote commitment for yet. These need to be
// persisted to be able to produce a valid commit signature if a restart would
// occur. This method its to be called when we revoke our prior commitment
// state.
//
// A map is returned of all the htlc resolutions that were locked in this
// commitment. Keys correspond to htlc indices and values indicate whether the
// htlc was settled or failed.
func (c *OpenChannel) UpdateCommitment(newCommitment *ChannelCommitment,
	unsignedAckedUpdates []LogUpdate) (map[uint64]bool, error) {

	c.Lock()
	defer c.Unlock()

	// If this is a restored channel, then we want to avoid mutating the
	// state as all, as it's impossible to do so in a protocol compliant
	// manner.
	if c.hasChanStatus(ChanStatusRestored) {
		return nil, ErrNoRestoredChannelMutation
	}

	finalHtlcs, err := c.Db.UpdateChannelCommitment(
		c, newCommitment, unsignedAckedUpdates,
	)
	if err != nil {
		return nil, err
	}

	c.LocalCommitment = *newCommitment

	return finalHtlcs, nil
}

// ActiveHtlcs returns a slice of HTLC's which are currently active on *both*
// commitment transactions.
func (c *OpenChannel) ActiveHtlcs() []HTLC {
	c.RLock()
	defer c.RUnlock()

	// We'll only return HTLC's that are locked into *both* commitment
	// transactions. So we'll iterate through their set of HTLC's to note
	// which ones are present on their commitment.
	remoteHtlcs := make(map[[32]byte]struct{})
	for _, htlc := range c.RemoteCommitment.Htlcs {
		log.Tracef("RemoteCommitment has htlc: id=%v, update=%v "+
			"incoming=%v", htlc.HtlcIndex, htlc.LogIndex,
			htlc.Incoming)

		onionHash := sha256.Sum256(htlc.OnionBlob[:])
		remoteHtlcs[onionHash] = struct{}{}
	}

	// Now that we know which HTLC's they have, we'll only mark the HTLC's
	// as active if *we* know them as well.
	activeHtlcs := make([]HTLC, 0, len(remoteHtlcs))
	for _, htlc := range c.LocalCommitment.Htlcs {
		log.Tracef("LocalCommitment has htlc: id=%v, update=%v "+
			"incoming=%v", htlc.HtlcIndex, htlc.LogIndex,
			htlc.Incoming)

		onionHash := sha256.Sum256(htlc.OnionBlob[:])
		if _, ok := remoteHtlcs[onionHash]; !ok {
			log.Tracef("Skipped htlc due to onion mismatched: "+
				"id=%v, update=%v incoming=%v",
				htlc.HtlcIndex, htlc.LogIndex, htlc.Incoming)

			continue
		}

		activeHtlcs = append(activeHtlcs, htlc)
	}

	return activeHtlcs
}

// AppendRemoteCommitChain appends a new CommitDiff to the end of the
// commitment chain for the remote party. This method is to be used once we
// have prepared a new commitment state for the remote party, but before we
// transmit it to the remote party. The contents of the argument should be
// sufficient to retransmit the updates and signature needed to reconstruct the
// state in full, in the case that we need to retransmit.
func (c *OpenChannel) AppendRemoteCommitChain(diff *CommitDiff) error {
	c.Lock()
	defer c.Unlock()

	// If this is a restored channel, then we want to avoid mutating the
	// state at all, as it's impossible to do so in a protocol compliant
	// manner.
	if c.hasChanStatus(ChanStatusRestored) {
		return ErrNoRestoredChannelMutation
	}

	return c.Db.AppendRemoteCommitChain(c, diff)
}

// RemoteCommitChainTip returns the "tip" of the current remote commitment
// chain. This value will be non-nil iff, we've created a new commitment for
// the remote party that they haven't yet ACK'd. In this case, their commitment
// chain will have a length of two: their current unrevoked commitment, and
// this new pending commitment. Once they revoked their prior state, we'll swap
// these pointers, causing the tip and the tail to point to the same entry.
func (c *OpenChannel) RemoteCommitChainTip() (*CommitDiff, error) {
	return c.Db.RemoteCommitChainTip(c)
}

// UnsignedAckedUpdates retrieves the persisted unsigned acked remote log
// updates that still need to be signed for.
func (c *OpenChannel) UnsignedAckedUpdates() ([]LogUpdate, error) {
	return c.Db.UnsignedAckedUpdates(c)
}

// RemoteUnsignedLocalUpdates retrieves the persisted, unsigned local log
// updates that the remote still needs to sign for.
func (c *OpenChannel) RemoteUnsignedLocalUpdates() ([]LogUpdate, error) {
	return c.Db.RemoteUnsignedLocalUpdates(c)
}

// InsertNextRevocation inserts the _next_ commitment point (revocation) into
// the database, and also modifies the internal RemoteNextRevocation attribute
// to point to the passed key. This method is to be using during final channel
// set up, _after_ the channel has been fully confirmed.
//
// NOTE: If this method isn't called, then the target channel won't be able to
// propose new states for the commitment state of the remote party.
func (c *OpenChannel) InsertNextRevocation(revKey *btcec.PublicKey) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.InsertNextRevocation(c, revKey)
}

// AdvanceCommitChainTail records the new state transition within an on-disk
// append-only log which records all state transitions by the remote peer. In
// the case of an uncooperative broadcast of a prior state by the remote peer,
// this log can be consulted in order to reconstruct the state needed to
// rectify the situation. This method will add the current commitment for the
// remote party to the revocation log, and promote the current pending
// commitment to the current remote commitment. The updates parameter is the
// set of local updates that the peer still needs to send us a signature for.
// We store this set of updates in case we go down.
func (c *OpenChannel) AdvanceCommitChainTail(fwdPkg *FwdPkg,
	updates []LogUpdate, ourOutputIndex, theirOutputIndex uint32) error {

	c.Lock()
	defer c.Unlock()

	// If this is a restored channel, then we want to avoid mutating the
	// state at all, as it's impossible to do so in a protocol compliant
	// manner.
	if c.hasChanStatus(ChanStatusRestored) {
		return ErrNoRestoredChannelMutation
	}

	return c.Db.AdvanceCommitChainTail(
		c, fwdPkg, updates, ourOutputIndex, theirOutputIndex,
	)
}

// NextLocalHtlcIndex returns the next unallocated local htlc index. To ensure
// this always returns the next index that has been not been allocated, this
// will first try to examine any pending commitments, before falling back to the
// last locked-in remote commitment.
func (c *OpenChannel) NextLocalHtlcIndex() (uint64, error) {
	// First, load the most recent commit diff that we initiated for the
	// remote party. If no pending commit is found, this is not treated as
	// a critical error, since we can always fall back.
	pendingRemoteCommit, err := c.RemoteCommitChainTip()
	if err != nil && !errors.Is(err, ErrNoPendingCommit) {
		return 0, err
	}

	// If a pending commit was found, its local htlc index will be at least
	// as large as the one on our local commitment.
	if pendingRemoteCommit != nil {
		return pendingRemoteCommit.Commitment.LocalHtlcIndex, nil
	}

	// Otherwise, fallback to using the local htlc index of their
	// commitment.
	return c.RemoteCommitment.LocalHtlcIndex, nil
}

// LoadFwdPkgs scans the forwarding log for any packages that haven't been
// processed, and returns their deserialized log updates in map indexed by the
// remote commitment height at which the updates were locked in.
func (c *OpenChannel) LoadFwdPkgs() ([]*FwdPkg, error) {
	c.RLock()
	defer c.RUnlock()

	return c.Db.LoadFwdPkgs(c)
}

// AckAddHtlcs updates the AckAddFilter containing any of the provided AddRefs
// indicating that a response to this Add has been committed to the remote
// party. Doing so will prevent these Add HTLCs from being reforwarded
// internally.
func (c *OpenChannel) AckAddHtlcs(addRefs ...AddRef) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.AckAddHtlcs(c, addRefs...)
}

// AckSettleFails updates the SettleFailFilter containing any of the provided
// SettleFailRefs, indicating that the response has been delivered to the
// incoming link, corresponding to a particular AddRef. Doing so will prevent
// the responses from being retransmitted internally.
func (c *OpenChannel) AckSettleFails(settleFailRefs ...SettleFailRef) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.AckSettleFails(c, settleFailRefs...)
}

// SetFwdFilter atomically sets the forwarding filter for the forwarding package
// identified by `height`.
func (c *OpenChannel) SetFwdFilter(height uint64, fwdFilter *PkgFilter) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.SetFwdFilter(c, height, fwdFilter)
}

// RemoveFwdPkgs atomically removes forwarding packages specified by the
// remote commitment heights. If one of the intermediate RemovePkg calls fails,
// then the later packages won't be removed.
//
// NOTE: This method should only be called on packages marked FwdStateCompleted.
func (c *OpenChannel) RemoveFwdPkgs(heights ...uint64) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.RemoveFwdPkgs(c, heights...)
}

// CommitmentHeight returns the current commitment height. The commitment
// height represents the number of updates to the commitment state to date.
// This value is always monotonically increasing. This method is provided in
// order to allow multiple instances of a particular open channel to obtain a
// consistent view of the number of channel updates to date.
func (c *OpenChannel) CommitmentHeight() (uint64, error) {
	c.RLock()
	defer c.RUnlock()

	return c.Db.CommitmentHeight(c)
}

// FindPreviousState scans through the append-only log in an attempt to recover
// the previous channel state indicated by the update number. This method is
// intended to be used for obtaining the relevant data needed to claim all
// funds rightfully spendable in the case of an on-chain broadcast of the
// commitment transaction.
func (c *OpenChannel) FindPreviousState(
	updateNum uint64) (*RevocationLog, *ChannelCommitment, error) {

	c.RLock()
	defer c.RUnlock()

	return c.Db.FindPreviousState(c, updateNum)
}

// CloseChannel closes a previously active Lightning channel. Closing a
// channel entails persisting a record of the close while either purging the
// nested per-channel state inline (synchronous backends like bbolt and etcd)
// or skipping the cascading delete on tombstone-enabled backends, where the
// outpoint-index flip to outpointClosed is the authoritative marker. The
// compact summary written to closedChannelBucket and the historical record
// under historicalChannelBucket are populated identically across both paths,
// so historical reads remain uniform regardless of backend. The optional set
// of channel statuses is OR'd into the chanStatus written to the historical
// bucket and is used to record close initiators.
func (c *OpenChannel) CloseChannel(summary *ChannelCloseSummary,
	statuses ...ChannelStatus) error {

	c.Lock()
	defer c.Unlock()

	return c.Db.CloseChannel(c, summary, statuses...)
}

// Snapshot returns a read-only snapshot of the current channel state. This
// snapshot includes information concerning the current settled balance within
// the channel, metadata detailing total flows, and any outstanding HTLCs.
func (c *OpenChannel) Snapshot() *ChannelSnapshot {
	c.RLock()
	defer c.RUnlock()

	localCommit := c.LocalCommitment
	snapshot := &ChannelSnapshot{
		RemoteIdentity:    *c.IdentityPub,
		ChannelPoint:      c.FundingOutpoint,
		Capacity:          c.Capacity,
		TotalMSatSent:     c.TotalMSatSent,
		TotalMSatReceived: c.TotalMSatReceived,
		ChainHash:         c.ChainHash,
		ChannelCommitment: ChannelCommitment{
			LocalBalance:  localCommit.LocalBalance,
			RemoteBalance: localCommit.RemoteBalance,
			CommitHeight:  localCommit.CommitHeight,
			CommitFee:     localCommit.CommitFee,
		},
	}

	localCommit.CustomBlob.WhenSome(func(blob tlv.Blob) {
		blobCopy := make([]byte, len(blob))
		copy(blobCopy, blob)

		snapshot.ChannelCommitment.CustomBlob = fn.Some(blobCopy)
	})

	// Copy over the current set of HTLCs to ensure the caller can't mutate
	// our internal state.
	snapshot.Htlcs = make([]HTLC, len(localCommit.Htlcs))
	for i, h := range localCommit.Htlcs {
		snapshot.Htlcs[i] = h.Copy()
	}

	return snapshot
}

// Copy returns a deep copy of the channel state.
func (c *OpenChannel) Copy() *OpenChannel {
	c.RLock()
	defer c.RUnlock()

	clone := &OpenChannel{
		ChanType:                c.ChanType,
		ChainHash:               c.ChainHash,
		FundingOutpoint:         c.FundingOutpoint,
		ShortChannelID:          c.ShortChannelID,
		IsPending:               c.IsPending,
		IsInitiator:             c.IsInitiator,
		chanStatus:              c.chanStatus,
		FundingBroadcastHeight:  c.FundingBroadcastHeight,
		ConfirmationHeight:      c.ConfirmationHeight,
		NumConfsRequired:        c.NumConfsRequired,
		ChannelFlags:            c.ChannelFlags,
		IdentityPub:             c.IdentityPub,
		Capacity:                c.Capacity,
		TotalMSatSent:           c.TotalMSatSent,
		TotalMSatReceived:       c.TotalMSatReceived,
		InitialLocalBalance:     c.InitialLocalBalance,
		InitialRemoteBalance:    c.InitialRemoteBalance,
		LocalChanCfg:            c.LocalChanCfg,
		RemoteChanCfg:           c.RemoteChanCfg,
		LocalCommitment:         c.LocalCommitment.Copy(),
		RemoteCommitment:        c.RemoteCommitment.Copy(),
		RemoteCurrentRevocation: c.RemoteCurrentRevocation,
		RemoteNextRevocation:    c.RemoteNextRevocation,
		RevocationProducer:      c.RevocationProducer,
		RevocationStore:         c.RevocationStore,
		ThawHeight:              c.ThawHeight,
		LastWasRevoke:           c.LastWasRevoke,
		RevocationKeyLocator:    c.RevocationKeyLocator,
		confirmedScid:           c.confirmedScid,
		TapscriptRoot:           c.TapscriptRoot,
	}

	if c.FundingTxn != nil {
		clone.FundingTxn = c.FundingTxn.Copy()
	}

	if len(c.LocalShutdownScript) > 0 {
		clone.LocalShutdownScript = make(
			lnwire.DeliveryAddress,
			len(c.LocalShutdownScript),
		)
		copy(clone.LocalShutdownScript, c.LocalShutdownScript)
	}
	if len(c.RemoteShutdownScript) > 0 {
		clone.RemoteShutdownScript = make(
			lnwire.DeliveryAddress,
			len(c.RemoteShutdownScript),
		)
		copy(clone.RemoteShutdownScript, c.RemoteShutdownScript)
	}

	if len(c.Memo) > 0 {
		clone.Memo = make([]byte, len(c.Memo))
		copy(clone.Memo, c.Memo)
	}

	c.CustomBlob.WhenSome(func(blob tlv.Blob) {
		blobCopy := make([]byte, len(blob))
		copy(blobCopy, blob)
		clone.CustomBlob = fn.Some(blobCopy)
	})

	return clone
}

// LatestCommitments returns the two latest commitments for both the local and
// remote party. These commitments are read from disk to ensure that only the
// latest fully committed state is returned. The first commitment returned is
// the local commitment, and the second returned is the remote commitment.
func (c *OpenChannel) LatestCommitments() (*ChannelCommitment,
	*ChannelCommitment, error) {

	return c.Db.LatestCommitments(c)
}

// RemoteRevocationStore returns the most up to date commitment version of the
// revocation storage tree for the remote party. This method can be used when
// acting on a possible contract breach to ensure, that the caller has the most
// up to date information required to deliver justice.
func (c *OpenChannel) RemoteRevocationStore() (shachain.Store, error) {
	return c.Db.RemoteRevocationStore(c)
}

// AbsoluteThawHeight determines a frozen channel's absolute thaw height. If the
// channel is not frozen, then 0 is returned.
func (c *OpenChannel) AbsoluteThawHeight() (uint32, error) {
	// Only frozen channels have a thaw height.
	if !c.ChanType.IsFrozen() && !c.ChanType.HasLeaseExpiration() {
		return 0, nil
	}

	// If the channel has the frozen bit set and it's thaw height is below
	// the absolute threshold, then it's interpreted as a relative height to
	// the chain's current height.
	if c.ChanType.IsFrozen() && c.ThawHeight < AbsoluteThawHeightThreshold {
		// We'll only known of the channel's short ID once it's
		// confirmed.
		if c.IsPending {
			return 0, errors.New("cannot use relative thaw " +
				"height for unconfirmed channel")
		}

		// For non-zero-conf channels, this is the base height to use.
		blockHeightBase := c.ShortChannelID.BlockHeight

		// If this is a zero-conf channel, the ShortChannelID will be
		// an alias.
		if c.IsZeroConf() {
			if !c.ZeroConfConfirmed() {
				return 0, errors.New("cannot use relative " +
					"height for unconfirmed zero-conf " +
					"channel")
			}

			// Use the confirmed SCID's BlockHeight.
			blockHeightBase = c.confirmedScid.BlockHeight
		}

		return blockHeightBase + c.ThawHeight, nil
	}

	return c.ThawHeight, nil
}

// DeriveHeightHint derives the block height for the channel opening.
func (c *OpenChannel) DeriveHeightHint() uint32 {
	// As a height hint, we'll try to use the opening height, but if the
	// channel isn't yet open, then we'll use the height it was broadcast
	// at. This may be an unconfirmed zero-conf channel.
	heightHint := c.ShortChanID().BlockHeight
	if heightHint == 0 {
		heightHint = c.BroadcastHeight()
	}

	// Since no zero-conf state is stored in a channel backup, the below
	// logic will not be triggered for restored, zero-conf channels. Set
	// the height hint for zero-conf channels.
	if c.IsZeroConf() {
		if c.ZeroConfConfirmed() {
			// If the zero-conf channel is confirmed, we'll use the
			// confirmed SCID's block height.
			heightHint = c.ZeroConfRealScid().BlockHeight
		} else {
			// The zero-conf channel is unconfirmed. We'll need to
			// use the FundingBroadcastHeight.
			heightHint = c.BroadcastHeight()
		}
	}

	return heightHint
}
