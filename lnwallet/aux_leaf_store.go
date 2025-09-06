package lnwallet

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
)

// CommitSortFunc is a function type alias for a function that sorts the
// commitment transaction outputs. The second parameter is a list of CLTV
// timeouts that must correspond to the number of transaction outputs, with the
// value of 0 for non-HTLC outputs. The HTLC indexes are needed to have a
// deterministic sort value for HTLCs that have the identical amount, CLTV
// timeout and payment hash (e.g. multiple MPP shards of the same payment, where
// the on-chain script would be identical).
type CommitSortFunc func(tx *wire.MsgTx, cltvs []uint32,
	indexes []input.HtlcIndex) error

// DefaultCommitSort is the default commitment sort function that sorts the
// commitment transaction inputs and outputs according to BIP69. The second
// parameter is a list of CLTV timeouts that must correspond to the number of
// transaction outputs, with the value of 0 for non-HTLC outputs. The third
// parameter is unused for the default sort function.
func DefaultCommitSort(tx *wire.MsgTx, cltvs []uint32,
	_ []input.HtlcIndex) error {

	InPlaceCommitSort(tx, cltvs)
	return nil
}

// CommitAuxLeaves stores two potential auxiliary leaves for the remote and
// local output that may be used to augment the final tapscript trees of the
// commitment transaction.
type CommitAuxLeaves struct {
	// LocalAuxLeaf is the local party's auxiliary leaf.
	LocalAuxLeaf input.AuxTapLeaf

	// RemoteAuxLeaf is the remote party's auxiliary leaf.
	RemoteAuxLeaf input.AuxTapLeaf

	// OutgoingHTLCLeaves is the set of aux leaves for the outgoing HTLCs
	// on this commitment transaction.
	OutgoingHtlcLeaves input.HtlcAuxLeaves

	// IncomingHTLCLeaves is the set of aux leaves for the incoming HTLCs
	// on this commitment transaction.
	IncomingHtlcLeaves input.HtlcAuxLeaves
}

// AuxChanState is a struct that holds certain fields of the
// channeldb.OpenChannel struct that are used by the aux components. The data
// is copied over to prevent accidental mutation of the original channel state.
type AuxChanState struct {
	// ChanType denotes which type of channel this is.
	ChanType channeldb.ChannelType

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

	// IsInitiator is a bool which indicates if we were the original
	// initiator for the channel. This value may affect how higher levels
	// negotiate fees, or close the channel.
	IsInitiator bool

	// Capacity is the total capacity of this channel.
	Capacity btcutil.Amount

	// LocalChanCfg is the channel configuration for the local node.
	LocalChanCfg channeldb.ChannelConfig

	// RemoteChanCfg is the channel configuration for the remote node.
	RemoteChanCfg channeldb.ChannelConfig

	// ThawHeight is the height when a frozen channel once again becomes a
	// normal channel. If this is zero, then there're no restrictions on
	// this channel. If the value is lower than 500,000, then it's
	// interpreted as a relative height, or an absolute height otherwise.
	ThawHeight uint32

	// TapscriptRoot is an optional tapscript root used to derive the MuSig2
	// funding output.
	TapscriptRoot fn.Option[chainhash.Hash]

	// PeerPubKey is the peer pub key of the peer we've established this
	// channel with.
	PeerPubKey route.Vertex

	// CustomBlob is an optional blob that can be used to store information
	// specific to a custom channel type. This information is only created
	// at channel funding time, and after wards is to be considered
	// immutable.
	CustomBlob fn.Option[tlv.Blob]
}

// NewAuxChanState creates a new AuxChanState from the given channel state.
func NewAuxChanState(chanState *channeldb.OpenChannel) AuxChanState {
	peerPub := chanState.IdentityPub.SerializeCompressed()

	return AuxChanState{
		ChanType:        chanState.ChanType,
		FundingOutpoint: chanState.FundingOutpoint,
		ShortChannelID:  chanState.ShortChannelID,
		IsInitiator:     chanState.IsInitiator,
		Capacity:        chanState.Capacity,
		LocalChanCfg:    chanState.LocalChanCfg,
		RemoteChanCfg:   chanState.RemoteChanCfg,
		ThawHeight:      chanState.ThawHeight,
		TapscriptRoot:   chanState.TapscriptRoot,
		PeerPubKey:      route.Vertex(peerPub),
		CustomBlob:      chanState.CustomBlob,
	}
}

// CommitDiffAuxInput is the input required to compute the diff of the auxiliary
// leaves for a commitment transaction.
type CommitDiffAuxInput struct {
	// ChannelState is the static channel information of the channel this
	// commitment transaction relates to.
	ChannelState AuxChanState

	// PrevBlob is the blob of the previous commitment transaction.
	PrevBlob tlv.Blob

	// UnfilteredView is the unfiltered, original HTLC view of the channel.
	// Unfiltered in this context means that the view contains all HTLCs,
	// including the canceled ones.
	UnfilteredView AuxHtlcView

	// WhoseCommit denotes whose commitment transaction we are computing the
	// diff for.
	WhoseCommit lntypes.ChannelParty

	// OurBalance is the balance of the local party.
	OurBalance lnwire.MilliSatoshi

	// TheirBalance is the balance of the remote party.
	TheirBalance lnwire.MilliSatoshi

	// KeyRing is the key ring that can be used to derive keys for the
	// commitment transaction.
	KeyRing CommitmentKeyRing
}

// CommitDiffAuxResult is the result of computing the diff of the auxiliary
// leaves for a commitment transaction.
type CommitDiffAuxResult struct {
	// AuxLeaves are the auxiliary leaves for the new commitment
	// transaction.
	AuxLeaves fn.Option[CommitAuxLeaves]

	// CommitSortFunc is an optional function that sorts the commitment
	// transaction inputs and outputs.
	CommitSortFunc fn.Option[CommitSortFunc]
}

// AuxLeafStore is used to optionally fetch auxiliary tapscript leaves for the
// commitment transaction given an opaque blob. This is also used to implement
// a state transition function for the blobs to allow them to be refreshed with
// each state.
type AuxLeafStore interface {
	// FetchLeavesFromView attempts to fetch the auxiliary leaves that
	// correspond to the passed aux blob, and pending original (unfiltered)
	// HTLC view.
	FetchLeavesFromView(
		in CommitDiffAuxInput) fn.Result[CommitDiffAuxResult]

	// FetchLeavesFromCommit attempts to fetch the auxiliary leaves that
	// correspond to the passed aux blob, and an existing channel
	// commitment.
	FetchLeavesFromCommit(chanState AuxChanState,
		commit channeldb.ChannelCommitment, keyRing CommitmentKeyRing,
		whoseCommit lntypes.ChannelParty) fn.Result[CommitDiffAuxResult]

	// FetchLeavesFromRevocation attempts to fetch the auxiliary leaves
	// from a channel revocation that stores balance + blob information.
	FetchLeavesFromRevocation(
		r *channeldb.RevocationLog) fn.Result[CommitDiffAuxResult]

	// ApplyHtlcView serves as the state transition function for the custom
	// channel's blob. Given the old blob, and an HTLC view, then a new
	// blob should be returned that reflects the pending updates.
	ApplyHtlcView(in CommitDiffAuxInput) fn.Result[fn.Option[tlv.Blob]]
}

// auxLeavesFromView is used to derive the set of commit aux leaves (if any),
// that are needed to create a new commitment transaction using the original
// (unfiltered) htlc view.
func auxLeavesFromView(leafStore AuxLeafStore, chanState *channeldb.OpenChannel,
	prevBlob fn.Option[tlv.Blob], originalView *HtlcView,
	whoseCommit lntypes.ChannelParty, ourBalance,
	theirBalance lnwire.MilliSatoshi,
	keyRing CommitmentKeyRing) fn.Result[CommitDiffAuxResult] {

	return fn.MapOptionZ(
		prevBlob, func(blob tlv.Blob) fn.Result[CommitDiffAuxResult] {
			return leafStore.FetchLeavesFromView(CommitDiffAuxInput{
				ChannelState:   NewAuxChanState(chanState),
				PrevBlob:       blob,
				UnfilteredView: newAuxHtlcView(originalView),
				WhoseCommit:    whoseCommit,
				OurBalance:     ourBalance,
				TheirBalance:   theirBalance,
				KeyRing:        keyRing,
			})
		},
	)
}

// updateAuxBlob is a helper function that attempts to update the aux blob
// given the prior and current state information.
func updateAuxBlob(leafStore AuxLeafStore, chanState *channeldb.OpenChannel,
	prevBlob fn.Option[tlv.Blob], nextViewUnfiltered *HtlcView,
	whoseCommit lntypes.ChannelParty, ourBalance,
	theirBalance lnwire.MilliSatoshi,
	keyRing CommitmentKeyRing) fn.Result[fn.Option[tlv.Blob]] {

	return fn.MapOptionZ(
		prevBlob, func(blob tlv.Blob) fn.Result[fn.Option[tlv.Blob]] {
			return leafStore.ApplyHtlcView(CommitDiffAuxInput{
				ChannelState: NewAuxChanState(chanState),
				PrevBlob:     blob,
				UnfilteredView: newAuxHtlcView(
					nextViewUnfiltered,
				),
				WhoseCommit:  whoseCommit,
				OurBalance:   ourBalance,
				TheirBalance: theirBalance,
				KeyRing:      keyRing,
			})
		},
	)
}
