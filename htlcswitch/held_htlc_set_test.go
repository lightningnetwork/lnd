package htlcswitch

import (
	"errors"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	lntestmock "github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var errTestForward = errors.New("test forward error")

// mockInterceptedForward is an InterceptedForward test double that records
// resolution calls and returns configured errors.
type mockInterceptedForward struct {
	mock.Mock

	packet InterceptedPacket
}

// newMockInterceptedForward creates a mock intercepted forward with the given
// circuit key and auto-fail deadline.
func newMockInterceptedForward(key models.CircuitKey,
	deadline int32) *mockInterceptedForward {

	return &mockInterceptedForward{
		packet: InterceptedPacket{
			IncomingCircuit: key,
			Deadline: fn.NewLeft[
				OffChainAutoFailHeight, OnChainSettleDeadline,
			](OffChainAutoFailHeight(deadline)),
		},
	}
}

// newMockOnChainInterceptedForward creates a mock on-chain intercepted forward
// with the given circuit key and settlement deadline.
func newMockOnChainInterceptedForward(key models.CircuitKey,
	deadline int32) *mockInterceptedForward {

	return &mockInterceptedForward{
		packet: InterceptedPacket{
			IncomingCircuit: key,
			Deadline: fn.NewRight[
				OffChainAutoFailHeight, OnChainSettleDeadline,
			](OnChainSettleDeadline(deadline)),
		},
	}
}

// Packet returns the intercepted packet represented by the mock.
func (m *mockInterceptedForward) Packet() InterceptedPacket {
	return m.packet
}

// Resume records a resume call and returns the configured return error.
func (m *mockInterceptedForward) Resume() error {
	args := m.Called()

	return args.Error(0)
}

// ResumeModified records a modified resume call and returns the configured
// return error.
func (m *mockInterceptedForward) ResumeModified(
	_ fn.Option[lnwire.MilliSatoshi],
	_ fn.Option[lnwire.MilliSatoshi],
	_ fn.Option[lnwire.CustomRecords]) error {

	args := m.Called()

	return args.Error(0)
}

// Settle records a settle call and returns the configured return error.
func (m *mockInterceptedForward) Settle(preimage lntypes.Preimage) error {
	args := m.Called(preimage)

	return args.Error(0)
}

// Fail records an encrypted failure call and returns the configured return
// error.
func (m *mockInterceptedForward) Fail(reason []byte) error {
	args := m.Called(reason)

	return args.Error(0)
}

// FailWithCode records a failure-code call and returns the configured
// return error.
func (m *mockInterceptedForward) FailWithCode(code lnwire.FailCode) error {
	args := m.Called(code)

	return args.Error(0)
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
	require.Empty(t, set.releaseAllOffChainHeld())
}

// TestHeldHtlcSetRejectsInvalidDeadline verifies invalid deadlines are
// rejected for both off-chain and on-chain held entries.
func TestHeldHtlcSetRejectsInvalidDeadline(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()

	require.Error(t, set.addOffChain(newMockInterceptedForward(key, 0)))
	require.Error(t, set.addOffChain(newMockInterceptedForward(key, -1)))
	require.Error(t, set.addOffChain(
		newMockOnChainInterceptedForward(key, 100),
	))
	require.Error(t, set.addOnChain(
		newMockOnChainInterceptedForward(key, 0),
	))
	require.Error(t, set.addOnChain(
		newMockOnChainInterceptedForward(key, -1),
	))
	require.Error(t, set.addOnChain(newMockInterceptedForward(key, 100)))
}

// TestHeldHtlcSetOffChainResolve verifies off-chain resolutions call through
// to the backing intercepted forward and remove the held entry.
func TestHeldHtlcSetOffChainResolve(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockInterceptedForward(key, 100)
	fwd.On("Resume").Return(nil).Once()

	require.NoError(t, set.addOffChain(fwd))
	require.NoError(t, set.resolve(&FwdResolution{
		Key:    key,
		Action: FwdActionResume,
	}))
	fwd.AssertExpectations(t)
	require.False(t, set.exists(key))
}

// TestHeldHtlcSetAddOffChainKeepsExisting verifies that duplicate off-chain
// forwards keep the existing held entry.
func TestHeldHtlcSetAddOffChainKeepsExisting(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	first := newMockInterceptedForward(key, 100)
	second := newMockInterceptedForward(key, 100)
	first.On("Resume").Return(nil).Once()

	require.NoError(t, set.addOffChain(first))
	require.NoError(t, set.addOffChain(second))
	require.NoError(t, set.resolve(&FwdResolution{
		Key:    key,
		Action: FwdActionResume,
	}))

	first.AssertExpectations(t)
	second.AssertNotCalled(t, "Resume")
}

// TestInterceptableSwitchForwardOffChainAlreadyHeld verifies that normal
// off-chain forwarding handles duplicates before adding them to the held set.
func TestInterceptableSwitchForwardOffChainAlreadyHeld(t *testing.T) {
	key := testCircuitKey()
	fwd := newMockInterceptedForward(key, 100)
	s := &InterceptableSwitch{
		heldHtlcSet: newHeldHtlcSet(),
	}

	require.NoError(t, s.heldHtlcSet.addOffChain(fwd))

	handled, err := s.forwardOffChain(
		newMockInterceptedForward(key, 100), true,
	)
	require.NoError(t, err)
	require.True(t, handled)
}

// TestHeldHtlcSetResolveKeepsEntryOnError verifies failed resolutions keep the
// held entry available for retry.
func TestHeldHtlcSetResolveKeepsEntryOnError(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockInterceptedForward(key, 100)
	fwd.On("Settle", lntypes.Preimage{}).Return(errTestForward).Once()

	require.NoError(t, set.addOffChain(fwd))
	require.ErrorIs(t, set.resolve(&FwdResolution{
		Key:    key,
		Action: FwdActionSettle,
	}), errTestForward)

	require.True(t, set.exists(key))
	fwd.AssertExpectations(t)
}

// TestHeldHtlcSetReleaseAllOffChainHeld verifies an optional interceptor
// disconnect resumes off-chain entries and clears them from the set.
func TestHeldHtlcSetReleaseAllOffChainHeld(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockInterceptedForward(key, 100)
	fwd.On("Resume").Return(nil).Once()

	require.NoError(t, set.addOffChain(fwd))
	require.Empty(t, set.releaseAllOffChainHeld())
	require.False(t, set.exists(key))
	fwd.AssertExpectations(t)
}

// TestHeldHtlcSetReleaseAllOffChainHeldKeepsOnChain verifies an optional
// interceptor disconnect keeps on-chain entries available for replay if the
// interceptor reconnects before expiry.
func TestHeldHtlcSetReleaseAllOffChainHeldKeepsOnChain(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockOnChainInterceptedForward(key, 100)

	require.NoError(t, set.addOnChain(fwd))
	require.Empty(t, set.releaseAllOffChainHeld())
	require.True(t, set.exists(key))
	fwd.AssertNotCalled(t, "Resume")
}

// TestHeldHtlcSetReleaseAllOffChainHeldKeepsReleaseErrors verifies release
// errors leave off-chain entries available for later resolution or expiry.
func TestHeldHtlcSetReleaseAllOffChainHeldKeepsReleaseErrors(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockInterceptedForward(key, 100)
	fwd.On("Resume").Return(errTestForward).Once()

	require.NoError(t, set.addOffChain(fwd))

	errs := set.releaseAllOffChainHeld()
	require.Len(t, errs, 1)
	require.Equal(t, key, errs[0].key)
	require.ErrorIs(t, errs[0].err, errTestForward)
	require.True(t, set.exists(key))
	fwd.AssertExpectations(t)
}

// TestHeldHtlcSetRemoveOnChainHeld verifies contractcourt teardown only removes
// on-chain entries.
func TestHeldHtlcSetRemoveOnChainHeld(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()

	offChain := newMockInterceptedForward(key, 100)
	require.NoError(t, set.addOffChain(offChain))
	require.False(t, set.removeOnChainHeld(key))
	require.True(t, set.exists(key))

	onChain := newMockOnChainInterceptedForward(key, 100)
	require.NoError(t, set.addOnChain(onChain))
	require.True(t, set.removeOnChainHeld(key))
	require.False(t, set.exists(key))
	require.False(t, set.removeOnChainHeld(key))
}

// TestHeldHtlcSetOffChainExpire verifies off-chain expiry fails the HTLC back.
func TestHeldHtlcSetOffChainExpire(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockInterceptedForward(key, 100)

	require.NoError(t, set.addOffChain(fwd))

	require.Empty(t, set.expire(99))
	require.True(t, set.exists(key))
	fwd.AssertNotCalled(t, "FailWithCode", mock.Anything)

	fwd.On(
		"FailWithCode",
		lnwire.CodeTemporaryChannelFailure,
	).Return(nil).Once()
	require.Empty(t, set.expire(100))
	require.False(t, set.exists(key))
	fwd.AssertExpectations(t)
}

// TestHeldHtlcSetOffChainExpireKeepsEntryOnError verifies expiry errors keep
// the off-chain entry available for retry.
func TestHeldHtlcSetOffChainExpireKeepsEntryOnError(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockInterceptedForward(key, 100)
	fwd.On(
		"FailWithCode", lnwire.CodeTemporaryChannelFailure,
	).Return(errTestForward).Once()

	require.NoError(t, set.addOffChain(fwd))

	errs := set.expire(100)
	require.Len(t, errs, 1)
	require.ErrorIs(t, errs[0].err, errTestForward)
	require.Equal(t, key, errs[0].key)
	require.True(t, set.exists(key))
	fwd.AssertExpectations(t)
}

// TestHeldHtlcSetOnChainResolve verifies on-chain entries reject non-settle
// resolutions directly and remain held until settlement.
func TestHeldHtlcSetOnChainResolve(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockOnChainInterceptedForward(key, 100)
	fwd.On("Settle", lntypes.Preimage{}).Return(nil).Once()

	require.NoError(t, set.addOnChain(fwd))
	require.ErrorIs(t, set.resolve(&FwdResolution{
		Key:         key,
		Action:      FwdActionFail,
		FailureCode: lnwire.CodeTemporaryChannelFailure,
	}), ErrCannotFailOnChain)
	require.True(t, set.exists(key))

	require.ErrorIs(t, set.resolve(&FwdResolution{
		Key:    key,
		Action: FwdActionResume,
	}), ErrCannotResumeOnChain)
	require.True(t, set.exists(key))

	require.ErrorIs(t, set.resolve(&FwdResolution{
		Key:    key,
		Action: FwdActionResumeModified,
	}), ErrCannotResumeOnChain)
	require.True(t, set.exists(key))

	require.NoError(t, set.resolve(&FwdResolution{
		Key:    key,
		Action: FwdActionSettle,
	}))
	fwd.AssertExpectations(t)
	fwd.AssertNotCalled(t, "Fail", mock.Anything)
	fwd.AssertNotCalled(t, "FailWithCode", mock.Anything)
	fwd.AssertNotCalled(t, "Resume")
	fwd.AssertNotCalled(t, "ResumeModified", mock.Anything, mock.Anything,
		mock.Anything)
	require.False(t, set.exists(key))
}

// TestHeldHtlcSetOnChainExpirePrunes verifies on-chain expiry only prunes the
// local held entry.
func TestHeldHtlcSetOnChainExpirePrunes(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	fwd := newMockOnChainInterceptedForward(key, 100)

	require.NoError(t, set.addOnChain(fwd))

	require.Empty(t, set.expire(99))
	require.True(t, set.exists(key))

	require.Empty(t, set.expire(100))
	require.False(t, set.exists(key))
	fwd.AssertNotCalled(t, "FailWithCode", mock.Anything)
}

// TestHeldHtlcSetOnChainReplacesOffChain verifies on-chain entries replace
// earlier off-chain entries with the same circuit key.
func TestHeldHtlcSetOnChainReplacesOffChain(t *testing.T) {
	set := newHeldHtlcSet()
	key := testCircuitKey()
	offChain := newMockInterceptedForward(key, 100)
	onChain := newMockOnChainInterceptedForward(key, 100)
	onChain.On("Settle", lntypes.Preimage{}).Return(nil).Once()

	require.NoError(t, set.addOffChain(offChain))
	require.NoError(t, set.addOnChain(onChain))

	require.NoError(t, set.resolve(&FwdResolution{
		Key:    key,
		Action: FwdActionSettle,
	}))

	offChain.AssertNotCalled(t, "Settle", mock.Anything)
	onChain.AssertExpectations(t)
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
		fwd := newMockOnChainInterceptedForward(key, 100)

		require.NoError(t, s.interceptOnChain(fwd))
		require.Len(t, intercepted, 1)
		require.Equal(t, key, intercepted[0].IncomingCircuit)
	})

	t.Run("on-chain htlc replaces off-chain htlc and notifies",
		func(t *testing.T) {
			intercepted = nil
			s := &InterceptableSwitch{
				heldHtlcSet: newHeldHtlcSet(),
				interceptor: interceptor,
			}
			offChain := newMockInterceptedForward(key, 80)
			onChain := newMockOnChainInterceptedForward(key, 100)
			onChain.On("Settle", lntypes.Preimage{}).Return(
				nil,
			).Once()

			require.NoError(t, s.heldHtlcSet.addOffChain(offChain))
			require.NoError(t, s.interceptOnChain(onChain))
			require.Len(t, intercepted, 1)
			require.Equal(t, key, intercepted[0].IncomingCircuit)
			require.Equal(
				t, int32(100), intercepted[0].AutoFailHeight(),
			)

			require.NoError(t, s.resolve(&FwdResolution{
				Key:    key,
				Action: FwdActionSettle,
			}))
			offChain.AssertNotCalled(t, "Settle", mock.Anything)
			onChain.AssertExpectations(t)
		})

	t.Run("on-chain htlc replays after disconnect", func(t *testing.T) {
		intercepted = nil
		s := &InterceptableSwitch{
			heldHtlcSet: newHeldHtlcSet(),
			interceptor: interceptor,
		}
		fwd := newMockOnChainInterceptedForward(key, 100)
		fwd.On("Settle", lntypes.Preimage{}).Return(nil).Once()

		require.NoError(t, s.interceptOnChain(fwd))
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
		fwd.AssertExpectations(t)
		require.False(t, s.heldHtlcSet.exists(key))
	})

	t.Run("duplicate on-chain htlc is not sent", func(t *testing.T) {
		intercepted = nil
		s := &InterceptableSwitch{
			heldHtlcSet: newHeldHtlcSet(),
			interceptor: interceptor,
		}
		fwd := newMockOnChainInterceptedForward(key, 100)

		require.NoError(t, s.interceptOnChain(fwd))
		require.Len(t, intercepted, 1)

		intercepted = nil
		require.NoError(t, s.interceptOnChain(fwd))
		require.Empty(t, intercepted)
	})

	t.Run("on-chain htlc removed after teardown", func(t *testing.T) {
		intercepted = nil
		s := &InterceptableSwitch{
			heldHtlcSet: newHeldHtlcSet(),
			interceptor: interceptor,
		}
		fwd := newMockOnChainInterceptedForward(key, 100)

		require.NoError(t, s.interceptOnChain(fwd))
		require.Len(t, intercepted, 1)

		s.removeOnChainIntercept(key)
		require.False(t, s.heldHtlcSet.exists(key))

		intercepted = nil
		s.setInterceptor(interceptor)
		require.Empty(t, intercepted)
	})
}

// TestInterceptableSwitchRemoveOnChainIntercept verifies that the public
// teardown path removes an on-chain hold through the switch run loop.
func TestInterceptableSwitchRemoveOnChainIntercept(t *testing.T) {
	notifier := &lntestmock.ChainNotifier{
		EpochChan: make(chan *chainntnfs.BlockEpoch, 1),
	}
	notifier.EpochChan <- &chainntnfs.BlockEpoch{Height: 1}

	s, err := NewInterceptableSwitch(&InterceptableSwitchConfig{
		Notifier:           notifier,
		CltvRejectDelta:    10,
		CltvInterceptDelta: 13,
	})
	require.NoError(t, err)
	require.NoError(t, s.Start())
	defer func() {
		require.NoError(t, s.Stop())
	}()

	intercepted := make(chan InterceptedPacket, 2)
	s.SetInterceptor(func(packet InterceptedPacket) error {
		intercepted <- packet

		return nil
	})

	key := testCircuitKey()
	require.NoError(t, s.ForwardPacket(
		newMockOnChainInterceptedForward(key, 100),
	))
	select {
	case packet := <-intercepted:
		require.Equal(t, key, packet.IncomingCircuit)

	case <-time.After(time.Second):
		require.Fail(t, "on-chain hold not intercepted")
	}

	require.NoError(t, s.RemoveOnChainIntercept(key))

	// Re-registering the interceptor replays all currently held HTLCs.
	// The removed on-chain hold should not be replayed.
	s.SetInterceptor(func(packet InterceptedPacket) error {
		intercepted <- packet

		return nil
	})

	// Synchronize with the switch event loop so any replay triggered by the
	// interceptor registration above has already run.
	require.NoError(t, s.RemoveOnChainIntercept(models.CircuitKey{}))

	select {
	case packet := <-intercepted:
		require.Failf(t, "unexpected replay", "packet=%v", packet)

	default:
	}
}

// TestInterceptableSwitchForwardPacketReturnsHoldError verifies that
// ForwardPacket returns the error produced while adding the on-chain hold.
func TestInterceptableSwitchForwardPacketReturnsHoldError(t *testing.T) {
	notifier := &lntestmock.ChainNotifier{
		EpochChan: make(chan *chainntnfs.BlockEpoch, 1),
	}
	notifier.EpochChan <- &chainntnfs.BlockEpoch{Height: 1}

	s, err := NewInterceptableSwitch(&InterceptableSwitchConfig{
		Notifier:           notifier,
		CltvRejectDelta:    10,
		CltvInterceptDelta: 13,
	})
	require.NoError(t, err)
	require.NoError(t, s.Start())
	defer func() {
		require.NoError(t, s.Stop())
	}()

	key := testCircuitKey()
	err = s.ForwardPacket(newMockInterceptedForward(key, 100))
	require.ErrorIs(t, err, errInvalidHeldDeadlineType)
	require.False(t, s.heldHtlcSet.exists(key))

	err = s.ForwardPacket(nil)
	require.ErrorIs(t, err, errNilHeldForward)
}
