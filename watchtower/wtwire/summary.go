package wtwire

import "fmt"

// MessageSummary creates a human-readable description of a given Message. If
// the type is unknown, an empty string is returned.
func MessageSummary(msg Message) string {
	switch msg := msg.(type) {
	case *Init:
		return ""

	case *CreateSession:
		return fmt.Sprintf("blob_type=%s, max_updates=%d "+
			"reward_base=%d reward_rate=%d sweep_fee_rate=%d",
			msg.BlobType, msg.MaxUpdates, msg.RewardBase,
			msg.RewardRate, msg.SweepFeeRate)

	case *CreateSessionReply:
		return fmt.Sprintf("code=%d", msg.Code)

	case *StateUpdate:
		return fmt.Sprintf("seqnum=%d last_applied=%d is_complete=%d "+
			"hint=%x", msg.SeqNum, msg.LastApplied, msg.IsComplete,
			msg.Hint)

	case *StateUpdateReply:
		return fmt.Sprintf("code=%d last_applied=%d", msg.Code,
			msg.LastApplied)

	case *Error:
		return fmt.Sprintf("code=%d", msg.Code)

	default:
		return ""
	}
}
