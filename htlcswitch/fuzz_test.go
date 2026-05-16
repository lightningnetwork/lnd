package htlcswitch

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	fnopt "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

const (
	// Size of the buffer for outgoing channel updates pending to be sent to
	// the peer. Kept small so fuzz tests exercise both message generation
	// and delivery rather than spending all resources on generation alone.
	pendingUpdatesBufSize = 50

	// Data length required for fuzzing.
	setupDataSize = 134

	// Configuration offsets.
	remoteConfigOffset = 84
	localConfigOffset  = 109

	// For the fuzz tests, cap the block height to roughly the year 2142.
	blockHeightCap = 6990480

	// Maximum and minimum limits on channel capacity currently enforced by
	// LND.
	maxFundingAmt = btcutil.Amount(1<<24) - 1
	minFundingAmt = btcutil.Amount(20000)
)

// fuzzState represents the different states in the HTLC fuzz state machine.
type fuzzState uint8

// Fuzz state machine actions.
const (
	// Send an AddHTLC message.
	sendAddHTLC fuzzState = iota

	// Send a CommitSig message for outstanding HTLCs.
	sendCommitSig

	// Send a UpdateFee message to the peer.
	sendUpdateFee

	// Process the stfu state between peers (e.g., initialize stfu or resume
	// the state).
	exchangeStfu

	// Exchange protocol messages between peers (e.g., UpdateFulfill,
	// UpdateFail, RevokeAndAck).
	exchangeStateUpdates

	// Update the block height in the fuzz network, this will always
	// increase the block height based on the fuzz data.
	updateBlockHeight

	// Restart nodes.
	restartNode

	// total number of fuzz state actions.
	numFuzzStates
)

// expectedErrors tracks which error conditions are expected during fuzzing.
// These flags indicate that the link may legitimately receive and reject
// invalid messages.
type expectedErrors struct {
	allowed map[errorCode]struct{}
}

// newExpectedErrors initializes an empty expectedErrors set.
func newExpectedErrors() *expectedErrors {
	return &expectedErrors{
		allowed: make(map[errorCode]struct{}),
	}
}

// allow marks the given error codes as expected.
func (e *expectedErrors) allow(codes ...errorCode) {
	for _, code := range codes {
		e.allowed[code] = struct{}{}
	}
}

// allows returns true if any of the provided error codes are expected.
func (e *expectedErrors) allows(codes ...errorCode) bool {
	for _, code := range codes {
		if _, ok := e.allowed[code]; ok {
			return true
		}
	}

	return false
}

// revoke removes the given error codes from the expected set.
func (e *expectedErrors) revoke(codes ...errorCode) {
	for _, code := range codes {
		delete(e.allowed, code)
	}
}

// merge adds all expected errors from another set.
func (e *expectedErrors) merge(other *expectedErrors) {
	if other == nil {
		return
	}

	for code := range other.allowed {
		e.allowed[code] = struct{}{}
	}
}

// fuzzNetwork represents a test network harness used for fuzzing HTLC state
// transitions between a local and a remote peer.
type fuzzNetwork struct {
	t           *testing.T
	data        []byte
	offset      int
	blockHeight *uint32

	remoteRegistry *mockInvoiceRegistry
	remoteLink     *channelLink
	remoteChannel  *testLightningChannel

	localRegistry *mockInvoiceRegistry
	localLink     *channelLink
	localChannel  *testLightningChannel
	localErrors   *expectedErrors
}

// HTLCFuzzParams holds parameters for HTLC creation during fuzz testing.
type HTLCFuzzParams struct {
	attemptID      uint64
	addInvoice     bool
	isRemoteSender bool
	amount         lnwire.MilliSatoshi
	preimage       lntypes.Preimage
	hash           [32]byte
	finalCLTVDelta int32
}

// byteToFloat64 converts a byte to a float64 in range [0.1, 1.0].
func byteToFloat64(b byte) float64 {
	return 0.1 + (float64(b)/255.0)*(0.9)
}

// getUint64 extracts a uint64 from a byte slice.
func getUint64(data []byte) uint64 {
	return binary.BigEndian.Uint64(data)
}

// getInt64 extracts a non-negative int64 from a byte slice.
func getInt64(data []byte) int64 {
	return int64(getUint64(data) % uint64(math.MaxInt64+1))
}

// getUint32 extracts a uint32 from a byte slice.
func getUint32(data []byte) uint32 {
	return binary.BigEndian.Uint32(data)
}

// getInt32 extracts a non-negative int32 from a byte slice.
func getInt32(data []byte) int32 {
	return int32(getUint32(data) % uint32(math.MaxInt32+1))
}

// ParsePrivKey parses raw private key bytes and returns the parsed private key,
// along with a boolean indicating whether the provided bytes represent a valid
// private key.
//
// NOTE: Ideally, this should be placed in btcd, but for now it is defined here.
func ParsePrivKey(privKeyBytes []byte) (*btcec.PrivateKey, bool) {
	var key btcec.ModNScalar
	overflows := key.SetByteSlice(privKeyBytes)
	if overflows || key.IsZero() {
		return nil, false
	}

	return btcec.PrivKeyFromScalar(&key), true
}

// createChannelLink constructs a channelLink for fuzz testing.
func createChannelLink(t *testing.T, privKey *btcec.PrivateKey, peer *mockPeer,
	channel *lnwallet.LightningChannel, registry *mockInvoiceRegistry,
	data []byte, blockHeight *uint32, isRemoteSide bool) (*channelLink,
	*expectedErrors) {

	feeEstimator := chainfee.NewStaticEstimator(
		chainfee.SatPerKWeight(getInt64(data[0:8])),
		chainfee.SatPerKWeight(getInt64(data[8:16])),
	)

	sphinxRouter := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: privKey},
		sphinx.NewMemoryReplayLog(),
	)
	require.NoError(t, sphinxRouter.Start())
	t.Cleanup(func() { sphinxRouter.Stop() })

	decoder := hop.NewOnionProcessor(sphinxRouter)
	pCache := newMockPreimageCache()
	expectedErrs := newExpectedErrors()

	notifyContractUpdate := func(u *contractcourt.ContractUpdate) error {
		return nil
	}

	getAliases := func(base lnwire.ShortChannelID) []lnwire.ShortChannelID {
		return nil
	}

	forwardPackets := func(linkQuit <-chan struct{}, _ bool,
		packets ...*htlcPacket) error {

		for _, packet := range packets {
			// Currently we are considering direct payments only in
			// the fuzz test, so no forwarding should happen.
			if _, ok := packet.htlc.(*lnwire.UpdateAddHTLC); ok {
				t.Fatalf("unexpected forwarded HTLC "+
					"packets: %v", packets)
			}
		}

		return nil
	}

	onChannelFailure := func(_ lnwire.ChannelID,
		_ lnwire.ShortChannelID, linkErr LinkFailureError) {

		// We only check link failures on the local side. In this fuzz
		// harness, we assume that the remote side may behave
		// arbitrarily and attempt to play smart. Because of this, the
		// remote side may itself encounter link errors due to sending
		// messages that are not in sync with the peer.
		//
		// If the channel has already been marked as borked, then in our
		// fuzz tests we can restart the node and skip the fail-link
		// check. After that, we might receive various link errors.
		// Although this would never happen in production, so in case of
		// a borked channel we will ignore any error checking.
		if !channel.State().HasChanStatus(channeldb.ChanStatusBorked) &&
			!isRemoteSide {

			require.True(t, expectedErrs.allows(linkErr.code),
				"unexpected link error: %v", linkErr.code)
		}
	}

	bestHeight := func() uint32 { return *blockHeight }

	link := NewChannelLink(
		ChannelLinkConfig{
			BestHeight:             bestHeight,
			Peer:                   peer,
			Circuits:               &mockCircuitMap{},
			ForwardPackets:         forwardPackets,
			DecodeHopIterators:     decoder.DecodeHopIterators,
			ExtractErrorEncrypter:  decoder.ExtractErrorEncrypter,
			FetchLastChannelUpdate: mockGetChanUpdateMessage,
			Registry:               registry,
			FeeEstimator:           feeEstimator,
			PreimageCache:          pCache,
			UpdateContractSignals: func(*contractcourt.
				ContractSignals) error {

				return nil
			},
			NotifyContractUpdate: notifyContractUpdate,
			ChainEvents: &contractcourt.
				ChainEventSubscription{},
			BatchSize:           10000,
			BatchTicker:         &noopTicker{},
			FwdPkgGCTicker:      &noopTicker{},
			PendingCommitTicker: &noopTicker{},
			MinUpdateTimeout:    30 * time.Minute,
			MaxUpdateTimeout:    40 * time.Minute,
			OnChannelFailure:    onChannelFailure,
			SyncStates:          true,
			MaxFeeAllocation:    byteToFloat64(data[16]),
			MaxFeeExposure: lnwire.MilliSatoshi(
				getUint64(data[17:25]),
			),
			NotifyActiveLink:        func(wire.OutPoint) {},
			NotifyActiveChannel:     func(wire.OutPoint) {},
			NotifyInactiveChannel:   func(wire.OutPoint) {},
			NotifyInactiveLinkEvent: func(wire.OutPoint) {},
			NotifyChannelUpdate: func(*channeldb.OpenChannel) {
			},
			HtlcNotifier: &mockHTLCNotifier{},
			GetAliases:   getAliases,
		},
		channel,
	)

	// Attach a mock mailbox.
	link.AttachMailBox(&mockMailBox{})

	channelLink, ok := link.(*channelLink)
	require.True(t, ok)

	return channelLink, expectedErrs
}

// setupSide initializes one side of invoice registry and channel link.
func setupSide(t *testing.T, privKey *btcec.PrivateKey, remotePub [33]byte,
	channel *lnwallet.LightningChannel, data []byte, blockHeight *uint32,
	syncMsg lnwire.Message, isRemoteSide, canGetSyncErr bool) (
	*mockInvoiceRegistry, *channelLink, *expectedErrors) {

	registry := newMockRegistry(t)
	link, expectedErrors := createChannelLink(
		t, privKey, createMockPeer(remotePub), channel, registry, data,
		blockHeight, isRemoteSide,
	)

	// We might get a sync/internal error in the link if the peer has sent
	// us a malformed channel_reestablish message.
	if canGetSyncErr {
		expectedErrors.allow(
			ErrSyncError, ErrRecoveryError, ErrInternalError,
		)
	}

	// Forcefully share the channel_reestablish message to mark the link as
	// reestablished. If this is not done forcefully, the resumeLink
	// goroutine will block.
	link.upstream = make(chan lnwire.Message, 1)
	link.upstream <- syncMsg
	_ = link.resumeLink(t.Context())

	return registry, link, expectedErrors
}

// setupFuzzNetwork creates a two peer network for fuzz testing the HTLC state
// machine.
func setupFuzzNetwork(t *testing.T, data []byte) *fuzzNetwork {
	if len(data) < setupDataSize {
		return nil
	}

	_, chanID := genID()

	// Cap the channel size to the maximum channel size currently accepted
	// on the Bitcoin chain within the Lightning Protocol, and also enforce
	// a minimum equal to the smallest channel size currently accepted by
	// LND.
	chanCapacity := minFundingAmt + (btcutil.Amount(getInt64(data[0:8])) %
		(maxFundingAmt - minFundingAmt + 1))
	aliceAmount := btcutil.Amount(getInt64(data[8:16])) %
		(chanCapacity + 1)
	bobAmount := chanCapacity - aliceAmount

	// The maximum limit on channel reserves is set to be 20% of the channel
	// capacity.
	maxReserve := chanCapacity / 5
	aliceReserve := btcutil.Amount(getInt64(data[16:24])) % (maxReserve + 1)
	bobReserve := btcutil.Amount(getInt64(data[24:32])) % (maxReserve + 1)

	// The dust limit must be less than or equal to the channel reserve on
	// that side (as specified in BOLT #2). Also, the smaller channel
	// reserve must be greater than or equal to the larger dust limit to
	// avoid stuck channels.
	aliceDustLimit := btcutil.Amount(getInt64(data[32:40])) %
		(aliceReserve + 1)
	bobDustLimit := btcutil.Amount(getInt64(data[40:48])) % (bobReserve + 1)
	aliceDustLimit = min(aliceDustLimit, aliceReserve, bobReserve)
	bobDustLimit = min(bobDustLimit, aliceReserve, bobReserve)

	// If the minimum HTLC limit is too large, then the channel won't be
	// useful for sending any payments.
	aliceMinHTLC := lnwire.MilliSatoshi(getUint64(
		data[48:56]) % (uint64(chanCapacity + 1)),
	)
	bobMinHTLC := lnwire.MilliSatoshi(
		getUint64(data[56:64]) % (uint64(chanCapacity + 1)),
	)

	aliceFeeWu := lntypes.WeightUnit(getUint64(data[64:72]))

	// Since we are enforcing valid fee limits during startup, we cap the
	// fee rate between the allowed minimum (253 sats) and maximum
	// (initiator's balance) values.
	maxFeePerKw := chainfee.NewSatPerKWeight(aliceAmount, aliceFeeWu)
	aliceFeePerKw := min(
		max(
			chainfee.FeePerKwFloor,
			chainfee.SatPerKWeight(getInt64(data[72:80])),
		),
		maxFeePerKw,
	)

	blockHeight := getUint32(data[80:84]) % (blockHeightCap + 1)

	remoteKeyPriv, remoteKeyPub := btcec.PrivKeyFromBytes(alicePrivKey)
	localKeyPriv, localKeyPub := btcec.PrivKeyFromBytes(bobPrivKey)

	remotePub := [33]byte(remoteKeyPub.SerializeCompressed())
	localPub := [33]byte(localKeyPub.SerializeCompressed())

	// Create lightning channels between Local and Remote peers.
	remoteChannel, localChannel, err := createTestChannel(t, alicePrivKey,
		bobPrivKey, aliceAmount, bobAmount, aliceReserve, bobReserve,
		aliceDustLimit, bobDustLimit, aliceFeePerKw, aliceMinHTLC,
		bobMinHTLC, aliceFeeWu, chanID,
	)
	require.NoError(t, err)

	// Remote side setup.
	localChanSyncMsg, err := localChannel.channel.State().ChanSyncMsg()
	require.NoError(t, err)
	remoteRegistry, remoteLink, _ := setupSide(
		t, remoteKeyPriv, localPub, remoteChannel.channel,
		data[remoteConfigOffset:], &blockHeight, localChanSyncMsg,
		true, false,
	)

	// Local side setup.
	remoteChanSyncMsg, err := remoteChannel.channel.State().ChanSyncMsg()
	require.NoError(t, err)
	localRegistry, localLink, localErrors := setupSide(
		t, localKeyPriv, remotePub, localChannel.channel,
		data[localConfigOffset:], &blockHeight, remoteChanSyncMsg,
		false, false,
	)

	return &fuzzNetwork{
		t:           t,
		data:        data,
		offset:      setupDataSize,
		blockHeight: &blockHeight,

		remoteRegistry: remoteRegistry,
		remoteLink:     remoteLink,
		remoteChannel:  remoteChannel,

		localRegistry: localRegistry,
		localLink:     localLink,
		localChannel:  localChannel,
		localErrors:   localErrors,
	}
}

// getBytes returns the next required bytes from the fuzz input and advances the
// offset.
func (fn *fuzzNetwork) getBytes(required int) []byte {
	b := fn.data[fn.offset : fn.offset+required]
	fn.offset += required

	return b
}

// hasEnoughData checks if there's sufficient data remaining.
func (fn *fuzzNetwork) hasEnoughData(required int) bool {
	return fn.offset+required <= len(fn.data)
}

// parseHTLCParams extracts HTLC parameters from fuzz data.
func (fn *fuzzNetwork) parseHTLCParams(attemptID uint64) HTLCFuzzParams {
	params := HTLCFuzzParams{
		attemptID:      attemptID,
		addInvoice:     uint64(fn.getBytes(1)[0])%2 > 0,
		isRemoteSender: uint64(fn.getBytes(1)[0])%2 > 0,
		// Set the amount/CLTV delta to be greater than 0, and cap the
		// amount at 21 million BTC.
		amount: lnwire.MilliSatoshi(
			max(1, (getUint64(fn.getBytes(8)) %
				uint64(1000*btcutil.MaxSatoshi+1))),
		),
		finalCLTVDelta: max(1, getInt32(fn.getBytes(4))),
	}

	// Extract preimage from fuzz data.
	copy(params.preimage[:], fn.getBytes(32))
	params.hash = sha256.Sum256(params.preimage[:])

	return params
}

// addInvoiceToRegistry adds an invoice to the appropriate registry.
func (fn *fuzzNetwork) addInvoiceToRegistry(params HTLCFuzzParams) {
	invoice := invoices.Invoice{
		CreationDate: time.Now(),
		Terms: invoices.ContractTerm{
			FinalCltvDelta:  params.finalCLTVDelta,
			PaymentPreimage: &params.preimage,
			Features: lnwire.NewFeatureVector(
				nil, lnwire.Features,
			),
		},
	}

	var registry *mockInvoiceRegistry
	switch {
	case params.isRemoteSender:
		registry = fn.localRegistry
	default:
		registry = fn.remoteRegistry
	}

	// We will skip checking the error returned while adding the invoice to
	// the registry because there are cases where the fuzzer may generate a
	// duplicate preimage or an all-zero preimage. In such cases, the
	// invoice won't be added, so we will proceed with the payment with
	// unknown hash case.
	_ = registry.AddInvoice(fn.t.Context(), invoice, params.hash)
}

// buildRoute constructs a route for the HTLC.
func buildRoute(sender, receiver *channelLink,
	amount lnwire.MilliSatoshi, timelock uint32) *route.Route {

	hop := &route.Hop{
		PubKeyBytes:      sender.PeerPubKey(),
		AmtToForward:     amount,
		OutgoingTimeLock: timelock,
	}

	return &route.Route{
		TotalTimeLock: timelock,
		TotalAmount:   amount,
		SourcePubKey:  receiver.PeerPubKey(),
		Hops:          []*route.Hop{hop},
	}
}

// generateOnionBlob creates an encoded onion packet.
func (fn *fuzzNetwork) generateOnionBlob(sphinxPath *sphinx.PaymentPath,
	paymentHash [32]byte) [lnwire.OnionPacketSize]byte {

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(fn.t, err)

	onionPacket, err := sphinx.NewOnionPacket(
		sphinxPath, sessionKey, paymentHash[:],
		sphinx.DeterministicPacketFiller,
	)
	require.NoError(fn.t, err)

	var buffer bytes.Buffer
	err = onionPacket.Encode(&buffer)
	require.NoError(fn.t, err)

	var blob [lnwire.OnionPacketSize]byte
	copy(blob[:], buffer.Bytes())

	return blob
}

// createAddHTLCFromParams creates an HTLC from fuzz parameters.
func (fn *fuzzNetwork) createAddHTLCFromParams(
	params HTLCFuzzParams) *lnwire.UpdateAddHTLC {

	if params.addInvoice {
		fn.addInvoiceToRegistry(params)
	}

	// Determine sender and receiver based on forwarding direction.
	sender := fn.localLink
	receiver := fn.remoteLink
	if params.isRemoteSender {
		sender = fn.remoteLink
		receiver = fn.localLink
	}

	timeLock := *fn.blockHeight + uint32(params.finalCLTVDelta)

	route := buildRoute(sender, receiver, params.amount, timeLock)
	sphinxPath, err := route.ToSphinxPath()
	require.NoError(fn.t, err)

	blob := fn.generateOnionBlob(sphinxPath, params.hash)

	return &lnwire.UpdateAddHTLC{
		PaymentHash: params.hash,
		Amount:      params.amount,
		Expiry:      timeLock,
		OnionBlob:   blob,
	}
}

// LND imposes a maximum channel buffer size of 50 for sending channel update
// messages. Once the buffer is full, additional messages must wait until buffer
// space becomes available. Therefore, before creating/sending more updates,
// check whether buffer space is available, otherwise, first exchange the
// pending updates.
// Also, if the sender is the local link and it has failed, it will not create
// any further updates. We don't check this for the remote sender, since remote
// side may behave arbitrarily.
func (fn *fuzzNetwork) canCreateUpdate(sender *channelLink,
	isRemoteSender bool) bool {

	if !isRemoteSender && sender.failed {
		return false
	}

	peer, ok := sender.cfg.Peer.(*mockPeer)
	require.True(fn.t, ok)

	if len(peer.sentMsgs)+1 > pendingUpdatesBufSize {
		return false
	}

	// Cap the number of uncommitted pending updates to prevent the sentMsgs
	// buffer from overflowing during a node restart.
	//
	// Pending updates can pile up because the fuzzer may generate many
	// update actions (sendUpdateFee, sendAddHTLC) without interleaving a
	// sendCommitSig to commit them.
	//
	// When a restart occurs, the node replays all pending log updates (N),
	// plus up to 3 additional messages (a revocation, a CommitSig, and
	// possibly a second CommitSig). Each message fills the buffer, which
	// blocks when it becomes full. If N + 3 exceeds pendingUpdatesBufSize,
	// the send blocks indefinitely and the test times out.
	//
	// Therefore, we enforce N + 3 < pendingUpdatesBufSize
	pending := sender.channel.NumPendingUpdates(
		lntypes.Local, lntypes.Remote,
	)

	return pending+3 < pendingUpdatesBufSize
}

// processHTLCAdd handles the HTLC addition actions.
func (fn *fuzzNetwork) processHTLCAdd(attemptID uint64) {
	// Ensure we have enough data for HTLC parameters.
	if !fn.hasEnoughData(46) {
		return
	}

	// Send HTLC through the appropriate link.
	params := fn.parseHTLCParams(attemptID)
	sender := fn.localLink

	// We may receive a link failure if the given HTLC increases the dust
	// output or commit fee such that the configured maximum fee exposure is
	// exceeded.
	//
	// The peer might return a link error due to an invalid update. This can
	// happen when the (for e.g.) local side has queued HTLCs that are not
	// yet committed or known to the remote peer. When the remote then sends
	// an HTLC that appears valid from its own perspective, the local side
	// may reject it because its own queued HTLCs have already consumed
	// enough headroom to push the combined balance below chan reserve.
	if params.isRemoteSender {
		sender = fn.remoteLink
		fn.localErrors.allow(ErrInternalError, ErrInvalidUpdate)
	}

	if !fn.canCreateUpdate(sender, params.isRemoteSender) {
		return
	}

	htlc := fn.createAddHTLCFromParams(params)

	packet := &htlcPacket{
		incomingChanID: hop.Source,
		incomingHTLCID: params.attemptID,
		outgoingChanID: sender.ShortChanID(),
		htlc:           htlc,
		amount:         htlc.Amount,
	}
	circuit := newPaymentCircuit(&htlc.PaymentHash, packet)
	packet.circuit = circuit

	// If adding this HTLC would exceed the maximum fee exposure or would
	// cause our channel balance or the peer's channel balance to fall below
	// the channel reserve requirement, the HTLC addition will fail. So we
	// will not add further HTLCs.
	_ = sender.handleDownstreamUpdateAdd(fn.t.Context(), packet)
}

// processCommitSig handles sending a CommitSig message to the peer.
func (fn *fuzzNetwork) processCommitSig() {
	// Ensure we have enough data for CommitSignatures.
	if !fn.hasEnoughData(1) {
		return
	}

	isRemoteSender := uint64(fn.getBytes(1)[0])%2 > 0
	sender := fn.localLink
	if isRemoteSender {
		sender = fn.remoteLink
	}

	if !fn.canCreateUpdate(sender, isRemoteSender) {
		return
	}

	// Send the commit_sig message only if there are pending commitment
	// update messages on the sender side, or if the sender is the remote
	// node.
	pending := sender.channel.NumPendingUpdates(
		lntypes.Local, lntypes.Remote,
	)
	if pending > 0 || isRemoteSender {
		_ = sender.updateCommitTx(fn.t.Context())
	}
}

// processUpdateFee handles sending a UpdateFee message to the peer.
func (fn *fuzzNetwork) processUpdateFee() {
	// Ensure we have enough data for UpdateFee.
	if !fn.hasEnoughData(9) {
		return
	}

	isRemoteSender := uint64(fn.getBytes(1)[0])%2 > 0
	sender := fn.localLink

	// We may receive a link failure if the given fee increases the dust
	// output or commit fee such that the configured maximum fee exposure is
	// exceeded.
	if isRemoteSender {
		sender = fn.remoteLink
		fn.localErrors.allow(ErrInternalError)
	}

	if !fn.canCreateUpdate(sender, isRemoteSender) {
		return
	}

	// The minimum feePerKw limit is 253, so ensure that the fee is set
	// above this value.
	// Also we will cap the fee rate within the max fee rate that we can use
	// given our max fee allocation and given the local reserve balance that
	// we must preserve.
	maxFeeRate, _ := sender.channel.MaxFeeRate(sender.cfg.MaxFeeAllocation)
	rawFee := chainfee.SatPerKWeight(getInt64(fn.getBytes(8)))

	feePerKw := max(chainfee.FeePerKwFloor, rawFee)
	feePerKw = min(feePerKw, maxFeeRate)

	_ = sender.updateChannelFee(fn.t.Context(), feePerKw)
}

// processStfu drives the quiescence (stfu) state transition for a channel link.
func (fn *fuzzNetwork) processStfu() {
	// Ensure we have enough data to drive the stfu transition.
	if !fn.hasEnoughData(3) {
		return
	}

	isRemoteSender := uint64(fn.getBytes(1)[0])%2 > 0
	sender := fn.localLink
	if isRemoteSender {
		sender = fn.remoteLink
	}

	if !fn.canCreateUpdate(sender, isRemoteSender) {
		return
	}

	switch fn.getBytes(1)[0] % 3 {
	case 0:
		// Send the stfu message.
		req, _ := fnopt.NewReq[
			fnopt.Unit, fnopt.Result[lntypes.ChannelParty],
		](fnopt.Unit{})
		_ = sender.handleQuiescenceReq(req)
	case 1:
		if isRemoteSender {
			// remote sender will send the stfu anyway.
			stfu := &lnwire.Stfu{
				ChanID:    sender.ChanID(),
				Initiator: (fn.getBytes(1)[0] % 2) == 0,
			}
			_ = sender.cfg.Peer.SendMessage(false, stfu)
		}
	default:
		// Stop the quiescence state.
		sender.quiescer.Resume()
	}

	// Local can experience a link failure if the remote sends an invalid
	// stfu message.
	//
	// Remote can also experience errors if it forcefully sends an stfu
	// message to the peer and then receives an stfu reply back, which the
	// remote was not expecting.
	//
	// Either of them may experience a link failure if they send the stfu,
	// but then resume their state before receiving the stfu update from the
	// peer.
	//
	// Because of this, the link can fail on the remote side, but we will
	// ignore the link failure on the remote side and still proceed (in a
	// real scenario, the remote link can be restarted to fix the issue).
	fn.localErrors.allow(ErrStfuViolation)
}

// maybeMalformMessage conditionally mutates an lnwire message using
// fuzzing input data. Mutation is applied only for messages received from a
// remote peer and only when the initial selector byte allows it.
//
// Fuzz data is consumed sequentially starting from offset. Each field mutation
// uses its own selector byte. If insufficient data is available for a field,
// that field is left unchanged while others may still be mutated.
func (fn *fuzzNetwork) maybeMalformMessage(msg lnwire.Message,
	isRemoteSender bool) lnwire.Message {

	if !isRemoteSender || !fn.hasEnoughData(1) {
		return msg
	}

	// skip malformation for even selector bytes.
	if fn.getBytes(1)[0]%2 == 0 {
		return msg
	}

	canMutate := func(n int, useSelector bool) bool {
		// If we don't want to consume a selector byte, mutation is
		// allowed.
		if !useSelector {
			return fn.hasEnoughData(n)
		}

		if !fn.hasEnoughData(n + 1) {
			return false
		}

		allowed := (fn.getBytes(1)[0] % 2) == 0

		return allowed
	}

	mutate := func(mut func([]byte), size int, errs ...errorCode) {
		if !canMutate(size, true) {
			return
		}

		mut(fn.getBytes(size))
		fn.localErrors.allow(errs...)
	}

	switch m := msg.(type) {
	case *lnwire.UpdateAddHTLC:
		out := *m

		// ID
		mutate(
			func(b []byte) { out.ID = getUint64(b) },
			8, ErrInvalidUpdate,
		)

		// Amount
		//
		// If the remote malforms the HTLC amount, the commitment
		// signature later sent for this HTLC will not be validated
		// on the receiving node.
		mutate(
			func(b []byte) {
				out.Amount = lnwire.MilliSatoshi(getUint64(b))
			},
			8, ErrInvalidUpdate, ErrInvalidCommitment,
		)

		// Expiry
		//
		// If the peer malforms the Expiry field in the HTLC add, then
		// when the remote sends the commit sig for this HTLC, the
		// signature will not match on the local side. This happens
		// because the local side computes the signature using the
		// malformed expiry, while the remote side signs using the
		// correct value, even though it previously sent the malformed
		// add HTLC.
		mutate(
			func(b []byte) { out.Expiry = getUint32(b) },
			4, ErrInvalidUpdate, ErrInvalidCommitment,
		)

		// OnionBlob
		mutate(
			func(b []byte) { copy(out.OnionBlob[:], b) },
			1366, ErrInvalidUpdate,
		)

		// BlindingPoint
		//
		//nolint:ll
		mutate(
			func(b []byte) {
				// Here we only malform the blinding key if it is a valid private key.
				// The reason is that, although in theory a peer can send any private key,
				// even one that is not valid on the secp256k1 curve, such keys will be
				// discarded when decoding the add HTLC message from the peer. Therefore,
				// only a valid (but incorrect) private key will reach the link layer.
				blinding, isValid := ParsePrivKey(b)
				if isValid {
					out.BlindingPoint = tlv.SomeRecordT(tlv.NewPrimitiveRecord[lnwire.BlindingPointTlvType](blinding.PubKey()))
				}
			},
			32, ErrInvalidUpdate,
		)

		return &out

	case *lnwire.UpdateFulfillHTLC:
		out := *m

		// ID
		mutate(
			func(b []byte) { out.ID = getUint64(b) },
			8, ErrInvalidUpdate,
		)

		// PaymentPreimage
		mutate(
			func(b []byte) { copy(out.PaymentPreimage[:], b) },
			32, ErrInvalidUpdate,
		)

		return &out

	case *lnwire.UpdateFailMalformedHTLC:
		out := *m

		// ID
		mutate(
			func(b []byte) { out.ID = getUint64(b) },
			8, ErrInvalidUpdate,
		)

		// FailureCode
		//
		//nolint:ll
		mutate(
			func(b []byte) {
				out.FailureCode = lnwire.FailCode(binary.BigEndian.Uint16(b))
			},
			2, ErrInvalidUpdate,
		)

		// ShaOnionBlob
		mutate(
			func(b []byte) { copy(out.ShaOnionBlob[:], b) },
			32, ErrInvalidUpdate,
		)

		return &out

	case *lnwire.UpdateFailHTLC:
		out := *m

		// ID
		mutate(
			func(b []byte) { out.ID = getUint64(b) },
			8, ErrInvalidUpdate,
		)

		// Reason
		//
		//nolint:ll
		mutate(
			func(b []byte) {
				length := int(binary.BigEndian.Uint16(b))
				if canMutate(length, false) {
					out.Reason = lnwire.OpaqueReason(fn.getBytes(length))
					fn.localErrors.allow(ErrInvalidUpdate)
				}
			}, 2,
		)

		return &out

	case *lnwire.CommitSig:
		out := *m

		// CommitSig
		mutate(
			func(b []byte) {
				sig, err := lnwire.NewSigFromWireECDSA(b)
				require.NoError(fn.t, err)
				out.CommitSig = sig
			},
			64, ErrInvalidCommitment,
		)

		// HTLC sigs
		//
		// If we are dropping any HTLC sigs, the peer could send
		// us an invalid commitment, because the actual HTLC
		// sigs count do not match the expected ones.
		//
		//nolint:ll
		mutate(
			func(b []byte) {
				var mutatedHtlcSigs []lnwire.Sig
				iterations := int(b[0])
				sigIdx := 0

				for range iterations {
					if sigIdx < len(out.HtlcSigs) {
						// From the original HTLC sig, we will either keep
						// it or drop it during this malformation.
						if canMutate(0, true) {
							mutatedHtlcSigs = append(mutatedHtlcSigs, out.HtlcSigs[sigIdx])
						}
						sigIdx++
					}

					if canMutate(64, true) {
						sig, err := lnwire.NewSigFromWireECDSA(fn.getBytes(64))
						require.NoError(fn.t, err)
						mutatedHtlcSigs = append(mutatedHtlcSigs, sig)
					}
				}

				out.HtlcSigs = mutatedHtlcSigs
			},
			1, ErrInvalidCommitment,
		)

		return &out

	case *lnwire.RevokeAndAck:
		out := *m

		// Revocation
		mutate(
			func(b []byte) { copy(out.Revocation[:], b) },
			32, ErrInvalidRevocation,
		)

		// NextRevocationKey
		//
		//nolint:ll
		mutate(
			func(b []byte) {
				// Here we only malform the revocation key if it is a valid private key.
				// The reason is that, although in theory a peer can send any private key,
				// even one that is not valid on the secp256k1 curve, such keys will be
				// discarded when decoding the revoke ack message from the peer. Therefore,
				// only a valid (but incorrect) private key will reach the link layer.
				revocationKey, isValid := ParsePrivKey(b)
				if isValid {
					out.NextRevocationKey = revocationKey.PubKey()
				}
			},
			32, ErrInvalidRevocation,
		)

		return &out

	case *lnwire.UpdateFee:
		out := *m

		// FeePerKw
		//
		// LND directly accepts the update fee if it is from
		// the initiator without validating the balances.
		// However, once the peer sends the commit signature,
		// the balances are validated, and an invalid commitment
		// is returned if the fee we applied causes an invalid
		// balance amount.
		//
		//nolint:ll
		mutate(
			func(b []byte) {
				out.FeePerKw = uint32(chainfee.SatPerKWeight(getInt64(b)))
			},
			8, ErrInvalidUpdate, ErrInvalidCommitment,
		)

		return &out

	case *lnwire.ChannelReestablish:
		out := *m

		// NextLocalCommitHeight
		mutate(
			func(b []byte) {
				out.NextLocalCommitHeight = getUint64(b)
			},
			8, ErrSyncError, ErrRecoveryError,
		)

		// RemoteCommitTailHeight
		mutate(
			func(b []byte) {
				out.RemoteCommitTailHeight = getUint64(b)
			},
			8, ErrSyncError, ErrRecoveryError,
		)

		// LastRemoteCommitSecret
		mutate(
			func(b []byte) {
				copy(out.LastRemoteCommitSecret[:], b)
			},
			32, ErrSyncError, ErrRecoveryError,
		)

		// LocalUnrevokedCommitPoint
		//
		//nolint:ll
		mutate(
			func(b []byte) {
				// Here we only malform the unrevoked commit point if it is a valid private key.
				// The reason is that, although in theory a peer can send any private key,
				// even one that is not valid on the secp256k1 curve, such keys will be
				// discarded when decoding the reestablish message from the peer. Therefore,
				// only a valid (but incorrect) private key will reach the link layer.
				localUnRevokedCommitPt, isValid := ParsePrivKey(b)
				if isValid {
					out.LocalUnrevokedCommitPoint = localUnRevokedCommitPt.PubKey()
				}
			},
			32, ErrSyncError, ErrRecoveryError,
		)

		return &out

	default:
		return msg
	}
}

// maybeReorderMessages conditionally reorder outbound messages from the remote
// peer using fuzz input data.
func (fn *fuzzNetwork) maybeReorderMessages() {
	// Ensure we have enough fuzz data to drive message reordering.
	if !fn.hasEnoughData(2) {
		return
	}

	// skip reordering for even selector bytes.
	if fn.getBytes(1)[0]%2 == 0 {
		return
	}

	// Reordering is applied only to messages sent by the remote peer.
	peer, ok := fn.remoteLink.cfg.Peer.(*mockPeer)
	require.True(fn.t, ok)
	sentMsgs := peer.sentMsgs

	var msgs []lnwire.Message
readLoop:
	for {
		select {
		case msg := <-sentMsgs:
			msgs = append(msgs, msg)
		default:
			break readLoop
		}
	}

	// Swap two messages using fuzz data.
	for i := len(msgs) - 1; i > 0; i-- {
		if !fn.hasEnoughData(1) {
			break
		}
		j := int(fn.getBytes(1)[0]) % (i + 1)
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}

	// Re-inject all messages in their new order.
	for _, msg := range msgs {
		_ = fn.remoteLink.cfg.Peer.SendMessage(false, msg)
	}

	// Reordering can cause the CommitSig/RevokeAndAck message to be
	// considered malformed.
	//
	// Reordering can cause ADD HTLCs to be shuffled, resulting in different
	// HTLC IDs than the peer expects, which may cause it to return an
	// invalid update.
	fn.localErrors.allow(
		ErrInvalidCommitment, ErrInvalidRevocation, ErrInvalidUpdate,
	)
}

// exchangeUpdates handles message sending between peers.
func (fn *fuzzNetwork) exchangeUpdates() {
	// Ensure we have enough data for exchanging updates.
	if !fn.hasEnoughData(1) {
		return
	}

	isRemoteSender := uint64(fn.getBytes(1)[0])%2 > 0
	sender := fn.localLink
	receiver := fn.remoteLink
	if isRemoteSender {
		sender = fn.remoteLink
		receiver = fn.localLink

		// If the remote peer is sending a message to the local peer, we
		// may conditionally reorder the message.
		fn.maybeReorderMessages()
	}

	peer, ok := sender.cfg.Peer.(*mockPeer)
	require.True(fn.t, ok)
	sentMsgs := peer.sentMsgs

	select {
	case msg := <-sentMsgs:
		// If the receiver link has failed, it will not receive any
		// further updates. This mirrors the actual LND behavior,
		// where the htlcManager goroutine shuts down.
		if receiver.failed {
			return
		}

		// We conditionally malform the message when the remote peer
		// is sending a message to the local peer.
		mayBeMalformedMsg := fn.maybeMalformMessage(msg, isRemoteSender)

		receiver.handleUpstreamMsg(fn.t.Context(), mayBeMalformedMsg)
	default:
		// No message to send.
	}
}

// restartNode initiates the restart of the channel and channel links. If a peer
// restarts, the channel link on both sides is stopped and then restarted
// again to handle the synchronization process.
func (fn *fuzzNetwork) restartNode() {
	remoteKeyPriv, remoteKeyPub := btcec.PrivKeyFromBytes(alicePrivKey)
	localKeyPriv, localKeyPub := btcec.PrivKeyFromBytes(bobPrivKey)

	remotePub := [33]byte(remoteKeyPub.SerializeCompressed())
	localPub := [33]byte(localKeyPub.SerializeCompressed())

	// Restore LN channel on both sides.
	remoteChannel, err := fn.remoteChannel.restore()
	require.NoError(fn.t, err)
	localChannel, err := fn.localChannel.restore()
	require.NoError(fn.t, err)

	// Remote side setup.
	localChanSyncMsg, err := localChannel.State().ChanSyncMsg()
	require.NoError(fn.t, err)
	remoteRegistry, remoteLink, _ := setupSide(
		fn.t, remoteKeyPriv, localPub, remoteChannel,
		fn.data[remoteConfigOffset:], fn.blockHeight,
		localChanSyncMsg, true, false,
	)

	// Local side setup.
	remoteChanSyncMsg, err := remoteChannel.State().ChanSyncMsg()
	require.NoError(fn.t, err)
	malformedMsg := fn.maybeMalformMessage(remoteChanSyncMsg, true)

	// If we malformed the message, we might get a sync or recovery error.
	// However, if we previously expected an internal error, we may later
	// see a sync/recovery error instead. For example, if we previously sent
	// an invalid update and its commitment, it could trigger an internal
	// error. When we restart and attempt to restore the same update, we may
	// hit the same issue again, resulting in a sync/recovery error.
	localRegistry, localLink, localErrors := setupSide(
		fn.t, localKeyPriv, remotePub, localChannel,
		fn.data[localConfigOffset:], fn.blockHeight,
		malformedMsg, false,
		fn.localErrors.allows(
			ErrSyncError, ErrRecoveryError, ErrInternalError,
		),
	)

	fn.remoteChannel.channel = remoteChannel
	fn.remoteRegistry = remoteRegistry
	fn.remoteLink = remoteLink

	fn.localChannel.channel = localChannel
	fn.localRegistry = localRegistry
	fn.localLink = localLink

	// The new link is tied to the new expectedErrors instance, so carry
	// over any previously expected errors before reassigning.
	localErrors.merge(fn.localErrors)
	fn.localErrors = localErrors

	// We will revoke the STFU violation after the restart, since the
	// message queue has been reset.
	fn.localErrors.revoke(ErrStfuViolation)
}

// updateBlockHeight updates the best known block height in the fuzz network.
// The new height is selected from the fuzz data and is guaranteed to be
// monotonically increasing.
func (fn *fuzzNetwork) updateBlockHeight() {
	// Ensure we have enough data for updating block height.
	if !fn.hasEnoughData(4) {
		return
	}

	*fn.blockHeight = max(
		getUint32(fn.getBytes(4))%(blockHeightCap+1),
		*fn.blockHeight,
	)
}

// checkChannelInvariants verifies that the channel state is consistent between
// local and remote peers.
func (fn *fuzzNetwork) checkChannelInvariants() {
	localChan := fn.localLink.channel
	remoteChan := fn.remoteLink.channel
	localCommit := localChan.State().LocalCommitment
	remoteCommit := remoteChan.State().LocalCommitment

	// Check total balances.
	var localHtlcAmt, remoteHtlcAmt lnwire.MilliSatoshi
	localTotal := localCommit.LocalBalance + localCommit.RemoteBalance +
		lnwire.NewMSatFromSatoshis(localCommit.CommitFee)
	remoteTotal := remoteCommit.LocalBalance + remoteCommit.RemoteBalance +
		lnwire.NewMSatFromSatoshis(remoteCommit.CommitFee)

	for _, htlc := range localCommit.Htlcs {
		localHtlcAmt += htlc.Amt
	}
	for _, htlc := range remoteCommit.Htlcs {
		remoteHtlcAmt += htlc.Amt
	}

	require.Equal(
		fn.t, localTotal+localHtlcAmt,
		lnwire.NewMSatFromSatoshis(localChan.Capacity),
	)
	require.Equal(
		fn.t, remoteTotal+remoteHtlcAmt,
		lnwire.NewMSatFromSatoshis(remoteChan.Capacity),
	)

	// Check commitment heights.
	require.InDelta(
		fn.t, localCommit.CommitHeight,
		remoteChan.State().RemoteCommitment.CommitHeight, 1,
	)
	require.InDelta(
		fn.t, remoteCommit.CommitHeight,
		localChan.State().RemoteCommitment.CommitHeight, 1,
	)
}

// drainMessages processes all pending messages. Here, all pending messages
// are processed directly, without any malformation or further message
// reordering.
func (fn *fuzzNetwork) drainMessages() {
	remotePeer, ok := fn.remoteLink.cfg.Peer.(*mockPeer)
	require.True(fn.t, ok)

	localPeer, ok := fn.localLink.cfg.Peer.(*mockPeer)
	require.True(fn.t, ok)

	for {
		select {
		case localMsg := <-localPeer.sentMsgs:
			// If the receiver link has failed, it will not receive
			// any further updates. This mirrors the actual LND
			// behavior, where the htlcManager goroutine shuts down.
			if fn.remoteLink.failed {
				continue
			}
			fn.remoteLink.handleUpstreamMsg(
				fn.t.Context(), localMsg,
			)

		case remoteMsg := <-remotePeer.sentMsgs:
			// If the receiver link has failed, it will not receive
			// any further updates. This mirrors the actual LND
			// behavior, where the htlcManager goroutine shuts down.
			if fn.localLink.failed {
				continue
			}
			fn.localLink.handleUpstreamMsg(
				fn.t.Context(), remoteMsg,
			)

		default:
			return
		}

		fn.checkChannelInvariants()
	}
}

// runHTLCFuzzStateMachine executes the HTLC state machine with fuzz input data.
func (fn *fuzzNetwork) runHTLCFuzzStateMachine() {
	htlcID := uint64(0)
	isLastRestarted := false

	for fn.hasEnoughData(1) {
		// Extract action from current data byte
		action := fuzzState(int(fn.getBytes(1)[0]) % int(numFuzzStates))

		switch action {
		case sendAddHTLC:
			fn.processHTLCAdd(htlcID)
			htlcID++

		case sendCommitSig:
			fn.processCommitSig()

		case sendUpdateFee:
			fn.processUpdateFee()

		case exchangeStfu:
			fn.processStfu()

		case exchangeStateUpdates:
			fn.exchangeUpdates()
			isLastRestarted = false

		case updateBlockHeight:
			fn.updateBlockHeight()

		case restartNode:
			// Only restart the node if some message exchange has
			// happened between peers, otherwise, there is no point
			// in restarting the node again and again, as that will
			// only increase the fuzzing time.
			if !isLastRestarted {
				fn.restartNode()
				isLastRestarted = true
			}
		}

		fn.checkChannelInvariants()
	}

	fn.drainMessages()
}

// FuzzHTLCStates fuzz-tests the HTLC state machine. It gets the input data and
// performs operations such as add, revoke, commit, fulfill, or fail  etc.
// Message exchange is controlled using mock peer instances so we can precisely
// manage transport-layer messaging and uncover edge cases.
func FuzzHTLCStates(f *testing.F) {
	// Adding appropriate htlc state machine seed inputs.
	addRemoteFulfillSeed(f)
	addLocalFailSeed(f)
	addRemoteMalformedSeed(f)

	f.Fuzz(func(t *testing.T, data []byte) {
		network := setupFuzzNetwork(t, data)
		if network == nil {
			return
		}

		// Execute the HTLC state machine with fuzz input.
		network.runHTLCFuzzStateMachine()
	})
}

// buildHTLCFuzzSetup builds the base channel, link, and HTLC setup data used by
// the fuzz seeds.
func buildHTLCFuzzSetup() ([]byte, []byte, []byte, []byte) {
	// Preimage.
	preimage := make([]byte, 32)
	for i := range 32 {
		preimage[i] = byte(i)
	}

	// Channel setup data.
	config := make([]byte, setupDataSize)

	// Channel Configuration.
	binary.BigEndian.PutUint64(config[0:8], 50*btcutil.SatoshiPerBitcoin)
	binary.BigEndian.PutUint64(config[8:16], 50*btcutil.SatoshiPerBitcoin)
	binary.BigEndian.PutUint64(config[16:24], 0)
	binary.BigEndian.PutUint64(config[24:32], 0)
	binary.BigEndian.PutUint64(config[32:40], 200)
	binary.BigEndian.PutUint64(config[40:48], 800)
	binary.BigEndian.PutUint64(config[48:56], 0)
	binary.BigEndian.PutUint64(config[56:64], 0)
	binary.BigEndian.PutUint64(config[64:72], 6000)
	binary.BigEndian.PutUint64(config[72:80], 724)
	binary.BigEndian.PutUint32(config[80:84], 100)

	// Remote Link.
	binary.BigEndian.PutUint64(config[84:92], 10)
	binary.BigEndian.PutUint64(config[92:100], 0)
	config[100] = 114
	binary.BigEndian.PutUint64(config[101:109], 500000000)

	// Local Link.
	binary.BigEndian.PutUint64(config[109:117], 10)
	binary.BigEndian.PutUint64(config[117:125], 0)
	config[125] = 114
	binary.BigEndian.PutUint64(config[126:134], 500000000)

	data := make([]byte, 0, setupDataSize+12)
	data = append(data, config...)

	// Share the initial ChannelReestablish and ChannelReady between peers.
	data = append(data, byte(exchangeStateUpdates), 0,
		byte(exchangeStateUpdates), 1, 0, 0,
		byte(exchangeStateUpdates), 0,
		byte(exchangeStateUpdates), 1, 0, 0)

	// HTLC parameters.
	htlcAmt := make([]byte, 8)
	cltvDelta := make([]byte, 4)
	binary.BigEndian.PutUint64(htlcAmt, 1)
	binary.BigEndian.PutUint32(cltvDelta, 5)

	return data, preimage, htlcAmt, cltvDelta
}

// addRemoteFulfillSeed adds a fuzz seed that simulates a Remote-to-Local HTLC
// fulfill flow.
func addRemoteFulfillSeed(f *testing.F) {
	data, preimage, htlcAmt, cltvDelta := buildHTLCFuzzSetup()
	remoteFulfill := make([]byte, 0)

	// Base setup.
	remoteFulfill = append(remoteFulfill, data...)

	// Add HTLC.
	remoteFulfill = append(remoteFulfill, byte(sendAddHTLC), 1, 1)
	remoteFulfill = append(remoteFulfill, htlcAmt...)
	remoteFulfill = append(remoteFulfill, cltvDelta...)
	remoteFulfill = append(remoteFulfill, preimage...)

	// State transitions.
	remoteFulfill = append(remoteFulfill,
		byte(exchangeStateUpdates), 1, 0, 0,
		byte(sendCommitSig), 1,
		byte(exchangeStateUpdates), 1, 0, 0,
		byte(exchangeStateUpdates), 0,
		byte(sendCommitSig), 0,
		byte(exchangeStateUpdates), 0,
		byte(exchangeStateUpdates), 1, 0, 0,
		byte(exchangeStateUpdates), 0,
		byte(sendCommitSig), 0,
		byte(exchangeStateUpdates), 0,
		byte(exchangeStateUpdates), 1, 0, 0,
		byte(sendCommitSig), 1,
		byte(exchangeStateUpdates), 1, 0, 0,
		byte(exchangeStateUpdates), 0,
	)

	f.Add(remoteFulfill)
}

// addLocalFailSeed adds a fuzz seed that simulates a Local-to-Remote HTLC fail
// flow.
func addLocalFailSeed(f *testing.F) {
	data, preimage, htlcAmt, cltvDelta := buildHTLCFuzzSetup()
	localFail := make([]byte, 0)

	// Base setup.
	localFail = append(localFail, data...)

	// Add HTLC.
	localFail = append(localFail, byte(sendAddHTLC), 0, 0)
	localFail = append(localFail, htlcAmt...)
	localFail = append(localFail, cltvDelta...)
	localFail = append(localFail, preimage...)

	// State transitions.
	localFail = append(localFail,
		byte(exchangeStateUpdates), 0,
		byte(sendCommitSig), 0,
		byte(exchangeStateUpdates), 0,
		byte(exchangeStateUpdates), 1, 0, 0,
		byte(sendCommitSig), 1,
		byte(exchangeStateUpdates), 1, 0, 0,
		byte(exchangeStateUpdates), 0,
		byte(exchangeStateUpdates), 1, 0, 0,
		byte(sendCommitSig), 1,
		byte(exchangeStateUpdates), 1, 0, 0,
		byte(exchangeStateUpdates), 0,
		byte(sendCommitSig), 0,
		byte(exchangeStateUpdates), 0,
		byte(exchangeStateUpdates), 1, 0, 0,
	)

	f.Add(localFail)
}

// addRemoteMalformedSeed adds a fuzz seed that simulates a Remote-to-Local
// HTLC fail malformed flow.
func addRemoteMalformedSeed(f *testing.F) {
	data, preimage, htlcAmt, cltvDelta := buildHTLCFuzzSetup()
	malformedBlob := make([]byte, 1366)
	remoteMalformed := make([]byte, 0)

	// Base setup.
	remoteMalformed = append(remoteMalformed, data...)

	// Add HTLC.
	remoteMalformed = append(remoteMalformed, byte(sendAddHTLC), 1, 1)
	remoteMalformed = append(remoteMalformed, htlcAmt...)
	remoteMalformed = append(remoteMalformed, cltvDelta...)
	remoteMalformed = append(remoteMalformed, preimage...)

	// State transitions.
	remoteMalformed = append(remoteMalformed,
		byte(exchangeStateUpdates), 1, 0, 1, 1, 1, 1, 0,
	)
	remoteMalformed = append(remoteMalformed, malformedBlob...)
	remoteMalformed = append(remoteMalformed, 1)
	remoteMalformed = append(remoteMalformed,
		byte(sendCommitSig), 1,
		byte(exchangeStateUpdates), 1, 0, 0,
		byte(exchangeStateUpdates), 0,
		byte(sendCommitSig), 0,
		byte(exchangeStateUpdates), 0,
		byte(exchangeStateUpdates), 1, 0, 0,
		byte(exchangeStateUpdates), 0,
		byte(sendCommitSig), 0,
		byte(exchangeStateUpdates), 0,
		byte(exchangeStateUpdates), 1, 0, 0,
		byte(sendCommitSig), 1,
		byte(exchangeStateUpdates), 1, 0, 0,
		byte(exchangeStateUpdates), 0,
	)

	f.Add(remoteMalformed)
}
