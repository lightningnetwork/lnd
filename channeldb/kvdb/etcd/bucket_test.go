// +build kvdb_etcd

package etcd

// bkey is a helper functon used in tests to create a bucket key from passed
// bucket list.
func bkey(buckets ...string) string {
	var bucketKey []byte

	rootID := makeBucketID([]byte(""))
	parent := rootID[:]

	for _, bucketName := range buckets {
		bucketKey = makeBucketKey(parent, []byte(bucketName))
		id := makeBucketID(bucketKey)
		parent = id[:]
	}

	return string(bucketKey)
}

// bval is a helper function used in tests to create a bucket value (the value
// for a bucket key) from the passed bucket list.
func bval(buckets ...string) string {
	id := makeBucketID([]byte(bkey(buckets...)))
	return string(id[:])
}

// vkey is a helper function used in tests to create a value key from the
// passed key and bucket list.
func vkey(key string, buckets ...string) string {
	rootID := makeBucketID([]byte(""))
	bucket := rootID[:]

	for _, bucketName := range buckets {
		bucketKey := makeBucketKey(bucket, []byte(bucketName))
		id := makeBucketID(bucketKey)
		bucket = id[:]
	}

	return string(makeValueKey(bucket, []byte(key)))
}
