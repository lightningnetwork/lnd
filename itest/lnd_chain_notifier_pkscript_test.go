package itest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	crpc "github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type (
	pkScriptEventType    = crpc.PkScriptEventType
	pkScriptNotification = crpc.PkScriptNotification
	pkScriptRequest      = crpc.PkScriptRequest
)

const confirmEvent = crpc.PkScriptEventType_PK_SCRIPT_EVENT_TYPE_CONFIRM
const spendEvent = crpc.PkScriptEventType_PK_SCRIPT_EVENT_TYPE_SPEND
const confUpd = crpc.PkScriptEventType_PK_SCRIPT_EVENT_TYPE_CONFIRMATION_UPDATE
const regAct = crpc.PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_REGISTER
const removeAct = crpc.PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_REMOVE

// errPkScriptEventTimeout is returned when no pkScript stream event arrives
// before the caller's timeout expires.
var errPkScriptEventTimeout = errors.New("timed out waiting for pkScript event")

// pkScriptStreamEvent couples a received pkScript stream event with the receive
// error returned by the RPC client.
type pkScriptStreamEvent struct {
	event *crpc.PkScriptEvent
	err   error
}

// pkScriptStreamHarness wraps the bidirectional pkScript notification RPC
// stream and buffers out-of-order event types for focused test assertions.
type pkScriptStreamHarness struct {
	ht *lntest.HarnessTest

	cancel context.CancelFunc
	client crpc.ChainNotifier_RegisterPkScriptNtfnClient
	events chan pkScriptStreamEvent

	pending []*crpc.PkScriptNotification
}

// newPkScriptStreamHarness opens a pkScript notification stream for the target
// node and starts a receive loop for stream events.
func newPkScriptStreamHarness(ht *lntest.HarnessTest,
	hn *node.HarnessNode) *pkScriptStreamHarness {

	ctx, cancel := context.WithCancel(ht.Context())
	client, err := hn.RPC.ChainClient.RegisterPkScriptNtfn(ctx)
	require.NoError(ht, err)

	a := &pkScriptStreamHarness{
		ht:     ht,
		cancel: cancel,
		client: client,
		events: make(chan pkScriptStreamEvent, 100),
	}

	go a.recvEvents()

	return a
}

// recvEvents forwards RPC stream receive results into the harness event queue
// until the stream returns a terminal error.
func (a *pkScriptStreamHarness) recvEvents() {
	for {
		event, err := a.client.Recv()
		a.events <- pkScriptStreamEvent{
			event: event,
			err:   err,
		}

		if err != nil {
			close(a.events)
			return
		}
	}
}

// close closes the send side of the RPC stream and cancels its context.
func (a *pkScriptStreamHarness) close() {
	_ = a.client.CloseSend()
	a.cancel()
}

// send sends a request on the pkScript stream and fails the test on error.
func (a *pkScriptStreamHarness) send(req *crpc.PkScriptRequest) {
	err := a.client.Send(req)
	require.NoError(a.ht, err)
}

// pkScriptRegisterReq builds the initial registration request required by the
// pkScript notification stream.
func pkScriptRegisterReq() *crpc.PkScriptRequest {
	return &crpc.PkScriptRequest{
		Request: &crpc.PkScriptRequest_Register{
			Register: &crpc.PkScriptRegisterRequest{},
		},
	}
}

// pkScriptAddReq builds an add request without partial confirmation updates.
func pkScriptAddReq(
	pkScripts [][]byte, events []pkScriptEventType,
	numConfs uint32, includeBlock, includeTx bool) *pkScriptRequest {

	return pkScriptAddReqWithConfUpdates(
		pkScripts, events, numConfs, includeBlock, includeTx, false,
	)
}

// pkScriptAddReqWithConfUpdates builds an add request, optionally enabling
// partial confirmation update notifications.
func pkScriptAddReqWithConfUpdates(pkScripts [][]byte,
	events []crpc.PkScriptEventType, numConfs uint32,
	includeBlock, includeTx, includeConfirmationUpdates bool,
) *crpc.PkScriptRequest {

	add := &crpc.AddPkScriptRequest{
		PkScripts:                  pkScripts,
		Events:                     events,
		NumConfs:                   numConfs,
		IncludeBlock:               includeBlock,
		IncludeTx:                  includeTx,
		IncludeConfirmationUpdates: includeConfirmationUpdates,
	}

	return &crpc.PkScriptRequest{
		Request: &crpc.PkScriptRequest_Add{Add: add},
	}
}

// pkScriptRemoveReq builds a remove request for the provided pkScripts.
func pkScriptRemoveReq(pkScripts [][]byte) *crpc.PkScriptRequest {
	return &crpc.PkScriptRequest{
		Request: &crpc.PkScriptRequest_Remove{
			Remove: &crpc.RemovePkScriptRequest{
				PkScripts: pkScripts,
			},
		},
	}
}

// recvEvent returns the next queued stream event or a timeout error.
func (a *pkScriptStreamHarness) recvEvent(
	timeout time.Duration) (*crpc.PkScriptEvent, error) {

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil, errPkScriptEventTimeout

	case event, ok := <-a.events:
		if !ok {
			return nil, fmt.Errorf("pkScript event stream closed")
		}

		return event.event, event.err
	}
}

// requireAck waits for a mutation ack, buffering notifications that arrive
// first.
func (a *pkScriptStreamHarness) requireAck(
	action crpc.PkScriptMutationAction) *crpc.PkScriptMutationAck {

	a.ht.Helper()

	for {
		event, err := a.recvEvent(defaultTimeout)
		require.NoError(a.ht, err)

		if ack := event.GetAck(); ack != nil {
			require.Equal(a.ht, action, ack.Action)
			return ack
		}

		ntfn := event.GetNotification()
		require.NotNil(a.ht, ntfn)
		a.pending = append(a.pending, ntfn)
	}
}

// requireNotification returns the next notification, failing the test if a
// mutation ack arrives unexpectedly.
func (a *pkScriptStreamHarness) requireNotification() *pkScriptNotification {

	a.ht.Helper()

	if len(a.pending) > 0 {
		ntfn := a.pending[0]
		a.pending = a.pending[1:]
		return ntfn
	}

	for {
		event, err := a.recvEvent(defaultTimeout)
		require.NoError(a.ht, err)

		if ack := event.GetAck(); ack != nil {
			require.Failf(
				a.ht, "unexpected pkScript ack", "action=%v",
				ack.Action,
			)
		}

		ntfn := event.GetNotification()
		require.NotNil(a.ht, ntfn)

		return ntfn
	}
}

// requireNotifications returns count notifications from the pkScript stream.
func (a *pkScriptStreamHarness) requireNotifications(
	count int) []*crpc.PkScriptNotification {

	a.ht.Helper()

	ntfns := make([]*crpc.PkScriptNotification, 0, count)
	for i := 0; i < count; i++ {
		ntfns = append(ntfns, a.requireNotification())
	}

	return ntfns
}

// assertNoNotification asserts that no notification arrives before the timeout.
func (a *pkScriptStreamHarness) assertNoNotification(timeout time.Duration) {
	a.ht.Helper()

	if len(a.pending) > 0 {
		require.Failf(
			a.ht, "unexpected buffered pkScript notification",
			"count=%d", len(a.pending),
		)
	}

	event, err := a.recvEvent(timeout)
	if errors.Is(err, errPkScriptEventTimeout) {
		return
	}
	require.NoError(a.ht, err)

	require.Failf(a.ht, "unexpected pkScript event", "%v", event)
}

// newPkScriptTestAddress creates a fresh P2WPKH test address, its pkScript,
// and the corresponding private key.
func newPkScriptTestAddress(ht *lntest.HarnessTest) (address.Address, []byte,
	*btcec.PrivateKey) {

	ht.Helper()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(ht, err)

	pubKeyHash := address.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := address.NewAddressWitnessPubKeyHash(
		pubKeyHash, harnessNetParams,
	)
	require.NoError(ht, err)

	pkScript, err := txscript.PayToAddrScript(addr)
	require.NoError(ht, err)

	return addr, pkScript, privKey
}

// fundAddress sends coins to addr and returns the matching mempool output data.
func fundAddress(
	ht *lntest.HarnessTest, sender *node.HarnessNode,
	addr address.Address, pkScript []byte, value int64,
) (*chainhash.Hash, *wire.OutPoint, *wire.TxOut, *wire.MsgTx) {

	ht.Helper()

	txidStr := sender.RPC.SendCoins(&lnrpc.SendCoinsRequest{
		Addr:        addr.EncodeAddress(),
		Amount:      value,
		SatPerVbyte: 2,
	}).Txid

	txid, err := chainhash.NewHashFromStr(txidStr)
	require.NoError(ht, err)

	tx := ht.AssertTxInMempool(*txid)

	for i, txOut := range tx.TxOut {
		if txOut.Value != value {
			continue
		}
		if !bytes.Equal(txOut.PkScript, pkScript) {
			continue
		}

		outpoint := &wire.OutPoint{
			Hash:  *txid,
			Index: uint32(i),
		}
		output := &wire.TxOut{
			Value:    txOut.Value,
			PkScript: txOut.PkScript,
		}

		return txid, outpoint, output, tx
	}

	require.Fail(ht, "funding output not found")

	return nil, nil, nil, nil
}

// createSpendTx creates and signs a transaction spending the provided output to
// a fresh test address.
func createSpendTx(ht *lntest.HarnessTest, prevOutPoint *wire.OutPoint,
	prevOutput *wire.TxOut, privKey *btcec.PrivateKey) *wire.MsgTx {

	ht.Helper()

	spendAddr, _, _ := newPkScriptTestAddress(ht)
	spendScript, err := txscript.PayToAddrScript(spendAddr)
	require.NoError(ht, err)

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(prevOutPoint, nil, nil))
	tx.AddTxOut(wire.NewTxOut(prevOutput.Value-10_000, spendScript))

	sigHashes := input.NewTxSigHashesV0Only(tx)
	witnessScript, err := txscript.WitnessSignature(
		tx, sigHashes, 0, prevOutput.Value, prevOutput.PkScript,
		txscript.SigHashAll, privKey, true,
	)
	require.NoError(ht, err)

	tx.TxIn[0].Witness = witnessScript

	return tx
}

// notificationKey returns a stable key for a notification's watched outpoint.
func notificationKey(ntfn *crpc.PkScriptNotification) string {
	return fmt.Sprintf(
		"%x:%d", ntfn.Utxo.Outpoint.Hash, ntfn.Utxo.Outpoint.Index,
	)
}

// pkScriptNotificationMap indexes notifications by outpoint and event type.
func pkScriptNotificationMap(ht *lntest.HarnessTest,
	ntfns []*crpc.PkScriptNotification,
) map[string]*crpc.PkScriptNotification {

	ht.Helper()

	ntfnMap := make(map[string]*crpc.PkScriptNotification, len(ntfns))
	for _, ntfn := range ntfns {
		require.NotNil(ht, ntfn)

		ntfnKey := notificationKey(ntfn)
		key := fmt.Sprintf("%s:%d", ntfnKey, ntfn.EventType)
		require.NotContains(ht, ntfnMap, key)

		ntfnMap[key] = ntfn
	}

	return ntfnMap
}

// wireOutpointKey returns a stable string key for a wire outpoint.
func wireOutpointKey(outpoint *wire.OutPoint) string {
	return fmt.Sprintf("%x:%d", outpoint.Hash[:], outpoint.Index)
}

// pkScriptNotificationExpectations captures optional fields asserted for a
// pkScript notification.
type pkScriptNotificationExpectations struct {
	value         *int64
	pkScript      []byte
	utxoHeight    *uint32
	utxoBlockHash *chainhash.Hash
	utxoTxIndex   *uint32
	txIndex       *uint32
	inputIndex    *uint32
}

// pkScriptNotificationOption configures optional notification expectations.
type pkScriptNotificationOption func(*pkScriptNotificationExpectations)

// withPkScriptUTXO expects the notification to include UTXO metadata.
func withPkScriptUTXO(output *wire.TxOut, height uint32,
	blockHash *chainhash.Hash) pkScriptNotificationOption {

	return func(e *pkScriptNotificationExpectations) {
		e.value = &output.Value
		e.pkScript = output.PkScript
		e.utxoHeight = &height
		e.utxoBlockHash = blockHash
	}
}

// withPkScriptTxIndex expects the notification's top-level transaction index.
func withPkScriptTxIndex(txIndex uint32) pkScriptNotificationOption {
	return func(e *pkScriptNotificationExpectations) {
		e.txIndex = &txIndex
	}
}

// withPkScriptUTXOTxIndex expects the notification UTXO's transaction index.
func withPkScriptUTXOTxIndex(txIndex uint32) pkScriptNotificationOption {
	return func(e *pkScriptNotificationExpectations) {
		e.utxoTxIndex = &txIndex
	}
}

// withPkScriptInputIndex expects the notification's spend input index.
func withPkScriptInputIndex(inputIndex uint32) pkScriptNotificationOption {
	return func(e *pkScriptNotificationExpectations) {
		e.inputIndex = &inputIndex
	}
}

// assertPkScriptNotification verifies the common RPC notification fields,
// optional index fields, and optional raw transaction/block payloads.
func assertPkScriptNotification(
	ht *lntest.HarnessTest, ntfn *pkScriptNotification,
	expectedType pkScriptEventType,
	disconnected bool, height uint32, blockHash, txHash *chainhash.Hash,
	outpoint *wire.OutPoint, numConfs uint32, expectRaw bool,
	opts ...pkScriptNotificationOption) {

	ht.Helper()

	expectations := &pkScriptNotificationExpectations{}
	for _, opt := range opts {
		opt(expectations)
	}

	require.NotNil(ht, ntfn)
	require.Equal(ht, expectedType, ntfn.EventType)
	require.Equal(ht, disconnected, ntfn.Disconnected)
	require.Equal(ht, height, ntfn.Height)
	require.Equal(ht, numConfs, ntfn.NumConfirmations)
	require.NotNil(ht, ntfn.Utxo)
	require.NotNil(ht, ntfn.Utxo.Outpoint)
	require.Equal(ht, outpoint.Hash[:], ntfn.Utxo.Outpoint.Hash)
	require.Equal(ht, outpoint.Index, ntfn.Utxo.Outpoint.Index)
	require.Equal(ht, txHash[:], ntfn.TxHash)
	require.Equal(ht, blockHash[:], ntfn.BlockHash)

	if expectations.value != nil {
		require.Equal(ht, *expectations.value, ntfn.Utxo.Value)
	}
	if expectations.pkScript != nil {
		require.Equal(ht, expectations.pkScript, ntfn.Utxo.PkScript)
	}
	if expectations.utxoHeight != nil {
		require.Equal(
			ht, *expectations.utxoHeight, ntfn.Utxo.BlockHeight,
		)
	}
	if expectations.utxoBlockHash != nil {
		require.Equal(
			ht, expectations.utxoBlockHash[:], ntfn.Utxo.BlockHash,
		)
	}
	if expectations.utxoTxIndex != nil {
		require.Equal(ht, *expectations.utxoTxIndex, ntfn.Utxo.TxIndex)
	}
	if expectations.txIndex != nil {
		require.Equal(ht, *expectations.txIndex, ntfn.TxIndex)
	}
	if expectations.inputIndex != nil {
		require.Equal(ht, *expectations.inputIndex, ntfn.InputIndex)
	}

	if expectRaw {
		require.NotEmpty(ht, ntfn.RawTx)
		require.NotEmpty(ht, ntfn.RawBlock)

		var rawTx wire.MsgTx
		err := rawTx.Deserialize(bytes.NewReader(ntfn.RawTx))
		require.NoError(ht, err)
		require.Equal(ht, *txHash, rawTx.TxHash())

		var rawBlock wire.MsgBlock
		err = rawBlock.Deserialize(bytes.NewReader(ntfn.RawBlock))
		require.NoError(ht, err)
		require.Equal(ht, *blockHash, rawBlock.BlockHash())

		if expectedType ==
			spendEvent {

			block := btcutil.NewBlock(&rawBlock)
			indexedHash, err := block.TxHash(int(ntfn.TxIndex))
			require.NoError(ht, err)
			require.True(ht, indexedHash.IsEqual(txHash))
		}

		return
	}

	require.Empty(ht, ntfn.RawTx)
	require.Empty(ht, ntfn.RawBlock)
}

// requirePkScriptStreamError sends a request sequence and waits for the
// pkScript stream to fail with the expected RPC status code.
func requirePkScriptStreamError(ht *lntest.HarnessTest, hn *node.HarnessNode,
	code codes.Code, reqs ...*crpc.PkScriptRequest) {

	ht.Helper()

	stream := newPkScriptStreamHarness(ht, hn)
	defer stream.close()

	for _, req := range reqs {
		err := stream.client.Send(req)
		if err != nil {
			require.Equal(ht, code, status.Code(err))
			return
		}
	}

	for {
		_, err := stream.recvEvent(defaultTimeout)
		if err != nil {
			require.Equal(ht, code, status.Code(err))
			return
		}
	}
}

// newRegisteredPkScriptStream opens a pkScript stream and completes the initial
// register handshake.
func newRegisteredPkScriptStream(ht *lntest.HarnessTest,
	hn *node.HarnessNode) *pkScriptStreamHarness {

	stream := newPkScriptStreamHarness(ht, hn)
	stream.send(pkScriptRegisterReq())
	stream.requireAck(
		regAct,
	)

	return stream
}

// newPkScriptNotifierRPCScenario creates funded sender/receiver nodes and a
// registered pkScript stream on the receiver.
func newPkScriptNotifierRPCScenario(ht *lntest.HarnessTest) (
	*node.HarnessNode, *node.HarnessNode, *pkScriptStreamHarness) {

	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNode("Bob", nil)

	stream := newRegisteredPkScriptStream(ht, bob)

	return alice, bob, stream
}

// testPkScriptNotifierFutureRegistrationRPC ensures a newly registered
// stream can receive future confirmations and spends for multiple scripts.
func testPkScriptNotifierFutureRegistrationRPC(ht *lntest.HarnessTest) {
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNode("Bob", nil)

	addrA, scriptA, keyA := newPkScriptTestAddress(ht)
	addrB, scriptB, keyB := newPkScriptTestAddress(ht)

	stream := newRegisteredPkScriptStream(ht, bob)
	defer stream.close()

	stream.send(pkScriptAddReq(
		[][]byte{scriptA, scriptB}, []crpc.PkScriptEventType{
			confirmEvent,
			spendEvent,
		}, 2, true, true,
	))
	addAckAB := stream.requireAck(
		crpc.PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_ADD,
	)
	require.Equal(ht, uint32(2), addAckAB.NumAdded)

	txidA, outpointA, outputA, _ := fundAddress(
		ht, alice, addrA, scriptA, 1_000_111,
	)
	txidB, outpointB, outputB, _ := fundAddress(
		ht, alice, addrB, scriptB, 1_000_222,
	)

	ht.MineBlocksAndAssertNumTxes(1, 2)
	confirmBlockAB := ht.MineEmptyBlocks(1)[0]
	confirmHeightAB := ht.CurrentHeight()
	confirmBlockHashAB := confirmBlockAB.BlockHash()

	spendTxA := createSpendTx(ht, outpointA, outputA, keyA)
	spendHashA, err := ht.SendRawTransaction(spendTxA, true)
	require.NoError(ht, err)
	ht.AssertTxInMempool(spendHashA)

	spendTxB := createSpendTx(ht, outpointB, outputB, keyB)
	spendHashB, err := ht.SendRawTransaction(spendTxB, true)
	require.NoError(ht, err)
	ht.AssertTxInMempool(spendHashB)

	spendBlockAB := ht.MineBlocksAndAssertNumTxes(1, 2)[0]
	spendHeightAB := ht.CurrentHeight()
	spendBlockHashAB := spendBlockAB.BlockHash()

	regMap := pkScriptNotificationMap(ht, stream.requireNotifications(4))

	assertPkScriptNotification(
		ht,
		regMap[fmt.Sprintf("%s:%d", wireOutpointKey(outpointA),
			confirmEvent)],
		confirmEvent, false,
		confirmHeightAB, &confirmBlockHashAB, txidA, outpointA, 2, true,
	)
	assertPkScriptNotification(
		ht,
		regMap[fmt.Sprintf("%s:%d", wireOutpointKey(outpointB),
			confirmEvent)],
		confirmEvent, false,
		confirmHeightAB, &confirmBlockHashAB, txidB, outpointB, 2, true,
	)
	assertPkScriptNotification(
		ht,
		regMap[fmt.Sprintf("%s:%d", wireOutpointKey(outpointA),
			spendEvent)],
		spendEvent, false,
		spendHeightAB, &spendBlockHashAB, &spendHashA, outpointA, 0,
		true,
	)
	assertPkScriptNotification(
		ht,
		regMap[fmt.Sprintf("%s:%d", wireOutpointKey(outpointB),
			spendEvent)],
		spendEvent, false,
		spendHeightAB, &spendBlockHashAB, &spendHashB, outpointB, 0,
		true,
	)
}

// testPkScriptNotifierFutureAddRPC ensures a registered stream can add multiple
// scripts later and receive future notifications for all of them.
func testPkScriptNotifierFutureAddRPC(ht *lntest.HarnessTest) {
	alice, _, stream := newPkScriptNotifierRPCScenario(ht)
	defer stream.close()

	addrC, scriptC, keyC := newPkScriptTestAddress(ht)
	addrD, scriptD, _ := newPkScriptTestAddress(ht)

	stream.send(pkScriptAddReq(
		[][]byte{scriptC, scriptD}, nil, 2, true, true,
	))
	addAckCD := stream.requireAck(
		crpc.PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_ADD,
	)
	require.Equal(ht, uint32(2), addAckCD.NumAdded)

	txidC, outpointC, outputC, _ := fundAddress(
		ht, alice, addrC, scriptC, 1_000_333,
	)
	txidD, outpointD, _, _ := fundAddress(
		ht, alice, addrD, scriptD, 1_000_444,
	)

	ht.MineBlocksAndAssertNumTxes(1, 2)
	confirmBlockCD := ht.MineEmptyBlocks(1)[0]
	confirmHeightCD := ht.CurrentHeight()
	confirmBlockHashCD := confirmBlockCD.BlockHash()

	spendTxC := createSpendTx(ht, outpointC, outputC, keyC)
	spendHashC, err := ht.SendRawTransaction(spendTxC, true)
	require.NoError(ht, err)
	ht.AssertTxInMempool(spendHashC)

	// D is left unspent so this covers multiple confirms and one spend
	// in the same add request.
	spendBlockC := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	spendHeightC := ht.CurrentHeight()
	spendBlockHashC := spendBlockC.BlockHash()

	addMap := pkScriptNotificationMap(ht, stream.requireNotifications(3))

	assertPkScriptNotification(
		ht,
		addMap[fmt.Sprintf("%s:%d", wireOutpointKey(outpointC),
			confirmEvent)],
		confirmEvent, false,
		confirmHeightCD, &confirmBlockHashCD, txidC, outpointC, 2, true,
	)
	assertPkScriptNotification(
		ht,
		addMap[fmt.Sprintf("%s:%d", wireOutpointKey(outpointD),
			confirmEvent)],
		confirmEvent, false,
		confirmHeightCD, &confirmBlockHashCD, txidD, outpointD, 2, true,
	)
	assertPkScriptNotification(
		ht,
		addMap[fmt.Sprintf("%s:%d", wireOutpointKey(outpointC),
			spendEvent)],
		spendEvent, false,
		spendHeightC, &spendBlockHashC, &spendHashC, outpointC, 0, true,
	)
}

// testPkScriptNotifierRemoveRPC ensures removing a watched script stops later
// spend notifications for outputs that were already tracked.
func testPkScriptNotifierRemoveRPC(ht *lntest.HarnessTest) {
	alice, _, stream := newPkScriptNotifierRPCScenario(ht)
	defer stream.close()

	addrE, scriptE, keyE := newPkScriptTestAddress(ht)

	stream.send(pkScriptAddReq(
		[][]byte{scriptE}, nil, 2, true, true,
	))
	addAck := stream.requireAck(
		crpc.PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_ADD,
	)
	require.Equal(ht, uint32(1), addAck.NumAdded)

	txidE, outpointE, outputE, _ := fundAddress(
		ht, alice, addrE, scriptE, 1_000_555,
	)
	ht.MineBlocksAndAssertNumTxes(1, 1)
	confirmBlockE := ht.MineEmptyBlocks(1)[0]
	confirmHeightE := ht.CurrentHeight()
	confirmBlockHashE := confirmBlockE.BlockHash()

	assertPkScriptNotification(
		ht, stream.requireNotification(),
		confirmEvent, false,
		confirmHeightE, &confirmBlockHashE, txidE, outpointE, 2, true,
	)

	stream.send(pkScriptRemoveReq([][]byte{scriptE}))
	stream.requireAck(
		removeAct,
	)

	spendTxE := createSpendTx(ht, outpointE, outputE, keyE)
	_, err := ht.SendRawTransaction(spendTxE, true)
	require.NoError(ht, err)
	ht.MineBlocksAndAssertNumTxes(1, 1)
	stream.assertNoNotification(2 * time.Second)
}

// testPkScriptNotifierConfirmOnlyRPC ensures confirm-only subscriptions
// omit raw payloads and do not receive spend notifications.
func testPkScriptNotifierConfirmOnlyRPC(ht *lntest.HarnessTest) {
	alice, _, confirmOnlyStream := newPkScriptNotifierRPCScenario(ht)
	defer confirmOnlyStream.close()

	addrG, scriptG, keyG := newPkScriptTestAddress(ht)
	confirmOnlyStream.send(pkScriptAddReq(
		[][]byte{scriptG}, []crpc.PkScriptEventType{
			confirmEvent,
		}, 1, false, false,
	))
	confirmOnlyStream.requireAck(
		crpc.PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_ADD,
	)

	txidG, outpointG, outputG, _ := fundAddress(
		ht, alice, addrG, scriptG, 1_000_777,
	)
	fundingBlockG := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	fundingHeightG := ht.CurrentHeight()
	fundingBlockHashG := fundingBlockG.BlockHash()

	assertPkScriptNotification(
		ht, confirmOnlyStream.requireNotification(),
		confirmEvent, false,
		fundingHeightG, &fundingBlockHashG, txidG, outpointG, 1, false,
		withPkScriptUTXO(outputG, fundingHeightG, &fundingBlockHashG),
		withPkScriptTxIndex(1), withPkScriptUTXOTxIndex(1),
		withPkScriptInputIndex(0),
	)

	spendTxG := createSpendTx(ht, outpointG, outputG, keyG)
	_, err := ht.SendRawTransaction(spendTxG, true)
	require.NoError(ht, err)
	ht.MineBlocksAndAssertNumTxes(1, 1)
	confirmOnlyStream.assertNoNotification(2 * time.Second)
}

// testPkScriptNotifierPartialConfirmationRPC ensures partial confirmation
// updates are delivered before the final confirmation when requested.
func testPkScriptNotifierPartialConfirmationRPC(ht *lntest.HarnessTest) {
	alice, _, updatesStream := newPkScriptNotifierRPCScenario(ht)
	defer updatesStream.close()

	addrUpdates, scriptUpdates, _ := newPkScriptTestAddress(ht)
	updatesStream.send(pkScriptAddReqWithConfUpdates(
		[][]byte{scriptUpdates}, []crpc.PkScriptEventType{
			confirmEvent,
		}, 3, false, false, true,
	))
	updatesAck := updatesStream.requireAck(
		crpc.PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_ADD,
	)
	require.Equal(ht, uint32(1), updatesAck.NumAdded)

	txidUpdates, outpointUpdates, outputUpdates, _ := fundAddress(
		ht, alice, addrUpdates, scriptUpdates, 1_000_800,
	)
	fundingBlockUpdates := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	fundingHeightUpdates := ht.CurrentHeight()
	fundingBlockHashUpdates := fundingBlockUpdates.BlockHash()

	updateOne := updatesStream.requireNotification()
	assertPkScriptNotification(
		ht, updateOne,
		confUpd,
		false, fundingHeightUpdates, &fundingBlockHashUpdates,
		txidUpdates, outpointUpdates, 1, false,
		withPkScriptUTXO(
			outputUpdates, fundingHeightUpdates,
			&fundingBlockHashUpdates,
		),
		withPkScriptTxIndex(1), withPkScriptUTXOTxIndex(1),
	)
	require.Equal(ht, uint32(3), updateOne.RequiredConfirmations)

	updateBlockTwo := ht.MineEmptyBlocks(1)[0]
	updateHeightTwo := ht.CurrentHeight()
	updateBlockHashTwo := updateBlockTwo.BlockHash()

	updateTwo := updatesStream.requireNotification()
	assertPkScriptNotification(
		ht, updateTwo,
		confUpd,
		false, updateHeightTwo, &updateBlockHashTwo, txidUpdates,
		outpointUpdates, 2, false,
		withPkScriptUTXO(
			outputUpdates, fundingHeightUpdates,
			&fundingBlockHashUpdates,
		),
		withPkScriptTxIndex(1), withPkScriptUTXOTxIndex(1),
	)
	require.Equal(ht, uint32(3), updateTwo.RequiredConfirmations)

	confirmBlockUpdates := ht.MineEmptyBlocks(1)[0]
	confirmHeightUpdates := ht.CurrentHeight()
	confirmBlockHashUpdates := confirmBlockUpdates.BlockHash()

	finalUpdateConfirm := updatesStream.requireNotification()
	assertPkScriptNotification(
		ht, finalUpdateConfirm,
		confirmEvent, false,
		confirmHeightUpdates, &confirmBlockHashUpdates, txidUpdates,
		outpointUpdates, 3, false,
		withPkScriptUTXO(
			outputUpdates, fundingHeightUpdates,
			&fundingBlockHashUpdates,
		),
		withPkScriptTxIndex(1), withPkScriptUTXOTxIndex(1),
	)
	require.Equal(
		ht, uint32(3), finalUpdateConfirm.RequiredConfirmations,
	)
}

// testPkScriptNotifierMultiStreamRPC ensures multiple streams can watch the
// same script with different event/raw-payload options, and re-adds are
// no-ops.
func testPkScriptNotifierMultiStreamRPC(ht *lntest.HarnessTest) {
	alice, bob, stream := newPkScriptNotifierRPCScenario(ht)
	defer stream.close()

	addrH, scriptH, keyH := newPkScriptTestAddress(ht)

	stream.send(pkScriptAddReq(
		[][]byte{scriptH}, nil, 2, true, true,
	))
	firstAck := stream.requireAck(
		crpc.PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_ADD,
	)
	require.Equal(ht, uint32(1), firstAck.NumAdded)

	stream.send(pkScriptAddReq(
		[][]byte{scriptH}, nil, 2, true, true,
	))
	reAddAck := stream.requireAck(
		crpc.PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_ADD,
	)
	require.Zero(ht, reAddAck.NumAdded)

	confirmOnlyStream := newRegisteredPkScriptStream(ht, bob)
	defer confirmOnlyStream.close()

	confirmOnlyStream.send(pkScriptAddReq(
		[][]byte{scriptH}, []crpc.PkScriptEventType{
			confirmEvent,
		}, 1, false, false,
	))
	confirmOnlyStream.requireAck(
		crpc.PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_ADD,
	)

	spendOnlyStream := newRegisteredPkScriptStream(ht, bob)
	defer spendOnlyStream.close()

	spendOnlyStream.send(pkScriptAddReq(
		[][]byte{scriptH}, []crpc.PkScriptEventType{
			spendEvent,
		}, 0, false, false,
	))
	spendOnlyStream.requireAck(
		crpc.PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_ADD,
	)

	txidH, outpointH, outputH, _ := fundAddress(
		ht, alice, addrH, scriptH, 1_000_888,
	)
	fundingBlockH := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	fundingHeightH := ht.CurrentHeight()
	fundingBlockHashH := fundingBlockH.BlockHash()
	confirmBlockH := ht.MineEmptyBlocks(1)[0]
	confirmHeightH := ht.CurrentHeight()
	confirmBlockHashH := confirmBlockH.BlockHash()

	assertPkScriptNotification(
		ht, confirmOnlyStream.requireNotification(),
		confirmEvent, false,
		fundingHeightH, &fundingBlockHashH, txidH, outpointH, 1, false,
		withPkScriptUTXO(outputH, fundingHeightH, &fundingBlockHashH),
		withPkScriptTxIndex(1), withPkScriptUTXOTxIndex(1),
		withPkScriptInputIndex(0),
	)
	assertPkScriptNotification(
		ht, stream.requireNotification(),
		confirmEvent, false,
		confirmHeightH, &confirmBlockHashH, txidH, outpointH, 2, true,
		withPkScriptUTXO(outputH, fundingHeightH, &fundingBlockHashH),
		withPkScriptTxIndex(1), withPkScriptUTXOTxIndex(1),
		withPkScriptInputIndex(0),
	)
	spendOnlyStream.assertNoNotification(2 * time.Second)

	spendTxH := createSpendTx(ht, outpointH, outputH, keyH)
	spendHashH, err := ht.SendRawTransaction(spendTxH, true)
	require.NoError(ht, err)
	ht.AssertTxInMempool(spendHashH)

	spendBlockH := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	spendHeightH := ht.CurrentHeight()
	spendBlockHashH := spendBlockH.BlockHash()

	assertPkScriptNotification(
		ht, stream.requireNotification(),
		spendEvent, false,
		spendHeightH, &spendBlockHashH, &spendHashH, outpointH, 0, true,
		withPkScriptUTXO(outputH, fundingHeightH, &fundingBlockHashH),
		withPkScriptTxIndex(1), withPkScriptUTXOTxIndex(1),
		withPkScriptInputIndex(0),
	)
	assertPkScriptNotification(
		ht, spendOnlyStream.requireNotification(),
		spendEvent, false,
		spendHeightH, &spendBlockHashH, &spendHashH, outpointH, 0,
		false,
		withPkScriptUTXO(outputH, fundingHeightH, &fundingBlockHashH),
		withPkScriptTxIndex(1), withPkScriptUTXOTxIndex(1),
		withPkScriptInputIndex(0),
	)
	confirmOnlyStream.assertNoNotification(2 * time.Second)
}

// testPkScriptNotifierFutureOnlyRPC ensures newly added watches ignore old
// outputs and spends while still tracking future receives and spends.
func testPkScriptNotifierFutureOnlyRPC(ht *lntest.HarnessTest) {
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNode("Bob", nil)

	addrI, scriptI, keyI := newPkScriptTestAddress(ht)
	_, oldOutpointI, oldOutputI, _ := fundAddress(
		ht, alice, addrI, scriptI, 1_000_999,
	)
	ht.MineBlocksAndAssertNumTxes(1, 1)

	skipHistoryStream := newRegisteredPkScriptStream(ht, bob)
	defer skipHistoryStream.close()

	skipHistoryStream.send(pkScriptAddReq(
		[][]byte{scriptI}, []crpc.PkScriptEventType{
			confirmEvent,
			spendEvent,
		}, 1, false, false,
	))
	skipHistoryStream.requireAck(
		crpc.PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_ADD,
	)
	skipHistoryStream.assertNoNotification(2 * time.Second)

	oldSpendTxI := createSpendTx(ht, oldOutpointI, oldOutputI, keyI)
	oldSpendHashI, err := ht.SendRawTransaction(oldSpendTxI, true)
	require.NoError(ht, err)
	ht.AssertTxInMempool(oldSpendHashI)
	ht.MineBlocksAndAssertNumTxes(1, 1)
	skipHistoryStream.assertNoNotification(2 * time.Second)

	txidI, outpointI, outputI, _ := fundAddress(
		ht, alice, addrI, scriptI, 1_001_111,
	)
	fundingBlockI := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	fundingHeightI := ht.CurrentHeight()
	fundingBlockHashI := fundingBlockI.BlockHash()

	assertPkScriptNotification(
		ht, skipHistoryStream.requireNotification(),
		confirmEvent, false,
		fundingHeightI, &fundingBlockHashI, txidI, outpointI, 1, false,
		withPkScriptUTXO(outputI, fundingHeightI, &fundingBlockHashI),
		withPkScriptTxIndex(1), withPkScriptUTXOTxIndex(1),
		withPkScriptInputIndex(0),
	)

	spendTxI := createSpendTx(ht, outpointI, outputI, keyI)
	spendHashI, err := ht.SendRawTransaction(spendTxI, true)
	require.NoError(ht, err)
	ht.AssertTxInMempool(spendHashI)

	spendBlockI := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	spendHeightI := ht.CurrentHeight()
	spendBlockHashI := spendBlockI.BlockHash()

	assertPkScriptNotification(
		ht, skipHistoryStream.requireNotification(),
		spendEvent, false,
		spendHeightI, &spendBlockHashI, &spendHashI, outpointI, 0,
		false, withPkScriptUTXO(
			outputI, fundingHeightI, &fundingBlockHashI,
		),
		withPkScriptTxIndex(1), withPkScriptUTXOTxIndex(1),
		withPkScriptInputIndex(0),
	)

	addrJ, scriptJ, _ := newPkScriptTestAddress(ht)
	_, _, _, _ = fundAddress(ht, alice, addrJ, scriptJ, 1_001_222)
	ht.MineBlocksAndAssertNumTxes(1, 1)

	skipHistoryStream.send(pkScriptAddReq(
		[][]byte{scriptJ}, nil, 0, false, false,
	))
	skipHistoryStream.requireAck(
		crpc.PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_ADD,
	)
	skipHistoryStream.assertNoNotification(2 * time.Second)

	txidJ, outpointJ, outputJ, _ := fundAddress(
		ht, alice, addrJ, scriptJ, 1_001_333,
	)
	fundingBlockJ := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	fundingHeightJ := ht.CurrentHeight()
	fundingBlockHashJ := fundingBlockJ.BlockHash()

	assertPkScriptNotification(
		ht, skipHistoryStream.requireNotification(),
		confirmEvent, false,
		fundingHeightJ, &fundingBlockHashJ, txidJ, outpointJ, 1, false,
		withPkScriptUTXO(outputJ, fundingHeightJ, &fundingBlockHashJ),
		withPkScriptTxIndex(1), withPkScriptUTXOTxIndex(1),
		withPkScriptInputIndex(0),
	)
}

// testPkScriptNotifierValidationRPC ensures invalid request sequences and event
// options fail the stream with invalid argument errors.
func testPkScriptNotifierValidationRPC(ht *lntest.HarnessTest) {
	bob := ht.NewNode("Bob", nil)
	_, script, _ := newPkScriptTestAddress(ht)

	requirePkScriptStreamError(
		ht, bob, codes.InvalidArgument,
		pkScriptAddReq(
			[][]byte{script}, nil, 0, false, false,
		),
	)

	validRegister := pkScriptRegisterReq()
	requirePkScriptStreamError(
		ht, bob, codes.InvalidArgument, validRegister, validRegister,
	)

	requirePkScriptStreamError(
		ht, bob, codes.InvalidArgument,
		pkScriptRegisterReq(),
		pkScriptAddReq(
			[][]byte{script}, []crpc.PkScriptEventType{
				confUpd,
			}, 1, false, false,
		),
	)
}

// testPkScriptNotifierReorgRPC ensures confirmations and spends are invalidated
// and redelivered across a block reorg.
func testPkScriptNotifierReorgRPC(ht *lntest.HarnessTest) {
	alice, _, stream := newPkScriptNotifierRPCScenario(ht)
	defer stream.close()

	addrF, scriptF, keyF := newPkScriptTestAddress(ht)

	stream.send(pkScriptAddReq(
		[][]byte{scriptF}, nil, 2, true, true,
	))
	stream.requireAck(
		crpc.PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_ADD,
	)

	txidF, outpointF, outputF, _ := fundAddress(
		ht, alice, addrF, scriptF, 1_000_666,
	)
	ht.MineBlocksAndAssertNumTxes(1, 1)
	confirmBlockF := ht.MineEmptyBlocks(1)[0]
	confirmHeightF := ht.CurrentHeight()
	confirmBlockHashF := confirmBlockF.BlockHash()

	assertPkScriptNotification(
		ht, stream.requireNotification(),
		confirmEvent, false,
		confirmHeightF, &confirmBlockHashF, txidF, outpointF, 2, true,
	)

	spendTxF := createSpendTx(ht, outpointF, outputF, keyF)
	spendHashF, err := ht.SendRawTransaction(spendTxF, true)
	require.NoError(ht, err)
	ht.AssertTxInMempool(spendHashF)

	spendBlockF := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	spendHeightF := ht.CurrentHeight()
	spendBlockHashF := spendBlockF.BlockHash()

	assertPkScriptNotification(
		ht, stream.requireNotification(),
		spendEvent, false,
		spendHeightF, &spendBlockHashF, &spendHashF, outpointF, 0, true,
	)

	require.NoError(ht, ht.Miner().InvalidateBlock(&spendBlockHashF))
	require.NoError(ht, ht.Miner().InvalidateBlock(&confirmBlockHashF))

	// Mine a distinct first replacement block. MineEmptyBlocks uses a
	// deterministic empty block template for btcd, which can recreate the
	// invalidated empty confirmation block and be rejected as already
	// known.
	fillerAddr, _, _ := newPkScriptTestAddress(ht)
	fillerScript, err := txscript.PayToAddrScript(fillerAddr)
	require.NoError(ht, err)
	fillerTxID := ht.SendOutputsWithoutChange(
		[]*wire.TxOut{wire.NewTxOut(10_000, fillerScript)}, 10,
	)
	fillerTx := ht.AssertTxInMempool(*fillerTxID)

	newBlocks := []*wire.MsgBlock{ht.MineBlockWithTx(fillerTx)}
	newBlocks = append(newBlocks, ht.MineEmptyBlocks(1)...)
	reconfirmBlockHashF := newBlocks[0].BlockHash()

	reorgNtfns := stream.requireNotifications(3)
	var (
		seenSpendReorg   bool
		seenConfirmReorg bool
		seenReconfirm    bool
	)
	for _, ntfn := range reorgNtfns {
		switch {
		case ntfn.EventType ==
			spendEvent &&
			ntfn.Disconnected:

			assertPkScriptNotification(
				ht, ntfn,
				spendEvent,
				true, spendHeightF, &spendBlockHashF,
				&spendHashF,
				outpointF, 0, true,
			)
			seenSpendReorg = true

		case ntfn.EventType ==
			confirmEvent &&
			ntfn.Disconnected:

			assertPkScriptNotification(
				ht, ntfn,
				confirmEvent,
				true, confirmHeightF, &confirmBlockHashF, txidF,
				outpointF, 2, true,
			)
			seenConfirmReorg = true

		case ntfn.EventType ==
			confirmEvent &&
			!ntfn.Disconnected:

			assertPkScriptNotification(
				ht, ntfn,
				confirmEvent,
				false, confirmHeightF, &reconfirmBlockHashF,
				txidF,
				outpointF, 2, true,
			)
			seenReconfirm = true
		}
	}
	require.True(ht, seenSpendReorg)
	require.True(ht, seenConfirmReorg)
	require.True(ht, seenReconfirm)

	ht.AssertTxInMempool(spendTxF.TxHash())
	respendBlockF := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	respendHeightF := ht.CurrentHeight()
	respendBlockHashF := respendBlockF.BlockHash()

	assertPkScriptNotification(
		ht, stream.requireNotification(),
		spendEvent, false,
		respendHeightF, &respendBlockHashF, &spendHashF, outpointF, 0,
		true,
	)
}
