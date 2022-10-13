//go:build kvdb_etcd
// +build kvdb_etcd

package etcd

import (
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBucketKey tests that a key for a bucket can be created correctly.
func TestBucketKey(t *testing.T) {
	rootID := sha256.Sum256([]byte("@"))
	key := append(rootID[:], []byte("foo")...)
	key = append(key, 0xff)
	require.Equal(t, string(key), BucketKey("foo"))
}

// TestBucketVal tests that a key for a bucket value can be created correctly.
func TestBucketVal(t *testing.T) {
	rootID := sha256.Sum256([]byte("@"))
	key := append(rootID[:], []byte("foo")...)
	key = append(key, 0xff)

	keyID := sha256.Sum256(key)
	require.Equal(t, string(keyID[:]), BucketVal("foo"))
}

// TestSequenceKey tests that a key for a sequence can be created correctly.
func TestSequenceKey(t *testing.T) {
	require.Contains(t, SequenceKey("foo", "bar", "baz"), "$seq$")
}
