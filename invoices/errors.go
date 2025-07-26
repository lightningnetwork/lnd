package invoices

import (
	"errors"
	"fmt"
)

var (
	// ErrInvoiceAlreadySettled is returned when the invoice is already
	// settled.
	ErrInvoiceAlreadySettled = errors.New("invoice already settled")

	// ErrInvoiceAlreadyCanceled is returned when the invoice is already
	// canceled.
	ErrInvoiceAlreadyCanceled = errors.New("invoice already canceled")

	// ErrInvoiceNotCanceled is returned when the invoice is not canceled.
	ErrInvoiceNotCanceled = errors.New("invoice not canceled")

	// ErrInvoiceAlreadyAccepted is returned when the invoice is already
	// accepted.
	ErrInvoiceAlreadyAccepted = errors.New("invoice already accepted")

	// ErrInvoiceStillOpen is returned when the invoice is still open.
	ErrInvoiceStillOpen = errors.New("invoice still open")

	// ErrInvoiceCannotOpen is returned when an attempt is made to move an
	// invoice to the open state.
	ErrInvoiceCannotOpen = errors.New("cannot move invoice to open")

	// ErrInvoiceCannotAccept is returned when an attempt is made to accept
	// an invoice while the invoice is not in the open state.
	ErrInvoiceCannotAccept = errors.New("cannot accept invoice")

	// ErrInvoicePreimageMismatch is returned when the preimage doesn't
	// match the invoice hash.
	ErrInvoicePreimageMismatch = errors.New("preimage does not match")

	// ErrNoInvoiceHash is returned when an invoice hash is expected, and
	// none is provided.
	ErrNoInvoiceHash = errors.New("invoice hash must be provided")

	// ErrHTLCPreimageMissing is returned when trying to accept/settle an
	// AMP HTLC but the HTLC-level preimage has not been set.
	ErrHTLCPreimageMissing = errors.New("AMP htlc missing preimage")

	// ErrHTLCPreimageMismatch is returned when trying to accept/settle an
	// AMP HTLC but the HTLC-level preimage does not satisfying the
	// HTLC-level payment hash.
	ErrHTLCPreimageMismatch = errors.New("htlc preimage mismatch")

	// ErrHTLCAlreadySettled is returned when trying to settle an invoice
	// but HTLC already exists in the settled state.
	ErrHTLCAlreadySettled = errors.New("htlc already settled")

	// ErrInvoiceHasHtlcs is returned when attempting to insert an invoice
	// that already has HTLCs.
	ErrInvoiceHasHtlcs = errors.New("cannot add invoice with htlcs")

	// ErrEmptyHTLCSet is returned when attempting to accept or settle and
	// HTLC set that has no HTLCs.
	ErrEmptyHTLCSet = errors.New("cannot settle/accept empty HTLC set")

	// ErrUnexpectedInvoicePreimage is returned when an invoice-level
	// preimage is provided when trying to settle an invoice that shouldn't
	// have one, e.g. an AMP invoice.
	ErrUnexpectedInvoicePreimage = errors.New(
		"unexpected invoice preimage provided on settle",
	)

	// ErrHTLCPreimageAlreadyExists is returned when trying to set an
	// htlc-level preimage but one is already known.
	ErrHTLCPreimageAlreadyExists = errors.New(
		"htlc-level preimage already exists",
	)

	// ErrInvoiceNotFound is returned when a targeted invoice can't be
	// found.
	ErrInvoiceNotFound = errors.New("unable to locate invoice")

	// ErrNoInvoicesCreated is returned when we don't have invoices in
	// our database to return.
	ErrNoInvoicesCreated = errors.New("there are no existing invoices")

	// ErrDuplicateInvoice is returned when an invoice with the target
	// payment hash already exists.
	ErrDuplicateInvoice = errors.New(
		"invoice with payment hash already exists",
	)

	// ErrDuplicatePayAddr is returned when an invoice with the target
	// payment addr already exists.
	ErrDuplicatePayAddr = errors.New(
		"invoice with payemnt addr already exists",
	)

	// ErrInvRefEquivocation is returned when an InvoiceRef targets
	// multiple, distinct invoices.
	ErrInvRefEquivocation = errors.New("inv ref matches multiple invoices")

	// ErrNoPaymentsCreated is returned when bucket of payments hasn't been
	// created.
	ErrNoPaymentsCreated = errors.New("there are no existing payments")
)

// ErrDuplicateSetID is an error returned when attempting to adding an AMP HTLC
// to an invoice, but another invoice is already indexed by the same set id.
type ErrDuplicateSetID struct {
	SetID [32]byte
}

// Error returns a human-readable description of ErrDuplicateSetID.
func (e ErrDuplicateSetID) Error() string {
	return fmt.Sprintf("invoice with set_id=%x already exists", e.SetID)
}
