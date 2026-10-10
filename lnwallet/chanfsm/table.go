package chanfsm

import "github.com/lightningnetwork/lnd/protofsm"

type (
	// chanTable is the transition table type of the channel state
	// machine.
	chanTable = protofsm.TransitionTable[ChanState, Event, Outbox]

	// chanFrom is the per-state entry type of the table.
	chanFrom = protofsm.StateTransitions[ChanState, Event, Outbox]

	// chanEntry is a single transition of the table.
	chanEntry = protofsm.TransitionEntry[ChanState, Event, Outbox]
)

// outbox is shorthand for a list of outbox types in a table entry.
func outbox(o ...Outbox) []Outbox {
	return o
}

// commandEntries lists how an open state handles the commands both open
// states handle alike: authorize the operation, or refuse a removal or fee
// update the ledger doesn't allow.
func commandEntries(s ChanState) []chanEntry {
	var entries []chanEntry
	for _, ev := range []Event{
		&SettleHTLC{}, &FailHTLC{}, &MalformedFailHTLC{},
		&UpdateFee{},
	} {
		entries = append(entries, chanEntry{
			Event:       ev,
			ToState:     &Applying{},
			Description: "Allowed by the ledger, authorize it.",
			EmitsOutbox: outbox(&ApplyOp{}),
		}, chanEntry{
			Event:   ev,
			ToState: s,
			Description: "Refused: the HTLC isn't irrevocably " +
				"committed, is unknown or already removed, " +
				"or we aren't the initiator.",
			EmitsOutbox: outbox(&Reply{}),
		})
	}

	return append(entries, chanEntry{
		Event:       &AddHTLC{},
		ToState:     &Applying{},
		Description: "Authorize the add. The channel checks amounts.",
		EmitsOutbox: outbox(&ApplyOp{}),
	})
}

// peerEntries lists how an open state handles the peer messages both open
// states handle alike: authorize the operation, or fail the channel on a
// protocol violation without touching it.
func peerEntries() []chanEntry {
	var entries []chanEntry
	for _, ev := range []Event{
		&PeerAdd{}, &PeerFulfill{}, &PeerFail{}, &PeerFailMalformed{},
		&PeerUpdateFee{}, &PeerCommitSig{},
	} {
		entries = append(entries, chanEntry{
			Event:       ev,
			ToState:     &Applying{},
			Description: "Allowed by the ledger, authorize it.",
			EmitsOutbox: outbox(&ApplyOp{}),
		}, chanEntry{
			Event:   ev,
			ToState: &Failed{},
			Description: "Protocol violation: wrong HTLC ID, " +
				"removal of an HTLC not in our current " +
				"commitment, fee update from the " +
				"non-initiator, or malformed fail without " +
				"BADONION. The channel is not touched.",
			EmitsOutbox: outbox(&FailChannel{}),
			IsTerminal:  true,
		})
	}

	return append(entries, chanEntry{
		Event:   &PeerReestablish{},
		ToState: &Failed{},
		Description: "channel_reestablish only starts a " +
			"connection.",
		EmitsOutbox: outbox(&FailChannel{}),
		IsTerminal:  true,
	})
}

// reestablishEntries lists how the Reestablishing state handles events:
// only the peer's channel_reestablish moves it on.
func reestablishEntries() []chanEntry {
	entries := []chanEntry{{
		Event:   &PeerReestablish{},
		ToState: &Applying{},
		Description: "Heights the ledger can answer, or a claim " +
			"that we lost state, which only the channel can " +
			"confirm: authorize it.",
		EmitsOutbox: outbox(&ApplyOp{}),
	}, {
		Event:   &PeerReestablish{},
		ToState: &Failed{},
		Description: "The peer lost state, or asks for a " +
			"commitment no retransmission reaches. The channel " +
			"is not touched.",
		EmitsOutbox: outbox(&FailChannel{}),
		IsTerminal:  true,
	}}
	for _, ev := range []Event{
		&AddHTLC{}, &SettleHTLC{}, &FailHTLC{}, &MalformedFailHTLC{},
		&UpdateFee{}, &SignCommitment{},
	} {
		entries = append(entries, chanEntry{
			Event:       ev,
			ToState:     &Reestablishing{},
			Description: "Refused until reestablished.",
			EmitsOutbox: outbox(&Reply{}),
		})
	}
	for _, ev := range []Event{
		&PeerAdd{}, &PeerFulfill{}, &PeerFail{}, &PeerFailMalformed{},
		&PeerUpdateFee{}, &PeerCommitSig{}, &PeerRevokeAndAck{},
	} {
		entries = append(entries, chanEntry{
			Event:   ev,
			ToState: &Failed{},
			Description: "BOLT 2 requires channel_reestablish " +
				"first.",
			EmitsOutbox: outbox(&FailChannel{}),
			IsTerminal:  true,
		})
	}

	return entries
}

// completions lists how Applying handles the outcome of each operation.
func completions() []chanEntry {
	entries := []chanEntry{{
		Event:       &OpDone{},
		ToState:     &Reestablishing{},
		Description: "Send our channel_reestablish.",
		EmitsOutbox: outbox(&SendToPeer{}),
	}}
	for _, rest := range []ChanState{&Synced{}, &AwaitingRevocation{}} {
		entries = append(entries, chanEntry{
			Event:   &OpDone{},
			ToState: rest,
			Description: "Reestablished: retransmit what the " +
				"peer missed.",
			EmitsOutbox: outbox(&SendToPeer{}),
		}, chanEntry{
			Event:   &OpDone{},
			ToState: rest,
			Description: "A local command the channel refused, " +
				"which left it unchanged.",
			EmitsOutbox: outbox(&Reply{}),
		}, chanEntry{
			Event:       &OpDone{},
			ToState:     rest,
			Description: "A local update applied: send it.",
			EmitsOutbox: outbox(&SendToPeer{}, &Reply{}),
		}, chanEntry{
			Event:       &OpDone{},
			ToState:     rest,
			Description: "A peer update applied.",
		}, chanEntry{
			Event:   &OpDone{},
			ToState: rest,
			Description: "Revoked our previous commitment, " +
				"nothing to sign.",
			EmitsOutbox: outbox(&SendToPeer{}, &ContractUpdate{}),
		}, chanEntry{
			Event:   &OpDone{},
			ToState: rest,
			Description: "Revoked our previous commitment, " +
				"locking in resolutions, nothing to sign.",
			EmitsOutbox: outbox(
				&SendToPeer{}, &FinalHtlcs{},
				&ContractUpdate{},
			),
		})
	}

	return append(entries, chanEntry{
		Event:       &OpDone{},
		ToState:     &AwaitingRevocation{},
		Description: "Signed on command: send commitment_signed.",
		EmitsOutbox: outbox(
			&ContractUpdate{}, &SendToPeer{}, &Reply{},
		),
	}, chanEntry{
		Event:   &OpDone{},
		ToState: &AwaitingRevocation{},
		Description: "Signed after a commitment exchange: send " +
			"commitment_signed.",
		EmitsOutbox: outbox(&ContractUpdate{}, &SendToPeer{}),
	}, chanEntry{
		Event:   &OpDone{},
		ToState: &Applying{},
		Description: "Accepted the peer's commitment: revoke our " +
			"previous one.",
		EmitsOutbox: outbox(&ApplyOp{}),
	}, chanEntry{
		Event:   &OpDone{},
		ToState: &Applying{},
		Description: "Revoked our previous commitment and owe one: " +
			"sign.",
		EmitsOutbox: outbox(
			&SendToPeer{}, &ContractUpdate{}, &ApplyOp{},
		),
	}, chanEntry{
		Event:   &OpDone{},
		ToState: &Applying{},
		Description: "Revoked our previous commitment, locking in " +
			"resolutions, and owe one: sign.",
		EmitsOutbox: outbox(
			&SendToPeer{}, &FinalHtlcs{}, &ContractUpdate{},
			&ApplyOp{},
		),
	}, chanEntry{
		Event:   &OpDone{},
		ToState: &Synced{},
		Description: "The peer revoked: forward what it locked in, " +
			"nothing to sign.",
		EmitsOutbox: outbox(&ContractUpdate{}, &ForwardPackage{}),
	}, chanEntry{
		Event:   &OpDone{},
		ToState: &Applying{},
		Description: "The peer revoked: forward what it locked in, " +
			"and sign what we owe.",
		EmitsOutbox: outbox(
			&ContractUpdate{}, &ForwardPackage{}, &ApplyOp{},
		),
	}, chanEntry{
		Event:   &OpDone{},
		ToState: &Failed{},
		Description: "The channel refused a peer update, a " +
			"commitment operation failed, or its result " +
			"disagrees with the ledger.",
		EmitsOutbox: outbox(&FailChannel{}),
		IsTerminal:  true,
	}, chanEntry{
		Event:   &OpDone{},
		ToState: &Failed{},
		Description: "Signing on command failed, or its result " +
			"disagrees with the ledger.",
		EmitsOutbox: outbox(&FailChannel{}, &Reply{}),
		IsTerminal:  true,
	})
}

// failedEntries lists how Failed absorbs every event.
func failedEntries() []chanEntry {
	var entries []chanEntry
	for _, ev := range []Event{
		&AddHTLC{}, &SettleHTLC{}, &FailHTLC{}, &MalformedFailHTLC{},
		&UpdateFee{}, &SignCommitment{},
	} {
		entries = append(entries, chanEntry{
			Event:       ev,
			ToState:     &Failed{},
			Description: "Refused: the channel failed.",
			EmitsOutbox: outbox(&Reply{}),
			IsTerminal:  true,
		})
	}
	for _, ev := range []Event{
		&PeerAdd{}, &PeerFulfill{}, &PeerFail{}, &PeerFailMalformed{},
		&PeerUpdateFee{}, &PeerCommitSig{}, &PeerRevokeAndAck{},
		&PeerReestablish{},
	} {
		entries = append(entries, chanEntry{
			Event:       ev,
			ToState:     &Failed{},
			Description: "Dropped: the channel failed.",
			IsTerminal:  true,
		})
	}

	return entries
}

// ChannelTransitions is the transition table of the channel state machine.
// It documents the machine, and the tests fail on any transition it doesn't
// list.
var ChannelTransitions = chanTable{
	MachineName: "ChannelStateMachine",
	States: []chanFrom{{
		FromState: &Connecting{},
		Transitions: []chanEntry{{
			Event:   &Connect{},
			ToState: &Applying{},
			Description: "Loaded from disk and connected: build " +
				"our channel_reestablish.",
			EmitsOutbox: outbox(&ApplyOp{}),
		}},
	}, {
		FromState:   &Reestablishing{},
		Transitions: reestablishEntries(),
	}, {
		FromState: &Synced{},
		Transitions: append(append([]chanEntry{{
			Event:   &PeerRevokeAndAck{},
			ToState: &Failed{},
			Description: "No commitment is awaiting revocation, " +
				"so there is nothing to revoke. The channel " +
				"is not touched.",
			EmitsOutbox: outbox(&FailChannel{}),
			IsTerminal:  true,
		}, {
			Event:       &SignCommitment{},
			ToState:     &Applying{},
			Description: "We owe a commitment: sign it.",
			EmitsOutbox: outbox(&ApplyOp{}),
		}, {
			Event:       &SignCommitment{},
			ToState:     &Synced{},
			Description: "Nothing to sign.",
			EmitsOutbox: outbox(&Reply{}),
		}}, commandEntries(&Synced{})...), peerEntries()...),
	}, {
		FromState: &AwaitingRevocation{},
		Transitions: append(append([]chanEntry{{
			Event:   &PeerRevokeAndAck{},
			ToState: &Applying{},
			Description: "The revocation that completes the " +
				"commitment we signed: authorize it.",
			EmitsOutbox: outbox(&ApplyOp{}),
		}, {
			Event:   &SignCommitment{},
			ToState: &AwaitingRevocation{},
			Description: "The peer hasn't revoked, so we may not " +
				"sign.",
			EmitsOutbox: outbox(&Reply{}),
		}}, commandEntries(&AwaitingRevocation{})...),
			peerEntries()...),
	}, {
		FromState:   &Applying{},
		Transitions: completions(),
	}, {
		FromState:   &Failed{},
		Transitions: failedEntries(),
	}},
}
