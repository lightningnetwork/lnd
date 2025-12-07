package peer

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/connmgr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/aliasmgr"
	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/buffer"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/funding"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/htlcswitch/hodl"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/msgmux"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/onionmessage"
	"github.com/lightningnetwork/lnd/pool"
	"github.com/lightningnetwork/lnd/protofsm"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/watchtower/wtclient"
)

const (
	// pingInterval is the interval at which ping messages are sent.
	pingInterval = 1 * time.Minute

	// pingTimeout is the amount of time we will wait for a pong response
	// before considering the peer to be unresponsive.
	//
	// This MUST be a smaller value than the pingInterval.
	pingTimeout = 30 * time.Second

	// idleTimeout is the duration of inactivity before we time out a peer.
	idleTimeout = 5 * time.Minute

	// writeMessageTimeout is the timeout used when writing a message to the
	// peer.
	writeMessageTimeout = 5 * time.Second

	// readMessageTimeout is the timeout used when reading a message from a
	// peer.
	readMessageTimeout = 5 * time.Second

	// handshakeTimeout is the timeout used when waiting for the peer's init
	// message.
	handshakeTimeout = 15 * time.Second

	// ErrorBufferSize is the number of historic peer errors that we store.
	ErrorBufferSize = 10

	// pongSizeCeiling is the upper bound on a uniformly distributed random
	// variable that we use for requesting pong responses. We don't use the
	// MaxPongBytes (upper bound accepted by the protocol) because it is
	// needlessly wasteful of precious Tor bandwidth for little to no gain.
	pongSizeCeiling = 4096

	// torTimeoutMultiplier is the scaling factor we use on network timeouts
	// for Tor peers.
	torTimeoutMultiplier = 3

	// msgStreamSize is the size of the message streams.
	msgStreamSize = 50
)

var (
	// ErrChannelNotFound is an error returned when a channel is queried and
	// either the Brontide doesn't know of it, or the channel in question
	// is pending.
	ErrChannelNotFound = fmt.Errorf("channel not found")
)

// outgoingMsg packages an lnwire.Message to be sent out on the wire, along with
// a buffered channel which will be sent upon once the write is complete. This
// buffered channel acts as a semaphore to be used for synchronization purposes.
type outgoingMsg struct {
	priority bool
	msg      lnwire.Message
	errChan  chan error // MUST be buffered.
}

// newChannelMsg packages a channeldb.OpenChannel with a channel that allows
// the receiver of the request to report when the channel creation process has
// completed.
type newChannelMsg struct {
	// channel is used when the pending channel becomes active.
	channel *lnpeer.NewChannel

	// channelID is used when there's a new pending channel.
	channelID lnwire.ChannelID

	err chan error
}

type customMsg struct {
	peer [33]byte
	msg  lnwire.Custom
}

// closeMsg is a wrapper struct around any wire messages that deal with the
// cooperative channel closure negotiation process. This struct includes the
// raw channel ID targeted along with the original message.
type closeMsg struct {
	cid lnwire.ChannelID
	msg lnwire.Message
}

// PendingUpdate describes the pending state of a closing channel.
type PendingUpdate struct {
	// Txid is the txid of the closing transaction.
	Txid []byte

	// OutputIndex is the output index of our output in the closing
	// transaction.
	OutputIndex uint32

	// FeePerVByte is an optional field, that is set only when the new RBF
	// coop close flow is used. This indicates the new closing fee rate on
	// the closing transaction.
	FeePerVbyte fn.Option[chainfee.SatPerVByte]

	// IsLocalCloseTx is an optional field that indicates if this update is
	// sent for our local close txn, or the close txn of the remote party.
	// This is only set if the new RBF coop close flow is used.
	IsLocalCloseTx fn.Option[bool]
}

// ChannelCloseUpdate contains the outcome of the close channel operation.
type ChannelCloseUpdate struct {
	ClosingTxid []byte
	Success     bool

	// LocalCloseOutput is an optional, additional output on the closing
	// transaction that the local party should be paid to. This will only be
	// populated if the local balance isn't dust.
	LocalCloseOutput fn.Option[chancloser.CloseOutput]

	// RemoteCloseOutput is an optional, additional output on the closing
	// transaction that the remote party should be paid to. This will only
	// be populated if the remote balance isn't dust.
	RemoteCloseOutput fn.Option[chancloser.CloseOutput]

	// AuxOutputs is an optional set of additional outputs that might be
	// included in the closing transaction. These are used for custom
	// channel types.
	AuxOutputs fn.Option[chancloser.AuxCloseOutputs]
}

// TimestampedError is a timestamped error that is used to store the most recent
// errors we have experienced with our peers.
type TimestampedError struct {
	Error     error
	Timestamp time.Time
}

// Config defines configuration fields that are necessary for a peer object
// to function.
type Config struct {
	// Conn is the underlying network connection for this peer.
	Conn MessageConn

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
	Switch messageSwitch

	// InterceptSwitch is a pointer to the InterceptableSwitch, a wrapper around
	// the regular Switch. We only export it here to pass ForwardPackets to the
	// ChannelLinkConfig.
	InterceptSwitch *htlcswitch.InterceptableSwitch

	// ChannelDB is used to fetch opened channels, and closed channels.
	ChannelDB *channeldb.ChannelStateDB

	// ChannelGraph is a pointer to the channel graph which is used to
	// query information about the set of known active channels.
	ChannelGraph *graphdb.ChannelGraph

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

	// Wallet is used to publish transactions and generates delivery
	// scripts during the coop close process.
	Wallet *lnwallet.LightningWallet

	// ChainNotifier is used to receive confirmations of a coop close
	// transaction.
	ChainNotifier chainntnfs.ChainNotifier

	// BestBlockView is used to efficiently query for up-to-date
	// blockchain state information
	BestBlockView chainntnfs.BestBlockView

	// RoutingPolicy is used to set the forwarding policy for links created by
	// the Brontide.
	RoutingPolicy models.ForwardingPolicy

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

	// TowerClient is used to backup revoked states.
	TowerClient wtclient.ClientManager

	// DisconnectPeer is used to disconnect this peer if the cooperative close
	// process fails.
	DisconnectPeer func(*btcec.PublicKey) error

	// GenNodeAnnouncement is used to send our node announcement to the remote
	// on startup.
	GenNodeAnnouncement func(...netann.NodeAnnModifier) (
		lnwire.NodeAnnouncement1, error)

	// PrunePersistentPeerConnection is used to remove all internal state
	// related to this peer in the server.
	PrunePersistentPeerConnection func([33]byte)

	// FetchLastChanUpdate fetches our latest channel update for a target
	// channel.
	FetchLastChanUpdate func(lnwire.ShortChannelID) (*lnwire.ChannelUpdate1,
		error)

	// FundingManager is an implementation of the funding.Controller interface.
	FundingManager funding.Controller

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

	// MaxAnchorsCommitFeeRate is the maximum fee rate we'll use as an
	// initiator for anchor channel commitments.
	MaxAnchorsCommitFeeRate chainfee.SatPerKWeight

	// CoopCloseTargetConfs is the confirmation target that will be used
	// to estimate the fee rate to use during a cooperative channel
	// closure initiated by the remote peer.
	CoopCloseTargetConfs uint32

	// ServerPubKey is the serialized, compressed public key of our lnd node.
	// It is used to determine which policy (channel edge) to pass to the
	// ChannelLink.
	ServerPubKey [33]byte

	// ChannelCommitInterval is the maximum time that is allowed to pass between
	// receiving a channel state update and signing the next commitment.
	// Setting this to a longer duration allows for more efficient channel
	// operations at the cost of latency.
	ChannelCommitInterval time.Duration

	// PendingCommitInterval is the maximum time that is allowed to pass
	// while waiting for the remote party to revoke a locally initiated
	// commitment state. Setting this to a longer duration if a slow
	// response is expected from the remote party or large number of
	// payments are attempted at the same time.
	PendingCommitInterval time.Duration

	// ChannelCommitBatchSize is the maximum number of channel state updates
	// that is accumulated before signing a new commitment.
	ChannelCommitBatchSize uint32

	// HandleCustomMessage is called whenever a custom message is received
	// from the peer.
	HandleCustomMessage func(peer [33]byte, msg *lnwire.Custom) error

	// GetAliases is passed to created links so the Switch and link can be
	// aware of the channel's aliases.
	GetAliases func(base lnwire.ShortChannelID) []lnwire.ShortChannelID

	// RequestAlias allows the Brontide struct to request an alias to send
	// to the peer.
	RequestAlias func() (lnwire.ShortChannelID, error)

	// AddLocalAlias persists an alias to an underlying alias store.
	AddLocalAlias func(alias, base lnwire.ShortChannelID, gossip,
		liveUpdate bool, opts ...aliasmgr.AddLocalAliasOption) error

	// AuxLeafStore is an optional store that can be used to store auxiliary
	// leaves for certain custom channel types.
	AuxLeafStore fn.Option[lnwallet.AuxLeafStore]

	// AuxSigner is an optional signer that can be used to sign auxiliary
	// leaves for certain custom channel types.
	AuxSigner fn.Option[lnwallet.AuxSigner]

	// AuxResolver is an optional interface that can be used to modify the
	// way contracts are resolved.
	AuxResolver fn.Option[lnwallet.AuxContractResolver]

	// AuxTrafficShaper is an optional auxiliary traffic shaper that can be
	// used to manage the bandwidth of peer links.
	AuxTrafficShaper fn.Option[htlcswitch.AuxTrafficShaper]

	// PongBuf is a slice we'll reuse instead of allocating memory on the
	// heap. Since only reads will occur and no writes, there is no need
	// for any synchronization primitives. As a result, it's safe to share
	// this across multiple Peer struct instances.
	PongBuf []byte

	// Adds the option to disable forwarding payments in blinded routes
	// by failing back any blinding-related payloads as if they were
	// invalid.
	DisallowRouteBlinding bool

	// DisallowQuiescence is a flag that indicates whether the Brontide
	// should have the quiescence feature disabled.
	DisallowQuiescence bool

	// QuiescenceTimeout is the max duration that the channel can be
	// quiesced. Any dependent protocols (dynamic commitments, splicing,
	// etc.) must finish their operations under this timeout value,
	// otherwise the node will disconnect.
	QuiescenceTimeout time.Duration

	// MaxFeeExposure limits the number of outstanding fees in a channel.
	// This value will be passed to created links.
	MaxFeeExposure lnwire.MilliSatoshi

	// MsgRouter is an optional instance of the main message router that
	// the peer will use. If None, then a new default version will be used
	// in place.
	MsgRouter fn.Option[msgmux.Router]

	// AuxChanCloser is an optional instance of an abstraction that can be
	// used to modify the way the co-op close transaction is constructed.
	AuxChanCloser fn.Option[chancloser.AuxChanCloser]

	// AuxChannelNegotiator is an optional interface that allows aux channel
	// implementations to inject and process custom records over channel
	// related wire messages.
	AuxChannelNegotiator fn.Option[lnwallet.AuxChannelNegotiator]

	// OnionMessageServer is an instance of a message server that dispatches
	// onion messages to subscribers.
	OnionMessageServer *subscribe.Server

	// ShouldFwdExpEndorsement is a closure that indicates whether
	// experimental endorsement signals should be set.
	ShouldFwdExpEndorsement func() bool

	// NoDisconnectOnPongFailure indicates whether the peer should *not* be
	// disconnected if a pong is not received in time or is mismatched.
	NoDisconnectOnPongFailure bool

	// Quit is the server's quit channel. If this is closed, we halt operation.
	Quit chan struct{}
}

// chanCloserFsm is a union-like type that can hold the two versions of co-op
// close we support: negotiation, and RBF based.
//
// TODO(roasbeef): rename to chancloser.Negotiator and chancloser.RBF?
type chanCloserFsm = fn.Either[*chancloser.ChanCloser, *chancloser.RbfChanCloser] //nolint:ll

// makeNegotiateCloser creates a new negotiate closer from a
// chancloser.ChanCloser.
func makeNegotiateCloser(chanCloser *chancloser.ChanCloser) chanCloserFsm {
	return fn.NewLeft[*chancloser.ChanCloser, *chancloser.RbfChanCloser](
		chanCloser,
	)
}

// makeRbfCloser creates a new RBF closer from a chancloser.RbfChanCloser.
func makeRbfCloser(rbfCloser *chancloser.RbfChanCloser) chanCloserFsm {
	return fn.NewRight[*chancloser.ChanCloser](
		rbfCloser,
	)
}

// Brontide is an active peer on the Lightning Network. This struct is responsible
// for managing any channel state related to this peer. To do so, it has
// several helper goroutines to handle events such as HTLC timeouts, new
// funding workflow, and detecting an uncooperative closure of any active
// channels.
type Brontide struct {
	// MUST be used atomically.
	started    int32
	disconnect int32

	// MUST be used atomically.
	bytesReceived uint64
	bytesSent     uint64

	// isTorConnection is a flag that indicates whether or not we believe
	// the remote peer is a tor connection. It is not always possible to
	// know this with certainty but we have heuristics we use that should
	// catch most cases.
	//
	// NOTE: We judge the tor-ness of a connection by if the remote peer has
	// ".onion" in the address OR if it's connected over localhost.
	// This will miss cases where our peer is connected to our clearnet
	// address over the tor network (via exit nodes). It will also misjudge
	// actual localhost connections as tor. We need to include this because
	// inbound connections to our tor address will appear to come from the
	// local socks5 proxy. This heuristic is only used to expand the timeout
	// window for peers so it is OK to misjudge this. If you use this field
	// for any other purpose you should seriously consider whether or not
	// this heuristic is good enough for your use case.
	isTorConnection bool

	pingManager *PingManager

	// lastPingPayload stores an unsafe pointer wrapped as an atomic
	// variable which points to the last payload the remote party sent us
	// as their ping.
	//
	// MUST be used atomically.
	lastPingPayload atomic.Value

	cfg Config

	// activeSignal when closed signals that the peer is now active and
	// ready to process messages.
	activeSignal chan struct{}

	// startTime is the time this peer connection was successfully established.
	// It will be zero for peers that did not successfully call Start().
	startTime time.Time

	// sendQueue is the channel which is used to queue outgoing messages to be
	// written onto the wire. Note that this channel is unbuffered.
	sendQueue chan outgoingMsg

	// outgoingQueue is a buffered channel which allows second/third party
	// objects to queue messages to be sent out on the wire.
	outgoingQueue chan outgoingMsg

	// activeChannels is a map which stores the state machines of all
	// active channels. Channels are indexed into the map by the txid of
	// the funding transaction which opened the channel.
	//
	// NOTE: On startup, pending channels are stored as nil in this map.
	// Confirmed channels have channel data populated in the map. This means
	// that accesses to this map should nil-check the LightningChannel to
	// see if this is a pending channel or not. The tradeoff here is either
	// having two maps everywhere (one for pending, one for confirmed chans)
	// or having an extra nil-check per access.
	activeChannels *lnutils.SyncMap[
		lnwire.ChannelID, *lnwallet.LightningChannel]

	// addedChannels tracks any new channels opened during this peer's
	// lifecycle. We use this to filter out these new channels when the time
	// comes to request a reenable for active channels, since they will have
	// waited a shorter duration.
	addedChannels *lnutils.SyncMap[lnwire.ChannelID, struct{}]

	// newActiveChannel is used by the fundingManager to send fully opened
	// channels to the source peer which handled the funding workflow.
	newActiveChannel chan *newChannelMsg

	// newPendingChannel is used by the fundingManager to send pending open
	// channels to the source peer which handled the funding workflow.
	newPendingChannel chan *newChannelMsg

	// removePendingChannel is used by the fundingManager to cancel pending
	// open channels to the source peer when the funding flow is failed.
	removePendingChannel chan *newChannelMsg

	// activeMsgStreams is a map from channel id to the channel streams that
	// proxy messages to individual, active links.
	activeMsgStreams map[lnwire.ChannelID]*msgStream

	// activeChanCloses is a map that keeps track of all the active
	// cooperative channel closures. Any channel closing messages are directed
	// to one of these active state machines. Once the channel has been closed,
	// the state machine will be deleted from the map.
	activeChanCloses *lnutils.SyncMap[lnwire.ChannelID, chanCloserFsm]

	// localCloseChanReqs is a channel in which any local requests to close
	// a particular channel are sent over.
	localCloseChanReqs chan *htlcswitch.ChanClose

	// linkFailures receives all reported channel failures from the switch,
	// and instructs the channelManager to clean remaining channel state.
	linkFailures chan linkFailureReport

	// chanCloseMsgs is a channel that any message related to channel
	// closures are sent over. This includes lnwire.Shutdown message as
	// well as lnwire.ClosingSigned messages.
	chanCloseMsgs chan *closeMsg

	// remoteFeatures is the feature vector received from the peer during
	// the connection handshake.
	remoteFeatures *lnwire.FeatureVector

	// resentChanSyncMsg is a set that keeps track of which channels we
	// have re-sent channel reestablishment messages for. This is done to
	// avoid getting into loop where both peers will respond to the other
	// peer's chansync message with its own over and over again.
	resentChanSyncMsg map[lnwire.ChannelID]struct{}

	// channelEventClient is the channel event subscription client that's
	// used to assist retry enabling the channels. This client is only
	// created when the reenableTimeout is no greater than 1 minute. Once
	// created, it is canceled once the reenabling has been finished.
	//
	// NOTE: we choose to create the client conditionally to avoid
	// potentially holding lots of un-consumed events.
	channelEventClient *subscribe.Client

	// msgRouter is an instance of the msgmux.Router which is used to send
	// off new wire messages for handing.
	msgRouter fn.Option[msgmux.Router]

	// globalMsgRouter is a flag that indicates whether we have a global
	// msg router. If so, then we don't worry about stopping the msg router
	// when a peer disconnects.
	globalMsgRouter bool

	startReady chan struct{}

	// cg is a helper that encapsulates a wait group and quit channel and
	// allows contexts that either block or cancel on those depending on
	// the use case.
	cg *fn.ContextGuard

	// log is a peer-specific logging instance.
	log btclog.Logger
}

// A compile-time check to ensure that Brontide satisfies the lnpeer.Peer
// interface.
var _ lnpeer.Peer = (*Brontide)(nil)

// NewBrontide creates a new Brontide from a peer.Config struct.
func NewBrontide(cfg Config) *Brontide {
	logPrefix := fmt.Sprintf("Peer(%x):", cfg.PubKeyBytes)

	// We have a global message router if one was passed in via the config.
	// In this case, we don't need to attempt to tear it down when the peer
	// is stopped.
	globalMsgRouter := cfg.MsgRouter.IsSome()

	// We'll either use the msg router instance passed in, or create a new
	// blank instance.
	msgRouter := cfg.MsgRouter.Alt(fn.Some[msgmux.Router](
		msgmux.NewMultiMsgRouter(),
	))

	p := &Brontide{
		cfg:           cfg,
		activeSignal:  make(chan struct{}),
		sendQueue:     make(chan outgoingMsg),
		outgoingQueue: make(chan outgoingMsg),
		addedChannels: &lnutils.SyncMap[lnwire.ChannelID, struct{}]{},
		activeChannels: &lnutils.SyncMap[
			lnwire.ChannelID, *lnwallet.LightningChannel,
		]{},
		newActiveChannel:     make(chan *newChannelMsg, 1),
		newPendingChannel:    make(chan *newChannelMsg, 1),
		removePendingChannel: make(chan *newChannelMsg),

		activeMsgStreams: make(map[lnwire.ChannelID]*msgStream),
		activeChanCloses: &lnutils.SyncMap[
			lnwire.ChannelID, chanCloserFsm,
		]{},
		localCloseChanReqs: make(chan *htlcswitch.ChanClose),
		linkFailures:       make(chan linkFailureReport),
		chanCloseMsgs:      make(chan *closeMsg),
		resentChanSyncMsg:  make(map[lnwire.ChannelID]struct{}),
		startReady:         make(chan struct{}),
		log:                peerLog.WithPrefix(logPrefix),
		msgRouter:          msgRouter,
		globalMsgRouter:    globalMsgRouter,
		cg:                 fn.NewContextGuard(),
	}

	if cfg.Conn != nil && cfg.Conn.RemoteAddr() != nil {
		remoteAddr := cfg.Conn.RemoteAddr().String()
		p.isTorConnection = strings.Contains(remoteAddr, ".onion") ||
			strings.Contains(remoteAddr, "127.0.0.1")
	}

	var (
		lastBlockHeader           *wire.BlockHeader
		lastSerializedBlockHeader [wire.MaxBlockHeaderPayload]byte
	)
	newPingPayload := func() []byte {
		// We query the BestBlockHeader from our BestBlockView each time
		// this is called, and update our serialized block header if
		// they differ.  Over time, we'll use this to disseminate the
		// latest block header between all our peers, which can later be
		// used to cross-check our own view of the network to mitigate
		// various types of eclipse attacks.
		header, err := p.cfg.BestBlockView.BestBlockHeader()
		if err != nil && header == lastBlockHeader {
			return lastSerializedBlockHeader[:]
		}

		buf := bytes.NewBuffer(lastSerializedBlockHeader[0:0])
		err = header.Serialize(buf)
		if err == nil {
			lastBlockHeader = header
		} else {
			p.log.Warn("unable to serialize current block" +
				"header for ping payload generation." +
				"This should be impossible and means" +
				"there is an implementation bug.")
		}

		return lastSerializedBlockHeader[:]
	}

	// TODO(roasbeef): make dynamic in order to create fake cover traffic.
	//
	// NOTE(proofofkeags): this was changed to be dynamic to allow better
	// pong identification, however, more thought is needed to make this
	// actually usable as a traffic decoy.
	randPongSize := func() uint16 {
		return uint16(
			// We don't need cryptographic randomness here.
			/* #nosec */
			rand.Intn(pongSizeCeiling) + 1,
		)
	}

	p.pingManager = NewPingManager(&PingManagerConfig{
		NewPingPayload:   newPingPayload,
		NewPongSize:      randPongSize,
		IntervalDuration: p.scaleTimeout(pingInterval),
		TimeoutDuration:  p.scaleTimeout(pingTimeout),
		SendPing: func(ping *lnwire.Ping) {
			p.queueMsg(ping, nil)
		},
		OnPongFailure: func(reason error,
			timeWaitedForPong time.Duration,
			lastKnownRTT time.Duration) {

			logMsg := fmt.Sprintf("pong response "+
				"failure for %s: %v. Time waited for this "+
				"pong: %v. Last successful RTT: %v.",
				p, reason, timeWaitedForPong, lastKnownRTT)

			// If NoDisconnectOnPongFailure is true, we don't
			// disconnect. Otherwise (if it's false, the default),
			// we disconnect.
			if p.cfg.NoDisconnectOnPongFailure {
				p.log.Warnf("%s -- not disconnecting "+
					"due to config", logMsg)
				return
			}

			p.log.Warnf("%s -- disconnecting", logMsg)

			go p.Disconnect(fmt.Errorf("pong failure: %w", reason))
		},
	})

	return p
}

// Start starts all helper goroutines the peer needs for normal operations.  In
// the case this peer has already been started, then this function is a noop.
func (p *Brontide) Start() error {
	if atomic.AddInt32(&p.started, 1) != 1 {
		return nil
	}

	// Once we've finished starting up the peer, we'll signal to other
	// goroutines that the they can move forward to tear down the peer, or
	// carry out other relevant changes.
	defer close(p.startReady)

	p.log.Tracef("starting with conn[%v->%v]",
		p.cfg.Conn.LocalAddr(), p.cfg.Conn.RemoteAddr())

	// Fetch and then load all the active channels we have with this remote
	// peer from the database.
	activeChans, err := p.cfg.ChannelDB.FetchOpenChannels(
		p.cfg.Addr.IdentityKey,
	)
	if err != nil {
		p.log.Errorf("Unable to fetch active chans "+
			"for peer: %v", err)
		return err
	}

	if len(activeChans) == 0 {
		go p.cfg.PrunePersistentPeerConnection(p.cfg.PubKeyBytes)
	}

	// Quickly check if we have any existing legacy channels with this
	// peer.
	haveLegacyChan := false
	for _, c := range activeChans {
		if c.ChanType.IsTweakless() {
			continue
		}

		haveLegacyChan = true
		break
	}

	// Exchange local and global features, the init message should be very
	// first between two nodes.
	if err := p.sendInitMsg(haveLegacyChan); err != nil {
		return fmt.Errorf("unable to send init msg: %w", err)
	}

	// Before we launch any of the helper goroutines off the peer struct,
	// we'll first ensure proper adherence to the p2p protocol. The init
	// message MUST be sent before any other message.
	readErr := make(chan error, 1)
	msgChan := make(chan lnwire.Message, 1)
	p.cg.WgAdd(1)
	go func() {
		defer p.cg.WgDone()

		msg, err := p.readNextMessage()
		if err != nil {
			readErr <- err
			msgChan <- nil
			return
		}
		readErr <- nil
		msgChan <- msg
	}()

	select {
	// In order to avoid blocking indefinitely, we'll give the other peer
	// an upper timeout to respond before we bail out early.
	case <-time.After(handshakeTimeout):
		return fmt.Errorf("peer did not complete handshake within %v",
			handshakeTimeout)
	case err := <-readErr:
		if err != nil {
			return fmt.Errorf("unable to read init msg: %w", err)
		}
	}

	// Once the init message arrives, we can parse it so we can figure out
	// the negotiation of features for this session.
	msg := <-msgChan
	if msg, ok := msg.(*lnwire.Init); ok {
		if err := p.handleInitMsg(msg); err != nil {
			p.storeError(err)
			return err
		}
	} else {
		return errors.New("very first message between nodes " +
			"must be init message")
	}

	// Next, load all the active channels we have with this peer,
	// registering them with the switch and launching the necessary
	// goroutines required to operate them.
	p.log.Debugf("Loaded %v active channels from database",
		len(activeChans))

	// Conditionally subscribe to channel events before loading channels so
	// we won't miss events. This subscription is used to listen to active
	// channel event when reenabling channels. Once the reenabling process
	// is finished, this subscription will be canceled.
	//
	// NOTE: ChannelNotifier must be started before subscribing events
	// otherwise we'd panic here.
	if err := p.attachChannelEventSubscription(); err != nil {
		return err
	}

	// Register the message router now as we may need to register some
	// endpoints while loading the channels below.
	p.msgRouter.WhenSome(func(router msgmux.Router) {
		router.Start(context.Background())
	})

	msgs, err := p.loadActiveChannels(activeChans)
	if err != nil {
		return fmt.Errorf("unable to load channels: %w", err)
	}

	onionMessageEndpoint := onionmessage.NewOnionEndpoint(
		p.cfg.OnionMessageServer,
	)

	// We register the onion message endpoint with the message router.
	err = fn.MapOptionZ(p.msgRouter, func(r msgmux.Router) error {
		_ = r.UnregisterEndpoint(onionMessageEndpoint.Name())

		return r.RegisterEndpoint(onionMessageEndpoint)
	})
	if err != nil {
		return fmt.Errorf("unable to register endpoint for onion "+
			"messaging: %w", err)
	}

	p.startTime = time.Now()

	// Before launching the writeHandler goroutine, we send any channel
	// sync messages that must be resent for borked channels. We do this to
	// avoid data races with WriteMessage & Flush calls.
	if len(msgs) > 0 {
		p.log.Infof("Sending %d channel sync messages to peer after "+
			"loading active channels", len(msgs))

		// Send the messages directly via writeMessage and bypass the
		// writeHandler goroutine.
		for _, msg := range msgs {
			if err := p.writeMessage(msg); err != nil {
				return fmt.Errorf("unable to send "+
					"reestablish msg: %v", err)
			}
		}
	}

	err = p.pingManager.Start()
	if err != nil {
		return fmt.Errorf("could not start ping manager %w", err)
	}

	p.cg.WgAdd(4)
	go p.queueHandler()
	go p.writeHandler()
	go p.channelManager()
	go p.readHandler()

	// Signal to any external processes that the peer is now active.
	close(p.activeSignal)

	// Node announcements don't propagate very well throughout the network
	// as there isn't a way to efficiently query for them through their
	// timestamp, mostly affecting nodes that were offline during the time
	// of broadcast. We'll resend our node announcement to the remote peer
	// as a best-effort delivery such that it can also propagate to their
	// peers. To ensure they can successfully process it in most cases,
	// we'll only resend it as long as we have at least one confirmed
	// advertised channel with the remote peer.
	//
	// TODO(wilmer): Remove this once we're able to query for node
	// announcements through their timestamps.
	p.cg.WgAdd(2)
	go p.maybeSendNodeAnn(activeChans)
	go p.maybeSendChannelUpdates()

	return nil
}

// initGossipSync initializes either a gossip syncer or an initial routing
// dump, depending on the negotiated synchronization method.
func (p *Brontide) initGossipSync() {
	// If the remote peer knows of the new gossip queries feature, then
	// we'll create a new gossipSyncer in the AuthenticatedGossiper for it.
	if p.remoteFeatures.HasFeature(lnwire.GossipQueriesOptional) {
		p.log.Info("Negotiated chan series queries")

		if p.cfg.AuthGossiper == nil {
			// This should only ever be hit in the unit tests.
			p.log.Warn("No AuthGossiper configured. Abandoning " +
				"gossip sync.")
			return
		}

		// Register the peer's gossip syncer with the gossiper.
		// This blocks synchronously to ensure the gossip syncer is
		// registered with the gossiper before attempting to read
		// messages from the remote peer.
		//
		// TODO(wilmer): Only sync updates from non-channel peers. This
		// requires an improved version of the current network
		// bootstrapper to ensure we can find and connect to non-channel
		// peers.
		p.cfg.AuthGossiper.InitSyncState(p)
	}
}

// taprootShutdownAllowed returns true if both parties have negotiated the
// shutdown-any-segwit feature.
func (p *Brontide) taprootShutdownAllowed() bool {
	return p.RemoteFeatures().HasFeature(lnwire.ShutdownAnySegwitOptional) &&
		p.LocalFeatures().HasFeature(lnwire.ShutdownAnySegwitOptional)
}

// rbfCoopCloseAllowed returns true if both parties have negotiated the new RBF
// coop close feature.
func (p *Brontide) rbfCoopCloseAllowed() bool {
	bothHaveBit := func(bit lnwire.FeatureBit) bool {
		return p.RemoteFeatures().HasFeature(bit) &&
			p.LocalFeatures().HasFeature(bit)
	}

	return bothHaveBit(lnwire.RbfCoopCloseOptional) ||
		bothHaveBit(lnwire.RbfCoopCloseOptionalStaging)
}

// QuitSignal is a method that should return a channel which will be sent upon
// or closed once the backing peer exits. This allows callers using the
// interface to cancel any processing in the event the backing implementation
// exits.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *Brontide) QuitSignal() <-chan struct{} {
	return p.cg.Done()
}

// addrWithInternalKey takes a delivery script, then attempts to supplement it
// with information related to the internal key for the addr, but only if it's
// a taproot addr.
func (p *Brontide) addrWithInternalKey(
	deliveryScript []byte) (*chancloser.DeliveryAddrWithKey, error) {

	// Currently, custom channels cannot be created with external upfront
	// shutdown addresses, so this shouldn't be an issue. We only require
	// the internal key for taproot addresses to be able to provide a non
	// inclusion proof of any scripts.
	internalKeyDesc, err := lnwallet.InternalKeyForAddr(
		p.cfg.Wallet, &p.cfg.Wallet.Cfg.NetParams, deliveryScript,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch internal key: %w", err)
	}

	return &chancloser.DeliveryAddrWithKey{
		DeliveryAddress: deliveryScript,
		InternalKey: fn.MapOption(
			func(desc keychain.KeyDescriptor) btcec.PublicKey {
				return *desc.PubKey
			},
		)(internalKeyDesc),
	}, nil
}

// loadActiveChannels creates indexes within the peer for tracking all active
// channels returned by the database. It returns a slice of channel reestablish
// messages that should be sent to the peer immediately, in case we have borked
// channels that haven't been closed yet.
func (p *Brontide) loadActiveChannels(chans []*channeldb.OpenChannel) (
	[]lnwire.Message, error) {

	// Return a slice of messages to send to the peers in case the channel
	// cannot be loaded normally.
	var msgs []lnwire.Message

	scidAliasNegotiated := p.hasNegotiatedScidAlias()

	for _, dbChan := range chans {
		hasScidFeature := dbChan.ChanType.HasScidAliasFeature()
		if scidAliasNegotiated && !hasScidFeature {
			// We'll request and store an alias, making sure that a
			// gossiper mapping is not created for the alias to the
			// real SCID. This is done because the peer and funding
			// manager are not aware of each other's states and if
			// we did not do this, we would accept alias channel
			// updates after 6 confirmations, which would be buggy.
			// We'll queue a channel_ready message with the new
			// alias. This should technically be done *after* the
			// reestablish, but this behavior is pre-existing since
			// the funding manager may already queue a
			// channel_ready before the channel_reestablish.
			if !dbChan.IsPending {
				aliasScid, err := p.cfg.RequestAlias()
				if err != nil {
					return nil, err
				}

				err = p.cfg.AddLocalAlias(
					aliasScid, dbChan.ShortChanID(), false,
					false,
				)
				if err != nil {
					return nil, err
				}

				chanID := lnwire.NewChanIDFromOutPoint(
					dbChan.FundingOutpoint,
				)

				// Fetch the second commitment point to send in
				// the channel_ready message.
				second, err := dbChan.SecondCommitmentPoint()
				if err != nil {
					return nil, err
				}

				channelReadyMsg := lnwire.NewChannelReady(
					chanID, second,
				)
				channelReadyMsg.AliasScid = &aliasScid

				msgs = append(msgs, channelReadyMsg)
			}

			// If we've negotiated the option-scid-alias feature
			// and this channel does not have ScidAliasFeature set
			// to true due to an upgrade where the feature bit was
			// turned on, we'll update the channel's database
			// state.
			err := dbChan.MarkScidAliasNegotiated()
			if err != nil {
				return nil, err
			}
		}

		var chanOpts []lnwallet.ChannelOpt
		p.cfg.AuxLeafStore.WhenSome(func(s lnwallet.AuxLeafStore) {
			chanOpts = append(chanOpts, lnwallet.WithLeafStore(s))
		})
		p.cfg.AuxSigner.WhenSome(func(s lnwallet.AuxSigner) {
			chanOpts = append(chanOpts, lnwallet.WithAuxSigner(s))
		})
		p.cfg.AuxResolver.WhenSome(
			func(s lnwallet.AuxContractResolver) {
				chanOpts = append(
					chanOpts, lnwallet.WithAuxResolver(s),
				)
			},
		)

		lnChan, err := lnwallet.NewLightningChannel(
			p.cfg.Signer, dbChan, p.cfg.SigPool, chanOpts...,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to create channel "+
				"state machine: %w", err)
		}

		chanPoint := dbChan.FundingOutpoint

		chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

		p.log.Infof("Loading ChannelPoint(%v), isPending=%v",
			chanPoint, lnChan.IsPending())

		// Skip adding any permanently irreconcilable channels to the
		// htlcswitch.
		if !dbChan.HasChanStatus(channeldb.ChanStatusDefault) &&
			!dbChan.HasChanStatus(channeldb.ChanStatusRestored) {

			p.log.Warnf("ChannelPoint(%v) has status %v, won't "+
				"start.", chanPoint, dbChan.ChanStatus())

			// To help our peer recover from a potential data loss,
			// we resend our channel reestablish message if the
			// channel is in a borked state. We won't process any
			// channel reestablish message sent from the peer, but
			// that's okay since the assumption is that we did when
			// marking the channel borked.
			chanSync, err := dbChan.ChanSyncMsg()
			if err != nil {
				p.log.Errorf("Unable to create channel "+
					"reestablish message for channel %v: "+
					"%v", chanPoint, err)
				continue
			}

			msgs = append(msgs, chanSync)

			// Check if this channel needs to have the cooperative
			// close process restarted. If so, we'll need to send
			// the Shutdown message that is returned.
			if dbChan.HasChanStatus(
				channeldb.ChanStatusCoopBroadcasted,
			) {

				shutdownMsg, err := p.restartCoopClose(lnChan)
				if err != nil {
					p.log.Errorf("Unable to restart "+
						"coop close for channel: %v",
						err)
					continue
				}

				if shutdownMsg == nil {
					continue
				}

				// Append the message to the set of messages to
				// send.
				msgs = append(msgs, shutdownMsg)
			}

			continue
		}

		// Before we register this new link with the HTLC Switch, we'll
		// need to fetch its current link-layer forwarding policy from
		// the database.
		graph := p.cfg.ChannelGraph
		info, p1, p2, err := graph.FetchChannelEdgesByOutpoint(
			&chanPoint,
		)
		if err != nil && !errors.Is(err, graphdb.ErrEdgeNotFound) {
			return nil, err
		}

		// We'll filter out our policy from the directional channel
		// edges based whom the edge connects to. If it doesn't connect
		// to us, then we know that we were the one that advertised the
		// policy.
		//
		// TODO(roasbeef): can add helper method to get policy for
		// particular channel.
		var selfPolicy *models.ChannelEdgePolicy
		if info != nil && bytes.Equal(info.NodeKey1Bytes[:],
			p.cfg.ServerPubKey[:]) {

			selfPolicy = p1
		} else {
			selfPolicy = p2
		}

		// If we don't yet have an advertised routing policy, then
		// we'll use the current default, otherwise we'll translate the
		// routing policy into a forwarding policy.
		var forwardingPolicy *models.ForwardingPolicy
		if selfPolicy != nil {
			forwardingPolicy = &models.ForwardingPolicy{
				MinHTLCOut:    selfPolicy.MinHTLC,
				MaxHTLC:       selfPolicy.MaxHTLC,
				BaseFee:       selfPolicy.FeeBaseMSat,
				FeeRate:       selfPolicy.FeeProportionalMillionths,
				TimeLockDelta: uint32(selfPolicy.TimeLockDelta),
			}
			selfPolicy.InboundFee.WhenSome(func(fee lnwire.Fee) {
				inboundFee := models.NewInboundFeeFromWire(fee)
				forwardingPolicy.InboundFee = inboundFee
			})
		} else {
			p.log.Warnf("Unable to find our forwarding policy "+
				"for channel %v, using default values",
				chanPoint)
			forwardingPolicy = &p.cfg.RoutingPolicy
		}

		p.log.Tracef("Using link policy of: %v",
			lnutils.SpewLogClosure(forwardingPolicy))

		// If the channel is pending, set the value to nil in the
		// activeChannels map. This is done to signify that the channel
		// is pending. We don't add the link to the switch here - it's
		// the funding manager's responsibility to spin up pending
		// channels. Adding them here would just be extra work as we'll
		// tear them down when creating + adding the final link.
		if lnChan.IsPending() {
			p.activeChannels.Store(chanID, nil)

			continue
		}

		shutdownInfo, err := lnChan.State().ShutdownInfo()
		if err != nil && !errors.Is(err, channeldb.ErrNoShutdownInfo) {
			return nil, err
		}

		isTaprootChan := lnChan.ChanType().IsTaproot()

		var (
			shutdownMsg     fn.Option[lnwire.Shutdown]
			shutdownInfoErr error
		)
		shutdownInfo.WhenSome(func(info channeldb.ShutdownInfo) {
			// If we can use the new RBF close feature, we don't
			// need to create the legacy closer. However for taproot
			// channels, we'll continue to use the legacy closer.
			if p.rbfCoopCloseAllowed() && !isTaprootChan {
				return
			}

			// Compute an ideal fee.
			feePerKw, err := p.cfg.FeeEstimator.EstimateFeePerKW(
				p.cfg.CoopCloseTargetConfs,
			)
			if err != nil {
				shutdownInfoErr = fmt.Errorf("unable to "+
					"estimate fee: %w", err)

				return
			}

			addr, err := p.addrWithInternalKey(
				info.DeliveryScript.Val,
			)
			if err != nil {
				shutdownInfoErr = fmt.Errorf("unable to make "+
					"delivery addr: %w", err)
				return
			}
			negotiateChanCloser, err := p.createChanCloser(
				lnChan, addr, feePerKw, nil,
				info.Closer(),
			)
			if err != nil {
				shutdownInfoErr = fmt.Errorf("unable to "+
					"create chan closer: %w", err)

				return
			}

			chanID := lnwire.NewChanIDFromOutPoint(
				lnChan.State().FundingOutpoint,
			)

			p.activeChanCloses.Store(chanID, makeNegotiateCloser(
				negotiateChanCloser,
			))

			// Create the Shutdown message.
			shutdown, err := negotiateChanCloser.ShutdownChan()
			if err != nil {
				p.activeChanCloses.Delete(chanID)
				shutdownInfoErr = err

				return
			}

			shutdownMsg = fn.Some(*shutdown)
		})
		if shutdownInfoErr != nil {
			return nil, shutdownInfoErr
		}

		// Subscribe to the set of on-chain events for this channel.
		chainEvents, err := p.cfg.ChainArb.SubscribeChannelEvents(
			chanPoint,
		)
		if err != nil {
			return nil, err
		}

		err = p.addLink(
			&chanPoint, lnChan, forwardingPolicy, chainEvents,
			true, shutdownMsg,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to add link %v to "+
				"switch: %v", chanPoint, err)
		}

		p.activeChannels.Store(chanID, lnChan)

		// We're using the old co-op close, so we don't need to init
		// the new RBF chan closer. If we have a taproot chan, then
		// we'll also use the legacy type, so we don't need to make the
		// new closer.
		if !p.rbfCoopCloseAllowed() || isTaprootChan {
			continue
		}

		// Now that the link has been added above, we'll also init an
		// RBF chan closer for this channel, but only if the new close
		// feature is negotiated.
		//
		// Creating this here ensures that any shutdown messages sent
		// will be automatically routed by the msg router.
		if _, err := p.initRbfChanCloser(lnChan); err != nil {
			p.activeChanCloses.Delete(chanID)

			return nil, fmt.Errorf("unable to init RBF chan "+
				"closer during peer connect: %w", err)
		}

		// If the shutdown info isn't blank, then we should kick things
		// off by sending a shutdown message to the remote party to
		// continue the old shutdown flow.
		restartShutdown := func(s channeldb.ShutdownInfo) error {
			return p.startRbfChanCloser(
				newRestartShutdownInit(s),
				lnChan.ChannelPoint(),
			)
		}
		err = fn.MapOptionZ(shutdownInfo, restartShutdown)
		if err != nil {
			return nil, fmt.Errorf("unable to start RBF "+
				"chan closer: %w", err)
		}
	}

	return msgs, nil
}

// addLink creates and adds a new ChannelLink from the specified channel.
func (p *Brontide) addLink(chanPoint *wire.OutPoint,
	lnChan *lnwallet.LightningChannel,
	forwardingPolicy *models.ForwardingPolicy,
	chainEvents *contractcourt.ChainEventSubscription,
	syncStates bool, shutdownMsg fn.Option[lnwire.Shutdown]) error {

	// onChannelFailure will be called by the link in case the channel
	// fails for some reason.
	onChannelFailure := func(chanID lnwire.ChannelID,
		shortChanID lnwire.ShortChannelID,
		linkErr htlcswitch.LinkFailureError) {

		failure := linkFailureReport{
			chanPoint:   *chanPoint,
			chanID:      chanID,
			shortChanID: shortChanID,
			linkErr:     linkErr,
		}

		select {
		case p.linkFailures <- failure:
		case <-p.cg.Done():
		case <-p.cfg.Quit:
		}
	}

	updateContractSignals := func(signals *contractcourt.ContractSignals) error {
		return p.cfg.ChainArb.UpdateContractSignals(*chanPoint, signals)
	}

	notifyContractUpdate := func(update *contractcourt.ContractUpdate) error {
		return p.cfg.ChainArb.NotifyContractUpdate(*chanPoint, update)
	}

	//nolint:ll
	linkCfg := htlcswitch.ChannelLinkConfig{
		Peer:                   p,
		DecodeHopIterators:     p.cfg.Sphinx.DecodeHopIterators,
		ExtractErrorEncrypter:  p.cfg.Sphinx.ExtractErrorEncrypter,
		FetchLastChannelUpdate: p.cfg.FetchLastChanUpdate,
		HodlMask:               p.cfg.Hodl.Mask(),
		Registry:               p.cfg.Invoices,
		BestHeight:             p.cfg.Switch.BestHeight,
		Circuits:               p.cfg.Switch.CircuitModifier(),
		ForwardPackets:         p.cfg.InterceptSwitch.ForwardPackets,
		FwrdingPolicy:          *forwardingPolicy,
		FeeEstimator:           p.cfg.FeeEstimator,
		PreimageCache:          p.cfg.WitnessBeacon,
		ChainEvents:            chainEvents,
		UpdateContractSignals:  updateContractSignals,
		NotifyContractUpdate:   notifyContractUpdate,
		OnChannelFailure:       onChannelFailure,
		SyncStates:             syncStates,
		BatchTicker:            ticker.New(p.cfg.ChannelCommitInterval),
		FwdPkgGCTicker:         ticker.New(time.Hour),
		PendingCommitTicker: ticker.New(
			p.cfg.PendingCommitInterval,
		),
		BatchSize:               p.cfg.ChannelCommitBatchSize,
		UnsafeReplay:            p.cfg.UnsafeReplay,
		MinUpdateTimeout:        htlcswitch.DefaultMinLinkFeeUpdateTimeout,
		MaxUpdateTimeout:        htlcswitch.DefaultMaxLinkFeeUpdateTimeout,
		OutgoingCltvRejectDelta: p.cfg.OutgoingCltvRejectDelta,
		TowerClient:             p.cfg.TowerClient,
		MaxOutgoingCltvExpiry:   p.cfg.MaxOutgoingCltvExpiry,
		MaxFeeAllocation:        p.cfg.MaxChannelFeeAllocation,
		MaxAnchorsCommitFeeRate: p.cfg.MaxAnchorsCommitFeeRate,
		NotifyActiveLink:        p.cfg.ChannelNotifier.NotifyActiveLinkEvent,
		NotifyActiveChannel:     p.cfg.ChannelNotifier.NotifyActiveChannelEvent,
		NotifyInactiveChannel:   p.cfg.ChannelNotifier.NotifyInactiveChannelEvent,
		NotifyInactiveLinkEvent: p.cfg.ChannelNotifier.NotifyInactiveLinkEvent,
		HtlcNotifier:            p.cfg.HtlcNotifier,
		GetAliases:              p.cfg.GetAliases,
		PreviouslySentShutdown:  shutdownMsg,
		DisallowRouteBlinding:   p.cfg.DisallowRouteBlinding,
		MaxFeeExposure:          p.cfg.MaxFeeExposure,
		ShouldFwdExpEndorsement: p.cfg.ShouldFwdExpEndorsement,
		DisallowQuiescence: p.cfg.DisallowQuiescence ||
			!p.remoteFeatures.HasFeature(lnwire.QuiescenceOptional),
		AuxTrafficShaper:     p.cfg.AuxTrafficShaper,
		AuxChannelNegotiator: p.cfg.AuxChannelNegotiator,
		QuiescenceTimeout:    p.cfg.QuiescenceTimeout,
	}

	// Before adding our new link, purge the switch of any pending or live
	// links going by the same channel id. If one is found, we'll shut it
	// down to ensure that the mailboxes are only ever under the control of
	// one link.
	chanID := lnwire.NewChanIDFromOutPoint(*chanPoint)
	p.cfg.Switch.RemoveLink(chanID)

	// With the channel link created, we'll now notify the htlc switch so
	// this channel can be used to dispatch local payments and also
	// passively forward payments.
	return p.cfg.Switch.CreateAndAddLink(linkCfg, lnChan)
}

// maybeSendNodeAnn sends our node announcement to the remote peer if at least
// one confirmed public channel exists with them.
func (p *Brontide) maybeSendNodeAnn(channels []*channeldb.OpenChannel) {
	defer p.cg.WgDone()

	hasConfirmedPublicChan := false
	for _, channel := range channels {
		if channel.IsPending {
			continue
		}
		if channel.ChannelFlags&lnwire.FFAnnounceChannel == 0 {
			continue
		}

		hasConfirmedPublicChan = true
		break
	}
	if !hasConfirmedPublicChan {
		return
	}

	ourNodeAnn, err := p.cfg.GenNodeAnnouncement()
	if err != nil {
		p.log.Debugf("Unable to retrieve node announcement: %v", err)
		return
	}

	if err := p.SendMessageLazy(false, &ourNodeAnn); err != nil {
		p.log.Debugf("Unable to resend node announcement: %v", err)
	}
}

// maybeSendChannelUpdates sends our channel updates to the remote peer if we
// have any active channels with them.
func (p *Brontide) maybeSendChannelUpdates() {
	defer p.cg.WgDone()

	// If we don't have any active channels, then we can exit early.
	if p.activeChannels.Len() == 0 {
		return
	}

	maybeSendUpd := func(cid lnwire.ChannelID,
		lnChan *lnwallet.LightningChannel) error {

		// Nil channels are pending, so we'll skip them.
		if lnChan == nil {
			return nil
		}

		dbChan := lnChan.State()
		scid := func() lnwire.ShortChannelID {
			switch {
			// Otherwise if it's a zero conf channel and confirmed,
			// then we need to use the "real" scid.
			case dbChan.IsZeroConf() && dbChan.ZeroConfConfirmed():
				return dbChan.ZeroConfRealScid()

			// Otherwise, we can use the normal scid.
			default:
				return dbChan.ShortChanID()
			}
		}()

		// Now that we know the channel is in a good state, we'll try
		// to fetch the update to send to the remote peer. If the
		// channel is pending, and not a zero conf channel, we'll get
		// an error here which we'll ignore.
		chanUpd, err := p.cfg.FetchLastChanUpdate(scid)
		if err != nil {
			p.log.Debugf("Unable to fetch channel update for "+
				"ChannelPoint(%v), scid=%v: %v",
				dbChan.FundingOutpoint, dbChan.ShortChanID, err)

			return nil
		}

		p.log.Debugf("Sending channel update for ChannelPoint(%v), "+
			"scid=%v", dbChan.FundingOutpoint, dbChan.ShortChanID)

		// We'll send it as a normal message instead of using the lazy
		// queue to prioritize transmission of the fresh update.
		if err := p.SendMessage(false, chanUpd); err != nil {
			err := fmt.Errorf("unable to send channel update for "+
				"ChannelPoint(%v), scid=%v: %w",
				dbChan.FundingOutpoint, dbChan.ShortChanID(),
				err)
			p.log.Errorf(err.Error())

			return err
		}

		return nil
	}

	p.activeChannels.ForEach(maybeSendUpd)
}

// WaitForDisconnect waits until the peer has disconnected. A peer may be
// disconnected if the local or remote side terminates the connection, or an
// irrecoverable protocol error has been encountered. This method will only
// begin watching the peer's waitgroup after the ready channel or the peer's
// quit channel are signaled. The ready channel should only be signaled if a
// call to Start returns no error. Otherwise, if the peer fails to start,
// calling Disconnect will signal the quit channel and the method will not
// block, since no goroutines were spawned.
func (p *Brontide) WaitForDisconnect(ready chan struct{}) {
	// Before we try to call the `Wait` goroutine, we'll make sure the main
	// set of goroutines are already active.
	select {
	case <-p.startReady:
	case <-p.cg.Done():
		return
	}

	select {
	case <-ready:
	case <-p.cg.Done():
	}

	p.cg.WgWait()
}

// Disconnect terminates the connection with the remote peer. Additionally, a
// signal is sent to the server and htlcSwitch indicating the resources
// allocated to the peer can now be cleaned up.
//
// NOTE: Be aware that this method will block if the peer is still starting up.
// Therefore consider starting it in a goroutine if you cannot guarantee that
// the peer has finished starting up before calling this method.
func (p *Brontide) Disconnect(reason error) {
	if !atomic.CompareAndSwapInt32(&p.disconnect, 0, 1) {
		return
	}

	// Make sure initialization has completed before we try to tear things
	// down.
	//
	// NOTE: We only read the `startReady` chan if the peer has been
	// started, otherwise we will skip reading it as this chan won't be
	// closed, hence blocks forever.
	if atomic.LoadInt32(&p.started) == 1 {
		p.log.Debugf("Peer hasn't finished starting up yet, waiting " +
			"on startReady signal before closing connection")

		select {
		case <-p.startReady:
		case <-p.cg.Done():
			return
		}
	}

	err := fmt.Errorf("disconnecting %s, reason: %v", p, reason)
	p.storeError(err)

	p.log.Infof(err.Error())

	// Stop PingManager before closing TCP connection.
	p.pingManager.Stop()

	// Ensure that the TCP connection is properly closed before continuing.
	p.cfg.Conn.Close()

	p.cg.Quit()

	// If our msg router isn't global (local to this instance), then we'll
	// stop it. Otherwise, we'll leave it running.
	if !p.globalMsgRouter {
		p.msgRouter.WhenSome(func(router msgmux.Router) {
			router.Stop()
		})
	}
}

// String returns the string representation of this peer.
func (p *Brontide) String() string {
	return fmt.Sprintf("%x@%s", p.cfg.PubKeyBytes, p.cfg.Conn.RemoteAddr())
}

// readNextMessage reads, and returns the next message on the wire along with
// any additional raw payload.
func (p *Brontide) readNextMessage() (lnwire.Message, error) {
	noiseConn := p.cfg.Conn
	err := noiseConn.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, err
	}

	pktLen, err := noiseConn.ReadNextHeader()
	if err != nil {
		return nil, fmt.Errorf("read next header: %w", err)
	}

	// First we'll read the next _full_ message. We do this rather than
	// reading incrementally from the stream as the Lightning wire protocol
	// is message oriented and allows nodes to pad on additional data to
	// the message stream.
	var (
		nextMsg lnwire.Message
		msgLen  uint64
	)
	err = p.cfg.ReadPool.Submit(func(buf *buffer.Read) error {
		// Before reading the body of the message, set the read timeout
		// accordingly to ensure we don't block other readers using the
		// pool. We do so only after the task has been scheduled to
		// ensure the deadline doesn't expire while the message is in
		// the process of being scheduled.
		readDeadline := time.Now().Add(
			p.scaleTimeout(readMessageTimeout),
		)
		readErr := noiseConn.SetReadDeadline(readDeadline)
		if readErr != nil {
			return readErr
		}

		// The ReadNextBody method will actually end up re-using the
		// buffer, so within this closure, we can continue to use
		// rawMsg as it's just a slice into the buf from the buffer
		// pool.
		rawMsg, readErr := noiseConn.ReadNextBody(buf[:pktLen])
		if readErr != nil {
			return fmt.Errorf("read next body: %w", readErr)
		}
		msgLen = uint64(len(rawMsg))

		// Next, create a new io.Reader implementation from the raw
		// message, and use this to decode the message directly from.
		msgReader := bytes.NewReader(rawMsg)
		nextMsg, err = lnwire.ReadMessage(msgReader, 0)
		if err != nil {
			return err
		}

		// At this point, rawMsg and buf will be returned back to the
		// buffer pool for re-use.
		return nil
	})
	atomic.AddUint64(&p.bytesReceived, msgLen)
	if err != nil {
		return nil, err
	}

	p.logWireMessage(nextMsg, true)

	return nextMsg, nil
}

// msgStream implements a goroutine-safe, in-order stream of messages to be
// delivered via closure to a receiver. These messages MUST be in order due to
// the nature of the lightning channel commitment and gossiper state machines.
// TODO(conner): use stream handler interface to abstract out stream
// state/logging.
type msgStream struct {
	streamShutdown int32 // To be used atomically.

	peer *Brontide

	apply func(lnwire.Message)

	startMsg string
	stopMsg  string

	msgCond *sync.Cond
	msgs    []lnwire.Message

	mtx sync.Mutex

	producerSema chan struct{}

	wg   sync.WaitGroup
	quit chan struct{}
}

// newMsgStream creates a new instance of a chanMsgStream for a particular
// channel identified by its channel ID. bufSize is the max number of messages
// that should be buffered in the internal queue. Callers should set this to a
// sane value that avoids blocking unnecessarily, but doesn't allow an
// unbounded amount of memory to be allocated to buffer incoming messages.
func newMsgStream(p *Brontide, startMsg, stopMsg string, bufSize uint32,
	apply func(lnwire.Message)) *msgStream {

	stream := &msgStream{
		peer:         p,
		apply:        apply,
		startMsg:     startMsg,
		stopMsg:      stopMsg,
		producerSema: make(chan struct{}, bufSize),
		quit:         make(chan struct{}),
	}
	stream.msgCond = sync.NewCond(&stream.mtx)

	// Before we return the active stream, we'll populate the producer's
	// semaphore channel. We'll use this to ensure that the producer won't
	// attempt to allocate memory in the queue for an item until it has
	// sufficient extra space.
	for i := uint32(0); i < bufSize; i++ {
		stream.producerSema <- struct{}{}
	}

	return stream
}

// Start starts the chanMsgStream.
func (ms *msgStream) Start() {
	ms.wg.Add(1)
	go ms.msgConsumer()
}

// Stop stops the chanMsgStream.
func (ms *msgStream) Stop() {
	// TODO(roasbeef): signal too?

	close(ms.quit)

	// Now that we've closed the channel, we'll repeatedly signal the msg
	// consumer until we've detected that it has exited.
	for atomic.LoadInt32(&ms.streamShutdown) == 0 {
		ms.msgCond.Signal()
		time.Sleep(time.Millisecond * 100)
	}

	ms.wg.Wait()
}

// msgConsumer is the main goroutine that streams messages from the peer's
// readHandler directly to the target channel.
func (ms *msgStream) msgConsumer() {
	defer ms.wg.Done()
	defer peerLog.Tracef(ms.stopMsg)
	defer atomic.StoreInt32(&ms.streamShutdown, 1)

	peerLog.Tracef(ms.startMsg)

	for {
		// First, we'll check our condition. If the queue of messages
		// is empty, then we'll wait until a new item is added.
		ms.msgCond.L.Lock()
		for len(ms.msgs) == 0 {
			ms.msgCond.Wait()

			// If we woke up in order to exit, then we'll do so.
			// Otherwise, we'll check the message queue for any new
			// items.
			select {
			case <-ms.peer.cg.Done():
				ms.msgCond.L.Unlock()
				return
			case <-ms.quit:
				ms.msgCond.L.Unlock()
				return
			default:
			}
		}

		// Grab the message off the front of the queue, shifting the
		// slice's reference down one in order to remove the message
		// from the queue.
		msg := ms.msgs[0]
		ms.msgs[0] = nil // Set to nil to prevent GC leak.
		ms.msgs = ms.msgs[1:]

		ms.msgCond.L.Unlock()

		ms.apply(msg)

		// We've just successfully processed an item, so we'll signal
		// to the producer that a new slot in the buffer. We'll use
		// this to bound the size of the buffer to avoid allowing it to
		// grow indefinitely.
		select {
		case ms.producerSema <- struct{}{}:
		case <-ms.peer.cg.Done():
			return
		case <-ms.quit:
			return
		}
	}
}

// AddMsg adds a new message to the msgStream. This function is safe for
// concurrent access.
func (ms *msgStream) AddMsg(msg lnwire.Message) {
	// First, we'll attempt to receive from the producerSema struct. This
	// acts as a semaphore to prevent us from indefinitely buffering
	// incoming items from the wire. Either the msg queue isn't full, and
	// we'll not block, or the queue is full, and we'll block until either
	// we're signalled to quit, or a slot is freed up.
	select {
	case <-ms.producerSema:
	case <-ms.peer.cg.Done():
		return
	case <-ms.quit:
		return
	}

	// Next, we'll lock the condition, and add the message to the end of
	// the message queue.
	ms.msgCond.L.Lock()
	ms.msgs = append(ms.msgs, msg)
	ms.msgCond.L.Unlock()

	// With the message added, we signal to the msgConsumer that there are
	// additional messages to consume.
	ms.msgCond.Signal()
}

// waitUntilLinkActive waits until the target link is active and returns a
// ChannelLink to pass messages to. It accomplishes this by subscribing to
// an ActiveLinkEvent which is emitted by the link when it first starts up.
func waitUntilLinkActive(p *Brontide,
	cid lnwire.ChannelID) htlcswitch.ChannelUpdateHandler {

	p.log.Tracef("Waiting for link=%v to be active", cid)

	// Subscribe to receive channel events.
	//
	// NOTE: If the link is already active by SubscribeChannelEvents, then
	// GetLink will retrieve the link and we can send messages. If the link
	// becomes active between SubscribeChannelEvents and GetLink, then GetLink
	// will retrieve the link. If the link becomes active after GetLink, then
	// we will get an ActiveLinkEvent notification and retrieve the link. If
	// the call to GetLink is before SubscribeChannelEvents, however, there
	// will be a race condition.
	sub, err := p.cfg.ChannelNotifier.SubscribeChannelEvents()
	if err != nil {
		// If we have a non-nil error, then the server is shutting down and we
		// can exit here and return nil. This means no message will be delivered
		// to the link.
		return nil
	}
	defer sub.Cancel()

	// The link may already be active by this point, and we may have missed the
	// ActiveLinkEvent. Check if the link exists.
	link := p.fetchLinkFromKeyAndCid(cid)
	if link != nil {
		return link
	}

	// If the link is nil, we must wait for it to be active.
	for {
		select {
		// A new event has been sent by the ChannelNotifier. We first check
		// whether the event is an ActiveLinkEvent. If it is, we'll check
		// that the event is for this channel. Otherwise, we discard the
		// message.
		case e := <-sub.Updates():
			event, ok := e.(channelnotifier.ActiveLinkEvent)
			if !ok {
				// Ignore this notification.
				continue
			}

			chanPoint := event.ChannelPoint

			// Check whether the retrieved chanPoint matches the target
			// channel id.
			if !cid.IsChanPoint(chanPoint) {
				continue
			}

			// The link shouldn't be nil as we received an
			// ActiveLinkEvent. If it is nil, we return nil and the
			// calling function should catch it.
			return p.fetchLinkFromKeyAndCid(cid)

		case <-p.cg.Done():
			return nil
		}
	}
}

// newChanMsgStream is used to create a msgStream between the peer and
// particular channel link in the htlcswitch. We utilize additional
// synchronization with the fundingManager to ensure we don't attempt to
// dispatch a message to a channel before it is fully active. A reference to the
// channel this stream forwards to is held in scope to prevent unnecessary
// lookups.
func newChanMsgStream(p *Brontide, cid lnwire.ChannelID) *msgStream {
	var chanLink htlcswitch.ChannelUpdateHandler

	apply := func(msg lnwire.Message) {
		// This check is fine because if the link no longer exists, it will
		// be removed from the activeChannels map and subsequent messages
		// shouldn't reach the chan msg stream.
		if chanLink == nil {
			chanLink = waitUntilLinkActive(p, cid)

			// If the link is still not active and the calling function
			// errored out, just return.
			if chanLink == nil {
				p.log.Warnf("Link=%v is not active", cid)
				return
			}
		}

		// In order to avoid unnecessarily delivering message
		// as the peer is exiting, we'll check quickly to see
		// if we need to exit.
		select {
		case <-p.cg.Done():
			return
		default:
		}

		chanLink.HandleChannelUpdate(msg)
	}

	return newMsgStream(p,
		fmt.Sprintf("Update stream for ChannelID(%x) created", cid[:]),
		fmt.Sprintf("Update stream for ChannelID(%x) exiting", cid[:]),
		msgStreamSize,
		apply,
	)
}

// newDiscMsgStream is used to setup a msgStream between the peer and the
// authenticated gossiper. This stream should be used to forward all remote
// channel announcements.
func newDiscMsgStream(p *Brontide) *msgStream {
	apply := func(msg lnwire.Message) {
		// TODO(elle): thread contexts through the peer system properly
		// so that a parent context can be passed in here.
		ctx := context.TODO()

		// Processing here means we send it to the gossiper which then
		// decides whether this message is processed immediately or
		// waits for dependent messages to be processed. It can also
		// happen that the message is not processed at all if it is
		// premature and the LRU cache fills up and the message is
		// deleted.
		p.log.Debugf("Processing remote msg %T", msg)

		// TODO(ziggie): ProcessRemoteAnnouncement returns an error
		// channel, but we cannot rely on it being written to.
		// Because some messages might never be processed (e.g.
		// premature channel updates). We should change the design here
		// and use the actor model pattern as soon as it is available.
		// So for now we should NOT use the error channel.
		// See https://github.com/lightningnetwork/lnd/pull/9820.
		p.cfg.AuthGossiper.ProcessRemoteAnnouncement(ctx, msg, p)
	}

	return newMsgStream(
		p,
		"Update stream for gossiper created",
		"Update stream for gossiper exited",
		msgStreamSize,
		apply,
	)
}

// readHandler is responsible for reading messages off the wire in series, then
// properly dispatching the handling of the message to the proper subsystem.
//
// NOTE: This method MUST be run as a goroutine.
func (p *Brontide) readHandler() {
	defer p.cg.WgDone()

	// We'll stop the timer after a new messages is received, and also
	// reset it after we process the next message.
	idleTimer := time.AfterFunc(idleTimeout, func() {
		err := fmt.Errorf("peer %s no answer for %s -- disconnecting",
			p, idleTimeout)
		p.Disconnect(err)
	})

	// Initialize our negotiated gossip sync method before reading messages
	// off the wire. When using gossip queries, this ensures a gossip
	// syncer is active by the time query messages arrive.
	//
	// TODO(conner): have peer store gossip syncer directly and bypass
	// gossiper?
	p.initGossipSync()

	discStream := newDiscMsgStream(p)
	discStream.Start()
	defer discStream.Stop()
out:
	for atomic.LoadInt32(&p.disconnect) == 0 {
		nextMsg, err := p.readNextMessage()
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		if err != nil {
			p.log.Infof("unable to read message from peer: %v", err)

			// If we could not read our peer's message due to an
			// unknown type or invalid alias, we continue processing
			// as normal. We store unknown message and address
			// types, as they may provide debugging insight.
			switch e := err.(type) {
			// If this is just a message we don't yet recognize,
			// we'll continue processing as normal as this allows
			// us to introduce new messages in a forwards
			// compatible manner.
			case *lnwire.UnknownMessage:
				p.storeError(e)
				idleTimer.Reset(idleTimeout)
				continue

			// If they sent us an address type that we don't yet
			// know of, then this isn't a wire error, so we'll
			// simply continue parsing the remainder of their
			// messages.
			case *lnwire.ErrUnknownAddrType:
				p.storeError(e)
				idleTimer.Reset(idleTimeout)
				continue

			// If the NodeAnnouncement1 has an invalid alias, then
			// we'll log that error above and continue so we can
			// continue to read messages from the peer. We do not
			// store this error because it is of little debugging
			// value.
			case *lnwire.ErrInvalidNodeAlias:
				idleTimer.Reset(idleTimeout)
				continue

			// If the error we encountered wasn't just a message we
			// didn't recognize, then we'll stop all processing as
			// this is a fatal error.
			default:
				break out
			}
		}

		// If a message router is active, then we'll try to have it
		// handle this message. If it can, then we're able to skip the
		// rest of the message handling logic.
		err = fn.MapOptionZ(p.msgRouter, func(r msgmux.Router) error {
			return r.RouteMsg(msgmux.PeerMsg{
				PeerPub: *p.IdentityKey(),
				Message: nextMsg,
			})
		})

		// No error occurred, and the message was handled by the
		// router.
		if err == nil {
			continue
		}

		var (
			targetChan   lnwire.ChannelID
			isLinkUpdate bool
		)

		switch msg := nextMsg.(type) {
		case *lnwire.Pong:
			// When we receive a Pong message in response to our
			// last ping message, we send it to the pingManager
			p.pingManager.ReceivedPong(msg)

		case *lnwire.Ping:
			// First, we'll store their latest ping payload within
			// the relevant atomic variable.
			p.lastPingPayload.Store(msg.PaddingBytes[:])

			// Next, we'll send over the amount of specified pong
			// bytes.
			pong := lnwire.NewPong(p.cfg.PongBuf[0:msg.NumPongBytes])
			p.queueMsg(pong, nil)

		case *lnwire.OpenChannel,
			*lnwire.AcceptChannel,
			*lnwire.FundingCreated,
			*lnwire.FundingSigned,
			*lnwire.ChannelReady:

			p.cfg.FundingManager.ProcessFundingMsg(msg, p)

		case *lnwire.Shutdown:
			select {
			case p.chanCloseMsgs <- &closeMsg{msg.ChannelID, msg}:
			case <-p.cg.Done():
				break out
			}
		case *lnwire.ClosingSigned:
			select {
			case p.chanCloseMsgs <- &closeMsg{msg.ChannelID, msg}:
			case <-p.cg.Done():
				break out
			}

		case *lnwire.Warning:
			targetChan = msg.ChanID
			isLinkUpdate = p.handleWarningOrError(targetChan, msg)

		case *lnwire.Error:
			targetChan = msg.ChanID
			isLinkUpdate = p.handleWarningOrError(targetChan, msg)

		case *lnwire.ChannelReestablish:
			targetChan = msg.ChanID
			isLinkUpdate = p.hasChannel(targetChan)

			// If we failed to find the link in question, and the
			// message received was a channel sync message, then
			// this might be a peer trying to resync closed channel.
			// In this case we'll try to resend our last channel
			// sync message, such that the peer can recover funds
			// from the closed channel.
			if !isLinkUpdate {
				err := p.resendChanSyncMsg(targetChan)
				if err != nil {
					// TODO(halseth): send error to peer?
					p.log.Errorf("resend failed: %v",
						err)
				}
			}

		// For messages that implement the LinkUpdater interface, we
		// will consider them as link updates and send them to
		// chanStream. These messages will be queued inside chanStream
		// if the channel is not active yet.
		case lnwire.LinkUpdater:
			targetChan = msg.TargetChanID()
			isLinkUpdate = p.hasChannel(targetChan)

			// Log an error if we don't have this channel. This
			// means the peer has sent us a message with unknown
			// channel ID.
			if !isLinkUpdate {
				p.log.Errorf("Unknown channel ID: %v found "+
					"in received msg=%s", targetChan,
					nextMsg.MsgType())
			}

		case *lnwire.ChannelUpdate1,
			*lnwire.ChannelAnnouncement1,
			*lnwire.NodeAnnouncement1,
			*lnwire.AnnounceSignatures1,
			*lnwire.GossipTimestampRange,
			*lnwire.QueryShortChanIDs,
			*lnwire.QueryChannelRange,
			*lnwire.ReplyChannelRange,
			*lnwire.ReplyShortChanIDsEnd:

			discStream.AddMsg(msg)

		case *lnwire.Custom:
			err := p.handleCustomMessage(msg)
			if err != nil {
				p.storeError(err)
				p.log.Errorf("%v", err)
			}

		default:
			// If the message we received is unknown to us, store
			// the type to track the failure.
			err := fmt.Errorf("unknown message type %v received",
				uint16(msg.MsgType()))
			p.storeError(err)

			p.log.Errorf("%v", err)
		}

		if isLinkUpdate {
			// If this is a channel update, then we need to feed it
			// into the channel's in-order message stream.
			p.sendLinkUpdateMsg(targetChan, nextMsg)
		}

		idleTimer.Reset(idleTimeout)
	}

	p.Disconnect(errors.New("read handler closed"))

	p.log.Trace("readHandler for peer done")
}

// handleCustomMessage handles the given custom message if a handler is
// registered.
func (p *Brontide) handleCustomMessage(msg *lnwire.Custom) error {
	if p.cfg.HandleCustomMessage == nil {
		return fmt.Errorf("no custom message handler for "+
			"message type %v", uint16(msg.MsgType()))
	}

	return p.cfg.HandleCustomMessage(p.PubKey(), msg)
}

// isLoadedFromDisk returns true if the provided channel ID is loaded from
// disk.
//
// NOTE: only returns true for pending channels.
func (p *Brontide) isLoadedFromDisk(chanID lnwire.ChannelID) bool {
	// If this is a newly added channel, no need to reestablish.
	_, added := p.addedChannels.Load(chanID)
	if added {
		return false
	}

	// Return false if the channel is unknown.
	channel, ok := p.activeChannels.Load(chanID)
	if !ok {
		return false
	}

	// During startup, we will use a nil value to mark a pending channel
	// that's loaded from disk.
	return channel == nil
}

// isActiveChannel returns true if the provided channel id is active, otherwise
// returns false.
func (p *Brontide) isActiveChannel(chanID lnwire.ChannelID) bool {
	// The channel would be nil if,
	// - the channel doesn't exist, or,
	// - the channel exists, but is pending. In this case, we don't
	//   consider this channel active.
	channel, _ := p.activeChannels.Load(chanID)

	return channel != nil
}

// isPendingChannel returns true if the provided channel ID is pending, and
// returns false if the channel is active or unknown.
func (p *Brontide) isPendingChannel(chanID lnwire.ChannelID) bool {
	// Return false if the channel is unknown.
	channel, ok := p.activeChannels.Load(chanID)
	if !ok {
		return false
	}

	return channel == nil
}

// hasChannel returns true if the peer has a pending/active channel specified
// by the channel ID.
func (p *Brontide) hasChannel(chanID lnwire.ChannelID) bool {
	_, ok := p.activeChannels.Load(chanID)
	return ok
}

// storeError stores an error in our peer's buffer of recent errors with the
// current timestamp. Errors are only stored if we have at least one active
// channel with the peer to mitigate a dos vector where a peer costlessly
// connects to us and spams us with errors.
func (p *Brontide) storeError(err error) {
	var haveChannels bool

	p.activeChannels.Range(func(_ lnwire.ChannelID,
		channel *lnwallet.LightningChannel) bool {

		// Pending channels will be nil in the activeChannels map.
		if channel == nil {
			// Return true to continue the iteration.
			return true
		}

		haveChannels = true

		// Return false to break the iteration.
		return false
	})

	// If we do not have any active channels with the peer, we do not store
	// errors as a dos mitigation.
	if !haveChannels {
		p.log.Trace("no channels with peer, not storing err")
		return
	}

	p.cfg.ErrorBuffer.Add(
		&TimestampedError{Timestamp: time.Now(), Error: err},
	)
}

// handleWarningOrError processes a warning or error msg and returns true if
// msg should be forwarded to the associated channel link. False is returned if
// any necessary forwarding of msg was already handled by this method. If msg is
// an error from a peer with an active channel, we'll store it in memory.
//
// NOTE: This method should only be called from within the readHandler.
func (p *Brontide) handleWarningOrError(chanID lnwire.ChannelID,
	msg lnwire.Message) bool {

	if errMsg, ok := msg.(*lnwire.Error); ok {
		p.storeError(errMsg)
	}

	switch {
	// Connection wide messages should be forwarded to all channel links
	// with this peer.
	case chanID == lnwire.ConnectionWideID:
		for _, chanStream := range p.activeMsgStreams {
			chanStream.AddMsg(msg)
		}

		return false

	// If the channel ID for the message corresponds to a pending channel,
	// then the funding manager will handle it.
	case p.cfg.FundingManager.IsPendingChannel(chanID, p):
		p.cfg.FundingManager.ProcessFundingMsg(msg, p)
		return false

	// If not we hand the message to the channel link for this channel.
	case p.isActiveChannel(chanID):
		return true

	default:
		return false
	}
}

// messageSummary returns a human-readable string that summarizes a
// incoming/outgoing message. Not all messages will have a summary, only those
// which have additional data that can be informative at a glance.
func messageSummary(msg lnwire.Message) string {
	switch msg := msg.(type) {
	case *lnwire.Init:
		// No summary.
		return ""

	case *lnwire.OpenChannel:
		return fmt.Sprintf("temp_chan_id=%x, chain=%v, csv=%v, amt=%v, "+
			"push_amt=%v, reserve=%v, flags=%v",
			msg.PendingChannelID[:], msg.ChainHash,
			msg.CsvDelay, msg.FundingAmount, msg.PushAmount,
			msg.ChannelReserve, msg.ChannelFlags)

	case *lnwire.AcceptChannel:
		return fmt.Sprintf("temp_chan_id=%x, reserve=%v, csv=%v, num_confs=%v",
			msg.PendingChannelID[:], msg.ChannelReserve, msg.CsvDelay,
			msg.MinAcceptDepth)

	case *lnwire.FundingCreated:
		return fmt.Sprintf("temp_chan_id=%x, chan_point=%v",
			msg.PendingChannelID[:], msg.FundingPoint)

	case *lnwire.FundingSigned:
		return fmt.Sprintf("chan_id=%v", msg.ChanID)

	case *lnwire.ChannelReady:
		return fmt.Sprintf("chan_id=%v, next_point=%x",
			msg.ChanID, msg.NextPerCommitmentPoint.SerializeCompressed())

	case *lnwire.Shutdown:
		return fmt.Sprintf("chan_id=%v, script=%x", msg.ChannelID,
			msg.Address[:])

	case *lnwire.ClosingComplete:
		return fmt.Sprintf("chan_id=%v, fee_sat=%v, locktime=%v",
			msg.ChannelID, msg.FeeSatoshis, msg.LockTime)

	case *lnwire.ClosingSig:
		return fmt.Sprintf("chan_id=%v", msg.ChannelID)

	case *lnwire.ClosingSigned:
		return fmt.Sprintf("chan_id=%v, fee_sat=%v", msg.ChannelID,
			msg.FeeSatoshis)

	case *lnwire.UpdateAddHTLC:
		var blindingPoint []byte
		msg.BlindingPoint.WhenSome(
			func(b tlv.RecordT[lnwire.BlindingPointTlvType,
				*btcec.PublicKey]) {

				blindingPoint = b.Val.SerializeCompressed()
			},
		)

		return fmt.Sprintf("chan_id=%v, id=%v, amt=%v, expiry=%v, "+
			"hash=%x, blinding_point=%x, custom_records=%v",
			msg.ChanID, msg.ID, msg.Amount, msg.Expiry,
			msg.PaymentHash[:], blindingPoint, msg.CustomRecords)

	case *lnwire.UpdateFailHTLC:
		return fmt.Sprintf("chan_id=%v, id=%v, reason=%x", msg.ChanID,
			msg.ID, msg.Reason)

	case *lnwire.UpdateFulfillHTLC:
		return fmt.Sprintf("chan_id=%v, id=%v, preimage=%x, "+
			"custom_records=%v", msg.ChanID, msg.ID,
			msg.PaymentPreimage[:], msg.CustomRecords)

	case *lnwire.CommitSig:
		return fmt.Sprintf("chan_id=%v, num_htlcs=%v", msg.ChanID,
			len(msg.HtlcSigs))

	case *lnwire.RevokeAndAck:
		return fmt.Sprintf("chan_id=%v, rev=%x, next_point=%x",
			msg.ChanID, msg.Revocation[:],
			msg.NextRevocationKey.SerializeCompressed())

	case *lnwire.UpdateFailMalformedHTLC:
		return fmt.Sprintf("chan_id=%v, id=%v, fail_code=%v",
			msg.ChanID, msg.ID, msg.FailureCode)

	case *lnwire.Warning:
		return fmt.Sprintf("%v", msg.Warning())

	case *lnwire.Error:
		return fmt.Sprintf("%v", msg.Error())

	case *lnwire.AnnounceSignatures1:
		return fmt.Sprintf("chan_id=%v, short_chan_id=%v", msg.ChannelID,
			msg.ShortChannelID.ToUint64())

	case *lnwire.ChannelAnnouncement1:
		return fmt.Sprintf("chain_hash=%v, short_chan_id=%v",
			msg.ChainHash, msg.ShortChannelID.ToUint64())

	case *lnwire.ChannelUpdate1:
		return fmt.Sprintf("chain_hash=%v, short_chan_id=%v, "+
			"mflags=%v, cflags=%v, update_time=%v", msg.ChainHash,
			msg.ShortChannelID.ToUint64(), msg.MessageFlags,
			msg.ChannelFlags, time.Unix(int64(msg.Timestamp), 0))

	case *lnwire.NodeAnnouncement1:
		return fmt.Sprintf("node=%x, update_time=%v",
			msg.NodeID, time.Unix(int64(msg.Timestamp), 0))

	case *lnwire.Ping:
		return fmt.Sprintf("ping_bytes=%x", msg.PaddingBytes[:])

	case *lnwire.Pong:
		return fmt.Sprintf("len(pong_bytes)=%d", len(msg.PongBytes[:]))

	case *lnwire.UpdateFee:
		return fmt.Sprintf("chan_id=%v, fee_update_sat=%v",
			msg.ChanID, int64(msg.FeePerKw))

	case *lnwire.ChannelReestablish:
		return fmt.Sprintf("chan_id=%v, next_local_height=%v, "+
			"remote_tail_height=%v", msg.ChanID,
			msg.NextLocalCommitHeight, msg.RemoteCommitTailHeight)

	case *lnwire.ReplyShortChanIDsEnd:
		return fmt.Sprintf("chain_hash=%v, complete=%v", msg.ChainHash,
			msg.Complete)

	case *lnwire.ReplyChannelRange:
		return fmt.Sprintf("start_height=%v, end_height=%v, "+
			"num_chans=%v, encoding=%v", msg.FirstBlockHeight,
			msg.LastBlockHeight(), len(msg.ShortChanIDs),
			msg.EncodingType)

	case *lnwire.QueryShortChanIDs:
		return fmt.Sprintf("chain_hash=%v, encoding=%v, num_chans=%v",
			msg.ChainHash, msg.EncodingType, len(msg.ShortChanIDs))

	case *lnwire.QueryChannelRange:
		return fmt.Sprintf("chain_hash=%v, start_height=%v, "+
			"end_height=%v", msg.ChainHash, msg.FirstBlockHeight,
			msg.LastBlockHeight())

	case *lnwire.GossipTimestampRange:
		return fmt.Sprintf("chain_hash=%v, first_stamp=%v, "+
			"stamp_range=%v", msg.ChainHash,
			time.Unix(int64(msg.FirstTimestamp), 0),
			msg.TimestampRange)

	case *lnwire.Stfu:
		return fmt.Sprintf("chan_id=%v, initiator=%v", msg.ChanID,
			msg.Initiator)

	case *lnwire.Custom:
		return fmt.Sprintf("type=%d", msg.Type)
	}

	return fmt.Sprintf("unknown msg type=%T", msg)
}

// logWireMessage logs the receipt or sending of particular wire message. This
// function is used rather than just logging the message in order to produce
// less spammy log messages in trace mode by setting the 'Curve" parameter to
// nil. Doing this avoids printing out each of the field elements in the curve
// parameters for secp256k1.
func (p *Brontide) logWireMessage(msg lnwire.Message, read bool) {
	summaryPrefix := "Received"
	if !read {
		summaryPrefix = "Sending"
	}

	p.log.Debugf("%v", lnutils.NewLogClosure(func() string {
		// Debug summary of message.
		summary := messageSummary(msg)
		if len(summary) > 0 {
			summary = "(" + summary + ")"
		}

		preposition := "to"
		if read {
			preposition = "from"
		}

		var msgType string
		if msg.MsgType() < lnwire.CustomTypeStart {
			msgType = msg.MsgType().String()
		} else {
			msgType = "custom"
		}

		return fmt.Sprintf("%v %v%s %v %s", summaryPrefix,
			msgType, summary, preposition, p)
	}))

	prefix := "readMessage from peer"
	if !read {
		prefix = "writeMessage to peer"
	}

	p.log.Tracef(prefix+": %v", lnutils.SpewLogClosure(msg))
}

// writeMessage writes and flushes the target lnwire.Message to the remote peer.
// If the passed message is nil, this method will only try to flush an existing
// message buffered on the connection. It is safe to call this method again
// with a nil message iff a timeout error is returned. This will continue to
// flush the pending message to the wire.
//
// NOTE:
// Besides its usage in Start, this function should not be used elsewhere
// except in writeHandler. If multiple goroutines call writeMessage at the same
// time, panics can occur because WriteMessage and Flush don't use any locking
// internally.
func (p *Brontide) writeMessage(msg lnwire.Message) error {
	// Only log the message on the first attempt.
	if msg != nil {
		p.logWireMessage(msg, false)
	}

	noiseConn := p.cfg.Conn

	flushMsg := func() error {
		// Ensure the write deadline is set before we attempt to send
		// the message.
		writeDeadline := time.Now().Add(
			p.scaleTimeout(writeMessageTimeout),
		)
		err := noiseConn.SetWriteDeadline(writeDeadline)
		if err != nil {
			return err
		}

		// Flush the pending message to the wire. If an error is
		// encountered, e.g. write timeout, the number of bytes written
		// so far will be returned.
		n, err := noiseConn.Flush()

		// Record the number of bytes written on the wire, if any.
		if n > 0 {
			atomic.AddUint64(&p.bytesSent, uint64(n))
		}

		return err
	}

	// If the current message has already been serialized, encrypted, and
	// buffered on the underlying connection we will skip straight to
	// flushing it to the wire.
	if msg == nil {
		return flushMsg()
	}

	// Otherwise, this is a new message. We'll acquire a write buffer to
	// serialize the message and buffer the ciphertext on the connection.
	err := p.cfg.WritePool.Submit(func(buf *bytes.Buffer) error {
		// Using a buffer allocated by the write pool, encode the
		// message directly into the buffer.
		_, writeErr := lnwire.WriteMessage(buf, msg, 0)
		if writeErr != nil {
			return writeErr
		}

		// Finally, write the message itself in a single swoop. This
		// will buffer the ciphertext on the underlying connection. We
		// will defer flushing the message until the write pool has been
		// released.
		return noiseConn.WriteMessage(buf.Bytes())
	})
	if err != nil {
		return err
	}

	return flushMsg()
}

// writeHandler is a goroutine dedicated to reading messages off of an incoming
// queue, and writing them out to the wire. This goroutine coordinates with the
// queueHandler in order to ensure the incoming message queue is quickly
// drained.
//
// NOTE: This method MUST be run as a goroutine.
func (p *Brontide) writeHandler() {
	// We'll stop the timer after a new messages is sent, and also reset it
	// after we process the next message.
	idleTimer := time.AfterFunc(idleTimeout, func() {
		err := fmt.Errorf("peer %s no write for %s -- disconnecting",
			p, idleTimeout)
		p.Disconnect(err)
	})

	var exitErr error

out:
	for {
		select {
		case outMsg := <-p.sendQueue:
			// Record the time at which we first attempt to send the
			// message.
			startTime := time.Now()

		retry:
			// Write out the message to the socket. If a timeout
			// error is encountered, we will catch this and retry
			// after backing off in case the remote peer is just
			// slow to process messages from the wire.
			err := p.writeMessage(outMsg.msg)
			if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
				p.log.Debugf("Write timeout detected for "+
					"peer, first write for message "+
					"attempted %v ago",
					time.Since(startTime))

				// If we received a timeout error, this implies
				// that the message was buffered on the
				// connection successfully and that a flush was
				// attempted. We'll set the message to nil so
				// that on a subsequent pass we only try to
				// flush the buffered message, and forgo
				// reserializing or reencrypting it.
				outMsg.msg = nil

				goto retry
			}

			// Message has either been successfully sent or an
			// unrecoverable error occurred.  Either way, we can
			// free the memory used to store the message.
			if bConn, ok := p.cfg.Conn.(*brontide.Conn); ok {
				bConn.ClearPendingSend()
			}

			// The write succeeded, reset the idle timer to prevent
			// us from disconnecting the peer.
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(idleTimeout)

			// If the peer requested a synchronous write, respond
			// with the error.
			if outMsg.errChan != nil {
				outMsg.errChan <- err
			}

			if err != nil {
				exitErr = fmt.Errorf("unable to write "+
					"message: %v", err)
				break out
			}

		case <-p.cg.Done():
			exitErr = lnpeer.ErrPeerExiting
			break out
		}
	}

	// Avoid an exit deadlock by ensuring WaitGroups are decremented before
	// disconnect.
	p.cg.WgDone()

	p.Disconnect(exitErr)

	p.log.Trace("writeHandler for peer done")
}

// queueHandler is responsible for accepting messages from outside subsystems
// to be eventually sent out on the wire by the writeHandler.
//
// NOTE: This method MUST be run as a goroutine.
func (p *Brontide) queueHandler() {
	defer p.cg.WgDone()

	// priorityMsgs holds an in order list of messages deemed high-priority
	// to be added to the sendQueue. This predominately includes messages
	// from the funding manager and htlcswitch.
	priorityMsgs := list.New()

	// lazyMsgs holds an in order list of messages deemed low-priority to be
	// added to the sendQueue only after all high-priority messages have
	// been queued. This predominately includes messages from the gossiper.
	lazyMsgs := list.New()

	for {
		// Examine the front of the priority queue, if it is empty check
		// the low priority queue.
		elem := priorityMsgs.Front()
		if elem == nil {
			elem = lazyMsgs.Front()
		}

		if elem != nil {
			front := elem.Value.(outgoingMsg)

			// There's an element on the queue, try adding
			// it to the sendQueue. We also watch for
			// messages on the outgoingQueue, in case the
			// writeHandler cannot accept messages on the
			// sendQueue.
			select {
			case p.sendQueue <- front:
				if front.priority {
					priorityMsgs.Remove(elem)
				} else {
					lazyMsgs.Remove(elem)
				}
			case msg := <-p.outgoingQueue:
				if msg.priority {
					priorityMsgs.PushBack(msg)
				} else {
					lazyMsgs.PushBack(msg)
				}
			case <-p.cg.Done():
				return
			}
		} else {
			// If there weren't any messages to send to the
			// writeHandler, then we'll accept a new message
			// into the queue from outside sub-systems.
			select {
			case msg := <-p.outgoingQueue:
				if msg.priority {
					priorityMsgs.PushBack(msg)
				} else {
					lazyMsgs.PushBack(msg)
				}
			case <-p.cg.Done():
				return
			}
		}
	}
}

// PingTime returns the estimated ping time to the peer in microseconds.
func (p *Brontide) PingTime() int64 {
	return p.pingManager.GetPingTimeMicroSeconds()
}

// queueMsg adds the lnwire.Message to the back of the high priority send queue.
// If the errChan is non-nil, an error is sent back if the msg failed to queue
// or failed to write, and nil otherwise.
func (p *Brontide) queueMsg(msg lnwire.Message, errChan chan error) {
	p.queue(true, msg, errChan)
}

// queueMsgLazy adds the lnwire.Message to the back of the low priority send
// queue. If the errChan is non-nil, an error is sent back if the msg failed to
// queue or failed to write, and nil otherwise.
func (p *Brontide) queueMsgLazy(msg lnwire.Message, errChan chan error) {
	p.queue(false, msg, errChan)
}

// queue sends a given message to the queueHandler using the passed priority. If
// the errChan is non-nil, an error is sent back if the msg failed to queue or
// failed to write, and nil otherwise.
func (p *Brontide) queue(priority bool, msg lnwire.Message,
	errChan chan error) {

	select {
	case p.outgoingQueue <- outgoingMsg{priority, msg, errChan}:
	case <-p.cg.Done():
		p.log.Tracef("Peer shutting down, could not enqueue msg: %v.",
			lnutils.SpewLogClosure(msg))
		if errChan != nil {
			errChan <- lnpeer.ErrPeerExiting
		}
	}
}

// ChannelSnapshots returns a slice of channel snapshots detailing all
// currently active channels maintained with the remote peer.
func (p *Brontide) ChannelSnapshots() []*channeldb.ChannelSnapshot {
	snapshots := make(
		[]*channeldb.ChannelSnapshot, 0, p.activeChannels.Len(),
	)

	p.activeChannels.ForEach(func(_ lnwire.ChannelID,
		activeChan *lnwallet.LightningChannel) error {

		// If the activeChan is nil, then we skip it as the channel is
		// pending.
		if activeChan == nil {
			return nil
		}

		// We'll only return a snapshot for channels that are
		// *immediately* available for routing payments over.
		if activeChan.RemoteNextRevocation() == nil {
			return nil
		}

		snapshot := activeChan.StateSnapshot()
		snapshots = append(snapshots, snapshot)

		return nil
	})

	return snapshots
}

// genDeliveryScript returns a new script to be used to send our funds to in
// the case of a cooperative channel close negotiation.
func (p *Brontide) genDeliveryScript() ([]byte, error) {
	// We'll send a normal p2wkh address unless we've negotiated the
	// shutdown-any-segwit feature.
	addrType := lnwallet.WitnessPubKey
	if p.taprootShutdownAllowed() {
		addrType = lnwallet.TaprootPubkey
	}

	deliveryAddr, err := p.cfg.Wallet.NewAddress(
		addrType, false, lnwallet.DefaultAccountName,
	)
	if err != nil {
		return nil, err
	}
	p.log.Infof("Delivery addr for channel close: %v",
		deliveryAddr)

	return txscript.PayToAddrScript(deliveryAddr)
}

// channelManager is goroutine dedicated to handling all requests/signals
// pertaining to the opening, cooperative closing, and force closing of all
// channels maintained with the remote peer.
//
// NOTE: This method MUST be run as a goroutine.
func (p *Brontide) channelManager() {
	defer p.cg.WgDone()

	// reenableTimeout will fire once after the configured channel status
	// interval has elapsed. This will trigger us to sign new channel
	// updates and broadcast them with the "disabled" flag unset.
	reenableTimeout := time.After(p.cfg.ChanActiveTimeout)

out:
	for {
		select {
		// A new pending channel has arrived which means we are about
		// to complete a funding workflow and is waiting for the final
		// `ChannelReady` messages to be exchanged. We will add this
		// channel to the `activeChannels` with a nil value to indicate
		// this is a pending channel.
		case req := <-p.newPendingChannel:
			p.handleNewPendingChannel(req)

		// A new channel has arrived which means we've just completed a
		// funding workflow. We'll initialize the necessary local
		// state, and notify the htlc switch of a new link.
		case req := <-p.newActiveChannel:
			p.handleNewActiveChannel(req)

		// The funding flow for a pending channel is failed, we will
		// remove it from Brontide.
		case req := <-p.removePendingChannel:
			p.handleRemovePendingChannel(req)

		// We've just received a local request to close an active
		// channel. It will either kick of a cooperative channel
		// closure negotiation, or be a notification of a breached
		// contract that should be abandoned.
		case req := <-p.localCloseChanReqs:
			p.handleLocalCloseReq(req)

		// We've received a link failure from a link that was added to
		// the switch. This will initiate the teardown of the link, and
		// initiate any on-chain closures if necessary.
		case failure := <-p.linkFailures:
			p.handleLinkFailure(failure)

		// We've received a new cooperative channel closure related
		// message from the remote peer, we'll use this message to
		// advance the chan closer state machine.
		case closeMsg := <-p.chanCloseMsgs:
			p.handleCloseMsg(closeMsg)

		// The channel reannounce delay has elapsed, broadcast the
		// reenabled channel updates to the network. This should only
		// fire once, so we set the reenableTimeout channel to nil to
		// mark it for garbage collection. If the peer is torn down
		// before firing, reenabling will not be attempted.
		// TODO(conner): consolidate reenables timers inside chan status
		// manager
		case <-reenableTimeout:
			p.reenableActiveChannels()

			// Since this channel will never fire again during the
			// lifecycle of the peer, we nil the channel to mark it
			// eligible for garbage collection, and make this
			// explicitly ineligible to receive in future calls to
			// select. This also shaves a few CPU cycles since the
			// select will ignore this case entirely.
			reenableTimeout = nil

			// Once the reenabling is attempted, we also cancel the
			// channel event subscription to free up the overflow
			// queue used in channel notifier.
			//
			// NOTE: channelEventClient will be nil if the
			// reenableTimeout is greater than 1 minute.
			if p.channelEventClient != nil {
				p.channelEventClient.Cancel()
			}

		case <-p.cg.Done():
			// As, we've been signalled to exit, we'll reset all
			// our active channel back to their default state.
			p.activeChannels.ForEach(func(_ lnwire.ChannelID,
				lc *lnwallet.LightningChannel) error {

				// Exit if the channel is nil as it's a pending
				// channel.
				if lc == nil {
					return nil
				}

				lc.ResetState()

				return nil
			})

			break out
		}
	}
}

// reenableActiveChannels searches the index of channels maintained with this
// peer, and reenables each public, non-pending channel. This is done at the
// gossip level by broadcasting a new ChannelUpdate with the disabled bit unset.
// No message will be sent if the channel is already enabled.
func (p *Brontide) reenableActiveChannels() {
	// First, filter all known channels with this peer for ones that are
	// both public and not pending.
	activePublicChans := p.filterChannelsToEnable()

	// Create a map to hold channels that needs to be retried.
	retryChans := make(map[wire.OutPoint]struct{}, len(activePublicChans))

	// For each of the public, non-pending channels, set the channel
	// disabled bit to false and send out a new ChannelUpdate. If this
	// channel is already active, the update won't be sent.
	for _, chanPoint := range activePublicChans {
		err := p.cfg.ChanStatusMgr.RequestEnable(chanPoint, false)

		switch {
		// No error occurred, continue to request the next channel.
		case err == nil:
			continue

		// Cannot auto enable a manually disabled channel so we do
		// nothing but proceed to the next channel.
		case errors.Is(err, netann.ErrEnableManuallyDisabledChan):
			p.log.Debugf("Channel(%v) was manually disabled, "+
				"ignoring automatic enable request", chanPoint)

			continue

		// If the channel is reported as inactive, we will give it
		// another chance. When handling the request, ChanStatusManager
		// will check whether the link is active or not. One of the
		// conditions is whether the link has been marked as
		// reestablished, which happens inside a goroutine(htlcManager)
		// after the link is started. And we may get a false negative
		// saying the link is not active because that goroutine hasn't
		// reached the line to mark the reestablishment. Thus we give
		// it a second chance to send the request.
		case errors.Is(err, netann.ErrEnableInactiveChan):
			// If we don't have a client created, it means we
			// shouldn't retry enabling the channel.
			if p.channelEventClient == nil {
				p.log.Errorf("Channel(%v) request enabling "+
					"failed due to inactive link",
					chanPoint)

				continue
			}

			p.log.Warnf("Channel(%v) cannot be enabled as " +
				"ChanStatusManager reported inactive, retrying")

			// Add the channel to the retry map.
			retryChans[chanPoint] = struct{}{}
		}
	}

	// Retry the channels if we have any.
	if len(retryChans) != 0 {
		p.retryRequestEnable(retryChans)
	}
}

// fetchActiveChanCloser attempts to fetch the active chan closer state machine
// for the target channel ID. If the channel isn't active an error is returned.
// Otherwise, either an existing state machine will be returned, or a new one
// will be created.
func (p *Brontide) fetchActiveChanCloser(chanID lnwire.ChannelID) (
	*chanCloserFsm, error) {

	chanCloser, found := p.activeChanCloses.Load(chanID)
	if found {
		// An entry will only be found if the closer has already been
		// created for a non-pending channel or for a channel that had
		// previously started the shutdown process but the connection
		// was restarted.
		return &chanCloser, nil
	}

	// First, we'll ensure that we actually know of the target channel. If
	// not, we'll ignore this message.
	channel, ok := p.activeChannels.Load(chanID)

	// If the channel isn't in the map or the channel is nil, return
	// ErrChannelNotFound as the channel is pending.
	if !ok || channel == nil {
		return nil, ErrChannelNotFound
	}

	// We'll create a valid closing state machine in order to respond to
	// the initiated cooperative channel closure. First, we set the
	// delivery script that our funds will be paid out to. If an upfront
	// shutdown script was set, we will use it. Otherwise, we get a fresh
	// delivery script.
	//
	// TODO: Expose option to allow upfront shutdown script from watch-only
	// accounts.
	deliveryScript := channel.LocalUpfrontShutdownScript()
	if len(deliveryScript) == 0 {
		var err error
		deliveryScript, err = p.genDeliveryScript()
		if err != nil {
			p.log.Errorf("unable to gen delivery script: %v",
				err)
			return nil, fmt.Errorf("close addr unavailable")
		}
	}

	// In order to begin fee negotiations, we'll first compute our target
	// ideal fee-per-kw.
	feePerKw, err := p.cfg.FeeEstimator.EstimateFeePerKW(
		p.cfg.CoopCloseTargetConfs,
	)
	if err != nil {
		p.log.Errorf("unable to query fee estimator: %v", err)
		return nil, fmt.Errorf("unable to estimate fee")
	}

	addr, err := p.addrWithInternalKey(deliveryScript)
	if err != nil {
		return nil, fmt.Errorf("unable to parse addr: %w", err)
	}
	negotiateChanCloser, err := p.createChanCloser(
		channel, addr, feePerKw, nil, lntypes.Remote,
	)
	if err != nil {
		p.log.Errorf("unable to create chan closer: %v", err)
		return nil, fmt.Errorf("unable to create chan closer")
	}

	chanCloser = makeNegotiateCloser(negotiateChanCloser)

	p.activeChanCloses.Store(chanID, chanCloser)

	return &chanCloser, nil
}

// filterChannelsToEnable filters a list of channels to be enabled upon start.
// The filtered channels are active channels that's neither private nor
// pending.
func (p *Brontide) filterChannelsToEnable() []wire.OutPoint {
	var activePublicChans []wire.OutPoint

	p.activeChannels.Range(func(chanID lnwire.ChannelID,
		lnChan *lnwallet.LightningChannel) bool {

		// If the lnChan is nil, continue as this is a pending channel.
		if lnChan == nil {
			return true
		}

		dbChan := lnChan.State()
		isPublic := dbChan.ChannelFlags&lnwire.FFAnnounceChannel != 0
		if !isPublic || dbChan.IsPending {
			return true
		}

		// We'll also skip any channels added during this peer's
		// lifecycle since they haven't waited out the timeout. Their
		// first announcement will be enabled, and the chan status
		// manager will begin monitoring them passively since they exist
		// in the database.
		if _, ok := p.addedChannels.Load(chanID); ok {
			return true
		}

		activePublicChans = append(
			activePublicChans, dbChan.FundingOutpoint,
		)

		return true
	})

	return activePublicChans
}

// retryRequestEnable takes a map of channel outpoints and a channel event
// client. It listens to the channel events and removes a channel from the map
// if it's matched to the event. Upon receiving an active channel event, it
// will send the enabling request again.
func (p *Brontide) retryRequestEnable(activeChans map[wire.OutPoint]struct{}) {
	p.log.Debugf("Retry enabling %v channels", len(activeChans))

	// retryEnable is a helper closure that sends an enable request and
	// removes the channel from the map if it's matched.
	retryEnable := func(chanPoint wire.OutPoint) error {
		// If this is an active channel event, check whether it's in
		// our targeted channels map.
		_, found := activeChans[chanPoint]

		// If this channel is irrelevant, return nil so the loop can
		// jump to next iteration.
		if !found {
			return nil
		}

		// Otherwise we've just received an active signal for a channel
		// that's previously failed to be enabled, we send the request
		// again.
		//
		// We only give the channel one more shot, so we delete it from
		// our map first to keep it from being attempted again.
		delete(activeChans, chanPoint)

		// Send the request.
		err := p.cfg.ChanStatusMgr.RequestEnable(chanPoint, false)
		if err != nil {
			return fmt.Errorf("request enabling channel %v "+
				"failed: %w", chanPoint, err)
		}

		return nil
	}

	for {
		// If activeChans is empty, we've done processing all the
		// channels.
		if len(activeChans) == 0 {
			p.log.Debug("Finished retry enabling channels")
			return
		}

		select {
		// A new event has been sent by the ChannelNotifier. We now
		// check whether it's an active or inactive channel event.
		case e := <-p.channelEventClient.Updates():
			// If this is an active channel event, try enable the
			// channel then jump to the next iteration.
			active, ok := e.(channelnotifier.ActiveChannelEvent)
			if ok {
				chanPoint := *active.ChannelPoint

				// If we received an error for this particular
				// channel, we log an error and won't quit as
				// we still want to retry other channels.
				if err := retryEnable(chanPoint); err != nil {
					p.log.Errorf("Retry failed: %v", err)
				}

				continue
			}

			// Otherwise check for inactive link event, and jump to
			// next iteration if it's not.
			inactive, ok := e.(channelnotifier.InactiveLinkEvent)
			if !ok {
				continue
			}

			// Found an inactive link event, if this is our
			// targeted channel, remove it from our map.
			chanPoint := *inactive.ChannelPoint
			_, found := activeChans[chanPoint]
			if !found {
				continue
			}

			delete(activeChans, chanPoint)
			p.log.Warnf("Re-enable channel %v failed, received "+
				"inactive link event", chanPoint)

		case <-p.cg.Done():
			p.log.Debugf("Peer shutdown during retry enabling")
			return
		}
	}
}

// chooseDeliveryScript takes two optionally set shutdown scripts and returns
// a suitable script to close out to. This may be nil if neither script is
// set. If both scripts are set, this function will error if they do not match.
func chooseDeliveryScript(upfront, requested lnwire.DeliveryAddress,
	genDeliveryScript func() ([]byte, error),
) (lnwire.DeliveryAddress, error) {

	switch {
	// If no script was provided, then we'll generate a new delivery script.
	case len(upfront) == 0 && len(requested) == 0:
		return genDeliveryScript()

	// If no upfront shutdown script was provided, return the user
	// requested address (which may be nil).
	case len(upfront) == 0:
		return requested, nil

	// If an upfront shutdown script was provided, and the user did not
	// request a custom shutdown script, return the upfront address.
	case len(requested) == 0:
		return upfront, nil

	// If both an upfront shutdown script and a custom close script were
	// provided, error if the user provided shutdown script does not match
	// the upfront shutdown script (because closing out to a different
	// script would violate upfront shutdown).
	case !bytes.Equal(upfront, requested):
		return nil, chancloser.ErrUpfrontShutdownScriptMismatch

	// The user requested script matches the upfront shutdown script, so we
	// can return it without error.
	default:
		return upfront, nil
	}
}

// restartCoopClose checks whether we need to restart the cooperative close
// process for a given channel.
func (p *Brontide) restartCoopClose(lnChan *lnwallet.LightningChannel) (
	*lnwire.Shutdown, error) {

	isTaprootChan := lnChan.ChanType().IsTaproot()

	// If this channel has status ChanStatusCoopBroadcasted and does not
	// have a closing transaction, then the cooperative close process was
	// started but never finished. We'll re-create the chanCloser state
	// machine and resend Shutdown. BOLT#2 requires that we retransmit
	// Shutdown exactly, but doing so would mean persisting the RPC
	// provided close script. Instead use the LocalUpfrontShutdownScript
	// or generate a script.
	c := lnChan.State()
	_, err := c.BroadcastedCooperative()
	if err != nil && err != channeldb.ErrNoCloseTx {
		// An error other than ErrNoCloseTx was encountered.
		return nil, err
	} else if err == nil && !p.rbfCoopCloseAllowed() {
		// This is a channel that doesn't support RBF coop close, and it
		// already had a coop close txn broadcast. As a result, we can
		// just exit here as all we can do is wait for it to confirm.
		return nil, nil
	}

	chanID := lnwire.NewChanIDFromOutPoint(c.FundingOutpoint)

	var deliveryScript []byte

	shutdownInfo, err := c.ShutdownInfo()
	switch {
	// We have previously stored the delivery script that we need to use
	// in the shutdown message. Re-use this script.
	case err == nil:
		shutdownInfo.WhenSome(func(info channeldb.ShutdownInfo) {
			deliveryScript = info.DeliveryScript.Val
		})

	// An error other than ErrNoShutdownInfo was returned
	case !errors.Is(err, channeldb.ErrNoShutdownInfo):
		return nil, err

	case errors.Is(err, channeldb.ErrNoShutdownInfo):
		deliveryScript = c.LocalShutdownScript
		if len(deliveryScript) == 0 {
			var err error
			deliveryScript, err = p.genDeliveryScript()
			if err != nil {
				p.log.Errorf("unable to gen delivery script: "+
					"%v", err)

				return nil, fmt.Errorf("close addr unavailable")
			}
		}
	}

	// If the new RBF co-op close is negotiated, then we'll init and start
	// that state machine, skipping the steps for the negotiate machine
	// below. We don't support this close type for taproot channels though.
	if p.rbfCoopCloseAllowed() && !isTaprootChan {
		_, err := p.initRbfChanCloser(lnChan)
		if err != nil {
			return nil, fmt.Errorf("unable to init rbf chan "+
				"closer during restart: %w", err)
		}

		shutdownDesc := fn.MapOption(
			newRestartShutdownInit,
		)(shutdownInfo)

		err = p.startRbfChanCloser(
			fn.FlattenOption(shutdownDesc), lnChan.ChannelPoint(),
		)

		return nil, err
	}

	// Compute an ideal fee.
	feePerKw, err := p.cfg.FeeEstimator.EstimateFeePerKW(
		p.cfg.CoopCloseTargetConfs,
	)
	if err != nil {
		p.log.Errorf("unable to query fee estimator: %v", err)
		return nil, fmt.Errorf("unable to estimate fee")
	}

	// Determine whether we or the peer are the initiator of the coop
	// close attempt by looking at the channel's status.
	closingParty := lntypes.Remote
	if c.HasChanStatus(channeldb.ChanStatusLocalCloseInitiator) {
		closingParty = lntypes.Local
	}

	addr, err := p.addrWithInternalKey(deliveryScript)
	if err != nil {
		return nil, fmt.Errorf("unable to parse addr: %w", err)
	}
	chanCloser, err := p.createChanCloser(
		lnChan, addr, feePerKw, nil, closingParty,
	)
	if err != nil {
		p.log.Errorf("unable to create chan closer: %v", err)
		return nil, fmt.Errorf("unable to create chan closer")
	}

	p.activeChanCloses.Store(chanID, makeNegotiateCloser(chanCloser))

	// Create the Shutdown message.
	shutdownMsg, err := chanCloser.ShutdownChan()
	if err != nil {
		p.log.Errorf("unable to create shutdown message: %v", err)
		p.activeChanCloses.Delete(chanID)
		return nil, err
	}

	return shutdownMsg, nil
}

// createChanCloser constructs a ChanCloser from the passed parameters and is
// used to de-duplicate code.
func (p *Brontide) createChanCloser(channel *lnwallet.LightningChannel,
	deliveryScript *chancloser.DeliveryAddrWithKey,
	fee chainfee.SatPerKWeight, req *htlcswitch.ChanClose,
	closer lntypes.ChannelParty) (*chancloser.ChanCloser, error) {

	_, startingHeight, err := p.cfg.ChainIO.GetBestBlock()
	if err != nil {
		p.log.Errorf("unable to obtain best block: %v", err)
		return nil, fmt.Errorf("cannot obtain best block")
	}

	// The req will only be set if we initiated the co-op closing flow.
	var maxFee chainfee.SatPerKWeight
	if req != nil {
		maxFee = req.MaxFee
	}

	chanCloser := chancloser.NewChanCloser(
		chancloser.ChanCloseCfg{
			Channel:      channel,
			MusigSession: NewMusigChanCloser(channel),
			FeeEstimator: &chancloser.SimpleCoopFeeEstimator{},
			BroadcastTx:  p.cfg.Wallet.PublishTransaction,
			AuxCloser:    p.cfg.AuxChanCloser,
			DisableChannel: func(op wire.OutPoint) error {
				return p.cfg.ChanStatusMgr.RequestDisable(
					op, false,
				)
			},
			MaxFee: maxFee,
			Disconnect: func() error {
				return p.cfg.DisconnectPeer(p.IdentityKey())
			},
			ChainParams: &p.cfg.Wallet.Cfg.NetParams,
		},
		*deliveryScript,
		fee,
		uint32(startingHeight),
		req,
		closer,
	)

	return chanCloser, nil
}

// initNegotiateChanCloser initializes the channel closer for a channel that is
// using the original "negotiation" based protocol. This path is used when
// we're the one initiating the channel close.
//
// TODO(roasbeef): can make a MsgEndpoint for existing handling logic to
// further abstract.
func (p *Brontide) initNegotiateChanCloser(req *htlcswitch.ChanClose,
	channel *lnwallet.LightningChannel) error {

	// First, we'll choose a delivery address that we'll use to send the
	// funds to in the case of a successful negotiation.

	// An upfront shutdown and user provided script are both optional, but
	// must be equal if both set  (because we cannot serve a request to
	// close out to a script which violates upfront shutdown). Get the
	// appropriate address to close out to (which may be nil if neither are
	// set) and error if they are both set and do not match.
	deliveryScript, err := chooseDeliveryScript(
		channel.LocalUpfrontShutdownScript(), req.DeliveryScript,
		p.genDeliveryScript,
	)
	if err != nil {
		return fmt.Errorf("cannot close channel %v: %w",
			req.ChanPoint, err)
	}

	addr, err := p.addrWithInternalKey(deliveryScript)
	if err != nil {
		return fmt.Errorf("unable to parse addr for channel "+
			"%v: %w", req.ChanPoint, err)
	}

	chanCloser, err := p.createChanCloser(
		channel, addr, req.TargetFeePerKw, req, lntypes.Local,
	)
	if err != nil {
		return fmt.Errorf("unable to make chan closer: %w", err)
	}

	chanID := lnwire.NewChanIDFromOutPoint(channel.ChannelPoint())
	p.activeChanCloses.Store(chanID, makeNegotiateCloser(chanCloser))

	// Finally, we'll initiate the channel shutdown within the
	// chanCloser, and send the shutdown message to the remote
	// party to kick things off.
	shutdownMsg, err := chanCloser.ShutdownChan()
	if err != nil {
		// As we were unable to shutdown the channel, we'll return it
		// back to its normal state.
		defer channel.ResetState()

		p.activeChanCloses.Delete(chanID)

		return fmt.Errorf("unable to shutdown channel: %w", err)
	}

	link := p.fetchLinkFromKeyAndCid(chanID)
	if link == nil {
		// If the link is nil then it means it was already removed from
		// the switch or it never existed in the first place. The
		// latter case is handled at the beginning of this function, so
		// in the case where it has already been removed, we can skip
		// adding the commit hook to queue a Shutdown message.
		p.log.Warnf("link not found during attempted closure: "+
			"%v", chanID)
		return nil
	}

	if !link.DisableAdds(htlcswitch.Outgoing) {
		p.log.Warnf("Outgoing link adds already "+
			"disabled: %v", link.ChanID())
	}

	link.OnCommitOnce(htlcswitch.Outgoing, func() {
		p.queueMsg(shutdownMsg, nil)
	})

	return nil
}

// ChooseAddr returns the provided address if it is non-zero length, otherwise
// None.
func ChooseAddr(addr lnwire.DeliveryAddress) fn.Option[lnwire.DeliveryAddress] {
	if len(addr) == 0 {
		return fn.None[lnwire.DeliveryAddress]()
	}

	return fn.Some(addr)
}

// observeRbfCloseUpdates observes the channel for any updates that may
// indicate that a new txid has been broadcasted, or the channel fully closed
// on chain.
func (p *Brontide) observeRbfCloseUpdates(chanCloser *chancloser.RbfChanCloser,
	closeReq *htlcswitch.ChanClose,
	coopCloseStates chancloser.RbfStateSub) {

	newStateChan := coopCloseStates.NewItemCreated.ChanOut()
	defer chanCloser.RemoveStateSub(coopCloseStates)

	var (
		lastTxids    lntypes.Dual[chainhash.Hash]
		lastFeeRates lntypes.Dual[chainfee.SatPerVByte]
	)

	maybeNotifyTxBroadcast := func(state chancloser.AsymmetricPeerState,
		party lntypes.ChannelParty) {

		// First, check to see if we have an error to report to the
		// caller. If so, then we''ll return that error and exit, as the
		// stream will exit as well.
		if closeErr, ok := state.(*chancloser.CloseErr); ok {
			// We hit an error during the last state transition, so
			// we'll extract the error then send it to the
			// user.
			err := closeErr.Err()

			peerLog.Warnf("ChannelPoint(%v): encountered close "+
				"err: %v", closeReq.ChanPoint, err)

			select {
			case closeReq.Err <- err:
			case <-closeReq.Ctx.Done():
			case <-p.cg.Done():
			}

			return
		}

		closePending, ok := state.(*chancloser.ClosePending)

		// If this isn't the close pending state, we aren't at the
		// terminal state yet.
		if !ok {
			return
		}

		// Only notify if the fee rate is greater.
		newFeeRate := closePending.FeeRate
		lastFeeRate := lastFeeRates.GetForParty(party)
		if newFeeRate <= lastFeeRate {
			peerLog.Debugf("ChannelPoint(%v): remote party made "+
				"update for fee rate %v, but we already have "+
				"a higher fee rate of %v", closeReq.ChanPoint,
				newFeeRate, lastFeeRate)

			return
		}

		feeRate := closePending.FeeRate
		lastFeeRates.SetForParty(party, feeRate)

		// At this point, we'll have a txid that we can use to notify
		// the client, but only if it's different from the last one we
		// sent. If the user attempted to bump, but was rejected due to
		// RBF, then we'll send a redundant update.
		closingTxid := closePending.CloseTx.TxHash()
		lastTxid := lastTxids.GetForParty(party)
		if closeReq != nil && closingTxid != lastTxid {
			select {
			case closeReq.Updates <- &PendingUpdate{
				Txid:        closingTxid[:],
				FeePerVbyte: fn.Some(closePending.FeeRate),
				IsLocalCloseTx: fn.Some(
					party == lntypes.Local,
				),
			}:

			case <-closeReq.Ctx.Done():
				return

			case <-p.cg.Done():
				return
			}
		}

		lastTxids.SetForParty(party, closingTxid)
	}

	peerLog.Infof("Observing RBF close updates for channel %v",
		closeReq.ChanPoint)

	// We'll consume each new incoming state to send out the appropriate
	// RPC update.
	for {
		select {
		case newState := <-newStateChan:

			switch closeState := newState.(type) {
			// Once we've reached the state of pending close, we
			// have a txid that we broadcasted.
			case *chancloser.ClosingNegotiation:
				peerState := closeState.PeerState

				// Each side may have gained a new co-op close
				// tx, so we'll examine both to see if they've
				// changed.
				maybeNotifyTxBroadcast(
					peerState.GetForParty(lntypes.Local),
					lntypes.Local,
				)
				maybeNotifyTxBroadcast(
					peerState.GetForParty(lntypes.Remote),
					lntypes.Remote,
				)

			// Otherwise, if we're transition to CloseFin, then we
			// know that we're done.
			case *chancloser.CloseFin:
				// To clean up, we'll remove the chan closer
				// from the active map, and send the final
				// update to the client.
				closingTxid := closeState.ConfirmedTx.TxHash()
				if closeReq != nil {
					closeReq.Updates <- &ChannelCloseUpdate{
						ClosingTxid: closingTxid[:],
						Success:     true,
					}
				}
				chanID := lnwire.NewChanIDFromOutPoint(
					*closeReq.ChanPoint,
				)
				p.activeChanCloses.Delete(chanID)

				return
			}

		case <-closeReq.Ctx.Done():
			return

		case <-p.cg.Done():
			return
		}
	}
}

// chanErrorReporter is a simple implementation of the
// chancloser.ErrorReporter. This is bound to a single channel by the channel
// ID.
type chanErrorReporter struct {
	chanID lnwire.ChannelID
	peer   *Brontide
}

// newChanErrorReporter creates a new instance of the chanErrorReporter.
func newChanErrorReporter(chanID lnwire.ChannelID,
	peer *Brontide) *chanErrorReporter {

	return &chanErrorReporter{
		chanID: chanID,
		peer:   peer,
	}
}

// ReportError is a method that's used to report an error that occurred during
// state machine execution. This is used by the RBF close state machine to
// terminate the state machine and send an error to the remote peer.
//
// This is a part of the chancloser.ErrorReporter interface.
func (c *chanErrorReporter) ReportError(chanErr error) {
	c.peer.log.Errorf("coop close error for channel %v: %v",
		c.chanID, chanErr)

	var errMsg []byte
	if errors.Is(chanErr, chancloser.ErrInvalidStateTransition) {
		errMsg = []byte("unexpected protocol message")
	} else {
		errMsg = []byte(chanErr.Error())
	}

	err := c.peer.SendMessageLazy(false, &lnwire.Error{
		ChanID: c.chanID,
		Data:   errMsg,
	})
	if err != nil {
		c.peer.log.Warnf("unable to send error message to peer: %v",
			err)
	}

	// After we send the error message to the peer, we'll re-initialize the
	// coop close state machine as they may send a shutdown message to
	// retry the coop close.
	lnChan, ok := c.peer.activeChannels.Load(c.chanID)
	if !ok {
		return
	}

	if lnChan == nil {
		c.peer.log.Debugf("channel %v is pending, not "+
			"re-initializing coop close state machine",
			c.chanID)

		return
	}

	if _, err := c.peer.initRbfChanCloser(lnChan); err != nil {
		c.peer.activeChanCloses.Delete(c.chanID)

		c.peer.log.Errorf("unable to init RBF chan closer after "+
			"error case: %v", err)
	}
}

// chanFlushEventSentinel is used to send the RBF coop close state machine the
// channel flushed event. We'll wait until the state machine enters the
// ChannelFlushing state, then request the link to send the event once flushed.
//
// NOTE: This MUST be run as a goroutine.
func (p *Brontide) chanFlushEventSentinel(chanCloser *chancloser.RbfChanCloser,
	link htlcswitch.ChannelUpdateHandler,
	channel *lnwallet.LightningChannel) {

	defer p.cg.WgDone()

	// If there's no link, then the channel has already been flushed, so we
	// don't need to continue.
	if link == nil {
		return
	}

	coopCloseStates := chanCloser.RegisterStateEvents()
	defer chanCloser.RemoveStateSub(coopCloseStates)

	newStateChan := coopCloseStates.NewItemCreated.ChanOut()

	sendChanFlushed := func() {
		chanState := channel.StateSnapshot()

		peerLog.Infof("ChannelPoint(%v) has been flushed for co-op "+
			"close, sending event to chan closer",
			channel.ChannelPoint())

		chanBalances := chancloser.ShutdownBalances{
			LocalBalance:  chanState.LocalBalance,
			RemoteBalance: chanState.RemoteBalance,
		}
		ctx := context.Background()
		chanCloser.SendEvent(ctx, &chancloser.ChannelFlushed{
			ShutdownBalances: chanBalances,
			FreshFlush:       true,
		})
	}

	// We'll wait until the channel enters the ChannelFlushing state. We
	// exit after a success loop. As after the first RBF iteration, the
	// channel will always be flushed.
	for {
		select {
		case newState, ok := <-newStateChan:
			if !ok {
				return
			}

			if _, ok := newState.(*chancloser.ChannelFlushing); ok {
				peerLog.Infof("ChannelPoint(%v): rbf coop "+
					"close is awaiting a flushed state, "+
					"registering with link..., ",
					channel.ChannelPoint())

				// Request the link to send the event once the
				// channel is flushed. We only need this event
				// sent once, so we can exit now.
				link.OnFlushedOnce(sendChanFlushed)

				return
			}

		case <-p.cg.Done():
			return
		}
	}
}

// initRbfChanCloser initializes the channel closer for a channel that
// is using the new RBF based co-op close protocol. This only creates the chan
// closer, but doesn't attempt to trigger any manual state transitions.
func (p *Brontide) initRbfChanCloser(
	channel *lnwallet.LightningChannel) (*chancloser.RbfChanCloser, error) {

	chanID := lnwire.NewChanIDFromOutPoint(channel.ChannelPoint())

	link := p.fetchLinkFromKeyAndCid(chanID)

	_, startingHeight, err := p.cfg.ChainIO.GetBestBlock()
	if err != nil {
		return nil, fmt.Errorf("cannot obtain best block: %w", err)
	}

	defaultFeePerKw, err := p.cfg.FeeEstimator.EstimateFeePerKW(
		p.cfg.CoopCloseTargetConfs,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to estimate fee: %w", err)
	}

	thawHeight, err := channel.AbsoluteThawHeight()
	if err != nil {
		return nil, fmt.Errorf("unable to get thaw height: %w", err)
	}

	peerPub := *p.IdentityKey()

	msgMapper := chancloser.NewRbfMsgMapper(
		uint32(startingHeight), chanID, peerPub,
	)

	initialState := chancloser.ChannelActive{}

	scid := channel.ZeroConfRealScid().UnwrapOr(
		channel.ShortChanID(),
	)

	env := chancloser.Environment{
		ChainParams:    p.cfg.Wallet.Cfg.NetParams,
		ChanPeer:       peerPub,
		ChanPoint:      channel.ChannelPoint(),
		ChanID:         chanID,
		Scid:           scid,
		ChanType:       channel.ChanType(),
		DefaultFeeRate: defaultFeePerKw.FeePerVByte(),
		ThawHeight:     fn.Some(thawHeight),
		RemoteUpfrontShutdown: ChooseAddr(
			channel.RemoteUpfrontShutdownScript(),
		),
		LocalUpfrontShutdown: ChooseAddr(
			channel.LocalUpfrontShutdownScript(),
		),
		NewDeliveryScript: func() (lnwire.DeliveryAddress, error) {
			return p.genDeliveryScript()
		},
		FeeEstimator: &chancloser.SimpleCoopFeeEstimator{},
		CloseSigner:  channel,
		ChanObserver: newChanObserver(
			channel, link, p.cfg.ChanStatusMgr,
		),
	}

	spendEvent := protofsm.RegisterSpend[chancloser.ProtocolEvent]{
		OutPoint:   channel.ChannelPoint(),
		PkScript:   channel.FundingTxOut().PkScript,
		HeightHint: channel.DeriveHeightHint(),
		PostSpendEvent: fn.Some[chancloser.RbfSpendMapper](
			chancloser.SpendMapper,
		),
	}

	daemonAdapters := NewLndDaemonAdapters(LndAdapterCfg{
		MsgSender:     newPeerMsgSender(peerPub, p),
		TxBroadcaster: p.cfg.Wallet,
		ChainNotifier: p.cfg.ChainNotifier,
	})

	protoCfg := chancloser.RbfChanCloserCfg{
		Daemon:        daemonAdapters,
		InitialState:  &initialState,
		Env:           &env,
		InitEvent:     fn.Some[protofsm.DaemonEvent](&spendEvent),
		ErrorReporter: newChanErrorReporter(chanID, p),
		MsgMapper: fn.Some[protofsm.MsgMapper[chancloser.ProtocolEvent]]( //nolint:ll
			msgMapper,
		),
	}

	ctx := context.Background()
	chanCloser := protofsm.NewStateMachine(protoCfg)
	chanCloser.Start(ctx)

	// Finally, we'll register this new endpoint with the message router so
	// future co-op close messages are handled by this state machine.
	err = fn.MapOptionZ(p.msgRouter, func(r msgmux.Router) error {
		_ = r.UnregisterEndpoint(chanCloser.Name())

		return r.RegisterEndpoint(&chanCloser)
	})
	if err != nil {
		chanCloser.Stop()

		return nil, fmt.Errorf("unable to register endpoint for co-op "+
			"close: %w", err)
	}

	p.activeChanCloses.Store(chanID, makeRbfCloser(&chanCloser))

	// Now that we've created the rbf closer state machine, we'll launch a
	// new goroutine to eventually send in the ChannelFlushed event once
	// needed.
	p.cg.WgAdd(1)
	go p.chanFlushEventSentinel(&chanCloser, link, channel)

	return &chanCloser, nil
}

// shutdownInit describes the two ways we can initiate a new shutdown. Either we
// got an RPC request to do so (left), or we sent a shutdown message to the
// party (for w/e reason), but crashed before the close was complete.
//
//nolint:ll
type shutdownInit = fn.Option[fn.Either[*htlcswitch.ChanClose, channeldb.ShutdownInfo]]

// shutdownStartFeeRate returns the fee rate that should be used for the
// shutdown.  This returns a doubly wrapped option as the shutdown info might
// be none, and the fee rate is only defined for the user initiated shutdown.
func shutdownStartFeeRate(s shutdownInit) fn.Option[chainfee.SatPerKWeight] {
	feeRateOpt := fn.MapOption(func(init fn.Either[*htlcswitch.ChanClose,
		channeldb.ShutdownInfo]) fn.Option[chainfee.SatPerKWeight] {

		var feeRate fn.Option[chainfee.SatPerKWeight]
		init.WhenLeft(func(req *htlcswitch.ChanClose) {
			feeRate = fn.Some(req.TargetFeePerKw)
		})

		return feeRate
	})(s)

	return fn.FlattenOption(feeRateOpt)
}

// shutdownStartAddr returns the delivery address that should be used when
// restarting the shutdown process.  If we didn't send a shutdown before we
// restarted, and the user didn't initiate one either, then None is returned.
func shutdownStartAddr(s shutdownInit) fn.Option[lnwire.DeliveryAddress] {
	addrOpt := fn.MapOption(func(init fn.Either[*htlcswitch.ChanClose,
		channeldb.ShutdownInfo]) fn.Option[lnwire.DeliveryAddress] {

		var addr fn.Option[lnwire.DeliveryAddress]
		init.WhenLeft(func(req *htlcswitch.ChanClose) {
			if len(req.DeliveryScript) != 0 {
				addr = fn.Some(req.DeliveryScript)
			}
		})
		init.WhenRight(func(info channeldb.ShutdownInfo) {
			addr = fn.Some(info.DeliveryScript.Val)
		})

		return addr
	})(s)

	return fn.FlattenOption(addrOpt)
}

// whenRPCShutdown registers a callback to be executed when the shutdown init
// type is and RPC request.
func whenRPCShutdown(s shutdownInit, f func(r *htlcswitch.ChanClose)) {
	s.WhenSome(func(init fn.Either[*htlcswitch.ChanClose,
		channeldb.ShutdownInfo]) {

		init.WhenLeft(f)
	})
}

// newRestartShutdownInit creates a new shutdownInit for the case where we need
// to restart the shutdown flow after a restart.
func newRestartShutdownInit(info channeldb.ShutdownInfo) shutdownInit {
	return fn.Some(fn.NewRight[*htlcswitch.ChanClose](info))
}

// newRPCShutdownInit creates a new shutdownInit for the case where we
// initiated the shutdown via an RPC client.
func newRPCShutdownInit(req *htlcswitch.ChanClose) shutdownInit {
	return fn.Some(
		fn.NewLeft[*htlcswitch.ChanClose, channeldb.ShutdownInfo](req),
	)
}

// waitUntilRbfCoastClear waits until the RBF co-op close state machine has
// advanced to a terminal state before attempting another fee bump.
func waitUntilRbfCoastClear(ctx context.Context,
	rbfCloser *chancloser.RbfChanCloser) error {

	coopCloseStates := rbfCloser.RegisterStateEvents()
	newStateChan := coopCloseStates.NewItemCreated.ChanOut()
	defer rbfCloser.RemoveStateSub(coopCloseStates)

	isTerminalState := func(newState chancloser.RbfState) bool {
		// If we're not in the negotiation sub-state, then we aren't at
		// the terminal state yet.
		state, ok := newState.(*chancloser.ClosingNegotiation)
		if !ok {
			return false
		}

		localState := state.PeerState.GetForParty(lntypes.Local)

		// If this isn't the close pending state, we aren't at the
		// terminal state yet.
		_, ok = localState.(*chancloser.ClosePending)

		return ok
	}

	// Before we enter the subscription loop below, check to see if we're
	// already in the terminal state.
	rbfState, err := rbfCloser.CurrentState()
	if err != nil {
		return err
	}
	if isTerminalState(rbfState) {
		return nil
	}

	peerLog.Debugf("Waiting for RBF iteration to complete...")

	for {
		select {
		case newState := <-newStateChan:
			if isTerminalState(newState) {
				return nil
			}

		case <-ctx.Done():
			return fmt.Errorf("context canceled")
		}
	}
}

// startRbfChanCloser kicks off the co-op close process using the new RBF based
// co-op close protocol. This is called when we're the one that's initiating
// the cooperative channel close.
//
// TODO(roasbeef): just accept the two shutdown pointer params instead??
func (p *Brontide) startRbfChanCloser(shutdown shutdownInit,
	chanPoint wire.OutPoint) error {

	// Unlike the old negotiate chan closer, we'll always create the RBF
	// chan closer on startup, so we can skip init here.
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)
	chanCloser, found := p.activeChanCloses.Load(chanID)
	if !found {
		return fmt.Errorf("rbf chan closer not found for channel %v",
			chanPoint)
	}

	defaultFeePerKw, err := shutdownStartFeeRate(
		shutdown,
	).UnwrapOrFuncErr(func() (chainfee.SatPerKWeight, error) {
		return p.cfg.FeeEstimator.EstimateFeePerKW(
			p.cfg.CoopCloseTargetConfs,
		)
	})
	if err != nil {
		return fmt.Errorf("unable to estimate fee: %w", err)
	}

	chanCloser.WhenRight(func(rbfCloser *chancloser.RbfChanCloser) {
		peerLog.Infof("ChannelPoint(%v): rbf-coop close requested, "+
			"sending shutdown", chanPoint)

		rbfState, err := rbfCloser.CurrentState()
		if err != nil {
			peerLog.Warnf("ChannelPoint(%v): unable to get "+
				"current state for rbf-coop close: %v",
				chanPoint, err)

			return
		}

		coopCloseStates := rbfCloser.RegisterStateEvents()

		// Before we send our event below, we'll launch a goroutine to
		// watch for the final terminal state to send updates to the RPC
		// client. We only need to do this if there's an RPC caller.
		var rpcShutdown bool
		whenRPCShutdown(shutdown, func(req *htlcswitch.ChanClose) {
			rpcShutdown = true

			p.cg.WgAdd(1)
			go func() {
				defer p.cg.WgDone()

				p.observeRbfCloseUpdates(
					rbfCloser, req, coopCloseStates,
				)
			}()
		})

		if !rpcShutdown {
			defer rbfCloser.RemoveStateSub(coopCloseStates)
		}

		ctx, _ := p.cg.Create(context.Background())
		feeRate := defaultFeePerKw.FeePerVByte()

		// Depending on the state of the state machine, we'll either
		// kick things off by sending shutdown, or attempt to send a new
		// offer to the remote party.
		switch rbfState.(type) {
		// The channel is still active, so we'll now kick off the co-op
		// close process by instructing it to send a shutdown message to
		// the remote party.
		case *chancloser.ChannelActive:
			rbfCloser.SendEvent(
				context.Background(),
				&chancloser.SendShutdown{
					IdealFeeRate: feeRate,
					DeliveryAddr: shutdownStartAddr(
						shutdown,
					),
				},
			)

		// If we haven't yet sent an offer (didn't have enough funds at
		// the prior fee rate), or we've sent an offer, then we'll
		// trigger a new offer event.
		case *chancloser.ClosingNegotiation:
			// Before we send the event below, we'll wait until
			// we're in a semi-terminal state.
			err := waitUntilRbfCoastClear(ctx, rbfCloser)
			if err != nil {
				peerLog.Warnf("ChannelPoint(%v): unable to "+
					"wait for coast to clear: %v",
					chanPoint, err)

				return
			}

			event := chancloser.ProtocolEvent(
				&chancloser.SendOfferEvent{
					TargetFeeRate: feeRate,
				},
			)
			rbfCloser.SendEvent(ctx, event)

		default:
			peerLog.Warnf("ChannelPoint(%v): unexpected state "+
				"for rbf-coop close: %T", chanPoint, rbfState)
		}
	})

	return nil
}

// handleLocalCloseReq kicks-off the workflow to execute a cooperative or
// forced unilateral closure of the channel initiated by a local subsystem.
func (p *Brontide) handleLocalCloseReq(req *htlcswitch.ChanClose) {
	chanID := lnwire.NewChanIDFromOutPoint(*req.ChanPoint)

	channel, ok := p.activeChannels.Load(chanID)

	// Though this function can't be called for pending channels, we still
	// check whether channel is nil for safety.
	if !ok || channel == nil {
		err := fmt.Errorf("unable to close channel, ChannelID(%v) is "+
			"unknown", chanID)
		p.log.Errorf(err.Error())
		req.Err <- err
		return
	}

	isTaprootChan := channel.ChanType().IsTaproot()

	switch req.CloseType {
	// A type of CloseRegular indicates that the user has opted to close
	// out this channel on-chain, so we execute the cooperative channel
	// closure workflow.
	case contractcourt.CloseRegular:
		var err error
		switch {
		// If this is the RBF coop state machine, then we'll instruct
		// it to send the shutdown message. This also might be an RBF
		// iteration, in which case we'll be obtaining a new
		// transaction w/ a higher fee rate.
		//
		// We don't support this close type for taproot channels yet
		// however.
		case !isTaprootChan && p.rbfCoopCloseAllowed():
			err = p.startRbfChanCloser(
				newRPCShutdownInit(req), channel.ChannelPoint(),
			)
		default:
			err = p.initNegotiateChanCloser(req, channel)
		}

		if err != nil {
			p.log.Errorf(err.Error())
			req.Err <- err
		}

	// A type of CloseBreach indicates that the counterparty has breached
	// the channel therefore we need to clean up our local state.
	case contractcourt.CloseBreach:
		// TODO(roasbeef): no longer need with newer beach logic?
		p.log.Infof("ChannelPoint(%v) has been breached, wiping "+
			"channel", req.ChanPoint)
		p.WipeChannel(req.ChanPoint)
	}
}

// linkFailureReport is sent to the channelManager whenever a link reports a
// link failure, and is forced to exit. The report houses the necessary
// information to clean up the channel state, send back the error message, and
// force close if necessary.
type linkFailureReport struct {
	chanPoint   wire.OutPoint
	chanID      lnwire.ChannelID
	shortChanID lnwire.ShortChannelID
	linkErr     htlcswitch.LinkFailureError
}

// handleLinkFailure processes a link failure report when a link in the switch
// fails. It facilitates the removal of all channel state within the peer,
// force closing the channel depending on severity, and sending the error
// message back to the remote party.
func (p *Brontide) handleLinkFailure(failure linkFailureReport) {
	// Retrieve the channel from the map of active channels. We do this to
	// have access to it even after WipeChannel remove it from the map.
	chanID := lnwire.NewChanIDFromOutPoint(failure.chanPoint)
	lnChan, _ := p.activeChannels.Load(chanID)

	// We begin by wiping the link, which will remove it from the switch,
	// such that it won't be attempted used for any more updates.
	//
	// TODO(halseth): should introduce a way to atomically stop/pause the
	// link and cancel back any adds in its mailboxes such that we can
	// safely force close without the link being added again and updates
	// being applied.
	p.WipeChannel(&failure.chanPoint)

	// If the error encountered was severe enough, we'll now force close
	// the channel to prevent reading it to the switch in the future.
	if failure.linkErr.FailureAction == htlcswitch.LinkFailureForceClose {
		p.log.Warnf("Force closing link(%v)", failure.shortChanID)

		closeTx, err := p.cfg.ChainArb.ForceCloseContract(
			failure.chanPoint,
		)
		if err != nil {
			p.log.Errorf("unable to force close "+
				"link(%v): %v", failure.shortChanID, err)
		} else {
			p.log.Infof("channel(%v) force "+
				"closed with txid %v",
				failure.shortChanID, closeTx.TxHash())
		}
	}

	// If this is a permanent failure, we will mark the channel borked.
	if failure.linkErr.PermanentFailure && lnChan != nil {
		p.log.Warnf("Marking link(%v) borked due to permanent "+
			"failure", failure.shortChanID)

		if err := lnChan.State().MarkBorked(); err != nil {
			p.log.Errorf("Unable to mark channel %v borked: %v",
				failure.shortChanID, err)
		}
	}

	// Send an error to the peer, why we failed the channel.
	if failure.linkErr.ShouldSendToPeer() {
		// If SendData is set, send it to the peer. If not, we'll use
		// the standard error messages in the payload. We only include
		// sendData in the cases where the error data does not contain
		// sensitive information.
		data := []byte(failure.linkErr.Error())
		if failure.linkErr.SendData != nil {
			data = failure.linkErr.SendData
		}

		var networkMsg lnwire.Message
		if failure.linkErr.Warning {
			networkMsg = &lnwire.Warning{
				ChanID: failure.chanID,
				Data:   data,
			}
		} else {
			networkMsg = &lnwire.Error{
				ChanID: failure.chanID,
				Data:   data,
			}
		}

		err := p.SendMessage(true, networkMsg)
		if err != nil {
			p.log.Errorf("unable to send msg to "+
				"remote peer: %v", err)
		}
	}

	// If the failure action is disconnect, then we'll execute that now. If
	// we had to send an error above, it was a sync call, so we expect the
	// message to be flushed on the wire by now.
	if failure.linkErr.FailureAction == htlcswitch.LinkFailureDisconnect {
		p.Disconnect(fmt.Errorf("link requested disconnect"))
	}
}

// fetchLinkFromKeyAndCid fetches a link from the switch via the remote's
// public key and the channel id.
func (p *Brontide) fetchLinkFromKeyAndCid(
	cid lnwire.ChannelID) htlcswitch.ChannelUpdateHandler {

	var chanLink htlcswitch.ChannelUpdateHandler

	// We don't need to check the error here, and can instead just loop
	// over the slice and return nil.
	links, _ := p.cfg.Switch.GetLinksByInterface(p.cfg.PubKeyBytes)
	for _, link := range links {
		if link.ChanID() == cid {
			chanLink = link
			break
		}
	}

	return chanLink
}

// finalizeChanClosure performs the final clean up steps once the cooperative
// closure transaction has been fully broadcast. The finalized closing state
// machine should be passed in. Once the transaction has been sufficiently
// confirmed, the channel will be marked as fully closed within the database,
// and any clients will be notified of updates to the closing state.
func (p *Brontide) finalizeChanClosure(chanCloser *chancloser.ChanCloser) {
	closeReq := chanCloser.CloseRequest()

	// First, we'll clear all indexes related to the channel in question.
	chanPoint := chanCloser.Channel().ChannelPoint()
	p.WipeChannel(&chanPoint)

	// Also clear the activeChanCloses map of this channel.
	cid := lnwire.NewChanIDFromOutPoint(chanPoint)
	p.activeChanCloses.Delete(cid) // TODO(roasbeef): existing race

	// Next, we'll launch a goroutine which will request to be notified by
	// the ChainNotifier once the closure transaction obtains a single
	// confirmation.
	notifier := p.cfg.ChainNotifier

	// If any error happens during waitForChanToClose, forward it to
	// closeReq. If this channel closure is not locally initiated, closeReq
	// will be nil, so just ignore the error.
	errChan := make(chan error, 1)
	if closeReq != nil {
		errChan = closeReq.Err
	}

	closingTx, err := chanCloser.ClosingTx()
	if err != nil {
		if closeReq != nil {
			p.log.Error(err)
			closeReq.Err <- err
		}
	}

	closingTxid := closingTx.TxHash()

	// If this is a locally requested shutdown, update the caller with a
	// new event detailing the current pending state of this request.
	if closeReq != nil {
		closeReq.Updates <- &PendingUpdate{
			Txid: closingTxid[:],
		}
	}

	localOut := chanCloser.LocalCloseOutput()
	remoteOut := chanCloser.RemoteCloseOutput()
	auxOut := chanCloser.AuxOutputs()
	go WaitForChanToClose(
		chanCloser.NegotiationHeight(), notifier, errChan,
		&chanPoint, &closingTxid, closingTx.TxOut[0].PkScript, func() {
			// Respond to the local subsystem which requested the
			// channel closure.
			if closeReq != nil {
				closeReq.Updates <- &ChannelCloseUpdate{
					ClosingTxid:       closingTxid[:],
					Success:           true,
					LocalCloseOutput:  localOut,
					RemoteCloseOutput: remoteOut,
					AuxOutputs:        auxOut,
				}
			}
		},
	)
}

// WaitForChanToClose uses the passed notifier to wait until the channel has
// been detected as closed on chain and then concludes by executing the
// following actions: the channel point will be sent over the settleChan, and
// finally the callback will be executed. If any error is encountered within
// the function, then it will be sent over the errChan.
func WaitForChanToClose(bestHeight uint32, notifier chainntnfs.ChainNotifier,
	errChan chan error, chanPoint *wire.OutPoint,
	closingTxID *chainhash.Hash, closeScript []byte, cb func()) {

	peerLog.Infof("Waiting for confirmation of close of ChannelPoint(%v) "+
		"with txid: %v", chanPoint, closingTxID)

	// TODO(roasbeef): add param for num needed confs
	confNtfn, err := notifier.RegisterConfirmationsNtfn(
		closingTxID, closeScript, 1, bestHeight,
	)
	if err != nil {
		if errChan != nil {
			errChan <- err
		}
		return
	}

	// In the case that the ChainNotifier is shutting down, all subscriber
	// notification channels will be closed, generating a nil receive.
	height, ok := <-confNtfn.Confirmed
	if !ok {
		return
	}

	// The channel has been closed, remove it from any active indexes, and
	// the database state.
	peerLog.Infof("ChannelPoint(%v) is now closed at "+
		"height %v", chanPoint, height.BlockHeight)

	// Finally, execute the closure call back to mark the confirmation of
	// the transaction closing the contract.
	cb()
}

// WipeChannel removes the passed channel point from all indexes associated with
// the peer and the switch.
func (p *Brontide) WipeChannel(chanPoint *wire.OutPoint) {
	chanID := lnwire.NewChanIDFromOutPoint(*chanPoint)

	p.activeChannels.Delete(chanID)

	// Instruct the HtlcSwitch to close this link as the channel is no
	// longer active.
	p.cfg.Switch.RemoveLink(chanID)
}

// handleInitMsg handles the incoming init message which contains global and
// local feature vectors. If feature vectors are incompatible then disconnect.
func (p *Brontide) handleInitMsg(msg *lnwire.Init) error {
	// First, merge any features from the legacy global features field into
	// those presented in the local features fields.
	err := msg.Features.Merge(msg.GlobalFeatures)
	if err != nil {
		return fmt.Errorf("unable to merge legacy global features: %w",
			err)
	}

	// Then, finalize the remote feature vector providing the flattened
	// feature bit namespace.
	p.remoteFeatures = lnwire.NewFeatureVector(
		msg.Features, lnwire.Features,
	)

	// Now that we have their features loaded, we'll ensure that they
	// didn't set any required bits that we don't know of.
	err = feature.ValidateRequired(p.remoteFeatures)
	if err != nil {
		return fmt.Errorf("invalid remote features: %w", err)
	}

	// Ensure the remote party's feature vector contains all transitive
	// dependencies. We know ours are correct since they are validated
	// during the feature manager's instantiation.
	err = feature.ValidateDeps(p.remoteFeatures)
	if err != nil {
		return fmt.Errorf("invalid remote features: %w", err)
	}

	// Now that we know we understand their requirements, we'll check to
	// see if they don't support anything that we deem to be mandatory.
	if !p.remoteFeatures.HasFeature(lnwire.DataLossProtectRequired) {
		return fmt.Errorf("data loss protection required")
	}

	// If we have an AuxChannelNegotiator and the peer sent aux features,
	// process them.
	p.cfg.AuxChannelNegotiator.WhenSome(
		func(acn lnwallet.AuxChannelNegotiator) {
			err = acn.ProcessInitRecords(
				p.cfg.PubKeyBytes, msg.CustomRecords.Copy(),
			)
		},
	)
	if err != nil {
		return fmt.Errorf("could not process init records: %w", err)
	}

	return nil
}

// LocalFeatures returns the set of global features that has been advertised by
// the local node. This allows sub-systems that use this interface to gate their
// behavior off the set of negotiated feature bits.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *Brontide) LocalFeatures() *lnwire.FeatureVector {
	return p.cfg.Features
}

// RemoteFeatures returns the set of global features that has been advertised by
// the remote node. This allows sub-systems that use this interface to gate
// their behavior off the set of negotiated feature bits.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *Brontide) RemoteFeatures() *lnwire.FeatureVector {
	return p.remoteFeatures
}

// hasNegotiatedScidAlias returns true if we've negotiated the
// option-scid-alias feature bit with the peer.
func (p *Brontide) hasNegotiatedScidAlias() bool {
	peerHas := p.remoteFeatures.HasFeature(lnwire.ScidAliasOptional)
	localHas := p.cfg.Features.HasFeature(lnwire.ScidAliasOptional)
	return peerHas && localHas
}

// sendInitMsg sends the Init message to the remote peer. This message contains
// our currently supported local and global features.
func (p *Brontide) sendInitMsg(legacyChan bool) error {
	features := p.cfg.Features.Clone()
	legacyFeatures := p.cfg.LegacyFeatures.Clone()

	// If we have a legacy channel open with a peer, we downgrade static
	// remote required to optional in case the peer does not understand the
	// required feature bit. If we do not do this, the peer will reject our
	// connection because it does not understand a required feature bit, and
	// our channel will be unusable.
	if legacyChan && features.RequiresFeature(lnwire.StaticRemoteKeyRequired) {
		p.log.Infof("Legacy channel open with peer, " +
			"downgrading static remote required feature bit to " +
			"optional")

		// Unset and set in both the local and global features to
		// ensure both sets are consistent and merge able by old and
		// new nodes.
		features.Unset(lnwire.StaticRemoteKeyRequired)
		legacyFeatures.Unset(lnwire.StaticRemoteKeyRequired)

		features.Set(lnwire.StaticRemoteKeyOptional)
		legacyFeatures.Set(lnwire.StaticRemoteKeyOptional)
	}

	msg := lnwire.NewInitMessage(
		legacyFeatures.RawFeatureVector,
		features.RawFeatureVector,
	)

	var err error

	// If we have an AuxChannelNegotiator, get custom feature bits to
	// include in the init message.
	p.cfg.AuxChannelNegotiator.WhenSome(
		func(negotiator lnwallet.AuxChannelNegotiator) {
			var auxRecords lnwire.CustomRecords
			auxRecords, err = negotiator.GetInitRecords(
				p.cfg.PubKeyBytes,
			)
			if err != nil {
				p.log.Warnf("Failed to get aux init features: "+
					"%v", err)
				return
			}

			mergedRecs := msg.CustomRecords.MergedCopy(auxRecords)
			msg.CustomRecords = mergedRecs
		},
	)
	if err != nil {
		return err
	}

	return p.writeMessage(msg)
}

// resendChanSyncMsg will attempt to find a channel sync message for the closed
// channel and resend it to our peer.
func (p *Brontide) resendChanSyncMsg(cid lnwire.ChannelID) error {
	// If we already re-sent the mssage for this channel, we won't do it
	// again.
	if _, ok := p.resentChanSyncMsg[cid]; ok {
		return nil
	}

	// Check if we have any channel sync messages stored for this channel.
	c, err := p.cfg.ChannelDB.FetchClosedChannelForID(cid)
	if err != nil {
		return fmt.Errorf("unable to fetch channel sync messages for "+
			"peer %v: %v", p, err)
	}

	if c.LastChanSyncMsg == nil {
		return fmt.Errorf("no chan sync message stored for channel %v",
			cid)
	}

	if !c.RemotePub.IsEqual(p.IdentityKey()) {
		return fmt.Errorf("ignoring channel reestablish from "+
			"peer=%x", p.IdentityKey().SerializeCompressed())
	}

	p.log.Debugf("Re-sending channel sync message for channel %v to "+
		"peer", cid)

	if err := p.SendMessage(true, c.LastChanSyncMsg); err != nil {
		return fmt.Errorf("failed resending channel sync "+
			"message to peer %v: %v", p, err)
	}

	p.log.Debugf("Re-sent channel sync message for channel %v to peer ",
		cid)

	// Note down that we sent the message, so we won't resend it again for
	// this connection.
	p.resentChanSyncMsg[cid] = struct{}{}

	return nil
}

// SendMessage sends a variadic number of high-priority messages to the remote
// peer. The first argument denotes if the method should block until the
// messages have been sent to the remote peer or an error is returned,
// otherwise it returns immediately after queuing.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *Brontide) SendMessage(sync bool, msgs ...lnwire.Message) error {
	return p.sendMessage(sync, true, msgs...)
}

// SendMessageLazy sends a variadic number of low-priority messages to the
// remote peer. The first argument denotes if the method should block until
// the messages have been sent to the remote peer or an error is returned,
// otherwise it returns immediately after queueing.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *Brontide) SendMessageLazy(sync bool, msgs ...lnwire.Message) error {
	return p.sendMessage(sync, false, msgs...)
}

// sendMessage queues a variadic number of messages using the passed priority
// to the remote peer. If sync is true, this method will block until the
// messages have been sent to the remote peer or an error is returned, otherwise
// it returns immediately after queueing.
func (p *Brontide) sendMessage(sync, priority bool, msgs ...lnwire.Message) error {
	// Add all incoming messages to the outgoing queue. A list of error
	// chans is populated for each message if the caller requested a sync
	// send.
	var errChans []chan error
	if sync {
		errChans = make([]chan error, 0, len(msgs))
	}
	for _, msg := range msgs {
		// If a sync send was requested, create an error chan to listen
		// for an ack from the writeHandler.
		var errChan chan error
		if sync {
			errChan = make(chan error, 1)
			errChans = append(errChans, errChan)
		}

		if priority {
			p.queueMsg(msg, errChan)
		} else {
			p.queueMsgLazy(msg, errChan)
		}
	}

	// Wait for all replies from the writeHandler. For async sends, this
	// will be a NOP as the list of error chans is nil.
	for _, errChan := range errChans {
		select {
		case err := <-errChan:
			return err
		case <-p.cg.Done():
			return lnpeer.ErrPeerExiting
		case <-p.cfg.Quit:
			return lnpeer.ErrPeerExiting
		}
	}

	return nil
}

// PubKey returns the pubkey of the peer in compressed serialized format.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *Brontide) PubKey() [33]byte {
	return p.cfg.PubKeyBytes
}

// IdentityKey returns the public key of the remote peer.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *Brontide) IdentityKey() *btcec.PublicKey {
	return p.cfg.Addr.IdentityKey
}

// Address returns the network address of the remote peer.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *Brontide) Address() net.Addr {
	return p.cfg.Addr.Address
}

// AddNewChannel adds a new channel to the peer. The channel should fail to be
// added if the cancel channel is closed.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *Brontide) AddNewChannel(newChan *lnpeer.NewChannel,
	cancel <-chan struct{}) error {

	errChan := make(chan error, 1)
	newChanMsg := &newChannelMsg{
		channel: newChan,
		err:     errChan,
	}

	select {
	case p.newActiveChannel <- newChanMsg:
	case <-cancel:
		return errors.New("canceled adding new channel")
	case <-p.cg.Done():
		return lnpeer.ErrPeerExiting
	}

	// We pause here to wait for the peer to recognize the new channel
	// before we close the channel barrier corresponding to the channel.
	select {
	case err := <-errChan:
		return err
	case <-p.cg.Done():
		return lnpeer.ErrPeerExiting
	}
}

// AddPendingChannel adds a pending open channel to the peer. The channel
// should fail to be added if the cancel channel is closed.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *Brontide) AddPendingChannel(cid lnwire.ChannelID,
	cancel <-chan struct{}) error {

	errChan := make(chan error, 1)
	newChanMsg := &newChannelMsg{
		channelID: cid,
		err:       errChan,
	}

	select {
	case p.newPendingChannel <- newChanMsg:

	case <-cancel:
		return errors.New("canceled adding pending channel")

	case <-p.cg.Done():
		return lnpeer.ErrPeerExiting
	}

	// We pause here to wait for the peer to recognize the new pending
	// channel before we close the channel barrier corresponding to the
	// channel.
	select {
	case err := <-errChan:
		return err

	case <-cancel:
		return errors.New("canceled adding pending channel")

	case <-p.cg.Done():
		return lnpeer.ErrPeerExiting
	}
}

// RemovePendingChannel removes a pending open channel from the peer.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *Brontide) RemovePendingChannel(cid lnwire.ChannelID) error {
	errChan := make(chan error, 1)
	newChanMsg := &newChannelMsg{
		channelID: cid,
		err:       errChan,
	}

	select {
	case p.removePendingChannel <- newChanMsg:
	case <-p.cg.Done():
		return lnpeer.ErrPeerExiting
	}

	// We pause here to wait for the peer to respond to the cancellation of
	// the pending channel before we close the channel barrier
	// corresponding to the channel.
	select {
	case err := <-errChan:
		return err

	case <-p.cg.Done():
		return lnpeer.ErrPeerExiting
	}
}

// StartTime returns the time at which the connection was established if the
// peer started successfully, and zero otherwise.
func (p *Brontide) StartTime() time.Time {
	return p.startTime
}

// handleCloseMsg is called when a new cooperative channel closure related
// message is received from the remote peer. We'll use this message to advance
// the chan closer state machine.
func (p *Brontide) handleCloseMsg(msg *closeMsg) {
	link := p.fetchLinkFromKeyAndCid(msg.cid)

	// We'll now fetch the matching closing state machine in order to
	// continue, or finalize the channel closure process.
	chanCloserE, err := p.fetchActiveChanCloser(msg.cid)
	if err != nil {
		// If the channel is not known to us, we'll simply ignore this
		// message.
		if err == ErrChannelNotFound {
			return
		}

		p.log.Errorf("Unable to respond to remote close msg: %v", err)

		errMsg := &lnwire.Error{
			ChanID: msg.cid,
			Data:   lnwire.ErrorData(err.Error()),
		}
		p.queueMsg(errMsg, nil)
		return
	}

	if chanCloserE.IsRight() {
		// TODO(roasbeef): assert?
		return
	}

	// At this point, we'll only enter this call path if a negotiate chan
	// closer was used. So we'll extract that from the either now.
	//
	// TODO(roabeef): need extra helper func for either to make cleaner
	var chanCloser *chancloser.ChanCloser
	chanCloserE.WhenLeft(func(c *chancloser.ChanCloser) {
		chanCloser = c
	})

	handleErr := func(err error) {
		err = fmt.Errorf("unable to process close msg: %w", err)
		p.log.Error(err)

		// As the negotiations failed, we'll reset the channel state
		// machine to ensure we act to on-chain events as normal.
		chanCloser.Channel().ResetState()
		if chanCloser.CloseRequest() != nil {
			chanCloser.CloseRequest().Err <- err
		}

		p.activeChanCloses.Delete(msg.cid)

		p.Disconnect(err)
	}

	// Next, we'll process the next message using the target state machine.
	// We'll either continue negotiation, or halt.
	switch typed := msg.msg.(type) {
	case *lnwire.Shutdown:
		// Disable incoming adds immediately.
		if link != nil && !link.DisableAdds(htlcswitch.Incoming) {
			p.log.Warnf("Incoming link adds already disabled: %v",
				link.ChanID())
		}

		oShutdown, err := chanCloser.ReceiveShutdown(*typed)
		if err != nil {
			handleErr(err)
			return
		}

		oShutdown.WhenSome(func(msg lnwire.Shutdown) {
			// If the link is nil it means we can immediately queue
			// the Shutdown message since we don't have to wait for
			// commitment transaction synchronization.
			if link == nil {
				p.queueMsg(&msg, nil)
				return
			}

			// Immediately disallow any new HTLC's from being added
			// in the outgoing direction.
			if !link.DisableAdds(htlcswitch.Outgoing) {
				p.log.Warnf("Outgoing link adds already "+
					"disabled: %v", link.ChanID())
			}

			// When we have a Shutdown to send, we defer it till the
			// next time we send a CommitSig to remain spec
			// compliant.
			link.OnCommitOnce(htlcswitch.Outgoing, func() {
				p.queueMsg(&msg, nil)
			})
		})

		beginNegotiation := func() {
			oClosingSigned, err := chanCloser.BeginNegotiation()
			if err != nil {
				handleErr(err)
				return
			}

			oClosingSigned.WhenSome(func(msg lnwire.ClosingSigned) {
				p.queueMsg(&msg, nil)
			})
		}

		if link == nil {
			beginNegotiation()
		} else {
			// Now we register a flush hook to advance the
			// ChanCloser and possibly send out a ClosingSigned
			// when the link finishes draining.
			link.OnFlushedOnce(func() {
				// Remove link in goroutine to prevent deadlock.
				go p.cfg.Switch.RemoveLink(msg.cid)
				beginNegotiation()
			})
		}

	case *lnwire.ClosingSigned:
		oClosingSigned, err := chanCloser.ReceiveClosingSigned(*typed)
		if err != nil {
			handleErr(err)
			return
		}

		oClosingSigned.WhenSome(func(msg lnwire.ClosingSigned) {
			p.queueMsg(&msg, nil)
		})

	default:
		panic("impossible closeMsg type")
	}

	// If we haven't finished close negotiations, then we'll continue as we
	// can't yet finalize the closure.
	if _, err := chanCloser.ClosingTx(); err != nil {
		return
	}

	// Otherwise, we've agreed on a closing fee! In this case, we'll wrap up
	// the channel closure by notifying relevant sub-systems and launching a
	// goroutine to wait for close tx conf.
	p.finalizeChanClosure(chanCloser)
}

// HandleLocalCloseChanReqs accepts a *htlcswitch.ChanClose and passes it onto
// the channelManager goroutine, which will shut down the link and possibly
// close the channel.
func (p *Brontide) HandleLocalCloseChanReqs(req *htlcswitch.ChanClose) {
	select {
	case p.localCloseChanReqs <- req:
		p.log.Info("Local close channel request is going to be " +
			"delivered to the peer")
	case <-p.cg.Done():
		p.log.Info("Unable to deliver local close channel request " +
			"to peer")
	}
}

// NetAddress returns the network of the remote peer as an lnwire.NetAddress.
func (p *Brontide) NetAddress() *lnwire.NetAddress {
	return p.cfg.Addr
}

// Inbound is a getter for the Brontide's Inbound boolean in cfg.
func (p *Brontide) Inbound() bool {
	return p.cfg.Inbound
}

// ConnReq is a getter for the Brontide's connReq in cfg.
func (p *Brontide) ConnReq() *connmgr.ConnReq {
	return p.cfg.ConnReq
}

// ErrorBuffer is a getter for the Brontide's errorBuffer in cfg.
func (p *Brontide) ErrorBuffer() *queue.CircularBuffer {
	return p.cfg.ErrorBuffer
}

// SetAddress sets the remote peer's address given an address.
func (p *Brontide) SetAddress(address net.Addr) {
	p.cfg.Addr.Address = address
}

// ActiveSignal returns the peer's active signal.
func (p *Brontide) ActiveSignal() chan struct{} {
	return p.activeSignal
}

// Conn returns a pointer to the peer's connection struct.
func (p *Brontide) Conn() net.Conn {
	return p.cfg.Conn
}

// BytesReceived returns the number of bytes received from the peer.
func (p *Brontide) BytesReceived() uint64 {
	return atomic.LoadUint64(&p.bytesReceived)
}

// BytesSent returns the number of bytes sent to the peer.
func (p *Brontide) BytesSent() uint64 {
	return atomic.LoadUint64(&p.bytesSent)
}

// LastRemotePingPayload returns the last payload the remote party sent as part
// of their ping.
func (p *Brontide) LastRemotePingPayload() []byte {
	pingPayload := p.lastPingPayload.Load()
	if pingPayload == nil {
		return []byte{}
	}

	pingBytes, ok := pingPayload.(lnwire.PingPayload)
	if !ok {
		return nil
	}

	return pingBytes
}

// attachChannelEventSubscription creates a channel event subscription and
// attaches to client to Brontide if the reenableTimeout is no greater than 1
// minute.
func (p *Brontide) attachChannelEventSubscription() error {
	// If the timeout is greater than 1 minute, it's unlikely that the link
	// hasn't yet finished its reestablishment. Return a nil without
	// creating the client to specify that we don't want to retry.
	if p.cfg.ChanActiveTimeout > 1*time.Minute {
		return nil
	}

	// When the reenable timeout is less than 1 minute, it's likely the
	// channel link hasn't finished its reestablishment yet. In that case,
	// we'll give it a second chance by subscribing to the channel update
	// events. Upon receiving the `ActiveLinkEvent`, we'll then request
	// enabling the channel again.
	sub, err := p.cfg.ChannelNotifier.SubscribeChannelEvents()
	if err != nil {
		return fmt.Errorf("SubscribeChannelEvents failed: %w", err)
	}

	p.channelEventClient = sub

	return nil
}

// updateNextRevocation updates the existing channel's next revocation if it's
// nil.
func (p *Brontide) updateNextRevocation(c *channeldb.OpenChannel) error {
	chanPoint := c.FundingOutpoint
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

	// Read the current channel.
	currentChan, loaded := p.activeChannels.Load(chanID)

	// currentChan should exist, but we perform a check anyway to avoid nil
	// pointer dereference.
	if !loaded {
		return fmt.Errorf("missing active channel with chanID=%v",
			chanID)
	}

	// currentChan should not be nil, but we perform a check anyway to
	// avoid nil pointer dereference.
	if currentChan == nil {
		return fmt.Errorf("found nil active channel with chanID=%v",
			chanID)
	}

	// If we're being sent a new channel, and our existing channel doesn't
	// have the next revocation, then we need to update the current
	// existing channel.
	if currentChan.RemoteNextRevocation() != nil {
		return nil
	}

	p.log.Infof("Processing retransmitted ChannelReady for "+
		"ChannelPoint(%v)", chanPoint)

	nextRevoke := c.RemoteNextRevocation

	err := currentChan.InitNextRevocation(nextRevoke)
	if err != nil {
		return fmt.Errorf("unable to init next revocation: %w", err)
	}

	return nil
}

// addActiveChannel adds a new active channel to the `activeChannels` map. It
// takes a `channeldb.OpenChannel`, creates a `lnwallet.LightningChannel` from
// it and assembles it with a channel link.
func (p *Brontide) addActiveChannel(c *lnpeer.NewChannel) error {
	chanPoint := c.FundingOutpoint
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

	// If we've reached this point, there are two possible scenarios.  If
	// the channel was in the active channels map as nil, then it was
	// loaded from disk and we need to send reestablish. Else, it was not
	// loaded from disk and we don't need to send reestablish as this is a
	// fresh channel.
	shouldReestablish := p.isLoadedFromDisk(chanID)

	chanOpts := c.ChanOpts
	if shouldReestablish {
		// If we have to do the reestablish dance for this channel,
		// ensure that we don't try to call InitRemoteMusigNonces twice
		// by calling SkipNonceInit.
		chanOpts = append(chanOpts, lnwallet.WithSkipNonceInit())
	}

	p.cfg.AuxLeafStore.WhenSome(func(s lnwallet.AuxLeafStore) {
		chanOpts = append(chanOpts, lnwallet.WithLeafStore(s))
	})
	p.cfg.AuxSigner.WhenSome(func(s lnwallet.AuxSigner) {
		chanOpts = append(chanOpts, lnwallet.WithAuxSigner(s))
	})
	p.cfg.AuxResolver.WhenSome(func(s lnwallet.AuxContractResolver) {
		chanOpts = append(chanOpts, lnwallet.WithAuxResolver(s))
	})

	// If not already active, we'll add this channel to the set of active
	// channels, so we can look it up later easily according to its channel
	// ID.
	lnChan, err := lnwallet.NewLightningChannel(
		p.cfg.Signer, c.OpenChannel, p.cfg.SigPool, chanOpts...,
	)
	if err != nil {
		return fmt.Errorf("unable to create LightningChannel: %w", err)
	}

	// Store the channel in the activeChannels map.
	p.activeChannels.Store(chanID, lnChan)

	p.log.Infof("New channel active ChannelPoint(%v) with peer", chanPoint)

	// Next, we'll assemble a ChannelLink along with the necessary items it
	// needs to function.
	chainEvents, err := p.cfg.ChainArb.SubscribeChannelEvents(chanPoint)
	if err != nil {
		return fmt.Errorf("unable to subscribe to chain events: %w",
			err)
	}

	// We'll query the channel DB for the new channel's initial forwarding
	// policies to determine the policy we start out with.
	initialPolicy, err := p.cfg.ChannelDB.GetInitialForwardingPolicy(chanID)
	if err != nil {
		return fmt.Errorf("unable to query for initial forwarding "+
			"policy: %v", err)
	}

	// Create the link and add it to the switch.
	err = p.addLink(
		&chanPoint, lnChan, initialPolicy, chainEvents,
		shouldReestablish, fn.None[lnwire.Shutdown](),
	)
	if err != nil {
		return fmt.Errorf("can't register new channel link(%v) with "+
			"peer", chanPoint)
	}

	isTaprootChan := c.ChanType.IsTaproot()

	// We're using the old co-op close, so we don't need to init the new RBF
	// chan closer. If this is a taproot channel, then we'll also fall
	// through, as we don't support this type yet w/ rbf close.
	if !p.rbfCoopCloseAllowed() || isTaprootChan {
		return nil
	}

	// Now that the link has been added above, we'll also init an RBF chan
	// closer for this channel, but only if the new close feature is
	// negotiated.
	//
	// Creating this here ensures that any shutdown messages sent will be
	// automatically routed by the msg router.
	if _, err := p.initRbfChanCloser(lnChan); err != nil {
		p.activeChanCloses.Delete(chanID)

		return fmt.Errorf("unable to init RBF chan closer for new "+
			"chan: %w", err)
	}

	return nil
}

// handleNewActiveChannel handles a `newChannelMsg` request. Depending on we
// know this channel ID or not, we'll either add it to the `activeChannels` map
// or init the next revocation for it.
func (p *Brontide) handleNewActiveChannel(req *newChannelMsg) {
	newChan := req.channel
	chanPoint := newChan.FundingOutpoint
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

	// Only update RemoteNextRevocation if the channel is in the
	// activeChannels map and if we added the link to the switch. Only
	// active channels will be added to the switch.
	if p.isActiveChannel(chanID) {
		p.log.Infof("Already have ChannelPoint(%v), ignoring",
			chanPoint)

		// Handle it and close the err chan on the request.
		close(req.err)

		// Update the next revocation point.
		err := p.updateNextRevocation(newChan.OpenChannel)
		if err != nil {
			p.log.Errorf(err.Error())
		}

		return
	}

	// This is a new channel, we now add it to the map.
	if err := p.addActiveChannel(req.channel); err != nil {
		// Log and send back the error to the request.
		p.log.Errorf(err.Error())
		req.err <- err

		return
	}

	// Close the err chan if everything went fine.
	close(req.err)
}

// handleNewPendingChannel takes a `newChannelMsg` request and add it to
// `activeChannels` map with nil value. This pending channel will be saved as
// it may become active in the future. Once active, the funding manager will
// send it again via `AddNewChannel`, and we'd handle the link creation there.
func (p *Brontide) handleNewPendingChannel(req *newChannelMsg) {
	defer close(req.err)

	chanID := req.channelID

	// If we already have this channel, something is wrong with the funding
	// flow as it will only be marked as active after `ChannelReady` is
	// handled. In this case, we will do nothing but log an error, just in
	// case this is a legit channel.
	if p.isActiveChannel(chanID) {
		p.log.Errorf("Channel(%v) is already active, ignoring "+
			"pending channel request", chanID)

		return
	}

	// The channel has already been added, we will do nothing and return.
	if p.isPendingChannel(chanID) {
		p.log.Infof("Channel(%v) is already added, ignoring "+
			"pending channel request", chanID)

		return
	}

	// This is a new channel, we now add it to the map `activeChannels`
	// with nil value and mark it as a newly added channel in
	// `addedChannels`.
	p.activeChannels.Store(chanID, nil)
	p.addedChannels.Store(chanID, struct{}{})
}

// handleRemovePendingChannel takes a `newChannelMsg` request and removes it
// from `activeChannels` map. The request will be ignored if the channel is
// considered active by Brontide. Noop if the channel ID cannot be found.
func (p *Brontide) handleRemovePendingChannel(req *newChannelMsg) {
	defer close(req.err)

	chanID := req.channelID

	// If we already have this channel, something is wrong with the funding
	// flow as it will only be marked as active after `ChannelReady` is
	// handled. In this case, we will log an error and exit.
	if p.isActiveChannel(chanID) {
		p.log.Errorf("Channel(%v) is active, ignoring remove request",
			chanID)
		return
	}

	// The channel has not been added yet, we will log a warning as there
	// is an unexpected call from funding manager.
	if !p.isPendingChannel(chanID) {
		p.log.Warnf("Channel(%v) not found, removing it anyway", chanID)
	}

	// Remove the record of this pending channel.
	p.activeChannels.Delete(chanID)
	p.addedChannels.Delete(chanID)
}

// sendLinkUpdateMsg sends a message that updates the channel to the
// channel's message stream.
func (p *Brontide) sendLinkUpdateMsg(cid lnwire.ChannelID, msg lnwire.Message) {
	p.log.Tracef("Sending link update msg=%v", msg.MsgType())

	chanStream, ok := p.activeMsgStreams[cid]
	if !ok {
		// If a stream hasn't yet been created, then we'll do so, add
		// it to the map, and finally start it.
		chanStream = newChanMsgStream(p, cid)
		p.activeMsgStreams[cid] = chanStream
		chanStream.Start()

		// Stop the stream when quit.
		go func() {
			<-p.cg.Done()
			chanStream.Stop()
		}()
	}

	// With the stream obtained, add the message to the stream so we can
	// continue processing message.
	chanStream.AddMsg(msg)
}

// scaleTimeout multiplies the argument duration by a constant factor depending
// on variious heuristics. Currently this is only used to check whether our peer
// appears to be connected over Tor and relaxes the timout deadline. However,
// this is subject to change and should be treated as opaque.
func (p *Brontide) scaleTimeout(timeout time.Duration) time.Duration {
	if p.isTorConnection {
		return timeout * time.Duration(torTimeoutMultiplier)
	}

	return timeout
}

// CoopCloseUpdates is a struct used to communicate updates for an active close
// to the caller.
type CoopCloseUpdates struct {
	UpdateChan chan interface{}

	ErrChan chan error
}

// ChanHasRbfCoopCloser returns true if the channel as identifier by the channel
// point has an active RBF chan closer.
func (p *Brontide) ChanHasRbfCoopCloser(chanPoint wire.OutPoint) bool {
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)
	chanCloser, found := p.activeChanCloses.Load(chanID)
	if !found {
		return false
	}

	return chanCloser.IsRight()
}

// TriggerCoopCloseRbfBump given a chan ID, and the params needed to trigger a
// new RBF co-op close update, a bump is attempted. A channel used for updates,
// along with one used to o=communicate any errors is returned. If no chan
// closer is found, then false is returned for the second argument.
func (p *Brontide) TriggerCoopCloseRbfBump(ctx context.Context,
	chanPoint wire.OutPoint, feeRate chainfee.SatPerKWeight,
	deliveryScript lnwire.DeliveryAddress) (*CoopCloseUpdates, error) {

	// If RBF coop close isn't permitted, then we'll an error.
	if !p.rbfCoopCloseAllowed() {
		return nil, fmt.Errorf("rbf coop close not enabled for " +
			"channel")
	}

	closeUpdates := &CoopCloseUpdates{
		UpdateChan: make(chan interface{}, 1),
		ErrChan:    make(chan error, 1),
	}

	// We'll re-use the existing switch struct here, even though we're
	// bypassing the switch entirely.
	closeReq := htlcswitch.ChanClose{
		CloseType:      contractcourt.CloseRegular,
		ChanPoint:      &chanPoint,
		TargetFeePerKw: feeRate,
		DeliveryScript: deliveryScript,
		Updates:        closeUpdates.UpdateChan,
		Err:            closeUpdates.ErrChan,
		Ctx:            ctx,
	}

	err := p.startRbfChanCloser(newRPCShutdownInit(&closeReq), chanPoint)
	if err != nil {
		return nil, err
	}

	return closeUpdates, nil
}
