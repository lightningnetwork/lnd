package wtwire_test

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

var (
	testnetChainHash = *chaincfg.TestNet3Params.GenesisHash
	mainnetChainHash = *chaincfg.MainNetParams.GenesisHash
)

type checkRemoteInitTest struct {
	name      string
	lFeatures *lnwire.RawFeatureVector
	lHash     chainhash.Hash
	rFeatures *lnwire.RawFeatureVector
	rHash     chainhash.Hash
	expErr    error
}

var checkRemoteInitTests = []checkRemoteInitTest{
	{
		name:      "same chain, local-optional remote-required",
		lFeatures: lnwire.NewRawFeatureVector(wtwire.AltruistSessionsOptional),
		lHash:     testnetChainHash,
		rFeatures: lnwire.NewRawFeatureVector(wtwire.AltruistSessionsRequired),
		rHash:     testnetChainHash,
	},
	{
		name:      "same chain, local-required remote-optional",
		lFeatures: lnwire.NewRawFeatureVector(wtwire.AltruistSessionsRequired),
		lHash:     testnetChainHash,
		rFeatures: lnwire.NewRawFeatureVector(wtwire.AltruistSessionsOptional),
		rHash:     testnetChainHash,
	},
	{
		name:      "different chain, local-optional remote-required",
		lFeatures: lnwire.NewRawFeatureVector(wtwire.AltruistSessionsOptional),
		lHash:     testnetChainHash,
		rFeatures: lnwire.NewRawFeatureVector(wtwire.AltruistSessionsRequired),
		rHash:     mainnetChainHash,
		expErr:    wtwire.NewErrUnknownChainHash(mainnetChainHash),
	},
	{
		name:      "different chain, local-required remote-optional",
		lFeatures: lnwire.NewRawFeatureVector(wtwire.AltruistSessionsOptional),
		lHash:     testnetChainHash,
		rFeatures: lnwire.NewRawFeatureVector(wtwire.AltruistSessionsRequired),
		rHash:     mainnetChainHash,
		expErr:    wtwire.NewErrUnknownChainHash(mainnetChainHash),
	},
	{
		name:      "same chain, remote-unknown-required",
		lFeatures: lnwire.NewRawFeatureVector(wtwire.AltruistSessionsOptional),
		lHash:     testnetChainHash,
		rFeatures: lnwire.NewRawFeatureVector(lnwire.GossipQueriesRequired),
		rHash:     testnetChainHash,
		expErr: feature.NewErrUnknownRequired(
			[]lnwire.FeatureBit{lnwire.GossipQueriesRequired},
		),
	},
}

// TestCheckRemoteInit asserts the behavior of CheckRemoteInit when called with
// the remote party's Init message and the default wtwire.Features. We assert
// the validity of advertised features from the perspective of both client and
// server, as well as failure cases such as differing chain hashes or unknown
// required features.
func TestCheckRemoteInit(t *testing.T) {
	for _, test := range checkRemoteInitTests {
		t.Run(test.name, func(t *testing.T) {
			testCheckRemoteInit(t, test)
		})
	}
}

func testCheckRemoteInit(t *testing.T, test checkRemoteInitTest) {
	localInit := wtwire.NewInitMessage(test.lFeatures, test.lHash)
	remoteInit := wtwire.NewInitMessage(test.rFeatures, test.rHash)

	err := localInit.CheckRemoteInit(remoteInit, wtwire.FeatureNames)
	switch {

	// Both non-nil, pass.
	case err == nil && test.expErr == nil:
		return

	// One is nil and one is non-nil, fail.
	default:
		t.Fatalf("error mismatch, want: %v, got: %v", test.expErr, err)

	// Both non-nil, assert same error type.
	case err != nil && test.expErr != nil:
	}

	// Compare error strings to assert same type.
	if err.Error() != test.expErr.Error() {
		t.Fatalf("error mismatch, want: %v, got: %v",
			test.expErr.Error(), err.Error())
	}
}
