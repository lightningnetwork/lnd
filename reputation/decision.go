package reputation

import "fmt"

// decision is the result of evaluating the reputation inequality for an HTLC:
//
//	sufficient = outgoingReputation - risk >= revenueThreshold
//
// It is evaluated two ways. inIsolation scores the HTLC on its own risk, which
// answers "could this HTLC stand on the outgoing channel's reputation if it
// were the only one in flight?". withInFlight additionally subtracts the risk
// of the accountable HTLCs already in flight on that channel, which is the
// verdict BOLT #1280 defines. Both are log-only: neither affects forwarding.
type decision struct {
	// inIsolation reports whether the outgoing channel's reputation covers
	// this HTLC's risk alone.
	inIsolation bool

	// withInFlight reports whether it also covers the risk of the
	// accountable HTLCs already in flight on the outgoing channel.
	withInFlight bool

	// outgoingReputation is the outgoing channel's reputation at decision
	// time.
	outgoingReputation int64

	// htlcRisk is the in-flight risk of this HTLC alone.
	htlcRisk uint64

	// totalRisk is htlcRisk plus the risk of the accountable HTLCs already
	// in flight on the outgoing channel.
	totalRisk int64

	// threshold is the incoming channel's revenue threshold the reputation
	// was compared against.
	threshold int64
}

// String returns a human readable description of the decision for logging.
func (d decision) String() string {
	return fmt.Sprintf("in_isolation=%v with_in_flight=%v "+
		"(outgoing_reputation=%d - htlc_risk=%d / total_risk=%d vs "+
		"threshold=%d)", d.inIsolation, d.withInFlight,
		d.outgoingReputation, d.htlcRisk, d.totalRisk, d.threshold)
}
