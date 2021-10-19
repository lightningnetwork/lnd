module github.com/lightningnetwork/lnd/kvdb

require (
	github.com/andybalholm/brotli v1.0.3 // indirect
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/btcsuite/btcwallet/walletdb v1.3.6-0.20210803004036-eebed51155ec
	github.com/davecgh/go-spew v1.1.1
	github.com/fergusstrange/embedded-postgres v1.10.0
	github.com/golang/snappy v0.0.4 // indirect
	github.com/google/btree v1.0.1
	github.com/jackc/pgx/v4 v4.13.0
	github.com/klauspost/compress v1.13.6 // indirect
	github.com/klauspost/pgzip v1.2.5 // indirect
	github.com/lib/pq v1.10.3 // indirect
	github.com/lightningnetwork/lnd/healthcheck v1.0.0
	github.com/nwaples/rardecode v1.1.2 // indirect
	github.com/pierrec/lz4/v4 v4.1.8 // indirect
	github.com/stretchr/testify v1.7.0
	github.com/ulikunitz/xz v0.5.10 // indirect
	go.etcd.io/bbolt v1.3.6
	go.etcd.io/etcd/api/v3 v3.5.0
	go.etcd.io/etcd/client/pkg/v3 v3.5.0
	go.etcd.io/etcd/client/v3 v3.5.0
	go.etcd.io/etcd/server/v3 v3.5.0
	golang.org/x/net v0.0.0-20210405180319-a5a99cb37ef4
)

// This replace is for https://github.com/advisories/GHSA-w73w-5m7g-f7qc
replace github.com/dgrijalva/jwt-go => github.com/golang-jwt/jwt v3.2.1+incompatible

// This replace is for https://github.com/advisories/GHSA-25xm-hr59-7c27
replace github.com/ulikunitz/xz => github.com/ulikunitz/xz v0.5.8

// This replace is for
// https://deps.dev/advisory/OSV/GO-2021-0053?from=%2Fgo%2Fgithub.com%252Fgogo%252Fprotobuf%2Fv1.3.1
replace github.com/gogo/protobuf => github.com/gogo/protobuf v1.3.2

go 1.16
