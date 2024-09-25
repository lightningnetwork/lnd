package chancloser

import (
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/mock"

	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
)

type dummyAdapters struct {
	mock.Mock

	msgSent atomic.Bool

	confChan  chan *chainntnfs.TxConfirmation
	spendChan chan *chainntnfs.SpendDetail
}

func newDaemonAdapters() *dummyAdapters {
	return &dummyAdapters{
		confChan:  make(chan *chainntnfs.TxConfirmation, 1),
		spendChan: make(chan *chainntnfs.SpendDetail, 1),
	}
}

func (d *dummyAdapters) SendMessages(pub btcec.PublicKey,
	msgs []lnwire.Message) error {

	defer d.msgSent.Store(true)

	args := d.Called(pub, msgs)

	return args.Error(0)
}

func (d *dummyAdapters) BroadcastTransaction(tx *wire.MsgTx,
	label string) error {

	args := d.Called(tx, label)

	return args.Error(0)
}

func (d *dummyAdapters) DisableChannel(op wire.OutPoint) error {
	args := d.Called(op)

	return args.Error(0)
}

func (d *dummyAdapters) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption,
) (*chainntnfs.ConfirmationEvent, error) {

	args := d.Called(txid, pkScript, numConfs)

	err := args.Error(0)

	return &chainntnfs.ConfirmationEvent{
		Confirmed: d.confChan,
	}, err
}

func (d *dummyAdapters) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	args := d.Called(outpoint, pkScript, heightHint)

	err := args.Error(0)

	return &chainntnfs.SpendEvent{
		Spend: d.spendChan,
	}, err
}

type mockFeeEstimator struct {
	mock.Mock
}

func (m *mockFeeEstimator) EstimateFee(chanType channeldb.ChannelType,
	localTxOut, remoteTxOut *wire.TxOut,
	idealFeeRate chainfee.SatPerKWeight) btcutil.Amount {

	args := m.Called(chanType, localTxOut, remoteTxOut, idealFeeRate)
	return args.Get(0).(btcutil.Amount)
}

type mockChanObserver struct {
	mock.Mock
}

func (m *mockChanObserver) NoDanglingUpdates() bool {
	args := m.Called()
	return args.Bool(0)
}

func (m *mockChanObserver) DisableIncomingAdds() error {
	args := m.Called()
	return args.Error(0)
}

func (m *mockChanObserver) DisableOutgoingAdds() error {
	args := m.Called()
	return args.Error(0)
}

func (m *mockChanObserver) MarkCoopBroadcasted(txn *wire.MsgTx,
	local bool) error {

	args := m.Called(txn, local)
	return args.Error(0)
}

func (m *mockChanObserver) MarkShutdownSent(deliveryAddr []byte, isInitiator bool) error {
	args := m.Called(deliveryAddr, isInitiator)
	return args.Error(0)
}

func (m *mockChanObserver) FinalBalances() fn.Option[ShutdownBalances] {
	args := m.Called()
	return args.Get(0).(fn.Option[ShutdownBalances])
}

func (m *mockChanObserver) DisableChannel() error {
	args := m.Called()
	return args.Error(0)
}

type mockErrorReporter struct {
	mock.Mock
}

func (m *mockErrorReporter) ReportError(err error) {
	m.Called(err)
}

type mockCloseSigner struct {
	mock.Mock
}

func (m *mockCloseSigner) CreateCloseProposal(fee btcutil.Amount,
	localScript []byte, remoteScript []byte,
	closeOpt ...lnwallet.ChanCloseOpt) (
	input.Signature, *wire.MsgTx, btcutil.Amount, error) {

	args := m.Called(fee, localScript, remoteScript, closeOpt)

	return args.Get(0).(input.Signature), args.Get(1).(*wire.MsgTx),
		args.Get(2).(btcutil.Amount), args.Error(3)
}

func (m *mockCloseSigner) CompleteCooperativeClose(localSig,
	remoteSig input.Signature,
	localScript, remoteScript []byte,
	fee btcutil.Amount, closeOpt ...lnwallet.ChanCloseOpt,
) (*wire.MsgTx, btcutil.Amount, error) {

	args := m.Called(
		localSig, remoteSig, localScript, remoteScript, fee, closeOpt,
	)

	return args.Get(0).(*wire.MsgTx), args.Get(1).(btcutil.Amount),
		args.Error(2)
}
