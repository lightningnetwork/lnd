package htlcswitch

import (
	"crypto/sha256"
	"sync"
	"testing"

	"sync/atomic"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwire"
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

	recordFuncs []func(lnwire.Message)
}

var _ Peer = (*mockServer)(nil)

func newMockServer(t *testing.T, name string) *mockServer {
	return &mockServer{
		t:        t,
		id:       []byte(name),
		name:     name,
		messages: make(chan lnwire.Message, 3000),

		quit:        make(chan bool),
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
