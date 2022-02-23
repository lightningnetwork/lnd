module github.com/lightningnetwork/lnd

require (
	github.com/NebulousLabs/go-upnp v0.0.0-20180202185039-29b680b06c82
	github.com/Yawning/aez v0.0.0-20180114000226-4dad034d9db2
	github.com/btcsuite/btcd v0.22.0-beta.0.20220207191057-4dc4ff7963b4
	github.com/btcsuite/btcd/btcec/v2 v2.1.0
	github.com/btcsuite/btcd/btcutil v1.1.0
	github.com/btcsuite/btcd/btcutil/psbt v1.1.0
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/btcsuite/btcwallet v0.14.0
	github.com/btcsuite/btcwallet/wallet/txauthor v1.2.1
	github.com/btcsuite/btcwallet/wallet/txrules v1.2.0
	github.com/btcsuite/btcwallet/walletdb v1.3.6-0.20210803004036-eebed51155ec
	github.com/btcsuite/btcwallet/wtxmgr v1.5.0
	github.com/coreos/go-systemd v0.0.0-20190719114852-fd7a80b32e1f
	github.com/davecgh/go-spew v1.1.1
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.0.1
	github.com/dvyukov/go-fuzz v0.0.0-20210602112143-b1f3d6f4ef4e
	github.com/go-errors/errors v1.0.1
	github.com/golang/protobuf v1.5.2
	github.com/gorilla/websocket v1.4.2
	github.com/grpc-ecosystem/go-grpc-middleware v1.3.0
	github.com/grpc-ecosystem/go-grpc-prometheus v1.2.0
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.5.0
	github.com/jackc/pgx/v4 v4.13.0
	github.com/jackpal/gateway v1.0.5
	github.com/jackpal/go-nat-pmp v0.0.0-20170405195558-28a68d0c24ad
	github.com/jedib0t/go-pretty/v6 v6.2.7
	github.com/jessevdk/go-flags v1.4.0
	github.com/jrick/logrotate v1.0.0
	github.com/kkdai/bstream v1.0.0
	github.com/lightninglabs/neutrino v0.13.2
	github.com/lightninglabs/protobuf-hex-display v1.4.3-hex-display
	github.com/lightningnetwork/lightning-onion v1.0.2-0.20220211021909-bb84a1ccb0c5
	github.com/lightningnetwork/lnd/cert v1.1.1
	github.com/lightningnetwork/lnd/clock v1.1.0
	github.com/lightningnetwork/lnd/healthcheck v1.2.1
	github.com/lightningnetwork/lnd/kvdb v1.3.1
	github.com/lightningnetwork/lnd/queue v1.1.0
	github.com/lightningnetwork/lnd/ticker v1.1.0
	github.com/lightningnetwork/lnd/tlv v1.0.2
	github.com/ltcsuite/ltcd v0.0.0-20190101042124-f37f8bf35796
	github.com/miekg/dns v1.1.43
	github.com/prometheus/client_golang v1.11.0
	github.com/stretchr/testify v1.7.0
	github.com/tv42/zbase32 v0.0.0-20160707012821-501572607d02
	github.com/urfave/cli v1.22.4
	go.etcd.io/etcd/client/pkg/v3 v3.5.0
	go.etcd.io/etcd/client/v3 v3.5.0
	golang.org/x/crypto v0.0.0-20210921155107-089bfa567519
	golang.org/x/net v0.0.0-20211015210444-4f30a5c0130f
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c
	golang.org/x/sys v0.0.0-20211210111614-af8b64212486 // indirect
	golang.org/x/term v0.0.0-20201126162022-7de9c90e9dd1
	golang.org/x/time v0.0.0-20210220033141-f8bda1e9f3ba
	golang.org/x/tools v0.1.8 // indirect
	google.golang.org/grpc v1.38.0
	google.golang.org/protobuf v1.26.0
	gopkg.in/macaroon-bakery.v2 v2.0.1
	gopkg.in/macaroon.v2 v2.0.0
)

// This replace is for addressing the CVE https://github.com/advisories/GHSA-f6mq-5m25-4r72
// This is a indirect dependency that cannot be upgraded directly.
replace go.mongodb.org/mongo-driver => go.mongodb.org/mongo-driver v1.5.1

replace git.schwanenlied.me/yawning/bsaes.git => github.com/Yawning/bsaes v0.0.0-20180720073208-c0276d75487e

// This replace is for https://github.com/advisories/GHSA-w73w-5m7g-f7qc
replace github.com/dgrijalva/jwt-go => github.com/golang-jwt/jwt v3.2.1+incompatible

// This replace is for https://github.com/advisories/GHSA-25xm-hr59-7c27
replace github.com/ulikunitz/xz => github.com/ulikunitz/xz v0.5.8

// This replace is for
// https://deps.dev/advisory/OSV/GO-2021-0053?from=%2Fgo%2Fgithub.com%252Fgogo%252Fprotobuf%2Fv1.3.1
replace github.com/gogo/protobuf => github.com/gogo/protobuf v1.3.2

// The old version of ginko that's used in btcd imports an ancient version of
// gopkg.in/fsnotify.v1 that isn't go mod compatible. We fix that import error
// by replacing ginko (which is only a test library anyway) with a more recent
// version.
replace github.com/onsi/ginkgo => github.com/onsi/ginkgo v1.14.2

// There's a bug in Neutrino that causes our tests to fail. Downgrade to the
// version just before the offending PR. Can remove again once
// https://github.com/lightninglabs/neutrino/pull/247 is merged.
replace github.com/lightninglabs/neutrino => github.com/lightninglabs/neutrino v0.13.2-0.20220209052920-0c79b771272b

replace (
	github.com/btcsuite/btcd => github.com/Roasbeef/btcd v0.0.0-20220309025241-21f170aa104d
	github.com/btcsuite/btcd/btcec/v2 => github.com/Roasbeef/btcd/btcec/v2 v2.0.0-20220309025241-21f170aa104d
	github.com/btcsuite/btcwallet => github.com/guggero/btcwallet v0.13.1-0.20220309190142-cb37aecb5d28
	github.com/btcsuite/btcwallet/wallet/txauthor => github.com/guggero/btcwallet/wallet/txauthor v1.1.1-0.20220309190142-cb37aecb5d28
)

// If you change this please also update .github/pull_request_template.md and
// docs/INSTALL.md.
go 1.16

retract v0.0.2
