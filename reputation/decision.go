package reputation

import "fmt"

// isolationDecision is the result of the per-HTLC isolation verdict: if this
// HTLC were forwarded in isolation, would its outgoing channel have sufficient
// reputation to be protected?
//
//	sufficient = outgoingReputation - htlcRisk >= revenueThreshold
//
// It is log-only: the verdict never affects forwarding. In a later enforcement
// step an insufficient verdict is what would deny an HTLC the protected
// resources.
type isolationDecision struct {
	// sufficient reports whether the outgoing channel's reputation would be
	// sufficient for this HTLC to be protected in isolation.
	sufficient bool

	// outgoingReputation is the outgoing channel's reputation at decision
	// time.
	outgoingReputation int64

	// htlcRisk is the in-flight risk of this HTLC.
	htlcRisk uint64

	// threshold is the incoming channel's revenue threshold the reputation
	// was compared against.
	threshold int64
}

// String returns a human readable description of the isolation decision for
// logging.
func (d isolationDecision) String() string {
	return fmt.Sprintf("sufficient=%v (outgoing_reputation=%d - "+
		"htlc_risk=%d vs threshold=%d)", d.sufficient,
		d.outgoingReputation, d.htlcRisk, d.threshold)
}
