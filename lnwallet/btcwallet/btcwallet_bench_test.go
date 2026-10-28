package btcwallet

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

// wtxmgrNamespaceKey is the top-level bucket key under which the transaction
// manager (wtxmgr) stores its state. It mirrors the unexported key used by the
// btcwallet wallet package and lets the benchmark inject credits directly.
var wtxmgrNamespaceKey = []byte("wtxmgr")

// benchWallet creates a fresh btcwallet backed by the given kvdb backend. It
// does not attach a chain backend or start the wallet, since the benchmarked
// calls only read from the wallet database.
func benchWallet(b *testing.B, backend kvdb.Backend) *base.Wallet {
	b.Helper()

	netParams := &chaincfg.RegressionNetParams
	loader, err := NewWalletLoader(
		netParams, 0, LoaderWithExternalWalletDB(backend),
	)
	require.NoError(b, err)

	// A fixed seed keeps wallet creation reproducible across runs.
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 1)
	}

	w, err := loader.CreateNewWallet(
		[]byte(base.InsecurePubPassphrase), []byte("private"), seed,
		time.Unix(1, 0),
	)
	require.NoError(b, err)

	return w
}

// injectCredits writes n mined, unspent credits into the wallet's transaction
// store. The outputs are not owned by any of the wallet's accounts, which is
// irrelevant to what we measure: the old ListAccounts path scanned every
// unspent output regardless of ownership.
func injectCredits(b *testing.B, w *base.Wallet, n int) {
	b.Helper()

	// A minimal valid P2WKH script (OP_0 <20-byte-hash>) so that the old
	// path's per-output address extraction has something to parse.
	script := append([]byte{0x00, 0x14}, make([]byte, 20)...)

	block := wtxmgr.BlockMeta{
		Block: wtxmgr.Block{Height: 100},
		Time:  time.Unix(1000, 0),
	}
	binary.LittleEndian.PutUint32(block.Hash[0:4], 100)

	db := w.Database()
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(wtxmgrNamespaceKey)

		for i := range n {
			// Give each transaction a unique input so that every
			// transaction hash, and therefore every outpoint, is
			// distinct.
			var prev chainhash.Hash
			binary.LittleEndian.PutUint64(prev[0:8], uint64(i))

			mtx := wire.NewMsgTx(2)
			mtx.AddTxIn(&wire.TxIn{
				PreviousOutPoint: wire.OutPoint{
					Hash:  prev,
					Index: 0,
				},
			})
			mtx.AddTxOut(wire.NewTxOut(100000, script))

			rec, err := wtxmgr.NewTxRecordFromMsgTx(
				mtx, time.Unix(1000, 0),
			)
			if err != nil {
				return err
			}

			if err := w.TxStore.InsertTx(
				ns, rec, &block,
			); err != nil {
				return err
			}
			err = w.TxStore.AddCredit(ns, rec, &block, 0, false)
			if err != nil {
				return err
			}
		}

		return nil
	})
	require.NoError(b, err)
}

// BenchmarkPostgresListAccounts measures the cost of enumerating a key scope's
// accounts as the wallet's unspent-output set grows. The old path
// (base.Wallet.Accounts) walked every unspent output per call to compute
// balances that ListAccounts discarded; the new path (BtcWallet.ListAccounts)
// reads only the stored account metadata. On a remote SQL backend each bucket
// read is a separate round trip, so the old path's runtime scales with the
// number of outputs while the new path stays flat.
//
// This benchmarks a single key scope; WalletBalance enumerates all of lnd's
// key scopes, so the measured old-vs-new gap is a conservative lower bound on
// the real per-call improvement.
func BenchmarkPostgresListAccounts(b *testing.B) {
	if !kvdb.PostgresBackend {
		b.Skip("requires the kvdb_postgres build tag")
	}

	stop, err := kvdb.StartEmbeddedPostgres()
	require.NoError(b, err)
	b.Cleanup(func() {
		require.NoError(b, stop())
	})

	// BIP0084 is one of the default scopes created with every wallet, so it
	// is guaranteed to exist for both the old and new enumeration paths.
	scope := waddrmgr.KeyScopeBIP0084

	for _, numUTXOs := range []int{0, 1_000, 10_000} {
		backend, cleanup, err := kvdb.GetTestBackend(
			b.TempDir(), "wallet",
		)
		require.NoError(b, err)

		w := benchWallet(b, backend)
		injectCredits(b, w, numUTXOs)

		bw := &BtcWallet{
			wallet: w,
			cfg: &Config{
				NetParams: &chaincfg.RegressionNetParams,
				CoinType: chaincfg.RegressionNetParams.
					HDCoinType,
			},
		}

		b.Run(fmt.Sprintf("utxos=%d/old_Accounts", numUTXOs),
			func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					_, err := w.Accounts(scope)
					require.NoError(b, err)
				}
			},
		)

		b.Run(fmt.Sprintf("utxos=%d/new_ListAccounts", numUTXOs),
			func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					_, err := bw.ListAccounts("", &scope)
					require.NoError(b, err)
				}
			},
		)

		require.NoError(b, backend.Close())
		cleanup()
	}
}

// oldWalletBalance replicates the pre-single-pass WalletBalance work: it scans
// the unspent outputs twice per account (once for the total, once for the
// confirmed balance). ConfirmedBalance scans the full unspent set on every
// call regardless of the account filter, so this reproduces the 2*accounts
// scans the old loop performed.
func oldWalletBalance(b *testing.B, bw *BtcWallet, numAccounts int) {
	b.Helper()

	for range numAccounts {
		_, err := bw.ConfirmedBalance(0, "")
		require.NoError(b, err)

		_, err = bw.ConfirmedBalance(1, "")
		require.NoError(b, err)
	}
}

// newWalletBalance performs the single-pass equivalent: one listing of the
// unspent outputs, bucketed by account.
func newWalletBalance(b *testing.B, bw *BtcWallet) {
	b.Helper()

	utxos, err := bw.ListUnspentWitness(0, math.MaxInt32, "")
	require.NoError(b, err)

	balances := make(map[string]int64)
	for _, utxo := range utxos {
		balances[utxo.Account] += int64(utxo.Value)
	}
}

// BenchmarkPostgresWalletBalance measures how computing wallet balances scales
// with the number of accounts. Before the single-pass change the balance was
// computed by scanning the unspent outputs twice per account, so the work grew
// with the account count; the single pass lists the unspent outputs once and
// buckets them by account, so its cost is independent of the account count.
func BenchmarkPostgresWalletBalance(b *testing.B) {
	if !kvdb.PostgresBackend {
		b.Skip("requires the kvdb_postgres build tag")
	}

	stop, err := kvdb.StartEmbeddedPostgres()
	require.NoError(b, err)
	b.Cleanup(func() {
		require.NoError(b, stop())
	})

	// Fix the unspent-output count and vary the number of accounts, the
	// factor the old per-account scanning grew with.
	const numUTXOs = 1_000

	for _, numAccounts := range []int{1, 4, 16} {
		backend, cleanup, err := kvdb.GetTestBackend(
			b.TempDir(), "wallet",
		)
		require.NoError(b, err)

		w := benchWallet(b, backend)
		injectCredits(b, w, numUTXOs)

		bw := &BtcWallet{
			wallet: w,
			cfg: &Config{
				NetParams: &chaincfg.RegressionNetParams,
				CoinType: chaincfg.RegressionNetParams.
					HDCoinType,
			},
		}

		b.Run(fmt.Sprintf("accounts=%d/old_perAccount", numAccounts),
			func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					oldWalletBalance(b, bw, numAccounts)
				}
			},
		)

		b.Run(fmt.Sprintf("accounts=%d/new_singlePass", numAccounts),
			func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					newWalletBalance(b, bw)
				}
			},
		)

		require.NoError(b, backend.Close())
		cleanup()
	}
}
