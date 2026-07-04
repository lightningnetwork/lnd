package chanstate

import (
	"bytes"
	"math/rand"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntest/channels"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/stretchr/testify/require"
)

var (
	// uniqueOutputIndex is used to create a unique funding outpoint.
	//
	// NOTE: must be incremented when used.
	uniqueOutputIndex = atomic.Uint32{}
)

const (
	// defaultPendingHeight is the default height at which we set
	// channels to pending.
	defaultPendingHeight = 100
)

type testChannelOption func(channel *OpenChannel)

// localShutdownOption is an option which sets the local upfront shutdown
// script for the channel.
func localShutdownOption(addr lnwire.DeliveryAddress) testChannelOption {
	return func(channel *OpenChannel) {
		channel.LocalShutdownScript = addr
	}
}

// remoteShutdownOption is an option which sets the remote upfront shutdown
// script for the channel.
func remoteShutdownOption(addr lnwire.DeliveryAddress) testChannelOption {
	return func(channel *OpenChannel) {
		channel.RemoteShutdownScript = addr
	}
}

// createTestChannel writes a test channel to the database.
func createTestChannel(t *testing.T, store *KVStore,
	opts ...testChannelOption) *OpenChannel {

	t.Helper()

	channel := createTestChannelState(t, store)
	for _, option := range opts {
		option(channel)
	}

	err := kvdb.Update(store.backend, func(tx kvdb.RwTx) error {
		_, err := tx.CreateTopLevelBucket(outpointBucket)
		if err != nil {
			return err
		}

		_, err = tx.CreateTopLevelBucket(chanIDBucket)
		if err != nil {
			return err
		}

		return SyncPendingOpenChannel(
			tx, channel, defaultPendingHeight,
		)
	}, func() {})
	require.NoError(t, err, "unable to save and serialize channel state")

	return channel
}

func createTestOpenChannel(t *testing.T, store *KVStore) *OpenChannel {
	t.Helper()

	channel := createTestChannel(t, store)

	err := channel.MarkAsOpen(channel.ShortChannelID)
	require.NoError(t, err, "unable to mark channel open")

	return channel
}

func createTestChannelState(t *testing.T, store *KVStore) *OpenChannel {
	t.Helper()

	// Simulate 1000 channel updates.
	producer, err := shachain.NewRevocationProducerFromBytes(key[:])
	require.NoError(t, err, "could not get producer")
	storeRevocation := shachain.NewRevocationStore()
	for i := 0; i < 1; i++ {
		preImage, err := producer.AtIndex(uint64(i))
		if err != nil {
			t.Fatalf("could not get "+
				"preimage: %v", err)
		}

		if err := storeRevocation.AddNextEntry(preImage); err != nil {
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
		RevocationStore:         storeRevocation,
		Db:                      store,
		FundingTxn:              channels.TestFundingTx,
		ThawHeight:              uint32(defaultPendingHeight),
		InitialLocalBalance:     lnwire.MilliSatoshi(9000),
		InitialRemoteBalance:    lnwire.MilliSatoshi(3000),
		Memo:                    []byte("test"),
		TapscriptRoot:           fn.Some(tapscriptRoot),
		CustomBlob:              fn.Some([]byte{1, 2, 3}),
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
		t.Run(test.name, func(t *testing.T) {
			backend, cleanup := makeTestBackend(t)
			t.Cleanup(cleanup)

			store := NewKVStore(
				backend, WithStoreFinalHtlcResolutions(true),
			)

			// Create a channel with upfront scripts set as
			// specified in the test.
			state := createTestChannel(
				t, store,
				localShutdownOption(test.localShutdown),
				remoteShutdownOption(test.remoteShutdown),
			)

			openChannels, err := store.FetchOpenChannels(
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
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			testShutdownInfo(t, test.localInit)
		})
	}
}

func testShutdownInfo(t *testing.T, locallyInitiated bool) {
	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend, WithStoreFinalHtlcResolutions(true))

	// First a test channel.
	channel := createTestChannel(t, store)

	// We haven't persisted any shutdown info for this channel yet.
	_, err := channel.ShutdownInfo()
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

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend, WithStoreFinalHtlcResolutions(true))

	// First create a test channel.
	state := createTestChannel(t, store)

	// Next, locate the pending channel with the database.
	pendingChannels, err := store.FetchPendingChannels()
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

	// Check to ensure that this channel is no longer pending and this field
	// is up to date.
	if pendingChannel.IsPending {
		t.Fatalf("channel pending state wasn't updated: want false " +
			"got true")
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
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			backend, cleanup := makeTestBackend(t)
			t.Cleanup(cleanup)

			store := NewKVStore(
				backend, WithStoreFinalHtlcResolutions(true),
			)

			// Create an open channel.
			channel := createTestOpenChannel(t, store)

			err := test.updateChannel(channel)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Lookup open channels in the database.
			dbChans, err := store.FetchAllChannels()
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
						status, dbChans[0].ChanStatus())
				}
			}
		})
	}
}

// TestCloseChannelStatus tests setting of a channel status on the historical
// channel on channel close.
func TestCloseChannelStatus(t *testing.T) {
	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend, WithStoreFinalHtlcResolutions(true))

	// Create an open channel.
	channel := createTestOpenChannel(t, store)

	if err := channel.CloseChannel(
		&ChannelCloseSummary{
			ChanPoint: channel.FundingOutpoint,
			RemotePub: channel.IdentityPub,
		}, ChanStatusRemoteCloseInitiator,
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	histChan, err := store.FetchHistoricalChannel(
		&channel.FundingOutpoint,
	)
	require.NoError(t, err, "unexpected error")

	if !histChan.HasChanStatus(ChanStatusRemoteCloseInitiator) {
		t.Fatalf("channel should have status")
	}
}
