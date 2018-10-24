// +build dev

package wtdb

import "sync"

type MockDB struct {
	mu       sync.Mutex
	sessions map[SessionID]*SessionInfo
}

func NewMockDB() *MockDB {
	return &MockDB{
		sessions: make(map[SessionID]*SessionInfo),
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
