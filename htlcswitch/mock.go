package htlcswitch

import (
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"testing"

	"io"
	"sync/atomic"

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
	"github.com/roasbeef/btcutil"
)

type mockServer struct {
	sync.Mutex

	started  int32
	shutdown int32
	wg       sync.WaitGroup
	quit     chan bool

	t        *testing.T
	name     string
	messages chan lnwire.Message

	id         []byte
	htlcSwitch *Switch

	registry    *mockInvoiceRegistry
	recordFuncs []func(lnwire.Message)
}

var _ Peer = (*mockServer)(nil)

func newMockServer(t *testing.T, name string) *mockServer {
	return &mockServer{
		t:           t,
		id:          []byte(name),
		name:        name,
		messages:    make(chan lnwire.Message, 3000),
		quit:        make(chan bool),
		registry:    newMockRegistry(),
		htlcSwitch:  New(Config{}),
		recordFuncs: make([]func(lnwire.Message), 0),
	}
}

func (s *mockServer) Start() error {
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		return nil
	}

	s.htlcSwitch.Start()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		for {
			select {
			case msg := <-s.messages:
				for _, f := range s.recordFuncs {
					f(msg)
				}

				if err := s.readHandler(msg); err != nil {
					s.Lock()
					defer s.Unlock()
					s.t.Fatalf("%v server error: %v", s.name, err)
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
	hops []HopID
}

func newMockHopIterator(hops ...HopID) HopIterator {
	return &mockHopIterator{hops: hops}
}

func (r *mockHopIterator) Next() *HopID {
	if len(r.hops) != 0 {
		next := r.hops[0]
		r.hops = r.hops[1:]
		return &next
	}

	return nil
}

func (r *mockHopIterator) Encode(w io.Writer) error {
	var hopLength [4]byte
	binary.BigEndian.PutUint32(hopLength[:], uint32(len(r.hops)))

	if _, err := w.Write(hopLength[:]); err != nil {
		return err
	}

	for _, hop := range r.hops {
		if _, err := w.Write(hop[:]); err != nil {
			return err
		}
	}

	return nil
}

var _ HopIterator = (*mockHopIterator)(nil)

// mockIteratorDecoder test version of hop iterator decoder which decodes the
// encoded array of hops.
type mockIteratorDecoder struct{}

func (p *mockIteratorDecoder) Decode(r io.Reader, meta []byte) (
	HopIterator, error) {

	var b [4]byte
	_, err := r.Read(b[:])
	if err != nil {
		return nil, err
	}
	hopLength := binary.BigEndian.Uint32(b[:])

	hops := make([]HopID, hopLength)
	for i := uint32(0); i < hopLength; i++ {
		var hop HopID

		_, err := r.Read(hop[:])
		if err != nil {
			return nil, err
		}

		hops[i] = hop
	}

	return newMockHopIterator(hops...), nil
}

// messageInterceptor is function that handles the incoming peer messages and
// may decide should we handle it or not.
type messageInterceptor func(m lnwire.Message)

// Record is used to set the function which will be triggered when new
// lnwire message was received.
func (s *mockServer) record(f messageInterceptor) {
	s.recordFuncs = append(s.recordFuncs, f)
}

func (s *mockServer) SendMessage(message lnwire.Message) error {
	select {
	case s.messages <- message:
	case <-s.quit:
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
	case *lnwire.RevokeAndAck:
		targetChan = msg.ChanID
	case *lnwire.CommitSig:
		targetChan = msg.ChanID
	default:
		return errors.New("unknown message type")
	}

	// Dispatch the commitment update message to the proper
	// channel link dedicated to this channel.
	link, err := s.htlcSwitch.GetLink(targetChan)
	if err != nil {
		return err
	}

	// Create goroutine for this, in order to be able to properly stop
	// the server when handler stacked (server unavailable)
	done := make(chan struct{})
	go func() {
		defer func() {
			done <- struct{}{}
		}()

		link.HandleChannelUpdate(message)
	}()
	select {
	case <-done:
	case <-s.quit:
	}

	return nil
}

func (s *mockServer) ID() [sha256.Size]byte {
	return [sha256.Size]byte{}
}

func (s *mockServer) PubKey() []byte {
	return s.id
}

func (s *mockServer) Disconnect() {
	s.Stop()
	s.t.Fatalf("server %v was disconnected", s.name)
}

func (s *mockServer) WipeChannel(*lnwallet.LightningChannel) error {
	return nil
}

func (s *mockServer) Stop() {
	if !atomic.CompareAndSwapInt32(&s.shutdown, 0, 1) {
		return
	}

	go s.htlcSwitch.Stop()

	close(s.quit)
	s.wg.Wait()
}

func (s *mockServer) String() string {
	return string(s.id)
}

type mockChannelLink struct {
	chanID  lnwire.ChannelID
	peer    Peer
	packets chan *htlcPacket
}

func newMockChannelLink(chanID lnwire.ChannelID,
	peer Peer) *mockChannelLink {
	return &mockChannelLink{
		chanID:  chanID,
		packets: make(chan *htlcPacket, 1),
		peer:    peer,
	}
}

func (f *mockChannelLink) HandleSwitchPacket(packet *htlcPacket) {
	f.packets <- packet
}

func (f *mockChannelLink) HandleChannelUpdate(lnwire.Message) {
}

func (f *mockChannelLink) Stats() (uint64, btcutil.Amount, btcutil.Amount) {
	return 0, 0, 0
}

func (f *mockChannelLink) ChanID() lnwire.ChannelID  { return f.chanID }
func (f *mockChannelLink) Bandwidth() btcutil.Amount { return 99999999 }
func (f *mockChannelLink) Peer() Peer                { return f.peer }
func (f *mockChannelLink) Start() error              { return nil }
func (f *mockChannelLink) Stop()                     {}

var _ ChannelLink = (*mockChannelLink)(nil)

type mockInvoiceRegistry struct {
	sync.Mutex
	invoices map[chainhash.Hash]*channeldb.Invoice
}

func newMockRegistry() *mockInvoiceRegistry {
	return &mockInvoiceRegistry{
		invoices: make(map[chainhash.Hash]*channeldb.Invoice),
	}
}

func (i *mockInvoiceRegistry) LookupInvoice(rHash chainhash.Hash) (*channeldb.Invoice, error) {
	i.Lock()
	defer i.Unlock()

	invoice, ok := i.invoices[rHash]
	if !ok {
		return nil, errors.New("can't find mock invoice")
	}

	return invoice, nil
}

func (i *mockInvoiceRegistry) SettleInvoice(rhash chainhash.Hash) error {

	invoice, err := i.LookupInvoice(rhash)
	if err != nil {
		return err
	}

	i.Lock()
	invoice.Terms.Settled = true
	i.Unlock()

	return nil
}

func (i *mockInvoiceRegistry) AddInvoice(invoice *channeldb.Invoice) error {
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

	sig, err := txscript.RawTxInWitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, amt, witnessScript, txscript.SigHashAll, privKey)
	if err != nil {
		return nil, err
	}

	return sig[:len(sig)-1], nil
}
func (m *mockSigner) ComputeInputScript(tx *wire.MsgTx, signDesc *lnwallet.SignDescriptor) (*lnwallet.InputScript, error) {

	witnessScript, err := txscript.WitnessScript(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value, signDesc.Output.PkScript,
		txscript.SigHashAll, m.key, true)
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
