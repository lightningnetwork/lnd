package lnmock

import (
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/stretchr/testify/mock"
)

// MockChain is a mock implementation of the Chain interface.
type MockChain struct {
	mock.Mock
}

// Compile-time constraint to ensure MockChain implements the Chain interface.
var _ chain.Interface = (*MockChain)(nil)

func (m *MockChain) Start() error {
	args := m.Called()

	return args.Error(0)
}

func (m *MockChain) Stop() {
	m.Called()
}

func (m *MockChain) WaitForShutdown() {
	m.Called()
}

func (m *MockChain) GetBestBlock() (*chainhash.Hash, int32, error) {
	args := m.Called()

	if args.Get(0) == nil {
		return nil, args.Get(1).(int32), args.Error(2)
	}

	return args.Get(0).(*chainhash.Hash), args.Get(1).(int32), args.Error(2)
}

func (m *MockChain) GetBlock(hash *chainhash.Hash) (*wire.MsgBlock, error) {
	args := m.Called(hash)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*wire.MsgBlock), args.Error(1)
}

func (m *MockChain) GetBlockHash(height int64) (*chainhash.Hash, error) {
	args := m.Called(height)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*chainhash.Hash), args.Error(1)
}

func (m *MockChain) GetBlockHeader(hash *chainhash.Hash) (
	*wire.BlockHeader, error) {

	args := m.Called(hash)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*wire.BlockHeader), args.Error(1)
}

func (m *MockChain) GetUtxo(op *wire.OutPoint, pkScript []byte,
	heightHint uint32, cancel <-chan struct{}) (*wire.TxOut, error) {

	args := m.Called(op, pkScript, heightHint, cancel)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*wire.TxOut), args.Error(1)
}

func (m *MockChain) IsCurrent() bool {
	args := m.Called()

	return args.Bool(0)
}

func (m *MockChain) FilterBlocks(req *chain.FilterBlocksRequest) (
	*chain.FilterBlocksResponse, error) {

	args := m.Called(req)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*chain.FilterBlocksResponse), args.Error(1)
}

func (m *MockChain) BlockStamp() (*waddrmgr.BlockStamp, error) {
	args := m.Called()

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*waddrmgr.BlockStamp), args.Error(1)
}

func (m *MockChain) SendRawTransaction(tx *wire.MsgTx, allowHighFees bool) (
	*chainhash.Hash, error) {

	args := m.Called(tx, allowHighFees)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*chainhash.Hash), args.Error(1)
}

func (m *MockChain) Rescan(startHash *chainhash.Hash, addrs []btcutil.Address,
	outPoints map[wire.OutPoint]btcutil.Address) error {

	args := m.Called(startHash, addrs, outPoints)

	return args.Error(0)
}

func (m *MockChain) NotifyReceived(addrs []btcutil.Address) error {
	args := m.Called(addrs)

	return args.Error(0)
}

func (m *MockChain) NotifyBlocks() error {
	args := m.Called()

	return args.Error(0)
}

func (m *MockChain) Notifications() <-chan interface{} {
	args := m.Called()

	return args.Get(0).(<-chan interface{})
}

func (m *MockChain) BackEnd() string {
	args := m.Called()

	return args.String(0)
}

func (m *MockChain) TestMempoolAccept(txns []*wire.MsgTx, maxFeeRate float64) (
	[]*btcjson.TestMempoolAcceptResult, error) {

	args := m.Called(txns, maxFeeRate)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]*btcjson.TestMempoolAcceptResult), args.Error(1)
}

func (m *MockChain) MapRPCErr(err error) error {
	args := m.Called(err)

	return args.Error(0)
}
