// +build dev

package wtdb

import (
	"sync"

	"github.com/lightningnetwork/lnd/chainntnfs"
)

type MockDB struct {
	mu        sync.Mutex
	lastEpoch *chainntnfs.BlockEpoch
	sessions  map[SessionID]*SessionInfo
	blobs     map[BreachHint]map[SessionID]*SessionStateUpdate
}

func NewMockDB() *MockDB {
	return &MockDB{
		sessions: make(map[SessionID]*SessionInfo),
		blobs:    make(map[BreachHint]map[SessionID]*SessionStateUpdate),
	}
}

func (db *MockDB) InsertStateUpdate(update *SessionStateUpdate) (uint16, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	info, ok := db.sessions[update.ID]
	if !ok {
		return 0, ErrSessionNotFound
	}

	err := info.AcceptUpdateSequence(update.SeqNum, update.LastApplied)
	if err != nil {
		return info.LastApplied, err
	}

	sessionsToUpdates, ok := db.blobs[update.Hint]
	if !ok {
		sessionsToUpdates = make(map[SessionID]*SessionStateUpdate)
		db.blobs[update.Hint] = sessionsToUpdates
	}
	sessionsToUpdates[update.ID] = update

	return info.LastApplied, nil
}

func (db *MockDB) GetSessionInfo(id *SessionID) (*SessionInfo, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	if info, ok := db.sessions[*id]; ok {
		return info, nil
	}

	return nil, ErrSessionNotFound
}

func (db *MockDB) InsertSessionInfo(info *SessionInfo) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if _, ok := db.sessions[info.ID]; ok {
		return ErrSessionAlreadyExists
	}

	db.sessions[info.ID] = info

	return nil
}

func (db *MockDB) DeleteSession(target SessionID) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Fail if the session doesn't exit.
	if _, ok := db.sessions[target]; !ok {
		return ErrSessionNotFound
	}

	// Remove the target session.
	delete(db.sessions, target)

	// Remove the state updates for any blobs stored under the target
	// session identifier.
	for hint, sessionUpdates := range db.blobs {
		delete(sessionUpdates, target)

		//If this was the last state update, we can also remove the hint
		//that would map to an empty set.
		if len(sessionUpdates) == 0 {
			delete(db.blobs, hint)
		}
	}

	return nil
}

func (db *MockDB) GetLookoutTip() (*chainntnfs.BlockEpoch, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	return db.lastEpoch, nil
}

func (db *MockDB) QueryMatches(breachHints []BreachHint) ([]Match, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	var matches []Match
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

			match := Match{
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

func (db *MockDB) SetLookoutTip(epoch *chainntnfs.BlockEpoch) error {
	db.lastEpoch = epoch
	return nil
}
