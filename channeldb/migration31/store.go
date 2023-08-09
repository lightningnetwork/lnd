package migration31

var (
	// lastTxBucketKey is the key that points to a bucket containing a
	// single item storing the last published sweep tx.
	//
	// maps: lastTxKey -> serialized_tx
	lastTxBucketKey = []byte("sweeper-last-tx")
)
