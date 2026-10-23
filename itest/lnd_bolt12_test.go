package itest

import (
	"bytes"
	"context"
	"encoding/hex"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/devrpc"
	"github.com/lightningnetwork/lnd/lntest"
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

	offer, err := bolt12.DecodeOfferString(
		offerResp.Offer, time.Now(), *harnessNetParams.GenesisHash,
	)
	require.NoError(ht, err)

	// Step 2: Bob subscribes to onion messages for the reply.
	msgClient, msgCancel := bob.RPC.SubscribeOnionMessages()
	defer msgCancel()

	bobMessages := make(chan *lnrpc.OnionMessageUpdate, 1)
	go func() {
		for {
			msg, recvErr := msgClient.Recv()
			if recvErr != nil {
				return
			}

			select {
			case bobMessages <- msg:
			case <-ht.Context().Done():
				return
			}
		}
	}()

	// Step 3: Build a signed invoice request.
	payerKey, err := btcec.NewPrivateKey()
	require.NoError(ht, err)

	// The request copies the offer fields and sets invreq_chain, because
	// an absent invreq_chain means mainnet and the node runs on regtest.
	ir, err := bolt12.NewInvoiceRequestFromOffer(
		offer, payerKey.PubKey(), []byte("itest-payer-metadata"),
		*harnessNetParams.GenesisHash,
	)
	require.NoError(ht, err)

	amt := bolt12.TUint64(50000)
	ir.InvreqAmount = tlv.SomeRecordT(
		tlv.RecordT[tlv.TlvType82, bolt12.TUint64]{
			Val: amt,
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

	// Step 4: Build onion message to Alice with invoice request (type 64)
	// and a reply path back to Bob.
	alicePubKey, err := btcec.ParsePubKey(alice.PubKey[:])
	require.NoError(ht, err)

	bobPubKey, err := btcec.ParsePubKey(bob.PubKey[:])
	require.NoError(ht, err)

	// Blinded path to Alice (single hop: Alice is the final dest).
	aliceHops := []*sphinx.HopInfo{
		{
			NodePub: alicePubKey,
			PlainText: onionmessage.EncodeBlindedRouteData(
				ht.T, &record.BlindedRouteData{},
			),
		},
	}
	alicePath := onionmessage.BuildBlindedPath(ht.T, aliceHops)

	// Reply path to Bob (single hop: Bob is the final dest).
	bobHops := []*sphinx.HopInfo{
		{
			NodePub: bobPubKey,
			PlainText: onionmessage.EncodeBlindedRouteData(
				ht.T, &record.BlindedRouteData{},
			),
		},
	}
	bobReplyPathInfo := onionmessage.BuildBlindedPath(
		ht.T, bobHops,
	)

	// Build the onion with the reply path embedded in the final hop
	// payload.
	finalHopTLVs := []*lnwire.FinalHopTLV{
		{
			TLVType: lnwire.InvoiceRequestNamespaceType,
			Value:   signedIRBytes,
		},
	}

	replyPath, err := lnwire.NewBlindedPathFromSphinx(bobReplyPathInfo.Path)
	require.NoError(ht, err)

	sphinxPath, err := route.OnionMessageBlindedPathToSphinxPath(
		alicePath.Path,
		replyPath,
		finalHopTLVs,
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

	pathKey := alicePath.SessionKey.PubKey().
		SerializeCompressed()

	bob.RPC.SendOnionMessage(
		&lnrpc.SendOnionMessageRequest{
			Peer:    alice.PubKey[:],
			PathKey: pathKey,
			Onion:   buf.Bytes(),
		},
	)
	ht.Log("Bob sent invoice request to Alice")

	// Step 5: Wait for Alice's invoice reply on Bob's onion message
	// subscription.
	select {
	case msg := <-bobMessages:
		ht.Log("Bob received reply")

		// The reply should contain a type-66 (invoice) TLV.
		invoiceBytes, hasInvoice := msg.CustomRecords[uint64(
			lnwire.InvoiceNamespaceType,
		)]
		require.True(
			ht, hasInvoice,
			"reply should contain invoice (type 66)",
		)

		// Check the invoice as a payer does: against the request and
		// against Alice, whose offer carries her node ID as
		// offer_issuer_id.
		invoice, decErr := bolt12.DecodeInvoice(invoiceBytes)
		require.NoError(ht, decErr, "decode invoice")

		valErr := bolt12.ValidateInvoiceForPayment(
			invoice, ir, time.Now(), *harnessNetParams.GenesisHash,
			bolt12.InvoiceKnownFeatures{
				Invoice: bolt12.Bolt12Features,
				Blinded: bolt12.Bolt12Features,
			},
			alicePubKey,
		)
		require.NoError(ht, valErr, "validate invoice")

		ht.Log("Invoice reply verified successfully")

	case <-time.After(lntest.DefaultTimeout):
		ht.Fatal("Bob did not receive invoice reply")
	}
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
