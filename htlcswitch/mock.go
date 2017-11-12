package htlcswitch

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"

	"io"
	"sync/atomic"

	"bytes"

	"github.com/btcsuite/fastsha256"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
)

type mockServer struct {
	started  int32
	shutdown int32
	wg       sync.WaitGroup
	quit     chan struct{}

	t testing.TB

	name     string
	messages chan lnwire.Message

	errChan chan error

	id         [33]byte
	htlcSwitch *Switch

	registry         *mockInvoiceRegistry
	interceptorFuncs []messageInterceptor
}

var _ Peer = (*mockServer)(nil)

func newMockServer(t testing.TB, name string) *mockServer {
	var id [33]byte
	h := sha256.Sum256([]byte(name))
	copy(id[:], h[:])

	return &mockServer{
		t:                t,
		id:               id,
		name:             name,
		messages:         make(chan lnwire.Message, 3000),
		quit:             make(chan struct{}),
		registry:         newMockRegistry(),
		htlcSwitch:       New(Config{}),
		interceptorFuncs: make([]messageInterceptor, 0),
	}
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

// mockHopIterator represents the test version of hop iterator which instead
// of encrypting the path in onion blob just stores the path as a list of hops.
type mockHopIterator struct {
	hops []ForwardingInfo
}

func newMockHopIterator(hops ...ForwardingInfo) HopIterator {
	return &mockHopIterator{hops: hops}
}

func (r *mockHopIterator) ForwardingInstructions() ForwardingInfo {
	h := r.hops[0]
	r.hops = r.hops[1:]
	return h
}

func (r *mockHopIterator) EncodeNextHop(w io.Writer) error {
	var hopLength [4]byte
	binary.BigEndian.PutUint32(hopLength[:], uint32(len(r.hops)))

	if _, err := w.Write(hopLength[:]); err != nil {
		return err
	}

	for _, hop := range r.hops {
		if err := hop.encode(w); err != nil {
			return err
		}
	}

	return nil
}

func (f *ForwardingInfo) encode(w io.Writer) error {
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
type mockObfuscator struct{}

func newMockObfuscator() ErrorEncrypter {
	return &mockObfuscator{}
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
		FailureMessage: failure,
	}, nil
}

var _ ErrorDecrypter = (*mockDeobfuscator)(nil)

// mockIteratorDecoder test version of hop iterator decoder which decodes the
// encoded array of hops.
type mockIteratorDecoder struct{}

func (p *mockIteratorDecoder) DecodeHopIterator(r io.Reader, meta []byte) (
	HopIterator, lnwire.FailCode) {

	var b [4]byte
	_, err := r.Read(b[:])
	if err != nil {
		return nil, lnwire.CodeTemporaryChannelFailure
	}
	hopLength := binary.BigEndian.Uint32(b[:])

	hops := make([]ForwardingInfo, hopLength)
	for i := uint32(0); i < hopLength; i++ {
		f := &ForwardingInfo{}
		if err := f.decode(r); err != nil {
			return nil, lnwire.CodeTemporaryChannelFailure
		}

		hops[i] = *f
	}

	return newMockHopIterator(hops...), lnwire.CodeNone
}

func (f *ForwardingInfo) decode(r io.Reader) error {
	var net [1]byte
	if _, err := r.Read(net[:]); err != nil {
		return err
	}
	f.Network = NetworkHop(net[0])

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

func (s *mockServer) SendMessage(message lnwire.Message) error {

	select {
	case s.messages <- message:
	case <-s.quit:
		return errors.New("server is stopped")
	}

	return nil
}

func (s *mockServer) readHandler(message lnwire.Message) error {
	var targetChan lnwire.ChannelID

	switch msg := message.(type) {
	case *lnwire.UpdateAddHTLC:
		targetChan = msg.ChanID
	case *lnwire.UpdateFufillHTLC:
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
	default:
		return fmt.Errorf("unknown message type: %T", msg)
	}

	// Dispatch the commitment update message to the proper
	// channel link dedicated to this channel.
	link, err := s.htlcSwitch.GetLink(targetChan)
	if err != nil {
		return err
	}

	// Create goroutine for this, in order to be able to properly stop
	// the server when handler stacked (server unavailable)
	link.HandleChannelUpdate(message)

	return nil
}

func (s *mockServer) PubKey() [33]byte {
	return s.id
}

func (s *mockServer) Disconnect(reason error) {
	fmt.Printf("server %v disconnected due to %v\n", s.name, reason)

	s.t.Fatalf("server %v was disconnected: %v", s.name, reason)
}

func (s *mockServer) WipeChannel(*lnwallet.LightningChannel) error {
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
	shortChanID lnwire.ShortChannelID

	chanID lnwire.ChannelID

	peer Peer

	packets chan *htlcPacket
}

func newMockChannelLink(chanID lnwire.ChannelID, shortChanID lnwire.ShortChannelID,
	peer Peer) *mockChannelLink {

	return &mockChannelLink{
		chanID:      chanID,
		shortChanID: shortChanID,
		packets:     make(chan *htlcPacket, 1),
		peer:        peer,
	}
}

func (f *mockChannelLink) HandleSwitchPacket(packet *htlcPacket) {
	f.packets <- packet
}

func (f *mockChannelLink) HandleChannelUpdate(lnwire.Message) {
}

func (f *mockChannelLink) UpdateForwardingPolicy(_ ForwardingPolicy) {
}

func (f *mockChannelLink) Stats() (uint64, lnwire.MilliSatoshi, lnwire.MilliSatoshi) {
	return 0, 0, 0
}

func (f *mockChannelLink) ChanID() lnwire.ChannelID           { return f.chanID }
func (f *mockChannelLink) ShortChanID() lnwire.ShortChannelID { return f.shortChanID }
func (f *mockChannelLink) Bandwidth() lnwire.MilliSatoshi     { return 99999999 }
func (f *mockChannelLink) Peer() Peer                         { return f.peer }
func (f *mockChannelLink) Start() error                       { return nil }
func (f *mockChannelLink) Stop()                              {}

var _ ChannelLink = (*mockChannelLink)(nil)

type mockInvoiceRegistry struct {
	sync.Mutex
	invoices map[chainhash.Hash]channeldb.Invoice
}

func newMockRegistry() *mockInvoiceRegistry {
	return &mockInvoiceRegistry{
		invoices: make(map[chainhash.Hash]channeldb.Invoice),
	}
}

func (i *mockInvoiceRegistry) LookupInvoice(rHash chainhash.Hash) (channeldb.Invoice, error) {
	i.Lock()
	defer i.Unlock()

	invoice, ok := i.invoices[rHash]
	if !ok {
		return channeldb.Invoice{}, errors.New("can't find mock invoice")
	}

	return invoice, nil
}

func (i *mockInvoiceRegistry) SettleInvoice(rhash chainhash.Hash) error {
	i.Lock()
	defer i.Unlock()

	invoice, ok := i.invoices[rhash]
	if !ok {
		return errors.New("can't find mock invoice")
	}

	invoice.Terms.Settled = true
	i.invoices[rhash] = invoice

	return nil
}

func (i *mockInvoiceRegistry) AddInvoice(invoice channeldb.Invoice) error {
	i.Lock()
	defer i.Unlock()

	rhash := fastsha256.Sum256(invoice.Terms.PaymentPreimage[:])
	i.invoices[chainhash.Hash(rhash)] = invoice
	return nil
}

var _ InvoiceDatabase = (*mockInvoiceRegistry)(nil)

type mockSigner struct {
	key *btcec.PrivateKey
}

func (m *mockSigner) SignOutputRaw(tx *wire.MsgTx, signDesc *lnwallet.SignDescriptor) ([]byte, error) {
	amt := signDesc.Output.Value
	witnessScript := signDesc.WitnessScript
	privKey := m.key

	if !privKey.PubKey().IsEqual(signDesc.PubKey) {
		return nil, fmt.Errorf("incorrect key passed")
	}

	switch {
	case signDesc.SingleTweak != nil:
		privKey = lnwallet.TweakPrivKey(privKey,
			signDesc.SingleTweak)
	case signDesc.DoubleTweak != nil:
		privKey = lnwallet.DeriveRevocationPrivKey(privKey,
			signDesc.DoubleTweak)
	}

	sig, err := txscript.RawTxInWitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, amt, witnessScript, txscript.SigHashAll,
		privKey)
	if err != nil {
		return nil, err
	}

	return sig[:len(sig)-1], nil
}
func (m *mockSigner) ComputeInputScript(tx *wire.MsgTx, signDesc *lnwallet.SignDescriptor) (*lnwallet.InputScript, error) {

	// TODO(roasbeef): expose tweaked signer from lnwallet so don't need to
	// duplicate this code?

	privKey := m.key

	switch {
	case signDesc.SingleTweak != nil:
		privKey = lnwallet.TweakPrivKey(privKey,
			signDesc.SingleTweak)
	case signDesc.DoubleTweak != nil:
		privKey = lnwallet.DeriveRevocationPrivKey(privKey,
			signDesc.DoubleTweak)
	}

	witnessScript, err := txscript.WitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value, signDesc.Output.PkScript,
		txscript.SigHashAll, privKey, true)
	if err != nil {
		return nil, err
	}

	return &lnwallet.InputScript{
		Witness: witnessScript,
	}, nil
}

type mockNotifier struct {
}

func (m *mockNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash, numConfs uint32) (*chainntnfs.ConfirmationEvent, error) {
	return nil, nil
}
func (m *mockNotifier) RegisterBlockEpochNtfn() (*chainntnfs.BlockEpochEvent, error) {
	return nil, nil
}

func (m *mockNotifier) Start() error {
	return nil
}

func (m *mockNotifier) Stop() error {
	return nil
}
func (m *mockNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint) (*chainntnfs.SpendEvent, error) {
	return &chainntnfs.SpendEvent{
		Spend: make(chan *chainntnfs.SpendDetail),
	}, nil
}
