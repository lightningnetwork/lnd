package tlv

const (
	// AmtOnionType is the type used in the onion to refrence the amount to
	// send to the next hop.
	AmtOnionType Type = 2

	// LockTimeTLV is the type used in the onion to refenernce the CLTV
	// value that should be used for the next hop's HTLC.
	LockTimeOnionType Type = 4

	// NextHopOnionType is the type used in the onion to reference the ID
	// of the next hop.
	NextHopOnionType Type = 6
)
