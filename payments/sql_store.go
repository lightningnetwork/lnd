package payments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

const (
	// defaultQueryPaginationLimit is used in the LIMIT clause of the SQL
	// queries to limit the number of rows returned.
	defaultQueryPaginationLimit = 100
)

// SQLPaymentQueries ...
type SQLPaymentQueries interface {
	GetPaymentCreation(ctx context.Context, paymentIdentifier []byte) (sqlc.Payment, error)
	GetPaymentInfo(ctx context.Context, paymentID int64) (sqlc.PaymentInfo, error)
	DeleteHTLCAttempts(ctx context.Context, paymentID int64) error
	InsertPayment(ctx context.Context, arg sqlc.InsertPaymentParams) (int64, error)
	InsertPaymentRequest(ctx context.Context, arg sqlc.InsertPaymentRequestParams) (int64, error)
	InsertTLVRecord(ctx context.Context, arg sqlc.InsertTLVRecordParams) (int64, error)
	InsertFirstHopCustomRecord(ctx context.Context, arg sqlc.InsertFirstHopCustomRecordParams) error
}

type BatchedSQLPaymentQueries interface {
	SQLPaymentQueries

	sqldb.BatchedTx[SQLPaymentQueries]
}

type SQLStoreOptions struct {
	paginationLimit int
}

func defaultSQLStoreOptions() SQLStoreOptions {
	return SQLStoreOptions{
		paginationLimit: defaultQueryPaginationLimit,
	}
}

type SQLStoreOption func(*SQLStoreOptions)

func WithPaginationLimit(limit int) SQLStoreOption {
	return func(o *SQLStoreOptions) {
		o.paginationLimit = limit
	}
}

// SQLStore represents a storage backend.
type SQLStore struct {
	db    BatchedSQLPaymentQueries
	clock clock.Clock
	opts  SQLStoreOptions
}

var _ PaymentDB = (*SQLStore)(nil)

type SQLPaymentQueriesTxOptions struct {
	// readOnly governs if a read only transaction is needed or not.
	readOnly bool
}

// ReadOnly returns true if the transaction should be read only.
//
// NOTE: This implements the TxOptions.
func (a *SQLPaymentQueriesTxOptions) ReadOnly() bool {
	return a.readOnly
}

// NewSQLInvoiceQueryReadTx creates a new read transaction option set.
func NewSQLInvoiceQueryReadTx() SQLPaymentQueriesTxOptions {
	return SQLPaymentQueriesTxOptions{
		readOnly: true,
	}
}

func NewSQLStore(db BatchedSQLPaymentQueries,
	clock clock.Clock, options ...SQLStoreOption) *SQLStore {

	opts := defaultSQLStoreOptions()
	for _, applyOption := range options {
		applyOption(&opts)
	}

	return &SQLStore{
		db:    db,
		clock: clock,
		opts:  opts,
	}
}

// Now implement the PaymentDB interface.

func (p *SQLStore) InitPayment(paymentHash lntypes.Hash,
	info *PaymentCreationInfo) error {

	// There is no need for a sequence number.
	// Need an upsert here because the payment might already exist.

	// Because we allow payment retries we make sure we fetch the payment
	// first, if it exists we make sure we delete all corresponding htlcs
	// data and then insert the new payment data.

	// Or better, we fetch the payment data, if its available we delete all
	// the lingering htlc attemps and also the failed attempts.

	// We should discuss whether we should remove all the htlc attempts if
	// we retry a payment, I think we should not.

	// Can a payment ever fail if it partially succeeded?

	// PaymentIdentifier lntypes.Hash

	// // Value is the amount we are paying.
	// Value lnwire.MilliSatoshi

	// // CreationTime is the time when this payment was initiated.
	// CreationTime time.Time

	// // PaymentRequest is the full payment request, if any.
	// PaymentRequest []byte

	// // FirstHopCustomRecords are the TLV records that are to be sent to the
	// // first hop of this payment. These records will be transmitted via the
	// // wire message only and therefore do not affect the onion payload size.
	// FirstHopCustomRecords lnwire.CustomRecords

	// If the payment already exists we remove all the htlc attempts.

	var writeTxOpts SQLPaymentQueriesTxOptions
	ctx := context.Background()

	err := p.db.ExecTx(ctx, &writeTxOpts, func(db SQLPaymentQueries) error {
		payment, err := db.GetPaymentCreation(ctx, paymentHash[:])
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("unable to get payment: %w", err)
		}

		// We we already have created a payment we make sure
		// we can reinitiate it.

		if !errors.Is(err, sql.ErrNoRows) {
			// We need fetch the payment status.
			paymentInfo, err := db.GetPaymentInfo(ctx, payment.ID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("unable to get payment info: %w", err)
			}

			if PaymentStatus(paymentInfo.PaymentStatus).Initializable() != nil {

				return fmt.Errorf("payment already initialized")
			}

			// Delete htlc attempts and return.
			err = db.DeleteHTLCAttempts(ctx, payment.ID)
			if err != nil {
				return fmt.Errorf("unable to delete htlc attempts: %w", err)
			}

			// TODO(ziggie): Make sure the creation info does not
			// change when retrying a failed payment.

			return nil
		}

		// Insert the new paymentCreation and return here.
		paymentID, err := db.InsertPayment(ctx, sqlc.InsertPaymentParams{
			PaymentIdentifier: info.PaymentIdentifier[:],
			CreatedAt:         info.CreationTime,
			AmountMsat:        int64(info.Value),
		})
		if err != nil {
			return fmt.Errorf("unable to insert payment: %w", err)
		}

		// We only insert the payment request if its available.
		// TODO(ziggie): Make payment request an option.
		if info.PaymentRequest != nil {
			_, err = db.InsertPaymentRequest(ctx, sqlc.InsertPaymentRequestParams{
				PaymentID:      paymentID,
				PaymentRequest: info.PaymentRequest,
			})
			if err != nil {
				return fmt.Errorf("unable to insert payment "+
					"request: %w", err)
			}
		}

		// Also insert the first hop custom records.
		for key, value := range info.FirstHopCustomRecords {
			tlvRecordID, err := db.InsertTLVRecord(ctx, sqlc.InsertTLVRecordParams{
				Key:   int64(key),
				Value: value,
			})
			if err != nil {
				return fmt.Errorf("unable to insert tlv "+
					"record: %w", err)
			}

			db.InsertFirstHopCustomRecord(ctx, sqlc.InsertFirstHopCustomRecordParams{
				TlvRecordID: tlvRecordID,
				PaymentID:   paymentID,
			})
		}

		return nil

		// If we can reattempt the payment we delete all the htlc data
		// before retrying.

	}, func() {})
	if err != nil {
		return fmt.Errorf("unable to initialize payment: %w", err)
	}

	return nil

}

func (p *SQLStore) DeleteFailedAttempts(hash lntypes.Hash) error {
	return nil
}

func (p *SQLStore) RegisterAttempt(paymentHash lntypes.Hash,
	attempt *HTLCAttemptInfo) (*MPPayment, error) {

	return nil, nil
}

func (p *SQLStore) SettleAttempt(hash lntypes.Hash,
	attemptID uint64, settleInfo *HTLCSettleInfo) (*MPPayment, error) {

	return nil, nil
}

func (p *SQLStore) FailAttempt(hash lntypes.Hash,
	attemptID uint64, failInfo *HTLCFailInfo) (*MPPayment, error) {

	return nil, nil
}

func (p *SQLStore) FetchPayment(paymentHash lntypes.Hash) (
	*MPPayment, error) {

	return nil, nil
}

func (p *SQLStore) DeletePayment(paymentHash lntypes.Hash,
	failedHtlcsOnly bool) error {

	return nil
}

func (p *SQLStore) DeletePayments(failedOnly, failedHtlcsOnly bool) error {
	return nil
}

func (p *SQLStore) Fail(paymentHash lntypes.Hash,
	reason FailureReason) (*MPPayment, error) {

	return nil, nil
}

func (p *SQLStore) FetchInFlightPayments() ([]*MPPayment, error) {
	return nil, nil
}

func (p *SQLStore) QueryPayments(ctx context.Context,
	q PaymentsQuery) (PaymentsSlice, error) {

	return PaymentsSlice{}, nil
}
