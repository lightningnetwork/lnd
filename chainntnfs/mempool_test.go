package chainntnfs

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

const testTimeout = 5 * time.Second

// dummyWitness is used to fill the witness data in a transaction.
var dummyWitness = [][]byte{{0x01}}

// TestMempoolSubscribeInput tests that we can successfully subscribe an input.
func TestMempoolSubscribeInput(t *testing.T) {
	t.Parallel()

	// Create a new mempool notifier instance.
	notifier := NewMempoolNotifier()

	// Create a test input.
	input := wire.OutPoint{Hash: [32]byte{1}}

	// Create the expected subscription.
	expectedSub := newMempoolSpendEvent(1, input)

	// Subscribe to the input.
	sub := notifier.SubscribeInput(input)

	// Verify the subscription is returned.
	require.Equal(t, expectedSub.id, sub.id)
	require.Equal(t, expectedSub.outpoint, sub.outpoint)

	// Verify that the subscription was added to the notifier.
	subs, loaded := notifier.subscribedInputs.Load(input)
	require.True(t, loaded)

	// Verify the saved subscription is the same as the expected one.
	sub, loaded = subs.Load(sub.id)
	require.True(t, loaded)
	require.Equal(t, expectedSub.id, sub.id)
	require.Equal(t, expectedSub.outpoint, sub.outpoint)
}

// TestMempoolUnsubscribeInput tests that we can successfully unsubscribe an
// input.
func TestMempoolUnsubscribeInput(t *testing.T) {
	t.Parallel()

	// Create a new mempool notifier instance.
	notifier := NewMempoolNotifier()

	// Register a spend notification for an outpoint.
	input := wire.OutPoint{Hash: [32]byte{1}}
	notifier.SubscribeInput(input)

	// Verify that the subscription was added to the notifier.
	_, loaded := notifier.subscribedInputs.Load(input)
	require.True(t, loaded)

	// Unsubscribe the input.
	notifier.UnsubscribeInput(input)

	// Verify that the input is gone.
	_, loaded = notifier.subscribedInputs.Load(input)
	require.False(t, loaded)
}

// TestMempoolUnsubscribeEvent tests that when a given input has multiple
// subscribers, removing one of them won't affect the others.
func TestMempoolUnsubscribeEvent(t *testing.T) {
	t.Parallel()

	// Create a new mempool notifier instance.
	notifier := NewMempoolNotifier()

	// Register a spend notification for an outpoint.
	input := wire.OutPoint{Hash: [32]byte{1}}
	sub1 := notifier.SubscribeInput(input)
	sub2 := notifier.SubscribeInput(input)

	// Verify that the subscription was added to the notifier.
	subs, loaded := notifier.subscribedInputs.Load(input)
	require.True(t, loaded)

	// sub1 should be found.
	_, loaded = subs.Load(sub1.id)
	require.True(t, loaded)

	// sub2 should be found.
	_, loaded = subs.Load(sub2.id)
	require.True(t, loaded)

	// Unsubscribe sub1.
	notifier.UnsubscribeEvent(sub1)

	// Verify that the subscription was removed from the notifier.
	subs, loaded = notifier.subscribedInputs.Load(input)
	require.True(t, loaded)

	// sub1 should be gone.
	_, loaded = subs.Load(sub1.id)
	require.False(t, loaded)

	// sub2 should still be found.
	_, loaded = subs.Load(sub2.id)
	require.True(t, loaded)
}

// TestMempoolFindRelevantInputsEmptyWitness tests that the mempool notifier
// returns an error when the witness stack is empty.
func TestMempoolFindRelevantInputsEmptyWitness(t *testing.T) {
	t.Parallel()

	// Create a new mempool notifier instance.
	notifier := NewMempoolNotifier()

	// Create two inputs and subscribe to the second one.
	input1 := wire.OutPoint{Hash: [32]byte{1}}
	input2 := wire.OutPoint{Hash: [32]byte{2}}

	// Make input2 the subscribed input.
	notifier.SubscribeInput(input2)

	// Create a transaction that spends the above two inputs.
	msgTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: input1},
			{PreviousOutPoint: input2},
		},
		TxOut: []*wire.TxOut{},
	}
	tx := btcutil.NewTx(msgTx)

	// Call the method.
	result, err := notifier.findRelevantInputs(tx)
	require.ErrorIs(t, err, ErrEmptyWitnessStack)
	require.Nil(t, result)
}

// TestMempoolFindRelevantInputs tests that the mempool notifier can find the
// spend of subscribed inputs from a given transaction.
func TestMempoolFindRelevantInputs(t *testing.T) {
	t.Parallel()

	// Create a new mempool notifier instance.
	notifier := NewMempoolNotifier()

	// Create two inputs and subscribe to the second one.
	input1 := wire.OutPoint{Hash: [32]byte{1}}
	input2 := wire.OutPoint{Hash: [32]byte{2}}

	// Make input2 the subscribed input.
	notifier.SubscribeInput(input2)

	// Create a transaction that spends the above two inputs.
	msgTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: input1, Witness: dummyWitness},
			{PreviousOutPoint: input2, Witness: dummyWitness},
		},
		TxOut: []*wire.TxOut{},
	}
	tx := btcutil.NewTx(msgTx)

	// Create the expected spend detail.
	detailExp := &SpendDetail{
		SpentOutPoint:     &input2,
		SpenderTxHash:     tx.Hash(),
		SpendingTx:        msgTx,
		SpenderInputIndex: 1,
	}

	// Call the method.
	result, err := notifier.findRelevantInputs(tx)
	require.NoError(t, err)

	// Verify that the result is as expected.
	require.Contains(t, result, input2)

	// Verify the returned spend details is as expected.
	detail := result[input2]
	require.Equal(t, detailExp, detail)
}

// TestMempoolNotifySpentSameInputs tests that the mempool notifier sends
// notifications to all subscribers of the same input.
func TestMempoolNotifySpentSameInputs(t *testing.T) {
	t.Parallel()

	// Create a new mempool notifier instance.
	notifier := NewMempoolNotifier()

	// Register a spend notification for an outpoint.
	input := wire.OutPoint{Hash: [32]byte{1}}
	sub1 := notifier.SubscribeInput(input)
	sub2 := notifier.SubscribeInput(input)

	// Create a transaction that spends input.
	msgTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: input},
		},
	}
	tx := btcutil.NewTx(msgTx)

	// Notify the subscribers about the spent input.
	spendDetail := &SpendDetail{
		SpentOutPoint:     &input,
		SpenderTxHash:     tx.Hash(),
		SpendingTx:        msgTx,
		SpenderInputIndex: 0,
	}
	notifier.notifySpent(inputsWithTx{input: spendDetail})

	// Verify that sub1 received the spend notification for input1.
	select {
	case spend := <-sub1.Spend:
		require.Equal(t, tx.Hash(), spend.SpenderTxHash)

	case <-time.After(testTimeout):
		require.Fail(t, "timeout for sub1 to receive")
	}

	// Verify that sub2 received the spend notification for input1.
	select {
	case spend := <-sub2.Spend:
		require.Equal(t, tx.Hash(), spend.SpenderTxHash)

	case <-time.After(testTimeout):
		require.Fail(t, "timeout for sub2 to receive")
	}
}

// TestMempoolNotifySpentDifferentInputs tests that the mempool notifier sends
// notifications to different subscribers of different inputs.
func TestMempoolNotifySpentDifferentInputs(t *testing.T) {
	t.Parallel()

	// Create a new mempool notifier instance.
	notifier := NewMempoolNotifier()

	// Create two inputs and subscribe to them.
	input1 := wire.OutPoint{Hash: [32]byte{1}, Index: 0}
	input2 := wire.OutPoint{Hash: [32]byte{2}, Index: 0}
	sub1 := notifier.SubscribeInput(input1)
	sub2 := notifier.SubscribeInput(input2)

	// Create a transaction that spends input1.
	msgTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: input1},
		},
	}
	tx := btcutil.NewTx(msgTx)

	spendDetail1 := &SpendDetail{
		SpentOutPoint:     &input1,
		SpenderTxHash:     tx.Hash(),
		SpendingTx:        msgTx,
		SpenderInputIndex: 0,
	}

	// Notify the subscribers about the spent input.
	notifier.notifySpent(inputsWithTx{input1: spendDetail1})

	// Verify that sub1 received the spend notification for input1.
	select {
	case spend := <-sub1.Spend:
		require.Equal(t, tx.Hash(), spend.SpenderTxHash)

	case <-time.After(testTimeout):
		require.Fail(t, "timeout for sub1 to receive")
	}

	// Verify that sub2 did not receive any spend notifications.
	select {
	case <-sub2.Spend:
		require.Fail(t, "Expected sub2 to not receive")

	// Give it one second to NOT receive a spend notification.
	case <-time.After(1 * time.Second):
	}

	// Create another transaction that spends input1 and input2.
	msgTx2 := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: input1},
			{PreviousOutPoint: input2},
		},
	}
	tx2 := btcutil.NewTx(msgTx)

	spendDetail2 := &SpendDetail{
		SpentOutPoint:     &input2,
		SpenderTxHash:     tx2.Hash(),
		SpendingTx:        msgTx2,
		SpenderInputIndex: 1,
	}

	// Notify the subscribers about the spent inputs.
	notifier.notifySpent(inputsWithTx{
		input1: spendDetail1, input2: spendDetail2,
	})

	// Verify that sub1 received the spend notification for input1.
	select {
	case spend := <-sub1.Spend:
		require.Equal(t, tx.Hash(), spend.SpenderTxHash)

	case <-time.After(testTimeout):
		require.Fail(t, "timeout for sub1 to receive")
	}

	// Verify that sub2 received the spend notification for input2.
	select {
	case spend := <-sub2.Spend:
		require.Equal(t, tx2.Hash(), spend.SpenderTxHash)

	case <-time.After(testTimeout):
		require.Fail(t, "timeout for sub2 to receive")
	}
}

// TestMempoolNotifySpentCancel tests that once a subscription is canceled, it
// won't get notified and won't affect other subscriptions.
func TestMempoolNotifySpentCancel(t *testing.T) {
	t.Parallel()

	// Create a new mempool notifier instance.
	notifier := NewMempoolNotifier()

	// Register a spend notification for an outpoint.
	input := wire.OutPoint{Hash: [32]byte{1}}
	sub1 := notifier.SubscribeInput(input)
	sub2 := notifier.SubscribeInput(input)

	// Create a transaction that spends input.
	msgTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: input},
		},
	}
	tx := btcutil.NewTx(msgTx)

	// Cancel the second subscription before notify.
	notifier.UnsubscribeEvent(sub2)

	// Notify the subscribers about the spent input.
	spendDetail := &SpendDetail{
		SpentOutPoint:     &input,
		SpenderTxHash:     tx.Hash(),
		SpendingTx:        msgTx,
		SpenderInputIndex: 0,
	}
	notifier.notifySpent(inputsWithTx{input: spendDetail})

	// Verify that sub1 received the spend notification for input1.
	select {
	case spend := <-sub1.Spend:
		require.Equal(t, tx.Hash(), spend.SpenderTxHash)

	case <-time.After(testTimeout):
		require.Fail(t, "timeout for sub1 to receive")
	}

	// Verify that sub2 did not receive any spend notifications.
	select {
	case <-sub2.Spend:
		require.Fail(t, "expected sub2 to not receive")

	// Give it one second to NOT receive a spend notification.
	case <-time.After(1 * time.Second):
		// Expected
	}
}

// TestMempoolUnsubscribeConfirmedSpentTx tests that the subscriptions for a
// confirmed tx are removed when calling the method.
func TestMempoolUnsubscribeConfirmedSpentTx(t *testing.T) {
	t.Parallel()

	// Create a new mempool notifier instance.
	notifier := NewMempoolNotifier()

	// Create two inputs and subscribe to them.
	input1 := wire.OutPoint{Hash: [32]byte{1}, Index: 0}
	input2 := wire.OutPoint{Hash: [32]byte{2}, Index: 0}

	// sub1 and sub2 are subscribed to the same input.
	notifier.SubscribeInput(input1)
	notifier.SubscribeInput(input1)

	// sub3 is subscribed to a different input.
	sub3 := notifier.SubscribeInput(input2)

	// Create a transaction that spends input1.
	msgTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: input1, Witness: dummyWitness},
		},
	}
	tx := btcutil.NewTx(msgTx)

	// Unsubscribe the relevant transaction.
	notifier.UnsubsribeConfirmedSpentTx(tx)

	// Verify that the sub1 and sub2 are removed from the notifier.
	_, loaded := notifier.subscribedInputs.Load(input1)
	require.False(t, loaded)

	// Verify that the sub3 is not affected.
	subs, loaded := notifier.subscribedInputs.Load(input2)
	require.True(t, loaded)

	// sub3 should still be found.
	_, loaded = subs.Load(sub3.id)
	require.True(t, loaded)
}
