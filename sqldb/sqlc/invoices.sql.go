// Code generated by sqlc. DO NOT EDIT.
// versions:
//   sqlc v1.29.0
// source: invoices.sql

package sqlc

import (
	"context"
	"database/sql"
	"time"
)

const clearKVInvoiceHashIndex = `-- name: ClearKVInvoiceHashIndex :exec
DELETE FROM invoice_payment_hashes
`

func (q *Queries) ClearKVInvoiceHashIndex(ctx context.Context) error {
	_, err := q.db.ExecContext(ctx, clearKVInvoiceHashIndex)
	return err
}

const deleteCanceledInvoices = `-- name: DeleteCanceledInvoices :execresult
DELETE
FROM invoices
WHERE state = 2
`

func (q *Queries) DeleteCanceledInvoices(ctx context.Context) (sql.Result, error) {
	return q.db.ExecContext(ctx, deleteCanceledInvoices)
}

const deleteInvoice = `-- name: DeleteInvoice :execresult
DELETE 
FROM invoices 
WHERE (
    id = $1 OR 
    $1 IS NULL
) AND (
    hash = $2 OR 
    $2 IS NULL
) AND (
    settle_index = $3 OR 
    $3 IS NULL
) AND (
    payment_addr = $4 OR
    $4 IS NULL
)
`

type DeleteInvoiceParams struct {
	AddIndex    sql.NullInt64
	Hash        []byte
	SettleIndex sql.NullInt64
	PaymentAddr []byte
}

func (q *Queries) DeleteInvoice(ctx context.Context, arg DeleteInvoiceParams) (sql.Result, error) {
	return q.db.ExecContext(ctx, deleteInvoice,
		arg.AddIndex,
		arg.Hash,
		arg.SettleIndex,
		arg.PaymentAddr,
	)
}

const filterInvoices = `-- name: FilterInvoices :many
SELECT
    invoices.id, invoices.hash, invoices.preimage, invoices.settle_index, invoices.settled_at, invoices.memo, invoices.amount_msat, invoices.cltv_delta, invoices.expiry, invoices.payment_addr, invoices.payment_request, invoices.payment_request_hash, invoices.state, invoices.amount_paid_msat, invoices.is_amp, invoices.is_hodl, invoices.is_keysend, invoices.created_at
FROM invoices
WHERE (
    id >= $1 OR 
    $1 IS NULL
) AND (
    id <= $2 OR 
    $2 IS NULL
) AND (
    settle_index >= $3 OR
    $3 IS NULL
) AND (
    settle_index <= $4 OR
    $4 IS NULL
) AND (
    state = $5 OR 
    $5 IS NULL
) AND (
    created_at >= $6 OR
    $6 IS NULL
) AND (
    created_at < $7 OR 
    $7 IS NULL
) AND (
    CASE
        WHEN $8 = TRUE THEN (state = 0 OR state = 3)
        ELSE TRUE 
    END
)
ORDER BY
CASE
    WHEN $9 = FALSE OR $9 IS NULL THEN id
    ELSE NULL
    END ASC,
CASE
    WHEN $9 = TRUE THEN id
    ELSE NULL
END DESC
LIMIT $11 OFFSET $10
`

type FilterInvoicesParams struct {
	AddIndexGet    sql.NullInt64
	AddIndexLet    sql.NullInt64
	SettleIndexGet sql.NullInt64
	SettleIndexLet sql.NullInt64
	State          sql.NullInt16
	CreatedAfter   sql.NullTime
	CreatedBefore  sql.NullTime
	PendingOnly    interface{}
	Reverse        interface{}
	NumOffset      int32
	NumLimit       int32
}

func (q *Queries) FilterInvoices(ctx context.Context, arg FilterInvoicesParams) ([]Invoice, error) {
	rows, err := q.db.QueryContext(ctx, filterInvoices,
		arg.AddIndexGet,
		arg.AddIndexLet,
		arg.SettleIndexGet,
		arg.SettleIndexLet,
		arg.State,
		arg.CreatedAfter,
		arg.CreatedBefore,
		arg.PendingOnly,
		arg.Reverse,
		arg.NumOffset,
		arg.NumLimit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Invoice
	for rows.Next() {
		var i Invoice
		if err := rows.Scan(
			&i.ID,
			&i.Hash,
			&i.Preimage,
			&i.SettleIndex,
			&i.SettledAt,
			&i.Memo,
			&i.AmountMsat,
			&i.CltvDelta,
			&i.Expiry,
			&i.PaymentAddr,
			&i.PaymentRequest,
			&i.PaymentRequestHash,
			&i.State,
			&i.AmountPaidMsat,
			&i.IsAmp,
			&i.IsHodl,
			&i.IsKeysend,
			&i.CreatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const getInvoice = `-- name: GetInvoice :many

SELECT i.id, i.hash, i.preimage, i.settle_index, i.settled_at, i.memo, i.amount_msat, i.cltv_delta, i.expiry, i.payment_addr, i.payment_request, i.payment_request_hash, i.state, i.amount_paid_msat, i.is_amp, i.is_hodl, i.is_keysend, i.created_at
FROM invoices i
LEFT JOIN amp_sub_invoices a 
ON i.id = a.invoice_id
AND (
    a.set_id = $1 OR $1 IS NULL
)
WHERE (
    i.id = $2 OR 
    $2 IS NULL
) AND (
    i.hash = $3 OR 
    $3 IS NULL
) AND (
    i.payment_addr = $4 OR 
    $4 IS NULL
)
GROUP BY i.id
LIMIT 2
`

type GetInvoiceParams struct {
	SetID       []byte
	AddIndex    sql.NullInt64
	Hash        []byte
	PaymentAddr []byte
}

// This method may return more than one invoice if filter using multiple fields
// from different invoices. It is the caller's responsibility to ensure that
// we bubble up an error in those cases.
func (q *Queries) GetInvoice(ctx context.Context, arg GetInvoiceParams) ([]Invoice, error) {
	rows, err := q.db.QueryContext(ctx, getInvoice,
		arg.SetID,
		arg.AddIndex,
		arg.Hash,
		arg.PaymentAddr,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Invoice
	for rows.Next() {
		var i Invoice
		if err := rows.Scan(
			&i.ID,
			&i.Hash,
			&i.Preimage,
			&i.SettleIndex,
			&i.SettledAt,
			&i.Memo,
			&i.AmountMsat,
			&i.CltvDelta,
			&i.Expiry,
			&i.PaymentAddr,
			&i.PaymentRequest,
			&i.PaymentRequestHash,
			&i.State,
			&i.AmountPaidMsat,
			&i.IsAmp,
			&i.IsHodl,
			&i.IsKeysend,
			&i.CreatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const getInvoiceByHash = `-- name: GetInvoiceByHash :one
SELECT i.id, i.hash, i.preimage, i.settle_index, i.settled_at, i.memo, i.amount_msat, i.cltv_delta, i.expiry, i.payment_addr, i.payment_request, i.payment_request_hash, i.state, i.amount_paid_msat, i.is_amp, i.is_hodl, i.is_keysend, i.created_at
FROM invoices i
WHERE i.hash = $1
`

func (q *Queries) GetInvoiceByHash(ctx context.Context, hash []byte) (Invoice, error) {
	row := q.db.QueryRowContext(ctx, getInvoiceByHash, hash)
	var i Invoice
	err := row.Scan(
		&i.ID,
		&i.Hash,
		&i.Preimage,
		&i.SettleIndex,
		&i.SettledAt,
		&i.Memo,
		&i.AmountMsat,
		&i.CltvDelta,
		&i.Expiry,
		&i.PaymentAddr,
		&i.PaymentRequest,
		&i.PaymentRequestHash,
		&i.State,
		&i.AmountPaidMsat,
		&i.IsAmp,
		&i.IsHodl,
		&i.IsKeysend,
		&i.CreatedAt,
	)
	return i, err
}

const getInvoiceBySetID = `-- name: GetInvoiceBySetID :many
SELECT i.id, i.hash, i.preimage, i.settle_index, i.settled_at, i.memo, i.amount_msat, i.cltv_delta, i.expiry, i.payment_addr, i.payment_request, i.payment_request_hash, i.state, i.amount_paid_msat, i.is_amp, i.is_hodl, i.is_keysend, i.created_at
FROM invoices i
INNER JOIN amp_sub_invoices a 
ON i.id = a.invoice_id AND a.set_id = $1
`

func (q *Queries) GetInvoiceBySetID(ctx context.Context, setID []byte) ([]Invoice, error) {
	rows, err := q.db.QueryContext(ctx, getInvoiceBySetID, setID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Invoice
	for rows.Next() {
		var i Invoice
		if err := rows.Scan(
			&i.ID,
			&i.Hash,
			&i.Preimage,
			&i.SettleIndex,
			&i.SettledAt,
			&i.Memo,
			&i.AmountMsat,
			&i.CltvDelta,
			&i.Expiry,
			&i.PaymentAddr,
			&i.PaymentRequest,
			&i.PaymentRequestHash,
			&i.State,
			&i.AmountPaidMsat,
			&i.IsAmp,
			&i.IsHodl,
			&i.IsKeysend,
			&i.CreatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const getInvoiceFeatures = `-- name: GetInvoiceFeatures :many
SELECT feature, invoice_id
FROM invoice_features
WHERE invoice_id = $1
`

func (q *Queries) GetInvoiceFeatures(ctx context.Context, invoiceID int64) ([]InvoiceFeature, error) {
	rows, err := q.db.QueryContext(ctx, getInvoiceFeatures, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []InvoiceFeature
	for rows.Next() {
		var i InvoiceFeature
		if err := rows.Scan(&i.Feature, &i.InvoiceID); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const getInvoiceHTLCCustomRecords = `-- name: GetInvoiceHTLCCustomRecords :many
SELECT ihcr.htlc_id, key, value
FROM invoice_htlcs ih JOIN invoice_htlc_custom_records ihcr ON ih.id=ihcr.htlc_id 
WHERE ih.invoice_id = $1
`

type GetInvoiceHTLCCustomRecordsRow struct {
	HtlcID int64
	Key    int64
	Value  []byte
}

func (q *Queries) GetInvoiceHTLCCustomRecords(ctx context.Context, invoiceID int64) ([]GetInvoiceHTLCCustomRecordsRow, error) {
	rows, err := q.db.QueryContext(ctx, getInvoiceHTLCCustomRecords, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []GetInvoiceHTLCCustomRecordsRow
	for rows.Next() {
		var i GetInvoiceHTLCCustomRecordsRow
		if err := rows.Scan(&i.HtlcID, &i.Key, &i.Value); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const getInvoiceHTLCs = `-- name: GetInvoiceHTLCs :many
SELECT id, chan_id, htlc_id, amount_msat, total_mpp_msat, accept_height, accept_time, expiry_height, state, resolve_time, invoice_id
FROM invoice_htlcs
WHERE invoice_id = $1
`

func (q *Queries) GetInvoiceHTLCs(ctx context.Context, invoiceID int64) ([]InvoiceHtlc, error) {
	rows, err := q.db.QueryContext(ctx, getInvoiceHTLCs, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []InvoiceHtlc
	for rows.Next() {
		var i InvoiceHtlc
		if err := rows.Scan(
			&i.ID,
			&i.ChanID,
			&i.HtlcID,
			&i.AmountMsat,
			&i.TotalMppMsat,
			&i.AcceptHeight,
			&i.AcceptTime,
			&i.ExpiryHeight,
			&i.State,
			&i.ResolveTime,
			&i.InvoiceID,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const getKVInvoicePaymentHashByAddIndex = `-- name: GetKVInvoicePaymentHashByAddIndex :one
SELECT hash
FROM invoice_payment_hashes
WHERE add_index = $1
`

func (q *Queries) GetKVInvoicePaymentHashByAddIndex(ctx context.Context, addIndex int64) ([]byte, error) {
	row := q.db.QueryRowContext(ctx, getKVInvoicePaymentHashByAddIndex, addIndex)
	var hash []byte
	err := row.Scan(&hash)
	return hash, err
}

const insertInvoice = `-- name: InsertInvoice :one
INSERT INTO invoices (
    hash, preimage, memo, amount_msat, cltv_delta, expiry, payment_addr, 
    payment_request, payment_request_hash, state, amount_paid_msat, is_amp,
    is_hodl, is_keysend, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
) RETURNING id
`

type InsertInvoiceParams struct {
	Hash               []byte
	Preimage           []byte
	Memo               sql.NullString
	AmountMsat         int64
	CltvDelta          sql.NullInt32
	Expiry             int32
	PaymentAddr        []byte
	PaymentRequest     sql.NullString
	PaymentRequestHash []byte
	State              int16
	AmountPaidMsat     int64
	IsAmp              bool
	IsHodl             bool
	IsKeysend          bool
	CreatedAt          time.Time
}

func (q *Queries) InsertInvoice(ctx context.Context, arg InsertInvoiceParams) (int64, error) {
	row := q.db.QueryRowContext(ctx, insertInvoice,
		arg.Hash,
		arg.Preimage,
		arg.Memo,
		arg.AmountMsat,
		arg.CltvDelta,
		arg.Expiry,
		arg.PaymentAddr,
		arg.PaymentRequest,
		arg.PaymentRequestHash,
		arg.State,
		arg.AmountPaidMsat,
		arg.IsAmp,
		arg.IsHodl,
		arg.IsKeysend,
		arg.CreatedAt,
	)
	var id int64
	err := row.Scan(&id)
	return id, err
}

const insertInvoiceFeature = `-- name: InsertInvoiceFeature :exec
INSERT INTO invoice_features (
    invoice_id, feature
) VALUES (
    $1, $2
)
`

type InsertInvoiceFeatureParams struct {
	InvoiceID int64
	Feature   int32
}

func (q *Queries) InsertInvoiceFeature(ctx context.Context, arg InsertInvoiceFeatureParams) error {
	_, err := q.db.ExecContext(ctx, insertInvoiceFeature, arg.InvoiceID, arg.Feature)
	return err
}

const insertInvoiceHTLC = `-- name: InsertInvoiceHTLC :one
INSERT INTO invoice_htlcs (
    htlc_id, chan_id, amount_msat, total_mpp_msat, accept_height, accept_time,
    expiry_height, state, resolve_time, invoice_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
) RETURNING id
`

type InsertInvoiceHTLCParams struct {
	HtlcID       int64
	ChanID       string
	AmountMsat   int64
	TotalMppMsat sql.NullInt64
	AcceptHeight int32
	AcceptTime   time.Time
	ExpiryHeight int32
	State        int16
	ResolveTime  sql.NullTime
	InvoiceID    int64
}

func (q *Queries) InsertInvoiceHTLC(ctx context.Context, arg InsertInvoiceHTLCParams) (int64, error) {
	row := q.db.QueryRowContext(ctx, insertInvoiceHTLC,
		arg.HtlcID,
		arg.ChanID,
		arg.AmountMsat,
		arg.TotalMppMsat,
		arg.AcceptHeight,
		arg.AcceptTime,
		arg.ExpiryHeight,
		arg.State,
		arg.ResolveTime,
		arg.InvoiceID,
	)
	var id int64
	err := row.Scan(&id)
	return id, err
}

const insertInvoiceHTLCCustomRecord = `-- name: InsertInvoiceHTLCCustomRecord :exec
INSERT INTO invoice_htlc_custom_records (
    key, value, htlc_id 
) VALUES (
    $1, $2, $3
)
`

type InsertInvoiceHTLCCustomRecordParams struct {
	Key    int64
	Value  []byte
	HtlcID int64
}

func (q *Queries) InsertInvoiceHTLCCustomRecord(ctx context.Context, arg InsertInvoiceHTLCCustomRecordParams) error {
	_, err := q.db.ExecContext(ctx, insertInvoiceHTLCCustomRecord, arg.Key, arg.Value, arg.HtlcID)
	return err
}

const insertKVInvoiceKeyAndAddIndex = `-- name: InsertKVInvoiceKeyAndAddIndex :exec
INSERT INTO invoice_payment_hashes (
    id, add_index
) VALUES (
    $1, $2
)
`

type InsertKVInvoiceKeyAndAddIndexParams struct {
	ID       int64
	AddIndex int64
}

func (q *Queries) InsertKVInvoiceKeyAndAddIndex(ctx context.Context, arg InsertKVInvoiceKeyAndAddIndexParams) error {
	_, err := q.db.ExecContext(ctx, insertKVInvoiceKeyAndAddIndex, arg.ID, arg.AddIndex)
	return err
}

const insertMigratedInvoice = `-- name: InsertMigratedInvoice :one
INSERT INTO invoices (
    hash, preimage, settle_index, settled_at, memo, amount_msat, cltv_delta, 
    expiry, payment_addr, payment_request, payment_request_hash, state, 
    amount_paid_msat, is_amp, is_hodl, is_keysend, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
) RETURNING id
`

type InsertMigratedInvoiceParams struct {
	Hash               []byte
	Preimage           []byte
	SettleIndex        sql.NullInt64
	SettledAt          sql.NullTime
	Memo               sql.NullString
	AmountMsat         int64
	CltvDelta          sql.NullInt32
	Expiry             int32
	PaymentAddr        []byte
	PaymentRequest     sql.NullString
	PaymentRequestHash []byte
	State              int16
	AmountPaidMsat     int64
	IsAmp              bool
	IsHodl             bool
	IsKeysend          bool
	CreatedAt          time.Time
}

func (q *Queries) InsertMigratedInvoice(ctx context.Context, arg InsertMigratedInvoiceParams) (int64, error) {
	row := q.db.QueryRowContext(ctx, insertMigratedInvoice,
		arg.Hash,
		arg.Preimage,
		arg.SettleIndex,
		arg.SettledAt,
		arg.Memo,
		arg.AmountMsat,
		arg.CltvDelta,
		arg.Expiry,
		arg.PaymentAddr,
		arg.PaymentRequest,
		arg.PaymentRequestHash,
		arg.State,
		arg.AmountPaidMsat,
		arg.IsAmp,
		arg.IsHodl,
		arg.IsKeysend,
		arg.CreatedAt,
	)
	var id int64
	err := row.Scan(&id)
	return id, err
}

const nextInvoiceSettleIndex = `-- name: NextInvoiceSettleIndex :one
UPDATE invoice_sequences SET current_value = current_value + 1
WHERE name = 'settle_index'
RETURNING current_value
`

func (q *Queries) NextInvoiceSettleIndex(ctx context.Context) (int64, error) {
	row := q.db.QueryRowContext(ctx, nextInvoiceSettleIndex)
	var current_value int64
	err := row.Scan(&current_value)
	return current_value, err
}

const setKVInvoicePaymentHash = `-- name: SetKVInvoicePaymentHash :exec
UPDATE invoice_payment_hashes
SET hash = $2
WHERE id = $1
`

type SetKVInvoicePaymentHashParams struct {
	ID   int64
	Hash []byte
}

func (q *Queries) SetKVInvoicePaymentHash(ctx context.Context, arg SetKVInvoicePaymentHashParams) error {
	_, err := q.db.ExecContext(ctx, setKVInvoicePaymentHash, arg.ID, arg.Hash)
	return err
}

const updateInvoiceAmountPaid = `-- name: UpdateInvoiceAmountPaid :execresult
UPDATE invoices
SET amount_paid_msat = $2
WHERE id = $1
`

type UpdateInvoiceAmountPaidParams struct {
	ID             int64
	AmountPaidMsat int64
}

func (q *Queries) UpdateInvoiceAmountPaid(ctx context.Context, arg UpdateInvoiceAmountPaidParams) (sql.Result, error) {
	return q.db.ExecContext(ctx, updateInvoiceAmountPaid, arg.ID, arg.AmountPaidMsat)
}

const updateInvoiceHTLC = `-- name: UpdateInvoiceHTLC :exec
UPDATE invoice_htlcs 
SET state=$4, resolve_time=$5
WHERE htlc_id = $1 AND chan_id = $2 AND invoice_id = $3
`

type UpdateInvoiceHTLCParams struct {
	HtlcID      int64
	ChanID      string
	InvoiceID   int64
	State       int16
	ResolveTime sql.NullTime
}

func (q *Queries) UpdateInvoiceHTLC(ctx context.Context, arg UpdateInvoiceHTLCParams) error {
	_, err := q.db.ExecContext(ctx, updateInvoiceHTLC,
		arg.HtlcID,
		arg.ChanID,
		arg.InvoiceID,
		arg.State,
		arg.ResolveTime,
	)
	return err
}

const updateInvoiceHTLCs = `-- name: UpdateInvoiceHTLCs :exec
UPDATE invoice_htlcs 
SET state=$2, resolve_time=$3
WHERE invoice_id = $1 AND resolve_time IS NULL
`

type UpdateInvoiceHTLCsParams struct {
	InvoiceID   int64
	State       int16
	ResolveTime sql.NullTime
}

func (q *Queries) UpdateInvoiceHTLCs(ctx context.Context, arg UpdateInvoiceHTLCsParams) error {
	_, err := q.db.ExecContext(ctx, updateInvoiceHTLCs, arg.InvoiceID, arg.State, arg.ResolveTime)
	return err
}

const updateInvoiceState = `-- name: UpdateInvoiceState :execresult
UPDATE invoices
SET state = $2,
    preimage = COALESCE(preimage, $3),
    settle_index = COALESCE(settle_index, $4),
    settled_at = COALESCE(settled_at, $5)
WHERE id = $1
`

type UpdateInvoiceStateParams struct {
	ID          int64
	State       int16
	Preimage    []byte
	SettleIndex sql.NullInt64
	SettledAt   sql.NullTime
}

func (q *Queries) UpdateInvoiceState(ctx context.Context, arg UpdateInvoiceStateParams) (sql.Result, error) {
	return q.db.ExecContext(ctx, updateInvoiceState,
		arg.ID,
		arg.State,
		arg.Preimage,
		arg.SettleIndex,
		arg.SettledAt,
	)
}
