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

// testExperimentalEndorsement tests setting of positive and negative
// experimental endorsement signals.
func testExperimentalEndorsement(ht *lntest.HarnessTest) {
	testEndorsement(ht, true)
	testEndorsement(ht, false)
}

// testEndorsement sets up a 5 hop network and tests propagation of
// experimental endorsement signals.
func testEndorsement(ht *lntest.HarnessTest, aliceEndorse bool) {
	cfg := node.CfgAnchor
	carolCfg := append(
		[]string{"--protocol.no-experimental-endorsement"}, cfg...,
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

	expectedValue := []byte{lnwire.ExperimentalUnendorsed}
	if aliceEndorse {
		expectedValue = []byte{lnwire.ExperimentalEndorsed}
		t := uint64(lnwire.ExperimentalEndorsementType)
		sendReq.FirstHopCustomRecords = map[uint64][]byte{
			t: expectedValue,
		}
	}

	_ = alice.RPC.SendPayment(sendReq)

	// Validate that our signal (positive or zero) propagates until carol
	// and then is dropped because she has disabled the feature.
	validateEndorsedAndResume(ht, bobIntercept, true, expectedValue)
	validateEndorsedAndResume(ht, carolIntercept, true, expectedValue)
	validateEndorsedAndResume(ht, daveIntercept, false, nil)

	var preimage lntypes.Preimage
	copy(preimage[:], invoice.RPreimage)
	ht.AssertPaymentStatus(alice, preimage.Hash(), lnrpc.Payment_SUCCEEDED)
}

func validateEndorsedAndResume(ht *lntest.HarnessTest,
	interceptor rpc.InterceptorClient, hasEndorsement bool,
	expectedValue []byte) {

	packet := ht.ReceiveHtlcInterceptor(interceptor)

	var expectedRecords map[uint64][]byte
	if hasEndorsement {
		u64Type := uint64(lnwire.ExperimentalEndorsementType)
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
