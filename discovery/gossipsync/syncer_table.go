package gossipsync

import "github.com/lightningnetwork/lnd/protofsm"

type (
	// syncerTable is the transition table type of the peer syncer.
	syncerTable = protofsm.TransitionTable[
		SyncerState, SyncerEvent, SyncerOutbox,
	]

	// syncerFrom is the per-state entry type of the syncer table.
	syncerFrom = protofsm.StateTransitions[
		SyncerState, SyncerEvent, SyncerOutbox,
	]

	// syncerEntry is a single transition of the syncer table.
	syncerEntry = protofsm.TransitionEntry[
		SyncerState, SyncerEvent, SyncerOutbox,
	]
)

// outbox is shorthand for a list of outbox types in a table entry.
func outbox(o ...SyncerOutbox) []SyncerOutbox {
	return o
}

// setSyncTypeEntries lists the two ways every state handles SetSyncType: with
// no message when the role change doesn't change what we ask the peer for,
// and with a new timestamp filter when it does.
func setSyncTypeEntries(s SyncerState) []syncerEntry {
	return []syncerEntry{{
		Event:       &SetSyncType{},
		ToState:     s,
		Description: "Role unchanged, or active to pinned.",
	}, {
		Event:       &SetSyncType{},
		ToState:     s,
		Description: "Role now does or doesn't want gossip.",
		EmitsOutbox: outbox(&SendToPeer{}),
	}}
}

// SyncerTransitions is the transition table of the peer syncer. It documents
// the machine, and the property tests fail on any transition it doesn't list.
var SyncerTransitions = syncerTable{
	MachineName: "PeerSyncer",
	States: []syncerFrom{{
		FromState: &Idle{},
		Transitions: append([]syncerEntry{{
			Event:   &StartHistoricalSync{},
			ToState: &AwaitingRange{},
			Description: "Send query_channel_range for the " +
				"whole chain.",
			EmitsOutbox: outbox(&SendToPeer{}, &ArmReplyTimer{}),
		}, {
			Event:       &RangeReplyReceived{},
			ToState:     &Idle{},
			Description: "Unsolicited reply, ignored.",
		}, {
			Event:       &SCIDsEndReceived{},
			ToState:     &Idle{},
			Description: "Unsolicited reply, ignored.",
		}, {
			Event:       &ReplyTimerFired{},
			ToState:     &Idle{},
			Description: "Timer of an ended exchange, ignored.",
		}}, setSyncTypeEntries(&Idle{})...),
	}, {
		FromState: &AwaitingRange{},
		Transitions: append([]syncerEntry{{
			Event:       &RangeReplyReceived{},
			ToState:     &AwaitingRange{},
			Description: "Valid partial reply buffered.",
			EmitsOutbox: outbox(&ArmReplyTimer{}),
		}, {
			Event:   &RangeReplyReceived{},
			ToState: &QueryingSCIDs{},
			Description: "Stream complete with missing channels, " +
				"send the first SCID batch.",
			EmitsOutbox: outbox(&SendToPeer{}, &ArmReplyTimer{}),
		}, {
			Event:   &RangeReplyReceived{},
			ToState: &Idle{},
			Description: "Stream complete with nothing missing, " +
				"or the local lookup failed.",
			EmitsOutbox: outbox(
				&DisarmReplyTimer{}, &ReportOutcome{},
			),
		}, {
			Event:   &RangeReplyReceived{},
			ToState: &Draining{},
			Description: "Invalid or oversized reply, the " +
				"attempt fails.",
			EmitsOutbox: outbox(&ReportOutcome{}, &ArmReplyTimer{}),
		}, {
			Event:   &ReplyTimerFired{},
			ToState: &Draining{},
			Description: "Current timer: the peer stopped " +
				"answering, the attempt fails.",
			EmitsOutbox: outbox(&ReportOutcome{}, &ArmReplyTimer{}),
		}, {
			Event:       &ReplyTimerFired{},
			ToState:     &AwaitingRange{},
			Description: "Stale timer, ignored.",
		}, {
			Event:   &StartHistoricalSync{},
			ToState: &AwaitingRange{},
			Description: "Already busy, the new attempt is " +
				"refused.",
			EmitsOutbox: outbox(&ReportOutcome{}),
		}, {
			Event:       &SCIDsEndReceived{},
			ToState:     &AwaitingRange{},
			Description: "Unsolicited reply, ignored.",
		}}, setSyncTypeEntries(&AwaitingRange{})...),
	}, {
		FromState: &QueryingSCIDs{},
		Transitions: append([]syncerEntry{{
			Event:       &SCIDsEndReceived{},
			ToState:     &QueryingSCIDs{},
			Description: "Batch answered, send the next one.",
			EmitsOutbox: outbox(&SendToPeer{}, &ArmReplyTimer{}),
		}, {
			Event:   &SCIDsEndReceived{},
			ToState: &Idle{},
			Description: "Last batch answered, the attempt " +
				"is done.",
			EmitsOutbox: outbox(
				&DisarmReplyTimer{}, &ReportOutcome{},
			),
		}, {
			Event:   &ReplyTimerFired{},
			ToState: &Draining{},
			Description: "Current timer: the peer stopped " +
				"answering, the attempt fails.",
			EmitsOutbox: outbox(&ReportOutcome{}, &ArmReplyTimer{}),
		}, {
			Event:       &ReplyTimerFired{},
			ToState:     &QueryingSCIDs{},
			Description: "Stale timer, ignored.",
		}, {
			Event:   &StartHistoricalSync{},
			ToState: &QueryingSCIDs{},
			Description: "Already busy, the new attempt is " +
				"refused.",
			EmitsOutbox: outbox(&ReportOutcome{}),
		}, {
			Event:       &RangeReplyReceived{},
			ToState:     &QueryingSCIDs{},
			Description: "Unsolicited reply, ignored.",
		}}, setSyncTypeEntries(&QueryingSCIDs{})...),
	}, {
		FromState: &Draining{},
		Transitions: append([]syncerEntry{{
			Event:   &RangeReplyReceived{},
			ToState: &Draining{},
			Description: "Reply of the abandoned stream " +
				"absorbed, the drain timer re-armed.",
			EmitsOutbox: outbox(&ArmReplyTimer{}),
		}, {
			Event:       &RangeReplyReceived{},
			ToState:     &Draining{},
			Description: "Unsolicited reply, ignored.",
		}, {
			Event:   &RangeReplyReceived{},
			ToState: &Idle{},
			Description: "Abandoned stream ended, or the absorb " +
				"budget is spent.",
			EmitsOutbox: outbox(&DisarmReplyTimer{}),
		}, {
			Event:   &SCIDsEndReceived{},
			ToState: &Idle{},
			Description: "Abandoned SCID query answered at " +
				"last.",
			EmitsOutbox: outbox(&DisarmReplyTimer{}),
		}, {
			Event:       &SCIDsEndReceived{},
			ToState:     &Draining{},
			Description: "Unsolicited reply, ignored.",
		}, {
			Event:       &ReplyTimerFired{},
			ToState:     &Idle{},
			Description: "Drain timer fired, give up waiting.",
			EmitsOutbox: outbox(&DisarmReplyTimer{}),
		}, {
			Event:       &ReplyTimerFired{},
			ToState:     &Draining{},
			Description: "Stale timer, ignored.",
		}, {
			Event:   &StartHistoricalSync{},
			ToState: &Draining{},
			Description: "Still draining, the new attempt is " +
				"refused.",
			EmitsOutbox: outbox(&ReportOutcome{}),
		}}, setSyncTypeEntries(&Draining{})...),
	}},
}
