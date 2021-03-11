package main

import "github.com/lightningnetwork/lnd/lnrpc/walletrpc"

// PendingSweep is a CLI-friendly type of the walletrpc.PendingSweep proto. We
// use this to show more useful string versions of byte slices and enums.
type PendingSweep struct {
	OutPoint             OutPoint `json:"outpoint"`
	WitnessType          string   `json:"witness_type"`
	AmountSat            uint32   `json:"amount_sat"`
	SatPerVByte          uint32   `json:"sat_per_vbyte"`
	BroadcastAttempts    uint32   `json:"broadcast_attempts"`
	NextBroadcastHeight  uint32   `json:"next_broadcast_height"`
	RequestedSatPerVByte uint32   `json:"requested_sat_per_vbyte"`
	RequestedConfTarget  uint32   `json:"requested_conf_target"`
	Force                bool     `json:"force"`
}

// NewPendingSweepFromProto converts the walletrpc.PendingSweep proto type into
// its corresponding CLI-friendly type.
func NewPendingSweepFromProto(pendingSweep *walletrpc.PendingSweep) *PendingSweep {
	return &PendingSweep{
		OutPoint:             NewOutPointFromProto(pendingSweep.Outpoint),
		WitnessType:          pendingSweep.WitnessType.String(),
		AmountSat:            pendingSweep.AmountSat,
		SatPerVByte:          uint32(pendingSweep.SatPerVbyte),
		BroadcastAttempts:    pendingSweep.BroadcastAttempts,
		NextBroadcastHeight:  pendingSweep.NextBroadcastHeight,
		RequestedSatPerVByte: uint32(pendingSweep.RequestedSatPerVbyte),
		RequestedConfTarget:  pendingSweep.RequestedConfTarget,
		Force:                pendingSweep.Force,
	}
}
