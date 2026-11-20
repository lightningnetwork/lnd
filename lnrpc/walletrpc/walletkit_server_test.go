//go:build walletrpc
// +build walletrpc

package walletrpc

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	testifymock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// TestMarshallLeasesIncludesSpendProgress verifies that lease responses
// report the no-expiry lifetime alongside the stored spend height.
func TestMarshallLeasesIncludesSpendProgress(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		depth       uint32
		spendHeight int32
		noExpiry    bool
	}{
		{
			depth:       0,
			spendHeight: 0,
			noExpiry:    false,
		},
		{
			depth:       1,
			spendHeight: 123,
			noExpiry:    true,
		},
	}
	for _, testCase := range testCases {
		testName := fmt.Sprintf("depth %d", testCase.depth)
		t.Run(testName, func(t *testing.T) {
			t.Parallel()

			// Arrange timed and no-expiry wallet lifetimes so their
			// response flags can distinguish them without a count.
			lockedOutput := &wtxmgr.LockedOutput{
				Outpoint:               wire.OutPoint{Index: 2},
				LockID:                 wtxmgr.LockID{1},
				Expiration:             time.Unix(123, 0),
				ReleaseAfterSpendConfs: testCase.depth,
				ConfirmedSpendHeight:   testCase.spendHeight,
			}
			locks := []*wallet.ListLeasedOutputResult{{
				LockedOutput: lockedOutput,
				Value:        1000,
				PkScript:     []byte{0x51},
			}}

			// Act by converting the same lease metadata returned by
			// funding and listing through their shared marshaller.
			rpcLocks := marshallLeases(locks)

			// Assert the boolean reports the lifetime and preserves
			// the independent spend-height readback.
			require.Len(t, rpcLocks, 1)
			require.Equal(
				t, testCase.noExpiry, rpcLocks[0].NoExpiry,
			)
			require.Equal(
				t, testCase.spendHeight,
				rpcLocks[0].ConfirmedSpendHeight,
			)
		})
	}
}

// TestWitnessTypeMapping tests that the two witness type enums in the `input`
// package and the `walletrpc` package remain equal.
func TestWitnessTypeMapping(t *testing.T) {
	t.Parallel()

	// Tests that both enum types have the same length except the
	// UNKNOWN_WITNESS type which is only present in the walletrpc
	// witness type enum.
	require.Equal(
		t, len(allWitnessTypes), len(WitnessType_name)-1,
		"number of witness types should match proto definition",
	)

	// Tests that the string representations of both enum types are
	// equivalent.
	for witnessType, witnessTypeProto := range allWitnessTypes {
		// Redeclare to avoid loop variables being captured
		// by func literal.

		t.Run(witnessType.String(), func(tt *testing.T) {
			tt.Parallel()

			witnessTypeName := witnessType.String()
			witnessTypeName = strings.ToUpper(witnessTypeName)
			witnessTypeProtoName := witnessTypeProto.String()
			witnessTypeProtoName = strings.ReplaceAll(
				witnessTypeProtoName, "_", "",
			)

			require.Equal(
				t, witnessTypeName, witnessTypeProtoName,
				"mapped witness types should be named the same",
			)
		})
	}
}

type mockCoinSelectionLocker struct {
	fail bool
}

// noExpiryLeaseWallet checks lease acquisition and renewal through the public
// handler without substituting another implementation of lease lifetime rules.
type noExpiryLeaseWallet struct {
	lnwallet.WalletController
	testifymock.Mock
}

var _ lnwallet.OutputLeaserWithOptions = (*noExpiryLeaseWallet)(nil)

// LeaseOutput models renewal through the existing zero-option wallet path.
func (w *noExpiryLeaseWallet) LeaseOutput(id wtxmgr.LockID, op wire.OutPoint,
	duration time.Duration) (time.Time, error) {

	args := w.Called(id, op, duration)
	expiration, _ := args.Get(0).(time.Time)
	return expiration, args.Error(1)
}

// LeaseOutputWithOptions checks the persisted lifetime requested by the RPC.
func (w *noExpiryLeaseWallet) LeaseOutputWithOptions(id wtxmgr.LockID,
	op wire.OutPoint, duration time.Duration,
	opts lnwallet.LeaseOutputOptions) (time.Time, error) {

	args := w.Called(id, op, duration, opts)
	expiration, _ := args.Get(0).(time.Time)
	return expiration, args.Error(1)
}

// ListLeasedOutputs provides the effective policy after a same-owner renewal.
func (w *noExpiryLeaseWallet) ListLeasedOutputs() (
	[]*wallet.ListLeasedOutputResult, error) {

	args := w.Called()
	leases, _ := args.Get(0).([]*wallet.ListLeasedOutputResult)
	return leases, args.Error(1)
}

// TestLeaseOutputNoExpiry verifies the explicit flag acquires the wallet's
// no-timeout lifetime and reports that policy to clients.
func TestLeaseOutputNoExpiry(t *testing.T) {
	t.Parallel()

	// Arrange an exact wallet expectation and use the real coin-selection
	// mutex so only the optional wallet capability needs to be mocked.
	id := wtxmgr.LockID{1}
	op := wire.OutPoint{Index: 1}
	controller := &noExpiryLeaseWallet{}
	defer controller.AssertExpectations(t)
	controller.On(
		"LeaseOutputWithOptions", id, op,
		chanfunding.DefaultLockDuration,
		lnwallet.LeaseOutputOptions{ReleaseAfterSpendConfs: 1},
	).Return(time.Unix(123, 0), nil).Once()
	server := &WalletKit{cfg: &Config{
		Wallet:              controller,
		CoinSelectionLocker: &lnwallet.LightningWallet{},
	}}

	// Act with no duration: the flag alone chooses the
	// persisted reservation, without requiring a user-supplied count.
	resp, err := server.LeaseOutput(t.Context(), &LeaseOutputRequest{
		Id: id[:],
		Outpoint: &lnrpc.OutPoint{
			TxidBytes:   op.Hash[:],
			OutputIndex: op.Index,
		},
		NoExpiry: true,
	})

	// Assert the new response field acknowledges the installed policy;
	// deferred mock verification checks the exact owner and lifetime once.
	require.NoError(t, err)
	require.True(t, resp.NoExpiry)
}

// TestLeaseOutputRejectsNoExpiryConflicts verifies both lease RPCs reject
// competing lifetime settings before touching wallet state.
func TestLeaseOutputRejectsNoExpiryConflicts(t *testing.T) {
	t.Parallel()

	// Arrange without a wallet so validation must reject the conflicting
	// lifetimes before either handler can acquire a reservation.
	server := &WalletKit{cfg: &Config{}}

	// Act through both public request forms with a duration that cannot
	// apply to the explicitly requested no-expiry lifetime.
	_, leaseErr := server.LeaseOutput(t.Context(), &LeaseOutputRequest{
		Id: bytes.Repeat([]byte{1}, 32),
		Outpoint: &lnrpc.OutPoint{
			TxidBytes: make([]byte, 32),
		},
		NoExpiry:          true,
		ExpirationSeconds: 1,
	})
	_, fundErr := server.FundPsbt(t.Context(), &FundPsbtRequest{
		InputLeaseNoExpiry:    true,
		LockExpirationSeconds: 1,
	})

	// Assert both handlers expose the shared identity, so clients and
	// regression coverage do not depend on diagnostic wording.
	require.ErrorIs(t, leaseErr, errLeaseLifetimeConflict)
	require.ErrorIs(t, fundErr, errLeaseLifetimeConflict)
}

// TestLeaseOutputRejectsRetiredSpendConfs verifies old protobuf requests
// fail explicitly at both lease entry points instead of selecting timed locks.
func TestLeaseOutputRejectsRetiredSpendConfs(t *testing.T) {
	t.Parallel()

	// Arrange the old count value at each retired wire number. Ordinary
	// decoding retains these bytes even though the fields are now reserved;
	// no wallet is configured: rejection must need no acquisition.
	leaseReq := &LeaseOutputRequest{}
	leaseWire := protowire.AppendTag(nil, 4, protowire.VarintType)
	leaseWire = protowire.AppendVarint(leaseWire, 3)
	require.NoError(t, proto.Unmarshal(leaseWire, leaseReq))
	fundReq := &FundPsbtRequest{}
	fundWire := protowire.AppendTag(nil, 15, protowire.VarintType)
	fundWire = protowire.AppendVarint(fundWire, 3)
	require.NoError(t, proto.Unmarshal(fundWire, fundReq))
	server := &WalletKit{cfg: &Config{}}

	// Act through both public handlers with the decoded old requests, as
	// gRPC would deliver them from clients using the removed parameters.
	_, leaseErr := server.LeaseOutput(t.Context(), leaseReq)
	_, fundErr := server.FundPsbt(t.Context(), fundReq)

	// Assert the shared migration error before other validation or wallet
	// work; error identity keeps this independent of diagnostic wording.
	require.ErrorIs(t, leaseErr, errLeaseSpendConfsUnsupported)
	require.ErrorIs(t, fundErr, errLeaseSpendConfsUnsupported)
}

// WithCoinSelectLock runs the callback and optionally returns a test error.
func (m *mockCoinSelectionLocker) WithCoinSelectLock(f func() error) error {
	if err := f(); err != nil {
		return err
	}

	if m.fail {
		return fmt.Errorf("kek")
	}

	return nil
}

// TestLeaseOutputRejectsUnsupportedOptions verifies WalletKit does not
// silently downgrade an option-bearing request to a time-only lease.
func TestLeaseOutputRejectsUnsupportedOptions(t *testing.T) {
	t.Parallel()

	// Arrange a concrete wallet with no underlying optional leaser. The
	// existing resolver must reject it without invoking a wallet method.
	rpcServer := &WalletKit{cfg: &Config{
		Wallet:              &lnwallet.LightningWallet{},
		CoinSelectionLocker: &lnwallet.LightningWallet{},
	}}

	// Act with only the no-expiry flag so a competing duration cannot
	// obscure the missing wallet capability.
	_, err := rpcServer.LeaseOutput(t.Context(), &LeaseOutputRequest{
		Id: bytes.Repeat([]byte{1}, 32),
		Outpoint: &lnrpc.OutPoint{
			TxidBytes:   make([]byte, 32),
			OutputIndex: 1,
		},
		NoExpiry: true,
	})

	// Assert the capability error rather than accepting a timed fallback;
	// the unconfigured wallet would fail if the handler called through it.
	require.ErrorIs(t, err, errOutputLeaseOptionsUnsupported)
}

// TestLeaseOutputReturnsEffectiveRenewalDepth verifies a same-owner renewal
// acknowledges the timed or no-expiry lifetime preserved by the wallet.
func TestLeaseOutputReturnsEffectiveRenewalDepth(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		depth    uint32
		noExpiry bool
	}{
		{
			depth:    1,
			noExpiry: true,
		},
		{
			depth:    0,
			noExpiry: false,
		},
	}
	for _, testCase := range testCases {
		name := fmt.Sprintf("depth %d", testCase.depth)
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// Arrange a zero-option renewal and one readback of its
			// stored lifetime, with the real coin-selection mutex.
			lockID := wtxmgr.LockID{1}
			outpoint := wire.OutPoint{Index: 1}
			controller := &noExpiryLeaseWallet{}
			defer controller.AssertExpectations(t)
			controller.On(
				"LeaseOutput", lockID, outpoint, time.Minute,
			).Return(time.Unix(123, 0), nil).Once()
			lockedOutput := &wtxmgr.LockedOutput{
				Outpoint:               outpoint,
				LockID:                 lockID,
				ReleaseAfterSpendConfs: testCase.depth,
			}
			leases := []*wallet.ListLeasedOutputResult{
				{
					LockedOutput: lockedOutput,
				},
			}
			controller.On("ListLeasedOutputs").Return(
				leases, nil,
			).Once()
			locker := &lnwallet.LightningWallet{}
			server := &WalletKit{cfg: &Config{
				Wallet:              controller,
				CoinSelectionLocker: locker,
			}}

			// Act without a new policy: the existing lease
			// must keep its persisted lifetime on this renewal.
			resp, err := server.LeaseOutput(
				t.Context(), &LeaseOutputRequest{
					Id: lockID[:],
					Outpoint: &lnrpc.OutPoint{
						TxidBytes:   outpoint.Hash[:],
						OutputIndex: outpoint.Index,
					},
					ExpirationSeconds: 60,
				},
			)

			// Assert the boolean describes the retained lifetime,
			// even though the renewal omitted the no-expiry flag.
			require.NoError(t, err)
			require.Equal(t, testCase.noExpiry, resp.NoExpiry)
		})
	}
}

// TestLeaseOutputIgnoresRenewalDepthReadError verifies that failed policy
// readback cannot turn successful lease acquisition into an RPC failure.
func TestLeaseOutputIgnoresRenewalDepthReadError(t *testing.T) {
	t.Parallel()

	// Arrange a successful renewal followed by failed policy readback.
	// Reuse the wallet mock so both calls are checked without another
	// implementation of the wallet interface.
	lockID := wtxmgr.LockID{1}
	outpoint := wire.OutPoint{}
	controller := &noExpiryLeaseWallet{}
	defer controller.AssertExpectations(t)
	controller.On(
		"LeaseOutput", lockID, outpoint, time.Minute,
	).Return(time.Unix(123, 0), nil).Once()
	controller.On("ListLeasedOutputs").Return(
		[]*wallet.ListLeasedOutputResult(nil),
		errors.New("list leases failed"),
	).Once()
	rpcServer := &WalletKit{cfg: &Config{
		Wallet:              controller,
		CoinSelectionLocker: &lnwallet.LightningWallet{},
	}}

	// Act through the timed renewal path, which reads the retained policy
	// after the wallet has successfully acquired or extended the lease.
	resp, err := rpcServer.LeaseOutput(t.Context(), &LeaseOutputRequest{
		Id: lockID[:],
		Outpoint: &lnrpc.OutPoint{
			TxidBytes: outpoint.Hash[:],
		},
		ExpirationSeconds: 60,
	})

	// Assert acquisition still succeeds without claiming an unverified
	// no-expiry policy; deferred verification checks each wallet call once.
	require.NoError(t, err)
	require.False(t, resp.NoExpiry)
}

// TestFundPsbtRequiresCustomLockID verifies no-expiry input
// leases require an external, caller-specific owner ID.
func TestFundPsbtRequiresCustomLockID(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		lockID   []byte
		expected error
	}{
		{
			name:     "missing",
			expected: errMissingCustomLockID,
		},
		{
			name:     "all zero",
			lockID:   make([]byte, 32),
			expected: errZeroCustomLockID,
		},
		{
			name:     "reserved internal",
			lockID:   chanfunding.LndInternalLockID[:],
			expected: errReservedCustomLockID,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			// Arrange a no-expiry request with an unsafe owner and
			// no wallet, requiring rejection before acquisition.
			server := &WalletKit{cfg: &Config{}}
			req := &FundPsbtRequest{
				InputLeaseNoExpiry: true,
				CustomLockId:       testCase.lockID,
			}

			// Act before template validation or funding can obscure
			// the owner-ID rejection.
			_, err := server.FundPsbt(t.Context(), req)

			// Assert each invalid identity is rejected, requiring a
			// caller-specific owner for a persistent reservation.
			// Compare identities without matching message text.
			require.ErrorIs(t, err, testCase.expected)
		})
	}
}

// TestFundPsbtCoinSelect tests that the coin selection for a PSBT template
// works as expected.
func TestFundPsbtCoinSelect(t *testing.T) {
	t.Parallel()

	const fundAmt = 50_000
	var (
		p2wkhDustLimit = lnwallet.DustLimitForSize(input.P2WPKHSize)
		p2trDustLimit  = lnwallet.DustLimitForSize(input.P2TRSize)
		p2wkhScript, _ = input.WitnessPubKeyHash([]byte{})
		p2trScript, _  = txscript.PayToTaprootScript(
			&input.TaprootNUMSKey,
		)
	)

	makePacket := func(outs ...*wire.TxOut) *psbt.Packet {
		p := &psbt.Packet{
			UnsignedTx: &wire.MsgTx{},
		}

		for _, out := range outs {
			p.UnsignedTx.TxOut = append(p.UnsignedTx.TxOut, out)
			p.Outputs = append(p.Outputs, psbt.POutput{})
		}

		return p
	}
	updatePacket := func(p *psbt.Packet,
		f func(*psbt.Packet) *psbt.Packet) *psbt.Packet {

		return f(p)
	}
	calcFee := func(p2trIn, p2wkhIn, p2trOut, p2wkhOut int,
		dust btcutil.Amount) btcutil.Amount {

		estimator := input.TxWeightEstimator{}
		for i := 0; i < p2trIn; i++ {
			estimator.AddTaprootKeySpendInput(
				txscript.SigHashDefault,
			)
		}
		for i := 0; i < p2wkhIn; i++ {
			estimator.AddP2WKHInput()
		}
		for i := 0; i < p2trOut; i++ {
			estimator.AddP2TROutput()
		}
		for i := 0; i < p2wkhOut; i++ {
			estimator.AddP2WKHOutput()
		}

		weight := estimator.Weight()
		fee := chainfee.FeePerKwFloor.FeeForWeight(weight)

		return fee + dust
	}

	testCases := []struct {
		name        string
		utxos       []*lnwallet.Utxo
		packet      *psbt.Packet
		changeIndex int32
		changeType  chanfunding.ChangeAddressType
		feeRate     chainfee.SatPerKWeight

		// expectedUtxoIndexes is the list of utxo indexes that are
		// expected to be used for funding the psbt.
		expectedUtxoIndexes []int

		// expectChangeOutputIndex is the expected output index that is
		// returned from the tested method.
		expectChangeOutputIndex int32

		// expectedChangeOutputAmount is the expected final total amount
		// of the output marked as the change output. This will only be
		// checked if the expected amount is non-zero.
		expectedChangeOutputAmount btcutil.Amount

		// expectedFee is the total amount of fees paid by the funded
		// packet in bytes.
		expectedFee btcutil.Amount

		// maxFeeRatio is the maximum fee to total output amount ratio
		// that we consider valid.
		maxFeeRatio float64

		// expectedErr is the expected concrete error. If not nil, then
		// the error must match exactly.
		expectedErr error

		// expectedContainedErrStr is the expected string to be
		// contained in the returned error.
		expectedContainedErrStr string

		// expectedErrType is the expected error type. If not nil, then
		// the error must be of this type.
		expectedErrType error
	}{{
		name:  "no utxos",
		utxos: []*lnwallet.Utxo{},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:     -1,
		feeRate:         chainfee.FeePerKwFloor,
		maxFeeRatio:     chanfunding.DefaultMaxFeeRatio,
		expectedErrType: &chanfunding.ErrInsufficientFunds{},
	}, {
		name: "1 p2wpkh utxo, add p2wkh change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    100_000,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 1,
		expectedFee:             calcFee(0, 1, 1, 1, 0),
	}, {
		name: "1 p2wpkh utxo, add p2tr change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    100_000,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2TRChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 1,
		expectedFee:             calcFee(0, 1, 2, 0, 0),
	}, {
		name: "1 p2wpkh utxo, no change, exact amount",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt + 123,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: -1,
		expectedFee:             calcFee(0, 1, 1, 0, 0),
	}, {
		name: "1 p2wpkh utxo, no change, p2wpkh change dust to fee",
		utxos: []*lnwallet.Utxo{
			{
				Value: fundAmt + calcFee(
					0, 1, 1, 0, p2wkhDustLimit-1,
				),
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2WKHChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: -1,
		expectedFee:             calcFee(0, 1, 1, 0, p2wkhDustLimit-1),
	}, {
		name: "1 p2wpkh utxo, no change, p2tr change dust to fee",
		utxos: []*lnwallet.Utxo{
			{
				Value: fundAmt + calcFee(
					0, 1, 1, 0, p2trDustLimit-1,
				),
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2TRChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: -1,
		expectedFee:             calcFee(0, 1, 1, 0, p2trDustLimit-1),
	}, {
		name: "1 p2wpkh utxo, existing p2tr change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt + 50_000,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 1, 0, 0),
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt + 50_000,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change, dust change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt + calcFee(0, 1, 0, 1, 0) + 50,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),
	}, {
		name: "1 p2wpkh + 1 p2tr utxo, existing p2tr input, existing " +
			"p2tr change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt / 2,
				PkScript: p2wkhScript,
			}, {
				Value:    fundAmt / 2,
				PkScript: p2trScript,
			},
		},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    1000,
					PkScript: p2trScript,
				},
				SighashType:            txscript.SigHashSingle,
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0, 1},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(2, 1, 1, 0, 0),
	}, {
		name: "1 p2wpkh + 1 p2tr utxo, existing p2tr input, add p2tr " +
			"change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt / 2,
				PkScript: p2wkhScript,
			}, {
				Value:    fundAmt / 2,
				PkScript: p2trScript,
			},
		},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    1000,
					PkScript: p2trScript,
				},
				SighashType:            txscript.SigHashSingle,
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2TRChangeAddress,
		expectedUtxoIndexes:     []int{0, 1},
		expectChangeOutputIndex: 1,
		expectedFee:             calcFee(2, 1, 2, 0, 0),
	}, {
		name: "large existing p2tr input, fee estimation p2wpkh " +
			"change",
		utxos: []*lnwallet.Utxo{},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    fundAmt * 3,
					PkScript: p2trScript,
				},
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2WKHChangeAddress,
		expectedUtxoIndexes:     []int{},
		expectChangeOutputIndex: 1,
		expectedChangeOutputAmount: fundAmt*3 - fundAmt -
			calcFee(1, 0, 1, 1, 0),
		expectedFee: calcFee(1, 0, 1, 1, 0),
	}, {
		name:  "large existing p2tr input, fee estimation no change",
		utxos: []*lnwallet.Utxo{},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value: fundAmt +
						int64(calcFee(1, 0, 1, 0, 0)),
					PkScript: p2trScript,
				},
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2TRChangeAddress,
		expectedUtxoIndexes:     []int{},
		expectChangeOutputIndex: -1,
		expectedFee:             calcFee(1, 0, 1, 0, 0),
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change, invalid fee ratio",
		utxos: []*lnwallet.Utxo{
			{
				Value:    250,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    50,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),

		expectedContainedErrStr: "fee 0.00000111 BTC exceeds max fee " +
			"(0.00000027 BTC) on total output value",
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change, negative feeratio",
		utxos: []*lnwallet.Utxo{
			{
				Value:    250,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    50,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio * (-1),
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),

		expectedContainedErrStr: "maxFeeRatio must be between 0.00 " +
			"and 5.00, got -0.20",
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change, big fee ratio",
		utxos: []*lnwallet.Utxo{
			{
				Value:    250,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    50,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             0.85,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),
	}, {
		name: "large existing p2tr input, fee estimation existing " +
			"change output",
		utxos: []*lnwallet.Utxo{},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    fundAmt * 2,
					PkScript: p2trScript,
				},
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:                0,
		feeRate:                    chainfee.FeePerKwFloor,
		maxFeeRatio:                chanfunding.DefaultMaxFeeRatio,
		changeType:                 chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:        []int{},
		expectChangeOutputIndex:    0,
		expectedChangeOutputAmount: fundAmt*2 - calcFee(1, 0, 1, 0, 0),
		expectedFee:                calcFee(1, 0, 1, 0, 0),
	}}

	for _, tc := range testCases {

		privKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		walletMock := &mock.WalletController{
			RootKey: privKey,
			Utxos:   tc.utxos,
		}
		rpcServer, _, err := New(&Config{
			Wallet:                walletMock,
			CoinSelectionLocker:   &mockCoinSelectionLocker{},
			CoinSelectionStrategy: wallet.CoinSelectionLargest,
		})
		require.NoError(t, err)

		t.Run(tc.name, func(tt *testing.T) {
			// To avoid our packet being mutated, we'll make a deep
			// copy of it, so we can still use the original in the
			// test case to compare the results to.
			var buf bytes.Buffer
			err := tc.packet.Serialize(&buf)
			require.NoError(tt, err)

			copiedPacket, err := psbt.NewFromRawBytes(&buf, false)
			require.NoError(tt, err)

			resp, err := rpcServer.fundPsbtCoinSelect(
				"", tc.changeIndex, copiedPacket, 0,
				tc.changeType, tc.feeRate,
				rpcServer.cfg.CoinSelectionStrategy,
				tc.maxFeeRatio, nil, 0, 0,
			)

			switch {
			case tc.expectedErr != nil:
				require.Error(tt, err)
				require.ErrorIs(tt, err, tc.expectedErr)

				return

			case tc.expectedErrType != nil:
				require.Error(tt, err)
				require.ErrorAs(tt, err, &tc.expectedErr)

				return
			case tc.expectedContainedErrStr != "":
				require.ErrorContains(
					tt, err, tc.expectedContainedErrStr,
				)

				return
			}

			require.NoError(tt, err)
			require.NotNil(tt, resp)

			resultPacket, err := psbt.NewFromRawBytes(
				bytes.NewReader(resp.FundedPsbt), false,
			)
			require.NoError(tt, err)
			resultTx := resultPacket.UnsignedTx

			expectedNumInputs := len(tc.expectedUtxoIndexes) +
				len(tc.packet.Inputs)
			require.Len(tt, resultPacket.Inputs, expectedNumInputs)
			require.Len(tt, resultTx.TxIn, expectedNumInputs)
			require.Equal(
				tt, tc.expectChangeOutputIndex,
				resp.ChangeOutputIndex,
			)

			fee, err := resultPacket.GetTxFee()
			require.NoError(tt, err)
			require.EqualValues(tt, tc.expectedFee, fee)

			if tc.expectedChangeOutputAmount != 0 {
				changeIdx := resp.ChangeOutputIndex
				require.GreaterOrEqual(tt, changeIdx, int32(-1))
				require.Less(
					tt, changeIdx,
					int32(len(resultTx.TxOut)),
				)

				changeOut := resultTx.TxOut[changeIdx]

				require.EqualValues(
					tt, tc.expectedChangeOutputAmount,
					changeOut.Value,
				)
			}
		})
	}
}
