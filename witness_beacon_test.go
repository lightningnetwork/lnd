package main

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/chainntnfs"
)

var (
	preimage = [32]byte{
		0xa7, 0xfd, 0xc6, 0xf2, 0x8, 0xb4, 0xd8, 0x1b,
		0x5d, 0xc3, 0x95, 0x73, 0x70, 0x94, 0x1b, 0x4d,
		0x17, 0x3b, 0xf0, 0x3d, 0x7e, 0x85, 0xb1, 0x77,
		0x1f, 0xed, 0x33, 0xd4, 0x48, 0x1c, 0x42, 0xfd,
	}
	payhash = sha256.Sum256(preimage[:])
)

func createPreimageBeacon() *mockWitnessBeacon {
	notifier := &mockNotfier{
		confChannel:  nil,
		epochChannel: make(chan *chainntnfs.BlockEpoch),
	}

	witnessBeacon := &mockWitnessBeacon{
		cache:    make(map[[32]byte]*preimageAndExp),
		notifier: notifier,
		quit:     make(chan struct{}),
	}

	return witnessBeacon
}

// TestPreimageBeaconGarbageCollector adds several preimages to the WitnessBeacon
// and tests that, upon receiving blocks, the expired preimages are deleted.
func TestPreimageBeaconGarbageCollector(t *testing.T) {
	witnessBeacon := createPreimageBeacon()

	// Start garbage collector
	witnessBeacon.Start()
	defer witnessBeacon.Stop()

	// Add the preimage to the underlying cache
	witnessBeacon.AddPreimage(preimage[:])

	witnessBeacon.notifier.epochChannel <- &chainntnfs.BlockEpoch{
		Hash:   nil,
		Height: 6,
	}

	// Sleep so that the GC, which is in a goroutine, can clean up
	// the expired preimage.
	time.Sleep(2000 * time.Millisecond)

	// Make sure that the preimage was deleted from the cache
	_, ok := witnessBeacon.LookupPreimage(payhash[:])
	if ok {
		t.Fatal("The preimage was not deleted from the cache even" +
			"though it's expired.")
	}
}
