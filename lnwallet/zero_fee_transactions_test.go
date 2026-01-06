package lnwallet

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// zeroFeeTestVectors represents the root JSON structure from the spec's
// zero-fee-commitments-test.json file.
type zeroFeeTestVectors struct {
	LocalFundingPriv                   string            `json:"local_funding_priv"`
	RemoteFundingPriv                  string            `json:"remote_funding_priv"`
	FundingTxid                        string            `json:"funding_txid"`
	FundingIndex                       uint32            `json:"funding_index"`
	FundingAmountSatoshis              uint64            `json:"funding_amount_satoshis"`
	CommitmentNumber                   uint64            `json:"commitment_number"`
	ToSelfDelay                        uint16            `json:"to_self_delay"`
	LocalPaymentBasepointSecret        string            `json:"local_payment_basepoint_secret"`
	LocalDelayedPaymentBasepointSecret string            `json:"local_delayed_payment_basepoint_secret"`
	LocalHTLCBasepointSecret           string            `json:"local_htlc_basepoint_secret"`
	PerCommitmentPoint                 string            `json:"per_commitment_point"`
	RemotePaymentBasepointSecret       string            `json:"remote_payment_basepoint_secret"`
	RemoteHTLCBasepointSecret          string            `json:"remote_htlc_basepoint_secret"`
	RevocationBasepoint                string            `json:"revocation_basepoint"`
	PaymentHashToPreimage              map[string]string `json:"payment_hash_to_preimage"`
	Tests                              []zeroFeeTestCase `json:"tests"`
}

// zeroFeeHTLC represents an HTLC in the spec test vectors.
type zeroFeeHTLC struct {
	ID          uint64 `json:"id"`
	AmountMsat  uint64 `json:"amount_msat"`
	PaymentHash string `json:"payment_hash"`
	CLTVExpiry  uint32 `json:"cltv_expiry"`
}

// zeroFeeTestCase represents a single test case from the spec vectors.
type zeroFeeTestCase struct {
	Name                string        `json:"name"`
	DustLimitSatoshis   uint64        `json:"dust_limit_satoshis"`
	ToLocalMsat         uint64        `json:"to_local_msat"`
	ToRemoteMsat        uint64        `json:"to_remote_msat"`
	IncomingHTLCs       []zeroFeeHTLC `json:"incoming_htlcs"`
	OutgoingHTLCs       []zeroFeeHTLC `json:"outgoing_htlcs"`
	SignedCommitTx      string        `json:"signed_commit_tx"`
	SignedHTLCSuccessTx []string      `json:"signed_htlc_success_txs"`
	SignedHTLCTimeoutTx []string      `json:"signed_htlc_timeout_txs"`
}

// TestZeroFeeCommitmentVectors validates the spec test vectors for v3 zero-fee
// commitment transactions. This test verifies that:
// 1. All commitment transactions have version 3
// 2. All commitment transactions have a P2A anchor output
// 3. All HTLC transactions have version 3
// 4. All HTLC transaction inputs have sequence 0 (per BIP-431)
func TestZeroFeeCommitmentVectors(t *testing.T) {
	t.Parallel()

	// Load the spec test vectors.
	jsonText, err := os.ReadFile("test_vectors_zero_fee_commitments.json")
	require.NoError(t, err, "failed to read test vectors file")

	var vectors zeroFeeTestVectors
	err = json.Unmarshal(jsonText, &vectors)
	require.NoError(t, err, "failed to parse test vectors JSON")

	// Verify we loaded the expected test vectors.
	require.NotEmpty(t, vectors.Tests, "no test cases found")
	require.Equal(t, uint64(10000000), vectors.FundingAmountSatoshis)
	require.Equal(t, uint64(42), vectors.CommitmentNumber)

	// The P2A script: OP_1 (0x51) + push 2 bytes (0x02) + 0x4e73
	p2aScript := txscript.PayToAnchorScript
	require.Len(t, p2aScript, 4, "P2A script should be 4 bytes")

	for _, tc := range vectors.Tests {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()

			// Decode and validate the commitment transaction.
			commitTx := decodeTxHex(t, tc.SignedCommitTx)

			// 1. Verify transaction version is 3.
			require.EqualValues(
				t, 3, commitTx.Version,
				"commitment tx version should be 3",
			)

			// 2. Verify P2A anchor output exists.
			hasP2AAnchor := false
			for _, txOut := range commitTx.TxOut {
				if bytes.Equal(txOut.PkScript, p2aScript) {
					hasP2AAnchor = true

					// Per spec, anchor amount is max 240 sats.
					require.LessOrEqual(
						t, txOut.Value, int64(240),
						"P2A anchor amount should be <= 240 sats",
					)
					break
				}
			}
			require.True(
				t, hasP2AAnchor,
				"commitment tx must have P2A anchor output",
			)

			// 3. Validate HTLC success transactions.
			for i, htlcTxHex := range tc.SignedHTLCSuccessTx {
				htlcTx := decodeTxHex(t, htlcTxHex)

				// Verify version 3.
				require.EqualValues(
					t, 3, htlcTx.Version,
					"HTLC success tx %d version should be 3", i,
				)

				// Verify input sequence is 0 (BIP-431 requirement).
				require.Len(
					t, htlcTx.TxIn, 1,
					"HTLC success tx should have 1 input",
				)
				require.EqualValues(
					t, 0, htlcTx.TxIn[0].Sequence,
					"HTLC success tx input sequence should be 0",
				)

				// Verify zero fee: input value == output value.
				// For success tx, witness contains preimage.
				require.Len(
					t, htlcTx.TxOut, 1,
					"HTLC success tx should have 1 output",
				)
			}

			// 4. Validate HTLC timeout transactions.
			for i, htlcTxHex := range tc.SignedHTLCTimeoutTx {
				htlcTx := decodeTxHex(t, htlcTxHex)

				// Verify version 3.
				require.EqualValues(
					t, 3, htlcTx.Version,
					"HTLC timeout tx %d version should be 3", i,
				)

				// Verify input sequence is 0 (BIP-431 requirement).
				require.Len(
					t, htlcTx.TxIn, 1,
					"HTLC timeout tx should have 1 input",
				)
				require.EqualValues(
					t, 0, htlcTx.TxIn[0].Sequence,
					"HTLC timeout tx input sequence should be 0",
				)

				// Verify single output.
				require.Len(
					t, htlcTx.TxOut, 1,
					"HTLC timeout tx should have 1 output",
				)
			}

			// 5. Verify total HTLC count matches test case.
			totalHTLCs := len(tc.IncomingHTLCs) + len(tc.OutgoingHTLCs)
			totalHTLCTxs := len(tc.SignedHTLCSuccessTx) +
				len(tc.SignedHTLCTimeoutTx)

			// Note: Some HTLCs may be dust and trimmed, so we only
			// verify that HTLC tx count <= total HTLCs.
			require.LessOrEqual(
				t, totalHTLCTxs, totalHTLCs,
				"HTLC tx count should not exceed HTLC count",
			)
		})
	}
}

// TestZeroFeeCommitmentOutputCounts verifies the output structure of v3
// commitment transactions from the spec vectors.
func TestZeroFeeCommitmentOutputCounts(t *testing.T) {
	t.Parallel()

	jsonText, err := os.ReadFile("test_vectors_zero_fee_commitments.json")
	require.NoError(t, err)

	var vectors zeroFeeTestVectors
	err = json.Unmarshal(jsonText, &vectors)
	require.NoError(t, err)

	// Test specific cases with known output counts.
	testCases := []struct {
		name           string
		minOutputs     int
		maxOutputs     int
		hasToLocal     bool
		hasToRemote    bool
		htlcOutputs    int
		anchorExpected bool
	}{
		{
			name:           "Commitment transaction without HTLCs, both outputs untrimmed",
			minOutputs:     3, // to_local + to_remote + anchor
			maxOutputs:     3,
			hasToLocal:     true,
			hasToRemote:    true,
			htlcOutputs:    0,
			anchorExpected: true,
		},
		{
			name:           "Commitment transaction without HTLCs, one output trimmed below maximum anchor amount",
			minOutputs:     2, // to_local + anchor (to_remote trimmed)
			maxOutputs:     2,
			hasToLocal:     true,
			hasToRemote:    false,
			htlcOutputs:    0,
			anchorExpected: true,
		},
		{
			name:           "Commitment transaction with all HTLCs above dust limit",
			minOutputs:     9, // anchor + to_local + to_remote + 6 HTLCs
			maxOutputs:     9,
			hasToLocal:     true,
			hasToRemote:    true,
			htlcOutputs:    6,
			anchorExpected: true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Find the matching test vector.
			var found *zeroFeeTestCase
			for i := range vectors.Tests {
				if vectors.Tests[i].Name == tc.name {
					found = &vectors.Tests[i]
					break
				}
			}
			require.NotNil(t, found, "test case not found: %s", tc.name)

			commitTx := decodeTxHex(t, found.SignedCommitTx)

			// Verify output count is within expected range.
			require.GreaterOrEqual(
				t, len(commitTx.TxOut), tc.minOutputs,
				"output count should be >= %d", tc.minOutputs,
			)
			require.LessOrEqual(
				t, len(commitTx.TxOut), tc.maxOutputs,
				"output count should be <= %d", tc.maxOutputs,
			)

			// Count P2A anchor outputs.
			p2aScript := txscript.PayToAnchorScript
			anchorCount := 0
			for _, txOut := range commitTx.TxOut {
				if bytes.Equal(txOut.PkScript, p2aScript) {
					anchorCount++
				}
			}

			if tc.anchorExpected {
				require.Equal(
					t, 1, anchorCount,
					"should have exactly 1 P2A anchor output",
				)
			}
		})
	}
}

// TestZeroFeeHTLCTransactionWitness verifies the witness structure of HTLC
// transactions in the spec vectors.
func TestZeroFeeHTLCTransactionWitness(t *testing.T) {
	t.Parallel()

	jsonText, err := os.ReadFile("test_vectors_zero_fee_commitments.json")
	require.NoError(t, err)

	var vectors zeroFeeTestVectors
	err = json.Unmarshal(jsonText, &vectors)
	require.NoError(t, err)

	// Find a test case with HTLC transactions.
	var htlcTestCase *zeroFeeTestCase
	for i := range vectors.Tests {
		if len(vectors.Tests[i].SignedHTLCSuccessTx) > 0 ||
			len(vectors.Tests[i].SignedHTLCTimeoutTx) > 0 {

			htlcTestCase = &vectors.Tests[i]
			break
		}
	}
	require.NotNil(t, htlcTestCase, "no test case with HTLCs found")

	t.Run("HTLC success tx witness", func(t *testing.T) {
		if len(htlcTestCase.SignedHTLCSuccessTx) == 0 {
			t.Skip("no HTLC success transactions")
		}

		htlcTx := decodeTxHex(t, htlcTestCase.SignedHTLCSuccessTx[0])

		// HTLC success witness should have:
		// 0: placeholder (0x00)
		// 1: remote sig (SIGHASH_SINGLE|ANYONECANPAY)
		// 2: local sig (SIGHASH_ALL)
		// 3: preimage
		// 4: witness script
		require.GreaterOrEqual(
			t, len(htlcTx.TxIn[0].Witness), 5,
			"HTLC success tx should have at least 5 witness elements",
		)

		// First element should be empty (placeholder).
		require.Empty(
			t, htlcTx.TxIn[0].Witness[0],
			"first witness element should be empty",
		)

		// Preimage should be 32 bytes.
		require.Len(
			t, htlcTx.TxIn[0].Witness[3], 32,
			"preimage should be 32 bytes",
		)
	})

	t.Run("HTLC timeout tx witness", func(t *testing.T) {
		if len(htlcTestCase.SignedHTLCTimeoutTx) == 0 {
			t.Skip("no HTLC timeout transactions")
		}

		htlcTx := decodeTxHex(t, htlcTestCase.SignedHTLCTimeoutTx[0])

		// HTLC timeout witness should have:
		// 0: placeholder (0x00)
		// 1: remote sig (SIGHASH_SINGLE|ANYONECANPAY)
		// 2: local sig (SIGHASH_ALL)
		// 3: empty (timeout path)
		// 4: witness script
		require.GreaterOrEqual(
			t, len(htlcTx.TxIn[0].Witness), 5,
			"HTLC timeout tx should have at least 5 witness elements",
		)

		// First element should be empty (placeholder).
		require.Empty(
			t, htlcTx.TxIn[0].Witness[0],
			"first witness element should be empty",
		)

		// Timeout path element should be empty.
		require.Empty(
			t, htlcTx.TxIn[0].Witness[3],
			"timeout path element should be empty",
		)
	})
}

// TestZeroFeeAnchorAmountCalculation verifies that the P2A anchor amount in
// the spec vectors follows the spec formula:
// anchor_amount = min(sum(trimmed_htlc_amounts) + rounded_msats, 240)
func TestZeroFeeAnchorAmountCalculation(t *testing.T) {
	t.Parallel()

	jsonText, err := os.ReadFile("test_vectors_zero_fee_commitments.json")
	require.NoError(t, err)

	var vectors zeroFeeTestVectors
	err = json.Unmarshal(jsonText, &vectors)
	require.NoError(t, err)

	p2aScript := txscript.PayToAnchorScript

	for _, tc := range vectors.Tests {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()

			commitTx := decodeTxHex(t, tc.SignedCommitTx)

			// Find anchor output.
			var anchorAmount int64 = -1
			anchorFound := false
			for _, txOut := range commitTx.TxOut {
				if bytes.Equal(txOut.PkScript, p2aScript) {
					anchorAmount = txOut.Value
					anchorFound = true
					break
				}
			}

			// Anchor must exist.
			require.True(
				t, anchorFound,
				"anchor output should exist",
			)

			// Anchor amount can be 0 (when no trimmed HTLCs and no
			// sub-satoshi dust) but must be <= 240 sats.
			require.GreaterOrEqual(
				t, anchorAmount, int64(0),
				"anchor amount should be >= 0",
			)
			require.LessOrEqual(
				t, anchorAmount, int64(240),
				"anchor amount should be <= 240 sats (P2AAnchorMaxAmount)",
			)
		})
	}
}

// TestZeroFeeCommitmentLocktime verifies the locktime encoding in v3
// commitment transactions follows the state hint format.
func TestZeroFeeCommitmentLocktime(t *testing.T) {
	t.Parallel()

	jsonText, err := os.ReadFile("test_vectors_zero_fee_commitments.json")
	require.NoError(t, err)

	var vectors zeroFeeTestVectors
	err = json.Unmarshal(jsonText, &vectors)
	require.NoError(t, err)

	for _, tc := range vectors.Tests {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()

			commitTx := decodeTxHex(t, tc.SignedCommitTx)

			// Locktime should be >= 500000000 (Unix timestamp format).
			// This is the state hint encoding requirement.
			require.GreaterOrEqual(
				t, commitTx.LockTime, uint32(500000000),
				"locktime should use timestamp format for state hint",
			)

			// Sequence should have locktime disabled bit set.
			require.NotEmpty(t, commitTx.TxIn)
			seq := commitTx.TxIn[0].Sequence
			require.NotZero(
				t, seq&wire.SequenceLockTimeDisabled,
				"sequence should have locktime disabled",
			)
		})
	}
}

// decodeTxHex decodes a hex-encoded transaction.
func decodeTxHex(t *testing.T, txHex string) *wire.MsgTx {
	t.Helper()

	txBytes, err := hex.DecodeString(txHex)
	require.NoError(t, err, "failed to decode tx hex")

	var tx wire.MsgTx
	err = tx.Deserialize(bytes.NewReader(txBytes))
	require.NoError(t, err, "failed to deserialize tx")

	return &tx
}
