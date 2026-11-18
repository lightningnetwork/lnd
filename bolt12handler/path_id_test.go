package bolt12handler

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestPathIDVectors pins the path ids and the preimage of one node key. Offers
// and invoices outlive a node restart and an upgrade, so a change in the key
// derivation or the HMAC input breaks the paths and payments in flight.
func TestPathIDVectors(t *testing.T) {
	t.Parallel()

	const (
		offerID = "a154a15c8c4f5cf901d0b9e03622fb97" +
			"758ed94a1fba1f42dcb1ffdb1a28503f"
		replyID = "b559429c22ae07cdfe0a902b53406f39" +
			"862128a729cb6ba19d072c87db58b0f8"
		paymentID = "02000000000000000000000000000000" +
			"7253c83b388302b9f9683d465c294cba"
		preimage = "aeeaa2d57ab8850e3b60c84a0b8c9dff" +
			"81d97c76fd847b6eb0853fcb319dbab3"
	)

	key, err := DerivePathIDKey(&keychain.PrivKeyECDH{PrivKey: testKey(t)})
	require.NoError(t, err)
	binding := [32]byte{1}

	gotOffer := key.New(PathIDOffer, binding)
	require.Equal(t, offerID, hex.EncodeToString(gotOffer[:]))

	gotReply := key.New(PathIDReply, binding)
	require.Equal(t, replyID, hex.EncodeToString(gotReply[:]))

	id, err := hex.DecodeString(paymentID)
	require.NoError(t, err)
	gotPreimage, ok := key.checkPaymentPathID(id, binding)
	require.True(t, ok)
	require.Equal(t, preimage, hex.EncodeToString(gotPreimage[:]))
}

// TestPathIDContexts verifies that a path id of one kind never verifies as a
// path id of another kind, for any binding.
func TestPathIDContexts(t *testing.T) {
	t.Parallel()

	key, err := DerivePathIDKey(&keychain.PrivKeyECDH{PrivKey: testKey(t)})
	require.NoError(t, err)
	contexts := []PathIDContext{PathIDOffer, PathIDReply}

	rapid.Check(t, func(t *rapid.T) {
		binding := [32]byte(rapid.SliceOfN(
			rapid.Byte(), 32, 32,
		).Draw(t, "binding"))

		for _, ctx := range contexts {
			id := key.New(ctx, binding)
			require.True(t, key.isOurs(ctx, id[:]))

			requireOtherContextsReject(
				t, key, contexts, ctx, id[:], binding,
			)

			_, ok := key.checkPaymentPathID(id[:], binding)
			require.False(t, ok)
		}

		paymentID, err := key.newPaymentPathID(binding)
		require.NoError(t, err)
		for _, ctx := range contexts {
			require.False(t, key.isOurs(ctx, paymentID[:]))
			require.False(
				t, key.matches(ctx, paymentID[:], binding),
			)
		}
	})
}

// requireOtherContextsReject asserts that every context in contexts except ctx
// rejects the path id.
func requireOtherContextsReject(t require.TestingT, key PathIDKey,
	contexts []PathIDContext, ctx PathIDContext, id []byte,
	binding [32]byte) {

	for _, other := range contexts {
		if other == ctx {
			continue
		}
		require.False(t, key.isOurs(other, id))
		require.False(t, key.matches(other, id, binding))
	}
}

// TestPathIDIsOurs verifies that isOurs accepts every path id that this node
// made for the kind, whatever its message, and nothing else.
func TestPathIDIsOurs(t *testing.T) {
	t.Parallel()

	key, err := DerivePathIDKey(&keychain.PrivKeyECDH{PrivKey: testKey(t)})
	require.NoError(t, err)
	id := key.New(PathIDOffer, [32]byte{1})
	otherMessage := key.New(PathIDOffer, [32]byte{2})

	otherNode, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	otherKey, err := DerivePathIDKey(
		&keychain.PrivKeyECDH{PrivKey: otherNode},
	)
	require.NoError(t, err)
	foreign := otherKey.New(PathIDOffer, [32]byte{1})

	changed := id
	changed[pathIDLen-1] ^= 1

	tests := []struct {
		name string
		id   []byte
		want bool
	}{
		{name: "ours", id: id[:], want: true},
		{name: "other message", id: otherMessage[:], want: true},
		{name: "other node", id: foreign[:]},
		{name: "changed tag", id: changed[:]},
		{name: "short", id: id[:pathIDLen-1]},
		{name: "none"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := key.isOurs(PathIDOffer, tc.id)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestPathIDMatches verifies that a path id matches only the message it was
// made for.
func TestPathIDMatches(t *testing.T) {
	t.Parallel()

	key, err := DerivePathIDKey(&keychain.PrivKeyECDH{PrivKey: testKey(t)})
	require.NoError(t, err)
	id := key.New(PathIDOffer, [32]byte{1})

	require.True(t, key.matches(PathIDOffer, id[:], [32]byte{1}))
	require.False(t, key.matches(PathIDOffer, id[:], [32]byte{2}))
}

// TestPaymentPathID verifies that a payment path id gives its preimage, and
// that the path id does not show the preimage to the payer.
func TestPaymentPathID(t *testing.T) {
	t.Parallel()

	key, err := DerivePathIDKey(&keychain.PrivKeyECDH{PrivKey: testKey(t)})
	require.NoError(t, err)
	binding := [32]byte{1}

	id, err := key.newPaymentPathID(binding)
	require.NoError(t, err)

	preimage, ok := key.checkPaymentPathID(id[:], binding)
	require.True(t, ok)
	require.NotEqual(t, lntypes.Preimage{}, preimage)
	require.NotContains(
		t, hex.EncodeToString(preimage[:]),
		hex.EncodeToString(id[pathIDHalfLen:]),
	)
}

// TestCheckPaymentPathIDRejects verifies that a payment path id gives no
// preimage when its nonce, its tag or the envelope binding changed.
func TestCheckPaymentPathIDRejects(t *testing.T) {
	t.Parallel()

	key, err := DerivePathIDKey(&keychain.PrivKeyECDH{PrivKey: testKey(t)})
	require.NoError(t, err)
	binding := [32]byte{1}

	id, err := key.newPaymentPathID(binding)
	require.NoError(t, err)

	changedBinding := binding
	changedBinding[31] ^= 1
	changedNonce := id
	changedNonce[0] ^= 1
	changedTag := id
	changedTag[pathIDLen-1] ^= 1

	tests := []struct {
		name    string
		id      []byte
		binding [32]byte
	}{
		{name: "changed binding", id: id[:], binding: changedBinding},
		{name: "changed nonce", id: changedNonce[:], binding: binding},
		{name: "changed tag", id: changedTag[:], binding: binding},
		{name: "short", id: id[:pathIDLen-1], binding: binding},
		{name: "none", binding: binding},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, ok := key.checkPaymentPathID(tc.id, tc.binding)
			require.False(t, ok)
		})
	}
}

// TestPaymentPathIDUnique verifies that two invoices with one binding get two
// path ids and two preimages.
func TestPaymentPathIDUnique(t *testing.T) {
	t.Parallel()

	key, err := DerivePathIDKey(&keychain.PrivKeyECDH{PrivKey: testKey(t)})
	require.NoError(t, err)
	binding := [32]byte{1}

	id, err := key.newPaymentPathID(binding)
	require.NoError(t, err)
	otherID, err := key.newPaymentPathID(binding)
	require.NoError(t, err)
	require.NotEqual(t, id, otherID)

	preimage, ok := key.checkPaymentPathID(id[:], binding)
	require.True(t, ok)
	otherPreimage, ok := key.checkPaymentPathID(otherID[:], binding)
	require.True(t, ok)
	require.NotEqual(t, preimage, otherPreimage)
}

// TestZeroPathIDKey verifies that the zero key is unusable. Anybody can compute
// path ids under it, so no check may accept one.
func TestZeroPathIDKey(t *testing.T) {
	t.Parallel()

	var zero PathIDKey
	binding := [32]byte{1}

	for _, ctx := range []PathIDContext{PathIDOffer, PathIDReply} {
		id := zero.New(ctx, binding)
		require.False(t, zero.isOurs(ctx, id[:]))
		require.False(t, zero.matches(ctx, id[:], binding))
	}

	// A forger computes a payment path id under the zero key.
	var forged [pathIDLen]byte
	copy(
		forged[pathIDHalfLen:],
		zero.mac('t', pathIDPayment, paymentInput(forged[:], binding)),
	)
	_, ok := zero.checkPaymentPathID(forged[:], binding)
	require.False(t, ok)

	_, err := zero.newPaymentPathID(binding)
	require.ErrorContains(t, err, "zero path id key")
}

// TestPathIDContextLengthPrefix verifies that the context length prefix keeps
// inputs apart when the context and data split the same bytes differently.
func TestPathIDContextLengthPrefix(t *testing.T) {
	t.Parallel()

	key, err := DerivePathIDKey(&keychain.PrivKeyECDH{PrivKey: testKey(t)})
	require.NoError(t, err)

	require.NotEqual(
		t, key.sum('t', "ab", []byte("c")),
		key.sum('t', "a", []byte("bc")),
	)
}
