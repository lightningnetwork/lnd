package main

import (
	"encoding/hex"

	"github.com/lightningnetwork/lnd/lnrpc/wtclientrpc"
)

// TowerSession encompasses information about a tower session.
type TowerSession struct {
	NumBackups        uint32 `json:"num_backups"`
	NumPendingBackups uint32 `json:"num_pending_backups"`
	MaxBackups        uint32 `json:"max_backups"`
	SweepSatPerByte   uint32 `json:"sweep_sat_per_byte"`
}

// NewTowerSessionsFromProto converts a set of tower sessions from their RPC
// type to a CLI-friendly type.
func NewTowerSessionsFromProto(sessions []*wtclientrpc.TowerSession) []*TowerSession {
	towerSessions := make([]*TowerSession, 0, len(sessions))
	for _, session := range sessions {
		towerSessions = append(towerSessions, &TowerSession{
			NumBackups:        session.NumBackups,
			NumPendingBackups: session.NumPendingBackups,
			MaxBackups:        session.MaxBackups,
			SweepSatPerByte:   session.SweepSatPerByte,
		})
	}
	return towerSessions
}

// Tower encompasses information about a registered watchtower.
type Tower struct {
	PubKey                 string          `json:"pubkey"`
	Addresses              []string        `json:"addresses"`
	ActiveSessionCandidate bool            `json:"active_session_candidate"`
	NumSessions            uint32          `json:"num_sessions"`
	Sessions               []*TowerSession `json:"sessions"`
}

// NewTowerFromProto converts a tower from its RPC type to a CLI-friendly type.
func NewTowerFromProto(tower *wtclientrpc.Tower) *Tower {
	return &Tower{
		PubKey:                 hex.EncodeToString(tower.Pubkey),
		Addresses:              tower.Addresses,
		ActiveSessionCandidate: tower.ActiveSessionCandidate,
		NumSessions:            tower.NumSessions,
		Sessions:               NewTowerSessionsFromProto(tower.Sessions),
	}
}
