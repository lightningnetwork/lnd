package onionmessage

import (
	"bytes"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcwallet/snacl"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// noopDispatcher is a zero-overhead OnionMessageUpdateDispatcher used for
// benchmarks so the dispatch path does not perturb timing.
type noopDispatcher struct{}

func (noopDispatcher) SendUpdate(any) error { return nil }

// noopSender is a zero-overhead PeerMessageSender used for benchmarks.
type noopSender struct{}

func (noopSender) SendToPeer([33]byte, *lnwire.OnionMessage) error {
	return nil
}

// testHDSeed is the deterministic seed used to create the benchmark wallet.
var testHDSeed = chainhash.Hash{
	0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
	0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
	0x4f, 0x2f, 0x6f, 0x25, 0x98, 0xa3, 0xef, 0xb9,
	0x69, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
}

// lightningAddrSchema mirrors keychain/btcwallet.go (unexported there).
var lightningAddrSchema = waddrmgr.ScopeAddrSchema{
	ExternalAddrType: waddrmgr.WitnessPubKey,
	InternalAddrType: waddrmgr.WitnessPubKey,
}

// waddrmgrNamespaceKey mirrors keychain/btcwallet.go (unexported there).
var waddrmgrNamespaceKey = []byte("waddrmgr")

// createBenchBtcWallet builds a real on-disk btcwallet (simnet, fast scrypt)
// suitable for driving keychain.BtcWalletKeyRing in benchmarks. It mirrors
// the unexported createTestBtcWallet in keychain/interface_test.go so the
// onionmessage bench can construct keychain.PubKeyECDH against a real
// SecretKeyRing — exactly as server.go does in production.
func createBenchBtcWallet(b *testing.B) *wallet.Wallet {
	b.Helper()

	fast := waddrmgr.FastScryptOptions
	waddrmgr.SetSecretKeyGen(func(passphrase *[]byte,
		_ *waddrmgr.ScryptOptions) (*snacl.SecretKey, error) {

		return snacl.NewSecretKey(passphrase, fast.N, fast.R, fast.P)
	})

	loader := wallet.NewLoader(
		&chaincfg.SimNetParams, b.TempDir(), true, time.Second*10, 0,
	)

	pass := []byte("test")
	w, err := loader.CreateNewWallet(
		pass, pass, testHDSeed[:], time.Time{},
	)
	require.NoError(b, err)
	require.NoError(b, w.Unlock(pass, nil))

	scope := waddrmgr.KeyScope{
		Purpose: keychain.BIP0043Purpose,
		Coin:    keychain.CoinTypeBitcoin,
	}
	if _, err := w.Manager.FetchScopedKeyManager(scope); err != nil {
		require.NoError(b, walletdb.Update(w.Database(),
			func(tx walletdb.ReadWriteTx) error {
				ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
				_, err := w.Manager.NewScopedKeyManager(
					ns, scope, lightningAddrSchema,
				)

				return err
			}))
	}

	b.Cleanup(func() { w.Lock() })

	return w
}

// BenchmarkOnionMessagePipeline measures OnionPeerActor.Receive end to end on
// the deliver path: decode -> sphinx process -> routing decision -> dispatch.
// It isolates the actor + pipeline overhead on top of sphinx.
// ProcessOnionPacket.
func BenchmarkOnionMessagePipeline(b *testing.B) {
	// Mirror server.go: derive the node key from a real BtcWalletKeyRing
	// and wrap it in keychain.NewPubKeyECDH. This is the exact onion-key
	// type the production sphinx router uses.
	w := createBenchBtcWallet(b)
	keyRing := keychain.NewBtcWalletKeyRing(w, keychain.CoinTypeBitcoin)

	nodeKeyDesc, err := keyRing.DeriveKey(keychain.KeyLocator{
		Family: keychain.KeyFamilyNodeKey,
		Index:  0,
	})
	require.NoError(b, err)

	nodeKeyECDH := keychain.NewPubKeyECDH(nodeKeyDesc, keyRing)

	router := sphinx.NewRouter(nodeKeyECDH, sphinx.NewNoOpReplayLog())
	require.NoError(b, router.Start())
	b.Cleanup(func() { router.Stop() })

	var peerPubKey [33]byte
	copy(peerPubKey[:], nodeKeyDesc.PubKey.SerializeCompressed())

	a := &OnionPeerActor{
		peerPubKey:       peerPubKey,
		peerSender:       noopSender{},
		router:           router,
		resolver:         newMockNodeIDResolver(),
		updateDispatcher: noopDispatcher{},
	}

	// Build a single-hop "deliver" onion message addressed to nodeKey.
	plain, err := record.EncodeBlindedRouteData(&record.BlindedRouteData{})
	require.NoError(b, err)

	hops := []*sphinx.HopInfo{
		{NodePub: nodeKeyDesc.PubKey, PlainText: plain},
	}

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(b, err)

	blindedPath, err := sphinx.BuildBlindedPath(sessionKey, hops)
	require.NoError(b, err)

	sphinxPath, err := route.OnionMessageBlindedPathToSphinxPath(
		blindedPath.Path, nil, nil,
	)
	require.NoError(b, err)

	onionSessionKey, err := btcec.NewPrivateKey()
	require.NoError(b, err)

	pkt, err := sphinx.NewOnionPacket(
		sphinxPath, onionSessionKey, nil,
		sphinx.DeterministicPacketFiller,
		sphinx.WithMaxPayloadSize(sphinx.MaxRoutingPayloadSize),
	)
	require.NoError(b, err)

	var buf bytes.Buffer
	require.NoError(b, pkt.Encode(&buf))

	onionBlob := buf.Bytes()
	pathKey := blindedPath.SessionKey.PubKey()

	ctx := b.Context()

	newReq := func() *Request {
		return &Request{msg: lnwire.OnionMessage{
			PathKey:   pathKey,
			OnionBlob: onionBlob,
		}}
	}

	// Make sure we're benchmarking the success path and not silently
	// timing an error path.
	require.NoError(b, a.Receive(ctx, newReq()).Err())

	b.ReportAllocs()

	for b.Loop() {
		if err := a.Receive(ctx, newReq()).Err(); err != nil {
			b.Fatal(err)
		}
	}
}
