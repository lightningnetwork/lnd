package lnwallet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/stretchr/testify/require"
)

// generateTaprootVectors controls whether to generate test vectors and write
// them to disk, or to verify the stored vectors match regenerated values.
var generateTaprootVectors = flag.Bool(
	"generate-taproot-vectors", false,
	"generate taproot test vectors and write to "+taprootVectorFile,
)

const (
	// taprootVectorSeedHex is the single deterministic seed from which all
	// test vector keys are derived.
	taprootVectorSeedHex = "000102030405060708090a0b0c0d0e0f" +
		"101112131415161718191a1b1c1d1e1f"

	// taprootVectorFile is the JSON file where test vectors are stored.
	taprootVectorFile = "test_vectors_taproot.json"
)

// deriveKeyFromSeed derives a deterministic private key from a seed and a
// label string. The key is computed as SHA256(seed || label).
func deriveKeyFromSeed(seed []byte, label string) *btcec.PrivateKey {
	h := sha256.New()
	h.Write(seed)
	h.Write([]byte(label))
	keyBytes := h.Sum(nil)

	privKey, _ := btcec.PrivKeyFromBytes(keyBytes)
	return privKey
}

// pubHex returns the compressed hex encoding of a public key.
func pubHex(pub *btcec.PublicKey) string {
	return hex.EncodeToString(pub.SerializeCompressed())
}

// privHex returns the hex encoding of a private key scalar.
func privHex(priv *btcec.PrivateKey) string {
	return hex.EncodeToString(priv.Serialize())
}

// scriptHex returns the hex encoding of a byte slice (script, hash, etc.).
func scriptHex(b []byte) string {
	return hex.EncodeToString(b)
}

// leafHash computes the TapHash of a tap leaf script.
func leafHash(script []byte) string {
	leaf := txscript.NewBaseTapLeaf(script)
	h := leaf.TapHash()
	return hex.EncodeToString(h[:])
}

// taprootTestContext holds all deterministic keys and parameters for taproot
// test vector generation.
type taprootTestContext struct {
	seed []byte

	localFundingPrivkey                *btcec.PrivateKey
	remoteFundingPrivkey               *btcec.PrivateKey
	localPaymentBasepointSecret        *btcec.PrivateKey
	remotePaymentBasepointSecret       *btcec.PrivateKey
	localDelayedPaymentBasepointSecret *btcec.PrivateKey
	remoteRevocationBasepointSecret    *btcec.PrivateKey
	localHtlcBasepointSecret           *btcec.PrivateKey
	remoteHtlcBasepointSecret          *btcec.PrivateKey

	localPerCommitSecret lntypes.Hash

	fundingAmount btcutil.Amount
	dustLimit     btcutil.Amount
	localCsvDelay uint16
	commitHeight  uint64

	t *testing.T
}

// newTaprootTestContext creates a new test context with all keys derived
// deterministically from the single seed.
func newTaprootTestContext(t *testing.T) *taprootTestContext {
	seed, err := hex.DecodeString(taprootVectorSeedHex)
	require.NoError(t, err)

	tc := &taprootTestContext{
		seed:          seed,
		fundingAmount: 10_000_000,
		dustLimit:     354,
		localCsvDelay: 144,
		commitHeight:  42,
		t:             t,
	}

	tc.localFundingPrivkey = deriveKeyFromSeed(seed, "local-funding")
	tc.remoteFundingPrivkey = deriveKeyFromSeed(seed, "remote-funding")
	tc.localPaymentBasepointSecret = deriveKeyFromSeed(
		seed, "local-payment-basepoint",
	)
	tc.remotePaymentBasepointSecret = deriveKeyFromSeed(
		seed, "remote-payment-basepoint",
	)
	tc.localDelayedPaymentBasepointSecret = deriveKeyFromSeed(
		seed, "local-delayed-payment-basepoint",
	)
	tc.remoteRevocationBasepointSecret = deriveKeyFromSeed(
		seed, "remote-revocation-basepoint",
	)
	tc.localHtlcBasepointSecret = deriveKeyFromSeed(
		seed, "local-htlc-basepoint",
	)
	tc.remoteHtlcBasepointSecret = deriveKeyFromSeed(
		seed, "remote-htlc-basepoint",
	)

	// Derive per-commitment secret from the seed as well.
	h := sha256.New()
	h.Write(seed)
	h.Write([]byte("local-per-commit-secret"))
	copy(tc.localPerCommitSecret[:], h.Sum(nil))

	return tc
}

// commitPoint returns the per-commitment point derived from the secret.
func (tc *taprootTestContext) commitPoint() *btcec.PublicKey {
	return input.ComputeCommitmentPoint(tc.localPerCommitSecret[:])
}

// ---------------------------------------------------------------------------
// JSON output types
// ---------------------------------------------------------------------------

// TaprootTestVectors is the top-level JSON structure for taproot test vectors.
type TaprootTestVectors struct {
	Params       TestVectorParams       `json:"params"`
	Scripts      ScriptVectors          `json:"scripts"`
	Transactions []TransactionTestCase  `json:"transactions"`
}

// TestVectorParams holds the seed, channel parameters, and all keys.
type TestVectorParams struct {
	Seed                  string `json:"seed"`
	FundingAmountSatoshis int64  `json:"funding_amount_satoshis"`
	DustLimitSatoshis     int64  `json:"dust_limit_satoshis"`
	CsvDelay              uint16 `json:"csv_delay"`
	CommitHeight          uint64 `json:"commit_height"`
	NumsPoint             string `json:"nums_point"`
	Keys                  KeySet `json:"keys"`
}

// KeySet contains all base point keys and derived per-commitment keys.
type KeySet struct {
	LocalFundingPrivkey  string `json:"local_funding_privkey"`
	LocalFundingPubkey   string `json:"local_funding_pubkey"`
	RemoteFundingPrivkey string `json:"remote_funding_privkey"`
	RemoteFundingPubkey  string `json:"remote_funding_pubkey"`

	LocalPaymentBasepointSecret string `json:"local_payment_basepoint_secret"`
	LocalPaymentBasepoint       string `json:"local_payment_basepoint"`
	RemotePaymentBasepointSecret string `json:"remote_payment_basepoint_secret"`
	RemotePaymentBasepoint       string `json:"remote_payment_basepoint"`

	LocalDelayedPaymentBasepointSecret string `json:"local_delayed_payment_basepoint_secret"`
	LocalDelayedPaymentBasepoint       string `json:"local_delayed_payment_basepoint"`
	RemoteRevocationBasepointSecret    string `json:"remote_revocation_basepoint_secret"`
	RemoteRevocationBasepoint          string `json:"remote_revocation_basepoint"`

	LocalHtlcBasepointSecret string `json:"local_htlc_basepoint_secret"`
	LocalHtlcBasepoint       string `json:"local_htlc_basepoint"`
	RemoteHtlcBasepointSecret string `json:"remote_htlc_basepoint_secret"`
	RemoteHtlcBasepoint       string `json:"remote_htlc_basepoint"`

	LocalPerCommitSecret string `json:"local_per_commit_secret"`
	LocalPerCommitPoint  string `json:"local_per_commit_point"`

	// Derived per-commitment keys.
	DerivedLocalDelayedPubkey string `json:"derived_local_delayed_pubkey"`
	DerivedRevocationPubkey   string `json:"derived_revocation_pubkey"`
	DerivedLocalHtlcPubkey    string `json:"derived_local_htlc_pubkey"`
	DerivedRemoteHtlcPubkey   string `json:"derived_remote_htlc_pubkey"`
	DerivedRemotePaymentPubkey string `json:"derived_remote_payment_pubkey"`
}

// ScriptVectorEntry represents a single tapscript tree decomposition.
type ScriptVectorEntry struct {
	// For scripts with named leaves.
	Scripts   map[string]string `json:"scripts,omitempty"`
	LeafHashes map[string]string `json:"leaf_hashes,omitempty"`

	TapscriptRoot string `json:"tapscript_root"`
	InternalKey   string `json:"internal_key"`
	OutputKey     string `json:"output_key"`
	PkScript      string `json:"pkscript"`
}

// FundingScriptVector holds the funding output vector.
type FundingScriptVector struct {
	FundingTxHex string `json:"funding_tx_hex"`
	CombinedKey  string `json:"combined_key"`
	PkScript     string `json:"pkscript"`
}

// ScriptVectors holds all script test vectors.
type ScriptVectors struct {
	Funding                  FundingScriptVector `json:"funding"`
	ToLocal                  ScriptVectorEntry   `json:"to_local"`
	ToRemote                 ScriptVectorEntry   `json:"to_remote"`
	LocalAnchor              ScriptVectorEntry   `json:"local_anchor"`
	RemoteAnchor             ScriptVectorEntry   `json:"remote_anchor"`
	OfferedHtlcLocalCommit   ScriptVectorEntry   `json:"offered_htlc_local_commit"`
	OfferedHtlcRemoteCommit  ScriptVectorEntry   `json:"offered_htlc_remote_commit"`
	AcceptedHtlcLocalCommit  ScriptVectorEntry   `json:"accepted_htlc_local_commit"`
	AcceptedHtlcRemoteCommit ScriptVectorEntry   `json:"accepted_htlc_remote_commit"`
	SecondLevelHtlcSuccess   ScriptVectorEntry   `json:"second_level_htlc_success"`
	SecondLevelHtlcTimeout   ScriptVectorEntry   `json:"second_level_htlc_timeout"`
}

// HtlcDesc describes an HTLC resolution in the transaction vectors.
type HtlcDesc struct {
	RemotePartialSigHex string `json:"remote_partial_sig_hex"`
	ResolutionTxHex     string `json:"resolution_tx_hex"`
}

// HtlcInput describes an HTLC added to the channel for a test case.
type HtlcInput struct {
	Incoming  bool   `json:"incoming"`
	AmountMsat uint64 `json:"amount_msat"`
	Expiry    uint32 `json:"expiry"`
	Preimage  string `json:"preimage"`
}

// TransactionTestCase is one transaction test vector.
type TransactionTestCase struct {
	Name                    string     `json:"name"`
	LocalBalanceMsat        uint64     `json:"local_balance_msat"`
	RemoteBalanceMsat       uint64     `json:"remote_balance_msat"`
	FeePerKw                int64      `json:"fee_per_kw"`
	DustLimitSatoshis       int64      `json:"dust_limit_satoshis,omitempty"`
	Htlcs                   []HtlcInput `json:"htlcs"`
	RemotePartialSig        string     `json:"remote_partial_sig"`
	ExpectedCommitmentTxHex string     `json:"expected_commitment_tx_hex"`
	HtlcDescs               []HtlcDesc `json:"htlc_descs"`
}

// ---------------------------------------------------------------------------
// Script vector generation (Section A)
// ---------------------------------------------------------------------------

// generateParams populates the params section of the test vectors.
func (tc *taprootTestContext) generateParams() TestVectorParams {
	commitPt := tc.commitPoint()

	// Derive per-commitment tweaked keys.
	localDelayedPubkey := input.TweakPubKey(
		tc.localDelayedPaymentBasepointSecret.PubKey(), commitPt,
	)
	revocationPubkey := input.DeriveRevocationPubkey(
		tc.remoteRevocationBasepointSecret.PubKey(), commitPt,
	)
	localHtlcPubkey := input.TweakPubKey(
		tc.localHtlcBasepointSecret.PubKey(), commitPt,
	)
	remoteHtlcPubkey := input.TweakPubKey(
		tc.remoteHtlcBasepointSecret.PubKey(), commitPt,
	)
	// For tweakless channels, the remote payment key is untweaked.
	remotePaymentPubkey := tc.remotePaymentBasepointSecret.PubKey()

	return TestVectorParams{
		Seed:                  taprootVectorSeedHex,
		FundingAmountSatoshis: int64(tc.fundingAmount),
		DustLimitSatoshis:     int64(tc.dustLimit),
		CsvDelay:              tc.localCsvDelay,
		CommitHeight:          tc.commitHeight,
		NumsPoint:             input.TaprootNUMSHex,
		Keys: KeySet{
			LocalFundingPrivkey:  privHex(tc.localFundingPrivkey),
			LocalFundingPubkey:   pubHex(tc.localFundingPrivkey.PubKey()),
			RemoteFundingPrivkey: privHex(tc.remoteFundingPrivkey),
			RemoteFundingPubkey:  pubHex(tc.remoteFundingPrivkey.PubKey()),

			LocalPaymentBasepointSecret: privHex(tc.localPaymentBasepointSecret),
			LocalPaymentBasepoint:       pubHex(tc.localPaymentBasepointSecret.PubKey()),
			RemotePaymentBasepointSecret: privHex(tc.remotePaymentBasepointSecret),
			RemotePaymentBasepoint:       pubHex(tc.remotePaymentBasepointSecret.PubKey()),

			LocalDelayedPaymentBasepointSecret: privHex(tc.localDelayedPaymentBasepointSecret),
			LocalDelayedPaymentBasepoint:       pubHex(tc.localDelayedPaymentBasepointSecret.PubKey()),
			RemoteRevocationBasepointSecret:    privHex(tc.remoteRevocationBasepointSecret),
			RemoteRevocationBasepoint:          pubHex(tc.remoteRevocationBasepointSecret.PubKey()),

			LocalHtlcBasepointSecret: privHex(tc.localHtlcBasepointSecret),
			LocalHtlcBasepoint:       pubHex(tc.localHtlcBasepointSecret.PubKey()),
			RemoteHtlcBasepointSecret: privHex(tc.remoteHtlcBasepointSecret),
			RemoteHtlcBasepoint:       pubHex(tc.remoteHtlcBasepointSecret.PubKey()),

			LocalPerCommitSecret: hex.EncodeToString(tc.localPerCommitSecret[:]),
			LocalPerCommitPoint:  pubHex(commitPt),

			DerivedLocalDelayedPubkey:  pubHex(localDelayedPubkey),
			DerivedRevocationPubkey:    pubHex(revocationPubkey),
			DerivedLocalHtlcPubkey:     pubHex(localHtlcPubkey),
			DerivedRemoteHtlcPubkey:    pubHex(remoteHtlcPubkey),
			DerivedRemotePaymentPubkey: pubHex(remotePaymentPubkey),
		},
	}
}

// generateFundingVector generates the funding output script vector.
func (tc *taprootTestContext) generateFundingVector() FundingScriptVector {
	t := tc.t

	pkScript, _, err := input.GenTaprootFundingScript(
		tc.localFundingPrivkey.PubKey(),
		tc.remoteFundingPrivkey.PubKey(),
		int64(tc.fundingAmount),
		fn.None[chainhash.Hash](),
	)
	require.NoError(t, err)

	// Build a minimal funding transaction with the P2TR output.
	fundingTx := wire.NewMsgTx(2)
	fundingTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: 0,
		},
	})
	fundingTx.AddTxOut(&wire.TxOut{
		Value:    int64(tc.fundingAmount),
		PkScript: pkScript,
	})

	var txBuf bytes.Buffer
	require.NoError(t, fundingTx.Serialize(&txBuf))

	// Extract the combined key from the pkScript. For P2TR, the pkScript
	// is OP_1 <32-byte-key>, so the key starts at byte 2.
	combinedKeyBytes := pkScript[2:]

	return FundingScriptVector{
		FundingTxHex: hex.EncodeToString(txBuf.Bytes()),
		CombinedKey:  hex.EncodeToString(combinedKeyBytes),
		PkScript:     scriptHex(pkScript),
	}
}

// commitScriptTreeToEntry converts a CommitScriptTree into a ScriptVectorEntry.
func commitScriptTreeToEntry(
	tree *input.CommitScriptTree) ScriptVectorEntry {

	scripts := make(map[string]string)
	leafHashes := make(map[string]string)

	settleScript := tree.SettleLeaf.Script
	scripts["settle"] = scriptHex(settleScript)
	leafHashes["settle"] = leafHash(settleScript)

	if tree.RevocationLeaf.Script != nil {
		revokeScript := tree.RevocationLeaf.Script
		scripts["revocation"] = scriptHex(revokeScript)
		leafHashes["revocation"] = leafHash(revokeScript)
	}

	return ScriptVectorEntry{
		Scripts:       scripts,
		LeafHashes:    leafHashes,
		TapscriptRoot: scriptHex(tree.TapscriptRoot),
		InternalKey:   pubHex(tree.InternalKey),
		OutputKey:     pubHex(tree.TaprootKey),
		PkScript:      scriptHex(tree.PkScript()),
	}
}

// htlcScriptTreeToEntry converts an HtlcScriptTree into a ScriptVectorEntry.
func htlcScriptTreeToEntry(tree *input.HtlcScriptTree) ScriptVectorEntry {
	scripts := make(map[string]string)
	leafHashes := make(map[string]string)

	successScript := tree.SuccessTapLeaf.Script
	scripts["success"] = scriptHex(successScript)
	leafHashes["success"] = leafHash(successScript)

	timeoutScript := tree.TimeoutTapLeaf.Script
	scripts["timeout"] = scriptHex(timeoutScript)
	leafHashes["timeout"] = leafHash(timeoutScript)

	return ScriptVectorEntry{
		Scripts:       scripts,
		LeafHashes:    leafHashes,
		TapscriptRoot: scriptHex(tree.TapscriptRoot),
		InternalKey:   pubHex(tree.InternalKey),
		OutputKey:     pubHex(tree.TaprootKey),
		PkScript:      scriptHex(tree.PkScript()),
	}
}

// secondLevelScriptTreeToEntry converts a SecondLevelScriptTree into a
// ScriptVectorEntry.
func secondLevelScriptTreeToEntry(
	tree *input.SecondLevelScriptTree) ScriptVectorEntry {

	scripts := make(map[string]string)
	leafHashes := make(map[string]string)

	successScript := tree.SuccessTapLeaf.Script
	scripts["success"] = scriptHex(successScript)
	leafHashes["success"] = leafHash(successScript)

	return ScriptVectorEntry{
		Scripts:       scripts,
		LeafHashes:    leafHashes,
		TapscriptRoot: scriptHex(tree.TapscriptRoot),
		InternalKey:   pubHex(tree.InternalKey),
		OutputKey:     pubHex(tree.TaprootKey),
		PkScript:      scriptHex(tree.PkScript()),
	}
}

// anchorScriptTreeToEntry converts an AnchorScriptTree into a
// ScriptVectorEntry.
func anchorScriptTreeToEntry(
	tree *input.AnchorScriptTree) ScriptVectorEntry {

	scripts := make(map[string]string)
	leafHashes := make(map[string]string)

	sweepScript := tree.SweepLeaf.Script
	scripts["sweep"] = scriptHex(sweepScript)
	leafHashes["sweep"] = leafHash(sweepScript)

	return ScriptVectorEntry{
		Scripts:       scripts,
		LeafHashes:    leafHashes,
		TapscriptRoot: scriptHex(tree.TapscriptRoot),
		InternalKey:   pubHex(tree.InternalKey),
		OutputKey:     pubHex(tree.TaprootKey),
		PkScript:      scriptHex(tree.PkScript()),
	}
}

// generateScriptVectors generates all script-only test vectors.
func (tc *taprootTestContext) generateScriptVectors() ScriptVectors {
	t := tc.t
	commitPt := tc.commitPoint()

	// Derive per-commitment tweaked keys.
	localDelayedPubkey := input.TweakPubKey(
		tc.localDelayedPaymentBasepointSecret.PubKey(), commitPt,
	)
	revocationPubkey := input.DeriveRevocationPubkey(
		tc.remoteRevocationBasepointSecret.PubKey(), commitPt,
	)
	localHtlcPubkey := input.TweakPubKey(
		tc.localHtlcBasepointSecret.PubKey(), commitPt,
	)
	remoteHtlcPubkey := input.TweakPubKey(
		tc.remoteHtlcBasepointSecret.PubKey(), commitPt,
	)
	remotePaymentPubkey := tc.remotePaymentBasepointSecret.PubKey()

	noAux := fn.None[txscript.TapLeaf]()

	// 1. to_local script tree.
	toLocalTree, err := input.NewLocalCommitScriptTree(
		uint32(tc.localCsvDelay), localDelayedPubkey,
		revocationPubkey, noAux, input.WithProdScripts(),
	)
	require.NoError(t, err)

	// 2. to_remote script tree.
	toRemoteTree, err := input.NewRemoteCommitScriptTree(
		remotePaymentPubkey, noAux, input.WithProdScripts(),
	)
	require.NoError(t, err)

	// 3. Anchor script trees.
	localAnchorTree, err := input.NewAnchorScriptTree(
		localDelayedPubkey,
	)
	require.NoError(t, err)

	remoteAnchorTree, err := input.NewAnchorScriptTree(
		remotePaymentPubkey,
	)
	require.NoError(t, err)

	// Use HTLC 0 for offered/accepted HTLC vectors.
	preimage0, err := lntypes.MakePreimageFromStr(
		"0000000000000000000000000000000000000000000000000000000000000000",
	)
	require.NoError(t, err)
	payHash0 := preimage0.Hash()

	// 4. Offered HTLC (local commit).
	offeredLocalTree, err := input.SenderHTLCScriptTaproot(
		localHtlcPubkey, remoteHtlcPubkey, revocationPubkey,
		payHash0[:], lntypes.Local, noAux,
		input.WithProdScripts(),
	)
	require.NoError(t, err)

	// 5. Offered HTLC (remote commit).
	offeredRemoteTree, err := input.SenderHTLCScriptTaproot(
		localHtlcPubkey, remoteHtlcPubkey, revocationPubkey,
		payHash0[:], lntypes.Remote, noAux,
		input.WithProdScripts(),
	)
	require.NoError(t, err)

	// 6. Accepted HTLC (local commit).
	acceptedLocalTree, err := input.ReceiverHTLCScriptTaproot(
		500, localHtlcPubkey, remoteHtlcPubkey, revocationPubkey,
		payHash0[:], lntypes.Local, noAux,
		input.WithProdScripts(),
	)
	require.NoError(t, err)

	// 7. Accepted HTLC (remote commit).
	acceptedRemoteTree, err := input.ReceiverHTLCScriptTaproot(
		500, localHtlcPubkey, remoteHtlcPubkey, revocationPubkey,
		payHash0[:], lntypes.Remote, noAux,
		input.WithProdScripts(),
	)
	require.NoError(t, err)

	// 8. Second-level HTLC success.
	secondLevelSuccess, err := input.TaprootSecondLevelScriptTree(
		revocationPubkey, localDelayedPubkey,
		uint32(tc.localCsvDelay), noAux,
		input.WithProdScripts(),
	)
	require.NoError(t, err)

	// 9. Second-level HTLC timeout (same function, different keys in a
	// real scenario, but for vectors we show the construction with the
	// same delay key since second-level success and timeout share the
	// same script tree structure).
	secondLevelTimeout, err := input.TaprootSecondLevelScriptTree(
		revocationPubkey, localDelayedPubkey,
		uint32(tc.localCsvDelay), noAux,
		input.WithProdScripts(),
	)
	require.NoError(t, err)

	return ScriptVectors{
		Funding:                  tc.generateFundingVector(),
		ToLocal:                  commitScriptTreeToEntry(toLocalTree),
		ToRemote:                 commitScriptTreeToEntry(toRemoteTree),
		LocalAnchor:              anchorScriptTreeToEntry(localAnchorTree),
		RemoteAnchor:             anchorScriptTreeToEntry(remoteAnchorTree),
		OfferedHtlcLocalCommit:   htlcScriptTreeToEntry(offeredLocalTree),
		OfferedHtlcRemoteCommit:  htlcScriptTreeToEntry(offeredRemoteTree),
		AcceptedHtlcLocalCommit:  htlcScriptTreeToEntry(acceptedLocalTree),
		AcceptedHtlcRemoteCommit: htlcScriptTreeToEntry(acceptedRemoteTree),
		SecondLevelHtlcSuccess:   secondLevelScriptTreeToEntry(secondLevelSuccess),
		SecondLevelHtlcTimeout:   secondLevelScriptTreeToEntry(secondLevelTimeout),
	}
}

// ---------------------------------------------------------------------------
// Transaction vector generation (Section B)
// ---------------------------------------------------------------------------

// taprootChanType is the channel type used for taproot test vectors.
var taprootChanType = channeldb.SingleFunderTweaklessBit |
	channeldb.AnchorOutputsBit |
	channeldb.ZeroHtlcTxFeeBit |
	channeldb.SimpleTaprootFeatureBit |
	channeldb.TaprootFinalBit

// createTaprootTestChannelsForVectors creates a pair of LightningChannel
// instances configured for taproot test vector generation. All keys are
// deterministic.
func createTaprootTestChannelsForVectors(tc *taprootTestContext,
	feeRate btcutil.Amount, remoteBalance,
	localBalance btcutil.Amount) (*LightningChannel, *LightningChannel) {

	t := tc.t

	// Build the funding transaction with a P2TR output.
	pkScript, _, err := input.GenTaprootFundingScript(
		tc.localFundingPrivkey.PubKey(),
		tc.remoteFundingPrivkey.PubKey(),
		int64(tc.fundingAmount),
		fn.None[chainhash.Hash](),
	)
	require.NoError(t, err)

	fundingTx := wire.NewMsgTx(2)
	fundingTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: 0,
		},
	})
	fundingTx.AddTxOut(&wire.TxOut{
		Value:    int64(tc.fundingAmount),
		PkScript: pkScript,
	})
	btcFundingTx := btcutil.NewTx(fundingTx)

	prevOut := &wire.OutPoint{
		Hash:  *btcFundingTx.Hash(),
		Index: 0,
	}
	fundingTxIn := wire.NewTxIn(prevOut, nil, nil)

	chanType := taprootChanType

	// Channel configurations using all deterministic keys.
	remoteCfg := channeldb.ChannelConfig{
		ChannelStateBounds: channeldb.ChannelStateBounds{
			MaxPendingAmount: lnwire.NewMSatFromSatoshis(
				tc.fundingAmount,
			),
			ChanReserve:      0,
			MinHTLC:          0,
			MaxAcceptedHtlcs: input.MaxHTLCNumber / 2,
		},
		CommitmentParams: channeldb.CommitmentParams{
			DustLimit: tc.dustLimit,
			CsvDelay:  tc.localCsvDelay,
		},
		MultiSigKey: keychain.KeyDescriptor{
			PubKey: tc.remoteFundingPrivkey.PubKey(),
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			PubKey: tc.remotePaymentBasepointSecret.PubKey(),
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			PubKey: tc.remoteHtlcBasepointSecret.PubKey(),
		},
		DelayBasePoint: keychain.KeyDescriptor{
			PubKey: tc.remotePaymentBasepointSecret.PubKey(),
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			PubKey: tc.remoteRevocationBasepointSecret.PubKey(),
		},
	}
	localCfg := channeldb.ChannelConfig{
		ChannelStateBounds: channeldb.ChannelStateBounds{
			MaxPendingAmount: lnwire.NewMSatFromSatoshis(
				tc.fundingAmount,
			),
			ChanReserve:      0,
			MinHTLC:          0,
			MaxAcceptedHtlcs: input.MaxHTLCNumber / 2,
		},
		CommitmentParams: channeldb.CommitmentParams{
			DustLimit: tc.dustLimit,
			CsvDelay:  tc.localCsvDelay,
		},
		MultiSigKey: keychain.KeyDescriptor{
			PubKey: tc.localFundingPrivkey.PubKey(),
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			PubKey: tc.localPaymentBasepointSecret.PubKey(),
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			PubKey: tc.localHtlcBasepointSecret.PubKey(),
		},
		DelayBasePoint: keychain.KeyDescriptor{
			PubKey: tc.localDelayedPaymentBasepointSecret.PubKey(),
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			PubKey: tc.localPaymentBasepointSecret.PubKey(),
		},
	}

	// Create mock producers for deterministic revocation secrets.
	remotePreimageProducer := &mockProducer{
		secret: chainhash.Hash(tc.localPerCommitSecret),
	}
	remoteCommitPoint := input.ComputeCommitmentPoint(
		tc.localPerCommitSecret[:],
	)

	localPreimageProducer := &mockProducer{
		secret: chainhash.Hash(tc.localPerCommitSecret),
	}
	localCommitPoint := input.ComputeCommitmentPoint(
		tc.localPerCommitSecret[:],
	)

	// Create temporary databases.
	dbRemote := channeldb.OpenForTesting(t, t.TempDir())
	dbLocal := channeldb.OpenForTesting(t, t.TempDir())

	// Create initial commitment transactions.
	feePerKw := chainfee.SatPerKWeight(feeRate)
	commitWeight := lntypes.WeightUnit(input.AnchorCommitWeight)
	commitFee := feePerKw.FeeForWeight(commitWeight)
	anchorAmt := btcutil.Amount(2 * AnchorSize)

	remoteCommitTx, localCommitTx, err := CreateCommitmentTxns(
		remoteBalance, localBalance-commitFee,
		&remoteCfg, &localCfg, remoteCommitPoint,
		localCommitPoint, *fundingTxIn, chanType, true, 0,
	)
	require.NoError(t, err)

	var commitHeight = tc.commitHeight - 1

	remoteCommit := channeldb.ChannelCommitment{
		CommitHeight:  commitHeight,
		LocalBalance:  lnwire.NewMSatFromSatoshis(remoteBalance),
		RemoteBalance: lnwire.NewMSatFromSatoshis(localBalance - commitFee - anchorAmt),
		CommitFee:     commitFee,
		FeePerKw:      btcutil.Amount(feePerKw),
		CommitTx:      remoteCommitTx,
		CommitSig:     testSigBytes,
	}
	localCommit := channeldb.ChannelCommitment{
		CommitHeight:  commitHeight,
		LocalBalance:  lnwire.NewMSatFromSatoshis(localBalance - commitFee - anchorAmt),
		RemoteBalance: lnwire.NewMSatFromSatoshis(remoteBalance),
		CommitFee:     commitFee,
		FeePerKw:      btcutil.Amount(feePerKw),
		CommitTx:      localCommitTx,
		CommitSig:     testSigBytes,
	}

	shortChanID := lnwire.NewShortChanIDFromInt(0xdeadbeef)

	remoteChannelState := &channeldb.OpenChannel{
		LocalChanCfg:            remoteCfg,
		RemoteChanCfg:           localCfg,
		IdentityPub:             tc.remoteFundingPrivkey.PubKey(),
		FundingOutpoint:         *prevOut,
		ShortChannelID:          shortChanID,
		ChanType:                chanType,
		IsInitiator:             false,
		Capacity:                tc.fundingAmount,
		RemoteCurrentRevocation: localCommitPoint,
		RevocationProducer:      remotePreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		LocalCommitment:         remoteCommit,
		RemoteCommitment:        remoteCommit,
		Db:                      dbRemote.ChannelStateDB(),
		Packager:                channeldb.NewChannelPackager(shortChanID),
		FundingTxn:              fundingTx,
	}
	localChannelState := &channeldb.OpenChannel{
		LocalChanCfg:            localCfg,
		RemoteChanCfg:           remoteCfg,
		IdentityPub:             tc.localFundingPrivkey.PubKey(),
		FundingOutpoint:         *prevOut,
		ShortChannelID:          shortChanID,
		ChanType:                chanType,
		IsInitiator:             true,
		Capacity:                tc.fundingAmount,
		RemoteCurrentRevocation: remoteCommitPoint,
		RevocationProducer:      localPreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		LocalCommitment:         localCommit,
		RemoteCommitment:        localCommit,
		Db:                      dbLocal.ChannelStateDB(),
		Packager:                channeldb.NewChannelPackager(shortChanID),
		FundingTxn:              fundingTx,
	}

	// Create mock signers with all deterministic keys. The funding key must
	// be at index 0 because the MusigSessionManager's key fetcher always
	// returns Privkeys[0] as the MuSig2 signing key.
	localSigner := input.NewMockSigner([]*btcec.PrivateKey{
		tc.localFundingPrivkey,
		tc.localPaymentBasepointSecret,
		tc.localDelayedPaymentBasepointSecret,
		tc.localHtlcBasepointSecret,
	}, nil)

	remoteSigner := input.NewMockSigner([]*btcec.PrivateKey{
		tc.remoteFundingPrivkey,
		tc.remoteRevocationBasepointSecret,
		tc.remotePaymentBasepointSecret,
		tc.remoteHtlcBasepointSecret,
	}, nil)

	// Derive deterministic signing rand for JIT nonces so MuSig2
	// signatures are reproducible across runs.
	localRandHash := sha256.Sum256(append(tc.seed, []byte("local-signing-rand")...))
	remoteRandHash := sha256.Sum256(append(tc.seed, []byte("remote-signing-rand")...))

	auxSigner := NewDefaultAuxSignerMock(t)
	remotePool := NewSigPool(1, remoteSigner)
	channelRemote, err := NewLightningChannel(
		remoteSigner, remoteChannelState, remotePool,
		WithLeafStore(&MockAuxLeafStore{}),
		WithAuxSigner(auxSigner),
		WithCustomSigningRand(bytes.NewReader(remoteRandHash[:])),
	)
	require.NoError(t, err)
	require.NoError(t, remotePool.Start())

	localPool := NewSigPool(1, localSigner)
	channelLocal, err := NewLightningChannel(
		localSigner, localChannelState, localPool,
		WithLeafStore(&MockAuxLeafStore{}),
		WithAuxSigner(auxSigner),
		WithCustomSigningRand(bytes.NewReader(localRandHash[:])),
	)
	require.NoError(t, err)
	require.NoError(t, localPool.Start())

	// Create state hint obfuscator.
	obfuscator := createStateHintObfuscator(remoteChannelState)
	err = SetStateNumHint(remoteCommitTx, commitHeight, obfuscator)
	require.NoError(t, err)
	err = SetStateNumHint(localCommitTx, commitHeight, obfuscator)
	require.NoError(t, err)

	// Initialize the databases.
	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18556,
	}
	require.NoError(t, channelRemote.channelState.SyncPending(addr, 101))

	addr = &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18555,
	}
	require.NoError(t, channelLocal.channelState.SyncPending(addr, 101))

	// Initialize revocation windows and musig nonces.
	err = initRevocationWindows(channelRemote, channelLocal)
	require.NoError(t, err)

	t.Cleanup(func() {
		dbLocal.Close()
		dbRemote.Close()

		require.NoError(t, remotePool.Stop())
		require.NoError(t, localPool.Stop())
	})

	return channelRemote, channelLocal
}

// taprootTransactionTestCases defines the set of transaction test cases.
var taprootTransactionTestCases = []struct {
	name         string
	localBalance lnwire.MilliSatoshi
	remoteBalance lnwire.MilliSatoshi
	feePerKw     btcutil.Amount
	dustLimit    btcutil.Amount
	useTestHtlcs bool
}{
	{
		name:         "simple commitment tx with no HTLCs",
		localBalance: 7_000_000_000,
		remoteBalance: 3_000_000_000,
		feePerKw:     15_000,
		useTestHtlcs: false,
	},
	{
		name:         "commitment tx with five HTLCs untrimmed",
		localBalance: 6_988_000_000,
		remoteBalance: 3_000_000_000,
		feePerKw:     644,
		useTestHtlcs: true,
	},
	{
		name:          "commitment tx with some HTLCs trimmed",
		localBalance:  6_988_000_000,
		remoteBalance: 3_000_000_000,
		feePerKw:      100_000,
		dustLimit:     546,
		useTestHtlcs:  true,
	},
}

// generateTransactionVectors generates all transaction test vectors.
func (tc *taprootTestContext) generateTransactionVectors() []TransactionTestCase {
	t := tc.t
	var results []TransactionTestCase

	for _, testCase := range taprootTransactionTestCases {
		// Override dust limit if specified in the test case.
		origDust := tc.dustLimit
		if testCase.dustLimit != 0 {
			tc.dustLimit = testCase.dustLimit
		}

		// Compute spendable balances by adding back in-flight HTLCs.
		remoteBalance := testCase.remoteBalance
		localBalance := testCase.localBalance
		if testCase.useTestHtlcs {
			for _, htlc := range testHtlcsSet1 {
				if htlc.incoming {
					remoteBalance += htlc.amount
				} else {
					localBalance += htlc.amount
				}
			}
		}

		// Verify balances add up to channel capacity.
		require.EqualValues(t,
			lnwire.NewMSatFromSatoshis(tc.fundingAmount),
			remoteBalance+localBalance,
		)

		remoteChannel, localChannel := createTaprootTestChannelsForVectors(
			tc, testCase.feePerKw,
			remoteBalance.ToSatoshis(),
			localBalance.ToSatoshis(),
		)

		// Add HTLCs if needed.
		var hash160map map[[20]byte]lntypes.Preimage
		if testCase.useTestHtlcs {
			hash160map = addTestHtlcs(
				t, remoteChannel, localChannel,
				testHtlcsSet1,
			)
		}

		// Execute commit dance.
		localNewCommit, err := localChannel.SignNextCommitment(ctxb)
		require.NoError(t, err)

		err = remoteChannel.ReceiveNewCommitment(
			localNewCommit.CommitSigs,
		)
		require.NoError(t, err)

		revMsg, _, _, err := remoteChannel.RevokeCurrentCommitment()
		require.NoError(t, err)

		_, _, err = localChannel.ReceiveRevocation(revMsg)
		require.NoError(t, err)

		remoteNewCommit, err := remoteChannel.SignNextCommitment(ctxb)
		require.NoError(t, err)

		// Capture remote partial signature.
		remoteSigHex := hex.EncodeToString(
			remoteNewCommit.CommitSig.ToSignatureBytes(),
		)

		err = localChannel.ReceiveNewCommitment(
			remoteNewCommit.CommitSigs,
		)
		require.NoError(t, err)

		_, _, _, err = localChannel.RevokeCurrentCommitment()
		require.NoError(t, err)

		// Force close to get the commitment transaction.
		forceCloseSum, err := localChannel.ForceClose()
		require.NoError(t, err)

		var txBytes bytes.Buffer
		require.NoError(t, forceCloseSum.CloseTx.Serialize(&txBytes))

		// Collect HTLC resolution transactions.
		var htlcDescs []HtlcDesc
		if testCase.useTestHtlcs {
			resolutions := forceCloseSum.ContractResolutions.UnwrapOrFail(t)
			htlcResolutions := resolutions.HtlcResolutions

			secondLevelTxes := map[uint32]*wire.MsgTx{}
			secondLevelSigs := map[uint32]string{}
			storeTx := func(
				index uint32, tx *wire.MsgTx, sig string,
			) {
				secondLevelTxes[index] = tx
				secondLevelSigs[index] = sig
			}

			for i, r := range htlcResolutions.IncomingHTLCs {
				successTx := r.SignedSuccessTx
				// Complete the witness with the preimage.
				witnessScript := successTx.TxIn[0].Witness[4]
				var hash160 [20]byte
				copy(hash160[:], witnessScript[69:69+20])
				preimage := hash160map[hash160]
				successTx.TxIn[0].Witness[3] = preimage[:]

				sigHex := hex.EncodeToString(
					remoteNewCommit.HtlcSigs[i].ToSignatureBytes(),
				)
				storeTx(
					r.HtlcPoint().Index, successTx, sigHex,
				)
			}
			for i, r := range htlcResolutions.OutgoingHTLCs {
				sigIdx := len(htlcResolutions.IncomingHTLCs) + i
				sigHex := hex.EncodeToString(
					remoteNewCommit.HtlcSigs[sigIdx].ToSignatureBytes(),
				)
				storeTx(
					r.HtlcPoint().Index,
					r.SignedTimeoutTx, sigHex,
				)
			}

			var keys []uint32
			for k := range secondLevelTxes {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(a, b int) bool {
				return keys[a] < keys[b]
			})

			for _, idx := range keys {
				tx := secondLevelTxes[idx]
				var b bytes.Buffer
				err := tx.Serialize(&b)
				require.NoError(t, err)

				htlcDescs = append(htlcDescs, HtlcDesc{
					RemotePartialSigHex: secondLevelSigs[idx],
					ResolutionTxHex:     hex.EncodeToString(b.Bytes()),
				})
			}
		}

		// Build the HTLC input list.
		var htlcInputs []HtlcInput
		if testCase.useTestHtlcs {
			for _, h := range testHtlcsSet1 {
				htlcInputs = append(htlcInputs, HtlcInput{
					Incoming:   h.incoming,
					AmountMsat: uint64(h.amount),
					Expiry:     h.expiry,
					Preimage:   h.preimage,
				})
			}
		}

		result := TransactionTestCase{
			Name:                    testCase.name,
			LocalBalanceMsat:        uint64(testCase.localBalance),
			RemoteBalanceMsat:       uint64(testCase.remoteBalance),
			FeePerKw:                int64(testCase.feePerKw),
			Htlcs:                   htlcInputs,
			RemotePartialSig:        remoteSigHex,
			ExpectedCommitmentTxHex: hex.EncodeToString(txBytes.Bytes()),
			HtlcDescs:               htlcDescs,
		}
		if testCase.dustLimit != 0 {
			result.DustLimitSatoshis = int64(testCase.dustLimit)
		}

		results = append(results, result)

		// Restore dust limit.
		tc.dustLimit = origDust
	}

	return results
}

// ---------------------------------------------------------------------------
// Main test entry point
// ---------------------------------------------------------------------------

// TestTaprootVectors either generates or verifies taproot test vectors
// depending on the -generate-taproot-vectors flag.
func TestTaprootVectors(t *testing.T) {
	if *generateTaprootVectors {
		t.Log("Generating taproot test vectors...")
		generateAndWriteTaprootVectors(t)
		return
	}

	t.Log("Verifying taproot test vectors...")
	verifyTaprootVectors(t)
}

// generateAndWriteTaprootVectors generates all taproot test vectors and writes
// them to the JSON file.
func generateAndWriteTaprootVectors(t *testing.T) {
	tc := newTaprootTestContext(t)

	vectors := TaprootTestVectors{
		Params:       tc.generateParams(),
		Scripts:      tc.generateScriptVectors(),
		Transactions: tc.generateTransactionVectors(),
	}

	jsonData, err := json.MarshalIndent(vectors, "", "  ")
	require.NoError(t, err)

	err = os.WriteFile(taprootVectorFile, jsonData, 0644)
	require.NoError(t, err)

	t.Logf("Wrote taproot test vectors to %s (%d bytes)",
		taprootVectorFile, len(jsonData))
}

// verifyTaprootVectors reads the stored test vectors and verifies them by
// regenerating all values from the seed.
func verifyTaprootVectors(t *testing.T) {
	jsonData, err := os.ReadFile(taprootVectorFile)
	require.NoError(t, err, "test vectors file not found, run with "+
		"-generate-taproot-vectors first")

	var stored TaprootTestVectors
	err = json.Unmarshal(jsonData, &stored)
	require.NoError(t, err)

	tc := newTaprootTestContext(t)

	// Verify params.
	t.Run("params", func(t *testing.T) {
		params := tc.generateParams()
		require.Equal(t, stored.Params, params)
	})

	// Verify script vectors.
	t.Run("scripts", func(t *testing.T) {
		scripts := tc.generateScriptVectors()

		t.Run("funding", func(t *testing.T) {
			require.Equal(t,
				stored.Scripts.Funding.CombinedKey,
				scripts.Funding.CombinedKey,
			)
			require.Equal(t,
				stored.Scripts.Funding.PkScript,
				scripts.Funding.PkScript,
			)
		})

		t.Run("to_local", func(t *testing.T) {
			require.Equal(t,
				stored.Scripts.ToLocal, scripts.ToLocal,
			)
		})

		t.Run("to_remote", func(t *testing.T) {
			require.Equal(t,
				stored.Scripts.ToRemote, scripts.ToRemote,
			)
		})

		t.Run("local_anchor", func(t *testing.T) {
			require.Equal(t,
				stored.Scripts.LocalAnchor,
				scripts.LocalAnchor,
			)
		})

		t.Run("remote_anchor", func(t *testing.T) {
			require.Equal(t,
				stored.Scripts.RemoteAnchor,
				scripts.RemoteAnchor,
			)
		})

		t.Run("offered_htlc_local_commit", func(t *testing.T) {
			require.Equal(t,
				stored.Scripts.OfferedHtlcLocalCommit,
				scripts.OfferedHtlcLocalCommit,
			)
		})

		t.Run("offered_htlc_remote_commit", func(t *testing.T) {
			require.Equal(t,
				stored.Scripts.OfferedHtlcRemoteCommit,
				scripts.OfferedHtlcRemoteCommit,
			)
		})

		t.Run("accepted_htlc_local_commit", func(t *testing.T) {
			require.Equal(t,
				stored.Scripts.AcceptedHtlcLocalCommit,
				scripts.AcceptedHtlcLocalCommit,
			)
		})

		t.Run("accepted_htlc_remote_commit", func(t *testing.T) {
			require.Equal(t,
				stored.Scripts.AcceptedHtlcRemoteCommit,
				scripts.AcceptedHtlcRemoteCommit,
			)
		})

		t.Run("second_level_htlc_success", func(t *testing.T) {
			require.Equal(t,
				stored.Scripts.SecondLevelHtlcSuccess,
				scripts.SecondLevelHtlcSuccess,
			)
		})

		t.Run("second_level_htlc_timeout", func(t *testing.T) {
			require.Equal(t,
				stored.Scripts.SecondLevelHtlcTimeout,
				scripts.SecondLevelHtlcTimeout,
			)
		})
	})

	// Verify transaction vectors.
	t.Run("transactions", func(t *testing.T) {
		txVectors := tc.generateTransactionVectors()
		require.Equal(t, len(stored.Transactions), len(txVectors))

		for i, storedTx := range stored.Transactions {
			genTx := txVectors[i]
			t.Run(storedTx.Name, func(t *testing.T) {
				require.Equal(t,
					storedTx.ExpectedCommitmentTxHex,
					genTx.ExpectedCommitmentTxHex,
					"commitment tx mismatch",
				)
				require.Equal(t,
					storedTx.RemotePartialSig,
					genTx.RemotePartialSig,
					"remote partial sig mismatch",
				)
				require.Equal(t,
					len(storedTx.HtlcDescs),
					len(genTx.HtlcDescs),
					"htlc desc count mismatch",
				)
				for j, storedHtlc := range storedTx.HtlcDescs {
					require.Equal(t,
						storedHtlc.ResolutionTxHex,
						genTx.HtlcDescs[j].ResolutionTxHex,
						fmt.Sprintf(
							"htlc %d resolution tx mismatch", j,
						),
					)
				}
			})
		}
	})
}

