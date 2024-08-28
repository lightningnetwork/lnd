package lnpeer

import (
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/mock"
)

// MockPeer implements the `lnpeer.Peer` interface.
type MockPeer struct {
	mock.Mock
}

// Compile time assertion that MockPeer implements lnpeer.Peer.
var _ Peer = (*MockPeer)(nil)

func (m *MockPeer) SendMessage(sync bool, msgs ...lnwire.Message) error {
	args := m.Called(sync, msgs)
	return args.Error(0)
}

func (m *MockPeer) SendMessageLazy(sync bool, msgs ...lnwire.Message) error {
	args := m.Called(sync, msgs)
	return args.Error(0)
}

func (m *MockPeer) AddNewChannel(channel *NewChannel,
	cancel <-chan struct{}) error {

	args := m.Called(channel, cancel)
	return args.Error(0)
}

func (m *MockPeer) AddPendingChannel(cid lnwire.ChannelID,
	cancel <-chan struct{}) error {

	args := m.Called(cid, cancel)
	return args.Error(0)
}

func (m *MockPeer) RemovePendingChannel(cid lnwire.ChannelID) error {
	args := m.Called(cid)
	return args.Error(0)
}

func (m *MockPeer) WipeChannel(op *wire.OutPoint) {
	m.Called(op)
}

func (m *MockPeer) PubKey() [33]byte {
	args := m.Called()
	return args.Get(0).([33]byte)
}

func (m *MockPeer) IdentityKey() *btcec.PublicKey {
	args := m.Called()
	return args.Get(0).(*btcec.PublicKey)
}

func (m *MockPeer) Address() net.Addr {
	args := m.Called()
	return args.Get(0).(net.Addr)
}

func (m *MockPeer) QuitSignal() <-chan struct{} {
	args := m.Called()
	return args.Get(0).(<-chan struct{})
}

func (m *MockPeer) LocalFeatures() *lnwire.FeatureVector {
	args := m.Called()
	return args.Get(0).(*lnwire.FeatureVector)
}

func (m *MockPeer) RemoteFeatures() *lnwire.FeatureVector {
	args := m.Called()
	return args.Get(0).(*lnwire.FeatureVector)
}

func (m *MockPeer) Disconnect(err error) {}
