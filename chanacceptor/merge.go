package chanacceptor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcutil"
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

// fieldMismatchError returns a merge error for a named field when we get two
// channel acceptor responses which have different values set.
func fieldMismatchError(name string, current, new interface{}) error {
	return fmt.Errorf("multiple values set for: %v, %v and %v",
		name, current, new)
}

// mergeInt64 merges two int64 values, failing if they have different non-zero
// values.
func mergeInt64(name string, current, new int64) (int64, error) {
	switch {
	case current == 0:
		return new, nil

	case new == 0:
		return current, nil

	case current != new:
		return 0, fieldMismatchError(name, current, new)

	default:
		return new, nil
	}
}

// mergeMillisatoshi merges two msat values, failing if they have different
// non-zero values.
func mergeMillisatoshi(name string, current,
	new lnwire.MilliSatoshi) (lnwire.MilliSatoshi, error) {

	switch {
	case current == 0:
		return new, nil

	case new == 0:
		return current, nil

	case current != new:
		return 0, fieldMismatchError(name, current, new)

	default:
		return new, nil
	}
}

// mergeDeliveryAddress merges two delivery address values, failing if they have
// different non-zero values.
func mergeDeliveryAddress(name string, current,
	new lnwire.DeliveryAddress) (lnwire.DeliveryAddress, error) {

	switch {
	case current == nil:
		return new, nil

	case new == nil:
		return current, nil

	case !bytes.Equal(current, new):
		return nil, fieldMismatchError(name, current, new)

	default:
		return new, nil
	}
}

// mergeResponse takes two channel accept responses, and attempts to merge their
// fields, failing if any fields conflict (are non-zero and not equal). It
// returns a new response that has all the merged fields in it.
func mergeResponse(current, new ChannelAcceptResponse) (ChannelAcceptResponse,
	error) {

	csv, err := mergeInt64(
		fieldCSV, int64(current.CSVDelay), int64(new.CSVDelay),
	)
	if err != nil {
		return current, err
	}
	current.CSVDelay = uint16(csv)

	htlcLimit, err := mergeInt64(
		fieldHtlcLimit, int64(current.HtlcLimit),
		int64(new.HtlcLimit),
	)
	if err != nil {
		return current, err
	}
	current.HtlcLimit = uint16(htlcLimit)

	minDepth, err := mergeInt64(
		fieldMinDep, int64(current.MinAcceptDepth),
		int64(new.MinAcceptDepth),
	)
	if err != nil {
		return current, err
	}
	current.MinAcceptDepth = uint16(minDepth)

	reserve, err := mergeInt64(
		fieldReserve, int64(current.Reserve), int64(new.Reserve),
	)
	if err != nil {
		return current, err
	}
	current.Reserve = btcutil.Amount(reserve)

	current.MinHtlcIn, err = mergeMillisatoshi(
		fieldMinIn, current.MinHtlcIn, new.MinHtlcIn,
	)
	if err != nil {
		return current, err
	}

	current.InFlightTotal, err = mergeMillisatoshi(
		fieldInFlightTotal, current.InFlightTotal,
		new.InFlightTotal,
	)
	if err != nil {
		return current, err
	}

	current.UpfrontShutdown, err = mergeDeliveryAddress(
		fieldUpfrontShutdown, current.UpfrontShutdown,
		new.UpfrontShutdown,
	)
	if err != nil {
		return current, err
	}

	return current, nil
}
