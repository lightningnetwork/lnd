package itest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/devrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/onionmessage"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// testCreateOffer checks that CreateOffer returns a decodable offer issued
// by the node, with an offer hash that matches the offer.
func testCreateOffer(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", bolt12NodeArgs(ht))

	ctxt, cancel := context.WithTimeout(
		ht.Context(), lntest.DefaultTimeout,
	)
	defer cancel()

	resp, err := alice.RPC.Dev.CreateOffer(
		ctxt, &devrpc.CreateOfferRequest{
			Description: "integration test coffee",
			AmountMsat:  10000,
		},
	)
	require.NoError(ht, err, "CreateOffer")

	require.NotEmpty(ht, resp.Offer)

	decoded, err := bolt12.DecodeOfferString(
		resp.Offer, time.Now(), *harnessNetParams.GenesisHash,
	)
	require.NoError(ht, err, "decode offer string")

	issuerPub := decoded.OfferIssuerID.UnwrapOrFailV(ht.T)
	require.Equal(
		ht, alice.PubKey[:],
		issuerPub.SerializeCompressed(),
	)

	require.Len(ht, resp.OfferHash, 32)

	expectedID, err := bolt12.OfferHash(decoded)
	require.NoError(ht, err)
	require.Equal(ht, expectedID[:], resp.OfferHash)

	ht.Logf(
		"Created offer: %s (id=%s)", resp.Offer,
		hex.EncodeToString(resp.OfferHash),
	)

	// Second offer without amount.
	resp2 := alice.RPC.CreateOffer(
		&devrpc.CreateOfferRequest{
			Description: "tips welcome",
		},
	)
	require.NotEqual(ht, resp.OfferHash, resp2.OfferHash)

	decoded2, err := bolt12.DecodeOfferString(
		resp2.Offer, time.Now(), *harnessNetParams.GenesisHash,
	)
	require.NoError(ht, err)

	hasAmount := false
	decoded2.OfferAmount.WhenSome(
		func(_ tlv.RecordT[tlv.TlvType8, bolt12.TUint64]) {
			hasAmount = true
		},
	)
	require.False(ht, hasAmount)
}

// testBolt12InvoiceRequest tests that a node answers an invoice request for
// its offer with a signed BOLT 12 invoice.
func testBolt12InvoiceRequest(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", bolt12NodeArgs(ht))
	bob := ht.NewNode("Bob", bolt12NodeArgs(ht))

	ht.EnsureConnected(alice, bob)

	// Onion message ingress needs a fully open channel between the sender
	// and the receiver. This gate is the Sybil-resistance layer on top of
	// the byte-granular rate limiter. Open a channel before exchanging
	// invoice requests and invoice messages.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, bob)
	chanPoint := ht.OpenChannel(
		bob, alice, lntest.OpenChannelParams{Amt: 500_000},
	)
	defer ht.CloseChannel(bob, chanPoint)

	// Step 1: Alice creates an offer.
	ctxt, cancel := context.WithTimeout(
		ht.Context(), lntest.DefaultTimeout,
	)
	defer cancel()

	offerResp, err := alice.RPC.Dev.CreateOffer(
		ctxt, &devrpc.CreateOfferRequest{
			Description: "e2e test coffee",
			AmountMsat:  50000,
		},
	)
	require.NoError(ht, err, "CreateOffer")
	ht.Logf("Alice offer: %s", offerResp.Offer)

	// Step 2: Bob subscribes to onion messages for the reply.
	bobMessages := subscribeOnionMessages(ht, bob)

	// Step 3: Build a signed invoice request.
	ir, signedIRBytes, _ := buildInvoiceRequest(
		ht, offerResp.Offer, 50000,
	)

	// Step 4: Send the invoice request to Alice twice, and expect each
	// invoice on Bob's subscription. The first reply path starts at Bob.
	// The second one starts at Alice, as the reply path of a payer whose
	// only peer is Alice does, so Alice processes her own reply first.
	alicePubKey, err := btcec.ParsePubKey(alice.PubKey[:])
	require.NoError(ht, err)

	bobPubKey, err := btcec.ParsePubKey(bob.PubKey[:])
	require.NoError(ht, err)

	bobHop := &sphinx.HopInfo{
		NodePub: bobPubKey,
		PlainText: onionmessage.EncodeBlindedRouteData(
			ht.T, &record.BlindedRouteData{},
		),
	}
	aliceToBobHop := &sphinx.HopInfo{
		NodePub: alicePubKey,
		PlainText: onionmessage.EncodeBlindedRouteData(
			ht.T, record.NewNonFinalBlindedRouteDataOnionMessage(
				fn.NewLeft[*btcec.PublicKey,
					lnwire.ShortChannelID](bobPubKey),
				nil, nil,
			),
		),
	}

	sendInvoiceRequest(
		ht, alice, bob, signedIRBytes, []*sphinx.HopInfo{bobHop},
	)
	expectInvoiceReply(ht, bobMessages, ir, alicePubKey)

	sendInvoiceRequest(
		ht, alice, bob, signedIRBytes,
		[]*sphinx.HopInfo{aliceToBobHop, bobHop},
	)
	expectInvoiceReply(ht, bobMessages, ir, alicePubKey)
}

// testBolt12ReceiveSettle checks the receiver side of a BOLT 12 payment. The
// invoice uses a single-hop blinded payment path, and the payee keeps no
// invoice until the payment arrives. The invoice RPCs then report the
// settled BOLT 12 invoice.
func testBolt12ReceiveSettle(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", bolt12NodeArgs(ht))
	bob := ht.NewNode("Bob", bolt12NodeArgs(ht))

	ht.EnsureConnected(alice, bob)

	// Bob pays Alice, so he opens the channel and holds its balance.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, bob)
	chanPoint := ht.OpenChannel(
		bob, alice, lntest.OpenChannelParams{Amt: 500_000},
	)
	defer ht.CloseChannel(bob, chanPoint)

	const amtMsat = 50_000

	offerResp := alice.RPC.CreateOffer(&devrpc.CreateOfferRequest{
		Description: "settle test",
		AmountMsat:  amtMsat,
	})

	// Subscribe early, because the stream sends no backlog without an
	// add index.
	aliceInvoices := alice.RPC.SubscribeInvoices(
		&lnrpc.InvoiceSubscription{},
	)

	bobMessages := subscribeOnionMessages(ht, bob)
	ir, signedIRBytes, payerKey := buildInvoiceRequest(
		ht, offerResp.Offer, amtMsat,
	)

	bobPubKey, err := btcec.ParsePubKey(bob.PubKey[:])
	require.NoError(ht, err)
	alicePubKey, err := btcec.ParsePubKey(alice.PubKey[:])
	require.NoError(ht, err)

	bobHop := &sphinx.HopInfo{
		NodePub: bobPubKey,
		PlainText: onionmessage.EncodeBlindedRouteData(
			ht.T, &record.BlindedRouteData{},
		),
	}

	sendInvoiceRequest(
		ht, alice, bob, signedIRBytes, []*sphinx.HopInfo{bobHop},
	)
	invoice := expectInvoiceReply(ht, bobMessages, ir, alicePubKey)
	require.NotNil(ht, invoice)

	// Alice keeps no invoice until the payment arrives.
	invoices := alice.RPC.ListInvoices(&lnrpc.ListInvoiceRequest{})
	require.Empty(ht, invoices.Invoices)

	paths := invoice.InvoicePaths.ValOpt().UnwrapOr(
		lnwire.BlindedPaths{},
	).Paths
	payInfos := invoice.InvoiceBlindedPay.ValOpt().UnwrapOr(
		bolt12.BlindedPayInfos{},
	).Infos
	require.Len(ht, paths, 1, "invoice paths")
	require.Len(ht, payInfos, 1, "invoice blinded pay infos")

	// The path has a single hop, so Alice is its introduction node.
	path, payInfo := paths[0], payInfos[0]
	require.Equal(ht, alice.PubKey[:], path.IntroductionNode.Bytes())

	hops := make([]*lnrpc.BlindedHop, len(path.Hops))
	for i, hop := range path.Hops {
		hops[i] = &lnrpc.BlindedHop{
			BlindedNode:   hop.BlindedNodeID.SerializeCompressed(),
			EncryptedData: hop.EncryptedData,
		}
	}

	blindingPoint := path.BlindingPoint.SerializeCompressed()
	blindedPath := &lnrpc.BlindedPaymentPath{
		BlindedPath: &lnrpc.BlindedPath{
			IntroductionNode: path.IntroductionNode.Bytes(),
			BlindingPoint:    blindingPoint,
			BlindedHops:      hops,
		},
		BaseFeeMsat:         uint64(payInfo.FeeBaseMsat),
		ProportionalFeeRate: payInfo.FeeProportionalMillionths,
		TotalCltvDelta:      uint32(payInfo.CltvExpiryDelta),
		HtlcMinMsat:         payInfo.HtlcMinimumMsat,
		HtlcMaxMsat:         payInfo.HtlcMaximumMsat,
	}

	routes := bob.RPC.QueryRoutes(&lnrpc.QueryRoutesRequest{
		AmtMsat:             amtMsat,
		BlindedPaymentPaths: []*lnrpc.BlindedPaymentPath{blindedPath},
	})
	require.NotEmpty(ht, routes.Routes, "no route to the blinded path")

	hash, err := invoice.InvoicePaymentHash.UnwrapOrErrV(
		errors.New("invoice has no payment hash"),
	)
	require.NoError(ht, err)

	attempt := bob.RPC.SendToRouteV2(&routerrpc.SendToRouteRequest{
		PaymentHash: hash[:],
		Route:       routes.Routes[0],
	})
	require.Nil(ht, attempt.Failure)
	require.Equal(ht, lnrpc.HTLCAttempt_SUCCEEDED, attempt.Status)
	require.Equal(ht, hash, sha256.Sum256(attempt.Preimage))

	// Alice stored the invoice with its BOLT 12 detail when the HTLC
	// arrived. The invoice goes through PENDING_SETTLE, and the payer can
	// finish before Alice locks in the settle, so wait for the settled
	// state first.
	inv := alice.RPC.LookupInvoice(hash[:])
	ht.AssertInvoiceSettled(alice, inv.PaymentAddr)
	inv = alice.RPC.LookupInvoice(hash[:])
	require.Equal(ht, lnrpc.Invoice_SETTLED, inv.State)
	require.EqualValues(ht, amtMsat, inv.AmtPaidMsat)
	require.Empty(ht, inv.PaymentRequest)
	require.NotNil(ht, inv.Bolt12Detail)
	require.Equal(ht, offerResp.OfferHash, inv.Bolt12Detail.OfferHash)
	require.Equal(
		ht, payerKey.PubKey().SerializeCompressed(),
		inv.Bolt12Detail.InvreqPayerId,
	)

	invoices = alice.RPC.ListInvoices(&lnrpc.ListInvoiceRequest{})
	require.Len(ht, invoices.Invoices, 1)
	require.Equal(ht, hash[:], invoices.Invoices[0].RHash)
	require.NotNil(ht, invoices.Invoices[0].Bolt12Detail)

	// The subscriber gets the Open event when Alice keeps the HTLC, and
	// then the Settled event.
	opened := ht.ReceiveInvoiceUpdate(aliceInvoices)
	require.Equal(ht, hash[:], opened.RHash)
	require.Equal(ht, lnrpc.Invoice_OPEN, opened.State)
	require.NotNil(ht, opened.Bolt12Detail)

	settled := ht.ReceiveInvoiceUpdate(aliceInvoices)
	require.Equal(ht, hash[:], settled.RHash)
	require.Equal(ht, lnrpc.Invoice_SETTLED, settled.State)

	// A second payment over the same path carries the same envelope and
	// payment hash. Bob's own payment store refuses to pay the hash again,
	// so Carol replays it. Alice must refuse it, because the invoice is
	// settled.
	carol := ht.NewNode("Carol", bolt12NodeArgs(ht))
	ht.EnsureConnected(carol, alice)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)
	carolChan := ht.OpenChannel(
		carol, alice, lntest.OpenChannelParams{Amt: 500_000},
	)
	defer ht.CloseChannel(carol, carolChan)

	routes = carol.RPC.QueryRoutes(&lnrpc.QueryRoutesRequest{
		AmtMsat:             amtMsat,
		BlindedPaymentPaths: []*lnrpc.BlindedPaymentPath{blindedPath},
	})
	require.NotEmpty(ht, routes.Routes, "no route to the blinded path")

	attempt = carol.RPC.SendToRouteV2(&routerrpc.SendToRouteRequest{
		PaymentHash: hash[:],
		Route:       routes.Routes[0],
	})
	require.Equal(ht, lnrpc.HTLCAttempt_FAILED, attempt.Status)

	inv = alice.RPC.LookupInvoice(hash[:])
	require.EqualValues(ht, amtMsat, inv.AmtPaidMsat)
}

// subscribeOnionMessages subscribes hn to onion messages for the test. The
// subscription is cancelled when the test ends.
func subscribeOnionMessages(ht *lntest.HarnessTest,
	hn *node.HarnessNode) <-chan *lnrpc.OnionMessageUpdate {

	msgClient, msgCancel := hn.RPC.SubscribeOnionMessages()
	ht.Cleanup(msgCancel)

	messages := make(chan *lnrpc.OnionMessageUpdate, 1)
	go func() {
		for {
			msg, recvErr := msgClient.Recv()
			if recvErr != nil {
				return
			}

			select {
			case messages <- msg:
			case <-ht.Context().Done():
				return
			}
		}
	}()

	return messages
}

// buildInvoiceRequest builds and signs an invoice request for the offer with
// a fresh payer key.
func buildInvoiceRequest(ht *lntest.HarnessTest, offerStr string,
	amtMsat uint64) (*bolt12.InvoiceRequest, []byte, *btcec.PrivateKey) {

	offer, err := bolt12.DecodeOfferString(
		offerStr, time.Now(), *harnessNetParams.GenesisHash,
	)
	require.NoError(ht, err)

	payerKey, err := btcec.NewPrivateKey()
	require.NoError(ht, err)

	// The request copies the offer fields and sets invreq_chain, because
	// an absent invreq_chain means mainnet and the node runs on regtest.
	ir, err := bolt12.NewInvoiceRequestFromOffer(
		offer, payerKey.PubKey(), []byte("itest-payer-metadata"),
		*harnessNetParams.GenesisHash,
	)
	require.NoError(ht, err)

	ir.InvreqAmount = tlv.SomeRecordT(
		tlv.RecordT[tlv.TlvType82, bolt12.TUint64]{
			Val: bolt12.TUint64(amtMsat),
		},
	)

	// SignInvoiceRequest validates the writer rules before it signs.
	sig, err := bolt12.SignInvoiceRequest(ir, payerKey)
	require.NoError(ht, err)

	ir.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240, [64]byte](sig),
	)

	signedIRBytes, err := ir.EncodeSigned()
	require.NoError(ht, err)
	ht.Logf("Signed invreq: %d bytes", len(signedIRBytes))

	return ir, signedIRBytes, payerKey
}

// sendInvoiceRequest sends the signed invoice request from the payer to the
// receiver in an onion message. The reply path goes through replyHops.
func sendInvoiceRequest(ht *lntest.HarnessTest, receiver,
	payer *node.HarnessNode, signedIRBytes []byte,
	replyHops []*sphinx.HopInfo) {

	receiverPubKey, err := btcec.ParsePubKey(receiver.PubKey[:])
	require.NoError(ht, err)

	// The blinded path to the receiver has one hop, the receiver itself.
	receiverPath := onionmessage.BuildBlindedPath(ht.T, []*sphinx.HopInfo{
		{
			NodePub: receiverPubKey,
			PlainText: onionmessage.EncodeBlindedRouteData(
				ht.T, &record.BlindedRouteData{},
			),
		},
	})

	replyPathInfo := onionmessage.BuildBlindedPath(ht.T, replyHops)
	replyPath, err := lnwire.NewBlindedPathFromSphinx(replyPathInfo.Path)
	require.NoError(ht, err)

	finalHopTLVs := []*lnwire.FinalHopTLV{
		{
			TLVType: lnwire.InvoiceRequestNamespaceType,
			Value:   signedIRBytes,
		},
	}
	sphinxPath, err := route.OnionMessageBlindedPathToSphinxPath(
		receiverPath.Path, replyPath, finalHopTLVs,
	)
	require.NoError(ht, err)

	onionSessionKey, err := btcec.NewPrivateKey()
	require.NoError(ht, err)

	onionPkt, err := sphinx.NewOnionPacket(
		sphinxPath, onionSessionKey, nil,
		sphinx.DeterministicPacketFiller, sphinx.WithMaxPayloadSize(
			sphinx.MaxRoutingPayloadSize,
		),
	)
	require.NoError(ht, err)

	var buf bytes.Buffer
	require.NoError(ht, onionPkt.Encode(&buf))

	pathKey := receiverPath.SessionKey.PubKey().SerializeCompressed()
	payer.RPC.SendOnionMessage(
		&lnrpc.SendOnionMessageRequest{
			Peer:    receiver.PubKey[:],
			PathKey: pathKey,
			Onion:   buf.Bytes(),
		},
	)
	ht.Logf("Payer sent invoice request with a %d-hop reply path",
		len(replyHops))
}

// expectInvoiceReply waits for an invoice on the payer's onion message
// subscription and checks it as a payer does: against the request and
// against the receiver, whose offer carries its node ID as offer_issuer_id.
func expectInvoiceReply(ht *lntest.HarnessTest,
	messages <-chan *lnrpc.OnionMessageUpdate,
	ir *bolt12.InvoiceRequest,
	receiverPubKey *btcec.PublicKey) *bolt12.Invoice {

	select {
	case msg := <-messages:
		invoiceBytes, hasInvoice := msg.CustomRecords[uint64(
			lnwire.InvoiceNamespaceType,
		)]
		require.True(
			ht, hasInvoice,
			"reply should contain invoice (type 66)",
		)

		invoice, err := bolt12.DecodeInvoice(invoiceBytes)
		require.NoError(ht, err, "decode invoice")

		err = bolt12.ValidateInvoiceForPayment(
			invoice, ir, time.Now(), *harnessNetParams.GenesisHash,
			bolt12.InvoiceKnownFeatures{
				Invoice: bolt12.Bolt12Features,
				Blinded: bolt12.Bolt12Features,
			},
			receiverPubKey,
		)
		require.NoError(ht, err, "validate invoice")

		return invoice

	case <-time.After(lntest.DefaultTimeout):
		ht.Fatal("payer did not receive the invoice reply")
	}

	return nil
}

// bolt12NodeArgs returns the node arguments that enable BOLT 12 offers. It
// skips the test when this build or the database backend cannot run them.
// BOLT 12 needs native SQL and the development migrations. Only a build with
// a SQL test tag such as test_native_sql applies them.
func bolt12NodeArgs(ht *lntest.HarnessTest) []string {
	if !bolt12DevMigrations {
		ht.Skipf("BOLT 12 needs the development migrations: build " +
			"the itests with tags=test_native_sql")
	}

	if !ht.NativeSQLEnabled() {
		ht.Skipf("BOLT 12 needs --dbbackend=sqlite or postgres " +
			"with --nativesql")
	}

	return []string{"--protocol.bolt12-offers"}
}
