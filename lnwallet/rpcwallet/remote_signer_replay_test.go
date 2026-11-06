//go:build signrpc && walletrpc

package rpcwallet

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/cert"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnrpc/watchonlyrpc"
	"github.com/lightningnetwork/lnd/lntest/unittest"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

// TestTaprootCommitReconstructionReplay covers the gap between signing a force
// close and persisting its transaction through a real outbound signer.
func TestTaprootCommitReconstructionReplay(t *testing.T) {
	// Arrange a wallet through its public loader and chain APIs. Retaining
	// its directory across reopen exercises daemon startup's durable nonce
	// authority behind the actual outbound RPC signer.
	params := &chaincfg.RegressionNetParams
	miner := unittest.NewMiner(t, params, nil, true, 0)
	dir := t.TempDir()
	openWallet := func() *btcwallet.BtcWallet {
		rpc := miner.RPCConfig()
		backend, err := chain.NewRPCClient(
			params, rpc.Host, rpc.User, rpc.Pass,
			rpc.Certificates, false, 20,
		)
		require.NoError(t, err)
		loader := btcwallet.LoaderWithLocalWalletDB(
			dir, false, time.Second,
		)
		seed := bytes.Repeat([]byte{1}, 32)
		wallet, err := btcwallet.New(btcwallet.Config{
			PrivatePass: []byte("replay-test"),
			HdSeed:      seed,
			NetParams:   params,
			CoinType:    params.HDCoinType,
			ChainSource: backend,
			LoaderOptions: []btcwallet.LoaderOption{
				loader,
			},
		}, blockcache.NewBlockCache(10000))
		require.NoError(t, err)
		require.NoError(t, wallet.Start())

		return wallet
	}
	wallet := openWallet()
	var stopRemote func()
	t.Cleanup(func() {
		if stopRemote != nil {
			stopRemote()
		}
		if wallet != nil {
			require.NoError(t, wallet.Stop())
			db := wallet.InternalWallet().Database()
			require.NoError(t, db.Close())
		}
	})
	inputs, digest := replayCommitInputs(t, wallet)
	signer, stopRemote := outboundReplaySigner(t, wallet)

	// Act by losing the first result before persistence, then reopen the
	// wallet to check durable history before exact reconstruction.
	first, err := lnwallet.GetSignedCommitTx(inputs, signer)
	require.NoError(t, err)
	stopRemote()
	stopRemote = nil
	require.NoError(t, wallet.Stop())
	require.NoError(
		t, wallet.InternalWallet().Database().Close(),
	)
	wallet = nil
	wallet = openWallet()
	signer, stopRemote = outboundReplaySigner(t, wallet)

	// Act on a changed commitment immediately after reopen, before exact
	// replay could recreate missing history. Assert the durable binding
	// rejects it before a partial signature escapes.
	changed := inputs
	changed.CommitTx = inputs.CommitTx.Copy()
	changed.CommitTx.TxOut[0].Value--
	_, err = lnwallet.GetSignedCommitTx(changed, signer)
	require.ErrorContains(
		t, err, "different signing transcript",
	)

	// Act by reconstructing the original commitment after rejection.
	second, err := lnwallet.GetSignedCommitTx(
		inputs, signer,
	)

	// Assert the identical transaction/witness verifies against the actual
	// funding key; this covers both exact replay and complete recovery.
	require.NoError(t, err)
	require.Equal(t, first, second)
	sig, err := schnorr.ParseSignature(
		second.TxIn[0].Witness[0],
	)
	require.NoError(t, err)
	key, err := schnorr.ParsePubKey(
		inputs.SignDesc.Output.PkScript[2:],
	)
	require.NoError(t, err)
	require.True(t, sig.Verify(digest[:], key))
}

// replayCommitInputs prepares an actual peer partial signature for the public
// reconstruction API, without replacing the wallet's signer with a mock.
func replayCommitInputs(t *testing.T,
	wallet *btcwallet.BtcWallet) (lnwallet.SignedCommitTxInputs, [32]byte) {

	t.Helper()
	// Derive our funding key and its deterministic verification nonce just
	// as ForceClose does for the persisted commitment height.
	ring := keychain.NewBtcWalletKeyRing(wallet.InternalWallet(), 1)
	ourKey, err := ring.DeriveKey(keychain.KeyLocator{
		Family: keychain.KeyFamilyMultiSig,
	})
	require.NoError(t, err)
	peerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	producer := shachain.NewRevocationProducer(chainhash.Hash{1})
	nonce, err := channeldb.NewMusigVerificationNonce(
		ourKey.PubKey, 42, producer,
	)
	require.NoError(t, err)
	keys := []*btcec.PublicKey{ourKey.PubKey, peerKey.PubKey()}
	_, peer, err := input.MuSig2CreateContext(
		input.MuSig2Version100RC2, peerKey, keys,
		&input.MuSig2Tweaks{
			TaprootBIP0086Tweak: true,
		}, nil,
	)
	require.NoError(t, err)
	_, err = peer.RegisterPubNonce(nonce.PubNonce)
	require.NoError(t, err)
	peerNonce := peer.PublicNonce()
	_, funding, err := input.GenTaprootFundingScript(
		ourKey.PubKey, peerKey.PubKey(), 100000,
		fn.None[chainhash.Hash](),
	)
	require.NoError(t, err)
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash: chainhash.Hash{2},
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    99000,
		PkScript: []byte{txscript.OP_TRUE},
	})
	fetch := txscript.NewCannedPrevOutputFetcher(
		funding.PkScript, funding.Value,
	)
	digestBytes, err := txscript.CalcTaprootSignatureHash(
		txscript.NewTxSigHashes(tx, fetch), txscript.SigHashDefault,
		tx, 0, fetch,
	)
	require.NoError(t, err)
	var digest [32]byte
	copy(digest[:], digestBytes)

	// This peer signs once. Only the local wallet is later reconstructed;
	// the peer signature and nonce are already persisted channel inputs.
	partial, err := input.MuSig2Sign(peer, digest, true)
	require.NoError(t, err)
	wireSig := lnwire.PartialSigWithNonce{
		PartialSig: lnwire.NewPartialSig(*partial.S),
		Nonce:      peerNonce,
	}
	var encoded bytes.Buffer
	require.NoError(t, wireSig.Encode(&encoded))

	return lnwallet.SignedCommitTxInputs{
		CommitTx:  tx,
		CommitSig: encoded.Bytes(),
		OurKey:    ourKey,
		TheirKey: keychain.KeyDescriptor{
			PubKey: peerKey.PubKey(),
		},
		SignDesc: &input.SignDescriptor{
			Output: funding,
		},
		Taproot: fn.Some(lnwallet.TaprootSignedCommitTxInputs{
			CommitHeight:         42,
			TaprootNonceProducer: producer,
		}),
	}, digest
}

// outboundReplaySigner connects the real outbound client/Signer RPC handlers to
// a wallet over TLS. Macaroon authorization has its separate daemon itest.
func outboundReplaySigner(t *testing.T,
	wallet *btcwallet.BtcWallet) (input.Signer, func()) {

	t.Helper()
	// Reuse production certificate generation and dialing, including the
	// macaroon file, rather than inserting an in-process signing shortcut.
	certBytes, keyBytes, err := cert.GenCertPair(
		"replay", nil, nil, false, time.Hour,
	)
	require.NoError(t, err)
	pair, err := tls.X509KeyPair(certBytes, keyBytes)
	require.NoError(t, err)
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.cert")
	macPath := filepath.Join(dir, "signer.macaroon")
	require.NoError(t, os.WriteFile(certPath, certBytes, 0600))
	mac, err := macaroon.New(
		bytes.Repeat([]byte{1}, 32), []byte("test"), "test",
		macaroon.LatestVersion,
	)
	require.NoError(t, err)
	macBytes, err := mac.MarshalBinary()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(macPath, macBytes, 0600))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	conn := NewInboundConnection(2*time.Second, 2*time.Second)
	tlsCredentials := credentials.NewTLS(cert.TLSConfFromCert(pair))
	server := grpc.NewServer(grpc.Creds(tlsCredentials))
	watchonlyrpc.RegisterWatchOnlyServer(
		server, &watchonlyrpc.InboundServer{
			Conn: conn,
		},
	)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()
	ring := keychain.NewBtcWalletKeyRing(wallet.InternalWallet(), 1)
	signer, _, err := signrpc.New(&signrpc.Config{
		Signer:  wallet,
		KeyRing: ring,
	})
	require.NoError(t, err)
	client, err := NewOutboundClient(
		&walletrpc.UnimplementedWalletKitServer{}, signer,
		NewStreamFeeder(lncfg.ConnectionCfg{
			RPCHost:      listener.Addr().String(),
			TLSCertPath:  certPath,
			MacaroonPath: macPath,
			Timeout:      2 * time.Second,
		}), 2*time.Second,
	)
	require.NoError(t, err)
	stop := sync.OnceFunc(func() {
		require.NoError(t, client.Stop())
		server.Stop()
		conn.Stop()
		require.NoError(t, <-serveErr)
	})
	t.Cleanup(stop)
	require.NoError(t, client.Start(t.Context()))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, <-conn.Ready(ctx))
	remote, err := NewRPCKeyRing(
		ring, wallet, conn, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	return remote, stop
}
