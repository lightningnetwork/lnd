package itest

import (
	"bytes"
	"encoding/hex"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/trampoline"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// testTrampolineRouting proves the complete native HTLC path:
//
//	Alice --outer payment--> Bob --peeled inner payment--> Carol --> Dave
func testTrampolineRouting(ht *lntest.HarnessTest) {
	const (
		channelAmount = btcutil.Amount(200_000)
		paymentAmount = btcutil.Amount(1_000)
	)
	trampolineArgs := []string{"--protocol.trampoline-routing"}

	alice := ht.NewNodeWithCoins("Alice", trampolineArgs)
	bob := ht.NewNodeWithCoins("Bob", trampolineArgs)
	carol := ht.NewNodeWithCoins("Carol", trampolineArgs)
	dave := ht.NewNode("Dave", trampolineArgs)

	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(bob, carol)
	ht.EnsureConnected(carol, dave)
	ht.OpenChannel(
		alice, bob,
		lntest.OpenChannelParams{Amt: channelAmount},
	)
	bobCarol := ht.OpenChannel(
		bob, carol,
		lntest.OpenChannelParams{Amt: channelAmount},
	)
	carolDave := ht.OpenChannel(
		carol, dave,
		lntest.OpenChannelParams{Amt: channelAmount},
	)
	ht.AssertChannelInGraph(alice, bobCarol)
	ht.AssertChannelInGraph(alice, carolDave)

	// First prove a single-relay BOLT 11 payment.
	_, hashes, invoices := ht.CreatePayReqs(
		carol, paymentAmount, 1,
	)
	paymentHash := hashes[0]
	invoice := invoices[0]

	currentHeight := ht.CurrentHeight()
	innerExpiry := currentHeight + 100
	innerOnion := buildTrampolineOnion(
		ht, paymentHash, []trampolineOnionHop{
			{
				node: mustPubKey(ht, bob.PubKeyStr),
				payload: relayPayload(
					ht, paymentAmount, innerExpiry,
					carol.PubKeyStr,
				),
			},
			{
				node: mustPubKey(ht, carol.PubKeyStr),
				payload: finalPayload(
					paymentAmount, innerExpiry,
					invoice.PaymentAddr,
				),
			},
		},
	)

	request := trampolineRequest(
		ht, bob.PubKeyStr, paymentAmount+2, paymentHash, 180,
		innerOnion,
	)
	payment := ht.SendPaymentAssertSettled(alice, request)
	require.Equal(ht, lnrpc.Payment_SUCCEEDED, payment.Status)
	require.Len(ht, payment.Htlcs, 1)
	routeHops := payment.Htlcs[0].Route.Hops
	finalHop := routeHops[len(routeHops)-1]
	require.NotEmpty(
		ht, finalHop.TrampolineOnion,
	)

	settled := ht.AssertNumInvoices(carol, 1)[0]
	require.True(ht, settled.Settled)
	require.EqualValues(ht, paymentAmount, settled.AmtPaidSat)

	// Exercise the structured RPC sender path, which derives the complete
	// inner onion from the invoice and the selected relay policy.
	payReqs, _, _ := ht.CreatePayReqs(carol, paymentAmount, 1)
	request = &routerrpc.SendPaymentRequest{
		PaymentRequest: payReqs[0],
		TimeoutSeconds: 60,
		FeeLimitMsat:   10_000,
		MaxParts:       1,
		TrampolineHops: []*routerrpc.TrampolineHop{{
			PubKey: mustPubKey(
				ht, bob.PubKeyStr,
			).SerializeCompressed(),
			FeeMsat:         1_001,
			CltvExpiryDelta: 80,
		}},
		TrampolinePaddingBytes: 47,
	}
	payment = ht.SendPaymentAssertSettled(alice, request)
	require.Equal(
		ht, int64(paymentAmount*1000)+1_001, payment.ValueMsat,
	)

	// Prove that a sender can apply the relay's fee and CLTV quote, then
	// retry the invoice with a fresh attempt-specific trampoline onion.
	payReqs, _, _ = ht.CreatePayReqs(carol, paymentAmount, 1)
	request = &routerrpc.SendPaymentRequest{
		PaymentRequest: payReqs[0],
		TimeoutSeconds: 60,
		FeeLimitMsat:   10_000,
		MaxParts:       1,
		TrampolineHops: []*routerrpc.TrampolineHop{{
			PubKey: mustPubKey(
				ht, bob.PubKeyStr,
			).SerializeCompressed(),
		}},
	}
	payment = ht.SendPaymentAndAssertStatus(
		alice, request, lnrpc.Payment_FAILED,
	)
	failure := lastFailure(ht, payment)
	require.Equal(
		ht, lnrpc.Failure_TRAMPOLINE_FEE_OR_EXPIRY_INSUFFICIENT,
		failure.Code,
	)
	require.EqualValues(ht, 1_000, failure.TrampolineFeeBaseMsat)
	require.EqualValues(ht, 80, failure.TrampolineCltvExpiryDelta)
	alice.RPC.ResetMissionControl()

	request.TrampolineHops[0].FeeMsat = 1_001
	request.TrampolineHops[0].CltvExpiryDelta = 80
	payment = ht.SendPaymentAssertSettled(alice, request)
	require.Equal(ht, lnrpc.Payment_SUCCEEDED, payment.Status)

	// Force two outer shards over the same channel and prove that the relay
	// waits for the complete declared total before making one downstream
	// payment.
	_, hashes, invoices = ht.CreatePayReqs(
		carol, paymentAmount, 1,
	)
	paymentHash = hashes[0]
	invoice = invoices[0]
	innerOnion = buildTrampolineOnion(
		ht, paymentHash, []trampolineOnionHop{
			{
				node: mustPubKey(ht, bob.PubKeyStr),
				payload: relayPayload(
					ht, paymentAmount, innerExpiry,
					carol.PubKeyStr,
				),
			},
			{
				node: mustPubKey(ht, carol.PubKeyStr),
				payload: finalPayload(
					paymentAmount, innerExpiry,
					invoice.PaymentAddr,
				),
			},
		},
	)
	request = trampolineRequest(
		ht, bob.PubKeyStr, paymentAmount+2, paymentHash, 180,
		innerOnion,
	)
	request.MaxParts = 2
	request.MaxShardSizeMsat = uint64((paymentAmount + 2) * 500)
	payment = ht.SendPaymentAssertSettled(alice, request)
	require.Len(ht, payment.Htlcs, 2)
	for _, htlc := range payment.Htlcs {
		require.Equal(ht, lnrpc.HTLCAttempt_SUCCEEDED, htlc.Status)
	}

	// Prove that one trampoline relay can hand the peeled packet to a
	// second trampoline relay before the final BOLT 11 recipient.
	_, hashes, invoices = ht.CreatePayReqs(dave, paymentAmount, 1)
	paymentHash = hashes[0]
	invoice = invoices[0]
	innerOnion = buildTrampolineOnion(
		ht, paymentHash, []trampolineOnionHop{
			{
				node: mustPubKey(ht, bob.PubKeyStr),
				payload: relayPayload(
					ht, paymentAmount+2, currentHeight+220,
					carol.PubKeyStr,
				),
			},
			{
				node: mustPubKey(ht, carol.PubKeyStr),
				payload: relayPayload(
					ht, paymentAmount, currentHeight+100,
					dave.PubKeyStr,
				),
			},
			{
				node: mustPubKey(ht, dave.PubKeyStr),
				payload: finalPayload(
					paymentAmount, currentHeight+100,
					invoice.PaymentAddr,
				),
			},
		},
	)
	request = trampolineRequest(
		ht, bob.PubKeyStr, paymentAmount+4, paymentHash, 320,
		innerOnion,
	)
	payment = ht.SendPaymentAssertSettled(alice, request)
	require.Equal(ht, lnrpc.Payment_SUCCEEDED, payment.Status)
	require.True(ht, ht.AssertNumInvoices(dave, 1)[0].Settled)

	// Pay a raw BOLT 12 invoice through Bob and a real blinded trampoline
	// path Carol -> Dave. Carol derives the next amount, expiry, node, and
	// path key exclusively from encrypted recipient data.
	_, hashes, invoices = ht.CreatePayReqs(dave, paymentAmount, 1)
	paymentHash = hashes[0]
	invoice = invoices[0]
	bolt12Invoice := buildBolt12TrampolineInvoice(
		ht, carol.PubKeyStr, dave, paymentHash,
		invoice.PaymentAddr,
		paymentAmount,
	)
	request = &routerrpc.SendPaymentRequest{
		Bolt12Invoice:  bolt12Invoice,
		TimeoutSeconds: 60,
		FeeLimitMsat:   10_000,
		MaxParts:       1,
		TrampolineHops: []*routerrpc.TrampolineHop{{
			PubKey: mustPubKey(
				ht, bob.PubKeyStr,
			).SerializeCompressed(),
			FeeMsat:         1_001,
			CltvExpiryDelta: 80,
		}},
		TrampolinePaddingBytes: 53,
	}
	payment = ht.SendPaymentAssertSettled(alice, request)
	require.Equal(ht, lnrpc.Payment_SUCCEEDED, payment.Status)
	require.Equal(
		ht, int64(paymentAmount*1000)+2_001, payment.ValueMsat,
	)
	require.True(ht, ht.AssertNumInvoices(dave, 2)[1].Settled)

	// An inner amount above the declared outer total must fail before any
	// downstream attempt and return the relay's bounded fee/CLTV quote.
	paymentHash = ht.Random32Bytes()
	overBudget := paymentAmount + 1
	innerOnion = buildTrampolineOnion(
		ht, paymentHash, []trampolineOnionHop{
			{
				node: mustPubKey(ht, bob.PubKeyStr),
				payload: relayPayload(
					ht, overBudget, innerExpiry,
					carol.PubKeyStr,
				),
			},
			{
				node: mustPubKey(ht, carol.PubKeyStr),
				payload: finalPayload(
					overBudget, innerExpiry,
					ht.Random32Bytes(),
				),
			},
		},
	)
	request = trampolineRequest(
		ht, bob.PubKeyStr, paymentAmount, paymentHash, 180,
		innerOnion,
	)
	payment = ht.SendPaymentAndAssertStatus(
		alice, request, lnrpc.Payment_FAILED,
	)
	failure = lastFailure(ht, payment)
	require.Equal(
		ht, lnrpc.Failure_TRAMPOLINE_FEE_OR_EXPIRY_INSUFFICIENT,
		failure.Code,
	)
	require.NotZero(ht, failure.TrampolineCltvExpiryDelta)
	alice.RPC.ResetMissionControl()

	// An inner expiry beyond the outer HTLC's budget is rejected with the
	// same quote-bearing failure.
	paymentHash = ht.Random32Bytes()
	innerOnion = buildTrampolineOnion(
		ht, paymentHash, []trampolineOnionHop{
			{
				node: mustPubKey(ht, bob.PubKeyStr),
				payload: relayPayload(
					ht, paymentAmount, currentHeight+400,
					carol.PubKeyStr,
				),
			},
			{
				node: mustPubKey(ht, carol.PubKeyStr),
				payload: finalPayload(
					paymentAmount, currentHeight+400,
					ht.Random32Bytes(),
				),
			},
		},
	)
	request = trampolineRequest(
		ht, bob.PubKeyStr, paymentAmount+2, paymentHash, 180,
		innerOnion,
	)
	payment = ht.SendPaymentAndAssertStatus(
		alice, request, lnrpc.Payment_FAILED,
	)
	failure = lastFailure(ht, payment)
	require.Equal(
		ht, lnrpc.Failure_TRAMPOLINE_FEE_OR_EXPIRY_INSUFFICIENT,
		failure.Code,
	)
	alice.RPC.ResetMissionControl()

	// A syntactically valid next trampoline that is absent from the graph
	// must map the terminal pathfinding result to unknown_next_trampoline.
	unknownKey, err := btcec.NewPrivateKey()
	require.NoError(ht, err)
	paymentHash = ht.Random32Bytes()
	forwardAmount := msatAmount(paymentAmount)
	innerOnion = buildTrampolineOnion(
		ht, paymentHash, []trampolineOnionHop{
			{
				node: mustPubKey(ht, bob.PubKeyStr),
				payload: &trampoline.Payload{
					AmountToForward: forwardAmount,
					OutgoingCLTV: uint32Value(
						innerExpiry,
					),
					OutgoingNodeID: unknownKey.PubKey(),
				},
			},
			{
				node: unknownKey.PubKey(),
				payload: finalPayload(
					paymentAmount, innerExpiry,
					ht.Random32Bytes(),
				),
			},
		},
	)
	request = trampolineRequest(
		ht, bob.PubKeyStr, paymentAmount+2, paymentHash, 180,
		innerOnion,
	)
	payment = ht.SendPaymentAndAssertStatus(
		alice, request, lnrpc.Payment_FAILED,
	)
	require.Equal(
		ht, lnrpc.Failure_UNKNOWN_NEXT_TRAMPOLINE,
		lastFailure(ht, payment).Code,
	)
	alice.RPC.ResetMissionControl()

	// A structurally valid embedded packet with a corrupted HMAC must fail
	// locally at the relay without reaching a downstream node.
	paymentHash = ht.Random32Bytes()
	innerOnion = buildTrampolineOnion(
		ht, paymentHash, []trampolineOnionHop{
			{
				node: mustPubKey(ht, bob.PubKeyStr),
				payload: relayPayload(
					ht, paymentAmount, innerExpiry,
					carol.PubKeyStr,
				),
			},
			{
				node: mustPubKey(ht, carol.PubKeyStr),
				payload: finalPayload(
					paymentAmount, innerExpiry,
					ht.Random32Bytes(),
				),
			},
		},
	)
	innerOnion.packet[len(innerOnion.packet)-1] ^= 1
	request = trampolineRequest(
		ht, bob.PubKeyStr, paymentAmount+2, paymentHash, 180,
		innerOnion,
	)
	payment = ht.SendPaymentAndAssertStatus(
		alice, request, lnrpc.Payment_FAILED,
	)
	require.Equal(
		ht, lnrpc.Failure_TEMPORARY_TRAMPOLINE_FAILURE,
		lastFailure(ht, payment).Code,
	)
	alice.RPC.ResetMissionControl()

	// Hold a payment at the final recipient, restart the first
	// trampoline relay while its downstream attempt is active, then cancel
	// the invoice.
	// The relay must reconcile the persisted attempt and return the final
	// recipient's nested error to the original sender exactly once.
	var holdPreimage lntypes.Preimage
	copy(holdPreimage[:], ht.Random32Bytes())
	holdHash := holdPreimage.Hash()
	holdInvoice := dave.RPC.AddHoldInvoice(
		&invoicesrpc.AddHoldInvoiceRequest{
			Memo:  "trampoline restart nested failure",
			Value: int64(paymentAmount),
			Hash:  holdHash[:],
		},
	)
	holdStream := dave.RPC.SubscribeSingleInvoice(holdHash[:])
	ht.AssertInvoiceState(holdStream, lnrpc.Invoice_OPEN)

	currentHeight = ht.CurrentHeight()
	innerOnion = buildTrampolineOnion(
		ht, holdHash[:], []trampolineOnionHop{
			{
				node: mustPubKey(ht, bob.PubKeyStr),
				payload: relayPayload(
					ht, paymentAmount+2, currentHeight+220,
					carol.PubKeyStr,
				),
			},
			{
				node: mustPubKey(ht, carol.PubKeyStr),
				payload: relayPayload(
					ht, paymentAmount, currentHeight+100,
					dave.PubKeyStr,
				),
			},
			{
				node: mustPubKey(ht, dave.PubKeyStr),
				payload: finalPayload(
					paymentAmount, currentHeight+100,
					holdInvoice.PaymentAddr,
				),
			},
		},
	)
	request = trampolineRequest(
		ht, bob.PubKeyStr, paymentAmount+4, holdHash[:], 320,
		innerOnion,
	)
	payment = ht.SendPaymentAndAssertStatus(
		alice, request, lnrpc.Payment_IN_FLIGHT,
	)
	require.Equal(ht, lnrpc.Payment_IN_FLIGHT, payment.Status)
	ht.AssertInvoiceState(holdStream, lnrpc.Invoice_ACCEPTED)

	ht.RestartNode(bob)
	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(bob, carol)
	dave.RPC.CancelInvoice(holdHash[:])
	ht.AssertInvoiceState(holdStream, lnrpc.Invoice_CANCELED)

	payment = ht.AssertPaymentStatus(
		alice, holdHash, lnrpc.Payment_FAILED,
	)
	require.Equal(
		ht, lnrpc.Failure_INCORRECT_OR_UNKNOWN_PAYMENT_DETAILS,
		lastFailure(ht, payment).Code,
	)
}

type trampolineOnionHop struct {
	node    *btcec.PublicKey
	payload *trampoline.Payload
}

type encodedTrampolineOnion struct {
	packet     []byte
	sessionKey []byte
	errorPath  [][]byte
}

func buildTrampolineOnion(ht *lntest.HarnessTest, paymentHash []byte,
	hops []trampolineOnionHop) encodedTrampolineOnion {

	require.LessOrEqual(ht, len(hops), sphinx.NumMaxHops)
	var path sphinx.PaymentPath
	for i, hop := range hops {
		hopPayload, err := sphinx.NewTLVHopPayload(
			encodeTrampolinePayload(ht, hop.payload),
		)
		require.NoError(ht, err)
		path[i] = sphinx.OnionHop{
			NodePub:    *hop.node,
			HopPayload: hopPayload,
		}
	}

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(ht, err)
	packet, err := sphinx.NewOnionPacket(
		&path, sessionKey, paymentHash,
		sphinx.DeterministicPacketFiller,
		sphinx.WithMaxPayloadSize(path.TotalPayloadSize()),
	)
	require.NoError(ht, err)

	encoded, err := trampoline.EncodeOnionPacket(packet)
	require.NoError(ht, err)

	errorPath := make([][]byte, len(hops))
	for i, hop := range hops {
		errorPath[i] = hop.node.SerializeCompressed()
	}

	return encodedTrampolineOnion{
		packet:     encoded,
		sessionKey: sessionKey.Serialize(),
		errorPath:  errorPath,
	}
}

func trampolineRequest(ht *lntest.HarnessTest, relayKey string,
	amount btcutil.Amount, paymentHash []byte, finalDelta int32,
	onion encodedTrampolineOnion) *routerrpc.SendPaymentRequest {

	return &routerrpc.SendPaymentRequest{
		Dest: mustPubKey(
			ht, relayKey,
		).SerializeCompressed(),
		Amt:                       int64(amount),
		PaymentHash:               paymentHash,
		FinalCltvDelta:            finalDelta,
		TimeoutSeconds:            60,
		FeeLimitMsat:              10_000,
		PaymentAddr:               ht.Random32Bytes(),
		MaxParts:                  1,
		TrampolineOnion:           onion.packet,
		TrampolineErrorSessionKey: onion.sessionKey,
		TrampolineErrorPath:       onion.errorPath,
		DestFeatures: []lnrpc.FeatureBit{
			lnrpc.FeatureBit_TLV_ONION_OPT,
			lnrpc.FeatureBit_PAYMENT_ADDR_OPT,
			lnrpc.FeatureBit_MPP_OPT,
			lnrpc.FeatureBit_TRAMPOLINE_ROUTING_OPT,
		},
	}
}

func relayPayload(ht *lntest.HarnessTest, amount btcutil.Amount,
	expiry uint32, nextKey string) *trampoline.Payload {

	return &trampoline.Payload{
		AmountToForward: msatAmount(amount),
		OutgoingCLTV:    uint32Value(expiry),
		OutgoingNodeID:  mustPubKey(ht, nextKey),
	}
}

func finalPayload(amount btcutil.Amount, expiry uint32,
	paymentAddr []byte) *trampoline.Payload {

	var addr [32]byte
	copy(addr[:], paymentAddr)
	msat := lnwire.NewMSatFromSatoshis(amount)

	return &trampoline.Payload{
		AmountToForward: msatAmount(amount),
		OutgoingCLTV:    uint32Value(expiry),
		PaymentData:     record.NewMPP(msat, addr),
	}
}

func msatAmount(amount btcutil.Amount) *uint64 {
	msat := uint64(lnwire.NewMSatFromSatoshis(amount))
	return &msat
}

func uint32Value(value uint32) *uint32 {
	return &value
}

func lastFailure(ht *lntest.HarnessTest,
	payment *lnrpc.Payment) *lnrpc.Failure {

	require.NotEmpty(ht, payment.Htlcs)
	failure := payment.Htlcs[len(payment.Htlcs)-1].Failure
	require.NotNil(ht, failure)

	return failure
}

func encodeTrampolinePayload(ht *lntest.HarnessTest,
	payload *trampoline.Payload) []byte {

	var encoded bytes.Buffer
	require.NoError(ht, payload.Encode(&encoded))

	return encoded.Bytes()
}

func buildBolt12TrampolineInvoice(ht *lntest.HarnessTest,
	introduction string, recipient *node.HarnessNode, paymentHash,
	paymentAddr []byte, amount btcutil.Amount) []byte {

	introKey := mustPubKey(ht, introduction)
	recipientKey := mustPubKey(ht, recipient.PubKeyStr)
	relayData, err := record.EncodeBlindedRouteData(
		record.NewDummyHopRouteData(
			recipientKey,
			record.PaymentRelayInfo{
				CltvExpiryDelta: 18,
				BaseFee:         1_000,
			},
			record.PaymentConstraints{
				MaxCltvExpiry:   ht.CurrentHeight() + 1_000,
				HtlcMinimumMsat: 1,
			},
		),
	)
	require.NoError(ht, err)
	finalData, err := record.EncodeBlindedRouteData(
		record.NewFinalHopBlindedRouteData(nil, paymentAddr),
	)
	require.NoError(ht, err)
	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(ht, err)
	blinded, err := sphinx.BuildBlindedPath(
		sessionKey, []*sphinx.HopInfo{
			{NodePub: introKey, PlainText: relayData},
			{NodePub: recipientKey, PlainText: finalData},
		},
	)
	require.NoError(ht, err)

	intro, err := lnwire.NewPubkeyIntro(introKey)
	require.NoError(ht, err)
	path := lnwire.BlindedPath{
		IntroductionNode: intro,
		BlindingPoint:    blinded.Path.BlindingPoint,
		Hops: make(
			[]lnwire.BlindedHop, len(blinded.Path.BlindedHops),
		),
	}
	for i := range blinded.Path.BlindedHops {
		hop := blinded.Path.BlindedHops[i]
		path.Hops[i] = lnwire.BlindedHop{
			BlindedNodeID: hop.BlindedNodePub,
			EncryptedData: hop.CipherText,
		}
	}

	var hash [32]byte
	copy(hash[:], paymentHash)
	var chain [32]byte
	copy(chain[:], harnessNetParams.GenesisHash[:])
	features := *lnwire.NewRawFeatureVector(
		lnwire.TLVOnionPayloadOptional,
		lnwire.TrampolineRoutingOptional,
	)
	invoice := &bolt12.Invoice{
		InvreqChain: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType80](chain),
		),
		InvoicePaths: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType160, lnwire.BlindedPaths](
				lnwire.BlindedPaths{
					Paths: []lnwire.BlindedPath{path},
				},
			),
		),
		InvoiceBlindedPay: tlv.SomeRecordT(
			tlv.NewRecordT[
				tlv.TlvType162, bolt12.BlindedPayInfos,
			](bolt12.BlindedPayInfos{
				Infos: []bolt12.BlindedPayInfo{{
					FeeBaseMsat:     1_000,
					CltvExpiryDelta: 98,
					HtlcMinimumMsat: 1,
					HtlcMaximumMsat: 10_000_000_000,
				}},
			}),
		),
		InvoiceCreatedAt: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType164](
				bolt12.TUint64(time.Now().Unix()),
			),
		),
		InvoicePaymentHash: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType168](hash),
		),
		InvoiceAmount: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType170](
				bolt12.TUint64(
					lnwire.NewMSatFromSatoshis(amount),
				),
			),
		),
		InvoiceFeatures: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType174](features),
		),
		InvoiceNodeID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType176](recipientKey),
		),
	}
	root, err := bolt12.InvoiceSigningRoot(invoice)
	require.NoError(ht, err)
	response := recipient.RPC.SignMessageSigner(&signrpc.SignMessageReq{
		Msg: root[:],
		KeyLoc: &signrpc.KeyLocator{
			KeyFamily: int32(keychain.KeyFamilyNodeKey),
			KeyIndex:  0,
		},
		SchnorrSig: true,
		Tag:        []byte(bolt12.InvoiceSignatureTag),
	})
	require.Len(ht, response.Signature, 64)
	var signature [64]byte
	copy(signature[:], response.Signature)
	invoice.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240](signature),
	)
	encoded, err := invoice.EncodeSigned()
	require.NoError(ht, err)

	return encoded
}

func mustPubKey(ht *lntest.HarnessTest, encoded string) *btcec.PublicKey {
	decoded, err := hex.DecodeString(encoded)
	require.NoError(ht, err)
	key, err := btcec.ParsePubKey(decoded)
	require.NoError(ht, err)

	return key
}
