module github.com/lightningnetwork/lnd/cmd/lndinit

require (
	github.com/btcsuite/btcwallet/walletdb v1.3.5
	github.com/gogo/protobuf v1.3.1 // indirect
	github.com/jessevdk/go-flags v1.4.0
	github.com/kkdai/bstream v1.0.0 // indirect
	github.com/lightningnetwork/lnd v0.13.0-beta
	github.com/lightningnetwork/lnd/kvdb v1.0.0
	github.com/onsi/ginkgo v1.11.0 // indirect
	github.com/onsi/gomega v1.7.0 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	golang.org/x/time v0.0.0-20200416051211-89c76fbcd5d1 // indirect
	sigs.k8s.io/yaml v1.2.0 // indirect
)

replace github.com/lightningnetwork/lnd => ../../

replace github.com/lightningnetwork/lnd/kvdb => ../../kvdb

// Fix incompatibility of etcd go.mod package.
// See https://github.com/etcd-io/etcd/issues/11154
replace go.etcd.io/etcd => go.etcd.io/etcd v0.5.0-alpha.5.0.20201125193152-8a03d2e9614b

go 1.15
