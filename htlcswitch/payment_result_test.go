package htlcswitch

import (
	"bytes"
	"io/ioutil"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
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
		ExtraData:       make([]byte, 0),
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

	tempDir, err := ioutil.TempDir("", "testdb")
	if err != nil {
		t.Fatal(err)
	}

	db, err := channeldb.Open(tempDir)
	if err != nil {
		t.Fatal(err)
	}

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
	if err != nil {
		t.Fatalf("unable to subscribe: %v", err)
	}
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
	if err != nil {
		t.Fatalf("unable to store result: %v", err)
	}

	_, err = store.getResult(3)
	if err != nil {
		t.Fatalf("unable to get result: %v", err)
	}

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
