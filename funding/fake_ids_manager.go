package funding

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// FakeIDsManager is an interface for managing fake ids.
// The implementation keeps a set of fake ids that are currently in use and
// provide interface for using/releasing such ids.
type FakeIDsManager interface {
	// Use marked the given short channel id as used.
	Use(lnwire.ShortChannelID) error

	// Release released the given short channel id, prune it from the graph and
	// removes it from the underline storage.
	Release(lnwire.ShortChannelID) error
}

// persistentFakeIDsManager is an implementation of FakeIDsManager that uses
// a persistent storage for keeping the set of used ids.
type persistentFakeIDsManager struct {
	db *channeldb.DB
}

// newPersistentFakeIDsManager creates a new PersistentFakeIDsManager instance.
// In addition this method also loads all fake channel ids from the channels
// db and prunes all those ids that were marked as used and do not exist
// any more.
func newPersistentFakeIDsManager(db *channeldb.DB) (*persistentFakeIDsManager, error) {

	channels, err := db.FetchAllChannels()
	if err != nil {
		return nil, err
	}

	graph := db.ChannelGraph()

	// We would like to prune from the fake ids bucket all those that are not
	// in use anymore. We start by create a mapping between ids to channels.
	fakeIDsToChannels := make(map[uint64]*channeldb.OpenChannel, len(channels))
	for _, c := range channels {
		shortID := c.ShortChannelID
		if shortID.IsFake() {
			fakeIDsToChannels[shortID.ToUint64()] = c
		}
	}

	// We mark each fake id in our bucket that is not associated with
	// a pending or opened channel for prune.
	var idsToPrune [][]byte
	err = kvdb.Update(db, func(tx kvdb.RwTx) error {
		fakeIDsBucket, err := tx.CreateTopLevelBucket(fakeIDsBucket)
		if err != nil {
			return err
		}
		return fakeIDsBucket.ForEach(func(k, v []byte) error {
			shortID := byteOrder.Uint64(k)
			_, exists := fakeIDsToChannels[shortID]
			if !exists {
				log.Infof("prunning fake id from graph %v", shortID)
				idsToPrune = append(idsToPrune, k)
			}
			return nil
		})
	}, func() {})
	if err != nil {
		return nil, err
	}

	// We prune first from the graph.
	for _, id := range idsToPrune {
		shortID := byteOrder.Uint64(id)
		if err = graph.DeleteChannelEdges(false, shortID); err != nil {
			log.Debugf("failed to delete fake edge, probably "+
				"already deleted %v: %v", shortID, err)
		}
	}

	// Now we can safely remove from the fake id store.
	err = kvdb.Update(db, func(tx kvdb.RwTx) error {
		fakeIDsBucket := tx.ReadWriteBucket(fakeIDsBucket)
		for _, id := range idsToPrune {
			if err := fakeIDsBucket.Delete(id); err != nil {
				return err
			}
		}
		return nil
	}, func() {})
	if err != nil {
		return nil, err
	}

	return &persistentFakeIDsManager{db: db}, nil
}

func (f *persistentFakeIDsManager) Use(
	fakeID lnwire.ShortChannelID) error {

	err := kvdb.Update(f.db, func(tx kvdb.RwTx) error {
		fakeIDsBucket := tx.ReadWriteBucket(fakeIDsBucket)
		var id = make([]byte, 8)
		byteOrder.PutUint64(id[:8], fakeID.ToUint64())
		return fakeIDsBucket.Put(id, []byte{})
	}, func() {})
	return err
}

func (f *persistentFakeIDsManager) Release(
	fakeID lnwire.ShortChannelID) error {

	log.Infof("releasing fake short channel id %v", fakeID)
	err := kvdb.Update(f.db, func(tx kvdb.RwTx) error {
		fakeIDsBucket := tx.ReadWriteBucket(fakeIDsBucket)
		var id = make([]byte, 8)
		byteOrder.PutUint64(id[:8], fakeID.ToUint64())
		return fakeIDsBucket.Delete(id)
	}, func() {})
	return err
}
