package funding

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"golang.org/x/crypto/salsa20"
)

var (
	// byteOrder defines the endian-ness we use for encoding to and from
	// buffers.
	byteOrder = binary.BigEndian

	// checkPeerChannelReadyInterval is used when we are waiting for the
	// peer to send us ChannelReady. We will check every 1 second to see
	// if the message is received.
	//
	// NOTE: for itest, this value is changed to 10ms.
	checkPeerChannelReadyInterval = 1 * time.Second
)

// WriteOutpoint writes an outpoint to an io.Writer. This is not the same as
// the channeldb variant as this uses WriteVarBytes for the Hash.
func WriteOutpoint(w io.Writer, o *wire.OutPoint) error {
	scratch := make([]byte, 4)

	if err := wire.WriteVarBytes(w, 0, o.Hash[:]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch, o.Index)
	_, err := w.Write(scratch)
	return err
}

const (
	// MinBtcRemoteDelay is the minimum CSV delay we will require the remote
	// to use for its commitment transaction.
	MinBtcRemoteDelay uint16 = 144

	// MaxBtcRemoteDelay is the maximum CSV delay we will require the remote
	// to use for its commitment transaction.
	MaxBtcRemoteDelay uint16 = 2016

	// MinLtcRemoteDelay is the minimum Litecoin CSV delay we will require
	// the remote to use for its commitment transaction.
	MinLtcRemoteDelay uint16 = 576

	// MaxLtcRemoteDelay is the maximum Litecoin CSV delay we will require
	// the remote to use for its commitment transaction.
	MaxLtcRemoteDelay uint16 = 8064

	// MinChanFundingSize is the smallest channel that we'll allow to be
	// created over the RPC interface.
	MinChanFundingSize = btcutil.Amount(20000)

	// MaxBtcFundingAmount is a soft-limit of the maximum channel size
	// currently accepted on the Bitcoin chain within the Lightning
	// Protocol. This limit is defined in BOLT-0002, and serves as an
	// initial precautionary limit while implementations are battle tested
	// in the real world.
	MaxBtcFundingAmount = btcutil.Amount(1<<24) - 1

	// MaxBtcFundingAmountWumbo is a soft-limit on the maximum size of wumbo
	// channels. This limit is 10 BTC and is the only thing standing between
	// you and limitless channel size (apart from 21 million cap).
	MaxBtcFundingAmountWumbo = btcutil.Amount(1000000000)

	// MaxLtcFundingAmount is a soft-limit of the maximum channel size
	// currently accepted on the Litecoin chain within the Lightning
	// Protocol.
	MaxLtcFundingAmount = MaxBtcFundingAmount *
		chainreg.BtcToLtcConversionRate

	// TODO(roasbeef): tune.
	msgBufferSize = 50

	// maxWaitNumBlocksFundingConf is the maximum number of blocks to wait
	// for the funding transaction to be confirmed before forgetting
	// channels that aren't initiated by us. 2016 blocks is ~2 weeks.
	maxWaitNumBlocksFundingConf = 2016

	// pendingChansLimit is the maximum number of pending channels that we
	// can have. After this point, pending channel opens will start to be
	// rejected.
	pendingChansLimit = 1_000
)

var (
	// ErrFundingManagerShuttingDown is an error returned when attempting to
	// process a funding request/message but the funding manager has already
	// been signaled to shut down.
	ErrFundingManagerShuttingDown = errors.New("funding manager shutting " +
		"down")

	// ErrConfirmationTimeout is an error returned when we as a responder
	// are waiting for a funding transaction to confirm, but too many
	// blocks pass without confirmation.
	ErrConfirmationTimeout = errors.New("timeout waiting for funding " +
		"confirmation")

	// errUpfrontShutdownScriptNotSupported is returned if an upfront
	// shutdown script is set for a peer that does not support the feature
	// bit.
	errUpfrontShutdownScriptNotSupported = errors.New("peer does not " +
		"support option upfront shutdown script")

	zeroID [32]byte
)

// reservationWithCtx encapsulates a pending channel reservation. This wrapper
// struct is used internally within the funding manager to track and progress
// the funding workflow initiated by incoming/outgoing methods from the target
// peer. Additionally, this struct houses a response and error channel which is
// used to respond to the caller in the case a channel workflow is initiated
// via a local signal such as RPC.
//
// TODO(roasbeef): actually use the context package
//   - deadlines, etc.
type reservationWithCtx struct {
	reservation *lnwallet.ChannelReservation
	peer        lnpeer.Peer

	chanAmt btcutil.Amount

	// forwardingPolicy is the policy provided by the initFundingMsg.
	forwardingPolicy htlcswitch.ForwardingPolicy

	// Constraints we require for the remote.
	remoteCsvDelay    uint16
	remoteMinHtlc     lnwire.MilliSatoshi
	remoteMaxValue    lnwire.MilliSatoshi
	remoteMaxHtlcs    uint16
	remoteChanReserve btcutil.Amount

	// maxLocalCsv is the maximum csv we will accept from the remote.
	maxLocalCsv uint16

	// channelType is the explicit channel type proposed by the initiator of
	// the channel.
	channelType *lnwire.ChannelType

	updateMtx   sync.RWMutex
	lastUpdated time.Time

	updates chan *lnrpc.OpenStatusUpdate
	err     chan error
}

// isLocked checks the reservation's timestamp to determine whether it is
// locked.
func (r *reservationWithCtx) isLocked() bool {
	r.updateMtx.RLock()
	defer r.updateMtx.RUnlock()

	// The time zero value represents a locked reservation.
	return r.lastUpdated.IsZero()
}

// updateTimestamp updates the reservation's timestamp with the current time.
func (r *reservationWithCtx) updateTimestamp() {
	r.updateMtx.Lock()
	defer r.updateMtx.Unlock()

	r.lastUpdated = time.Now()
}

// InitFundingMsg is sent by an outside subsystem to the funding manager in
// order to kick off a funding workflow with a specified target peer. The
// original request which defines the parameters of the funding workflow are
// embedded within this message giving the funding manager full context w.r.t
// the workflow.
type InitFundingMsg struct {
	// Peer is the peer that we want to open a channel to.
	Peer lnpeer.Peer

	// TargetPubkey is the public key of the peer.
	TargetPubkey *btcec.PublicKey

	// ChainHash is the target genesis hash for this channel.
	ChainHash chainhash.Hash

	// SubtractFees set to true means that fees will be subtracted
	// from the LocalFundingAmt.
	SubtractFees bool

	// LocalFundingAmt is the size of the channel.
	LocalFundingAmt btcutil.Amount

	// BaseFee is the base fee charged for routing payments regardless of
	// the number of milli-satoshis sent.
	BaseFee *uint64

	// FeeRate is the fee rate in ppm (parts per million) that will be
	// charged proportionally based on the value of each forwarded HTLC, the
	// lowest possible rate is 0 with a granularity of 0.000001
	// (millionths).
	FeeRate *uint64

	// PushAmt is the amount pushed to the counterparty.
	PushAmt lnwire.MilliSatoshi

	// FundingFeePerKw is the fee for the funding transaction.
	FundingFeePerKw chainfee.SatPerKWeight

	// Private determines whether or not this channel will be private.
	Private bool

	// MinHtlcIn is the minimum incoming HTLC that we accept.
	MinHtlcIn lnwire.MilliSatoshi

	// RemoteCsvDelay is the CSV delay we require for the remote peer.
	RemoteCsvDelay uint16

	// RemoteChanReserve is the channel reserve we required for the remote
	// peer.
	RemoteChanReserve btcutil.Amount

	// MinConfs indicates the minimum number of confirmations that each
	// output selected to fund the channel should satisfy.
	MinConfs int32

	// ShutdownScript is an optional upfront shutdown script for the
	// channel. This value is optional, so may be nil.
	ShutdownScript lnwire.DeliveryAddress

	// MaxValueInFlight is the maximum amount of coins in MilliSatoshi
	// that can be pending within the channel. It only applies to the
	// remote party.
	MaxValueInFlight lnwire.MilliSatoshi

	// MaxHtlcs is the maximum number of HTLCs that the remote peer
	// can offer us.
	MaxHtlcs uint16

	// MaxLocalCsv is the maximum local csv delay we will accept from our
	// peer.
	MaxLocalCsv uint16

	// FundUpToMaxAmt is the maximum amount to try to commit to. If set, the
	// MinFundAmt field denotes the acceptable minimum amount to commit to,
	// while trying to commit as many coins as possible up to this value.
	FundUpToMaxAmt btcutil.Amount

	// MinFundAmt must be set iff FundUpToMaxAmt is set. It denotes the
	// minimum amount to commit to.
	MinFundAmt btcutil.Amount

	// ChanFunder is an optional channel funder that allows the caller to
	// control exactly how the channel funding is carried out. If not
	// specified, then the default chanfunding.WalletAssembler will be
	// used.
	ChanFunder chanfunding.Assembler

	// PendingChanID is not all zeroes (the default value), then this will
	// be the pending channel ID used for the funding flow within the wire
	// protocol.
	PendingChanID [32]byte

	// ChannelType allows the caller to use an explicit channel type for the
	// funding negotiation. This type will only be observed if BOTH sides
	// support explicit channel type negotiation.
	ChannelType *lnwire.ChannelType

	// Updates is a channel which updates to the opening status of the
	// channel are sent on.
	Updates chan *lnrpc.OpenStatusUpdate

	// Err is a channel which errors encountered during the funding flow are
	// sent on.
	Err chan error
}

// fundingMsg is sent by the ProcessFundingMsg function and packages a
// funding-specific lnwire.Message along with the lnpeer.Peer that sent it.
type fundingMsg struct {
	msg  lnwire.Message
	peer lnpeer.Peer
}

// pendingChannels is a map instantiated per-peer which tracks all active
// pending single funded channels indexed by their pending channel identifier,
// which is a set of 32-bytes generated via a CSPRNG.
type pendingChannels map[[32]byte]*reservationWithCtx

// serializedPubKey is used within the FundingManager's activeReservations list
// to identify the nodes with which the FundingManager is actively working to
// initiate new channels.
type serializedPubKey [33]byte

// newSerializedKey creates a new serialized public key from an instance of a
// live pubkey object.
func newSerializedKey(pubKey *btcec.PublicKey) serializedPubKey {
	var s serializedPubKey
	copy(s[:], pubKey.SerializeCompressed())
	return s
}

// Config defines the configuration for the FundingManager. All elements
// within the configuration MUST be non-nil for the FundingManager to carry out
// its duties.
type Config struct {
	// NoWumboChans indicates if we're to reject all incoming wumbo channel
	// requests, and also reject all outgoing wumbo channel requests.
	NoWumboChans bool

	// IDKey is the PublicKey that is used to identify this node within the
	// Lightning Network.
	IDKey *btcec.PublicKey

	// IDKeyLoc is the locator for the key that is used to identify this
	// node within the LightningNetwork.
	IDKeyLoc keychain.KeyLocator

	// Wallet handles the parts of the funding process that involves moving
	// funds from on-chain transaction outputs into Lightning channels.
	Wallet *lnwallet.LightningWallet

	// PublishTransaction facilitates the process of broadcasting a
	// transaction to the network.
	PublishTransaction func(*wire.MsgTx, string) error

	// UpdateLabel updates the label that a transaction has in our wallet,
	// overwriting any existing labels.
	UpdateLabel func(chainhash.Hash, string) error

	// FeeEstimator calculates appropriate fee rates based on historical
	// transaction information.
	FeeEstimator chainfee.Estimator

	// Notifier is used by the FundingManager to determine when the
	// channel's funding transaction has been confirmed on the blockchain
	// so that the channel creation process can be completed.
	Notifier chainntnfs.ChainNotifier

	// SignMessage signs an arbitrary message with a given public key. The
	// actual digest signed is the double sha-256 of the message. In the
	// case that the private key corresponding to the passed public key
	// cannot be located, then an error is returned.
	//
	// TODO(roasbeef): should instead pass on this responsibility to a
	// distinct sub-system?
	SignMessage func(keyLoc keychain.KeyLocator,
		msg []byte, doubleHash bool) (*ecdsa.Signature, error)

	// CurrentNodeAnnouncement should return the latest, fully signed node
	// announcement from the backing Lightning Network node.
	CurrentNodeAnnouncement func() (lnwire.NodeAnnouncement, error)

	// SendAnnouncement is used by the FundingManager to send announcement
	// messages to the Gossiper to possibly broadcast to the greater
	// network. A set of optional message fields can be provided to populate
	// any information within the graph that is not included in the gossip
	// message.
	SendAnnouncement func(msg lnwire.Message,
		optionalFields ...discovery.OptionalMsgField) chan error

	// NotifyWhenOnline allows the FundingManager to register with a
	// subsystem that will notify it when the peer comes online. This is
	// used when sending the channelReady message, since it MUST be
	// delivered after the funding transaction is confirmed.
	//
	// NOTE: The peerChan channel must be buffered.
	NotifyWhenOnline func(peer [33]byte, peerChan chan<- lnpeer.Peer)

	// FindChannel queries the database for the channel with the given
	// channel ID. Providing the node's public key is an optimization that
	// prevents deserializing and scanning through all possible channels.
	FindChannel func(node *btcec.PublicKey,
		chanID lnwire.ChannelID) (*channeldb.OpenChannel, error)

	// TempChanIDSeed is a cryptographically random string of bytes that's
	// used as a seed to generate pending channel ID's.
	TempChanIDSeed [32]byte

	// DefaultRoutingPolicy is the default routing policy used when
	// initially announcing channels.
	DefaultRoutingPolicy htlcswitch.ForwardingPolicy

	// DefaultMinHtlcIn is the default minimum incoming htlc value that is
	// set as a channel parameter.
	DefaultMinHtlcIn lnwire.MilliSatoshi

	// NumRequiredConfs is a function closure that helps the funding
	// manager decide how many confirmations it should require for a
	// channel extended to it. The function is able to take into account
	// the amount of the channel, and any funds we'll be pushed in the
	// process to determine how many confirmations we'll require.
	NumRequiredConfs func(btcutil.Amount, lnwire.MilliSatoshi) uint16

	// RequiredRemoteDelay is a function that maps the total amount in a
	// proposed channel to the CSV delay that we'll require for the remote
	// party. Naturally a larger channel should require a higher CSV delay
	// in order to give us more time to claim funds in the case of a
	// contract breach.
	RequiredRemoteDelay func(btcutil.Amount) uint16

	// RequiredRemoteChanReserve is a function closure that, given the
	// channel capacity and dust limit, will return an appropriate amount
	// for the remote peer's required channel reserve that is to be adhered
	// to at all times.
	RequiredRemoteChanReserve func(capacity,
		dustLimit btcutil.Amount) btcutil.Amount

	// RequiredRemoteMaxValue is a function closure that, given the channel
	// capacity, returns the amount of MilliSatoshis that our remote peer
	// can have in total outstanding HTLCs with us.
	RequiredRemoteMaxValue func(btcutil.Amount) lnwire.MilliSatoshi

	// RequiredRemoteMaxHTLCs is a function closure that, given the channel
	// capacity, returns the number of maximum HTLCs the remote peer can
	// offer us.
	RequiredRemoteMaxHTLCs func(btcutil.Amount) uint16

	// WatchNewChannel is to be called once a new channel enters the final
	// funding stage: waiting for on-chain confirmation. This method sends
	// the channel to the ChainArbitrator so it can watch for any on-chain
	// events related to the channel. We also provide the public key of the
	// node we're establishing a channel with for reconnection purposes.
	WatchNewChannel func(*channeldb.OpenChannel, *btcec.PublicKey) error

	// ReportShortChanID allows the funding manager to report the confirmed
	// short channel ID of a formerly pending zero-conf channel to outside
	// sub-systems.
	ReportShortChanID func(wire.OutPoint) error

	// ZombieSweeperInterval is the periodic time interval in which the
	// zombie sweeper is run.
	ZombieSweeperInterval time.Duration

	// ReservationTimeout is the length of idle time that must pass before
	// a reservation is considered a zombie.
	ReservationTimeout time.Duration

	// MinChanSize is the smallest channel size that we'll accept as an
	// inbound channel. We have such a parameter, as otherwise, nodes could
	// flood us with very small channels that would never really be usable
	// due to fees.
	MinChanSize btcutil.Amount

	// MaxChanSize is the largest channel size that we'll accept as an
	// inbound channel. We have such a parameter, so that you may decide how
	// WUMBO you would like your channel.
	MaxChanSize btcutil.Amount

	// MaxPendingChannels is the maximum number of pending channels we
	// allow for each peer.
	MaxPendingChannels int

	// RejectPush is set true if the fundingmanager should reject any
	// incoming channels having a non-zero push amount.
	RejectPush bool

	// MaxLocalCSVDelay is the maximum csv delay we will allow for our
	// commit output. Channels that exceed this value will be failed.
	MaxLocalCSVDelay uint16

	// NotifyOpenChannelEvent informs the ChannelNotifier when channels
	// transition from pending open to open.
	NotifyOpenChannelEvent func(wire.OutPoint)

	// UpdateForwardingPolicies is used by the manager to update active
	// links with a new policy.
	UpdateForwardingPolicies func(
		chanPolicies map[wire.OutPoint]htlcswitch.ForwardingPolicy)

	// OpenChannelPredicate is a predicate on the lnwire.OpenChannel message
	// and on the requesting node's public key that returns a bool which
	// tells the funding manager whether or not to accept the channel.
	OpenChannelPredicate chanacceptor.ChannelAcceptor

	// NotifyPendingOpenChannelEvent informs the ChannelNotifier when
	// channels enter a pending state.
	NotifyPendingOpenChannelEvent func(wire.OutPoint,
		*channeldb.OpenChannel)

	// EnableUpfrontShutdown specifies whether the upfront shutdown script
	// is enabled.
	EnableUpfrontShutdown bool

	// RegisteredChains keeps track of all chains that have been registered
	// with the daemon.
	RegisteredChains *chainreg.ChainRegistry

	// MaxAnchorsCommitFeeRate is the max commitment fee rate we'll use as
	// the initiator for channels of the anchor type.
	MaxAnchorsCommitFeeRate chainfee.SatPerKWeight

	// DeleteAliasEdge allows the Manager to delete an alias channel edge
	// from the graph. It also returns our local to-be-deleted policy.
	DeleteAliasEdge func(scid lnwire.ShortChannelID) (
		*channeldb.ChannelEdgePolicy, error)

	// AliasManager is an implementation of the aliasHandler interface that
	// abstracts away the handling of many alias functions.
	AliasManager aliasHandler
}

// Manager acts as an orchestrator/bridge between the wallet's
// 'ChannelReservation' workflow, and the wire protocol's funding initiation
// messages. Any requests to initiate the funding workflow for a channel,
// either kicked-off locally or remotely are handled by the funding manager.
// Once a channel's funding workflow has been completed, any local callers, the
// local peer, and possibly the remote peer are notified of the completion of
// the channel workflow. Additionally, any temporary or permanent access
// controls between the wallet and remote peers are enforced via the funding
// manager.
type Manager struct {
	started sync.Once
	stopped sync.Once

	// cfg is a copy of the configuration struct that the FundingManager
	// was initialized with.
	cfg *Config

	// chanIDKey is a cryptographically random key that's used to generate
	// temporary channel ID's.
	chanIDKey [32]byte

	// chanIDNonce is a nonce that's incremented for each new funding
	// reservation created.
	nonceMtx    sync.RWMutex
	chanIDNonce uint64

	// activeReservations is a map which houses the state of all pending
	// funding workflows.
	activeReservations map[serializedPubKey]pendingChannels

	// signedReservations is a utility map that maps the permanent channel
	// ID of a funding reservation to its temporary channel ID. This is
	// required as mid funding flow, we switch to referencing the channel
	// by its full channel ID once the commitment transactions have been
	// signed by both parties.
	signedReservations map[lnwire.ChannelID][32]byte

	// resMtx guards both of the maps above to ensure that all access is
	// goroutine safe.
	resMtx sync.RWMutex

	// fundingMsgs is a channel that relays fundingMsg structs from
	// external sub-systems using the ProcessFundingMsg call.
	fundingMsgs chan *fundingMsg

	// fundingRequests is a channel used to receive channel initiation
	// requests from a local subsystem within the daemon.
	fundingRequests chan *InitFundingMsg

	// newChanBarriers is a map from a channel ID to a 'barrier' which will
	// be signalled once the channel is fully open. This barrier acts as a
	// synchronization point for any incoming/outgoing HTLCs before the
	// channel has been fully opened.
	barrierMtx      sync.RWMutex
	newChanBarriers map[lnwire.ChannelID]chan struct{}

	localDiscoveryMtx     sync.Mutex
	localDiscoverySignals map[lnwire.ChannelID]chan struct{}

	handleChannelReadyMtx      sync.RWMutex
	handleChannelReadyBarriers map[lnwire.ChannelID]struct{}

	quit chan struct{}
	wg   sync.WaitGroup
}

// channelOpeningState represents the different states a channel can be in
// between the funding transaction has been confirmed and the channel is
// announced to the network and ready to be used.
type channelOpeningState uint8

const (
	// markedOpen is the opening state of a channel if the funding
	// transaction is confirmed on-chain, but channelReady is not yet
	// successfully sent to the other peer.
	markedOpen channelOpeningState = iota

	// channelReadySent is the opening state of a channel if the
	// channelReady message has successfully been sent to the other peer,
	// but we still haven't announced the channel to the network.
	channelReadySent

	// addedToRouterGraph is the opening state of a channel if the
	// channel has been successfully added to the router graph
	// immediately after the channelReady message has been sent, but
	// we still haven't announced the channel to the network.
	addedToRouterGraph
)

func (c channelOpeningState) String() string {
	switch c {
	case markedOpen:
		return "markedOpen"
	case channelReadySent:
		return "channelReady"
	case addedToRouterGraph:
		return "addedToRouterGraph"
	default:
		return "unknown"
	}
}

// NewFundingManager creates and initializes a new instance of the
// fundingManager.
func NewFundingManager(cfg Config) (*Manager, error) {
	return &Manager{
		cfg:       &cfg,
		chanIDKey: cfg.TempChanIDSeed,
		activeReservations: make(
			map[serializedPubKey]pendingChannels,
		),
		signedReservations: make(
			map[lnwire.ChannelID][32]byte,
		),
		newChanBarriers: make(
			map[lnwire.ChannelID]chan struct{},
		),
		fundingMsgs: make(
			chan *fundingMsg, msgBufferSize,
		),
		fundingRequests: make(
			chan *InitFundingMsg, msgBufferSize,
		),
		localDiscoverySignals: make(
			map[lnwire.ChannelID]chan struct{},
		),
		handleChannelReadyBarriers: make(
			map[lnwire.ChannelID]struct{},
		),
		quit: make(chan struct{}),
	}, nil
}

// Start launches all helper goroutines required for handling requests sent
// to the funding manager.
func (f *Manager) Start() error {
	var err error
	f.started.Do(func() {
		log.Info("Funding manager starting")
		err = f.start()
	})
	return err
}

func (f *Manager) start() error {
	// Upon restart, the Funding Manager will check the database to load any
	// channels that were  waiting for their funding transactions to be
	// confirmed on the blockchain at the time when the daemon last went
	// down.
	// TODO(roasbeef): store height that funding finished?
	//  * would then replace call below
	allChannels, err := f.cfg.Wallet.Cfg.Database.FetchAllChannels()
	if err != nil {
		return err
	}

	for _, channel := range allChannels {
		chanID := lnwire.NewChanIDFromOutPoint(&channel.FundingOutpoint)

		// For any channels that were in a pending state when the
		// daemon was last connected, the Funding Manager will
		// re-initialize the channel barriers, and republish the
		// funding transaction if we're the initiator.
		if channel.IsPending {
			f.barrierMtx.Lock()
			log.Tracef("Loading pending ChannelPoint(%v), "+
				"creating chan barrier",
				channel.FundingOutpoint)

			f.newChanBarriers[chanID] = make(chan struct{})
			f.barrierMtx.Unlock()

			f.localDiscoveryMtx.Lock()
			f.localDiscoverySignals[chanID] = make(chan struct{})
			f.localDiscoveryMtx.Unlock()

			// Rebroadcast the funding transaction for any pending
			// channel that we initiated. No error will be returned
			// if the transaction already has been broadcast.
			chanType := channel.ChanType
			if chanType.IsSingleFunder() &&
				chanType.HasFundingTx() &&
				channel.IsInitiator {

				f.rebroadcastFundingTx(channel)
			}
		} else if channel.ChanType.IsSingleFunder() &&
			channel.ChanType.HasFundingTx() &&
			channel.IsZeroConf() && channel.IsInitiator &&
			!channel.ZeroConfConfirmed() {

			// Rebroadcast the funding transaction for unconfirmed
			// zero-conf channels if we have the funding tx and are
			// also the initiator.
			f.rebroadcastFundingTx(channel)
		}

		// We will restart the funding state machine for all channels,
		// which will wait for the channel's funding transaction to be
		// confirmed on the blockchain, and transmit the messages
		// necessary for the channel to be operational.
		f.wg.Add(1)
		go f.advanceFundingState(channel, chanID, nil)
	}

	f.wg.Add(1) // TODO(roasbeef): tune
	go f.reservationCoordinator()

	return nil
}

// Stop signals all helper goroutines to execute a graceful shutdown. This
// method will block until all goroutines have exited.
func (f *Manager) Stop() error {
	f.stopped.Do(func() {
		log.Info("Funding manager shutting down")
		close(f.quit)
		f.wg.Wait()
	})

	return nil
}

// rebroadcastFundingTx publishes the funding tx on startup for each
// unconfirmed channel.
func (f *Manager) rebroadcastFundingTx(c *channeldb.OpenChannel) {
	var fundingTxBuf bytes.Buffer
	err := c.FundingTxn.Serialize(&fundingTxBuf)
	if err != nil {
		log.Errorf("Unable to serialize funding transaction %v: %v",
			c.FundingTxn.TxHash(), err)

		// Clear the buffer of any bytes that were written before the
		// serialization error to prevent logging an incomplete
		// transaction.
		fundingTxBuf.Reset()
	} else {
		log.Debugf("Rebroadcasting funding tx for ChannelPoint(%v): "+
			"%x", c.FundingOutpoint, fundingTxBuf.Bytes())
	}

	// Set a nil short channel ID at this stage because we do not know it
	// until our funding tx confirms.
	label := labels.MakeLabel(labels.LabelTypeChannelOpen, nil)

	err = f.cfg.PublishTransaction(c.FundingTxn, label)
	if err != nil {
		log.Errorf("Unable to rebroadcast funding tx %x for "+
			"ChannelPoint(%v): %v", fundingTxBuf.Bytes(),
			c.FundingOutpoint, err)
	}
}

// nextPendingChanID returns the next free pending channel ID to be used to
// identify a particular future channel funding workflow.
func (f *Manager) nextPendingChanID() [32]byte {
	// Obtain a fresh nonce. We do this by encoding the current nonce
	// counter, then incrementing it by one.
	f.nonceMtx.Lock()
	var nonce [8]byte
	binary.LittleEndian.PutUint64(nonce[:], f.chanIDNonce)
	f.chanIDNonce++
	f.nonceMtx.Unlock()

	// We'll generate the next pending channelID by "encrypting" 32-bytes
	// of zeroes which'll extract 32 random bytes from our stream cipher.
	var (
		nextChanID [32]byte
		zeroes     [32]byte
	)
	salsa20.XORKeyStream(nextChanID[:], zeroes[:], nonce[:], &f.chanIDKey)

	return nextChanID
}

// CancelPeerReservations cancels all active reservations associated with the
// passed node. This will ensure any outputs which have been pre committed,
// (and thus locked from coin selection), are properly freed.
func (f *Manager) CancelPeerReservations(nodePub [33]byte) {
	log.Debugf("Cancelling all reservations for peer %x", nodePub[:])

	f.resMtx.Lock()
	defer f.resMtx.Unlock()

	// We'll attempt to look up this node in the set of active
	// reservations.  If they don't have any, then there's no further work
	// to be done.
	nodeReservations, ok := f.activeReservations[nodePub]
	if !ok {
		log.Debugf("No active reservations for node: %x", nodePub[:])
		return
	}

	// If they do have any active reservations, then we'll cancel all of
	// them (which releases any locked UTXO's), and also delete it from the
	// reservation map.
	for pendingID, resCtx := range nodeReservations {
		if err := resCtx.reservation.Cancel(); err != nil {
			log.Errorf("unable to cancel reservation for "+
				"node=%x: %v", nodePub[:], err)
		}

		resCtx.err <- fmt.Errorf("peer disconnected")
		delete(nodeReservations, pendingID)
	}

	// Finally, we'll delete the node itself from the set of reservations.
	delete(f.activeReservations, nodePub)
}

// failFundingFlow will fail the active funding flow with the target peer,
// identified by its unique temporary channel ID. This method will send an
// error to the remote peer, and also remove the reservation from our set of
// pending reservations.
//
// TODO(roasbeef): if peer disconnects, and haven't yet broadcast funding
// transaction, then all reservations should be cleared.
func (f *Manager) failFundingFlow(peer lnpeer.Peer, tempChanID [32]byte,
	fundingErr error) {

	log.Debugf("Failing funding flow for pending_id=%x: %v",
		tempChanID, fundingErr)

	ctx, err := f.cancelReservationCtx(
		peer.IdentityKey(), tempChanID, false,
	)
	if err != nil {
		log.Errorf("unable to cancel reservation: %v", err)
	}

	// In case the case where the reservation existed, send the funding
	// error on the error channel.
	if ctx != nil {
		ctx.err <- fundingErr
	}

	// We only send the exact error if it is part of out whitelisted set of
	// errors (lnwire.FundingError or lnwallet.ReservationError).
	var msg lnwire.ErrorData
	switch e := fundingErr.(type) {
	// Let the actual error message be sent to the remote for the
	// whitelisted types.
	case lnwallet.ReservationError:
		msg = lnwire.ErrorData(e.Error())
	case lnwire.FundingError:
		msg = lnwire.ErrorData(e.Error())
	case chanacceptor.ChanAcceptError:
		msg = lnwire.ErrorData(e.Error())

	// For all other error types we just send a generic error.
	default:
		msg = lnwire.ErrorData("funding failed due to internal error")
	}

	errMsg := &lnwire.Error{
		ChanID: tempChanID,
		Data:   msg,
	}

	log.Debugf("Sending funding error to peer (%x): %v",
		peer.IdentityKey().SerializeCompressed(), spew.Sdump(errMsg))
	if err := peer.SendMessage(false, errMsg); err != nil {
		log.Errorf("unable to send error message to peer %v", err)
	}
}

// reservationCoordinator is the primary goroutine tasked with progressing the
// funding workflow between the wallet, and any outside peers or local callers.
//
// NOTE: This MUST be run as a goroutine.
func (f *Manager) reservationCoordinator() {
	defer f.wg.Done()

	zombieSweepTicker := time.NewTicker(f.cfg.ZombieSweeperInterval)
	defer zombieSweepTicker.Stop()

	for {
		select {
		case fmsg := <-f.fundingMsgs:
			switch msg := fmsg.msg.(type) {
			case *lnwire.OpenChannel:
				f.handleFundingOpen(fmsg.peer, msg)

			case *lnwire.AcceptChannel:
				f.handleFundingAccept(fmsg.peer, msg)

			case *lnwire.FundingCreated:
				f.handleFundingCreated(fmsg.peer, msg)

			case *lnwire.FundingSigned:
				f.handleFundingSigned(fmsg.peer, msg)

			case *lnwire.ChannelReady:
				f.wg.Add(1)
				go f.handleChannelReady(fmsg.peer, msg)

			case *lnwire.Warning:
				f.handleWarningMsg(fmsg.peer, msg)

			case *lnwire.Error:
				f.handleErrorMsg(fmsg.peer, msg)
			}
		case req := <-f.fundingRequests:
			f.handleInitFundingMsg(req)

		case <-zombieSweepTicker.C:
			f.pruneZombieReservations()

		case <-f.quit:
			return
		}
	}
}

// advanceFundingState will advance the channel through the steps after the
// funding transaction is broadcasted, up until the point where the channel is
// ready for operation. This includes waiting for the funding transaction to
// confirm, sending funding locked to the peer, adding the channel to the
// router graph, and announcing the channel. The updateChan can be set non-nil
// to get OpenStatusUpdates.
//
// NOTE: This MUST be run as a goroutine.
func (f *Manager) advanceFundingState(channel *channeldb.OpenChannel,
	pendingChanID [32]byte, updateChan chan<- *lnrpc.OpenStatusUpdate) {

	defer f.wg.Done()

	// If the channel is still pending we must wait for the funding
	// transaction to confirm.
	if channel.IsPending {
		err := f.advancePendingChannelState(channel, pendingChanID)
		if err != nil {
			log.Errorf("Unable to advance pending state of "+
				"ChannelPoint(%v): %v",
				channel.FundingOutpoint, err)
			return
		}
	}

	// We create the state-machine object which wraps the database state.
	lnChannel, err := lnwallet.NewLightningChannel(
		nil, channel, nil,
	)
	if err != nil {
		log.Errorf("Unable to create LightningChannel(%v): %v",
			channel.FundingOutpoint, err)
		return
	}

	for {
		channelState, shortChanID, err := f.getChannelOpeningState(
			&channel.FundingOutpoint,
		)
		if err == channeldb.ErrChannelNotFound {
			// Channel not in fundingManager's opening database,
			// meaning it was successfully announced to the
			// network.
			// TODO(halseth): could do graph consistency check
			// here, and re-add the edge if missing.
			log.Debugf("ChannelPoint(%v) with chan_id=%x not "+
				"found in opening database, assuming already "+
				"announced to the network",
				channel.FundingOutpoint, pendingChanID)
			return
		} else if err != nil {
			log.Errorf("Unable to query database for "+
				"channel opening state(%v): %v",
				channel.FundingOutpoint, err)
			return
		}

		// If we did find the channel in the opening state database, we
		// have seen the funding transaction being confirmed, but there
		// are still steps left of the setup procedure. We continue the
		// procedure where we left off.
		err = f.stateStep(
			channel, lnChannel, shortChanID, pendingChanID,
			channelState, updateChan,
		)
		if err != nil {
			log.Errorf("Unable to advance state(%v): %v",
				channel.FundingOutpoint, err)
			return
		}
	}
}

// stateStep advances the confirmed channel one step in the funding state
// machine. This method is synchronous and the new channel opening state will
// have been written to the database when it successfully returns. The
// updateChan can be set non-nil to get OpenStatusUpdates.
func (f *Manager) stateStep(channel *channeldb.OpenChannel,
	lnChannel *lnwallet.LightningChannel,
	shortChanID *lnwire.ShortChannelID, pendingChanID [32]byte,
	channelState channelOpeningState,
	updateChan chan<- *lnrpc.OpenStatusUpdate) error {

	chanID := lnwire.NewChanIDFromOutPoint(&channel.FundingOutpoint)
	log.Debugf("Channel(%v) with ShortChanID %v has opening state %v",
		chanID, shortChanID, channelState)

	switch channelState {
	// The funding transaction was confirmed, but we did not successfully
	// send the channelReady message to the peer, so let's do that now.
	case markedOpen:
		err := f.sendChannelReady(channel, lnChannel)
		if err != nil {
			return fmt.Errorf("failed sending channelReady: %v",
				err)
		}

		// As the channelReady message is now sent to the peer, the
		// channel is moved to the next state of the state machine. It
		// will be moved to the last state (actually deleted from the
		// database) after the channel is finally announced.
		err = f.saveChannelOpeningState(
			&channel.FundingOutpoint, channelReadySent,
			shortChanID,
		)
		if err != nil {
			return fmt.Errorf("error setting channel state to"+
				" channelReadySent: %v", err)
		}

		log.Debugf("Channel(%v) with ShortChanID %v: successfully "+
			"sent ChannelReady", chanID, shortChanID)

		return nil

	// channelReady was sent to peer, but the channel was not added to the
	// router graph and the channel announcement was not sent.
	case channelReadySent:
		// We must wait until we've received the peer's funding locked
		// before sending a channel_update according to BOLT#07.
		received, err := f.receivedChannelReady(
			channel.IdentityPub, chanID,
		)
		if err != nil {
			return fmt.Errorf("failed to check if funding locked "+
				"was received: %v", err)
		}

		if !received {
			// We haven't received ChannelReady, so we'll continue
			// to the next iteration of the loop after sleeping for
			// checkPeerChannelReadyInterval.
			select {
			case <-time.After(checkPeerChannelReadyInterval):
			case <-f.quit:
				return ErrFundingManagerShuttingDown
			}

			return nil
		}

		var peerAlias *lnwire.ShortChannelID
		if channel.IsZeroConf() {
			// We'll need to wait until channel_ready has been
			// received and the peer lets us know the alias they
			// want to use for the channel. With this information,
			// we can then construct a ChannelUpdate for them.
			// If an alias does not yet exist, we'll just return,
			// letting the next iteration of the loop check again.
			var defaultAlias lnwire.ShortChannelID
			chanID := lnwire.NewChanIDFromOutPoint(
				&channel.FundingOutpoint,
			)
			foundAlias, _ := f.cfg.AliasManager.GetPeerAlias(
				chanID,
			)
			if foundAlias == defaultAlias {
				return nil
			}

			peerAlias = &foundAlias
		}

		err = f.addToRouterGraph(channel, shortChanID, peerAlias, nil)
		if err != nil {
			return fmt.Errorf("failed adding to "+
				"router graph: %v", err)
		}

		// As the channel is now added to the ChannelRouter's topology,
		// the channel is moved to the next state of the state machine.
		// It will be moved to the last state (actually deleted from
		// the database) after the channel is finally announced.
		err = f.saveChannelOpeningState(
			&channel.FundingOutpoint, addedToRouterGraph,
			shortChanID,
		)
		if err != nil {
			return fmt.Errorf("error setting channel state to"+
				" addedToRouterGraph: %v", err)
		}

		log.Debugf("Channel(%v) with ShortChanID %v: successfully "+
			"added to router graph", chanID, shortChanID)

		// Give the caller a final update notifying them that
		// the channel is now open.
		// TODO(roasbeef): only notify after recv of funding locked?
		fundingPoint := channel.FundingOutpoint
		cp := &lnrpc.ChannelPoint{
			FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
				FundingTxidBytes: fundingPoint.Hash[:],
			},
			OutputIndex: fundingPoint.Index,
		}

		if updateChan != nil {
			upd := &lnrpc.OpenStatusUpdate{
				Update: &lnrpc.OpenStatusUpdate_ChanOpen{
					ChanOpen: &lnrpc.ChannelOpenUpdate{
						ChannelPoint: cp,
					},
				},
				PendingChanId: pendingChanID[:],
			}

			select {
			case updateChan <- upd:
			case <-f.quit:
				return ErrFundingManagerShuttingDown
			}
		}

		return nil

	// The channel was added to the Router's topology, but the channel
	// announcement was not sent.
	case addedToRouterGraph:
		if channel.IsZeroConf() {
			// If this is a zero-conf channel, then we will wait
			// for it to be confirmed before announcing it to the
			// greater network.
			err := f.waitForZeroConfChannel(channel, pendingChanID)
			if err != nil {
				return fmt.Errorf("failed waiting for zero "+
					"channel: %v", err)
			}

			// Update the local shortChanID variable such that
			// annAfterSixConfs uses the confirmed SCID.
			confirmedScid := channel.ZeroConfRealScid()
			shortChanID = &confirmedScid
		}

		err := f.annAfterSixConfs(channel, shortChanID)
		if err != nil {
			return fmt.Errorf("error sending channel "+
				"announcement: %v", err)
		}

		// We delete the channel opening state from our internal
		// database as the opening process has succeeded. We can do
		// this because we assume the AuthenticatedGossiper queues the
		// announcement messages, and persists them in case of a daemon
		// shutdown.
		err = f.deleteChannelOpeningState(&channel.FundingOutpoint)
		if err != nil {
			return fmt.Errorf("error deleting channel state: %v",
				err)
		}

		log.Debugf("Channel(%v) with ShortChanID %v: successfully "+
			"announced", chanID, shortChanID)

		return nil
	}

	return fmt.Errorf("undefined channelState: %v", channelState)
}

// advancePendingChannelState waits for a pending channel's funding tx to
// confirm, and marks it open in the database when that happens.
func (f *Manager) advancePendingChannelState(
	channel *channeldb.OpenChannel, pendingChanID [32]byte) error {

	if channel.IsZeroConf() {
		// Persist the alias to the alias database.
		baseScid := channel.ShortChannelID
		err := f.cfg.AliasManager.AddLocalAlias(
			baseScid, baseScid, true,
		)
		if err != nil {
			return fmt.Errorf("error adding local alias to "+
				"store: %v", err)
		}

		// We don't wait for zero-conf channels to be confirmed and
		// instead immediately proceed with the rest of the funding
		// flow. The channel opening state is stored under the alias
		// SCID.
		err = f.saveChannelOpeningState(
			&channel.FundingOutpoint, markedOpen,
			&channel.ShortChannelID,
		)
		if err != nil {
			return fmt.Errorf("error setting zero-conf channel "+
				"state to markedOpen: %v", err)
		}

		// The ShortChannelID is already set since it's an alias, but
		// we still need to mark the channel as no longer pending.
		err = channel.MarkAsOpen(channel.ShortChannelID)
		if err != nil {
			return fmt.Errorf("error setting zero-conf channel's "+
				"pending flag to false: %v", err)
		}

		// Inform the ChannelNotifier that the channel has transitioned
		// from pending open to open.
		f.cfg.NotifyOpenChannelEvent(channel.FundingOutpoint)

		// Find and close the discoverySignal for this channel such
		// that ChannelReady messages will be processed.
		chanID := lnwire.NewChanIDFromOutPoint(
			&channel.FundingOutpoint,
		)
		f.localDiscoveryMtx.Lock()
		defer f.localDiscoveryMtx.Unlock()
		if discoverySignal, ok := f.localDiscoverySignals[chanID]; ok {
			close(discoverySignal)
		}

		return nil
	}

	confChannel, err := f.waitForFundingWithTimeout(channel)
	if err == ErrConfirmationTimeout {
		return f.fundingTimeout(channel, pendingChanID)
	} else if err != nil {
		return fmt.Errorf("error waiting for funding "+
			"confirmation for ChannelPoint(%v): %v",
			channel.FundingOutpoint, err)
	}

	// Success, funding transaction was confirmed.
	chanID := lnwire.NewChanIDFromOutPoint(&channel.FundingOutpoint)
	log.Debugf("ChannelID(%v) is now fully confirmed! "+
		"(shortChanID=%v)", chanID, confChannel.shortChanID)

	err = f.handleFundingConfirmation(channel, confChannel)
	if err != nil {
		return fmt.Errorf("unable to handle funding "+
			"confirmation for ChannelPoint(%v): %v",
			channel.FundingOutpoint, err)
	}

	return nil
}

// ProcessFundingMsg sends a message to the internal fundingManager goroutine,
// allowing it to handle the lnwire.Message.
func (f *Manager) ProcessFundingMsg(msg lnwire.Message, peer lnpeer.Peer) {
	select {
	case f.fundingMsgs <- &fundingMsg{msg, peer}:
	case <-f.quit:
		return
	}
}

// handleFundingOpen creates an initial 'ChannelReservation' within the wallet,
// then responds to the source peer with an accept channel message progressing
// the funding workflow.
//
// TODO(roasbeef): add error chan to all, let channelManager handle
// error+propagate.
func (f *Manager) handleFundingOpen(peer lnpeer.Peer,
	msg *lnwire.OpenChannel) {

	// Check number of pending channels to be smaller than maximum allowed
	// number and send ErrorGeneric to remote peer if condition is
	// violated.
	peerPubKey := peer.IdentityKey()
	peerIDKey := newSerializedKey(peerPubKey)

	amt := msg.FundingAmount

	// We get all pending channels for this peer. This is the list of the
	// active reservations and the channels pending open in the database.
	f.resMtx.RLock()
	reservations := f.activeReservations[peerIDKey]

	// We don't count reservations that were created from a canned funding
	// shim. The user has registered the shim and therefore expects this
	// channel to arrive.
	numPending := 0
	for _, res := range reservations {
		if !res.reservation.IsCannedShim() {
			numPending++
		}
	}
	f.resMtx.RUnlock()

	// Also count the channels that are already pending. There we don't know
	// the underlying intent anymore, unfortunately.
	channels, err := f.cfg.Wallet.Cfg.Database.FetchOpenChannels(peerPubKey)
	if err != nil {
		f.failFundingFlow(
			peer, msg.PendingChannelID, err,
		)
		return
	}

	for _, c := range channels {
		// Pending channels that have a non-zero thaw height were also
		// created through a canned funding shim. Those also don't
		// count towards the DoS protection limit.
		//
		// TODO(guggero): Properly store the funding type (wallet, shim,
		// PSBT) on the channel so we don't need to use the thaw height.
		if c.IsPending && c.ThawHeight == 0 {
			numPending++
		}
	}

	// TODO(roasbeef): modify to only accept a _single_ pending channel per
	// block unless white listed
	if numPending >= f.cfg.MaxPendingChannels {
		f.failFundingFlow(
			peer, msg.PendingChannelID,
			lnwire.ErrMaxPendingChannels,
		)
		return
	}

	// Ensure that the pendingChansLimit is respected.
	pendingChans, err := f.cfg.Wallet.Cfg.Database.FetchPendingChannels()
	if err != nil {
		f.failFundingFlow(
			peer, msg.PendingChannelID, err,
		)

		return
	}

	if len(pendingChans) > pendingChansLimit {
		f.failFundingFlow(
			peer, msg.PendingChannelID,
			lnwire.ErrMaxPendingChannels,
		)

		return
	}

	// We'll also reject any requests to create channels until we're fully
	// synced to the network as we won't be able to properly validate the
	// confirmation of the funding transaction.
	isSynced, _, err := f.cfg.Wallet.IsSynced()
	if err != nil || !isSynced {
		if err != nil {
			log.Errorf("unable to query wallet: %v", err)
		}
		err := errors.New("Synchronizing blockchain")
		f.failFundingFlow(
			peer, msg.PendingChannelID,
			err,
		)
		return
	}

	// Ensure that the remote party respects our maximum channel size.
	if amt > f.cfg.MaxChanSize {
		f.failFundingFlow(
			peer, msg.PendingChannelID,
			lnwallet.ErrChanTooLarge(amt, f.cfg.MaxChanSize),
		)
		return
	}

	// We'll, also ensure that the remote party isn't attempting to propose
	// a channel that's below our current min channel size.
	if amt < f.cfg.MinChanSize {
		f.failFundingFlow(
			peer, msg.PendingChannelID,
			lnwallet.ErrChanTooSmall(amt, f.cfg.MinChanSize),
		)
		return
	}

	// If request specifies non-zero push amount and 'rejectpush' is set,
	// signal an error.
	if f.cfg.RejectPush && msg.PushAmount > 0 {
		f.failFundingFlow(
			peer, msg.PendingChannelID,
			lnwallet.ErrNonZeroPushAmount(),
		)
		return
	}

	// Send the OpenChannel request to the ChannelAcceptor to determine
	// whether this node will accept the channel.
	chanReq := &chanacceptor.ChannelAcceptRequest{
		Node:        peer.IdentityKey(),
		OpenChanMsg: msg,
	}

	// Query our channel acceptor to determine whether we should reject
	// the channel.
	acceptorResp := f.cfg.OpenChannelPredicate.Accept(chanReq)
	if acceptorResp.RejectChannel() {
		f.failFundingFlow(
			peer, msg.PendingChannelID,
			acceptorResp.ChanAcceptError,
		)
		return
	}

	log.Infof("Recv'd fundingRequest(amt=%v, push=%v, delay=%v, "+
		"pendingId=%x) from peer(%x)", amt, msg.PushAmount,
		msg.CsvDelay, msg.PendingChannelID,
		peer.IdentityKey().SerializeCompressed())

	// Attempt to initialize a reservation within the wallet. If the wallet
	// has insufficient resources to create the channel, then the
	// reservation attempt may be rejected. Note that since we're on the
	// responding side of a single funder workflow, we don't commit any
	// funds to the channel ourselves.
	//
	// Before we init the channel, we'll also check to see what commitment
	// format we can use with this peer. This is dependent on *both* us and
	// the remote peer are signaling the proper feature bit if we're using
	// implicit negotiation, and simply the channel type sent over if we're
	// using explicit negotiation.
	wasExplicit, _, commitType, err := negotiateCommitmentType(
		msg.ChannelType, peer.LocalFeatures(), peer.RemoteFeatures(),
		false,
	)
	if err != nil {
		// TODO(roasbeef): should be using soft errors
		log.Errorf("channel type negotiation failed: %v", err)
		f.failFundingFlow(peer, msg.PendingChannelID, err)
		return
	}

	var scidFeatureVal bool
	if hasFeatures(
		peer.LocalFeatures(), peer.RemoteFeatures(),
		lnwire.ScidAliasOptional,
	) {

		scidFeatureVal = true
	}

	var (
		chanTypeFeatureBits *lnwire.ChannelType
		zeroConf            bool
		scid                bool
	)

	if wasExplicit {
		// Only echo back a channel type in AcceptChannel if we actually
		// used explicit negotiation above.
		chanTypeFeatureBits = msg.ChannelType

		// Check if the channel type includes the zero-conf or
		// scid-alias bits.
		featureVec := lnwire.RawFeatureVector(*chanTypeFeatureBits)
		zeroConf = featureVec.IsSet(lnwire.ZeroConfRequired)
		scid = featureVec.IsSet(lnwire.ScidAliasRequired)

		// If the zero-conf channel type was negotiated, ensure that
		// the acceptor allows it.
		if zeroConf && !acceptorResp.ZeroConf {
			// Fail the funding flow.
			flowErr := fmt.Errorf("channel acceptor blocked " +
				"zero-conf channel negotiation")
			f.failFundingFlow(
				peer, msg.PendingChannelID, flowErr,
			)
			return
		}

		// If the zero-conf channel type wasn't negotiated and the
		// fundee still wants a zero-conf channel, perform more checks.
		// Require that both sides have the scid-alias feature bit set.
		// We don't require anchors here - this is for compatibility
		// with LDK.
		if !zeroConf && acceptorResp.ZeroConf {
			if !scidFeatureVal {
				// Fail the funding flow.
				flowErr := fmt.Errorf("scid-alias feature " +
					"must be negotiated for zero-conf")
				f.failFundingFlow(
					peer, msg.PendingChannelID, flowErr,
				)
				return
			}

			// Set zeroConf to true to enable the zero-conf flow.
			zeroConf = true
		}
	}

	// Sending the option-scid-alias channel type for a public channel is
	// disallowed.
	public := msg.ChannelFlags&lnwire.FFAnnounceChannel != 0
	if public && scid {
		err = fmt.Errorf("option-scid-alias chantype for public " +
			"channel")
		log.Error(err)
		f.failFundingFlow(peer, msg.PendingChannelID, err)
		return
	}

	req := &lnwallet.InitFundingReserveMsg{
		ChainHash:        &msg.ChainHash,
		PendingChanID:    msg.PendingChannelID,
		NodeID:           peer.IdentityKey(),
		NodeAddr:         peer.Address(),
		LocalFundingAmt:  0,
		RemoteFundingAmt: amt,
		CommitFeePerKw:   chainfee.SatPerKWeight(msg.FeePerKiloWeight),
		FundingFeePerKw:  0,
		PushMSat:         msg.PushAmount,
		Flags:            msg.ChannelFlags,
		MinConfs:         1,
		CommitType:       commitType,
		ZeroConf:         zeroConf,
		OptionScidAlias:  scid,
		ScidAliasFeature: scidFeatureVal,
	}

	reservation, err := f.cfg.Wallet.InitChannelReservation(req)
	if err != nil {
		log.Errorf("Unable to initialize reservation: %v", err)
		f.failFundingFlow(peer, msg.PendingChannelID, err)
		return
	}

	log.Debugf("Initialized channel reservation: zeroConf=%v, psbt=%v, "+
		"cannedShim=%v", reservation.IsZeroConf(),
		reservation.IsPsbt(), reservation.IsCannedShim())

	if zeroConf {
		// Store an alias for zero-conf channels. Other option-scid
		// channels will do this at a later point.
		aliasScid, err := f.cfg.AliasManager.RequestAlias()
		if err != nil {
			log.Errorf("Unable to request alias: %v", err)
			f.failFundingFlow(peer, msg.PendingChannelID, err)
			return
		}

		reservation.AddAlias(aliasScid)
	}

	// As we're the responder, we get to specify the number of confirmations
	// that we require before both of us consider the channel open. We'll
	// use our mapping to derive the proper number of confirmations based on
	// the amount of the channel, and also if any funds are being pushed to
	// us. If a depth value was set by our channel acceptor, we will use
	// that value instead.
	numConfsReq := f.cfg.NumRequiredConfs(msg.FundingAmount, msg.PushAmount)
	if acceptorResp.MinAcceptDepth != 0 {
		numConfsReq = acceptorResp.MinAcceptDepth
	}

	// We'll ignore the min_depth calculated above if this is a zero-conf
	// channel.
	if zeroConf {
		numConfsReq = 0
	}

	reservation.SetNumConfsRequired(numConfsReq)

	// We'll also validate and apply all the constraints the initiating
	// party is attempting to dictate for our commitment transaction.
	channelConstraints := &channeldb.ChannelConstraints{
		DustLimit:        msg.DustLimit,
		ChanReserve:      msg.ChannelReserve,
		MaxPendingAmount: msg.MaxValueInFlight,
		MinHTLC:          msg.HtlcMinimum,
		MaxAcceptedHtlcs: msg.MaxAcceptedHTLCs,
		CsvDelay:         msg.CsvDelay,
	}
	err = reservation.CommitConstraints(
		channelConstraints, f.cfg.MaxLocalCSVDelay, true,
	)
	if err != nil {
		log.Errorf("Unacceptable channel constraints: %v", err)
		f.failFundingFlow(peer, msg.PendingChannelID, err)
		return
	}

	// Check whether the peer supports upfront shutdown, and get a new
	// wallet address if our node is configured to set shutdown addresses by
	// default. We use the upfront shutdown script provided by our channel
	// acceptor (if any) in lieu of user input.
	shutdown, err := getUpfrontShutdownScript(
		f.cfg.EnableUpfrontShutdown, peer, acceptorResp.UpfrontShutdown,
		f.selectShutdownScript,
	)
	if err != nil {
		f.failFundingFlow(
			peer, msg.PendingChannelID,
			fmt.Errorf("getUpfrontShutdownScript error: %v", err),
		)
		return
	}
	reservation.SetOurUpfrontShutdown(shutdown)

	// If a script enforced channel lease is being proposed, we'll need to
	// validate its custom TLV records.
	if commitType == lnwallet.CommitmentTypeScriptEnforcedLease {
		if msg.LeaseExpiry == nil {
			err := errors.New("missing lease expiry")
			f.failFundingFlow(peer, msg.PendingChannelID, err)
			return
		}

		// If we had a shim registered for this channel prior to
		// receiving its corresponding OpenChannel message, then we'll
		// validate the proposed LeaseExpiry against what was registered
		// in our shim.
		if reservation.LeaseExpiry() != 0 {
			if uint32(*msg.LeaseExpiry) !=
				reservation.LeaseExpiry() {

				err := errors.New("lease expiry mismatch")
				f.failFundingFlow(
					peer, msg.PendingChannelID, err,
				)
				return
			}
		}
	}

	log.Infof("Requiring %v confirmations for pendingChan(%x): "+
		"amt=%v, push_amt=%v, committype=%v, upfrontShutdown=%x",
		numConfsReq, msg.PendingChannelID, amt, msg.PushAmount,
		commitType, msg.UpfrontShutdownScript)

	// Generate our required constraints for the remote party, using the
	// values provided by the channel acceptor if they are non-zero.
	remoteCsvDelay := f.cfg.RequiredRemoteDelay(amt)
	if acceptorResp.CSVDelay != 0 {
		remoteCsvDelay = acceptorResp.CSVDelay
	}

	// If our default dust limit was above their ChannelReserve, we change
	// it to the ChannelReserve. We must make sure the ChannelReserve we
	// send in the AcceptChannel message is above both dust limits.
	// Therefore, take the maximum of msg.DustLimit and our dust limit.
	//
	// NOTE: Even with this bounding, the ChannelAcceptor may return an
	// BOLT#02-invalid ChannelReserve.
	maxDustLimit := reservation.OurContribution().DustLimit
	if msg.DustLimit > maxDustLimit {
		maxDustLimit = msg.DustLimit
	}

	chanReserve := f.cfg.RequiredRemoteChanReserve(amt, maxDustLimit)
	if acceptorResp.Reserve != 0 {
		chanReserve = acceptorResp.Reserve
	}

	remoteMaxValue := f.cfg.RequiredRemoteMaxValue(amt)
	if acceptorResp.InFlightTotal != 0 {
		remoteMaxValue = acceptorResp.InFlightTotal
	}

	maxHtlcs := f.cfg.RequiredRemoteMaxHTLCs(amt)
	if acceptorResp.HtlcLimit != 0 {
		maxHtlcs = acceptorResp.HtlcLimit
	}

	// Default to our default minimum hltc value, replacing it with the
	// channel acceptor's value if it is set.
	minHtlc := f.cfg.DefaultMinHtlcIn
	if acceptorResp.MinHtlcIn != 0 {
		minHtlc = acceptorResp.MinHtlcIn
	}

	// If we are handling a FundingOpen request then we need to
	// specify the default channel fees since they are not provided
	// by the responder interactively.
	forwardingPolicy := htlcswitch.ForwardingPolicy{
		BaseFee: f.cfg.DefaultRoutingPolicy.BaseFee,
		FeeRate: f.cfg.DefaultRoutingPolicy.FeeRate,
	}

	// Once the reservation has been created successfully, we add it to
	// this peer's map of pending reservations to track this particular
	// reservation until either abort or completion.
	f.resMtx.Lock()
	if _, ok := f.activeReservations[peerIDKey]; !ok {
		f.activeReservations[peerIDKey] = make(pendingChannels)
	}
	resCtx := &reservationWithCtx{
		reservation:       reservation,
		chanAmt:           amt,
		forwardingPolicy:  forwardingPolicy,
		remoteCsvDelay:    remoteCsvDelay,
		remoteMinHtlc:     minHtlc,
		remoteMaxValue:    remoteMaxValue,
		remoteMaxHtlcs:    maxHtlcs,
		remoteChanReserve: chanReserve,
		maxLocalCsv:       f.cfg.MaxLocalCSVDelay,
		channelType:       msg.ChannelType,
		err:               make(chan error, 1),
		peer:              peer,
	}
	f.activeReservations[peerIDKey][msg.PendingChannelID] = resCtx
	f.resMtx.Unlock()

	// Update the timestamp once the fundingOpenMsg has been handled.
	defer resCtx.updateTimestamp()

	// With our parameters set, we'll now process their contribution so we
	// can move the funding workflow ahead.
	remoteContribution := &lnwallet.ChannelContribution{
		FundingAmount:        amt,
		FirstCommitmentPoint: msg.FirstCommitmentPoint,
		ChannelConfig: &channeldb.ChannelConfig{
			ChannelConstraints: channeldb.ChannelConstraints{
				DustLimit:        msg.DustLimit,
				MaxPendingAmount: remoteMaxValue,
				ChanReserve:      chanReserve,
				MinHTLC:          minHtlc,
				MaxAcceptedHtlcs: maxHtlcs,
				CsvDelay:         remoteCsvDelay,
			},
			MultiSigKey: keychain.KeyDescriptor{
				PubKey: copyPubKey(msg.FundingKey),
			},
			RevocationBasePoint: keychain.KeyDescriptor{
				PubKey: copyPubKey(msg.RevocationPoint),
			},
			PaymentBasePoint: keychain.KeyDescriptor{
				PubKey: copyPubKey(msg.PaymentPoint),
			},
			DelayBasePoint: keychain.KeyDescriptor{
				PubKey: copyPubKey(msg.DelayedPaymentPoint),
			},
			HtlcBasePoint: keychain.KeyDescriptor{
				PubKey: copyPubKey(msg.HtlcPoint),
			},
		},
		UpfrontShutdown: msg.UpfrontShutdownScript,
	}
	err = reservation.ProcessSingleContribution(remoteContribution)
	if err != nil {
		log.Errorf("unable to add contribution reservation: %v", err)
		f.failFundingFlow(peer, msg.PendingChannelID, err)
		return
	}

	log.Infof("Sending fundingResp for pending_id(%x)",
		msg.PendingChannelID)
	log.Debugf("Remote party accepted commitment constraints: %v",
		spew.Sdump(remoteContribution.ChannelConfig.ChannelConstraints))

	// With the initiator's contribution recorded, respond with our
	// contribution in the next message of the workflow.
	ourContribution := reservation.OurContribution()
	fundingAccept := lnwire.AcceptChannel{
		PendingChannelID:      msg.PendingChannelID,
		DustLimit:             ourContribution.DustLimit,
		MaxValueInFlight:      remoteMaxValue,
		ChannelReserve:        chanReserve,
		MinAcceptDepth:        uint32(numConfsReq),
		HtlcMinimum:           minHtlc,
		CsvDelay:              remoteCsvDelay,
		MaxAcceptedHTLCs:      maxHtlcs,
		FundingKey:            ourContribution.MultiSigKey.PubKey,
		RevocationPoint:       ourContribution.RevocationBasePoint.PubKey,
		PaymentPoint:          ourContribution.PaymentBasePoint.PubKey,
		DelayedPaymentPoint:   ourContribution.DelayBasePoint.PubKey,
		HtlcPoint:             ourContribution.HtlcBasePoint.PubKey,
		FirstCommitmentPoint:  ourContribution.FirstCommitmentPoint,
		UpfrontShutdownScript: ourContribution.UpfrontShutdown,
		ChannelType:           chanTypeFeatureBits,
		LeaseExpiry:           msg.LeaseExpiry,
	}

	if err := peer.SendMessage(true, &fundingAccept); err != nil {
		log.Errorf("unable to send funding response to peer: %v", err)
		f.failFundingFlow(peer, msg.PendingChannelID, err)
		return
	}
}

// handleFundingAccept processes a response to the workflow initiation sent by
// the remote peer. This message then queues a message with the funding
// outpoint, and a commitment signature to the remote peer.
func (f *Manager) handleFundingAccept(peer lnpeer.Peer,
	msg *lnwire.AcceptChannel) {

	pendingChanID := msg.PendingChannelID
	peerKey := peer.IdentityKey()
	var peerKeyBytes []byte
	if peerKey != nil {
		peerKeyBytes = peerKey.SerializeCompressed()
	}

	resCtx, err := f.getReservationCtx(peerKey, pendingChanID)
	if err != nil {
		log.Warnf("Can't find reservation (peerKey:%x, chan_id:%v)",
			peerKeyBytes, pendingChanID)
		return
	}

	// Update the timestamp once the fundingAcceptMsg has been handled.
	defer resCtx.updateTimestamp()

	log.Infof("Recv'd fundingResponse for pending_id(%x)",
		pendingChanID[:])

	// Perform some basic validation of any custom TLV records included.
	//
	// TODO: Return errors as funding.Error to give context to remote peer?
	if resCtx.channelType != nil {
		// We'll want to quickly check that the ChannelType echoed by
		// the channel request recipient matches what we proposed.
		if msg.ChannelType == nil {
			err := errors.New("explicit channel type not echoed " +
				"back")
			f.failFundingFlow(peer, msg.PendingChannelID, err)
			return
		}
		proposedFeatures := lnwire.RawFeatureVector(*resCtx.channelType)
		ackedFeatures := lnwire.RawFeatureVector(*msg.ChannelType)
		if !proposedFeatures.Equals(&ackedFeatures) {
			err := errors.New("channel type mismatch")
			f.failFundingFlow(peer, msg.PendingChannelID, err)
			return
		}

		// We'll want to do the same with the LeaseExpiry if one should
		// be set.
		if resCtx.reservation.LeaseExpiry() != 0 {
			if msg.LeaseExpiry == nil {
				err := errors.New("lease expiry not echoed " +
					"back")
				f.failFundingFlow(
					peer, msg.PendingChannelID, err,
				)
				return
			}
			if uint32(*msg.LeaseExpiry) !=
				resCtx.reservation.LeaseExpiry() {

				err := errors.New("lease expiry mismatch")
				f.failFundingFlow(
					peer, msg.PendingChannelID, err,
				)
				return
			}
		}
	} else if msg.ChannelType != nil {
		// The spec isn't too clear about whether it's okay to set the
		// channel type in the accept_channel response if we didn't
		// explicitly set it in the open_channel message. For now, let's
		// just log the problem instead of failing the funding flow.
		_, implicitChannelType := implicitNegotiateCommitmentType(
			peer.LocalFeatures(), peer.RemoteFeatures(),
		)

		// We pass in false here as the funder since at this point, we
		// didn't set a chan type ourselves, so falling back to
		// implicit funding is acceptable.
		_, _, negotiatedChannelType, err := negotiateCommitmentType(
			msg.ChannelType, peer.LocalFeatures(),
			peer.RemoteFeatures(), false,
		)
		if err != nil {
			err := errors.New("received unexpected channel type")
			f.failFundingFlow(peer, msg.PendingChannelID, err)
			return
		}

		// Even though we don't expect a channel type to be set when we
		// didn't send one in the first place, we check that it's the
		// same type we'd have arrived through implicit negotiation. If
		// it's another type, we fail the flow.
		if implicitChannelType != negotiatedChannelType {
			err := errors.New("negotiated unexpected channel type")
			f.failFundingFlow(peer, msg.PendingChannelID, err)
			return
		}
	}

	// The required number of confirmations should not be greater than the
	// maximum number of confirmations required by the ChainNotifier to
	// properly dispatch confirmations.
	if msg.MinAcceptDepth > chainntnfs.MaxNumConfs {
		err := lnwallet.ErrNumConfsTooLarge(
			msg.MinAcceptDepth, chainntnfs.MaxNumConfs,
		)
		log.Warnf("Unacceptable channel constraints: %v", err)
		f.failFundingFlow(peer, msg.PendingChannelID, err)
		return
	}

	// Check that zero-conf channels have minimum depth set to 0.
	if resCtx.reservation.IsZeroConf() && msg.MinAcceptDepth != 0 {
		err = fmt.Errorf("zero-conf channel has min_depth non-zero")
		log.Warn(err)
		f.failFundingFlow(peer, msg.PendingChannelID, err)
		return
	}

	// Fail early if minimum depth is set to 0 and the channel is not
	// zero-conf.
	if !resCtx.reservation.IsZeroConf() && msg.MinAcceptDepth == 0 {
		err = fmt.Errorf("non-zero-conf channel has min depth zero")
		log.Warn(err)
		f.failFundingFlow(peer, msg.PendingChannelID, err)
		return
	}

	// We'll also specify the responder's preference for the number of
	// required confirmations, and also the set of channel constraints
	// they've specified for commitment states we can create.
	resCtx.reservation.SetNumConfsRequired(uint16(msg.MinAcceptDepth))
	channelConstraints := &channeldb.ChannelConstraints{
		DustLimit:        msg.DustLimit,
		ChanReserve:      msg.ChannelReserve,
		MaxPendingAmount: msg.MaxValueInFlight,
		MinHTLC:          msg.HtlcMinimum,
		MaxAcceptedHtlcs: msg.MaxAcceptedHTLCs,
		CsvDelay:         msg.CsvDelay,
	}
	err = resCtx.reservation.CommitConstraints(
		channelConstraints, resCtx.maxLocalCsv, false,
	)
	if err != nil {
		log.Warnf("Unacceptable channel constraints: %v", err)
		f.failFundingFlow(peer, msg.PendingChannelID, err)
		return
	}

	// The remote node has responded with their portion of the channel
	// contribution. At this point, we can process their contribution which
	// allows us to construct and sign both the commitment transaction, and
	// the funding transaction.
	remoteContribution := &lnwallet.ChannelContribution{
		FirstCommitmentPoint: msg.FirstCommitmentPoint,
		ChannelConfig: &channeldb.ChannelConfig{
			ChannelConstraints: channeldb.ChannelConstraints{
				DustLimit:        msg.DustLimit,
				MaxPendingAmount: resCtx.remoteMaxValue,
				ChanReserve:      resCtx.remoteChanReserve,
				MinHTLC:          resCtx.remoteMinHtlc,
				MaxAcceptedHtlcs: resCtx.remoteMaxHtlcs,
				CsvDelay:         resCtx.remoteCsvDelay,
			},
			MultiSigKey: keychain.KeyDescriptor{
				PubKey: copyPubKey(msg.FundingKey),
			},
			RevocationBasePoint: keychain.KeyDescriptor{
				PubKey: copyPubKey(msg.RevocationPoint),
			},
			PaymentBasePoint: keychain.KeyDescriptor{
				PubKey: copyPubKey(msg.PaymentPoint),
			},
			DelayBasePoint: keychain.KeyDescriptor{
				PubKey: copyPubKey(msg.DelayedPaymentPoint),
			},
			HtlcBasePoint: keychain.KeyDescriptor{
				PubKey: copyPubKey(msg.HtlcPoint),
			},
		},
		UpfrontShutdown: msg.UpfrontShutdownScript,
	}
	err = resCtx.reservation.ProcessContribution(remoteContribution)

	// The wallet has detected that a PSBT funding process was requested by
	// the user and has halted the funding process after negotiating the
	// multisig keys. We now have everything that is needed for the user to
	// start constructing a PSBT that sends to the multisig funding address.
	var psbtIntent *chanfunding.PsbtIntent
	if psbtErr, ok := err.(*lnwallet.PsbtFundingRequired); ok {
		// Return the information that is needed by the user to
		// construct the PSBT back to the caller.
		addr, amt, packet, err := psbtErr.Intent.FundingParams()
		if err != nil {
			log.Errorf("Unable to process PSBT funding params "+
				"for contribution from %x: %v", peerKeyBytes,
				err)
			f.failFundingFlow(peer, msg.PendingChannelID, err)
			return
		}
		var buf bytes.Buffer
		err = packet.Serialize(&buf)
		if err != nil {
			log.Errorf("Unable to serialize PSBT for "+
				"contribution from %x: %v", peerKeyBytes, err)
			f.failFundingFlow(peer, msg.PendingChannelID, err)
			return
		}
		resCtx.updates <- &lnrpc.OpenStatusUpdate{
			PendingChanId: pendingChanID[:],
			Update: &lnrpc.OpenStatusUpdate_PsbtFund{
				PsbtFund: &lnrpc.ReadyForPsbtFunding{
					FundingAddress: addr.EncodeAddress(),
					FundingAmount:  amt,
					Psbt:           buf.Bytes(),
				},
			},
		}
		psbtIntent = psbtErr.Intent
	} else if err != nil {
		log.Errorf("Unable to process contribution from %x: %v",
			peerKeyBytes, err)
		f.failFundingFlow(peer, msg.PendingChannelID, err)
		return
	}

	log.Infof("pendingChan(%x): remote party proposes num_confs=%v, "+
		"csv_delay=%v", pendingChanID[:], msg.MinAcceptDepth,
		msg.CsvDelay)
	log.Debugf("Remote party accepted commitment constraints: %v",
		spew.Sdump(remoteContribution.ChannelConfig.ChannelConstraints))

	// If the user requested funding through a PSBT, we cannot directly
	// continue now and need to wait for the fully funded and signed PSBT
	// to arrive. To not block any other channels from opening, we wait in
	// a separate goroutine.
	if psbtIntent != nil {
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()

			f.waitForPsbt(psbtIntent, resCtx, pendingChanID)
		}()

		// With the new goroutine spawned, we can now exit to unblock
		// the main event loop.
		return
	}

	// In a normal, non-PSBT funding flow, we can jump directly to the next
	// step where we expect our contribution to be finalized.
	f.continueFundingAccept(resCtx, pendingChanID)
}

// waitForPsbt blocks until either a signed PSBT arrives, an error occurs or
// the funding manager shuts down. In the case of a valid PSBT, the funding flow
// is continued.
//
// NOTE: This method must be called as a goroutine.
func (f *Manager) waitForPsbt(intent *chanfunding.PsbtIntent,
	resCtx *reservationWithCtx, pendingChanID [32]byte) {

	// failFlow is a helper that logs an error message with the current
	// context and then fails the funding flow.
	peerKey := resCtx.peer.IdentityKey()
	failFlow := func(errMsg string, cause error) {
		log.Errorf("Unable to handle funding accept message "+
			"for peer_key=%x, pending_chan_id=%x: %s: %v",
			peerKey.SerializeCompressed(), pendingChanID, errMsg,
			cause)
		f.failFundingFlow(resCtx.peer, pendingChanID, cause)
	}

	// We'll now wait until the intent has received the final and complete
	// funding transaction. If the channel is closed without any error being
	// sent, we know everything's going as expected.
	select {
	case err := <-intent.PsbtReady:
		switch err {
		// If the user canceled the funding reservation, we need to
		// inform the other peer about us canceling the reservation.
		case chanfunding.ErrUserCanceled:
			failFlow("aborting PSBT flow", err)
			return

		// If the remote canceled the funding reservation, we don't need
		// to send another fail message. But we want to inform the user
		// about what happened.
		case chanfunding.ErrRemoteCanceled:
			log.Infof("Remote canceled, aborting PSBT flow "+
				"for peer_key=%x, pending_chan_id=%x",
				peerKey.SerializeCompressed(), pendingChanID)
			return

		// Nil error means the flow continues normally now.
		case nil:

		// For any other error, we'll fail the funding flow.
		default:
			failFlow("error waiting for PSBT flow", err)
			return
		}

		// A non-nil error means we can continue the funding flow.
		// Notify the wallet so it can prepare everything we need to
		// continue.
		err = resCtx.reservation.ProcessPsbt()
		if err != nil {
			failFlow("error continuing PSBT flow", err)
			return
		}

		// We are now ready to continue the funding flow.
		f.continueFundingAccept(resCtx, pendingChanID)

	// Handle a server shutdown as well because the reservation won't
	// survive a restart as it's in memory only.
	case <-f.quit:
		log.Errorf("Unable to handle funding accept message "+
			"for peer_key=%x, pending_chan_id=%x: funding manager "+
			"shutting down", peerKey.SerializeCompressed(),
			pendingChanID)
		return
	}
}

// continueFundingAccept continues the channel funding flow once our
// contribution is finalized, the channel output is known and the funding
// transaction is signed.
func (f *Manager) continueFundingAccept(resCtx *reservationWithCtx,
	pendingChanID [32]byte) {

	// Now that we have their contribution, we can extract, then send over
	// both the funding out point and our signature for their version of
	// the commitment transaction to the remote peer.
	outPoint := resCtx.reservation.FundingOutpoint()
	_, sig := resCtx.reservation.OurSignatures()

	// A new channel has almost finished the funding process. In order to
	// properly synchronize with the writeHandler goroutine, we add a new
	// channel to the barriers map which will be closed once the channel is
	// fully open.
	f.barrierMtx.Lock()
	channelID := lnwire.NewChanIDFromOutPoint(outPoint)
	log.Debugf("Creating chan barrier for ChanID(%v)", channelID)
	f.newChanBarriers[channelID] = make(chan struct{})
	f.barrierMtx.Unlock()

	// The next message that advances the funding flow will reference the
	// channel via its permanent channel ID, so we'll set up this mapping
	// so we can retrieve the reservation context once we get the
	// FundingSigned message.
	f.resMtx.Lock()
	f.signedReservations[channelID] = pendingChanID
	f.resMtx.Unlock()

	log.Infof("Generated ChannelPoint(%v) for pending_id(%x)", outPoint,
		pendingChanID[:])

	var err error
	fundingCreated := &lnwire.FundingCreated{
		PendingChannelID: pendingChanID,
		FundingPoint:     *outPoint,
	}
	fundingCreated.CommitSig, err = lnwire.NewSigFromSignature(sig)
	if err != nil {
		log.Errorf("Unable to parse signature: %v", err)
		f.failFundingFlow(resCtx.peer, pendingChanID, err)
		return
	}
	if err := resCtx.peer.SendMessage(true, fundingCreated); err != nil {
		log.Errorf("Unable to send funding complete message: %v", err)
		f.failFundingFlow(resCtx.peer, pendingChanID, err)
		return
	}
}

// handleFundingCreated progresses the funding workflow when the daemon is on
// the responding side of a single funder workflow. Once this message has been
// processed, a signature is sent to the remote peer allowing it to broadcast
// the funding transaction, progressing the workflow into the final stage.
func (f *Manager) handleFundingCreated(peer lnpeer.Peer,
	msg *lnwire.FundingCreated) {

	peerKey := peer.IdentityKey()
	pendingChanID := msg.PendingChannelID

	resCtx, err := f.getReservationCtx(peerKey, pendingChanID)
	if err != nil {
		log.Warnf("can't find reservation (peer_id:%v, chan_id:%x)",
			peerKey, pendingChanID[:])
		return
	}

	// The channel initiator has responded with the funding outpoint of the
	// final funding transaction, as well as a signature for our version of
	// the commitment transaction. So at this point, we can validate the
	// initiator's commitment transaction, then send our own if it's valid.
	// TODO(roasbeef): make case (p vs P) consistent throughout
	fundingOut := msg.FundingPoint
	log.Infof("completing pending_id(%x) with ChannelPoint(%v)",
		pendingChanID[:], fundingOut)

	commitSig, err := msg.CommitSig.ToSignature()
	if err != nil {
		log.Errorf("unable to parse signature: %v", err)
		f.failFundingFlow(peer, pendingChanID, err)
		return
	}

	// With all the necessary data available, attempt to advance the
	// funding workflow to the next stage. If this succeeds then the
	// funding transaction will broadcast after our next message.
	// CompleteReservationSingle will also mark the channel as 'IsPending'
	// in the database.
	completeChan, err := resCtx.reservation.CompleteReservationSingle(
		&fundingOut, commitSig,
	)
	if err != nil {
		// TODO(roasbeef): better error logging: peerID, channelID, etc.
		log.Errorf("unable to complete single reservation: %v", err)
		f.failFundingFlow(peer, pendingChanID, err)
		return
	}

	// Get forwarding policy before deleting the reservation context.
	forwardingPolicy := resCtx.forwardingPolicy

	// The channel is marked IsPending in the database, and can be removed
	// from the set of active reservations.
	f.deleteReservationCtx(peerKey, msg.PendingChannelID)

	// If something goes wrong before the funding transaction is confirmed,
	// we use this convenience method to delete the pending OpenChannel
	// from the database.
	deleteFromDatabase := func() {
		localBalance := completeChan.LocalCommitment.LocalBalance.ToSatoshis()
		closeInfo := &channeldb.ChannelCloseSummary{
			ChanPoint:               completeChan.FundingOutpoint,
			ChainHash:               completeChan.ChainHash,
			RemotePub:               completeChan.IdentityPub,
			CloseType:               channeldb.FundingCanceled,
			Capacity:                completeChan.Capacity,
			SettledBalance:          localBalance,
			RemoteCurrentRevocation: completeChan.RemoteCurrentRevocation,
			RemoteNextRevocation:    completeChan.RemoteNextRevocation,
			LocalChanConfig:         completeChan.LocalChanCfg,
		}

		// Close the channel with us as the initiator because we are
		// deciding to exit the funding flow due to an internal error.
		if err := completeChan.CloseChannel(
			closeInfo, channeldb.ChanStatusLocalCloseInitiator,
		); err != nil {
			log.Errorf("Failed closing channel %v: %v",
				completeChan.FundingOutpoint, err)
		}
	}

	// A new channel has almost finished the funding process. In order to
	// properly synchronize with the writeHandler goroutine, we add a new
	// channel to the barriers map which will be closed once the channel is
	// fully open.
	f.barrierMtx.Lock()
	channelID := lnwire.NewChanIDFromOutPoint(&fundingOut)
	log.Debugf("Creating chan barrier for ChanID(%v)", channelID)
	f.newChanBarriers[channelID] = make(chan struct{})
	f.barrierMtx.Unlock()

	log.Infof("sending FundingSigned for pending_id(%x) over "+
		"ChannelPoint(%v)", pendingChanID[:], fundingOut)

	// With their signature for our version of the commitment transaction
	// verified, we can now send over our signature to the remote peer.
	_, sig := resCtx.reservation.OurSignatures()
	ourCommitSig, err := lnwire.NewSigFromSignature(sig)
	if err != nil {
		log.Errorf("unable to parse signature: %v", err)
		f.failFundingFlow(peer, pendingChanID, err)
		deleteFromDatabase()
		return
	}

	fundingSigned := &lnwire.FundingSigned{
		ChanID:    channelID,
		CommitSig: ourCommitSig,
	}
	if err := peer.SendMessage(true, fundingSigned); err != nil {
		log.Errorf("unable to send FundingSigned message: %v", err)
		f.failFundingFlow(peer, pendingChanID, err)
		deleteFromDatabase()
		return
	}

	// With a permanent channel id established we can save the respective
	// forwarding policy in the database. In the channel announcement phase
	// this forwarding policy is retrieved and applied.
	err = f.saveInitialFwdingPolicy(channelID, &forwardingPolicy)
	if err != nil {
		log.Errorf("Unable to store the forwarding policy: %v", err)
	}

	// Now that we've sent over our final signature for this channel, we'll
	// send it to the ChainArbitrator so it can watch for any on-chain
	// actions during this final confirmation stage.
	if err := f.cfg.WatchNewChannel(completeChan, peerKey); err != nil {
		log.Errorf("Unable to send new ChannelPoint(%v) for "+
			"arbitration: %v", fundingOut, err)
	}

	// Create an entry in the local discovery map so we can ensure that we
	// process the channel confirmation fully before we receive a funding
	// locked message.
	f.localDiscoveryMtx.Lock()
	f.localDiscoverySignals[channelID] = make(chan struct{})
	f.localDiscoveryMtx.Unlock()

	// Inform the ChannelNotifier that the channel has entered
	// pending open state.
	f.cfg.NotifyPendingOpenChannelEvent(fundingOut, completeChan)

	// At this point we have sent our last funding message to the
	// initiating peer before the funding transaction will be broadcast.
	// With this last message, our job as the responder is now complete.
	// We'll wait for the funding transaction to reach the specified number
	// of confirmations, then start normal operations.
	//
	// When we get to this point we have sent the signComplete message to
	// the channel funder, and BOLT#2 specifies that we MUST remember the
	// channel for reconnection. The channel is already marked
	// as pending in the database, so in case of a disconnect or restart,
	// we will continue waiting for the confirmation the next time we start
	// the funding manager. In case the funding transaction never appears
	// on the blockchain, we must forget this channel. We therefore
	// completely forget about this channel if we haven't seen the funding
	// transaction in 288 blocks (~ 48 hrs), by canceling the reservation
	// and canceling the wait for the funding confirmation.
	f.wg.Add(1)
	go f.advanceFundingState(completeChan, pendingChanID, nil)
}

// handleFundingSigned processes the final message received in a single funder
// workflow. Once this message is processed, the funding transaction is
// broadcast. Once the funding transaction reaches a sufficient number of
// confirmations, a message is sent to the responding peer along with a compact
// encoding of the location of the channel within the blockchain.
func (f *Manager) handleFundingSigned(peer lnpeer.Peer,
	msg *lnwire.FundingSigned) {

	// As the funding signed message will reference the reservation by its
	// permanent channel ID, we'll need to perform an intermediate look up
	// before we can obtain the reservation.
	f.resMtx.Lock()
	pendingChanID, ok := f.signedReservations[msg.ChanID]
	delete(f.signedReservations, msg.ChanID)
	f.resMtx.Unlock()
	if !ok {
		err := fmt.Errorf("unable to find signed reservation for "+
			"chan_id=%x", msg.ChanID)
		log.Warnf(err.Error())
		f.failFundingFlow(peer, msg.ChanID, err)
		return
	}

	peerKey := peer.IdentityKey()
	resCtx, err := f.getReservationCtx(peerKey, pendingChanID)
	if err != nil {
		log.Warnf("Unable to find reservation (peer_id:%v, "+
			"chan_id:%x)", peerKey, pendingChanID[:])
		// TODO: add ErrChanNotFound?
		f.failFundingFlow(peer, pendingChanID, err)
		return
	}

	// Create an entry in the local discovery map so we can ensure that we
	// process the channel confirmation fully before we receive a funding
	// locked message.
	fundingPoint := resCtx.reservation.FundingOutpoint()
	permChanID := lnwire.NewChanIDFromOutPoint(fundingPoint)
	f.localDiscoveryMtx.Lock()
	f.localDiscoverySignals[permChanID] = make(chan struct{})
	f.localDiscoveryMtx.Unlock()

	// We have to store the forwardingPolicy before the reservation context
	// is deleted. The policy will then be read and applied in
	// newChanAnnouncement.
	err = f.saveInitialFwdingPolicy(permChanID, &resCtx.forwardingPolicy)
	if err != nil {
		log.Errorf("Unable to store the forwarding policy: %v", err)
	}

	// The remote peer has responded with a signature for our commitment
	// transaction. We'll verify the signature for validity, then commit
	// the state to disk as we can now open the channel.
	commitSig, err := msg.CommitSig.ToSignature()
	if err != nil {
		log.Errorf("Unable to parse signature: %v", err)
		f.failFundingFlow(peer, pendingChanID, err)
		return
	}

	completeChan, err := resCtx.reservation.CompleteReservation(
		nil, commitSig,
	)
	if err != nil {
		log.Errorf("Unable to complete reservation sign "+
			"complete: %v", err)
		f.failFundingFlow(peer, pendingChanID, err)
		return
	}

	// The channel is now marked IsPending in the database, and we can
	// delete it from our set of active reservations.
	f.deleteReservationCtx(peerKey, pendingChanID)

	// Broadcast the finalized funding transaction to the network, but only
	// if we actually have the funding transaction.
	if completeChan.ChanType.HasFundingTx() {
		fundingTx := completeChan.FundingTxn
		var fundingTxBuf bytes.Buffer
		if err := fundingTx.Serialize(&fundingTxBuf); err != nil {
			log.Errorf("Unable to serialize funding "+
				"transaction %v: %v", fundingTx.TxHash(), err)

			// Clear the buffer of any bytes that were written
			// before the serialization error to prevent logging an
			// incomplete transaction.
			fundingTxBuf.Reset()
		}

		log.Infof("Broadcasting funding tx for ChannelPoint(%v): %x",
			completeChan.FundingOutpoint, fundingTxBuf.Bytes())

		// Set a nil short channel ID at this stage because we do not
		// know it until our funding tx confirms.
		label := labels.MakeLabel(
			labels.LabelTypeChannelOpen, nil,
		)

		err = f.cfg.PublishTransaction(fundingTx, label)
		if err != nil {
			log.Errorf("Unable to broadcast funding tx %x for "+
				"ChannelPoint(%v): %v", fundingTxBuf.Bytes(),
				completeChan.FundingOutpoint, err)

			// We failed to broadcast the funding transaction, but
			// watch the channel regardless, in case the
			// transaction made it to the network. We will retry
			// broadcast at startup.
			//
			// TODO(halseth): retry more often? Handle with CPFP?
			// Just delete from the DB?
		}
	}

	// Now that we have a finalized reservation for this funding flow,
	// we'll send the to be active channel to the ChainArbitrator so it can
	// watch for any on-chain actions before the channel has fully
	// confirmed.
	if err := f.cfg.WatchNewChannel(completeChan, peerKey); err != nil {
		log.Errorf("Unable to send new ChannelPoint(%v) for "+
			"arbitration: %v", fundingPoint, err)
	}

	log.Infof("Finalizing pending_id(%x) over ChannelPoint(%v), "+
		"waiting for channel open on-chain", pendingChanID[:],
		fundingPoint)

	// Send an update to the upstream client that the negotiation process
	// is over.
	//
	// TODO(roasbeef): add abstraction over updates to accommodate
	// long-polling, or SSE, etc.
	upd := &lnrpc.OpenStatusUpdate{
		Update: &lnrpc.OpenStatusUpdate_ChanPending{
			ChanPending: &lnrpc.PendingUpdate{
				Txid:        fundingPoint.Hash[:],
				OutputIndex: fundingPoint.Index,
			},
		},
		PendingChanId: pendingChanID[:],
	}

	select {
	case resCtx.updates <- upd:
		// Inform the ChannelNotifier that the channel has entered
		// pending open state.
		f.cfg.NotifyPendingOpenChannelEvent(*fundingPoint, completeChan)
	case <-f.quit:
		return
	}

	// At this point we have broadcast the funding transaction and done all
	// necessary processing.
	f.wg.Add(1)
	go f.advanceFundingState(completeChan, pendingChanID, resCtx.updates)
}

// confirmedChannel wraps a confirmed funding transaction, as well as the short
// channel ID which identifies that channel into a single struct. We'll use
// this to pass around the final state of a channel after it has been
// confirmed.
type confirmedChannel struct {
	// shortChanID expresses where in the block the funding transaction was
	// located.
	shortChanID lnwire.ShortChannelID

	// fundingTx is the funding transaction that created the channel.
	fundingTx *wire.MsgTx
}

// fundingTimeout is called when callers of waitForFundingWithTimeout receive
// an ErrConfirmationTimeout. It is used to clean-up channel state and mark the
// channel as closed. The error is only returned for the responder of the
// channel flow.
func (f *Manager) fundingTimeout(c *channeldb.OpenChannel,
	pendingID [32]byte) error {

	// We'll get a timeout if the number of blocks mined since the channel
	// was initiated reaches maxWaitNumBlocksFundingConf and we are not the
	// channel initiator.
	localBalance := c.LocalCommitment.LocalBalance.ToSatoshis()
	closeInfo := &channeldb.ChannelCloseSummary{
		ChainHash:               c.ChainHash,
		ChanPoint:               c.FundingOutpoint,
		RemotePub:               c.IdentityPub,
		Capacity:                c.Capacity,
		SettledBalance:          localBalance,
		CloseType:               channeldb.FundingCanceled,
		RemoteCurrentRevocation: c.RemoteCurrentRevocation,
		RemoteNextRevocation:    c.RemoteNextRevocation,
		LocalChanConfig:         c.LocalChanCfg,
	}

	// Close the channel with us as the initiator because we are timing the
	// channel out.
	if err := c.CloseChannel(
		closeInfo, channeldb.ChanStatusLocalCloseInitiator,
	); err != nil {
		return fmt.Errorf("failed closing channel %v: %v",
			c.FundingOutpoint, err)
	}

	timeoutErr := fmt.Errorf("timeout waiting for funding tx (%v) to "+
		"confirm", c.FundingOutpoint)

	// When the peer comes online, we'll notify it that we are now
	// considering the channel flow canceled.
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()

		peer, err := f.waitForPeerOnline(c.IdentityPub)
		switch err {
		// We're already shutting down, so we can just return.
		case ErrFundingManagerShuttingDown:
			return

		// nil error means we continue on.
		case nil:

		// For unexpected errors, we print the error and still try to
		// fail the funding flow.
		default:
			log.Errorf("Unexpected error while waiting for peer "+
				"to come online: %v", err)
		}

		// TODO(halseth): should this send be made
		// reliable?

		// The reservation won't exist at this point, but we'll send an
		// Error message over anyways with ChanID set to pendingID.
		f.failFundingFlow(peer, pendingID, timeoutErr)
	}()

	return timeoutErr
}

// waitForFundingWithTimeout is a wrapper around waitForFundingConfirmation and
// waitForTimeout that will return ErrConfirmationTimeout if we are not the
// channel initiator and the maxWaitNumBlocksFundingConf has passed from the
// funding broadcast height. In case of confirmation, the short channel ID of
// the channel and the funding transaction will be returned.
func (f *Manager) waitForFundingWithTimeout(
	ch *channeldb.OpenChannel) (*confirmedChannel, error) {

	confChan := make(chan *confirmedChannel)
	timeoutChan := make(chan error, 1)
	cancelChan := make(chan struct{})

	f.wg.Add(1)
	go f.waitForFundingConfirmation(ch, cancelChan, confChan)

	// If we are not the initiator, we have no money at stake and will
	// timeout waiting for the funding transaction to confirm after a
	// while.
	if !ch.IsInitiator && !ch.IsZeroConf() {
		f.wg.Add(1)
		go f.waitForTimeout(ch, cancelChan, timeoutChan)
	}
	defer close(cancelChan)

	select {
	case err := <-timeoutChan:
		if err != nil {
			return nil, err
		}
		return nil, ErrConfirmationTimeout

	case <-f.quit:
		// The fundingManager is shutting down, and will resume wait on
		// startup.
		return nil, ErrFundingManagerShuttingDown

	case confirmedChannel, ok := <-confChan:
		if !ok {
			return nil, fmt.Errorf("waiting for funding" +
				"confirmation failed")
		}
		return confirmedChannel, nil
	}
}

// makeFundingScript re-creates the funding script for the funding transaction
// of the target channel.
func makeFundingScript(channel *channeldb.OpenChannel) ([]byte, error) {
	localKey := channel.LocalChanCfg.MultiSigKey.PubKey.SerializeCompressed()
	remoteKey := channel.RemoteChanCfg.MultiSigKey.PubKey.SerializeCompressed()

	multiSigScript, err := input.GenMultiSigScript(localKey, remoteKey)
	if err != nil {
		return nil, err
	}

	return input.WitnessScriptHash(multiSigScript)
}

// waitForFundingConfirmation handles the final stages of the channel funding
// process once the funding transaction has been broadcast. The primary
// function of waitForFundingConfirmation is to wait for blockchain
// confirmation, and then to notify the other systems that must be notified
// when a channel has become active for lightning transactions.
// The wait can be canceled by closing the cancelChan. In case of success,
// a *lnwire.ShortChannelID will be passed to confChan.
//
// NOTE: This MUST be run as a goroutine.
func (f *Manager) waitForFundingConfirmation(
	completeChan *channeldb.OpenChannel, cancelChan <-chan struct{},
	confChan chan<- *confirmedChannel) {

	defer f.wg.Done()
	defer close(confChan)

	// Register with the ChainNotifier for a notification once the funding
	// transaction reaches `numConfs` confirmations.
	txid := completeChan.FundingOutpoint.Hash
	fundingScript, err := makeFundingScript(completeChan)
	if err != nil {
		log.Errorf("unable to create funding script for "+
			"ChannelPoint(%v): %v", completeChan.FundingOutpoint,
			err)
		return
	}
	numConfs := uint32(completeChan.NumConfsRequired)

	// If the underlying channel is a zero-conf channel, we'll set numConfs
	// to 6, since it will be zero here.
	if completeChan.IsZeroConf() {
		numConfs = 6
	}

	confNtfn, err := f.cfg.Notifier.RegisterConfirmationsNtfn(
		&txid, fundingScript, numConfs,
		completeChan.BroadcastHeight(),
	)
	if err != nil {
		log.Errorf("Unable to register for confirmation of "+
			"ChannelPoint(%v): %v", completeChan.FundingOutpoint,
			err)
		return
	}

	log.Infof("Waiting for funding tx (%v) to reach %v confirmations",
		txid, numConfs)

	var confDetails *chainntnfs.TxConfirmation
	var ok bool

	// Wait until the specified number of confirmations has been reached,
	// we get a cancel signal, or the wallet signals a shutdown.
	select {
	case confDetails, ok = <-confNtfn.Confirmed:
		// fallthrough

	case <-cancelChan:
		log.Warnf("canceled waiting for funding confirmation, "+
			"stopping funding flow for ChannelPoint(%v)",
			completeChan.FundingOutpoint)
		return

	case <-f.quit:
		log.Warnf("fundingManager shutting down, stopping funding "+
			"flow for ChannelPoint(%v)",
			completeChan.FundingOutpoint)
		return
	}

	if !ok {
		log.Warnf("ChainNotifier shutting down, cannot complete "+
			"funding flow for ChannelPoint(%v)",
			completeChan.FundingOutpoint)
		return
	}

	fundingPoint := completeChan.FundingOutpoint
	log.Infof("ChannelPoint(%v) is now active: ChannelID(%v)",
		fundingPoint, lnwire.NewChanIDFromOutPoint(&fundingPoint))

	// With the block height and the transaction index known, we can
	// construct the compact chanID which is used on the network to unique
	// identify channels.
	shortChanID := lnwire.ShortChannelID{
		BlockHeight: confDetails.BlockHeight,
		TxIndex:     confDetails.TxIndex,
		TxPosition:  uint16(fundingPoint.Index),
	}

	select {
	case confChan <- &confirmedChannel{
		shortChanID: shortChanID,
		fundingTx:   confDetails.Tx,
	}:
	case <-f.quit:
		return
	}
}

// waitForTimeout will close the timeout channel if maxWaitNumBlocksFundingConf
// has passed from the broadcast height of the given channel. In case of error,
// the error is sent on timeoutChan. The wait can be canceled by closing the
// cancelChan.
//
// NOTE: timeoutChan MUST be buffered.
// NOTE: This MUST be run as a goroutine.
func (f *Manager) waitForTimeout(completeChan *channeldb.OpenChannel,
	cancelChan <-chan struct{}, timeoutChan chan<- error) {

	defer f.wg.Done()

	epochClient, err := f.cfg.Notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		timeoutChan <- fmt.Errorf("unable to register for epoch "+
			"notification: %v", err)
		return
	}

	defer epochClient.Cancel()

	// On block maxHeight we will cancel the funding confirmation wait.
	broadcastHeight := completeChan.BroadcastHeight()
	maxHeight := broadcastHeight + maxWaitNumBlocksFundingConf
	for {
		select {
		case epoch, ok := <-epochClient.Epochs:
			if !ok {
				timeoutChan <- fmt.Errorf("epoch client " +
					"shutting down")
				return
			}

			// Close the timeout channel and exit if the block is
			// above the max height.
			if uint32(epoch.Height) >= maxHeight {
				log.Warnf("Waited for %v blocks without "+
					"seeing funding transaction confirmed,"+
					" cancelling.",
					maxWaitNumBlocksFundingConf)

				// Notify the caller of the timeout.
				close(timeoutChan)
				return
			}

			// TODO: If we are the channel initiator implement
			// a method for recovering the funds from the funding
			// transaction

		case <-cancelChan:
			return

		case <-f.quit:
			// The fundingManager is shutting down, will resume
			// waiting for the funding transaction on startup.
			return
		}
	}
}

// makeLabelForTx updates the label for the confirmed funding transaction. If
// we opened the channel, and lnd's wallet published our funding tx (which is
// not the case for some channels) then we update our transaction label with
// our short channel ID, which is known now that our funding transaction has
// confirmed. We do not label transactions we did not publish, because our
// wallet has no knowledge of them.
func (f *Manager) makeLabelForTx(c *channeldb.OpenChannel) {
	if c.IsInitiator && c.ChanType.HasFundingTx() {
		shortChanID := c.ShortChanID()

		// For zero-conf channels, we'll use the actually-confirmed
		// short channel id.
		if c.IsZeroConf() {
			shortChanID = c.ZeroConfRealScid()
		}

		label := labels.MakeLabel(
			labels.LabelTypeChannelOpen, &shortChanID,
		)

		err := f.cfg.UpdateLabel(c.FundingOutpoint.Hash, label)
		if err != nil {
			log.Errorf("unable to update label: %v", err)
		}
	}
}

// handleFundingConfirmation marks a channel as open in the database, and set
// the channelOpeningState markedOpen. In addition it will report the now
// decided short channel ID to the switch, and close the local discovery signal
// for this channel.
func (f *Manager) handleFundingConfirmation(
	completeChan *channeldb.OpenChannel,
	confChannel *confirmedChannel) error {

	fundingPoint := completeChan.FundingOutpoint
	chanID := lnwire.NewChanIDFromOutPoint(&fundingPoint)

	// TODO(roasbeef): ideally persistent state update for chan above
	// should be abstracted

	// Now that that the channel has been fully confirmed, we'll request
	// that the wallet fully verify this channel to ensure that it can be
	// used.
	err := f.cfg.Wallet.ValidateChannel(completeChan, confChannel.fundingTx)
	if err != nil {
		// TODO(roasbeef): delete chan state?
		return fmt.Errorf("unable to validate channel: %v", err)
	}

	// Now that the channel has been validated, we'll persist an alias for
	// this channel if the option-scid-alias feature-bit was negotiated.
	if completeChan.NegotiatedAliasFeature() {
		aliasScid, err := f.cfg.AliasManager.RequestAlias()
		if err != nil {
			return fmt.Errorf("unable to request alias: %v", err)
		}

		err = f.cfg.AliasManager.AddLocalAlias(
			aliasScid, confChannel.shortChanID, true,
		)
		if err != nil {
			return fmt.Errorf("unable to request alias: %v", err)
		}
	}

	// The funding transaction now being confirmed, we add this channel to
	// the fundingManager's internal persistent state machine that we use
	// to track the remaining process of the channel opening. This is
	// useful to resume the opening process in case of restarts. We set the
	// opening state before we mark the channel opened in the database,
	// such that we can receover from one of the db writes failing.
	err = f.saveChannelOpeningState(
		&fundingPoint, markedOpen, &confChannel.shortChanID,
	)
	if err != nil {
		return fmt.Errorf("error setting channel state to "+
			"markedOpen: %v", err)
	}

	// Now that the channel has been fully confirmed and we successfully
	// saved the opening state, we'll mark it as open within the database.
	err = completeChan.MarkAsOpen(confChannel.shortChanID)
	if err != nil {
		return fmt.Errorf("error setting channel pending flag to "+
			"false:	%v", err)
	}

	// Update the confirmed funding transaction label.
	f.makeLabelForTx(completeChan)

	// Inform the ChannelNotifier that the channel has transitioned from
	// pending open to open.
	f.cfg.NotifyOpenChannelEvent(completeChan.FundingOutpoint)

	// Close the discoverySignal channel, indicating to a separate
	// goroutine that the channel now is marked as open in the database
	// and that it is acceptable to process funding locked messages
	// from the peer.
	f.localDiscoveryMtx.Lock()
	if discoverySignal, ok := f.localDiscoverySignals[chanID]; ok {
		close(discoverySignal)
	}
	f.localDiscoveryMtx.Unlock()

	return nil
}

// sendChannelReady creates and sends the channelReady message.
// This should be called after the funding transaction has been confirmed,
// and the channelState is 'markedOpen'.
func (f *Manager) sendChannelReady(completeChan *channeldb.OpenChannel,
	channel *lnwallet.LightningChannel) error {

	chanID := lnwire.NewChanIDFromOutPoint(&completeChan.FundingOutpoint)

	var peerKey [33]byte
	copy(peerKey[:], completeChan.IdentityPub.SerializeCompressed())

	// Next, we'll send over the funding locked message which marks that we
	// consider the channel open by presenting the remote party with our
	// next revocation key. Without the revocation key, the remote party
	// will be unable to propose state transitions.
	nextRevocation, err := channel.NextRevocationKey()
	if err != nil {
		return fmt.Errorf("unable to create next revocation: %v", err)
	}
	channelReadyMsg := lnwire.NewChannelReady(chanID, nextRevocation)

	// If the channel negotiated the option-scid-alias feature bit, we'll
	// send a TLV segment that includes an alias the peer can use in their
	// invoice hop hints. We'll send the first alias we find for the
	// channel since it does not matter which alias we send. We'll error
	// out in the odd case that no aliases are found.
	if completeChan.NegotiatedAliasFeature() {
		aliases := f.cfg.AliasManager.GetAliases(
			completeChan.ShortChanID(),
		)
		if len(aliases) == 0 {
			return fmt.Errorf("no aliases found")
		}

		// We can use a pointer to aliases since GetAliases returns a
		// copy of the alias slice.
		channelReadyMsg.AliasScid = &aliases[0]
	}

	// If the peer has disconnected before we reach this point, we will need
	// to wait for him to come back online before sending the channelReady
	// message. This is special for channelReady, since failing to send any
	// of the previous messages in the funding flow just cancels the flow.
	// But now the funding transaction is confirmed, the channel is open
	// and we have to make sure the peer gets the channelReady message when
	// it comes back online. This is also crucial during restart of lnd,
	// where we might try to resend the channelReady message before the
	// server has had the time to connect to the peer. We keep trying to
	// send channelReady until we succeed, or the fundingManager is shut
	// down.
	for {
		peer, err := f.waitForPeerOnline(completeChan.IdentityPub)
		if err != nil {
			return err
		}

		localAlias := peer.LocalFeatures().HasFeature(
			lnwire.ScidAliasOptional,
		)
		remoteAlias := peer.RemoteFeatures().HasFeature(
			lnwire.ScidAliasOptional,
		)

		// We could also refresh the channel state instead of checking
		// whether the feature was negotiated, but this saves us a
		// database read.
		if channelReadyMsg.AliasScid == nil && localAlias &&
			remoteAlias {

			// If an alias was not assigned above and the scid
			// alias feature was negotiated, check if we already
			// have an alias stored in case handleChannelReady was
			// called before this. If an alias exists, use that in
			// channel_ready. Otherwise, request and store an
			// alias and use that.
			aliases := f.cfg.AliasManager.GetAliases(
				completeChan.ShortChannelID,
			)
			if len(aliases) == 0 {
				// No aliases were found.
				alias, err := f.cfg.AliasManager.RequestAlias()
				if err != nil {
					return err
				}

				err = f.cfg.AliasManager.AddLocalAlias(
					alias, completeChan.ShortChannelID,
					false,
				)
				if err != nil {
					return err
				}

				channelReadyMsg.AliasScid = &alias
			} else {
				channelReadyMsg.AliasScid = &aliases[0]
			}
		}

		log.Infof("Peer(%x) is online, sending ChannelReady "+
			"for ChannelID(%v)", peerKey, chanID)

		if err := peer.SendMessage(true, channelReadyMsg); err == nil {
			// Sending succeeded, we can break out and continue the
			// funding flow.
			break
		}

		log.Warnf("Unable to send channelReady to peer %x: %v. "+
			"Will retry when online", peerKey, err)
	}

	return nil
}

// receivedChannelReady checks whether or not we've received a ChannelReady
// from the remote peer. If we have, RemoteNextRevocation will be set.
func (f *Manager) receivedChannelReady(node *btcec.PublicKey,
	chanID lnwire.ChannelID) (bool, error) {

	// If the funding manager has exited, return an error to stop looping.
	// Note that the peer may appear as online while the funding manager
	// has stopped due to the shutdown order in the server.
	select {
	case <-f.quit:
		return false, ErrFundingManagerShuttingDown
	default:
	}

	// Avoid a tight loop if peer is offline.
	if _, err := f.waitForPeerOnline(node); err != nil {
		log.Errorf("Wait for peer online failed: %v", err)
		return false, err
	}

	channel, err := f.cfg.FindChannel(node, chanID)
	if err != nil {
		log.Errorf("Unable to locate ChannelID(%v) to determine if "+
			"ChannelReady was received", chanID)
		return false, err
	}

	return channel.RemoteNextRevocation != nil, nil
}

// extractAnnounceParams extracts the various channel announcement and update
// parameters that will be needed to construct a ChannelAnnouncement and a
// ChannelUpdate.
func (f *Manager) extractAnnounceParams(c *channeldb.OpenChannel) (
	lnwire.MilliSatoshi, lnwire.MilliSatoshi) {

	// We'll obtain the min HTLC value we can forward in our direction, as
	// we'll use this value within our ChannelUpdate. This constraint is
	// originally set by the remote node, as it will be the one that will
	// need to determine the smallest HTLC it deems economically relevant.
	fwdMinHTLC := c.LocalChanCfg.MinHTLC

	// We don't necessarily want to go as low as the remote party allows.
	// Check it against our default forwarding policy.
	if fwdMinHTLC < f.cfg.DefaultRoutingPolicy.MinHTLCOut {
		fwdMinHTLC = f.cfg.DefaultRoutingPolicy.MinHTLCOut
	}

	// We'll obtain the max HTLC value we can forward in our direction, as
	// we'll use this value within our ChannelUpdate. This value must be <=
	// channel capacity and <= the maximum in-flight msats set by the peer.
	fwdMaxHTLC := c.LocalChanCfg.MaxPendingAmount
	capacityMSat := lnwire.NewMSatFromSatoshis(c.Capacity)
	if fwdMaxHTLC > capacityMSat {
		fwdMaxHTLC = capacityMSat
	}

	return fwdMinHTLC, fwdMaxHTLC
}

// addToRouterGraph sends a ChannelAnnouncement and a ChannelUpdate to the
// gossiper so that the channel is added to the Router's internal graph.
// These announcement messages are NOT broadcasted to the greater network,
// only to the channel counter party. The proofs required to announce the
// channel to the greater network will be created and sent in annAfterSixConfs.
// The peerAlias is used for zero-conf channels to give the counter-party a
// ChannelUpdate they understand. ourPolicy may be set for various
// option-scid-alias channels to re-use the same policy.
func (f *Manager) addToRouterGraph(completeChan *channeldb.OpenChannel,
	shortChanID *lnwire.ShortChannelID,
	peerAlias *lnwire.ShortChannelID,
	ourPolicy *channeldb.ChannelEdgePolicy) error {

	chanID := lnwire.NewChanIDFromOutPoint(&completeChan.FundingOutpoint)

	fwdMinHTLC, fwdMaxHTLC := f.extractAnnounceParams(completeChan)

	ann, err := f.newChanAnnouncement(
		f.cfg.IDKey, completeChan.IdentityPub,
		&completeChan.LocalChanCfg.MultiSigKey,
		completeChan.RemoteChanCfg.MultiSigKey.PubKey, *shortChanID,
		chanID, fwdMinHTLC, fwdMaxHTLC, ourPolicy,
	)
	if err != nil {
		return fmt.Errorf("error generating channel "+
			"announcement: %v", err)
	}

	// Send ChannelAnnouncement and ChannelUpdate to the gossiper to add
	// to the Router's topology.
	errChan := f.cfg.SendAnnouncement(
		ann.chanAnn, discovery.ChannelCapacity(completeChan.Capacity),
		discovery.ChannelPoint(completeChan.FundingOutpoint),
	)
	select {
	case err := <-errChan:
		if err != nil {
			if routing.IsError(err, routing.ErrOutdated,
				routing.ErrIgnored) {

				log.Debugf("Router rejected "+
					"ChannelAnnouncement: %v", err)
			} else {
				return fmt.Errorf("error sending channel "+
					"announcement: %v", err)
			}
		}
	case <-f.quit:
		return ErrFundingManagerShuttingDown
	}

	errChan = f.cfg.SendAnnouncement(
		ann.chanUpdateAnn, discovery.RemoteAlias(peerAlias),
	)
	select {
	case err := <-errChan:
		if err != nil {
			if routing.IsError(err, routing.ErrOutdated,
				routing.ErrIgnored) {

				log.Debugf("Router rejected "+
					"ChannelUpdate: %v", err)
			} else {
				return fmt.Errorf("error sending channel "+
					"update: %v", err)
			}
		}
	case <-f.quit:
		return ErrFundingManagerShuttingDown
	}

	// The user can define non-default channel policies when opening a
	// channel. They are stored in the database to be persisted from the
	// moment of funding the channel to it being confirmed. We just
	// announced those policies to the network, but we also need to update
	// our local policy in the switch to make sure we can forward payments
	// with the correct fees. We can't do this when creating the link
	// initially as that only takes the static channel parameters.
	updatedPolicy := map[wire.OutPoint]htlcswitch.ForwardingPolicy{
		completeChan.FundingOutpoint: {
			MinHTLCOut: ann.chanUpdateAnn.HtlcMinimumMsat,
			MaxHTLC:    ann.chanUpdateAnn.HtlcMaximumMsat,
			BaseFee: lnwire.MilliSatoshi(
				ann.chanUpdateAnn.BaseFee,
			),
			FeeRate: lnwire.MilliSatoshi(
				ann.chanUpdateAnn.FeeRate,
			),
			TimeLockDelta: uint32(ann.chanUpdateAnn.TimeLockDelta),
		},
	}
	f.cfg.UpdateForwardingPolicies(updatedPolicy)

	return nil
}

// annAfterSixConfs broadcasts the necessary channel announcement messages to
// the network after 6 confs. Should be called after the channelReady message
// is sent and the channel is added to the router graph (channelState is
// 'addedToRouterGraph') and the channel is ready to be used. This is the last
// step in the channel opening process, and the opening state will be deleted
// from the database if successful.
func (f *Manager) annAfterSixConfs(completeChan *channeldb.OpenChannel,
	shortChanID *lnwire.ShortChannelID) error {

	// If this channel is not meant to be announced to the greater network,
	// we'll only send our NodeAnnouncement to our counterparty to ensure we
	// don't leak any of our information.
	announceChan := completeChan.ChannelFlags&lnwire.FFAnnounceChannel != 0
	if !announceChan {
		log.Debugf("Will not announce private channel %v.",
			shortChanID.ToUint64())

		peer, err := f.waitForPeerOnline(completeChan.IdentityPub)
		if err != nil {
			return err
		}

		nodeAnn, err := f.cfg.CurrentNodeAnnouncement()
		if err != nil {
			return fmt.Errorf("unable to retrieve current node "+
				"announcement: %v", err)
		}

		chanID := lnwire.NewChanIDFromOutPoint(
			&completeChan.FundingOutpoint,
		)
		pubKey := peer.PubKey()
		log.Debugf("Sending our NodeAnnouncement for "+
			"ChannelID(%v) to %x", chanID, pubKey)

		// TODO(halseth): make reliable. If the peer is not online this
		// will fail, and the opening process will stop. Should instead
		// block here, waiting for the peer to come online.
		if err := peer.SendMessage(true, &nodeAnn); err != nil {
			return fmt.Errorf("unable to send node announcement "+
				"to peer %x: %v", pubKey, err)
		}

		// For private channels we do not announce the channel policy
		// to the network but still need to delete them from the
		// database.
		err = f.deleteInitialFwdingPolicy(chanID)
		if err != nil {
			log.Infof("Could not delete channel fees "+
				"for chanId %x.", chanID)
		}
	} else {
		// Otherwise, we'll wait until the funding transaction has
		// reached 6 confirmations before announcing it.
		numConfs := uint32(completeChan.NumConfsRequired)
		if numConfs < 6 {
			numConfs = 6
		}
		txid := completeChan.FundingOutpoint.Hash
		log.Debugf("Will announce channel %v after ChannelPoint"+
			"(%v) has gotten %d confirmations",
			shortChanID.ToUint64(), completeChan.FundingOutpoint,
			numConfs)

		fundingScript, err := makeFundingScript(completeChan)
		if err != nil {
			return fmt.Errorf("unable to create funding script "+
				"for ChannelPoint(%v): %v",
				completeChan.FundingOutpoint, err)
		}

		// Register with the ChainNotifier for a notification once the
		// funding transaction reaches at least 6 confirmations.
		confNtfn, err := f.cfg.Notifier.RegisterConfirmationsNtfn(
			&txid, fundingScript, numConfs,
			completeChan.BroadcastHeight(),
		)
		if err != nil {
			return fmt.Errorf("unable to register for "+
				"confirmation of ChannelPoint(%v): %v",
				completeChan.FundingOutpoint, err)
		}

		// Wait until 6 confirmations has been reached or the wallet
		// signals a shutdown.
		select {
		case _, ok := <-confNtfn.Confirmed:
			if !ok {
				return fmt.Errorf("ChainNotifier shutting "+
					"down, cannot complete funding flow "+
					"for ChannelPoint(%v)",
					completeChan.FundingOutpoint)
			}
			// Fallthrough.

		case <-f.quit:
			return fmt.Errorf("%v, stopping funding flow for "+
				"ChannelPoint(%v)",
				ErrFundingManagerShuttingDown,
				completeChan.FundingOutpoint)
		}

		fundingPoint := completeChan.FundingOutpoint
		chanID := lnwire.NewChanIDFromOutPoint(&fundingPoint)

		log.Infof("Announcing ChannelPoint(%v), short_chan_id=%v",
			&fundingPoint, shortChanID)

		// If this is a non-zero-conf option-scid-alias channel, we'll
		// delete the mappings the gossiper uses so that ChannelUpdates
		// with aliases won't be accepted. This is done elsewhere for
		// zero-conf channels.
		isScidFeature := completeChan.NegotiatedAliasFeature()
		isZeroConf := completeChan.IsZeroConf()
		if isScidFeature && !isZeroConf {
			baseScid := completeChan.ShortChanID()
			err := f.cfg.AliasManager.DeleteSixConfs(baseScid)
			if err != nil {
				return fmt.Errorf("failed deleting six confs "+
					"maps: %v", err)
			}

			// We'll delete the edge and add it again via
			// addToRouterGraph. This is because the peer may have
			// sent us a ChannelUpdate with an alias and we don't
			// want to relay this.
			ourPolicy, err := f.cfg.DeleteAliasEdge(baseScid)
			if err != nil {
				return fmt.Errorf("failed deleting real edge "+
					"for alias channel from graph: %v",
					err)
			}

			err = f.addToRouterGraph(
				completeChan, &baseScid, nil, ourPolicy,
			)
			if err != nil {
				return fmt.Errorf("failed to re-add to "+
					"router graph: %v", err)
			}
		}

		// Create and broadcast the proofs required to make this channel
		// public and usable for other nodes for routing.
		err = f.announceChannel(
			f.cfg.IDKey, completeChan.IdentityPub,
			&completeChan.LocalChanCfg.MultiSigKey,
			completeChan.RemoteChanCfg.MultiSigKey.PubKey,
			*shortChanID, chanID,
		)
		if err != nil {
			return fmt.Errorf("channel announcement failed: %w",
				err)
		}

		log.Debugf("Channel with ChannelPoint(%v), short_chan_id=%v "+
			"sent to gossiper", &fundingPoint, shortChanID)
	}

	return nil
}

// waitForZeroConfChannel is called when the state is addedToRouterGraph with
// a zero-conf channel. This will wait for the real confirmation, add the
// confirmed SCID to the router graph, and then announce after six confs.
func (f *Manager) waitForZeroConfChannel(c *channeldb.OpenChannel,
	pendingID [32]byte) error {

	// First we'll check whether the channel is confirmed on-chain. If it
	// is already confirmed, the chainntnfs subsystem will return with the
	// confirmed tx. Otherwise, we'll wait here until confirmation occurs.
	confChan, err := f.waitForFundingWithTimeout(c)
	if err != nil {
		return fmt.Errorf("error waiting for zero-conf funding "+
			"confirmation for ChannelPoint(%v): %v",
			c.FundingOutpoint, err)
	}

	// We'll need to refresh the channel state so that things are properly
	// populated when validating the channel state. Otherwise, a panic may
	// occur due to inconsistency in the OpenChannel struct.
	err = c.Refresh()
	if err != nil {
		return fmt.Errorf("unable to refresh channel state: %v", err)
	}

	// Now that we have the confirmed transaction and the proper SCID,
	// we'll call ValidateChannel to ensure the confirmed tx is properly
	// formatted.
	err = f.cfg.Wallet.ValidateChannel(c, confChan.fundingTx)
	if err != nil {
		return fmt.Errorf("unable to validate zero-conf channel: "+
			"%v", err)
	}

	// Once we know the confirmed ShortChannelID, we'll need to save it to
	// the database and refresh the OpenChannel struct with it.
	err = c.MarkRealScid(confChan.shortChanID)
	if err != nil {
		return fmt.Errorf("unable to set confirmed SCID for zero "+
			"channel: %v", err)
	}

	// Six confirmations have been reached. If this channel is public,
	// we'll delete some of the alias mappings the gossiper uses.
	isPublic := c.ChannelFlags&lnwire.FFAnnounceChannel != 0
	if isPublic {
		err = f.cfg.AliasManager.DeleteSixConfs(c.ShortChannelID)
		if err != nil {
			return fmt.Errorf("unable to delete base alias after "+
				"six confirmations: %v", err)
		}

		// TODO: Make this atomic!
		ourPolicy, err := f.cfg.DeleteAliasEdge(c.ShortChanID())
		if err != nil {
			return fmt.Errorf("unable to delete alias edge from "+
				"graph: %v", err)
		}

		// We'll need to update the graph with the new ShortChannelID
		// via an addToRouterGraph call. We don't pass in the peer's
		// alias since we'll be using the confirmed SCID from now on
		// regardless if it's public or not.
		err = f.addToRouterGraph(
			c, &confChan.shortChanID, nil, ourPolicy,
		)
		if err != nil {
			return fmt.Errorf("failed adding confirmed zero-conf "+
				"SCID to router graph: %v", err)
		}
	}

	// Since we have now marked down the confirmed SCID, we'll also need to
	// tell the Switch to refresh the relevant ChannelLink so that forwards
	// under the confirmed SCID are possible if this is a public channel.
	err = f.cfg.ReportShortChanID(c.FundingOutpoint)
	if err != nil {
		// This should only fail if the link is not found in the
		// Switch's linkIndex map. If this is the case, then the peer
		// has gone offline and the next time the link is loaded, it
		// will have a refreshed state. Just log an error here.
		log.Errorf("unable to report scid for zero-conf channel "+
			"channel: %v", err)
	}

	// Update the confirmed transaction's label.
	f.makeLabelForTx(c)

	return nil
}

// handleChannelReady finalizes the channel funding process and enables the
// channel to enter normal operating mode.
func (f *Manager) handleChannelReady(peer lnpeer.Peer,
	msg *lnwire.ChannelReady) {

	defer f.wg.Done()
	log.Debugf("Received ChannelReady for ChannelID(%v) from "+
		"peer %x", msg.ChanID,
		peer.IdentityKey().SerializeCompressed())

	// If we are currently in the process of handling a funding locked
	// message for this channel, ignore.
	f.handleChannelReadyMtx.Lock()
	_, ok := f.handleChannelReadyBarriers[msg.ChanID]
	if ok {
		log.Infof("Already handling channelReady for "+
			"ChannelID(%v), ignoring.", msg.ChanID)
		f.handleChannelReadyMtx.Unlock()
		return
	}

	// If not already handling channelReady for this channel, set up
	// barrier, and move on.
	f.handleChannelReadyBarriers[msg.ChanID] = struct{}{}
	f.handleChannelReadyMtx.Unlock()

	defer func() {
		f.handleChannelReadyMtx.Lock()
		delete(f.handleChannelReadyBarriers, msg.ChanID)
		f.handleChannelReadyMtx.Unlock()
	}()

	f.localDiscoveryMtx.Lock()
	localDiscoverySignal, ok := f.localDiscoverySignals[msg.ChanID]
	f.localDiscoveryMtx.Unlock()

	if ok {
		// Before we proceed with processing the funding locked
		// message, we'll wait for the local waitForFundingConfirmation
		// goroutine to signal that it has the necessary state in
		// place. Otherwise, we may be missing critical information
		// required to handle forwarded HTLC's.
		select {
		case <-localDiscoverySignal:
			// Fallthrough
		case <-f.quit:
			return
		}

		// With the signal received, we can now safely delete the entry
		// from the map.
		f.localDiscoveryMtx.Lock()
		delete(f.localDiscoverySignals, msg.ChanID)
		f.localDiscoveryMtx.Unlock()
	}

	// First, we'll attempt to locate the channel whose funding workflow is
	// being finalized by this message. We go to the database rather than
	// our reservation map as we may have restarted, mid funding flow. Also
	// provide the node's public key to make the search faster.
	chanID := msg.ChanID
	channel, err := f.cfg.FindChannel(peer.IdentityKey(), chanID)
	if err != nil {
		log.Errorf("Unable to locate ChannelID(%v), cannot complete "+
			"funding", chanID)
		return
	}

	// We'll need to store the received TLV alias if the option_scid_alias
	// feature was negotiated. This will be used to provide route hints
	// during invoice creation. In the zero-conf case, it is also used to
	// provide a ChannelUpdate to the remote peer. This is done before the
	// call to InsertNextRevocation in case the call to PutPeerAlias fails.
	// If it were to fail on the first call to handleChannelReady, we
	// wouldn't want the channel to be usable yet.
	if channel.NegotiatedAliasFeature() {
		// If the AliasScid field is nil, we must fail out. We will
		// most likely not be able to route through the peer.
		if msg.AliasScid == nil {
			log.Debugf("Consider closing ChannelID(%v), peer "+
				"does not implement the option-scid-alias "+
				"feature properly", chanID)
			return
		}

		// We'll store the AliasScid so that invoice creation can use
		// it.
		err = f.cfg.AliasManager.PutPeerAlias(chanID, *msg.AliasScid)
		if err != nil {
			log.Errorf("unable to store peer's alias: %v", err)
			return
		}

		// If we do not have an alias stored, we'll create one now.
		// This is only used in the upgrade case where a user toggles
		// the option-scid-alias feature-bit to on. We'll also send the
		// channel_ready message here in case the link is created
		// before sendChannelReady is called.
		aliases := f.cfg.AliasManager.GetAliases(
			channel.ShortChannelID,
		)
		if len(aliases) == 0 {
			// No aliases were found so we'll request and store an
			// alias and use it in the channel_ready message.
			alias, err := f.cfg.AliasManager.RequestAlias()
			if err != nil {
				log.Errorf("unable to request alias: %v", err)
				return
			}

			err = f.cfg.AliasManager.AddLocalAlias(
				alias, channel.ShortChannelID, false,
			)
			if err != nil {
				log.Errorf("unable to add local alias: %v",
					err)
				return
			}

			secondPoint, err := channel.SecondCommitmentPoint()
			if err != nil {
				log.Errorf("unable to fetch second "+
					"commitment point: %v", err)
				return
			}

			channelReadyMsg := lnwire.NewChannelReady(
				chanID, secondPoint,
			)
			channelReadyMsg.AliasScid = &alias

			err = peer.SendMessage(true, channelReadyMsg)
			if err != nil {
				log.Errorf("unable to send funding locked: %v",
					err)
				return
			}
		}
	}

	// If the RemoteNextRevocation is non-nil, it means that we have
	// already processed channelReady for this channel, so ignore. This
	// check is after the alias logic so we store the peer's most recent
	// alias. The spec requires us to validate that subsequent
	// channel_ready messages use the same per commitment point (the
	// second), but it is not actually necessary since we'll just end up
	// ignoring it. We are, however, required to *send* the same per
	// commitment point, since another pedantic implementation might
	// verify it.
	if channel.RemoteNextRevocation != nil {
		log.Infof("Received duplicate channelReady for "+
			"ChannelID(%v), ignoring.", chanID)
		return
	}

	// The funding locked message contains the next commitment point we'll
	// need to create the next commitment state for the remote party. So
	// we'll insert that into the channel now before passing it along to
	// other sub-systems.
	err = channel.InsertNextRevocation(msg.NextPerCommitmentPoint)
	if err != nil {
		log.Errorf("unable to insert next commitment point: %v", err)
		return
	}

	// Launch a defer so we _ensure_ that the channel barrier is properly
	// closed even if the target peer is no longer online at this point.
	defer func() {
		// Close the active channel barrier signaling the readHandler
		// that commitment related modifications to this channel can
		// now proceed.
		f.barrierMtx.Lock()
		chanBarrier, ok := f.newChanBarriers[chanID]
		if ok {
			log.Tracef("Closing chan barrier for ChanID(%v)",
				chanID)
			close(chanBarrier)
			delete(f.newChanBarriers, chanID)
		}
		f.barrierMtx.Unlock()
	}()

	if err := peer.AddNewChannel(channel, f.quit); err != nil {
		log.Errorf("Unable to add new channel %v with peer %x: %v",
			channel.FundingOutpoint,
			peer.IdentityKey().SerializeCompressed(), err,
		)
	}
}

// chanAnnouncement encapsulates the two authenticated announcements that we
// send out to the network after a new channel has been created locally.
type chanAnnouncement struct {
	chanAnn       *lnwire.ChannelAnnouncement
	chanUpdateAnn *lnwire.ChannelUpdate
	chanProof     *lnwire.AnnounceSignatures
}

// newChanAnnouncement creates the authenticated channel announcement messages
// required to broadcast a newly created channel to the network. The
// announcement is two part: the first part authenticates the existence of the
// channel and contains four signatures binding the funding pub keys and
// identity pub keys of both parties to the channel, and the second segment is
// authenticated only by us and contains our directional routing policy for the
// channel. ourPolicy may be set in order to re-use an existing, non-default
// policy.
func (f *Manager) newChanAnnouncement(localPubKey,
	remotePubKey *btcec.PublicKey, localFundingKey *keychain.KeyDescriptor,
	remoteFundingKey *btcec.PublicKey, shortChanID lnwire.ShortChannelID,
	chanID lnwire.ChannelID, fwdMinHTLC,
	fwdMaxHTLC lnwire.MilliSatoshi,
	ourPolicy *channeldb.ChannelEdgePolicy) (*chanAnnouncement, error) {

	chainHash := *f.cfg.Wallet.Cfg.NetParams.GenesisHash

	// The unconditional section of the announcement is the ShortChannelID
	// itself which compactly encodes the location of the funding output
	// within the blockchain.
	chanAnn := &lnwire.ChannelAnnouncement{
		ShortChannelID: shortChanID,
		Features:       lnwire.NewRawFeatureVector(),
		ChainHash:      chainHash,
	}

	// The chanFlags field indicates which directed edge of the channel is
	// being updated within the ChannelUpdateAnnouncement announcement
	// below. A value of zero means it's the edge of the "first" node and 1
	// being the other node.
	var chanFlags lnwire.ChanUpdateChanFlags

	// The lexicographical ordering of the two identity public keys of the
	// nodes indicates which of the nodes is "first". If our serialized
	// identity key is lower than theirs then we're the "first" node and
	// second otherwise.
	selfBytes := localPubKey.SerializeCompressed()
	remoteBytes := remotePubKey.SerializeCompressed()
	if bytes.Compare(selfBytes, remoteBytes) == -1 {
		copy(chanAnn.NodeID1[:], localPubKey.SerializeCompressed())
		copy(chanAnn.NodeID2[:], remotePubKey.SerializeCompressed())
		copy(
			chanAnn.BitcoinKey1[:],
			localFundingKey.PubKey.SerializeCompressed(),
		)
		copy(
			chanAnn.BitcoinKey2[:],
			remoteFundingKey.SerializeCompressed(),
		)

		// If we're the first node then update the chanFlags to
		// indicate the "direction" of the update.
		chanFlags = 0
	} else {
		copy(chanAnn.NodeID1[:], remotePubKey.SerializeCompressed())
		copy(chanAnn.NodeID2[:], localPubKey.SerializeCompressed())
		copy(
			chanAnn.BitcoinKey1[:],
			remoteFundingKey.SerializeCompressed(),
		)
		copy(
			chanAnn.BitcoinKey2[:],
			localFundingKey.PubKey.SerializeCompressed(),
		)

		// If we're the second node then update the chanFlags to
		// indicate the "direction" of the update.
		chanFlags = 1
	}

	// Our channel update message flags will signal that we support the
	// max_htlc field.
	msgFlags := lnwire.ChanUpdateRequiredMaxHtlc

	// We announce the channel with the default values. Some of
	// these values can later be changed by crafting a new ChannelUpdate.
	chanUpdateAnn := &lnwire.ChannelUpdate{
		ShortChannelID: shortChanID,
		ChainHash:      chainHash,
		Timestamp:      uint32(time.Now().Unix()),
		MessageFlags:   msgFlags,
		ChannelFlags:   chanFlags,
		TimeLockDelta: uint16(
			f.cfg.DefaultRoutingPolicy.TimeLockDelta,
		),
		HtlcMinimumMsat: fwdMinHTLC,
		HtlcMaximumMsat: fwdMaxHTLC,
	}

	// The caller of newChanAnnouncement is expected to provide the initial
	// forwarding policy to be announced. If no persisted initial policy
	// values are found, then we will use the default policy values in the
	// channel announcement.
	storedFwdingPolicy, err := f.getInitialFwdingPolicy(chanID)
	if err != nil && !errors.Is(err, channeldb.ErrChannelNotFound) {
		return nil, errors.Errorf("unable to generate channel "+
			"update announcement: %v", err)
	}

	switch {
	case ourPolicy != nil:
		// If ourPolicy is non-nil, modify the default parameters of the
		// ChannelUpdate.
		chanUpdateAnn.MessageFlags = ourPolicy.MessageFlags
		chanUpdateAnn.ChannelFlags = ourPolicy.ChannelFlags
		chanUpdateAnn.TimeLockDelta = ourPolicy.TimeLockDelta
		chanUpdateAnn.HtlcMinimumMsat = ourPolicy.MinHTLC
		chanUpdateAnn.HtlcMaximumMsat = ourPolicy.MaxHTLC
		chanUpdateAnn.BaseFee = uint32(ourPolicy.FeeBaseMSat)
		chanUpdateAnn.FeeRate = uint32(
			ourPolicy.FeeProportionalMillionths,
		)

	case storedFwdingPolicy != nil:
		chanUpdateAnn.BaseFee = uint32(storedFwdingPolicy.BaseFee)
		chanUpdateAnn.FeeRate = uint32(storedFwdingPolicy.FeeRate)

	default:
		log.Infof("No channel forwarding policy specified for channel "+
			"announcement of ChannelID(%v). "+
			"Assuming default fee parameters.", chanID)
		chanUpdateAnn.BaseFee = uint32(
			f.cfg.DefaultRoutingPolicy.BaseFee,
		)
		chanUpdateAnn.FeeRate = uint32(
			f.cfg.DefaultRoutingPolicy.FeeRate,
		)
	}

	// With the channel update announcement constructed, we'll generate a
	// signature that signs a double-sha digest of the announcement.
	// This'll serve to authenticate this announcement and any other future
	// updates we may send.
	chanUpdateMsg, err := chanUpdateAnn.DataToSign()
	if err != nil {
		return nil, err
	}
	sig, err := f.cfg.SignMessage(f.cfg.IDKeyLoc, chanUpdateMsg, true)
	if err != nil {
		return nil, errors.Errorf("unable to generate channel "+
			"update announcement signature: %v", err)
	}
	chanUpdateAnn.Signature, err = lnwire.NewSigFromSignature(sig)
	if err != nil {
		return nil, errors.Errorf("unable to generate channel "+
			"update announcement signature: %v", err)
	}

	// The channel existence proofs itself is currently announced in
	// distinct message. In order to properly authenticate this message, we
	// need two signatures: one under the identity public key used which
	// signs the message itself and another signature of the identity
	// public key under the funding key itself.
	//
	// TODO(roasbeef): use SignAnnouncement here instead?
	chanAnnMsg, err := chanAnn.DataToSign()
	if err != nil {
		return nil, err
	}
	nodeSig, err := f.cfg.SignMessage(f.cfg.IDKeyLoc, chanAnnMsg, true)
	if err != nil {
		return nil, errors.Errorf("unable to generate node "+
			"signature for channel announcement: %v", err)
	}
	bitcoinSig, err := f.cfg.SignMessage(
		localFundingKey.KeyLocator, chanAnnMsg, true,
	)
	if err != nil {
		return nil, errors.Errorf("unable to generate bitcoin "+
			"signature for node public key: %v", err)
	}

	// Finally, we'll generate the announcement proof which we'll use to
	// provide the other side with the necessary signatures required to
	// allow them to reconstruct the full channel announcement.
	proof := &lnwire.AnnounceSignatures{
		ChannelID:      chanID,
		ShortChannelID: shortChanID,
	}
	proof.NodeSignature, err = lnwire.NewSigFromSignature(nodeSig)
	if err != nil {
		return nil, err
	}
	proof.BitcoinSignature, err = lnwire.NewSigFromSignature(bitcoinSig)
	if err != nil {
		return nil, err
	}

	return &chanAnnouncement{
		chanAnn:       chanAnn,
		chanUpdateAnn: chanUpdateAnn,
		chanProof:     proof,
	}, nil
}

// announceChannel announces a newly created channel to the rest of the network
// by crafting the two authenticated announcements required for the peers on
// the network to recognize the legitimacy of the channel. The crafted
// announcements are then sent to the channel router to handle broadcasting to
// the network during its next trickle.
// This method is synchronous and will return when all the network requests
// finish, either successfully or with an error.
func (f *Manager) announceChannel(localIDKey, remoteIDKey *btcec.PublicKey,
	localFundingKey *keychain.KeyDescriptor,
	remoteFundingKey *btcec.PublicKey, shortChanID lnwire.ShortChannelID,
	chanID lnwire.ChannelID) error {

	// First, we'll create the batch of announcements to be sent upon
	// initial channel creation. This includes the channel announcement
	// itself, the channel update announcement, and our half of the channel
	// proof needed to fully authenticate the channel.
	//
	// We can pass in zeroes for the min and max htlc policy, because we
	// only use the channel announcement message from the returned struct.
	ann, err := f.newChanAnnouncement(localIDKey, remoteIDKey,
		localFundingKey, remoteFundingKey, shortChanID, chanID,
		0, 0, nil,
	)
	if err != nil {
		log.Errorf("can't generate channel announcement: %v", err)
		return err
	}

	// After the fee parameters have been stored in the announcement
	// we can delete them from the database.
	err = f.deleteInitialFwdingPolicy(chanID)
	if err != nil {
		log.Infof("Could not delete channel fees for chanId %x.",
			chanID)
	}

	// We only send the channel proof announcement and the node announcement
	// because addToRouterGraph previously sent the ChannelAnnouncement and
	// the ChannelUpdate announcement messages. The channel proof and node
	// announcements are broadcast to the greater network.
	errChan := f.cfg.SendAnnouncement(ann.chanProof)
	select {
	case err := <-errChan:
		if err != nil {
			if routing.IsError(err, routing.ErrOutdated,
				routing.ErrIgnored) {

				log.Debugf("Router rejected "+
					"AnnounceSignatures: %v", err)
			} else {
				log.Errorf("Unable to send channel "+
					"proof: %v", err)
				return err
			}
		}

	case <-f.quit:
		return ErrFundingManagerShuttingDown
	}

	// Now that the channel is announced to the network, we will also
	// obtain and send a node announcement. This is done since a node
	// announcement is only accepted after a channel is known for that
	// particular node, and this might be our first channel.
	nodeAnn, err := f.cfg.CurrentNodeAnnouncement()
	if err != nil {
		log.Errorf("can't generate node announcement: %v", err)
		return err
	}

	errChan = f.cfg.SendAnnouncement(&nodeAnn)
	select {
	case err := <-errChan:
		if err != nil {
			if routing.IsError(err, routing.ErrOutdated,
				routing.ErrIgnored) {

				log.Debugf("Router rejected "+
					"NodeAnnouncement: %v", err)
			} else {
				log.Errorf("Unable to send node "+
					"announcement: %v", err)
				return err
			}
		}

	case <-f.quit:
		return ErrFundingManagerShuttingDown
	}

	return nil
}

// InitFundingWorkflow sends a message to the funding manager instructing it
// to initiate a single funder workflow with the source peer.
// TODO(roasbeef): re-visit blocking nature..
func (f *Manager) InitFundingWorkflow(msg *InitFundingMsg) {
	f.fundingRequests <- msg
}

// getUpfrontShutdownScript takes a user provided script and a getScript
// function which can be used to generate an upfront shutdown script. If our
// peer does not support the feature, this function will error if a non-zero
// script was provided by the user, and return an empty script otherwise. If
// our peer does support the feature, we will return the user provided script
// if non-zero, or a freshly generated script if our node is configured to set
// upfront shutdown scripts automatically.
func getUpfrontShutdownScript(enableUpfrontShutdown bool, peer lnpeer.Peer,
	script lnwire.DeliveryAddress,
	getScript func(bool) (lnwire.DeliveryAddress, error)) (lnwire.DeliveryAddress,
	error) {

	// Check whether the remote peer supports upfront shutdown scripts.
	remoteUpfrontShutdown := peer.RemoteFeatures().HasFeature(
		lnwire.UpfrontShutdownScriptOptional,
	)

	// If the peer does not support upfront shutdown scripts, and one has been
	// provided, return an error because the feature is not supported.
	if !remoteUpfrontShutdown && len(script) != 0 {
		return nil, errUpfrontShutdownScriptNotSupported
	}

	// If the peer does not support upfront shutdown, return an empty address.
	if !remoteUpfrontShutdown {
		return nil, nil
	}

	// If the user has provided an script and the peer supports the feature,
	// return it. Note that user set scripts override the enable upfront
	// shutdown flag.
	if len(script) > 0 {
		return script, nil
	}

	// If we do not have setting of upfront shutdown script enabled, return
	// an empty script.
	if !enableUpfrontShutdown {
		return nil, nil
	}

	// We can safely send a taproot address iff, both sides have negotiated
	// the shutdown-any-segwit feature.
	taprootOK := peer.RemoteFeatures().HasFeature(lnwire.ShutdownAnySegwitOptional) &&
		peer.LocalFeatures().HasFeature(lnwire.ShutdownAnySegwitOptional)

	return getScript(taprootOK)
}

// handleInitFundingMsg creates a channel reservation within the daemon's
// wallet, then sends a funding request to the remote peer kicking off the
// funding workflow.
func (f *Manager) handleInitFundingMsg(msg *InitFundingMsg) {
	var (
		peerKey        = msg.Peer.IdentityKey()
		localAmt       = msg.LocalFundingAmt
		baseFee        = msg.BaseFee
		feeRate        = msg.FeeRate
		minHtlcIn      = msg.MinHtlcIn
		remoteCsvDelay = msg.RemoteCsvDelay
		maxValue       = msg.MaxValueInFlight
		maxHtlcs       = msg.MaxHtlcs
		maxCSV         = msg.MaxLocalCsv
		chanReserve    = msg.RemoteChanReserve
	)

	// If no maximum CSV delay was set for this channel, we use our default
	// value.
	if maxCSV == 0 {
		maxCSV = f.cfg.MaxLocalCSVDelay
	}

	log.Infof("Initiating fundingRequest(local_amt=%v "+
		"(subtract_fees=%v), push_amt=%v, chain_hash=%v, peer=%x, "+
		"min_confs=%v)", localAmt, msg.SubtractFees, msg.PushAmt,
		msg.ChainHash, peerKey.SerializeCompressed(), msg.MinConfs)

	// We set the channel flags to indicate whether we want this channel to
	// be announced to the network.
	var channelFlags lnwire.FundingFlag
	if !msg.Private {
		// This channel will be announced.
		channelFlags = lnwire.FFAnnounceChannel
	}

	// If the caller specified their own channel ID, then we'll use that.
	// Otherwise we'll generate a fresh one as normal.  This will be used
	// to track this reservation throughout its lifetime.
	var chanID [32]byte
	if msg.PendingChanID == zeroID {
		chanID = f.nextPendingChanID()
	} else {
		// If the user specified their own pending channel ID, then
		// we'll ensure it doesn't collide with any existing pending
		// channel ID.
		chanID = msg.PendingChanID
		if _, err := f.getReservationCtx(peerKey, chanID); err == nil {
			msg.Err <- fmt.Errorf("pendingChannelID(%x) "+
				"already present", chanID[:])
			return
		}
	}

	// Check whether the peer supports upfront shutdown, and get an address
	// which should be used (either a user specified address or a new
	// address from the wallet if our node is configured to set shutdown
	// address by default).
	shutdown, err := getUpfrontShutdownScript(
		f.cfg.EnableUpfrontShutdown, msg.Peer, msg.ShutdownScript,
		f.selectShutdownScript,
	)
	if err != nil {
		msg.Err <- err
		return
	}

	// Initialize a funding reservation with the local wallet. If the
	// wallet doesn't have enough funds to commit to this channel, then the
	// request will fail, and be aborted.
	//
	// Before we init the channel, we'll also check to see what commitment
	// format we can use with this peer. This is dependent on *both* us and
	// the remote peer are signaling the proper feature bit.
	_, chanType, commitType, err := negotiateCommitmentType(
		msg.ChannelType, msg.Peer.LocalFeatures(),
		msg.Peer.RemoteFeatures(), true,
	)
	if err != nil {
		log.Errorf("channel type negotiation failed: %v", err)
		msg.Err <- err
		return
	}

	var (
		zeroConf bool
		scid     bool
	)

	// Check if the returned chanType includes either the zero-conf or
	// scid-alias bits.
	featureVec := lnwire.RawFeatureVector(*chanType)
	zeroConf = featureVec.IsSet(lnwire.ZeroConfRequired)
	scid = featureVec.IsSet(lnwire.ScidAliasRequired)

	// The option-scid-alias channel type for a public channel is
	// disallowed.
	if scid && !msg.Private {
		err = fmt.Errorf("option-scid-alias chantype for public " +
			"channel")
		log.Error(err)
		msg.Err <- err
		return
	}

	// First, we'll query the fee estimator for a fee that should get the
	// commitment transaction confirmed by the next few blocks (conf target
	// of 3). We target the near blocks here to ensure that we'll be able
	// to execute a timely unilateral channel closure if needed.
	commitFeePerKw, err := f.cfg.FeeEstimator.EstimateFeePerKW(3)
	if err != nil {
		msg.Err <- err
		return
	}

	// For anchor channels cap the initial commit fee rate at our defined
	// maximum.
	if commitType.HasAnchors() &&
		commitFeePerKw > f.cfg.MaxAnchorsCommitFeeRate {

		commitFeePerKw = f.cfg.MaxAnchorsCommitFeeRate
	}

	var scidFeatureVal bool
	if hasFeatures(
		msg.Peer.LocalFeatures(), msg.Peer.RemoteFeatures(),
		lnwire.ScidAliasOptional,
	) {

		scidFeatureVal = true
	}

	req := &lnwallet.InitFundingReserveMsg{
		ChainHash:         &msg.ChainHash,
		PendingChanID:     chanID,
		NodeID:            peerKey,
		NodeAddr:          msg.Peer.Address(),
		SubtractFees:      msg.SubtractFees,
		LocalFundingAmt:   localAmt,
		RemoteFundingAmt:  0,
		FundUpToMaxAmt:    msg.FundUpToMaxAmt,
		MinFundAmt:        msg.MinFundAmt,
		RemoteChanReserve: chanReserve,
		CommitFeePerKw:    commitFeePerKw,
		FundingFeePerKw:   msg.FundingFeePerKw,
		PushMSat:          msg.PushAmt,
		Flags:             channelFlags,
		MinConfs:          msg.MinConfs,
		CommitType:        commitType,
		ChanFunder:        msg.ChanFunder,
		ZeroConf:          zeroConf,
		OptionScidAlias:   scid,
		ScidAliasFeature:  scidFeatureVal,
	}

	reservation, err := f.cfg.Wallet.InitChannelReservation(req)
	if err != nil {
		msg.Err <- err
		return
	}

	if zeroConf {
		// Store the alias for zero-conf channels in the underlying
		// partial channel state.
		aliasScid, err := f.cfg.AliasManager.RequestAlias()
		if err != nil {
			msg.Err <- err
			return
		}

		reservation.AddAlias(aliasScid)
	}

	// Set our upfront shutdown address in the existing reservation.
	reservation.SetOurUpfrontShutdown(shutdown)

	// Now that we have successfully reserved funds for this channel in the
	// wallet, we can fetch the final channel capacity. This is done at
	// this point since the final capacity might change in case of
	// SubtractFees=true.
	capacity := reservation.Capacity()

	log.Infof("Target commit tx sat/kw for pendingID(%x): %v", chanID,
		int64(commitFeePerKw))

	// If the remote CSV delay was not set in the open channel request,
	// we'll use the RequiredRemoteDelay closure to compute the delay we
	// require given the total amount of funds within the channel.
	if remoteCsvDelay == 0 {
		remoteCsvDelay = f.cfg.RequiredRemoteDelay(capacity)
	}

	// If no minimum HTLC value was specified, use the default one.
	if minHtlcIn == 0 {
		minHtlcIn = f.cfg.DefaultMinHtlcIn
	}

	// If no max value was specified, use the default one.
	if maxValue == 0 {
		maxValue = f.cfg.RequiredRemoteMaxValue(capacity)
	}

	if maxHtlcs == 0 {
		maxHtlcs = f.cfg.RequiredRemoteMaxHTLCs(capacity)
	}

	// Prepare the optional channel fee values from the initFundingMsg.
	// If useBaseFee or useFeeRate are false the client did not
	// provide fee values hence we assume default fee settings from
	// the config.
	forwardingPolicy := htlcswitch.ForwardingPolicy{
		BaseFee: f.cfg.DefaultRoutingPolicy.BaseFee,
		FeeRate: f.cfg.DefaultRoutingPolicy.FeeRate,
	}
	if baseFee != nil {
		forwardingPolicy.BaseFee = lnwire.MilliSatoshi(*baseFee)
	}

	if feeRate != nil {
		forwardingPolicy.FeeRate = lnwire.MilliSatoshi(*feeRate)
	}

	// Once the reservation has been created, and indexed, queue a funding
	// request to the remote peer, kicking off the funding workflow.
	ourContribution := reservation.OurContribution()

	// Fetch our dust limit which is part of the default channel
	// constraints, and log it.
	ourDustLimit := ourContribution.DustLimit

	log.Infof("Dust limit for pendingID(%x): %v", chanID, ourDustLimit)

	// If the channel reserve is not specified, then we calculate an
	// appropriate amount here.
	if chanReserve == 0 {
		chanReserve = f.cfg.RequiredRemoteChanReserve(
			capacity, ourDustLimit,
		)
	}

	// If a pending channel map for this peer isn't already created, then
	// we create one, ultimately allowing us to track this pending
	// reservation within the target peer.
	peerIDKey := newSerializedKey(peerKey)
	f.resMtx.Lock()
	if _, ok := f.activeReservations[peerIDKey]; !ok {
		f.activeReservations[peerIDKey] = make(pendingChannels)
	}

	resCtx := &reservationWithCtx{
		chanAmt:           capacity,
		forwardingPolicy:  forwardingPolicy,
		remoteCsvDelay:    remoteCsvDelay,
		remoteMinHtlc:     minHtlcIn,
		remoteMaxValue:    maxValue,
		remoteMaxHtlcs:    maxHtlcs,
		remoteChanReserve: chanReserve,
		maxLocalCsv:       maxCSV,
		channelType:       msg.ChannelType,
		reservation:       reservation,
		peer:              msg.Peer,
		updates:           msg.Updates,
		err:               msg.Err,
	}
	f.activeReservations[peerIDKey][chanID] = resCtx
	f.resMtx.Unlock()

	// Update the timestamp once the InitFundingMsg has been handled.
	defer resCtx.updateTimestamp()

	// Check the sanity of the selected channel constraints.
	channelConstraints := &channeldb.ChannelConstraints{
		DustLimit:        ourDustLimit,
		ChanReserve:      chanReserve,
		MaxPendingAmount: maxValue,
		MinHTLC:          minHtlcIn,
		MaxAcceptedHtlcs: maxHtlcs,
		CsvDelay:         remoteCsvDelay,
	}
	err = lnwallet.VerifyConstraints(
		channelConstraints, resCtx.maxLocalCsv, capacity,
	)
	if err != nil {
		_, reserveErr := f.cancelReservationCtx(peerKey, chanID, false)
		if reserveErr != nil {
			log.Errorf("unable to cancel reservation: %v",
				reserveErr)
		}

		msg.Err <- err
		return
	}

	// When opening a script enforced channel lease, include the required
	// expiry TLV record in our proposal.
	var leaseExpiry *lnwire.LeaseExpiry
	if commitType == lnwallet.CommitmentTypeScriptEnforcedLease {
		leaseExpiry = new(lnwire.LeaseExpiry)
		*leaseExpiry = lnwire.LeaseExpiry(reservation.LeaseExpiry())
	}

	log.Infof("Starting funding workflow with %v for pending_id(%x), "+
		"committype=%v", msg.Peer.Address(), chanID, commitType)

	fundingOpen := lnwire.OpenChannel{
		ChainHash:             *f.cfg.Wallet.Cfg.NetParams.GenesisHash,
		PendingChannelID:      chanID,
		FundingAmount:         capacity,
		PushAmount:            msg.PushAmt,
		DustLimit:             ourDustLimit,
		MaxValueInFlight:      maxValue,
		ChannelReserve:        chanReserve,
		HtlcMinimum:           minHtlcIn,
		FeePerKiloWeight:      uint32(commitFeePerKw),
		CsvDelay:              remoteCsvDelay,
		MaxAcceptedHTLCs:      maxHtlcs,
		FundingKey:            ourContribution.MultiSigKey.PubKey,
		RevocationPoint:       ourContribution.RevocationBasePoint.PubKey,
		PaymentPoint:          ourContribution.PaymentBasePoint.PubKey,
		HtlcPoint:             ourContribution.HtlcBasePoint.PubKey,
		DelayedPaymentPoint:   ourContribution.DelayBasePoint.PubKey,
		FirstCommitmentPoint:  ourContribution.FirstCommitmentPoint,
		ChannelFlags:          channelFlags,
		UpfrontShutdownScript: shutdown,
		ChannelType:           chanType,
		LeaseExpiry:           leaseExpiry,
	}
	if err := msg.Peer.SendMessage(true, &fundingOpen); err != nil {
		e := fmt.Errorf("unable to send funding request message: %v",
			err)
		log.Errorf(e.Error())

		// Since we were unable to send the initial message to the peer
		// and start the funding flow, we'll cancel this reservation.
		_, err := f.cancelReservationCtx(peerKey, chanID, false)
		if err != nil {
			log.Errorf("unable to cancel reservation: %v", err)
		}

		msg.Err <- e
		return
	}
}

// handleWarningMsg processes the warning which was received from remote peer.
func (f *Manager) handleWarningMsg(peer lnpeer.Peer, msg *lnwire.Warning) {
	log.Warnf("received warning message from peer %x: %v",
		peer.IdentityKey().SerializeCompressed(), msg.Warning())
}

// handleErrorMsg processes the error which was received from remote peer,
// depending on the type of error we should do different clean up steps and
// inform the user about it.
func (f *Manager) handleErrorMsg(peer lnpeer.Peer, msg *lnwire.Error) {
	chanID := msg.ChanID
	peerKey := peer.IdentityKey()

	// First, we'll attempt to retrieve and cancel the funding workflow
	// that this error was tied to. If we're unable to do so, then we'll
	// exit early as this was an unwarranted error.
	resCtx, err := f.cancelReservationCtx(peerKey, chanID, true)
	if err != nil {
		log.Warnf("Received error for non-existent funding "+
			"flow: %v (%v)", err, msg.Error())
		return
	}

	// If we did indeed find the funding workflow, then we'll return the
	// error back to the caller (if any), and cancel the workflow itself.
	fundingErr := fmt.Errorf("received funding error from %x: %v",
		peerKey.SerializeCompressed(), msg.Error(),
	)
	log.Errorf(fundingErr.Error())

	// If this was a PSBT funding flow, the remote likely timed out because
	// we waited too long. Return a nice error message to the user in that
	// case so the user knows what's the problem.
	if resCtx.reservation.IsPsbt() {
		fundingErr = fmt.Errorf("%w: %v", chanfunding.ErrRemoteCanceled,
			fundingErr)
	}

	resCtx.err <- fundingErr
}

// pruneZombieReservations loops through all pending reservations and fails the
// funding flow for any reservations that have not been updated since the
// ReservationTimeout and are not locked waiting for the funding transaction.
func (f *Manager) pruneZombieReservations() {
	zombieReservations := make(pendingChannels)

	f.resMtx.RLock()
	for _, pendingReservations := range f.activeReservations {
		for pendingChanID, resCtx := range pendingReservations {
			if resCtx.isLocked() {
				continue
			}

			// We don't want to expire PSBT funding reservations.
			// These reservations are always initiated by us and the
			// remote peer is likely going to cancel them after some
			// idle time anyway. So no need for us to also prune
			// them.
			sinceLastUpdate := time.Since(resCtx.lastUpdated)
			isExpired := sinceLastUpdate > f.cfg.ReservationTimeout
			if !resCtx.reservation.IsPsbt() && isExpired {
				zombieReservations[pendingChanID] = resCtx
			}
		}
	}
	f.resMtx.RUnlock()

	for pendingChanID, resCtx := range zombieReservations {
		err := fmt.Errorf("reservation timed out waiting for peer "+
			"(peer_id:%x, chan_id:%x)",
			resCtx.peer.IdentityKey().SerializeCompressed(),
			pendingChanID[:])
		log.Warnf(err.Error())
		f.failFundingFlow(resCtx.peer, pendingChanID, err)
	}
}

// cancelReservationCtx does all needed work in order to securely cancel the
// reservation.
func (f *Manager) cancelReservationCtx(peerKey *btcec.PublicKey,
	pendingChanID [32]byte, byRemote bool) (*reservationWithCtx, error) {

	log.Infof("Cancelling funding reservation for node_key=%x, "+
		"chan_id=%x", peerKey.SerializeCompressed(), pendingChanID[:])

	peerIDKey := newSerializedKey(peerKey)
	f.resMtx.Lock()
	defer f.resMtx.Unlock()

	nodeReservations, ok := f.activeReservations[peerIDKey]
	if !ok {
		// No reservations for this node.
		return nil, errors.Errorf("no active reservations for peer(%x)",
			peerIDKey[:])
	}

	ctx, ok := nodeReservations[pendingChanID]
	if !ok {
		return nil, errors.Errorf("unknown channel (id: %x) for "+
			"peer(%x)", pendingChanID[:], peerIDKey[:])
	}

	// If the reservation was a PSBT funding flow and it was canceled by the
	// remote peer, then we need to thread through a different error message
	// to the subroutine that's waiting for the user input so it can return
	// a nice error message to the user.
	if ctx.reservation.IsPsbt() && byRemote {
		ctx.reservation.RemoteCanceled()
	}

	if err := ctx.reservation.Cancel(); err != nil {
		return nil, errors.Errorf("unable to cancel reservation: %v",
			err)
	}

	delete(nodeReservations, pendingChanID)

	// If this was the last active reservation for this peer, delete the
	// peer's entry altogether.
	if len(nodeReservations) == 0 {
		delete(f.activeReservations, peerIDKey)
	}
	return ctx, nil
}

// deleteReservationCtx deletes the reservation uniquely identified by the
// target public key of the peer, and the specified pending channel ID.
func (f *Manager) deleteReservationCtx(peerKey *btcec.PublicKey,
	pendingChanID [32]byte) {

	// TODO(roasbeef): possibly cancel funding barrier in peer's
	// channelManager?
	peerIDKey := newSerializedKey(peerKey)
	f.resMtx.Lock()
	defer f.resMtx.Unlock()

	nodeReservations, ok := f.activeReservations[peerIDKey]
	if !ok {
		// No reservations for this node.
		return
	}
	delete(nodeReservations, pendingChanID)

	// If this was the last active reservation for this peer, delete the
	// peer's entry altogether.
	if len(nodeReservations) == 0 {
		delete(f.activeReservations, peerIDKey)
	}
}

// getReservationCtx returns the reservation context for a particular pending
// channel ID for a target peer.
func (f *Manager) getReservationCtx(peerKey *btcec.PublicKey,
	pendingChanID [32]byte) (*reservationWithCtx, error) {

	peerIDKey := newSerializedKey(peerKey)
	f.resMtx.RLock()
	resCtx, ok := f.activeReservations[peerIDKey][pendingChanID]
	f.resMtx.RUnlock()

	if !ok {
		return nil, errors.Errorf("unknown channel (id: %x) for "+
			"peer(%x)", pendingChanID[:], peerIDKey[:])
	}

	return resCtx, nil
}

// IsPendingChannel returns a boolean indicating whether the channel identified
// by the pendingChanID and given peer is pending, meaning it is in the process
// of being funded. After the funding transaction has been confirmed, the
// channel will receive a new, permanent channel ID, and will no longer be
// considered pending.
func (f *Manager) IsPendingChannel(pendingChanID [32]byte,
	peer lnpeer.Peer) bool {

	peerIDKey := newSerializedKey(peer.IdentityKey())
	f.resMtx.RLock()
	_, ok := f.activeReservations[peerIDKey][pendingChanID]
	f.resMtx.RUnlock()

	return ok
}

func copyPubKey(pub *btcec.PublicKey) *btcec.PublicKey {
	var tmp btcec.JacobianPoint
	pub.AsJacobian(&tmp)
	tmp.ToAffine()
	return btcec.NewPublicKey(&tmp.X, &tmp.Y)
}

// saveInitialFwdingPolicy saves the forwarding policy for the provided
// chanPoint in the channelOpeningStateBucket.
func (f *Manager) saveInitialFwdingPolicy(permChanID lnwire.ChannelID,
	forwardingPolicy *htlcswitch.ForwardingPolicy) error {

	chanID := make([]byte, 32)
	copy(chanID, permChanID[:])

	scratch := make([]byte, 36)
	byteOrder.PutUint64(scratch[:8], uint64(forwardingPolicy.MinHTLCOut))
	byteOrder.PutUint64(scratch[8:16], uint64(forwardingPolicy.MaxHTLC))
	byteOrder.PutUint64(scratch[16:24], uint64(forwardingPolicy.BaseFee))
	byteOrder.PutUint64(scratch[24:32], uint64(forwardingPolicy.FeeRate))
	byteOrder.PutUint32(scratch[32:], forwardingPolicy.TimeLockDelta)

	return f.cfg.Wallet.Cfg.Database.SaveInitialFwdingPolicy(
		chanID, scratch,
	)
}

// getInitialFwdingPolicy fetches the initial forwarding policy for a given
// channel id from the database which will be applied during the channel
// announcement phase.
func (f *Manager) getInitialFwdingPolicy(permChanID lnwire.ChannelID) (
	*htlcswitch.ForwardingPolicy, error) {

	chanID := make([]byte, 32)
	copy(chanID, permChanID[:])

	value, err := f.cfg.Wallet.Cfg.Database.GetInitialFwdingPolicy(chanID)
	if err != nil {
		return nil, err
	}

	var fwdingPolicy htlcswitch.ForwardingPolicy
	fwdingPolicy.MinHTLCOut = lnwire.MilliSatoshi(
		byteOrder.Uint64(value[:8]),
	)
	fwdingPolicy.MaxHTLC = lnwire.MilliSatoshi(
		byteOrder.Uint64(value[8:16]),
	)
	fwdingPolicy.BaseFee = lnwire.MilliSatoshi(
		byteOrder.Uint64(value[16:24]),
	)
	fwdingPolicy.FeeRate = lnwire.MilliSatoshi(
		byteOrder.Uint64(value[24:32]),
	)
	fwdingPolicy.TimeLockDelta = byteOrder.Uint32(value[32:36])

	return &fwdingPolicy, nil
}

// deleteInitialFwdingPolicy removes channel fees for this chanID from
// the database.
func (f *Manager) deleteInitialFwdingPolicy(permChanID lnwire.ChannelID) error {
	chanID := make([]byte, 32)
	copy(chanID, permChanID[:])

	return f.cfg.Wallet.Cfg.Database.DeleteInitialFwdingPolicy(chanID)
}

// saveChannelOpeningState saves the channelOpeningState for the provided
// chanPoint to the channelOpeningStateBucket.
func (f *Manager) saveChannelOpeningState(chanPoint *wire.OutPoint,
	state channelOpeningState, shortChanID *lnwire.ShortChannelID) error {

	var outpointBytes bytes.Buffer
	if err := WriteOutpoint(&outpointBytes, chanPoint); err != nil {
		return err
	}

	// Save state and the uint64 representation of the shortChanID
	// for later use.
	scratch := make([]byte, 10)
	byteOrder.PutUint16(scratch[:2], uint16(state))
	byteOrder.PutUint64(scratch[2:], shortChanID.ToUint64())
	return f.cfg.Wallet.Cfg.Database.SaveChannelOpeningState(
		outpointBytes.Bytes(), scratch,
	)
}

// getChannelOpeningState fetches the channelOpeningState for the provided
// chanPoint from the database, or returns ErrChannelNotFound if the channel
// is not found.
func (f *Manager) getChannelOpeningState(chanPoint *wire.OutPoint) (
	channelOpeningState, *lnwire.ShortChannelID, error) {

	var outpointBytes bytes.Buffer
	if err := WriteOutpoint(&outpointBytes, chanPoint); err != nil {
		return 0, nil, err
	}

	value, err := f.cfg.Wallet.Cfg.Database.GetChannelOpeningState(
		outpointBytes.Bytes(),
	)
	if err != nil {
		return 0, nil, err
	}

	state := channelOpeningState(byteOrder.Uint16(value[:2]))
	shortChanID := lnwire.NewShortChanIDFromInt(byteOrder.Uint64(value[2:]))
	return state, &shortChanID, nil
}

// deleteChannelOpeningState removes any state for chanPoint from the database.
func (f *Manager) deleteChannelOpeningState(chanPoint *wire.OutPoint) error {
	var outpointBytes bytes.Buffer
	if err := WriteOutpoint(&outpointBytes, chanPoint); err != nil {
		return err
	}

	return f.cfg.Wallet.Cfg.Database.DeleteChannelOpeningState(
		outpointBytes.Bytes(),
	)
}

// selectShutdownScript selects the shutdown script we should send to the peer.
// If we can use taproot, then we prefer that, otherwise we'll use a p2wkh
// script.
func (f *Manager) selectShutdownScript(taprootOK bool,
) (lnwire.DeliveryAddress, error) {

	addrType := lnwallet.WitnessPubKey
	if taprootOK {
		addrType = lnwallet.TaprootPubkey
	}

	addr, err := f.cfg.Wallet.NewAddress(
		addrType, false, lnwallet.DefaultAccountName,
	)
	if err != nil {
		return nil, err
	}

	return txscript.PayToAddrScript(addr)
}

// waitForPeerOnline blocks until the peer specified by peerPubkey comes online
// and then returns the online peer.
func (f *Manager) waitForPeerOnline(peerPubkey *btcec.PublicKey) (lnpeer.Peer,
	error) {

	peerChan := make(chan lnpeer.Peer, 1)

	var peerKey [33]byte
	copy(peerKey[:], peerPubkey.SerializeCompressed())

	f.cfg.NotifyWhenOnline(peerKey, peerChan)

	var peer lnpeer.Peer
	select {
	case peer = <-peerChan:
	case <-f.quit:
		return peer, ErrFundingManagerShuttingDown
	}
	return peer, nil
}
