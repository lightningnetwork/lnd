package itest

import (
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/rpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testExperimentalAccountability tests setting of positive and negative
// experimental accountable signals.
func testExperimentalAccountability(ht *lntest.HarnessTest) {
	testAccountability(ht, true)
	testAccountability(ht, false)
}

// testAccountability sets up a 5 hop network and tests propagation of
// experimental accountable signals.
func testAccountability(ht *lntest.HarnessTest, aliceAccountable bool) {
	cfg := node.CfgAnchor
	carolCfg := append(
		[]string{"--protocol.no-experimental-accountability"}, cfg...,
	)
	cfgs := [][]string{cfg, cfg, carolCfg, cfg, cfg}

	const chanAmt = btcutil.Amount(300000)
	p := lntest.OpenChannelParams{Amt: chanAmt}

	_, nodes := ht.CreateSimpleNetwork(cfgs, p)
	alice, bob, carol, dave, eve := nodes[0], nodes[1], nodes[2], nodes[3],
		nodes[4]

	bobIntercept, cancelBob := bob.RPC.HtlcInterceptor()
	defer cancelBob()

	carolIntercept, cancelCarol := carol.RPC.HtlcInterceptor()
	defer cancelCarol()

	daveIntercept, cancelDave := dave.RPC.HtlcInterceptor()
	defer cancelDave()

	req := &lnrpc.Invoice{ValueMsat: 1000}
	addResponse := eve.RPC.AddInvoice(req)
	invoice := eve.RPC.LookupInvoice(addResponse.RHash)

	sendReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoice.PaymentRequest,
		TimeoutSeconds: int32(wait.PaymentTimeout.Seconds()),
		FeeLimitMsat:   math.MaxInt64,
	}

	expectedValue := []byte{lnwire.ExperimentalUnaccountable}
	if aliceAccountable {
		expectedValue = []byte{lnwire.ExperimentalAccountable}
		t := uint64(lnwire.ExperimentalAccountableType)
		sendReq.FirstHopCustomRecords = map[uint64][]byte{
			t: expectedValue,
		}
	}

	_ = alice.RPC.SendPayment(sendReq)

	// Validate that our signal (positive or zero) propagates until carol
	// and then is dropped because she has disabled the feature.
	validateAccountableAndResume(
		ht, bobIntercept, true, expectedValue,
	)
	validateAccountableAndResume(
		ht, carolIntercept, true, expectedValue,
	)
	validateAccountableAndResume(ht, daveIntercept, false, nil)

	var preimage lntypes.Preimage
	copy(preimage[:], invoice.RPreimage)
	ht.AssertPaymentStatus(alice, preimage.Hash(), lnrpc.Payment_SUCCEEDED)
}

func validateAccountableAndResume(ht *lntest.HarnessTest,
	interceptor rpc.InterceptorClient, hasAccountable bool,
	expectedValue []byte) {

	packet := ht.ReceiveHtlcInterceptor(interceptor)

	var expectedRecords map[uint64][]byte
	if hasAccountable {
		u64Type := uint64(lnwire.ExperimentalAccountableType)
		expectedRecords = map[uint64][]byte{
			u64Type: expectedValue,
		}
	}
	require.Equal(ht, expectedRecords, packet.InWireCustomRecords)

	err := interceptor.Send(&routerrpc.ForwardHtlcInterceptResponse{
		IncomingCircuitKey: packet.IncomingCircuitKey,
		Action:             routerrpc.ResolveHoldForwardAction_RESUME,
	})
	require.NoError(ht, err)
}
