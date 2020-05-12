// +build kvdb_etcd

package etcd

import (
	"crypto/sha256"
)

const (
	bucketIDLength = 32
)

var (
	bucketPrefix   = []byte("b")
	valuePrefix    = []byte("v")
	sequencePrefix = []byte("$")
)

// makeBucketID returns a deterministic key for the passed byte slice.
// Currently it returns the sha256 hash of the slice.
func makeBucketID(key []byte) [bucketIDLength]byte {
	return sha256.Sum256(key)
}

// isValidBucketID checks if the passed slice is the required length to be a
// valid bucket id.
func isValidBucketID(s []byte) bool {
	return len(s) == bucketIDLength
}

// makeKey concatenates prefix, parent and key into one byte slice.
// The prefix indicates the use of this key (whether bucket, value or sequence),
// while parentID refers to the parent bucket.
func makeKey(prefix, parent, key []byte) []byte {
	keyBuf := make([]byte, len(prefix)+len(parent)+len(key))
	copy(keyBuf, prefix)
	copy(keyBuf[len(prefix):], parent)
	copy(keyBuf[len(prefix)+len(parent):], key)

	return keyBuf
}

// makePrefix concatenates prefix with parent into one byte slice.
func makePrefix(prefix []byte, parent []byte) []byte {
	prefixBuf := make([]byte, len(prefix)+len(parent))
	copy(prefixBuf, prefix)
	copy(prefixBuf[len(prefix):], parent)

	return prefixBuf
}

// makeBucketKey returns a bucket key from the passed parent bucket id and
// the key.
func makeBucketKey(parent []byte, key []byte) []byte {
	return makeKey(bucketPrefix, parent, key)
}

// makeValueKey returns a value key from the passed parent bucket id and
// the key.
func makeValueKey(parent []byte, key []byte) []byte {
	return makeKey(valuePrefix, parent, key)
}

// makeSequenceKey returns a sequence key of the passed parent bucket id.
func makeSequenceKey(parent []byte) []byte {
	return makeKey(sequencePrefix, parent, nil)
}

// makeBucketPrefix returns the bucket prefix of the passed parent bucket id.
// This prefix is used for all sub buckets.
func makeBucketPrefix(parent []byte) []byte {
	return makePrefix(bucketPrefix, parent)
}

// makeValuePrefix returns the value prefix of the passed parent bucket id.
// This prefix is used for all key/values in the bucket.
func makeValuePrefix(parent []byte) []byte {
	return makePrefix(valuePrefix, parent)
}
