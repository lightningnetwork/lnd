package chainreg

import (
	"errors"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/chainntnfs"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/routing/chainview"
)

var (
	// defaultFee is the fee that is returned by NoChainBackend.
	defaultFee = chainfee.FeePerKwFloor

	// noChainBackendName is the backend name returned by NoChainBackend.
	noChainBackendName = "nochainbackend"

	// errNotImplemented is the error that is returned by NoChainBackend for
	// any operation that is not supported by it. Such paths should in
	// practice never been hit, so seeing this error either means a remote
	// signing instance was used for an unsupported purpose or a previously
	// forgotten edge case path was hit.
	errNotImplemented = errors.New("not implemented in nochainbackend " +
		"mode")

	// noChainBackendBestHash is the chain hash of the chain tip that is
	// returned by NoChainBackend.
	noChainBackendBestHash = &chainhash.Hash{0x01}

	// noChainBackendBestHeight is the best height that is returned by
	// NoChainBackend.
	noChainBackendBestHeight int32 = 1
)

// NoChainBackend is a mock implementation of the following interfaces:
//   - chainview.FilteredChainView
//   - chainntnfs.ChainNotifier
//   - chainfee.Estimator
type NoChainBackend struct {
}

func (n *NoChainBackend) EstimateFeePerKW(uint32) (chainfee.SatPerKWeight,
	error) {

	return defaultFee, nil
}

func (n *NoChainBackend) RelayFeePerKW() chainfee.SatPerKWeight {
	return defaultFee
}

func (n *NoChainBackend) RegisterConfirmationsNtfn(*chainhash.Hash, []byte,
	uint32, uint32,
	...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent, error) {

	return nil, errNotImplemented
}

func (n *NoChainBackend) RegisterSpendNtfn(*wire.OutPoint, []byte,
	uint32) (*chainntnfs.SpendEvent, error) {

	return nil, errNotImplemented
}

func (n *NoChainBackend) RegisterBlockEpochNtfn(
	*chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	epochChan := make(chan *chainntnfs.BlockEpoch)
	return &chainntnfs.BlockEpochEvent{
		Epochs: epochChan,
		Cancel: func() {
			close(epochChan)
		},
	}, nil
}

func (n *NoChainBackend) Started() bool {
	return true
}

func (n *NoChainBackend) FilteredBlocks() <-chan *chainview.FilteredBlock {
	return make(chan *chainview.FilteredBlock)
}

func (n *NoChainBackend) DisconnectedBlocks() <-chan *chainview.FilteredBlock {
	return make(chan *chainview.FilteredBlock)
}

func (n *NoChainBackend) UpdateFilter([]graphdb.EdgePoint, uint32) error {
	return nil
}

func (n *NoChainBackend) FilterBlock(*chainhash.Hash) (*chainview.FilteredBlock,
	error) {

	return nil, errNotImplemented
}

func (n *NoChainBackend) Start() error {
	return nil
}

func (n *NoChainBackend) Stop() error {
	return nil
}

var _ chainview.FilteredChainView = (*NoChainBackend)(nil)
var _ chainntnfs.ChainNotifier = (*NoChainBackend)(nil)
var _ chainfee.Estimator = (*NoChainBackend)(nil)

// NoChainSource is a mock implementation of chain.Interface.
// The mock is designed to return static values where necessary to make any
// caller believe the chain is fully synced to virtual block height 1 (hash
// 0x0000..0001). That should avoid calls to other methods completely since they
// are only used for advancing the chain forward.
type NoChainSource struct {
	notifChan chan interface{}

	BestBlockTime time.Time
}

func (n *NoChainSource) Start() error {
	n.notifChan = make(chan interface{})

	go func() {
		n.notifChan <- &chain.RescanFinished{
			Hash:   noChainBackendBestHash,
			Height: noChainBackendBestHeight,
			Time:   n.BestBlockTime,
		}
	}()

	return nil
}

func (n *NoChainSource) Stop() {
}

func (n *NoChainSource) WaitForShutdown() {
}

func (n *NoChainSource) GetBestBlock() (*chainhash.Hash, int32, error) {
	return noChainBackendBestHash, noChainBackendBestHeight, nil
}

func (n *NoChainSource) GetBlock(*chainhash.Hash) (*wire.MsgBlock, error) {
	return &wire.MsgBlock{
		Header: wire.BlockHeader{
			Timestamp: n.BestBlockTime,
		},
		Transactions: []*wire.MsgTx{},
	}, nil
}

func (n *NoChainSource) GetBlockHash(int64) (*chainhash.Hash, error) {
	return noChainBackendBestHash, nil
}

func (n *NoChainSource) GetBlockHeader(*chainhash.Hash) (*wire.BlockHeader,
	error) {

	return &wire.BlockHeader{
		Timestamp: n.BestBlockTime,
	}, nil
}

func (n *NoChainSource) IsCurrent() bool {
	return true
}

func (n *NoChainSource) FilterBlocks(
	*chain.FilterBlocksRequest) (*chain.FilterBlocksResponse, error) {

	return nil, errNotImplemented
}

func (n *NoChainSource) BlockStamp() (*waddrmgr.BlockStamp, error) {
	return nil, errNotImplemented
}

func (n *NoChainSource) SendRawTransaction(*wire.MsgTx, bool) (*chainhash.Hash,
	error) {

	return nil, errNotImplemented
}

func (n *NoChainSource) Rescan(*chainhash.Hash, []btcutil.Address,
	map[wire.OutPoint]btcutil.Address) error {

	return nil
}

func (n *NoChainSource) NotifyReceived([]btcutil.Address) error {
	return nil
}

func (n *NoChainSource) NotifyBlocks() error {
	return nil
}

func (n *NoChainSource) Notifications() <-chan interface{} {
	return n.notifChan
}

func (n *NoChainSource) BackEnd() string {
	return noChainBackendName
}

func (n *NoChainSource) TestMempoolAccept([]*wire.MsgTx,
	float64) ([]*btcjson.TestMempoolAcceptResult, error) {

	return nil, nil
}

func (n *NoChainSource) MapRPCErr(err error) error {
	return err
}

var _ chain.Interface = (*NoChainSource)(nil)
