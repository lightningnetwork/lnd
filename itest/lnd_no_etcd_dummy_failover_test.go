//go:build !kvdb_etcd
// +build !kvdb_etcd

package itest

import "github.com/lightningnetwork/lnd/lntemp"

// testEtcdFailover is an empty itest when LND is not compiled with etcd
// support.
func testEtcdFailover(ht *lntemp.HarnessTest) {}
