package wtmock

import (
	"sync"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
)

// TowerDB is a mock, in-memory implementation of a watchtower.DB.
type TowerDB struct {
	mu        sync.Mutex
	lastEpoch *chainntnfs.BlockEpoch
	sessions  map[wtdb.SessionID]*wtdb.SessionInfo
	blobs     map[blob.BreachHint]map[wtdb.SessionID]*wtdb.SessionStateUpdate
}

// NewTowerDB initializes a fresh mock TowerDB.
func NewTowerDB() *TowerDB {
	return &TowerDB{
		sessions: make(map[wtdb.SessionID]*wtdb.SessionInfo),
		blobs:    make(map[blob.BreachHint]map[wtdb.SessionID]*wtdb.SessionStateUpdate),
	}
}

// InsertStateUpdate stores an update sent by the client after validating that
// the update is well-formed in the context of other updates sent for the same
// session. This include verifying that the sequence number is incremented
// properly and the last applied values echoed by the client are sane.
func (db *TowerDB) InsertStateUpdate(update *wtdb.SessionStateUpdate) (uint16, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	info, ok := db.sessions[update.ID]
	if !ok {
		return 0, wtdb.ErrSessionNotFound
	}

	commitType, err := info.Policy.BlobType.CommitmentType(nil)
	if err != nil {
		return 0, err
	}

	kit, err := commitType.EmptyJusticeKit()
	if err != nil {
		return 0, err
	}

	// Assert that the blob is the correct size for the session's blob type.
	if len(update.EncryptedBlob) != blob.Size(kit) {
		return 0, wtdb.ErrInvalidBlobSize
	}

	err = info.AcceptUpdateSequence(update.SeqNum, update.LastApplied)
	if err != nil {
		return info.LastApplied, err
	}

	sessionsToUpdates, ok := db.blobs[update.Hint]
	if !ok {
		sessionsToUpdates = make(map[wtdb.SessionID]*wtdb.SessionStateUpdate)
		db.blobs[update.Hint] = sessionsToUpdates
	}
	sessionsToUpdates[update.ID] = update

	return info.LastApplied, nil
}

// GetSessionInfo retrieves the session for the passed session id. An error is
// returned if the session could not be found.
func (db *TowerDB) GetSessionInfo(id *wtdb.SessionID) (*wtdb.SessionInfo, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	if info, ok := db.sessions[*id]; ok {
		return info, nil
	}

	return nil, wtdb.ErrSessionNotFound
}

// InsertSessionInfo records a negotiated session in the tower database. An
// error is returned if the session already exists.
func (db *TowerDB) InsertSessionInfo(info *wtdb.SessionInfo) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	dbInfo, ok := db.sessions[info.ID]
	if ok && dbInfo.LastApplied > 0 {
		return wtdb.ErrSessionAlreadyExists
	}

	// Perform a quick sanity check on the session policy before accepting.
	if err := info.Policy.Validate(); err != nil {
		return err
	}

	db.sessions[info.ID] = info

	return nil
}

// DeleteSession removes all data associated with a particular session id from
// the tower's database.
func (db *TowerDB) DeleteSession(target wtdb.SessionID) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Fail if the session doesn't exit.
	if _, ok := db.sessions[target]; !ok {
		return wtdb.ErrSessionNotFound
	}

	// Remove the target session.
	delete(db.sessions, target)

	// Remove the state updates for any blobs stored under the target
	// session identifier.
	for hint, sessionUpdates := range db.blobs {
		delete(sessionUpdates, target)

		// If this was the last state update, we can also remove the
		// hint that would map to an empty set.
		if len(sessionUpdates) == 0 {
			delete(db.blobs, hint)
		}
	}

	return nil
}

// QueryMatches searches against all known state updates for any that match the
// passed breachHints. More than one Match will be returned for a given hint if
// they exist in the database.
func (db *TowerDB) QueryMatches(
	breachHints []blob.BreachHint) ([]wtdb.Match, error) {

	db.mu.Lock()
	defer db.mu.Unlock()

	var matches []wtdb.Match
	for _, hint := range breachHints {
		sessionsToUpdates, ok := db.blobs[hint]
		if !ok {
			continue
		}

		for id, update := range sessionsToUpdates {
			info, ok := db.sessions[id]
			if !ok {
				panic("session not found")
			}

			match := wtdb.Match{
				ID:            id,
				SeqNum:        update.SeqNum,
				Hint:          hint,
				EncryptedBlob: update.EncryptedBlob,
				SessionInfo:   info,
			}
			matches = append(matches, match)
		}
	}

	return matches, nil
}

// SetLookoutTip stores the provided epoch as the latest lookout tip epoch in
// the tower database.
func (db *TowerDB) SetLookoutTip(epoch *chainntnfs.BlockEpoch) error {
	db.lastEpoch = epoch
	return nil
}

// GetLookoutTip retrieves the current lookout tip block epoch from the tower
// database.
func (db *TowerDB) GetLookoutTip() (*chainntnfs.BlockEpoch, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	return db.lastEpoch, nil
}
