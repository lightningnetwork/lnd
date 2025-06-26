package htlcswitch

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
)

// InvoiceDatabase is an interface which represents the persistent subsystem
// which may search, lookup and settle invoices.
type InvoiceDatabase interface {
	// LookupInvoice attempts to look up an invoice according to its 32
	// byte payment hash.
	LookupInvoice(context.Context, lntypes.Hash) (invoices.Invoice, error)

	// NotifyExitHopHtlc attempts to mark an invoice as settled. If the
	// invoice is a debug invoice, then this method is a noop as debug
	// invoices are never fully settled. The return value describes how the
	// htlc should be resolved. If the htlc cannot be resolved immediately,
	// the resolution is sent on the passed in hodlChan later. The eob
	// field passes the entire onion hop payload into the invoice registry
	// for decoding purposes.
	NotifyExitHopHtlc(payHash lntypes.Hash, paidAmount lnwire.MilliSatoshi,
		expiry uint32, currentHeight int32,
		circuitKey models.CircuitKey, hodlChan chan<- interface{},
		wireCustomRecords lnwire.CustomRecords,
		payload invoices.Payload) (invoices.HtlcResolution, error)

	// CancelInvoice attempts to cancel the invoice corresponding to the
	// passed payment hash.
	CancelInvoice(ctx context.Context, payHash lntypes.Hash) error

	// SettleHodlInvoice settles a hold invoice.
	SettleHodlInvoice(ctx context.Context, preimage lntypes.Preimage) error

	// HodlUnsubscribeAll unsubscribes from all htlc resolutions.
	HodlUnsubscribeAll(subscriber chan<- interface{})
}

// packetHandler is an interface used exclusively by the Switch to handle
// htlcPacket and pass them to the link implementation.
type packetHandler interface {
	// handleSwitchPacket handles the switch packets. These packets might
	// be forwarded to us from another channel link in case the htlc
	// update came from another peer or if the update was created by user
	// initially.
	//
	// NOTE: This function should block as little as possible.
	handleSwitchPacket(*htlcPacket) error
}

// dustHandler is an interface used exclusively by the Switch to evaluate
// whether a link has too much dust exposure.
type dustHandler interface {
	// getDustSum returns the dust sum on either the local or remote
	// commitment. An optional fee parameter can be passed in which is used
	// to calculate the dust sum.
	getDustSum(whoseCommit lntypes.ChannelParty,
		fee fn.Option[chainfee.SatPerKWeight]) lnwire.MilliSatoshi

	// getFeeRate returns the current channel feerate.
	getFeeRate() chainfee.SatPerKWeight

	// getDustClosure returns a closure that can evaluate whether a passed
	// HTLC is dust.
	getDustClosure() dustClosure

	// getCommitFee returns the commitment fee in satoshis from either the
	// local or remote commitment. This does not include dust.
	getCommitFee(remote bool) btcutil.Amount
}

// scidAliasHandler is an interface that the ChannelLink implements so it can
// properly handle option_scid_alias channels.
type scidAliasHandler interface {
	// attachFailAliasUpdate allows the link to properly fail incoming
	// HTLCs on option_scid_alias channels.
	attachFailAliasUpdate(failClosure func(
		sid lnwire.ShortChannelID,
		incoming bool) *lnwire.ChannelUpdate1)

	// getAliases fetches the link's underlying aliases. This is used by
	// the Switch to determine whether to forward an HTLC and where to
	// forward an HTLC.
	getAliases() []lnwire.ShortChannelID

	// isZeroConf returns whether or not the underlying channel is a
	// zero-conf channel.
	isZeroConf() bool

	// negotiatedAliasFeature returns whether the option-scid-alias feature
	// bit was negotiated.
	negotiatedAliasFeature() bool

	// confirmedScid returns the confirmed SCID for a zero-conf channel.
	confirmedScid() lnwire.ShortChannelID

	// zeroConfConfirmed returns whether or not the zero-conf channel has
	// confirmed.
	zeroConfConfirmed() bool
}

// ChannelUpdateHandler is an interface that provides methods that allow
// sending lnwire.Message to the underlying link as well as querying state.
type ChannelUpdateHandler interface {
	// HandleChannelUpdate handles the htlc requests as settle/add/fail
	// which sent to us from remote peer we have a channel with.
	//
	// NOTE: This function MUST be non-blocking (or block as little as
	// possible).
	HandleChannelUpdate(lnwire.Message)

	// ChanID returns the channel ID for the channel link. The channel ID
	// is a more compact representation of a channel's full outpoint.
	ChanID() lnwire.ChannelID

	// Bandwidth returns the amount of milli-satoshis which current link
	// might pass through channel link. The value returned from this method
	// represents the up to date available flow through the channel. This
	// takes into account any forwarded but un-cleared HTLC's, and any
	// HTLC's which have been set to the over flow queue.
	Bandwidth() lnwire.MilliSatoshi

	// EligibleToForward returns a bool indicating if the channel is able
	// to actively accept requests to forward HTLC's. A channel may be
	// active, but not able to forward HTLC's if it hasn't yet finalized
	// the pre-channel operation protocol with the remote peer. The switch
	// will use this function in forwarding decisions accordingly.
	EligibleToForward() bool

	// MayAddOutgoingHtlc returns an error if we may not add an outgoing
	// htlc to the channel, taking the amount of the htlc to add as a
	// parameter.
	MayAddOutgoingHtlc(lnwire.MilliSatoshi) error

	// EnableAdds sets the ChannelUpdateHandler state to allow
	// UpdateAddHtlc's in the specified direction. It returns true if the
	// state was changed and false if the desired state was already set
	// before the method was called.
	EnableAdds(direction LinkDirection) bool

	// DisableAdds sets the ChannelUpdateHandler state to allow
	// UpdateAddHtlc's in the specified direction. It returns true if the
	// state was changed and false if the desired state was already set
	// before the method was called.
	DisableAdds(direction LinkDirection) bool

	// IsFlushing returns true when UpdateAddHtlc's are disabled in the
	// direction of the argument.
	IsFlushing(direction LinkDirection) bool

	// OnFlushedOnce adds a hook that will be called the next time the
	// channel state reaches zero htlcs. This hook will only ever be called
	// once. If the channel state already has zero htlcs, then this will be
	// called immediately.
	OnFlushedOnce(func())

	// OnCommitOnce adds a hook that will be called the next time a
	// CommitSig message is sent in the argument's LinkDirection. This hook
	// will only ever be called once. If no CommitSig is owed in the
	// argument's LinkDirection, then we will call this hook immediately.
	OnCommitOnce(LinkDirection, func())

	// InitStfu allows us to initiate quiescence on this link. It returns
	// a receive only channel that will block until quiescence has been
	// achieved, or definitively fails. The return value is the
	// ChannelParty who holds the role of initiator or Err if the operation
	// fails.
	//
	// This operation has been added to allow channels to be quiesced via
	// RPC. It may be removed or reworked in the future as RPC initiated
	// quiescence is a holdover until we have downstream protocols that use
	// it.
	InitStfu() <-chan fn.Result[lntypes.ChannelParty]
}

// CommitHookID is a value that is used to uniquely identify hooks in the
// ChannelUpdateHandler's commitment update lifecycle. You should never need to
// construct one of these by hand, nor should you try.
type CommitHookID uint64

// FlushHookID is a value that is used to uniquely identify hooks in the
// ChannelUpdateHandler's flush lifecycle. You should never need to construct
// one of these by hand, nor should you try.
type FlushHookID uint64

// LinkDirection is used to query and change any link state on a per-direction
// basis.
type LinkDirection = bool

const (
	// Incoming is the direction from the remote peer to our node.
	Incoming LinkDirection = false

	// Outgoing is the direction from our node to the remote peer.
	Outgoing LinkDirection = true
)

// OptionalBandwidth is a type alias for the result of a bandwidth query that
// may return a bandwidth value or fn.None if the bandwidth is not available or
// not applicable. IsHandled is set to false if the external traffic shaper does
// not handle the channel in question.
type OptionalBandwidth struct {
	// IsHandled is true if the external traffic shaper handles the channel.
	// If this is false, then the bandwidth value is not applicable.
	IsHandled bool

	// Bandwidth is the available bandwidth for the channel, as determined
	// by the external traffic shaper. If the external traffic shaper is not
	// handling the channel, this value will be fn.None.
	Bandwidth fn.Option[lnwire.MilliSatoshi]
}

// ChannelLink is an interface which represents the subsystem for managing the
// incoming htlc requests, applying the changes to the channel, and also
// propagating/forwarding it to htlc switch.
//
//	abstraction level
//	     ^
//	     |
//	     | - - - - - - - - - - - - Lightning - - - - - - - - - - - - -
//	     |
//	     | (Switch)		     (Switch)		       (Switch)
//	     |  Alice <-- channel link --> Bob <-- channel link --> Carol
//	     |
//	     | - - - - - - - - - - - - - TCP - - - - - - - - - - - - - - -
//	     |
//	     |  (Peer) 		     (Peer)	                (Peer)
//	     |  Alice <----- tcp conn --> Bob <---- tcp conn -----> Carol
//	     |
type ChannelLink interface {
	// TODO(roasbeef): modify interface to embed mail boxes?

	// Embed the packetHandler interface.
	packetHandler

	// Embed the ChannelUpdateHandler interface.
	ChannelUpdateHandler

	// Embed the dustHandler interface.
	dustHandler

	// Embed the scidAliasHandler interface.
	scidAliasHandler

	// IsUnadvertised returns true if the underlying channel is
	// unadvertised.
	IsUnadvertised() bool

	// ChannelPoint returns the channel outpoint for the channel link.
	ChannelPoint() wire.OutPoint

	// ShortChanID returns the short channel ID for the channel link. The
	// short channel ID encodes the exact location in the main chain that
	// the original funding output can be found.
	ShortChanID() lnwire.ShortChannelID

	// UpdateShortChanID updates the short channel ID for a link. This may
	// be required in the event that a link is created before the short
	// chan ID for it is known, or a re-org occurs, and the funding
	// transaction changes location within the chain.
	UpdateShortChanID() (lnwire.ShortChannelID, error)

	// UpdateForwardingPolicy updates the forwarding policy for the target
	// ChannelLink. Once updated, the link will use the new forwarding
	// policy to govern if it an incoming HTLC should be forwarded or not.
	UpdateForwardingPolicy(models.ForwardingPolicy)

	// CheckHtlcForward should return a nil error if the passed HTLC details
	// satisfy the current forwarding policy fo the target link. Otherwise,
	// a LinkError with a valid protocol failure message should be returned
	// in order to signal to the source of the HTLC, the policy consistency
	// issue.
	CheckHtlcForward(payHash [32]byte, incomingAmt lnwire.MilliSatoshi,
		amtToForward lnwire.MilliSatoshi, incomingTimeout,
		outgoingTimeout uint32, inboundFee models.InboundFee,
		heightNow uint32, scid lnwire.ShortChannelID,
		customRecords lnwire.CustomRecords) *LinkError

	// CheckHtlcTransit should return a nil error if the passed HTLC details
	// satisfy the current channel policy.  Otherwise, a LinkError with a
	// valid protocol failure message should be returned in order to signal
	// the violation. This call is intended to be used for locally initiated
	// payments for which there is no corresponding incoming htlc.
	CheckHtlcTransit(payHash [32]byte, amt lnwire.MilliSatoshi,
		timeout uint32, heightNow uint32,
		customRecords lnwire.CustomRecords) *LinkError

	// Stats return the statistics of channel link. Number of updates,
	// total sent/received milli-satoshis.
	Stats() (uint64, lnwire.MilliSatoshi, lnwire.MilliSatoshi)

	// PeerPubKey returns the serialized public key of remote peer with
	// which we have the channel link opened.
	PeerPubKey() [33]byte

	// AttachMailBox delivers an active MailBox to the link. The MailBox may
	// have buffered messages.
	AttachMailBox(MailBox)

	// FundingCustomBlob returns the custom funding blob of the channel that
	// this link is associated with. The funding blob represents static
	// information about the channel that was created at channel funding
	// time.
	FundingCustomBlob() fn.Option[tlv.Blob]

	// CommitmentCustomBlob returns the custom blob of the current local
	// commitment of the channel that this link is associated with.
	CommitmentCustomBlob() fn.Option[tlv.Blob]

	// AuxBandwidth returns the bandwidth that can be used for a channel,
	// expressed in milli-satoshi. This might be different from the regular
	// BTC bandwidth for custom channels. This will always return fn.None()
	// for a regular (non-custom) channel.
	AuxBandwidth(amount lnwire.MilliSatoshi, cid lnwire.ShortChannelID,
		htlcBlob fn.Option[tlv.Blob],
		ts AuxTrafficShaper) fn.Result[OptionalBandwidth]

	// Start starts the channel link.
	Start() error

	// Stop requests the channel link to be shut down.
	Stop()
}

// ForwardingLog is an interface that represents a time series database which
// keep track of all successfully completed payment circuits. Every few
// seconds, the switch will collate and flush out all the successful payment
// circuits during the last interval.
type ForwardingLog interface {
	// AddForwardingEvents is a method that should write out the set of
	// forwarding events in a batch to persistent storage. Outside
	// sub-systems can then query the contents of the log for analysis,
	// visualizations, etc.
	AddForwardingEvents([]channeldb.ForwardingEvent) error
}

// TowerClient is the primary interface used by the daemon to backup pre-signed
// justice transactions to watchtowers.
type TowerClient interface {
	// RegisterChannel persistently initializes any channel-dependent
	// parameters within the client. This should be called during link
	// startup to ensure that the client is able to support the link during
	// operation.
	RegisterChannel(lnwire.ChannelID, channeldb.ChannelType) error

	// BackupState initiates a request to back up a particular revoked
	// state. If the method returns nil, the backup is guaranteed to be
	// successful unless the justice transaction would create dust outputs
	// when trying to abide by the negotiated policy.
	BackupState(chanID *lnwire.ChannelID, stateNum uint64) error
}

// InterceptableHtlcForwarder is the interface to set the interceptor
// implementation that intercepts htlc forwards.
type InterceptableHtlcForwarder interface {
	// SetInterceptor sets a ForwardInterceptor.
	SetInterceptor(interceptor ForwardInterceptor)

	// Resolve resolves an intercepted packet.
	Resolve(res *FwdResolution) error
}

// ForwardInterceptor is a function that is invoked from the switch for every
// incoming htlc that is intended to be forwarded. It is passed with the
// InterceptedForward that contains the information about the packet and a way
// to resolve it manually later in case it is held.
// The return value indicates if this handler will take control of this forward
// and resolve it later or let the switch execute its default behavior.
type ForwardInterceptor func(InterceptedPacket) error

// InterceptedPacket contains the relevant information for the interceptor about
// an HTLC.
type InterceptedPacket struct {
	// IncomingCircuit contains the incoming channel and htlc id of the
	// packet.
	IncomingCircuit models.CircuitKey

	// OutgoingChanID is the destination channel for this packet.
	OutgoingChanID lnwire.ShortChannelID

	// Hash is the payment hash of the htlc.
	Hash lntypes.Hash

	// OutgoingExpiry is the absolute block height at which the outgoing
	// htlc expires.
	OutgoingExpiry uint32

	// OutgoingAmount is the amount to forward.
	OutgoingAmount lnwire.MilliSatoshi

	// IncomingExpiry is the absolute block height at which the incoming
	// htlc expires.
	IncomingExpiry uint32

	// IncomingAmount is the amount of the accepted htlc.
	IncomingAmount lnwire.MilliSatoshi

	// InOnionCustomRecords are user-defined records in the custom type
	// range that were included in the payload.
	InOnionCustomRecords record.CustomSet

	// OnionBlob is the onion packet for the next hop
	OnionBlob [lnwire.OnionPacketSize]byte

	// InWireCustomRecords are user-defined p2p wire message records that
	// were defined by the peer that forwarded this HTLC to us.
	InWireCustomRecords lnwire.CustomRecords

	// AutoFailHeight is the block height at which this intercept will be
	// failed back automatically.
	AutoFailHeight int32
}

// InterceptedForward is passed to the ForwardInterceptor for every forwarded
// htlc. It contains all the information about the packet which accordingly
// the interceptor decides if to hold or not.
// In addition this interface allows a later resolution by calling either
// Resume, Settle or Fail.
type InterceptedForward interface {
	// Packet returns the intercepted packet.
	Packet() InterceptedPacket

	// Resume notifies the intention to resume an existing hold forward. This
	// basically means the caller wants to resume with the default behavior for
	// this htlc which usually means forward it.
	Resume() error

	// ResumeModified notifies the intention to resume an existing hold
	// forward with modified fields.
	ResumeModified(inAmountMsat,
		outAmountMsat fn.Option[lnwire.MilliSatoshi],
		outWireCustomRecords fn.Option[lnwire.CustomRecords]) error

	// Settle notifies the intention to settle an existing hold
	// forward with a given preimage.
	Settle(lntypes.Preimage) error

	// Fail notifies the intention to fail an existing hold forward with an
	// encrypted failure reason.
	Fail(reason []byte) error

	// FailWithCode notifies the intention to fail an existing hold forward
	// with the specified failure code.
	FailWithCode(code lnwire.FailCode) error
}

// htlcNotifier is an interface which represents the input side of the
// HtlcNotifier which htlc events are piped through. This interface is intended
// to allow for mocking of the htlcNotifier in tests, so is unexported because
// it is not needed outside of the htlcSwitch package.
type htlcNotifier interface {
	// NotifyForwardingEvent notifies the HtlcNotifier than a htlc has been
	// forwarded.
	NotifyForwardingEvent(key HtlcKey, info HtlcInfo,
		eventType HtlcEventType)

	// NotifyLinkFailEvent notifies that a htlc has failed on our
	// incoming link. It takes an isReceive bool to differentiate between
	// our node's receives and forwards.
	NotifyLinkFailEvent(key HtlcKey, info HtlcInfo,
		eventType HtlcEventType, linkErr *LinkError, incoming bool)

	// NotifyForwardingFailEvent notifies the HtlcNotifier that a htlc we
	// forwarded has failed down the line.
	NotifyForwardingFailEvent(key HtlcKey, eventType HtlcEventType)

	// NotifySettleEvent notifies the HtlcNotifier that a htlc that we
	// committed to as part of a forward or a receive to our node has been
	// settled.
	NotifySettleEvent(key HtlcKey, preimage lntypes.Preimage,
		eventType HtlcEventType)

	// NotifyFinalHtlcEvent notifies the HtlcNotifier that the final outcome
	// for an htlc has been determined.
	NotifyFinalHtlcEvent(key models.CircuitKey,
		info channeldb.FinalHtlcInfo)
}

// AuxHtlcModifier is an interface that allows the sender to modify the outgoing
// HTLC of a payment by changing the amount or the wire message tlv records.
type AuxHtlcModifier interface {
	// ProduceHtlcExtraData is a function that, based on the previous extra
	// data blob of an HTLC, may produce a different blob or modify the
	// amount of bitcoin this htlc should carry.
	ProduceHtlcExtraData(totalAmount lnwire.MilliSatoshi,
		htlcCustomRecords lnwire.CustomRecords,
		peer route.Vertex) (lnwire.MilliSatoshi, lnwire.CustomRecords,
		error)
}

// AuxTrafficShaper is an interface that allows the sender to determine if a
// payment should be carried by a channel based on the TLV records that may be
// present in the `update_add_htlc` message or the channel commitment itself.
type AuxTrafficShaper interface {
	AuxHtlcModifier

	// ShouldHandleTraffic is called in order to check if the channel
	// identified by the provided channel ID may have external mechanisms
	// that would allow it to carry out the payment.
	ShouldHandleTraffic(cid lnwire.ShortChannelID,
		fundingBlob, htlcBlob fn.Option[tlv.Blob]) (bool, error)

	// PaymentBandwidth returns the available bandwidth for a custom channel
	// decided by the given channel funding/commitment aux blob and HTLC
	// blob. A return value of 0 means there is no bandwidth available. To
	// find out if a channel is a custom channel that should be handled by
	// the traffic shaper, the ShouldHandleTraffic method should be called
	// first.
	PaymentBandwidth(fundingBlob, htlcBlob,
		commitmentBlob fn.Option[tlv.Blob],
		linkBandwidth, htlcAmt lnwire.MilliSatoshi,
		htlcView lnwallet.AuxHtlcView,
		peer route.Vertex) (lnwire.MilliSatoshi, error)

	// IsCustomHTLC returns true if the HTLC carries the set of relevant
	// custom records to put it under the purview of the traffic shaper,
	// meaning that it's from a custom channel.
	IsCustomHTLC(htlcRecords lnwire.CustomRecords) bool
}
