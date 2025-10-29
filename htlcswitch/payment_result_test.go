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

	store, err := newNetworkResultStore(db, false)
	require.NoError(t, err, "unable create result store")

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

// TestDisableRemoteRouter tests that the DisableRemoteRouter method behaves as
// expected.
func TestDisableRemoteRouter(t *testing.T) {
	t.Parallel()

	checkMarker := func(t *testing.T, db kvdb.Backend) bool {
		var marked bool
		err := db.View(func(tx kvdb.RTx) error {
			bucket := tx.ReadBucket(remoteRouterMarkerBucket)
			if bucket != nil &&
				bucket.Get(remoteRouterMarkerKey) != nil {

				marked = true
			}
			return nil
		}, func() {
			marked = false
		})
		require.NoError(t, err)
		return marked
	}

	// Test case 1: In-flight payments exist.
	t.Run("in-flight payments", func(t *testing.T) {
		t.Parallel()

		db := channeldb.OpenForTesting(t, t.TempDir())
		store, err := newNetworkResultStore(db, true)
		require.NoError(t, err)
		require.True(t, checkMarker(t, db))

		// Add a payment result to simulate an in-flight payment.
		err = store.storeResult(0, &networkResult{
			msg: &lnwire.UpdateAddHTLC{},
		})
		require.NoError(t, err)

		// Attempt to disable the remote router.
		err = store.DisableRemoteRouter()
		require.Error(t, err)
		require.Contains(t, err.Error(), "in-flight payments exist")

		// The marker should still exist.
		require.True(t, checkMarker(t, db))
	})

	// Test case 2: No in-flight payments.
	t.Run("no in-flight payments", func(t *testing.T) {
		t.Parallel()

		db := channeldb.OpenForTesting(t, t.TempDir())
		store, err := newNetworkResultStore(db, true)
		require.NoError(t, err)
		require.True(t, checkMarker(t, db))

		// Attempt to disable the remote router.
		err = store.DisableRemoteRouter()
		require.NoError(t, err)

		// The marker should be gone.
		require.False(t, checkMarker(t, db))
	})

	// Test case 3: No in-flight payments and no marker.
	t.Run("no marker", func(t *testing.T) {
		t.Parallel()

		db := channeldb.OpenForTesting(t, t.TempDir())
		store, err := newNetworkResultStore(db, false)
		require.NoError(t, err)
		require.False(t, checkMarker(t, db))

		// Attempt to disable the remote router.
		err = store.DisableRemoteRouter()
		require.NoError(t, err)

		// The marker should still be gone.
		require.False(t, checkMarker(t, db))
	})
}
