module github.com/lightningnetwork/lnd/kvdb

require (
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/btcsuite/btcwallet/walletdb v1.3.6-0.20210803004036-eebed51155ec
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/google/btree v1.0.1
	github.com/lightningnetwork/lnd/healthcheck v1.0.0
	github.com/stretchr/testify v1.7.0
	go.etcd.io/bbolt v1.3.6
	go.etcd.io/etcd/api/v3 v3.5.0
	go.etcd.io/etcd/client/pkg/v3 v3.5.0
	go.etcd.io/etcd/client/v3 v3.5.0
	go.etcd.io/etcd/server/v3 v3.5.0
)

// This replace is for https://github.com/advisories/GHSA-w73w-5m7g-f7qc
replace github.com/dgrijalva/jwt-go => github.com/golang-jwt/jwt v3.2.1+incompatible

go 1.15
