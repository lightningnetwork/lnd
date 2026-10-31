//go:build kvdb_etcd

package etcd

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type failingReadSTM struct {
	STM
	err        error
	committed  bool
	rolledBack bool
}

func (s *failingReadSTM) Get(string) ([]byte, error)       { return nil, s.err }
func (s *failingReadSTM) Commit() error                    { s.committed = true; return nil }
func (s *failingReadSTM) Rollback()                        { s.rolledBack = true }
func (s *failingReadSTM) Put(string, string)               {}
func (s *failingReadSTM) First(string) (*KV, error)        { return nil, s.err }
func (s *failingReadSTM) Last(string) (*KV, error)         { return nil, s.err }
func (s *failingReadSTM) Next(string, string) (*KV, error) { return nil, s.err }
func (s *failingReadSTM) Prev(string, string) (*KV, error) { return nil, s.err }
func (s *failingReadSTM) Seek(string, string) (*KV, error) { return nil, s.err }

func TestErrorlessReadShapes(t *testing.T) {
	readErr := errors.New("read failed")
	tests := []struct {
		name string
		read func(*readWriteTx)
	}{
		{"top-level bucket", func(tx *readWriteTx) { tx.ReadBucket([]byte("bucket")) }},
		{"nested bucket", func(tx *readWriteTx) { rootBucket(tx).NestedReadBucket([]byte("bucket")) }},
		{"sequence", func(tx *readWriteTx) { rootBucket(tx).Sequence() }},
		{"next sequence", func(tx *readWriteTx) { _, _ = rootBucket(tx).NextSequence() }},
		{"cursor first", func(tx *readWriteTx) { rootBucket(tx).ReadCursor().First() }},
		{"cursor last", func(tx *readWriteTx) { rootBucket(tx).ReadCursor().Last() }},
		{"cursor next", func(tx *readWriteTx) { rootBucket(tx).ReadCursor().Next() }},
		{"cursor prev", func(tx *readWriteTx) { rootBucket(tx).ReadCursor().Prev() }},
		{"cursor seek", func(tx *readWriteTx) { rootBucket(tx).ReadCursor().Seek([]byte("key")) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manualSTM := &failingReadSTM{err: readErr}
			manual := newReadWriteTx(manualSTM, "test", nil)
			tc.read(manual)
			require.ErrorIs(t, manual.Commit(), readErr)
			require.False(t, manualSTM.committed)
			require.True(t, manualSTM.rolledBack)

			managedSTM := &failingReadSTM{err: readErr}
			managed := newReadWriteTx(managedSTM, "test", nil)
			err := managed.apply(func() error { tc.read(managed); return nil })
			require.ErrorIs(t, err, readErr)
			require.False(t, managedSTM.committed)
		})
	}
}

func TestFirstReadErrorRetained(t *testing.T) {
	first := errors.New("first")
	second := errors.New("second")
	stm := &failingReadSTM{err: first}
	tx := newReadWriteTx(stm, "test", nil)
	rootBucket(tx).Get([]byte("key"))
	stm.err = second
	rootBucket(tx).ReadCursor().First()
	require.ErrorIs(t, tx.Commit(), first)
	require.True(t, stm.rolledBack)
	require.False(t, stm.committed)
}

func TestBucketReadErrorTransaction(t *testing.T) {
	readErr := errors.New("injected read failure")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "missing"},
		{name: "read failure", err: readErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stm := &failingReadSTM{err: tc.err}
			tx := newReadWriteTx(stm, "test", nil)
			bucket := rootBucket(tx)
			require.Nil(t, bucket.Get([]byte("key")))
			require.ErrorIs(t, tx.Commit(), tc.err)
			require.Equal(t, tc.err == nil, stm.committed)
			require.Equal(t, tc.err != nil, stm.rolledBack)

			managed := newReadWriteTx(&failingReadSTM{err: tc.err}, "test", nil)
			err := managed.apply(func() error {
				require.Nil(t, rootBucket(managed).Get([]byte("key")))
				return nil
			})
			require.ErrorIs(t, err, tc.err)

			readOnly := newReadWriteTx(&failingReadSTM{err: tc.err}, "test", nil)
			require.Nil(t, rootBucket(readOnly).Get([]byte("key")))
			require.ErrorIs(t, readOnly.Rollback(), tc.err)
		})
	}
}
