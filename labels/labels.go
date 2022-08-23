// Package labels contains labels used to label transactions broadcast by lnd.
// These labels are used across packages, so they are declared in a separate
// package to avoid dependency issues.
//
// Labels for transactions broadcast by lnd have two set fields followed by an
// optional set labelled data values, all separated by colons.
//   - Label version: an integer that indicates the version lnd used
//   - Label type: the type of transaction we are labelling
//   - {field name}-{value}: a named field followed by its value, these items are
//     optional, and there may be more than field present.
//
// For version 0 we have the following optional data fields defined:
//   - shortchanid: the short channel ID that a transaction is associated with,
//     with its value set to the uint64 short channel id.
package labels

import (
	"fmt"

	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/lnwire"
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

// LabelVersion versions our labels so they can be easily update to contain
// new data while still easily string matched.
type LabelVersion uint8

// LabelVersionZero is the label version for labels that contain label type and
// channel ID (where available).
const LabelVersionZero LabelVersion = iota

// LabelType indicates the type of label we are creating. It is a string rather
// than an int for easy string matching and human-readability.
type LabelType string

const (
	// LabelTypeChannelOpen is used to label channel opens.
	LabelTypeChannelOpen LabelType = "openchannel"

	// LabelTypeChannelClose is used to label channel closes.
	LabelTypeChannelClose LabelType = "closechannel"

	// LabelTypeJusticeTransaction is used to label justice transactions.
	LabelTypeJusticeTransaction LabelType = "justicetx"

	// LabelTypeSweepTransaction is used to label sweeps.
	LabelTypeSweepTransaction LabelType = "sweep"
)

// LabelField is used to tag a value within a label.
type LabelField string

const (
	// ShortChanID is used to tag short channel id values in our labels.
	ShortChanID LabelField = "shortchanid"
)

// MakeLabel creates a label with the provided type and short channel id. If
// our short channel ID is not known, we simply return version:label_type. If
// we do have a short channel ID set, the label will also contain its value:
// shortchanid-{int64 chan ID}.
func MakeLabel(labelType LabelType, channelID *lnwire.ShortChannelID) string {
	if channelID == nil {
		return fmt.Sprintf("%v:%v", LabelVersionZero, labelType)
	}

	return fmt.Sprintf("%v:%v:%v-%v", LabelVersionZero, labelType,
		ShortChanID, channelID.ToUint64())
}
