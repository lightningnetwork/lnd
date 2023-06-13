package channeldb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
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

	// payAddrIndexBucket is the name of the top-level bucket that maps
	// payment addresses to their invoice number. This can be used
	// to efficiently query or update non-legacy invoices. Note that legacy
	// invoices will not be included in this index since they all have the
	// same, all-zero payment address, however all newly generated invoices
	// will end up in this index.
	//
	// maps: payAddr => invoiceKey
	payAddrIndexBucket = []byte("pay-addr-index")

	// setIDIndexBucket is the name of the top-level bucket that maps set
	// ids to their invoice number. This can be used to efficiently query or
	// update AMP invoice. Note that legacy or MPP invoices will not be
	// included in this index, since their HTLCs do not have a set id.
	//
	// maps: setID => invoiceKey
	setIDIndexBucket = []byte("set-id-index")

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
)

const (
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
	htlcAMPType      tlv.Type = 19
	htlcHashType     tlv.Type = 21
	htlcPreimageType tlv.Type = 23

	// A set of tlv type definitions used to serialize invoice bodiees.
	//
	// NOTE: A migration should be added whenever this list changes. This
	// prevents against the database being rolled back to an older
	// format where the surrounding logic might assume a different set of
	// fields are known.
	memoType            tlv.Type = 0
	payReqType          tlv.Type = 1
	createTimeType      tlv.Type = 2
	settleTimeType      tlv.Type = 3
	addIndexType        tlv.Type = 4
	settleIndexType     tlv.Type = 5
	preimageType        tlv.Type = 6
	valueType           tlv.Type = 7
	cltvDeltaType       tlv.Type = 8
	expiryType          tlv.Type = 9
	paymentAddrType     tlv.Type = 10
	featuresType        tlv.Type = 11
	invStateType        tlv.Type = 12
	amtPaidType         tlv.Type = 13
	hodlInvoiceType     tlv.Type = 14
	invoiceAmpStateType tlv.Type = 15

	// A set of tlv type definitions used to serialize the invoice AMP
	// state along-side the main invoice body.
	ampStateSetIDType       tlv.Type = 0
	ampStateHtlcStateType   tlv.Type = 1
	ampStateSettleIndexType tlv.Type = 2
	ampStateSettleDateType  tlv.Type = 3
	ampStateCircuitKeysType tlv.Type = 4
	ampStateAmtPaidType     tlv.Type = 5
)

// AddInvoice inserts the targeted invoice into the database. If the invoice has
// *any* payment hashes which already exists within the database, then the
// insertion will be aborted and rejected due to the strict policy banning any
// duplicate payment hashes. A side effect of this function is that it sets
// AddIndex on newInvoice.
func (d *DB) AddInvoice(newInvoice *invpkg.Invoice, paymentHash lntypes.Hash) (
	uint64, error) {

	if err := invpkg.ValidateInvoice(newInvoice, paymentHash); err != nil {
		return 0, err
	}

	var invoiceAddIndex uint64
	err := kvdb.Update(d, func(tx kvdb.RwTx) error {
		invoices, err := tx.CreateTopLevelBucket(invoiceBucket)
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
			return invpkg.ErrDuplicateInvoice
		}

		// Check that we aren't inserting an invoice with a duplicate
		// payment address. The all-zeros payment address is
		// special-cased to support legacy keysend invoices which don't
		// assign one. This is safe since later we also will avoid
		// indexing them and avoid collisions.
		payAddrIndex := tx.ReadWriteBucket(payAddrIndexBucket)
		if newInvoice.Terms.PaymentAddr != invpkg.BlankPayAddr {
			paymentAddr := newInvoice.Terms.PaymentAddr[:]
			if payAddrIndex.Get(paymentAddr) != nil {
				return invpkg.ErrDuplicatePayAddr
			}
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
			invoices, invoiceIndex, payAddrIndex, addIndex,
			newInvoice, invoiceNum, paymentHash,
		)
		if err != nil {
			return err
		}

		invoiceAddIndex = newIndex
		return nil
	}, func() {
		invoiceAddIndex = 0
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
func (d *DB) InvoicesAddedSince(sinceAddIndex uint64) ([]invpkg.Invoice,
	error) {

	var newInvoices []invpkg.Invoice

	// If an index of zero was specified, then in order to maintain
	// backwards compat, we won't send out any new invoices.
	if sinceAddIndex == 0 {
		return newInvoices, nil
	}

	var startIndex [8]byte
	byteOrder.PutUint64(startIndex[:], sinceAddIndex)

	err := kvdb.View(d, func(tx kvdb.RTx) error {
		invoices := tx.ReadBucket(invoiceBucket)
		if invoices == nil {
			return nil
		}

		addIndex := invoices.NestedReadBucket(addIndexBucket)
		if addIndex == nil {
			return nil
		}

		// We'll now run through each entry in the add index starting
		// at our starting index. We'll continue until we reach the
		// very end of the current key space.
		invoiceCursor := addIndex.ReadCursor()

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
	}, func() {
		newInvoices = nil
	})
	if err != nil {
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
func (d *DB) LookupInvoice(ref invpkg.InvoiceRef) (invpkg.Invoice, error) {
	var invoice invpkg.Invoice
	err := kvdb.View(d, func(tx kvdb.RTx) error {
		invoices := tx.ReadBucket(invoiceBucket)
		if invoices == nil {
			return invpkg.ErrNoInvoicesCreated
		}
		invoiceIndex := invoices.NestedReadBucket(invoiceIndexBucket)
		if invoiceIndex == nil {
			return invpkg.ErrNoInvoicesCreated
		}
		payAddrIndex := tx.ReadBucket(payAddrIndexBucket)
		setIDIndex := tx.ReadBucket(setIDIndexBucket)

		// Retrieve the invoice number for this invoice using
		// the provided invoice reference.
		invoiceNum, err := fetchInvoiceNumByRef(
			invoiceIndex, payAddrIndex, setIDIndex, ref,
		)
		if err != nil {
			return err
		}

		var setID *invpkg.SetID
		switch {
		// If this is a payment address ref, and the blank modified was
		// specified, then we'll use the zero set ID to indicate that
		// we won't want any HTLCs returned.
		case ref.PayAddr() != nil &&
			ref.Modifier() == invpkg.HtlcSetBlankModifier:

			var zeroSetID invpkg.SetID
			setID = &zeroSetID

		// If this is a set ID ref, and the htlc set only modified was
		// specified, then we'll pass through the specified setID so
		// only that will be returned.
		case ref.SetID() != nil &&
			ref.Modifier() == invpkg.HtlcSetOnlyModifier:

			setID = (*invpkg.SetID)(ref.SetID())
		}

		// An invoice was found, retrieve the remainder of the invoice
		// body.
		i, err := fetchInvoice(invoiceNum, invoices, setID)
		if err != nil {
			return err
		}
		invoice = i

		return nil
	}, func() {})
	if err != nil {
		return invoice, err
	}

	return invoice, nil
}

// fetchInvoiceNumByRef retrieve the invoice number for the provided invoice
// reference. The payment address will be treated as the primary key, falling
// back to the payment hash if nothing is found for the payment address. An
// error is returned if the invoice is not found.
func fetchInvoiceNumByRef(invoiceIndex, payAddrIndex, setIDIndex kvdb.RBucket,
	ref invpkg.InvoiceRef) ([]byte, error) {

	// If the set id is present, we only consult the set id index for this
	// invoice. This type of query is only used to facilitate user-facing
	// requests to lookup, settle or cancel an AMP invoice.
	setID := ref.SetID()
	if setID != nil {
		invoiceNumBySetID := setIDIndex.Get(setID[:])
		if invoiceNumBySetID == nil {
			return nil, invpkg.ErrInvoiceNotFound
		}

		return invoiceNumBySetID, nil
	}

	payHash := ref.PayHash()
	payAddr := ref.PayAddr()

	getInvoiceNumByHash := func() []byte {
		if payHash != nil {
			return invoiceIndex.Get(payHash[:])
		}
		return nil
	}

	getInvoiceNumByAddr := func() []byte {
		if payAddr != nil {
			// Only allow lookups for payment address if it is not a
			// blank payment address, which is a special-cased value
			// for legacy keysend invoices.
			if *payAddr != invpkg.BlankPayAddr {
				return payAddrIndex.Get(payAddr[:])
			}
		}
		return nil
	}

	invoiceNumByHash := getInvoiceNumByHash()
	invoiceNumByAddr := getInvoiceNumByAddr()
	switch {
	// If payment address and payment hash both reference an existing
	// invoice, ensure they reference the _same_ invoice.
	case invoiceNumByAddr != nil && invoiceNumByHash != nil:
		if !bytes.Equal(invoiceNumByAddr, invoiceNumByHash) {
			return nil, invpkg.ErrInvRefEquivocation
		}

		return invoiceNumByAddr, nil

	// Return invoices by payment addr only.
	//
	// NOTE: We constrain this lookup to only apply if the invoice ref does
	// not contain a payment hash. Legacy and MPP payments depend on the
	// payment hash index to enforce that the HTLCs payment hash matches the
	// payment hash for the invoice, without this check we would
	// inadvertently assume the invoice contains the correct preimage for
	// the HTLC, which we only enforce via the lookup by the invoice index.
	case invoiceNumByAddr != nil && payHash == nil:
		return invoiceNumByAddr, nil

	// If we were only able to reference the invoice by hash, return the
	// corresponding invoice number. This can happen when no payment address
	// was provided, or if it didn't match anything in our records.
	case invoiceNumByHash != nil:
		return invoiceNumByHash, nil

	// Otherwise we don't know of the target invoice.
	default:
		return nil, invpkg.ErrInvoiceNotFound
	}
}

// ScanInvoices scans through all invoices and calls the passed scanFunc for
// for each invoice with its respective payment hash. Additionally a reset()
// closure is passed which is used to reset/initialize partial results and also
// to signal if the kvdb.View transaction has been retried.
func (d *DB) ScanInvoices(scanFunc invpkg.InvScanFunc, reset func()) error {
	return kvdb.View(d, func(tx kvdb.RTx) error {
		invoices := tx.ReadBucket(invoiceBucket)
		if invoices == nil {
			return invpkg.ErrNoInvoicesCreated
		}

		invoiceIndex := invoices.NestedReadBucket(invoiceIndexBucket)
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

			// Skip sub-buckets.
			if v == nil {
				return nil
			}

			invoice, err := fetchInvoice(v, invoices)
			if err != nil {
				return err
			}

			var paymentHash lntypes.Hash
			copy(paymentHash[:], k)

			return scanFunc(paymentHash, &invoice)
		})
	}, reset)
}

// QueryInvoices allows a caller to query the invoice database for invoices
// within the specified add index range.
func (d *DB) QueryInvoices(q invpkg.InvoiceQuery) (invpkg.InvoiceSlice,
	error) {

	var (
		resp         invpkg.InvoiceSlice
		startDateSet = !q.CreationDateStart.IsZero()
		endDateSet   = !q.CreationDateEnd.IsZero()
	)

	err := kvdb.View(d, func(tx kvdb.RTx) error {
		// If the bucket wasn't found, then there aren't any invoices
		// within the database yet, so we can simply exit.
		invoices := tx.ReadBucket(invoiceBucket)
		if invoices == nil {
			return invpkg.ErrNoInvoicesCreated
		}

		// Get the add index bucket which we will use to iterate through
		// our indexed invoices.
		invoiceAddIndex := invoices.NestedReadBucket(addIndexBucket)
		if invoiceAddIndex == nil {
			return invpkg.ErrNoInvoicesCreated
		}

		// Create a paginator which reads from our add index bucket with
		// the parameters provided by the invoice query.
		paginator := newPaginator(
			invoiceAddIndex.ReadCursor(), q.Reversed, q.IndexOffset,
			q.NumMaxInvoices,
		)

		// accumulateInvoices looks up an invoice based on the index we
		// are given, adds it to our set of invoices if it has the right
		// characteristics for our query and returns the number of items
		// we have added to our set of invoices.
		accumulateInvoices := func(_, indexValue []byte) (bool, error) {
			invoice, err := fetchInvoice(indexValue, invoices)
			if err != nil {
				return false, err
			}

			// Skip any settled or canceled invoices if the caller
			// is only interested in pending ones.
			if q.PendingOnly && !invoice.IsPending() {
				return false, nil
			}

			// Skip any invoices that were created before the
			// specified time.
			if startDateSet && invoice.CreationDate.Before(
				q.CreationDateStart,
			) {

				return false, nil
			}

			// Skip any invoices that were created after the
			// specified time.
			if endDateSet && invoice.CreationDate.After(
				q.CreationDateEnd,
			) {

				return false, nil
			}

			// At this point, we've exhausted the offset, so we'll
			// begin collecting invoices found within the range.
			resp.Invoices = append(resp.Invoices, invoice)
			return true, nil
		}

		// Query our paginator using accumulateInvoices to build up a
		// set of invoices.
		if err := paginator.query(accumulateInvoices); err != nil {
			return err
		}

		// If we iterated through the add index in reverse order, then
		// we'll need to reverse the slice of invoices to return them in
		// forward order.
		if q.Reversed {
			numInvoices := len(resp.Invoices)
			for i := 0; i < numInvoices/2; i++ {
				reverse := numInvoices - i - 1
				resp.Invoices[i], resp.Invoices[reverse] =
					resp.Invoices[reverse], resp.Invoices[i]
			}
		}

		return nil
	}, func() {
		resp = invpkg.InvoiceSlice{
			InvoiceQuery: q,
		}
	})
	if err != nil && !errors.Is(err, invpkg.ErrNoInvoicesCreated) {
		return resp, err
	}

	// Finally, record the indexes of the first and last invoices returned
	// so that the caller can resume from this point later on.
	if len(resp.Invoices) > 0 {
		resp.FirstIndexOffset = resp.Invoices[0].AddIndex
		lastIdx := len(resp.Invoices) - 1
		resp.LastIndexOffset = resp.Invoices[lastIdx].AddIndex
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
func (d *DB) UpdateInvoice(ref invpkg.InvoiceRef, setIDHint *invpkg.SetID,
	callback invpkg.InvoiceUpdateCallback) (*invpkg.Invoice, error) {

	var updatedInvoice *invpkg.Invoice
	err := kvdb.Update(d, func(tx kvdb.RwTx) error {
		invoices, err := tx.CreateTopLevelBucket(invoiceBucket)
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
		payAddrIndex := tx.ReadBucket(payAddrIndexBucket)
		setIDIndex := tx.ReadWriteBucket(setIDIndexBucket)

		// Retrieve the invoice number for this invoice using the
		// provided invoice reference.
		invoiceNum, err := fetchInvoiceNumByRef(
			invoiceIndex, payAddrIndex, setIDIndex, ref,
		)
		if err != nil {
			return err
		}

		payHash := ref.PayHash()
		updatedInvoice, err = d.updateInvoice(
			payHash, setIDHint, invoices, settleIndex, setIDIndex,
			invoiceNum, callback,
		)

		return err
	}, func() {
		updatedInvoice = nil
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
func (d *DB) InvoicesSettledSince(sinceSettleIndex uint64) ([]invpkg.Invoice,
	error) {

	var settledInvoices []invpkg.Invoice

	// If an index of zero was specified, then in order to maintain
	// backwards compat, we won't send out any new invoices.
	if sinceSettleIndex == 0 {
		return settledInvoices, nil
	}

	var startIndex [8]byte
	byteOrder.PutUint64(startIndex[:], sinceSettleIndex)

	err := kvdb.View(d, func(tx kvdb.RTx) error {
		invoices := tx.ReadBucket(invoiceBucket)
		if invoices == nil {
			return nil
		}

		settleIndex := invoices.NestedReadBucket(settleIndexBucket)
		if settleIndex == nil {
			return nil
		}

		// We'll now run through each entry in the add index starting
		// at our starting index. We'll continue until we reach the
		// very end of the current key space.
		invoiceCursor := settleIndex.ReadCursor()

		// We'll seek to the starting index, then manually advance the
		// cursor in order to skip the entry with the since add index.
		invoiceCursor.Seek(startIndex[:])
		seqNo, indexValue := invoiceCursor.Next()

		for ; seqNo != nil && bytes.Compare(seqNo, startIndex[:]) > 0; seqNo, indexValue = invoiceCursor.Next() {
			// Depending on the length of the index value, this may
			// or may not be an AMP invoice, so we'll extract the
			// invoice value into two components: the invoice num,
			// and the setID (may not be there).
			var (
				invoiceKey [4]byte
				setID      *invpkg.SetID
			)

			valueLen := copy(invoiceKey[:], indexValue)
			if len(indexValue) == invoiceSetIDKeyLen {
				setID = new(invpkg.SetID)
				copy(setID[:], indexValue[valueLen:])
			}

			// For each key found, we'll look up the actual
			// invoice, then accumulate it into our return value.
			invoice, err := fetchInvoice(
				invoiceKey[:], invoices, setID,
			)
			if err != nil {
				return err
			}

			settledInvoices = append(settledInvoices, invoice)
		}

		return nil
	}, func() {
		settledInvoices = nil
	})
	if err != nil {
		return nil, err
	}

	return settledInvoices, nil
}

func putInvoice(invoices, invoiceIndex, payAddrIndex, addIndex kvdb.RwBucket,
	i *invpkg.Invoice, invoiceNum uint32, paymentHash lntypes.Hash) (
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

	// Add the invoice to the payment address index, but only if the invoice
	// has a non-zero payment address. The all-zero payment address is still
	// in use by legacy keysend, so we special-case here to avoid
	// collisions.
	if i.Terms.PaymentAddr != invpkg.BlankPayAddr {
		err = payAddrIndex.Put(i.Terms.PaymentAddr[:], invoiceKey[:])
		if err != nil {
			return 0, err
		}
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

// recordSize returns the amount of bytes this TLV record will occupy when
// encoded.
func ampRecordSize(a *invpkg.AMPInvoiceState) func() uint64 {
	var (
		b   bytes.Buffer
		buf [8]byte
	)

	// We know that encoding works since the tests pass in the build this
	// file is checked into, so we'll simplify things and simply encode it
	// ourselves then report the total amount of bytes used.
	if err := ampStateEncoder(&b, a, &buf); err != nil {
		// This should never error out, but we log it just in case it
		// does.
		log.Errorf("encoding the amp invoice state failed: %v", err)
	}

	return func() uint64 {
		return uint64(len(b.Bytes()))
	}
}

// serializeInvoice serializes an invoice to a writer.
//
// Note: this function is in use for a migration. Before making changes that
// would modify the on disk format, make a copy of the original code and store
// it with the migration.
func serializeInvoice(w io.Writer, i *invpkg.Invoice) error {
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

	preimage := [32]byte(invpkg.UnknownPreimage)
	if i.Terms.PaymentPreimage != nil {
		preimage = *i.Terms.PaymentPreimage
		if preimage == invpkg.UnknownPreimage {
			return errors.New("cannot use all-zeroes preimage")
		}
	}
	value := uint64(i.Terms.Value)
	cltvDelta := uint32(i.Terms.FinalCltvDelta)
	expiry := uint64(i.Terms.Expiry)

	amtPaid := uint64(i.AmtPaid)
	state := uint8(i.State)

	var hodlInvoice uint8
	if i.HodlInvoice {
		hodlInvoice = 1
	}

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

		tlv.MakePrimitiveRecord(hodlInvoiceType, &hodlInvoice),

		// Invoice AMP state.
		tlv.MakeDynamicRecord(
			invoiceAmpStateType, &i.AMPState,
			ampRecordSize(&i.AMPState),
			ampStateEncoder, ampStateDecoder,
		),
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

	// Only if this is a _non_ AMP invoice do we serialize the HTLCs
	// in-line with the rest of the invoice.
	if i.IsAMP() {
		return nil
	}

	return serializeHtlcs(w, i.Htlcs)
}

// serializeHtlcs serializes a map containing circuit keys and invoice htlcs to
// a writer.
func serializeHtlcs(w io.Writer,
	htlcs map[models.CircuitKey]*invpkg.InvoiceHTLC) error {

	for key, htlc := range htlcs {
		// Encode the htlc in a tlv stream.
		chanID := key.ChanID.ToUint64()
		amt := uint64(htlc.Amt)
		mppTotalAmt := uint64(htlc.MppTotalAmt)
		acceptTime := putNanoTime(htlc.AcceptTime)
		resolveTime := putNanoTime(htlc.ResolveTime)
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

		if htlc.AMP != nil {
			setIDRecord := tlv.MakeDynamicRecord(
				htlcAMPType, &htlc.AMP.Record,
				htlc.AMP.Record.PayloadSize,
				record.AMPEncoder, record.AMPDecoder,
			)
			records = append(records, setIDRecord)

			hash32 := [32]byte(htlc.AMP.Hash)
			hashRecord := tlv.MakePrimitiveRecord(
				htlcHashType, &hash32,
			)
			records = append(records, hashRecord)

			if htlc.AMP.Preimage != nil {
				preimage32 := [32]byte(*htlc.AMP.Preimage)
				preimageRecord := tlv.MakePrimitiveRecord(
					htlcPreimageType, &preimage32,
				)
				records = append(records, preimageRecord)
			}
		}

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

// putNanoTime returns the unix nano time for the passed timestamp. A zero-value
// timestamp will be mapped to 0, since calling UnixNano in that case is
// undefined.
func putNanoTime(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	return uint64(t.UnixNano())
}

// getNanoTime returns a timestamp for the given number of nano seconds. If zero
// is provided, an zero-value time stamp is returned.
func getNanoTime(ns uint64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(ns))
}

// fetchFilteredAmpInvoices retrieves only a select set of AMP invoices
// identified by the setID value.
func fetchFilteredAmpInvoices(invoiceBucket kvdb.RBucket, invoiceNum []byte,
	setIDs ...*invpkg.SetID) (map[models.CircuitKey]*invpkg.InvoiceHTLC,
	error) {

	htlcs := make(map[models.CircuitKey]*invpkg.InvoiceHTLC)
	for _, setID := range setIDs {
		invoiceSetIDKey := makeInvoiceSetIDKey(invoiceNum, setID[:])

		htlcSetBytes := invoiceBucket.Get(invoiceSetIDKey[:])
		if htlcSetBytes == nil {
			// A set ID was passed in, but we don't have this
			// stored yet, meaning that the setID is being added
			// for the first time.
			return htlcs, invpkg.ErrInvoiceNotFound
		}

		htlcSetReader := bytes.NewReader(htlcSetBytes)
		htlcsBySetID, err := deserializeHtlcs(htlcSetReader)
		if err != nil {
			return nil, err
		}

		for key, htlc := range htlcsBySetID {
			htlcs[key] = htlc
		}
	}

	return htlcs, nil
}

// forEachAMPInvoice is a helper function that attempts to iterate over each of
// the HTLC sets (based on their set ID) for the given AMP invoice identified
// by its invoiceNum. The callback closure is called for each key within the
// prefix range.
func forEachAMPInvoice(invoiceBucket kvdb.RBucket, invoiceNum []byte,
	callback func(key, htlcSet []byte) error) error {

	invoiceCursor := invoiceBucket.ReadCursor()

	// Seek to the first key that includes the invoice data itself.
	invoiceCursor.Seek(invoiceNum)

	// Advance to the very first key _after_ the invoice data, as this is
	// where we'll encounter our first HTLC (if any are present).
	cursorKey, htlcSet := invoiceCursor.Next()

	// If at this point, the cursor key doesn't match the invoice num
	// prefix, then we know that this HTLC doesn't have any set ID HTLCs
	// associated with it.
	if !bytes.HasPrefix(cursorKey, invoiceNum) {
		return nil
	}

	// Otherwise continue to iterate until we no longer match the prefix,
	// executing the call back at each step.
	for ; cursorKey != nil && bytes.HasPrefix(cursorKey, invoiceNum); cursorKey, htlcSet = invoiceCursor.Next() {
		err := callback(cursorKey, htlcSet)
		if err != nil {
			return err
		}
	}

	return nil
}

// fetchAmpSubInvoices attempts to use the invoiceNum as a prefix  within the
// AMP bucket to find all the individual HTLCs (by setID) associated with a
// given invoice. If a list of set IDs are specified, then only HTLCs
// associated with that setID will be retrieved.
func fetchAmpSubInvoices(invoiceBucket kvdb.RBucket, invoiceNum []byte,
	setIDs ...*invpkg.SetID) (map[models.CircuitKey]*invpkg.InvoiceHTLC,
	error) {

	// If a set of setIDs was specified, then we can skip the cursor and
	// just read out exactly what we need.
	if len(setIDs) != 0 && setIDs[0] != nil {
		return fetchFilteredAmpInvoices(
			invoiceBucket, invoiceNum, setIDs...,
		)
	}

	// Otherwise, iterate over all the htlc sets that are prefixed beside
	// this invoice in the main invoice bucket.
	htlcs := make(map[models.CircuitKey]*invpkg.InvoiceHTLC)
	err := forEachAMPInvoice(invoiceBucket, invoiceNum,
		func(key, htlcSet []byte) error {
			htlcSetReader := bytes.NewReader(htlcSet)
			htlcsBySetID, err := deserializeHtlcs(htlcSetReader)
			if err != nil {
				return err
			}

			for key, htlc := range htlcsBySetID {
				htlcs[key] = htlc
			}

			return nil
		},
	)

	if err != nil {
		return nil, err
	}

	return htlcs, nil
}

// fetchInvoice attempts to read out the relevant state for the invoice as
// specified by the invoice number. If the setID fields are set, then only the
// HTLC information pertaining to those set IDs is returned.
func fetchInvoice(invoiceNum []byte, invoices kvdb.RBucket,
	setIDs ...*invpkg.SetID) (invpkg.Invoice, error) {

	invoiceBytes := invoices.Get(invoiceNum)
	if invoiceBytes == nil {
		return invpkg.Invoice{}, invpkg.ErrInvoiceNotFound
	}

	invoiceReader := bytes.NewReader(invoiceBytes)

	invoice, err := deserializeInvoice(invoiceReader)
	if err != nil {
		return invpkg.Invoice{}, err
	}

	// If this is an AMP invoice we'll also attempt to read out the set of
	// HTLCs that were paid to prior set IDs, if needed.
	if !invoice.IsAMP() {
		return invoice, nil
	}

	if shouldFetchAMPHTLCs(invoice, setIDs) {
		invoice.Htlcs, err = fetchAmpSubInvoices(
			invoices, invoiceNum, setIDs...,
		)
		// TODO(positiveblue): we should fail when we are not able to
		// fetch all the HTLCs for an AMP invoice. Multiple tests in
		// the invoice and channeldb package break if we return this
		// error. We need to update them when we migrate this logic to
		// the sql implementation.
		if err != nil {
			log.Errorf("unable to fetch amp htlcs for inv "+
				"%v and setIDs %v: %w", invoiceNum, setIDs, err)
		}
	}

	return invoice, nil
}

// shouldFetchAMPHTLCs returns true if we need to fetch the set of HTLCs that
// were paid to the relevant set IDs.
func shouldFetchAMPHTLCs(invoice invpkg.Invoice, setIDs []*invpkg.SetID) bool {
	// For AMP invoice that already have HTLCs populated (created before
	// recurring invoices), then we don't need to read from the prefix
	// keyed section of the bucket.
	if len(invoice.Htlcs) != 0 {
		return false
	}

	// If the "zero" setID was specified, then this means that no HTLC data
	// should be returned alongside of it.
	if len(setIDs) != 0 && setIDs[0] != nil &&
		*setIDs[0] == invpkg.BlankPayAddr {

		return false
	}

	return true
}

// fetchInvoiceStateAMP retrieves the state of all the relevant sub-invoice for
// an AMP invoice. This methods only decode the relevant state vs the entire
// invoice.
func fetchInvoiceStateAMP(invoiceNum []byte,
	invoices kvdb.RBucket) (invpkg.AMPInvoiceState, error) {

	// Fetch the raw invoice bytes.
	invoiceBytes := invoices.Get(invoiceNum)
	if invoiceBytes == nil {
		return nil, invpkg.ErrInvoiceNotFound
	}

	r := bytes.NewReader(invoiceBytes)

	var bodyLen int64
	err := binary.Read(r, byteOrder, &bodyLen)
	if err != nil {
		return nil, err
	}

	// Next, we'll make a new TLV stream that only attempts to decode the
	// bytes we actually need.
	ampState := make(invpkg.AMPInvoiceState)
	tlvStream, err := tlv.NewStream(
		// Invoice AMP state.
		tlv.MakeDynamicRecord(
			invoiceAmpStateType, &ampState, nil,
			ampStateEncoder, ampStateDecoder,
		),
	)
	if err != nil {
		return nil, err
	}

	invoiceReader := io.LimitReader(r, bodyLen)
	if err = tlvStream.Decode(invoiceReader); err != nil {
		return nil, err
	}

	return ampState, nil
}

func deserializeInvoice(r io.Reader) (invpkg.Invoice, error) {
	var (
		preimageBytes [32]byte
		value         uint64
		cltvDelta     uint32
		expiry        uint64
		amtPaid       uint64
		state         uint8
		hodlInvoice   uint8

		creationDateBytes []byte
		settleDateBytes   []byte
		featureBytes      []byte
	)

	var i invpkg.Invoice
	i.AMPState = make(invpkg.AMPInvoiceState)
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
		tlv.MakePrimitiveRecord(preimageType, &preimageBytes),
		tlv.MakePrimitiveRecord(valueType, &value),
		tlv.MakePrimitiveRecord(cltvDeltaType, &cltvDelta),
		tlv.MakePrimitiveRecord(expiryType, &expiry),
		tlv.MakePrimitiveRecord(paymentAddrType, &i.Terms.PaymentAddr),
		tlv.MakePrimitiveRecord(featuresType, &featureBytes),

		// Invoice state.
		tlv.MakePrimitiveRecord(invStateType, &state),
		tlv.MakePrimitiveRecord(amtPaidType, &amtPaid),

		tlv.MakePrimitiveRecord(hodlInvoiceType, &hodlInvoice),

		// Invoice AMP state.
		tlv.MakeDynamicRecord(
			invoiceAmpStateType, &i.AMPState, nil,
			ampStateEncoder, ampStateDecoder,
		),
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

	preimage := lntypes.Preimage(preimageBytes)
	if preimage != invpkg.UnknownPreimage {
		i.Terms.PaymentPreimage = &preimage
	}

	i.Terms.Value = lnwire.MilliSatoshi(value)
	i.Terms.FinalCltvDelta = int32(cltvDelta)
	i.Terms.Expiry = time.Duration(expiry)
	i.AmtPaid = lnwire.MilliSatoshi(amtPaid)
	i.State = invpkg.ContractState(state)

	if hodlInvoice != 0 {
		i.HodlInvoice = true
	}

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

func encodeCircuitKeys(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*map[models.CircuitKey]struct{}); ok {
		// We encode the set of circuit keys as a varint length prefix.
		// followed by a series of fixed sized uint8 integers.
		numKeys := uint64(len(*v))

		if err := tlv.WriteVarInt(w, numKeys, buf); err != nil {
			return err
		}

		for key := range *v {
			scidInt := key.ChanID.ToUint64()

			if err := tlv.EUint64(w, &scidInt, buf); err != nil {
				return err
			}
			if err := tlv.EUint64(w, &key.HtlcID, buf); err != nil {
				return err
			}
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "*map[CircuitKey]struct{}")
}

func decodeCircuitKeys(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if v, ok := val.(*map[models.CircuitKey]struct{}); ok {
		// First, we'll read out the varint that encodes the number of
		// circuit keys encoded.
		numKeys, err := tlv.ReadVarInt(r, buf)
		if err != nil {
			return err
		}

		// Now that we know how many keys to expect, iterate reading
		// each one until we're done.
		for i := uint64(0); i < numKeys; i++ {
			var (
				key  models.CircuitKey
				scid uint64
			)

			if err := tlv.DUint64(r, &scid, buf, 8); err != nil {
				return err
			}

			key.ChanID = lnwire.NewShortChanIDFromInt(scid)

			err := tlv.DUint64(r, &key.HtlcID, buf, 8)
			if err != nil {
				return err
			}

			(*v)[key] = struct{}{}
		}

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "*map[CircuitKey]struct{}", l, l)
}

// ampStateEncoder is a custom TLV encoder for the AMPInvoiceState record.
func ampStateEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*invpkg.AMPInvoiceState); ok {
		// We'll encode the AMP state as a series of KV pairs on the
		// wire with a length prefix.
		numRecords := uint64(len(*v))

		// First, we'll write out the number of records as a var int.
		if err := tlv.WriteVarInt(w, numRecords, buf); err != nil {
			return err
		}

		// With that written out, we'll now encode the entries
		// themselves as a sub-TLV record, which includes its _own_
		// inner length prefix.
		for setID, ampState := range *v {
			setID := [32]byte(setID)
			ampState := ampState

			htlcState := uint8(ampState.State)
			settleDate := ampState.SettleDate
			settleDateBytes, err := settleDate.MarshalBinary()
			if err != nil {
				return err
			}

			amtPaid := uint64(ampState.AmtPaid)

			var ampStateTlvBytes bytes.Buffer
			tlvStream, err := tlv.NewStream(
				tlv.MakePrimitiveRecord(
					ampStateSetIDType, &setID,
				),
				tlv.MakePrimitiveRecord(
					ampStateHtlcStateType, &htlcState,
				),
				tlv.MakePrimitiveRecord(
					ampStateSettleIndexType,
					&ampState.SettleIndex,
				),
				tlv.MakePrimitiveRecord(
					ampStateSettleDateType,
					&settleDateBytes,
				),
				tlv.MakeDynamicRecord(
					ampStateCircuitKeysType,
					&ampState.InvoiceKeys,
					func() uint64 {
						// The record takes 8 bytes to
						// encode the set of circuits,
						// 8 bytes for the scid for the
						// key, and 8 bytes for the HTLC
						// index.
						keys := ampState.InvoiceKeys
						numKeys := uint64(len(keys))
						size := tlv.VarIntSize(numKeys)
						dataSize := (numKeys * 16)

						return size + dataSize
					},
					encodeCircuitKeys, decodeCircuitKeys,
				),
				tlv.MakePrimitiveRecord(
					ampStateAmtPaidType, &amtPaid,
				),
			)
			if err != nil {
				return err
			}

			err = tlvStream.Encode(&ampStateTlvBytes)
			if err != nil {
				return err
			}

			// We encode the record with a varint length followed by
			// the _raw_ TLV bytes.
			tlvLen := uint64(len(ampStateTlvBytes.Bytes()))
			if err := tlv.WriteVarInt(w, tlvLen, buf); err != nil {
				return err
			}

			_, err = w.Write(ampStateTlvBytes.Bytes())
			if err != nil {
				return err
			}
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "channeldb.AMPInvoiceState")
}

// ampStateDecoder is a custom TLV decoder for the AMPInvoiceState record.
func ampStateDecoder(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if v, ok := val.(*invpkg.AMPInvoiceState); ok {
		// First, we'll decode the varint that encodes how many set IDs
		// are encoded within the greater map.
		numRecords, err := tlv.ReadVarInt(r, buf)
		if err != nil {
			return err
		}

		// Now that we know how many records we'll need to read, we can
		// iterate and read them all out in series.
		for i := uint64(0); i < numRecords; i++ {
			// Read out the varint that encodes the size of this
			// inner TLV record.
			stateRecordSize, err := tlv.ReadVarInt(r, buf)
			if err != nil {
				return err
			}

			// Using this information, we'll create a new limited
			// reader that'll return an EOF once the end has been
			// reached so the stream stops consuming bytes.
			innerTlvReader := io.LimitedReader{
				R: r,
				N: int64(stateRecordSize),
			}

			var (
				setID           [32]byte
				htlcState       uint8
				settleIndex     uint64
				settleDateBytes []byte
				invoiceKeys     = make(
					map[models.CircuitKey]struct{},
				)
				amtPaid uint64
			)
			tlvStream, err := tlv.NewStream(
				tlv.MakePrimitiveRecord(
					ampStateSetIDType, &setID,
				),
				tlv.MakePrimitiveRecord(
					ampStateHtlcStateType, &htlcState,
				),
				tlv.MakePrimitiveRecord(
					ampStateSettleIndexType, &settleIndex,
				),
				tlv.MakePrimitiveRecord(
					ampStateSettleDateType,
					&settleDateBytes,
				),
				tlv.MakeDynamicRecord(
					ampStateCircuitKeysType,
					&invoiceKeys, nil,
					encodeCircuitKeys, decodeCircuitKeys,
				),
				tlv.MakePrimitiveRecord(
					ampStateAmtPaidType, &amtPaid,
				),
			)
			if err != nil {
				return err
			}

			err = tlvStream.Decode(&innerTlvReader)
			if err != nil {
				return err
			}

			var settleDate time.Time
			err = settleDate.UnmarshalBinary(settleDateBytes)
			if err != nil {
				return err
			}

			(*v)[setID] = invpkg.InvoiceStateAMP{
				State:       invpkg.HtlcState(htlcState),
				SettleIndex: settleIndex,
				SettleDate:  settleDate,
				InvoiceKeys: invoiceKeys,
				AmtPaid:     lnwire.MilliSatoshi(amtPaid),
			}
		}

		return nil
	}

	return tlv.NewTypeForDecodingErr(
		val, "channeldb.AMPInvoiceState", l, l,
	)
}

// deserializeHtlcs reads a list of invoice htlcs from a reader and returns it
// as a map.
func deserializeHtlcs(r io.Reader) (map[models.CircuitKey]*invpkg.InvoiceHTLC,
	error) {

	htlcs := make(map[models.CircuitKey]*invpkg.InvoiceHTLC)
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
			htlc                    invpkg.InvoiceHTLC
			key                     models.CircuitKey
			chanID                  uint64
			state                   uint8
			acceptTime, resolveTime uint64
			amt, mppTotalAmt        uint64
			amp                     = &record.AMP{}
			hash32                  = &[32]byte{}
			preimage32              = &[32]byte{}
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
			tlv.MakeDynamicRecord(
				htlcAMPType, amp, amp.PayloadSize,
				record.AMPEncoder, record.AMPDecoder,
			),
			tlv.MakePrimitiveRecord(htlcHashType, hash32),
			tlv.MakePrimitiveRecord(htlcPreimageType, preimage32),
		)
		if err != nil {
			return nil, err
		}

		parsedTypes, err := tlvStream.DecodeWithParsedTypes(htlcReader)
		if err != nil {
			return nil, err
		}

		if _, ok := parsedTypes[htlcAMPType]; !ok {
			amp = nil
		}

		var preimage *lntypes.Preimage
		if _, ok := parsedTypes[htlcPreimageType]; ok {
			pimg := lntypes.Preimage(*preimage32)
			preimage = &pimg
		}

		var hash *lntypes.Hash
		if _, ok := parsedTypes[htlcHashType]; ok {
			h := lntypes.Hash(*hash32)
			hash = &h
		}

		key.ChanID = lnwire.NewShortChanIDFromInt(chanID)
		htlc.AcceptTime = getNanoTime(acceptTime)
		htlc.ResolveTime = getNanoTime(resolveTime)
		htlc.State = invpkg.HtlcState(state)
		htlc.Amt = lnwire.MilliSatoshi(amt)
		htlc.MppTotalAmt = lnwire.MilliSatoshi(mppTotalAmt)
		if amp != nil && hash != nil {
			htlc.AMP = &invpkg.InvoiceHtlcAMPData{
				Record:   *amp,
				Hash:     *hash,
				Preimage: preimage,
			}
		}

		// Reconstruct the custom records fields from the parsed types
		// map return from the tlv parser.
		htlc.CustomRecords = hop.NewCustomRecords(parsedTypes)

		htlcs[key] = &htlc
	}

	return htlcs, nil
}

// invoiceSetIDKeyLen is the length of the key that's used to store the
// individual HTLCs prefixed by their ID along side the main invoice within the
// invoiceBytes. We use 4 bytes for the invoice number, and 32 bytes for the
// set ID.
const invoiceSetIDKeyLen = 4 + 32

// makeInvoiceSetIDKey returns the prefix key, based on the set ID and invoice
// number where the HTLCs for this setID will be stored udner.
func makeInvoiceSetIDKey(invoiceNum, setID []byte) [invoiceSetIDKeyLen]byte {
	// Construct the prefix key we need to obtain the invoice information:
	// invoiceNum || setID.
	var invoiceSetIDKey [invoiceSetIDKeyLen]byte
	copy(invoiceSetIDKey[:], invoiceNum)
	copy(invoiceSetIDKey[len(invoiceNum):], setID)

	return invoiceSetIDKey
}

// updateAMPInvoices updates the set of AMP invoices in-place. For AMP, rather
// then continually write the invoices to the end of the invoice value, we
// instead write the invoices into a new key preifx that follows the main
// invoice number. This ensures that we don't need to continually decode a
// potentially massive HTLC set, and also allows us to quickly find the HLTCs
// associated with a particular HTLC set.
func updateAMPInvoices(invoiceBucket kvdb.RwBucket, invoiceNum []byte,
	htlcsToUpdate map[invpkg.SetID]map[models.CircuitKey]*invpkg.InvoiceHTLC) error { //nolint:lll

	for setID, htlcSet := range htlcsToUpdate {
		// First write out the set of HTLCs including all the relevant
		// TLV values.
		var b bytes.Buffer
		if err := serializeHtlcs(&b, htlcSet); err != nil {
			return err
		}

		// Next store each HTLC in-line, using a prefix based off the
		// invoice number.
		invoiceSetIDKey := makeInvoiceSetIDKey(invoiceNum, setID[:])

		err := invoiceBucket.Put(invoiceSetIDKey[:], b.Bytes())
		if err != nil {
			return err
		}
	}

	return nil
}

// updateHtlcsAmp takes an invoice, and a new HTLC to be added (along with its
// set ID), and update sthe internal AMP state of an invoice, and also tallies
// the set of HTLCs to be updated on disk.
func updateHtlcsAmp(invoice *invpkg.Invoice,
	updateMap map[invpkg.SetID]map[models.CircuitKey]*invpkg.InvoiceHTLC,
	htlc *invpkg.InvoiceHTLC, setID invpkg.SetID,
	circuitKey models.CircuitKey) {

	ampState, ok := invoice.AMPState[setID]
	if !ok {
		// If an entry for this set ID doesn't already exist, then
		// we'll need to create it.
		ampState = invpkg.InvoiceStateAMP{
			State:       invpkg.HtlcStateAccepted,
			InvoiceKeys: make(map[models.CircuitKey]struct{}),
		}
	}

	ampState.AmtPaid += htlc.Amt
	ampState.InvoiceKeys[circuitKey] = struct{}{}

	// Due to the way maps work, we need to read out the value, update it,
	// then re-assign it into the map.
	invoice.AMPState[setID] = ampState

	// Now that we've updated the invoice state, we'll inform the caller of
	// the _neitre_ HTLC set they need to write for this new set ID.
	if _, ok := updateMap[setID]; !ok {
		// If we're just now creating the HTLCs for this set then we'll
		// also pull in the existing HTLCs are part of this set, so we
		// can write them all to disk together (same value)
		updateMap[setID] = invoice.HTLCSet(
			(*[32]byte)(&setID), invpkg.HtlcStateAccepted,
		)
	}
	updateMap[setID][circuitKey] = htlc
}

// cancelHtlcsAmp processes a cancellation of an HTLC that belongs to an AMP
// HTLC set. We'll need to update the meta data in the  main invoice, and also
// apply the new update to the update MAP, since all the HTLCs for a given HTLC
// set need to be written in-line with each other.
func cancelHtlcsAmp(invoice *invpkg.Invoice,
	updateMap map[invpkg.SetID]map[models.CircuitKey]*invpkg.InvoiceHTLC,
	htlc *invpkg.InvoiceHTLC, circuitKey models.CircuitKey) {

	setID := htlc.AMP.Record.SetID()

	// First, we'll update the state of the entire HTLC set to cancelled.
	ampState := invoice.AMPState[setID]
	ampState.State = invpkg.HtlcStateCanceled

	ampState.InvoiceKeys[circuitKey] = struct{}{}
	ampState.AmtPaid -= htlc.Amt

	// With the state update,d we'll set the new value so the struct
	// changes are propagated.
	invoice.AMPState[setID] = ampState

	if _, ok := updateMap[setID]; !ok {
		// Only HTLCs in the accepted state, can be cancelled, but we
		// also want to merge that with HTLCs that may be canceled as
		// well since it can be cancelled one by one.
		updateMap[setID] = invoice.HTLCSet(
			&setID, invpkg.HtlcStateAccepted,
		)

		cancelledHtlcs := invoice.HTLCSet(
			&setID, invpkg.HtlcStateCanceled,
		)
		for htlcKey, htlc := range cancelledHtlcs {
			updateMap[setID][htlcKey] = htlc
		}
	}

	// Finally, include the newly cancelled HTLC in the set of HTLCs we
	// need to cancel.
	updateMap[setID][circuitKey] = htlc

	// We'll only decrement the total amount paid if the invoice was
	// already in the accepted state.
	if invoice.AmtPaid != 0 {
		invoice.AmtPaid -= htlc.Amt
	}
}

// settleHtlcsAmp processes a new settle operation on an HTLC set for an AMP
// invoice. We'll update some meta data in the main invoice, and also signal
// that this HTLC set needs to be re-written back to disk.
func settleHtlcsAmp(invoice *invpkg.Invoice,
	settledSetIDs map[invpkg.SetID]struct{},
	updateMap map[invpkg.SetID]map[models.CircuitKey]*invpkg.InvoiceHTLC,
	htlc *invpkg.InvoiceHTLC, circuitKey models.CircuitKey) {

	// First, add the set ID to the set that was settled in this invoice
	// update. We'll use this later to update the settle index.
	setID := htlc.AMP.Record.SetID()
	settledSetIDs[setID] = struct{}{}

	// Next update the main AMP meta-data to indicate that this HTLC set
	// has been fully settled.
	ampState := invoice.AMPState[setID]
	ampState.State = invpkg.HtlcStateSettled

	ampState.InvoiceKeys[circuitKey] = struct{}{}

	invoice.AMPState[setID] = ampState

	// Finally, we'll add this to the set of HTLCs that need to be updated.
	if _, ok := updateMap[setID]; !ok {
		mapEntry := make(map[models.CircuitKey]*invpkg.InvoiceHTLC)
		updateMap[setID] = mapEntry
	}
	updateMap[setID][circuitKey] = htlc
}

// updateInvoice fetches the invoice, obtains the update descriptor from the
// callback and applies the updates in a single db transaction.
func (d *DB) updateInvoice(hash *lntypes.Hash, refSetID *invpkg.SetID, invoices,
	settleIndex, setIDIndex kvdb.RwBucket, invoiceNum []byte,
	callback invpkg.InvoiceUpdateCallback) (*invpkg.Invoice, error) {

	// If the set ID is non-nil, then we'll use that to filter out the
	// HTLCs for AMP invoice so we don't need to read them all out to
	// satisfy the invoice callback below. If it's nil, then we pass in the
	// zero set ID which means no HTLCs will be read out.
	var invSetID invpkg.SetID
	if refSetID != nil {
		invSetID = *refSetID
	}
	invoice, err := fetchInvoice(invoiceNum, invoices, &invSetID)
	if err != nil {
		return nil, err
	}

	// Create deep copy to prevent any accidental modification in the
	// callback.
	invoiceCopy, err := invpkg.CopyInvoice(&invoice)
	if err != nil {
		return nil, err
	}

	// Call the callback and obtain the update descriptor.
	update, err := callback(invoiceCopy)
	if err != nil {
		return &invoice, err
	}

	// If there is nothing to update, return early.
	if update == nil {
		return &invoice, nil
	}

	switch update.UpdateType {
	case invpkg.CancelHTLCsUpdate:
		return d.cancelHTLCs(invoices, invoiceNum, &invoice, update)

	case invpkg.AddHTLCsUpdate:
		return d.addHTLCs(
			invoices, settleIndex, setIDIndex, invoiceNum, &invoice,
			hash, update,
		)

	case invpkg.SettleHodlInvoiceUpdate:
		return d.settleHodlInvoice(
			invoices, settleIndex, invoiceNum, &invoice, hash,
			update.State,
		)

	case invpkg.CancelInvoiceUpdate:
		return d.cancelInvoice(
			invoices, invoiceNum, &invoice, hash, update.State,
		)

	default:
		return nil, fmt.Errorf("unknown update type: %s",
			update.UpdateType)
	}
}

// cancelHTLCs tries to cancel the htlcs in the given InvoiceUpdateDesc.
//
// NOTE: cancelHTLCs updates will only use the `CancelHtlcs` field in the
// InvoiceUpdateDesc.
func (d *DB) cancelHTLCs(invoices kvdb.RwBucket, invoiceNum []byte,
	invoice *invpkg.Invoice,
	update *invpkg.InvoiceUpdateDesc) (*invpkg.Invoice, error) {

	timestamp := d.clock.Now()

	// Process add actions from update descriptor.
	htlcsAmpUpdate := make(map[invpkg.SetID]map[models.CircuitKey]*invpkg.InvoiceHTLC) //nolint:lll

	// Process cancel actions from update descriptor.
	cancelHtlcs := update.CancelHtlcs
	for key, htlc := range invoice.Htlcs {
		htlc := htlc

		// Check whether this htlc needs to be canceled. If it does,
		// update the htlc state to Canceled.
		_, cancel := cancelHtlcs[key]
		if !cancel {
			continue
		}

		err := cancelSingleHtlc(timestamp, htlc, invoice.State)
		if err != nil {
			return nil, err
		}

		// Delete processed cancel action, so that we can check later
		// that there are no actions left.
		delete(cancelHtlcs, key)

		// Tally this into the set of HTLCs that need to be updated on
		// disk, but once again, only if this is an AMP invoice.
		if invoice.IsAMP() {
			cancelHtlcsAmp(
				invoice, htlcsAmpUpdate, htlc, key,
			)
		}
	}

	// Verify that we didn't get an action for htlcs that are not present on
	// the invoice.
	if len(cancelHtlcs) > 0 {
		return nil, errors.New("cancel action on non-existent htlc(s)")
	}

	err := d.serializeAndStoreInvoice(invoices, invoiceNum, invoice)
	if err != nil {
		return nil, err
	}

	// If this is an AMP invoice, then we'll actually store the rest of the
	// HTLCs in-line with the invoice, using the invoice ID as a prefix,
	// and the AMP key as a suffix: invoiceNum || setID.
	if invoice.IsAMP() {
		err := updateAMPInvoices(invoices, invoiceNum, htlcsAmpUpdate)
		if err != nil {
			return nil, err
		}
	}

	return invoice, nil
}

// serializeAndStoreInvoice is a helper function used to store invoices.
func (d *DB) serializeAndStoreInvoice(invoices kvdb.RwBucket, invoiceNum []byte,
	invoice *invpkg.Invoice) error {

	var buf bytes.Buffer
	if err := serializeInvoice(&buf, invoice); err != nil {
		return err
	}

	return invoices.Put(invoiceNum, buf.Bytes())
}

// addHTLCs tries to add the htlcs in the given InvoiceUpdateDesc.
func (d *DB) addHTLCs(invoices, settleIndex, //nolint:funlen
	setIDIndex kvdb.RwBucket, invoiceNum []byte, invoice *invpkg.Invoice,
	hash *lntypes.Hash, update *invpkg.InvoiceUpdateDesc) (*invpkg.Invoice,
	error) {

	var setID *[32]byte
	invoiceIsAMP := invoice.IsAMP()
	if invoiceIsAMP && update.State != nil {
		setID = update.State.SetID
	}
	timestamp := d.clock.Now()

	// Process add actions from update descriptor.
	htlcsAmpUpdate := make(map[invpkg.SetID]map[models.CircuitKey]*invpkg.InvoiceHTLC) //nolint:lll
	for key, htlcUpdate := range update.AddHtlcs {
		if _, exists := invoice.Htlcs[key]; exists {
			return nil, fmt.Errorf("duplicate add of htlc %v", key)
		}

		// Force caller to supply htlc without custom records in a
		// consistent way.
		if htlcUpdate.CustomRecords == nil {
			return nil, errors.New("nil custom records map")
		}

		if invoiceIsAMP {
			if htlcUpdate.AMP == nil {
				return nil, fmt.Errorf("unable to add htlc "+
					"without AMP data to AMP invoice(%v)",
					invoice.AddIndex)
			}

			// Check if this SetID already exist.
			htlcSetID := htlcUpdate.AMP.Record.SetID()
			setIDInvNum := setIDIndex.Get(htlcSetID[:])

			if setIDInvNum == nil {
				err := setIDIndex.Put(htlcSetID[:], invoiceNum)
				if err != nil {
					return nil, err
				}
			} else if !bytes.Equal(setIDInvNum, invoiceNum) {
				return nil, invpkg.ErrDuplicateSetID{
					SetID: htlcSetID,
				}
			}
		}

		htlc := &invpkg.InvoiceHTLC{
			Amt:           htlcUpdate.Amt,
			MppTotalAmt:   htlcUpdate.MppTotalAmt,
			Expiry:        htlcUpdate.Expiry,
			AcceptHeight:  uint32(htlcUpdate.AcceptHeight),
			AcceptTime:    timestamp,
			State:         invpkg.HtlcStateAccepted,
			CustomRecords: htlcUpdate.CustomRecords,
			AMP:           htlcUpdate.AMP.Copy(),
		}

		invoice.Htlcs[key] = htlc

		// Collect the set of new HTLCs so we can write them properly
		// below, but only if this is an AMP invoice.
		if invoiceIsAMP {
			updateHtlcsAmp(
				invoice, htlcsAmpUpdate, htlc,
				htlcUpdate.AMP.Record.SetID(), key,
			)
		}
	}

	// At this point, the set of accepted HTLCs should be fully
	// populated with added HTLCs or removed of canceled ones. Update
	// invoice state if the update descriptor indicates an invoice state
	// change, which depends on having an accurate view of the accepted
	// HTLCs.
	if update.State != nil {
		newState, err := updateInvoiceState(
			invoice, hash, *update.State,
		)
		if err != nil {
			return nil, err
		}

		// If this isn't an AMP invoice, then we'll go ahead and update
		// the invoice state directly here. For AMP invoices, we
		// instead will keep the top-level invoice open, and instead
		// update the state of each _htlc set_ instead. However, we'll
		// allow the invoice to transition to the cancelled state
		// regardless.
		if !invoiceIsAMP || *newState == invpkg.ContractCanceled {
			invoice.State = *newState
		}

		// If this is a non-AMP invoice, then the state can eventually
		// go to ContractSettled, so we pass in  nil value as part of
		// setSettleMetaFields.
		isSettled := update.State.NewState == invpkg.ContractSettled
		if !invoiceIsAMP && isSettled {
			err := setSettleMetaFields(
				settleIndex, invoiceNum, invoice, timestamp,
				nil,
			)
			if err != nil {
				return nil, err
			}
		}
	}

	// The set of HTLC pre-images will only be set if we were actually able
	// to reconstruct all the AMP pre-images.
	var settleEligibleAMP bool
	if update.State != nil {
		settleEligibleAMP = len(update.State.HTLCPreimages) != 0
	}

	// With any invoice level state transitions recorded, we'll now
	// finalize the process by updating the state transitions for
	// individual HTLCs
	var (
		settledSetIDs = make(map[invpkg.SetID]struct{})
		amtPaid       lnwire.MilliSatoshi
	)
	for key, htlc := range invoice.Htlcs {
		// Set the HTLC preimage for any AMP HTLCs.
		if setID != nil && update.State != nil {
			preimage, ok := update.State.HTLCPreimages[key]
			switch {
			// If we don't already have a preimage for this HTLC, we
			// can set it now.
			case ok && htlc.AMP.Preimage == nil:
				htlc.AMP.Preimage = &preimage

			// Otherwise, prevent over-writing an existing
			// preimage.  Ignore the case where the preimage is
			// identical.
			case ok && *htlc.AMP.Preimage != preimage:
				return nil, invpkg.ErrHTLCPreimageAlreadyExists
			}
		}

		// The invoice state may have changed and this could have
		// implications for the states of the individual htlcs. Align
		// the htlc state with the current invoice state.
		//
		// If we have all the pre-images for an AMP invoice, then we'll
		// act as if we're able to settle the entire invoice. We need
		// to do this since it's possible for us to settle AMP invoices
		// while the contract state (on disk) is still in the accept
		// state.
		htlcContextState := invoice.State
		if settleEligibleAMP {
			htlcContextState = invpkg.ContractSettled
		}
		htlcSettled, err := updateHtlc(
			timestamp, htlc, htlcContextState, setID,
		)
		if err != nil {
			return nil, err
		}

		// If the HTLC has being settled for the first time, and this
		// is an AMP invoice, then we'll need to update some additional
		// meta data state.
		if htlcSettled && invoiceIsAMP {
			settleHtlcsAmp(
				invoice, settledSetIDs, htlcsAmpUpdate, htlc,
				key,
			)
		}

		accepted := htlc.State == invpkg.HtlcStateAccepted
		settled := htlc.State == invpkg.HtlcStateSettled
		invoiceStateReady := accepted || settled

		if !invoiceIsAMP {
			// Update the running amount paid to this invoice. We
			// don't include accepted htlcs when the invoice is
			// still open.
			if invoice.State != invpkg.ContractOpen &&
				invoiceStateReady {

				amtPaid += htlc.Amt
			}
		} else {
			// For AMP invoices, since we won't always be reading
			// out the total invoice set each time, we'll instead
			// accumulate newly added invoices to the total amount
			// paid.
			if _, ok := update.AddHtlcs[key]; !ok {
				continue
			}

			// Update the running amount paid to this invoice. AMP
			// invoices never go to the settled state, so if it's
			// open, then we tally the HTLC.
			if invoice.State == invpkg.ContractOpen &&
				invoiceStateReady {

				amtPaid += htlc.Amt
			}
		}
	}

	// For non-AMP invoices we recalculate the amount paid from scratch
	// each time, while for AMP invoices, we'll accumulate only based on
	// newly added HTLCs.
	if !invoiceIsAMP {
		invoice.AmtPaid = amtPaid
	} else {
		invoice.AmtPaid += amtPaid
	}

	// As we don't update the settle index above for AMP invoices, we'll do
	// it here for each sub-AMP invoice that was settled.
	for settledSetID := range settledSetIDs {
		settledSetID := settledSetID
		err := setSettleMetaFields(
			settleIndex, invoiceNum, invoice, timestamp,
			&settledSetID,
		)
		if err != nil {
			return nil, err
		}
	}

	err := d.serializeAndStoreInvoice(invoices, invoiceNum, invoice)
	if err != nil {
		return nil, err
	}

	// If this is an AMP invoice, then we'll actually store the rest of the
	// HTLCs in-line with the invoice, using the invoice ID as a prefix,
	// and the AMP key as a suffix: invoiceNum || setID.
	if invoiceIsAMP {
		err := updateAMPInvoices(invoices, invoiceNum, htlcsAmpUpdate)
		if err != nil {
			return nil, err
		}
	}

	return invoice, nil
}

// settleHodlInvoice marks a hodl invoice as settled.
//
// NOTE: Currently it is not possible to have HODL AMP invoices.
func (d *DB) settleHodlInvoice(invoices, settleIndex kvdb.RwBucket,
	invoiceNum []byte, invoice *invpkg.Invoice, hash *lntypes.Hash,
	update *invpkg.InvoiceStateUpdateDesc) (*invpkg.Invoice, error) {

	if !invoice.HodlInvoice {
		return nil, fmt.Errorf("unable to settle hodl invoice: %v is "+
			"not a hodl invoice", invoice.AddIndex)
	}

	// TODO(positiveblue): because NewState can only be ContractSettled we
	// can remove it from the API and set it here directly.
	switch {
	case update == nil:
		fallthrough

	case update.NewState != invpkg.ContractSettled:
		return nil, fmt.Errorf("unable to settle hodl invoice: "+
			"not valid InvoiceUpdateDesc.State: %v", update)

	case update.Preimage == nil:
		return nil, fmt.Errorf("unable to settle hodl invoice: " +
			"preimage is nil")
	}

	// TODO(positiveblue): create a invoice.CanSettleHodlInvoice func.
	newState, err := updateInvoiceState(invoice, hash, *update)
	if err != nil {
		return nil, err
	}

	if newState == nil || *newState != invpkg.ContractSettled {
		return nil, fmt.Errorf("unable to settle hodl invoice: "+
			"new computed state is not settled: %s", newState)
	}

	invoice.State = invpkg.ContractSettled
	timestamp := d.clock.Now()

	err = setSettleMetaFields(
		settleIndex, invoiceNum, invoice, timestamp, nil,
	)
	if err != nil {
		return nil, err
	}

	// TODO(positiveblue): this logic can be further simplified.
	var amtPaid lnwire.MilliSatoshi
	for _, htlc := range invoice.Htlcs {
		_, err := updateHtlc(
			timestamp, htlc, invpkg.ContractSettled, nil,
		)
		if err != nil {
			return nil, err
		}

		if htlc.State == invpkg.HtlcStateSettled {
			amtPaid += htlc.Amt
		}
	}

	invoice.AmtPaid = amtPaid

	err = d.serializeAndStoreInvoice(invoices, invoiceNum, invoice)
	if err != nil {
		return nil, err
	}

	return invoice, nil
}

// cancelInvoice attempts to cancel the given invoice. That includes changing
// the invoice state and the state of any relevant HTLC.
func (d *DB) cancelInvoice(invoices kvdb.RwBucket, invoiceNum []byte,
	invoice *invpkg.Invoice, hash *lntypes.Hash,
	update *invpkg.InvoiceStateUpdateDesc) (*invpkg.Invoice, error) {

	switch {
	case update == nil:
		fallthrough

	case update.NewState != invpkg.ContractCanceled:
		return nil, fmt.Errorf("unable to cancel invoice: "+
			"InvoiceUpdateDesc.State not valid: %v", update)
	}

	var (
		setID        *[32]byte
		invoiceIsAMP bool
	)

	invoiceIsAMP = invoice.IsAMP()
	if invoiceIsAMP {
		setID = update.SetID
	}

	newState, err := updateInvoiceState(invoice, hash, *update)
	if err != nil {
		return nil, err
	}

	if newState == nil || *newState != invpkg.ContractCanceled {
		return nil, fmt.Errorf("unable to cancel invoice(%v): new "+
			"computed state is not canceled: %s", invoice.AddIndex,
			newState)
	}

	invoice.State = invpkg.ContractCanceled
	timestamp := d.clock.Now()

	// TODO(positiveblue): this logic can be simplified.
	for _, htlc := range invoice.Htlcs {
		_, err := updateHtlc(
			timestamp, htlc, invpkg.ContractCanceled, setID,
		)
		if err != nil {
			return nil, err
		}
	}

	err = d.serializeAndStoreInvoice(invoices, invoiceNum, invoice)
	if err != nil {
		return nil, err
	}

	return invoice, nil
}

// updateInvoiceState validates and processes an invoice state update. The new
// state to transition to is returned, so the caller is able to select exactly
// how the invoice state is updated.
func updateInvoiceState(invoice *invpkg.Invoice, hash *lntypes.Hash,
	update invpkg.InvoiceStateUpdateDesc) (*invpkg.ContractState, error) {

	// Returning to open is never allowed from any state.
	if update.NewState == invpkg.ContractOpen {
		return nil, invpkg.ErrInvoiceCannotOpen
	}

	switch invoice.State {
	// Once a contract is accepted, we can only transition to settled or
	// canceled. Forbid transitioning back into this state. Otherwise this
	// state is identical to ContractOpen, so we fallthrough to apply the
	// same checks that we apply to open invoices.
	case invpkg.ContractAccepted:
		if update.NewState == invpkg.ContractAccepted {
			return nil, invpkg.ErrInvoiceCannotAccept
		}

		fallthrough

	// If a contract is open, permit a state transition to accepted, settled
	// or canceled. The only restriction is on transitioning to settled
	// where we ensure the preimage is valid.
	case invpkg.ContractOpen:
		if update.NewState == invpkg.ContractCanceled {
			return &update.NewState, nil
		}

		// Sanity check that the user isn't trying to settle or accept a
		// non-existent HTLC set.
		set := invoice.HTLCSet(update.SetID, invpkg.HtlcStateAccepted)
		if len(set) == 0 {
			return nil, invpkg.ErrEmptyHTLCSet
		}

		// For AMP invoices, there are no invoice-level preimage checks.
		// However, we still sanity check that we aren't trying to
		// settle an AMP invoice with a preimage.
		if update.SetID != nil {
			if update.Preimage != nil {
				return nil, errors.New("AMP set cannot have " +
					"preimage")
			}
			return &update.NewState, nil
		}

		switch {
		// If an invoice-level preimage was supplied, but the InvoiceRef
		// doesn't specify a hash (e.g. AMP invoices) we fail.
		case update.Preimage != nil && hash == nil:
			return nil, invpkg.ErrUnexpectedInvoicePreimage

		// Validate the supplied preimage for non-AMP invoices.
		case update.Preimage != nil:
			if update.Preimage.Hash() != *hash {
				return nil, invpkg.ErrInvoicePreimageMismatch
			}
			invoice.Terms.PaymentPreimage = update.Preimage

		// Permit non-AMP invoices to be accepted without knowing the
		// preimage. When trying to settle we'll have to pass through
		// the above check in order to not hit the one below.
		case update.NewState == invpkg.ContractAccepted:

		// Fail if we still don't have a preimage when transitioning to
		// settle the non-AMP invoice.
		case update.NewState == invpkg.ContractSettled &&
			invoice.Terms.PaymentPreimage == nil:

			return nil, errors.New("unknown preimage")
		}

		return &update.NewState, nil

	// Once settled, we are in a terminal state.
	case invpkg.ContractSettled:
		return nil, invpkg.ErrInvoiceAlreadySettled

	// Once canceled, we are in a terminal state.
	case invpkg.ContractCanceled:
		return nil, invpkg.ErrInvoiceAlreadyCanceled

	default:
		return nil, errors.New("unknown state transition")
	}
}

// cancelSingleHtlc validates cancellation of a single htlc and update its
// state.
func cancelSingleHtlc(resolveTime time.Time, htlc *invpkg.InvoiceHTLC,
	invState invpkg.ContractState) error {

	// It is only possible to cancel individual htlcs on an open invoice.
	if invState != invpkg.ContractOpen {
		return fmt.Errorf("htlc canceled on invoice in "+
			"state %v", invState)
	}

	// It is only possible if the htlc is still pending.
	if htlc.State != invpkg.HtlcStateAccepted {
		return fmt.Errorf("htlc canceled in state %v",
			htlc.State)
	}

	htlc.State = invpkg.HtlcStateCanceled
	htlc.ResolveTime = resolveTime

	return nil
}

// updateHtlc aligns the state of an htlc with the given invoice state. A
// boolean is returned if the HTLC was settled.
func updateHtlc(resolveTime time.Time, htlc *invpkg.InvoiceHTLC,
	invState invpkg.ContractState, setID *[32]byte) (bool, error) {

	trySettle := func(persist bool) (bool, error) {
		if htlc.State != invpkg.HtlcStateAccepted {
			return false, nil
		}

		// Settle the HTLC if it matches the settled set id. If
		// there're other HTLCs with distinct setIDs, then we'll leave
		// them, as they may eventually be settled as we permit
		// multiple settles to a single pay_addr for AMP.
		var htlcState invpkg.HtlcState
		if htlc.IsInHTLCSet(setID) {
			// Non-AMP HTLCs can be settled immediately since we
			// already know the preimage is valid due to checks at
			// the invoice level. For AMP HTLCs, verify that the
			// per-HTLC preimage-hash pair is valid.
			switch {
			// Non-AMP HTLCs can be settle immediately since we
			// already know the preimage is valid due to checks at
			// the invoice level.
			case setID == nil:

			// At this point, the setID is non-nil, meaning this is
			// an AMP HTLC. We know that htlc.AMP cannot be nil,
			// otherwise IsInHTLCSet would have returned false.
			//
			// Fail if an accepted AMP HTLC has no preimage.
			case htlc.AMP.Preimage == nil:
				return false, invpkg.ErrHTLCPreimageMissing

			// Fail if the accepted AMP HTLC has an invalid
			// preimage.
			case !htlc.AMP.Preimage.Matches(htlc.AMP.Hash):
				return false, invpkg.ErrHTLCPreimageMismatch
			}

			htlcState = invpkg.HtlcStateSettled
		}

		// Only persist the changes if the invoice is moving to the
		// settled state, and we're actually updating the state to
		// settled.
		if persist && htlcState == invpkg.HtlcStateSettled {
			htlc.State = htlcState
			htlc.ResolveTime = resolveTime
		}

		return persist && htlcState == invpkg.HtlcStateSettled, nil
	}

	if invState == invpkg.ContractSettled {
		// Check that we can settle the HTLCs. For legacy and MPP HTLCs
		// this will be a NOP, but for AMP HTLCs this asserts that we
		// have a valid hash/preimage pair. Passing true permits the
		// method to update the HTLC to HtlcStateSettled.
		return trySettle(true)
	}

	// We should never find a settled HTLC on an invoice that isn't in
	// ContractSettled.
	if htlc.State == invpkg.HtlcStateSettled {
		return false, invpkg.ErrHTLCAlreadySettled
	}

	switch invState {
	case invpkg.ContractCanceled:
		if htlc.State == invpkg.HtlcStateAccepted {
			htlc.State = invpkg.HtlcStateCanceled
			htlc.ResolveTime = resolveTime
		}
		return false, nil

	// TODO(roasbeef): never fully passed thru now?
	case invpkg.ContractAccepted:
		// Check that we can settle the HTLCs. For legacy and MPP HTLCs
		// this will be a NOP, but for AMP HTLCs this asserts that we
		// have a valid hash/preimage pair. Passing false prevents the
		// method from putting the HTLC in HtlcStateSettled, leaving it
		// in HtlcStateAccepted.
		return trySettle(false)

	case invpkg.ContractOpen:
		return false, nil

	default:
		return false, errors.New("unknown state transition")
	}
}

// setSettleMetaFields updates the metadata associated with settlement of an
// invoice. If a non-nil setID is passed in, then the value will be append to
// the invoice number as well, in order to allow us to detect repeated payments
// to the same AMP invoices "across time".
func setSettleMetaFields(settleIndex kvdb.RwBucket, invoiceNum []byte,
	invoice *invpkg.Invoice, now time.Time, setID *invpkg.SetID) error {

	// Now that we know the invoice hasn't already been settled, we'll
	// update the settle index so we can place this settle event in the
	// proper location within our time series.
	nextSettleSeqNo, err := settleIndex.NextSequence()
	if err != nil {
		return err
	}

	// Make a new byte array on the stack that can potentially store the 4
	// byte invoice number along w/ the 32 byte set ID. We capture valueLen
	// here which is the number of bytes copied so we can only store the 4
	// bytes if this is a non-AMP invoice.
	var indexKey [invoiceSetIDKeyLen]byte
	valueLen := copy(indexKey[:], invoiceNum)

	if setID != nil {
		valueLen += copy(indexKey[valueLen:], setID[:])
	}

	var seqNoBytes [8]byte
	byteOrder.PutUint64(seqNoBytes[:], nextSettleSeqNo)
	err = settleIndex.Put(seqNoBytes[:], indexKey[:valueLen])
	if err != nil {
		return err
	}

	// If the setID is nil, then this means that this is a non-AMP settle,
	// so we'll update the invoice settle index directly.
	if setID == nil {
		invoice.SettleDate = now
		invoice.SettleIndex = nextSettleSeqNo
	} else {
		// If the set ID isn't blank, we'll update the AMP state map
		// which tracks when each of the setIDs associated with a given
		// AMP invoice are settled.
		ampState := invoice.AMPState[*setID]

		ampState.SettleDate = now
		ampState.SettleIndex = nextSettleSeqNo

		invoice.AMPState[*setID] = ampState
	}

	return nil
}

// delAMPInvoices attempts to delete all the "sub" invoices associated with a
// greater AMP invoices. We do this by deleting the set of keys that share the
// invoice number as a prefix.
func delAMPInvoices(invoiceNum []byte, invoiceBucket kvdb.RwBucket) error {
	// Since it isn't safe to delete using an active cursor, we'll use the
	// cursor simply to collect the set of keys we need to delete, _then_
	// delete them in another pass.
	var keysToDel [][]byte
	err := forEachAMPInvoice(
		invoiceBucket, invoiceNum,
		func(cursorKey, v []byte) error {
			keysToDel = append(keysToDel, cursorKey)
			return nil
		},
	)
	if err != nil {
		return err
	}

	// In this next phase, we'll then delete all the relevant invoices.
	for _, keyToDel := range keysToDel {
		if err := invoiceBucket.Delete(keyToDel); err != nil {
			return err
		}
	}

	return nil
}

// delAMPSettleIndex removes all the entries in the settle index associated
// with a given AMP invoice.
func delAMPSettleIndex(invoiceNum []byte, invoices,
	settleIndex kvdb.RwBucket) error {

	// First, we need to grab the AMP invoice state to see if there's
	// anything that we even need to delete.
	ampState, err := fetchInvoiceStateAMP(invoiceNum, invoices)
	if err != nil {
		return err
	}

	// If there's no AMP state at all (non-AMP invoice), then we can return
	// early.
	if len(ampState) == 0 {
		return nil
	}

	// Otherwise, we'll need to iterate and delete each settle index within
	// the set of returned entries.
	var settleIndexKey [8]byte
	for _, subState := range ampState {
		byteOrder.PutUint64(
			settleIndexKey[:], subState.SettleIndex,
		)

		if err := settleIndex.Delete(settleIndexKey[:]); err != nil {
			return err
		}
	}

	return nil
}

// DeleteInvoice attempts to delete the passed invoices from the database in
// one transaction. The passed delete references hold all keys required to
// delete the invoices without also needing to deserialze them.
func (d *DB) DeleteInvoice(invoicesToDelete []invpkg.InvoiceDeleteRef) error {
	err := kvdb.Update(d, func(tx kvdb.RwTx) error {
		invoices := tx.ReadWriteBucket(invoiceBucket)
		if invoices == nil {
			return invpkg.ErrNoInvoicesCreated
		}

		invoiceIndex := invoices.NestedReadWriteBucket(
			invoiceIndexBucket,
		)
		if invoiceIndex == nil {
			return invpkg.ErrNoInvoicesCreated
		}

		invoiceAddIndex := invoices.NestedReadWriteBucket(
			addIndexBucket,
		)
		if invoiceAddIndex == nil {
			return invpkg.ErrNoInvoicesCreated
		}

		// settleIndex can be nil, as the bucket is created lazily
		// when the first invoice is settled.
		settleIndex := invoices.NestedReadWriteBucket(settleIndexBucket)

		payAddrIndex := tx.ReadWriteBucket(payAddrIndexBucket)

		for _, ref := range invoicesToDelete {
			// Fetch the invoice key for using it to check for
			// consistency and also to delete from the invoice
			// index.
			invoiceKey := invoiceIndex.Get(ref.PayHash[:])
			if invoiceKey == nil {
				return invpkg.ErrInvoiceNotFound
			}

			err := invoiceIndex.Delete(ref.PayHash[:])
			if err != nil {
				return err
			}

			// Delete payment address index reference if there's a
			// valid payment address passed.
			if ref.PayAddr != nil {
				// To ensure consistency check that the already
				// fetched invoice key matches the one in the
				// payment address index.
				key := payAddrIndex.Get(ref.PayAddr[:])
				if bytes.Equal(key, invoiceKey) {
					// Delete from the payment address
					// index. Note that since the payment
					// address index has been introduced
					// with an empty migration it may be
					// possible that the index doesn't have
					// an entry for this invoice.
					// ref: https://github.com/lightningnetwork/lnd/pull/4285/commits/cbf71b5452fa1d3036a43309e490787c5f7f08dc#r426368127
					if err := payAddrIndex.Delete(
						ref.PayAddr[:],
					); err != nil {
						return err
					}
				}
			}

			var addIndexKey [8]byte
			byteOrder.PutUint64(addIndexKey[:], ref.AddIndex)

			// To ensure consistency check that the key stored in
			// the add index also matches the previously fetched
			// invoice key.
			key := invoiceAddIndex.Get(addIndexKey[:])
			if !bytes.Equal(key, invoiceKey) {
				return fmt.Errorf("unknown invoice " +
					"in add index")
			}

			// Remove from the add index.
			err = invoiceAddIndex.Delete(addIndexKey[:])
			if err != nil {
				return err
			}

			// Remove from the settle index if available and
			// if the invoice is settled.
			if settleIndex != nil && ref.SettleIndex > 0 {
				var settleIndexKey [8]byte
				byteOrder.PutUint64(
					settleIndexKey[:], ref.SettleIndex,
				)

				// To ensure consistency check that the already
				// fetched invoice key matches the one in the
				// settle index
				key := settleIndex.Get(settleIndexKey[:])
				if !bytes.Equal(key, invoiceKey) {
					return fmt.Errorf("unknown invoice " +
						"in settle index")
				}

				err = settleIndex.Delete(settleIndexKey[:])
				if err != nil {
					return err
				}
			}

			// In addition to deleting the main invoice state, if
			// this is an AMP invoice, then we'll also need to
			// delete the set HTLC set stored as a key prefix. For
			// non-AMP invoices, this'll be a noop.
			err = delAMPSettleIndex(
				invoiceKey, invoices, settleIndex,
			)
			if err != nil {
				return err
			}
			err = delAMPInvoices(invoiceKey, invoices)
			if err != nil {
				return err
			}

			// Finally remove the serialized invoice from the
			// invoice bucket.
			err = invoices.Delete(invoiceKey)
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})

	return err
}
