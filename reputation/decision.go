package reputation

// outcome is the result of the (log-only) forwarding decision for an HTLC. In
// enforcement mode `forward == false` would cause the HTLC to be failed; here
// it is only logged.
type outcome struct {
	// forward reports whether the HTLC would be forwarded (true) or failed
	// (false) under enforcement.
	forward bool

	// accountable is the accountable signal that would be set on the
	// outgoing HTLC if it were forwarded.
	accountable bool

	// bucket is the resource bucket the HTLC was assigned to (only
	// meaningful when forward is true).
	bucket bucketAssigned
}

// String returns a human readable description of the outcome for logging.
func (o outcome) String() string {
	if !o.forward {
		return "FAIL (insufficient resources/reputation)"
	}

	if o.accountable {
		return "FORWARD accountable in " + o.bucket.String() + " bucket"
	}

	return "FORWARD in " + o.bucket.String() + " bucket"
}
