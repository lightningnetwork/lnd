package chanacceptor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// We use field names in our errors for more readable errors. Create
	// consts for them here so that we can exactly match in our unit tests.
	fieldCSV             = "csv delay"
	fieldHtlcLimit       = "htlc limit"
	fieldMinDep          = "min depth"
	fieldReserve         = "reserve"
	fieldMinIn           = "min htlc in"
	fieldInFlightTotal   = "in flight total"
	fieldUpfrontShutdown = "upfront shutdown"
)

var (
	errZeroConf = fmt.Errorf("zero-conf set with non-zero min-depth")
)

// fieldMismatchError returns a merge error for a named field when we get two
// channel acceptor responses which have different values set.
func fieldMismatchError(name string, current, newValue interface{}) error {
	return fmt.Errorf("multiple values set for: %v, %v and %v",
		name, current, newValue)
}

// mergeBool merges two boolean values.
func mergeBool(current, newValue bool) bool {
	// If either is true, return true. It is not possible to have different
	// "non-zero" values like the other cases.
	return current || newValue
}

// mergeInt64 merges two int64 values, failing if they have different non-zero
// values.
func mergeInt64(name string, current, newValue int64) (int64, error) {
	switch {
	case current == 0:
		return newValue, nil

	case newValue == 0:
		return current, nil

	case current != newValue:
		return 0, fieldMismatchError(name, current, newValue)

	default:
		return newValue, nil
	}
}

// mergeMillisatoshi merges two msat values, failing if they have different
// non-zero values.
func mergeMillisatoshi(name string, current,
	newValue lnwire.MilliSatoshi) (lnwire.MilliSatoshi, error) {

	switch {
	case current == 0:
		return newValue, nil

	case newValue == 0:
		return current, nil

	case current != newValue:
		return 0, fieldMismatchError(name, current, newValue)

	default:
		return newValue, nil
	}
}

// mergeDeliveryAddress merges two delivery address values, failing if they have
// different non-zero values.
func mergeDeliveryAddress(name string, current,
	newValue lnwire.DeliveryAddress) (lnwire.DeliveryAddress, error) {

	switch {
	case current == nil:
		return newValue, nil

	case newValue == nil:
		return current, nil

	case !bytes.Equal(current, newValue):
		return nil, fieldMismatchError(name, current, newValue)

	default:
		return newValue, nil
	}
}

// mergeResponse takes two channel accept responses, and attempts to merge their
// fields, failing if any fields conflict (are non-zero and not equal). It
// returns a new response that has all the merged fields in it.
func mergeResponse(current,
	newValue ChannelAcceptResponse) (ChannelAcceptResponse, error) {

	csv, err := mergeInt64(
		fieldCSV, int64(current.CSVDelay), int64(newValue.CSVDelay),
	)
	if err != nil {
		return current, err
	}
	current.CSVDelay = uint16(csv)

	htlcLimit, err := mergeInt64(
		fieldHtlcLimit, int64(current.HtlcLimit),
		int64(newValue.HtlcLimit),
	)
	if err != nil {
		return current, err
	}
	current.HtlcLimit = uint16(htlcLimit)

	minDepth, err := mergeInt64(
		fieldMinDep, int64(current.MinAcceptDepth),
		int64(newValue.MinAcceptDepth),
	)
	if err != nil {
		return current, err
	}
	current.MinAcceptDepth = uint16(minDepth)

	current.ZeroConf = mergeBool(current.ZeroConf, newValue.ZeroConf)

	// Assert that if zero-conf is set, min-depth is zero.
	if current.ZeroConf && current.MinAcceptDepth != 0 {
		return current, errZeroConf
	}

	reserve, err := mergeInt64(
		fieldReserve, int64(current.Reserve), int64(newValue.Reserve),
	)
	if err != nil {
		return current, err
	}
	current.Reserve = btcutil.Amount(reserve)

	current.MinHtlcIn, err = mergeMillisatoshi(
		fieldMinIn, current.MinHtlcIn, newValue.MinHtlcIn,
	)
	if err != nil {
		return current, err
	}

	current.InFlightTotal, err = mergeMillisatoshi(
		fieldInFlightTotal, current.InFlightTotal,
		newValue.InFlightTotal,
	)
	if err != nil {
		return current, err
	}

	current.UpfrontShutdown, err = mergeDeliveryAddress(
		fieldUpfrontShutdown, current.UpfrontShutdown,
		newValue.UpfrontShutdown,
	)
	if err != nil {
		return current, err
	}

	return current, nil
}
