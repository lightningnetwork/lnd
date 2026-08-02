package routing

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/routing/trampoline"
	"github.com/stretchr/testify/require"
)

// TestTrampolineErrorDecrypter verifies origin decryption and relay-only outer
// peeling across two nested trampoline hops.
func TestTrampolineErrorDecrypter(t *testing.T) {
	t.Parallel()

	outerFirst := [32]byte{1}
	outerSecond := [32]byte{2}
	innerFirst := [32]byte{3}
	innerSecond := [32]byte{4}

	failure := &lnwire.FailUnknownNextTrampoline{}
	var encoded bytes.Buffer
	require.NoError(t, lnwire.EncodeFailure(&encoded, failure, 0))

	downstream, err := trampoline.EncryptNestedFailure(
		encoded.Bytes(), innerSecond, outerSecond,
	)
	require.NoError(t, err)

	relayDecrypter := &trampolineErrorDecrypter{
		outerSecrets:  [][32]byte{outerSecond},
		outerHopCount: 1,
	}
	_, err = relayDecrypter.DecryptError(downstream)
	var opaque *OpaqueTrampolineError
	require.ErrorAs(t, err, &opaque)
	require.Len(t, opaque.Reason, trampoline.FailurePacketSize)

	upstream, err := trampoline.WrapNestedFailure(
		opaque.Reason, innerFirst, outerFirst,
	)
	require.NoError(t, err)

	originDecrypter := &trampolineErrorDecrypter{
		outerSecrets: [][32]byte{outerFirst},
		trampolineSecrets: [][32]byte{
			innerFirst, innerSecond,
		},
		outerHopCount: 1,
	}
	decrypted, err := originDecrypter.DecryptError(upstream)
	require.NoError(t, err)
	require.IsType(t, failure, decrypted.WireMessage())
	require.Equal(t, 1, decrypted.FailureSourceIdx)
	require.NotNil(t, decrypted.TrampolineFailureSourceIdx)
	require.Equal(t, 1, *decrypted.TrampolineFailureSourceIdx)
}

// TestTrampolineErrorDecrypterRejectsUnauthenticated verifies an origin never
// accepts a reason that authenticates under neither error path.
func TestTrampolineErrorDecrypterRejectsUnauthenticated(t *testing.T) {
	t.Parallel()

	decrypter := &trampolineErrorDecrypter{
		outerSecrets:      [][32]byte{{1}},
		trampolineSecrets: [][32]byte{{2}},
		outerHopCount:     1,
	}
	_, err := decrypter.DecryptError(
		make([]byte, trampoline.FailurePacketSize),
	)
	require.Error(t, err)
	require.True(t, errors.Is(
		err, trampoline.ErrNestedErrorUndecryptable,
	))
}

// TestOpaqueTrampolineFailurePersistence verifies the payment lifecycle marks
// an authenticated relay-only failure distinctly and can reconstruct it from
// durable attempt state without aliasing the database byte array.
func TestOpaqueTrampolineFailurePersistence(t *testing.T) {
	t.Parallel()

	reason := bytes.Repeat([]byte{0x7a}, trampoline.FailurePacketSize)
	failInfo := marshallError(
		&OpaqueTrampolineError{Reason: reason}, time.Unix(123, 0),
	)
	require.Equal(t, paymentsdb.HTLCFailTrampoline, failInfo.Reason)
	require.Equal(t, reason, failInfo.TrampolineReason[:])

	paymentFailure := paymentsdb.FailureReasonError
	payment := &paymentsdb.MPPayment{
		HTLCs:         []paymentsdb.HTLCAttempt{{Failure: failInfo}},
		FailureReason: &paymentFailure,
	}
	opaque := TrampolineFailureFromPayment(payment)
	require.NotNil(t, opaque)
	require.Equal(t, lnwire.OpaqueReason(reason), opaque.Reason)

	opaque.Reason[0] ^= 0xff
	require.Equal(t, byte(0x7a), failInfo.TrampolineReason[0])
}
