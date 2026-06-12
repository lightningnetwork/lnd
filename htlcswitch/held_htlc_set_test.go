package htlcswitch

import (
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

var errTestForward = errors.New("test forward error")

// mockInterceptedForward is an InterceptedForward test double that records
// resolution calls and returns configured errors.
type mockInterceptedForward struct {
	packet InterceptedPacket

	resumeCount           int
	settleCount           int
	failCount             int
	failCodeCount         int
	failCode              lnwire.FailCode
	failWithCodeReturnErr error
	settleReturnErr       error
	failReturnErr         error
	resumeReturnErr       error
	resumeModified        bool
}

// newMockInterceptedForward creates a mock intercepted forward with the given
// circuit key and auto-fail deadline.
func newMockInterceptedForward(key models.CircuitKey,
	deadline int32) *mockInterceptedForward {

	return &mockInterceptedForward{
		packet: InterceptedPacket{
			IncomingCircuit: key,
			AutoFailHeight:  deadline,
		},
	}
}

// Packet returns the intercepted packet represented by the mock.
func (m *mockInterceptedForward) Packet() InterceptedPacket {
	return m.packet
}

// Resume records a resume call and returns the configured return error.
func (m *mockInterceptedForward) Resume() error {
	m.resumeCount++

	return m.resumeReturnErr
}

// ResumeModified records a modified resume call and returns the configured
// return error.
func (m *mockInterceptedForward) ResumeModified(
	_ fn.Option[lnwire.MilliSatoshi],
	_ fn.Option[lnwire.MilliSatoshi],
	_ fn.Option[lnwire.CustomRecords]) error {

	m.resumeModified = true

	return m.resumeReturnErr
}

// Settle records a settle call and returns the configured return error.
func (m *mockInterceptedForward) Settle(lntypes.Preimage) error {
	m.settleCount++

	return m.settleReturnErr
}

// Fail records an encrypted failure call and returns the configured return
// error.
func (m *mockInterceptedForward) Fail([]byte) error {
	m.failCount++

	return m.failReturnErr
}

// FailWithCode records a failure-code call and returns the configured
// return error.
func (m *mockInterceptedForward) FailWithCode(code lnwire.FailCode) error {
	m.failCodeCount++
	m.failCode = code

	return m.failWithCodeReturnErr
}

// testCircuitKey returns a stable circuit key for held HTLC set tests.
func testCircuitKey() models.CircuitKey {
	return models.CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(1),
		HtlcID: 2,
	}
}

// TestHeldHtlcSetEmpty verifies empty held HTLC set behavior.
func TestHeldHtlcSetEmpty(t *testing.T) {
	set := newHeldHtlcSet()

	require.False(t, set.exists(models.CircuitKey{}))
	require.ErrorIs(t, set.resolve(&FwdResolution{}), ErrFwdNotExists)
	require.Empty(t, set.releaseAll())
}

// TestHeldHtlcSetRejectsInvalidDeadline verifies invalid deadlines are
// rejected for both off-chain and on-chain held entries.
func TestHeldHtlcSetRejectsInvalidDeadline(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()

	require.Error(t, set.addOffChain(newMockInterceptedForward(key, 0)))
	require.Error(t, set.addOffChain(newMockInterceptedForward(key, -1)))
	require.Error(t, set.addOnChain(newMockInterceptedForward(key, 0)))
	require.Error(t, set.addOnChain(newMockInterceptedForward(key, -1)))
}

// TestHeldHtlcSetOffChainResolve verifies off-chain resolutions call through
// to the backing intercepted forward and remove the held entry.
func TestHeldHtlcSetOffChainResolve(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockInterceptedForward(key, 100)

	require.NoError(t, set.addOffChain(fwd))
	require.NoError(t, set.resolve(&FwdResolution{
		Key:    key,
		Action: FwdActionResume,
	}))
	require.Equal(t, 1, fwd.resumeCount)
	require.False(t, set.exists(key))
}

// TestHeldHtlcSetResolveKeepsEntryOnError verifies failed resolutions keep the
// held entry available for retry.
func TestHeldHtlcSetResolveKeepsEntryOnError(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockInterceptedForward(key, 100)
	fwd.settleReturnErr = errTestForward

	require.NoError(t, set.addOffChain(fwd))
	require.ErrorIs(t, set.resolve(&FwdResolution{
		Key:    key,
		Action: FwdActionSettle,
	}), errTestForward)

	require.True(t, set.exists(key))
	require.Equal(t, 1, fwd.settleCount)
}

// TestHeldHtlcSetReleaseAllOffChain verifies an optional interceptor
// disconnect resumes off-chain entries and clears them from the set.
func TestHeldHtlcSetReleaseAllOffChain(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockInterceptedForward(key, 100)

	require.NoError(t, set.addOffChain(fwd))
	require.Empty(t, set.releaseAll())
	require.False(t, set.exists(key))
	require.Equal(t, 1, fwd.resumeCount)
}

// TestHeldHtlcSetReleaseAllKeepsOnChain verifies an optional interceptor
// disconnect keeps on-chain entries available for replay if the interceptor
// reconnects before expiry.
func TestHeldHtlcSetReleaseAllKeepsOnChain(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockInterceptedForward(key, 100)

	require.NoError(t, set.addOnChain(fwd))
	require.Empty(t, set.releaseAll())
	require.True(t, set.exists(key))
	require.Zero(t, fwd.resumeCount)
}

// TestHeldHtlcSetOffChainExpire verifies off-chain expiry fails the HTLC back.
func TestHeldHtlcSetOffChainExpire(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockInterceptedForward(key, 100)

	require.NoError(t, set.addOffChain(fwd))

	require.Empty(t, set.expire(99))
	require.True(t, set.exists(key))
	require.Zero(t, fwd.failCodeCount)

	require.Empty(t, set.expire(100))
	require.False(t, set.exists(key))
	require.Equal(t, 1, fwd.failCodeCount)
	require.Equal(t, lnwire.CodeTemporaryChannelFailure, fwd.failCode)
}

// TestHeldHtlcSetOffChainExpireKeepsEntryOnError verifies expiry errors keep
// the off-chain entry available for retry.
func TestHeldHtlcSetOffChainExpireKeepsEntryOnError(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockInterceptedForward(key, 100)
	fwd.failWithCodeReturnErr = errTestForward

	require.NoError(t, set.addOffChain(fwd))

	errs := set.expire(100)
	require.Len(t, errs, 1)
	require.ErrorIs(t, errs[0].err, errTestForward)
	require.Equal(t, key, errs[0].key)
	require.True(t, set.exists(key))
}

// TestHeldHtlcSetOnChainResolve verifies on-chain entries ignore non-settle
// resolutions and remain held until settlement.
func TestHeldHtlcSetOnChainResolve(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockInterceptedForward(key, 100)

	require.NoError(t, set.addOnChain(fwd))
	require.NoError(t, set.resolve(&FwdResolution{
		Key:    key,
		Action: FwdActionFail,
	}))
	require.True(t, set.exists(key))
	require.Zero(t, fwd.failCodeCount)

	require.NoError(t, set.resolve(&FwdResolution{
		Key:    key,
		Action: FwdActionResume,
	}))
	require.True(t, set.exists(key))
	require.Zero(t, fwd.resumeCount)

	require.NoError(t, set.resolve(&FwdResolution{
		Key:    key,
		Action: FwdActionSettle,
	}))
	require.Equal(t, 1, fwd.settleCount)
	require.False(t, set.exists(key))
}

// TestHeldHtlcSetOnChainExpirePrunes verifies on-chain expiry only prunes the
// local held entry.
func TestHeldHtlcSetOnChainExpirePrunes(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockInterceptedForward(key, 100)

	require.NoError(t, set.addOnChain(fwd))

	require.Empty(t, set.expire(99))
	require.True(t, set.exists(key))

	require.Empty(t, set.expire(100))
	require.False(t, set.exists(key))
	require.Zero(t, fwd.failCodeCount)
}

// TestHeldHtlcSetOnChainReplacesOffChain verifies on-chain entries replace
// earlier off-chain entries with the same circuit key.
func TestHeldHtlcSetOnChainReplacesOffChain(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	offChain := newMockInterceptedForward(key, 100)
	onChain := newMockInterceptedForward(key, 100)

	require.NoError(t, set.addOffChain(offChain))
	require.NoError(t, set.addOnChain(onChain))

	require.NoError(t, set.resolve(&FwdResolution{
		Key:    key,
		Action: FwdActionSettle,
	}))

	require.Zero(t, offChain.settleCount)
	require.Equal(t, 1, onChain.settleCount)
}

// TestInterceptableSwitchForwardOnChain verifies on-chain intercept handling
// for fresh and already-held HTLCs.
func TestInterceptableSwitchForwardOnChain(t *testing.T) {
	key := testCircuitKey()

	var intercepted []InterceptedPacket
	interceptor := func(packet InterceptedPacket) error {
		intercepted = append(intercepted, packet)

		return nil
	}

	t.Run("fresh on-chain htlc is sent", func(t *testing.T) {
		s := &InterceptableSwitch{
			heldHtlcSet: newHeldHtlcSet(),
			interceptor: interceptor,
		}
		fwd := newMockInterceptedForward(key, 100)

		require.NoError(t, s.forwardOnChain(fwd))
		require.Len(t, intercepted, 1)
		require.Equal(t, key, intercepted[0].IncomingCircuit)
	})

	t.Run("on-chain htlc replaces off-chain htlc", func(t *testing.T) {
		intercepted = nil
		s := &InterceptableSwitch{
			heldHtlcSet: newHeldHtlcSet(),
			interceptor: interceptor,
		}
		offChain := newMockInterceptedForward(key, 100)
		onChain := newMockInterceptedForward(key, 100)

		require.NoError(t, s.heldHtlcSet.addOffChain(offChain))
		require.NoError(t, s.forwardOnChain(onChain))
		require.Empty(t, intercepted)

		require.NoError(t, s.resolve(&FwdResolution{
			Key:    key,
			Action: FwdActionSettle,
		}))
		require.Zero(t, offChain.settleCount)
		require.Equal(t, 1, onChain.settleCount)
	})

	t.Run("on-chain htlc replays after disconnect", func(t *testing.T) {
		intercepted = nil
		s := &InterceptableSwitch{
			heldHtlcSet: newHeldHtlcSet(),
			interceptor: interceptor,
		}
		fwd := newMockInterceptedForward(key, 100)

		require.NoError(t, s.forwardOnChain(fwd))
		require.Len(t, intercepted, 1)

		s.setInterceptor(nil)
		require.True(t, s.heldHtlcSet.exists(key))

		intercepted = nil
		s.setInterceptor(interceptor)
		require.Len(t, intercepted, 1)
		require.Equal(t, key, intercepted[0].IncomingCircuit)

		require.NoError(t, s.resolve(&FwdResolution{
			Key:    key,
			Action: FwdActionSettle,
		}))
		require.Equal(t, 1, fwd.settleCount)
		require.False(t, s.heldHtlcSet.exists(key))
	})
}
