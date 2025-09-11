package chainntnfs_test

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

var (
	testRawScript = []byte{
		// OP_HASH160
		0xa9,
		// OP_DATA_20
		0x14,
		// <20-byte script hash>
		0x90, 0x1c, 0x86, 0x94, 0xc0, 0x3f, 0xaf, 0xd5,
		0x52, 0x28, 0x10, 0xe0, 0x33, 0x0f, 0x26, 0xe6,
		0x7a, 0x85, 0x33, 0xcd,
		// OP_EQUAL
		0x87,
	}
	testSigScript = []byte{
		// OP_DATA_16
		0x16,
		// <22-byte redeem script>
		0x00, 0x14, 0x1d, 0x7c, 0xd6, 0xc7, 0x5c, 0x2e,
		0x86, 0xf4, 0xcb, 0xf9, 0x8e, 0xae, 0xd2, 0x21,
		0xb3, 0x0b, 0xd9, 0xa0, 0xb9, 0x28,
	}

	testWitness = [][]byte{{0x01}}
)

type mockHintCache struct {
	mu         sync.Mutex
	confHints  map[chainntnfs.ConfRequest]uint32
	spendHints map[chainntnfs.SpendRequest]uint32
}

var _ chainntnfs.SpendHintCache = (*mockHintCache)(nil)
var _ chainntnfs.ConfirmHintCache = (*mockHintCache)(nil)

func (c *mockHintCache) CommitSpendHint(heightHint uint32,
	spendRequests ...chainntnfs.SpendRequest) error {

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, spendRequest := range spendRequests {
		c.spendHints[spendRequest] = heightHint
	}

	return nil
}

func (c *mockHintCache) QuerySpendHint(spendRequest chainntnfs.SpendRequest) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	hint, ok := c.spendHints[spendRequest]
	if !ok {
		return 0, chainntnfs.ErrSpendHintNotFound
	}

	return hint, nil
}

func (c *mockHintCache) PurgeSpendHint(spendRequests ...chainntnfs.SpendRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, spendRequest := range spendRequests {
		delete(c.spendHints, spendRequest)
	}

	return nil
}

func (c *mockHintCache) CommitConfirmHint(heightHint uint32,
	confRequests ...chainntnfs.ConfRequest) error {

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, confRequest := range confRequests {
		c.confHints[confRequest] = heightHint
	}

	return nil
}

func (c *mockHintCache) QueryConfirmHint(confRequest chainntnfs.ConfRequest) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	hint, ok := c.confHints[confRequest]
	if !ok {
		return 0, chainntnfs.ErrConfirmHintNotFound
	}

	return hint, nil
}

func (c *mockHintCache) PurgeConfirmHint(confRequests ...chainntnfs.ConfRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, confRequest := range confRequests {
		delete(c.confHints, confRequest)
	}

	return nil
}

func newMockHintCache() *mockHintCache {
	return &mockHintCache{
		confHints:  make(map[chainntnfs.ConfRequest]uint32),
		spendHints: make(map[chainntnfs.SpendRequest]uint32),
	}
}

// TestTxNotifierRegistrationValidation ensures that we are not able to register
// requests with invalid parameters.
func TestTxNotifierRegistrationValidation(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		pkScript   []byte
		numConfs   uint32
		heightHint uint32
		checkSpend bool
		err        error
	}{
		{
			name:       "empty output script",
			pkScript:   nil,
			numConfs:   1,
			heightHint: 1,
			checkSpend: true,
			err:        chainntnfs.ErrNoScript,
		},
		{
			name:       "zero num confs",
			pkScript:   testRawScript,
			numConfs:   0,
			heightHint: 1,
			err:        chainntnfs.ErrNumConfsOutOfRange,
		},
		{
			name:       "exceed max num confs",
			pkScript:   testRawScript,
			numConfs:   chainntnfs.MaxNumConfs + 1,
			heightHint: 1,
			err:        chainntnfs.ErrNumConfsOutOfRange,
		},
		{
			name:       "empty height hint",
			pkScript:   testRawScript,
			numConfs:   1,
			heightHint: 0,
			checkSpend: true,
			err:        chainntnfs.ErrNoHeightHint,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		success := t.Run(testCase.name, func(t *testing.T) {
			hintCache := newMockHintCache()
			n := chainntnfs.NewTxNotifier(
				10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
			)

			_, err := n.RegisterConf(
				&chainntnfs.ZeroHash, testCase.pkScript,
				testCase.numConfs, testCase.heightHint,
			)
			if err != testCase.err {
				t.Fatalf("conf registration expected error "+
					"\"%v\", got \"%v\"", testCase.err, err)
			}

			if !testCase.checkSpend {
				return
			}

			_, err = n.RegisterSpend(
				&chainntnfs.ZeroOutPoint, testCase.pkScript,
				testCase.heightHint,
			)
			if err != testCase.err {
				t.Fatalf("spend registration expected error "+
					"\"%v\", got \"%v\"", testCase.err, err)
			}
		})

		if !success {
			return
		}
	}
}

// TestTxNotifierFutureConfDispatch tests that the TxNotifier dispatches
// registered notifications when a transaction confirms after registration.
func TestTxNotifierFutureConfDispatch(t *testing.T) {
	t.Parallel()

	const (
		tx1NumConfs uint32 = 1
		tx2NumConfs uint32 = 2
	)

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	// Create the test transactions and register them with the TxNotifier
	// before including them in a block to receive future
	// notifications.
	tx1 := wire.MsgTx{Version: 1}
	tx1.AddTxOut(&wire.TxOut{PkScript: testRawScript})
	tx1Hash := tx1.TxHash()
	ntfn1, err := n.RegisterConf(&tx1Hash, testRawScript, tx1NumConfs, 1)
	require.NoError(t, err, "unable to register ntfn")

	tx2 := wire.MsgTx{Version: 2}
	tx2.AddTxOut(&wire.TxOut{PkScript: testRawScript})
	tx2Hash := tx2.TxHash()
	ntfn2, err := n.RegisterConf(&tx2Hash, testRawScript, tx2NumConfs, 1)
	require.NoError(t, err, "unable to register ntfn")

	// We should not receive any notifications from both transactions
	// since they have not been included in a block yet.
	select {
	case <-ntfn1.Event.Updates:
		t.Fatal("Received unexpected confirmation update for tx1")
	case txConf := <-ntfn1.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx1: %v", txConf)
	default:
	}

	select {
	case <-ntfn2.Event.Updates:
		t.Fatal("Received unexpected confirmation update for tx2")
	case txConf := <-ntfn2.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx2: %v", txConf)
	default:
	}

	// Include the transactions in a block and add it to the TxNotifier.
	// This should confirm tx1, but not tx2.
	block1 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx1, &tx2},
	})

	err = n.ConnectTip(block1, 11)
	require.NoError(t, err, "Failed to connect block")
	if err := n.NotifyHeight(11); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// We should only receive one update for tx1 since it only requires
	// one confirmation and it already met it.
	select {
	case updDetails := <-ntfn1.Event.Updates:
		expected := chainntnfs.TxUpdateInfo{
			NumConfsLeft: 0,
			BlockHeight:  11,
		}
		require.Equal(t, expected, updDetails)
	default:
		t.Fatal("Expected confirmation update for tx1")
	}

	// A confirmation notification for this transaction should be dispatched,
	// as it only required one confirmation.
	select {
	case txConf := <-ntfn1.Event.Confirmed:
		expectedConf := chainntnfs.TxConfirmation{
			BlockHash:   block1.Hash(),
			BlockHeight: 11,
			TxIndex:     0,
			Tx:          &tx1,
		}
		assertConfDetails(t, txConf, &expectedConf)
	default:
		t.Fatalf("Expected confirmation for tx1")
	}

	// We should only receive one update for tx2 since it only has one
	// confirmation so far and it requires two.
	select {
	case updDetails := <-ntfn2.Event.Updates:
		expected := chainntnfs.TxUpdateInfo{
			NumConfsLeft: 1,
			BlockHeight:  11,
		}
		require.Equal(t, expected, updDetails)
	default:
		t.Fatal("Expected confirmation update for tx2")
	}

	// A confirmation notification for tx2 should not be dispatched yet, as
	// it requires one more confirmation.
	select {
	case txConf := <-ntfn2.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx2: %v", txConf)
	default:
	}

	// Create a new block and add it to the TxNotifier at the next height.
	// This should confirm tx2.
	block2 := btcutil.NewBlock(&wire.MsgBlock{})
	err = n.ConnectTip(block2, 12)
	require.NoError(t, err, "Failed to connect block")
	if err := n.NotifyHeight(12); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// We should not receive any event notifications for tx1 since it has
	// already been confirmed.
	select {
	case <-ntfn1.Event.Updates:
		t.Fatal("Received unexpected confirmation update for tx1")
	case txConf := <-ntfn1.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx1: %v", txConf)
	default:
	}

	// We should only receive one update since the last at the new height,
	// indicating how many confirmations are still left.
	select {
	case updDetails := <-ntfn2.Event.Updates:
		expected := chainntnfs.TxUpdateInfo{
			NumConfsLeft: 0,
			BlockHeight:  11,
		}
		require.Equal(t, expected, updDetails)
	default:
		t.Fatal("Expected confirmation update for tx2")
	}

	// A confirmation notification for tx2 should be dispatched, since it
	// now meets its required number of confirmations.
	select {
	case txConf := <-ntfn2.Event.Confirmed:
		expectedConf := chainntnfs.TxConfirmation{
			BlockHash:   block1.Hash(),
			BlockHeight: 11,
			TxIndex:     1,
			Tx:          &tx2,
		}
		assertConfDetails(t, txConf, &expectedConf)
	default:
		t.Fatalf("Expected confirmation for tx2")
	}
}

// TestTxNotifierHistoricalConfDispatch tests that the TxNotifier dispatches
// registered notifications when the transaction is confirmed before
// registration.
func TestTxNotifierHistoricalConfDispatch(t *testing.T) {
	t.Parallel()

	const (
		tx1NumConfs uint32 = 1
		tx2NumConfs uint32 = 3
	)

	var (
		tx1 = wire.MsgTx{Version: 1}
		tx2 = wire.MsgTx{Version: 2}
		tx3 = wire.MsgTx{Version: 3}
	)

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	// Create the test transactions at a height before the TxNotifier's
	// starting height so that they are confirmed once registering them.
	tx1Hash := tx1.TxHash()
	ntfn1, err := n.RegisterConf(&tx1Hash, testRawScript, tx1NumConfs, 1)
	require.NoError(t, err, "unable to register ntfn")

	tx2Hash := tx2.TxHash()
	ntfn2, err := n.RegisterConf(&tx2Hash, testRawScript, tx2NumConfs, 1)
	require.NoError(t, err, "unable to register ntfn")

	// Update tx1 with its confirmation details. We should only receive one
	// update since it only requires one confirmation and it already met it.
	txConf1 := chainntnfs.TxConfirmation{
		BlockHash:   &chainntnfs.ZeroHash,
		BlockHeight: 9,
		TxIndex:     1,
		Tx:          &tx1,
	}
	err = n.UpdateConfDetails(ntfn1.HistoricalDispatch.ConfRequest, &txConf1)
	require.NoError(t, err, "unable to update conf details")
	select {
	case updDetails := <-ntfn1.Event.Updates:
		expected := chainntnfs.TxUpdateInfo{
			NumConfsLeft: 0,
			BlockHeight:  9,
		}
		require.Equal(t, expected, updDetails)
	default:
		t.Fatal("Expected confirmation update for tx1")
	}

	// A confirmation notification for tx1 should also be dispatched.
	select {
	case txConf := <-ntfn1.Event.Confirmed:
		assertConfDetails(t, txConf, &txConf1)
	default:
		t.Fatalf("Expected confirmation for tx1")
	}

	// Update tx2 with its confirmation details. This should not trigger a
	// confirmation notification since it hasn't reached its required number
	// of confirmations, but we should receive a confirmation update
	// indicating how many confirmation are left.
	txConf2 := chainntnfs.TxConfirmation{
		BlockHash:   &chainntnfs.ZeroHash,
		BlockHeight: 9,
		TxIndex:     2,
		Tx:          &tx2,
	}
	err = n.UpdateConfDetails(ntfn2.HistoricalDispatch.ConfRequest, &txConf2)
	require.NoError(t, err, "unable to update conf details")
	select {
	case updDetails := <-ntfn2.Event.Updates:
		expected := chainntnfs.TxUpdateInfo{
			NumConfsLeft: 1,
			BlockHeight:  9,
		}
		require.Equal(t, expected, updDetails)
	default:
		t.Fatal("Expected confirmation update for tx2")
	}

	select {
	case txConf := <-ntfn2.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx2: %v", txConf)
	default:
	}

	// Create a new block and add it to the TxNotifier at the next height.
	// This should confirm tx2.
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx3},
	})

	err = n.ConnectTip(block, 11)
	require.NoError(t, err, "Failed to connect block")
	if err := n.NotifyHeight(11); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// We should not receive any event notifications for tx1 since it has
	// already been confirmed.
	select {
	case <-ntfn1.Event.Updates:
		t.Fatal("Received unexpected confirmation update for tx1")
	case txConf := <-ntfn1.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx1: %v", txConf)
	default:
	}

	// We should only receive one update for tx2 since the last one,
	// indicating how many confirmations are still left.
	select {
	case updDetails := <-ntfn2.Event.Updates:
		expected := chainntnfs.TxUpdateInfo{
			NumConfsLeft: 0,
			BlockHeight:  9,
		}
		require.Equal(t, expected, updDetails)
	default:
		t.Fatal("Expected confirmation update for tx2")
	}

	// A confirmation notification for tx2 should be dispatched, as it met
	// its required number of confirmations.
	select {
	case txConf := <-ntfn2.Event.Confirmed:
		assertConfDetails(t, txConf, &txConf2)
	default:
		t.Fatalf("Expected confirmation for tx2")
	}
}

// TestTxNotifierFutureSpendDispatch tests that the TxNotifier dispatches
// registered notifications when an outpoint is spent after registration.
func TestTxNotifierFutureSpendDispatch(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	// We'll start off by registering for a spend notification of an
	// outpoint.
	op := wire.OutPoint{Index: 1}
	ntfn, err := n.RegisterSpend(&op, testRawScript, 1)
	require.NoError(t, err, "unable to register spend ntfn")

	// We should not receive a notification as the outpoint has not been
	// spent yet.
	select {
	case <-ntfn.Event.Spend:
		t.Fatal("received unexpected spend notification")
	default:
	}

	// Construct the details of the spending transaction of the outpoint
	// above. We'll include it in the next block, which should trigger a
	// spend notification.
	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: op,
		SignatureScript:  testSigScript,
	})
	spendTxHash := spendTx.TxHash()
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx},
	})
	err = n.ConnectTip(block, 11)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(11); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	expectedSpendDetails := &chainntnfs.SpendDetail{
		SpentOutPoint:     &op,
		SpenderTxHash:     &spendTxHash,
		SpendingTx:        spendTx,
		SpenderInputIndex: 0,
		SpendingHeight:    11,
	}

	// Ensure that the details of the notification match as expected.
	select {
	case spendDetails := <-ntfn.Event.Spend:
		assertSpendDetails(t, spendDetails, expectedSpendDetails)
	default:
		t.Fatal("expected to receive spend details")
	}

	// Finally, we'll ensure that if the spending transaction has also been
	// spent, then we don't receive another spend notification.
	prevOut := wire.OutPoint{Hash: spendTxHash, Index: 0}
	spendOfSpend := wire.NewMsgTx(2)
	spendOfSpend.AddTxIn(&wire.TxIn{
		PreviousOutPoint: prevOut,
		SignatureScript:  testSigScript,
	})
	block = btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendOfSpend},
	})
	err = n.ConnectTip(block, 12)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(12); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	select {
	case <-ntfn.Event.Spend:
		t.Fatal("received unexpected spend notification")
	default:
	}
}

// TestTxNotifierFutureConfDispatchReuseSafe tests that the notifier does not
// misbehave even if two confirmation requests for the same script are issued
// at different block heights (which means funds are being sent to the same
// script multiple times).
func TestTxNotifierFutureConfDispatchReuseSafe(t *testing.T) {
	t.Parallel()

	currentBlock := uint32(10)
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		currentBlock, 2, hintCache, hintCache,
	)

	// We'll register a TX that sends to our test script and put it into a
	// block. Additionally we register a notification request for just the
	// script which should also be confirmed with that block.
	tx1 := wire.MsgTx{Version: 1}
	tx1.AddTxOut(&wire.TxOut{PkScript: testRawScript})
	tx1Hash := tx1.TxHash()
	ntfn1, err := n.RegisterConf(&tx1Hash, testRawScript, 1, 1)
	require.NoError(t, err, "unable to register ntfn")
	scriptNtfn1, err := n.RegisterConf(nil, testRawScript, 1, 1)
	require.NoError(t, err, "unable to register ntfn")
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx1},
	})
	currentBlock++
	err = n.ConnectTip(block, currentBlock)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(currentBlock); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// Expect an update and confirmation of TX 1 at this point. We save the
	// confirmation details because we expect to receive the same details
	// for all further registrations.
	var confDetails *chainntnfs.TxConfirmation
	select {
	case <-ntfn1.Event.Updates:
	default:
		t.Fatal("expected update of TX 1")
	}
	select {
	case confDetails = <-ntfn1.Event.Confirmed:
		if confDetails.BlockHeight != currentBlock {
			t.Fatalf("expected TX to be confirmed in latest block")
		}
	default:
		t.Fatal("expected confirmation of TX 1")
	}

	// The notification for the script should also have received a
	// confirmation.
	select {
	case <-scriptNtfn1.Event.Updates:
	default:
		t.Fatal("expected update of script ntfn")
	}
	select {
	case details := <-scriptNtfn1.Event.Confirmed:
		assertConfDetails(t, details, confDetails)
	default:
		t.Fatal("expected update of script ntfn")
	}

	// Now register a second TX that spends to two outputs with the same
	// script so we have a different TXID. And again register a confirmation
	// for just the script.
	tx2 := wire.MsgTx{Version: 1}
	tx2.AddTxOut(&wire.TxOut{PkScript: testRawScript})
	tx2.AddTxOut(&wire.TxOut{PkScript: testRawScript})
	tx2Hash := tx2.TxHash()
	ntfn2, err := n.RegisterConf(&tx2Hash, testRawScript, 1, 1)
	require.NoError(t, err, "unable to register ntfn")
	scriptNtfn2, err := n.RegisterConf(nil, testRawScript, 1, 1)
	require.NoError(t, err, "unable to register ntfn")
	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx2},
	})
	currentBlock++
	err = n.ConnectTip(block2, currentBlock)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(currentBlock); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// Transaction 2 should get a confirmation here too. Since it was
	// a different TXID we wouldn't get the cached details here but the TX
	// should be confirmed right away still.
	select {
	case <-ntfn2.Event.Updates:
	default:
		t.Fatal("expected update of TX 2")
	}
	select {
	case details := <-ntfn2.Event.Confirmed:
		if details.BlockHeight != currentBlock {
			t.Fatalf("expected TX to be confirmed in latest block")
		}
	default:
		t.Fatal("expected update of TX 2")
	}

	// The second notification for the script should also have received a
	// confirmation. Since it's the same script, we expect to get the cached
	// details from the first TX back immediately. Nothing should be
	// registered at the notifier for the current block height for that
	// script any more.
	select {
	case <-scriptNtfn2.Event.Updates:
	default:
		t.Fatal("expected update of script ntfn")
	}
	select {
	case details := <-scriptNtfn2.Event.Confirmed:
		assertConfDetails(t, details, confDetails)
	default:
		t.Fatal("expected update of script ntfn")
	}

	// Finally, mine a few empty blocks and expect both TXs to be confirmed.
	for currentBlock < 15 {
		block := btcutil.NewBlock(&wire.MsgBlock{})
		currentBlock++
		err = n.ConnectTip(block, currentBlock)
		if err != nil {
			t.Fatalf("unable to connect block: %v", err)
		}
		if err := n.NotifyHeight(currentBlock); err != nil {
			t.Fatalf("unable to dispatch notifications: %v", err)
		}
	}

	// Events for both confirmation requests should have been dispatched.
	select {
	case <-ntfn1.Event.Done:
	default:
		t.Fatal("expected notifications for TX 1 to be done")
	}
	select {
	case <-ntfn2.Event.Done:
	default:
		t.Fatal("expected notifications for TX 2 to be done")
	}
}

// TestTxNotifierHistoricalSpendDispatch tests that the TxNotifier dispatches
// registered notifications when an outpoint is spent before registration.
func TestTxNotifierHistoricalSpendDispatch(t *testing.T) {
	t.Parallel()

	const startingHeight = 10

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startingHeight, chainntnfs.ReorgSafetyLimit, hintCache,
		hintCache,
	)

	// We'll start by constructing the spending details of the outpoint
	// below.
	spentOutpoint := wire.OutPoint{Index: 1}
	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: spentOutpoint,
		Witness:          testWitness,
		SignatureScript:  testSigScript,
	})
	spendTxHash := spendTx.TxHash()

	expectedSpendDetails := &chainntnfs.SpendDetail{
		SpentOutPoint:     &spentOutpoint,
		SpenderTxHash:     &spendTxHash,
		SpendingTx:        spendTx,
		SpenderInputIndex: 0,
		SpendingHeight:    startingHeight - 1,
	}

	// We'll register for a spend notification of the outpoint and ensure
	// that a notification isn't dispatched.
	ntfn, err := n.RegisterSpend(&spentOutpoint, testRawScript, 1)
	require.NoError(t, err, "unable to register spend ntfn")

	select {
	case <-ntfn.Event.Spend:
		t.Fatal("received unexpected spend notification")
	default:
	}

	// Because we're interested in testing the case of a historical spend,
	// we'll hand off the spending details of the outpoint to the notifier
	// as it is not possible for it to view historical events in the chain.
	// By doing this, we replicate the functionality of the ChainNotifier.
	err = n.UpdateSpendDetails(
		ntfn.HistoricalDispatch.SpendRequest, expectedSpendDetails,
	)
	require.NoError(t, err, "unable to update spend details")

	// Now that we have the spending details, we should receive a spend
	// notification. We'll ensure that the details match as intended.
	select {
	case spendDetails := <-ntfn.Event.Spend:
		assertSpendDetails(t, spendDetails, expectedSpendDetails)
	default:
		t.Fatalf("expected to receive spend details")
	}

	// Finally, we'll ensure that if the spending transaction has also been
	// spent, then we don't receive another spend notification.
	prevOut := wire.OutPoint{Hash: spendTxHash, Index: 0}
	spendOfSpend := wire.NewMsgTx(2)
	spendOfSpend.AddTxIn(&wire.TxIn{
		PreviousOutPoint: prevOut,
		SignatureScript:  testSigScript,
	})
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendOfSpend},
	})
	err = n.ConnectTip(block, startingHeight+1)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(startingHeight + 1); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	select {
	case <-ntfn.Event.Spend:
		t.Fatal("received unexpected spend notification")
	default:
	}
}

// TestTxNotifierMultipleHistoricalConfRescans ensures that we don't attempt to
// request multiple historical confirmation rescans per transactions.
func TestTxNotifierMultipleHistoricalConfRescans(t *testing.T) {
	t.Parallel()

	const startingHeight = 10
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startingHeight, chainntnfs.ReorgSafetyLimit, hintCache,
		hintCache,
	)

	// The first registration for a transaction in the notifier should
	// request a historical confirmation rescan as it does not have a
	// historical view of the chain.
	ntfn1, err := n.RegisterConf(&chainntnfs.ZeroHash, testRawScript, 1, 1)
	require.NoError(t, err, "unable to register spend ntfn")
	if ntfn1.HistoricalDispatch == nil {
		t.Fatal("expected to receive historical dispatch request")
	}

	// We'll register another confirmation notification for the same
	// transaction. This should not request a historical confirmation rescan
	// since the first one is still pending.
	ntfn2, err := n.RegisterConf(&chainntnfs.ZeroHash, testRawScript, 1, 1)
	require.NoError(t, err, "unable to register spend ntfn")
	if ntfn2.HistoricalDispatch != nil {
		t.Fatal("received unexpected historical rescan request")
	}

	// Finally, we'll mark the ongoing historical rescan as complete and
	// register another notification. We should also expect not to see a
	// historical rescan request since the confirmation details should be
	// cached.
	confDetails := &chainntnfs.TxConfirmation{
		BlockHeight: startingHeight - 1,
	}
	err = n.UpdateConfDetails(ntfn1.HistoricalDispatch.ConfRequest, confDetails)
	require.NoError(t, err, "unable to update conf details")

	ntfn3, err := n.RegisterConf(&chainntnfs.ZeroHash, testRawScript, 1, 1)
	require.NoError(t, err, "unable to register spend ntfn")
	if ntfn3.HistoricalDispatch != nil {
		t.Fatal("received unexpected historical rescan request")
	}
}

// TestTxNotifierMultipleHistoricalSpendRescans ensures that we don't attempt to
// request multiple historical spend rescans per outpoints.
func TestTxNotifierMultipleHistoricalSpendRescans(t *testing.T) {
	t.Parallel()

	const startingHeight = 10
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startingHeight, chainntnfs.ReorgSafetyLimit, hintCache,
		hintCache,
	)

	// The first registration for an outpoint in the notifier should request
	// a historical spend rescan as it does not have a historical view of
	// the chain.
	op := wire.OutPoint{Index: 1}
	ntfn1, err := n.RegisterSpend(&op, testRawScript, 1)
	require.NoError(t, err, "unable to register spend ntfn")
	if ntfn1.HistoricalDispatch == nil {
		t.Fatal("expected to receive historical dispatch request")
	}

	// We'll register another spend notification for the same outpoint. This
	// should not request a historical spend rescan since the first one is
	// still pending.
	ntfn2, err := n.RegisterSpend(&op, testRawScript, 1)
	require.NoError(t, err, "unable to register spend ntfn")
	if ntfn2.HistoricalDispatch != nil {
		t.Fatal("received unexpected historical rescan request")
	}

	// Finally, we'll mark the ongoing historical rescan as complete and
	// register another notification. We should also expect not to see a
	// historical rescan request since the confirmation details should be
	// cached.
	msgTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op, Witness: testWitness},
		},
		TxOut: []*wire.TxOut{},
	}

	spendDetails := &chainntnfs.SpendDetail{
		SpentOutPoint:     &op,
		SpenderTxHash:     &chainntnfs.ZeroHash,
		SpendingTx:        msgTx,
		SpenderInputIndex: 0,
		SpendingHeight:    startingHeight - 1,
	}
	err = n.UpdateSpendDetails(
		ntfn1.HistoricalDispatch.SpendRequest, spendDetails,
	)
	require.NoError(t, err, "unable to update spend details")

	ntfn3, err := n.RegisterSpend(&op, testRawScript, 1)
	require.NoError(t, err, "unable to register spend ntfn")
	if ntfn3.HistoricalDispatch != nil {
		t.Fatal("received unexpected historical rescan request")
	}
}

// TestTxNotifierMultipleHistoricalNtfns ensures that the TxNotifier will only
// request one rescan for a transaction/outpoint when having multiple client
// registrations. Once the rescan has completed and retrieved the
// confirmation/spend details, a notification should be dispatched to _all_
// clients.
func TestTxNotifierMultipleHistoricalNtfns(t *testing.T) {
	t.Parallel()

	const (
		numNtfns       = 5
		startingHeight = 10
	)

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startingHeight, chainntnfs.ReorgSafetyLimit, hintCache,
		hintCache,
	)

	var txid chainhash.Hash
	copy(txid[:], bytes.Repeat([]byte{0x01}, 32))

	// We'll start off by registered 5 clients for a confirmation
	// notification on the same transaction.
	confNtfns := make([]*chainntnfs.ConfRegistration, numNtfns)
	for i := uint64(0); i < numNtfns; i++ {
		ntfn, err := n.RegisterConf(&txid, testRawScript, 1, 1)
		if err != nil {
			t.Fatalf("unable to register conf ntfn #%d: %v", i, err)
		}
		confNtfns[i] = ntfn
	}

	// Ensure none of them have received the confirmation details.
	for i, ntfn := range confNtfns {
		select {
		case <-ntfn.Event.Confirmed:
			t.Fatalf("request #%d received unexpected confirmation "+
				"notification", i)
		default:
		}
	}

	// We'll assume a historical rescan was dispatched and found the
	// following confirmation details. We'll let the notifier know so that
	// it can stop watching at tip.
	expectedConfDetails := &chainntnfs.TxConfirmation{
		BlockHeight: startingHeight - 1,
		Tx:          wire.NewMsgTx(1),
	}
	err := n.UpdateConfDetails(
		confNtfns[0].HistoricalDispatch.ConfRequest, expectedConfDetails,
	)
	require.NoError(t, err, "unable to update conf details")

	// With the confirmation details retrieved, each client should now have
	// been notified of the confirmation.
	for i, ntfn := range confNtfns {
		select {
		case confDetails := <-ntfn.Event.Confirmed:
			assertConfDetails(t, confDetails, expectedConfDetails)
		default:
			t.Fatalf("request #%d expected to received "+
				"confirmation notification", i)
		}
	}

	// In order to ensure that the confirmation details are properly cached,
	// we'll register another client for the same transaction. We should not
	// see a historical rescan request and the confirmation notification
	// should come through immediately.
	extraConfNtfn, err := n.RegisterConf(&txid, testRawScript, 1, 1)
	require.NoError(t, err, "unable to register conf ntfn")
	if extraConfNtfn.HistoricalDispatch != nil {
		t.Fatal("received unexpected historical rescan request")
	}

	select {
	case confDetails := <-extraConfNtfn.Event.Confirmed:
		assertConfDetails(t, confDetails, expectedConfDetails)
	default:
		t.Fatal("expected to receive spend notification")
	}

	// Similarly, we'll do the same thing but for spend notifications.
	op := wire.OutPoint{Index: 1}
	spendNtfns := make([]*chainntnfs.SpendRegistration, numNtfns)
	for i := uint64(0); i < numNtfns; i++ {
		ntfn, err := n.RegisterSpend(&op, testRawScript, 1)
		if err != nil {
			t.Fatalf("unable to register spend ntfn #%d: %v", i, err)
		}
		spendNtfns[i] = ntfn
	}

	// Ensure none of them have received the spend details.
	for i, ntfn := range spendNtfns {
		select {
		case <-ntfn.Event.Spend:
			t.Fatalf("request #%d received unexpected spend "+
				"notification", i)
		default:
		}
	}

	// We'll assume a historical rescan was dispatched and found the
	// following spend details. We'll let the notifier know so that it can
	// stop watching at tip.
	msgTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op, Witness: testWitness},
		},
		TxOut: []*wire.TxOut{},
	}

	expectedSpendDetails := &chainntnfs.SpendDetail{
		SpentOutPoint:     &op,
		SpenderTxHash:     &chainntnfs.ZeroHash,
		SpendingTx:        msgTx,
		SpenderInputIndex: 0,
		SpendingHeight:    startingHeight - 1,
	}
	err = n.UpdateSpendDetails(
		spendNtfns[0].HistoricalDispatch.SpendRequest, expectedSpendDetails,
	)
	require.NoError(t, err, "unable to update spend details")

	// With the spend details retrieved, each client should now have been
	// notified of the spend.
	for i, ntfn := range spendNtfns {
		select {
		case spendDetails := <-ntfn.Event.Spend:
			assertSpendDetails(t, spendDetails, expectedSpendDetails)
		default:
			t.Fatalf("request #%d expected to received spend "+
				"notification", i)
		}
	}

	// Finally, in order to ensure that the spend details are properly
	// cached, we'll register another client for the same outpoint. We
	// should not see a historical rescan request and the spend notification
	// should come through immediately.
	extraSpendNtfn, err := n.RegisterSpend(&op, testRawScript, 1)
	require.NoError(t, err, "unable to register spend ntfn")
	if extraSpendNtfn.HistoricalDispatch != nil {
		t.Fatal("received unexpected historical rescan request")
	}

	select {
	case spendDetails := <-extraSpendNtfn.Event.Spend:
		assertSpendDetails(t, spendDetails, expectedSpendDetails)
	default:
		t.Fatal("expected to receive spend notification")
	}
}

// TestTxNotifierCancelConf ensures that a confirmation notification after a
// client has canceled their intent to receive one.
func TestTxNotifierCancelConf(t *testing.T) {
	t.Parallel()

	const startingHeight = 10
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(startingHeight, 100, hintCache, hintCache)

	// We'll register four notification requests. The last three will be
	// canceled.
	tx1 := wire.NewMsgTx(1)
	tx1.AddTxOut(&wire.TxOut{PkScript: testRawScript})
	tx1Hash := tx1.TxHash()
	ntfn1, err := n.RegisterConf(&tx1Hash, testRawScript, 1, 1)
	require.NoError(t, err, "unable to register spend ntfn")

	tx2 := wire.NewMsgTx(2)
	tx2.AddTxOut(&wire.TxOut{PkScript: testRawScript})
	tx2Hash := tx2.TxHash()
	ntfn2, err := n.RegisterConf(&tx2Hash, testRawScript, 1, 1)
	require.NoError(t, err, "unable to register spend ntfn")
	ntfn3, err := n.RegisterConf(&tx2Hash, testRawScript, 1, 1)
	require.NoError(t, err, "unable to register spend ntfn")

	// This request will have a three block num confs.
	ntfn4, err := n.RegisterConf(&tx2Hash, testRawScript, 3, 1)
	require.NoError(t, err, "unable to register spend ntfn")

	// Extend the chain with a block that will confirm both transactions.
	// This will queue confirmation notifications to dispatch once their
	// respective heights have been met.
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx1, tx2},
	})
	tx1ConfDetails := &chainntnfs.TxConfirmation{
		BlockHeight: startingHeight + 1,
		BlockHash:   block.Hash(),
		TxIndex:     0,
		Tx:          tx1,
	}

	// Cancel the second notification before connecting the block.
	ntfn2.Event.Cancel()

	err = n.ConnectTip(block, startingHeight+1)
	require.NoError(t, err, "unable to connect block")

	// Cancel the third notification before notifying to ensure its queued
	// confirmation notification gets removed as well.
	ntfn3.Event.Cancel()

	if err := n.NotifyHeight(startingHeight + 1); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// The first request should still be active, so we should receive a
	// confirmation notification with the correct details.
	select {
	case confDetails := <-ntfn1.Event.Confirmed:
		assertConfDetails(t, confDetails, tx1ConfDetails)
	default:
		t.Fatalf("expected to receive confirmation notification")
	}

	// The second and third, however, should not have. The event's Confirmed
	// channel must have also been closed to indicate the caller that the
	// TxNotifier can no longer fulfill their canceled request.
	select {
	case _, ok := <-ntfn2.Event.Confirmed:
		if ok {
			t.Fatal("expected Confirmed channel to be closed")
		}
	default:
		t.Fatal("expected Confirmed channel to be closed")
	}
	select {
	case _, ok := <-ntfn3.Event.Confirmed:
		if ok {
			t.Fatal("expected Confirmed channel to be closed")
		}
	default:
		t.Fatal("expected Confirmed channel to be closed")
	}

	// Connect yet another block.
	block1 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{},
	})

	err = n.ConnectTip(block1, startingHeight+2)
	require.NoError(t, err, "unable to connect block")

	if err := n.NotifyHeight(startingHeight + 2); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// Since neither it reached the set confirmation height or was
	// canceled, nothing should happen to ntfn4 in this block.
	select {
	case <-ntfn4.Event.Confirmed:
		t.Fatal("expected nothing to happen")
	case <-time.After(10 * time.Millisecond):
	}

	// Now cancel the notification.
	ntfn4.Event.Cancel()
	select {
	case _, ok := <-ntfn4.Event.Confirmed:
		if ok {
			t.Fatal("expected Confirmed channel to be closed")
		}
	default:
		t.Fatal("expected Confirmed channel to be closed")
	}

	// Finally, confirm a block that would trigger ntfn4 confirmation
	// hadn't it already been canceled.
	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{},
	})

	err = n.ConnectTip(block2, startingHeight+3)
	require.NoError(t, err, "unable to connect block")

	if err := n.NotifyHeight(startingHeight + 3); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}
}

// TestTxNotifierCancelSpend ensures that a spend notification after a client
// has canceled their intent to receive one.
func TestTxNotifierCancelSpend(t *testing.T) {
	t.Parallel()

	const startingHeight = 10
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startingHeight, chainntnfs.ReorgSafetyLimit, hintCache,
		hintCache,
	)

	// We'll register two notification requests. Only the second one will be
	// canceled.
	op1 := wire.OutPoint{Index: 1}
	ntfn1, err := n.RegisterSpend(&op1, testRawScript, 1)
	require.NoError(t, err, "unable to register spend ntfn")

	op2 := wire.OutPoint{Index: 2}
	ntfn2, err := n.RegisterSpend(&op2, testRawScript, 1)
	require.NoError(t, err, "unable to register spend ntfn")

	// Construct the spending details of the outpoint and create a dummy
	// block containing it.
	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: op1,
		SignatureScript:  testSigScript,
	})
	spendTxHash := spendTx.TxHash()
	expectedSpendDetails := &chainntnfs.SpendDetail{
		SpentOutPoint:     &op1,
		SpenderTxHash:     &spendTxHash,
		SpendingTx:        spendTx,
		SpenderInputIndex: 0,
		SpendingHeight:    startingHeight + 1,
	}

	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx},
	})

	// Before extending the notifier's tip with the dummy block above, we'll
	// cancel the second request.
	n.CancelSpend(ntfn2.HistoricalDispatch.SpendRequest, 2)

	err = n.ConnectTip(block, startingHeight+1)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(startingHeight + 1); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// The first request should still be active, so we should receive a
	// spend notification with the correct spending details.
	select {
	case spendDetails := <-ntfn1.Event.Spend:
		assertSpendDetails(t, spendDetails, expectedSpendDetails)
	default:
		t.Fatalf("expected to receive spend notification")
	}

	// The second one, however, should not have. The event's Spend channel
	// must have also been closed to indicate the caller that the TxNotifier
	// can no longer fulfill their canceled request.
	select {
	case _, ok := <-ntfn2.Event.Spend:
		if ok {
			t.Fatal("expected Spend channel to be closed")
		}
	default:
		t.Fatal("expected Spend channel to be closed")
	}
}

// TestTxNotifierConfReorg ensures that clients are notified of a reorg when a
// transaction for which they registered a confirmation notification has been
// reorged out of the chain.
func TestTxNotifierConfReorg(t *testing.T) {
	t.Parallel()

	const (
		tx1NumConfs uint32 = 2
		tx2NumConfs uint32 = 1
		tx3NumConfs uint32 = 2
	)

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		7, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	// Tx 1 will be confirmed in block 9 and requires 2 confs.
	tx1 := wire.MsgTx{Version: 1}
	tx1.AddTxOut(&wire.TxOut{PkScript: testRawScript})
	tx1Hash := tx1.TxHash()
	ntfn1, err := n.RegisterConf(&tx1Hash, testRawScript, tx1NumConfs, 1)
	require.NoError(t, err, "unable to register ntfn")

	err = n.UpdateConfDetails(ntfn1.HistoricalDispatch.ConfRequest, nil)
	require.NoError(t, err, "unable to deliver conf details")

	// Tx 2 will be confirmed in block 10 and requires 1 conf.
	tx2 := wire.MsgTx{Version: 2}
	tx2.AddTxOut(&wire.TxOut{PkScript: testRawScript})
	tx2Hash := tx2.TxHash()
	ntfn2, err := n.RegisterConf(&tx2Hash, testRawScript, tx2NumConfs, 1)
	require.NoError(t, err, "unable to register ntfn")

	err = n.UpdateConfDetails(ntfn2.HistoricalDispatch.ConfRequest, nil)
	require.NoError(t, err, "unable to deliver conf details")

	// Tx 3 will be confirmed in block 10 and requires 2 confs.
	tx3 := wire.MsgTx{Version: 3}
	tx3.AddTxOut(&wire.TxOut{PkScript: testRawScript})
	tx3Hash := tx3.TxHash()
	ntfn3, err := n.RegisterConf(&tx3Hash, testRawScript, tx3NumConfs, 1)
	require.NoError(t, err, "unable to register ntfn")

	err = n.UpdateConfDetails(ntfn3.HistoricalDispatch.ConfRequest, nil)
	require.NoError(t, err, "unable to deliver conf details")

	// Sync chain to block 10. Txs 1 & 2 should be confirmed.
	block1 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx1},
	})
	if err := n.ConnectTip(block1, 8); err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(8); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}
	if err := n.ConnectTip(nil, 9); err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(9); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx2, &tx3},
	})
	if err := n.ConnectTip(block2, 10); err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(10); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// We should receive two updates for tx1 since it requires two
	// confirmations and it has already met them.
	for i := 0; i < 2; i++ {
		select {
		case <-ntfn1.Event.Updates:
		default:
			t.Fatal("Expected confirmation update for tx1")
		}
	}

	// A confirmation notification for tx1 should be dispatched, as it met
	// its required number of confirmations.
	select {
	case <-ntfn1.Event.Confirmed:
	default:
		t.Fatalf("Expected confirmation for tx1")
	}

	// We should only receive one update for tx2 since it only requires
	// one confirmation and it already met it.
	select {
	case <-ntfn2.Event.Updates:
	default:
		t.Fatal("Expected confirmation update for tx2")
	}

	// A confirmation notification for tx2 should be dispatched, as it met
	// its required number of confirmations.
	select {
	case <-ntfn2.Event.Confirmed:
	default:
		t.Fatalf("Expected confirmation for tx2")
	}

	// We should only receive one update for tx3 since it only has one
	// confirmation so far and it requires two.
	select {
	case <-ntfn3.Event.Updates:
	default:
		t.Fatal("Expected confirmation update for tx3")
	}

	// A confirmation notification for tx3 should not be dispatched yet, as
	// it requires one more confirmation.
	select {
	case txConf := <-ntfn3.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx3: %v", txConf)
	default:
	}

	// The block that included tx2 and tx3 is disconnected and two next
	// blocks without them are connected.
	if err := n.DisconnectTip(10); err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	if err := n.ConnectTip(nil, 10); err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(10); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	if err := n.ConnectTip(nil, 11); err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(11); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	select {
	case reorgDepth := <-ntfn2.Event.NegativeConf:
		if reorgDepth != 1 {
			t.Fatalf("Incorrect value for negative conf notification: "+
				"expected %d, got %d", 1, reorgDepth)
		}
	default:
		t.Fatalf("Expected negative conf notification for tx1")
	}

	// We should not receive any event notifications from all of the
	// transactions because tx1 has already been confirmed and tx2 and tx3
	// have not been included in the chain since the reorg.
	select {
	case <-ntfn1.Event.Updates:
		t.Fatal("Received unexpected confirmation update for tx1")
	case txConf := <-ntfn1.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx1: %v", txConf)
	default:
	}

	select {
	case <-ntfn2.Event.Updates:
		t.Fatal("Received unexpected confirmation update for tx2")
	case txConf := <-ntfn2.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx2: %v", txConf)
	default:
	}

	select {
	case <-ntfn3.Event.Updates:
		t.Fatal("Received unexpected confirmation update for tx3")
	case txConf := <-ntfn3.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx3: %v", txConf)
	default:
	}

	// Now transactions 2 & 3 are re-included in a new block.
	block3 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx2, &tx3},
	})
	block4 := btcutil.NewBlock(&wire.MsgBlock{})

	err = n.ConnectTip(block3, 12)
	require.NoError(t, err, "Failed to connect block")
	if err := n.NotifyHeight(12); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	err = n.ConnectTip(block4, 13)
	require.NoError(t, err, "Failed to connect block")
	if err := n.NotifyHeight(13); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// We should only receive one update for tx2 since it only requires
	// one confirmation and it already met it.
	select {
	case updDetails := <-ntfn2.Event.Updates:
		expected := chainntnfs.TxUpdateInfo{
			NumConfsLeft: 0,
			BlockHeight:  12,
		}
		require.Equal(t, expected, updDetails)
	default:
		t.Fatal("Expected confirmation update for tx2")
	}

	// A confirmation notification for tx2 should be dispatched, as it met
	// its required number of confirmations.
	select {
	case txConf := <-ntfn2.Event.Confirmed:
		expectedConf := chainntnfs.TxConfirmation{
			BlockHash:   block3.Hash(),
			BlockHeight: 12,
			TxIndex:     0,
			Tx:          &tx2,
		}
		assertConfDetails(t, txConf, &expectedConf)
	default:
		t.Fatalf("Expected confirmation for tx2")
	}

	// We should receive two updates for tx3 since it requires two
	// confirmations and it has already met them.
	for i := uint32(1); i <= 2; i++ {
		select {
		case updDetails := <-ntfn3.Event.Updates:
			expected := chainntnfs.TxUpdateInfo{
				NumConfsLeft: tx3NumConfs - i,
				BlockHeight:  12,
			}
			require.Equal(t, expected, updDetails)
		default:
			t.Fatal("Expected confirmation update for tx3")
		}
	}

	// A confirmation notification for tx3 should be dispatched, as it met
	// its required number of confirmations.
	select {
	case txConf := <-ntfn3.Event.Confirmed:
		expectedConf := chainntnfs.TxConfirmation{
			BlockHash:   block3.Hash(),
			BlockHeight: 12,
			TxIndex:     1,
			Tx:          &tx3,
		}
		assertConfDetails(t, txConf, &expectedConf)
	default:
		t.Fatalf("Expected confirmation for tx3")
	}
}

// TestTxNotifierReorgPartialConfirmation ensures that a tx with intermediate
// confirmations handles a reorg correctly and emits the appropriate reorg ntfn.
func TestTxNotifierReorgPartialConfirmation(t *testing.T) {
	t.Parallel()

	const txNumConfs uint32 = 2
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		7, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	// Tx will be confirmed in block 9 and requires 2 confs.
	tx := wire.MsgTx{Version: 1}
	tx.AddTxOut(&wire.TxOut{PkScript: testRawScript})
	txHash := tx.TxHash()
	ntfn, err := n.RegisterConf(&txHash, testRawScript, txNumConfs, 1)
	require.NoError(t, err, "unable to register ntfn")

	err = n.UpdateConfDetails(ntfn.HistoricalDispatch.ConfRequest, nil)
	require.NoError(t, err, "unable to deliver conf details")

	// Mine 1 block to satisfy the requirement for a partially confirmed tx.
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx},
	})
	err = n.ConnectTip(block, 8)
	require.NoError(t, err, "failed to connect block")
	err = n.NotifyHeight(8)
	require.NoError(t, err, "unable to dispatch notifications")

	// Now that the transaction is partially confirmed, reorg out those
	// blocks.
	err = n.DisconnectTip(8)
	require.NoError(t, err, "unable to disconnect block")

	// After the intermediate confirmation is reorged out, the tx should not
	// trigger a confirmation ntfn, but should trigger a reorg ntfn.
	select {
	case <-ntfn.Event.Confirmed:
		t.Fatal("unexpected confirmation after reorg")
	default:
	}

	select {
	case <-ntfn.Event.NegativeConf:
	default:
		t.Fatal("expected to receive reorg notification")
	}
}

// TestTxNotifierSpendReorg ensures that clients are notified of a reorg when
// the spending transaction of an outpoint for which they registered a spend
// notification for has been reorged out of the chain.
func TestTxNotifierSpendReorg(t *testing.T) {
	t.Parallel()

	const startingHeight = 10
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startingHeight, chainntnfs.ReorgSafetyLimit, hintCache,
		hintCache,
	)

	// We'll have two outpoints that will be spent throughout the test. The
	// first will be spent and will not experience a reorg, while the second
	// one will.
	op1 := wire.OutPoint{Index: 1}
	spendTx1 := wire.NewMsgTx(2)
	spendTx1.AddTxIn(&wire.TxIn{
		PreviousOutPoint: op1,
		SignatureScript:  testSigScript,
	})
	spendTxHash1 := spendTx1.TxHash()
	expectedSpendDetails1 := &chainntnfs.SpendDetail{
		SpentOutPoint:     &op1,
		SpenderTxHash:     &spendTxHash1,
		SpendingTx:        spendTx1,
		SpenderInputIndex: 0,
		SpendingHeight:    startingHeight + 1,
	}

	op2 := wire.OutPoint{Index: 2}
	spendTx2 := wire.NewMsgTx(2)
	spendTx2.AddTxIn(&wire.TxIn{
		PreviousOutPoint: chainntnfs.ZeroOutPoint,
		SignatureScript:  testSigScript,
	})
	spendTx2.AddTxIn(&wire.TxIn{
		PreviousOutPoint: op2,
		SignatureScript:  testSigScript,
	})
	spendTxHash2 := spendTx2.TxHash()

	// The second outpoint will experience a reorg and get re-spent at a
	// different height, so we'll need to construct the spend details for
	// before and after the reorg.
	expectedSpendDetails2BeforeReorg := chainntnfs.SpendDetail{
		SpentOutPoint:     &op2,
		SpenderTxHash:     &spendTxHash2,
		SpendingTx:        spendTx2,
		SpenderInputIndex: 1,
		SpendingHeight:    startingHeight + 2,
	}

	// The spend details after the reorg will be exactly the same, except
	// for the spend confirming at the next height.
	expectedSpendDetails2AfterReorg := expectedSpendDetails2BeforeReorg
	expectedSpendDetails2AfterReorg.SpendingHeight++

	// We'll register for a spend notification for each outpoint above.
	ntfn1, err := n.RegisterSpend(&op1, testRawScript, 1)
	require.NoError(t, err, "unable to register spend ntfn")

	ntfn2, err := n.RegisterSpend(&op2, testRawScript, 1)
	require.NoError(t, err, "unable to register spend ntfn")

	// We'll extend the chain by connecting a new block at tip. This block
	// will only contain the spending transaction of the first outpoint.
	block1 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx1},
	})
	err = n.ConnectTip(block1, startingHeight+1)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(startingHeight + 1); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// We should receive a spend notification for the first outpoint with
	// its correct spending details.
	select {
	case spendDetails := <-ntfn1.Event.Spend:
		assertSpendDetails(t, spendDetails, expectedSpendDetails1)
	default:
		t.Fatal("expected to receive spend details")
	}

	// We should not, however, receive one for the second outpoint as it has
	// yet to be spent.
	select {
	case <-ntfn2.Event.Spend:
		t.Fatal("received unexpected spend notification")
	default:
	}

	// Now, we'll extend the chain again, this time with a block containing
	// the spending transaction of the second outpoint.
	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx2},
	})
	err = n.ConnectTip(block2, startingHeight+2)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(startingHeight + 2); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// We should not receive another spend notification for the first
	// outpoint.
	select {
	case <-ntfn1.Event.Spend:
		t.Fatal("received unexpected spend notification")
	default:
	}

	// We should receive one for the second outpoint.
	select {
	case spendDetails := <-ntfn2.Event.Spend:
		assertSpendDetails(
			t, spendDetails, &expectedSpendDetails2BeforeReorg,
		)
	default:
		t.Fatal("expected to receive spend details")
	}

	// Now, to replicate a chain reorg, we'll disconnect the block that
	// contained the spending transaction of the second outpoint.
	if err := n.DisconnectTip(startingHeight + 2); err != nil {
		t.Fatalf("unable to disconnect block: %v", err)
	}

	// No notifications should be dispatched for the first outpoint as it
	// was spent at a previous height.
	select {
	case <-ntfn1.Event.Spend:
		t.Fatal("received unexpected spend notification")
	case <-ntfn1.Event.Reorg:
		t.Fatal("received unexpected spend reorg notification")
	default:
	}

	// We should receive a reorg notification for the second outpoint.
	select {
	case <-ntfn2.Event.Spend:
		t.Fatal("received unexpected spend notification")
	case <-ntfn2.Event.Reorg:
	default:
		t.Fatal("expected spend reorg notification")
	}

	// We'll now extend the chain with an empty block, to ensure that we can
	// properly detect when an outpoint has been re-spent at a later height.
	emptyBlock := btcutil.NewBlock(&wire.MsgBlock{})
	err = n.ConnectTip(emptyBlock, startingHeight+2)
	require.NoError(t, err, "unable to disconnect block")
	if err := n.NotifyHeight(startingHeight + 2); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// We shouldn't receive notifications for either of the outpoints.
	select {
	case <-ntfn1.Event.Spend:
		t.Fatal("received unexpected spend notification")
	case <-ntfn1.Event.Reorg:
		t.Fatal("received unexpected spend reorg notification")
	case <-ntfn2.Event.Spend:
		t.Fatal("received unexpected spend notification")
	case <-ntfn2.Event.Reorg:
		t.Fatal("received unexpected spend reorg notification")
	default:
	}

	// Finally, extend the chain with another block containing the same
	// spending transaction of the second outpoint.
	err = n.ConnectTip(block2, startingHeight+3)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(startingHeight + 3); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// We should now receive a spend notification once again for the second
	// outpoint containing the new spend details.
	select {
	case spendDetails := <-ntfn2.Event.Spend:
		assertSpendDetails(
			t, spendDetails, &expectedSpendDetails2AfterReorg,
		)
	default:
		t.Fatalf("expected to receive spend notification")
	}

	// Once again, we should not receive one for the first outpoint.
	select {
	case <-ntfn1.Event.Spend:
		t.Fatal("received unexpected spend notification")
	default:
	}
}

// TestTxNotifierSpendReorgMissed tests that a call to RegisterSpend after the
// spend has been confirmed, and then UpdateSpendDetails (called by historical
// dispatch), followed by a chain re-org will notify on the Reorg channel. This
// was not always the case and has since been fixed.
func TestTxNotifierSpendReorgMissed(t *testing.T) {
	t.Parallel()

	const startingHeight = 10
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startingHeight, chainntnfs.ReorgSafetyLimit, hintCache,
		hintCache,
	)

	// We'll create a spending transaction that spends the outpoint we'll
	// watch.
	op := wire.OutPoint{Index: 1}
	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: op,
		Witness:          testWitness,
		SignatureScript:  testSigScript,
	})
	spendTxHash := spendTx.TxHash()

	// Create the spend details that we'll call UpdateSpendDetails with.
	spendDetails := &chainntnfs.SpendDetail{
		SpentOutPoint:     &op,
		SpenderTxHash:     &spendTxHash,
		SpendingTx:        spendTx,
		SpenderInputIndex: 0,
		SpendingHeight:    startingHeight + 1,
	}

	// Now confirm the spending transaction.
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx},
	})
	err := n.ConnectTip(block, startingHeight+1)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(startingHeight + 1); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// We register for the spend now and will not get a spend notification
	// until we call UpdateSpendDetails.
	ntfn, err := n.RegisterSpend(&op, testRawScript, 1)
	require.NoError(t, err, "unable to register spend")

	// Assert that the HistoricalDispatch variable is non-nil. We'll use
	// the SpendRequest member to update the spend details.
	require.NotEmpty(t, ntfn.HistoricalDispatch)

	select {
	case <-ntfn.Event.Spend:
		t.Fatalf("did not expect to receive spend ntfn")
	default:
	}

	// We now call UpdateSpendDetails with our generated spend details to
	// simulate a historical spend dispatch being performed. This should
	// result in a notification being received on the Spend channel.
	err = n.UpdateSpendDetails(
		ntfn.HistoricalDispatch.SpendRequest, spendDetails,
	)
	require.Empty(t, err)

	// Assert that we receive a Spend notification.
	select {
	case <-ntfn.Event.Spend:
	default:
		t.Fatalf("expected to receive spend ntfn")
	}

	// We will now re-org the spending transaction out of the chain, and we
	// should receive a notification on the Reorg channel.
	err = n.DisconnectTip(startingHeight + 1)
	require.Empty(t, err)

	select {
	case <-ntfn.Event.Spend:
		t.Fatalf("received unexpected spend ntfn")
	case <-ntfn.Event.Reorg:
	default:
		t.Fatalf("expected spend reorg ntfn")
	}
}

// TestTxNotifierConfirmHintCache ensures that the height hints for transactions
// are kept track of correctly with each new block connected/disconnected. This
// test also asserts that the height hints are not updated until the simulated
// historical dispatches have returned, and we know the transactions aren't
// already in the chain.
func TestTxNotifierConfirmHintCache(t *testing.T) {
	t.Parallel()

	const (
		startingHeight = 200
		txDummyHeight  = 201
		tx1Height      = 202
		tx2Height      = 203
	)

	// Initialize our TxNotifier instance backed by a height hint cache.
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startingHeight, chainntnfs.ReorgSafetyLimit, hintCache,
		hintCache,
	)

	// Create two test transactions and register them for notifications.
	tx1 := wire.MsgTx{Version: 1}
	tx1.AddTxOut(&wire.TxOut{PkScript: testRawScript})
	tx1Hash := tx1.TxHash()
	ntfn1, err := n.RegisterConf(&tx1Hash, testRawScript, 1, 1)
	require.NoError(t, err, "unable to register tx1")

	tx2 := wire.MsgTx{Version: 2}
	tx2.AddTxOut(&wire.TxOut{PkScript: testRawScript})
	tx2Hash := tx2.TxHash()
	ntfn2, err := n.RegisterConf(&tx2Hash, testRawScript, 2, 1)
	require.NoError(t, err, "unable to register tx2")

	// Both transactions should not have a height hint set, as RegisterConf
	// should not alter the cache state.
	_, err = hintCache.QueryConfirmHint(ntfn1.HistoricalDispatch.ConfRequest)
	if err != chainntnfs.ErrConfirmHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"want: %v, got %v",
			chainntnfs.ErrConfirmHintNotFound, err)
	}

	_, err = hintCache.QueryConfirmHint(ntfn2.HistoricalDispatch.ConfRequest)
	if err != chainntnfs.ErrConfirmHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"want: %v, got %v",
			chainntnfs.ErrConfirmHintNotFound, err)
	}

	// Create a new block that will include the dummy transaction and extend
	// the chain.
	txDummy := wire.MsgTx{Version: 3}
	block1 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&txDummy},
	})

	err = n.ConnectTip(block1, txDummyHeight)
	require.NoError(t, err, "Failed to connect block")
	if err := n.NotifyHeight(txDummyHeight); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// Since UpdateConfDetails has not been called for either transaction,
	// the height hints should remain unchanged. This simulates blocks
	// confirming while the historical dispatch is processing the
	// registration.
	hint, err := hintCache.QueryConfirmHint(ntfn1.HistoricalDispatch.ConfRequest)
	if err != chainntnfs.ErrConfirmHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"want: %v, got %v",
			chainntnfs.ErrConfirmHintNotFound, err)
	}

	hint, err = hintCache.QueryConfirmHint(ntfn2.HistoricalDispatch.ConfRequest)
	if err != chainntnfs.ErrConfirmHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"want: %v, got %v",
			chainntnfs.ErrConfirmHintNotFound, err)
	}

	// Now, update the conf details reporting that the neither txn was found
	// in the historical dispatch.
	err = n.UpdateConfDetails(ntfn1.HistoricalDispatch.ConfRequest, nil)
	require.NoError(t, err, "unable to update conf details")
	err = n.UpdateConfDetails(ntfn2.HistoricalDispatch.ConfRequest, nil)
	require.NoError(t, err, "unable to update conf details")

	// We'll create another block that will include the first transaction
	// and extend the chain.
	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx1},
	})

	err = n.ConnectTip(block2, tx1Height)
	require.NoError(t, err, "Failed to connect block")
	if err := n.NotifyHeight(tx1Height); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// Now that both notifications are waiting at tip for confirmations,
	// they should have their height hints updated to the latest block
	// height.
	hint, err = hintCache.QueryConfirmHint(ntfn1.HistoricalDispatch.ConfRequest)
	require.NoError(t, err, "unable to query for hint")
	if hint != tx1Height {
		t.Fatalf("expected hint %d, got %d",
			tx1Height, hint)
	}

	hint, err = hintCache.QueryConfirmHint(ntfn2.HistoricalDispatch.ConfRequest)
	require.NoError(t, err, "unable to query for hint")
	if hint != tx1Height {
		t.Fatalf("expected hint %d, got %d",
			tx2Height, hint)
	}

	// Next, we'll create another block that will include the second
	// transaction and extend the chain.
	block3 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx2},
	})

	err = n.ConnectTip(block3, tx2Height)
	require.NoError(t, err, "Failed to connect block")
	if err := n.NotifyHeight(tx2Height); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// The height hint for the first transaction should remain the same.
	hint, err = hintCache.QueryConfirmHint(ntfn1.HistoricalDispatch.ConfRequest)
	require.NoError(t, err, "unable to query for hint")
	if hint != tx1Height {
		t.Fatalf("expected hint %d, got %d",
			tx1Height, hint)
	}

	// The height hint for the second transaction should now be updated to
	// reflect its confirmation.
	hint, err = hintCache.QueryConfirmHint(ntfn2.HistoricalDispatch.ConfRequest)
	require.NoError(t, err, "unable to query for hint")
	if hint != tx2Height {
		t.Fatalf("expected hint %d, got %d",
			tx2Height, hint)
	}

	// Finally, we'll attempt do disconnect the last block in order to
	// simulate a chain reorg.
	if err := n.DisconnectTip(tx2Height); err != nil {
		t.Fatalf("Failed to disconnect block: %v", err)
	}

	// This should update the second transaction's height hint within the
	// cache to the previous height.
	hint, err = hintCache.QueryConfirmHint(ntfn2.HistoricalDispatch.ConfRequest)
	require.NoError(t, err, "unable to query for hint")
	if hint != tx1Height {
		t.Fatalf("expected hint %d, got %d",
			tx1Height, hint)
	}

	// The first transaction's height hint should remain at the original
	// confirmation height.
	hint, err = hintCache.QueryConfirmHint(ntfn2.HistoricalDispatch.ConfRequest)
	require.NoError(t, err, "unable to query for hint")
	if hint != tx1Height {
		t.Fatalf("expected hint %d, got %d",
			tx1Height, hint)
	}
}

// TestTxNotifierSpendHintCache ensures that the height hints for outpoints are
// kept track of correctly with each new block connected/disconnected. This test
// also asserts that the height hints are not updated until the simulated
// historical dispatches have returned, and we know the outpoints haven't
// already been spent in the chain.
func TestTxNotifierSpendHintCache(t *testing.T) {
	t.Parallel()

	const (
		startingHeight = 200
		dummyHeight    = 201
		op1Height      = 202
		op2Height      = 203
	)

	// Initialize our TxNotifier instance backed by a height hint cache.
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startingHeight, chainntnfs.ReorgSafetyLimit, hintCache,
		hintCache,
	)

	// Create two test outpoints and register them for spend notifications.
	op1 := wire.OutPoint{Index: 1}
	ntfn1, err := n.RegisterSpend(&op1, testRawScript, 1)
	require.NoError(t, err, "unable to register spend for op1")
	op2 := wire.OutPoint{Index: 2}
	ntfn2, err := n.RegisterSpend(&op2, testRawScript, 1)
	require.NoError(t, err, "unable to register spend for op2")

	// Both outpoints should not have a spend hint set upon registration, as
	// we must first determine whether they have already been spent in the
	// chain.
	_, err = hintCache.QuerySpendHint(ntfn1.HistoricalDispatch.SpendRequest)
	if err != chainntnfs.ErrSpendHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"expected: %v, got %v", chainntnfs.ErrSpendHintNotFound,
			err)
	}
	_, err = hintCache.QuerySpendHint(ntfn2.HistoricalDispatch.SpendRequest)
	if err != chainntnfs.ErrSpendHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"expected: %v, got %v", chainntnfs.ErrSpendHintNotFound,
			err)
	}

	// Create a new empty block and extend the chain.
	emptyBlock := btcutil.NewBlock(&wire.MsgBlock{})
	err = n.ConnectTip(emptyBlock, dummyHeight)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(dummyHeight); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// Since we haven't called UpdateSpendDetails on any of the test
	// outpoints, this implies that there is a still a pending historical
	// rescan for them, so their spend hints should not be created/updated.
	_, err = hintCache.QuerySpendHint(ntfn1.HistoricalDispatch.SpendRequest)
	if err != chainntnfs.ErrSpendHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"expected: %v, got %v", chainntnfs.ErrSpendHintNotFound,
			err)
	}
	_, err = hintCache.QuerySpendHint(ntfn2.HistoricalDispatch.SpendRequest)
	if err != chainntnfs.ErrSpendHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"expected: %v, got %v", chainntnfs.ErrSpendHintNotFound,
			err)
	}

	// Now, we'll simulate that their historical rescans have finished by
	// calling UpdateSpendDetails. This should allow their spend hints to be
	// updated upon every block connected/disconnected.
	err = n.UpdateSpendDetails(ntfn1.HistoricalDispatch.SpendRequest, nil)
	require.NoError(t, err, "unable to update spend details")
	err = n.UpdateSpendDetails(ntfn2.HistoricalDispatch.SpendRequest, nil)
	require.NoError(t, err, "unable to update spend details")

	// We'll create a new block that only contains the spending transaction
	// of the first outpoint.
	spendTx1 := wire.NewMsgTx(2)
	spendTx1.AddTxIn(&wire.TxIn{
		PreviousOutPoint: op1,
		SignatureScript:  testSigScript,
	})
	block1 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx1},
	})
	err = n.ConnectTip(block1, op1Height)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(op1Height); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// Both outpoints should have their spend hints reflect the height of
	// the new block being connected due to the first outpoint being spent
	// at this height, and the second outpoint still being unspent.
	op1Hint, err := hintCache.QuerySpendHint(ntfn1.HistoricalDispatch.SpendRequest)
	require.NoError(t, err, "unable to query for spend hint of op1")
	if op1Hint != op1Height {
		t.Fatalf("expected hint %d, got %d", op1Height, op1Hint)
	}
	op2Hint, err := hintCache.QuerySpendHint(ntfn2.HistoricalDispatch.SpendRequest)
	require.NoError(t, err, "unable to query for spend hint of op2")
	if op2Hint != op1Height {
		t.Fatalf("expected hint %d, got %d", op1Height, op2Hint)
	}

	// Then, we'll create another block that spends the second outpoint.
	spendTx2 := wire.NewMsgTx(2)
	spendTx2.AddTxIn(&wire.TxIn{
		PreviousOutPoint: op2,
		SignatureScript:  testSigScript,
	})
	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx2},
	})
	err = n.ConnectTip(block2, op2Height)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(op2Height); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// Only the second outpoint should have its spend hint updated due to
	// being spent within the new block. The first outpoint's spend hint
	// should remain the same as it's already been spent before.
	op1Hint, err = hintCache.QuerySpendHint(ntfn1.HistoricalDispatch.SpendRequest)
	require.NoError(t, err, "unable to query for spend hint of op1")
	if op1Hint != op1Height {
		t.Fatalf("expected hint %d, got %d", op1Height, op1Hint)
	}
	op2Hint, err = hintCache.QuerySpendHint(ntfn2.HistoricalDispatch.SpendRequest)
	require.NoError(t, err, "unable to query for spend hint of op2")
	if op2Hint != op2Height {
		t.Fatalf("expected hint %d, got %d", op2Height, op2Hint)
	}

	// Finally, we'll attempt do disconnect the last block in order to
	// simulate a chain reorg.
	if err := n.DisconnectTip(op2Height); err != nil {
		t.Fatalf("unable to disconnect block: %v", err)
	}

	// This should update the second outpoint's spend hint within the cache
	// to the previous height, as that's where its spending transaction was
	// included in within the chain. The first outpoint's spend hint should
	// remain the same.
	op1Hint, err = hintCache.QuerySpendHint(ntfn1.HistoricalDispatch.SpendRequest)
	require.NoError(t, err, "unable to query for spend hint of op1")
	if op1Hint != op1Height {
		t.Fatalf("expected hint %d, got %d", op1Height, op1Hint)
	}
	op2Hint, err = hintCache.QuerySpendHint(ntfn2.HistoricalDispatch.SpendRequest)
	require.NoError(t, err, "unable to query for spend hint of op2")
	if op2Hint != op1Height {
		t.Fatalf("expected hint %d, got %d", op1Height, op2Hint)
	}
}

// TestTxNotifierSpendDuringHistoricalRescan checks that the height hints and
// spend notifications behave as expected when a spend is found at tip during a
// historical rescan.
func TestTxNotifierSpendDuringHistoricalRescan(t *testing.T) {
	t.Parallel()

	const (
		startingHeight = 200
		reorgSafety    = 10
	)

	// Initialize our TxNotifier instance backed by a height hint cache.
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startingHeight, reorgSafety, hintCache, hintCache,
	)

	// Create a test outpoint and register it for spend notifications.
	op1 := wire.OutPoint{Index: 1}
	ntfn1, err := n.RegisterSpend(&op1, testRawScript, 1)
	require.NoError(t, err, "unable to register spend for op1")

	// A historical rescan should be initiated from the height hint to the
	// current height.
	if ntfn1.HistoricalDispatch.StartHeight != 1 {
		t.Fatalf("expected historical dispatch to start at height hint")
	}

	if ntfn1.HistoricalDispatch.EndHeight != startingHeight {
		t.Fatalf("expected historical dispatch to end at current height")
	}

	// It should not have a spend hint set upon registration, as we must
	// first determine whether it has already been spent in the chain.
	_, err = hintCache.QuerySpendHint(ntfn1.HistoricalDispatch.SpendRequest)
	if err != chainntnfs.ErrSpendHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"expected: %v, got %v", chainntnfs.ErrSpendHintNotFound,
			err)
	}

	// Create a new empty block and extend the chain.
	height := uint32(startingHeight) + 1
	emptyBlock := btcutil.NewBlock(&wire.MsgBlock{})
	err = n.ConnectTip(emptyBlock, height)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(height); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// Since we haven't called UpdateSpendDetails yet, there should be no
	// spend hint found.
	_, err = hintCache.QuerySpendHint(ntfn1.HistoricalDispatch.SpendRequest)
	if err != chainntnfs.ErrSpendHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"expected: %v, got %v", chainntnfs.ErrSpendHintNotFound,
			err)
	}

	// Simulate a bunch of blocks being mined while the historical rescan
	// is still in progress. We make sure to not mine more than reorgSafety
	// blocks after the spend, since it will be forgotten then.
	var spendHeight uint32
	for i := 0; i < reorgSafety; i++ {
		height++

		// Let the outpoint we are watching be spent midway.
		var block *btcutil.Block
		if i == 5 {
			// We'll create a new block that only contains the
			// spending transaction of the outpoint.
			spendTx1 := wire.NewMsgTx(2)
			spendTx1.AddTxIn(&wire.TxIn{
				PreviousOutPoint: op1,
				SignatureScript:  testSigScript,
			})
			block = btcutil.NewBlock(&wire.MsgBlock{
				Transactions: []*wire.MsgTx{spendTx1},
			})
			spendHeight = height
		} else {
			// Otherwise we just create an empty block.
			block = btcutil.NewBlock(&wire.MsgBlock{})
		}

		err = n.ConnectTip(block, height)
		if err != nil {
			t.Fatalf("unable to connect block: %v", err)
		}
		if err := n.NotifyHeight(height); err != nil {
			t.Fatalf("unable to dispatch notifications: %v", err)
		}
	}

	// Check that the height hint was set to the spending block.
	op1Hint, err := hintCache.QuerySpendHint(
		ntfn1.HistoricalDispatch.SpendRequest,
	)
	require.NoError(t, err, "unable to query for spend hint of op1")
	if op1Hint != spendHeight {
		t.Fatalf("expected hint %d, got %d", spendHeight, op1Hint)
	}

	// We should be getting notified about the spend at this point.
	select {
	case <-ntfn1.Event.Spend:
	default:
		t.Fatal("expected to receive spend notification")
	}

	// Now, we'll simulate that the historical rescan finished by
	// calling UpdateSpendDetails. Since a the spend actually happened at
	// tip while the rescan was in progress, the height hint should not be
	// updated to the latest height, but stay at the spend height.
	err = n.UpdateSpendDetails(ntfn1.HistoricalDispatch.SpendRequest, nil)
	require.NoError(t, err, "unable to update spend details")

	op1Hint, err = hintCache.QuerySpendHint(
		ntfn1.HistoricalDispatch.SpendRequest,
	)
	require.NoError(t, err, "unable to query for spend hint of op1")
	if op1Hint != spendHeight {
		t.Fatalf("expected hint %d, got %d", spendHeight, op1Hint)
	}

	// Then, we'll create another block that spends a second outpoint.
	op2 := wire.OutPoint{Index: 2}
	spendTx2 := wire.NewMsgTx(2)
	spendTx2.AddTxIn(&wire.TxIn{
		PreviousOutPoint: op2,
		SignatureScript:  testSigScript,
	})
	height++
	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx2},
	})
	err = n.ConnectTip(block2, height)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(height); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// The outpoint's spend hint should remain the same as it's already
	// been spent before.
	op1Hint, err = hintCache.QuerySpendHint(ntfn1.HistoricalDispatch.SpendRequest)
	require.NoError(t, err, "unable to query for spend hint of op1")
	if op1Hint != spendHeight {
		t.Fatalf("expected hint %d, got %d", spendHeight, op1Hint)
	}

	// Now mine enough blocks for the spend notification to be forgotten.
	for i := 0; i < 2*reorgSafety; i++ {
		height++
		block := btcutil.NewBlock(&wire.MsgBlock{})

		err := n.ConnectTip(block, height)
		if err != nil {
			t.Fatalf("unable to connect block: %v", err)
		}
		if err := n.NotifyHeight(height); err != nil {
			t.Fatalf("unable to dispatch notifications: %v", err)
		}
	}

	// Attempting to update spend details at this point should fail, since
	// the spend request should be removed. This is to ensure the height
	// hint won't be overwritten if the historical rescan finishes after
	// the spend request has been notified and removed because it has
	// matured.
	err = n.UpdateSpendDetails(ntfn1.HistoricalDispatch.SpendRequest, nil)
	if err == nil {
		t.Fatalf("expected updating spend details to fail")
	}

	// Finally, check that the height hint is still there, unchanged.
	op1Hint, err = hintCache.QuerySpendHint(ntfn1.HistoricalDispatch.SpendRequest)
	require.NoError(t, err, "unable to query for spend hint of op1")
	if op1Hint != spendHeight {
		t.Fatalf("expected hint %d, got %d", spendHeight, op1Hint)
	}
}

// TestTxNotifierNtfnDone ensures that a notification is sent to registered
// clients through the Done channel once the notification request is no longer
// under the risk of being reorged out of the chain.
func TestTxNotifierNtfnDone(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	const reorgSafetyLimit = 100
	n := chainntnfs.NewTxNotifier(10, reorgSafetyLimit, hintCache, hintCache)

	// We'll start by creating two notification requests: one confirmation
	// and one spend.
	confNtfn, err := n.RegisterConf(&chainntnfs.ZeroHash, testRawScript, 1, 1)
	require.NoError(t, err, "unable to register conf ntfn")
	spendNtfn, err := n.RegisterSpend(&chainntnfs.ZeroOutPoint, testRawScript, 1)
	require.NoError(t, err, "unable to register spend")

	// We'll create two transactions that will satisfy the notification
	// requests above and include them in the next block of the chain.
	tx := wire.NewMsgTx(1)
	tx.AddTxOut(&wire.TxOut{PkScript: testRawScript})
	spendTx := wire.NewMsgTx(1)
	spendTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 1},
		SignatureScript:  testSigScript,
	})
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx, spendTx},
	})

	err = n.ConnectTip(block, 11)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(11); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// With the chain extended, we should see notifications dispatched for
	// both requests.
	select {
	case <-confNtfn.Event.Confirmed:
	default:
		t.Fatal("expected to receive confirmation notification")
	}

	select {
	case <-spendNtfn.Event.Spend:
	default:
		t.Fatal("expected to receive spend notification")
	}

	// The done notifications should not be dispatched yet as the requests
	// are still under the risk of being reorged out the chain.
	select {
	case <-confNtfn.Event.Done:
		t.Fatal("received unexpected done notification for confirmation")
	case <-spendNtfn.Event.Done:
		t.Fatal("received unexpected done notification for spend")
	default:
	}

	// Now, we'll disconnect the block at tip to simulate a reorg. The reorg
	// notifications should be dispatched to the respective clients.
	if err := n.DisconnectTip(11); err != nil {
		t.Fatalf("unable to disconnect block: %v", err)
	}

	select {
	case <-confNtfn.Event.NegativeConf:
	default:
		t.Fatal("expected to receive reorg notification for confirmation")
	}

	select {
	case <-spendNtfn.Event.Reorg:
	default:
		t.Fatal("expected to receive reorg notification for spend")
	}

	// We'll reconnect the block that satisfies both of these requests.
	// We should see notifications dispatched for both once again.
	err = n.ConnectTip(block, 11)
	require.NoError(t, err, "unable to connect block")
	if err := n.NotifyHeight(11); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	select {
	case <-confNtfn.Event.Confirmed:
	default:
		t.Fatal("expected to receive confirmation notification")
	}

	select {
	case <-spendNtfn.Event.Spend:
	default:
		t.Fatal("expected to receive spend notification")
	}

	// Finally, we'll extend the chain with blocks until the requests are no
	// longer under the risk of being reorged out of the chain. We should
	// expect the done notifications to be dispatched.
	nextHeight := uint32(12)
	for i := nextHeight; i < nextHeight+reorgSafetyLimit; i++ {
		dummyBlock := btcutil.NewBlock(&wire.MsgBlock{})
		if err := n.ConnectTip(dummyBlock, i); err != nil {
			t.Fatalf("unable to connect block: %v", err)
		}
	}

	select {
	case <-confNtfn.Event.Done:
	default:
		t.Fatal("expected to receive done notification for confirmation")
	}

	select {
	case <-spendNtfn.Event.Done:
	default:
		t.Fatal("expected to receive done notification for spend")
	}
}

// TestTxNotifierTearDown ensures that the TxNotifier properly alerts clients
// that it is shutting down and will be unable to deliver notifications.
func TestTxNotifierTearDown(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	// To begin the test, we'll register for a confirmation and spend
	// notification.
	confNtfn, err := n.RegisterConf(&chainntnfs.ZeroHash, testRawScript, 1, 1)
	require.NoError(t, err, "unable to register conf ntfn")
	spendNtfn, err := n.RegisterSpend(&chainntnfs.ZeroOutPoint, testRawScript, 1)
	require.NoError(t, err, "unable to register spend ntfn")

	// With the notifications registered, we'll now tear down the notifier.
	// The notification channels should be closed for notifications, whether
	// they have been dispatched or not, so we should not expect to receive
	// any more updates.
	n.TearDown()

	select {
	case _, ok := <-confNtfn.Event.Confirmed:
		if ok {
			t.Fatal("expected closed Confirmed channel for conf ntfn")
		}
	case _, ok := <-confNtfn.Event.Updates:
		if ok {
			t.Fatal("expected closed Updates channel for conf ntfn")
		}
	case _, ok := <-confNtfn.Event.NegativeConf:
		if ok {
			t.Fatal("expected closed NegativeConf channel for conf ntfn")
		}
	case _, ok := <-spendNtfn.Event.Spend:
		if ok {
			t.Fatal("expected closed Spend channel for spend ntfn")
		}
	case _, ok := <-spendNtfn.Event.Reorg:
		if ok {
			t.Fatalf("expected closed Reorg channel for spend ntfn")
		}
	default:
		t.Fatalf("expected closed notification channels for all ntfns")
	}

	// Now that the notifier is torn down, we should no longer be able to
	// register notification requests.
	_, err = n.RegisterConf(&chainntnfs.ZeroHash, testRawScript, 1, 1)
	if err == nil {
		t.Fatal("expected confirmation registration to fail")
	}
	_, err = n.RegisterSpend(&chainntnfs.ZeroOutPoint, testRawScript, 1)
	if err == nil {
		t.Fatal("expected spend registration to fail")
	}
}

func assertConfDetails(t *testing.T, result, expected *chainntnfs.TxConfirmation) {
	t.Helper()

	if result.BlockHeight != expected.BlockHeight {
		t.Fatalf("Incorrect block height in confirmation details: "+
			"expected %d, got %d", expected.BlockHeight,
			result.BlockHeight)
	}
	if !result.BlockHash.IsEqual(expected.BlockHash) {
		t.Fatalf("Incorrect block hash in confirmation details: "+
			"expected %d, got %d", expected.BlockHash,
			result.BlockHash)
	}
	if result.TxIndex != expected.TxIndex {
		t.Fatalf("Incorrect tx index in confirmation details: "+
			"expected %d, got %d", expected.TxIndex, result.TxIndex)
	}
	if result.Tx.TxHash() != expected.Tx.TxHash() {
		t.Fatalf("expected tx hash %v, got %v", expected.Tx.TxHash(),
			result.Tx.TxHash())
	}
}

func assertSpendDetails(t *testing.T, result, expected *chainntnfs.SpendDetail) {
	t.Helper()

	if *result.SpentOutPoint != *expected.SpentOutPoint {
		t.Fatalf("expected spent outpoint %v, got %v",
			expected.SpentOutPoint, result.SpentOutPoint)
	}
	if !result.SpenderTxHash.IsEqual(expected.SpenderTxHash) {
		t.Fatalf("expected spender tx hash %v, got %v",
			expected.SpenderTxHash, result.SpenderTxHash)
	}
	if result.SpenderInputIndex != expected.SpenderInputIndex {
		t.Fatalf("expected spender input index %d, got %d",
			expected.SpenderInputIndex, result.SpenderInputIndex)
	}
	if result.SpendingHeight != expected.SpendingHeight {
		t.Fatalf("expected spending height %d, got %d",
			expected.SpendingHeight, result.SpendingHeight)
	}
}
