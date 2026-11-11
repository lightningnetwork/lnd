//go:build switchrpc
// +build switchrpc

package switchrpc

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestAttemptKey checks that the attempt key is the client's bytes, and that
// only keys within the length bounds are accepted.
func TestAttemptKey(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		key   *AttemptKey
		valid bool
	}{
		{name: "nil key message"},
		{name: "empty key", key: &AttemptKey{}},
		{name: "15 bytes", key: &AttemptKey{Id: make([]byte, 15)}},
		{name: "16 bytes", key: &AttemptKey{Id: make([]byte, 16)},
			valid: true},
		{name: "64 bytes", key: &AttemptKey{Id: make([]byte, 64)},
			valid: true},
		{name: "65 bytes", key: &AttemptKey{Id: make([]byte, 65)}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Every keyed request reads the key the same way.
			reqs := []keyedAttempt{
				&SendOnionRequest{Key: tc.key},
				&TrackOnionRequest{Key: tc.key},
			}
			for _, req := range reqs {
				key, err := attemptKey(req)
				if !tc.valid {
					require.Equal(t, codes.InvalidArgument,
						status.Code(err))

					continue
				}

				require.NoError(t, err)
				require.Equal(t, tc.key.Id, key)
			}
		})
	}

	// The key is opaque: it is returned unchanged, and the payment hash
	// plays no part in it.
	id := []byte("client-prefix/attempt-0000000001")
	key, err := attemptKey(&SendOnionRequest{
		Key:         &AttemptKey{Id: id},
		PaymentHash: make([]byte, 32),
	})
	require.NoError(t, err)
	require.Equal(t, id, key)
}

// TestRequestFingerprint checks that the fingerprint changes with every field
// that defines the HTLC, and only with those.
func TestRequestFingerprint(t *testing.T) {
	t.Parallel()

	base := func() *SendOnionRequest {
		return &SendOnionRequest{
			OnionBlob:      []byte{1, 2, 3},
			Key:            &AttemptKey{Id: make([]byte, 16)},
			FirstHopChanId: 100,
			Amount:         1000,
			Timelock:       500,
			PaymentHash:    make([]byte, 32),
			CustomRecords: map[uint64][]byte{
				65537: {1},
				65539: {2},
			},
		}
	}
	baseFP := requestFingerprint(base())

	// The same request always yields the same fingerprint.
	require.Equal(t, baseFP, requestFingerprint(base()))

	// The attempt key names the attempt and is not part of the
	// fingerprint.
	req := base()
	req.Key = &AttemptKey{Id: make([]byte, 17)}
	require.Equal(t, baseFP, requestFingerprint(req))

	mutations := map[string]func(*SendOnionRequest){
		"onion": func(r *SendOnionRequest) {
			r.OnionBlob = []byte{1, 2, 4}
		},
		"amount": func(r *SendOnionRequest) {
			r.Amount = 1001
		},
		"timelock": func(r *SendOnionRequest) {
			r.Timelock = 501
		},
		"payment hash": func(r *SendOnionRequest) {
			r.PaymentHash[0] = 1
		},
		"first hop channel": func(r *SendOnionRequest) {
			r.FirstHopChanId = 101
		},
		"first hop by pubkey": func(r *SendOnionRequest) {
			r.FirstHopChanId = 0
			r.FirstHopPubkey = []byte{2, 3}
		},
		"custom record value": func(r *SendOnionRequest) {
			r.CustomRecords[65537] = []byte{9}
		},
		"custom record removed": func(r *SendOnionRequest) {
			delete(r.CustomRecords, 65539)
		},
		"custom record key": func(r *SendOnionRequest) {
			delete(r.CustomRecords, 65539)
			r.CustomRecords[65541] = []byte{2}
		},
		"empty blinding point": func(r *SendOnionRequest) {
			r.BlindingPoint = []byte{}
		},
		"extra data": func(r *SendOnionRequest) {
			r.ExtraData = []byte{5}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req := base()
			mutate(req)
			require.NotEqual(t, baseFP, requestFingerprint(req))
		})
	}
}
