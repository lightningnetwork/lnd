//go:build kvdb_etcd
// +build kvdb_etcd

package cluster

import (
	"context"
	"io/ioutil"
	"os"
	"runtime/pprof"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/kvdb/etcd"
	"github.com/stretchr/testify/require"
)

// GuardTimeout implements a test level timeout guard.
func GuardTimeout(t *testing.T, timeout time.Duration) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(timeout):
			err := pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
			require.NoError(t, err)
			panic("test timeout")

		case <-done:
		}
	}()

	return func() {
		close(done)
	}
}

// TestEtcdElector tests that two candidates competing for leadership works as
// expected and that elected leader can resign and allow others to take on.
func TestEtcdElector(t *testing.T) {
	guard := GuardTimeout(t, 5*time.Second)
	defer guard()

	tmpDir, err := ioutil.TempDir("", "etcd")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v", err)
	}

	etcdCfg, cleanup, err := etcd.NewEmbeddedEtcdInstance(tmpDir, 0, 0, "")
	require.NoError(t, err)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		election = "/election/"
		id1      = "e1"
		id2      = "e2"
	)

	e1, err := newEtcdLeaderElector(
		ctx, id1, election, etcdCfg,
	)
	require.NoError(t, err)

	e2, err := newEtcdLeaderElector(
		ctx, id2, election, etcdCfg,
	)
	require.NoError(t, err)

	var wg sync.WaitGroup
	ch := make(chan *etcdLeaderElector)

	wg.Add(2)
	ctxb := context.Background()

	go func() {
		defer wg.Done()
		require.NoError(t, e1.Campaign(ctxb))
		ch <- e1
	}()

	go func() {
		defer wg.Done()
		require.NoError(t, e2.Campaign(ctxb))
		ch <- e2
	}()

	tmp := <-ch
	first, err := tmp.Leader(ctxb)
	require.NoError(t, err)
	require.NoError(t, tmp.Resign())

	tmp = <-ch
	second, err := tmp.Leader(ctxb)
	require.NoError(t, err)
	require.NoError(t, tmp.Resign())

	require.Contains(t, []string{id1, id2}, first)
	require.Contains(t, []string{id1, id2}, second)
	require.NotEqual(t, first, second)

	wg.Wait()
}
