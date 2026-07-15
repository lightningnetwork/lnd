package funding

import (
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

type Event uint8

const (
	EvStartAsLocalFunder Event = iota
	EvStartAsLocalFundee

	// EvSwitchFlow changes which active funding flow subsequent
	// peer-targeted events act on. It consumes one selector byte.
	EvSwitchFlow

	EvPeerInteraction

	ConfFundChannelTx
	EvReorg

	EvDisconnectPeer
	EvReconnectPeer

	EvNoOp

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
	reorg     uint64

	disconnectPeer uint64
	reconnectPeer  uint64
}

type flowState struct {
	peerID peerID

	// chanID is the permanent channel ID, derived from the funding outpoint
	// once the funder broadcasts (captured in drainBroadcast). Zero until
	// then. fundingTx is the broadcast funding transaction, replayed into the
	// notifier to confirm the channel in handleConfFundChannelTx.
	chanID    lnwire.ChannelID
	fundingTx *wire.MsgTx

	// pendingChanID is the pending channel ID negotiated before the
	// funding outpoint (and thus the final chanID) is known.
	pendingChanID [32]byte

	role    fundingRole
	stage   fundingStage
	history []deliveredMsg

	// pending is the most recent funding message one side has emitted but
	// the harness has not yet delivered to the other; A nil pending means
	// the wire handshake is complete (awaiting confirmation) or the flow
	// never started / has failed.
	pending lnwire.Message
	// pendingFrom identifies the emitter
	pendingFrom peerID

	// zeroConf reports whether this flow negotiated a zero-conf channel, in
	// which case channel_ready is exchanged during the handshake rather than
	// after confirmation.
	zeroConf bool

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

// record appends a delivered message to the flow's history.
func (p *flowState) record(from peerID, msg lnwire.Message) {
	p.history = append(p.history, deliveredMsg{
		msg:        msg,
		fromPeerID: from,
	})
}

// localPeerID identifies the SUT (Alice) as the source of a delivered message.
const localPeerID peerID = 0

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
	// (nil for implicit negotiation); zeroConf marks a zero-conf flow;
	// chanTypeMismatch requests the adversarial unsupported-type variant.
	localFeats       []lnwire.FeatureBit
	remoteFeats      []lnwire.FeatureBit
	chanType         *lnwire.ChannelType
	zeroConf         bool
	chanTypeMismatch bool

	remoteConfs    byte
	remoteCsvDelay uint16
	private        bool
	wumbo          bool
	fundingAmt     btcutil.Amount
	pushByte       byte

	// pushTooLarge requests the adversarial variant where the counterparty
	// (as funder) sends an OpenChannel whose push amount exceeds the funding
	// amount, so the SUT (fundee) must reject it (ErrPushAmountTooLarge). It
	// is only acted on in handleAcceptChannel; the SUT as funder always
	// proposes a valid push.
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
	max := maxChanSize(wumbo)

	switch raw % 64 {
	case 0, 1:
		// ~3%: just above this class's maximum -> rejected
		// (ErrChanTooLarge).
		return max + 1

	default:
		span := uint64(max-MinChanFundingSize) + 1

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
	switch max := maxChanSize(wumbo); {
	case amt < MinChanFundingSize:
		return MinChanFundingSize
	case amt > max:
		return max
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
// It returns ok=false if the stream is exhausted before the whole block is
// read, in which case the start event is dropped.
func (r *reader) readStartParams() (startParams, bool) {
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
	wumbo := wumboByte%8 == 1

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
		localFeats:       localFeats,
		remoteFeats:      remoteFeats,
		chanType:         cfg.chanType,
		zeroConf:         cfg.zeroConf,
		chanTypeMismatch: cfg.mismatch,
		remoteConfs:      remoteConfs,
		remoteCsvDelay:   csvDelay(csvByte),
		// Taproot channels must be private, so the preset can force it.
		private: privateByte&1 == 1 || cfg.forcePriv,
		wumbo:   wumbo,
		// Full-range amount; the funder side clamps it when the SUT funds.
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
	// handleConfFundChannelTx confirm exactly the current flow's funding tx even
	// though the SUT is shared across flows.
	confNotifier *fuzzConfNotifier

	// fuzzStats keeps track of various statistics during the fuzzing process.
	fuzzStats fuzzStats
}

// fuzzConfNotifier wraps the per-manager mock notifier so a confirmation can be
// targeted at a specific funding tx. The stock mock hands every registrant the
// SAME Confirmed channel, so with the SUT shared across flows a confirmation
// would wake an arbitrary waiter. Keying the Confirmed channel by txid lets
// handleConfFundChannelTx confirm exactly the intended flow.
type fuzzConfNotifier struct {
	*mockNotifier

	mu    sync.Mutex
	confs map[chainhash.Hash]chan *chainntnfs.TxConfirmation
}

func newFuzzConfNotifier(base *mockNotifier) *fuzzConfNotifier {
	return &fuzzConfNotifier{
		mockNotifier: base,
		confs: make(
			map[chainhash.Hash]chan *chainntnfs.TxConfirmation,
		),
	}
}

// chanFor returns the (created-on-demand) confirmed channel for txid. The
// buffer of one lets a confirmation delivered before the manager registers
// still be picked up when it does — removing the register/confirm race.
func (n *fuzzConfNotifier) chanFor(
	txid chainhash.Hash) chan *chainntnfs.TxConfirmation {

	n.mu.Lock()
	defer n.mu.Unlock()

	ch, ok := n.confs[txid]
	if !ok {
		ch = make(chan *chainntnfs.TxConfirmation, 1)
		n.confs[txid] = ch
	}

	return ch
}

// RegisterConfirmationsNtfn overrides the embedded mock's method (the whole
// point of the wrapper): the manager calls it through the ChainNotifier
// interface from waitForFundingConfirmation, and it hands back the txid-keyed
// Confirmed channel that confirm() later delivers on. Without this override,
// embedding would promote the mock's version and every flow would share one
// channel again.
func (n *fuzzConfNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	_ []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent,
	error) {

	return &chainntnfs.ConfirmationEvent{
		Confirmed:    n.chanFor(*txid),
		Updates:      make(chan chainntnfs.TxUpdateInfo, 1),
		NegativeConf: make(chan int32, 1),
		Cancel:       func() {},
	}, nil
}

// confirm delivers a single confirmation for txid, at the given block height,
// to whichever waiter has registered (or will register) for it.
func (n *fuzzConfNotifier) confirm(txid chainhash.Hash, tx *wire.MsgTx,
	height uint32) {

	select {
	case n.chanFor(txid) <- &chainntnfs.TxConfirmation{
		Tx: tx, BlockHeight: height,
	}:
	default:
	}
}

// newFuzzFSM initializes and returns a new fuzz finite state machine (FSM)
// instance.
func newFuzzFSM(t *testing.T) *fuzzFSM {
	// Alice's MaxChanSize is set per flow (in the start handlers) according
	// to that flow's channel class, since she is shared across flows.
	local, err := createTestFundingManager(
		t, alicePrivKey, aliceAddr, t.TempDir(),
	)
	require.NoError(t, err, "failed creating fundingManager")

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
	}

	// The SUT talks to a different counterparty per flow, so resolve the
	// peer that channel_ready waits on by the requested identity.
	local.fundingMgr.cfg.NotifyWhenOnline = func(pk [33]byte,
		peerChan chan<- lnpeer.Peer) {

		if peer := fsm.peerByPubKey(pk); peer != nil {
			peerChan <- peer
		}
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
		return v.(lnpeer.Peer)
	}

	return nil
}

// consume drives the FSM by interpreting data as a stream of events.
// EvStart and EvPeerInteraction consume their own parameter bytes.
func (f *fuzzFSM) consume(data []byte) {
	r := &reader{data: data}
	for {
		evByte, ok := r.u8()
		if !ok {
			return
		}

		event := Event(evByte) % NumEvents

		switch event {
		case EvStartAsLocalFunder, EvStartAsLocalFundee:
			params, ok := r.readStartParams()
			if !ok {
				return
			}
			f.applyStart(event, params)

		case EvSwitchFlow:
			b, ok := r.u8()
			if !ok {
				return
			}
			f.switchFlow(b)

		case EvPeerInteraction:
			b, ok := r.u8()
			if !ok {
				return
			}
			f.applyEvent(event, pickInteraction(b))

		case ConfFundChannelTx:
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

// featureBitsFromByte maps a fuzz byte onto the set of feature bits that
// influence channel-type negotiation.
func featureBitsFromByte(b byte) []lnwire.FeatureBit {
	candidates := []lnwire.FeatureBit{
		lnwire.StaticRemoteKeyOptional,
		lnwire.AnchorsZeroFeeHtlcTxOptional,
		lnwire.ExplicitChannelTypeOptional,
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

	// Anchors depends on static-remote-key; real nodes never advertise one
	// without the other. The mismatch matters here: implicit negotiation
	// selects anchors on the Anchors bit alone but builds a type that also
	// requires static-remote-key, which the fundee re-validates. An
	// anchors-without-SRK set would therefore fail negotiation
	// (errUnsupportedChannelType) instead of opening, starving deep-state
	// coverage. Enforce the dependency to keep implicit opens graceful; the
	// deliberate errUnsupportedChannelType path is still covered by the
	// chanTypeMismatch variant.
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

// chanTypeConfig is a curated, internally-consistent channel-type configuration
// the corpus can select.
type chanTypeConfig struct {
	localFeats  []lnwire.FeatureBit
	remoteFeats []lnwire.FeatureBit
	chanType    *lnwire.ChannelType
	zeroConf    bool
	forcePriv   bool

	// mismatch marks the adversarial variant where the counterparty (as
	// funder) sends an OpenChannel whose channel type is inconsistent with
	// the negotiated features, so the SUT (fundee) must reject it. It is
	// only acted on in handleAcceptChannel.
	mismatch bool
}

// resolveChanType maps a selector byte onto a channel-type configuration. Most
// values yield implicit negotiation (the common case: no explicit type, with
// the two distinct per-byte feature sets); a minority select one of a curated
// list of explicit-type presets, every one of which is a valid combination per
// explicitNegotiateCommitmentType, so the fuzzer reaches the deep flow rather
// than dying at errUnsupportedChannelType.
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
			mismatch: true,
		}

	// Implicit negotiation (the common case): no explicit type, with the
	// distinct per-byte feature sets for each side.
	default:
		return chanTypeConfig{
			localFeats:  featureBitsFromByte(localByte),
			remoteFeats: featureBitsFromByte(remoteByte),
		}
	}
}

// mismatchChanType is the explicit channel type injected into the
// counterparty's OpenChannel for the mismatched-type variant. The SUT runs with
// empty features in that variant, so any explicit type is unsupported.
func mismatchChanType() *lnwire.ChannelType {
	return explicitChanType(lnwire.SimpleTaprootChannelsRequiredFinal)
}

// confDepth maps a fuzz byte onto a channel confirmation depth (the fundee's
// MinAcceptDepth), most values land in the valid [0, MaxNumConfs], a small
// slice deliberately overshoots "confs too large" rejection path. A depth of 0
// means "use the manager's default", not a true zero-conf channel.
func confDepth(b byte) uint16 {
	const maxConfs = uint16(chainntnfs.MaxNumConfs)

	if b >= 250 {
		return maxConfs + 1 + uint16(b-250) // 145..150, all rejected
	}

	return uint16(b) % (maxConfs + 1) // 0..144
}

// csvDelay maps a fuzz byte onto the CSV delay (to_self_delay) the counterparty
// imposes on the SUT, so the SUT is the side that validates it against its
// MaxLocalCSVDelay. Most values scale into the valid (0, maxLocalCSV] range; a
// small slice is 0 (the counterparty then uses its size-scaled default) and
// another small slice overshoots to exercise the SUT's ErrCsvDelayTooLarge
// rejection.
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
		// (handleConfFundChannelTx drains it), so it needs a live intake channel.
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

	flow.remote = bob
	flow.local = alice

	// Give the counterparty a txid-keyed confirmation notifier too, so
	// handleConfFundChannelTx can confirm it regardless of the negotiated depth
	// (the stock mock only distinguishes 1-conf vs 6-conf channels).
	flow.remoteConf = newFuzzConfNotifier(bob.mockNotifier)
	bob.fundingMgr.cfg.Notifier = flow.remoteConf

	// Register the counterparty handle so the SUT can resolve it by
	// identity when it sends channel_ready (see NotifyWhenOnline in
	// newFuzzFSM).
	f.remotesByPubKey.Store(bob.PubKey(), bob)
}

// applyStart dispatches the two start events, which carry the parameter block
// parsed by readStartParams.
func (f *fuzzFSM) applyStart(event Event, params startParams) {
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
		// nil for implicit negotiation, or the preset's explicit type.
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

	case <-flow.errChan:
		return nil

	case <-time.After(5 * time.Second):
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
		p.chanTypeMismatch ||
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
	flow := f.flows[f.currentPeerID]

	// Build the counterparty side of this flow and wire the message pump.
	f.wireFlow(flow, params)

	flow.updates = make(chan *lnrpc.OpenStatusUpdate, 1)
	flow.errChan = make(chan error, 1)

	// Size Alice to this flow's channel class before she screens the amount
	// as funder.
	n.fundingMgr.cfg.MaxChanSize = maxChanSize(params.wumbo)

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

	// Adversarial variant: rewrite the counterparty's OpenChannel to carry
	// a channel type the SUT's features cannot support, so she must reject
	// it with errUnsupportedChannelType. This models a malicious/buggy peer
	// requesting a type inconsistent with the negotiated features.
	if params.chanTypeMismatch {
		open.ChannelType = mismatchChanType()
	}

	// Adversarial variant: rewrite the counterparty's OpenChannel to push
	// more than the whole channel capacity, which a solvent funder can never
	// honestly propose, so the SUT (fundee) must reject it with
	// ErrPushAmountTooLarge. This models a malicious/buggy funder.
	if params.pushTooLarge {
		open.PushAmount = lnwire.NewMSatFromSatoshis(
			open.FundingAmount + 1)
	}

	// Drive Alice's zero-conf and channel-size limit from the corpus for
	// this flow.
	n.fundingMgr.cfg.OpenChannelPredicate = &fundeeAcceptor{
		zeroConf: params.zeroConf,
	}
	n.fundingMgr.cfg.MaxChanSize = maxChanSize(params.wumbo)

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
			f.t.Fatalf("flow %d: SUT accepted an OpenChannel it must "+
				"reject (amt=%d max=%d csv=%d mismatch=%v "+
				"pushTooLarge=%v)",
				flow.peerID, params.fundingAmt,
				maxChanSize(params.wumbo), params.remoteCsvDelay,
				params.chanTypeMismatch, params.pushTooLarge)
		}

		flow.record(localPeerID, accept)

		// Alice's AcceptChannel awaits delivery to the counterparty
		// (funder); the message pump carries the handshake forward.
		flow.pending = accept
		flow.pendingFrom = localPeerID

		flow.stage = StageAcceptChannel
		f.fuzzStats.startedAsLocalFundee++
		f.assertReservations(flow, 1)

	case <-time.After(5 * time.Second):
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
// ConfFundChannelTx rather than here. advanceFlow is a no-op once the handshake is
// complete (pending == nil) or the flow has failed.
func (f *fuzzFSM) advanceFlow(flow *flowState) {
	if flow.pending == nil || flow.stage == StageFailed {
		return
	}

	// FundingSigned is the last wire message: the funder consumes it and
	// broadcasts the funding transaction instead of replying.
	if _, ok := flow.pending.(*lnwire.FundingSigned); ok {
		f.deliverPending(flow)
		f.drainBroadcast(flow)
		flow.pending = nil

		// Both sides have moved the channel from a pending reservation to
		// the pending-open database.
		f.assertReservations(flow, 0)

		// A zero-conf channel opens without waiting for a block: both sides
		// have already emitted channel_ready. Complete the exchange now,
		// rather than at ConfFundChannelTx, since there is nothing to mine.
		if flow.zeroConf {
			f.openZeroConf(flow)
		}

		return
	}

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

	flow.pending = resp
	flow.pendingFrom = responder
	flow.record(responder, resp)
	f.advanceStage(flow, resp)

	// As fundee, the SUT emits a pending-open notification the moment it
	// processes FundingCreated — right after replying FundingSigned
	// (manager.go:2712), a full step before that FundingSigned is delivered
	// and drainBroadcast would clear it. The notification rides the SUT's
	// single, shared, buffered pendingOpenEvent channel; left to accumulate
	// across concurrent fundee flows it fills the buffer and blocks the SUT's
	// reservationCoordinator mid-send, wedging every flow. Drain it now.
	if _, ok := resp.(*lnwire.FundingSigned); ok && responder == localPeerID {
		f.awaitPendingOpen(f.local)
	}
}

// awaitPendingOpen consumes the single pending-open notification the SUT emits
// as fundee, so it cannot accumulate on the shared buffered channel. The event
// is pushed by the reservationCoordinator just after the FundingSigned reply
// this call follows, so a bounded wait absorbs the small scheduling gap.
func (f *fuzzFSM) awaitPendingOpen(n *testNode) {
	select {
	case <-n.mockChanEvent.pendingOpenEvent:
	case <-time.After(5 * time.Second):
		f.t.Fatalf("flow: fundee did not emit pending-open event")
	}
}

// deliverPending hands the flow's pending message to the side opposite its
// emitter and returns that recipient's outgoing channel together with the
// recipient's peer ID (the expected responder).
func (f *fuzzFSM) deliverPending(flow *flowState) (chan lnwire.Message, peerID) {
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

	case <-time.After(5 * time.Second):
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
	case <-time.After(5 * time.Second):
		f.t.Fatalf("flow %d: funder did not broadcast funding tx",
			flow.peerID)
	}

	// ...and reports the channel as pending to its caller, carrying the
	// funding outpoint. From it we derive the permanent channel ID, which
	// handleConfChannelTx needs to confirm the channel and to match channel_ready.
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
	case <-time.After(5 * time.Second):
		f.t.Fatalf("flow %d: funder did not report ChanPending",
			flow.peerID)
	}

	// Both sides emit a pending-open notification; drain whatever is buffered
	// without blocking.
	drainPendingOpen(f.local)
	drainPendingOpen(flow.remote)
}

// drainPendingOpen empties a node's buffered pending-open notifications.
func drainPendingOpen(n *testNode) {
	for {
		select {
		case <-n.mockChanEvent.pendingOpenEvent:
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
	if flow.stage == StageFailed {
		return
	}

	// received is the last message the SUT got from the counterparty.
	// FundingSigned is excluded as a base: it keys by permanent ChanID and
	// the  manager consumes the global signedReservations[ChanID] entry
	// validating the peer, so injecting a forged one would delete the
	// mapping the legitimate flow still needs — a harness desync, not an
	// exploitable bug (an attacker can't know the victim's ChanID
	// pre-broadcast).
	var received lnwire.Message
	for i := len(flow.history) - 1; i >= 0; i-- {
		d := flow.history[i]
		if d.fromPeerID == flow.peerID &&
			msgRank(d.msg) != msgRankFundingSigned {

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

			// The funder has sent OpenChannel and not yet processed
			// AcceptChannel: inject the FundingSigned.
			msg = &lnwire.FundingSigned{
				ChanID: lnwire.ChannelID(flow.pendingChanID),
			}

		case flow.role == RoleLocalFunder && flow.pending != nil &&
			(flow.stage == StageFundingCreated ||
				flow.stage == StageFundingSigned):

			// The funder has sent FundingCreated and has not yet
			// processed FundingSigned (pending != nil).
			msg = prematureChannelReady(flow)

		case flow.role == RoleLocalFundee &&
			(flow.stage == StageAcceptChannel ||
				flow.stage == StageFundingCreated):

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
		priv, addr := flowIdentity(f.nextPeerID + probePeerOffset)
		peer = probePeer(priv, addr)

	case ModeAdversarialWrongChanID:
		// A valid message whose channel id matches no live reservation,
		// attributed to the real counterparty.
		msg = withChanID(received, f.otherPendingChannID(flow))
		peer = probePeer(flow.remote.privKey, flow.remote.addr)
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

// assertNoAdvance fails if the SUT emits a handshake-advancing message on the
// flow's outbox: an adversarial message must never move the live handshake
// forward. A rejection (Error) or silence is acceptable; whatever is emitted is
// drained so it cannot pollute the next real advance.
func (f *fuzzFSM) assertNoAdvance(flow *flowState, mode PeerInteraction) {
	select {
	case msg := <-flow.local.msgChan:
		switch msg.(type) {
		case *lnwire.AcceptChannel, *lnwire.FundingCreated,
			*lnwire.FundingSigned:

			f.t.Fatalf("flow %d: SUT advanced the handshake (%T) in "+
				"response to adversarial %v", flow.peerID, msg, mode)
		}

	case <-time.After(100 * time.Millisecond):
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

// handleConfFundChannelTx confirms the current flow's funding transaction and
// drives the channel_ready exchange that opens the channel. When early is set
// it exercises ordering (B): the counterparty's channel_ready reaches the SUT
// before the SUT sees its own confirmation.
func (n *testNode) handleConfFundChannelTx(f *fuzzFSM, early bool) {
	f.fuzzStats.confFundChannelTx++

	flow := f.flows[f.currentPeerID]
	if flow == nil {
		return
	}

	// A flow can be mined into an open channel only once its funder has
	// broadcast and it is awaiting confirmation. Zero-conf flows never reach
	// here: they open during the handshake (openZeroConf) and are already at
	// StageOpen, so the stage guard below excludes them.
	if flow.stage != StageFundingSigned || flow.pending != nil {
		return
	}

	f.confirmChannel(flow, early)
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
	txid := flow.fundingTx.TxHash()

	// Confirm both sides at the SAME height, unique per flow: the shortChanID
	// is {BlockHeight, TxIndex, output-index}, so a constant height would make
	// channels sharing a funding output index collide. Both peers must agree
	// on the height (they'd see the same block), hence one value for the flow.
	height := uint32(flow.peerID) + 1

	if early {
		// Confirm only the counterparty, capture its channel_ready, and
		// deliver it to the still-unconfirmed SUT.
		flow.remoteConf.confirm(txid, flow.fundingTx, height)
		bobReady := f.awaitChannelReady(flow, flow.remote.msgChan)
		f.local.fundingMgr.ProcessFundingMsg(bobReady, flow.remote)

		// The SUT must not act on the early channel_ready.
		f.assertParked(flow)

		// Now confirm the SUT: it emits its channel_ready and the
		// parked counterparty message resolves, opening the channel.
		f.confNotifier.confirm(txid, flow.fundingTx, height)
		aliceReady := f.awaitChannelReady(flow, flow.local.msgChan)
		flow.remote.fundingMgr.ProcessFundingMsg(aliceReady, flow.local)

		f.finishOpen(flow)

		return
	}

	// A block confirms the funding tx for both parties. The SUT confirms via
	// its shared txid-keyed notifier; the counterparty via its own.
	f.confNotifier.confirm(txid, flow.fundingTx, height)
	flow.remoteConf.confirm(txid, flow.fundingTx, height)

	// Once each side sees the confirmation it sends channel_ready: the SUT's
	// lands on flow.local.msgChan, the counterparty's on flow.remote.msgChan.
	aliceReady := f.awaitChannelReady(flow, flow.local.msgChan)
	bobReady := f.awaitChannelReady(flow, flow.remote.msgChan)

	// Exchange them so each side can finalize the channel.
	f.local.fundingMgr.ProcessFundingMsg(bobReady, flow.remote)
	flow.remote.fundingMgr.ProcessFundingMsg(aliceReady, flow.local)

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

	drainOpen(f.local)
	drainOpen(flow.remote)

	flow.stage = StageOpen
}

// assertParked verifies the SUT has neither emitted its own channel_ready nor
// handed off a channel — the state it must hold while an early channel_ready
// sits parked on the localDiscoverySignal awaiting the SUT's own confirmation.
func (f *fuzzFSM) assertParked(flow *flowState) {
	select {
	case msg := <-flow.local.msgChan:
		f.t.Fatalf("flow %d: SUT emitted %T before its own "+
			"confirmation (early channel_ready not parked)",
			flow.peerID, msg)

	case c := <-flow.remote.newChannels:
		close(c.err)
		f.t.Fatalf("flow %d: SUT opened the channel on an early "+
			"channel_ready, before confirmation", flow.peerID)

	case <-time.After(100 * time.Millisecond):
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

		return ready

	case <-time.After(5 * time.Second):
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

	case <-time.After(5 * time.Second):
		f.t.Fatalf("flow %d: node did not hand off the new channel",
			flow.peerID)
	}
}

// handleReorg simulates a chain reorg affecting flows awaiting confirmation.
//
// TODO: implement reorg simulation.
func (n *testNode) handleReorg(f *fuzzFSM) {
	f.fuzzStats.reorg++
}

// handleDisconnectPeer simulates the current flow's peer going offline.
//
// TODO: implement peer disconnect.
func (n *testNode) handleDisconnectPeer(f *fuzzFSM) {
	f.fuzzStats.disconnectPeer++
}

// handleReconnectPeer simulates the current flow's peer coming back online.
//
// TODO: implement peer reconnect and funding-state resync.
func (n *testNode) handleReconnectPeer(f *fuzzFSM) {
	f.fuzzStats.reconnectPeer++
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
	f.currentPeerID = f.flowOrder[int(b)%len(f.flowOrder)]
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
		byte(0),                            // local features bits (default to 0)
		byte(0),                            // remote features bits (default to 0)
		byte(3),                            // confirmation depth the counterparty requires
		byte(0),                            // private channel
		byte(0),                            // wumbo (0 -> standard channel)
		byte(0),                            // channel type (0 -> implicit negotiation)
		byte(0), byte(0), byte(0), byte(0), // funding amount
		byte(0),                          // percentual of pushing amount
		byte(0),                          // csv delay (0 -> default)
		byte(EvPeerInteraction), byte(0), // advance flow 1
		byte(EvPeerInteraction), byte(0), // advance flow 1

		// Flow 2: start as local fundee.
		byte(EvStartAsLocalFundee),
		byte(0),                            // local features bits (default to 0)
		byte(0),                            // remote features bits (default to 0)
		byte(3),                            // confirmation depth the counterparty requires
		byte(0),                            // private channel
		byte(0),                            // wumbo (0 -> standard channel)
		byte(0),                            // channel type (0 -> implicit negotiation)
		byte(0), byte(0), byte(0), byte(0), // funding amount
		byte(0),                          // percentual of pushing amount
		byte(0),                          // csv delay (0 -> default)
		byte(EvPeerInteraction), byte(0), // advance flow 2
		byte(EvPeerInteraction), byte(0), // advance flow 2

		// Switch back to flow 1 (selector 0 -> first flow)
		// and advance.
		byte(EvSwitchFlow), byte(0),
		byte(EvPeerInteraction), byte(0), // advance flow 1

		// Mine blocks (global: affects every flow awaiting
		// confirmation).
		byte(ConfFundChannelTx),byte(0),
		byte(ConfFundChannelTx),byte(0),
		byte(ConfFundChannelTx),byte(0),

		// Switch back to flow 2 (selector 0 -> first flow)
		// and advance.
		byte(EvSwitchFlow), byte(0),
		byte(EvPeerInteraction), byte(0)}, // advance flow 1,
	)

	// Seed: a zero-conf channel (channel-type selector 4) opened as the local
	// funder. A zero-conf channel opens during the handshake with no block to
	// mine, so four advances walk it OpenChannel -> AcceptChannel ->
	// FundingCreated -> FundingSigned -> broadcast + channel_ready exchange ->
	// StageOpen (see openZeroConf).
	f.Add([]byte{
		byte(EvStartAsLocalFunder),
		byte(0),                            // local features (preset-driven)
		byte(0),                            // remote features (preset-driven)
		byte(0),                            // confirmation depth (n/a zero-conf)
		byte(0),                            // private channel
		byte(0),                            // wumbo (standard)
		byte(4),                            // channel type -> zero-conf preset
		byte(0), byte(0), byte(0), byte(0), // funding amount
		byte(0),                          // push amount
		byte(0),                          // csv delay
		byte(EvPeerInteraction), byte(0), // advance: -> AcceptChannel
		byte(EvPeerInteraction), byte(0), // advance: -> FundingCreated
		byte(EvPeerInteraction), byte(0), // advance: -> FundingSigned
		byte(EvPeerInteraction), byte(0), // advance: broadcast + zero-conf open
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		fsm := newFuzzFSM(t)
		fsm.consume(data)
	})
}
