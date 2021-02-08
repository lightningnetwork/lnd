package lncfg

import (
	"fmt"

	"github.com/lightningnetwork/lnd/cluster"
)

// Cluster holds configuration for clustered LND.
type Cluster struct {
	LeaderElector string `long:"leader-elector" choice:"etcd" description:"Leader elector to use. Valid values: \"etcd\"."`

	EtcdElectionPrefix string `long:"etcd-election-prefix" description:"Election key prefix when using etcd leader elector."`
}

// DefaultCluster creates and returns a new default DB config.
func DefaultCluster() *Cluster {
	return &Cluster{}
}

// Validate validates the Cluster config.
func (c *Cluster) Validate() error {
	switch c.LeaderElector {
	case cluster.EtcdLeaderElector:
		if c.EtcdElectionPrefix == "" {
			return fmt.Errorf("etcd-election-prefix must be set")
		}

	default:
		return fmt.Errorf("unknown leader elector, valid values are: "+
			"\"%v\"", cluster.EtcdLeaderElector)
	}

	return nil
}

// Compile-time constraint to ensure Workers implements the Validator interface.
var _ Validator = (*Cluster)(nil)
