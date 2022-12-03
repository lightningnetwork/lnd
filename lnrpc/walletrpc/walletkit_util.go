package walletrpc

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// AccountsToWatchOnly converts the accounts returned by the walletkit's
// ListAccounts RPC into a struct that can be used to create a watch-only
// wallet.
func AccountsToWatchOnly(exported []*Account) ([]*lnrpc.WatchOnlyAccount,
	error) {

	result := make([]*lnrpc.WatchOnlyAccount, len(exported))
	for idx, acct := range exported {
		parsedPath, err := parseDerivationPath(acct.DerivationPath)
		if err != nil {
			return nil, fmt.Errorf("error parsing derivation path "+
				"of account %d: %v", idx, err)
		}
		if len(parsedPath) < 3 {
			return nil, fmt.Errorf("derivation path of account %d "+
				"has invalid derivation path, need at least "+
				"path of depth 3, instead has depth %d", idx,
				len(parsedPath))
		}

		result[idx] = &lnrpc.WatchOnlyAccount{
			Purpose:  parsedPath[0],
			CoinType: parsedPath[1],
			Account:  parsedPath[2],
			Xpub:     acct.ExtendedPublicKey,
		}
	}

	return result, nil
}

// parseDerivationPath parses a path in the form of m/x'/y'/z'/a/b into a slice
// of [x, y, z, a, b], meaning that the apostrophe is ignored and 2^31 is _not_
// added to the numbers.
func parseDerivationPath(path string) ([]uint32, error) {
	path = strings.TrimSpace(path)
	if len(path) == 0 {
		return nil, fmt.Errorf("path cannot be empty")
	}
	if !strings.HasPrefix(path, "m/") {
		return nil, fmt.Errorf("path must start with m/")
	}

	// Just the root key, no path was provided. This is valid but not useful
	// in most cases.
	rest := strings.ReplaceAll(path, "m/", "")
	if rest == "" {
		return []uint32{}, nil
	}

	parts := strings.Split(rest, "/")
	indices := make([]uint32, len(parts))
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if strings.Contains(parts[i], "'") {
			part = strings.TrimRight(parts[i], "'")
		}
		parsed, err := strconv.ParseInt(part, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("could not parse part \"%s\": "+
				"%v", part, err)
		}
		indices[i] = uint32(parsed)
	}
	return indices, nil
}

// doubleHashMessage creates the double hash (sha256) of a message
// prepended with a specified prefix.
func doubleHashMessage(prefix string, msg string) ([]byte, error) {
	var buf bytes.Buffer
	err := wire.WriteVarString(&buf, 0, prefix)
	if err != nil {
		return nil, err
	}

	err = wire.WriteVarString(&buf, 0, msg)
	if err != nil {
		return nil, err
	}

	digest := chainhash.DoubleHashB(buf.Bytes())

	return digest, nil
}
