package neutrinonotify

import (
	"sync"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

type progressHintCache struct {
	hint       uint32
	commits    int
	purges     int
	beforeSave func()
}

func (c *progressHintCache) CommitSpendHint(height uint32,
	_ ...chainntnfs.SpendRequest) error {

	c.commits++
	if c.beforeSave != nil {
		c.beforeSave()
	}
	c.hint = height

	return nil
}

func (c *progressHintCache) QuerySpendHint(
	_ chainntnfs.SpendRequest) (uint32, error) {

	if c.hint == 0 {
		return 0, chainntnfs.ErrSpendHintNotFound
	}

	return c.hint, nil
}

func (c *progressHintCache) PurgeSpendHint(
	_ ...chainntnfs.SpendRequest) error {

	c.purges++
	c.hint = 0

	return nil
}

func TestCommitSpendHintIfActive(t *testing.T) {
	request, err := chainntnfs.NewSpendRequest(
		&wire.OutPoint{Index: 1},
		chainntnfs.ZeroTaprootPkScript.Script(),
	)
	require.NoError(t, err)

	t.Run("active scan commits", func(t *testing.T) {
		cache := &progressHintCache{}
		var scanMtx sync.Mutex
		var canceled bool

		require.NoError(t, commitSpendHintIfActive(
			cache, &scanMtx, &canceled, 101, request,
		))
		require.Equal(t, 1, cache.commits)
		require.Zero(t, cache.purges)
		require.Equal(t, uint32(101), cache.hint)
	})

	t.Run("canceled scan does not commit", func(t *testing.T) {
		cache := &progressHintCache{}
		var scanMtx sync.Mutex
		canceled := true

		require.NoError(t, commitSpendHintIfActive(
			cache, &scanMtx, &canceled, 101, request,
		))
		require.Zero(t, cache.commits)
		require.Zero(t, cache.purges)
		require.Zero(t, cache.hint)
	})

	t.Run("cancel during commit purges", func(t *testing.T) {
		cache := &progressHintCache{}
		var scanMtx sync.Mutex
		var canceled bool
		commitStarted := make(chan struct{})
		finishCommit := make(chan struct{})
		cache.beforeSave = func() {
			close(commitStarted)
			<-finishCommit
		}

		commitErr := make(chan error, 1)
		go func() {
			commitErr <- commitSpendHintIfActive(
				cache, &scanMtx, &canceled, 101, request,
			)
		}()
		<-commitStarted

		cancelDone := make(chan struct{})
		purgeErr := make(chan error, 1)
		go func() {
			scanMtx.Lock()
			defer scanMtx.Unlock()
			defer close(cancelDone)

			canceled = true
			purgeErr <- cache.PurgeSpendHint(request)
		}()
		close(finishCommit)
		require.NoError(t, <-commitErr)
		<-cancelDone
		require.NoError(t, <-purgeErr)
		require.Equal(t, 1, cache.commits)
		require.Equal(t, 1, cache.purges)
		require.Zero(t, cache.hint)
	})
}
