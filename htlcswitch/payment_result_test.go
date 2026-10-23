package htlcswitch

import (
	"bytes"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestNetworkResultSerialization checks that NetworkResults are properly
// (de)serialized.
func TestNetworkResultSerialization(t *testing.T) {
	t.Parallel()

	var preimage lntypes.Preimage
	if _, err := rand.Read(preimage[:]); err != nil {
		t.Fatalf("unable gen rand preimag: %v", err)
	}

	var chanID lnwire.ChannelID
	if _, err := rand.Read(chanID[:]); err != nil {
		t.Fatalf("unable gen rand chanid: %v", err)
	}

	var reason [256]byte
	if _, err := rand.Read(reason[:]); err != nil {
		t.Fatalf("unable gen rand reason: %v", err)
	}

	settle := &lnwire.UpdateFulfillHTLC{
		ChanID:          chanID,
		ID:              2,
		PaymentPreimage: preimage,
	}

	fail := &lnwire.UpdateFailHTLC{
		ChanID:    chanID,
		ID:        1,
		Reason:    []byte{},
		ExtraData: make([]byte, 0),
	}

	fail2 := &lnwire.UpdateFailHTLC{
		ChanID:    chanID,
		ID:        1,
		Reason:    reason[:],
		ExtraData: make([]byte, 0),
	}

	testCases := []*networkResult{
		{
			msg: settle,
		},
		{
			msg:          fail,
			unencrypted:  false,
			isResolution: false,
		},
		{
			msg:          fail,
			unencrypted:  false,
			isResolution: true,
		},
		{
			msg:          fail2,
			unencrypted:  true,
			isResolution: false,
		},
	}

	for _, p := range testCases {
		var buf bytes.Buffer
		if err := serializeNetworkResult(&buf, p); err != nil {
			t.Fatalf("serialize failed: %v", err)
		}

		r := bytes.NewReader(buf.Bytes())
		p1, err := deserializeNetworkResult(r)
		if err != nil {
			t.Fatalf("unable to deserizlize: %v", err)
		}

		if !reflect.DeepEqual(p, p1) {
			t.Fatalf("not equal. %v vs %v", spew.Sdump(p),
				spew.Sdump(p1))
		}
	}
}

// TestNetworkResultStore tests that the networkResult store behaves as
// expected, and that we can store, get and subscribe to results.
func TestNetworkResultStore(t *testing.T) {
	t.Parallel()

	const numResults = 4

	db := channeldb.OpenForTesting(t, t.TempDir())

	store := newNetworkResultStore(db)

	var results []*networkResult
	for i := 0; i < numResults; i++ {
		n := &networkResult{
			msg:          &lnwire.UpdateAddHTLC{},
			unencrypted:  true,
			isResolution: true,
		}
		results = append(results, n)
	}

	// Subscribe to 2 of them.
	var subs []<-chan *networkResult
	for i := uint64(0); i < 2; i++ {
		sub, err := store.subscribeResult(i)
		if err != nil {
			t.Fatalf("unable to subscribe: %v", err)
		}
		subs = append(subs, sub)
	}

	// Store three of them.
	for i := uint64(0); i < 3; i++ {
		err := store.storeResult(i, results[i])
		if err != nil {
			t.Fatalf("unable to store result: %v", err)
		}
	}

	// The two subscribers should be notified.
	for _, sub := range subs {
		select {
		case <-sub:
		case <-time.After(1 * time.Second):
			t.Fatalf("no result received")
		}
	}

	// Let the third one subscribe now. THe result should be received
	// immediately.
	sub, err := store.subscribeResult(2)
	require.NoError(t, err, "unable to subscribe")
	select {
	case <-sub:
	case <-time.After(1 * time.Second):
		t.Fatalf("no result received")
	}

	// Try fetching the result directly for the non-stored one. This should
	// fail.
	_, err = store.getResult(3)
	if err != ErrPaymentIDNotFound {
		t.Fatalf("expected ErrPaymentIDNotFound, got %v", err)
	}

	// Add the result and try again.
	err = store.storeResult(3, results[3])
	require.NoError(t, err, "unable to store result")

	_, err = store.getResult(3)
	require.NoError(t, err, "unable to get result")

	// Since we don't delete results from the store (yet), make sure we
	// will get subscriptions for all of them.
	for i := uint64(0); i < numResults; i++ {
		sub, err := store.subscribeResult(i)
		if err != nil {
			t.Fatalf("unable to subscribe: %v", err)
		}

		select {
		case <-sub:
		case <-time.After(1 * time.Second):
			t.Fatalf("no result received")
		}
	}

	// Clean the store keeping the first two results.
	toKeep := map[uint64]struct{}{
		0: {},
		1: {},
	}
	// Finally, delete the result.
	err = store.cleanStore(toKeep)
	require.NoError(t, err)

	// Payment IDs 0 and 1 should be found, 2 and 3 should be deleted.
	for i := uint64(0); i < numResults; i++ {
		_, err = store.getResult(i)
		if i <= 1 {
			require.NoError(t, err, "unable to get result")
		}
		if i >= 2 && err != ErrPaymentIDNotFound {
			t.Fatalf("expected ErrPaymentIDNotFound, got %v", err)
		}
	}
}

// TestNetworkResultStoreCleanStore tests that cleanStore prunes the store down
// to exactly the requested attempt IDs. It opens the store via
// kvdb.GetTestBackend so that, under the postgres/sqlite build tags, the SQL
// drop-and-recreate path is exercised (TestNetworkResultStore above goes
// through channeldb.OpenForTesting, which always opens bbolt).
func TestNetworkResultStoreCleanStore(t *testing.T) {
	if kvdb.PostgresBackend {
		stop, err := kvdb.StartEmbeddedPostgres()
		require.NoError(t, err)

		t.Cleanup(func() {
			require.NoError(t, stop())
		})
	} else {
		t.Parallel()
	}

	tests := []struct {
		name       string
		numResults uint64
		keep       map[uint64]struct{}
	}{
		{
			name:       "partial keep",
			numResults: 5,
			keep:       map[uint64]struct{}{0: {}, 2: {}, 4: {}},
		},
		{
			name:       "empty keep",
			numResults: 5,
			keep:       map[uint64]struct{}{},
		},
		{
			name:       "keep all",
			numResults: 5,
			keep: map[uint64]struct{}{
				0: {}, 1: {}, 2: {}, 3: {}, 4: {},
			},
		},
		{
			name:       "empty store",
			numResults: 0,
			keep:       map[uint64]struct{}{0: {}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// GetTestBackend keys its fixture off the unique temp
			// dir, so parallel subtests get independent databases.
			backend, cleanup, err := kvdb.GetTestBackend(
				t.TempDir(), "db",
			)
			require.NoError(t, err)
			t.Cleanup(cleanup)

			store := newNetworkResultStore(backend)

			// Store a result for every attempt ID.
			for i := range test.numResults {
				msg := &lnwire.UpdateAddHTLC{ID: i}
				n := &networkResult{
					msg:          msg,
					unencrypted:  true,
					isResolution: true,
				}
				require.NoError(t, store.storeResult(i, n))
			}

			// Prune, keeping only the IDs in the keep set.
			require.NoError(t, store.cleanStore(test.keep))

			// Every kept ID must still be present; every other ID
			// must have been removed.
			for i := range test.numResults {
				result, err := store.getResult(i)
				if _, ok := test.keep[i]; ok {
					require.NoErrorf(t, err,
						"kept id %d missing", i)
					msg := result.msg
					add, ok := msg.(*lnwire.UpdateAddHTLC)
					require.True(t, ok)
					require.Equal(t, i, add.ID)

					continue
				}

				require.ErrorIsf(t, err, ErrPaymentIDNotFound,
					"id %d not removed", i)
			}
		})
	}
}
