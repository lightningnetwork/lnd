package itest

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/peersrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testCustomFeatures tests advertisement of custom features in various bolt 9
// sets. For completeness, it also asserts that features aren't set in places
// where they aren't intended to be.
func testCustomFeatures(ht *lntest.HarnessTest) {
	var (
		// Odd custom features so that we don't need to worry about
		// issues connecting to peers.
		customInit    uint32 = 101
		customNodeAnn uint32 = 103
		customInvoice uint32 = 105
	)

	// Restart Alice with custom feature bits configured.
	extraArgs := []string{
		fmt.Sprintf("--protocol.custom-init=%v", customInit),
		fmt.Sprintf("--protocol.custom-nodeann=%v", customNodeAnn),
		fmt.Sprintf("--protocol.custom-invoice=%v", customInvoice),
	}
	cfgs := [][]string{extraArgs, nil}

	_, nodes := ht.CreateSimpleNetwork(
		cfgs, lntest.OpenChannelParams{Amt: 1000000},
	)
	alice, bob := nodes[0], nodes[1]

	// Check that Alice's custom feature bit was sent to Bob in her init
	// message.
	peers := bob.RPC.ListPeers()
	require.Len(ht, peers.Peers, 1)
	require.Equal(ht, peers.Peers[0].PubKey, alice.PubKeyStr)

	_, customInitSet := peers.Peers[0].Features[customInit]
	require.True(ht, customInitSet)
	assertFeatureNotInSet(
		ht, []uint32{customNodeAnn, customInvoice},
		peers.Peers[0].Features,
	)

	// Assert that Alice's custom feature bit is contained in the node
	// announcement sent to Bob.
	updates := ht.AssertNumNodeAnns(bob, alice.PubKeyStr, 1)
	features := updates[len(updates)-1].Features
	_, customFeature := features[customNodeAnn]
	require.True(ht, customFeature)
	assertFeatureNotInSet(
		ht, []uint32{customInit, customInvoice}, features,
	)

	// Assert that Alice's custom feature bit is included in invoices.
	invoice := alice.RPC.AddInvoice(&lnrpc.Invoice{})
	payReq := alice.RPC.DecodePayReq(invoice.PaymentRequest)
	_, customInvoiceSet := payReq.Features[customInvoice]
	require.True(ht, customInvoiceSet)
	assertFeatureNotInSet(
		ht, []uint32{customInit, customNodeAnn}, payReq.Features,
	)

	// Check that Alice can't unset configured features via the node
	// announcement update API. This is only checked for node announcement
	// because it is the only set that can be updated via the API.
	nodeAnnReq := &peersrpc.NodeAnnouncementUpdateRequest{
		FeatureUpdates: []*peersrpc.UpdateFeatureAction{
			{
				Action:     peersrpc.UpdateAction_ADD,
				FeatureBit: lnrpc.FeatureBit(customNodeAnn),
			},
		},
	}
	alice.RPC.UpdateNodeAnnouncementErr(nodeAnnReq)
}

// assertFeatureNotInSet checks that the features provided aren't contained in
// a feature map.
func assertFeatureNotInSet(ht *lntest.HarnessTest, features []uint32,
	advertised map[uint32]*lnrpc.Feature) {

	for _, feature := range features {
		_, featureSet := advertised[feature]
		require.False(ht, featureSet)
	}
}
