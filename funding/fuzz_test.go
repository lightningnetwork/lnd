package funding

import (
	"encoding/binary"
	"errors"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

type Event uint8

const (
	// EvStartAsLocalFunder opens a funding flow with the SUT as the channel
	// funder. It consumes a 12-byte parameter block (readStartParams).
	EvStartAsLocalFunder Event = iota

	// EvStartAsLocalFundee opens a funding flow with the SUT as the channel
	// fundee. It consumes a 12-byte parameter block (readStartParams).
	EvStartAsLocalFundee

	// EvSwitchFlow changes which active funding flow subsequent
	// peer-targeted events act on. It consumes one selector byte.
	EvSwitchFlow

	// EvPeerInteraction is the message pump: it carries the current flow's
	// handshake one step forward, or fires an adversarial probe against the
	// SUT: a replayed, out-of-order, misdirected, or mis-identified
	// message, none of which the SUT may advance on.
	EvPeerInteraction

	// ConfFundChannelTx mines a single block on the current flow's funding
	// transaction. It consumes one byte, which in a small minority deliver
	// the counterparty's channel_ready to the SUT before her own
	// confirmation rather than after.
	ConfFundChannelTx

	// EvReorg takes the current flow's funding transaction back out of the
	// chain.
	EvReorg

	// EvDisconnectPeer takes the current flow's counterparty offline.
	EvDisconnectPeer

	// EvReconnectPeer brings him back and releases what was parked,
	// including a channel_ready the SUT held because the funding tx
	// confirmed while he was away.
	EvReconnectPeer

	// EvNoOp does nothing at all. It exists so the fuzzer can pad an input
	// without changing the FSM state.
	EvNoOp

	// NumEvents is the number of events above.
	NumEvents
)

type peerID uint64
type deliveredMsg struct {
	msg        lnwire.Message
	fromPeerID peerID
}

type PeerInteraction uint8

const (
	ModeAdvanceExpected PeerInteraction = iota
	ModeAdversarialReplay
	ModeAdversarialOutOfOrder
	ModeAdversarialWrongPeer
	ModeAdversarialWrongChanID
)

type fundingStage uint8

const (
	StageNone fundingStage = iota
	StageOpenChannel
	StageAcceptChannel
	StageFundingCreated
	StageFundingSigned
	StageFundingConfirmed
	StageOpen
	StageFailed
)

type fundingRole uint8

const (
	RoleNone fundingRole = iota
	RoleLocalFunder
	RoleLocalFundee
)

type fuzzStats struct {
	startedAsLocalFunder uint64
	startedAsLocalFundee uint64

	peerInteractions map[PeerInteraction]uint64

	confFundChannelTx uint64
	reorg             uint64

	disconnectPeer uint64
	reconnectPeer  uint64
}

type flowState struct {
	peerID peerID

	// chanID is the permanent channel ID, derived from the funding outpoint
	// once the funder broadcasts.
	chanID lnwire.ChannelID

	// fundingTx is the broadcast funding transaction.
	fundingTx *wire.MsgTx

	// pendingChanID is the pending channel ID negotiated before the
	// funding outpoint.
	pendingChanID [32]byte

	role    fundingRole
	stage   fundingStage
	history []deliveredMsg

	// pending is the most recent funding message one side has emitted but
	// the harness has not yet delivered to the other; A nil pending means
	// the wire handshake is complete (awaiting confirmation) or the flow
	// never started / has failed.
	pending lnwire.Message

	// pendingFrom identifies the emitter.
	pendingFrom peerID

	// openChanType is the channel_type this flow's OpenChannel carried as
	// the SUT saw it (nil when the message carried none). It is the
	// reference every channel_type oracle compares against.
	openChanType *lnwire.ChannelType

	// tamper is the flow's BOLT-2 channel_type conformance variant, kept on
	// the flow because the accept_channel variants act mid-handshake, long
	// after the start event that carried the parameters.
	tamper chanTypeTamper

	// zeroConf reports whether this flow negotiated a zero-conf channel, in
	// which case channel_ready is exchanged during the handshake rather
	// than after confirmation.
	zeroConf bool

	// confs is how deep the funding tx currently is: the number of blocks
	// mined on it, counting the one that contains it. It drops back to zero
	// when a reorg takes the tx out of the chain, and the channel confirms
	// once it reaches the depth the channel requires.
	confs uint32

	// reorgs counts how many times this flow's funding tx has been taken
	// back out of the chain, bounding a transition that would otherwise
	// repeat for as long as the input keeps asking (see maxReorgs).
	reorgs uint32

	// fundingHeight is the height of the block holding the funding tx. It
	// is the single source for both the confirmation height each side
	// records and the block height in the shortChanID, so the two can never
	// disagree. A reorg re-mines the tx into a later block, bumping it.
	fundingHeight uint32

	// offline reports whether the SUT currently believes this flow's
	// counterparty to be disconnected. The zero value is the connected
	// state, which is how every flow starts.
	offline bool

	// remoteReady holds the counterparty's channel_ready when it confirmed
	// while the peer was offline: it must not be delivered over a
	// connection that is supposed to be down, so it waits for
	// handleReconnectPeer.
	remoteReady *lnwire.ChannelReady

	// remote is the counterparty's real funding manager for this flow.
	// local is a lightweight peer handle carrying the SUT's identity,
	// passed as the peer argument when feeding messages into remote.
	remote *testNode
	local  *testNode

	// remoteConf is the counterparty's txid-keyed confirmation notifier
	// (installed in wireFlow), the counterpart of fsm.confNotifier for the
	// SUT, so handleConfFundChannelTx can confirm the counterparty
	// deterministically regardless of the negotiated confirmation depth.
	remoteConf *fuzzConfNotifier

	// updates and errChan receive the SUT's funding workflow updates and
	// errors for this flow.
	updates chan *lnrpc.OpenStatusUpdate
	errChan chan error
}

// awaitingConfirmation reports whether the funding tx has been broadcast and
// the channel is still waiting for it to confirm — the only state in which
// chain events (a confirmation, a reorg) mean anything for the flow.
func (p *flowState) awaitingConfirmation() bool {
	return p.stage == StageFundingSigned && p.pending == nil &&
		p.fundingTx != nil
}

// record appends a delivered message to the flow's history.
func (p *flowState) record(from peerID, msg lnwire.Message) {
	p.history = append(p.history, deliveredMsg{
		msg:        msg,
		fromPeerID: from,
	})
}

// localPeerID identifies the SUT (Alice) as the source of a delivered message.
const localPeerID peerID = 0

const (
	// managerTimeout bounds every wait on a manager the harness expects to
	// make progress. It is a deadlock detector rather than a delay: the
	// waits it guards are satisfied in microseconds on a healthy run, so
	// its size costs nothing and only a genuinely wedged manager ever pays
	// it — with a t.Fatalf at the end.
	managerTimeout = 5 * time.Second

	// handoffPollInterval is how often the harness re-checks a handoff it
	// is waiting on (a chain notification being taken off its buffer, a
	// confirmation height being persisted). Both land within microseconds
	// of the event that triggers them, and these waits sit on the path of
	// every mined block, so the interval is kept far below the 200ms of the
	// shared wait.Predicate helpers.
	handoffPollInterval = 50 * time.Microsecond
)

// reader is a cursor over the raw fuzz input.
type reader struct {
	data []byte
	pos  int
}

// u8 consumes a single byte from the stream.
func (r *reader) u8() (byte, bool) {
	if r.pos >= len(r.data) {
		return 0, false
	}
	b := r.data[r.pos]
	r.pos++

	return b, true
}

// u32 consumes four bytes from the stream, decoded as a big-endian uint32.
func (r *reader) u32() (uint32, bool) {
	if r.pos+4 > len(r.data) {
		return 0, false
	}
	v := binary.BigEndian.Uint32(r.data[r.pos : r.pos+4])
	r.pos += 4

	return v, true
}

// startParams holds the parameters consumed from the byte stream that follow
// an EvStartAsLocalFunder / EvStartAsLocalFundee event.
type startParams struct {
	// localFeats and remoteFeats are the resolved feature sets each side
	// advertises; chanType is the explicit channel type the funder requests
	// (nil when it names none); zeroConf marks a zero-conf flow;
	// tamper requests one of the adversarial channel_type variants.
	localFeats  []lnwire.FeatureBit
	remoteFeats []lnwire.FeatureBit
	chanType    *lnwire.ChannelType
	zeroConf    bool
	tamper      chanTypeTamper

	remoteConfs    byte
	remoteCsvDelay uint16
	private        bool
	wumbo          bool
	fundingAmt     btcutil.Amount
	pushByte       byte

	// pushTooLarge requests the adversarial variant where the counterparty
	// (as funder) sends an OpenChannel whose push amount exceeds the
	// funding amount, so the SUT (fundee) must reject it
	// (ErrPushAmountTooLarge). It is only acted on in handleAcceptChannel;
	// the SUT as funder always proposes a valid push.
	pushTooLarge bool
}

// maxChanSize returns the MaxChanSize a manager must run with to accept the
// given channel class.
func maxChanSize(wumbo bool) btcutil.Amount {
	if wumbo {
		return MaxBtcFundingAmountWumbo
	}

	return MaxBtcFundingAmount
}

// fundingAmount maps a 4-byte fuzz value; There is no too-small tier:
// the manager does not enforce a minimum (MinChanFundingSize lives in the
// wallet/rpc layer, which is mocked here).
func fundingAmount(raw uint32, wumbo bool) btcutil.Amount {
	maxAmt := maxChanSize(wumbo)

	switch raw % 64 {
	case 0, 1:
		// ~3%: just above this class's maximum -> rejected
		// (ErrChanTooLarge).
		return maxAmt + 1

	default:
		span := uint64(maxAmt-MinChanFundingSize) + 1

		return MinChanFundingSize + btcutil.Amount(uint64(raw)%span)
	}
}

// pushAmount maps a fuzz byte onto the amount the funder pushes to the fundee
// at open, as a fraction of the channel capacity.
func pushAmount(fundingAmt btcutil.Amount, b byte) btcutil.Amount {
	if b%4 == 0 {
		return 0 // ~25%: single-funder, no push (boundary).
	}

	// 1..50% of the capacity — always leaves the funder solvent.
	pct := btcutil.Amount(1 + uint64(b)%50)

	return fundingAmt * pct / 100
}

// clampFundingAmt clamps a funding amount into the valid range for its channel
// class, so the SUT as funder always proposes a consistent size.
func clampFundingAmt(amt btcutil.Amount, wumbo bool) btcutil.Amount {
	switch maxAmt := maxChanSize(wumbo); {
	case amt < MinChanFundingSize:
		return MinChanFundingSize
	case amt > maxAmt:
		return maxAmt
	default:
		return amt
	}
}

// readStartParams consumes the fixed-width parameter block that follows a start
// event, in the following order:
//
//	[localFeatures:1][remoteFeatures:1][remoteConfs:1][private:1][wumbo:1]
//	[chanType:1][fundingAmt:4][pushPercent:1][csv:1]
//
// lockedWumbo is the channel class an earlier start event already fixed for
// this input, or nil if this is the first one.
//
// It returns ok=false if the stream is exhausted before the whole block is
// read, in which case the start event is dropped.
func (r *reader) readStartParams(lockedWumbo *bool) (startParams, bool) {
	localFeatures, ok := r.u8()
	if !ok {
		return startParams{}, false
	}
	remoteFeatures, ok := r.u8()
	if !ok {
		return startParams{}, false
	}
	remoteConfs, ok := r.u8()
	if !ok {
		return startParams{}, false
	}
	privateByte, ok := r.u8()
	if !ok {
		return startParams{}, false
	}
	wumboByte, ok := r.u8()
	if !ok {
		return startParams{}, false
	}
	chanTypeByte, ok := r.u8()
	if !ok {
		return startParams{}, false
	}
	amt, ok := r.u32()
	if !ok {
		return startParams{}, false
	}
	pushByte, ok := r.u8()
	if !ok {
		return startParams{}, false
	}
	csvByte, ok := r.u8()
	if !ok {
		return startParams{}, false
	}

	// Only a small fraction of channels are wumbo, so the fuzzer mostly
	// exercises standard channels while still reaching the wumbo paths.
	// Later flows inherit the class the first one settled on, since the
	// SUT's limit is fixed for the whole input.
	wumbo := wumboByte%8 == 1
	if lockedWumbo != nil {
		wumbo = *lockedWumbo
	}

	// Resolve the channel-type preset (mostly implicit; a minority pick an
	// explicit type). The two feature bytes only matter for the implicit
	// case; explicit presets carry their own matched feature sets.
	cfg := resolveChanType(chanTypeByte, localFeatures, remoteFeatures)
	localFeats, remoteFeats := cfg.localFeats, cfg.remoteFeats

	// A wumbo channel needs both peers to advertise large-channel support.
	if wumbo {
		localFeats = append(localFeats, lnwire.WumboChannelsOptional)
		remoteFeats = append(remoteFeats, lnwire.WumboChannelsOptional)
	}

	return startParams{
		localFeats:     localFeats,
		remoteFeats:    remoteFeats,
		chanType:       cfg.chanType,
		zeroConf:       cfg.zeroConf,
		tamper:         cfg.tamper,
		remoteConfs:    remoteConfs,
		remoteCsvDelay: csvDelay(csvByte),
		// Taproot channels must be private, so the preset can force it.
		private: privateByte&1 == 1 || cfg.forcePriv,
		wumbo:   wumbo,
		// Full-range amount; the funder side clamps it when the SUT
		// funds.
		fundingAmt: fundingAmount(amt, wumbo),
		pushByte:   pushByte,
		// A small minority forge an over-100% push (only when the
		// counterparty funds); otherwise pushByte drives a valid push.
		pushTooLarge: pushByte%16 == 1,
	}, true
}

func pickInteraction(b byte) PeerInteraction {
	// 96% of interactions follow the expected flow.
	switch b % 100 {
	case 96:
		return ModeAdversarialReplay
	case 97:
		return ModeAdversarialOutOfOrder
	case 98:
		return ModeAdversarialWrongPeer
	case 99:
		return ModeAdversarialWrongChanID
	default:
		return ModeAdvanceExpected
	}
}

type fuzzFSM struct {
	t     *testing.T
	local *testNode

	// flows is a map of peer IDs to their corresponding flowState.
	flows map[peerID]*flowState

	// flowOrder lists the peer IDs of all funding flows in creation order.
	flowOrder []peerID

	// currentPeerID is the ID of the currently active peer.
	currentPeerID peerID

	// nextPeerID is used to generate unique peer IDs for new peers.
	nextPeerID peerID

	// remotesByPubKey maps a counterparty identity to its per-flow handle,
	// so the SUT can resolve the peer channel_ready waits on. It is a
	// sync.Map because the manager resolves it from background goroutines
	// (advanceFundingState) concurrently with the main loop creating flows.
	remotesByPubKey sync.Map

	// confNotifier is the SUT's txid-keyed confirmation notifier, letting
	// handleConfFundChannelTx confirm exactly the current flow's funding tx
	// even though the SUT is shared across flows.
	confNotifier *fuzzConfNotifier

	// peerLink models which counterparties the SUT currently believes to be
	// connected, backing handleDisconnectPeer / handleReconnectPeer.
	peerLink *peerLink

	// acceptor is the SUT's channel acceptor, installed once and never
	// swapped; flows register their parameters with it by pending channel
	// id.
	acceptor *flowAcceptor

	// wumbo is the channel class every flow of this input runs with, and
	// chanClassSet reports whether the first start event has fixed it yet.
	// It is a property of the input rather than of a flow because the
	// manager's MaxChanSize is node-level configuration in production, set
	// once from --protocol.wumbo-channels.
	wumbo        bool
	chanClassSet bool

	// fuzzStats keeps track of various statistics during the fuzzing
	// process.
	fuzzStats fuzzStats
}

// peerLink models the SUT's view of its counterparties' connectivity. Nothing
// in the funding manager polls for it: the manager asks to be told when a peer
// comes online (NotifyWhenOnline) and blocks in waitForPeerOnline until it is,
// so withholding that callback is what makes a disconnect observable. It only
// gates the SUT's channel_ready, which is deliberate — every earlier message in
// the handshake just cancels the flow if the peer goes away.
type peerLink struct {
	mu      sync.Mutex
	offline map[[33]byte]bool
	waiters map[[33]byte][]chan<- lnpeer.Peer

	// parked signals, per peer, that the SUT has just parked a waiter on
	// it. It is the observable the harness synchronizes on before asserting
	// that a channel confirmed with its peer away stays closed: the park is
	// the exact point the SUT reached sendChannelReady and stopped, so once
	// it is signalled the assertion is a check rather than a wait. Each
	// channel is buffered so the park never blocks the manager.
	parked map[[33]byte]chan struct{}
}

func newPeerLink() *peerLink {
	return &peerLink{
		offline: make(map[[33]byte]bool),
		waiters: make(map[[33]byte][]chan<- lnpeer.Peer),
		parked:  make(map[[33]byte]chan struct{}),
	}
}

// parkSignal returns the (created-on-demand) channel on which a park for pk is
// announced.
func (l *peerLink) parkSignal(pk [33]byte) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.parkSignalUnsafe(pk)
}

// parkSignalUnsafe is parkSignal without the locking; the caller must hold mu.
func (l *peerLink) parkSignalUnsafe(pk [33]byte) chan struct{} {
	sig, ok := l.parked[pk]
	if !ok {
		sig = make(chan struct{}, maxFlows)
		l.parked[pk] = sig
	}

	return sig
}

// awaitPark waits for the SUT to park a waiter on pk, reporting whether one
// arrived within the timeout.
func (l *peerLink) awaitPark(pk [33]byte) bool {
	select {
	case <-l.parkSignal(pk):
		return true

	case <-time.After(managerTimeout):
		return false
	}
}

// notifyWhenOnline backs the SUT's NotifyWhenOnline: it hands the peer over
// straight away while that peer is connected, and parks the waiter until
// reconnect otherwise.
func (l *peerLink) notifyWhenOnline(pk [33]byte, peerChan chan<- lnpeer.Peer,
	peer lnpeer.Peer) {

	l.mu.Lock()
	if l.offline[pk] {
		l.waiters[pk] = append(l.waiters[pk], peerChan)

		// Announce the park to whoever is waiting to assert on it,
		// without ever blocking the manager goroutine that is parking.
		select {
		case l.parkSignalUnsafe(pk) <- struct{}{}:
		default:
		}
		l.mu.Unlock()

		return
	}
	l.mu.Unlock()

	// Deliver with the lock released: the manager's waiter is buffered, but
	// a blocking send while holding the lock would wedge every other flow.
	peerChan <- peer
}

// disconnect marks a peer offline, so subsequent notifyWhenOnline calls park
// instead of resolving.
func (l *peerLink) disconnect(pk [33]byte) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.offline[pk] = true
}

// reconnect marks a peer online again and releases every waiter parked on it,
// reporting how many were released.
func (l *peerLink) reconnect(pk [33]byte, peer lnpeer.Peer) int {
	l.mu.Lock()
	parked := l.waiters[pk]
	delete(l.waiters, pk)
	delete(l.offline, pk)
	l.mu.Unlock()

	for _, peerChan := range parked {
		peerChan <- peer
	}

	return len(parked)
}

// ntfnChans bundles the channels a single confirmation registration hands back.
// waitForFundingConfirmation selects over all three, so keying the whole bundle
// by txid lets the harness aim a confirmation, a partial-confirmation update or
// a reorg at exactly one funding tx.
type ntfnChans struct {
	confirmed chan *chainntnfs.TxConfirmation
	updates   chan chainntnfs.TxUpdateInfo
	negConf   chan int32
}

// fuzzConfNotifier wraps the per-manager mock notifier so a chain notification
// can be targeted at a specific funding tx. The stock mock hands every
// registrant the SAME channels, so with the SUT shared across flows a
// notification would wake an arbitrary waiter. Keying them by txid lets
// handleConfFundChannelTx and handleReorg act on exactly the intended flow.
type fuzzConfNotifier struct {
	*mockNotifier

	mu    sync.Mutex
	ntfns map[chainhash.Hash]*ntfnChans
}

func newFuzzConfNotifier(base *mockNotifier) *fuzzConfNotifier {
	return &fuzzConfNotifier{
		mockNotifier: base,
		ntfns:        make(map[chainhash.Hash]*ntfnChans),
	}
}

// chansFor returns the (created-on-demand) notification channels for txid. The
// buffer of one on each lets a notification delivered before the manager
// registers still be picked up when it does — removing the register/notify
// race.
func (n *fuzzConfNotifier) chansFor(txid chainhash.Hash) *ntfnChans {
	n.mu.Lock()
	defer n.mu.Unlock()

	chans, ok := n.ntfns[txid]
	if !ok {
		chans = &ntfnChans{
			confirmed: make(chan *chainntnfs.TxConfirmation, 1),
			updates:   make(chan chainntnfs.TxUpdateInfo, 1),
			negConf:   make(chan int32, 1),
		}
		n.ntfns[txid] = chans
	}

	return chans
}

// RegisterConfirmationsNtfn overrides the embedded mock's method (the whole
// point of the wrapper): the manager calls it through the ChainNotifier
// interface from waitForFundingConfirmation, and it hands back the txid-keyed
// channels that confirm(), update() and reorg() later deliver on. Without this
// override, embedding would promote the mock's version and every flow would
// share one set of channels again.
func (n *fuzzConfNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	_ []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent,
	error) {

	chans := n.chansFor(*txid)

	return &chainntnfs.ConfirmationEvent{
		Confirmed:    chans.confirmed,
		Updates:      chans.updates,
		NegativeConf: chans.negConf,
		Cancel:       func() {},
	}, nil
}

// confirm delivers a single confirmation for txid, at the given block height,
// to whichever waiter has registered (or will register) for it.
func (n *fuzzConfNotifier) confirm(txid chainhash.Hash, tx *wire.MsgTx,
	height uint32) {

	select {
	case n.chansFor(txid).confirmed <- &chainntnfs.TxConfirmation{
		Tx: tx, BlockHeight: height,
	}:
	default:
	}
}

// update delivers a partial-confirmation update: txid has landed in the block
// at height but has not yet reached the depth the channel requires, so the
// manager records the height without opening the channel. It reports whether
// the waiter accepted the update.
func (n *fuzzConfNotifier) update(txid chainhash.Hash, height,
	confsLeft uint32) bool {

	chans := n.chansFor(txid)

	select {
	case chans.updates <- chainntnfs.TxUpdateInfo{
		BlockHeight: height, NumConfsLeft: confsLeft,
	}:

	case <-time.After(managerTimeout):
		return false
	}

	return awaitPickup(chans.updates)
}

// reorg notifies that txid was reorged out of the chain from the given depth.
// It reports whether the waiter accepted the notification.
func (n *fuzzConfNotifier) reorg(txid chainhash.Hash, depth int32) bool {
	chans := n.chansFor(txid)

	select {
	case chans.negConf <- depth:

	case <-time.After(managerTimeout):
		return false
	}

	return awaitPickup(chans.negConf)
}

// awaitPickup waits until the manager's confirmation goroutine has taken the
// notification just queued on ch, reporting whether it did so in time.
//
// The notification channels are buffered by one so a notification delivered
// before the manager registers is not lost, which means a successful send only
// proves the notification is queued — not that anyone has seen it. Waiting for
// that buffer to drain turns the send into a real handoff, and that is what
// lets the assertions which follow a chain event be immediate checks rather
// than fixed sleeps: by the time they run, the manager is already handling the
// notification. Nothing else ever reads these channels, so an emptied buffer
// can only mean the manager took the value.
func awaitPickup[T any](ch chan T) bool {
	deadline := time.Now().Add(managerTimeout)
	for len(ch) > 0 {
		if time.Now().After(deadline) {
			return false
		}

		time.Sleep(handoffPollInterval)
	}

	return true
}

// newFuzzFSM initializes and returns a new fuzz finite state machine (FSM)
// instance.
func newFuzzFSM(t *testing.T) *fuzzFSM {
	// Redirect all t.TempDir() calls to /dev/shm (tmpfs) so that the
	// channeldb bbolt files are kept in RAM rather than written to disk.
	if runtime.GOOS != "linux" {
		t.Skipf("Skipping fuzz/scenario test on non-Linux OS: %s",
			runtime.GOOS)
	}
	t.Setenv("TMPDIR", "/dev/shm")

	// Alice's MaxChanSize is set per flow (in the start handlers) according
	// to that flow's channel class, since she is shared across flows.
	local, err := createTestFundingManager(
		t, alicePrivKey, aliceAddr, t.TempDir(),
	)
	require.NoError(t, err, "failed creating fundingManager")

	// The manager and the wallet behind it each run goroutines that only
	// exit on Stop. Under `go test` these would live until the process
	// ends, so without this the fuzzer leaks a manager per iteration until
	// the worker is killed. Registered here so it runs before the cleanup
	// channeldb installed during creation, which closes the database out
	// from under those same goroutines.
	t.Cleanup(func() {
		close(local.shutdownChannel)
		require.NoError(t, local.fundingMgr.Stop())
		require.NoError(t, local.fundingMgr.cfg.Wallet.Shutdown())
	})

	fuzzStats := fuzzStats{
		peerInteractions: make(map[PeerInteraction]uint64),
	}
	fsm := &fuzzFSM{
		t:             t,
		local:         local,
		flows:         make(map[peerID]*flowState),
		currentPeerID: 0,
		nextPeerID:    1,
		fuzzStats:     fuzzStats,
		peerLink:      newPeerLink(),
		acceptor:      &flowAcceptor{},
	}

	// Install the SUT's acceptor once. From here on flows register their
	// terms with it by pending channel id rather than swapping it out, so
	// nothing writes to cfg while the manager is running.
	local.fundingMgr.cfg.OpenChannelPredicate = fsm.acceptor

	// The SUT talks to a different counterparty per flow, so resolve the
	// peer that channel_ready waits on by the requested identity, and park
	// the waiter if that peer is currently disconnected.
	local.fundingMgr.cfg.NotifyWhenOnline = func(pk [33]byte,
		peerChan chan<- lnpeer.Peer) {

		peer := fsm.peerByPubKey(pk)
		if peer == nil {
			return
		}

		fsm.peerLink.notifyWhenOnline(pk, peerChan, peer)
	}

	// Swap in a txid-keyed confirmation notifier so handleConfFundChannelTx
	// can confirm a specific flow on the shared SUT. Registration happens
	// lazily per flow (after broadcast), well after this point, so the swap
	// is safe.
	fsm.confNotifier = newFuzzConfNotifier(local.mockNotifier)
	local.fundingMgr.cfg.Notifier = fsm.confNotifier

	return fsm
}

// peerByPubKey returns the counterparty handle registered for the given
// identity, or nil if none matches.
func (f *fuzzFSM) peerByPubKey(pk [33]byte) lnpeer.Peer {
	if v, ok := f.remotesByPubKey.Load(pk); ok {
		if peer, ok := v.(lnpeer.Peer); ok {
			return peer
		}
	}

	return nil
}

// consume drives the FSM by interpreting data as a stream of events.
// EvStart and EvPeerInteraction consume their own parameter bytes.
func (f *fuzzFSM) consume(data []byte) {
	// The loop below returns from several places as the input runs out, and
	// a failing assertion unwinds through here too (t.Fatalf runs deferred
	// calls), so defer the summary rather than repeating it at each exit.
	defer f.logStats()

	r := &reader{data: data}
	for {
		evByte, ok := r.u8()
		if !ok {
			return
		}

		event := Event(evByte) % NumEvents

		switch event {
		case EvStartAsLocalFunder, EvStartAsLocalFundee:
			// This will be the flow f.currentPeerID+1
			f.t.Logf("flow %d: received Start event",
				f.currentPeerID+1)

			params, ok := r.readStartParams(f.lockedWumbo())
			if !ok {
				return
			}
			f.applyStart(event, params)

		case EvSwitchFlow:
			f.t.Logf("flow %d: received SwitchFlow event",
				f.currentPeerID)

			b, ok := r.u8()
			if !ok {
				return
			}
			f.switchFlow(b)

		case EvPeerInteraction:
			f.t.Logf("flow %d: received PeerInteraction event",
				f.currentPeerID)

			b, ok := r.u8()
			if !ok {
				return
			}
			f.applyEvent(event, pickInteraction(b))

		case ConfFundChannelTx:
			f.t.Logf("flow %d: received ConfFundChannelTx event",
				f.currentPeerID)

			// A selector byte chooses the channel_ready ordering:
			// normal (both sides confirm, then exchange) or early
			// (the counterparty's channel_ready reaches the SUT
			// before its own confirmation).
			b, ok := r.u8()
			if !ok {
				return
			}
			f.local.handleConfFundChannelTx(f, b%32 == 1)

		default:
			f.applyEvent(event, ModeAdvanceExpected)
		}
	}
}

// logStats reports what the input actually exercised..
func (f *fuzzFSM) logStats() {
	s := &f.fuzzStats

	f.t.Logf("stats: flows=%d (as funder=%d, as fundee=%d) "+
		"confirmations=%d reorgs=%d disconnects=%d reconnects=%d",
		len(f.flowOrder), s.startedAsLocalFunder,
		s.startedAsLocalFundee, s.confFundChannelTx, s.reorg,
		s.disconnectPeer, s.reconnectPeer)

	// Name each mode rather than ranging over the map: the map order varies
	// from run to run, and a mode the input never reached must still show
	// up as a zero.
	f.t.Logf("stats: peer interactions: expected=%d replay=%d "+
		"out-of-order=%d wrong-peer=%d wrong-chan-id=%d",
		s.peerInteractions[ModeAdvanceExpected],
		s.peerInteractions[ModeAdversarialReplay],
		s.peerInteractions[ModeAdversarialOutOfOrder],
		s.peerInteractions[ModeAdversarialWrongPeer],
		s.peerInteractions[ModeAdversarialWrongChanID])
}

// featureBitsFromByte maps a fuzz byte onto the set of feature bits that
// influence channel-type negotiation. Whether the pair signals
// option_channel_type is the caller's to decide rather than a bit of b, because
// it is not a per-node coin flip in practice: lnd carries
// ExplicitChannelTypeRequired in SetInit unconditionally
// (feature/default_sets.go), and being a *required* bit, a peer that does not
// understand it cannot complete the init handshake at all. Two nodes therefore
// either both signal it or neither does — the asymmetric case a per-side toggle
// spends half its inputs on cannot arise against a current lnd.
func featureBitsFromByte(b byte, explicitChanType bool) []lnwire.FeatureBit {
	candidates := []lnwire.FeatureBit{
		lnwire.StaticRemoteKeyOptional,
		lnwire.AnchorsZeroFeeHtlcTxOptional,
		lnwire.ScidAliasOptional,
		lnwire.ZeroConfOptional,
		lnwire.SimpleTaprootChannelsOptionalFinal,
	}

	var (
		bits      []lnwire.FeatureBit
		hasSRK    bool
		hasAnchor bool
	)
	for i, bit := range candidates {
		if b&(1<<uint(i)) == 0 {
			continue
		}
		bits = append(bits, bit)

		switch bit {
		case lnwire.StaticRemoteKeyOptional:
			hasSRK = true
		case lnwire.AnchorsZeroFeeHtlcTxOptional:
			hasAnchor = true
		}
	}

	if explicitChanType {
		bits = append(bits, lnwire.ExplicitChannelTypeOptional)
	}

	// Anchors depends on static-remote-key; real nodes never advertise one
	// without the other. The mismatch matters here: the default selection
	// takes anchors on the Anchors bit alone but builds a type that also
	// requires static-remote-key, which the fundee re-validates. An
	// anchors-without-SRK set would therefore fail negotiation
	// (errUnsupportedChannelType) instead of opening, starving deep-state
	// coverage. Enforce the dependency to keep these opens graceful; the
	// deliberate errUnsupportedChannelType path is still covered by the
	// tamperOpenUnsupported variant.
	if hasAnchor && !hasSRK {
		bits = append(bits, lnwire.StaticRemoteKeyOptional)
	}

	return bits
}

// explicitChanType builds an explicit channel type carrying the given Required
// feature bits.
func explicitChanType(bits ...lnwire.FeatureBit) *lnwire.ChannelType {
	ct := lnwire.ChannelType(*lnwire.NewRawFeatureVector(bits...))

	return &ct
}

// chanTypeTamper selects one of the channel_type conformance variants the
// corpus can request. Each one rewrites a message the counterparty emits, the
// way a buggy or malicious peer would, so that a BOLT-2 rule the SUT is
// required to enforce is actually put to the test. Every variant is a no-op in
// the direction it does not apply to, so the same preset stays valid whichever
// side of the flow the corpus made the SUT.
type chanTypeTamper uint8

const (
	// tamperNone is the honest flow: nothing is rewritten.
	tamperNone chanTypeTamper = iota

	// tamperOpenUnsupported has the counterparty (as funder) send an
	// OpenChannel whose channel type is inconsistent with the negotiated
	// features, so the SUT (fundee) must reject it with
	// errUnsupportedChannelType.
	tamperOpenUnsupported

	// tamperOpenStripType has the counterparty (as funder) send an
	// OpenChannel carrying no channel_type at all.
	tamperOpenStripType

	// tamperAcceptStripType has the counterparty (as fundee) drop the
	// channel_type from the AcceptChannel that should echo the SUT's
	// proposal.
	tamperAcceptStripType

	// tamperAcceptWrongType has the counterparty (as fundee) echo back some
	// channel_type other than the one the SUT proposed.
	tamperAcceptWrongType

	// tamperOpenTypeUnnegotiated has the counterparty (as funder) send an
	// OpenChannel that names a channel type without either side signalling
	// option_channel_type.
	//
	// TODO(MPins): PR 11064 has the default selection return a type
	// regardless of the bit, so the funder already sends this exact type
	// and the injection becomes a no-op. Repurpose the variant to strip
	// the type instead, which tests that the new rule really is
	// unconditional, or drop it — the feature set it needs is covered by
	// the legacy minority in resolveChanType's default branch.
	tamperOpenTypeUnnegotiated
)

// String names the variant for the log lines and failure messages, which are
// the only trace a fuzz failure leaves behind.
func (t chanTypeTamper) String() string {
	switch t {
	case tamperNone:
		return "none"

	case tamperOpenUnsupported:
		return "open-unsupported-type"

	case tamperOpenStripType:
		return "open-missing-type"

	case tamperAcceptStripType:
		return "accept-missing-type"

	case tamperAcceptWrongType:
		return "accept-wrong-type"

	case tamperOpenTypeUnnegotiated:
		return "open-type-unnegotiated"

	default:
		return "unknown"
	}
}

// chanTypeConfig is a curated, internally-consistent channel-type configuration
// the corpus can select.
type chanTypeConfig struct {
	localFeats  []lnwire.FeatureBit
	remoteFeats []lnwire.FeatureBit
	chanType    *lnwire.ChannelType
	zeroConf    bool
	forcePriv   bool

	// tamper marks the adversarial variant this preset carries, if any.
	tamper chanTypeTamper
}

// resolveChanType maps a selector byte onto a channel-type configuration. Half
// the sixteen slots leave InitFundingMsg.ChannelType unset — the common case
// and the node then picks a default out of the features both sides signal,
// still sending it explicitly.
//
// Six slots name an explicit type from a curated list, every one a combination
// explicitNegotiateCommitmentType accepts.
//
// The two left over hold the adversarial variants, which deliberately break a
// BOLT-2 channel_type rule the SUT has to enforce, and mostly end their flow
// early by design.
func resolveChanType(sel, localByte, remoteByte byte) chanTypeConfig {
	switch sel % 16 {
	// Tweakless (static remote key).
	case 1:
		feats := []lnwire.FeatureBit{
			lnwire.ExplicitChannelTypeOptional,
			lnwire.StaticRemoteKeyOptional,
		}

		return chanTypeConfig{
			localFeats:  feats,
			remoteFeats: feats,
			chanType: explicitChanType(
				lnwire.StaticRemoteKeyRequired,
			),
		}

	// Anchors.
	case 2:
		feats := []lnwire.FeatureBit{
			lnwire.ExplicitChannelTypeOptional,
			lnwire.StaticRemoteKeyOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
		}

		return chanTypeConfig{
			localFeats:  feats,
			remoteFeats: feats,
			chanType: explicitChanType(
				lnwire.AnchorsZeroFeeHtlcTxRequired,
				lnwire.StaticRemoteKeyRequired,
			),
		}

	// Anchors + scid-alias.
	case 3:
		feats := []lnwire.FeatureBit{
			lnwire.ExplicitChannelTypeOptional,
			lnwire.StaticRemoteKeyOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.ScidAliasOptional,
		}

		return chanTypeConfig{
			localFeats:  feats,
			remoteFeats: feats,
			chanType: explicitChanType(
				lnwire.ScidAliasRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
				lnwire.StaticRemoteKeyRequired,
			),
		}

	// Anchors + zero-conf.
	case 4:
		feats := []lnwire.FeatureBit{
			lnwire.ExplicitChannelTypeOptional,
			lnwire.StaticRemoteKeyOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.ZeroConfOptional,
			lnwire.ScidAliasOptional,
		}

		return chanTypeConfig{
			localFeats:  feats,
			remoteFeats: feats,
			chanType: explicitChanType(
				lnwire.ZeroConfRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
				lnwire.StaticRemoteKeyRequired,
			),
			zeroConf: true,
		}

	// Simple taproot (must be private).
	case 5:
		feats := []lnwire.FeatureBit{
			lnwire.ExplicitChannelTypeOptional,
			lnwire.SimpleTaprootChannelsOptionalFinal,
		}

		return chanTypeConfig{
			localFeats:  feats,
			remoteFeats: feats,
			chanType: explicitChanType(
				lnwire.SimpleTaprootChannelsRequiredFinal,
			),
			forcePriv: true,
		}

	// Simple taproot + scid-alias (must be private).
	case 6:
		feats := []lnwire.FeatureBit{
			lnwire.ExplicitChannelTypeOptional,
			lnwire.SimpleTaprootChannelsOptionalFinal,
			lnwire.ScidAliasOptional,
		}

		return chanTypeConfig{
			localFeats:  feats,
			remoteFeats: feats,
			chanType: explicitChanType(
				lnwire.SimpleTaprootChannelsRequiredFinal,
				lnwire.ScidAliasRequired,
			),
			forcePriv: true,
		}

	// Mismatched type: empty features on both sides, no explicit type for
	// the funder. When the counterparty funds (handleAcceptChannel), its
	// OpenChannel is rewritten to carry an explicit type the SUT's (empty)
	// features cannot support, exercising the SUT's rejection
	// errUnsupportedChannelType.
	case 7:
		return chanTypeConfig{
			tamper: tamperOpenUnsupported,
		}

	// BOLT-2 channel_type conformance probes. The high nibble of the
	// selector picks the variant, so the whole family costs one slot of the
	// low-nibble budget and leaves the deep flows their share of the
	// corpus.
	case 8:
		feats := []lnwire.FeatureBit{
			lnwire.ExplicitChannelTypeOptional,
			lnwire.StaticRemoteKeyOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
		}

		cfg := chanTypeConfig{
			localFeats:  feats,
			remoteFeats: feats,
			chanType: explicitChanType(
				lnwire.AnchorsZeroFeeHtlcTxRequired,
				lnwire.StaticRemoteKeyRequired,
			),
		}

		switch (sel >> 4) % 4 {
		// An OpenChannel reaching the SUT with no channel_type at all..
		case 0:
			cfg.chanType = nil
			cfg.tamper = tamperOpenStripType

		// The SUT proposes the base type as funder, and the echo comes
		// back empty.
		case 1:
			cfg.tamper = tamperAcceptStripType

		// The echo comes back naming a type that was never proposed.
		case 2:
			cfg.tamper = tamperAcceptWrongType

		// The one variant that drops option_channel_type, and only the
		// peer drops it: the SUT keeps the bit, lnd advertising it
		// unconditionally, so a node without it is one lnd cannot be.
		// hasFeatures wants the bit on both sides, so the peer alone
		// is enough to take the flow down the path where no explicit
		// negotiation happens. The features left are exactly those the
		// injected type is built from, so the type is supportable and
		// the flow turns on channel_type alone — on the SUT's echo
		// when she is the fundee, and on her own open_channel when she
		// is the funder, where the injection never runs.
		default:
			cfg.remoteFeats = []lnwire.FeatureBit{
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
			}
			cfg.chanType = nil
			cfg.tamper = tamperOpenTypeUnnegotiated
		}

		return cfg

	// Note this is not implicit negotiation on the wire: with the bit up on
	// both sides the funder picks a default type and sends it explicitly.
	default:
		explicitChanType := (sel>>4)%8 != 0

		return chanTypeConfig{
			localFeats: featureBitsFromByte(
				localByte, explicitChanType,
			),
			remoteFeats: featureBitsFromByte(
				remoteByte, explicitChanType,
			),
		}
	}
}

// mismatchChanType is the explicit channel type injected into the
// counterparty's OpenChannel for the mismatched-type variant.
func mismatchChanType() *lnwire.ChannelType {
	return explicitChanType(lnwire.SimpleTaprootChannelsRequiredFinal)
}

// wrongChanType returns a valid channel type that is never equal to proposed,
// for the variant where the counterparty echoes back a type the SUT never asked
// for.
func wrongChanType(proposed *lnwire.ChannelType) *lnwire.ChannelType {
	taproot := explicitChanType(lnwire.SimpleTaprootChannelsRequiredFinal)
	if proposed == nil {
		return taproot
	}

	proposedVec := lnwire.RawFeatureVector(*proposed)
	if proposedVec.IsSet(lnwire.SimpleTaprootChannelsRequiredFinal) {
		return explicitChanType(
			lnwire.AnchorsZeroFeeHtlcTxRequired,
			lnwire.StaticRemoteKeyRequired,
		)
	}

	return taproot
}

// chanTypeEqual compares two possibly-absent channel types by feature vector,
// the same way the funding manager compares a proposal against its echo. Two
// absent types count as equal: a flow that never named a type has nothing to
// echo.
func chanTypeEqual(proposed, echoed *lnwire.ChannelType) bool {
	if proposed == nil || echoed == nil {
		return proposed == nil && echoed == nil
	}

	proposedFeatures := lnwire.RawFeatureVector(*proposed)
	echoedFeatures := lnwire.RawFeatureVector(*echoed)

	return proposedFeatures.Equals(&echoedFeatures)
}

// chanTypeString renders a channel type by the names of the bits it sets. A
// fuzz failure leaves nothing behind but its message, and the raw feature
// vector prints as an unordered map of numbers.
func chanTypeString(ct *lnwire.ChannelType) string {
	if ct == nil {
		return "<absent>"
	}

	raw := lnwire.RawFeatureVector(*ct)
	fv := lnwire.NewFeatureVector(&raw, lnwire.Features)

	bits := make([]lnwire.FeatureBit, 0, len(fv.Features()))
	for bit := range fv.Features() {
		bits = append(bits, bit)
	}
	slices.Sort(bits)

	names := make([]string, 0, len(bits))
	for _, bit := range bits {
		names = append(names, fv.Name(bit))
	}

	return "[" + strings.Join(names, ",") + "]"
}

// sutSignals reports whether the SUT advertises the given feature bit. Only her
// own signalling can excuse her from a sender requirement — what the peer does
// or does not advertise is the peer's business.
func sutSignals(params startParams, bit lnwire.FeatureBit) bool {
	return slices.Contains(params.localFeats, bit)
}

// confDepth maps a fuzz byte onto a channel confirmation depth (the fundee's
// MinAcceptDepth). A depth of 0 means "use the manager's default", not a true
// zero-conf channel.
func confDepth(b byte) uint16 {
	const maxConfs = uint16(chainntnfs.MaxNumConfs)

	if b >= 250 {
		return maxConfs + 1 + uint16(b-250) // 145..150, all rejected
	}

	return uint16(b) % (maxConfs + 1) // 0..144
}

// csvDelay maps a fuzz byte onto the CSV delay (to_self_delay) the counterparty
// imposes on the SUT, so the SUT is the side that validates it against its
// MaxLocalCSVDelay.
func csvDelay(b byte) uint16 {
	const maxCSV = uint32(defaultMaxLocalCSVDelay)

	switch b % 32 {
	case 0:
		return 0 // ~3%: counterparty falls back to its scaled default

	case 1:
		return uint16(maxCSV) + 1 // ~3%: too large -> SUT rejects

	default:
		// Scale the byte across [1, maxCSV].
		return uint16(1 + uint32(b)*(maxCSV-1)/255)
	}
}

// fundeeAcceptor accepts every channel as the fundee, driving parameters from
// the corpus: either a zero-conf acceptance or a forced MinAcceptDepth, plus a
// CSV delay it imposes on the counterparty (which that side validates). All
// other fields are left zero, which the funding manager reads as "use the
// default".
type fundeeAcceptor struct {
	zeroConf bool
	depth    uint16
	csv      uint16
}

func (a *fundeeAcceptor) Accept(_ *chanacceptor.ChannelAcceptRequest,
) *chanacceptor.ChannelAcceptResponse {

	// A zero-conf channel must use min_depth 0, so we never set a depth
	// alongside the zero-conf flag.
	if a.zeroConf {
		return &chanacceptor.ChannelAcceptResponse{
			ZeroConf: true,
			CSVDelay: a.csv,
		}
	}

	return &chanacceptor.ChannelAcceptResponse{
		MinAcceptDepth: a.depth,
		CSVDelay:       a.csv,
	}
}

// flowAcceptor is the SUT's channel acceptor. Unlike the counterparties', which
// are per-flow managers and can be configured at construction, the SUT is
// shared, so its acceptor has to answer for whichever flow the OpenChannel in
// hand belongs to. It resolves that from the message's pending channel id.
type flowAcceptor struct {
	// params maps a pending channel id to the terms the flow that owns it
	// negotiated. It is a sync.Map because the manager reads it from the
	// coordinator goroutine while the main loop registers new flows.
	params sync.Map
}

// expect registers the terms to answer with when the OpenChannel carrying this
// pending channel id arrives. It must be called before that message is handed
// to the manager.
func (a *flowAcceptor) expect(id [32]byte, terms *fundeeAcceptor) {
	a.params.Store(id, terms)
}

// Accept answers with the registered flow's terms, or with the manager's
// defaults for a pending channel id we never set up — an adversarial probe,
// say, which is forged rather than negotiated.
func (a *flowAcceptor) Accept(req *chanacceptor.ChannelAcceptRequest,
) *chanacceptor.ChannelAcceptResponse {

	v, ok := a.params.Load(req.OpenChanMsg.PendingChannelID)
	if !ok {
		return &chanacceptor.ChannelAcceptResponse{}
	}

	terms, ok := v.(*fundeeAcceptor)
	if !ok {
		return &chanacceptor.ChannelAcceptResponse{}
	}

	return terms.Accept(req)
}

// pipe returns a sendMessage closure that forwards messages into dst, aborting
// if quit is closed so a shutting-down flow never blocks the manager goroutine.
func pipe(dst chan lnwire.Message,
	quit chan struct{}) func(lnwire.Message) error {

	return func(msg lnwire.Message) error {
		select {
		case dst <- msg:
			return nil
		case <-quit:
			return errors.New("shutting down")
		}
	}
}

// flowIdentity deterministically derives a unique node identity for a flow from
// its peer ID.
func flowIdentity(id peerID) (*btcec.PrivateKey, *lnwire.NetAddress) {
	var buf [32]byte
	binary.BigEndian.PutUint64(buf[24:], uint64(id))

	// id starts at 1, so buf is never all-zero and stays well below the
	// curve order, yielding a valid private key.
	priv, _ := btcec.PrivKeyFromBytes(buf[:])

	addr := &lnwire.NetAddress{
		IdentityKey: priv.PubKey(),
		Address:     bobAddr.Address,
	}

	return priv, addr
}

// wireFlow builds the counterparty funding manager for a flow and connects the
// bidirectional message pump between it and the SUT.
func (f *fuzzFSM) wireFlow(flow *flowState, params startParams) {
	// The real counterparty manager (Bob) for this flow.
	bobPriv, bobAddr := flowIdentity(flow.peerID)
	bob, err := createTestFundingManager(
		f.t, bobPriv, bobAddr, f.t.TempDir(),
		func(cfg *Config) {
			cfg.OpenChannelPredicate = &fundeeAcceptor{
				zeroConf: params.zeroConf,
				depth:    confDepth(params.remoteConfs),
				// When Bob is the fundee, this CSV rides in his
				// AcceptChannel and the SUT (funder) validates
				// it.
				csv: params.remoteCsvDelay,
			}

			// Size the manager to the flow's channel class so wumbo
			// is accepted only when the corpus asked for it.
			cfg.MaxChanSize = maxChanSize(params.wumbo)
		},
	)
	require.NoError(f.t, err, "failed creating counterparty manager")

	// A lightweight peer handle carrying Alice's identity, used as the peer
	// argument when feeding messages into Bob's manager.
	alice := &testNode{
		privKey:         alicePrivKey,
		addr:            aliceAddr,
		msgChan:         make(chan lnwire.Message, 1),
		shutdownChannel: make(chan struct{}),
		// Bob hands the opened channel to this handle via AddNewChannel
		// (handleConfFundChannelTx drains it), so it needs a live
		// intake channel.
		newChannels: make(chan *newChannelMsg, 1),
	}

	// Features were already resolved (channel-type preset + wumbo bit) in
	// readStartParams.
	aliceFeats, bobFeats := params.localFeats, params.remoteFeats

	// `bob` is passed to Alice's manager as her peer, so from her
	// perspective Local = Alice, Remote = Bob.
	bob.localFeatures = aliceFeats
	bob.remoteFeatures = bobFeats

	// `alice` is passed to Bob's manager as his peer, so from his
	// perspective Local = Bob, Remote = Alice.
	alice.localFeatures = bobFeats
	alice.remoteFeatures = aliceFeats

	// Wire the pump. Alice's manager sends to bob; Bob's manager sends
	// to alice.
	bob.remotePeer = alice
	bob.sendMessage = pipe(alice.msgChan, alice.shutdownChannel)
	alice.remotePeer = bob
	alice.sendMessage = pipe(bob.msgChan, bob.shutdownChannel)

	// Tear the flow down at the end of the iteration. Closing both ends of
	// the pump first releases any manager goroutine parked on a send into
	// it, so Stop cannot block. Cleanups run last-registered-first, so this
	// also runs before the SUT is stopped in newFuzzFSM, which is what
	// unblocks the SUT if it is parked writing to this flow.
	f.t.Cleanup(func() {
		close(alice.shutdownChannel)
		close(bob.shutdownChannel)
		require.NoError(f.t, bob.fundingMgr.Stop())
		require.NoError(f.t, bob.fundingMgr.cfg.Wallet.Shutdown())
	})

	flow.remote = bob
	flow.local = alice

	// Give the counterparty a txid-keyed confirmation notifier too, so
	// handleConfFundChannelTx can confirm it regardless of the negotiated
	// depth (the stock mock only distinguishes 1-conf vs 6-conf channels).
	flow.remoteConf = newFuzzConfNotifier(bob.mockNotifier)
	bob.fundingMgr.cfg.Notifier = flow.remoteConf

	// Register the counterparty handle so the SUT can resolve it by
	// identity when it sends channel_ready (see NotifyWhenOnline in
	// newFuzzFSM).
	f.remotesByPubKey.Store(bob.PubKey(), bob)
}

// maxFlows caps how many funding flows a single input may open.
const maxFlows = 8

// lockedWumbo returns the channel class already fixed for this input, or nil
// while no start event has fixed one yet.
func (f *fuzzFSM) lockedWumbo() *bool {
	if !f.chanClassSet {
		return nil
	}

	return &f.wumbo
}

// applyStart dispatches the two start events, which carry the parameter block
// parsed by readStartParams.
func (f *fuzzFSM) applyStart(event Event, params startParams) {
	// Fix the SUT's channel class on the first start event. This is the
	// only write to its cfg after the manager is running, and it is safe
	// precisely because it is the first: the SUT has not been handed a
	// message yet, so no coordinator goroutine can be reading cfg while we
	// write it. Every later flow inherits the class instead of rewriting
	// it — which also matches production, where MaxChanSize comes once from
	// --protocol.wumbo-channels and is node-level, not per-channel.
	if !f.chanClassSet {
		f.local.fundingMgr.cfg.MaxChanSize = maxChanSize(params.wumbo)
		f.wumbo = params.wumbo
		f.chanClassSet = true

		f.t.Logf("channel class fixed for this input: wumbo=%v "+
			"(SUT MaxChanSize=%d)", params.wumbo,
			maxChanSize(params.wumbo))
	}

	// Past the ceiling the start event is dropped. Its parameter block has
	// already been consumed by the caller, so the rest of the input still
	// decodes exactly as it would have — dropping a flow shifts nothing.
	if len(f.flowOrder) >= maxFlows {
		f.t.Logf("ignoring start event: already at the %d flow limit",
			maxFlows)

		return
	}

	// Each start event begins a new funding flow, which becomes the current
	// flow that subsequent peer-targeted events act on until an
	// EvSwitchFlow (or another start event) changes it.
	flow := f.newFlow()

	flow.zeroConf = params.zeroConf

	switch event {
	case EvStartAsLocalFunder:
		flow.role = RoleLocalFunder
		f.local.handleOpenChannel(f, params)

	case EvStartAsLocalFundee:
		flow.role = RoleLocalFundee
		f.local.handleAcceptChannel(f, params)

	default:
		f.t.Fatalf("not a start event: %v", event)
	}
}

// funderInitReq builds the InitFundingMsg for the funder of a flow, opening a
// channel toward peer.
func (f *fuzzFSM) funderInitReq(flow *flowState, peer *testNode,
	params startParams, validAmt bool) *InitFundingMsg {

	// validAmt clamps the funding amount into the valid range:
	// the SUT as funder should only ever send a consistent size (an
	// out-of-range size is validated by the fundee, so it is only fuzzed
	// when the counterparty funds)
	amt := params.fundingAmt
	if validAmt {
		amt = clampFundingAmt(amt, params.wumbo)
	}
	// The push amount is derived from the effective funding amount so it
	// stays within capacity.
	push := pushAmount(amt, params.pushByte)

	return &InitFundingMsg{
		Peer:            peer,
		TargetPubkey:    peer.privKey.PubKey(),
		ChainHash:       *fundingNetParams.GenesisHash,
		LocalFundingAmt: amt,
		PushAmt:         lnwire.NewMSatFromSatoshis(push),
		FundingFeePerKw: 1000,
		Private:         params.private,
		MinConfs:        1,
		// The preset's explicit type, or nil when it names none — in
		// which case the manager picks its own default and, with
		// option_channel_type up, still sends that explicitly.
		//
		// TODO(MPins): PR 11064 drops that condition, the default
		// going on the wire either way.
		ChannelType: params.chanType,
		Updates:     flow.updates,
		Err:         flow.errChan,
	}
}

// awaitOpenChannel waits for the funder's OpenChannel on outChan after its
// workflow was kicked off. It returns nil if the funder rejected the
// parameters; a hang is a harness bug and fails the test.
func (f *fuzzFSM) awaitOpenChannel(flow *flowState,
	outChan chan lnwire.Message) *lnwire.OpenChannel {

	select {
	case msg := <-outChan:
		open, ok := msg.(*lnwire.OpenChannel)
		if !ok {
			return nil
		}

		return open

	case err := <-flow.errChan:
		// Log it: a flow that fails here goes to StageFailed and then
		// silently ignores every later event, which is hard to tell
		// apart from a harness bug without the reason.
		f.t.Logf("flow %d: funder rejected its own open: %v",
			flow.peerID, err)

		return nil

	case <-time.After(managerTimeout):
		f.t.Fatalf("flow %d: funder did not send OpenChannel",
			flow.peerID)

		return nil
	}
}

// fundeeMustReject reports whether the SUT, acting as fundee, must reject the
// counterparty's OpenChannel for this flow. These are the out-of-bounds
// conditions validated in fundeeProcessOpenChannel; accepting despite any of
// them would be a validation-bypass bug. A too-small amount is excluded (the
// manager does not check MinChanFundingSize — that lives in the wallet/rpc),
// and the confirmation depth is excluded (validated by the funder, not the
// fundee).
func (p startParams) fundeeMustReject() bool {
	return p.fundingAmt > maxChanSize(p.wumbo) ||
		p.remoteCsvDelay > defaultMaxLocalCSVDelay ||
		p.tamper == tamperOpenUnsupported ||
		// TODO(MPins): uncomment once PR 11064 lands.
		// p.tamper == tamperOpenStripType ||
		p.pushTooLarge
}

// assertReservations checks the SUT holds exactly n pending reservations for
// the flow's counterparty.
func (f *fuzzFSM) assertReservations(flow *flowState, n int) {
	assertNumPendingReservations(
		f.t, f.local, flow.remote.privKey.PubKey(), n,
	)
}

// handleOpenChannel starts a funding flow with the SUT (Alice) acting as the
// channel funder.
func (n *testNode) handleOpenChannel(f *fuzzFSM, params startParams) {
	f.t.Logf("flow %d: starting as funder (amt=%d pushByte=%d "+
		"chanType=%v zeroConf=%v wumbo=%v)",
		f.currentPeerID, params.fundingAmt, params.pushByte,
		params.chanType, params.zeroConf, params.wumbo)
	flow := f.flows[f.currentPeerID]

	// Build the counterparty side of this flow and wire the message pump.
	f.wireFlow(flow, params)

	flow.updates = make(chan *lnrpc.OpenStatusUpdate, 1)
	flow.errChan = make(chan error, 1)

	// Alice is the funder, sending toward the counterparty. As the SUT she
	// only ever proposes a valid (clamped) channel size.
	initReq := f.funderInitReq(flow, flow.remote, params, true)
	n.fundingMgr.InitFundingWorkflow(initReq)

	// The SUT's OpenChannel is routed into flow.local.msgChan (Alice's
	// per-flow.
	open := f.awaitOpenChannel(flow, flow.local.msgChan)
	if open == nil {
		flow.stage = StageFailed
		f.assertReservations(flow, 0)
		return
	}

	flow.pendingChanID = open.PendingChannelID
	flow.record(localPeerID, open)

	// Oracle (direction 2): the SUT's own OpenChannel must satisfy BOLT 2's
	// sender requirement for channel_type.
	f.assertFunderChanType(flow, params, open)

	// Remember what she proposed, and which conformance variant this flow
	// carries: both are needed once the counterparty's AcceptChannel comes
	// back, several events later.
	flow.openChanType = open.ChannelType
	flow.tamper = params.tamper

	// Alice's OpenChannel awaits delivery to the counterparty; the message
	// pump (handlePeerInteraction) carries it forward from here.
	flow.pending = open
	flow.pendingFrom = localPeerID

	flow.stage = StageOpenChannel
	f.fuzzStats.startedAsLocalFunder++
	f.assertReservations(flow, 1)
}

// handleAcceptChannel starts a funding flow with the SUT (Alice) acting as the
// channel fundee.
func (n *testNode) handleAcceptChannel(f *fuzzFSM, params startParams) {
	f.t.Logf("flow %d: starting as fundee (amt=%d pushByte=%d "+
		"chanType=%v zeroConf=%v wumbo=%v)",
		f.currentPeerID, params.fundingAmt, params.pushByte,
		params.chanType, params.zeroConf, params.wumbo)
	flow := f.flows[f.currentPeerID]

	// Build the counterparty side of this flow and wire the message pump.
	f.wireFlow(flow, params)

	flow.updates = make(chan *lnrpc.OpenStatusUpdate, 1)
	flow.errChan = make(chan error, 1)

	// Bob is the funder here, opening a channel toward Alice (flow.local).
	// As the counterparty he may propose an inconsistent (out-of-range)
	// channel size; likewise the CSV the SUT validates against
	// its MaxLocalCSVDelay.
	initReq := f.funderInitReq(flow, flow.local, params, false)
	initReq.RemoteCsvDelay = params.remoteCsvDelay
	flow.remote.fundingMgr.InitFundingWorkflow(initReq)

	// Bob's OpenChannel lands in flow.remote.msgChan (his outgoing).
	open := f.awaitOpenChannel(flow, flow.remote.msgChan)
	if open == nil {
		// The counterparty (funder) failed to emit — not a SUT
		// decision; the SUT was never engaged and holds no reservation.
		flow.stage = StageFailed
		f.assertReservations(flow, 0)
		return
	}

	flow.pendingChanID = open.PendingChannelID
	flow.record(flow.peerID, open)

	// Adversarial variants: rewrite the counterparty's OpenChannel so that
	// it breaks one of BOLT 2's channel_type rules the SUT is required to
	// enforce as fundee.
	switch params.tamper {
	// A channel type the SUT's features cannot support, so she must reject
	// it with errUnsupportedChannelType. This models a malicious/buggy peer
	// requesting a type inconsistent with the negotiated features.
	case tamperOpenUnsupported:
		open.ChannelType = mismatchChanType()

	// No channel type at all.
	case tamperOpenStripType:
		open.ChannelType = nil

	// A channel type on an open_channel neither side signalled
	// option_channel_type for. The SUT may accept it — the type is one her
	// features support — but she still owes the funder an accept_channel
	// echoing it back, which the oracle below checks.
	//
	// TODO(MPins): PR 11064 makes this injection a no-op, the funder
	// already sending this exact type. Goes with the variant itself, see
	// tamperOpenTypeUnnegotiated.
	case tamperOpenTypeUnnegotiated:
		open.ChannelType = explicitChanType(
			lnwire.AnchorsZeroFeeHtlcTxRequired,
			lnwire.StaticRemoteKeyRequired,
		)
	}

	// Record the type as the SUT will see it, together with the variant, so
	// the echo oracle below compares against the message actually
	// delivered.
	flow.openChanType = open.ChannelType
	flow.tamper = params.tamper

	// Adversarial variant: rewrite the counterparty's OpenChannel to push
	// more than the whole channel capacity, which a solvent funder can
	// never honestly propose, so the SUT (fundee) must reject it with
	// ErrPushAmountTooLarge. This models a malicious/buggy funder.
	if params.pushTooLarge {
		open.PushAmount = lnwire.NewMSatFromSatoshis(
			open.FundingAmount + 1)
	}

	// Drive Alice's zero-conf decision from the corpus, registered under
	// this flow's pending channel id so the answer reaches the flow it was
	// meant for. Her channel-size limit is not set here: it belongs to the
	// input, not the flow, and was fixed at the first start event.
	f.acceptor.expect(flow.pendingChanID, &fundeeAcceptor{
		zeroConf: params.zeroConf,
	})

	// Hand Bob's OpenChannel to Alice; if she accept it emit an
	// AcceptChannel, unless she rejects the parameters, in which case she
	// replies with an Error.
	n.fundingMgr.ProcessFundingMsg(open, flow.remote)

	// Oracle (direction 1): an out-of-bounds OpenChannel MUST be rejected
	// by the SUT — accepting it would be a validation-bypass bug.
	expectFail := params.fundeeMustReject()

	select {
	case msg := <-flow.local.msgChan:
		accept, ok := msg.(*lnwire.AcceptChannel)
		if !ok {
			// The SUT rejected (Error). Always acceptable under
			// direction 1; just ensure no reservation leaked.
			flow.stage = StageFailed
			f.assertReservations(flow, 0)
			return
		}

		if expectFail {
			f.t.Fatalf("flow %d: SUT accepted an OpenChannel it "+
				"must reject (amt=%d max=%d csv=%d "+
				"tamper=%v pushTooLarge=%v)",
				flow.peerID, params.fundingAmt,
				maxChanSize(params.wumbo),
				params.remoteCsvDelay, params.tamper,
				params.pushTooLarge)
		}

		// Oracle (direction 1, continued): having accepted, her
		// AcceptChannel must echo the funder's channel_type exactly.
		f.assertFundeeChanTypeEcho(flow, accept)

		flow.record(localPeerID, accept)

		// Alice's AcceptChannel awaits delivery to the counterparty
		// (funder); the message pump carries the handshake forward.
		flow.pending = accept
		flow.pendingFrom = localPeerID

		flow.stage = StageAcceptChannel
		f.fuzzStats.startedAsLocalFundee++
		f.assertReservations(flow, 1)

	case <-time.After(managerTimeout):
		f.t.Fatalf("flow %d: Alice did not respond to OpenChannel",
			f.currentPeerID)
	}
}

// handlePeerInteraction is the message pump at the heart of the harness. In the
// expected mode it advances the current flow's funding handshake by one step;
// in an adversarial mode it injects a malformed or misdirected message into the
// SUT as an isolated robustness probe.
func (n *testNode) handlePeerInteraction(f *fuzzFSM, mode PeerInteraction) {
	flow := f.flows[f.currentPeerID]
	if flow == nil {
		return
	}
	f.fuzzStats.peerInteractions[mode]++

	if mode == ModeAdvanceExpected {
		f.advanceFlow(flow)

		return
	}

	f.adversarialProbe(flow, mode)
}

// advanceFlow delivers the flow's pending message to its recipient and captures
// the response, walking the funding handshake one step:
//
//	OpenChannel -> AcceptChannel -> FundingCreated -> FundingSigned
//	  -> (funder broadcasts the funding tx)
//
// channel_ready only follows on-chain confirmations, so it is driven by
// ConfFundChannelTx rather than here. advanceFlow is a no-op once the handshake
// is complete (pending == nil) or the flow has failed.
func (f *fuzzFSM) advanceFlow(flow *flowState) {
	f.t.Logf("flow %d: advancing stage %v (pending=%T)",
		flow.peerID, flow.stage, flow.pending)

	if flow.pending == nil || flow.stage == StageFailed {
		return
	}

	// FundingSigned is the last wire message: the funder consumes it and
	// broadcasts the funding transaction instead of replying.
	if _, ok := flow.pending.(*lnwire.FundingSigned); ok {
		f.deliverPending(flow)
		f.drainBroadcast(flow)
		flow.pending = nil

		// Both sides have moved the channel from a pending reservation
		// to the pending-open database.
		f.assertReservations(flow, 0)

		// A zero-conf channel opens without waiting for a block: both
		// sides have already emitted channel_ready. Complete the
		// exchange now, rather than at ConfFundChannelTx, since there
		// is nothing to mine.
		if flow.zeroConf {
			f.openZeroConf(flow)
		}

		return
	}

	// A channel_type conformance variant may rewrite the counterparty's
	// AcceptChannel on its way to the SUT. She must then fail the flow
	// rather than sign a channel whose type was never agreed.
	mustReject := f.tamperAcceptChanType(flow)

	out, responder := f.deliverPending(flow)

	resp := f.awaitResponse(flow, out)
	if resp == nil {
		// The recipient declined to advance. Only assert no leak when
		// the SUT was the rejecter: it cancels synchronously before
		// replying.
		// When the counterparty rejected the SUT's message, the SUT
		// keeps its reservation until it receives the error, so we
		// don't assert.
		flow.stage = StageFailed
		if flow.pendingFrom != localPeerID {
			f.assertReservations(flow, 0)
		}

		return
	}

	// Oracle (direction 2): the SUT answered a message BOLT 2 obliges her
	// to fail on.
	if mustReject != "" {
		f.t.Fatalf("flow %d: SUT replied %T to %s", flow.peerID, resp,
			mustReject)
	}

	flow.pending = resp
	flow.pendingFrom = responder
	flow.record(responder, resp)
	f.advanceStage(flow, resp)

	// As fundee, the SUT emits a pending-open notification the moment it
	// processes FundingCreated — right after replying FundingSigned
	// (manager.go:2712), a full step before that FundingSigned is delivered
	// and drainBroadcast would clear it. The notification rides the SUT's
	// single, shared, buffered pendingOpenEvent channel; left to accumulate
	// across concurrent fundee flows it fills the buffer and blocks the
	// SUT's reservationCoordinator mid-send, wedging every flow. Drain it
	// now.
	if _, ok := resp.(*lnwire.FundingSigned); ok &&
		responder == localPeerID {

		f.awaitPendingOpen(f.local)

		// A zero-conf channel is marked open in that same goroutine,
		// immediately after the pending-open notification
		// (advancePendingChannelState), so a second event lands on a
		// second shared buffer. finishOpen drains that one, but only
		// once the flow reaches its channel_ready exchange — and an
		// input that leaves several zero-conf fundee flows parked
		// right here fills the buffer long before then, blocking a
		// manager goroutine mid-send. That goroutine is in the
		// manager's WaitGroup, so the stall reaches Stop and wedges
		// the whole iteration rather than just the flow.
		if flow.zeroConf {
			f.awaitOpen(f.local)
		}
	}
}

// awaitPendingOpen consumes the single pending-open notification the SUT emits
// as fundee, so it cannot accumulate on the shared buffered channel. The event
// is pushed by the reservationCoordinator just after the FundingSigned reply
// this call follows, so a bounded wait absorbs the small scheduling gap.
func (f *fuzzFSM) awaitPendingOpen(n *testNode) {
	select {
	case <-n.mockChanEvent.pendingOpenEvent:
	case <-time.After(managerTimeout):
		f.t.Fatalf("flow: fundee did not emit pending-open event")
	}
}

// awaitOpen consumes the open-channel notification a zero-conf channel emits as
// soon as it is marked open.
func (f *fuzzFSM) awaitOpen(n *testNode) {
	select {
	case <-n.mockChanEvent.openEvent:
	case <-time.After(managerTimeout):
		f.t.Fatalf("flow: zero-conf fundee did not emit open event")
	}
}

// deliverPending hands the flow's pending message to the side opposite its
// emitter and returns that recipient's outgoing channel together with the
// recipient's peer ID (the expected responder).
func (f *fuzzFSM) deliverPending(flow *flowState) (
	chan lnwire.Message, peerID) {

	msg := flow.pending

	if flow.pendingFrom == localPeerID {
		// Emitted by the SUT; deliver to the counterparty and capture
		// his response on his outgoing channel.
		flow.remote.fundingMgr.ProcessFundingMsg(msg, flow.local)

		return flow.remote.msgChan, flow.peerID
	}

	// Emitted by the counterparty; deliver to the SUT and capture her
	// response on her outgoing channel.
	f.local.fundingMgr.ProcessFundingMsg(msg, flow.remote)

	return flow.local.msgChan, localPeerID
}

// awaitResponse waits for the recipient's handshake reply on out. A reply that
// is not a handshake message (an Error, say) means the recipient declined to
// advance and yields nil. Silence is a deadlock and fails the test, since every
// handshake step before the funder's broadcast expects a reply.
func (f *fuzzFSM) awaitResponse(flow *flowState,
	out chan lnwire.Message) lnwire.Message {

	select {
	case msg := <-out:
		switch msg.(type) {
		case *lnwire.AcceptChannel, *lnwire.FundingCreated,
			*lnwire.FundingSigned:

			return msg

		default:
			return nil
		}

	case <-flow.errChan:
		return nil

	case <-time.After(managerTimeout):
		f.t.Fatalf("flow %d: no funding response while advancing "+
			"(stage=%v)", flow.peerID, flow.stage)

		return nil
	}
}

// advanceStage moves the flow's stage forward to reflect the handshake message
// just captured.
func (f *fuzzFSM) advanceStage(flow *flowState, msg lnwire.Message) {
	switch msg.(type) {
	case *lnwire.AcceptChannel:
		flow.stage = StageAcceptChannel

		// With the AcceptChannel in hand both sides hold a pending
		// reservation for the channel.
		f.assertReservations(flow, 1)

	case *lnwire.FundingCreated:
		flow.stage = StageFundingCreated

	case *lnwire.FundingSigned:
		flow.stage = StageFundingSigned
	}
}

// drainBroadcast consumes the side effects the funder emits when it processes
// FundingSigned: the published funding transaction and the ChanPending update.
// It also drains the pending-open notifications so their bounded buffers never
// stall a manager shared across flows.
func (f *fuzzFSM) drainBroadcast(flow *flowState) {
	funder := f.local
	if flow.role == RoleLocalFundee {
		funder = flow.remote
	}

	// The funder publishes the funding transaction...
	select {
	case flow.fundingTx = <-funder.publTxChan:
		f.t.Logf("flow %d: funder broadcast funding tx %s",
			flow.peerID, flow.fundingTx.TxHash())

	case <-time.After(managerTimeout):
		f.t.Fatalf("flow %d: funder did not broadcast funding tx",
			flow.peerID)
	}

	// ...and reports the channel as pending to its caller, carrying the
	// funding outpoint. From it we derive the permanent channel ID, which
	// handleConfChannelTx needs to confirm the channel and to match
	// channel_ready.
	select {
	case upd := <-flow.updates:
		pending, ok := upd.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
		if !ok {
			f.t.Fatalf("flow %d: expected ChanPending, got %T",
				flow.peerID, upd.Update)
		}
		outpoint := wire.OutPoint{
			Hash:  flow.fundingTx.TxHash(),
			Index: pending.ChanPending.OutputIndex,
		}
		flow.chanID = lnwire.NewChanIDFromOutPoint(outpoint)
		f.t.Logf("openStatusUpdate: flow %d: funder reports "+
			"ChanPending for chan-id %s", flow.peerID, flow.chanID)

	case <-time.After(managerTimeout):
		f.t.Fatalf("flow %d: funder did not report ChanPending",
			flow.peerID)
	}

	// Both sides emit a pending-open notification; drain whatever is
	// buffered without blocking.
	drainPendingOpen(f, f.local)
	drainPendingOpen(f, flow.remote)
}

// drainPendingOpen empties a node's buffered pending-open notifications.
func drainPendingOpen(f *fuzzFSM, n *testNode) {
	for {
		select {
		case event := <-n.mockChanEvent.pendingOpenEvent:
			f.t.Logf("drained pending-open %T", event)

		default:
			return
		}
	}
}

// drainOpen empties a node's buffered open-channel notifications. The manager
// emits one per channel it opens (NotifyOpenChannelEvent), into a buffer of
// maxPending; left undrained on the shared SUT it would fill and stall the next
// channel opening.
func drainOpen(n *testNode) {
	for {
		select {
		case <-n.mockChanEvent.openEvent:
		default:
			return
		}
	}
}

// adversarialProbe injects adversarial messages into the SUT and asserts the
// live handshake is unaffected.
func (f *fuzzFSM) adversarialProbe(flow *flowState, mode PeerInteraction) {
	f.t.Logf("flow %d: testing adversarial probe (mode=%v)",
		flow.peerID, mode)

	if flow.stage == StageFailed {
		return
	}

	// received is the last message the SUT got from the counterparty, and
	// is the base only for the miss modes (WrongPeer/WrongChanID); Replay
	// picks its own below and OutOfOrder synthesises one.
	//
	// FundingSigned is excluded as a base: it keys by permanent ChanID and
	// the  manager consumes the global signedReservations[ChanID] entry
	// validating the peer, so injecting a forged one would delete the
	// mapping the legitimate flow still needs — a harness desync, not an
	// exploitable bug (an attacker can't know the victim's ChanID
	// pre-broadcast).
	//
	// OpenChannel is excluded because it cannot MISS: it is the message
	// that opens a channel, so a fresh pending chan id (WrongChanID) or a
	// fresh identity (WrongPeer) does not name state the SUT fails to find
	// — it names a new channel, which the SUT correctly accepts, creating a
	// reservation the flow never asked for. That breaks the peer's
	// reservation accounting: handleDisconnectPeer would see a reservation
	// it cannot attribute, and because ProcessFundingMsg only enqueues, the
	// new one can even materialise AFTER CancelPeerReservations has run.
	var received lnwire.Message
	for i := len(flow.history) - 1; i >= 0; i-- {
		d := flow.history[i]
		if d.fromPeerID == flow.peerID &&
			msgRank(d.msg) != msgRankFundingSigned &&
			msgRank(d.msg) != msgRankOpenChannel {

			received = d.msg

			break
		}
	}
	if received == nil {
		// The SUT has received no forgeable message yet.
		return
	}

	var (
		msg  lnwire.Message = received
		peer *testNode

		// hitsLive reports whether the injected (peer, chan-id) pair
		// matches the flow's live reservation. Only then can the SUT
		// possibly advance or tear it down, so only then do we assert
		// on it.
		hitsLive bool
	)

	switch mode {
	case ModeAdversarialReplay:
		// Replay a message the SUT has ALREADY processed, on the real
		// peer and channel id: an idempotency probe against the live
		// reservation. Exclude the message still pending delivery — the
		// SUT has not seen it yet, so injecting it would just advance
		// the handshake legitimately rather than test replay.
		//
		// FundingSigned IS a valid replay base here.
		var replay lnwire.Message
		for i := len(flow.history) - 1; i >= 0; i-- {
			d := flow.history[i]
			if d.fromPeerID == flow.peerID &&
				d.msg != flow.pending {

				f.t.Logf("flow %d: replaying message %T",
					flow.peerID, d.msg)

				replay = d.msg

				break
			}
		}
		if replay == nil {
			return
		}
		msg = replay
		peer = flow.remote
		hitsLive = true

	case ModeAdversarialOutOfOrder:
		// Skip a step: deliver a message this flow's role legitimately
		// receives, but ahead of its turn.
		switch {
		case flow.role == RoleLocalFunder &&
			flow.stage == StageAcceptChannel:

			f.t.Logf("flow %d: injecting FundingSigned to funder",
				flow.peerID)

			// The funder has sent OpenChannel and not yet processed
			// AcceptChannel: inject the FundingSigned.
			msg = &lnwire.FundingSigned{
				ChanID: lnwire.ChannelID(flow.pendingChanID),
			}

		case flow.role == RoleLocalFunder && flow.pending != nil &&
			(flow.stage == StageFundingCreated ||
				flow.stage == StageFundingSigned):

			f.t.Logf("flow %d: injecting channel_ready",
				flow.peerID)

			// The funder has sent FundingCreated and has not yet
			// processed FundingSigned (pending != nil).
			msg = prematureChannelReady(flow)

		case flow.role == RoleLocalFundee &&
			(flow.stage == StageAcceptChannel ||
				flow.stage == StageFundingCreated):

			f.t.Logf("flow %d: injecting channel_ready",
				flow.peerID)

			// The fundee has sent AcceptChannel and is awaiting
			// FundingCreated: inject the channel_ready that only
			// follows on-chain confirmation.
			msg = prematureChannelReady(flow)

		default:
			return
		}
		peer = flow.remote
		hitsLive = true

	case ModeAdversarialWrongPeer:
		// A valid message (the real channel id) attributed to a peer
		// that holds no reservation.
		f.t.Logf("flow %d: injecting message %T from wrong peer",
			flow.peerID, received)

		priv, addr := flowIdentity(f.nextPeerID + probePeerOffset)
		peer = probePeer(priv, addr)

	case ModeAdversarialWrongChanID:
		// A valid message whose channel id matches no live reservation,
		// attributed to the real counterparty.
		msg = withChanID(received, f.otherPendingChannID(flow))
		peer = probePeer(flow.remote.privKey, flow.remote.addr)

		f.t.Logf("flow %d: injecting message %T with wrong channel ID",
			flow.peerID, msg)
	}

	// Modes that miss the live reservation can neither advance nor touch
	// it, so the fuzzer's crash detection is the only oracle for them.
	if !hitsLive {
		f.local.fundingMgr.ProcessFundingMsg(msg, peer)

		return
	}

	peerKey := flow.remote.privKey.PubKey()
	before := numReservations(f.local.fundingMgr, peerKey)

	f.local.fundingMgr.ProcessFundingMsg(msg, peer)

	// The SUT must never be fooled into advancing the live handshake.
	f.assertNoAdvance(flow, mode)

	// A cancellation is NOT a failure: tearing the flow down is legitimate
	// per-peer DoS protection (the peer only affects its own reservation,
	// and the authenticated transport blocks third-party injection). But
	// the live flow is now dead, so mark it failed — otherwise the next
	// real advance would block waiting on a reservation that no longer
	// exists.
	if numReservations(f.local.fundingMgr, peerKey) < before {
		flow.stage = StageFailed
		flow.pending = nil
	}
}

// assertFunderChanType checks BOLT 2's open_channel sender requirement against
// the SUT's own message: "MUST set `channel_type`". A funder that leaves it out
// hands its peer a message the spec obliges the peer to fail on.
//
// The guard below turns on what the SUT herself advertises, not on what the two
// of them negotiated, and the difference matters. A SUT without the bit is a
// node lnd cannot be, carrying ExplicitChannelTypeRequired in SetInit
// unconditionally, so a message out of that configuration is a harness artefact
// and nothing more. A peer without it is another matter: lnd's required bit
// obliges a peer to understand it, not to advertise it back, so that pairing is
// one a real node meets — and what lnd sends into it is lnd's to answer for.
func (f *fuzzFSM) assertFunderChanType(flow *flowState, params startParams,
	open *lnwire.OpenChannel) {

	if !sutSignals(params, lnwire.ExplicitChannelTypeOptional) {
		return
	}

	if open.ChannelType == nil {
		// TODO(MPins): make this a Fatalf once PR 11064 lands.
		f.t.Logf("flow %d: SUT sent an OpenChannel with no "+
			"channel_type", flow.peerID)
	}
}

// assertFundeeChanTypeEcho checks BOLT 2's accept_channel sender requirement
// against the SUT's own message: "MUST set `channel_type` to the `channel_type`
// from `open_channel`". Echoing anything else — above all inventing a type the
// funder never sent — is an interop break rather than a cosmetic one, since the
// funder is required to fail the channel on an echo that does not match what it
// proposed.
func (f *fuzzFSM) assertFundeeChanTypeEcho(flow *flowState,
	accept *lnwire.AcceptChannel) {

	if chanTypeEqual(flow.openChanType, accept.ChannelType) {
		return
	}
	// TODO(MPins): make this a Fatalf once PR 11064 lands.
	f.t.Logf("flow %d: SUT's AcceptChannel does not echo the "+
		"OpenChannel channel_type: echoed %s, received %s",
		flow.peerID, chanTypeString(accept.ChannelType),
		chanTypeString(flow.openChanType))
}

// tamperAcceptChanType applies the flow's accept_channel conformance variant to
// the counterparty's pending AcceptChannel, just before it reaches the SUT. It
// returns a description of the violation the SUT now has to fail on, or the
// empty string when nothing was rewritten.
func (f *fuzzFSM) tamperAcceptChanType(flow *flowState) string {
	accept, ok := flow.pending.(*lnwire.AcceptChannel)
	if !ok || flow.pendingFrom == localPeerID {
		return ""
	}

	// Both variants break the echo of a proposal, so neither means anything
	// against an OpenChannel that named no type in the first place.
	if flow.openChanType == nil {
		return ""
	}

	switch flow.tamper {
	case tamperAcceptStripType:
		accept.ChannelType = nil

		return "an AcceptChannel carrying no channel_type"

	case tamperAcceptWrongType:
		accept.ChannelType = wrongChanType(flow.openChanType)

		return "an AcceptChannel echoing a channel_type she never " +
			"proposed"
	}

	return ""
}

// assertNoAdvance fails if the SUT emits a handshake-advancing message on the
// flow's outbox: an adversarial message must never move the live handshake
// forward. A rejection (Error) or silence is acceptable; whatever is emitted is
// drained so it cannot pollute the next real advance.
//
// The probe was handed to the coordinator, and the reply to any funding message
// is written to the outbox by that same goroutine before it takes the next one,
// so syncSUT is enough to make this an exact check: once it returns, a reply
// the probe provoked is already there.
func (f *fuzzFSM) assertNoAdvance(flow *flowState, mode PeerInteraction) {
	f.syncSUT()

	select {
	case msg := <-flow.local.msgChan:
		switch msg.(type) {
		case *lnwire.AcceptChannel, *lnwire.FundingCreated,
			*lnwire.FundingSigned:

			f.t.Fatalf("flow %d: SUT advanced the handshake (%T) "+
				"in response to adversarial %v",
				flow.peerID, msg, mode)

		case *lnwire.Error:
			// The expected reply to a rejected probe. It belongs to
			// this assertion, so consume it.

		default:
			// Anything else is legitimate traffic that merely
			// raced the probe, and the flow still needs it: a
			// zero-conf channel_ready is emitted from the
			// advanceFundingState goroutine, which syncSUT (a
			// reservationCoordinator round trip) does not order
			// against. Consuming it here would strand the flow,
			// and openZeroConf would later time out waiting for a
			// message this assertion had already thrown away.
			select {
			case flow.local.msgChan <- msg:

			default:
				f.t.Fatalf("flow %d: cannot restore %T the "+
					"probe did not judge", flow.peerID, msg)
			}
		}

	default:
	}
}

// numReservations returns the SUT's pending reservation count for peerKey, read
// under the manager's lock so it is safe to sample while background goroutines
// mutate the reservation table.
func numReservations(mgr *Manager, peerKey *btcec.PublicKey) int {
	key := newSerializedKey(peerKey)

	mgr.resMtx.RLock()
	defer mgr.resMtx.RUnlock()

	return len(mgr.activeReservations[key])
}

// Handshake message ranks, ordered by their position in the opening flow.
const (
	msgRankOpenChannel = iota + 1
	msgRankAcceptChannel
	msgRankFundingCreated
	msgRankFundingSigned
)

// msgRank orders the funding handshake messages by their position in the
// opening flow (0 for anything else).
func msgRank(m lnwire.Message) int {
	switch m.(type) {
	case *lnwire.OpenChannel:
		return msgRankOpenChannel
	case *lnwire.AcceptChannel:
		return msgRankAcceptChannel
	case *lnwire.FundingCreated:
		return msgRankFundingCreated
	case *lnwire.FundingSigned:
		return msgRankFundingSigned
	default:
		return 0
	}
}

// prematureChannelReady builds a channel_ready for this flow carrying its
// pending channel id.
func prematureChannelReady(flow *flowState) lnwire.Message {
	return lnwire.NewChannelReady(
		lnwire.ChannelID(flow.pendingChanID),
		flow.remote.privKey.PubKey(),
	)
}

// withChanID returns a copy of msg with its channel identifier replaced.
func withChanID(msg lnwire.Message, id [32]byte) lnwire.Message {
	switch m := msg.(type) {
	case *lnwire.OpenChannel:
		c := *m
		c.PendingChannelID = id

		return &c

	case *lnwire.AcceptChannel:
		c := *m
		c.PendingChannelID = id

		return &c

	case *lnwire.FundingCreated:
		c := *m
		c.PendingChannelID = id

		return &c

	case *lnwire.FundingSigned:
		c := *m
		c.ChanID = lnwire.ChannelID(id)

		return &c

	default:
		return msg
	}
}

// probePeerOffset keeps wrong-peer probe identities clear of the IDs assigned
// to real flows.
const probePeerOffset peerID = 1 << 32

// probePeer builds a throwaway peer handle with the given identity whose
// outgoing messages drain into an isolated sink, so a probe reply is discarded
// rather than delivered into a real flow's channel.
func probePeer(priv *btcec.PrivateKey,
	addr *lnwire.NetAddress) *testNode {

	sink := make(chan lnwire.Message, 4)

	return &testNode{
		privKey: priv,
		addr:    addr,
		msgChan: sink,
		sendMessage: func(m lnwire.Message) error {
			select {
			case sink <- m:
			default:
			}

			return nil
		},
	}
}

// syncPeerID is the identity the coordinator round-trip probe funds from, kept
// clear of both the real flows and the wrong-peer probes.
const syncPeerID peerID = 2 << 32

// syncSUT blocks until the SUT's reservation coordinator has finished handling
// every funding message the harness handed it before this call.
//
// Every message goes through the single reservationCoordinator goroutine in
// FIFO order (manager.go), so a reply to a message queued last proves the ones
// before it are done. The probe is an OpenChannel pushing more than its
// capacity, from a peer of its own: that fails the coordinator's very first
// check, before it touches the wallet or the database, and the rejection comes
// straight back on the probe peer's private sink — so the round trip costs a
// channel send and leaves no trace on the SUT.
//
// This is what replaces "sleep and hope" in the negative assertions: after it
// returns, whatever the SUT was going to emit in response is already in its
// outbox and can be checked for without waiting.
func (f *fuzzFSM) syncSUT() {
	priv, addr := flowIdentity(syncPeerID)
	peer := probePeer(priv, addr)

	f.local.fundingMgr.ProcessFundingMsg(&lnwire.OpenChannel{
		PushAmount: 1,
	}, peer)

	select {
	case <-peer.msgChan:

	case <-time.After(managerTimeout):
		f.t.Fatalf("SUT did not answer the coordinator probe")
	}
}

// otherPendingChannID returns a pending channel id guaranteed to differ from
// the flow's own, so a WrongChanID probe genuinely misses the live reservation.
//
// It deliberately does NOT reuse another flow's pending id: the per-flow
// counterparty managers are each created fresh and generate the SAME first
// pending channel id deterministically, so another flow's id routinely collides
// with this one's. Feeding a colliding id back (under the real peer's identity)
// would hit the live reservation and advance it — a harness false positive.
// Flipping a byte of our own id sidesteps the collision entirely.
func (f *fuzzFSM) otherPendingChannID(flow *flowState) [32]byte {
	foreign := flow.pendingChanID
	foreign[0] ^= 0xFF

	return foreign
}

// handleConfFundChannelTx mines one block on top of the current flow's funding
// transaction. Every block but the last only deepens it — the manager records
// the height and the channel stays pending — and the block that finally reaches
// the depth the channel requires is the one that confirms it and drives the
// channel_ready exchange.
func (n *testNode) handleConfFundChannelTx(f *fuzzFSM, early bool) {
	f.fuzzStats.confFundChannelTx++

	flow := f.flows[f.currentPeerID]
	if flow == nil {
		return
	}

	// A flow can be mined on only once its funder has broadcast and it is
	// awaiting confirmation.
	if !flow.awaitingConfirmation() {
		return
	}

	f.mineBlock(flow, early)
}

// mineBlock advances the flow's funding tx one block deeper, confirming the
// channel if that block reaches the required depth.
func (f *fuzzFSM) mineBlock(flow *flowState, early bool) {
	required := f.requiredConfs(flow)

	// The first block on a tx that is not in the chain is the one that
	// mines it, which fixes the height everything else is derived from. A
	// reorg bumps it, so re-mining lands in a later block than the one it
	// left.
	if flow.confs == 0 && flow.fundingHeight == 0 {
		// Unique per flow: the shortChanID is {BlockHeight, TxIndex,
		// output-index}, so a shared height would collide across
		// channels that happen to share a funding output index.
		flow.fundingHeight = uint32(flow.peerID) + 1
	}
	flow.confs++

	f.t.Logf("flow %d: mined block, funding tx %d/%d confs at height %d",
		flow.peerID, flow.confs, required, flow.fundingHeight)

	if flow.confs >= required {
		f.confirmChannel(flow, early)

		return
	}

	// Not deep enough yet: both sides see the tx gain a confirmation and
	// record its height, but neither may treat the channel as open.
	txid := flow.fundingTx.TxHash()
	left := required - flow.confs

	if !f.confNotifier.update(txid, flow.fundingHeight, left) {
		f.t.Fatalf("flow %d: SUT did not accept the confirmation "+
			"update", flow.peerID)
	}
	if !flow.remoteConf.update(txid, flow.fundingHeight, left) {
		f.t.Fatalf("flow %d: counterparty did not accept the "+
			"confirmation update", flow.peerID)
	}

	f.assertConfHeight(flow)
	f.assertNotOpened(flow, "before its funding tx reached the required "+
		"confirmation depth")
}

// requiredConfs returns the confirmation depth the flow's channel needs, read
// from the SUT's own channel state rather than re-derived from the negotiated
// parameters.
func (f *fuzzFSM) requiredConfs(flow *flowState) uint32 {
	channel, err := f.local.fundingMgr.cfg.Wallet.Cfg.Database.
		FetchChannelByID(flow.chanID)
	if err != nil {
		f.t.Fatalf("flow %d: unable to read the SUT's channel: %v",
			flow.peerID, err)
	}

	if channel.NumConfsRequired == 0 {
		return 1
	}

	return uint32(channel.NumConfsRequired)
}

// confirmChannel confirms the flow's funding tx on both sides, exchanges the
// resulting channel_ready messages, and drives the flow to StageOpen.
//
// Ordering (A), early == false: a block confirms both parties at once; each
// sends channel_ready and we exchange them.
//
// Ordering (B), early == true: the counterparty confirms and sends
// channel_ready FIRST, and the SUT receives it while its own channel is still
// pending-open awaiting confirmation.
func (f *fuzzFSM) confirmChannel(flow *flowState, early bool) {
	// With the peer disconnected neither ordering applies: the SUT cannot
	// send its channel_ready at all until the peer is back.
	if flow.offline {
		f.confirmWhileOffline(flow)

		return
	}

	txid := flow.fundingTx.TxHash()

	// Both sides confirm at the height of the block that actually holds the
	// tx — the same one they have been recording all along, and the one the
	// shortChanID is built from.
	height := flow.fundingHeight

	if early {
		// Confirm only the counterparty, capture its channel_ready, and
		// deliver it to the still-unconfirmed SUT.
		flow.remoteConf.confirm(txid, flow.fundingTx, height)
		bobReady := f.awaitChannelReady(flow, flow.remote.msgChan)
		f.local.fundingMgr.ProcessFundingMsg(bobReady, flow.remote)

		// The SUT must not act on the early channel_ready. Sync on the
		// coordinator having handled it: the message is parked by a
		// goroutine it spawns (handleChannelReady), so the round trip
		// is what gives that goroutine the chance to misbehave before
		// we look.
		f.syncSUT()
		f.assertNotOpened(flow, "while an early channel_ready was "+
			"parked awaiting its own confirmation")

		// Now confirm the SUT: it emits its channel_ready and the
		// parked counterparty message resolves, opening the channel.
		f.confNotifier.confirm(txid, flow.fundingTx, height)
		aliceReady := f.awaitChannelReady(flow, flow.local.msgChan)
		flow.remote.fundingMgr.ProcessFundingMsg(aliceReady, flow.local)

		f.finishOpen(flow)

		return
	}

	// A block confirms the funding tx for both parties. The SUT confirms
	// via its shared txid-keyed notifier; the counterparty via its own.
	f.confNotifier.confirm(txid, flow.fundingTx, height)
	flow.remoteConf.confirm(txid, flow.fundingTx, height)

	// Once each side sees the confirmation it sends channel_ready: the
	// SUT's lands on flow.local.msgChan, the counterparty's on
	// flow.remote.msgChan.
	aliceReady := f.awaitChannelReady(flow, flow.local.msgChan)
	bobReady := f.awaitChannelReady(flow, flow.remote.msgChan)

	// Exchange them so each side can finalize the channel.
	f.local.fundingMgr.ProcessFundingMsg(bobReady, flow.remote)
	flow.remote.fundingMgr.ProcessFundingMsg(aliceReady, flow.local)

	f.finishOpen(flow)
}

// confirmWhileOffline confirms the funding tx of a flow whose counterparty the
// SUT believes to be disconnected. The chain does not care about connectivity,
// so both sides confirm and the counterparty emits its channel_ready as usual —
// but the SUT parks in waitForPeerOnline before sending its own, so the channel
// cannot open until handleReconnectPeer releases it.
func (f *fuzzFSM) confirmWhileOffline(flow *flowState) {
	txid := flow.fundingTx.TxHash()

	f.confNotifier.confirm(txid, flow.fundingTx, flow.fundingHeight)
	flow.remoteConf.confirm(txid, flow.fundingTx, flow.fundingHeight)

	// Hold the counterparty's channel_ready back rather than delivering it:
	// the connection is meant to be down, and the SUT would otherwise
	// process it and hand the channel off (its local discovery signal is
	// already closed at this point, so nothing else would stop it).
	flow.remoteReady = f.awaitChannelReady(flow, flow.remote.msgChan)

	// Wait for the SUT to park on the absent peer. That park is the exact
	// state this assertion is about — it is sendChannelReady stopping in
	// waitForPeerOnline — so waiting for it turns "has it stayed silent so
	// far?" into "it is silent because it is blocked, and here is where".
	var peerPub [33]byte
	copy(peerPub[:], flow.remote.privKey.PubKey().SerializeCompressed())
	if !f.peerLink.awaitPark(peerPub) {
		f.t.Fatalf("flow %d: SUT never waited for its disconnected "+
			"peer to come back", flow.peerID)
	}

	// The SUT must stay silent: its channel_ready is waiting for the peer.
	f.assertNotOpened(flow, "while its peer was disconnected")

	// handleFundingConfirmation fires the open event before stateStep
	// reaches sendChannelReady, so the event is already queued while the
	// SUT parks. The SUT's buffer is shared across flows, so drain it
	// now — several flows parked here at once would otherwise fill it and
	// wedge their goroutines before they ever reach the park.
	drainOpen(f.local)
	drainOpen(flow.remote)

	flow.stage = StageFundingConfirmed
}

// completeReconnectedOpen finishes a channel that confirmed while its peer was
// away: the reconnect released the SUT's parked channel_ready, so the exchange
// held back by confirmWhileOffline can finally happen.
func (f *fuzzFSM) completeReconnectedOpen(flow *flowState) {
	aliceReady := f.awaitChannelReady(flow, flow.local.msgChan)

	f.local.fundingMgr.ProcessFundingMsg(flow.remoteReady, flow.remote)
	flow.remote.fundingMgr.ProcessFundingMsg(aliceReady, flow.local)

	flow.remoteReady = nil

	f.finishOpen(flow)
}

// openZeroConf completes a zero-conf channel, which has no block to mine: the
// manager opens it (advancePendingChannelState) the moment the funding tx is
// broadcast, so each side has ALREADY emitted channel_ready and fired its open
// event during the handshake. We just exchange those channel_ready messages and
// hand off. Called from advanceFlow right after the broadcast.
func (f *fuzzFSM) openZeroConf(flow *flowState) {
	aliceReady := f.awaitChannelReady(flow, flow.local.msgChan)
	bobReady := f.awaitChannelReady(flow, flow.remote.msgChan)

	f.local.fundingMgr.ProcessFundingMsg(bobReady, flow.remote)
	flow.remote.fundingMgr.ProcessFundingMsg(aliceReady, flow.local)

	f.finishOpen(flow)
}

// finishOpen consumes the finished channel each side hands off through
// AddNewChannel (the SUT to flow.remote, the counterparty to flow.local),
// drains the open events both fired, and marks the flow open.
func (f *fuzzFSM) finishOpen(flow *flowState) {
	f.acceptNewChannel(flow, flow.remote)
	f.acceptNewChannel(flow, flow.local)

	f.t.Logf("flow %d: draining open events", flow.peerID)
	drainOpen(f.local)
	drainOpen(flow.remote)

	flow.stage = StageOpen

	f.t.Logf("flow %d: channel opened", flow.peerID)
}

// assertNotOpened verifies the SUT has neither emitted its own channel_ready
// nor handed off a channel — the state it must hold for as long as its funding
// tx is not confirmed. situation describes why that is the case, for the
// failure  message.
//
// It does not wait. Every caller synchronizes with the SUT first — on the
// confirmation height it persists for a chain event, on the waiter it parks
// when its peer is away, or on a coordinator round trip — so by the time this
// runs, anything the SUT was going to emit is already in its outbox. Waiting a
// fixed window here instead is what made a chain-heavy input take seconds: the
// assertion fires on every mined block and every reorg, so at 100ms an input
// mining ~70 blocks spent 7 of its 7.5 seconds asleep, and go-fuzz got a
// handful of executions out of a whole core.
func (f *fuzzFSM) assertNotOpened(flow *flowState, situation string) {
	select {
	case msg := <-flow.local.msgChan:
		f.t.Fatalf("flow %d: SUT emitted %T %s", flow.peerID, msg,
			situation)

	case c := <-flow.remote.newChannels:
		close(c.err)
		f.t.Fatalf("flow %d: SUT opened the channel %s", flow.peerID,
			situation)

	default:
	}
}

// awaitChannelReady reads the next channel_ready a side emits, failing if none
// arrives or the message is of another type.
func (f *fuzzFSM) awaitChannelReady(flow *flowState,
	ch chan lnwire.Message) *lnwire.ChannelReady {

	select {
	case msg := <-ch:
		ready, ok := msg.(*lnwire.ChannelReady)
		if !ok {
			f.t.Fatalf("flow %d: expected ChannelReady, got %T",
				flow.peerID, msg)
		}

		f.t.Logf("flow %d: received ChannelReady", flow.peerID)

		return ready

	case <-time.After(managerTimeout):
		f.t.Fatalf("flow %d: no ChannelReady after confirmation",
			flow.peerID)

		return nil
	}
}

// acceptNewChannel consumes the finished channel a node hands off through
// AddNewChannel, unblocking that call so the funding flow can complete.
func (f *fuzzFSM) acceptNewChannel(flow *flowState, n *testNode) {
	select {
	case c := <-n.newChannels:
		close(c.err)

	case <-time.After(managerTimeout):
		f.t.Fatalf("flow %d: node did not hand off the new channel",
			flow.peerID)
	}
}

// maxReorgs caps how many times a single flow's funding tx may be reorged out.
const maxReorgs = 6

// handleReorg reorgs the current flow's funding transaction out of the chain
// while the channel is awaiting confirmation, then puts it back in a later
// block.
//
// A reorg does not fail the flow: waitForFundingConfirmation resets the
// recorded confirmation height to zero on NegativeConf and stays in its wait
// loop, so the channel remains pending and the flow is left at
// StageFundingSigned, where a later ConfFundChannelTx event can still open it.
func (n *testNode) handleReorg(f *fuzzFSM) {
	f.t.Logf("flow %d: handling reorg", f.currentPeerID)
	f.fuzzStats.reorg++

	flow := f.flows[f.currentPeerID]
	if flow == nil {
		return
	}

	// Only a flow whose funder has broadcast, and which is still waiting
	// for that tx to confirm, has anything to reorg out.
	if !flow.awaitingConfirmation() {
		return
	}

	// A tx that was never mined cannot be reorged out.
	if flow.confs == 0 {
		f.t.Logf("flow %d: nothing to reorg, funding tx is not in the "+
			"chain", flow.peerID)

		return
	}

	// Confirmations bound themselves — once the tx reaches its depth the
	// channel opens and later blocks are no-ops — but reorgs do not: each
	// one resets the depth and re-mines, so a flow can be held in the
	// awaiting-confirmation window indefinitely. Every round through it
	// walks the same transition with the same state, and pays for the
	// assertions again, so cap it.
	if flow.reorgs >= maxReorgs {
		f.t.Logf("flow %d: ignoring reorg: already at the %d reorg "+
			"limit", flow.peerID, maxReorgs)

		return
	}
	flow.reorgs++

	f.reorgChannel(flow)
}

// reorgChannel takes the flow's partially confirmed funding tx back out of the
// chain, checks both sides forget its height without treating the channel as
// open, and re-mines it into a later block.
func (f *fuzzFSM) reorgChannel(flow *flowState) {
	// Reorg the tx out on both sides — they observe the same chain, so both
	// see it leave. The depth is the one it had reached; the manager reacts
	// to the notification itself and discards the value.
	txid := flow.fundingTx.TxHash()
	f.t.Logf("flow %d: reorging funding tx %v out of the chain from %d "+
		"conf(s)", flow.peerID, txid, flow.confs)

	if !f.confNotifier.reorg(txid, int32(flow.confs)) {
		f.t.Fatalf("flow %d: SUT did not accept the reorg", flow.peerID)
	}
	if !flow.remoteConf.reorg(txid, int32(flow.confs)) {
		f.t.Fatalf("flow %d: counterparty did not accept the reorg",
			flow.peerID)
	}

	// The tx is out of the chain, so it has no depth and neither side may
	// hold a confirmation height for it any more.
	flow.confs = 0
	f.assertConfHeight(flow)

	// A reorged-out funding tx must leave the channel pending: the SUT may
	// neither send its channel_ready nor hand the channel off.
	f.assertNotOpened(flow, "after its funding tx was reorged out")

	// The tx is re-mined into a later block, and everything proceeds from
	// there as if it had just been mined for the first time.
	flow.fundingHeight++
	f.mineBlock(flow, false)
}

// assertConfHeight checks both sides have persisted the confirmation height the
// flow's chain position implies: the height of the block holding the funding tx
// while it is in the chain, and zero while it is not.
func (f *fuzzFSM) assertConfHeight(flow *flowState) {
	var expected uint32
	if flow.confs > 0 {
		expected = flow.fundingHeight
	}

	f.t.Logf("flow %d: asserting confirmation height %d", flow.peerID,
		expected)

	f.awaitConfHeight(flow, f.local, expected)
	f.awaitConfHeight(flow, flow.remote, expected)
}

// awaitConfHeight waits for a node to persist the confirmation height the
// flow's chain position implies. The height is written from the manager's
// waitForFundingConfirmation goroutine, so some wait is genuinely needed — but
// it checks before it ever sleeps, which is the whole point of not reusing the
// shared assertConfirmationHeight helper here. That one runs on wait.Predicate,
// which sleeps a full 200ms poll interval before looking even once; this
// assertion fires on every mined block and every reorg, so paying that toll
// unconditionally is what turns a chain-heavy input into a multi-second one and
// starves the fuzzer of iterations.
func (f *fuzzFSM) awaitConfHeight(flow *flowState, n *testNode,
	expected uint32) {

	var (
		deadline = time.Now().Add(managerTimeout)
		last     uint32
		lastErr  error
	)
	for {
		channel, err := n.fundingMgr.cfg.Wallet.Cfg.Database.
			FetchChannelByID(flow.chanID)
		switch {
		// The channel may not be readable yet; treat it as "not there
		// so far" and report the error only if we run out of time.
		case err != nil:
			lastErr = err

		case channel.ConfirmationHeight == expected:
			return

		default:
			last, lastErr = channel.ConfirmationHeight, nil
		}

		if time.Now().After(deadline) {
			if lastErr != nil {
				f.t.Fatalf("flow %d: unable to read the "+
					"channel: %v", flow.peerID, lastErr)
			}

			f.t.Fatalf("flow %d: node recorded confirmation "+
				"height %d, expected %d", flow.peerID, last,
				expected)
		}

		time.Sleep(handoffPollInterval)
	}
}

// handleDisconnectPeer takes the current flow's counterparty offline, mirroring
// what the server does once a peer drops (server.go, after WaitForDisconnect):
// every reservation held for that peer is cancelled so the UTXOs it committed
// are released.
//
// The cut-off is the funding broadcast. Before it the flow lives in a
// reservation and the disconnect kills it; after it the channel is a pending
// open in the database, holds no reservation, and survives — only its
// channel_ready is held back until the peer returns.
func (n *testNode) handleDisconnectPeer(f *fuzzFSM) {
	f.t.Logf("flow %d: handling disconnect", f.currentPeerID)
	f.fuzzStats.disconnectPeer++

	flow := f.flows[f.currentPeerID]
	if flow == nil || flow.offline || flow.remote == nil {
		return
	}

	peerKey := flow.remote.privKey.PubKey()
	var peerPub [33]byte
	copy(peerPub[:], peerKey.SerializeCompressed())

	// From here on the SUT must treat this peer as gone, so
	// NotifyWhenOnline parks rather than resolving.
	flow.offline = true
	f.peerLink.disconnect(peerPub)

	// Anything still in flight cannot be delivered over a dead connection.
	flow.pending = nil

	had := numReservations(f.local.fundingMgr, peerKey)
	f.local.fundingMgr.CancelPeerReservations(peerPub)

	// Whatever the flow was doing, the SUT must not keep a reservation for
	// a peer that is gone — those are exactly the committed UTXOs the
	// cancel is there to release.
	if got := numReservations(f.local.fundingMgr, peerKey); got != 0 {
		f.t.Fatalf("flow %d: SUT still holds %d reservation(s) for a "+
			"disconnected peer", flow.peerID, got)
	}

	if had == 0 {
		// Nothing was in flight: the handshake either already completed
		// (the channel is pending-open and unaffected) or the flow was
		// already dead. Leave the stage alone.
		return
	}

	f.t.Logf("flow %d: disconnect cancelled %d reservation(s)",
		flow.peerID, had)

	// The handshake was still live, so it is now dead. Acting as funder the
	// SUT owes its caller an error; as fundee the reservation's error
	// channel is internal to the manager and nothing observable is emitted.
	if flow.role == RoleLocalFunder {
		f.awaitFundingError(flow)
	}

	flow.stage = StageFailed
}

// awaitFundingError reads the error the SUT reports to the caller that asked it
// to fund. Only meaningful for RoleLocalFunder: that is the only role where the
// reservation's error channel is the one the harness supplied (funderInitReq),
// rather than one the manager created for itself.
func (f *fuzzFSM) awaitFundingError(flow *flowState) {
	select {
	case err := <-flow.errChan:
		f.t.Logf("flow %d: SUT reported %v", flow.peerID, err)

	case <-time.After(managerTimeout):
		f.t.Fatalf("flow %d: SUT cancelled the reservation without "+
			"telling the funding caller", flow.peerID)
	}
}

// handleReconnectPeer brings the current flow's counterparty back online,
// releasing anything the SUT parked on it. A channel that confirmed while the
// peer was away has its channel_ready waiting in sendChannelReady; this is what
// lets it out, so the open completes here rather than at confirmation time.
func (n *testNode) handleReconnectPeer(f *fuzzFSM) {
	f.t.Logf("flow %d: handling reconnect", f.currentPeerID)
	f.fuzzStats.reconnectPeer++

	flow := f.flows[f.currentPeerID]
	if flow == nil || !flow.offline || flow.remote == nil {
		return
	}

	var peerPub [33]byte
	copy(peerPub[:], flow.remote.privKey.PubKey().SerializeCompressed())

	flow.offline = false

	// Not asserting that anything was parked: the SUT reaches
	// waitForPeerOnline from a background goroutine, so a reconnect can
	// legitimately land before it gets there — in which case the peer is
	// already online again by the time it asks, and it never parks at all.
	released := f.peerLink.reconnect(peerPub, flow.remote)
	f.t.Logf("flow %d: reconnect released %d parked waiter(s)",
		flow.peerID, released)

	if flow.stage == StageFundingConfirmed {
		f.completeReconnectedOpen(flow)
	}
}

// newFlow allocates a fresh funding flow, registers it, and makes it the
// current flow that subsequent peer-targeted events act on.
func (f *fuzzFSM) newFlow() *flowState {
	id := f.nextPeerID
	f.nextPeerID++

	flow := &flowState{peerID: id}
	f.flows[id] = flow
	f.flowOrder = append(f.flowOrder, id)
	f.currentPeerID = id

	return flow
}

// switchFlow points currentPeerID at the flow selected by b.
func (f *fuzzFSM) switchFlow(b byte) {
	if len(f.flowOrder) == 0 {
		return
	}

	next := f.flowOrder[int(b)%len(f.flowOrder)]
	f.t.Logf("switching flow from %d to %d", f.currentPeerID, next)

	f.currentPeerID = next
}

// applyEvent dispatches every event that is not a start event. peerInteraction
// is only meaningful for EvPeerInteraction; the remaining events ignore it.
func (f *fuzzFSM) applyEvent(event Event, peerInteraction PeerInteraction) {
	switch event {
	case EvPeerInteraction:
		f.local.handlePeerInteraction(f, peerInteraction)

	case EvReorg:
		f.local.handleReorg(f)

	case EvDisconnectPeer:
		f.local.handleDisconnectPeer(f)

	case EvReconnectPeer:
		f.local.handleReconnectPeer(f)

	case EvNoOp:
		// Nothing to do; EvNoOp exists so the fuzzer can pad the input
		// without changing the FSM state.

	default:
		f.t.Fatalf("unknown event: %v", event)
	}
}

// FuzzFundingManagerFSM is a fuzz test for the funding manager's finite state
// machine (FSM).
func FuzzFundingManagerFSM(f *testing.F) {
	// Seed input that exercises two concurrent funding flows and switching
	// between them. Flow 1 is opened as the local funder and flow 2 as the
	// local fundee.
	f.Add([]byte{
		// Flow 1: start as local funder.
		byte(EvStartAsLocalFunder),
		// local features bits (default to 0)
		byte(0),
		// remote features bits (default to 0)
		byte(0),
		// confirmation depth the counterparty requires
		byte(3),
		// private channel
		byte(0),
		// wumbo (0 -> standard channel)
		byte(0),
		// channel type (implicit)
		byte(0),
		// Funding amount: 520000 sat channel (500000 raw).
		byte(0x00), byte(0x07), byte(0xA1), byte(0x20),
		byte(0),                          // pushing amount
		byte(0),                          // csv delay (0 -> default)
		byte(EvPeerInteraction), byte(0), // advance flow 1
		byte(EvPeerInteraction), byte(0), // advance flow 1

		// Flow 2: start as local fundee.
		byte(EvStartAsLocalFundee),
		// local features bits (default to 0)
		byte(0),
		// remote features bits (default to 0)
		byte(0),
		// confirmation depth the counterparty requires
		byte(3),
		// private channel
		byte(0),
		// wumbo (0 -> standard channel)
		byte(0),
		// channel type
		byte(0),
		// Funding amount 520000 sat channel.
		byte(0x00), byte(0x07), byte(0xA1), byte(0x20),
		byte(0),                          // pushing amount
		byte(0),                          // csv delay (0 -> default)
		byte(EvPeerInteraction), byte(0), // advance flow 2
		byte(EvPeerInteraction), byte(0), // advance flow 2

		// Switch back to flow 1 (selector 0 -> first flow)
		// and advance.
		byte(EvSwitchFlow), byte(0),
		byte(EvPeerInteraction), byte(0), // advance flow 1
		byte(EvPeerInteraction), byte(0), // advance flow 1
		byte(ConfFundChannelTx), byte(0), // mined 1/3 block
		byte(ConfFundChannelTx), byte(0), // mined 2/3 block
		byte(ConfFundChannelTx), byte(0), // mined 3/3 block

		// Switch back to flow 2 (selector 1 -> second flow)
		// and advance.
		byte(EvSwitchFlow), byte(1),
		byte(EvPeerInteraction), byte(0), // advance flow 2
		byte(ConfFundChannelTx), byte(0), // mined 1/3 block
		byte(ConfFundChannelTx), byte(0), // mined 2/3 block
		byte(ConfFundChannelTx), byte(0), // mined 3/3 block
	})

	// Seed: a zero-conf channel.
	f.Add([]byte{
		byte(EvStartAsLocalFunder),
		// local features (default to 0)
		byte(0),
		// remote features (default to 0)
		byte(0),
		// confirmation depth
		byte(0),
		// private channel
		byte(0),
		// wumbo (standard)
		byte(0),
		// channel type -> zero-conf
		byte(4),
		// Funding amount: raw 500000 -> a 520000 sat channel.
		byte(0x00), byte(0x07), byte(0xA1), byte(0x20),
		byte(0),                          // push amount
		byte(0),                          // csv delay
		byte(EvPeerInteraction), byte(0), // advance: -> AcceptChannel
		byte(EvPeerInteraction), byte(0), // advance: -> FundingCreated
		byte(EvPeerInteraction), byte(0), // advance: -> FundingSigned
		// advance: broadcast + zero-conf open
		byte(EvPeerInteraction), byte(0),
	})

	// Seed: a funding tx that is reorged out before it finally confirms.
	f.Add([]byte{
		byte(EvStartAsLocalFunder),
		// local features bits (default to 0)
		byte(0),
		// remote features bits (default to 0)
		byte(0),
		// confirmation depth required
		byte(3),
		// private channel
		byte(0),
		// wumbo (0 -> standard channel)
		byte(0),
		// channel type (implicit)
		byte(0),
		// funding amount
		byte(0x00), byte(0x07), byte(0xA1), byte(0x20),
		byte(0),                          // pushing amount
		byte(0),                          // csv delay (0 -> default)
		byte(EvPeerInteraction), byte(0), // advance: -> AcceptChannel
		byte(EvPeerInteraction), byte(0), // advance: -> FundingCreated
		byte(EvPeerInteraction), byte(0), // advance: -> FundingSigned
		byte(EvPeerInteraction), byte(0), // advance: broadcast

		// Mine one block: the tx is in the chain at 1/3 confs, so the
		// channel is still pending and the reorg has something to undo.
		byte(ConfFundChannelTx), byte(0),

		// Reorg it out, then re-mine it in a later block (back to 1/3).
		byte(EvReorg),

		// Two more blocks reach the required depth and open the
		// channel.
		byte(ConfFundChannelTx), byte(0),
		byte(ConfFundChannelTx), byte(0),
	})

	// Seed: the peer drops mid-handshake, while the flow still lives in a
	// reservation. The SUT must release it and tell the funding caller, and
	// the reconnect must not resurrect anything — a cancelled handshake is
	// gone for good, a fresh one would have to start from OpenChannel.
	f.Add([]byte{
		byte(EvStartAsLocalFunder),
		// local features bits (default to 0)
		byte(0),
		// remote features bits (default to 0)
		byte(0),
		// confirmation depth required
		byte(3),
		// private channel
		byte(0),
		// wumbo (0 -> standard channel)
		byte(0),
		// channel type (implicit)
		byte(0),
		// funding amount
		byte(0x00), byte(0x07), byte(0xA1), byte(0x20),
		byte(0),                          // pushing amount
		byte(0),                          // csv delay (0 -> default)
		byte(EvPeerInteraction), byte(0), // advance: -> AcceptChannel
		byte(EvPeerInteraction), byte(0), // advance: -> FundingCreated
		byte(EvDisconnectPeer),           // cancels live reservation
		byte(EvReconnectPeer),            // nothing to resume
		byte(EvPeerInteraction), byte(0), // advance: -> FundingCreated
	})
	// Seed: the peer drops after the funding tx is broadcast and is back
	// before it confirms. The handshake is already over by then, so the
	// flow holds no reservation for the disconnect to cancel and the
	// pending-open channel survives it untouched.
	f.Add([]byte{
		byte(EvStartAsLocalFunder),
		// local features bits (default to 0)
		byte(0),
		// remote features bits (default to 0)
		byte(0),
		// confirmation depth required
		byte(3),
		// private channel
		byte(0),
		// wumbo (0 -> standard channel)
		byte(0),
		// channel type (implicit)
		byte(0),
		// funding amount
		byte(0x00), byte(0x07), byte(0xA1), byte(0x20),
		byte(0),                          // pushing amount
		byte(0),                          // csv delay (0 -> default)
		byte(EvPeerInteraction), byte(0), // advance: -> AcceptChannel
		byte(EvPeerInteraction), byte(0), // advance: -> FundingCreated
		byte(EvPeerInteraction), byte(0), // advance: -> FundingSigned
		byte(EvPeerInteraction), byte(0), // advance: broadcast
		byte(EvDisconnectPeer), // no reservation left to cancel
		// Back before any block, so the SUT has not yet reached
		// sendChannelReady and never asked to be told when the peer
		// returns: there is no parked waiter to release.
		byte(EvReconnectPeer),

		// Three blocks reach the required depth and open the channel,
		// through the ordinary path — the outage left no trace.
		byte(ConfFundChannelTx), byte(0),
		byte(ConfFundChannelTx), byte(0),
		byte(ConfFundChannelTx), byte(0),
	})

	// Seed: the peer drops after the funding tx is broadcast, so there is
	// no  reservation left to cancel and the pending-open channel survives.
	// The tx then reaches its depth while the peer is away, which parks the
	// SUT's channel_ready in waitForPeerOnline; only the reconnect
	// completes the open.
	f.Add([]byte{
		byte(EvStartAsLocalFunder),
		byte(0), // local features bits
		byte(0), // remote features bits
		byte(3), // confirmation depth required
		byte(0), // private channel
		byte(0), // wumbo (0 -> standard channel)
		byte(0), // channel type (0 -> implicit)
		// Funding amount: raw 500000 -> a 520000 sat channel.
		byte(0x00), byte(0x07), byte(0xA1), byte(0x20),
		byte(0),                          // pushing amount
		byte(0),                          // csv delay (0 -> default)
		byte(EvPeerInteraction), byte(0), // advance: -> AcceptChannel
		byte(EvPeerInteraction), byte(0), // advance: -> FundingCreated
		byte(EvPeerInteraction), byte(0), // advance: -> FundingSigned
		byte(EvPeerInteraction), byte(0), // advance: broadcast

		byte(EvDisconnectPeer), // no reservation left to cancel

		// Three blocks reach the required depth with the peer away, so
		// the SUT's channel_ready parks instead of going out.
		byte(ConfFundChannelTx), byte(0),
		byte(ConfFundChannelTx), byte(0),
		byte(ConfFundChannelTx), byte(0),

		byte(EvReconnectPeer), // releases it -> the channel opens
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		fsm := newFuzzFSM(t)
		fsm.consume(data)
	})
}
