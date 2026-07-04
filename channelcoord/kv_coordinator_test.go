package channelcoord_test

import (
	"bytes"
	"net"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/channelcoord"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/linknode"
	"github.com/lightningnetwork/lnd/lntest/channels"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/stretchr/testify/require"
)

var kvTestOutputIndex atomic.Uint32

func TestMain(m *testing.M) {
	kvdb.RunTests(m)
}

type kvCoordinatorTestFixture struct {
	coordinator   channelcoord.Coordinator
	chanStore     chanstate.Store
	linkNodeStore linknode.Store
}

func makeKVCoordinatorTestFixture(t *testing.T) kvCoordinatorTestFixture {
	t.Helper()

	backend, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "cdb")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	err = kvdb.Update(backend, func(tx kvdb.RwTx) error {
		_, err := tx.CreateTopLevelBucket(
			chanstate.OutpointBucketKey(),
		)
		if err != nil {
			return err
		}

		_, err = tx.CreateTopLevelBucket(chanstate.ChanIDBucketKey())

		return err
	}, func() {})
	require.NoError(t, err)

	chanStore := chanstate.NewKVStore(backend)
	linkNodeDB := linknode.NewDB(backend)
	coordinator := channelcoord.NewKVCoordinator(
		backend, linkNodeDB, chanStore,
	)

	return kvCoordinatorTestFixture{
		coordinator:   coordinator,
		chanStore:     chanStore,
		linkNodeStore: linkNodeDB,
	}
}

func createKVCoordinatorTestChannel(t *testing.T,
	store chanstate.Store) *chanstate.OpenChannel {

	t.Helper()

	key := [chainhash.HashSize]byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x68, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
	}
	privKey, pubKey := btcec.PrivKeyFromBytes(key[:])

	producer, err := shachain.NewRevocationProducerFromBytes(key[:])
	require.NoError(t, err)

	revocationStore := shachain.NewRevocationStore()
	preImage, err := producer.AtIndex(0)
	require.NoError(t, err)
	require.NoError(t, revocationStore.AddNextEntry(preImage))

	channelStateBounds := chanstate.ChannelStateBounds{
		MaxPendingAmount: lnwire.MilliSatoshi(10_000),
		ChanReserve:      btcutil.Amount(1_000),
		MinHTLC:          lnwire.MilliSatoshi(1),
		MaxAcceptedHtlcs: 10,
	}
	commitmentParams := chanstate.CommitmentParams{
		DustLimit: btcutil.Amount(546),
		CsvDelay:  144,
	}

	localCfg := chanstate.ChannelConfig{
		ChannelStateBounds: channelStateBounds,
		CommitmentParams:   commitmentParams,
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
	remoteCfg := localCfg

	kvTestOutputIndex.Add(1)
	op := wire.OutPoint{
		Hash:  key,
		Index: kvTestOutputIndex.Load(),
	}

	var tapscriptRoot chainhash.Hash
	copy(tapscriptRoot[:], bytes.Repeat([]byte{1}, 32))

	return &chanstate.OpenChannel{
		ChanType:        chanstate.SingleFunderBit,
		ChainHash:       key,
		FundingOutpoint: op,
		ShortChannelID:  lnwire.NewShortChanIDFromInt(1),
		IsInitiator:     true,
		IsPending:       true,
		IdentityPub:     pubKey,
		Capacity:        btcutil.Amount(10_000),
		LocalChanCfg:    localCfg,
		RemoteChanCfg:   remoteCfg,
		LocalCommitment: chanstate.ChannelCommitment{
			CommitHeight:  0,
			LocalBalance:  lnwire.MilliSatoshi(9_000),
			RemoteBalance: lnwire.MilliSatoshi(1_000),
			CommitTx:      channels.TestFundingTx,
			CommitSig:     bytes.Repeat([]byte{1}, 71),
		},
		RemoteCommitment: chanstate.ChannelCommitment{
			CommitHeight:  0,
			LocalBalance:  lnwire.MilliSatoshi(1_000),
			RemoteBalance: lnwire.MilliSatoshi(9_000),
			CommitTx:      channels.TestFundingTx,
			CommitSig:     bytes.Repeat([]byte{1}, 71),
		},
		NumConfsRequired:        4,
		RemoteCurrentRevocation: privKey.PubKey(),
		RemoteNextRevocation:    privKey.PubKey(),
		RevocationProducer:      producer,
		RevocationStore:         revocationStore,
		Db:                      store,
		FundingTxn:              channels.TestFundingTx,
		ThawHeight:              100,
		TapscriptRoot:           fn.Some(tapscriptRoot),
	}
}

// TestRepairLinkNodes tests that RepairLinkNodes identifies and repairs
// missing link nodes for channels that exist in the database.
func TestRepairLinkNodes(t *testing.T) {
	t.Parallel()

	fixture := makeKVCoordinatorTestFixture(t)

	channel := createKVCoordinatorTestChannel(t, fixture.chanStore)
	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18555,
	}
	err := fixture.coordinator.SyncPendingChannel(
		channel, addr, uint32(100),
	)
	require.NoError(t, err, "unable to sync pending channel")

	fetchedLinkNode, err := fixture.linkNodeStore.FetchLinkNode(
		channel.IdentityPub,
	)
	require.NoError(t, err, "link node should exist")
	require.NotNil(t, fetchedLinkNode, "link node should not be nil")

	err = fixture.linkNodeStore.DeleteLinkNode(channel.IdentityPub)
	require.NoError(t, err, "unable to delete link node")

	_, err = fixture.linkNodeStore.FetchLinkNode(channel.IdentityPub)
	require.ErrorIs(t, err, linknode.ErrNodeNotFound)

	err = fixture.coordinator.RepairLinkNodes(wire.MainNet)
	require.NoError(t, err, "repair should succeed")

	repairedLinkNode, err := fixture.linkNodeStore.FetchLinkNode(
		channel.IdentityPub,
	)
	require.NoError(t, err, "repaired link node should exist")
	require.NotNil(
		t, repairedLinkNode, "repaired link node should not be nil",
	)
	require.Equal(t, wire.MainNet, repairedLinkNode.Network)

	err = fixture.coordinator.RepairLinkNodes(wire.MainNet)
	require.NoError(t, err, "second repair should succeed")

	err = fixture.linkNodeStore.DeleteLinkNode(channel.IdentityPub)
	require.NoError(t, err, "unable to delete link node")

	err = fixture.coordinator.RepairLinkNodes(wire.TestNet3)
	require.NoError(t, err, "repair with testnet should succeed")

	repairedLinkNode, err = fixture.linkNodeStore.FetchLinkNode(
		channel.IdentityPub,
	)
	require.NoError(t, err, "repaired link node should exist")
	require.Equal(t, wire.TestNet3, repairedLinkNode.Network)
}
