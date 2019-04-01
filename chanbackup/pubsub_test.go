package chanbackup

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/keychain"
)

type mockSwapper struct {
	fail bool

	swaps chan PackedMulti
}

func newMockSwapper() *mockSwapper {
	return &mockSwapper{
		swaps: make(chan PackedMulti),
	}
}

func (m *mockSwapper) UpdateAndSwap(newBackup PackedMulti) error {
	if m.fail {
		return fmt.Errorf("fail")
	}

	m.swaps <- newBackup

	return nil
}

type mockChannelNotifier struct {
	fail bool

	chanEvents chan ChannelEvent
}

func newMockChannelNotifier() *mockChannelNotifier {
	return &mockChannelNotifier{
		chanEvents: make(chan ChannelEvent),
	}
}

func (m *mockChannelNotifier) SubscribeChans(chans map[wire.OutPoint]struct{}) (
	*ChannelSubscription, error) {

	if m.fail {
		return nil, fmt.Errorf("fail")
	}

	return &ChannelSubscription{
		ChanUpdates: m.chanEvents,
		Cancel: func() {
		},
	}, nil
}

// TestNewSubSwapperSubscribeFail tests that if we're unable to obtain a
// channel subscription, then the entire sub-swapper will fail to start.
func TestNewSubSwapperSubscribeFail(t *testing.T) {
	t.Parallel()

	keyRing := &mockKeyRing{}

	var swapper mockSwapper
	chanNotifier := mockChannelNotifier{
		fail: true,
	}

	_, err := NewSubSwapper(nil, &chanNotifier, keyRing, &swapper)
	if err == nil {
		t.Fatalf("expected fail due to lack of subscription")
	}
}

func assertExpectedBackupSwap(t *testing.T, swapper *mockSwapper,
	subSwapper *SubSwapper, keyRing keychain.KeyRing,
	expectedChanSet map[wire.OutPoint]Single) {

	t.Helper()

	select {
	case newPackedMulti := <-swapper.swaps:
		// If we unpack the new multi, then we should find all the old
		// channels, and also the new channel included and any deleted
		// channel omitted..
		newMulti, err := newPackedMulti.Unpack(keyRing)
		if err != nil {
			t.Fatalf("unable to unpack multi: %v", err)
		}

		// Ensure that once unpacked, the current backup has the
		// expected number of Singles.
		if len(newMulti.StaticBackups) != len(expectedChanSet) {
			t.Fatalf("new backup wasn't included: expected %v "+
				"backups have %v", len(expectedChanSet),
				len(newMulti.StaticBackups))
		}

		// We should also find all the old and new channels in this new
		// backup.
		for _, backup := range newMulti.StaticBackups {
			_, ok := expectedChanSet[backup.FundingOutpoint]
			if !ok {
				t.Fatalf("didn't find backup in original set: %v",
					backup.FundingOutpoint)
			}
		}

		// The internal state of the sub-swapper should also be one
		// larger.
		if !reflect.DeepEqual(expectedChanSet, subSwapper.backupState) {
			t.Fatalf("backup set doesn't match: expected %v got %v",
				spew.Sdump(expectedChanSet),
				spew.Sdump(subSwapper.backupState))
		}

	case <-time.After(time.Second * 5):
		t.Fatalf("update swapper didn't swap out multi")
	}
}

// TestSubSwapperIdempotentStartStop tests that calling the Start/Stop methods
// multiple time is permitted.
func TestSubSwapperIdempotentStartStop(t *testing.T) {
	t.Parallel()

	keyRing := &mockKeyRing{}

	var chanNotifier mockChannelNotifier

	swapper := newMockSwapper()
	subSwapper, err := NewSubSwapper(nil, &chanNotifier, keyRing, swapper)
	if err != nil {
		t.Fatalf("unable to init subSwapper: %v", err)
	}

	if err := subSwapper.Start(); err != nil {
		t.Fatalf("unable to start swapper: %v", err)
	}

	// The swapper should write the initial channel state as soon as it's
	// active.
	backupSet := make(map[wire.OutPoint]Single)
	assertExpectedBackupSwap(t, swapper, subSwapper, keyRing, backupSet)

	subSwapper.Start()

	subSwapper.Stop()
	subSwapper.Stop()
}

// TestSubSwapperUpdater tests that the SubSwapper will properly swap out
// new/old channels within the channel set, and notify the swapper to update
// the master multi file backup.
func TestSubSwapperUpdater(t *testing.T) {
	t.Parallel()

	keyRing := &mockKeyRing{}
	chanNotifier := newMockChannelNotifier()
	swapper := newMockSwapper()

	// First, we'll start out by creating a channels set for the initial
	// set of channels known to the sub-swapper.
	const numStartingChans = 3
	initialChanSet := make([]Single, 0, numStartingChans)
	backupSet := make(map[wire.OutPoint]Single)
	for i := 0; i < numStartingChans; i++ {
		channel, err := genRandomOpenChannelShell()
		if err != nil {
			t.Fatalf("unable to make test chan: %v", err)
		}

		single := NewSingle(channel, nil)

		backupSet[channel.FundingOutpoint] = single
		initialChanSet = append(initialChanSet, single)
	}

	// With our channel set created, we'll make a fresh sub swapper
	// instance to begin our test.
	subSwapper, err := NewSubSwapper(
		initialChanSet, chanNotifier, keyRing, swapper,
	)
	if err != nil {
		t.Fatalf("unable to make swapper: %v", err)
	}
	if err := subSwapper.Start(); err != nil {
		t.Fatalf("unable to start sub swapper: %v", err)
	}
	defer subSwapper.Stop()

	// The swapper should write the initial channel state as soon as it's
	// active.
	assertExpectedBackupSwap(t, swapper, subSwapper, keyRing, backupSet)

	// Now that the sub-swapper is active, we'll notify to add a brand new
	// channel to the channel state.
	newChannel, err := genRandomOpenChannelShell()
	if err != nil {
		t.Fatalf("unable to create new chan: %v", err)
	}

	// With the new channel created, we'll send a new update to the main
	// goroutine telling it about this new channel.
	select {
	case chanNotifier.chanEvents <- ChannelEvent{
		NewChans: []ChannelWithAddrs{
			{
				OpenChannel: newChannel,
			},
		},
	}:
	case <-time.After(time.Second * 5):
		t.Fatalf("update swapper didn't read new channel: %v", err)
	}

	backupSet[newChannel.FundingOutpoint] = NewSingle(newChannel, nil)

	// At this point, the sub-swapper should now have packed a new multi,
	// and then sent it to the swapper so the back up can be updated.
	assertExpectedBackupSwap(t, swapper, subSwapper, keyRing, backupSet)

	// We'll now trigger an update to remove an existing channel.
	chanToDelete := initialChanSet[0].FundingOutpoint
	select {
	case chanNotifier.chanEvents <- ChannelEvent{
		ClosedChans: []wire.OutPoint{chanToDelete},
	}:

	case <-time.After(time.Second * 5):
		t.Fatalf("update swapper didn't read new channel: %v", err)
	}

	delete(backupSet, chanToDelete)

	// Verify that the new set of backups, now has one less after the
	// sub-swapper switches the new set with the old.
	assertExpectedBackupSwap(t, swapper, subSwapper, keyRing, backupSet)
}
