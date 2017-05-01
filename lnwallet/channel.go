package lnwallet

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/chaincfg/chainhash"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcutil/txsort"
)

var zeroHash chainhash.Hash

var (
	// ErrChanClosing is returned when a caller attempts to close a channel
	// that has already been closed or is in the process of being closed.
	ErrChanClosing = fmt.Errorf("channel is being closed, operation disallowed")

	// ErrNoWindow is returned when revocation window is exausted.
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
)

const (
	// InitialRevocationWindow is the number of revoked commitment
	// transactions allowed within the commitment chain. This value allows
	// a greater degree of de-synchronization by allowing either parties to
	// extend the other's commitment chain non-interactively, and also
	// serves as a flow control mechanism to a degree.
	InitialRevocationWindow = 1
)

// channelState is an enum like type which represents the current state of a
// particular channel.
// TODO(roasbeef): actually update state
type channelState uint8

const (
	// channelPending indicates this channel is still going through the
	// funding workflow, and isn't yet open.
	channelPending channelState = iota

	// channelOpen represents an open, active channel capable of
	// sending/receiving HTLCs.
	channelOpen

	// channelClosing represents a channel which is in the process of being
	// closed.
	channelClosing

	// channelClosed represents a channel which has been fully closed. Note
	// that before a channel can be closed, ALL pending HTLCs must be
	// settled/removed.
	channelClosed

	// channelDispute indicates that an un-cooperative closure has been
	// detected within the channel.
	channelDispute

	// channelPendingPayment indicates that there a currently outstanding
	// HTLCs within the channel.
	channelPendingPayment
)

// PaymentHash represents the sha256 of a random value. This hash is used to
// uniquely track incoming/outgoing payments within this channel, as well as
// payments requested by the wallet/daemon.
type PaymentHash [32]byte

const maxUint16 uint16 = ^uint16(0)

// UpdateType is the exact type of an entry within the shared HTLC log.
type updateType uint8

const (
	// Add is an update type that adds a new HTLC entry into the log.
	// Either side can add a new pending HTLC by adding a new Add entry
	// into their update log.
	Add updateType = iota

	// Fail is an update type which removes a prior HTLC entry from the
	// log. Adding a Fail entry to ones log will modify the _remote_
	// parties update log once a new commitment view has been evaluated
	// which contains the Fail entry.
	Fail

	// Settle is an update type which settles a prior HTLC crediting the
	// balance of the receiving node. Adding a Settle entry to a log will
	// result in the settle entry being removed on the log as well as the
	// original add entry from the remote party's log after the next state
	// transition.
	Settle
)

// PaymentDescriptor represents a commitment state update which either adds,
// settles, or removes an HTLC. PaymentDescriptors encapsulate all necessary
// metadata w.r.t to an HTLC, and additional data pairing a settle message to
// the original added HTLC.
// TODO(roasbeef): LogEntry interface??
//  * need to separate attrs for cancel/add/settle
type PaymentDescriptor struct {
	// RHash is the payment hash for this HTLC. The HTLC can be settled iff
	// the preimage to this hash is presented.
	RHash PaymentHash

	// RPreimage is the preimage that settles the HTLC pointed to wthin the
	// log by the ParentIndex.
	RPreimage PaymentHash

	// Timeout is the absolute timeout in blocks, after which this HTLC
	// expires.
	Timeout uint32

	// Amount is the HTLC amount in satoshis.
	Amount btcutil.Amount

	// Index is the log entry number that his HTLC update has within the
	// log. Depending on if IsIncoming is true, this is either an entry the
	// remote party added, or one that we added locally.
	Index uint64

	// ParentIndex is the index of the log entry that this HTLC update
	// settles or times out.
	ParentIndex uint64

	// addCommitHeight[Remote|Local] encodes the height of the commitment
	// which included this HTLC on either the remote or local commitment
	// chain. This value is used to determine when an HTLC is fully
	// "locked-in".
	addCommitHeightRemote uint64
	addCommitHeightLocal  uint64

	// removeCommitHeight[Remote|Local] encodes the height of the
	// commitment which removed the parent pointer of this
	// PaymentDescriptor either due to a timeout or a settle. Once both
	// these heights are above the tail of both chains, the log entries can
	// safely be removed.
	removeCommitHeightRemote uint64
	removeCommitHeightLocal  uint64

	// Payload is an opaque blob which is used to complete multi-hop
	// routing.
	Payload []byte

	// [our|their|]PkScript are the raw public key scripts that encodes the
	// redemption rules for this particular HTLC. These fields will only be
	// populated iff the EntryType of this PaymentDescriptor is Add.
	// ourPkScript is the ourPkScript from the context of our local
	// commitment chain. theirPkScript is the latest pkScript from the
	// context of the remote commitment chain.
	//
	// NOTE: These values may change within the logs themselves, however,
	// they'll stay consistent within the commitment chain entries
	// themselves.
	ourPkScript   []byte
	theirPkScript []byte

	// EntryType denotes the exact type of the PaymentDescriptor. In the
	// case of a Timeout, or Settle type, then the Parent field will point
	// into the log to the HTLC being modified.
	EntryType updateType

	// isDust[Local|Remote] denotes if this HTLC is below the dust limit in
	// locally or remotely.
	isDustLocal  bool
	isDustRemote bool

	// isForwarded denotes if an incoming HTLC has been forwarded to any
	// possible upstream peers in the route.
	isForwarded bool
}

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

	// [our|their]MessageIndex are indexes into the HTLC log, up to which
	// this commitment transaction includes. These indexes allow both sides
	// to independently, and concurrent send create new commitments. Each
	// new commitment sent to the remote party includes an index in the
	// shared log which details which of their updates we're including in
	// this new commitment.
	ourMessageIndex   uint64
	theirMessageIndex uint64

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
	ourBalance   btcutil.Amount
	theirBalance btcutil.Amount

	// fee is the amount that will be paid as fees for this commitment
	// transaction. The fee is recorded here so that it can be added
	// back and recalculated for each new update to the channel state.
	fee btcutil.Amount

	// htlcs is the set of HTLCs which remain unsettled within this
	// commitment.
	outgoingHTLCs []PaymentDescriptor
	incomingHTLCs []PaymentDescriptor
}

// toChannelDelta converts the target commitment into a format suitable to be
// written to disk after an accepted state transition.
// TODO(roasbeef): properly fill in refund timeouts
func (c *commitment) toChannelDelta(ourCommit bool) (*channeldb.ChannelDelta, error) {
	numHtlcs := len(c.outgoingHTLCs) + len(c.incomingHTLCs)

	// Save output indexes for RHash values found, so we don't return the
	// same output index more than once.
	dups := make(map[PaymentHash][]uint16)
	delta := &channeldb.ChannelDelta{
		LocalBalance:  c.ourBalance,
		RemoteBalance: c.theirBalance,
		UpdateNum:     c.height,
		CommitFee:     c.fee,
		Htlcs:         make([]*channeldb.HTLC, 0, numHtlcs),
	}

	// Check to see if element (e) exists in slice (s)
	contains := func(s []uint16, e uint16) bool {
		for _, a := range s {
			if a == e {
				return true
			}
		}
		return false
	}

	// As we also store the output index of the HTLC for continence
	// purposes, we create a small helper function to locate the output
	// index of a particular HTLC within the current commitment
	// transaction.
	locateOutputIndex := func(p *PaymentDescriptor) (uint16, error) {
		var (
			idx   uint16
			found bool
		)

		pkScript := p.theirPkScript
		if ourCommit {
			pkScript = p.ourPkScript
		}

		for i, txOut := range c.txn.TxOut {
			if bytes.Equal(txOut.PkScript, pkScript) &&
				txOut.Value == int64(p.Amount) {
				if contains(dups[p.RHash], uint16(i)) {
					continue
				}
				found = true
				idx = uint16(i)
				dups[p.RHash] = append(dups[p.RHash], idx)
				break
			}
		}
		if !found {
			return 0, fmt.Errorf("unable to find htlc: script=%x, value=%v",
				pkScript, p.Amount)
		}

		return idx, nil
	}

	var (
		index uint16
		err   error
	)
	for _, htlc := range c.outgoingHTLCs {
		if (ourCommit && htlc.isDustLocal) ||
			(!ourCommit && htlc.isDustRemote) {
			index = maxUint16
		} else if index, err = locateOutputIndex(&htlc); err != nil {
			return nil, fmt.Errorf("unable to find outgoing htlc: %v", err)
		}

		h := &channeldb.HTLC{
			Incoming:        false,
			Amt:             htlc.Amount,
			RHash:           htlc.RHash,
			RefundTimeout:   htlc.Timeout,
			RevocationDelay: 0,
			OutputIndex:     index,
		}
		delta.Htlcs = append(delta.Htlcs, h)
	}

	for _, htlc := range c.incomingHTLCs {
		if (ourCommit && htlc.isDustLocal) ||
			(!ourCommit && htlc.isDustRemote) {
			index = maxUint16
		} else if index, err = locateOutputIndex(&htlc); err != nil {
			return nil, fmt.Errorf("unable to find incoming htlc: %v", err)
		}

		h := &channeldb.HTLC{
			Incoming:        true,
			Amt:             htlc.Amount,
			RHash:           htlc.RHash,
			RefundTimeout:   htlc.Timeout,
			RevocationDelay: 0,
			OutputIndex:     index,
		}
		delta.Htlcs = append(delta.Htlcs, h)
	}

	return delta, nil
}

// commitmentChain represents a chain of unrevoked commitments. The tail of the
// chain is the latest fully signed, yet unrevoked commitment. Two chains are
// tracked, one for the local node, and another for the remote node. New
// commitments we create locally extend the remote node's chain, and vice
// versa. Commitment chains are allowed to grow to a bounded length, after
// which the tail needs to be "dropped" before new commitments can be received.
// The tail is "dropped" when the owner of the chain sends a revocation for the
// previous tail.
type commitmentChain struct {
	// commitments is a linked list of commitments to new states. New
	// commitments are added to the end of the chain with increase height.
	// Once a commitment transaction is revoked, the tail is incremented,
	// freeing up the revocation window for new commitments.
	commitments *list.List

	// startingHeight is the starting height of this commitment chain on a
	// session basis.
	startingHeight uint64
}

// newCommitmentChain creates a new commitment chain from an initial height.
func newCommitmentChain(initialHeight uint64) *commitmentChain {
	return &commitmentChain{
		commitments:    list.New(),
		startingHeight: initialHeight,
	}
}

// addCommitment extends the commitment chain by a single commitment. This
// added commitment represents a state update propsed by either party. Once the
// commitment prior to this commitment is revoked, the commitment becomes the
// new defacto state within the channel.
func (s *commitmentChain) addCommitment(c *commitment) {
	s.commitments.PushBack(c)
}

// advanceTail reduces the length of the commitment chain by one. The tail of
// the chain should be advanced once a revocation for the lowest unrevoked
// commitment in the chain is received.
func (s *commitmentChain) advanceTail() {
	s.commitments.Remove(s.commitments.Front())
}

// tip returns the latest commitment added to the chain.
func (s *commitmentChain) tip() *commitment {
	return s.commitments.Back().Value.(*commitment)
}

// tail returns the lowest unrevoked commitment transaction in the chain.
func (s *commitmentChain) tail() *commitment {
	return s.commitments.Front().Value.(*commitment)
}

// updateLog is an append-only log that stores updates to a node's commitment
// chain. This structure can be seen as the "mempool" within Lightning where
// changes are stored before they're committed to the chain. Once an entry has
// been committed in both the local and remote commitment chain, then it can be
// removed from this log.
//
// TODO(roasbeef): create lightning package, move commitment and update to
// package?
//  * also move state machine, separate from lnwallet package
//  * possible embed updateLog within commitmentChain.
type updateLog struct {
	// logIndex is a monotonically increasing integer that tracks the total
	// number of update entries ever applied to the log. When sending new
	// commitment states, we include all updates up to this index.
	logIndex uint64

	// ackIndex is a special "pointer" index into the log that tracks the
	// position which, up to, all changes have been ACK'd by the remote
	// party.  When receiving new commitment states, we include all of our
	// updates up to this index to restore the commitment view.
	ackedIndex uint64

	// pendingACKIndex is another special "pointer" index into the log that
	// tracks our logIndex value right before we extend the remote party's
	// commitment chain. Once we receive an ACK for this changes, then we
	// set ackedIndex=pendingAckIndex.
	//
	// TODO(roasbeef): eventually expand into list when we go back to a
	// sliding window format
	pendingAckIndex uint64

	// List is the updatelog itself, we embed this value so updateLog has
	// access to all the method of a list.List.
	*list.List

	// updateIndex is an index that maps a particular entries index to the
	// list element within the list.List above.
	updateIndex map[uint64]*list.Element
}

// newUpdateLog creates a new updateLog instance.
func newUpdateLog() *updateLog {
	return &updateLog{
		List:        list.New(),
		updateIndex: make(map[uint64]*list.Element),
	}
}

// appendUpdate appends a new update to the tip of the updateLog. The entry is
// also added to index accordingly.
func (u *updateLog) appendUpdate(pd *PaymentDescriptor) {
	u.updateIndex[u.logIndex] = u.PushBack(pd)
	u.logIndex++
}

// lookup attempts to look up an update entry according to it's index value. In
// the case that the entry isn't found, a nil pointer is returned.
func (u *updateLog) lookup(i uint64) *PaymentDescriptor {
	return u.updateIndex[i].Value.(*PaymentDescriptor)
}

// remove attempts to remove an entry from the update log. If the entry is
// found, then the entry will be removed from the update log and index.
func (u *updateLog) remove(i uint64) {
	entry := u.updateIndex[i]
	u.Remove(entry)
	delete(u.updateIndex, i)
}

// initiateTransition marks that the caller has extended the commitment chain
// of the remote party with the contents of the updateLog. This function will
// mark the log index value at this point so it can later be marked as ACK'd.
func (u *updateLog) initiateTransition() {
	u.pendingAckIndex = u.logIndex
}

// ackTransition updates the internal indexes of the updateLog to mark that the
// last pending state transition has been accepted by the remote party. To do
// so, we mark the prior pendingAckIndex as fully ACK'd.
func (u *updateLog) ackTransition() {
	u.ackedIndex = u.pendingAckIndex
	u.pendingAckIndex = 0
}

// compactLogs performs garbage collection within the log removing HTLCs which
// have been removed from the point-of-view of the tail of both chains. The
// entries which timeout/settle HTLCs are also removed.
func compactLogs(ourLog, theirLog *updateLog,
	localChainTail, remoteChainTail uint64) {

	compactLog := func(logA, logB *updateLog) {
		var nextA *list.Element
		for e := logA.Front(); e != nil; e = nextA {
			nextA = e.Next()

			htlc := e.Value.(*PaymentDescriptor)
			if htlc.EntryType == Add {
				continue
			}

			// If the HTLC hasn't yet been removed from either
			// chain, the skip it.
			if htlc.removeCommitHeightRemote == 0 ||
				htlc.removeCommitHeightLocal == 0 {
				continue
			}

			// Otherwise if the height of the tail of both chains
			// is at least the height in which the HTLC was
			// removed, then evict the settle/timeout entry along
			// with the original add entry.
			if remoteChainTail >= htlc.removeCommitHeightRemote &&
				localChainTail >= htlc.removeCommitHeightLocal {

				logA.remove(htlc.Index)
				logB.remove(htlc.ParentIndex)
			}

		}
	}

	compactLog(ourLog, theirLog)

	compactLog(theirLog, ourLog)
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
// Ths method .ExtendRevocationWindow() is used to extend the revocation window
// by a single revocation.
//
// The state machine has for main methods:
//  * .SignNextCommitment()
//    * Called one one wishes to sign the next commitment, either initiating a
//      new state update, or responding to a received commitment.
//  * .ReceiveNewCommitment()
//    * Called upon receipt of a new commitment from the remote party. If the
//      new commitment is valid, then a revocation should immediately be
//      generated and sent.
//  * .RevokeCurrentCommitment()
//    * Revokes the current commitment. Should be called directly after
//      receiving a new commitment.
//  * .ReceiveRevocation()
//   * Processes a revocation from the remote party. If successful creates a
//     new defacto broadcastable state.
//
// See the individual comments within the above methods for further details.
type LightningChannel struct {
	signer   Signer
	signDesc *SignDescriptor

	channelEvents chainntnfs.ChainNotifier

	sync.RWMutex

	pendingACK bool

	status channelState

	// feeEstimator is used to calculate the fee rate for the channel's
	// commitment and cooperative close transactions.
	feeEstimator FeeEstimator

	// Capcity is the total capacity of this channel.
	Capacity btcutil.Amount

	// currentHeight is the current height of our local commitment chain.
	// This is also the same as the number of updates to the channel we've
	// accepted.
	currentHeight uint64

	// revocationWindowEdge is the edge of the current revocation window.
	// New revocations for prior states created by this channel extend the
	// edge of this revocation window. The existence of a revocation window
	// allows the remote party to initiate new state updates independently
	// until the window is exhausted.
	revocationWindowEdge uint64

	// usedRevocations is a slice of revocations given to us by the remote
	// party that we've used. This slice is extended each time we create a
	// new commitment. The front of the slice is popped off once we receive
	// a revocation for a prior state. This head element then becomes the
	// next set of keys/hashes we expect to be revoked.
	usedRevocations []*lnwire.RevokeAndAck

	// revocationWindow is a window of revocations sent to use by the
	// remote party, allowing us to create new commitment transactions
	// until depleted. The revocations don't contain a valid preimage,
	// only an additional key/hash allowing us to create a new commitment
	// transaction for the remote node that they are able to revoke. If
	// this slice is empty, then we cannot make any new updates to their
	// commitment chain.
	revocationWindow []*lnwire.RevokeAndAck

	// remoteCommitChain is the remote node's commitment chain. Any new
	// commitments we initiate are added to the tip of this chain.
	remoteCommitChain *commitmentChain

	// localCommitChain is our local commitment chain. Any new commitments
	// received are added to the tip of this chain. The tail (or lowest
	// height) in this chain is our current accepted state, which we are
	// able to broadcast safely.
	localCommitChain *commitmentChain

	// stateMtx protects concurrent access to the state struct.
	stateMtx     sync.RWMutex
	channelState *channeldb.OpenChannel

	// [local|remote]Log is a (mostly) append-only log storing all the HTLC
	// updates to this channel. The log is walked backwards as HTLC updates
	// are applied in order to re-construct a commitment transaction from a
	// commitment. The log is compacted once a revocation is received.
	localUpdateLog  *updateLog
	remoteUpdateLog *updateLog

	// rHashMap is a map with PaymentHashes pointing to their respective
	// PaymentDescriptors. We insert *PaymentDescriptors whenever we
	// receive HTLCs. When a state transition happens (settling or
	// canceling the HTLC), rHashMap will provide an efficient
	// way to lookup the original PaymentDescriptor.
	rHashMap map[PaymentHash][]*PaymentDescriptor

	LocalDeliveryScript  []byte
	RemoteDeliveryScript []byte

	// FundingWitnessScript is the witness script for the 2-of-2 multi-sig
	// that opened the channel.
	FundingWitnessScript []byte

	fundingTxIn  *wire.TxIn
	fundingP2WSH []byte

	// ForceCloseSignal is a channel that is closed to indicate that a
	// local system has initiated a force close by broadcasting the current
	// commitment transaction directly on-chain.
	ForceCloseSignal chan struct{}

	// UnilateralCloseSignal is a channel that is closed to indicate that
	// the remote party has performed a unilateral close by broadcasting
	// their version of the commitment transaction on-chain.
	UnilateralCloseSignal chan struct{}

	// UnilateralClose is a channel that will be sent upon by the close
	// observer once the unilateral close of a channel is detected.
	UnilateralClose chan *UnilateralCloseSummary

	// ContractBreach is a channel that is used to communicate the data
	// necessary to fully resolve the channel in the case that a contract
	// breach is detected. A contract breach occurs it is detected that the
	// counterparty has broadcast a prior *revoked* state.
	ContractBreach chan *BreachRetribution

	// LocalFundingKey is the public key under control by the wallet that
	// was used for the 2-of-2 funding output which created this channel.
	LocalFundingKey *btcec.PublicKey

	// RemoteFundingKey is the public key for the remote channel counter
	// party  which used for the 2-of-2 funding output which created this
	// channel.
	RemoteFundingKey *btcec.PublicKey

	shutdown int32
	quit     chan struct{}
}

// NewLightningChannel creates a new, active payment channel given an
// implementation of the chain notifier, channel database, and the current
// settled channel state. Throughout state transitions, then channel will
// automatically persist pertinent state to the database in an efficient
// manner.
func NewLightningChannel(signer Signer, events chainntnfs.ChainNotifier,
	fe FeeEstimator, state *channeldb.OpenChannel) (*LightningChannel,
	error) {

	lc := &LightningChannel{
		signer:                signer,
		channelEvents:         events,
		feeEstimator:          fe,
		currentHeight:         state.NumUpdates,
		remoteCommitChain:     newCommitmentChain(state.NumUpdates),
		localCommitChain:      newCommitmentChain(state.NumUpdates),
		channelState:          state,
		revocationWindowEdge:  state.NumUpdates,
		localUpdateLog:        newUpdateLog(),
		remoteUpdateLog:       newUpdateLog(),
		rHashMap:              make(map[PaymentHash][]*PaymentDescriptor),
		Capacity:              state.Capacity,
		LocalDeliveryScript:   state.OurDeliveryScript,
		RemoteDeliveryScript:  state.TheirDeliveryScript,
		FundingWitnessScript:  state.FundingWitnessScript,
		ForceCloseSignal:      make(chan struct{}),
		UnilateralClose:       make(chan *UnilateralCloseSummary, 1),
		UnilateralCloseSignal: make(chan struct{}),
		ContractBreach:        make(chan *BreachRetribution, 1),
		LocalFundingKey:       state.OurMultiSigKey,
		RemoteFundingKey:      state.TheirMultiSigKey,
		quit:                  make(chan struct{}),
	}

	// Initialize both of our chains using current un-revoked commitment
	// for each side.
	lc.localCommitChain.addCommitment(&commitment{
		height:            lc.currentHeight,
		ourBalance:        state.OurBalance,
		ourMessageIndex:   0,
		theirBalance:      state.TheirBalance,
		theirMessageIndex: 0,
	})
	walletLog.Debugf("ChannelPoint(%v), starting local commitment: %v",
		state.ChanID, newLogClosure(func() string {
			return spew.Sdump(lc.localCommitChain.tail())
		}),
	)

	// To obtain the proper height for the remote node's commitment state,
	// we'll need to fetch the tail end of their revocation log from the
	// database.
	logTail, err := state.RevocationLogTail()
	if err != nil && err != channeldb.ErrNoActiveChannels &&
		err != channeldb.ErrNoPastDeltas {
		return nil, err
	}
	remoteCommitment := &commitment{
		ourBalance:        state.OurBalance,
		ourMessageIndex:   0,
		theirBalance:      state.TheirBalance,
		theirMessageIndex: 0,
	}
	if logTail == nil {
		remoteCommitment.height = 0
	} else {
		remoteCommitment.height = logTail.UpdateNum + 1
	}
	lc.remoteCommitChain.addCommitment(remoteCommitment)
	walletLog.Debugf("ChannelPoint(%v), starting remote commitment: %v",
		state.ChanID, newLogClosure(func() string {
			return spew.Sdump(lc.remoteCommitChain.tail())
		}),
	)

	// If we're restarting from a channel with history, then restore the
	// update in-memory update logs to that of the prior state.
	if lc.currentHeight != 0 {
		lc.restoreStateLogs()
	}

	// Create the sign descriptor which we'll be using very frequently to
	// request a signature for the 2-of-2 multi-sig from the signer in
	// order to complete channel state transitions.
	fundingPkScript, err := witnessScriptHash(state.FundingWitnessScript)
	if err != nil {
		return nil, err
	}
	lc.fundingTxIn = wire.NewTxIn(state.FundingOutpoint, nil, nil)
	lc.fundingP2WSH = fundingPkScript
	lc.signDesc = &SignDescriptor{
		PubKey:        lc.channelState.OurMultiSigKey,
		WitnessScript: lc.channelState.FundingWitnessScript,
		Output: &wire.TxOut{
			PkScript: lc.fundingP2WSH,
			Value:    int64(lc.channelState.Capacity),
		},
		HashType:   txscript.SigHashAll,
		InputIndex: 0,
	}

	// We'll only launch a close observer if the ChainNotifier
	// implementation is non-nil. Passing a nil value indicates that the
	// channel shouldn't be actively watched for.
	if lc.channelEvents != nil {
		// Register for a notification to be dispatched if the funding
		// outpoint has been spent. This indicates that either us or
		// the remote party has broadcasted a commitment transaction
		// on-chain.
		fundingOut := &lc.fundingTxIn.PreviousOutPoint
		openHeight := lc.channelState.OpeningHeight
		channelCloseNtfn, err := lc.channelEvents.RegisterSpendNtfn(fundingOut, openHeight)
		if err != nil {
			return nil, err
		}

		// Launch the close observer which will vigilantly watch the
		// network for any broadcasts the current or prior commitment
		// transactions, taking action accordingly.
		go lc.closeObserver(channelCloseNtfn)
	}

	return lc, nil
}

// BreachRetribution contains all the data necessary to bring a channel
// counterparty to justice claiming ALL lingering funds within the channel in
// the scenario that they broadcast a revoked commitment transaction. A
// BreachRetribution is created by the closeObserver if it detects an
// uncooperative close of the channel which uses a revoked commitment
// transaction. The BreachRetribution is then sent over the ContractBreach
// channel in order to allow the subscriber of the channel to dispatch justice.
type BreachRetribution struct {
	// BreachTransaction is the transaction which breached the channel
	// contract by spending from the funding multi-sig with a revoked
	// commitment transaction.
	BreachTransaction *wire.MsgTx

	// RevokedStateNum is the revoked state number which was broadcast.
	RevokedStateNum uint64

	// PendingHTLCs is a slice of the HTLCs which were pending at this
	// point within the channel's history transcript.
	PendingHTLCs []*channeldb.HTLC

	// LocalOutputSignDesc is a SignDescriptor which is capable of
	// generating the signature necessary to sweep the output within the
	// BreachTransaction that pays directly us.
	LocalOutputSignDesc *SignDescriptor

	// LocalOutpoint is the outpoint of the output paying to us (the local
	// party) within the breach transaction.
	LocalOutpoint wire.OutPoint

	// RemoteOutputSignDesc is a SignDescriptor which is capable of
	// generating the signature required to claim the funds as described
	// within the revocation clause of the remote party's commitment
	// output.
	RemoteOutputSignDesc *SignDescriptor

	// RemoteOutpoint is the output of the output paying to the remote
	// party within the breach transaction.
	RemoteOutpoint wire.OutPoint
}

// newBreachRetribution creates a new fully populated BreachRetribution for the
// passed channel, at a particular revoked state number, and one which targets
// the passed commitment transaction.
func newBreachRetribution(chanState *channeldb.OpenChannel, stateNum uint64,
	broadcastCommitment *wire.MsgTx) (*BreachRetribution, error) {

	commitHash := broadcastCommitment.TxHash()

	// Query the on-disk revocation log for the snapshot which was recorded
	// at this particular state num.
	revokedSnapshot, err := chanState.FindPreviousState(stateNum)
	if err != nil {
		return nil, err
	}

	// With the state number broadcast known, we can now derive/restore the
	// proper revocation preimage necessary to sweep the remote party's
	// output.
	revocationPreimage, err := chanState.RevocationStore.LookUp(stateNum)
	if err != nil {
		return nil, err
	}

	// Once we derive the revocation leaf, we can then re-create the
	// revocation public key used within this state. This is needed in
	// order to create the proper script below.
	localCommitKey := chanState.OurCommitKey
	revocationKey := DeriveRevocationPubkey(localCommitKey, revocationPreimage[:])

	remoteCommitkey := chanState.TheirCommitKey
	remoteDelay := chanState.RemoteCsvDelay

	// Next, reconstruct the scripts as they were present at this state
	// number so we can have the proper witness script to sign and include
	// within the final witness.
	remotePkScript, err := commitScriptToSelf(remoteDelay,
		remoteCommitkey, revocationKey)
	if err != nil {
		return nil, err
	}
	remoteWitnessHash, err := witnessScriptHash(remotePkScript)
	if err != nil {
		return nil, err
	}
	localPkScript, err := commitScriptUnencumbered(localCommitKey)
	if err != nil {
		return nil, err
	}

	// In order to fully populate the breach retribution struct, we'll need
	// to find the exact index of the local+remote commitment outputs.
	localOutpoint := wire.OutPoint{
		Hash: commitHash,
	}
	remoteOutpoint := wire.OutPoint{
		Hash: commitHash,
	}
	for i, txOut := range broadcastCommitment.TxOut {
		switch {
		case bytes.Equal(txOut.PkScript, localPkScript):
			localOutpoint.Index = uint32(i)
		case bytes.Equal(txOut.PkScript, remoteWitnessHash):
			remoteOutpoint.Index = uint32(i)
		}
	}

	// Finally, with all the necessary data constructed, we can create the
	// BreachRetribution struct which houses all the data necessary to
	// swiftly bring justice to the cheating remote party.
	return &BreachRetribution{
		BreachTransaction: broadcastCommitment,
		RevokedStateNum:   stateNum,
		PendingHTLCs:      revokedSnapshot.Htlcs,
		LocalOutpoint:     localOutpoint,
		LocalOutputSignDesc: &SignDescriptor{
			PubKey: localCommitKey,
			Output: &wire.TxOut{
				PkScript: localPkScript,
				Value:    int64(revokedSnapshot.LocalBalance),
			},
			HashType: txscript.SigHashAll,
		},
		RemoteOutpoint: remoteOutpoint,
		RemoteOutputSignDesc: &SignDescriptor{
			PubKey:        localCommitKey,
			PrivateTweak:  revocationPreimage[:],
			WitnessScript: remotePkScript,
			Output: &wire.TxOut{
				PkScript: remoteWitnessHash,
				Value:    int64(revokedSnapshot.RemoteBalance),
			},
			HashType: txscript.SigHashAll,
		},
	}, nil
}

// closeObserver is a goroutine which watches the network for any spends of the
// multi-sig funding output. A spend from the multi-sig output may occur under
// the following three scenarios: a cooperative close, a unilateral close, and
// a uncooperative contract breaching close. In the case of the last scenario a
// BreachRetribution struct is created and sent over the ContractBreach channel
// notifying subscribers that the counterparty has violated the condition of
// the channel by broadcasting a revoked prior state.
//
// NOTE: This MUST be run as a goroutine.
func (lc *LightningChannel) closeObserver(channelCloseNtfn *chainntnfs.SpendEvent) {
	walletLog.Infof("Close observer for ChannelPoint(%v) active",
		lc.channelState.ChanID)

	var (
		commitSpend *chainntnfs.SpendDetail
		ok          bool
	)

	select {
	// If the daemon is shutting down, then this notification channel will
	// be closed, so check the second read-value to avoid a false positive.
	case commitSpend, ok = <-channelCloseNtfn.Spend:
		if !ok {
			return
		}

	// Otherwise, we've beeen signalled to bail out early by the
	// caller/maintainer of this channel.
	case <-lc.quit:
		// As we're exiting before the spend notification has been
		// triggered, we'll cancel the notification intent so the
		// ChainNotiifer can free up the resources.
		channelCloseNtfn.Cancel()
		return
	}

	// If we've already initiated a local cooperative or unilateral close
	// locally, then we have nothing more to do.
	lc.RLock()
	if lc.status == channelClosed || lc.status == channelDispute ||
		lc.status == channelClosing {

		lc.RUnlock()
		return
	}
	lc.RUnlock()

	lc.Lock()
	defer lc.Unlock()

	walletLog.Warnf("Unprompted commitment broadcast for ChannelPoint(%v) "+
		"detected!", lc.channelState.ChanID)

	// Otherwise, the remote party might have broadcast a prior revoked
	// state...!!!
	commitTxBroadcast := commitSpend.SpendingTx

	// If this is our commitment transaction, then we can exit here as we
	// don't have any further processing we need to do (we can't cheat
	// ourselves :p).
	commitmentHash := lc.channelState.OurCommitTx.TxHash()
	isOurCommitment := commitSpend.SpenderTxHash.IsEqual(&commitmentHash)
	if isOurCommitment {
		return
	}

	// Decode the state hint encoded within the commitment transaction to
	// determine if this is a revoked state or not.
	obsfucator := lc.channelState.StateHintObsfucator
	broadcastStateNum := GetStateNumHint(commitTxBroadcast, obsfucator)

	currentStateNum := lc.currentHeight

	// TODO(roasbeef): track heights distinctly?

	switch {
	// If state number spending transaction matches the current latest
	// state, then they've initiated a unilateral close. So we'll trigger
	// the unilateral close signal so subscribers can clean up the state as
	// necessary.
	//
	// We'll also handle the case of the remote party broadcasting their
	// commitment transaction which is one height above ours. This case an
	// arise when we initiate a state transition, but the remote party has
	// a fail crash _after_ accepting the new state, but _before_ sending
	// their signature to us.
	case broadcastStateNum >= currentStateNum:
		walletLog.Infof("Unilateral close of ChannelPoint(%v) "+
			"detected", lc.channelState.ChanID)

		// As we've detected that the channel has been closed,
		// immediately delete the state from disk, creating a close
		// summary for future usage by related sub-systems.
		// TODO(roasbeef): include HTLC's
		//  * and time-locked balance
		closeSummary := &channeldb.ChannelCloseSummary{
			ChanPoint:      *lc.channelState.ChanID,
			ClosingTXID:    *commitSpend.SpenderTxHash,
			RemotePub:      lc.channelState.IdentityPub,
			Capacity:       lc.Capacity,
			SettledBalance: lc.channelState.OurBalance,
			CloseType:      channeldb.ForceClose,
			IsPending:      true,
		}
		if err := lc.DeleteState(closeSummary); err != nil {
			walletLog.Errorf("unable to delete channel state: %v",
				err)
		}

		// Notify any subscribers that we've detected a unilateral
		// commitment transaction broadcast.
		close(lc.UnilateralCloseSignal)
		lc.UnilateralClose <- &UnilateralCloseSummary{
			SpendDetail:         commitSpend,
			ChannelCloseSummary: closeSummary,
		}

	// If the state number broadcast is lower than the remote node's
	// current un-revoked height, then THEY'RE ATTEMPTING TO VIOLATE THE
	// CONTRACT LAID OUT WITHIN THE PAYMENT CHANNEL.  Therefore we close
	// the signal indicating a revoked broadcast to allow subscribers to
	// swiftly dispatch justice!!!
	case broadcastStateNum < currentStateNum:
		walletLog.Warnf("Remote peer has breached the channel "+
			"contract for ChannelPoint(%v). Revoked state #%v was "+
			"broadcast!!!", lc.channelState.ChanID,
			broadcastStateNum)

		// Create a new reach retribution struct which contains all the
		// data needed to swiftly bring the cheating peer to justice.
		retribution, err := newBreachRetribution(lc.channelState,
			broadcastStateNum, commitTxBroadcast)
		if err != nil {
			walletLog.Errorf("unable to create breach retribution: %v", err)
			return
		}

		walletLog.Debugf("Punishment breach retribution created: %v",
			spew.Sdump(retribution))

		// Finally, send the retribution struct over the contract beach
		// channel to allow the observer the use the breach retribution
		// to sweep ALL funds.
		lc.ContractBreach <- retribution
	}
}

// Stop gracefully shuts down any active goroutines spawned by the
// LightningChannel during regular duties.
func (lc *LightningChannel) Stop() {
	if !atomic.CompareAndSwapInt32(&lc.shutdown, 0, 1) {
		return
	}

	close(lc.quit)
}

// restoreStateLogs runs through the current locked-in HTLCs from the point of
// view of the channel and insert corresponding log entries (both local and
// remote) for each HTLC read from disk. This method is required to sync the
// in-memory state of the state machine with that read from persistent storage.
func (lc *LightningChannel) restoreStateLogs() error {
	var pastHeight uint64
	if lc.currentHeight > 0 {
		pastHeight = lc.currentHeight - 1
	}

	// In order to reconstruct the pkScripts on each of the pending HTLC
	// outputs (if any) we'll need to regenerate the current revocation for
	// this current un-revoked state.
	ourRevPreImage, err := lc.channelState.RevocationProducer.AtIndex(lc.currentHeight)
	if err != nil {
		return err
	}
	ourRevocation := sha256.Sum256(ourRevPreImage[:])
	theirRevocation := lc.channelState.TheirCurrentRevocationHash

	// Additionally, we'll fetch the current sent to commitment keys and
	// CSV delay values which are also required to fully generate the
	// scripts.
	localKey := lc.channelState.OurCommitKey
	remoteKey := lc.channelState.TheirCommitKey
	ourDelay := lc.channelState.LocalCsvDelay
	theirDelay := lc.channelState.RemoteCsvDelay

	var ourCounter, theirCounter uint64

	// TODO(roasbeef): partition entries added based on our current review
	// an our view of them from the log?
	for _, htlc := range lc.channelState.Htlcs {
		// TODO(roasbeef): set isForwarded to false for all? need to
		// persist state w.r.t to if forwarded or not, or can
		// inadvertently trigger replays

		// The proper pkScripts for this PaymentDescriptor must be
		// generated so we can easily locate them within the commitment
		// transaction in the future.
		var ourP2WSH, theirP2WSH []byte

		// If the either outputs is dust from the local or remote
		// node's perspective, then we don't need to generate the
		// scripts as we only generate them in order to locate the
		// outputs within the commitment transaction. As we'll mark
		// dust with a special output index in the on-disk state
		// snapshot.
		isDustLocal := htlc.Amt < lc.channelState.OurDustLimit
		isDustRemote := htlc.Amt < lc.channelState.TheirDustLimit
		if !isDustLocal {
			ourP2WSH, err = lc.genHtlcScript(htlc.Incoming, true,
				htlc.RefundTimeout, ourDelay, remoteKey,
				localKey, ourRevocation, htlc.RHash)
			if err != nil {
				return err
			}
		}
		if !isDustRemote {
			theirP2WSH, err = lc.genHtlcScript(htlc.Incoming, false,
				htlc.RefundTimeout, theirDelay, remoteKey,
				localKey, theirRevocation, htlc.RHash)
			if err != nil {
				return err
			}
		}

		pd := &PaymentDescriptor{
			RHash:                 htlc.RHash,
			Timeout:               htlc.RefundTimeout,
			Amount:                htlc.Amt,
			EntryType:             Add,
			addCommitHeightRemote: pastHeight,
			addCommitHeightLocal:  pastHeight,
			isDustLocal:           isDustLocal,
			isDustRemote:          isDustRemote,
			ourPkScript:           ourP2WSH,
			theirPkScript:         theirP2WSH,
		}

		if !htlc.Incoming {
			pd.Index = ourCounter
			lc.localUpdateLog.appendUpdate(pd)

			ourCounter++
		} else {
			pd.Index = theirCounter
			lc.remoteUpdateLog.appendUpdate(pd)
			lc.rHashMap[pd.RHash] = append(lc.rHashMap[pd.RHash], pd)

			theirCounter++
		}
	}

	lc.localCommitChain.tail().ourMessageIndex = ourCounter
	lc.localCommitChain.tail().theirMessageIndex = theirCounter
	lc.remoteCommitChain.tail().ourMessageIndex = ourCounter
	lc.remoteCommitChain.tail().theirMessageIndex = theirCounter
	lc.localCommitChain.tail().fee = lc.channelState.CommitFee
	lc.remoteCommitChain.tail().fee = lc.channelState.CommitFee

	return nil
}

// htlcView represents the "active" HTLCs at a particular point within the
// history of the HTLC update log.
type htlcView struct {
	ourUpdates   []*PaymentDescriptor
	theirUpdates []*PaymentDescriptor
}

// fetchHTLCView returns all the candidate HTLC updates which should be
// considered for inclusion within a commitment based on the passed HTLC log
// indexes.
func (lc *LightningChannel) fetchHTLCView(theirLogIndex, ourLogIndex uint64) *htlcView {
	var ourHTLCs []*PaymentDescriptor
	for e := lc.localUpdateLog.Front(); e != nil; e = e.Next() {
		htlc := e.Value.(*PaymentDescriptor)

		// This HTLC is active from this point-of-view iff the log
		// index of the state update is below the specified index in
		// our update log.
		if htlc.Index < ourLogIndex {
			ourHTLCs = append(ourHTLCs, htlc)
		}
	}

	var theirHTLCs []*PaymentDescriptor
	for e := lc.remoteUpdateLog.Front(); e != nil; e = e.Next() {
		htlc := e.Value.(*PaymentDescriptor)

		// If this is an incoming HTLC, then it is only active from
		// this point-of-view if the index of the HTLC addition in
		// their log is below the specified view index.
		if htlc.Index < theirLogIndex {
			theirHTLCs = append(theirHTLCs, htlc)
		}
	}

	return &htlcView{
		ourUpdates:   ourHTLCs,
		theirUpdates: theirHTLCs,
	}
}

// fetchCommitmentView returns a populated commitment which expresses the state
// of the channel from the point of view of a local or remote chain, evaluating
// the HTLC log up to the passed indexes. This function is used to construct
// both local and remote commitment transactions in order to sign or verify new
// commitment updates. A fully populated commitment is returned which reflects
// the proper balances for both sides at this point in the commitment chain.
func (lc *LightningChannel) fetchCommitmentView(remoteChain bool,
	ourLogIndex, theirLogIndex uint64, revocationKey *btcec.PublicKey,
	revocationHash [32]byte) (*commitment, error) {

	var commitChain *commitmentChain
	if remoteChain {
		commitChain = lc.remoteCommitChain
	} else {
		commitChain = lc.localCommitChain
	}

	// TODO(roasbeef): don't assume view is always fetched from tip?
	var ourBalance, theirBalance btcutil.Amount
	ourBalance = commitChain.tip().ourBalance
	theirBalance = commitChain.tip().theirBalance

	if lc.channelState.IsInitiator {
		ourBalance = ourBalance + commitChain.tip().fee
	} else if !lc.channelState.IsInitiator {
		theirBalance = theirBalance + commitChain.tip().fee
	}

	nextHeight := commitChain.tip().height + 1

	// Run through all the HTLCs that will be covered by this transaction
	// in order to update their commitment addition height, and to adjust
	// the balances on the commitment transaction accordingly.
	// TODO(roasbeef): error if log empty?
	htlcView := lc.fetchHTLCView(theirLogIndex, ourLogIndex)
	filteredHTLCView := lc.evaluateHTLCView(htlcView, &ourBalance, &theirBalance,
		nextHeight, remoteChain)

	var selfKey *btcec.PublicKey
	var remoteKey *btcec.PublicKey
	var delay uint32
	var delayBalance, p2wkhBalance, dustLimit btcutil.Amount
	if remoteChain {
		selfKey = lc.channelState.TheirCommitKey
		remoteKey = lc.channelState.OurCommitKey
		delay = lc.channelState.RemoteCsvDelay
		delayBalance = theirBalance
		p2wkhBalance = ourBalance
		dustLimit = lc.channelState.TheirDustLimit
	} else {
		selfKey = lc.channelState.OurCommitKey
		remoteKey = lc.channelState.TheirCommitKey
		delay = lc.channelState.LocalCsvDelay
		delayBalance = ourBalance
		p2wkhBalance = theirBalance
		dustLimit = lc.channelState.OurDustLimit
	}

	// Generate a new commitment transaction with all the latest
	// unsettled/un-timed out HTLCs.
	ourCommitTx := !remoteChain
	commitTx, err := CreateCommitTx(lc.fundingTxIn, selfKey, remoteKey,
		revocationKey, delay, delayBalance, p2wkhBalance, dustLimit)
	if err != nil {
		return nil, err
	}

	for _, htlc := range filteredHTLCView.ourUpdates {
		if (ourCommitTx && htlc.isDustLocal) ||
			(!ourCommitTx && htlc.isDustRemote) {
			continue
		}

		err := lc.addHTLC(commitTx, ourCommitTx, htlc,
			revocationHash, delay, false)
		if err != nil {
			return nil, err
		}
	}
	for _, htlc := range filteredHTLCView.theirUpdates {
		if (ourCommitTx && htlc.isDustLocal) ||
			(!ourCommitTx && htlc.isDustRemote) {
			continue
		}

		err := lc.addHTLC(commitTx, ourCommitTx, htlc,
			revocationHash, delay, true)
		if err != nil {
			return nil, err
		}
	}

	// Set the state hint of the commitment transaction to facilitate
	// quickly recovering the necessary penalty state in the case of an
	// uncooperative broadcast.
	obsfucator := lc.channelState.StateHintObsfucator
	stateNum := nextHeight
	if err := SetStateNumHint(commitTx, stateNum, obsfucator); err != nil {
		return nil, err
	}

	// Sort the transactions according to the agreed upon canonical
	// ordering. This lets us skip sending the entire transaction over,
	// instead we'll just send signatures.
	txsort.InPlaceSort(commitTx)
	c := &commitment{
		txn:               commitTx,
		height:            nextHeight,
		ourBalance:        ourBalance,
		ourMessageIndex:   ourLogIndex,
		theirMessageIndex: theirLogIndex,
		theirBalance:      theirBalance,
	}

	// In order to ensure _none_ of the HTLC's associated with this new
	// commitment are mutated, we'll manually copy over each HTLC to its
	// respective slice.
	c.outgoingHTLCs = make([]PaymentDescriptor, len(filteredHTLCView.ourUpdates))
	for i, htlc := range filteredHTLCView.ourUpdates {
		c.outgoingHTLCs[i] = *htlc
	}
	c.incomingHTLCs = make([]PaymentDescriptor, len(filteredHTLCView.theirUpdates))
	for i, htlc := range filteredHTLCView.theirUpdates {
		c.incomingHTLCs[i] = *htlc
	}

	return c, nil
}

// evaluateHTLCView processes all update entries in both HTLC update logs,
// producing a final view which is the result of properly applying all adds,
// settles, and timeouts found in both logs. The resulting view returned
// reflects the current state of HTLCs within the remote or local commitment
// chain.
func (lc *LightningChannel) evaluateHTLCView(view *htlcView, ourBalance,
	theirBalance *btcutil.Amount, nextHeight uint64, remoteChain bool) *htlcView {

	newView := &htlcView{}

	// We use two maps, one for the local log and one for the remote log to
	// keep track of which entries we need to skip when creating the final
	// htlc view. We skip an entry whenever we find a settle or a timeout
	// modifying an entry.
	skipUs := make(map[uint64]struct{})
	skipThem := make(map[uint64]struct{})

	// First we run through non-add entries in both logs, populating the
	// skip sets and mutating the current chain state (crediting balances, etc) to
	// reflect the settle/timeout entry encountered.
	for _, entry := range view.ourUpdates {
		if entry.EntryType == Add {
			continue
		}

		// If we're settling in inbound HTLC, and it hasn't been
		// processed, yet, the increment our state tracking the total
		// number of satoshis we've received within the channel.
		if entry.EntryType == Settle && !remoteChain &&
			entry.removeCommitHeightLocal == 0 {
			lc.channelState.TotalSatoshisReceived += uint64(entry.Amount)
		}

		addEntry := lc.remoteUpdateLog.lookup(entry.ParentIndex)

		skipThem[addEntry.Index] = struct{}{}
		processRemoveEntry(entry, ourBalance, theirBalance,
			nextHeight, remoteChain, true)
	}
	for _, entry := range view.theirUpdates {
		if entry.EntryType == Add {
			continue
		}

		// If the remote party is settling one of our outbound HTLC's,
		// and it hasn't been processed, yet, the increment our state
		// tracking the total number of satoshis we've sent within the
		// channel.
		if entry.EntryType == Settle && !remoteChain &&
			entry.removeCommitHeightLocal == 0 {
			lc.channelState.TotalSatoshisSent += uint64(entry.Amount)
		}

		addEntry := lc.localUpdateLog.lookup(entry.ParentIndex)

		skipUs[addEntry.Index] = struct{}{}
		processRemoveEntry(entry, ourBalance, theirBalance,
			nextHeight, remoteChain, false)
	}

	// Next we take a second pass through all the log entries, skipping any
	// settled HTLCs, and debiting the chain state balance due to any
	// newly added HTLCs.
	for _, entry := range view.ourUpdates {
		isAdd := entry.EntryType == Add
		if _, ok := skipUs[entry.Index]; !isAdd || ok {
			continue
		}

		processAddEntry(entry, ourBalance, theirBalance, nextHeight,
			remoteChain, false)
		newView.ourUpdates = append(newView.ourUpdates, entry)
	}
	for _, entry := range view.theirUpdates {
		isAdd := entry.EntryType == Add
		if _, ok := skipThem[entry.Index]; !isAdd || ok {
			continue
		}

		processAddEntry(entry, ourBalance, theirBalance, nextHeight,
			remoteChain, true)
		newView.theirUpdates = append(newView.theirUpdates, entry)
	}

	return newView
}

// processAddEntry evaluates the effect of an add entry within the HTLC log.
// If the HTLC hasn't yet been committed in either chain, then the height it
// was committed is updated. Keeping track of this inclusion height allows us to
// later compact the log once the change is fully committed in both chains.
func processAddEntry(htlc *PaymentDescriptor, ourBalance, theirBalance *btcutil.Amount,
	nextHeight uint64, remoteChain bool, isIncoming bool) {

	// If we're evaluating this entry for the remote chain (to create/view
	// a new commitment), then we'll may be updating the height this entry
	// was added to the chain. Otherwise, we may be updating the entry's
	// height w.r.t the local chain.
	var addHeight *uint64
	if remoteChain {
		addHeight = &htlc.addCommitHeightRemote
	} else {
		addHeight = &htlc.addCommitHeightLocal
	}

	if *addHeight != 0 {
		return
	}

	if isIncoming {
		// If this is a new incoming (un-committed) HTLC, then we need
		// to update their balance accordingly by subtracting the
		// amount of the HTLC that are funds pending.
		*theirBalance -= htlc.Amount
	} else {
		// Similarly, we need to debit our balance if this is an out
		// going HTLC to reflect the pending balance.
		*ourBalance -= htlc.Amount
	}

	*addHeight = nextHeight
}

// processRemoveEntry processes a log entry which settles or timesout a
// previously added HTLC. If the removal entry has already been processed, it
// is skipped.
func processRemoveEntry(htlc *PaymentDescriptor, ourBalance,
	theirBalance *btcutil.Amount, nextHeight uint64,
	remoteChain bool, isIncoming bool) {

	var removeHeight *uint64
	if remoteChain {
		removeHeight = &htlc.removeCommitHeightRemote
	} else {
		removeHeight = &htlc.removeCommitHeightLocal
	}

	// Ignore any removal entries which have already been processed.
	if *removeHeight != 0 {
		return
	}

	switch {
	// If an incoming HTLC is being settled, then this means that we've
	// received the preimage either from another subsystem, or the
	// upstream peer in the route. Therefore, we increase our balance by
	// the HTLC amount.
	case isIncoming && htlc.EntryType == Settle:
		*ourBalance += htlc.Amount

	// Otherwise, this HTLC is being failed out, therefore the value of the
	// HTLC should return to the remote party.
	case isIncoming && htlc.EntryType == Fail:
		*theirBalance += htlc.Amount

	// If an outgoing HTLC is being settled, then this means that the
	// downstream party resented the preimage or learned of it via a
	// downstream peer. In either case, we credit their settled value with
	// the value of the HTLC.
	case !isIncoming && htlc.EntryType == Settle:
		*theirBalance += htlc.Amount

	// Otherwise, one of our outgoing HTLC's has timed out, so the value of
	// the HTLC should be returned to our settled balance.
	case !isIncoming && htlc.EntryType == Fail:
		*ourBalance += htlc.Amount
	}

	*removeHeight = nextHeight
}

// SignNextCommitment signs a new commitment which includes any previous
// unsettled HTLCs, any new HTLCs, and any modifications to prior HTLCs
// committed in previous commitment updates. Signing a new commitment
// decrements the available revocation window by 1. After a successful method
// call, the remote party's commitment chain is extended by a new commitment
// which includes all updates to the HTLC log prior to this method invocation.
func (lc *LightningChannel) SignNextCommitment() ([]byte, error) {
	lc.Lock()
	defer lc.Unlock()

	// If we're awaiting an ACK to a commitment signature, then we're
	// unable to create new states as we don't have any revocations we can
	// use.
	if lc.pendingACK {
		return nil, ErrNoWindow
	}

	// Ensure that we have enough unused revocation hashes given to us by the
	// remote party. If the set is empty, then we're unable to create a new
	// state unless they first revoke a prior commitment transaction.
	// TODO(roasbeef): remove now due to above?
	if len(lc.revocationWindow) == 0 ||
		len(lc.usedRevocations) == InitialRevocationWindow {
		return nil, ErrNoWindow
	}

	// Before we extend this new commitment to the remote commitment chain,
	// ensure that we aren't violating any of the constraints the remote
	// party set up when we initially set up the channel. If we are, then
	// we'll abort this state transition.
	err := lc.validateCommitmentSanity(lc.remoteUpdateLog.ackedIndex,
		lc.localUpdateLog.logIndex, false)
	if err != nil {
		return nil, err
	}

	// Grab the next revocation hash and key to use for this new commitment
	// transaction, if no errors occur then this revocation tuple will be
	// moved to the used set.
	nextRevocation := lc.revocationWindow[0]
	remoteRevocationKey := nextRevocation.NextRevocationKey
	remoteRevocationHash := nextRevocation.NextRevocationHash

	// Create a new commitment view which will calculate the evaluated
	// state of the remote node's new commitment including our latest added
	// HTLCs. The view includes the latest balances for both sides on the
	// remote node's chain, and also update the addition height of any new
	// HTLC log entries. When we creating a new remote view, we include
	// _all_ of our changes (pending or committed) but only the remote
	// node's changes up to the last change we've ACK'd.
	newCommitView, err := lc.fetchCommitmentView(true, lc.localUpdateLog.logIndex,
		lc.remoteUpdateLog.ackedIndex, remoteRevocationKey, remoteRevocationHash)
	if err != nil {
		return nil, err
	}

	walletLog.Tracef("ChannelPoint(%v): extending remote chain to height %v",
		lc.channelState.ChanID, newCommitView.height)
	walletLog.Tracef("ChannelPoint(%v): remote chain: our_balance=%v, "+
		"their_balance=%v, commit_tx: %v", lc.channelState.ChanID,
		newCommitView.ourBalance, newCommitView.theirBalance,
		newLogClosure(func() string {
			return spew.Sdump(newCommitView.txn)
		}))

	// Sign their version of the new commitment transaction.
	lc.signDesc.SigHashes = txscript.NewTxSigHashes(newCommitView.txn)
	sig, err := lc.signer.SignOutputRaw(newCommitView.txn, lc.signDesc)
	if err != nil {
		return nil, err
	}

	// Extend the remote commitment chain by one with the addition of our
	// latest commitment update.
	lc.remoteCommitChain.addCommitment(newCommitView)

	// Move the now used revocation hash from the unused set to the used set.
	// We only do this at the end, as we know at this point the procedure will
	// succeed without any errors.
	lc.usedRevocations = append(lc.usedRevocations, nextRevocation)
	lc.revocationWindow[0] = nil // Avoid a GC leak.
	lc.revocationWindow = lc.revocationWindow[1:]

	// As we've just created a new update for the remote commitment chain,
	// we set the bool indicating that we're waiting for an ACK to our new
	// changes.
	lc.pendingACK = true

	// Additionally, we'll remember our log index at this point, so we can
	// properly track which changes have been ACK'd.
	lc.localUpdateLog.initiateTransition()

	return sig, nil
}

// validateCommitmentSanity is used to validate that on current state the commitment
// transaction is valid in terms of propagating it over Bitcoin network, and
// also that all outputs are meet Bitcoin spec requirements and they are
// spendable.
func (lc *LightningChannel) validateCommitmentSanity(theirLogCounter,
	ourLogCounter uint64, prediction bool) error {

	htlcCount := 0

	if prediction {
		htlcCount++
	}

	// Run through all the HTLCs that will be covered by this transaction
	// in order to calculate theirs count.
	htlcView := lc.fetchHTLCView(theirLogCounter, ourLogCounter)

	for _, entry := range htlcView.ourUpdates {
		if entry.EntryType == Add {
			htlcCount++
		} else {
			htlcCount--
		}
	}

	for _, entry := range htlcView.theirUpdates {
		if entry.EntryType == Add {
			htlcCount++
		} else {
			htlcCount--
		}
	}

	if htlcCount > MaxHTLCNumber {
		return ErrMaxHTLCNumber
	}

	return nil
}

// ReceiveNewCommitment process a signature for a new commitment state sent by
// the remote party. This method will should be called in response to the
// remote party initiating a new change, or when the remote party sends a
// signature fully accepting a new state we've initiated. If we are able to
// successfully validate the signature, then the generated commitment is added
// to our local commitment chain. Once we send a revocation for our prior
// state, then this newly added commitment becomes our current accepted channel
// state.
func (lc *LightningChannel) ReceiveNewCommitment(rawSig []byte) error {
	lc.Lock()
	defer lc.Unlock()

	// Ensure that this new local update from the remote node respects all
	// the constraints we specified during initial channel setup. If not,
	// then we'll abort the channel as they've violated our constraints.
	err := lc.validateCommitmentSanity(lc.remoteUpdateLog.logIndex,
		lc.localUpdateLog.ackedIndex, false)
	if err != nil {
		return err
	}

	theirCommitKey := lc.channelState.TheirCommitKey
	theirMultiSigKey := lc.channelState.TheirMultiSigKey

	// We're receiving a new commitment which attempts to extend our local
	// commitment chain height by one, so fetch the proper revocation to
	// derive the key+hash needed to construct the new commitment view and
	// state.
	nextHeight := lc.currentHeight + 1
	revocation, err := lc.channelState.RevocationProducer.AtIndex(nextHeight)
	if err != nil {
		return err
	}
	revocationKey := DeriveRevocationPubkey(theirCommitKey, revocation[:])
	revocationHash := sha256.Sum256(revocation[:])

	// With the revocation information calculated, construct the new
	// commitment view which includes all the entries we know of in their
	// HTLC log, and up to ourLogIndex in our HTLC log.
	localCommitmentView, err := lc.fetchCommitmentView(false,
		lc.localUpdateLog.ackedIndex, lc.remoteUpdateLog.logIndex,
		revocationKey, revocationHash)
	if err != nil {
		return err
	}

	walletLog.Tracef("ChannelPoint(%v): extending local chain to height %v",
		lc.channelState.ChanID, localCommitmentView.height)

	walletLog.Tracef("ChannelPoint(%v): local chain: our_balance=%v, "+
		"their_balance=%v, commit_tx: %v", lc.channelState.ChanID,
		localCommitmentView.ourBalance, localCommitmentView.theirBalance,
		newLogClosure(func() string {
			return spew.Sdump(localCommitmentView.txn)
		}),
	)

	// Construct the sighash of the commitment transaction corresponding to
	// this newly proposed state update.
	localCommitTx := localCommitmentView.txn
	multiSigScript := lc.channelState.FundingWitnessScript
	hashCache := txscript.NewTxSigHashes(localCommitTx)
	sigHash, err := txscript.CalcWitnessSigHash(multiSigScript, hashCache,
		txscript.SigHashAll, localCommitTx, 0, int64(lc.channelState.Capacity))
	if err != nil {
		// TODO(roasbeef): fetchview has already mutated the HTLCs...
		//  * need to either roll-back, or make pure
		return err
	}

	// Ensure that the newly constructed commitment state has a valid
	// signature.
	theirMultiSigKey.Curve = btcec.S256()
	sig, err := btcec.ParseSignature(rawSig, btcec.S256())
	if err != nil {
		return err
	} else if !sig.Verify(sigHash, theirMultiSigKey) {
		return fmt.Errorf("invalid commitment signature")
	}

	// The signature checks out, so we can now add the new commitment to
	// our local commitment chain.
	localCommitmentView.sig = rawSig
	lc.localCommitChain.addCommitment(localCommitmentView)

	// Finally we'll keep track of the current pending index for the remote
	// party so we can ACK up to this value once we revoke our current
	// commitment.
	lc.remoteUpdateLog.initiateTransition()

	return nil
}

// FullySynced returns a boolean value reflecting if both commitment chains
// (remote+local) are fully in sync. Both commitment chains are fully in sync
// if the tip of each chain includes the latest committed changes from both
// sides.
func (lc *LightningChannel) FullySynced() bool {
	lc.RLock()
	defer lc.RUnlock()

	oweCommitment := (lc.localCommitChain.tip().height >
		lc.remoteCommitChain.tip().height)

	localUpdatesSynced := (lc.localCommitChain.tip().ourMessageIndex ==
		lc.remoteCommitChain.tip().ourMessageIndex)

	remoteUpdatesSynced := (lc.localCommitChain.tip().theirMessageIndex ==
		lc.remoteCommitChain.tip().theirMessageIndex)

	return !oweCommitment && localUpdatesSynced && remoteUpdatesSynced
}

// RevokeCurrentCommitment revokes the next lowest unrevoked commitment
// transaction in the local commitment chain. As a result the edge of our
// revocation window is extended by one, and the tail of our local commitment
// chain is advanced by a single commitment. This now lowest unrevoked
// commitment becomes our currently accepted state within the channel.
func (lc *LightningChannel) RevokeCurrentCommitment() (*lnwire.RevokeAndAck, error) {
	lc.Lock()
	defer lc.Unlock()

	theirCommitKey := lc.channelState.TheirCommitKey

	// Now that we've accept a new state transition, we send the remote
	// party the revocation for our current commitment state.
	revocationMsg := &lnwire.RevokeAndAck{}
	currentRevocation, err := lc.channelState.RevocationProducer.AtIndex(lc.currentHeight)
	if err != nil {
		return nil, err
	}
	copy(revocationMsg.Revocation[:], currentRevocation[:])

	// Along with this revocation, we'll also send an additional extension
	// to our revocation window to the remote party.
	lc.revocationWindowEdge++
	revocationEdge, err := lc.channelState.RevocationProducer.AtIndex(lc.revocationWindowEdge)
	if err != nil {
		return nil, err
	}
	revocationMsg.NextRevocationKey = DeriveRevocationPubkey(theirCommitKey,
		revocationEdge[:])
	revocationMsg.NextRevocationHash = sha256.Sum256(revocationEdge[:])

	walletLog.Tracef("ChannelPoint(%v): revoking height=%v, now at height=%v, window_edge=%v",
		lc.channelState.ChanID, lc.localCommitChain.tail().height,
		lc.currentHeight+1, lc.revocationWindowEdge)

	// Advance our tail, as we've revoked our previous state.
	lc.localCommitChain.advanceTail()
	lc.currentHeight++

	// Additionally, generate a channel delta for this state transition for
	// persistent storage.
	tail := lc.localCommitChain.tail()
	delta, err := tail.toChannelDelta(true)
	if err != nil {
		return nil, err
	}
	err = lc.channelState.UpdateCommitment(tail.txn, tail.sig, delta)
	if err != nil {
		return nil, err
	}

	walletLog.Tracef("ChannelPoint(%v): state transition accepted: "+
		"our_balance=%v, their_balance=%v", lc.channelState.ChanID,
		tail.ourBalance, tail.theirBalance)

	// In the process of revoking our current commitment, we've also
	// implicitly ACK'd their set of pending changes that arrived before
	// the signature the triggered this revocation. So we'll move up their
	// ACK'd index within the log to right at this set of pending changes.
	lc.remoteUpdateLog.ackTransition()

	revocationMsg.ChanID = lnwire.NewChanIDFromOutPoint(lc.channelState.ChanID)
	return revocationMsg, nil
}

// ReceiveRevocation processes a revocation sent by the remote party for the
// lowest unrevoked commitment within their commitment chain. We receive a
// revocation either during the initial session negotiation wherein revocation
// windows are extended, or in response to a state update that we initiate. If
// successful, then the remote commitment chain is advanced by a single
// commitment, and a log compaction is attempted. In addition, a slice of
// HTLC's which can be forwarded upstream are returned.
func (lc *LightningChannel) ReceiveRevocation(revMsg *lnwire.RevokeAndAck) ([]*PaymentDescriptor, error) {
	lc.Lock()
	defer lc.Unlock()

	// The revocation has a nil (zero) preimage, then this should simply be
	// added to the end of the revocation window for the remote node.
	if bytes.Equal(zeroHash[:], revMsg.Revocation[:]) {
		lc.revocationWindow = append(lc.revocationWindow, revMsg)
		return nil, nil
	}

	// Now that we've received a new revocation from the remote party,
	// we'll toggle our pendingACk bool to indicate that we can create a
	// new commitment state after we finish processing this revocation.
	lc.pendingACK = false

	ourCommitKey := lc.channelState.OurCommitKey
	currentRevocationKey := lc.channelState.TheirCurrentRevocation
	pendingRevocation := chainhash.Hash(revMsg.Revocation)

	// Ensure that the new pre-image can be placed in preimage store.
	// TODO(rosbeef): abstract into func
	store := lc.channelState.RevocationStore
	if err := store.AddNextEntry(&pendingRevocation); err != nil {
		return nil, err
	}

	// Verify that the revocation public key we can derive using this
	// preimage and our private key is identical to the revocation key we
	// were given for their current (prior) commitment transaction.
	revocationPub := DeriveRevocationPubkey(ourCommitKey, pendingRevocation[:])
	if !revocationPub.IsEqual(currentRevocationKey) {
		return nil, fmt.Errorf("revocation key mismatch")
	}

	// Additionally, we need to ensure we were given the proper preimage
	// to the revocation hash used within any current HTLCs.
	if !bytes.Equal(lc.channelState.TheirCurrentRevocationHash[:], zeroHash[:]) {
		revokeHash := sha256.Sum256(pendingRevocation[:])
		// TODO(roasbeef): rename to drop the "Their"
		if !bytes.Equal(lc.channelState.TheirCurrentRevocationHash[:], revokeHash[:]) {
			return nil, fmt.Errorf("revocation hash mismatch")
		}
	}

	// Advance the head of the revocation queue now that this revocation has
	// been verified. Additionally, extend the end of our unused revocation
	// queue with the newly extended revocation window update.
	nextRevocation := lc.usedRevocations[0]
	lc.channelState.TheirCurrentRevocation = nextRevocation.NextRevocationKey
	lc.channelState.TheirCurrentRevocationHash = nextRevocation.NextRevocationHash
	lc.usedRevocations[0] = nil // Prevent GC leak.
	lc.usedRevocations = lc.usedRevocations[1:]
	lc.revocationWindow = append(lc.revocationWindow, revMsg)

	walletLog.Tracef("ChannelPoint(%v): remote party accepted state transition, "+
		"revoked height %v, now at %v", lc.channelState.ChanID,
		lc.remoteCommitChain.tail().height,
		lc.remoteCommitChain.tail().height+1)

	// At this point, the revocation has been accepted, and we've rotated
	// the current revocation key+hash for the remote party. Therefore we
	// sync now to ensure the revocation producer state is consistent with
	// the current commitment height.
	tail := lc.remoteCommitChain.tail()
	delta, err := tail.toChannelDelta(false)
	if err != nil {
		return nil, err
	}
	if err := lc.channelState.AppendToRevocationLog(delta); err != nil {
		return nil, err
	}

	// Since they revoked the current lowest height in their commitment
	// chain, we can advance their chain by a single commitment.
	lc.remoteCommitChain.advanceTail()

	remoteChainTail := lc.remoteCommitChain.tail().height
	localChainTail := lc.localCommitChain.tail().height

	// Now that we've verified the revocation update the state of the HTLC
	// log as we may be able to prune portions of it now, and update their
	// balance.
	var htlcsToForward []*PaymentDescriptor
	for e := lc.remoteUpdateLog.Front(); e != nil; e = e.Next() {
		htlc := e.Value.(*PaymentDescriptor)

		if htlc.isForwarded {
			continue
		}

		// TODO(roasbeef): re-visit after adding persistence to HTLCs
		//  * either record add height, or set to N - 1
		uncomitted := (htlc.addCommitHeightRemote == 0 ||
			htlc.addCommitHeightLocal == 0)
		if htlc.EntryType == Add && uncomitted {
			continue
		}

		if htlc.EntryType == Add &&
			remoteChainTail >= htlc.addCommitHeightRemote &&
			localChainTail >= htlc.addCommitHeightLocal {

			htlc.isForwarded = true
			htlcsToForward = append(htlcsToForward, htlc)
		} else if htlc.EntryType != Add &&
			remoteChainTail >= htlc.removeCommitHeightRemote &&
			localChainTail >= htlc.removeCommitHeightLocal {

			htlc.isForwarded = true
			htlcsToForward = append(htlcsToForward, htlc)
		}
	}

	// As we've just completed a new state transition, attempt to see if we
	// can remove any entries from the update log which have been removed
	// from the PoV of both commitment chains.
	compactLogs(lc.localUpdateLog, lc.remoteUpdateLog,
		localChainTail, remoteChainTail)

	// As a final step, now that we've received an ACK for our last batch
	// of pending changes, we'll update our local ACK'd index to the now
	// commitment index, and reset our pendingACKIndex.
	lc.localUpdateLog.ackTransition()

	return htlcsToForward, nil
}

// ExtendRevocationWindow extends our revocation window by a single revocation,
// increasing the number of new commitment updates the remote party can
// initiate without our cooperation.
func (lc *LightningChannel) ExtendRevocationWindow() (*lnwire.RevokeAndAck, error) {
	lc.Lock()
	defer lc.Unlock()

	/// TODO(roasbeef): error if window edge differs from tail by more than
	// InitialRevocationWindow

	revMsg := &lnwire.RevokeAndAck{}
	revMsg.ChanID = lnwire.NewChanIDFromOutPoint(lc.channelState.ChanID)

	nextHeight := lc.revocationWindowEdge + 1
	revocation, err := lc.channelState.RevocationProducer.AtIndex(nextHeight)
	if err != nil {
		return nil, err
	}

	theirCommitKey := lc.channelState.TheirCommitKey
	revMsg.NextRevocationKey = DeriveRevocationPubkey(theirCommitKey,
		revocation[:])
	revMsg.NextRevocationHash = sha256.Sum256(revocation[:])

	lc.revocationWindowEdge++

	return revMsg, nil
}

// NextRevocationkey returns the revocation key for the _next_ commitment
// height. The pubkey returned by this function is required by the remote party
// to extend our commitment chain with a new commitment.
//
// TODO(roasbeef): after commitment tx re-write add methdod to ingest
// revocation key
func (lc *LightningChannel) NextRevocationkey() (*btcec.PublicKey, error) {
	lc.RLock()
	defer lc.RUnlock()

	nextHeight := lc.currentHeight + 1
	revocation, err := lc.channelState.RevocationProducer.AtIndex(nextHeight)
	if err != nil {
		return nil, err
	}

	theirCommitKey := lc.channelState.TheirCommitKey
	return DeriveRevocationPubkey(theirCommitKey,
		revocation[:]), nil
}

// AddHTLC adds an HTLC to the state machine's local update log. This method
// should be called when preparing to send an outgoing HTLC.
//
// TODO(roasbeef): check for duplicates below? edge case during restart w/ HTLC
// persistence
func (lc *LightningChannel) AddHTLC(htlc *lnwire.UpdateAddHTLC) (uint64, error) {
	lc.Lock()
	defer lc.Unlock()

	if err := lc.validateCommitmentSanity(lc.remoteUpdateLog.logIndex,
		lc.localUpdateLog.logIndex, true); err != nil {
		return 0, err
	}

	pd := &PaymentDescriptor{
		EntryType:    Add,
		RHash:        PaymentHash(htlc.PaymentHash),
		Timeout:      htlc.Expiry,
		Amount:       htlc.Amount,
		Index:        lc.localUpdateLog.logIndex,
		isDustLocal:  htlc.Amount < lc.channelState.OurDustLimit,
		isDustRemote: htlc.Amount < lc.channelState.TheirDustLimit,
	}

	lc.localUpdateLog.appendUpdate(pd)

	return pd.Index, nil
}

// ReceiveHTLC adds an HTLC to the state machine's remote update log. This
// method should be called in response to receiving a new HTLC from the remote
// party.
func (lc *LightningChannel) ReceiveHTLC(htlc *lnwire.UpdateAddHTLC) (uint64, error) {
	lc.Lock()
	defer lc.Unlock()

	if err := lc.validateCommitmentSanity(lc.remoteUpdateLog.logIndex,
		lc.localUpdateLog.logIndex, true); err != nil {
		return 0, err
	}

	pd := &PaymentDescriptor{
		EntryType:    Add,
		RHash:        PaymentHash(htlc.PaymentHash),
		Timeout:      htlc.Expiry,
		Amount:       htlc.Amount,
		Index:        lc.remoteUpdateLog.logIndex,
		isDustLocal:  htlc.Amount < lc.channelState.OurDustLimit,
		isDustRemote: htlc.Amount < lc.channelState.TheirDustLimit,
	}

	lc.remoteUpdateLog.appendUpdate(pd)

	lc.rHashMap[pd.RHash] = append(lc.rHashMap[pd.RHash], pd)

	return pd.Index, nil
}

// SettleHTLC attempts to settle an existing outstanding received HTLC. The
// remote log index of the HTLC settled is returned in order to facilitate
// creating the corresponding wire message. In the case the supplied preimage
// is invalid, an error is returned.
func (lc *LightningChannel) SettleHTLC(preimage [32]byte) (uint64, error) {
	lc.Lock()
	defer lc.Unlock()

	paymentHash := sha256.Sum256(preimage[:])
	targetHTLCs, ok := lc.rHashMap[paymentHash]
	if !ok {
		return 0, fmt.Errorf("invalid payment hash")
	}
	targetHTLC := targetHTLCs[0]

	pd := &PaymentDescriptor{
		Amount:      targetHTLC.Amount,
		RPreimage:   preimage,
		Index:       lc.localUpdateLog.logIndex,
		ParentIndex: targetHTLC.Index,
		EntryType:   Settle,
	}

	lc.localUpdateLog.appendUpdate(pd)

	lc.rHashMap[paymentHash][0] = nil
	lc.rHashMap[paymentHash] = lc.rHashMap[paymentHash][1:]
	if len(lc.rHashMap[paymentHash]) == 0 {
		delete(lc.rHashMap, paymentHash)
	}

	return targetHTLC.Index, nil
}

// ReceiveHTLCSettle attempts to settle an existing outgoing HTLC indexed by an
// index into the local log. If the specified index doesn't exist within the
// log, and error is returned. Similarly if the preimage is invalid w.r.t to
// the referenced of then a distinct error is returned.
func (lc *LightningChannel) ReceiveHTLCSettle(preimage [32]byte, logIndex uint64) error {
	lc.Lock()
	defer lc.Unlock()

	paymentHash := sha256.Sum256(preimage[:])
	htlc := lc.localUpdateLog.lookup(logIndex)
	if htlc == nil {
		return fmt.Errorf("non existant log entry")
	}

	if !bytes.Equal(htlc.RHash[:], paymentHash[:]) {
		return fmt.Errorf("invalid payment hash")
	}

	pd := &PaymentDescriptor{
		Amount:      htlc.Amount,
		RPreimage:   preimage,
		ParentIndex: htlc.Index,
		Index:       lc.remoteUpdateLog.logIndex,
		EntryType:   Settle,
	}

	lc.remoteUpdateLog.appendUpdate(pd)
	return nil
}

// FailHTLC attempts to fail a targeted HTLC by its payment hash, inserting an
// entry which will remove the target log entry within the next commitment
// update. This method is intended to be called in order to cancel in
// _incoming_ HTLC.
func (lc *LightningChannel) FailHTLC(rHash [32]byte) (uint64, error) {
	lc.Lock()
	defer lc.Unlock()

	addEntries, ok := lc.rHashMap[rHash]
	if !ok {
		return 0, fmt.Errorf("unable to find HTLC to fail")
	}
	addEntry := addEntries[0]

	pd := &PaymentDescriptor{
		Amount:      addEntry.Amount,
		RHash:       addEntry.RHash,
		ParentIndex: addEntry.Index,
		Index:       lc.localUpdateLog.logIndex,
		EntryType:   Fail,
	}

	lc.localUpdateLog.appendUpdate(pd)

	lc.rHashMap[rHash][0] = nil
	lc.rHashMap[rHash] = lc.rHashMap[rHash][1:]
	if len(lc.rHashMap[rHash]) == 0 {
		delete(lc.rHashMap, rHash)
	}

	return addEntry.Index, nil
}

// ReceiveFailHTLC attempts to cancel a targeted HTLC by its log index,
// inserting an entry which will remove the target log entry within the next
// commitment update. This method should be called in response to the upstream
// party cancelling an outgoing HTLC.
func (lc *LightningChannel) ReceiveFailHTLC(logIndex uint64) error {
	lc.Lock()
	defer lc.Unlock()

	htlc := lc.localUpdateLog.lookup(logIndex)
	if htlc == nil {
		return fmt.Errorf("unable to find HTLC to fail")
	}

	pd := &PaymentDescriptor{
		Amount:      htlc.Amount,
		RHash:       htlc.RHash,
		ParentIndex: htlc.Index,
		Index:       lc.remoteUpdateLog.logIndex,
		EntryType:   Fail,
	}

	lc.remoteUpdateLog.appendUpdate(pd)

	return nil
}

// ChannelPoint returns the outpoint of the original funding transaction which
// created this active channel. This outpoint is used throughout various
// subsystems to uniquely identify an open channel.
func (lc *LightningChannel) ChannelPoint() *wire.OutPoint {
	return lc.channelState.ChanID
}

// genHtlcScript generates the proper P2WSH public key scripts for the
// HTLC output modified by two-bits denoting if this is an incoming HTLC, and
// if the HTLC is being applied to their commitment transaction or ours.
func (lc *LightningChannel) genHtlcScript(isIncoming, ourCommit bool,
	timeout, delay uint32, remoteKey, localKey *btcec.PublicKey, revocation,
	rHash [32]byte) ([]byte, error) {
	// Generate the proper redeem scripts for the HTLC output modified by
	// two-bits denoting if this is an incoming HTLC, and if the HTLC is
	// being applied to their commitment transaction or ours.
	var pkScript []byte
	var err error
	switch {
	// The HTLC is paying to us, and being applied to our commitment
	// transaction. So we need to use the receiver's version of HTLC the
	// script.
	case isIncoming && ourCommit:
		pkScript, err = receiverHTLCScript(timeout, delay, remoteKey,
			localKey, revocation[:], rHash[:])
	// We're being paid via an HTLC by the remote party, and the HTLC is
	// being added to their commitment transaction, so we use the sender's
	// version of the HTLC script.
	case isIncoming && !ourCommit:
		pkScript, err = senderHTLCScript(timeout, delay, remoteKey,
			localKey, revocation[:], rHash[:])
	// We're sending an HTLC which is being added to our commitment
	// transaction. Therefore, we need to use the sender's version of the
	// HTLC script.
	case !isIncoming && ourCommit:
		pkScript, err = senderHTLCScript(timeout, delay, localKey,
			remoteKey, revocation[:], rHash[:])
	// Finally, we're paying the remote party via an HTLC, which is being
	// added to their commitment transaction. Therefore, we use the
	// receiver's version of the HTLC script.
	case !isIncoming && !ourCommit:
		pkScript, err = receiverHTLCScript(timeout, delay, localKey,
			remoteKey, revocation[:], rHash[:])
	}
	if err != nil {
		return nil, err
	}
	// Now that we have the redeem scripts, create the P2WSH public key
	// script for the output itself.
	htlcP2WSH, err := witnessScriptHash(pkScript)
	if err != nil {
		return nil, err
	}
	return htlcP2WSH, nil
}

// addHTLC adds a new HTLC to the passed commitment transaction. One of four
// full scripts will be generated for the HTLC output depending on if the HTLC
// is incoming and if it's being applied to our commitment transaction or that
// of the remote node's. Additionally, in order to be able to efficiently
// locate the added HTLC on the commitment transaction from the
// PaymentDescriptor that generated it, the generated script is stored within
// the descriptor itself.
func (lc *LightningChannel) addHTLC(commitTx *wire.MsgTx, ourCommit bool,
	paymentDesc *PaymentDescriptor, revocation [32]byte, delay uint32,
	isIncoming bool) error {

	localKey := lc.channelState.OurCommitKey
	remoteKey := lc.channelState.TheirCommitKey
	timeout := paymentDesc.Timeout
	rHash := paymentDesc.RHash

	htlcP2WSH, err := lc.genHtlcScript(isIncoming, ourCommit, timeout,
		delay, remoteKey, localKey, revocation, rHash)
	if err != nil {
		return err
	}

	// Add the new HTLC outputs to the respective commitment transactions.
	amountPending := int64(paymentDesc.Amount)
	commitTx.AddTxOut(wire.NewTxOut(amountPending, htlcP2WSH))

	// Store the pkScript of this particular PaymentDescriptor so we can
	// quickly locate it within the commitment transaction later.
	if ourCommit {
		paymentDesc.ourPkScript = htlcP2WSH
	} else {
		paymentDesc.theirPkScript = htlcP2WSH
	}

	return nil
}

// getSignedCommitTx function take the latest commitment transaction and populate
// it with witness data.
func (lc *LightningChannel) getSignedCommitTx() (*wire.MsgTx, error) {
	// Fetch the current commitment transaction, along with their signature
	// for the transaction.
	commitTx := lc.channelState.OurCommitTx
	theirSig := append(lc.channelState.OurCommitSig, byte(txscript.SigHashAll))

	// With this, we then generate the full witness so the caller can
	// broadcast a fully signed transaction.
	lc.signDesc.SigHashes = txscript.NewTxSigHashes(commitTx)
	ourSigRaw, err := lc.signer.SignOutputRaw(commitTx, lc.signDesc)
	if err != nil {
		return nil, err
	}

	ourSig := append(ourSigRaw, byte(txscript.SigHashAll))

	// With the final signature generated, create the witness stack
	// required to spend from the multi-sig output.
	ourKey := lc.channelState.OurMultiSigKey.SerializeCompressed()
	theirKey := lc.channelState.TheirMultiSigKey.SerializeCompressed()

	commitTx.TxIn[0].Witness = SpendMultiSig(lc.FundingWitnessScript, ourKey,
		ourSig, theirKey, theirSig)

	return commitTx, nil
}

// ForceCloseSummary describes the final commitment state before the channel is
// locked-down to initiate a force closure by broadcasting the latest state
// on-chain. The summary includes all the information required to claim all
// rightfully owned outputs.
// TODO(roasbeef): generalize, add HTLC info, etc.
type ForceCloseSummary struct {
	// ChanPoint is the outpoint that created the channel which has been
	// force closed.
	ChanPoint wire.OutPoint

	// SelfOutpoint is the output created by the above close tx which is
	// spendable by us after a relative time delay.
	SelfOutpoint wire.OutPoint

	// CloseTx is the transaction which closed the channel on-chain. If we
	// initiate the force close, then this'll be our latest commitment
	// state. Otherwise, this'll be the state that the remote peer
	// broadcasted on-chain.
	CloseTx *wire.MsgTx

	// SelfOutputSignDesc is a fully populated sign descriptor capable of
	// generating a valid signature to sweep the self output.
	SelfOutputSignDesc *SignDescriptor

	// SelfOutputMaturity is the relative maturity period before the above
	// output can be claimed.
	SelfOutputMaturity uint32
}

// UnilateralCloseSummary describes the details of a detected unilateral
// channel closure. This includes the information about with which
// transactions, and block the channel was unilaterally closed, as well as
// summarization details concerning the _state_ of the channel at the point of
// channel closure.
type UnilateralCloseSummary struct {
	*chainntnfs.SpendDetail
	*channeldb.ChannelCloseSummary
}

// ForceClose executes a unilateral closure of the transaction at the current
// lowest commitment height of the channel. Following a force closure, all
// state transitions, or modifications to the state update logs will be
// rejected. Additionally, this function also returns a ForceCloseSummary which
// includes the necessary details required to sweep all the time-locked within
// the commitment transaction.
//
// TODO(roasbeef): all methods need to abort if in dispute state
// TODO(roasbeef): method to generate CloseSummaries for when the remote peer
// does a unilateral close
func (lc *LightningChannel) ForceClose() (*ForceCloseSummary, error) {
	lc.Lock()
	defer lc.Unlock()

	// Set the channel state to indicate that the channel is now in a
	// contested state.
	lc.status = channelDispute

	commitTx, err := lc.getSignedCommitTx()
	if err != nil {
		return nil, err
	}

	// Re-derive the original pkScript for to-self output within the
	// commitment transaction. We'll need this to find the corresponding
	// output in the commitment transaction and potentially for creating
	// the sign descriptor.
	csvTimeout := lc.channelState.LocalCsvDelay
	selfKey := lc.channelState.OurCommitKey
	producer := lc.channelState.RevocationProducer
	unusedRevocation, err := producer.AtIndex(lc.currentHeight)
	if err != nil {
		return nil, err
	}
	revokeKey := DeriveRevocationPubkey(lc.channelState.TheirCommitKey,
		unusedRevocation[:])
	selfScript, err := commitScriptToSelf(csvTimeout, selfKey, revokeKey)
	if err != nil {
		return nil, err
	}
	payToUsScriptHash, err := witnessScriptHash(selfScript)
	if err != nil {
		return nil, err
	}

	// Locate the output index of the delayed commitment output back to us.
	// We'll return the details of this output to the caller so they can
	// sweep it once it's mature.
	// TODO(roasbeef): also return HTLC info, assumes only p2wsh is commit
	// tx
	var delayIndex uint32
	var delayScript []byte
	var selfSignDesc *SignDescriptor
	for i, txOut := range commitTx.TxOut {
		if !bytes.Equal(payToUsScriptHash, txOut.PkScript) {
			continue
		}

		delayIndex = uint32(i)
		delayScript = txOut.PkScript
		break
	}

	// With the necessary information gathered above, create a new sign
	// descriptor which is capable of generating the signature the caller
	// needs to sweep this output. The hash cache, and input index are not
	// set as the caller will decide these values once sweeping the output.
	// If the output is non-existant (dust), have the sign descriptor be nil.
	if len(delayScript) != 0 {
		selfSignDesc = &SignDescriptor{
			PubKey:        selfKey,
			WitnessScript: selfScript,
			Output: &wire.TxOut{
				PkScript: delayScript,
				Value:    int64(lc.channelState.OurBalance),
			},
			HashType: txscript.SigHashAll,
		}
	}

	// Finally, close the channel force close signal which notifies any
	// subscribers that the channel has now been forcibly closed. This
	// allows callers to begin to carry out any post channel closure
	// activities.
	close(lc.ForceCloseSignal)

	return &ForceCloseSummary{
		ChanPoint: *lc.channelState.ChanID,
		CloseTx:   commitTx,
		SelfOutpoint: wire.OutPoint{
			Hash:  commitTx.TxHash(),
			Index: delayIndex,
		},
		SelfOutputMaturity: csvTimeout,
		SelfOutputSignDesc: selfSignDesc,
	}, nil
}

// InitCooperativeClose initiates a cooperative closure of an active lightning
// channel. This method should only be executed once all pending HTLCs (if any)
// on the channel have been cleared/removed. Upon completion, the source
// channel will shift into the "closing" state, which indicates that all
// incoming/outgoing HTLC requests should be rejected. A signature for the
// closing transaction, and the txid of the closing transaction are returned.
// The initiator of the channel closure should then watch the blockchain for a
// confirmation of the closing transaction before considering the channel
// terminated. In the case of an unresponsive remote party, the initiator can
// either choose to execute a force closure, or backoff for a period of time,
// and retry the cooperative closure.
//
// TODO(roasbeef): caller should initiate signal to reject all incoming HTLCs,
// settle any inflight.
func (lc *LightningChannel) InitCooperativeClose() ([]byte, *chainhash.Hash, error) {
	lc.Lock()
	defer lc.Unlock()

	// If we're already closing the channel, then ignore this request.
	if lc.status == channelClosing || lc.status == channelClosed {
		// TODO(roasbeef): check to ensure no pending payments
		return nil, nil, ErrChanClosing
	}

	closeTx := CreateCooperativeCloseTx(lc.fundingTxIn,
		lc.channelState.OurDustLimit, lc.channelState.TheirDustLimit,
		lc.channelState.OurBalance, lc.channelState.TheirBalance,
		lc.channelState.OurDeliveryScript, lc.channelState.TheirDeliveryScript,
		lc.channelState.IsInitiator)

	// Ensure that the transaction doesn't explicitly violate any
	// consensus rules such as being too big, or having any value with a
	// negative output.
	tx := btcutil.NewTx(closeTx)
	if err := blockchain.CheckTransactionSanity(tx); err != nil {
		return nil, nil, err
	}

	// Finally, sign the completed cooperative closure transaction. As the
	// initiator we'll simply send our signature over to the remote party,
	// using the generated txid to be notified once the closure transaction
	// has been confirmed.
	lc.signDesc.SigHashes = txscript.NewTxSigHashes(closeTx)
	closeSig, err := lc.signer.SignOutputRaw(closeTx, lc.signDesc)
	if err != nil {
		return nil, nil, err
	}

	// As everything checks out, indicate in the channel status that a
	// channel closure has been initiated.
	lc.status = channelClosing

	closeTxSha := closeTx.TxHash()
	return closeSig, &closeTxSha, nil
}

// CompleteCooperativeClose completes the cooperative closure of the target
// active lightning channel. This method should be called in response to the
// remote node initiating a cooperative channel closure. A fully signed closure
// transaction is returned. It is the duty of the responding node to broadcast
// a signed+valid closure transaction to the network.
//
// NOTE: The passed remote sig is expected to be a fully complete signature
// including the proper sighash byte.
func (lc *LightningChannel) CompleteCooperativeClose(remoteSig []byte) (*wire.MsgTx, error) {
	lc.Lock()
	defer lc.Unlock()

	// If we're already closing the channel, then ignore this request.
	if lc.status == channelClosing || lc.status == channelClosed {
		// TODO(roasbeef): check to ensure no pending payments
		return nil, ErrChanClosing
	}

	// Create the transaction used to return the current settled balance
	// on this active channel back to both parties. In this current model,
	// the initiator pays full fees for the cooperative close transaction.
	closeTx := CreateCooperativeCloseTx(lc.fundingTxIn,
		lc.channelState.OurDustLimit, lc.channelState.TheirDustLimit,
		lc.channelState.OurBalance, lc.channelState.TheirBalance,
		lc.channelState.OurDeliveryScript, lc.channelState.TheirDeliveryScript,
		lc.channelState.IsInitiator)

	// Ensure that the transaction doesn't explicitly validate any
	// consensus rules such as being too big, or having any value with a
	// negative output.
	tx := btcutil.NewTx(closeTx)
	if err := blockchain.CheckTransactionSanity(tx); err != nil {
		return nil, err
	}

	// With the transaction created, we can finally generate our half of
	// the 2-of-2 multi-sig needed to redeem the funding output.
	hashCache := txscript.NewTxSigHashes(closeTx)
	lc.signDesc.SigHashes = hashCache
	closeSig, err := lc.signer.SignOutputRaw(closeTx, lc.signDesc)
	if err != nil {
		return nil, err
	}

	// Finally, construct the witness stack minding the order of the
	// pubkeys+sigs on the stack.
	ourKey := lc.channelState.OurMultiSigKey.SerializeCompressed()
	theirKey := lc.channelState.TheirMultiSigKey.SerializeCompressed()
	ourSig := append(closeSig, byte(txscript.SigHashAll))
	witness := SpendMultiSig(lc.signDesc.WitnessScript, ourKey, ourSig,
		theirKey, remoteSig)
	closeTx.TxIn[0].Witness = witness

	// Validate the finalized transaction to ensure the output script is
	// properly met, and that the remote peer supplied a valid signature.
	vm, err := txscript.NewEngine(lc.fundingP2WSH, closeTx, 0,
		txscript.StandardVerifyFlags, nil, hashCache,
		int64(lc.channelState.Capacity))
	if err != nil {
		return nil, err
	}
	if err := vm.Execute(); err != nil {
		return nil, err
	}

	// As the transaction is sane, and the scripts are valid we'll mark the
	// channel now as closed as the closure transaction should get into the
	// chain in a timely manner and possibly be re-broadcast by the wallet.
	lc.status = channelClosed

	return closeTx, nil
}

// DeleteState deletes all state concerning the channel from the underlying
// database, only leaving a small summary describing metadata of the
// channel's lifetime.
func (lc *LightningChannel) DeleteState(c *channeldb.ChannelCloseSummary) error {
	return lc.channelState.CloseChannel(c)
}

// StateSnapshot returns a snapshot of the current fully committed state within
// the channel.
func (lc *LightningChannel) StateSnapshot() *channeldb.ChannelSnapshot {
	lc.stateMtx.RLock()
	defer lc.stateMtx.RUnlock()

	return lc.channelState.Snapshot()
}

// CreateCommitTx creates a commitment transaction, spending from specified
// funding output. The commitment transaction contains two outputs: one paying
// to the "owner" of the commitment transaction which can be spent after a
// relative block delay or revocation event, and the other paying the the
// counterparty within the channel, which can be spent immediately.
func CreateCommitTx(fundingOutput *wire.TxIn, selfKey, theirKey *btcec.PublicKey,
	revokeKey *btcec.PublicKey, csvTimeout uint32, amountToSelf,
	amountToThem, dustLimit btcutil.Amount) (*wire.MsgTx, error) {

	// First, we create the script for the delayed "pay-to-self" output.
	// This output has 2 main redemption clauses: either we can redeem the
	// output after a relative block delay, or the remote node can claim
	// the funds with the revocation key if we broadcast a revoked
	// commitment transaction.
	ourRedeemScript, err := commitScriptToSelf(csvTimeout, selfKey,
		revokeKey)
	if err != nil {
		return nil, err
	}
	payToUsScriptHash, err := witnessScriptHash(ourRedeemScript)
	if err != nil {
		return nil, err
	}

	// Next, we create the script paying to them. This is just a regular
	// P2WPKH output, without any added CSV delay.
	theirWitnessKeyHash, err := commitScriptUnencumbered(theirKey)
	if err != nil {
		return nil, err
	}

	// Now that both output scripts have been created, we can finally create
	// the transaction itself. We use a transaction version of 2 since CSV
	// will fail unless the tx version is >= 2.
	commitTx := wire.NewMsgTx(2)
	commitTx.AddTxIn(fundingOutput)

	// Avoid creating dust outputs within the commitment transaction.
	if amountToSelf >= dustLimit {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: payToUsScriptHash,
			Value:    int64(amountToSelf),
		})
	}
	if amountToThem >= dustLimit {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: theirWitnessKeyHash,
			Value:    int64(amountToThem),
		})
	}

	return commitTx, nil
}

// CreateCooperativeCloseTx creates a transaction which if signed by both
// parties, then broadcast cooperatively closes an active channel. The creation
// of the closure transaction is modified by a boolean indicating if the party
// constructing the channel is the initiator of the closure. Currently it is
// expected that the initiator pays the transaction fees for the closing
// transaction in full.
func CreateCooperativeCloseTx(fundingTxIn *wire.TxIn,
	localDust, remoteDust, ourBalance, theirBalance btcutil.Amount,
	ourDeliveryScript, theirDeliveryScript []byte,
	initiator bool) *wire.MsgTx {

	// Construct the transaction to perform a cooperative closure of the
	// channel. In the event that one side doesn't have any settled funds
	// within the channel then a refund output for that particular side can
	// be omitted.
	closeTx := wire.NewMsgTx(2)
	closeTx.AddTxIn(fundingTxIn)

	// The initiator of a cooperative closure pays the fee in entirety.
	// Determine if we're the initiator so we can compute fees properly.
	if initiator {
		// TODO(roasbeef): take sat/byte here instead of properly calc
		ourBalance -= 5000
	} else {
		theirBalance -= 5000
	}

	// Create both cooperative closure outputs, properly respecting the
	// dust limits of both parties.
	if ourBalance >= localDust {
		closeTx.AddTxOut(&wire.TxOut{
			PkScript: ourDeliveryScript,
			Value:    int64(ourBalance),
		})
	}
	if theirBalance >= remoteDust {
		closeTx.AddTxOut(&wire.TxOut{
			PkScript: theirDeliveryScript,
			Value:    int64(theirBalance),
		})
	}

	txsort.InPlaceSort(closeTx)

	return closeTx
}
