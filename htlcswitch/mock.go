package htlcswitch

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/go-errors/errors"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/ticker"
)

type mockPreimageCache struct {
	sync.Mutex
	preimageMap map[lntypes.Hash]lntypes.Preimage
}

func newMockPreimageCache() *mockPreimageCache {
	return &mockPreimageCache{
		preimageMap: make(map[lntypes.Hash]lntypes.Preimage),
	}
}

func (m *mockPreimageCache) LookupPreimage(
	hash lntypes.Hash) (lntypes.Preimage, bool) {

	m.Lock()
	defer m.Unlock()

	p, ok := m.preimageMap[hash]
	return p, ok
}

func (m *mockPreimageCache) AddPreimages(preimages ...lntypes.Preimage) error {
	m.Lock()
	defer m.Unlock()

	for _, preimage := range preimages {
		m.preimageMap[preimage.Hash()] = preimage
	}

	return nil
}

func (m *mockPreimageCache) SubscribeUpdates() *contractcourt.WitnessSubscription {
	return nil
}

type mockFeeEstimator struct {
	byteFeeIn chan lnwallet.SatPerKWeight

	quit chan struct{}
}

func (m *mockFeeEstimator) EstimateFeePerKW(
	numBlocks uint32) (lnwallet.SatPerKWeight, error) {

	select {
	case feeRate := <-m.byteFeeIn:
		return feeRate, nil
	case <-m.quit:
		return 0, fmt.Errorf("exiting")
	}
}

func (m *mockFeeEstimator) RelayFeePerKW() lnwallet.SatPerKWeight {
	return 1e3
}

func (m *mockFeeEstimator) Start() error {
	return nil
}
func (m *mockFeeEstimator) Stop() error {
	close(m.quit)
	return nil
}

var _ lnwallet.FeeEstimator = (*mockFeeEstimator)(nil)

type mockForwardingLog struct {
	sync.Mutex

	events map[time.Time]channeldb.ForwardingEvent
}

func (m *mockForwardingLog) AddForwardingEvents(events []channeldb.ForwardingEvent) error {
	m.Lock()
	defer m.Unlock()

	for _, event := range events {
		m.events[event.Timestamp] = event
	}

	return nil
}

type mockServer struct {
	started  int32 // To be used atomically.
	shutdown int32 // To be used atomically.
	wg       sync.WaitGroup
	quit     chan struct{}

	t testing.TB

	name     string
	messages chan lnwire.Message

	errChan chan error

	id         [33]byte
	htlcSwitch *Switch

	registry         *mockInvoiceRegistry
	pCache           *mockPreimageCache
	interceptorFuncs []messageInterceptor
}

var _ lnpeer.Peer = (*mockServer)(nil)

func initDB() (*channeldb.DB, error) {
	tempPath, err := ioutil.TempDir("", "switchdb")
	if err != nil {
		return nil, err
	}

	db, err := channeldb.Open(tempPath)
	if err != nil {
		return nil, err
	}

	return db, err
}

func initSwitchWithDB(startingHeight uint32, db *channeldb.DB) (*Switch, error) {
	var err error

	if db == nil {
		db, err = initDB()
		if err != nil {
			return nil, err
		}
	}

	cfg := Config{
		DB:             db,
		SwitchPackager: channeldb.NewSwitchPackager(),
		FwdingLog: &mockForwardingLog{
			events: make(map[time.Time]channeldb.ForwardingEvent),
		},
		FetchLastChannelUpdate: func(lnwire.ShortChannelID) (*lnwire.ChannelUpdate, error) {
			return nil, nil
		},
		Notifier:              &mockNotifier{},
		FwdEventTicker:        ticker.NewForce(DefaultFwdEventInterval),
		LogEventTicker:        ticker.NewForce(DefaultLogInterval),
		AckEventTicker:        ticker.NewForce(DefaultAckInterval),
		NotifyActiveChannel:   func(wire.OutPoint) {},
		NotifyInactiveChannel: func(wire.OutPoint) {},
	}

	return New(cfg, startingHeight)
}

func newMockServer(t testing.TB, name string, startingHeight uint32,
	db *channeldb.DB, defaultDelta uint32) (*mockServer, error) {

	var id [33]byte
	h := sha256.Sum256([]byte(name))
	copy(id[:], h[:])

	pCache := newMockPreimageCache()

	htlcSwitch, err := initSwitchWithDB(startingHeight, db)
	if err != nil {
		return nil, err
	}

	registry := newMockRegistry(defaultDelta)

	return &mockServer{
		t:                t,
		id:               id,
		name:             name,
		messages:         make(chan lnwire.Message, 3000),
		quit:             make(chan struct{}),
		registry:         registry,
		htlcSwitch:       htlcSwitch,
		pCache:           pCache,
		interceptorFuncs: make([]messageInterceptor, 0),
	}, nil
}

func (s *mockServer) Start() error {
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		return errors.New("mock server already started")
	}

	if err := s.htlcSwitch.Start(); err != nil {
		return err
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		defer func() {
			s.htlcSwitch.Stop()
		}()

		for {
			select {
			case msg := <-s.messages:
				var shouldSkip bool

				for _, interceptor := range s.interceptorFuncs {
					skip, err := interceptor(msg)
					if err != nil {
						s.t.Fatalf("%v: error in the "+
							"interceptor: %v", s.name, err)
						return
					}
					shouldSkip = shouldSkip || skip
				}

				if shouldSkip {
					continue
				}

				if err := s.readHandler(msg); err != nil {
					s.t.Fatal(err)
					return
				}
			case <-s.quit:
				return
			}
		}
	}()

	return nil
}

func (s *mockServer) QuitSignal() <-chan struct{} {
	return s.quit
}

// mockHopIterator represents the test version of hop iterator which instead
// of encrypting the path in onion blob just stores the path as a list of hops.
type mockHopIterator struct {
	hops []hop.ForwardingInfo
}

func newMockHopIterator(hops ...hop.ForwardingInfo) HopIterator {
	return &mockHopIterator{hops: hops}
}

func (r *mockHopIterator) ForwardingInstructions() (
	hop.ForwardingInfo, error) {

	h := r.hops[0]
	r.hops = r.hops[1:]
	return h, nil
}

func (r *mockHopIterator) ExtraOnionBlob() []byte {
	return nil
}

func (r *mockHopIterator) ExtractErrorEncrypter(
	extracter ErrorEncrypterExtracter) (ErrorEncrypter, lnwire.FailCode) {

	return extracter(nil)
}

func (r *mockHopIterator) EncodeNextHop(w io.Writer) error {
	var hopLength [4]byte
	binary.BigEndian.PutUint32(hopLength[:], uint32(len(r.hops)))

	if _, err := w.Write(hopLength[:]); err != nil {
		return err
	}

	for _, hop := range r.hops {
		if err := encodeFwdInfo(w, &hop); err != nil {
			return err
		}
	}

	return nil
}

func encodeFwdInfo(w io.Writer, f *hop.ForwardingInfo) error {
	if _, err := w.Write([]byte{byte(f.Network)}); err != nil {
		return err
	}

	if err := binary.Write(w, binary.BigEndian, f.NextHop); err != nil {
		return err
	}

	if err := binary.Write(w, binary.BigEndian, f.AmountToForward); err != nil {
		return err
	}

	if err := binary.Write(w, binary.BigEndian, f.OutgoingCTLV); err != nil {
		return err
	}

	return nil
}

var _ HopIterator = (*mockHopIterator)(nil)

// mockObfuscator mock implementation of the failure obfuscator which only
// encodes the failure and do not makes any onion obfuscation.
type mockObfuscator struct {
	ogPacket *sphinx.OnionPacket
}

// NewMockObfuscator initializes a dummy mockObfuscator used for testing.
func NewMockObfuscator() ErrorEncrypter {
	return &mockObfuscator{}
}

func (o *mockObfuscator) OnionPacket() *sphinx.OnionPacket {
	return o.ogPacket
}

func (o *mockObfuscator) Type() EncrypterType {
	return EncrypterTypeMock
}

func (o *mockObfuscator) Encode(w io.Writer) error {
	return nil
}

func (o *mockObfuscator) Decode(r io.Reader) error {
	return nil
}

func (o *mockObfuscator) Reextract(extracter ErrorEncrypterExtracter) error {
	return nil
}

func (o *mockObfuscator) EncryptFirstHop(failure lnwire.FailureMessage) (
	lnwire.OpaqueReason, error) {

	var b bytes.Buffer
	if err := lnwire.EncodeFailure(&b, failure, 0); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func (o *mockObfuscator) IntermediateEncrypt(reason lnwire.OpaqueReason) lnwire.OpaqueReason {
	return reason
}

func (o *mockObfuscator) EncryptMalformedError(reason lnwire.OpaqueReason) lnwire.OpaqueReason {
	return reason
}

// mockDeobfuscator mock implementation of the failure deobfuscator which
// only decodes the failure do not makes any onion obfuscation.
type mockDeobfuscator struct{}

func newMockDeobfuscator() ErrorDecrypter {
	return &mockDeobfuscator{}
}

func (o *mockDeobfuscator) DecryptError(reason lnwire.OpaqueReason) (*ForwardingError, error) {

	r := bytes.NewReader(reason)
	failure, err := lnwire.DecodeFailure(r, 0)
	if err != nil {
		return nil, err
	}

	return &ForwardingError{
		FailureSourceIdx: 1,
		FailureMessage:   failure,
	}, nil
}

var _ ErrorDecrypter = (*mockDeobfuscator)(nil)

// mockIteratorDecoder test version of hop iterator decoder which decodes the
// encoded array of hops.
type mockIteratorDecoder struct {
	mu sync.RWMutex

	responses map[[32]byte][]DecodeHopIteratorResponse

	decodeFail bool
}

func newMockIteratorDecoder() *mockIteratorDecoder {
	return &mockIteratorDecoder{
		responses: make(map[[32]byte][]DecodeHopIteratorResponse),
	}
}

func (p *mockIteratorDecoder) DecodeHopIterator(r io.Reader, rHash []byte,
	cltv uint32) (HopIterator, lnwire.FailCode) {

	var b [4]byte
	_, err := r.Read(b[:])
	if err != nil {
		return nil, lnwire.CodeTemporaryChannelFailure
	}
	hopLength := binary.BigEndian.Uint32(b[:])

	hops := make([]hop.ForwardingInfo, hopLength)
	for i := uint32(0); i < hopLength; i++ {
		f := &hop.ForwardingInfo{}
		if err := decodeFwdInfo(r, f); err != nil {
			return nil, lnwire.CodeTemporaryChannelFailure
		}

		hops[i] = *f
	}

	return newMockHopIterator(hops...), lnwire.CodeNone
}

func (p *mockIteratorDecoder) DecodeHopIterators(id []byte,
	reqs []DecodeHopIteratorRequest) ([]DecodeHopIteratorResponse, error) {

	idHash := sha256.Sum256(id)

	p.mu.RLock()
	if resps, ok := p.responses[idHash]; ok {
		p.mu.RUnlock()
		return resps, nil
	}
	p.mu.RUnlock()

	batchSize := len(reqs)

	resps := make([]DecodeHopIteratorResponse, 0, batchSize)
	for _, req := range reqs {
		iterator, failcode := p.DecodeHopIterator(
			req.OnionReader, req.RHash, req.IncomingCltv,
		)

		if p.decodeFail {
			failcode = lnwire.CodeTemporaryChannelFailure
		}

		resp := DecodeHopIteratorResponse{
			HopIterator: iterator,
			FailCode:    failcode,
		}
		resps = append(resps, resp)
	}

	p.mu.Lock()
	p.responses[idHash] = resps
	p.mu.Unlock()

	return resps, nil
}

func decodeFwdInfo(r io.Reader, f *hop.ForwardingInfo) error {
	var net [1]byte
	if _, err := r.Read(net[:]); err != nil {
		return err
	}
	f.Network = hop.Network(net[0])

	if err := binary.Read(r, binary.BigEndian, &f.NextHop); err != nil {
		return err
	}

	if err := binary.Read(r, binary.BigEndian, &f.AmountToForward); err != nil {
		return err
	}

	if err := binary.Read(r, binary.BigEndian, &f.OutgoingCTLV); err != nil {
		return err
	}

	return nil
}

// messageInterceptor is function that handles the incoming peer messages and
// may decide should the peer skip the message or not.
type messageInterceptor func(m lnwire.Message) (bool, error)

// Record is used to set the function which will be triggered when new
// lnwire message was received.
func (s *mockServer) intersect(f messageInterceptor) {
	s.interceptorFuncs = append(s.interceptorFuncs, f)
}

func (s *mockServer) SendMessage(sync bool, msgs ...lnwire.Message) error {

	for _, msg := range msgs {
		select {
		case s.messages <- msg:
		case <-s.quit:
			return errors.New("server is stopped")
		}
	}

	return nil
}

func (s *mockServer) SendMessageLazy(sync bool, msgs ...lnwire.Message) error {
	panic("not implemented")
}

func (s *mockServer) readHandler(message lnwire.Message) error {
	var targetChan lnwire.ChannelID

	switch msg := message.(type) {
	case *lnwire.UpdateAddHTLC:
		targetChan = msg.ChanID
	case *lnwire.UpdateFulfillHTLC:
		targetChan = msg.ChanID
	case *lnwire.UpdateFailHTLC:
		targetChan = msg.ChanID
	case *lnwire.UpdateFailMalformedHTLC:
		targetChan = msg.ChanID
	case *lnwire.RevokeAndAck:
		targetChan = msg.ChanID
	case *lnwire.CommitSig:
		targetChan = msg.ChanID
	case *lnwire.FundingLocked:
		// Ignore
		return nil
	case *lnwire.ChannelReestablish:
		targetChan = msg.ChanID
	case *lnwire.UpdateFee:
		targetChan = msg.ChanID
	default:
		return fmt.Errorf("unknown message type: %T", msg)
	}

	// Dispatch the commitment update message to the proper channel link
	// dedicated to this channel. If the link is not found, we will discard
	// the message.
	link, err := s.htlcSwitch.GetLink(targetChan)
	if err != nil {
		return nil
	}

	// Create goroutine for this, in order to be able to properly stop
	// the server when handler stacked (server unavailable)
	link.HandleChannelUpdate(message)

	return nil
}

func (s *mockServer) PubKey() [33]byte {
	return s.id
}

func (s *mockServer) IdentityKey() *btcec.PublicKey {
	pubkey, _ := btcec.ParsePubKey(s.id[:], btcec.S256())
	return pubkey
}

func (s *mockServer) Address() net.Addr {
	return nil
}

func (s *mockServer) AddNewChannel(channel *channeldb.OpenChannel,
	cancel <-chan struct{}) error {

	return nil
}

func (s *mockServer) WipeChannel(*wire.OutPoint) error {
	return nil
}

func (s *mockServer) Stop() error {
	if !atomic.CompareAndSwapInt32(&s.shutdown, 0, 1) {
		return nil
	}

	close(s.quit)
	s.wg.Wait()

	return nil
}

func (s *mockServer) String() string {
	return s.name
}

type mockChannelLink struct {
	htlcSwitch *Switch

	shortChanID lnwire.ShortChannelID

	chanID lnwire.ChannelID

	peer lnpeer.Peer

	startMailBox bool

	mailBox MailBox

	packets chan *htlcPacket

	eligible bool

	htlcID uint64

	htlcSatifiesPolicyLocalResult lnwire.FailureMessage
}

// completeCircuit is a helper method for adding the finalized payment circuit
// to the switch's circuit map. In testing, this should be executed after
// receiving an htlc from the downstream packets channel.
func (f *mockChannelLink) completeCircuit(pkt *htlcPacket) error {
	switch htlc := pkt.htlc.(type) {
	case *lnwire.UpdateAddHTLC:
		pkt.outgoingChanID = f.shortChanID
		pkt.outgoingHTLCID = f.htlcID
		htlc.ID = f.htlcID

		keystone := Keystone{pkt.inKey(), pkt.outKey()}
		if err := f.htlcSwitch.openCircuits(keystone); err != nil {
			return err
		}

		f.htlcID++

	case *lnwire.UpdateFulfillHTLC, *lnwire.UpdateFailHTLC:
		err := f.htlcSwitch.teardownCircuit(pkt)
		if err != nil {
			return err
		}
	}

	f.mailBox.AckPacket(pkt.inKey())

	return nil
}

func (f *mockChannelLink) deleteCircuit(pkt *htlcPacket) error {
	return f.htlcSwitch.deleteCircuits(pkt.inKey())
}

func newMockChannelLink(htlcSwitch *Switch, chanID lnwire.ChannelID,
	shortChanID lnwire.ShortChannelID, peer lnpeer.Peer, eligible bool,
) *mockChannelLink {

	return &mockChannelLink{
		htlcSwitch:  htlcSwitch,
		chanID:      chanID,
		shortChanID: shortChanID,
		peer:        peer,
		eligible:    eligible,
	}
}

func (f *mockChannelLink) HandleSwitchPacket(pkt *htlcPacket) error {
	f.mailBox.AddPacket(pkt)
	return nil
}

func (f *mockChannelLink) HandleChannelUpdate(lnwire.Message) {
}

func (f *mockChannelLink) UpdateForwardingPolicy(_ ForwardingPolicy) {
}
func (f *mockChannelLink) HtlcSatifiesPolicy([32]byte, lnwire.MilliSatoshi,
	lnwire.MilliSatoshi, uint32, uint32, uint32) lnwire.FailureMessage {
	return nil
}

func (f *mockChannelLink) HtlcSatifiesPolicyLocal(payHash [32]byte,
	amt lnwire.MilliSatoshi, timeout uint32,
	heightNow uint32) lnwire.FailureMessage {

	return f.htlcSatifiesPolicyLocalResult
}

func (f *mockChannelLink) Stats() (uint64, lnwire.MilliSatoshi, lnwire.MilliSatoshi) {
	return 0, 0, 0
}

func (f *mockChannelLink) AttachMailBox(mailBox MailBox) {
	f.mailBox = mailBox
	f.packets = mailBox.PacketOutBox()
}

func (f *mockChannelLink) Start() error {
	f.mailBox.ResetMessages()
	f.mailBox.ResetPackets()
	return nil
}

func (f *mockChannelLink) ChanID() lnwire.ChannelID                     { return f.chanID }
func (f *mockChannelLink) ShortChanID() lnwire.ShortChannelID           { return f.shortChanID }
func (f *mockChannelLink) Bandwidth() lnwire.MilliSatoshi               { return 99999999 }
func (f *mockChannelLink) Peer() lnpeer.Peer                            { return f.peer }
func (f *mockChannelLink) ChannelPoint() *wire.OutPoint                 { return &wire.OutPoint{} }
func (f *mockChannelLink) Stop()                                        {}
func (f *mockChannelLink) EligibleToForward() bool                      { return f.eligible }
func (f *mockChannelLink) setLiveShortChanID(sid lnwire.ShortChannelID) { f.shortChanID = sid }
func (f *mockChannelLink) UpdateShortChanID() (lnwire.ShortChannelID, error) {
	f.eligible = true
	return f.shortChanID, nil
}

var _ ChannelLink = (*mockChannelLink)(nil)

func newDB() (*channeldb.DB, func(), error) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		return nil, nil, err
	}

	// Next, create channeldb for the first time.
	cdb, err := channeldb.Open(tempDirName)
	if err != nil {
		os.RemoveAll(tempDirName)
		return nil, nil, err
	}

	cleanUp := func() {
		cdb.Close()
		os.RemoveAll(tempDirName)
	}

	return cdb, cleanUp, nil
}

const testInvoiceCltvExpiry = 6

type mockInvoiceRegistry struct {
	settleChan chan lntypes.Hash

	registry *invoices.InvoiceRegistry

	cleanup func()
}

func newMockRegistry(minDelta uint32) *mockInvoiceRegistry {
	cdb, cleanup, err := newDB()
	if err != nil {
		panic(err)
	}

	finalCltvRejectDelta := int32(5)

	registry := invoices.NewRegistry(cdb, finalCltvRejectDelta)
	registry.Start()

	return &mockInvoiceRegistry{
		registry: registry,
		cleanup:  cleanup,
	}
}

func (i *mockInvoiceRegistry) LookupInvoice(rHash lntypes.Hash) (
	channeldb.Invoice, error) {

	return i.registry.LookupInvoice(rHash)
}

func (i *mockInvoiceRegistry) SettleHodlInvoice(preimage lntypes.Preimage) error {
	return i.registry.SettleHodlInvoice(preimage)
}

func (i *mockInvoiceRegistry) NotifyExitHopHtlc(rhash lntypes.Hash,
	amt lnwire.MilliSatoshi, expiry uint32, currentHeight int32,
	circuitKey channeldb.CircuitKey, hodlChan chan<- interface{},
	eob []byte) (*invoices.HodlEvent, error) {

	event, err := i.registry.NotifyExitHopHtlc(
		rhash, amt, expiry, currentHeight, circuitKey, hodlChan, eob,
	)
	if err != nil {
		return nil, err
	}
	if i.settleChan != nil {
		i.settleChan <- rhash
	}

	return event, nil
}

func (i *mockInvoiceRegistry) CancelInvoice(payHash lntypes.Hash) error {
	return i.registry.CancelInvoice(payHash)
}

func (i *mockInvoiceRegistry) AddInvoice(invoice channeldb.Invoice,
	paymentHash lntypes.Hash) error {

	_, err := i.registry.AddInvoice(&invoice, paymentHash)
	return err
}

func (i *mockInvoiceRegistry) HodlUnsubscribeAll(subscriber chan<- interface{}) {
	i.registry.HodlUnsubscribeAll(subscriber)
}

var _ InvoiceDatabase = (*mockInvoiceRegistry)(nil)

type mockSigner struct {
	key *btcec.PrivateKey
}

func (m *mockSigner) SignOutputRaw(tx *wire.MsgTx, signDesc *input.SignDescriptor) ([]byte, error) {
	amt := signDesc.Output.Value
	witnessScript := signDesc.WitnessScript
	privKey := m.key

	if !privKey.PubKey().IsEqual(signDesc.KeyDesc.PubKey) {
		return nil, fmt.Errorf("incorrect key passed")
	}

	switch {
	case signDesc.SingleTweak != nil:
		privKey = input.TweakPrivKey(privKey,
			signDesc.SingleTweak)
	case signDesc.DoubleTweak != nil:
		privKey = input.DeriveRevocationPrivKey(privKey,
			signDesc.DoubleTweak)
	}

	sig, err := txscript.RawTxInWitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, amt, witnessScript, signDesc.HashType,
		privKey)
	if err != nil {
		return nil, err
	}

	return sig[:len(sig)-1], nil
}
func (m *mockSigner) ComputeInputScript(tx *wire.MsgTx, signDesc *input.SignDescriptor) (*input.Script, error) {

	// TODO(roasbeef): expose tweaked signer from lnwallet so don't need to
	// duplicate this code?

	privKey := m.key

	switch {
	case signDesc.SingleTweak != nil:
		privKey = input.TweakPrivKey(privKey,
			signDesc.SingleTweak)
	case signDesc.DoubleTweak != nil:
		privKey = input.DeriveRevocationPrivKey(privKey,
			signDesc.DoubleTweak)
	}

	witnessScript, err := txscript.WitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value, signDesc.Output.PkScript,
		signDesc.HashType, privKey, true)
	if err != nil {
		return nil, err
	}

	return &input.Script{
		Witness: witnessScript,
	}, nil
}

type mockNotifier struct {
	epochChan chan *chainntnfs.BlockEpoch
}

func (m *mockNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash, _ []byte,
	numConfs uint32, heightHint uint32) (*chainntnfs.ConfirmationEvent, error) {
	return nil, nil
}
func (m *mockNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {
	return &chainntnfs.BlockEpochEvent{
		Epochs: m.epochChan,
		Cancel: func() {},
	}, nil
}

func (m *mockNotifier) Start() error {
	return nil
}

func (m *mockNotifier) Stop() error {
	return nil
}

func (m *mockNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint, _ []byte,
	heightHint uint32) (*chainntnfs.SpendEvent, error) {

	return &chainntnfs.SpendEvent{
		Spend: make(chan *chainntnfs.SpendDetail),
	}, nil
}

type mockCircuitMap struct {
	lookup chan *PaymentCircuit
}

var _ CircuitMap = (*mockCircuitMap)(nil)

func (m *mockCircuitMap) OpenCircuits(...Keystone) error {
	return nil
}

func (m *mockCircuitMap) TrimOpenCircuits(chanID lnwire.ShortChannelID,
	start uint64) error {
	return nil
}

func (m *mockCircuitMap) DeleteCircuits(inKeys ...CircuitKey) error {
	return nil
}

func (m *mockCircuitMap) CommitCircuits(
	circuit ...*PaymentCircuit) (*CircuitFwdActions, error) {

	return nil, nil
}

func (m *mockCircuitMap) CloseCircuit(outKey CircuitKey) (*PaymentCircuit,
	error) {
	return nil, nil
}

func (m *mockCircuitMap) FailCircuit(inKey CircuitKey) (*PaymentCircuit,
	error) {
	return nil, nil
}

func (m *mockCircuitMap) LookupCircuit(inKey CircuitKey) *PaymentCircuit {
	return <-m.lookup
}

func (m *mockCircuitMap) LookupOpenCircuit(outKey CircuitKey) *PaymentCircuit {
	return nil
}

func (m *mockCircuitMap) LookupByPaymentHash(hash [32]byte) []*PaymentCircuit {
	return nil
}

func (m *mockCircuitMap) NumPending() int {
	return 0
}

func (m *mockCircuitMap) NumOpen() int {
	return 0
}

type mockOnionErrorDecryptor struct {
	sourceIdx int
	message   []byte
	err       error
}

func (m *mockOnionErrorDecryptor) DecryptError(encryptedData []byte) (
	*sphinx.DecryptedError, error) {

	return &sphinx.DecryptedError{
		SenderIdx: m.sourceIdx,
		Message:   m.message,
	}, m.err
}
