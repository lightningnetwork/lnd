package peer

import (
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/connmgr"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/htlcswitch/hodl"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/pool"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/watchtower/wtclient"
)

// Config defines configuration fields that are necessary for a peer object
// to function.
type Config struct {
	// Conn is the underlying network connection for this peer.
	Conn net.Conn

	// ConnReq stores information related to the persistent connection request
	// for this peer.
	ConnReq *connmgr.ConnReq

	// PubKeyBytes is the serialized, compressed public key of this peer.
	PubKeyBytes [33]byte

	// Addr is the network address of the peer.
	Addr *lnwire.NetAddress

	// Inbound indicates whether or not the peer is an inbound peer.
	Inbound bool

	// Features is the set of features that we advertise to the remote party.
	Features *lnwire.FeatureVector

	// LegacyFeatures is the set of features that we advertise to the remote
	// peer for backwards compatibility. Nodes that have not implemented
	// flat features will still be able to read our feature bits from the
	// legacy global field, but we will also advertise everything in the
	// default features field.
	LegacyFeatures *lnwire.FeatureVector

	// OutgoingCltvRejectDelta defines the number of blocks before expiry of
	// an htlc where we don't offer it anymore.
	OutgoingCltvRejectDelta uint32

	// ChanActiveTimeout specifies the duration the peer will wait to request
	// a channel reenable, beginning from the time the peer was started.
	ChanActiveTimeout time.Duration

	// ErrorBuffer stores a set of errors related to a peer. It contains error
	// messages that our peer has recently sent us over the wire and records of
	// unknown messages that were sent to us so that we can have a full track
	// record of the communication errors we have had with our peer. If we
	// choose to disconnect from a peer, it also stores the reason we had for
	// disconnecting.
	ErrorBuffer *queue.CircularBuffer

	// WritePool is the task pool that manages reuse of write buffers. Write
	// tasks are submitted to the pool in order to conserve the total number of
	// write buffers allocated at any one time, and decouple write buffer
	// allocation from the peer life cycle.
	WritePool *pool.Write

	// ReadPool is the task pool that manages reuse of read buffers.
	ReadPool *pool.Read

	// Switch is a pointer to the htlcswitch. It is used to setup, get, and
	// tear-down ChannelLinks.
	Switch *htlcswitch.Switch

	// InterceptSwitch is a pointer to the InterceptableSwitch, a wrapper around
	// the regular Switch. We only export it here to pass ForwardPackets to the
	// ChannelLinkConfig.
	InterceptSwitch *htlcswitch.InterceptableSwitch

	// ChannelDB is used to fetch opened channels, and closed channels.
	ChannelDB *channeldb.DB

	// ChannelGraph is a pointer to the channel graph which is used to
	// query information about the set of known active channels.
	ChannelGraph *channeldb.ChannelGraph

	// ChainArb is used to subscribe to channel events, update contract signals,
	// and force close channels.
	ChainArb *contractcourt.ChainArbitrator

	// AuthGossiper is needed so that the Brontide impl can register with the
	// gossiper and process remote channel announcements.
	AuthGossiper *discovery.AuthenticatedGossiper

	// ChanStatusMgr is used to set or un-set the disabled bit in channel
	// updates.
	ChanStatusMgr *netann.ChanStatusManager

	// ChainIO is used to retrieve the best block.
	ChainIO lnwallet.BlockChainIO

	// FeeEstimator is used to compute our target ideal fee-per-kw when
	// initializing the coop close process.
	FeeEstimator chainfee.Estimator

	// Signer is used when creating *lnwallet.LightningChannel instances.
	Signer input.Signer

	// SigPool is used when creating *lnwallet.LightningChannel instances.
	SigPool *lnwallet.SigPool

	// Wallet is used to publish transactions and generate delivery scripts
	// during the coop close process.
	Wallet *lnwallet.LightningWallet

	// ChainNotifier is used to receive confirmations of a coop close
	// transaction.
	ChainNotifier chainntnfs.ChainNotifier

	// RoutingPolicy is used to set the forwarding policy for links created by
	// the Brontide.
	RoutingPolicy htlcswitch.ForwardingPolicy

	// Sphinx is used when setting up ChannelLinks so they can decode sphinx
	// onion blobs.
	Sphinx *hop.OnionProcessor

	// WitnessBeacon is used when setting up ChannelLinks so they can add any
	// preimages that they learn.
	WitnessBeacon contractcourt.WitnessBeacon

	// Invoices is passed to the ChannelLink on creation and handles all
	// invoice-related logic.
	Invoices *invoices.InvoiceRegistry

	// ChannelNotifier is used by the link to notify other sub-systems about
	// channel-related events and by the Brontide to subscribe to
	// ActiveLinkEvents.
	ChannelNotifier *channelnotifier.ChannelNotifier

	// HtlcNotifier is used when creating a ChannelLink.
	HtlcNotifier *htlcswitch.HtlcNotifier

	// TowerClient is used when creating a ChannelLink.
	TowerClient wtclient.Client

	// DisconnectPeer is used to disconnect this peer if the cooperative close
	// process fails.
	DisconnectPeer func(*btcec.PublicKey) error

	// GenNodeAnnouncement is used to send our node announcement to the remote
	// on startup.
	GenNodeAnnouncement func(bool,
		...netann.NodeAnnModifier) (lnwire.NodeAnnouncement, error)

	// PrunePersistentPeerConnection is used to remove all internal state
	// related to this peer in the server.
	PrunePersistentPeerConnection func([33]byte)

	// FetchLastChanUpdate fetches our latest channel update for a target
	// channel.
	FetchLastChanUpdate func(lnwire.ShortChannelID) (*lnwire.ChannelUpdate,
		error)

	// ProcessFundingOpen is used to hand off an OpenChannel message to the
	// funding manager.
	ProcessFundingOpen func(*lnwire.OpenChannel, lnpeer.Peer)

	// ProcessFundingAccept is used to hand off an AcceptChannel message to the
	// funding manager.
	ProcessFundingAccept func(*lnwire.AcceptChannel, lnpeer.Peer)

	// ProcessFundingCreated is used to hand off a FundingCreated message to
	// the funding manager.
	ProcessFundingCreated func(*lnwire.FundingCreated, lnpeer.Peer)

	// ProcessFundingSigned is used to hand off a FundingSigned message to the
	// funding manager.
	ProcessFundingSigned func(*lnwire.FundingSigned, lnpeer.Peer)

	// ProcessFundingLocked is used to hand off a FundingLocked message to the
	// funding manager.
	ProcessFundingLocked func(*lnwire.FundingLocked, lnpeer.Peer)

	// ProcessFundingError is used to hand off an Error message to the funding
	// manager.
	ProcessFundingError func(*lnwire.Error, *btcec.PublicKey)

	// IsPendingChannel is used to determine whether to send an Error message
	// to the funding manager or not.
	IsPendingChannel func([32]byte, *btcec.PublicKey) bool

	// Hodl is used when creating ChannelLinks to specify HodlFlags as
	// breakpoints in dev builds.
	Hodl *hodl.Config

	// UnsafeReplay is used when creating ChannelLinks to specify whether or
	// not to replay adds on its commitment tx.
	UnsafeReplay bool

	// MaxOutgoingCltvExpiry is used when creating ChannelLinks and is the max
	// number of blocks that funds could be locked up for when forwarding
	// payments.
	MaxOutgoingCltvExpiry uint32

	// MaxChannelFeeAllocation is used when creating ChannelLinks and is the
	// maximum percentage of total funds that can be allocated to a channel's
	// commitment fee. This only applies for the initiator of the channel.
	MaxChannelFeeAllocation float64

	// ServerPubKey is the serialized, compressed public key of our lnd node.
	// It is used to determine which policy (channel edge) to pass to the
	// ChannelLink.
	ServerPubKey [33]byte

	// Quit is the server's quit channel. If this is closed, we halt operation.
	Quit chan struct{}
}
