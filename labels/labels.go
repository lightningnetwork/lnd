// Package labels contains labels used to label transactions broadcast by lnd.
// These labels are used across packages, so they are declared in a separate
// package to avoid dependency issues.
package labels

import (
	"fmt"

	"github.com/btcsuite/btcwallet/wtxmgr"
)

// External labels a transaction as user initiated via the api. This
// label is only used when a custom user provided label is not given.
const External = "external"

// ValidateAPI returns the generic api label if the label provided is empty.
// This allows us to label all transactions published by the api, even if
// no label is provided. If a label is provided, it is validated against
// the known restrictions.
func ValidateAPI(label string) (string, error) {
	if len(label) > wtxmgr.TxLabelLimit {
		return "", fmt.Errorf("label length: %v exceeds "+
			"limit of %v", len(label), wtxmgr.TxLabelLimit)
	}

	// If no label was provided by the user, add the generic user
	// send label.
	if len(label) == 0 {
		return External, nil
	}

	return label, nil
}
