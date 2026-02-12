package sqlc

import (
	"fmt"
	"strings"
)

// GetTx returns the underlying DBTX (either *sql.DB or *sql.Tx) used by the
// Queries struct.
func (q *Queries) GetTx() DBTX {
	return q.db
}

// makeQueryParams generates a string of query parameters for a SQL query. It is
// meant to replace the `?` placeholders in a SQL query with numbered parameters
// like `$1`, `$2`, etc. This is required for the sqlc /*SLICE:<field_name>*/
// workaround. See scripts/gen_sqlc_docker.sh for more details.
func makeQueryParams(numTotalArgs, numListArgs int) string {
	if numListArgs == 0 {
		return ""
	}

	var b strings.Builder

	// Pre-allocate a rough estimation of the buffer size to avoid
	// re-allocations. A parameter like $1000, takes 6 bytes.
	b.Grow(numListArgs * 6)

	diff := numTotalArgs - numListArgs
	for i := 0; i < numListArgs; i++ {
		if i > 0 {
			// We don't need to check the error here because the
			// WriteString method of strings.Builder always returns
			// nil.
			_, _ = b.WriteString(",")
		}

		// We don't need to check the error here because the
		// Write method (called by fmt.Fprintf) of strings.Builder
		// always returns nil.
		_, _ = fmt.Fprintf(&b, "$%d", i+diff+1)
	}

	return b.String()
}

// PaymentAndIntent is an interface that provides access to a payment and its
// associated payment intent.
type PaymentAndIntent interface {
	// GetPayment returns the Payment associated with this interface.
	GetPayment() Payment

	// GetPaymentIntent returns the PaymentIntent associated with this
	// payment.
	GetPaymentIntent() PaymentIntent
}

// GetPayment returns the Payment associated with this interface.
//
// NOTE: This method is part of the PaymentAndIntent interface.
func (r FilterPaymentsRow) GetPayment() Payment {
	return r.Payment
}

// GetPaymentIntent returns the PaymentIntent associated with this payment.
// If the payment has no intent (IntentType is NULL), this returns a zero-value
// PaymentIntent.
//
// NOTE: This method is part of the PaymentAndIntent interface.
func (r FilterPaymentsRow) GetPaymentIntent() PaymentIntent {
	if !r.IntentType.Valid {
		return PaymentIntent{}
	}

	return PaymentIntent{
		IntentType:    r.IntentType.Int16,
		IntentPayload: r.IntentPayload,
	}
}

// GetPayment returns the Payment associated with this interface.
//
// NOTE: This method is part of the PaymentAndIntent interface.
func (r FetchPaymentRow) GetPayment() Payment {
	return r.Payment
}

// GetPaymentIntent returns the PaymentIntent associated with this payment.
// If the payment has no intent (IntentType is NULL), this returns a zero-value
// PaymentIntent.
//
// NOTE: This method is part of the PaymentAndIntent interface.
func (r FetchPaymentRow) GetPaymentIntent() PaymentIntent {
	if !r.IntentType.Valid {
		return PaymentIntent{}
	}

	return PaymentIntent{
		IntentType:    r.IntentType.Int16,
		IntentPayload: r.IntentPayload,
	}
}

func (r FetchPaymentsByIDsRow) GetPayment() Payment {
	return Payment{
		ID:                r.ID,
		AmountMsat:        r.AmountMsat,
		CreatedAt:         r.CreatedAt,
		PaymentIdentifier: r.PaymentIdentifier,
		FailReason:        r.FailReason,
	}
}

func (r FetchPaymentsByIDsRow) GetPaymentIntent() PaymentIntent {
	if !r.IntentType.Valid {
		return PaymentIntent{}
	}

	return PaymentIntent{
		IntentType:    r.IntentType.Int16,
		IntentPayload: r.IntentPayload,
	}
}
