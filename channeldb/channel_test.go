package channeldb

import (
	"bytes"
	"encoding/hex"
	"math/rand"
	"net"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnmock"
	"github.com/lightningnetwork/lnd/lntest/channels"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/lightningnetwork/lnd/tlv"
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

	// keyLocIndex is the KeyLocator Index we use for
	// TestKeyLocatorEncoding.
	keyLocIndex = uint32(2049)

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

// closedChannelOption is an option which can be used to create a test channel
// that is closed.
func closedChannelOption() testChannelOption {
	return func(params *testChannelParams) {
		params.closedChannel = true
	}
}

// pubKeyOption is an option which can be used to set the remote's pubkey.
func pubKeyOption(pubKey *btcec.PublicKey) testChannelOption {
	return func(params *testChannelParams) {
		params.channel.IdentityPub = pubKey
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
	err := params.channel.SyncPending(params.addr, params.pendingHeight)
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
		Db:                      cdb,
		Packager:                NewChannelPackager(chanID),
		FundingTxn:              channels.TestFundingTx,
		ThawHeight:              uint32(defaultPendingHeight),
		InitialLocalBalance:     lnwire.MilliSatoshi(9000),
		InitialRemoteBalance:    lnwire.MilliSatoshi(3000),
		Memo:                    []byte("test"),
		TapscriptRoot:           fn.Some(tapscriptRoot),
		CustomBlob:              fn.Some([]byte{1, 2, 3}),
	}
}

func TestOpenChannelPutGetDelete(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// Create the test channel state, with additional htlcs on the local
	// and remote commitment.
	localHtlcs := []HTLC{
		{
			Signature:     testSig.Serialize(),
			Incoming:      true,
			Amt:           10,
			RHash:         key,
			RefundTimeout: 1,
			OnionBlob:     lnmock.MockOnion(),
		},
	}

	remoteHtlcs := []HTLC{
		{
			Signature:     testSig.Serialize(),
			Incoming:      false,
			Amt:           10,
			RHash:         key,
			RefundTimeout: 1,
			OnionBlob:     lnmock.MockOnion(),
		},
	}

	state := createTestChannel(
		t, cdb,
		remoteHtlcsOption(remoteHtlcs),
		localHtlcsOption(localHtlcs),
	)

	openChannels, err := cdb.FetchOpenChannels(state.IdentityPub)
	require.NoError(t, err, "unable to fetch open channel")

	newState := openChannels[0]

	// The decoded channel state should be identical to what we stored
	// above.
	if !reflect.DeepEqual(state, newState) {
		t.Fatalf("channel state doesn't match:: %v vs %v",
			spew.Sdump(state), spew.Sdump(newState))
	}

	// We'll also test that the channel is properly able to hot swap the
	// next revocation for the state machine. This tests the initial
	// post-funding revocation exchange.
	nextRevKey, err := btcec.NewPrivateKey()
	require.NoError(t, err, "unable to create new private key")
	if err := state.InsertNextRevocation(nextRevKey.PubKey()); err != nil {
		t.Fatalf("unable to update revocation: %v", err)
	}

	openChannels, err = cdb.FetchOpenChannels(state.IdentityPub)
	require.NoError(t, err, "unable to fetch open channel")
	updatedChan := openChannels[0]

	// Ensure that the revocation was set properly.
	if !nextRevKey.PubKey().IsEqual(updatedChan.RemoteNextRevocation) {
		t.Fatalf("next revocation wasn't updated")
	}

	// Finally to wrap up the test, delete the state of the channel within
	// the database. This involves "closing" the channel which removes all
	// written state, and creates a small "summary" elsewhere within the
	// database.
	closeSummary := &ChannelCloseSummary{
		ChanPoint:         state.FundingOutpoint,
		RemotePub:         state.IdentityPub,
		SettledBalance:    btcutil.Amount(500),
		TimeLockedBalance: btcutil.Amount(10000),
		IsPending:         false,
		CloseType:         CooperativeClose,
	}
	if err := state.CloseChannel(closeSummary); err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	// As the channel is now closed, attempting to fetch all open channels
	// for our fake node ID should return an empty slice.
	openChans, err := cdb.FetchOpenChannels(state.IdentityPub)
	require.NoError(t, err, "unable to fetch open channels")
	if len(openChans) != 0 {
		t.Fatalf("all channels not deleted, found %v", len(openChans))
	}

	// Additionally, attempting to fetch all the open channels globally
	// should yield no results.
	openChans, err = cdb.FetchAllChannels()
	if err != nil {
		t.Fatal("unable to fetch all open chans")
	}
	if len(openChans) != 0 {
		t.Fatalf("all channels not deleted, found %v", len(openChans))
	}
}

// TestOptionalShutdown tests the reading and writing of channels with and
// without optional shutdown script fields.
func TestOptionalShutdown(t *testing.T) {
	local := lnwire.DeliveryAddress([]byte("local shutdown script"))
	remote := lnwire.DeliveryAddress([]byte("remote shutdown script"))

	if _, err := rand.Read(remote); err != nil {
		t.Fatalf("Could not create random script: %v", err)
	}

	tests := []struct {
		name           string
		localShutdown  lnwire.DeliveryAddress
		remoteShutdown lnwire.DeliveryAddress
	}{
		{
			name:           "no shutdown scripts",
			localShutdown:  nil,
			remoteShutdown: nil,
		},
		{
			name:           "local shutdown script",
			localShutdown:  local,
			remoteShutdown: nil,
		},
		{
			name:           "remote shutdown script",
			localShutdown:  nil,
			remoteShutdown: remote,
		},
		{
			name:           "both scripts set",
			localShutdown:  local,
			remoteShutdown: remote,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			fullDB, err := MakeTestDB(t)
			if err != nil {
				t.Fatalf("unable to make test database: %v", err)
			}

			cdb := fullDB.ChannelStateDB()

			// Create a channel with upfront scripts set as
			// specified in the test.
			state := createTestChannel(
				t, cdb,
				localShutdownOption(test.localShutdown),
				remoteShutdownOption(test.remoteShutdown),
			)

			openChannels, err := cdb.FetchOpenChannels(
				state.IdentityPub,
			)
			if err != nil {
				t.Fatalf("unable to fetch open"+
					" channel: %v", err)
			}

			if len(openChannels) != 1 {
				t.Fatalf("Expected one channel open,"+
					" got: %v", len(openChannels))
			}

			if !bytes.Equal(openChannels[0].LocalShutdownScript,
				test.localShutdown) {

				t.Fatalf("Expected local: %x, got: %x",
					test.localShutdown,
					openChannels[0].LocalShutdownScript)
			}

			if !bytes.Equal(openChannels[0].RemoteShutdownScript,
				test.remoteShutdown) {

				t.Fatalf("Expected remote: %x, got: %x",
					test.remoteShutdown,
					openChannels[0].RemoteShutdownScript)
			}
		})
	}
}

func assertCommitmentEqual(t *testing.T, a, b *ChannelCommitment) {
	if !reflect.DeepEqual(a, b) {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line %v: commitments don't match: %v vs %v",
			line, spew.Sdump(a), spew.Sdump(b))
	}
}

// assertRevocationLogEntryEqual asserts that, for all the fields of a given
// revocation log entry, their values match those on a given ChannelCommitment.
func assertRevocationLogEntryEqual(t *testing.T, c *ChannelCommitment,
	r *RevocationLog) {

	t.Helper()

	// Check the common fields.
	require.EqualValues(
		t, r.CommitTxHash.Val, c.CommitTx.TxHash(), "CommitTx mismatch",
	)

	// Now check the common fields from the HTLCs.
	require.Equal(t, len(r.HTLCEntries), len(c.Htlcs), "HTLCs len mismatch")
	for i, rHtlc := range r.HTLCEntries {
		cHtlc := c.Htlcs[i]
		require.Equal(t, rHtlc.RHash.Val[:], cHtlc.RHash[:], "RHash")
		require.Equal(
			t, rHtlc.Amt.Val.Int(), cHtlc.Amt.ToSatoshis(), "Amt",
		)
		require.Equal(
			t, rHtlc.RefundTimeout.Val, cHtlc.RefundTimeout,
			"RefundTimeout",
		)
		require.EqualValues(
			t, rHtlc.OutputIndex.Val, cHtlc.OutputIndex,
			"OutputIndex",
		)
		require.Equal(
			t, rHtlc.Incoming.Val, cHtlc.Incoming, "Incoming",
		)
	}
}

func TestChannelStateTransition(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// First create a minimal channel, then perform a full sync in order to
	// persist the data.
	channel := createTestChannel(t, cdb)

	// Add some HTLCs which were added during this new state transition.
	// Half of the HTLCs are incoming, while the other half are outgoing.
	var (
		htlcs   []HTLC
		htlcAmt lnwire.MilliSatoshi
	)
	for i := uint32(0); i < 10; i++ {
		var incoming bool
		if i > 5 {
			incoming = true
		}
		htlc := HTLC{
			Signature:     testSig.Serialize(),
			Incoming:      incoming,
			Amt:           10,
			RHash:         key,
			RefundTimeout: i,
			OutputIndex:   int32(i * 3),
			LogIndex:      uint64(i * 2),
			HtlcIndex:     uint64(i),
		}
		copy(
			htlc.OnionBlob[:],
			bytes.Repeat([]byte{2}, lnwire.OnionPacketSize),
		)
		htlcs = append(htlcs, htlc)
		htlcAmt += htlc.Amt
	}

	// Create a new channel delta which includes the above HTLCs, some
	// balance updates, and an increment of the current commitment height.
	// Additionally, modify the signature and commitment transaction.
	newSequence := uint32(129498)
	newSig := bytes.Repeat([]byte{3}, 71)
	newTx := channel.LocalCommitment.CommitTx.Copy()
	newTx.TxIn[0].Sequence = newSequence
	commitment := ChannelCommitment{
		CommitHeight:    1,
		LocalLogIndex:   2,
		LocalHtlcIndex:  1,
		RemoteLogIndex:  2,
		RemoteHtlcIndex: 1,
		LocalBalance:    lnwire.MilliSatoshi(1e8),
		RemoteBalance:   lnwire.MilliSatoshi(1e8),
		CommitFee:       55,
		FeePerKw:        99,
		CommitTx:        newTx,
		CommitSig:       newSig,
		Htlcs:           htlcs,
		CustomBlob:      fn.Some([]byte{4, 5, 6}),
	}

	// First update the local node's broadcastable state and also add a
	// CommitDiff remote node's as well in order to simulate a proper state
	// transition.
	unsignedAckedUpdates := []LogUpdate{
		{
			LogIndex: 2,
			UpdateMsg: &lnwire.UpdateAddHTLC{
				ChanID: lnwire.ChannelID{1, 2, 3},
			},
		},
	}

	_, err = channel.UpdateCommitment(&commitment, unsignedAckedUpdates)
	require.NoError(t, err, "unable to update commitment")

	// Assert that update is correctly written to the database.
	dbUnsignedAckedUpdates, err := channel.UnsignedAckedUpdates()
	require.NoError(t, err, "unable to fetch dangling remote updates")
	if len(dbUnsignedAckedUpdates) != 1 {
		t.Fatalf("unexpected number of dangling remote updates")
	}
	if !reflect.DeepEqual(
		dbUnsignedAckedUpdates[0], unsignedAckedUpdates[0],
	) {

		t.Fatalf("unexpected update: expected %v, got %v",
			spew.Sdump(unsignedAckedUpdates[0]),
			spew.Sdump(dbUnsignedAckedUpdates))
	}

	// The balances, new update, the HTLCs and the changes to the fake
	// commitment transaction along with the modified signature should all
	// have been updated.
	updatedChannel, err := cdb.FetchOpenChannels(channel.IdentityPub)
	require.NoError(t, err, "unable to fetch updated channel")

	assertCommitmentEqual(
		t, &commitment, &updatedChannel[0].LocalCommitment,
	)

	numDiskUpdates, err := updatedChannel[0].CommitmentHeight()
	require.NoError(t, err, "unable to read commitment height from disk")

	if numDiskUpdates != uint64(commitment.CommitHeight) {
		t.Fatalf("num disk updates doesn't match: %v vs %v",
			numDiskUpdates, commitment.CommitHeight)
	}

	// Attempting to query for a commitment diff should return
	// ErrNoPendingCommit as we haven't yet created a new state for them.
	_, err = channel.RemoteCommitChainTip()
	if err != ErrNoPendingCommit {
		t.Fatalf("expected ErrNoPendingCommit, instead got %v", err)
	}

	// To simulate us extending a new state to the remote party, we'll also
	// create a new commit diff for them.
	remoteCommit := commitment
	remoteCommit.LocalBalance = lnwire.MilliSatoshi(2e8)
	remoteCommit.RemoteBalance = lnwire.MilliSatoshi(3e8)
	remoteCommit.CommitHeight = 1
	commitDiff := &CommitDiff{
		Commitment: remoteCommit,
		CommitSig: &lnwire.CommitSig{
			ChanID:    lnwire.ChannelID(key),
			CommitSig: wireSig,
			HtlcSigs: []lnwire.Sig{
				wireSig,
				wireSig,
			},
		},
		LogUpdates: []LogUpdate{
			{
				LogIndex: 1,
				UpdateMsg: &lnwire.UpdateAddHTLC{
					ID:     1,
					Amount: lnwire.NewMSatFromSatoshis(100),
					Expiry: 25,
				},
			},
			{
				LogIndex: 2,
				UpdateMsg: &lnwire.UpdateAddHTLC{
					ID:     2,
					Amount: lnwire.NewMSatFromSatoshis(200),
					Expiry: 50,
				},
			},
		},
		OpenedCircuitKeys: []models.CircuitKey{},
		ClosedCircuitKeys: []models.CircuitKey{},
	}
	copy(commitDiff.LogUpdates[0].UpdateMsg.(*lnwire.UpdateAddHTLC).PaymentHash[:],
		bytes.Repeat([]byte{1}, 32))
	copy(commitDiff.LogUpdates[1].UpdateMsg.(*lnwire.UpdateAddHTLC).PaymentHash[:],
		bytes.Repeat([]byte{2}, 32))
	if err := channel.AppendRemoteCommitChain(commitDiff); err != nil {
		t.Fatalf("unable to add to commit chain: %v", err)
	}

	// The commitment tip should now match the commitment that we just
	// inserted.
	diskCommitDiff, err := channel.RemoteCommitChainTip()
	require.NoError(t, err, "unable to fetch commit diff")
	if !reflect.DeepEqual(commitDiff, diskCommitDiff) {
		t.Fatalf("commit diffs don't match: %v vs %v", spew.Sdump(remoteCommit),
			spew.Sdump(diskCommitDiff))
	}

	// We'll save the old remote commitment as this will be added to the
	// revocation log shortly.
	oldRemoteCommit := channel.RemoteCommitment

	// Next, write to the log which tracks the necessary revocation state
	// needed to rectify any fishy behavior by the remote party. Modify the
	// current uncollapsed revocation state to simulate a state transition
	// by the remote party.
	channel.RemoteCurrentRevocation = channel.RemoteNextRevocation
	newPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err, "unable to generate key")
	channel.RemoteNextRevocation = newPriv.PubKey()

	fwdPkg := NewFwdPkg(channel.ShortChanID(), oldRemoteCommit.CommitHeight,
		diskCommitDiff.LogUpdates, nil)

	err = channel.AdvanceCommitChainTail(
		fwdPkg, nil, dummyLocalOutputIndex, dummyRemoteOutIndex,
	)
	require.NoError(t, err, "unable to append to revocation log")

	// At this point, the remote commit chain should be nil, and the posted
	// remote commitment should match the one we added as a diff above.
	if _, err := channel.RemoteCommitChainTip(); err != ErrNoPendingCommit {
		t.Fatalf("expected ErrNoPendingCommit, instead got %v", err)
	}

	// We should be able to fetch the channel delta created above by its
	// update number with all the state properly reconstructed.
	diskPrevCommit, _, err := channel.FindPreviousState(
		oldRemoteCommit.CommitHeight,
	)
	require.NoError(t, err, "unable to fetch past delta")

	// Check the output indexes are saved as expected.
	require.EqualValues(
		t, dummyLocalOutputIndex, diskPrevCommit.OurOutputIndex.Val,
	)
	require.EqualValues(
		t, dummyRemoteOutIndex, diskPrevCommit.TheirOutputIndex.Val,
	)

	// The two deltas (the original vs the on-disk version) should
	// identical, and all HTLC data should properly be retained.
	assertRevocationLogEntryEqual(t, &oldRemoteCommit, diskPrevCommit)

	// The state number recovered from the tail of the revocation log
	// should be identical to this current state.
	logTailHeight, err := channel.revocationLogTailCommitHeight()
	require.NoError(t, err, "unable to retrieve log")
	if logTailHeight != oldRemoteCommit.CommitHeight {
		t.Fatal("update number doesn't match")
	}

	oldRemoteCommit = channel.RemoteCommitment

	// Next modify the posted diff commitment slightly, then create a new
	// commitment diff and advance the tail.
	commitDiff.Commitment.CommitHeight = 2
	commitDiff.Commitment.LocalBalance -= htlcAmt
	commitDiff.Commitment.RemoteBalance += htlcAmt
	commitDiff.LogUpdates = []LogUpdate{}
	if err := channel.AppendRemoteCommitChain(commitDiff); err != nil {
		t.Fatalf("unable to add to commit chain: %v", err)
	}

	fwdPkg = NewFwdPkg(channel.ShortChanID(), oldRemoteCommit.CommitHeight, nil, nil)

	err = channel.AdvanceCommitChainTail(
		fwdPkg, nil, dummyLocalOutputIndex, dummyRemoteOutIndex,
	)
	require.NoError(t, err, "unable to append to revocation log")

	// Once again, fetch the state and ensure it has been properly updated.
	prevCommit, _, err := channel.FindPreviousState(
		oldRemoteCommit.CommitHeight,
	)
	require.NoError(t, err, "unable to fetch past delta")

	// Check the output indexes are saved as expected.
	require.EqualValues(
		t, dummyLocalOutputIndex, diskPrevCommit.OurOutputIndex.Val,
	)
	require.EqualValues(
		t, dummyRemoteOutIndex, diskPrevCommit.TheirOutputIndex.Val,
	)

	assertRevocationLogEntryEqual(t, &oldRemoteCommit, prevCommit)

	// Once again, state number recovered from the tail of the revocation
	// log should be identical to this current state.
	logTailHeight, err = channel.revocationLogTailCommitHeight()
	require.NoError(t, err, "unable to retrieve log")
	if logTailHeight != oldRemoteCommit.CommitHeight {
		t.Fatal("update number doesn't match")
	}

	// The revocation state stored on-disk should now also be identical.
	updatedChannel, err = cdb.FetchOpenChannels(channel.IdentityPub)
	require.NoError(t, err, "unable to fetch updated channel")
	if !channel.RemoteCurrentRevocation.IsEqual(updatedChannel[0].RemoteCurrentRevocation) {
		t.Fatalf("revocation state was not synced")
	}
	if !channel.RemoteNextRevocation.IsEqual(updatedChannel[0].RemoteNextRevocation) {
		t.Fatalf("revocation state was not synced")
	}

	// At this point, we should have 2 forwarding packages added.
	fwdPkgs := loadFwdPkgs(t, cdb.backend, channel.Packager)
	require.Len(t, fwdPkgs, 2, "wrong number of forwarding packages")

	// Now attempt to delete the channel from the database.
	closeSummary := &ChannelCloseSummary{
		ChanPoint:         channel.FundingOutpoint,
		RemotePub:         channel.IdentityPub,
		SettledBalance:    btcutil.Amount(500),
		TimeLockedBalance: btcutil.Amount(10000),
		IsPending:         false,
		CloseType:         RemoteForceClose,
	}
	if err := updatedChannel[0].CloseChannel(closeSummary); err != nil {
		t.Fatalf("unable to delete updated channel: %v", err)
	}

	// If we attempt to fetch the target channel again, it shouldn't be
	// found.
	channels, err := cdb.FetchOpenChannels(channel.IdentityPub)
	require.NoError(t, err, "unable to fetch updated channels")
	if len(channels) != 0 {
		t.Fatalf("%v channels, found, but none should be",
			len(channels))
	}

	// Attempting to find previous states on the channel should fail as the
	// revocation log has been deleted.
	_, _, err = updatedChannel[0].FindPreviousState(
		oldRemoteCommit.CommitHeight,
	)
	if err == nil {
		t.Fatal("revocation log search should have failed")
	}

	// All forwarding packages of this channel has been deleted too.
	fwdPkgs = loadFwdPkgs(t, cdb.backend, channel.Packager)
	require.Empty(t, fwdPkgs, "no forwarding packages should exist")
}

// TestOpeningChannelTxConfirmation verifies that calling MarkConfirmationHeight
// correctly updates the confirmed state. It also ensures that calling Refresh
// on a different OpenChannel updates its in-memory state to reflect the prior
// MarkConfirmationHeight call.
func TestOpeningChannelTxConfirmation(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)

	cdb := fullDB.ChannelStateDB()

	// Create a pending channel that was broadcast at height 99.
	const broadcastHeight = uint32(99)
	channelState := createTestChannel(
		t, cdb, pendingHeightOption(broadcastHeight),
	)

	// Fetch pending channels from the database.
	pendingChannels, err := cdb.FetchPendingChannels()
	require.NoError(t, err)
	require.Len(t, pendingChannels, 1)

	// Verify the broadcast height of the pending channel.
	require.Equal(
		t, broadcastHeight, pendingChannels[0].FundingBroadcastHeight,
	)

	confirmationHeight := broadcastHeight + 1

	// Mark the channel's confirmation height.
	err = pendingChannels[0].MarkConfirmationHeight(confirmationHeight)
	require.NoError(t, err)

	// Verify the ConfirmationHeight is updated correctly.
	require.Equal(
		t, confirmationHeight, pendingChannels[0].ConfirmationHeight,
	)

	// Re-fetch the pending channels to confirm persistence.
	pendingChannels, err = cdb.FetchPendingChannels()
	require.NoError(t, err)
	require.Len(t, pendingChannels, 1)

	// Validate the confirmation and broadcast height.
	require.Equal(
		t, confirmationHeight, pendingChannels[0].ConfirmationHeight,
	)
	require.Equal(
		t, broadcastHeight, pendingChannels[0].FundingBroadcastHeight,
	)

	// Ensure the original channel state's confirmation height is not
	// updated before refresh.
	require.EqualValues(t, channelState.ConfirmationHeight, 0)

	// Refresh the original channel state.
	err = channelState.Refresh()
	require.NoError(t, err)

	// Verify that both channel states now have the same ConfirmationHeight.
	require.Equal(
		t, channelState.ConfirmationHeight,
		pendingChannels[0].ConfirmationHeight,
	)
}

func TestFetchPendingChannels(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// Create a pending channel that was broadcast at height 99.
	const broadcastHeight = 99
	createTestChannel(t, cdb, pendingHeightOption(broadcastHeight))

	pendingChannels, err := cdb.FetchPendingChannels()
	require.NoError(t, err, "unable to list pending channels")

	if len(pendingChannels) != 1 {
		t.Fatalf("incorrect number of pending channels: expecting %v,"+
			"got %v", 1, len(pendingChannels))
	}

	// The broadcast height of the pending channel should have been set
	// properly.
	if pendingChannels[0].FundingBroadcastHeight != broadcastHeight {
		t.Fatalf("broadcast height mismatch: expected %v, got %v",
			pendingChannels[0].FundingBroadcastHeight,
			broadcastHeight)
	}

	chanOpenLoc := lnwire.ShortChannelID{
		BlockHeight: broadcastHeight + 1,
		TxIndex:     10,
		TxPosition:  15,
	}
	err = pendingChannels[0].MarkAsOpen(chanOpenLoc)
	require.NoError(t, err, "unable to mark channel as open")

	if pendingChannels[0].IsPending {
		t.Fatalf("channel marked open should no longer be pending")
	}

	if pendingChannels[0].ShortChanID() != chanOpenLoc {
		t.Fatalf("channel opening height not updated: expected %v, "+
			"got %v", spew.Sdump(pendingChannels[0].ShortChanID()),
			chanOpenLoc)
	}

	// Next, we'll re-fetch the channel to ensure that the open height was
	// properly set.
	openChans, err := cdb.FetchAllChannels()
	require.NoError(t, err, "unable to fetch channels")
	if openChans[0].ShortChanID() != chanOpenLoc {
		t.Fatalf("channel opening heights don't match: expected %v, "+
			"got %v", spew.Sdump(openChans[0].ShortChanID()),
			chanOpenLoc)
	}
	if openChans[0].FundingBroadcastHeight != broadcastHeight {
		t.Fatalf("broadcast height mismatch: expected %v, got %v",
			openChans[0].FundingBroadcastHeight,
			broadcastHeight)
	}

	pendingChannels, err = cdb.FetchPendingChannels()
	require.NoError(t, err, "unable to list pending channels")

	if len(pendingChannels) != 0 {
		t.Fatalf("incorrect number of pending channels: expecting %v,"+
			"got %v", 0, len(pendingChannels))
	}
}

func TestFetchClosedChannels(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// Create an open channel in the database.
	state := createTestChannel(t, cdb, openChannelOption())

	// Next, close the channel by including a close channel summary in the
	// database.
	summary := &ChannelCloseSummary{
		ChanPoint:         state.FundingOutpoint,
		ClosingTXID:       rev,
		RemotePub:         state.IdentityPub,
		Capacity:          state.Capacity,
		SettledBalance:    state.LocalCommitment.LocalBalance.ToSatoshis(),
		TimeLockedBalance: state.RemoteCommitment.LocalBalance.ToSatoshis() + 10000,
		CloseType:         RemoteForceClose,
		IsPending:         true,
		LocalChanConfig:   state.LocalChanCfg,
	}
	if err := state.CloseChannel(summary); err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	// Query the database to ensure that the channel has now been properly
	// closed. We should get the same result whether querying for pending
	// channels only, or not.
	pendingClosed, err := cdb.FetchClosedChannels(true)
	require.NoError(t, err, "failed fetching closed channels")
	if len(pendingClosed) != 1 {
		t.Fatalf("incorrect number of pending closed channels: expecting %v,"+
			"got %v", 1, len(pendingClosed))
	}
	if !reflect.DeepEqual(summary, pendingClosed[0]) {
		t.Fatalf("database summaries don't match: expected %v got %v",
			spew.Sdump(summary), spew.Sdump(pendingClosed[0]))
	}
	closed, err := cdb.FetchClosedChannels(false)
	require.NoError(t, err, "failed fetching all closed channels")
	if len(closed) != 1 {
		t.Fatalf("incorrect number of closed channels: expecting %v, "+
			"got %v", 1, len(closed))
	}
	if !reflect.DeepEqual(summary, closed[0]) {
		t.Fatalf("database summaries don't match: expected %v got %v",
			spew.Sdump(summary), spew.Sdump(closed[0]))
	}

	// Mark the channel as fully closed.
	err = cdb.MarkChanFullyClosed(&state.FundingOutpoint)
	require.NoError(t, err, "failed fully closing channel")

	// The channel should no longer be considered pending, but should still
	// be retrieved when fetching all the closed channels.
	closed, err = cdb.FetchClosedChannels(false)
	require.NoError(t, err, "failed fetching closed channels")
	if len(closed) != 1 {
		t.Fatalf("incorrect number of closed channels: expecting %v, "+
			"got %v", 1, len(closed))
	}
	pendingClose, err := cdb.FetchClosedChannels(true)
	require.NoError(t, err, "failed fetching channels pending close")
	if len(pendingClose) != 0 {
		t.Fatalf("incorrect number of closed channels: expecting %v, "+
			"got %v", 0, len(closed))
	}
}

// TestFetchWaitingCloseChannels ensures that the correct channels that are
// waiting to be closed are returned.
func TestFetchWaitingCloseChannels(t *testing.T) {
	t.Parallel()

	const numChannels = 2
	const broadcastHeight = 99

	// We'll start by creating two channels within our test database. One of
	// them will have their funding transaction confirmed on-chain, while
	// the other one will remain unconfirmed.
	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	channels := make([]*OpenChannel, numChannels)
	for i := 0; i < numChannels; i++ {
		// Create a pending channel in the database at the broadcast
		// height.
		channels[i] = createTestChannel(
			t, cdb, pendingHeightOption(broadcastHeight),
		)
	}

	// We'll only confirm the first one.
	channelConf := lnwire.ShortChannelID{
		BlockHeight: broadcastHeight + 1,
		TxIndex:     10,
		TxPosition:  15,
	}
	if err := channels[0].MarkAsOpen(channelConf); err != nil {
		t.Fatalf("unable to mark channel as open: %v", err)
	}

	// Then, we'll mark the channels as if their commitments were broadcast.
	// This would happen in the event of a force close and should make the
	// channels enter a state of waiting close.
	for _, channel := range channels {
		closeTx := wire.NewMsgTx(2)
		closeTx.AddTxIn(
			&wire.TxIn{
				PreviousOutPoint: channel.FundingOutpoint,
			},
		)

		if err := channel.MarkCommitmentBroadcasted(
			closeTx, lntypes.Local,
		); err != nil {
			t.Fatalf("unable to mark commitment broadcast: %v", err)
		}

		// Now try to marking a coop close with a nil tx. This should
		// succeed, but it shouldn't exit when queried.
		if err = channel.MarkCoopBroadcasted(
			nil, lntypes.Local,
		); err != nil {
			t.Fatalf("unable to mark nil coop broadcast: %v", err)
		}
		_, err := channel.BroadcastedCooperative()
		if err != ErrNoCloseTx {
			t.Fatalf("expected no closing tx error, got: %v", err)
		}

		// Finally, modify the close tx deterministically  and also mark
		// it as coop closed. Later we will test that distinct
		// transactions are returned for both coop and force closes.
		closeTx.TxIn[0].PreviousOutPoint.Index ^= 1
		if err := channel.MarkCoopBroadcasted(
			closeTx, lntypes.Local,
		); err != nil {
			t.Fatalf("unable to mark coop broadcast: %v", err)
		}
	}

	// Now, we'll fetch all the channels waiting to be closed from the
	// database. We should expect to see both channels above, even if any of
	// them haven't had their funding transaction confirm on-chain.
	waitingCloseChannels, err := cdb.FetchWaitingCloseChannels()
	require.NoError(t, err, "unable to fetch all waiting close channels")
	if len(waitingCloseChannels) != numChannels {
		t.Fatalf("expected %d channels waiting to be closed, got %d", 2,
			len(waitingCloseChannels))
	}
	expectedChannels := make(map[wire.OutPoint]struct{})
	for _, channel := range channels {
		expectedChannels[channel.FundingOutpoint] = struct{}{}
	}
	for _, channel := range waitingCloseChannels {
		if _, ok := expectedChannels[channel.FundingOutpoint]; !ok {
			t.Fatalf("expected channel %v to be waiting close",
				channel.FundingOutpoint)
		}

		chanPoint := channel.FundingOutpoint

		// Assert that the force close transaction is retrievable.
		forceCloseTx, err := channel.BroadcastedCommitment()
		if err != nil {
			t.Fatalf("Unable to retrieve commitment: %v", err)
		}

		if forceCloseTx.TxIn[0].PreviousOutPoint != chanPoint {
			t.Fatalf("expected outpoint %v, got %v",
				chanPoint,
				forceCloseTx.TxIn[0].PreviousOutPoint)
		}

		// Assert that the coop close transaction is retrievable.
		coopCloseTx, err := channel.BroadcastedCooperative()
		if err != nil {
			t.Fatalf("unable to retrieve coop close: %v", err)
		}

		chanPoint.Index ^= 1
		if coopCloseTx.TxIn[0].PreviousOutPoint != chanPoint {
			t.Fatalf("expected outpoint %v, got %v",
				chanPoint,
				coopCloseTx.TxIn[0].PreviousOutPoint)
		}
	}
}

// TestShutdownInfo tests that a channel's shutdown info can correctly be
// persisted and retrieved.
func TestShutdownInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		localInit bool
	}{
		{
			name:      "local node initiated",
			localInit: true,
		},
		{
			name:      "remote node initiated",
			localInit: false,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			testShutdownInfo(t, test.localInit)
		})
	}
}

func testShutdownInfo(t *testing.T, locallyInitiated bool) {
	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// First a test channel.
	channel := createTestChannel(t, cdb)

	// We haven't persisted any shutdown info for this channel yet.
	_, err = channel.ShutdownInfo()
	require.Error(t, err, ErrNoShutdownInfo)

	// Construct a new delivery script and create a new ShutdownInfo object.
	script := []byte{1, 3, 4, 5}

	// Create a ShutdownInfo struct.
	shutdownInfo := NewShutdownInfo(script, locallyInitiated)

	// Persist the shutdown info.
	require.NoError(t, channel.MarkShutdownSent(shutdownInfo))

	// We should now be able to retrieve the shutdown info.
	info, err := channel.ShutdownInfo()
	require.NoError(t, err)
	require.True(t, info.IsSome())

	// Assert that the decoded values of the shutdown info are correct.
	info.WhenSome(func(info ShutdownInfo) {
		require.EqualValues(t, script, info.DeliveryScript.Val)
		require.Equal(t, locallyInitiated, info.LocalInitiator.Val)
	})
}

// TestRefresh asserts that Refresh updates the in-memory state of another
// OpenChannel to reflect a preceding call to MarkOpen on a different
// OpenChannel.
func TestRefresh(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// First create a test channel.
	state := createTestChannel(t, cdb)

	// Next, locate the pending channel with the database.
	pendingChannels, err := cdb.FetchPendingChannels()
	if err != nil {
		t.Fatalf("unable to load pending channels; %v", err)
	}

	var pendingChannel *OpenChannel
	for _, channel := range pendingChannels {
		if channel.FundingOutpoint == state.FundingOutpoint {
			pendingChannel = channel
			break
		}
	}
	if pendingChannel == nil {
		t.Fatalf("unable to find pending channel with funding "+
			"outpoint=%v: %v", state.FundingOutpoint, err)
	}

	// Next, simulate the confirmation of the channel by marking it as
	// pending within the database.
	chanOpenLoc := lnwire.ShortChannelID{
		BlockHeight: 105,
		TxIndex:     10,
		TxPosition:  15,
	}

	err = state.MarkAsOpen(chanOpenLoc)
	require.NoError(t, err, "unable to mark channel open")

	// The short_chan_id of the receiver to MarkAsOpen should reflect the
	// open location, but the other pending channel should remain unchanged.
	if state.ShortChanID() == pendingChannel.ShortChanID() {
		t.Fatalf("pending channel short_chan_ID should not have been " +
			"updated before refreshing short_chan_id")
	}

	// Now that the receiver's short channel id has been updated, check to
	// ensure that the channel packager's source has been updated as well.
	// This ensures that the packager will read and write to buckets
	// corresponding to the new short chan id, instead of the prior.
	if state.Packager.(*ChannelPackager).source != chanOpenLoc {
		t.Fatalf("channel packager source was not updated: want %v, "+
			"got %v", chanOpenLoc,
			state.Packager.(*ChannelPackager).source)
	}

	// Now, refresh the state of the pending channel.
	err = pendingChannel.Refresh()
	require.NoError(t, err, "unable to refresh short_chan_id")

	// This should result in both OpenChannel's now having the same
	// ShortChanID.
	if state.ShortChanID() != pendingChannel.ShortChanID() {
		t.Fatalf("expected pending channel short_chan_id to be "+
			"refreshed: want %v, got %v", state.ShortChanID(),
			pendingChannel.ShortChanID())
	}

	// Check to ensure that the _other_ OpenChannel channel packager's
	// source has also been updated after the refresh. This ensures that the
	// other packagers will read and write to buckets corresponding to the
	// updated short chan id.
	if pendingChannel.Packager.(*ChannelPackager).source != chanOpenLoc {
		t.Fatalf("channel packager source was not updated: want %v, "+
			"got %v", chanOpenLoc,
			pendingChannel.Packager.(*ChannelPackager).source)
	}

	// Check to ensure that this channel is no longer pending and this field
	// is up to date.
	if pendingChannel.IsPending {
		t.Fatalf("channel pending state wasn't updated: want false got true")
	}
}

// TestCloseInitiator tests the setting of close initiator statuses for
// cooperative closes and local force closes.
func TestCloseInitiator(t *testing.T) {
	tests := []struct {
		name string
		// updateChannel is called to update the channel as broadcast,
		// cooperatively or not, based on the test's requirements.
		updateChannel    func(c *OpenChannel) error
		expectedStatuses []ChannelStatus
	}{
		{
			name: "local coop close",
			// Mark the channel as cooperatively closed, initiated
			// by the local party.
			updateChannel: func(c *OpenChannel) error {
				return c.MarkCoopBroadcasted(
					&wire.MsgTx{}, lntypes.Local,
				)
			},
			expectedStatuses: []ChannelStatus{
				ChanStatusLocalCloseInitiator,
				ChanStatusCoopBroadcasted,
			},
		},
		{
			name: "remote coop close",
			// Mark the channel as cooperatively closed, initiated
			// by the remote party.
			updateChannel: func(c *OpenChannel) error {
				return c.MarkCoopBroadcasted(
					&wire.MsgTx{}, lntypes.Remote,
				)
			},
			expectedStatuses: []ChannelStatus{
				ChanStatusRemoteCloseInitiator,
				ChanStatusCoopBroadcasted,
			},
		},
		{
			name: "local force close",
			// Mark the channel's commitment as broadcast with
			// local initiator.
			updateChannel: func(c *OpenChannel) error {
				return c.MarkCommitmentBroadcasted(
					&wire.MsgTx{}, lntypes.Local,
				)
			},
			expectedStatuses: []ChannelStatus{
				ChanStatusLocalCloseInitiator,
				ChanStatusCommitBroadcasted,
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fullDB, err := MakeTestDB(t)
			if err != nil {
				t.Fatalf("unable to make test database: %v",
					err)
			}

			cdb := fullDB.ChannelStateDB()

			// Create an open channel.
			channel := createTestChannel(
				t, cdb, openChannelOption(),
			)

			err = test.updateChannel(channel)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Lookup open channels in the database.
			dbChans, err := fetchChannels(
				cdb, pendingChannelFilter(false),
			)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(dbChans) != 1 {
				t.Fatalf("expected 1 channel, got: %v",
					len(dbChans))
			}

			// Check that the statuses that we expect were written
			// to disk.
			for _, status := range test.expectedStatuses {
				if !dbChans[0].HasChanStatus(status) {
					t.Fatalf("expected channel to have "+
						"status: %v, has status: %v",
						status, dbChans[0].chanStatus)
				}
			}
		})
	}
}

// TestCloseChannelStatus tests setting of a channel status on the historical
// channel on channel close.
func TestCloseChannelStatus(t *testing.T) {
	fullDB, err := MakeTestDB(t)
	if err != nil {
		t.Fatalf("unable to make test database: %v",
			err)
	}

	cdb := fullDB.ChannelStateDB()

	// Create an open channel.
	channel := createTestChannel(
		t, cdb, openChannelOption(),
	)

	if err := channel.CloseChannel(
		&ChannelCloseSummary{
			ChanPoint: channel.FundingOutpoint,
			RemotePub: channel.IdentityPub,
		}, ChanStatusRemoteCloseInitiator,
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	histChan, err := channel.Db.FetchHistoricalChannel(
		&channel.FundingOutpoint,
	)
	require.NoError(t, err, "unexpected error")

	if !histChan.HasChanStatus(ChanStatusRemoteCloseInitiator) {
		t.Fatalf("channel should have status")
	}
}

// TestHasChanStatus asserts the behavior of HasChanStatus by checking the
// behavior of various status flags in addition to the special case of
// ChanStatusDefault which is treated like a flag in the code base even though
// it isn't.
func TestHasChanStatus(t *testing.T) {
	tests := []struct {
		name   string
		status ChannelStatus
		expHas map[ChannelStatus]bool
	}{
		{
			name:   "default",
			status: ChanStatusDefault,
			expHas: map[ChannelStatus]bool{
				ChanStatusDefault: true,
				ChanStatusBorked:  false,
			},
		},
		{
			name:   "single flag",
			status: ChanStatusBorked,
			expHas: map[ChannelStatus]bool{
				ChanStatusDefault: false,
				ChanStatusBorked:  true,
			},
		},
		{
			name:   "multiple flags",
			status: ChanStatusBorked | ChanStatusLocalDataLoss,
			expHas: map[ChannelStatus]bool{
				ChanStatusDefault:       false,
				ChanStatusBorked:        true,
				ChanStatusLocalDataLoss: true,
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			c := &OpenChannel{
				chanStatus: test.status,
			}

			for status, expHas := range test.expHas {
				has := c.HasChanStatus(status)
				if has == expHas {
					continue
				}

				t.Fatalf("expected chan status to "+
					"have %s? %t, got: %t",
					status, expHas, has)
			}
		})
	}
}

// TestKeyLocatorEncoding tests that we are able to serialize a given
// keychain.KeyLocator. After successfully encoding, we check that the decode
// output arrives at the same initial KeyLocator.
func TestKeyLocatorEncoding(t *testing.T) {
	keyLoc := keychain.KeyLocator{
		Family: keychain.KeyFamilyRevocationRoot,
		Index:  keyLocIndex,
	}

	// First, we'll encode the KeyLocator into a buffer.
	var (
		b   bytes.Buffer
		buf [8]byte
	)

	err := EKeyLocator(&b, &keyLoc, &buf)
	require.NoError(t, err, "unable to encode key locator")

	// Next, we'll attempt to decode the bytes into a new KeyLocator.
	r := bytes.NewReader(b.Bytes())
	var decodedKeyLoc keychain.KeyLocator

	err = DKeyLocator(r, &decodedKeyLoc, &buf, 8)
	require.NoError(t, err, "unable to decode key locator")

	// Finally, we'll compare that the original KeyLocator and the decoded
	// version are equal.
	require.Equal(t, keyLoc, decodedKeyLoc)
}

// TestFinalHtlcs tests final htlc storage and retrieval.
func TestFinalHtlcs(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t, OptionStoreFinalHtlcResolutions(true))
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	chanID := lnwire.ShortChannelID{
		BlockHeight: 1,
		TxIndex:     2,
		TxPosition:  3,
	}

	// Test unknown htlc lookup.
	const unknownHtlcID = 999

	_, err = cdb.LookupFinalHtlc(chanID, unknownHtlcID)
	require.ErrorIs(t, err, ErrHtlcUnknown)

	// Test offchain final htlcs.
	const offchainHtlcID = 1

	err = kvdb.Update(cdb.backend, func(tx kvdb.RwTx) error {
		bucket, err := fetchFinalHtlcsBucketRw(
			tx, chanID,
		)
		require.NoError(t, err)

		return putFinalHtlc(bucket, offchainHtlcID, FinalHtlcInfo{
			Settled:  true,
			Offchain: true,
		})
	}, func() {})
	require.NoError(t, err)

	info, err := cdb.LookupFinalHtlc(chanID, offchainHtlcID)
	require.NoError(t, err)
	require.True(t, info.Settled)
	require.True(t, info.Offchain)

	// Test onchain final htlcs.
	const onchainHtlcID = 2

	err = cdb.PutOnchainFinalHtlcOutcome(chanID, onchainHtlcID, true)
	require.NoError(t, err)

	info, err = cdb.LookupFinalHtlc(chanID, onchainHtlcID)
	require.NoError(t, err)
	require.True(t, info.Settled)
	require.False(t, info.Offchain)

	// Test unknown htlc lookup for existing channel.
	_, err = cdb.LookupFinalHtlc(chanID, unknownHtlcID)
	require.ErrorIs(t, err, ErrHtlcUnknown)
}

// TestHTLCsExtraData tests serialization and deserialization of HTLCs
// combined with extra data.
func TestHTLCsExtraData(t *testing.T) {
	t.Parallel()

	mockHtlc := HTLC{
		Signature:     testSig.Serialize(),
		Incoming:      false,
		Amt:           10,
		RHash:         key,
		RefundTimeout: 1,
		OnionBlob:     lnmock.MockOnion(),
	}

	// Add a blinding point to a htlc.
	blindingPointHTLC := HTLC{
		Signature:     testSig.Serialize(),
		Incoming:      false,
		Amt:           10,
		RHash:         key,
		RefundTimeout: 1,
		OnionBlob:     lnmock.MockOnion(),
		BlindingPoint: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[lnwire.BlindingPointTlvType](
				pubKey,
			),
		),
	}

	// Custom channel data htlc with a blinding point.
	customDataHTLC := HTLC{
		Signature:     testSig.Serialize(),
		Incoming:      false,
		Amt:           10,
		RHash:         key,
		RefundTimeout: 1,
		OnionBlob:     lnmock.MockOnion(),
		BlindingPoint: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[lnwire.BlindingPointTlvType](
				pubKey,
			),
		),
		CustomRecords: map[uint64][]byte{
			uint64(lnwire.MinCustomRecordsTlvType + 3): {1, 2, 3},
		},
	}

	testCases := []struct {
		name        string
		htlcs       []HTLC
		blindingIdx int
	}{
		{
			// Serialize multiple HLTCs with no extra data to
			// assert that there is no regression for HTLCs with
			// no extra data.
			name: "no extra data",
			htlcs: []HTLC{
				mockHtlc, mockHtlc,
			},
		},
		{
			// Some HTLCs with extra data, some without.
			name: "mixed extra data",
			htlcs: []HTLC{
				mockHtlc,
				blindingPointHTLC,
				mockHtlc,
				customDataHTLC,
			},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer
			err := SerializeHtlcs(&b, testCase.htlcs...)
			require.NoError(t, err)

			r := bytes.NewReader(b.Bytes())
			htlcs, err := DeserializeHtlcs(r)
			require.NoError(t, err)

			require.EqualValues(t, len(testCase.htlcs), len(htlcs))
			for i, htlc := range htlcs {
				// We use the extra data field when we
				// serialize, so we set to nil to be able to
				// assert on equal for the test.
				htlc.ExtraData = nil
				require.Equal(t, testCase.htlcs[i], htlc)
			}
		})
	}
}

// TestOnionBlobIncorrectLength tests HTLC deserialization in the case where
// the OnionBlob saved on disk is of an unexpected length. This error case is
// only expected in the case of database corruption (or some severe protocol
// breakdown/bug). A HTLC is manually serialized because we cannot force a
// case where we write an onion blob of incorrect length.
func TestOnionBlobIncorrectLength(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer

	var numHtlcs uint16 = 1
	require.NoError(t, WriteElement(&b, numHtlcs))

	require.NoError(t, WriteElements(
		&b,
		// Number of HTLCs.
		numHtlcs,
		// Signature, incoming, amount, Rhash, Timeout.
		testSig.Serialize(), false, lnwire.MilliSatoshi(10), key,
		uint32(1),
		// Write an onion blob that is half of our expected size.
		bytes.Repeat([]byte{1}, lnwire.OnionPacketSize/2),
	))

	_, err := DeserializeHtlcs(&b)
	require.ErrorIs(t, err, ErrOnionBlobLength)
}
