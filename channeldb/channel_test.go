package channeldb

import (
	"bytes"
	"encoding/hex"
	"math/rand"
	"net"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntest/channels"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/stretchr/testify/require"
)

var (
	key = [chainhash.HashSize]byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x68, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
	}
	rev = [chainhash.HashSize]byte{
		0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0x2d, 0xe7, 0x93, 0xe4,
	}
	privKey, pubKey = btcec.PrivKeyFromBytes(key[:])

	testRBytes, _ = hex.DecodeString("8ce2bc69281ce27da07e6683571319d18e" +
		"949ddfa2965fb6caa1bf0314f882d7")
	testSBytes, _ = hex.DecodeString("299105481d63e0f4bc2a88121167221b67" +
		"00d72a0ead154c03be696a292d24ae")
	testRScalar = new(btcec.ModNScalar)
	testSScalar = new(btcec.ModNScalar)
	_           = testRScalar.SetByteSlice(testRBytes)
	_           = testSScalar.SetByteSlice(testSBytes)
	testSig     = ecdsa.NewSignature(testRScalar, testSScalar)

	wireSig, _ = lnwire.NewSigFromSignature(testSig)

	testPub = route.Vertex{2, 202, 4}

	testClock = clock.NewTestClock(testNow)

	// defaultPendingHeight is the default height at which we set
	// channels to pending.
	defaultPendingHeight = 100

	// defaultAddr is the default address that we mark test channels pending
	// with.
	defaultAddr = &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18555,
	}

	// dummyLocalOutputIndex specifics a default value for our output index
	// in this test.
	dummyLocalOutputIndex = uint32(0)

	// dummyRemoteOutIndex specifics a default value for their output index
	// in this test.
	dummyRemoteOutIndex = uint32(1)

	// uniqueOutputIndex is used to create a unique funding outpoint.
	//
	// NOTE: must be incremented when used.
	uniqueOutputIndex = atomic.Uint32{}
)

// testChannelParams is a struct which details the specifics of how a channel
// should be created.
type testChannelParams struct {
	// channel is the channel that will be written to disk.
	channel *OpenChannel

	// addr is the address that the channel will be synced pending with.
	addr *net.TCPAddr

	// pendingHeight is the height that the channel should be recorded as
	// pending.
	pendingHeight uint32

	// openChannel is set to true if the channel should be fully marked as
	// open if this is false, the channel will be left in pending state.
	openChannel bool

	// closedChannel is set to true if the channel should be marked as
	// closed after opening it.
	closedChannel bool
}

// testChannelOption is a functional option which can be used to alter the
// default channel that is creates for testing.
type testChannelOption func(params *testChannelParams)

// pendingHeightOption is an option which can be used to set the height the
// channel is marked as pending at.
func pendingHeightOption(height uint32) testChannelOption {
	return func(params *testChannelParams) {
		params.pendingHeight = height
	}
}

// openChannelOption is an option which can be used to create a test channel
// that is open.
func openChannelOption() testChannelOption {
	return func(params *testChannelParams) {
		params.openChannel = true
	}
}

// localHtlcsOption is an option which allows setting of htlcs on the local
// commitment.
func localHtlcsOption(htlcs []HTLC) testChannelOption {
	return func(params *testChannelParams) {
		params.channel.LocalCommitment.Htlcs = htlcs
	}
}

// remoteHtlcsOption is an option which allows setting of htlcs on the remote
// commitment.
func remoteHtlcsOption(htlcs []HTLC) testChannelOption {
	return func(params *testChannelParams) {
		params.channel.RemoteCommitment.Htlcs = htlcs
	}
}

// loadFwdPkgs is a helper method that reads all forwarding packages for a
// particular packager.
func loadFwdPkgs(t *testing.T, db kvdb.Backend,
	packager FwdPackager) []*FwdPkg {

	var (
		fwdPkgs []*FwdPkg
		err     error
	)

	err = kvdb.View(db, func(tx kvdb.RTx) error {
		fwdPkgs, err = packager.LoadFwdPkgs(tx)
		return err
	}, func() {})
	require.NoError(t, err, "unable to load fwd pkgs")

	return fwdPkgs
}

// localShutdownOption is an option which sets the local upfront shutdown
// script for the channel.
func localShutdownOption(addr lnwire.DeliveryAddress) testChannelOption {
	return func(params *testChannelParams) {
		params.channel.LocalShutdownScript = addr
	}
}

// remoteShutdownOption is an option which sets the remote upfront shutdown
// script for the channel.
func remoteShutdownOption(addr lnwire.DeliveryAddress) testChannelOption {
	return func(params *testChannelParams) {
		params.channel.RemoteShutdownScript = addr
	}
}

// fundingPointOption is an option which sets the funding outpoint of the
// channel.
func fundingPointOption(chanPoint wire.OutPoint) testChannelOption {
	return func(params *testChannelParams) {
		params.channel.FundingOutpoint = chanPoint
	}
}

// channelIDOption is an option which sets the short channel ID of the channel.
func channelIDOption(chanID lnwire.ShortChannelID) testChannelOption {
	return func(params *testChannelParams) {
		params.channel.ShortChannelID = chanID
	}
}

// createTestChannel writes a test channel to the database. It takes a set of
// functional options which can be used to overwrite the default of creating
// a pending channel that was broadcast at height 100.
func createTestChannel(t *testing.T, cdb *ChannelStateDB,
	opts ...testChannelOption) *OpenChannel {

	// Create a default set of parameters.
	params := &testChannelParams{
		channel:       createTestChannelState(t, cdb),
		addr:          defaultAddr,
		openChannel:   false,
		pendingHeight: uint32(defaultPendingHeight),
	}

	// Apply all functional options to the test channel params.
	for _, o := range opts {
		o(params)
	}

	// Mark the channel as pending.
	err := cdb.SyncPendingChannel(
		params.channel, params.addr, params.pendingHeight,
	)
	if err != nil {
		t.Fatalf("unable to save and serialize channel "+
			"state: %v", err)
	}

	// If the parameters do not specify that we should open the channel
	// fully, we return the pending channel.
	if !params.openChannel {
		return params.channel
	}

	// Mark the channel as open with the short channel id provided.
	err = params.channel.MarkAsOpen(params.channel.ShortChannelID)
	require.NoError(t, err, "unable to mark channel open")

	if params.closedChannel {
		// Set the other public keys so that serialization doesn't
		// panic.
		err = params.channel.CloseChannel(&ChannelCloseSummary{
			RemotePub:               params.channel.IdentityPub,
			RemoteCurrentRevocation: params.channel.IdentityPub,
			RemoteNextRevocation:    params.channel.IdentityPub,
		})
		require.NoError(t, err, "unable to close channel")
	}

	return params.channel
}

func createTestChannelState(t *testing.T, cdb *ChannelStateDB) *OpenChannel {
	// Simulate 1000 channel updates.
	producer, err := shachain.NewRevocationProducerFromBytes(key[:])
	require.NoError(t, err, "could not get producer")
	store := shachain.NewRevocationStore()
	for i := 0; i < 1; i++ {
		preImage, err := producer.AtIndex(uint64(i))
		if err != nil {
			t.Fatalf("could not get "+
				"preimage: %v", err)
		}

		if err := store.AddNextEntry(preImage); err != nil {
			t.Fatalf("could not add entry: %v", err)
		}
	}

	localStateBounds := ChannelStateBounds{
		MaxPendingAmount: lnwire.MilliSatoshi(rand.Int63()),
		ChanReserve:      btcutil.Amount(rand.Int63()),
		MinHTLC:          lnwire.MilliSatoshi(rand.Int63()),
		MaxAcceptedHtlcs: uint16(rand.Int31()),
	}

	localRenderingParams := CommitmentParams{
		DustLimit: btcutil.Amount(rand.Int63()),
		CsvDelay:  uint16(rand.Int31()),
	}

	localCfg := ChannelConfig{
		ChannelStateBounds: localStateBounds,
		CommitmentParams:   localRenderingParams,
		MultiSigKey: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
		},
		DelayBasePoint: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
		},
	}

	remoteStateBounds := ChannelStateBounds{
		MaxPendingAmount: lnwire.MilliSatoshi(rand.Int63()),
		ChanReserve:      btcutil.Amount(rand.Int63()),
		MinHTLC:          lnwire.MilliSatoshi(rand.Int63()),
		MaxAcceptedHtlcs: uint16(rand.Int31()),
	}

	remoteRenderingParams := CommitmentParams{
		DustLimit: btcutil.Amount(rand.Int63()),
		CsvDelay:  uint16(rand.Int31()),
	}

	remoteCfg := ChannelConfig{
		ChannelStateBounds: remoteStateBounds,
		CommitmentParams:   remoteRenderingParams,
		MultiSigKey: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyMultiSig,
				Index:  9,
			},
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyRevocationBase,
				Index:  8,
			},
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyPaymentBase,
				Index:  7,
			},
		},
		DelayBasePoint: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyDelayBase,
				Index:  6,
			},
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyHtlcBase,
				Index:  5,
			},
		},
	}

	chanID := lnwire.NewShortChanIDFromInt(uint64(rand.Int63()))

	// Increment the uniqueOutputIndex so we always get a unique value for
	// the funding outpoint.
	uniqueOutputIndex.Add(1)
	op := wire.OutPoint{Hash: key, Index: uniqueOutputIndex.Load()}

	var tapscriptRoot chainhash.Hash
	copy(tapscriptRoot[:], bytes.Repeat([]byte{1}, 32))

	return &OpenChannel{
		ChanType:          SingleFunderBit | FrozenBit,
		ChainHash:         key,
		FundingOutpoint:   op,
		ShortChannelID:    chanID,
		IsInitiator:       true,
		IsPending:         true,
		IdentityPub:       pubKey,
		Capacity:          btcutil.Amount(10000),
		LocalChanCfg:      localCfg,
		RemoteChanCfg:     remoteCfg,
		TotalMSatSent:     8,
		TotalMSatReceived: 2,
		LocalCommitment: ChannelCommitment{
			CommitHeight:  0,
			LocalBalance:  lnwire.MilliSatoshi(9000),
			RemoteBalance: lnwire.MilliSatoshi(3000),
			CommitFee:     btcutil.Amount(rand.Int63()),
			FeePerKw:      btcutil.Amount(5000),
			CommitTx:      channels.TestFundingTx,
			CommitSig:     bytes.Repeat([]byte{1}, 71),
			CustomBlob:    fn.Some([]byte{1, 2, 3}),
		},
		RemoteCommitment: ChannelCommitment{
			CommitHeight:  0,
			LocalBalance:  lnwire.MilliSatoshi(3000),
			RemoteBalance: lnwire.MilliSatoshi(9000),
			CommitFee:     btcutil.Amount(rand.Int63()),
			FeePerKw:      btcutil.Amount(5000),
			CommitTx:      channels.TestFundingTx,
			CommitSig:     bytes.Repeat([]byte{1}, 71),
			CustomBlob:    fn.Some([]byte{4, 5, 6}),
		},
		NumConfsRequired:        4,
		RemoteCurrentRevocation: privKey.PubKey(),
		RemoteNextRevocation:    privKey.PubKey(),
		RevocationProducer:      producer,
		RevocationStore:         store,
		Db:                      cdb.kvStore,
		FundingTxn:              channels.TestFundingTx,
		ThawHeight:              uint32(defaultPendingHeight),
		InitialLocalBalance:     lnwire.MilliSatoshi(9000),
		InitialRemoteBalance:    lnwire.MilliSatoshi(3000),
		Memo:                    []byte("test"),
		TapscriptRoot:           fn.Some(tapscriptRoot),
		CustomBlob:              fn.Some([]byte{1, 2, 3}),
	}
}
