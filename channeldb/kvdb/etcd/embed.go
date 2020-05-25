// +build kvdb_etcd

package etcd

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/coreos/etcd/embed"
)

const (
	// readyTimeout is the time until the embedded etcd instance should start.
	readyTimeout = 10 * time.Second
)

// getFreePort returns a random open TCP port.
func getFreePort() int {
	ln, err := net.Listen("tcp", "[::]:0")
	if err != nil {
		panic(err)
	}

	port := ln.Addr().(*net.TCPAddr).Port

	err = ln.Close()
	if err != nil {
		panic(err)
	}

	return port
}

// NewEmbeddedEtcdInstance creates an embedded etcd instance for testing,
// listening on random open ports. Returns the backend config and a cleanup
// func that will stop the etcd instance.
func NewEmbeddedEtcdInstance(path string) (*BackendConfig, func(), error) {
	cfg := embed.NewConfig()
	cfg.Dir = path

	// To ensure that we can submit large transactions.
	cfg.MaxTxnOps = 1000

	// Listen on random free ports.
	clientURL := fmt.Sprintf("127.0.0.1:%d", getFreePort())
	peerURL := fmt.Sprintf("127.0.0.1:%d", getFreePort())
	cfg.LCUrls = []url.URL{{Host: clientURL}}
	cfg.LPUrls = []url.URL{{Host: peerURL}}

	etcd, err := embed.StartEtcd(cfg)
	if err != nil {
		return nil, nil, err
	}

	select {
	case <-etcd.Server.ReadyNotify():
	case <-time.After(readyTimeout):
		etcd.Close()
		return nil, nil,
			fmt.Errorf("etcd failed to start after: %v", readyTimeout)
	}

	connConfig := &BackendConfig{
		Ctx:                context.Background(),
		Host:               "http://" + peerURL,
		User:               "user",
		Pass:               "pass",
		InsecureSkipVerify: true,
	}

	return connConfig, func() {
		etcd.Close()
	}, nil
}
