package discovery

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image/color"
	"math"
	"net"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
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

const (
	// For the fuzz tests, cap the block height to roughly the year 2142.
	blockHeightCap = 6990480

	// Maximum limits on channel capacity.
	maxFundingAmt = lnwire.MilliSatoshi(16777215000)
)

// getUint64 extracts a uint64 from a byte slice.
func getUint64(data []byte) uint64 {
	return binary.BigEndian.Uint64(data)
}

// getUint32 extracts a uint32 from a byte slice.
func getUint32(data []byte) uint32 {
	return binary.BigEndian.Uint32(data)
}

// getUint16 extracts a uint16 from a byte slice.
func getUint16(data []byte) uint16 {
	return binary.BigEndian.Uint16(data)
}

// getInt32 extracts a non-negative int32 from a byte slice.
func getInt32(data []byte) int32 {
	return int32(getUint32(data) % uint32(math.MaxInt32+1))
}

// ParsePrivKey parses raw private key bytes and returns the parsed private key,
// along with a boolean indicating whether the provided bytes represent a valid
// private key.
//
// NOTE: Ideally, this should be placed in btcd, but for now it is defined here.
func ParsePrivKey(privKeyBytes []byte) (*btcec.PrivateKey, bool) {
	var key btcec.ModNScalar
	overflows := key.SetByteSlice(privKeyBytes)
	if overflows || key.IsZero() {
		return nil, false
	}

	return btcec.PrivKeyFromScalar(&key), true
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
	updateBlockHeight
	triggerHistoricalSync
	numFuzzStates
)

// gossipPeer represents a mock peer along with its associated LN and BTC
// private keys.
type gossipPeer struct {
	connection *mockPeer
	lnPrivKey  *btcec.PrivateKey
	btcPrivKey *btcec.PrivateKey
}

// scidLookup returns the highest known ShortChannelID in the channel graph.
type scidLookup func(context.Context, lnwire.GossipVersion) (uint64, error)

// fuzzNetwork represents a test network harness used to fuzz the gossip
// subsystem between the local node and remote peers.
type fuzzNetwork struct {
	t      *testing.T
	data   []byte
	offset int

	gossiper     *AuthenticatedGossiper
	chain        *lnmock.MockChain
	notifier     *mockNotifier
	blockHeight  *int32
	getKnownSCID scidLookup

	selfBtcPrivKey *btcec.PrivateKey
	selfLNPrivKey  *btcec.PrivateKey
	peers          []*gossipPeer
}

// createGossiper creates and starts a gossiper for fuzz testing.
func createGossiper(t *testing.T, blockHeight *int32,
	selfPrivKey *btcec.PrivateKey) (*AuthenticatedGossiper,
	*lnmock.MockChain, *mockNotifier, scidLookup) {

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
		LastUpdate: time.Now(),
		Addresses:  testAddrs,
		Alias:      "kek" + hex.EncodeToString(pubKey[:]),
		Features:   testFeatures,
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

	return gossiper, chain, notifier, graphDb.HighestChanID
}

// setupFuzzNetwork initializes a new fuzz testing environment for gossip
// network testing.
func setupFuzzNetwork(t *testing.T, data []byte) *fuzzNetwork {
	if len(data) < 68 {
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

	gossiper, chain, notifier, getKnownSCID := createGossiper(
		t, &blockHeight, lnPrivKey,
	)

	return &fuzzNetwork{
		t:      t,
		data:   data,
		offset: 68,

		gossiper:     gossiper,
		chain:        chain,
		notifier:     notifier,
		blockHeight:  &blockHeight,
		getKnownSCID: getKnownSCID,

		selfBtcPrivKey: btcPrivKey,
		selfLNPrivKey:  lnPrivKey,
		peers:          make([]*gossipPeer, 0),
	}
}

// getBytes returns the next required bytes from the fuzz input and advances the
// offset.
func (fn *fuzzNetwork) getBytes(required int) []byte {
	b := fn.data[fn.offset : fn.offset+required]
	fn.offset += required

	return b
}

// getVarBytes reads a 2-byte length-prefixed byte slice from the fuzz input,
// returning the length prefix and payload concatenated, or nil if exhausted.
func (fn *fuzzNetwork) getVarBytes() []byte {
	if !fn.hasEnoughData(2) {
		return nil
	}

	lengthBytes := fn.getBytes(2)
	length := int(getUint16(lengthBytes))

	if !fn.hasEnoughData(length) {
		return nil
	}

	return append(lengthBytes, fn.getBytes(length)...)
}

// genUTXOLookupSCID generates a ShortChannelID used by the gossiper to validate
// the funding transaction from a ChannelAnnouncement.
//
// We set TxIndex and TxPosition to 0 because, during verification, the gossiper
// uses these as indices to locate the funding transaction and its output within
// the block. By keeping them at 0, we avoid the need to populate unnecessary
// transactions in the block.
func (fn *fuzzNetwork) genUTXOLookupSCID() lnwire.ShortChannelID {
	bh := uint32(fn.getBytes(1)[0])<<16 | uint32(fn.getBytes(1)[0])<<8 |
		uint32(fn.getBytes(1)[0])

	return lnwire.ShortChannelID{BlockHeight: bh}
}

// getFeatures reads a length-prefixed RawFeatureVector from fuzz input.
func (fn *fuzzNetwork) getFeatures() *lnwire.RawFeatureVector {
	fv := lnwire.NewRawFeatureVector()
	buf := bytes.NewBuffer(fn.getVarBytes())
	_ = lnwire.ReadElement(buf, &fv)

	return fv
}

// getAddresses reads a length-prefixed slice of net.Addr from fuzz input.
func (fn *fuzzNetwork) getAddresses() []net.Addr {
	var addresses []net.Addr
	buf := bytes.NewBuffer(fn.getVarBytes())
	_ = lnwire.ReadElement(buf, &addresses)

	return addresses
}

// hasEnoughData checks if there's sufficient data remaining.
func (fn *fuzzNetwork) hasEnoughData(required int) bool {
	return fn.offset+required <= len(fn.data)
}

// selectPeer returns a peer from the network based on the selector byte.
// Returns nil if the peer list is empty.
func (fn *fuzzNetwork) selectPeer() *gossipPeer {
	if len(fn.peers) == 0 {
		return nil
	}
	peerIndex := int(fn.getBytes(1)[0]) % len(fn.peers)

	return fn.peers[peerIndex]
}

// connectNewPeer creates a new peer connection and adds it to the network.
func (fn *fuzzNetwork) connectNewPeer() {
	if !fn.hasEnoughData(64) {
		return
	}

	privKey, isValid := ParsePrivKey(fn.getBytes(32))
	if !isValid {
		return
	}

	// If we already have a connection with this peer, we will not try to
	// connect to it again. In production, if this happens, the SyncManager
	// will silently drop the connection request.
	_, ok := fn.gossiper.SyncManager().GossipSyncer(
		route.Vertex(privKey.PubKey().SerializeCompressed()),
	)
	if ok {
		return
	}

	btcPrivKey, isValid := ParsePrivKey(fn.getBytes(32))
	if !isValid {
		return
	}

	peer := &mockPeer{privKey.PubKey(), nil, nil, atomic.Bool{}}
	fn.gossiper.InitSyncState(peer)

	fn.peers = append(fn.peers, &gossipPeer{
		connection: peer,
		lnPrivKey:  privKey,
		btcPrivKey: btcPrivKey,
	})
}

// disconnectPeer removes a peer from the network based on fuzz data.
func (fn *fuzzNetwork) disconnectPeer() {
	if !fn.hasEnoughData(1) || len(fn.peers) == 0 {
		return
	}

	peerIndex := int(fn.getBytes(1)[0]) % len(fn.peers)
	peerToDisconnect := fn.peers[peerIndex]

	fn.gossiper.PruneSyncState(peerToDisconnect.connection.PubKey())

	// Remove the selected peer in the mock peer list.
	fn.peers = append(fn.peers[:peerIndex], fn.peers[peerIndex+1:]...)
}

// maybeMalformMessage conditionally mutates an lnwire message using fuzzing
// input data.
func (fn *fuzzNetwork) maybeMalformMessage(msg lnwire.Message) lnwire.Message {
	if !fn.hasEnoughData(1) {
		return msg
	}

	// skip malformation for even selector bytes.
	if fn.getBytes(1)[0]%2 == 0 {
		return msg
	}

	canMutate := func(n int) bool {
		if !fn.hasEnoughData(n + 1) {
			return false
		}

		allowed := (fn.getBytes(1)[0] % 2) == 0

		return allowed
	}

	mayBeMutate := func(mut func([]byte), size int) {
		if !canMutate(size) {
			return
		}

		mut(fn.getBytes(size))
	}

	switch m := msg.(type) {
	case *lnwire.NodeAnnouncement1:
		out := *m

		// NodeID
		mayBeMutate(func(b []byte) { out.NodeID = [33]byte(b) }, 33)

		// Signature
		mayBeMutate(
			func(b []byte) {
				out.Signature, _ = lnwire.NewSigFromWireECDSA(b)
			}, 64,
		)

		return &out

	case *lnwire.ChannelAnnouncement1:
		out := *m

		// NodeID1
		mayBeMutate(func(b []byte) { out.NodeID1 = [33]byte(b) }, 33)

		// NodeID2
		mayBeMutate(func(b []byte) { out.NodeID2 = [33]byte(b) }, 33)

		// BitcoinKey1
		mayBeMutate(
			func(b []byte) { out.BitcoinKey1 = [33]byte(b) }, 33,
		)

		// BitcoinKey2
		mayBeMutate(
			func(b []byte) { out.BitcoinKey2 = [33]byte(b) }, 33,
		)

		// NodeSig1
		mayBeMutate(
			func(b []byte) {
				out.NodeSig1, _ = lnwire.NewSigFromWireECDSA(b)
			}, 64,
		)

		// NodeSig2
		mayBeMutate(
			func(b []byte) {
				out.NodeSig2, _ = lnwire.NewSigFromWireECDSA(b)
			}, 64,
		)

		// BitcoinSig1
		mayBeMutate(
			func(b []byte) {
				out.BitcoinSig1, _ = lnwire.NewSigFromWireECDSA(
					b,
				)
			}, 64,
		)

		// BitcoinSig2
		mayBeMutate(
			func(b []byte) {
				out.BitcoinSig2, _ = lnwire.NewSigFromWireECDSA(
					b,
				)
			}, 64,
		)

		// ChainHash
		mayBeMutate(func(b []byte) { out.ChainHash = [32]byte(b) }, 32)

		return &out

	case *lnwire.ChannelUpdate1:
		out := *m

		// Signature
		mayBeMutate(
			func(b []byte) {
				out.Signature, _ = lnwire.NewSigFromWireECDSA(b)
			}, 64,
		)

		// ChainHash
		mayBeMutate(func(b []byte) { out.ChainHash = [32]byte(b) }, 32)

		// MessageFlags
		mayBeMutate(func(b []byte) {
			out.MessageFlags = lnwire.ChanUpdateMsgFlags(b[0])
		}, 1)

		return &out

	case *lnwire.AnnounceSignatures1:
		out := *m

		// NodeSignature
		mayBeMutate(
			func(b []byte) {
				out.NodeSignature, _ = lnwire.
					NewSigFromWireECDSA(b)
			}, 64,
		)

		// BitcoinSignature
		mayBeMutate(
			func(b []byte) {
				out.BitcoinSignature, _ = lnwire.
					NewSigFromWireECDSA(b)
			}, 64,
		)

		return &out

	case *lnwire.QueryShortChanIDs:
		out := *m

		// ChainHash
		mayBeMutate(func(b []byte) { out.ChainHash = [32]byte(b) }, 32)

		// EncodingType
		mayBeMutate(
			func(b []byte) {
				out.EncodingType = lnwire.QueryEncoding(b[0])
			}, 1,
		)

		return &out

	case *lnwire.QueryChannelRange:
		out := *m

		// ChainHash
		mayBeMutate(func(b []byte) { out.ChainHash = [32]byte(b) }, 32)

		return &out

	case *lnwire.ReplyChannelRange:
		out := *m

		// ChainHash
		mayBeMutate(func(b []byte) { out.ChainHash = [32]byte(b) }, 32)

		// EncodingType
		mayBeMutate(
			func(b []byte) {
				out.EncodingType = lnwire.QueryEncoding(b[0])
			}, 1,
		)

		return &out

	case *lnwire.ReplyShortChanIDsEnd:
		out := *m

		// ChainHash
		mayBeMutate(func(b []byte) { out.ChainHash = [32]byte(b) }, 32)

		return &out

	case *lnwire.GossipTimestampRange:
		out := *m

		// ChainHash
		mayBeMutate(func(b []byte) { out.ChainHash = [32]byte(b) }, 32)

		return &out

	default:
		fn.t.Fatalf("received unexpected message while malformation: "+
			"%T (type=%d)", m, m.MsgType())

		return nil
	}
}

// createNodeAnnouncement builds a node announcement.
func (fn *fuzzNetwork) createNodeAnnouncement(
	peer *gossipPeer) *lnwire.NodeAnnouncement1 {

	nodeAnn := &lnwire.NodeAnnouncement1{
		Timestamp: getUint32(fn.getBytes(4)),
		Alias:     lnwire.NodeAlias(fn.getBytes(32)),
		RGBColor: color.RGBA{
			R: fn.getBytes(1)[0],
			G: fn.getBytes(1)[0],
			B: fn.getBytes(1)[0],
			A: fn.getBytes(1)[0],
		},
		Features:  fn.getFeatures(),
		Addresses: fn.getAddresses(),
	}
	copy(nodeAnn.NodeID[:], peer.lnPrivKey.PubKey().SerializeCompressed())

	signer := mock.SingleSigner{Privkey: peer.lnPrivKey}
	sig, err := netann.SignAnnouncement(&signer, testKeyLoc, nodeAnn)
	require.NoError(fn.t, err)

	nodeAnn.Signature, err = lnwire.NewSigFromSignature(sig)
	require.NoError(fn.t, err)

	return nodeAnn
}

// sendRemoteNodeAnnouncement creates and processes a remote node announcement.
func (fn *fuzzNetwork) sendRemoteNodeAnnouncement() {
	if !fn.hasEnoughData(42) {
		return
	}

	peer1 := fn.selectPeer()
	if peer1 == nil {
		return
	}

	peer2 := fn.selectPeer()
	if peer2 == nil {
		return
	}

	nodeAnn := fn.createNodeAnnouncement(peer1)
	malformedMsg := fn.maybeMalformMessage(nodeAnn)

	fn.gossiper.ProcessRemoteAnnouncement(
		fn.t.Context(), malformedMsg, peer2.connection,
	)
}

// createChannelAnnouncement builds a channel announcement with node and
// bitcoin keys populated.
func (fn *fuzzNetwork) createChannelAnnouncement(peer1, peer2 *gossipPeer,
	scid lnwire.ShortChannelID) *lnwire.ChannelAnnouncement1 {

	chanAnn := &lnwire.ChannelAnnouncement1{
		ChainHash:      *chaincfg.MainNetParams.GenesisHash,
		ShortChannelID: scid,
		Features:       fn.getFeatures(),
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

	_, tx, err := input.GenFundingPkScript(
		peer1.btcPrivKey.PubKey().SerializeCompressed(),
		peer2.btcPrivKey.PubKey().SerializeCompressed(),
		10000000,
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
func (fn *fuzzNetwork) sendRemoteChannelAnnouncement() {
	if !fn.hasEnoughData(6) {
		return
	}

	peer1 := fn.selectPeer()
	if peer1 == nil {
		return
	}

	peer2 := fn.selectPeer()
	if peer2 == nil {
		return
	}

	peer3 := fn.selectPeer()
	if peer3 == nil {
		return
	}

	scid := fn.genUTXOLookupSCID()

	chanAnn := fn.createChannelAnnouncement(peer1, peer2, scid)
	fn.signChannelAnnouncement(peer1, peer2, chanAnn)

	malformedMsg := fn.maybeMalformMessage(chanAnn)

	// We will only register the chain lookups if we have not malformed the
	// channel announcement message, since the chain lookup happens at the
	// end once all the fields are validated. This is done to ensure that we
	// do not poison future messages due to an expected chain lookup that
	// was never hit.
	if reflect.DeepEqual(chanAnn, malformedMsg) {
		// We conditionally allow the chain lookup to either pass or
		// fail.
		if fn.hasEnoughData(1) && fn.getBytes(1)[0]%2 == 0 {
			fn.setupMockChainForChannel(peer1, peer2, scid)
		} else {
			// the local on-chain lookups should fail.
			fn.chain.On("GetBlockHash", int64(scid.BlockHeight)).
				Return(nil, fmt.Errorf("block not found")).
				Once()
		}
	}

	fn.gossiper.ProcessRemoteAnnouncement(
		fn.t.Context(), malformedMsg, peer3.connection,
	)
}

// sendRemoteChannelUpdate creates and processes a remote channel update.
func (fn *fuzzNetwork) sendRemoteChannelUpdate() {
	if !fn.hasEnoughData(51) {
		return
	}

	peer1 := fn.selectPeer()
	if peer1 == nil {
		return
	}

	peer2 := fn.selectPeer()
	if peer2 == nil {
		return
	}

	// We will conditionally, return a previously known SCID obtained from a
	// valid channel announcement. Otherwise, generate a random SCID from
	// fuzz input.
	getSCID := func() lnwire.ShortChannelID {
		if fn.getBytes(1)[0]%2 == 0 {
			if chanID, err := fn.getKnownSCID(
				fn.t.Context(), lnwire.GossipVersion1,
			); err == nil {
				return lnwire.NewShortChanIDFromInt(chanID)
			}
		}

		return lnwire.NewShortChanIDFromInt(getUint64(fn.getBytes(8)))
	}

	updateAnn := &lnwire.ChannelUpdate1{
		ChainHash:      *chaincfg.MainNetParams.GenesisHash,
		ShortChannelID: getSCID(),
		Timestamp:      getUint32(fn.getBytes(4)),
		MessageFlags:   1,
		ChannelFlags:   lnwire.ChanUpdateChanFlags(fn.getBytes(1)[0]),
		TimeLockDelta:  getUint16(fn.getBytes(2)),
		HtlcMinimumMsat: lnwire.MilliSatoshi(
			getUint64(fn.getBytes(8)),
		) % (maxFundingAmt + 1),
		HtlcMaximumMsat: lnwire.MilliSatoshi(
			getUint64(fn.getBytes(8)),
		) % (maxFundingAmt + 1),
		FeeRate: getUint32(fn.getBytes(4)),
		BaseFee: getUint32(fn.getBytes(4)),
	}

	if fn.getBytes(1)[0]%2 == 0 {
		fee := lnwire.Fee{
			FeeRate: getInt32(fn.getBytes(4)),
			BaseFee: getInt32(fn.getBytes(4)),
		}
		updateAnn.InboundFee = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType55555](fee),
		)
	}

	err := signUpdate(peer1.lnPrivKey, updateAnn)
	require.NoError(fn.t, err)

	malformedMsg := fn.maybeMalformMessage(updateAnn)

	fn.gossiper.ProcessRemoteAnnouncement(
		fn.t.Context(), malformedMsg, peer2.connection,
	)
}

// sendRemoteAnnounceSignatures creates and processes a remote announcement
// signature.
func (fn *fuzzNetwork) sendRemoteAnnounceSignatures() {
	if !fn.hasEnoughData(36) {
		return
	}

	peer := fn.selectPeer()
	if peer == nil {
		return
	}

	selfConn := mockPeer{fn.selfLNPrivKey.PubKey(), nil, nil, atomic.Bool{}}
	self := &gossipPeer{
		connection: &selfConn,
		lnPrivKey:  fn.selfLNPrivKey,
		btcPrivKey: fn.selfBtcPrivKey,
	}

	scid := fn.genUTXOLookupSCID()
	chanID := [32]byte(fn.getBytes(32))

	chanAnn := fn.createChannelAnnouncement(peer, self, scid)
	fn.signChannelAnnouncement(peer, self, chanAnn)

	// We will conditionally send the opposite side of the proof from our
	// local node.
	if fn.hasEnoughData(1) && fn.getBytes(1)[0]%2 == 0 {
		// add channel to the Router's topology.
		fn.setupMockChainForChannel(peer, self, scid)

		_ = AwaitGossipResult(
			fn.t.Context(),
			fn.gossiper.ProcessLocalAnnouncement(chanAnn),
		)

		// Also send our local AnnounceSignatures so that when the
		// remote peer sends theirs, both halves will be available for
		// assembly.
		annSign := &lnwire.AnnounceSignatures1{
			ChannelID:        chanID,
			ShortChannelID:   scid,
			NodeSignature:    chanAnn.NodeSig2,
			BitcoinSignature: chanAnn.BitcoinSig2,
		}
		_ = AwaitGossipResult(
			fn.t.Context(),
			fn.gossiper.ProcessLocalAnnouncement(annSign),
		)
	}

	annSign := &lnwire.AnnounceSignatures1{
		ChannelID:        chanID,
		ShortChannelID:   scid,
		NodeSignature:    chanAnn.NodeSig1,
		BitcoinSignature: chanAnn.BitcoinSig1,
	}

	malformedMsg := fn.maybeMalformMessage(annSign)

	fn.gossiper.ProcessRemoteAnnouncement(
		fn.t.Context(), malformedMsg, peer.connection,
	)
}

// sendRemoteQueryShortChanIDs creates and processes a remote query short
// channel IDs request.
func (fn *fuzzNetwork) sendRemoteQueryShortChanIDs() {
	if !fn.hasEnoughData(2) {
		return
	}

	peer := fn.selectPeer()
	if peer == nil {
		return
	}

	var scidsList []lnwire.ShortChannelID
	iterations := fn.getBytes(1)[0]

	for range iterations {
		if !fn.hasEnoughData(8) {
			break
		}
		scidsList = append(
			scidsList,
			lnwire.NewShortChanIDFromInt(getUint64(fn.getBytes(8))),
		)
	}

	queryShortChanIDs := &lnwire.QueryShortChanIDs{
		ChainHash:    *chaincfg.MainNetParams.GenesisHash,
		EncodingType: lnwire.EncodingSortedPlain,
		ShortChanIDs: scidsList,
	}
	malformedMsg := fn.maybeMalformMessage(queryShortChanIDs)

	// Reply synchronously to peer queries to ensure they are processed
	// even when the fuzz data ends, and to reduce goroutine dependencies
	// and delays.
	syncer, ok := fn.gossiper.syncMgr.GossipSyncer(peer.connection.PubKey())
	require.True(fn.t, ok)
	_ = syncer.replyPeerQueries(fn.t.Context(), malformedMsg)
}

// sendRemoteQueryChannelRange creates and processes a remote query channel
// range request.
func (fn *fuzzNetwork) sendRemoteQueryChannelRange() {
	if !fn.hasEnoughData(10) {
		return
	}

	peer := fn.selectPeer()
	if peer == nil {
		return
	}

	queryChannelRange := &lnwire.QueryChannelRange{
		ChainHash:        *chaincfg.MainNetParams.GenesisHash,
		FirstBlockHeight: getUint32(fn.getBytes(4)),
		NumBlocks:        getUint32(fn.getBytes(4)),
	}

	if fn.getBytes(1)[0]%2 == 0 {
		qopt := lnwire.QueryOptions(*fn.getFeatures())
		queryChannelRange.QueryOptions = &qopt
	}

	malformedMsg := fn.maybeMalformMessage(queryChannelRange)

	// Reply synchronously to peer queries to ensure they are processed
	// even when the fuzz data ends, and to reduce goroutine dependencies
	// and delays.
	syncer, ok := fn.gossiper.syncMgr.GossipSyncer(peer.connection.PubKey())
	require.True(fn.t, ok)
	_ = syncer.replyPeerQueries(fn.t.Context(), malformedMsg)
}

// sendRemoteReplyChannelRange creates and processes a remote reply channel
// range response.
func (fn *fuzzNetwork) sendRemoteReplyChannelRange() {
	if !fn.hasEnoughData(12) {
		return
	}

	peer := fn.selectPeer()
	if peer == nil {
		return
	}

	// To avoid filling up the gossip buffer and hanging the fuzz tests, we
	// first check if gossipMsgs has capacity to handle the message. This is
	// similar to the brontide message buffer: if the brontide buffer is
	// full, it waits until space frees up. Here, we simply skip sending the
	// message to reduce load on fuzz tests.
	syncer, ok := fn.gossiper.syncMgr.GossipSyncer(peer.connection.PubKey())
	require.True(fn.t, ok)

	if len(syncer.gossipMsgs)+1 >= cap(syncer.gossipMsgs) {
		return
	}

	firstBlockHeight := getUint32(fn.getBytes(4))
	numBlocks := getUint32(fn.getBytes(4))
	complete := fn.getBytes(1)[0]

	var scidsList []lnwire.ShortChannelID
	var timestamps lnwire.Timestamps
	scidCount := fn.getBytes(1)[0]
	withTimestamp := (fn.getBytes(1)[0] % 2) == 0

	requiredLen := 8
	if withTimestamp {
		requiredLen += 8
	}

	for range scidCount {
		if !fn.hasEnoughData(requiredLen) {
			break
		}

		scidsList = append(
			scidsList, lnwire.NewShortChanIDFromInt(
				getUint64(fn.getBytes(8)),
			),
		)

		if withTimestamp {
			timestamp := lnwire.ChanUpdateTimestamps{
				Timestamp1: getUint32(fn.getBytes(4)),
				Timestamp2: getUint32(fn.getBytes(4)),
			}
			timestamps = append(timestamps, timestamp)
		}
	}

	replyChannelRange := &lnwire.ReplyChannelRange{
		ChainHash:        *chaincfg.MainNetParams.GenesisHash,
		FirstBlockHeight: firstBlockHeight,
		NumBlocks:        numBlocks,
		ShortChanIDs:     scidsList,
		Timestamps:       timestamps,
		Complete:         complete,
	}

	malformedMsg := fn.maybeMalformMessage(replyChannelRange)

	fn.gossiper.ProcessRemoteAnnouncement(
		fn.t.Context(), malformedMsg, peer.connection,
	)
}

// sendRemoteReplyShortChanIDsEnd creates and processes a remote reply short
// channel IDs end.
func (fn *fuzzNetwork) sendRemoteReplyShortChanIDsEnd() {
	if !fn.hasEnoughData(2) {
		return
	}

	peer := fn.selectPeer()
	if peer == nil {
		return
	}

	// To avoid filling up the gossip buffer and hanging the fuzz tests, we
	// first check if gossipMsgs has capacity to handle the message. This is
	// similar to the brontide message buffer: if the brontide buffer is
	// full, it waits until space frees up. Here, we simply skip sending the
	// message to reduce load on fuzz tests.
	syncer, ok := fn.gossiper.syncMgr.GossipSyncer(peer.connection.PubKey())
	require.True(fn.t, ok)

	if len(syncer.gossipMsgs)+1 >= cap(syncer.gossipMsgs) {
		return
	}

	replyScidEnd := lnwire.ReplyShortChanIDsEnd{
		ChainHash: *chaincfg.MainNetParams.GenesisHash,
		Complete:  fn.getBytes(1)[0],
	}

	malformedMsg := fn.maybeMalformMessage(&replyScidEnd)

	fn.gossiper.ProcessRemoteAnnouncement(
		fn.t.Context(), malformedMsg, peer.connection,
	)
}

// sendRemoteGossipTimestampRange creates and processes a remote gossip
// timestamp range.
func (fn *fuzzNetwork) sendRemoteGossipTimestampRange() {
	if !fn.hasEnoughData(19) {
		return
	}

	peer := fn.selectPeer()
	if peer == nil {
		return
	}

	gossipTimestampRange := &lnwire.GossipTimestampRange{
		ChainHash:      *chaincfg.MainNetParams.GenesisHash,
		FirstTimestamp: getUint32(fn.getBytes(4)),
		TimestampRange: getUint32(fn.getBytes(4)),
	}

	if fn.getBytes(1)[0]%2 == 0 {
		gossipTimestampRange.FirstBlockHeight = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType2](
				getUint32(fn.getBytes(4)),
			),
		)
	}

	if fn.getBytes(1)[0]%2 == 0 {
		gossipTimestampRange.BlockRange = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType4](
				getUint32(fn.getBytes(4)),
			),
		)
	}
	malformedMsg := fn.maybeMalformMessage(gossipTimestampRange)

	// Reply synchronously to peer queries to ensure they are processed
	// even when the fuzz data ends, and to reduce goroutine dependencies
	// and delays.
	syncer, ok := fn.gossiper.syncMgr.GossipSyncer(peer.connection.PubKey())
	require.True(fn.t, ok)

	filter, ok := malformedMsg.(*lnwire.GossipTimestampRange)
	require.True(fn.t, ok)
	_ = syncer.ApplyGossipFilter(fn.t.Context(), filter)
}

// updateBlockHeight updates the best known block height in the fuzz network.
// The new height is selected from the fuzz data and is guaranteed to be
// monotonically increasing.
func (fn *fuzzNetwork) updateBlockHeight() {
	// Ensure we have enough data for updating block height.
	if !fn.hasEnoughData(4) {
		return
	}

	*fn.blockHeight = max(
		getInt32(fn.getBytes(4))%(blockHeightCap+1),
		*fn.blockHeight,
	)
	fn.notifier.notifyBlock(chainhash.Hash{}, uint32(*fn.blockHeight))
}

// startHistoricalSync triggers a historical sync on a peer. This is implemented
// as an explicit state transition so we can deterministically control when a
// forced historical sync happens, instead of relying on time-based triggers or
// randomly selected peers.
func (fn *fuzzNetwork) startHistoricalSync() {
	if !fn.hasEnoughData(1) {
		return
	}

	peer := fn.selectPeer()
	if peer == nil {
		return
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

		time.Sleep(5 * time.Millisecond)
	}
}

// runGossipStateMachine executes the gossip state machine with fuzz input data.
func (fn *fuzzNetwork) runGossipStateMachine() {
	for fn.hasEnoughData(1) {
		// Extract action from current data byte
		action := fuzzState(int(fn.getBytes(1)[0]) % int(numFuzzStates))

		switch action {
		case connectPeer:
			fn.connectNewPeer()

		case disconnectPeer:
			fn.disconnectPeer()

		case nodeAnnouncementReceived:
			fn.sendRemoteNodeAnnouncement()

		case channelAnnouncementReceived:
			fn.sendRemoteChannelAnnouncement()

		case channelUpdateReceived:
			fn.sendRemoteChannelUpdate()

		case announcementSignaturesReceived:
			fn.sendRemoteAnnounceSignatures()

		case queryShortChanIDsReceived:
			fn.sendRemoteQueryShortChanIDs()

		case queryChannelRangeReceived:
			fn.sendRemoteQueryChannelRange()

		case replyChannelRangeReceived:
			fn.sendRemoteReplyChannelRange()

		case gossipTimestampRangeReceived:
			fn.sendRemoteGossipTimestampRange()

		case replyShortChanIDsEndReceived:
			fn.sendRemoteReplyShortChanIDsEnd()

		case updateBlockHeight:
			fn.updateBlockHeight()

		case triggerHistoricalSync:
			fn.startHistoricalSync()
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
