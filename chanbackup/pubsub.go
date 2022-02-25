package chanbackup

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
)

// Swapper is an interface that allows the chanbackup.SubSwapper to update the
// main multi backup location once it learns of new channels or that prior
// channels have been closed.
type Swapper interface {
	// UpdateAndSwap attempts to atomically update the main multi back up
	// file location with the new fully packed multi-channel backup.
	UpdateAndSwap(newBackup PackedMulti) error

	// ExtractMulti attempts to obtain and decode the current SCB instance
	// stored by the Swapper instance.
	ExtractMulti(keychain keychain.KeyRing) (*Multi, error)
}

// ChannelWithAddrs bundles an open channel along with all the addresses for
// the channel peer.
type ChannelWithAddrs struct {
	*channeldb.OpenChannel

	// Addrs is the set of addresses that we can use to reach the target
	// peer.
	Addrs []net.Addr
}

// ChannelEvent packages a new update of new channels since subscription, and
// channels that have been opened since prior channel event.
type ChannelEvent struct {
	// ClosedChans are the set of channels that have been closed since the
	// last event.
	ClosedChans []wire.OutPoint

	// NewChans is the set of channels that have been opened since the last
	// event.
	NewChans []ChannelWithAddrs
}

// ChannelSubscription represents an intent to be notified of any updates to
// the primary channel state.
type ChannelSubscription struct {
	// ChanUpdates is a channel that will be sent upon once the primary
	// channel state is updated.
	ChanUpdates chan ChannelEvent

	// Cancel is a closure that allows the caller to cancel their
	// subscription and free up any resources allocated.
	Cancel func()
}

// ChannelNotifier represents a system that allows the chanbackup.SubSwapper to
// be notified of any changes to the primary channel state.
type ChannelNotifier interface {
	// SubscribeChans requests a new channel subscription relative to the
	// initial set of known channels. We use the knownChans as a
	// synchronization point to ensure that the chanbackup.SubSwapper does
	// not miss any channel open or close events in the period between when
	// it's created, and when it requests the channel subscription.
	SubscribeChans(map[wire.OutPoint]struct{}) (*ChannelSubscription, error)
}

// SubSwapper subscribes to new updates to the open channel state, and then
// swaps out the on-disk channel backup state in response.  This sub-system
// that will ensure that the multi chan backup file on disk will always be
// updated with the latest channel back up state. We'll receive new
// opened/closed channels from the ChannelNotifier, then use the Swapper to
// update the file state on disk with the new set of open channels.  This can
// be used to implement a system that always keeps the multi-chan backup file
// on disk in a consistent state for safety purposes.
type SubSwapper struct {
	started sync.Once
	stopped sync.Once

	// backupState are the set of SCBs for all open channels we know of.
	backupState map[wire.OutPoint]Single

	// chanEvents is an active subscription to receive new channel state
	// over.
	chanEvents *ChannelSubscription

	// keyRing is the main key ring that will allow us to pack the new
	// multi backup.
	keyRing keychain.KeyRing

	Swapper

	quit chan struct{}
	wg   sync.WaitGroup
}

// NewSubSwapper creates a new instance of the SubSwapper given the starting
// set of channels, and the required interfaces to be notified of new channel
// updates, pack a multi backup, and swap the current best backup from its
// storage location.
func NewSubSwapper(startingChans []Single, chanNotifier ChannelNotifier,
	keyRing keychain.KeyRing, backupSwapper Swapper) (*SubSwapper, error) {

	// First, we'll subscribe to the latest set of channel updates given
	// the set of channels we already know of.
	knownChans := make(map[wire.OutPoint]struct{})
	for _, chanBackup := range startingChans {
		knownChans[chanBackup.FundingOutpoint] = struct{}{}
	}
	chanEvents, err := chanNotifier.SubscribeChans(knownChans)
	if err != nil {
		return nil, err
	}

	// Next, we'll construct our own backup state so we can add/remove
	// channels that have been opened and closed.
	backupState := make(map[wire.OutPoint]Single)
	for _, chanBackup := range startingChans {
		backupState[chanBackup.FundingOutpoint] = chanBackup
	}

	return &SubSwapper{
		backupState: backupState,
		chanEvents:  chanEvents,
		keyRing:     keyRing,
		Swapper:     backupSwapper,
		quit:        make(chan struct{}),
	}, nil
}

// Start starts the chanbackup.SubSwapper.
func (s *SubSwapper) Start() error {
	var startErr error
	s.started.Do(func() {
		log.Infof("chanbackup.SubSwapper starting")

		// Before we enter our main loop, we'll update the on-disk
		// state with the latest Single state, as nodes may have new
		// advertised addresses.
		if err := s.updateBackupFile(); err != nil {
			startErr = fmt.Errorf("unable to refresh backup "+
				"file: %v", err)
			return
		}

		s.wg.Add(1)
		go s.backupUpdater()
	})

	return startErr
}

// Stop signals the SubSwapper to being a graceful shutdown.
func (s *SubSwapper) Stop() error {
	s.stopped.Do(func() {
		log.Infof("Stopping chanbackup.SubSwapper")

		close(s.quit)
		s.wg.Wait()
	})
	return nil
}

// updateBackupFile updates the backup file in place given the current state of
// the SubSwapper. We accept the set of channels that were closed between this
// update and the last to make sure we leave them out of our backup set union.
func (s *SubSwapper) updateBackupFile(closedChans ...wire.OutPoint) error {
	// Before we pack the new set of SCBs, we'll first decode what we
	// already have on-disk, to make sure we can decode it (proper seed)
	// and that we're able to combine it with our new data.
	diskMulti, err := s.Swapper.ExtractMulti(s.keyRing)

	// If the file doesn't exist on disk, then that's OK as it was never
	// created. In this case we'll continue onwards as it isn't a critical
	// error.
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("unable to extract on disk encrypted "+
			"SCB: %v", err)
	}

	// Now that we have channels stored on-disk, we'll create a new set of
	// the combined old and new channels to make sure we retain what's
	// already on-disk.
	//
	// NOTE: The ordering of this operations means that our in-memory
	// structure will replace what we read from disk.
	combinedBackup := make(map[wire.OutPoint]Single)
	if diskMulti != nil {
		for _, diskChannel := range diskMulti.StaticBackups {
			chanPoint := diskChannel.FundingOutpoint
			combinedBackup[chanPoint] = diskChannel
		}
	}
	for _, memChannel := range s.backupState {
		chanPoint := memChannel.FundingOutpoint
		if _, ok := combinedBackup[chanPoint]; ok {
			log.Warnf("Replacing disk backup for ChannelPoint(%v) "+
				"w/ newer version", chanPoint)
		}

		combinedBackup[chanPoint] = memChannel
	}

	// Remove the set of closed channels from the final set of backups.
	for _, closedChan := range closedChans {
		delete(combinedBackup, closedChan)
	}

	// With our updated channel state obtained, we'll create a new multi
	// from our series of singles.
	var newMulti Multi
	for _, backup := range combinedBackup {
		newMulti.StaticBackups = append(
			newMulti.StaticBackups, backup,
		)
	}

	// Now that our multi has been assembled, we'll attempt to pack
	// (encrypt+encode) the new channel state to our target reader.
	var b bytes.Buffer
	err = newMulti.PackToWriter(&b, s.keyRing)
	if err != nil {
		return fmt.Errorf("unable to pack multi backup: %v", err)
	}

	// Finally, we'll swap out the old backup for this new one in a single
	// atomic step, combining the file already on-disk with this set of new
	// channels.
	err = s.Swapper.UpdateAndSwap(PackedMulti(b.Bytes()))
	if err != nil {
		return fmt.Errorf("unable to update multi backup: %v", err)
	}

	return nil
}

// backupFileUpdater is the primary goroutine of the SubSwapper which is
// responsible for listening for changes to the channel, and updating the
// persistent multi backup state with a new packed multi of the latest channel
// state.
func (s *SubSwapper) backupUpdater() {
	// Ensure that once we exit, we'll cancel our active channel
	// subscription.
	defer s.chanEvents.Cancel()
	defer s.wg.Done()

	log.Debugf("SubSwapper's backupUpdater is active!")

	for {
		select {
		// The channel state has been modified! We'll evaluate all
		// changes, and swap out the old packed multi with a new one
		// with the latest channel state.
		case chanUpdate := <-s.chanEvents.ChanUpdates:
			oldStateSize := len(s.backupState)

			// For all new open channels, we'll create a new SCB
			// given the required information.
			for _, newChan := range chanUpdate.NewChans {
				log.Debugf("Adding channel %v to backup state",
					newChan.FundingOutpoint)

				s.backupState[newChan.FundingOutpoint] = NewSingle(
					newChan.OpenChannel, newChan.Addrs,
				)
			}

			// For all closed channels, we'll remove the prior
			// backup state.
			closedChans := make(
				[]wire.OutPoint, 0, len(chanUpdate.ClosedChans),
			)
			for i, closedChan := range chanUpdate.ClosedChans {
				log.Debugf("Removing channel %v from backup "+
					"state", newLogClosure(func() string {
					return chanUpdate.ClosedChans[i].String()
				}))

				delete(s.backupState, closedChan)

				closedChans = append(closedChans, closedChan)
			}

			newStateSize := len(s.backupState)

			log.Infof("Updating on-disk multi SCB backup: "+
				"num_old_chans=%v, num_new_chans=%v",
				oldStateSize, newStateSize)

			// With out new state constructed, we'll, atomically
			// update the on-disk backup state.
			if err := s.updateBackupFile(closedChans...); err != nil {
				log.Errorf("unable to update backup file: %v",
					err)
			}

		// TODO(roasbeef): refresh periodically on a time basis due to
		// possible addr changes from node

		// Exit at once if a quit signal is detected.
		case <-s.quit:
			return
		}
	}
}
