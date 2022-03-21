module github.com/lightningnetwork/lnd/healthcheck

go 1.16

require (
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/lightningnetwork/lnd/ticker v1.1.0
	github.com/lightningnetwork/lnd/tor v1.0.0
	github.com/stretchr/testify v1.7.0
	golang.org/x/sys v0.0.0-20211216021012-1d35b9e2eb4e
)

// TODO(guggero): Remove these after merging #6350 and pushing the new tag!
replace github.com/lightningnetwork/lnd/tor => ../tor
