//go:build kvdb_etcd
// +build kvdb_etcd

package cluster

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb/etcd"
)

// makeEtcdElector will construct a new etcdLeaderElector. It expects a cancel
// context a unique (in the cluster) LND id and a *etcd.Config as arguments.
func makeEtcdElector(ctx context.Context, args ...interface{}) (LeaderElector,
	error) {

	if len(args) != 4 {
		return nil, fmt.Errorf("invalid number of arguments to "+
			"cluster.makeEtcdElector(): expected 4, got %v",
			len(args))
	}

	id, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("invalid argument (0) to " +
			"cluster.makeEtcdElector(), expected: string")
	}

	electionPrefix, ok := args[1].(string)
	if !ok {
		return nil, fmt.Errorf("invalid argument (1) to " +
			"cluster.makeEtcdElector(), expected: string")
	}

	leaderSessionTTL, ok := args[2].(int)
	if !ok {
		return nil, fmt.Errorf("invalid argument (2) to " +
			"cluster.makeEtcdElector(), expected: int")
	}

	etcdCfg, ok := args[3].(*etcd.Config)
	if !ok {
		return nil, fmt.Errorf("invalid argument (3) to " +
			"cluster.makeEtcdElector(), expected: *etcd.Config")
	}

	return newEtcdLeaderElector(
		ctx, id, electionPrefix, leaderSessionTTL, etcdCfg,
	)
}

func init() {
	RegisterLeaderElectorFactory(EtcdLeaderElector, makeEtcdElector)
}
