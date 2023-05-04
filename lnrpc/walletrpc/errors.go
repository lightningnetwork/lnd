package walletrpc

import "errors"

var (

	// ErrZeroLabel is returned when an attempt is made to label a
	// transaction with an empty label.
	ErrZeroLabel = errors.New("cannot label transaction with empty " +
		"label")

	// ErrInsufficientReserve is returned when SendOutputs wouldn't leave
	// enough funds in the wallet to cover for the anchor reserve.
	ErrInsufficientReserve = errors.New("the outputs to be sent " +
		"would leave insufficient reserves for anchor channels in " +
		"the wallet")
)
