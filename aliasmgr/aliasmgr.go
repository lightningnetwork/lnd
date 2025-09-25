package aliasmgr

import (
	"encoding/binary"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// UpdateLinkAliases is a function type for a function that locates the active
// link that matches the given shortID and triggers an update based on the
// latest values of the alias manager.
type UpdateLinkAliases func(shortID lnwire.ShortChannelID) error

// ScidAliasMap is a map from a base short channel ID to a set of alias short
// channel IDs.
type ScidAliasMap map[lnwire.ShortChannelID][]lnwire.ShortChannelID

var (
	// aliasBucket stores aliases as keys and their base SCIDs as values.
	// This is used to populate the maps that the Manager uses. The keys
	// are alias SCIDs and the values are their respective base SCIDs. This
	// is used instead of the other way around (base -> alias...) because
	// updating an alias would require fetching all the existing aliases,
	// adding another one, and then flushing the write to disk. This is
	// inefficient compared to N 1:1 mappings at the cost of marginally
	// more disk space.
	aliasBucket = []byte("alias-bucket")

	// confirmedBucket stores whether or not a given base SCID should no
	// longer have entries in the ToBase maps. The key is the SCID that is
	// confirmed with 6 confirmations and is public, and the value is
	// empty.
	confirmedBucket = []byte("base-bucket")

	// aliasAllocBucket is a root-level bucket that stores the last alias
	// that was allocated. It is used to allocate a new alias when
	// requested.
	aliasAllocBucket = []byte("alias-alloc-bucket")

	// lastAliasKey is a key in the aliasAllocBucket whose value is the
	// last allocated alias ShortChannelID. This will be updated upon calls
	// to RequestAlias.
	lastAliasKey = []byte("last-alias-key")

	// invoiceAliasBucket is a root-level bucket that stores the alias
	// SCIDs that our peers send us in the channel_ready TLV. The keys are
	// the ChannelID generated from the FundingOutpoint and the values are
	// the remote peer's alias SCID.
	invoiceAliasBucket = []byte("invoice-alias-bucket")

	// byteOrder denotes the byte order of database (de)-serialization
	// operations.
	byteOrder = binary.BigEndian

	// AliasStartBlockHeight is the starting block height of the alias
	// range.
	AliasStartBlockHeight uint32 = 16_000_000

	// AliasEndBlockHeight is the ending block height of the alias range.
	AliasEndBlockHeight uint32 = 16_250_000

	// StartingAlias is the first alias ShortChannelID that will get
	// assigned by RequestAlias. The starting BlockHeight is chosen so that
	// legitimate SCIDs in integration tests aren't mistaken for an alias.
	StartingAlias = lnwire.ShortChannelID{
		BlockHeight: AliasStartBlockHeight,
		TxIndex:     0,
		TxPosition:  0,
	}

	// errNoBase is returned when a base SCID isn't found.
	errNoBase = fmt.Errorf("no base found")

	// errNoPeerAlias is returned when the peer's alias for a given
	// channel is not found.
	errNoPeerAlias = fmt.Errorf("no peer alias found")

	// ErrAliasNotFound is returned when the alias is not found and can't
	// be mapped to a base SCID.
	ErrAliasNotFound = fmt.Errorf("alias not found")
)

// Manager is a struct that handles aliases for LND. It has an underlying
// database that can allocate aliases for channels, stores the peer's last
// alias for use in our hop hints, and contains mappings that both the Switch
// and Gossiper use.
type Manager struct {
	backend kvdb.Backend

	// linkAliasUpdater is a function used by the alias manager to
	// facilitate live update of aliases in other subsystems.
	linkAliasUpdater UpdateLinkAliases

	// baseToSet is a mapping from the "base" SCID to the set of aliases
	// for this channel. This mapping includes all channels that
	// negotiated the option-scid-alias feature bit.
	baseToSet ScidAliasMap

	// aliasToBase is a mapping that maps all aliases for a given channel
	// to its base SCID. This is only used for channels that have
	// negotiated option-scid-alias feature bit.
	aliasToBase map[lnwire.ShortChannelID]lnwire.ShortChannelID

	// peerAlias is a cache for the alias SCIDs that our peers send us in
	// the channel_ready TLV. The keys are the ChannelID generated from
	// the FundingOutpoint and the values are the remote peer's alias SCID.
	// The values should match the ones stored in the "invoice-alias-bucket"
	// bucket.
	peerAlias map[lnwire.ChannelID]lnwire.ShortChannelID

	sync.RWMutex
}

// NewManager initializes an alias Manager from the passed database backend.
func NewManager(db kvdb.Backend, linkAliasUpdater UpdateLinkAliases) (*Manager,
	error) {

	m := &Manager{
		backend:          db,
		baseToSet:        make(ScidAliasMap),
		linkAliasUpdater: linkAliasUpdater,
	}

	m.aliasToBase = make(map[lnwire.ShortChannelID]lnwire.ShortChannelID)
	m.peerAlias = make(map[lnwire.ChannelID]lnwire.ShortChannelID)

	err := m.populateMaps()
	return m, err
}

// populateMaps reads the database state and populates the maps.
func (m *Manager) populateMaps() error {
	// This map tracks the base SCIDs that are confirmed and don't need to
	// have entries in the *ToBase mappings as they won't be used in the
	// gossiper.
	baseConfMap := make(map[lnwire.ShortChannelID]struct{})

	// This map caches what is found in the database and is used to
	// populate the Manager's actual maps.
	aliasMap := make(map[lnwire.ShortChannelID]lnwire.ShortChannelID)

	// This map caches the ChannelID/alias SCIDs stored in the database and
	// is used to populate the Manager's cache.
	peerAliasMap := make(map[lnwire.ChannelID]lnwire.ShortChannelID)

	err := kvdb.Update(m.backend, func(tx kvdb.RwTx) error {
		baseConfBucket, err := tx.CreateTopLevelBucket(confirmedBucket)
		if err != nil {
			return err
		}

		err = baseConfBucket.ForEach(func(k, v []byte) error {
			// The key will the base SCID and the value will be
			// empty. Existence in the bucket means the SCID is
			// confirmed.
			baseScid := lnwire.NewShortChanIDFromInt(
				byteOrder.Uint64(k),
			)
			baseConfMap[baseScid] = struct{}{}
			return nil
		})
		if err != nil {
			return err
		}

		aliasToBaseBucket, err := tx.CreateTopLevelBucket(aliasBucket)
		if err != nil {
			return err
		}

		err = aliasToBaseBucket.ForEach(func(k, v []byte) error {
			// The key will be the alias SCID and the value will be
			// the base SCID.
			aliasScid := lnwire.NewShortChanIDFromInt(
				byteOrder.Uint64(k),
			)
			baseScid := lnwire.NewShortChanIDFromInt(
				byteOrder.Uint64(v),
			)
			aliasMap[aliasScid] = baseScid
			return nil
		})
		if err != nil {
			return err
		}

		invAliasBucket, err := tx.CreateTopLevelBucket(
			invoiceAliasBucket,
		)
		if err != nil {
			return err
		}

		err = invAliasBucket.ForEach(func(k, v []byte) error {
			var chanID lnwire.ChannelID
			copy(chanID[:], k)
			alias := lnwire.NewShortChanIDFromInt(
				byteOrder.Uint64(v),
			)

			peerAliasMap[chanID] = alias

			return nil
		})

		return err
	}, func() {
		baseConfMap = make(map[lnwire.ShortChannelID]struct{})
		aliasMap = make(map[lnwire.ShortChannelID]lnwire.ShortChannelID)
		peerAliasMap = make(map[lnwire.ChannelID]lnwire.ShortChannelID)
	})
	if err != nil {
		return err
	}

	// Populate the baseToSet map regardless if the baseSCID is marked as
	// public with 6 confirmations.
	for aliasSCID, baseSCID := range aliasMap {
		m.baseToSet[baseSCID] = append(m.baseToSet[baseSCID], aliasSCID)

		// Skip if baseSCID is in the baseConfMap.
		if _, ok := baseConfMap[baseSCID]; ok {
			continue
		}

		m.aliasToBase[aliasSCID] = baseSCID
	}

	// Populate the peer alias cache.
	m.peerAlias = peerAliasMap

	return nil
}

// addAliasCfg is a struct that hosts various options related to adding a local
// alias to the alias manager.
type addAliasCfg struct {
	// baseLookup signals that the alias should also store a reverse look-up
	// to the base scid.
	baseLookup bool
}

// AddLocalAliasOption is a functional option that modifies the configuration
// for adding a local alias.
type AddLocalAliasOption func(cfg *addAliasCfg)

// WithBaseLookup is a functional option that controls whether a reverse lookup
// will be stored from the alias to the base scid.
func WithBaseLookup() AddLocalAliasOption {
	return func(cfg *addAliasCfg) {
		cfg.baseLookup = true
	}
}

// AddLocalAlias adds a database mapping from the passed alias to the passed
// base SCID. The gossip boolean marks whether or not to create a mapping
// that the gossiper will use. It is set to false for the upgrade path where
// the feature-bit is toggled on and there are existing channels. The linkUpdate
// flag is used to signal whether this function should also trigger an update on
// the htlcswitch scid alias maps.
//
// NOTE: The following aliases will not be persisted (will be lost on restart):
//   - Aliases that were created without gossip flag.
//   - Aliases that correspond to confirmed channels.
func (m *Manager) AddLocalAlias(alias, baseScid lnwire.ShortChannelID,
	gossip, linkUpdate bool, opts ...AddLocalAliasOption) error {

	cfg := addAliasCfg{}
	for _, opt := range opts {
		opt(&cfg)
	}

	// We need to lock the manager for the whole duration of this method,
	// except for the very last part where we call the link updater. In
	// order for us to safely use a defer _and_ still be able to manually
	// unlock, we use a sync.Once.
	m.Lock()
	unlockOnce := sync.Once{}
	unlock := func() {
		unlockOnce.Do(m.Unlock)
	}
	defer unlock()

	err := kvdb.Update(m.backend, func(tx kvdb.RwTx) error {
		// If the caller does not want to allow the alias to be used
		// for a channel update, we'll mark it in the baseConfBucket.
		if !gossip {
			var baseGossipBytes [8]byte
			byteOrder.PutUint64(
				baseGossipBytes[:], baseScid.ToUint64(),
			)

			confBucket, err := tx.CreateTopLevelBucket(
				confirmedBucket,
			)
			if err != nil {
				return err
			}

			err = confBucket.Put(baseGossipBytes[:], []byte{})
			if err != nil {
				return err
			}
		}

		aliasToBaseBucket, err := tx.CreateTopLevelBucket(aliasBucket)
		if err != nil {
			return err
		}

		var (
			aliasBytes [8]byte
			baseBytes  [8]byte
		)

		byteOrder.PutUint64(aliasBytes[:], alias.ToUint64())
		byteOrder.PutUint64(baseBytes[:], baseScid.ToUint64())
		return aliasToBaseBucket.Put(aliasBytes[:], baseBytes[:])
	}, func() {})
	if err != nil {
		return err
	}

	// Update the aliasToBase and baseToSet maps.
	m.baseToSet[baseScid] = append(m.baseToSet[baseScid], alias)

	// Only store the gossiper map if gossip is true, or if the caller
	// explicitly asked to store this reverse mapping.
	if gossip || cfg.baseLookup {
		m.aliasToBase[alias] = baseScid
	}

	// We definitely need to unlock the Manager before calling the link
	// updater. If we don't, we'll deadlock. We use a sync.Once to ensure
	// that we only unlock once.
	unlock()

	// Finally, we trigger a htlcswitch update if the flag is set, in order
	// for any future htlc that references the added alias to be properly
	// routed.
	if linkUpdate {
		return m.linkAliasUpdater(baseScid)
	}

	return nil
}

// GetAliases fetches the set of aliases stored under a given base SCID from
// write-through caches.
func (m *Manager) GetAliases(
	base lnwire.ShortChannelID) []lnwire.ShortChannelID {

	m.RLock()
	defer m.RUnlock()

	aliasSet, ok := m.baseToSet[base]
	if ok {
		// Copy the found alias slice.
		setCopy := make([]lnwire.ShortChannelID, len(aliasSet))
		copy(setCopy, aliasSet)
		return setCopy
	}

	return nil
}

// FindBaseSCID finds the base SCID for a given alias. This is used in the
// gossiper to find the correct SCID to lookup in the graph database. It can
// also be used to look up the base for manual aliases that were added over the
// RPC.
func (m *Manager) FindBaseSCID(
	alias lnwire.ShortChannelID) (lnwire.ShortChannelID, error) {

	m.RLock()
	defer m.RUnlock()

	base, ok := m.aliasToBase[alias]
	if ok {
		return base, nil
	}

	return lnwire.ShortChannelID{}, errNoBase
}

// DeleteSixConfs removes a mapping for the gossiper once six confirmations
// have been reached and the channel is public. At this point, only the
// confirmed SCID should be used.
func (m *Manager) DeleteSixConfs(baseScid lnwire.ShortChannelID) error {
	m.Lock()
	defer m.Unlock()

	err := kvdb.Update(m.backend, func(tx kvdb.RwTx) error {
		baseConfBucket, err := tx.CreateTopLevelBucket(confirmedBucket)
		if err != nil {
			return err
		}

		var baseBytes [8]byte
		byteOrder.PutUint64(baseBytes[:], baseScid.ToUint64())
		return baseConfBucket.Put(baseBytes[:], []byte{})
	}, func() {})
	if err != nil {
		return err
	}

	// Now that the database state has been updated, we'll delete all of
	// the aliasToBase mappings for this SCID.
	for alias, base := range m.aliasToBase {
		if base.ToUint64() == baseScid.ToUint64() {
			delete(m.aliasToBase, alias)
		}
	}

	return nil
}

// DeleteLocalAlias removes a mapping from the database and the Manager's maps.
func (m *Manager) DeleteLocalAlias(alias,
	baseScid lnwire.ShortChannelID) error {

	// We need to lock the manager for the whole duration of this method,
	// except for the very last part where we call the link updater. In
	// order for us to safely use a defer _and_ still be able to manually
	// unlock, we use a sync.Once.
	m.Lock()
	unlockOnce := sync.Once{}
	unlock := func() {
		unlockOnce.Do(m.Unlock)
	}
	defer unlock()

	err := kvdb.Update(m.backend, func(tx kvdb.RwTx) error {
		aliasToBaseBucket, err := tx.CreateTopLevelBucket(aliasBucket)
		if err != nil {
			return err
		}

		var aliasBytes [8]byte
		byteOrder.PutUint64(aliasBytes[:], alias.ToUint64())

		// If the user attempts to delete an alias that doesn't exist,
		// we'll want to inform them about it and not just do nothing.
		if aliasToBaseBucket.Get(aliasBytes[:]) == nil {
			return ErrAliasNotFound
		}

		return aliasToBaseBucket.Delete(aliasBytes[:])
	}, func() {})
	if err != nil {
		return err
	}

	// Now that the database state has been updated, we'll delete the
	// mapping from the Manager's maps.
	aliasSet, ok := m.baseToSet[baseScid]
	if !ok {
		return ErrAliasNotFound
	}

	// We'll filter the alias set and remove the alias from it.
	aliasSet = fn.Filter(aliasSet, func(a lnwire.ShortChannelID) bool {
		return a.ToUint64() != alias.ToUint64()
	})

	// If the alias set is empty, we'll delete the base SCID from the
	// baseToSet map.
	if len(aliasSet) == 0 {
		delete(m.baseToSet, baseScid)
	} else {
		m.baseToSet[baseScid] = aliasSet
	}

	// Finally, we'll delete the aliasToBase mapping from the Manager's
	// cache.
	delete(m.aliasToBase, alias)

	// We definitely need to unlock the Manager before calling the link
	// updater. If we don't, we'll deadlock. We use a sync.Once to ensure
	// that we only unlock once.
	unlock()

	return m.linkAliasUpdater(baseScid)
}

// PutPeerAlias stores the peer's alias SCID once we learn of it in the
// channel_ready message.
func (m *Manager) PutPeerAlias(chanID lnwire.ChannelID,
	alias lnwire.ShortChannelID) error {

	m.Lock()
	defer m.Unlock()

	err := kvdb.Update(m.backend, func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(invoiceAliasBucket)
		if err != nil {
			return err
		}

		var scratch [8]byte
		byteOrder.PutUint64(scratch[:], alias.ToUint64())
		return bucket.Put(chanID[:], scratch[:])
	}, func() {})
	if err != nil {
		return err
	}

	// Now that the database state has been updated, we can update it in
	// our cache.
	m.peerAlias[chanID] = alias

	return nil
}

// GetPeerAlias retrieves a peer's alias SCID by the channel's ChanID.
func (m *Manager) GetPeerAlias(chanID lnwire.ChannelID) (lnwire.ShortChannelID,
	error) {

	m.RLock()
	defer m.RUnlock()

	alias, ok := m.peerAlias[chanID]
	if !ok || alias == hop.Source {
		return lnwire.ShortChannelID{}, errNoPeerAlias
	}

	return alias, nil
}

// RequestAlias returns a new ALIAS ShortChannelID to the caller by allocating
// the next un-allocated ShortChannelID. The starting ShortChannelID is
// 16000000:0:0 and the ending ShortChannelID is 16250000:16777215:65535. This
// gives roughly 2^58 possible ALIAS ShortChannelIDs which ensures this space
// won't get exhausted.
func (m *Manager) RequestAlias() (lnwire.ShortChannelID, error) {
	var nextAlias lnwire.ShortChannelID

	m.RLock()
	defer m.RUnlock()

	// haveAlias returns true if the passed alias is already assigned to a
	// channel in the baseToSet map.
	haveAlias := func(maybeNextAlias lnwire.ShortChannelID) bool {
		return fn.Any(
			slices.Collect(maps.Values(m.baseToSet)),
			func(aliasList []lnwire.ShortChannelID) bool {
				return fn.Any(
					aliasList,
					func(alias lnwire.ShortChannelID) bool {
						return alias == maybeNextAlias
					},
				)
			},
		)
	}

	err := kvdb.Update(m.backend, func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(aliasAllocBucket)
		if err != nil {
			return err
		}

		lastBytes := bucket.Get(lastAliasKey)
		if lastBytes == nil {
			// If the key does not exist, then we can write the
			// StartingAlias to it.
			nextAlias = StartingAlias

			// If the very first alias is already assigned, we'll
			// keep incrementing until we find an unassigned alias.
			// This is to avoid collision with custom added SCID
			// aliases that fall into the same range as the ones we
			// generate here monotonically. Those custom SCIDs are
			// stored in a different bucket, but we can just check
			// the in-memory map for simplicity.
			for {
				if !haveAlias(nextAlias) {
					break
				}

				nextAlias = getNextScid(nextAlias)

				// Abort if we've reached the end of the range.
				if nextAlias.BlockHeight >=
					AliasEndBlockHeight {

					return fmt.Errorf("range for custom " +
						"aliases exhausted")
				}
			}

			var scratch [8]byte
			byteOrder.PutUint64(scratch[:], nextAlias.ToUint64())
			return bucket.Put(lastAliasKey, scratch[:])
		}

		// Otherwise the key does exist so we can convert the retrieved
		// lastAlias to a ShortChannelID and use it to assign the next
		// ShortChannelID. This next ShortChannelID will then be
		// persisted in the database.
		lastScid := lnwire.NewShortChanIDFromInt(
			byteOrder.Uint64(lastBytes),
		)
		nextAlias = getNextScid(lastScid)

		// If the next alias is already assigned, we'll keep
		// incrementing until we find an unassigned alias. This is to
		// avoid collision with custom added SCID aliases that fall into
		// the same range as the ones we generate here monotonically.
		// Those custom SCIDs are stored in a different bucket, but we
		// can just check the in-memory map for simplicity.
		for {
			if !haveAlias(nextAlias) {
				break
			}

			nextAlias = getNextScid(nextAlias)

			// Abort if we've reached the end of the range.
			if nextAlias.BlockHeight >= AliasEndBlockHeight {
				return fmt.Errorf("range for custom " +
					"aliases exhausted")
			}
		}

		var scratch [8]byte
		byteOrder.PutUint64(scratch[:], nextAlias.ToUint64())
		return bucket.Put(lastAliasKey, scratch[:])
	}, func() {
		nextAlias = lnwire.ShortChannelID{}
	})
	if err != nil {
		return nextAlias, err
	}

	return nextAlias, nil
}

// ListAliases returns a carbon copy of baseToSet. This is used by the rpc
// layer.
func (m *Manager) ListAliases() ScidAliasMap {
	m.RLock()
	defer m.RUnlock()

	baseCopy := make(ScidAliasMap)

	for k, v := range m.baseToSet {
		setCopy := make([]lnwire.ShortChannelID, len(v))
		copy(setCopy, v)
		baseCopy[k] = setCopy
	}

	return baseCopy
}

// getNextScid is a utility function that returns the next SCID for a given
// alias SCID. The BlockHeight ranges from [16000000, 16250000], the TxIndex
// ranges from [1, 16777215], and the TxPosition ranges from [1, 65535].
func getNextScid(last lnwire.ShortChannelID) lnwire.ShortChannelID {
	var (
		next            lnwire.ShortChannelID
		incrementIdx    bool
		incrementHeight bool
	)

	// If the TxPosition is 65535, then it goes to 0 and we need to
	// increment the TxIndex.
	if last.TxPosition == 65535 {
		incrementIdx = true
	}

	// If the TxIndex is 16777215 and we need to increment it, then it goes
	// to 0 and we need to increment the BlockHeight.
	if last.TxIndex == 16777215 && incrementIdx {
		incrementIdx = false
		incrementHeight = true
	}

	switch {
	// If we increment the TxIndex, then TxPosition goes to 0.
	case incrementIdx:
		next.BlockHeight = last.BlockHeight
		next.TxIndex = last.TxIndex + 1
		next.TxPosition = 0

	// If we increment the BlockHeight, then the Tx fields go to 0.
	case incrementHeight:
		next.BlockHeight = last.BlockHeight + 1
		next.TxIndex = 0
		next.TxPosition = 0

	// Otherwise, we only need to increment the TxPosition.
	default:
		next.BlockHeight = last.BlockHeight
		next.TxIndex = last.TxIndex
		next.TxPosition = last.TxPosition + 1
	}

	return next
}

// IsAlias returns true if the passed SCID is an alias. The function determines
// this by looking at the BlockHeight. If the BlockHeight is greater than
// AliasStartBlockHeight and less than AliasEndBlockHeight, then it is an alias
// assigned by RequestAlias. These bounds only apply to aliases we generate.
// Our peers are free to use any range they choose.
func IsAlias(scid lnwire.ShortChannelID) bool {
	return scid.BlockHeight >= AliasStartBlockHeight &&
		scid.BlockHeight < AliasEndBlockHeight
}
