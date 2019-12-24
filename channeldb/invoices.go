package channeldb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// UnknownPreimage is an all-zeroes preimage that indicates that the
	// preimage for this invoice is not yet known.
	UnknownPreimage lntypes.Preimage

	// invoiceBucket is the name of the bucket within the database that
	// stores all data related to invoices no matter their final state.
	// Within the invoice bucket, each invoice is keyed by its invoice ID
	// which is a monotonically increasing uint32.
	invoiceBucket = []byte("invoices")

	// paymentHashIndexBucket is the name of the sub-bucket within the
	// invoiceBucket which indexes all invoices by their payment hash. The
	// payment hash is the sha256 of the invoice's payment preimage. This
	// index is used to detect duplicates, and also to provide a fast path
	// for looking up incoming HTLCs to determine if we're able to settle
	// them fully.
	//
	// maps: payHash => invoiceKey
	invoiceIndexBucket = []byte("paymenthashes")

	// numInvoicesKey is the name of key which houses the auto-incrementing
	// invoice ID which is essentially used as a primary key. With each
	// invoice inserted, the primary key is incremented by one. This key is
	// stored within the invoiceIndexBucket. Within the invoiceBucket
	// invoices are uniquely identified by the invoice ID.
	numInvoicesKey = []byte("nik")

	// addIndexBucket is an index bucket that we'll use to create a
	// monotonically increasing set of add indexes. Each time we add a new
	// invoice, this sequence number will be incremented and then populated
	// within the new invoice.
	//
	// In addition to this sequence number, we map:
	//
	//   addIndexNo => invoiceKey
	addIndexBucket = []byte("invoice-add-index")

	// settleIndexBucket is an index bucket that we'll use to create a
	// monotonically increasing integer for tracking a "settle index". Each
	// time an invoice is settled, this sequence number will be incremented
	// as populate within the newly settled invoice.
	//
	// In addition to this sequence number, we map:
	//
	//   settleIndexNo => invoiceKey
	settleIndexBucket = []byte("invoice-settle-index")

	// ErrInvoiceAlreadySettled is returned when the invoice is already
	// settled.
	ErrInvoiceAlreadySettled = errors.New("invoice already settled")

	// ErrInvoiceAlreadyCanceled is returned when the invoice is already
	// canceled.
	ErrInvoiceAlreadyCanceled = errors.New("invoice already canceled")

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
)

const (
	// MaxMemoSize is maximum size of the memo field within invoices stored
	// in the database.
	MaxMemoSize = 1024

	// MaxPaymentRequestSize is the max size of a payment request for
	// this invoice.
	// TODO(halseth): determine the max length payment request when field
	// lengths are final.
	MaxPaymentRequestSize = 4096

	// A set of tlv type definitions used to serialize invoice htlcs to the
	// database.
	//
	// NOTE: A migration should be added whenever this list changes. This
	// prevents against the database being rolled back to an older
	// format where the surrounding logic might assume a different set of
	// fields are known.
	chanIDType       tlv.Type = 1
	htlcIDType       tlv.Type = 3
	amtType          tlv.Type = 5
	acceptHeightType tlv.Type = 7
	acceptTimeType   tlv.Type = 9
	resolveTimeType  tlv.Type = 11
	expiryHeightType tlv.Type = 13
	htlcStateType    tlv.Type = 15
	mppTotalAmtType  tlv.Type = 17

	// A set of tlv type definitions used to serialize invoice bodiees.
	//
	// NOTE: A migration should be added whenever this list changes. This
	// prevents against the database being rolled back to an older
	// format where the surrounding logic might assume a different set of
	// fields are known.
	memoType        tlv.Type = 0
	payReqType      tlv.Type = 1
	createTimeType  tlv.Type = 2
	settleTimeType  tlv.Type = 3
	addIndexType    tlv.Type = 4
	settleIndexType tlv.Type = 5
	preimageType    tlv.Type = 6
	valueType       tlv.Type = 7
	cltvDeltaType   tlv.Type = 8
	expiryType      tlv.Type = 9
	paymentAddrType tlv.Type = 10
	featuresType    tlv.Type = 11
	invStateType    tlv.Type = 12
	amtPaidType     tlv.Type = 13
)

// ContractState describes the state the invoice is in.
type ContractState uint8

const (
	// ContractOpen means the invoice has only been created.
	ContractOpen ContractState = 0

	// ContractSettled means the htlc is settled and the invoice has been paid.
	ContractSettled ContractState = 1

	// ContractCanceled means the invoice has been canceled.
	ContractCanceled ContractState = 2

	// ContractAccepted means the HTLC has been accepted but not settled yet.
	ContractAccepted ContractState = 3
)

// String returns a human readable identifier for the ContractState type.
func (c ContractState) String() string {
	switch c {
	case ContractOpen:
		return "Open"
	case ContractSettled:
		return "Settled"
	case ContractCanceled:
		return "Canceled"
	case ContractAccepted:
		return "Accepted"
	}

	return "Unknown"
}

// ContractTerm is a companion struct to the Invoice struct. This struct houses
// the necessary conditions required before the invoice can be considered fully
// settled by the payee.
type ContractTerm struct {
	// FinalCltvDelta is the minimum required number of blocks before htlc
	// expiry when the invoice is accepted.
	FinalCltvDelta int32

	// Expiry defines how long after creation this invoice should expire.
	Expiry time.Duration

	// PaymentPreimage is the preimage which is to be revealed in the
	// occasion that an HTLC paying to the hash of this preimage is
	// extended.
	PaymentPreimage lntypes.Preimage

	// Value is the expected amount of milli-satoshis to be paid to an HTLC
	// which can be satisfied by the above preimage.
	Value lnwire.MilliSatoshi

	// PaymentAddr is a randomly generated value include in the MPP record
	// by the sender to prevent probing of the receiver.
	PaymentAddr [32]byte

	// Features is the feature vectors advertised on the payment request.
	Features *lnwire.FeatureVector
}

// Invoice is a payment invoice generated by a payee in order to request
// payment for some good or service. The inclusion of invoices within Lightning
// creates a payment work flow for merchants very similar to that of the
// existing financial system within PayPal, etc.  Invoices are added to the
// database when a payment is requested, then can be settled manually once the
// payment is received at the upper layer. For record keeping purposes,
// invoices are never deleted from the database, instead a bit is toggled
// denoting the invoice has been fully settled. Within the database, all
// invoices must have a unique payment hash which is generated by taking the
// sha256 of the payment preimage.
type Invoice struct {
	// Memo is an optional memo to be stored along side an invoice.  The
	// memo may contain further details pertaining to the invoice itself,
	// or any other message which fits within the size constraints.
	Memo []byte

	// PaymentRequest is the encoded payment request for this invoice. For
	// spontaneous (key send) payments, this field will be empty.
	PaymentRequest []byte

	// CreationDate is the exact time the invoice was created.
	CreationDate time.Time

	// SettleDate is the exact time the invoice was settled.
	SettleDate time.Time

	// Terms are the contractual payment terms of the invoice. Once all the
	// terms have been satisfied by the payer, then the invoice can be
	// considered fully fulfilled.
	//
	// TODO(roasbeef): later allow for multiple terms to fulfill the final
	// invoice: payment fragmentation, etc.
	Terms ContractTerm

	// AddIndex is an auto-incrementing integer that acts as a
	// monotonically increasing sequence number for all invoices created.
	// Clients can then use this field as a "checkpoint" of sorts when
	// implementing a streaming RPC to notify consumers of instances where
	// an invoice has been added before they re-connected.
	//
	// NOTE: This index starts at 1.
	AddIndex uint64

	// SettleIndex is an auto-incrementing integer that acts as a
	// monotonically increasing sequence number for all settled invoices.
	// Clients can then use this field as a "checkpoint" of sorts when
	// implementing a streaming RPC to notify consumers of instances where
	// an invoice has been settled before they re-connected.
	//
	// NOTE: This index starts at 1.
	SettleIndex uint64

	// State describes the state the invoice is in.
	State ContractState

	// AmtPaid is the final amount that we ultimately accepted for pay for
	// this invoice. We specify this value independently as it's possible
	// that the invoice originally didn't specify an amount, or the sender
	// overpaid.
	AmtPaid lnwire.MilliSatoshi

	// Htlcs records all htlcs that paid to this invoice. Some of these
	// htlcs may have been marked as canceled.
	Htlcs map[CircuitKey]*InvoiceHTLC
}

// HtlcState defines the states an htlc paying to an invoice can be in.
type HtlcState uint8

const (
	// HtlcStateAccepted indicates the htlc is locked-in, but not resolved.
	HtlcStateAccepted HtlcState = iota

	// HtlcStateCanceled indicates the htlc is canceled back to the
	// sender.
	HtlcStateCanceled

	// HtlcStateSettled indicates the htlc is settled.
	HtlcStateSettled
)

// InvoiceHTLC contains details about an htlc paying to this invoice.
type InvoiceHTLC struct {
	// Amt is the amount that is carried by this htlc.
	Amt lnwire.MilliSatoshi

	// MppTotalAmt is a field for mpp that indicates the expected total
	// amount.
	MppTotalAmt lnwire.MilliSatoshi

	// AcceptHeight is the block height at which the invoice registry
	// decided to accept this htlc as a payment to the invoice. At this
	// height, the invoice cltv delay must have been met.
	AcceptHeight uint32

	// AcceptTime is the wall clock time at which the invoice registry
	// decided to accept the htlc.
	AcceptTime time.Time

	// ResolveTime is the wall clock time at which the invoice registry
	// decided to settle the htlc.
	ResolveTime time.Time

	// Expiry is the expiry height of this htlc.
	Expiry uint32

	// State indicates the state the invoice htlc is currently in. A
	// canceled htlc isn't just removed from the invoice htlcs map, because
	// we need AcceptHeight to properly cancel the htlc back.
	State HtlcState

	// CustomRecords contains the custom key/value pairs that accompanied
	// the htlc.
	CustomRecords record.CustomSet
}

// HtlcAcceptDesc describes the details of a newly accepted htlc.
type HtlcAcceptDesc struct {
	// AcceptHeight is the block height at which this htlc was accepted.
	AcceptHeight int32

	// Amt is the amount that is carried by this htlc.
	Amt lnwire.MilliSatoshi

	// MppTotalAmt is a field for mpp that indicates the expected total
	// amount.
	MppTotalAmt lnwire.MilliSatoshi

	// Expiry is the expiry height of this htlc.
	Expiry uint32

	// CustomRecords contains the custom key/value pairs that accompanied
	// the htlc.
	CustomRecords record.CustomSet
}

// InvoiceUpdateDesc describes the changes that should be applied to the
// invoice.
type InvoiceUpdateDesc struct {
	// State is the new state that this invoice should progress to. If nil,
	// the state is left unchanged.
	State *InvoiceStateUpdateDesc

	// CancelHtlcs describes the htlcs that need to be canceled.
	CancelHtlcs map[CircuitKey]struct{}

	// AddHtlcs describes the newly accepted htlcs that need to be added to
	// the invoice.
	AddHtlcs map[CircuitKey]*HtlcAcceptDesc
}

// InvoiceStateUpdateDesc describes an invoice-level state transition.
type InvoiceStateUpdateDesc struct {
	// NewState is the new state that this invoice should progress to.
	NewState ContractState

	// Preimage must be set to the preimage when NewState is settled.
	Preimage lntypes.Preimage
}

// InvoiceUpdateCallback is a callback used in the db transaction to update the
// invoice.
type InvoiceUpdateCallback = func(invoice *Invoice) (*InvoiceUpdateDesc, error)

func validateInvoice(i *Invoice) error {
	if len(i.Memo) > MaxMemoSize {
		return fmt.Errorf("max length a memo is %v, and invoice "+
			"of length %v was provided", MaxMemoSize, len(i.Memo))
	}
	if len(i.PaymentRequest) > MaxPaymentRequestSize {
		return fmt.Errorf("max length of payment request is %v, length "+
			"provided was %v", MaxPaymentRequestSize,
			len(i.PaymentRequest))
	}
	if i.Terms.Features == nil {
		return errors.New("invoice must have a feature vector")
	}
	return nil
}

// IsPending returns ture if the invoice is in ContractOpen state.
func (i *Invoice) IsPending() bool {
	return i.State == ContractOpen || i.State == ContractAccepted
}

// AddInvoice inserts the targeted invoice into the database. If the invoice has
// *any* payment hashes which already exists within the database, then the
// insertion will be aborted and rejected due to the strict policy banning any
// duplicate payment hashes. A side effect of this function is that it sets
// AddIndex on newInvoice.
func (d *DB) AddInvoice(newInvoice *Invoice, paymentHash lntypes.Hash) (
	uint64, error) {

	if err := validateInvoice(newInvoice); err != nil {
		return 0, err
	}

	var invoiceAddIndex uint64
	err := d.Update(func(tx *bbolt.Tx) error {
		invoices, err := tx.CreateBucketIfNotExists(invoiceBucket)
		if err != nil {
			return err
		}

		invoiceIndex, err := invoices.CreateBucketIfNotExists(
			invoiceIndexBucket,
		)
		if err != nil {
			return err
		}
		addIndex, err := invoices.CreateBucketIfNotExists(
			addIndexBucket,
		)
		if err != nil {
			return err
		}

		// Ensure that an invoice an identical payment hash doesn't
		// already exist within the index.
		if invoiceIndex.Get(paymentHash[:]) != nil {
			return ErrDuplicateInvoice
		}

		// If the current running payment ID counter hasn't yet been
		// created, then create it now.
		var invoiceNum uint32
		invoiceCounter := invoiceIndex.Get(numInvoicesKey)
		if invoiceCounter == nil {
			var scratch [4]byte
			byteOrder.PutUint32(scratch[:], invoiceNum)
			err := invoiceIndex.Put(numInvoicesKey, scratch[:])
			if err != nil {
				return err
			}
		} else {
			invoiceNum = byteOrder.Uint32(invoiceCounter)
		}

		newIndex, err := putInvoice(
			invoices, invoiceIndex, addIndex, newInvoice, invoiceNum,
			paymentHash,
		)
		if err != nil {
			return err
		}

		invoiceAddIndex = newIndex
		return nil
	})
	if err != nil {
		return 0, err
	}

	return invoiceAddIndex, err
}

// InvoicesAddedSince can be used by callers to seek into the event time series
// of all the invoices added in the database. The specified sinceAddIndex
// should be the highest add index that the caller knows of. This method will
// return all invoices with an add index greater than the specified
// sinceAddIndex.
//
// NOTE: The index starts from 1, as a result. We enforce that specifying a
// value below the starting index value is a noop.
func (d *DB) InvoicesAddedSince(sinceAddIndex uint64) ([]Invoice, error) {
	var newInvoices []Invoice

	// If an index of zero was specified, then in order to maintain
	// backwards compat, we won't send out any new invoices.
	if sinceAddIndex == 0 {
		return newInvoices, nil
	}

	var startIndex [8]byte
	byteOrder.PutUint64(startIndex[:], sinceAddIndex)

	err := d.DB.View(func(tx *bbolt.Tx) error {
		invoices := tx.Bucket(invoiceBucket)
		if invoices == nil {
			return ErrNoInvoicesCreated
		}

		addIndex := invoices.Bucket(addIndexBucket)
		if addIndex == nil {
			return ErrNoInvoicesCreated
		}

		// We'll now run through each entry in the add index starting
		// at our starting index. We'll continue until we reach the
		// very end of the current key space.
		invoiceCursor := addIndex.Cursor()

		// We'll seek to the starting index, then manually advance the
		// cursor in order to skip the entry with the since add index.
		invoiceCursor.Seek(startIndex[:])
		addSeqNo, invoiceKey := invoiceCursor.Next()

		for ; addSeqNo != nil && bytes.Compare(addSeqNo, startIndex[:]) > 0; addSeqNo, invoiceKey = invoiceCursor.Next() {

			// For each key found, we'll look up the actual
			// invoice, then accumulate it into our return value.
			invoice, err := fetchInvoice(invoiceKey, invoices)
			if err != nil {
				return err
			}

			newInvoices = append(newInvoices, invoice)
		}

		return nil
	})
	switch {
	// If no invoices have been created, then we'll return the empty set of
	// invoices.
	case err == ErrNoInvoicesCreated:

	case err != nil:
		return nil, err
	}

	return newInvoices, nil
}

// LookupInvoice attempts to look up an invoice according to its 32 byte
// payment hash. If an invoice which can settle the HTLC identified by the
// passed payment hash isn't found, then an error is returned. Otherwise, the
// full invoice is returned. Before setting the incoming HTLC, the values
// SHOULD be checked to ensure the payer meets the agreed upon contractual
// terms of the payment.
func (d *DB) LookupInvoice(paymentHash [32]byte) (Invoice, error) {
	var invoice Invoice
	err := d.View(func(tx *bbolt.Tx) error {
		invoices := tx.Bucket(invoiceBucket)
		if invoices == nil {
			return ErrNoInvoicesCreated
		}
		invoiceIndex := invoices.Bucket(invoiceIndexBucket)
		if invoiceIndex == nil {
			return ErrNoInvoicesCreated
		}

		// Check the invoice index to see if an invoice paying to this
		// hash exists within the DB.
		invoiceNum := invoiceIndex.Get(paymentHash[:])
		if invoiceNum == nil {
			return ErrInvoiceNotFound
		}

		// An invoice matching the payment hash has been found, so
		// retrieve the record of the invoice itself.
		i, err := fetchInvoice(invoiceNum, invoices)
		if err != nil {
			return err
		}
		invoice = i

		return nil
	})
	if err != nil {
		return invoice, err
	}

	return invoice, nil
}

// InvoiceWithPaymentHash is used to store an invoice and its corresponding
// payment hash. This struct is only used to store results of
// ChannelDB.FetchAllInvoicesWithPaymentHash() call.
type InvoiceWithPaymentHash struct {
	// Invoice holds the invoice as selected from the invoices bucket.
	Invoice Invoice

	// PaymentHash is the payment hash for the Invoice.
	PaymentHash lntypes.Hash
}

// FetchAllInvoicesWithPaymentHash returns all invoices and their payment hashes
// currently stored within the database. If the pendingOnly param is true, then
// only open or accepted invoices and their payment hashes will be returned,
// skipping all invoices that are fully settled or canceled. Note that the
// returned array is not ordered by add index.
func (d *DB) FetchAllInvoicesWithPaymentHash(pendingOnly bool) (
	[]InvoiceWithPaymentHash, error) {

	var result []InvoiceWithPaymentHash

	err := d.View(func(tx *bbolt.Tx) error {
		invoices := tx.Bucket(invoiceBucket)
		if invoices == nil {
			return ErrNoInvoicesCreated
		}

		invoiceIndex := invoices.Bucket(invoiceIndexBucket)
		if invoiceIndex == nil {
			// Mask the error if there's no invoice
			// index as that simply means there are no
			// invoices added yet to the DB. In this case
			// we simply return an empty list.
			return nil
		}

		return invoiceIndex.ForEach(func(k, v []byte) error {
			// Skip the special numInvoicesKey as that does not
			// point to a valid invoice.
			if bytes.Equal(k, numInvoicesKey) {
				return nil
			}

			if v == nil {
				return nil
			}

			invoice, err := fetchInvoice(v, invoices)
			if err != nil {
				return err
			}

			if pendingOnly && !invoice.IsPending() {
				return nil
			}

			invoiceWithPaymentHash := InvoiceWithPaymentHash{
				Invoice: invoice,
			}

			copy(invoiceWithPaymentHash.PaymentHash[:], k)
			result = append(result, invoiceWithPaymentHash)

			return nil
		})
	})

	if err != nil {
		return nil, err
	}

	return result, nil
}

// FetchAllInvoices returns all invoices currently stored within the database.
// If the pendingOnly param is set to true, then only invoices in open or
// accepted state will be returned, skipping all invoices that are fully
// settled or canceled.
func (d *DB) FetchAllInvoices(pendingOnly bool) ([]Invoice, error) {
	var invoices []Invoice

	err := d.View(func(tx *bbolt.Tx) error {
		invoiceB := tx.Bucket(invoiceBucket)
		if invoiceB == nil {
			return ErrNoInvoicesCreated
		}

		// Iterate through the entire key space of the top-level
		// invoice bucket. If key with a non-nil value stores the next
		// invoice ID which maps to the corresponding invoice.
		return invoiceB.ForEach(func(k, v []byte) error {
			if v == nil {
				return nil
			}

			invoiceReader := bytes.NewReader(v)
			invoice, err := deserializeInvoice(invoiceReader)
			if err != nil {
				return err
			}

			if pendingOnly && !invoice.IsPending() {
				return nil
			}

			invoices = append(invoices, invoice)

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return invoices, nil
}

// InvoiceQuery represents a query to the invoice database. The query allows a
// caller to retrieve all invoices starting from a particular add index and
// limit the number of results returned.
type InvoiceQuery struct {
	// IndexOffset is the offset within the add indices to start at. This
	// can be used to start the response at a particular invoice.
	IndexOffset uint64

	// NumMaxInvoices is the maximum number of invoices that should be
	// starting from the add index.
	NumMaxInvoices uint64

	// PendingOnly, if set, returns unsettled invoices starting from the
	// add index.
	PendingOnly bool

	// Reversed, if set, indicates that the invoices returned should start
	// from the IndexOffset and go backwards.
	Reversed bool
}

// InvoiceSlice is the response to a invoice query. It includes the original
// query, the set of invoices that match the query, and an integer which
// represents the offset index of the last item in the set of returned invoices.
// This integer allows callers to resume their query using this offset in the
// event that the query's response exceeds the maximum number of returnable
// invoices.
type InvoiceSlice struct {
	InvoiceQuery

	// Invoices is the set of invoices that matched the query above.
	Invoices []Invoice

	// FirstIndexOffset is the index of the first element in the set of
	// returned Invoices above. Callers can use this to resume their query
	// in the event that the slice has too many events to fit into a single
	// response.
	FirstIndexOffset uint64

	// LastIndexOffset is the index of the last element in the set of
	// returned Invoices above. Callers can use this to resume their query
	// in the event that the slice has too many events to fit into a single
	// response.
	LastIndexOffset uint64
}

// QueryInvoices allows a caller to query the invoice database for invoices
// within the specified add index range.
func (d *DB) QueryInvoices(q InvoiceQuery) (InvoiceSlice, error) {
	resp := InvoiceSlice{
		InvoiceQuery: q,
	}

	err := d.View(func(tx *bbolt.Tx) error {
		// If the bucket wasn't found, then there aren't any invoices
		// within the database yet, so we can simply exit.
		invoices := tx.Bucket(invoiceBucket)
		if invoices == nil {
			return ErrNoInvoicesCreated
		}
		invoiceAddIndex := invoices.Bucket(addIndexBucket)
		if invoiceAddIndex == nil {
			return ErrNoInvoicesCreated
		}

		// keyForIndex is a helper closure that retrieves the invoice
		// key for the given add index of an invoice.
		keyForIndex := func(c *bbolt.Cursor, index uint64) []byte {
			var keyIndex [8]byte
			byteOrder.PutUint64(keyIndex[:], index)
			_, invoiceKey := c.Seek(keyIndex[:])
			return invoiceKey
		}

		// nextKey is a helper closure to determine what the next
		// invoice key is when iterating over the invoice add index.
		nextKey := func(c *bbolt.Cursor) ([]byte, []byte) {
			if q.Reversed {
				return c.Prev()
			}
			return c.Next()
		}

		// We'll be using a cursor to seek into the database and return
		// a slice of invoices. We'll need to determine where to start
		// our cursor depending on the parameters set within the query.
		c := invoiceAddIndex.Cursor()
		invoiceKey := keyForIndex(c, q.IndexOffset+1)

		// If the query is specifying reverse iteration, then we must
		// handle a few offset cases.
		if q.Reversed {
			switch q.IndexOffset {

			// This indicates the default case, where no offset was
			// specified. In that case we just start from the last
			// invoice.
			case 0:
				_, invoiceKey = c.Last()

			// This indicates the offset being set to the very
			// first invoice. Since there are no invoices before
			// this offset, and the direction is reversed, we can
			// return without adding any invoices to the response.
			case 1:
				return nil

			// Otherwise we start iteration at the invoice prior to
			// the offset.
			default:
				invoiceKey = keyForIndex(c, q.IndexOffset-1)
			}
		}

		// If we know that a set of invoices exists, then we'll begin
		// our seek through the bucket in order to satisfy the query.
		// We'll continue until either we reach the end of the range, or
		// reach our max number of invoices.
		for ; invoiceKey != nil; _, invoiceKey = nextKey(c) {
			// If our current return payload exceeds the max number
			// of invoices, then we'll exit now.
			if uint64(len(resp.Invoices)) >= q.NumMaxInvoices {
				break
			}

			invoice, err := fetchInvoice(invoiceKey, invoices)
			if err != nil {
				return err
			}

			// Skip any settled or canceled invoices if the caller is
			// only interested in pending ones.
			if q.PendingOnly && !invoice.IsPending() {
				continue
			}

			// At this point, we've exhausted the offset, so we'll
			// begin collecting invoices found within the range.
			resp.Invoices = append(resp.Invoices, invoice)
		}

		// If we iterated through the add index in reverse order, then
		// we'll need to reverse the slice of invoices to return them in
		// forward order.
		if q.Reversed {
			numInvoices := len(resp.Invoices)
			for i := 0; i < numInvoices/2; i++ {
				opposite := numInvoices - i - 1
				resp.Invoices[i], resp.Invoices[opposite] =
					resp.Invoices[opposite], resp.Invoices[i]
			}
		}

		return nil
	})
	if err != nil && err != ErrNoInvoicesCreated {
		return resp, err
	}

	// Finally, record the indexes of the first and last invoices returned
	// so that the caller can resume from this point later on.
	if len(resp.Invoices) > 0 {
		resp.FirstIndexOffset = resp.Invoices[0].AddIndex
		resp.LastIndexOffset = resp.Invoices[len(resp.Invoices)-1].AddIndex
	}

	return resp, nil
}

// UpdateInvoice attempts to update an invoice corresponding to the passed
// payment hash. If an invoice matching the passed payment hash doesn't exist
// within the database, then the action will fail with a "not found" error.
//
// The update is performed inside the same database transaction that fetches the
// invoice and is therefore atomic. The fields to update are controlled by the
// supplied callback.
func (d *DB) UpdateInvoice(paymentHash lntypes.Hash,
	callback InvoiceUpdateCallback) (*Invoice, error) {

	var updatedInvoice *Invoice
	err := d.Update(func(tx *bbolt.Tx) error {
		invoices, err := tx.CreateBucketIfNotExists(invoiceBucket)
		if err != nil {
			return err
		}
		invoiceIndex, err := invoices.CreateBucketIfNotExists(
			invoiceIndexBucket,
		)
		if err != nil {
			return err
		}
		settleIndex, err := invoices.CreateBucketIfNotExists(
			settleIndexBucket,
		)
		if err != nil {
			return err
		}

		// Check the invoice index to see if an invoice paying to this
		// hash exists within the DB.
		invoiceNum := invoiceIndex.Get(paymentHash[:])
		if invoiceNum == nil {
			return ErrInvoiceNotFound
		}

		updatedInvoice, err = d.updateInvoice(
			paymentHash, invoices, settleIndex, invoiceNum,
			callback,
		)

		return err
	})

	return updatedInvoice, err
}

// InvoicesSettledSince can be used by callers to catch up any settled invoices
// they missed within the settled invoice time series. We'll return all known
// settled invoice that have a settle index higher than the passed
// sinceSettleIndex.
//
// NOTE: The index starts from 1, as a result. We enforce that specifying a
// value below the starting index value is a noop.
func (d *DB) InvoicesSettledSince(sinceSettleIndex uint64) ([]Invoice, error) {
	var settledInvoices []Invoice

	// If an index of zero was specified, then in order to maintain
	// backwards compat, we won't send out any new invoices.
	if sinceSettleIndex == 0 {
		return settledInvoices, nil
	}

	var startIndex [8]byte
	byteOrder.PutUint64(startIndex[:], sinceSettleIndex)

	err := d.DB.View(func(tx *bbolt.Tx) error {
		invoices := tx.Bucket(invoiceBucket)
		if invoices == nil {
			return ErrNoInvoicesCreated
		}

		settleIndex := invoices.Bucket(settleIndexBucket)
		if settleIndex == nil {
			return ErrNoInvoicesCreated
		}

		// We'll now run through each entry in the add index starting
		// at our starting index. We'll continue until we reach the
		// very end of the current key space.
		invoiceCursor := settleIndex.Cursor()

		// We'll seek to the starting index, then manually advance the
		// cursor in order to skip the entry with the since add index.
		invoiceCursor.Seek(startIndex[:])
		seqNo, invoiceKey := invoiceCursor.Next()

		for ; seqNo != nil && bytes.Compare(seqNo, startIndex[:]) > 0; seqNo, invoiceKey = invoiceCursor.Next() {

			// For each key found, we'll look up the actual
			// invoice, then accumulate it into our return value.
			invoice, err := fetchInvoice(invoiceKey, invoices)
			if err != nil {
				return err
			}

			settledInvoices = append(settledInvoices, invoice)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return settledInvoices, nil
}

func putInvoice(invoices, invoiceIndex, addIndex *bbolt.Bucket,
	i *Invoice, invoiceNum uint32, paymentHash lntypes.Hash) (
	uint64, error) {

	// Create the invoice key which is just the big-endian representation
	// of the invoice number.
	var invoiceKey [4]byte
	byteOrder.PutUint32(invoiceKey[:], invoiceNum)

	// Increment the num invoice counter index so the next invoice bares
	// the proper ID.
	var scratch [4]byte
	invoiceCounter := invoiceNum + 1
	byteOrder.PutUint32(scratch[:], invoiceCounter)
	if err := invoiceIndex.Put(numInvoicesKey, scratch[:]); err != nil {
		return 0, err
	}

	// Add the payment hash to the invoice index. This will let us quickly
	// identify if we can settle an incoming payment, and also to possibly
	// allow a single invoice to have multiple payment installations.
	err := invoiceIndex.Put(paymentHash[:], invoiceKey[:])
	if err != nil {
		return 0, err
	}

	// Next, we'll obtain the next add invoice index (sequence
	// number), so we can properly place this invoice within this
	// event stream.
	nextAddSeqNo, err := addIndex.NextSequence()
	if err != nil {
		return 0, err
	}

	// With the next sequence obtained, we'll updating the event series in
	// the add index bucket to map this current add counter to the index of
	// this new invoice.
	var seqNoBytes [8]byte
	byteOrder.PutUint64(seqNoBytes[:], nextAddSeqNo)
	if err := addIndex.Put(seqNoBytes[:], invoiceKey[:]); err != nil {
		return 0, err
	}

	i.AddIndex = nextAddSeqNo

	// Finally, serialize the invoice itself to be written to the disk.
	var buf bytes.Buffer
	if err := serializeInvoice(&buf, i); err != nil {
		return 0, err
	}

	if err := invoices.Put(invoiceKey[:], buf.Bytes()); err != nil {
		return 0, err
	}

	return nextAddSeqNo, nil
}

// serializeInvoice serializes an invoice to a writer.
//
// Note: this function is in use for a migration. Before making changes that
// would modify the on disk format, make a copy of the original code and store
// it with the migration.
func serializeInvoice(w io.Writer, i *Invoice) error {
	creationDateBytes, err := i.CreationDate.MarshalBinary()
	if err != nil {
		return err
	}

	settleDateBytes, err := i.SettleDate.MarshalBinary()
	if err != nil {
		return err
	}

	var fb bytes.Buffer
	err = i.Terms.Features.EncodeBase256(&fb)
	if err != nil {
		return err
	}
	featureBytes := fb.Bytes()

	preimage := [32]byte(i.Terms.PaymentPreimage)
	value := uint64(i.Terms.Value)
	cltvDelta := uint32(i.Terms.FinalCltvDelta)
	expiry := uint64(i.Terms.Expiry)

	amtPaid := uint64(i.AmtPaid)
	state := uint8(i.State)

	tlvStream, err := tlv.NewStream(
		// Memo and payreq.
		tlv.MakePrimitiveRecord(memoType, &i.Memo),
		tlv.MakePrimitiveRecord(payReqType, &i.PaymentRequest),

		// Add/settle metadata.
		tlv.MakePrimitiveRecord(createTimeType, &creationDateBytes),
		tlv.MakePrimitiveRecord(settleTimeType, &settleDateBytes),
		tlv.MakePrimitiveRecord(addIndexType, &i.AddIndex),
		tlv.MakePrimitiveRecord(settleIndexType, &i.SettleIndex),

		// Terms.
		tlv.MakePrimitiveRecord(preimageType, &preimage),
		tlv.MakePrimitiveRecord(valueType, &value),
		tlv.MakePrimitiveRecord(cltvDeltaType, &cltvDelta),
		tlv.MakePrimitiveRecord(expiryType, &expiry),
		tlv.MakePrimitiveRecord(paymentAddrType, &i.Terms.PaymentAddr),
		tlv.MakePrimitiveRecord(featuresType, &featureBytes),

		// Invoice state.
		tlv.MakePrimitiveRecord(invStateType, &state),
		tlv.MakePrimitiveRecord(amtPaidType, &amtPaid),
	)
	if err != nil {
		return err
	}

	var b bytes.Buffer
	if err = tlvStream.Encode(&b); err != nil {
		return err
	}

	err = binary.Write(w, byteOrder, uint64(b.Len()))
	if err != nil {
		return err
	}

	if _, err = w.Write(b.Bytes()); err != nil {
		return err
	}

	return serializeHtlcs(w, i.Htlcs)
}

// serializeHtlcs serializes a map containing circuit keys and invoice htlcs to
// a writer.
func serializeHtlcs(w io.Writer, htlcs map[CircuitKey]*InvoiceHTLC) error {
	for key, htlc := range htlcs {
		// Encode the htlc in a tlv stream.
		chanID := key.ChanID.ToUint64()
		amt := uint64(htlc.Amt)
		mppTotalAmt := uint64(htlc.MppTotalAmt)
		acceptTime := uint64(htlc.AcceptTime.UnixNano())
		resolveTime := uint64(htlc.ResolveTime.UnixNano())
		state := uint8(htlc.State)

		var records []tlv.Record
		records = append(records,
			tlv.MakePrimitiveRecord(chanIDType, &chanID),
			tlv.MakePrimitiveRecord(htlcIDType, &key.HtlcID),
			tlv.MakePrimitiveRecord(amtType, &amt),
			tlv.MakePrimitiveRecord(
				acceptHeightType, &htlc.AcceptHeight,
			),
			tlv.MakePrimitiveRecord(acceptTimeType, &acceptTime),
			tlv.MakePrimitiveRecord(resolveTimeType, &resolveTime),
			tlv.MakePrimitiveRecord(expiryHeightType, &htlc.Expiry),
			tlv.MakePrimitiveRecord(htlcStateType, &state),
			tlv.MakePrimitiveRecord(mppTotalAmtType, &mppTotalAmt),
		)

		// Convert the custom records to tlv.Record types that are ready
		// for serialization.
		customRecords := tlv.MapToRecords(htlc.CustomRecords)

		// Append the custom records. Their ids are in the experimental
		// range and sorted, so there is no need to sort again.
		records = append(records, customRecords...)

		tlvStream, err := tlv.NewStream(records...)
		if err != nil {
			return err
		}

		var b bytes.Buffer
		if err := tlvStream.Encode(&b); err != nil {
			return err
		}

		// Write the length of the tlv stream followed by the stream
		// bytes.
		err = binary.Write(w, byteOrder, uint64(b.Len()))
		if err != nil {
			return err
		}

		if _, err := w.Write(b.Bytes()); err != nil {
			return err
		}
	}

	return nil
}

func fetchInvoice(invoiceNum []byte, invoices *bbolt.Bucket) (Invoice, error) {
	invoiceBytes := invoices.Get(invoiceNum)
	if invoiceBytes == nil {
		return Invoice{}, ErrInvoiceNotFound
	}

	invoiceReader := bytes.NewReader(invoiceBytes)

	return deserializeInvoice(invoiceReader)
}

func deserializeInvoice(r io.Reader) (Invoice, error) {
	var (
		preimage  [32]byte
		value     uint64
		cltvDelta uint32
		expiry    uint64
		amtPaid   uint64
		state     uint8

		creationDateBytes []byte
		settleDateBytes   []byte
		featureBytes      []byte
	)

	var i Invoice
	tlvStream, err := tlv.NewStream(
		// Memo and payreq.
		tlv.MakePrimitiveRecord(memoType, &i.Memo),
		tlv.MakePrimitiveRecord(payReqType, &i.PaymentRequest),

		// Add/settle metadata.
		tlv.MakePrimitiveRecord(createTimeType, &creationDateBytes),
		tlv.MakePrimitiveRecord(settleTimeType, &settleDateBytes),
		tlv.MakePrimitiveRecord(addIndexType, &i.AddIndex),
		tlv.MakePrimitiveRecord(settleIndexType, &i.SettleIndex),

		// Terms.
		tlv.MakePrimitiveRecord(preimageType, &preimage),
		tlv.MakePrimitiveRecord(valueType, &value),
		tlv.MakePrimitiveRecord(cltvDeltaType, &cltvDelta),
		tlv.MakePrimitiveRecord(expiryType, &expiry),
		tlv.MakePrimitiveRecord(paymentAddrType, &i.Terms.PaymentAddr),
		tlv.MakePrimitiveRecord(featuresType, &featureBytes),

		// Invoice state.
		tlv.MakePrimitiveRecord(invStateType, &state),
		tlv.MakePrimitiveRecord(amtPaidType, &amtPaid),
	)
	if err != nil {
		return i, err
	}

	var bodyLen int64
	err = binary.Read(r, byteOrder, &bodyLen)
	if err != nil {
		return i, err
	}

	lr := io.LimitReader(r, bodyLen)
	if err = tlvStream.Decode(lr); err != nil {
		return i, err
	}

	i.Terms.PaymentPreimage = lntypes.Preimage(preimage)
	i.Terms.Value = lnwire.MilliSatoshi(value)
	i.Terms.FinalCltvDelta = int32(cltvDelta)
	i.Terms.Expiry = time.Duration(expiry)
	i.AmtPaid = lnwire.MilliSatoshi(amtPaid)
	i.State = ContractState(state)

	err = i.CreationDate.UnmarshalBinary(creationDateBytes)
	if err != nil {
		return i, err
	}

	err = i.SettleDate.UnmarshalBinary(settleDateBytes)
	if err != nil {
		return i, err
	}

	rawFeatures := lnwire.NewRawFeatureVector()
	err = rawFeatures.DecodeBase256(
		bytes.NewReader(featureBytes), len(featureBytes),
	)
	if err != nil {
		return i, err
	}

	i.Terms.Features = lnwire.NewFeatureVector(
		rawFeatures, lnwire.Features,
	)

	i.Htlcs, err = deserializeHtlcs(r)
	return i, err
}

// deserializeHtlcs reads a list of invoice htlcs from a reader and returns it
// as a map.
func deserializeHtlcs(r io.Reader) (map[CircuitKey]*InvoiceHTLC, error) {
	htlcs := make(map[CircuitKey]*InvoiceHTLC, 0)

	for {
		// Read the length of the tlv stream for this htlc.
		var streamLen int64
		if err := binary.Read(r, byteOrder, &streamLen); err != nil {
			if err == io.EOF {
				break
			}

			return nil, err
		}

		// Limit the reader so that it stops at the end of this htlc's
		// stream.
		htlcReader := io.LimitReader(r, streamLen)

		// Decode the contents into the htlc fields.
		var (
			htlc                    InvoiceHTLC
			key                     CircuitKey
			chanID                  uint64
			state                   uint8
			acceptTime, resolveTime uint64
			amt, mppTotalAmt        uint64
		)
		tlvStream, err := tlv.NewStream(
			tlv.MakePrimitiveRecord(chanIDType, &chanID),
			tlv.MakePrimitiveRecord(htlcIDType, &key.HtlcID),
			tlv.MakePrimitiveRecord(amtType, &amt),
			tlv.MakePrimitiveRecord(
				acceptHeightType, &htlc.AcceptHeight,
			),
			tlv.MakePrimitiveRecord(acceptTimeType, &acceptTime),
			tlv.MakePrimitiveRecord(resolveTimeType, &resolveTime),
			tlv.MakePrimitiveRecord(expiryHeightType, &htlc.Expiry),
			tlv.MakePrimitiveRecord(htlcStateType, &state),
			tlv.MakePrimitiveRecord(mppTotalAmtType, &mppTotalAmt),
		)
		if err != nil {
			return nil, err
		}

		parsedTypes, err := tlvStream.DecodeWithParsedTypes(htlcReader)
		if err != nil {
			return nil, err
		}

		key.ChanID = lnwire.NewShortChanIDFromInt(chanID)
		htlc.AcceptTime = time.Unix(0, int64(acceptTime))
		htlc.ResolveTime = time.Unix(0, int64(resolveTime))
		htlc.State = HtlcState(state)
		htlc.Amt = lnwire.MilliSatoshi(amt)
		htlc.MppTotalAmt = lnwire.MilliSatoshi(mppTotalAmt)

		// Reconstruct the custom records fields from the parsed types
		// map return from the tlv parser.
		htlc.CustomRecords = hop.NewCustomRecords(parsedTypes)

		htlcs[key] = &htlc
	}

	return htlcs, nil
}

// copySlice allocates a new slice and copies the source into it.
func copySlice(src []byte) []byte {
	dest := make([]byte, len(src))
	copy(dest, src)
	return dest
}

// copyInvoice makes a deep copy of the supplied invoice.
func copyInvoice(src *Invoice) *Invoice {
	dest := Invoice{
		Memo:           copySlice(src.Memo),
		PaymentRequest: copySlice(src.PaymentRequest),
		CreationDate:   src.CreationDate,
		SettleDate:     src.SettleDate,
		Terms:          src.Terms,
		AddIndex:       src.AddIndex,
		SettleIndex:    src.SettleIndex,
		State:          src.State,
		AmtPaid:        src.AmtPaid,
		Htlcs: make(
			map[CircuitKey]*InvoiceHTLC, len(src.Htlcs),
		),
	}

	dest.Terms.Features = src.Terms.Features.Clone()

	for k, v := range src.Htlcs {
		dest.Htlcs[k] = v
	}

	return &dest
}

// updateInvoice fetches the invoice, obtains the update descriptor from the
// callback and applies the updates in a single db transaction.
func (d *DB) updateInvoice(hash lntypes.Hash, invoices, settleIndex *bbolt.Bucket,
	invoiceNum []byte, callback InvoiceUpdateCallback) (*Invoice, error) {

	invoice, err := fetchInvoice(invoiceNum, invoices)
	if err != nil {
		return nil, err
	}

	// Create deep copy to prevent any accidental modification in the
	// callback.
	invoiceCopy := copyInvoice(&invoice)

	// Call the callback and obtain the update descriptor.
	update, err := callback(invoiceCopy)
	if err != nil {
		return &invoice, err
	}

	// If there is nothing to update, return early.
	if update == nil {
		return &invoice, nil
	}

	now := d.Now()

	// Update invoice state if the update descriptor indicates an invoice
	// state change.
	if update.State != nil {
		err := updateInvoiceState(&invoice, hash, *update.State)
		if err != nil {
			return nil, err
		}

		if update.State.NewState == ContractSettled {
			err := setSettleMetaFields(
				settleIndex, invoiceNum, &invoice, now,
			)
			if err != nil {
				return nil, err
			}
		}
	}

	// Process add actions from update descriptor.
	for key, htlcUpdate := range update.AddHtlcs {
		if _, exists := invoice.Htlcs[key]; exists {
			return nil, fmt.Errorf("duplicate add of htlc %v", key)
		}

		// Force caller to supply htlc without custom records in a
		// consistent way.
		if htlcUpdate.CustomRecords == nil {
			return nil, errors.New("nil custom records map")
		}

		htlc := &InvoiceHTLC{
			Amt:           htlcUpdate.Amt,
			MppTotalAmt:   htlcUpdate.MppTotalAmt,
			Expiry:        htlcUpdate.Expiry,
			AcceptHeight:  uint32(htlcUpdate.AcceptHeight),
			AcceptTime:    now,
			State:         HtlcStateAccepted,
			CustomRecords: htlcUpdate.CustomRecords,
		}

		invoice.Htlcs[key] = htlc
	}

	// Align htlc states with invoice state and recalculate amount paid.
	var (
		amtPaid     lnwire.MilliSatoshi
		cancelHtlcs = update.CancelHtlcs
	)
	for key, htlc := range invoice.Htlcs {
		// Check whether this htlc needs to be canceled. If it does,
		// update the htlc state to Canceled.
		_, cancel := cancelHtlcs[key]
		if cancel {
			// Consistency check to verify that there is no overlap
			// between the add and cancel sets.
			if _, added := update.AddHtlcs[key]; added {
				return nil, fmt.Errorf("added htlc %v canceled",
					key)
			}

			err := cancelSingleHtlc(now, htlc, invoice.State)
			if err != nil {
				return nil, err
			}

			// Delete processed cancel action, so that we can check
			// later that there are no actions left.
			delete(cancelHtlcs, key)

			continue
		}

		// The invoice state may have changed and this could have
		// implications for the states of the individual htlcs. Align
		// the htlc state with the current invoice state.
		err := updateHtlc(now, htlc, invoice.State)
		if err != nil {
			return nil, err
		}

		// Update the running amount paid to this invoice. We don't
		// include accepted htlcs when the invoice is still open.
		if invoice.State != ContractOpen &&
			(htlc.State == HtlcStateAccepted ||
				htlc.State == HtlcStateSettled) {

			amtPaid += htlc.Amt
		}
	}
	invoice.AmtPaid = amtPaid

	// Verify that we didn't get an action for htlcs that are not present on
	// the invoice.
	if len(cancelHtlcs) > 0 {
		return nil, errors.New("cancel action on non-existent htlc(s)")
	}

	// Reserialize and update invoice.
	var buf bytes.Buffer
	if err := serializeInvoice(&buf, &invoice); err != nil {
		return nil, err
	}

	if err := invoices.Put(invoiceNum[:], buf.Bytes()); err != nil {
		return nil, err
	}

	return &invoice, nil
}

// updateInvoiceState validates and processes an invoice state update.
func updateInvoiceState(invoice *Invoice, hash lntypes.Hash,
	update InvoiceStateUpdateDesc) error {

	// Returning to open is never allowed from any state.
	if update.NewState == ContractOpen {
		return ErrInvoiceCannotOpen
	}

	switch invoice.State {

	// Once a contract is accepted, we can only transition to settled or
	// canceled. Forbid transitioning back into this state. Otherwise this
	// state is identical to ContractOpen, so we fallthrough to apply the
	// same checks that we apply to open invoices.
	case ContractAccepted:
		if update.NewState == ContractAccepted {
			return ErrInvoiceCannotAccept
		}

		fallthrough

	// If a contract is open, permit a state transition to accepted, settled
	// or canceled. The only restriction is on transitioning to settled
	// where we ensure the preimage is valid.
	case ContractOpen:
		if update.NewState == ContractSettled {
			// Validate preimage.
			if update.Preimage.Hash() != hash {
				return ErrInvoicePreimageMismatch
			}
			invoice.Terms.PaymentPreimage = update.Preimage
		}

	// Once settled, we are in a terminal state.
	case ContractSettled:
		return ErrInvoiceAlreadySettled

	// Once canceled, we are in a terminal state.
	case ContractCanceled:
		return ErrInvoiceAlreadyCanceled

	default:
		return errors.New("unknown state transition")
	}

	invoice.State = update.NewState

	return nil
}

// cancelSingleHtlc validates cancelation of a single htlc and update its state.
func cancelSingleHtlc(resolveTime time.Time, htlc *InvoiceHTLC,
	invState ContractState) error {

	// It is only possible to cancel individual htlcs on an open invoice.
	if invState != ContractOpen {
		return fmt.Errorf("htlc canceled on invoice in "+
			"state %v", invState)
	}

	// It is only possible if the htlc is still pending.
	if htlc.State != HtlcStateAccepted {
		return fmt.Errorf("htlc canceled in state %v",
			htlc.State)
	}

	htlc.State = HtlcStateCanceled
	htlc.ResolveTime = resolveTime

	return nil
}

// updateHtlc aligns the state of an htlc with the given invoice state.
func updateHtlc(resolveTime time.Time, htlc *InvoiceHTLC,
	invState ContractState) error {

	switch invState {

	case ContractSettled:
		if htlc.State == HtlcStateAccepted {
			htlc.State = HtlcStateSettled
			htlc.ResolveTime = resolveTime
		}

	case ContractCanceled:
		switch htlc.State {

		case HtlcStateAccepted:
			htlc.State = HtlcStateCanceled
			htlc.ResolveTime = resolveTime

		case HtlcStateSettled:
			return fmt.Errorf("cannot have a settled htlc with " +
				"invoice in state canceled")
		}

	case ContractOpen, ContractAccepted:
		if htlc.State == HtlcStateSettled {
			return fmt.Errorf("cannot have a settled htlc with "+
				"invoice in state %v", invState)
		}

	default:
		return errors.New("unknown state transition")
	}

	return nil
}

// setSettleMetaFields updates the metadata associated with settlement of an
// invoice.
func setSettleMetaFields(settleIndex *bbolt.Bucket, invoiceNum []byte,
	invoice *Invoice, now time.Time) error {

	// Now that we know the invoice hasn't already been settled, we'll
	// update the settle index so we can place this settle event in the
	// proper location within our time series.
	nextSettleSeqNo, err := settleIndex.NextSequence()
	if err != nil {
		return err
	}

	var seqNoBytes [8]byte
	byteOrder.PutUint64(seqNoBytes[:], nextSettleSeqNo)
	if err := settleIndex.Put(seqNoBytes[:], invoiceNum); err != nil {
		return err
	}

	invoice.SettleDate = now
	invoice.SettleIndex = nextSettleSeqNo

	return nil
}
