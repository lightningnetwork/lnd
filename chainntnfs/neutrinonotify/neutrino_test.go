package neutrinonotify

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2/gcs/builder"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

var testP2SHPkScript = []byte{
	txscript.OP_HASH160,
	txscript.OP_DATA_20,
	0x90, 0x1c, 0x86, 0x94, 0xc0, 0x3f, 0xaf, 0xd5,
	0x52, 0x28, 0x10, 0xe0, 0x33, 0x0f, 0x26, 0xe6,
	0x7a, 0x85, 0x33, 0xcd,
	txscript.OP_EQUAL,
}

// testBareMultisigScript returns a standard bare multisig script. Converting
// this script into its extracted pubkey address would produce a different
// pay-to-pubkey script, so it is a useful regression case for exact matching.
func testBareMultisigScript(t *testing.T) []byte {
	t.Helper()

	pubKey := []byte{
		0x02, 0x79, 0xbe, 0x66, 0x7e, 0xf9, 0xdc, 0xbb,
		0xac, 0x55, 0xa0, 0x62, 0x95, 0xce, 0x87, 0x0b,
		0x07, 0x02, 0x9b, 0xfc, 0xdb, 0x2d, 0xce, 0x28,
		0xd9, 0x59, 0xf2, 0x81, 0x5b, 0x16, 0xf8, 0x17,
		0x98,
	}

	pkScript, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_1).
		AddData(pubKey).
		AddOp(txscript.OP_1).
		AddOp(txscript.OP_CHECKMULTISIG).
		Script()
	require.NoError(t, err)

	return pkScript
}

// TestPkScriptsToInputsUsesExactScripts ensures all scripts included in basic
// compact filters are installed without an address conversion.
func TestPkScriptsToInputsUsesExactScripts(t *testing.T) {
	t.Parallel()

	pkScripts := [][]byte{
		{txscript.OP_TRUE},
		testBareMultisigScript(t),
		testP2SHPkScript,
	}
	inputs, err := pkScriptsToInputs(pkScripts)
	require.NoError(t, err)
	require.Len(t, inputs, len(pkScripts))

	block := &wire.MsgBlock{}
	tx := wire.NewMsgTx(2)
	for i, pkScript := range pkScripts {
		require.Equal(t, chainntnfs.ZeroOutPoint, inputs[i].OutPoint)
		require.Equal(t, pkScript, inputs[i].PkScript)

		tx.AddTxOut(&wire.TxOut{PkScript: pkScript})
	}
	require.NoError(t, block.AddTransaction(tx))

	filter, err := builder.BuildBasicFilter(block, nil)
	require.NoError(t, err)

	blockHash := block.BlockHash()
	key := builder.DeriveKey(&blockHash)
	for _, pkScript := range pkScripts {
		match, err := filter.Match(key, pkScript)
		require.NoError(t, err)
		require.True(t, match)
	}
}

// TestPkScriptsToInputsRejectsOpReturn ensures scripts omitted from basic
// compact filters are rejected before mutating the local registration.
func TestPkScriptsToInputsRejectsOpReturn(t *testing.T) {
	t.Parallel()

	_, err := pkScriptsToInputs([][]byte{{
		txscript.OP_RETURN, txscript.OP_DATA_1, 0x01,
	}})
	require.ErrorIs(t, err, chainntnfs.ErrUnsupportedPkScript)
}

// TestPkScriptsToInputsDeduplicatesScripts ensures duplicate scripts only
// produce one exact neutrino filter input.
func TestPkScriptsToInputsDeduplicatesScripts(t *testing.T) {
	t.Parallel()

	inputs, err := pkScriptsToInputs(
		[][]byte{testP2SHPkScript, testP2SHPkScript},
	)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
}

func newTestNeutrinoNotifier() *NeutrinoNotifier {
	return &NeutrinoNotifier{
		notificationRegistry: make(chan interface{}),
		backendPkScripts:     make(map[string]struct{}),
		quit:                 make(chan struct{}),
	}
}

func updateFilterAsync(n *NeutrinoNotifier, pkScripts [][]byte) <-chan error {
	errChan := make(chan error, 1)
	go func() {
		errChan <- n.updatePkScriptFilter(pkScripts, 1)
	}()

	return errChan
}

func receiveFilterUpdate(t *testing.T,
	n *NeutrinoNotifier) *rescanFilterUpdate {

	t.Helper()

	select {
	case msg := <-n.notificationRegistry:
		update, ok := msg.(*rescanFilterUpdate)
		require.True(t, ok)

		return update

	case <-time.After(5 * time.Second):
		t.Fatal("filter update not received")
		return nil
	}
}

// TestUpdatePkScriptFilterFailureCanRetry verifies a failed backend update is
// not remembered as installed and a retry reaches the backend again.
func TestUpdatePkScriptFilterFailureCanRetry(t *testing.T) {
	t.Parallel()

	n := newTestNeutrinoNotifier()
	backendErr := errors.New("backend update failed")

	result := updateFilterAsync(n, [][]byte{{txscript.OP_TRUE}})
	update := receiveFilterUpdate(t, n)
	update.errChan <- backendErr
	require.ErrorIs(t, <-result, backendErr)
	require.Empty(t, n.backendPkScripts)
	require.Zero(t, n.backendPkScriptsBytes)

	result = updateFilterAsync(n, [][]byte{{txscript.OP_TRUE}})
	update = receiveFilterUpdate(t, n)
	update.errChan <- nil
	require.NoError(t, <-result)
	require.Len(t, n.backendPkScripts, 1)
	require.Equal(t, uint64(1), n.backendPkScriptsBytes)
}

// TestUpdatePkScriptFilterConcurrentDedup verifies concurrent additions are
// serialized around the backend update and only install one filter entry.
func TestUpdatePkScriptFilterConcurrentDedup(t *testing.T) {
	t.Parallel()

	n := newTestNeutrinoNotifier()
	start := make(chan struct{})
	results := make(chan error, 2)
	var callers sync.WaitGroup
	callers.Add(2)

	for i := 0; i < 2; i++ {
		go func() {
			defer callers.Done()
			<-start
			results <- n.updatePkScriptFilter(
				[][]byte{{txscript.OP_TRUE}}, 1,
			)
		}()
	}
	close(start)

	update := receiveFilterUpdate(t, n)
	update.errChan <- nil
	callers.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}

	select {
	case <-n.notificationRegistry:
		t.Fatal("duplicate backend filter update")
	default:
	}
	require.Len(t, n.backendPkScripts, 1)
}

// TestConcurrentRegistrationAddsRollbackIndependently verifies one failed add
// cannot roll back a concurrent successful add on the same registration.
func TestConcurrentRegistrationAddsRollbackIndependently(t *testing.T) {
	t.Parallel()

	n := newTestNeutrinoNotifier()
	n.txNotifier = chainntnfs.NewTxNotifier(
		1, chainntnfs.ReorgSafetyLimit, nil, nil,
	)
	defer n.txNotifier.TearDown()

	reg, err := n.RegisterPkScriptNotifier()
	require.NoError(t, err)

	type addResponse struct {
		result *chainntnfs.PkScriptAddResult
		err    error
	}
	add := func() <-chan addResponse {
		response := make(chan addResponse, 1)
		go func() {
			result, err := reg.AddPkScripts(
				[][]byte{{txscript.OP_TRUE}},
			)
			response <- addResponse{result: result, err: err}
		}()

		return response
	}

	first := add()
	firstUpdate := receiveFilterUpdate(t, n)
	second := add()

	backendErr := errors.New("backend update failed")
	firstUpdate.errChan <- backendErr
	firstResponse := <-first
	require.ErrorIs(t, firstResponse.err, backendErr)

	secondUpdate := receiveFilterUpdate(t, n)
	secondUpdate.errChan <- nil
	secondResponse := <-second
	require.NoError(t, secondResponse.err)
	require.Equal(t, uint32(1), secondResponse.result.NumAdded)
	require.Len(t, n.backendPkScripts, 1)
}

// TestRegistrationCancelDoesNotWaitForFilterUpdate ensures cancellation stays
// responsive even while an add is waiting for the backend.
func TestRegistrationCancelDoesNotWaitForFilterUpdate(t *testing.T) {
	t.Parallel()

	n := newTestNeutrinoNotifier()
	n.txNotifier = chainntnfs.NewTxNotifier(
		1, chainntnfs.ReorgSafetyLimit, nil, nil,
	)
	defer n.txNotifier.TearDown()

	reg, err := n.RegisterPkScriptNotifier()
	require.NoError(t, err)

	addResult := make(chan error, 1)
	go func() {
		_, err := reg.AddPkScripts([][]byte{{txscript.OP_TRUE}})
		addResult <- err
	}()
	update := receiveFilterUpdate(t, n)

	canceled := make(chan struct{})
	go func() {
		reg.Cancel()
		close(canceled)
	}()

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("cancel blocked on backend filter update")
	}

	update.errChan <- nil
	require.NoError(t, <-addResult)
}

// TestUpdatePkScriptFilterLifetimeByteLimit ensures unique add/remove churn
// cannot grow neutrino's append-only filter beyond the existing global limit.
func TestUpdatePkScriptFilterLifetimeByteLimit(t *testing.T) {
	t.Parallel()

	n := newTestNeutrinoNotifier()
	n.backendPkScriptsBytes = chainntnfs.MaxPkScriptWatchBytes

	err := n.updatePkScriptFilter([][]byte{{txscript.OP_TRUE}}, 1)
	require.ErrorIs(t, err, chainntnfs.ErrTooManyPkScripts)
	require.Empty(t, n.backendPkScripts)

	select {
	case <-n.notificationRegistry:
		t.Fatal("over-limit backend filter update")
	default:
	}
}

// TestPkScriptFilterDedupSurvivesRemoval verifies local removal and stream
// cancellation don't cause the append-only backend filter to grow on re-add.
func TestPkScriptFilterDedupSurvivesRemoval(t *testing.T) {
	t.Parallel()

	n := newTestNeutrinoNotifier()
	n.txNotifier = chainntnfs.NewTxNotifier(
		1, chainntnfs.ReorgSafetyLimit, nil, nil,
	)
	defer n.txNotifier.TearDown()

	reg, err := n.RegisterPkScriptNotifier()
	require.NoError(t, err)

	result := make(chan error, 1)
	go func() {
		_, err := reg.AddPkScripts([][]byte{{txscript.OP_TRUE}})
		result <- err
	}()
	update := receiveFilterUpdate(t, n)
	update.errChan <- nil
	require.NoError(t, <-result)

	require.NoError(t, reg.RemovePkScripts(
		[][]byte{{txscript.OP_TRUE}},
	))
	addResult, err := reg.AddPkScripts([][]byte{{txscript.OP_TRUE}})
	require.NoError(t, err)
	require.Equal(t, uint32(1), addResult.NumAdded)

	reg.Cancel()
	reg, err = n.RegisterPkScriptNotifier()
	require.NoError(t, err)
	addResult, err = reg.AddPkScripts([][]byte{{txscript.OP_TRUE}})
	require.NoError(t, err)
	require.Equal(t, uint32(1), addResult.NumAdded)

	select {
	case <-n.notificationRegistry:
		t.Fatal("duplicate backend filter update after re-add")
	default:
	}
}
