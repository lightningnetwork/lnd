package discovery

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightningnetwork/lnd/channeldb"
	fnopt "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnmock"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	testRBytes, _ = hex.DecodeString("8ce2bc69281ce27da07e6683571319d18e" +
		"949ddfa2965fb6caa1bf0314f882d7")
	testSBytes, _ = hex.DecodeString("299105481d63e0f4bc2a88121167221b67" +
		"00d72a0ead154c03be696a292d24ae")
	testRScalar = new(btcec.ModNScalar)
	testSScalar = new(btcec.ModNScalar)
	_           = testRScalar.SetByteSlice(testRBytes)
	_           = testSScalar.SetByteSlice(testSBytes)
	testSig     = ecdsa.NewSignature(testRScalar, testSScalar)
)

const (
	// total number of fuzz state actions.
	numFuzzStates = 13

	// For the fuzz tests, cap the block height to roughly the year 2142.
	blockHeightCap = 6990480
)

// getUint64 extracts a uint64 from a byte slice.
func getUint64(data []byte) uint64 {
	return binary.BigEndian.Uint64(data)
}

// getUint32 extracts a uint32 from a byte slice.
func getUint32(data []byte) uint32 {
	return binary.BigEndian.Uint32(data)
}

// getUint16 extracts a uint32 from a byte slice.
func getUint16(data []byte) uint16 {
	return binary.BigEndian.Uint16(data)
}

// getInt64 extracts a non-negative int32 from a byte slice.
func getInt32(data []byte) int32 {
	return int32(getUint32(data) % uint32(math.MaxInt32+1))
}

// ParsePrivKey parses raw private key bytes and returns the parsed private key,
// along with a boolean indicating whether the provided bytes represent a valid
// private key.
//
// NOTE: Ideally, this should be placed in btcd, but for now it is defined here.
func ParsePrivKey(privKeyBytes []byte) (*secp256k1.PrivateKey, bool) {
	var key secp256k1.ModNScalar
	overflows := key.SetByteSlice(privKeyBytes)
	if overflows || key.IsZero() {
		return nil, false
	}

	return secp256k1.NewPrivateKey(&key), true
}

// fuzzState represents the different states in the gossip protocol operation.
type fuzzState uint8

const (
	connectPeer fuzzState = iota
	disconnectPeer
	nodeAnnouncementReceived
	channelAnnouncementReceived
	channelUpdateReceived
	announcementSignaturesReceived
	queryShortChanIDsReceived
	queryChannelRangeReceived
	replyChannelRangeReceived
	gossipTimestampRangeReceived
	replyShortChanIDsEndReceived
	udpateBlockHeight
	triggerHistoricalSync
)

// gossipPeer represents a mock peer along with its associated LN and BTC
// private keys.
type gossipPeer struct {
	connection *mockPeer
	lnPrivKey  *btcec.PrivateKey
	btcPrivKey *btcec.PrivateKey
}

// fuzzNetwork represents a test network harness used to fuzz the gossip
// subsystem between the local node and remote peers.
type fuzzNetwork struct {
	t    *testing.T
	data []byte

	gossiper    *AuthenticatedGossiper
	chain       *lnmock.MockChain
	notifier    *mockNotifier
	blockHeight *int32

	selfBtcPrivKey *btcec.PrivateKey
	selfLNPrivKey  *btcec.PrivateKey
	peers          []*gossipPeer
}

// createGossiper creates and starts a gossiper for fuzz testing.
func createGossiper(t *testing.T, blockHeight *int32,
	selfPrivKey *btcec.PrivateKey) (*AuthenticatedGossiper,
	*lnmock.MockChain, *mockNotifier) {

	t.Helper()

	chain := &lnmock.MockChain{}

	// Return blockHeight dynamically so each call reflects the latest
	// value.
	chain.On("GetBestBlock").
		Return(func() (*chainhash.Hash, int32, error) {
			return &chainhash.Hash{}, *blockHeight, nil
		})

	// We can use the mock channel series and graph builder, but there is a
	// trade-off between RAM and CPU. For now, I prefer higher CPU usage to
	// avoid unnecessary OOMs, which are worse than increased CPU usage.
	graphDb := graphdb.MakeTestGraph(t)
	channelSeries := NewChanSeries(
		graphdb.NewVersionedGraph(graphDb, lnwire.GossipVersion1),
	)

	notifier := newMockNotifier()

	graphBuilder, err := graph.NewBuilder(&graph.Config{
		Graph:              graphDb,
		Chain:              chain,
		ChainView:          &noopChainView{},
		ChannelPruneExpiry: graph.DefaultChannelPruneExpiry,
		IsAlias: func(lnwire.ShortChannelID) bool {
			return false
		},
	})
	require.NoError(t, err)

	pubKey := route.NewVertex(selfPrivKey.PubKey())
	dbNode := models.NewV1Node(pubKey, &models.NodeV1Fields{
		AuthSigBytes: testSig.Serialize(),
		LastUpdate:   time.Now(),
		Addresses:    testAddrs,
		Alias:        "kek" + hex.EncodeToString(pubKey[:]),
		Features:     testFeatures,
	})
	err = graphDb.SetSourceNode(t.Context(), dbNode)
	require.NoError(t, err)

	db := channeldb.OpenForTesting(t, t.TempDir())
	waitingProofStore, err := channeldb.NewWaitingProofStore(db)
	require.NoError(t, err)

	gossiper := New(Config{
		Graph:       graphBuilder,
		ChanSeries:  channelSeries,
		ChainIO:     chain,
		ChainParams: &chaincfg.MainNetParams,
		Notifier:    notifier,
		Broadcast: func(senders map[route.Vertex]struct{},
			msgs ...lnwire.Message) error {

			for _, msg := range msgs {
				switch msg.(type) {
				case *lnwire.NodeAnnouncement1,
					*lnwire.ChannelAnnouncement1,
					*lnwire.ChannelUpdate1,
					*lnwire.AnnounceSignatures1:
					// Allowed announcement messages.

				default:
					t.Fatalf("received unexpected message "+
						"in broadcast: %T (type=%d)",
						msg, msg.MsgType())
				}
			}

			return nil
		},
		FetchSelfAnnouncement: func() lnwire.NodeAnnouncement1 {
			return lnwire.NodeAnnouncement1{
				Timestamp: uint32(time.Now().Unix()),
			}
		},
		UpdateSelfAnnouncement: func() (lnwire.NodeAnnouncement1,
			error) {

			return lnwire.NodeAnnouncement1{
				Timestamp: uint32(time.Now().Unix()),
			}, nil
		},
		NotifyWhenOnline: func(target [33]byte,
			peerChan chan<- lnpeer.Peer) {

			pk, _ := btcec.ParsePubKey(target[:])
			peerChan <- &mockPeer{pk, nil, nil, atomic.Bool{}}
		},
		NotifyWhenOffline: func(_ [33]byte) <-chan struct{} {
			return nil
		},
		TrickleDelay:          time.Millisecond * 10,
		RetransmitTicker:      &noopTicker{},
		RebroadcastInterval:   rebroadcastInterval,
		ProofMatureDelta:      DefaultProofMatureDelta,
		WaitingProofStore:     waitingProofStore,
		MessageStore:          newMockMessageStore(),
		RotateTicker:          &noopTicker{},
		HistoricalSyncTicker:  &noopTicker{},
		NumActiveSyncers:      3,
		AnnSigner:             &mock.SingleSigner{Privkey: selfPrivKey},
		SubBatchDelay:         1 * time.Millisecond,
		MinimumBatchSize:      10,
		MaxChannelUpdateBurst: DefaultMaxChannelUpdateBurst,
		ChannelUpdateInterval: DefaultChannelUpdateInterval,
		IsAlias: func(lnwire.ShortChannelID) bool {
			return false
		},
		SignAliasUpdate: func(*lnwire.ChannelUpdate1) (*ecdsa.Signature,
			error) {

			return nil, nil
		},
		FindBaseByAlias: func(lnwire.ShortChannelID) (lnwire.
			ShortChannelID, error) {

			return lnwire.ShortChannelID{},
				fmt.Errorf("no base scid")
		},
		GetAlias: func(lnwire.ChannelID) (lnwire.ShortChannelID,
			error) {

			return lnwire.ShortChannelID{},
				fmt.Errorf("no peer alias")
		},
		FindChannel:  mockFindChannel,
		ScidCloser:   newMockScidCloser(true),
		BanThreshold: DefaultBanThreshold,
	}, &keychain.KeyDescriptor{
		PubKey:     selfPrivKey.PubKey(),
		KeyLocator: testKeyLoc,
	})

	require.NoError(t, gossiper.Start())
	t.Cleanup(func() { require.NoError(t, gossiper.Stop()) })

	// Mark the graph as synced in order to allow the announcements to be
	// broadcast.
	gossiper.syncMgr.markGraphSynced()

	return gossiper, chain, notifier
}

// setupFuzzNetwork initializes a new fuzz testing environment for gossip
// network testing.
func setupFuzzNetwork(t *testing.T, data []byte) *fuzzNetwork {
	t.Helper()

	if !hasEnoughData(data, 0, 68) {
		return nil
	}

	// We ensure that the private/public key is valid according to the
	// secp256k1 curve so that there are no errors caused by an invalid key.
	btcPrivKey, isValid := ParsePrivKey(data[0:32])
	if !isValid {
		return nil
	}

	lnPrivKey, isValid := ParsePrivKey(data[32:64])
	if !isValid {
		return nil
	}

	blockHeight := getInt32(data[64:68]) % (blockHeightCap + 1)

	gossiper, chain, notifier := createGossiper(t, &blockHeight, lnPrivKey)

	return &fuzzNetwork{
		t:    t,
		data: data[68:],

		gossiper:    gossiper,
		chain:       chain,
		notifier:    notifier,
		blockHeight: &blockHeight,

		selfBtcPrivKey: btcPrivKey,
		selfLNPrivKey:  lnPrivKey,
		peers:          make([]*gossipPeer, 0),
	}
}

// hasEnoughData checks if there's sufficient data remaining.
func hasEnoughData(data []byte, offset, required int) bool {
	return offset+required <= len(data)
}

// selectPeer returns a peer from the network based on the selector byte.
// Returns nil if the peer list is empty.
func (fn *fuzzNetwork) selectPeer(selector byte) *gossipPeer {
	fn.t.Helper()

	if len(fn.peers) == 0 {
		return nil
	}
	peerIndex := int(selector) % len(fn.peers)

	return fn.peers[peerIndex]
}

// connectNewPeer creates a new peer connection and adds it to the network.
func (fn *fuzzNetwork) connectNewPeer(offset int) int {
	fn.t.Helper()

	if !hasEnoughData(fn.data, offset, 65) {
		return len(fn.data)
	}

	privKey, isValid := ParsePrivKey(fn.data[offset+1 : offset+33])
	if !isValid {
		return offset + 33
	}

	btcPrivKey, isValid := ParsePrivKey(fn.data[offset+33 : offset+65])
	if !isValid {
		return offset + 65
	}

	peer := &mockPeer{privKey.PubKey(), nil, nil, atomic.Bool{}}
	fn.gossiper.InitSyncState(peer)

	fn.peers = append(fn.peers, &gossipPeer{
		connection: peer,
		lnPrivKey:  privKey,
		btcPrivKey: btcPrivKey,
	})

	return offset + 65
}

// disconnectPeer removes a peer from the network based on fuzz data.
func (fn *fuzzNetwork) disconnectPeer(offset int) int {
	fn.t.Helper()

	if !hasEnoughData(fn.data, offset, 2) {
		return len(fn.data)
	}

	peerToDisconnect := fn.selectPeer(fn.data[offset+1])
	if peerToDisconnect == nil {
		return offset + 1
	}

	fn.gossiper.PruneSyncState(peerToDisconnect.connection.PubKey())

	// There is a possibility that we attempted to connect to the same peer
	// multiple times, resulting in duplicate entries in the mock peer list.
	// When removing a peer, we therefore filter out all entries that match
	// the peer's public key.
	var peerList []*gossipPeer
	disconnectPubKey := peerToDisconnect.lnPrivKey.PubKey()
	for _, peer := range fn.peers {
		if !disconnectPubKey.IsEqual(peer.lnPrivKey.PubKey()) {
			peerList = append(peerList, peer)
		}
	}
	fn.peers = peerList

	return offset + 2
}

// maybeMalformMessage conditionally mutates an lnwire message using fuzzing
// input data.
func (fn *fuzzNetwork) maybeMalformMessage(msg lnwire.Message,
	offset int) (lnwire.Message, int) {

	fn.t.Helper()

	if !hasEnoughData(fn.data, offset, 1) {
		return msg, offset
	}

	// Global selector.
	cursor := offset
	// skip malformation for even selector bytes.
	if fn.data[cursor]%2 == 0 {
		return msg, cursor + 1
	}
	cursor++

	canMutate := func(n int) bool {
		if !hasEnoughData(fn.data, cursor, n+1) {
			return false
		}

		allowed := (fn.data[cursor] % 2) == 0
		cursor++

		return allowed
	}

	switch m := msg.(type) {
	case *lnwire.NodeAnnouncement1:
		out := *m

		// NodeID
		if canMutate(33) {
			var nodeID [33]byte
			copy(nodeID[:], fn.data[cursor:cursor+33])
			out.NodeID = nodeID
			cursor += 33
		}

		// Signature
		if canMutate(64) {
			out.Signature, _ = lnwire.NewSigFromWireECDSA(
				fn.data[cursor : cursor+64],
			)
			cursor += 64
		}

		return &out, cursor

	case *lnwire.ChannelAnnouncement1:
		out := *m

		// NodeID1
		if canMutate(33) {
			var nodeID [33]byte
			copy(nodeID[:], fn.data[cursor:cursor+33])
			out.NodeID1 = nodeID
			cursor += 33
		}

		// NodeID2
		if canMutate(33) {
			var nodeID [33]byte
			copy(nodeID[:], fn.data[cursor:cursor+33])
			out.NodeID2 = nodeID
			cursor += 33
		}

		// BitcoinKey1
		if canMutate(33) {
			var btcKey [33]byte
			copy(btcKey[:], fn.data[cursor:cursor+33])
			out.BitcoinKey1 = btcKey
			cursor += 33
		}

		// BitcoinKey2
		if canMutate(33) {
			var btcKey [33]byte
			copy(btcKey[:], fn.data[cursor:cursor+33])
			out.BitcoinKey2 = btcKey
			cursor += 33
		}

		// NodeSig1
		if canMutate(64) {
			out.NodeSig1, _ = lnwire.NewSigFromWireECDSA(
				fn.data[cursor : cursor+64],
			)
			cursor += 64
		}

		// NodeSig2
		if canMutate(64) {
			out.NodeSig2, _ = lnwire.NewSigFromWireECDSA(
				fn.data[cursor : cursor+64],
			)
			cursor += 64
		}

		// BitcoinSig1
		if canMutate(64) {
			out.BitcoinSig1, _ = lnwire.NewSigFromWireECDSA(
				fn.data[cursor : cursor+64],
			)
			cursor += 64
		}

		// BitcoinSig2
		if canMutate(64) {
			out.BitcoinSig2, _ = lnwire.NewSigFromWireECDSA(
				fn.data[cursor : cursor+64],
			)
			cursor += 64
		}

		// ChainHash
		if canMutate(32) {
			var cHash [32]byte
			copy(cHash[:], fn.data[cursor:cursor+32])
			out.ChainHash = cHash
			cursor += 32
		}

		// ShortChannelID
		if canMutate(8) {
			scid := lnwire.NewShortChanIDFromInt(
				getUint64(fn.data[cursor : cursor+8]),
			)
			out.ShortChannelID = scid
			cursor += 8

			// Since the peer provided a malformed SCID, the local
			// on-chain lookups should fail.
			fn.chain.On("GetBlockHash", int64(scid.BlockHeight)).
				Return(nil, fmt.Errorf("block not found")).
				Once()
			fn.chain.On("GetBlock", tmock.Anything).
				Return(nil, fmt.Errorf("block not found")).
				Once()
			fn.chain.On(
				"GetUtxo", tmock.Anything, tmock.Anything,
				scid.BlockHeight, tmock.Anything,
			).Return(nil, fmt.Errorf("utxo not found")).Once()
		}

		return &out, cursor

	case *lnwire.ChannelUpdate1:
		out := *m

		// Signature
		if canMutate(64) {
			out.Signature, _ = lnwire.NewSigFromWireECDSA(
				fn.data[cursor : cursor+64],
			)
			cursor += 64
		}

		// ChainHash
		if canMutate(32) {
			var cHash [32]byte
			copy(cHash[:], fn.data[cursor:cursor+32])
			out.ChainHash = cHash
			cursor += 32
		}

		return &out, cursor

	case *lnwire.AnnounceSignatures1:
		out := *m

		// NodeSignature
		if canMutate(64) {
			out.NodeSignature, _ = lnwire.NewSigFromWireECDSA(
				fn.data[cursor : cursor+64],
			)
			cursor += 64
		}

		// BitcoinSignature
		if canMutate(64) {
			out.BitcoinSignature, _ = lnwire.NewSigFromWireECDSA(
				fn.data[cursor : cursor+64],
			)
			cursor += 64
		}

		// ShortChannelID
		if canMutate(8) {
			scid := lnwire.NewShortChanIDFromInt(
				getUint64(fn.data[cursor : cursor+8]),
			)
			out.ShortChannelID = scid
			cursor += 8
		}

		return &out, cursor

	case *lnwire.QueryShortChanIDs:
		out := *m

		// ChainHash
		if canMutate(32) {
			var cHash [32]byte
			copy(cHash[:], fn.data[cursor:cursor+32])
			out.ChainHash = cHash
			cursor += 32
		}

		// EncodingType
		if canMutate(1) {
			out.EncodingType = lnwire.QueryEncoding(fn.data[cursor])
			cursor++
		}

		return &out, cursor

	case *lnwire.QueryChannelRange:
		out := *m

		// ChainHash
		if canMutate(32) {
			var cHash [32]byte
			copy(cHash[:], fn.data[cursor:cursor+32])
			out.ChainHash = cHash
			cursor += 32
		}

		// QueryOptions
		if canMutate(1) {
			iterations := int(fn.data[cursor])
			var bits []lnwire.FeatureBit
			cursor++

			for range iterations {
				if !canMutate(2) {
					break
				}

				bits = append(bits, lnwire.FeatureBit(
					getUint16(fn.data[cursor:cursor+2]),
				))

				cursor += 2
			}

			fv := lnwire.NewRawFeatureVector(bits...)
			qopt := lnwire.QueryOptions(*fv)

			out.QueryOptions = &qopt
		}

		return &out, cursor

	case *lnwire.ReplyChannelRange:
		out := *m

		// ChainHash
		if canMutate(32) {
			var cHash [32]byte
			copy(cHash[:], fn.data[cursor:cursor+32])
			out.ChainHash = cHash
			cursor += 32
		}

		// EncodingType
		if canMutate(1) {
			out.EncodingType = lnwire.QueryEncoding(fn.data[cursor])
			cursor++
		}

		return &out, cursor

	case *lnwire.ReplyShortChanIDsEnd:
		out := *m

		// ChainHash
		if canMutate(32) {
			var cHash [32]byte
			copy(cHash[:], fn.data[cursor:cursor+32])
			out.ChainHash = cHash
			cursor += 32
		}

		return &out, cursor

	case *lnwire.GossipTimestampRange:
		out := *m

		// ChainHash
		if canMutate(32) {
			var cHash [32]byte
			copy(cHash[:], fn.data[cursor:cursor+32])
			out.ChainHash = cHash
			cursor += 32
		}

		return &out, cursor

	default:
		fn.t.Fatalf("received unexpected message while malformation: "+
			"%T (type=%d)", m, m.MsgType())

		return nil, cursor
	}
}

// sendRemoteNodeAnnouncement creates and processes a remote node announcement.
func (fn *fuzzNetwork) sendRemoteNodeAnnouncement(offset int) int {
	fn.t.Helper()

	if !hasEnoughData(fn.data, offset, 6) {
		return len(fn.data)
	}

	peer := fn.selectPeer(fn.data[offset+1])
	if peer == nil {
		return offset + 1
	}

	timestamp := getUint32(fn.data[offset+2 : offset+6])
	nodeAnn, err := createNodeAnnouncement(peer.lnPrivKey, timestamp)
	require.NoError(fn.t, err)

	malformedMsg, offset := fn.maybeMalformMessage(nodeAnn, offset+6)

	fn.gossiper.ProcessRemoteAnnouncement(
		fn.t.Context(), malformedMsg, peer.connection,
	)

	return offset
}

// createChannelAnnouncement builds a channel announcement with node and
// bitcoin keys populated.
func (fn *fuzzNetwork) createChannelAnnouncement(peer1, peer2 *gossipPeer,
	scid lnwire.ShortChannelID) *lnwire.ChannelAnnouncement1 {

	chanAnn := &lnwire.ChannelAnnouncement1{
		ChainHash:      *chaincfg.MainNetParams.GenesisHash,
		ShortChannelID: scid,
		Features:       testFeatures,
	}

	copy(chanAnn.NodeID1[:], peer1.lnPrivKey.PubKey().SerializeCompressed())
	copy(
		chanAnn.NodeID2[:],
		peer2.lnPrivKey.PubKey().SerializeCompressed(),
	)
	copy(
		chanAnn.BitcoinKey1[:],
		peer1.btcPrivKey.PubKey().SerializeCompressed(),
	)
	copy(
		chanAnn.BitcoinKey2[:],
		peer2.btcPrivKey.PubKey().SerializeCompressed(),
	)

	return chanAnn
}

// signChannelAnnouncement signs the channel announcement with all required
// signatures.
func (fn *fuzzNetwork) signChannelAnnouncement(peer1, peer2 *gossipPeer,
	chanAnn *lnwire.ChannelAnnouncement1) {

	signer := mock.SingleSigner{Privkey: peer1.lnPrivKey}
	sig, err := netann.SignAnnouncement(&signer, testKeyLoc, chanAnn)
	require.NoError(fn.t, err)
	chanAnn.NodeSig1, err = lnwire.NewSigFromSignature(sig)
	require.NoError(fn.t, err)

	signer = mock.SingleSigner{Privkey: peer2.lnPrivKey}
	sig, err = netann.SignAnnouncement(&signer, testKeyLoc, chanAnn)
	require.NoError(fn.t, err)
	chanAnn.NodeSig2, err = lnwire.NewSigFromSignature(sig)
	require.NoError(fn.t, err)

	signer = mock.SingleSigner{Privkey: peer1.btcPrivKey}
	sig, err = netann.SignAnnouncement(&signer, testKeyLoc, chanAnn)
	require.NoError(fn.t, err)
	chanAnn.BitcoinSig1, err = lnwire.NewSigFromSignature(sig)
	require.NoError(fn.t, err)

	signer = mock.SingleSigner{Privkey: peer2.btcPrivKey}
	sig, err = netann.SignAnnouncement(&signer, testKeyLoc, chanAnn)
	require.NoError(fn.t, err)
	chanAnn.BitcoinSig2, err = lnwire.NewSigFromSignature(sig)
	require.NoError(fn.t, err)
}

// setupMockChainForChannel mocks the chain backend for a given ShortChannelID
// by providing the expected block hash and a funding transaction, allowing the
// channel announcement to pass on-chain checks.
func (fn *fuzzNetwork) setupMockChainForChannel(peer1, peer2 *gossipPeer,
	scid lnwire.ShortChannelID) {

	fn.t.Helper()

	_, tx, err := input.GenFundingPkScript(
		peer1.btcPrivKey.PubKey().SerializeCompressed(),
		peer2.btcPrivKey.PubKey().SerializeCompressed(),
		int64(1000),
	)
	require.NoError(fn.t, err)

	fundingTx := wire.NewMsgTx(2)
	fundingTx.TxOut = append(fundingTx.TxOut, tx)

	// We will use the same hash for both the block and the funding
	// transaction to ensure that if the gossiper searches for the
	// transaction, it will definitely find it using this technique.
	chainHash := fundingTx.TxHash()

	// Mock the block hash lookup for the given block height to ensure the
	// channel's SCID can be resolved by the gossiper.
	fn.chain.On("GetBlockHash", int64(scid.BlockHeight)).
		Return(&chainHash, nil).Once()

	// Mock the block retrieval to return a block containing the funding
	// transaction.
	fundingBlock := &wire.MsgBlock{Transactions: []*wire.MsgTx{fundingTx}}

	fn.chain.On("GetBlock", &chainHash).
		Return(fundingBlock, nil).Once()

	// Mock the UTXO lookup for the funding outpoint to simulate an
	// unspent output, allowing funding validation to succeed.
	chanPoint := &wire.OutPoint{
		Hash:  fundingTx.TxHash(),
		Index: uint32(scid.TxPosition),
	}

	var tapscriptRoot fnopt.Option[chainhash.Hash]
	fundingPkScript, err := makeFundingScript(
		peer1.btcPrivKey.PubKey().SerializeCompressed(),
		peer2.btcPrivKey.PubKey().SerializeCompressed(),
		testFeatures,
		tapscriptRoot,
	)
	require.NoError(fn.t, err)

	fn.chain.On(
		"GetUtxo", chanPoint, fundingPkScript, scid.BlockHeight,
		tmock.Anything,
	).Return(tx, nil).Once()
}

// sendRemoteChannelAnnouncement creates and processes a remote channel
// announcement.
func (fn *fuzzNetwork) sendRemoteChannelAnnouncement(offset int) int {
	fn.t.Helper()

	if !hasEnoughData(fn.data, offset, 6) {
		return len(fn.data)
	}

	peer1 := fn.selectPeer(fn.data[offset+1])
	if peer1 == nil {
		return offset + 1
	}

	peer2 := fn.selectPeer(fn.data[offset+2])
	if peer2 == nil {
		return offset + 2
	}

	bh := uint32(fn.data[offset+3])<<16 | uint32(fn.data[offset+4])<<8 |
		uint32(fn.data[offset+5])
	scid := lnwire.ShortChannelID{BlockHeight: bh}
	fn.setupMockChainForChannel(peer1, peer2, scid)

	chanAnn := fn.createChannelAnnouncement(peer1, peer2, scid)
	fn.signChannelAnnouncement(peer1, peer2, chanAnn)

	malformedMsg, offset := fn.maybeMalformMessage(chanAnn, offset+6)

	fn.gossiper.ProcessRemoteAnnouncement(
		fn.t.Context(), malformedMsg, peer1.connection,
	)

	return offset
}

// sendRemoteChannelUpdate creates and processes a remote channel update.
func (fn *fuzzNetwork) sendRemoteChannelUpdate(offset int) int {
	fn.t.Helper()

	if !hasEnoughData(fn.data, offset, 50) {
		return len(fn.data)
	}

	peer := fn.selectPeer(fn.data[offset+1])
	if peer == nil {
		return offset + 1
	}

	scid := lnwire.NewShortChanIDFromInt(getUint64(
		fn.data[offset+2 : offset+10],
	))
	fee := lnwire.Fee{
		FeeRate: getInt32(fn.data[offset+10 : offset+14]),
		BaseFee: getInt32(fn.data[offset+14 : offset+18]),
	}

	updateAnn := &lnwire.ChannelUpdate1{
		ChainHash:      *chaincfg.MainNetParams.GenesisHash,
		ShortChannelID: scid,
		Timestamp:      getUint32(fn.data[offset+18 : offset+22]),
		MessageFlags:   lnwire.ChanUpdateMsgFlags(fn.data[offset+22]),
		ChannelFlags:   lnwire.ChanUpdateChanFlags(fn.data[offset+23]),
		TimeLockDelta:  getUint16(fn.data[offset+24 : offset+26]),
		HtlcMinimumMsat: lnwire.MilliSatoshi(getUint64(
			fn.data[offset+26 : offset+34],
		)),
		HtlcMaximumMsat: lnwire.MilliSatoshi(getUint64(
			fn.data[offset+34 : offset+42],
		)),
		FeeRate: getUint32(fn.data[offset+42 : offset+46]),
		BaseFee: getUint32(fn.data[offset+46 : offset+50]),
		InboundFee: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType55555](fee),
		),
	}

	err := signUpdate(peer.lnPrivKey, updateAnn)
	require.NoError(fn.t, err)

	malformedMsg, offset := fn.maybeMalformMessage(updateAnn, offset+50)

	fn.gossiper.ProcessRemoteAnnouncement(
		fn.t.Context(), malformedMsg, peer.connection,
	)

	return offset
}

// sendRemoteAnnounceSignatures creates and processes a remote announcement
// signature.
func (fn *fuzzNetwork) sendRemoteAnnounceSignatures(offset int) int {
	fn.t.Helper()

	if !hasEnoughData(fn.data, offset, 38) {
		return len(fn.data)
	}

	peer := fn.selectPeer(fn.data[offset+1])
	if peer == nil {
		return offset + 1
	}

	selfConn := mockPeer{fn.selfLNPrivKey.PubKey(), nil, nil, atomic.Bool{}}
	self := &gossipPeer{
		connection: &selfConn,
		lnPrivKey:  fn.selfLNPrivKey,
		btcPrivKey: fn.selfBtcPrivKey,
	}

	bh := uint32(fn.data[offset+2])<<16 | uint32(fn.data[offset+3])<<8 |
		uint32(fn.data[offset+4])
	scid := lnwire.ShortChannelID{BlockHeight: bh}
	var chanID [32]byte
	copy(chanID[:], fn.data[offset+5:offset+37])

	chanAnn := fn.createChannelAnnouncement(peer, self, scid)
	fn.signChannelAnnouncement(peer, self, chanAnn)

	// We will conditionally send the opposite side of the proof from our
	// local node.
	if fn.data[offset+37]%2 == 0 {
		// add channel to the Router's topology.
		fn.setupMockChainForChannel(peer, self, scid)
		select {
		case <-fn.gossiper.ProcessLocalAnnouncement(chanAnn):
		case <-fn.t.Context().Done():
		}

		// Also send our local AnnounceSignatures so that when the
		// remote peer sends theirs, both halves will be available for
		// assembly.
		annSign := &lnwire.AnnounceSignatures1{
			ChannelID:        chanID,
			ShortChannelID:   scid,
			NodeSignature:    chanAnn.NodeSig2,
			BitcoinSignature: chanAnn.BitcoinSig2,
		}
		select {
		case <-fn.gossiper.ProcessLocalAnnouncement(annSign):
		case <-fn.t.Context().Done():
		}
	}

	annSign := &lnwire.AnnounceSignatures1{
		ChannelID:        chanID,
		ShortChannelID:   scid,
		NodeSignature:    chanAnn.NodeSig1,
		BitcoinSignature: chanAnn.BitcoinSig1,
	}

	malformedMsg, offset := fn.maybeMalformMessage(annSign, offset+38)

	fn.gossiper.ProcessRemoteAnnouncement(
		fn.t.Context(), malformedMsg, peer.connection,
	)

	return offset
}

// sendRemoteQueryShortChanIDs creates and processes a remote query short
// channel IDs request.
func (fn *fuzzNetwork) sendRemoteQueryShortChanIDs(offset int) int {
	fn.t.Helper()

	if !hasEnoughData(fn.data, offset, 3) {
		return len(fn.data)
	}

	peer := fn.selectPeer(fn.data[offset+1])
	if peer == nil {
		return offset + 1
	}

	var scidsList []lnwire.ShortChannelID
	iterations := int(fn.data[offset+2])
	currentOffset := offset + 3

	for range iterations {
		if !hasEnoughData(fn.data, currentOffset, 8) {
			break
		}
		scidsList = append(
			scidsList,
			lnwire.NewShortChanIDFromInt(getUint64(
				fn.data[currentOffset:currentOffset+8],
			)),
		)
		currentOffset += 8
	}

	queryShortChanIDs := &lnwire.QueryShortChanIDs{
		ChainHash:    *chaincfg.MainNetParams.GenesisHash,
		EncodingType: lnwire.EncodingSortedPlain,
		ShortChanIDs: scidsList,
	}
	malformedMsg, offset := fn.maybeMalformMessage(
		queryShortChanIDs, currentOffset,
	)

	// Reply synchronously to peer queries to ensure they are processed
	// even when the fuzz data ends, and to reduce goroutine dependencies
	// and delays.
	syncer, ok := fn.gossiper.syncMgr.GossipSyncer(peer.connection.PubKey())
	require.True(fn.t, ok)
	_ = syncer.replyPeerQueries(fn.t.Context(), malformedMsg)

	return offset
}

// sendRemoteQueryChannelRange creates and processes a remote query channel
// range request.
func (fn *fuzzNetwork) sendRemoteQueryChannelRange(offset int) int {
	fn.t.Helper()

	if !hasEnoughData(fn.data, offset, 10) {
		return len(fn.data)
	}

	peer := fn.selectPeer(fn.data[offset+1])
	if peer == nil {
		return offset + 1
	}

	queryChannelRange := &lnwire.QueryChannelRange{
		ChainHash:        *chaincfg.MainNetParams.GenesisHash,
		FirstBlockHeight: getUint32(fn.data[offset+2 : offset+6]),
		NumBlocks:        getUint32(fn.data[offset+6 : offset+10]),
	}
	malformedMsg, offset := fn.maybeMalformMessage(
		queryChannelRange, offset+10,
	)

	// Reply synchronously to peer queries to ensure they are processed
	// even when the fuzz data ends, and to reduce goroutine dependencies
	// and delays.
	syncer, ok := fn.gossiper.syncMgr.GossipSyncer(peer.connection.PubKey())
	require.True(fn.t, ok)
	_ = syncer.replyPeerQueries(fn.t.Context(), malformedMsg)

	return offset
}

// sendRemoteReplyChannelRange creates and processes a remote reply channel
// range response.
func (fn *fuzzNetwork) sendRemoteReplyChannelRange(offset int) int {
	fn.t.Helper()

	if !hasEnoughData(fn.data, offset, 13) {
		return len(fn.data)
	}

	peer := fn.selectPeer(fn.data[offset+1])
	if peer == nil {
		return offset + 1
	}

	// To avoid filling up the gossip buffer and hanging the fuzz tests, we
	// first check if gossipMsgs has capacity to handle the message. This is
	// similar to the brontide message buffer: if the brontide buffer is
	// full, it waits until space frees up. Here, we simply skip sending the
	// message to reduce load on fuzz tests.
	syncer, ok := fn.gossiper.syncMgr.GossipSyncer(peer.connection.PubKey())
	require.True(fn.t, ok)

	if len(syncer.gossipMsgs)+1 >= cap(syncer.gossipMsgs) {
		return offset + 2
	}

	firstBlockHeight := getUint32(fn.data[offset+2 : offset+6])
	numBlocks := getUint32(fn.data[offset+6 : offset+10])
	complete := fn.data[offset+10]

	var scidsList []lnwire.ShortChannelID
	var timeStamps lnwire.Timestamps
	scidCount := int(fn.data[offset+11])
	withTimestamp := (fn.data[offset+12] % 2) == 0

	currentOffset := offset + 13
	requiredOffset := 8
	if withTimestamp {
		requiredOffset += 8
	}

	for range scidCount {
		if !hasEnoughData(fn.data, currentOffset, requiredOffset) {
			break
		}

		scidsList = append(
			scidsList, lnwire.NewShortChanIDFromInt(getUint64(
				fn.data[currentOffset:currentOffset+8],
			)),
		)
		currentOffset += 8

		if withTimestamp {
			timestamp1 := getUint32(
				fn.data[currentOffset : currentOffset+4],
			)
			timestamp2 := getUint32(
				fn.data[currentOffset+4 : currentOffset+8],
			)
			timestamp := lnwire.ChanUpdateTimestamps{
				Timestamp1: timestamp1,
				Timestamp2: timestamp2,
			}
			timeStamps = append(timeStamps, timestamp)
			currentOffset += 8
		}
	}

	replyChannelRange := &lnwire.ReplyChannelRange{
		ChainHash:        *chaincfg.MainNetParams.GenesisHash,
		FirstBlockHeight: firstBlockHeight,
		NumBlocks:        numBlocks,
		ShortChanIDs:     scidsList,
		Timestamps:       timeStamps,
		Complete:         complete,
	}

	malformedMsg, offset := fn.maybeMalformMessage(
		replyChannelRange, currentOffset,
	)

	fn.gossiper.ProcessRemoteAnnouncement(
		fn.t.Context(), malformedMsg, peer.connection,
	)

	return offset
}

// sendRemoteReplyShortChanIDsEnd creates and processes a remote reply short
// channel IDs end.
func (fn *fuzzNetwork) sendRemoteReplyShortChanIDsEnd(offset int) int {
	fn.t.Helper()

	if !hasEnoughData(fn.data, offset, 3) {
		return len(fn.data)
	}

	peer := fn.selectPeer(fn.data[offset+1])
	if peer == nil {
		return offset + 1
	}

	// To avoid filling up the gossip buffer and hanging the fuzz tests, we
	// first check if gossipMsgs has capacity to handle the message. This is
	// similar to the brontide message buffer: if the brontide buffer is
	// full, it waits until space frees up. Here, we simply skip sending the
	// message to reduce load on fuzz tests.
	syncer, ok := fn.gossiper.syncMgr.GossipSyncer(peer.connection.PubKey())
	require.True(fn.t, ok)

	if len(syncer.gossipMsgs)+1 >= cap(syncer.gossipMsgs) {
		return offset + 2
	}

	replyScidEnd := lnwire.ReplyShortChanIDsEnd{
		ChainHash: *chaincfg.MainNetParams.GenesisHash,
		Complete:  fn.data[offset+2],
	}

	malformedMsg, offset := fn.maybeMalformMessage(&replyScidEnd, offset+3)

	fn.gossiper.ProcessRemoteAnnouncement(
		fn.t.Context(), malformedMsg, peer.connection,
	)

	return offset
}

// sendRemoteGossipTimestampRange creates and processes a remote gossip
// timestamp range.
func (fn *fuzzNetwork) sendRemoteGossipTimestampRange(offset int) int {
	fn.t.Helper()

	if !hasEnoughData(fn.data, offset, 18) {
		return len(fn.data)
	}

	peer := fn.selectPeer(fn.data[offset+1])
	if peer == nil {
		return offset + 1
	}

	gossipTimestampRange := &lnwire.GossipTimestampRange{
		ChainHash:      *chaincfg.MainNetParams.GenesisHash,
		FirstTimestamp: getUint32(fn.data[offset+2 : offset+6]),
		TimestampRange: getUint32(fn.data[offset+6 : offset+10]),
		FirstBlockHeight: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType2](
				getUint32(fn.data[offset+10 : offset+14]),
			),
		),
		BlockRange: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType4](
				getUint32(fn.data[offset+14 : offset+18]),
			),
		),
	}
	malformedMsg, offset := fn.maybeMalformMessage(
		gossipTimestampRange, offset+18,
	)

	// Reply synchronously to peer queries to ensure they are processed
	// even when the fuzz data ends, and to reduce goroutine dependencies
	// and delays.
	syncer, ok := fn.gossiper.syncMgr.GossipSyncer(peer.connection.PubKey())
	require.True(fn.t, ok)

	filter, ok := malformedMsg.(*lnwire.GossipTimestampRange)
	require.True(fn.t, ok)
	_ = syncer.ApplyGossipFilter(fn.t.Context(), filter)

	return offset
}

// udpateBlockHeight updates the best known block height in the fuzz network.
// The new height is selected from the fuzz data and is guaranteed to be
// monotonically increasing.
func (fn *fuzzNetwork) udpateBlockHeight(offset int) int {
	fn.t.Helper()

	// Ensure we have enough data for updating block height.
	if !hasEnoughData(fn.data, offset, 5) {
		return len(fn.data)
	}

	*fn.blockHeight = max(
		getInt32(fn.data[offset+1:offset+5])%(blockHeightCap+1),
		*fn.blockHeight,
	)
	fn.notifier.notifyBlock(chainhash.Hash{}, uint32(*fn.blockHeight))

	return offset + 5
}

// startHistoricalSync triggers a historical sync on a peer. This is implemented
// as an explicit state transition so we can deterministically control when a
// forced historical sync happens, instead of relying on time-based triggers or
// randomly selected peers.
func (fn *fuzzNetwork) startHistoricalSync(offset int) int {
	fn.t.Helper()

	if !hasEnoughData(fn.data, offset, 2) {
		return len(fn.data)
	}

	peer := fn.selectPeer(fn.data[offset+1])
	if peer == nil {
		return offset + 1
	}

	syncer, ok := fn.gossiper.syncMgr.GossipSyncer(peer.connection.PubKey())
	require.True(fn.t, ok)

	// We only send the request if the syncer is in chansSynced state to
	// ensure the internal state machine remains consistent.
	if syncer.syncState() == chansSynced {
		done := make(chan struct{})

		select {
		case syncer.historicalSyncReqs <- &historicalSyncReq{
			doneChan: done,
		}:

			select {
			case <-done:
			case <-fn.t.Context().Done():
			}

		default:
		}
	}

	return offset + 2
}

// waitForValidationSemaphore blocks until the validation semaphore is fully
// restored, meaning all pending announcements have been processed. This
// emulates serial announcement processing rather than spawning multiple
// goroutines concurrently, which reduces stress on the fuzz tests and helps
// identify bugs where the semaphore is never released due to a deadlock or
// logic error.
func (fn *fuzzNetwork) waitForValidationSemaphore() {
	vb := fn.gossiper.vb
	for len(vb.validationSemaphore) < cap(vb.validationSemaphore) {
		select {
		case <-fn.t.Context().Done():
			return
		default:
		}
	}
}

// runGossipStateMachine executes the gossip state machine with fuzz input data.
func (fn *fuzzNetwork) runGossipStateMachine() {
	fn.t.Helper()

	for offset := 0; offset < len(fn.data); {
		// Extract action from current data byte
		action := fuzzState(int(fn.data[offset]) % numFuzzStates)

		switch action {
		case connectPeer:
			offset = fn.connectNewPeer(offset)

		case disconnectPeer:
			offset = fn.disconnectPeer(offset)

		case nodeAnnouncementReceived:
			offset = fn.sendRemoteNodeAnnouncement(offset)

		case channelAnnouncementReceived:
			offset = fn.sendRemoteChannelAnnouncement(offset)

		case channelUpdateReceived:
			offset = fn.sendRemoteChannelUpdate(offset)

		case announcementSignaturesReceived:
			offset = fn.sendRemoteAnnounceSignatures(offset)

		case queryShortChanIDsReceived:
			offset = fn.sendRemoteQueryShortChanIDs(offset)

		case queryChannelRangeReceived:
			offset = fn.sendRemoteQueryChannelRange(offset)

		case replyChannelRangeReceived:
			offset = fn.sendRemoteReplyChannelRange(offset)

		case gossipTimestampRangeReceived:
			offset = fn.sendRemoteGossipTimestampRange(offset)

		case replyShortChanIDsEndReceived:
			offset = fn.sendRemoteReplyShortChanIDsEnd(offset)

		case udpateBlockHeight:
			offset = fn.udpateBlockHeight(offset)

		case triggerHistoricalSync:
			offset = fn.startHistoricalSync(offset)
		}

		fn.waitForValidationSemaphore()
	}
}

// FuzzGossipStateMachine fuzzes the gossip state machine by consuming arbitrary
// input and performing operations such as node and channel announcements, peer
// connections, and other protocol interactions.
func FuzzGossipStateMachine(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		fn := setupFuzzNetwork(t, data)
		if fn == nil {
			return
		}

		fn.runGossipStateMachine()
	})
}
