package cluster

import (
	"context"
)

const (
	// EtcdLeaderElector is the id used when constructing an
	// etcdLeaderElector instance through the factory.
	EtcdLeaderElector = "etcd"
)

// LeaderElector is a general interface implementing basic leader elections
// in a clustered environment.
type LeaderElector interface {
	// Campaign starts a run for leadership. Campaign will block until
	// the caller is elected as the leader.
	Campaign(ctx context.Context) error

	// Resign resigns from the leader role, allowing other election members
	// to take on leadership.
	Resign() error

	// Leader returns the leader value for the current election.
	Leader(ctx context.Context) (string, error)
}
