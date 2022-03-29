package channeldb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// unknownPreimage is an all-zeroes preimage that indicates that the
	// preimage for this invoice is not yet known.
	unknownPreimage lntypes.Preimage

	// BlankPayAddr is a sentinel payment address for legacy invoices.
	// Invoices with this payment address are special-cased in the insertion
	// logic to prevent being indexed in the payment address index,
	// otherwise they would cause collisions after the first insertion.
	BlankPayAddr [32]byte

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
)

// ErrDuplicateSetID is an error returned when attempting to adding an AMP HTLC
// to an invoice, but another invoice is already indexed by the same set id.
type ErrDuplicateSetID struct {
	setID [32]byte
}

// Error returns a human-readable description of ErrDuplicateSetID.
func (e ErrDuplicateSetID) Error() string {
	return fmt.Sprintf("invoice with set_id=%x already exists", e.setID)
}

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

// RefModifier is a modification on top of a base invoice ref. It allows the
// caller to opt to skip out on HTLCs for a given payAddr, or only return the
// set of specified HTLCs for a given setID.
type RefModifier uint8

const (
	// DefaultModifier is the base modifier that doesn't change any behavior.
	DefaultModifier RefModifier = iota

	// HtlcSetOnlyModifier can only be used with a setID based invoice ref, and
	// specifies that only the set of HTLCs related to that setID are to be
	// returned.
	HtlcSetOnlyModifier

	// HtlcSetOnlyModifier can only be used with a payAddr based invoice ref,
	// and specifies that the returned invoice shouldn't include any HTLCs at
	// all.
	HtlcSetBlankModifier
)

// InvoiceRef is a composite identifier for invoices. Invoices can be referenced
// by various combinations of payment hash and payment addr, in certain contexts
// only some of these are known. An InvoiceRef and its constructors thus
// encapsulate the valid combinations of query parameters that can be supplied
// to LookupInvoice and UpdateInvoice.
type InvoiceRef struct {
	// payHash is the payment hash of the target invoice. All invoices are
	// currently indexed by payment hash. This value will be used as a
	// fallback when no payment address is known.
	payHash *lntypes.Hash

	// payAddr is the payment addr of the target invoice. Newer invoices
	// (0.11 and up) are indexed by payment address in addition to payment
	// hash, but pre 0.8 invoices do not have one at all. When this value is
	// known it will be used as the primary identifier, falling back to
	// payHash if no value is known.
	payAddr *[32]byte

	// setID is the optional set id for an AMP payment. This can be used to
	// lookup or update the invoice knowing only this value. Queries by set
	// id are only used to facilitate user-facing requests, e.g. lookup,
	// settle or cancel an AMP invoice. The regular update flow from the
	// invoice registry will always query for the invoice by
	// payHash+payAddr.
	setID *[32]byte

	// refModifier allows an invoice ref to include or exclude specific
	// HTLC sets based on the payAddr or setId.
	refModifier RefModifier
}

// InvoiceRefByHash creates an InvoiceRef that queries for an invoice only by
// its payment hash.
func InvoiceRefByHash(payHash lntypes.Hash) InvoiceRef {
	return InvoiceRef{
		payHash: &payHash,
	}
}

// InvoiceRefByHashAndAddr creates an InvoiceRef that first queries for an
// invoice by the provided payment address, falling back to the payment hash if
// the payment address is unknown.
func InvoiceRefByHashAndAddr(payHash lntypes.Hash,
	payAddr [32]byte) InvoiceRef {

	return InvoiceRef{
		payHash: &payHash,
		payAddr: &payAddr,
	}
}

// InvoiceRefByAddr creates an InvoiceRef that queries the payment addr index
// for an invoice with the provided payment address.
func InvoiceRefByAddr(addr [32]byte) InvoiceRef {
	return InvoiceRef{
		payAddr: &addr,
	}
}

// InvoiceRefByAddrBlankHtlc creates an InvoiceRef that queries the payment addr index
// for an invoice with the provided payment address, but excludes any of the
// core HTLC information.
func InvoiceRefByAddrBlankHtlc(addr [32]byte) InvoiceRef {
	return InvoiceRef{
		payAddr:     &addr,
		refModifier: HtlcSetBlankModifier,
	}
}

// InvoiceRefBySetID creates an InvoiceRef that queries the set id index for an
// invoice with the provided setID. If the invoice is not found, the query will
// not fallback to payHash or payAddr.
func InvoiceRefBySetID(setID [32]byte) InvoiceRef {
	return InvoiceRef{
		setID: &setID,
	}
}

// InvoiceRefBySetIDFiltered is similar to the InvoiceRefBySetID identifier,
// but it specifies that the returned set of HTLCs should be filtered to only
// include HTLCs that are part of that set.
func InvoiceRefBySetIDFiltered(setID [32]byte) InvoiceRef {
	return InvoiceRef{
		setID:       &setID,
		refModifier: HtlcSetOnlyModifier,
	}
}

// PayHash returns the optional payment hash of the target invoice.
//
// NOTE: This value may be nil.
func (r InvoiceRef) PayHash() *lntypes.Hash {
	if r.payHash != nil {
		hash := *r.payHash
		return &hash
	}
	return nil
}

// PayAddr returns the optional payment address of the target invoice.
//
// NOTE: This value may be nil.
func (r InvoiceRef) PayAddr() *[32]byte {
	if r.payAddr != nil {
		addr := *r.payAddr
		return &addr
	}
	return nil
}

// SetID returns the optional set id of the target invoice.
//
// NOTE: This value may be nil.
func (r InvoiceRef) SetID() *[32]byte {
	if r.setID != nil {
		id := *r.setID
		return &id
	}
	return nil
}

// Modifier defines the set of available modifications to the base invoice ref
// look up that are available.
func (r InvoiceRef) Modifier() RefModifier {
	return r.refModifier
}

// String returns a human-readable representation of an InvoiceRef.
func (r InvoiceRef) String() string {
	var ids []string
	if r.payHash != nil {
		ids = append(ids, fmt.Sprintf("pay_hash=%v", *r.payHash))
	}
	if r.payAddr != nil {
		ids = append(ids, fmt.Sprintf("pay_addr=%x", *r.payAddr))
	}
	if r.setID != nil {
		ids = append(ids, fmt.Sprintf("set_id=%x", *r.setID))
	}
	return fmt.Sprintf("(%s)", strings.Join(ids, ", "))
}

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

// IsFinal returns a boolean indicating whether an invoice state is final.
func (c ContractState) IsFinal() bool {
	return c == ContractSettled || c == ContractCanceled
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
	// extended. Set to nil if the preimage isn't known yet.
	PaymentPreimage *lntypes.Preimage

	// Value is the expected amount of milli-satoshis to be paid to an HTLC
	// which can be satisfied by the above preimage.
	Value lnwire.MilliSatoshi

	// PaymentAddr is a randomly generated value include in the MPP record
	// by the sender to prevent probing of the receiver.
	PaymentAddr [32]byte

	// Features is the feature vectors advertised on the payment request.
	Features *lnwire.FeatureVector
}

// String returns a human-readable description of the prominent contract terms.
func (c ContractTerm) String() string {
	return fmt.Sprintf("amt=%v, expiry=%v, final_cltv_delta=%v", c.Value,
		c.Expiry, c.FinalCltvDelta)
}

// SetID is the extra unique tuple item for AMP invoices. In addition to
// setting a payment address, each repeated payment to an AMP invoice will also
// contain a set ID as well.
type SetID [32]byte

// InvoiceStateAMP is a struct that associates the current state of an AMP
// invoice identified by its set ID along with the set of invoices identified
// by the circuit key. This allows callers to easily look up the latest state
// of an AMP "sub-invoice" and also look up the invoice HLTCs themselves in the
// greater HTLC map index.
type InvoiceStateAMP struct {
	// State is the state of this sub-AMP invoice.
	State HtlcState

	// SettleIndex indicates the location in the settle index that
	// references this instance of InvoiceStateAMP, but only if
	// this value is set (non-zero), and State is HtlcStateSettled.
	SettleIndex uint64

	// SettleDate is the date that the setID was settled.
	SettleDate time.Time

	// InvoiceKeys is the set of circuit keys that can be used to locate
	// the invoices for a given set ID.
	InvoiceKeys map[CircuitKey]struct{}

	// AmtPaid is the total amount that was paid in the AMP sub-invoice.
	// Fetching the full HTLC/invoice state allows one to extract the
	// custom records as well as the break down of the payment splits used
	// when paying.
	AmtPaid lnwire.MilliSatoshi
}

// AMPInvoiceState represents a type that stores metadata related to the set of
// settled AMP "sub-invoices".
type AMPInvoiceState map[SetID]InvoiceStateAMP

// recordSize returns the amount of bytes this TLV record will occupy when
// encoded.
func (a *AMPInvoiceState) recordSize() uint64 {
	var (
		b   bytes.Buffer
		buf [8]byte
	)

	// We know that encoding works since the tests pass in the build this file
	// is checked into, so we'll simplify things and simply encode it ourselves
	// then report the total amount of bytes used.
	if err := ampStateEncoder(&b, a, &buf); err != nil {
		// This should never error out, but we log it just in case it
		// does.
		log.Errorf("encoding the amp invoice state failed: %v", err)
	}

	return uint64(len(b.Bytes()))
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
	// spontaneous (keysend) payments, this field will be empty.
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

	// State describes the state the invoice is in. This is the global
	// state of the invoice which may remain open even when a series of
	// sub-invoices for this invoice has been settled.
	State ContractState

	// AmtPaid is the final amount that we ultimately accepted for pay for
	// this invoice. We specify this value independently as it's possible
	// that the invoice originally didn't specify an amount, or the sender
	// overpaid.
	AmtPaid lnwire.MilliSatoshi

	// Htlcs records all htlcs that paid to this invoice. Some of these
	// htlcs may have been marked as canceled.
	Htlcs map[CircuitKey]*InvoiceHTLC

	// AMPState describes the state of any related sub-invoices AMP to this
	// greater invoice. A sub-invoice is defined by a set of HTLCs with the
	// same set ID that attempt to make one time or recurring payments to
	// this greater invoice. It's possible for a sub-invoice to be canceled
	// or settled, but the greater invoice still open.
	AMPState AMPInvoiceState

	// HodlInvoice indicates whether the invoice should be held in the
	// Accepted state or be settled right away.
	HodlInvoice bool
}

// HTLCSet returns the set of HTLCs belonging to setID and in the provided
// state. Passing a nil setID will return all HTLCs in the provided state in the
// case of legacy or MPP, and no HTLCs in the case of AMP.  Otherwise, the
// returned set will be filtered by the populated setID which is used to
// retrieve AMP HTLC sets.
func (i *Invoice) HTLCSet(setID *[32]byte, state HtlcState) map[CircuitKey]*InvoiceHTLC {
	htlcSet := make(map[CircuitKey]*InvoiceHTLC)
	for key, htlc := range i.Htlcs {
		// Only add HTLCs that are in the requested HtlcState.
		if htlc.State != state {
			continue
		}

		if !htlc.IsInHTLCSet(setID) {
			continue
		}

		htlcSet[key] = htlc
	}

	return htlcSet
}

// HTLCSetCompliment returns the set of all HTLCs not belonging to setID that
// are in the target state. Passing a nil setID will return no invoices, since
// all MPP HTLCs are part of the same HTLC set.
func (i *Invoice) HTLCSetCompliment(setID *[32]byte,
	state HtlcState) map[CircuitKey]*InvoiceHTLC {

	htlcSet := make(map[CircuitKey]*InvoiceHTLC)
	for key, htlc := range i.Htlcs {
		// Only add HTLCs that are in the requested HtlcState.
		if htlc.State != state {
			continue
		}

		// We are constructing the compliment, so filter anything that
		// matches this set id.
		if htlc.IsInHTLCSet(setID) {
			continue
		}

		htlcSet[key] = htlc
	}

	return htlcSet
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

	// AMP encapsulates additional data relevant to AMP HTLCs. This includes
	// the AMP onion record, in addition to the HTLC's payment hash and
	// preimage since these are unique to each AMP HTLC, and not the invoice
	// as a whole.
	//
	// NOTE: This value will only be set for AMP HTLCs.
	AMP *InvoiceHtlcAMPData
}

// Copy makes a deep copy of the target InvoiceHTLC.
func (h *InvoiceHTLC) Copy() *InvoiceHTLC {
	result := *h

	// Make a copy of the CustomSet map.
	result.CustomRecords = make(record.CustomSet)
	for k, v := range h.CustomRecords {
		result.CustomRecords[k] = v
	}

	result.AMP = h.AMP.Copy()

	return &result
}

// IsInHTLCSet returns true if this HTLC is part an HTLC set. If nil is passed,
// this method returns true if this is an MPP HTLC. Otherwise, it only returns
// true if the AMP HTLC's set id matches the populated setID.
func (h *InvoiceHTLC) IsInHTLCSet(setID *[32]byte) bool {
	wantAMPSet := setID != nil
	isAMPHtlc := h.AMP != nil

	// Non-AMP HTLCs cannot be part of AMP HTLC sets, and vice versa.
	if wantAMPSet != isAMPHtlc {
		return false
	}

	// Skip AMP HTLCs that have differing set ids.
	if isAMPHtlc && *setID != h.AMP.Record.SetID() {
		return false
	}

	return true
}

// InvoiceHtlcAMPData is a struct hodling the additional metadata stored for
// each received AMP HTLC. This includes the AMP onion record, in addition to
// the HTLC's payment hash and preimage.
type InvoiceHtlcAMPData struct {
	// AMP is a copy of the AMP record presented in the onion payload
	// containing the information necessary to correlate and settle a
	// spontaneous HTLC set. Newly accepted legacy keysend payments will
	// also have this field set as we automatically promote them into an AMP
	// payment for internal processing.
	Record record.AMP

	// Hash is an HTLC-level payment hash that is stored only for AMP
	// payments. This is done because an AMP HTLC will carry a different
	// payment hash from the invoice it might be satisfying, so we track the
	// payment hashes individually to able to compute whether or not the
	// reconstructed preimage correctly matches the HTLC's hash.
	Hash lntypes.Hash

	// Preimage is an HTLC-level preimage that satisfies the AMP HTLC's
	// Hash. The preimage will be be derived either from secret share
	// reconstruction of the shares in the AMP payload.
	//
	// NOTE: Preimage will only be present once the HTLC is in
	// HtlcStateSettled.
	Preimage *lntypes.Preimage
}

// Copy returns a deep copy of the InvoiceHtlcAMPData.
func (d *InvoiceHtlcAMPData) Copy() *InvoiceHtlcAMPData {
	if d == nil {
		return nil
	}

	var preimage *lntypes.Preimage
	if d.Preimage != nil {
		pimg := *d.Preimage
		preimage = &pimg
	}

	return &InvoiceHtlcAMPData{
		Record:   d.Record,
		Hash:     d.Hash,
		Preimage: preimage,
	}
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

	// AMP encapsulates additional data relevant to AMP HTLCs. This includes
	// the AMP onion record, in addition to the HTLC's payment hash and
	// preimage since these are unique to each AMP HTLC, and not the invoice
	// as a whole.
	//
	// NOTE: This value will only be set for AMP HTLCs.
	AMP *InvoiceHtlcAMPData
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

	// SetID is an optional set ID for AMP invoices that allows operations
	// to be more efficient by ensuring we don't need to read out the
	// entire HTLC set each timee an HTLC is to be cancelled.
	SetID *SetID
}

// InvoiceStateUpdateDesc describes an invoice-level state transition.
type InvoiceStateUpdateDesc struct {
	// NewState is the new state that this invoice should progress to.
	NewState ContractState

	// Preimage must be set to the preimage when NewState is settled.
	Preimage *lntypes.Preimage

	// HTLCPreimages set the HTLC-level preimages stored for AMP HTLCs.
	// These are only learned when settling the invoice as a whole. Must be
	// set when settling an invoice with non-nil SetID.
	HTLCPreimages map[CircuitKey]lntypes.Preimage

	// SetID identifies a specific set of HTLCs destined for the same
	// invoice as part of a larger AMP payment. This value will be nil for
	// legacy or MPP payments.
	SetID *[32]byte
}

// InvoiceUpdateCallback is a callback used in the db transaction to update the
// invoice.
type InvoiceUpdateCallback = func(invoice *Invoice) (*InvoiceUpdateDesc, error)

func validateInvoice(i *Invoice, paymentHash lntypes.Hash) error {
	// Avoid conflicts with all-zeroes magic value in the database.
	if paymentHash == unknownPreimage.Hash() {
		return fmt.Errorf("cannot use hash of all-zeroes preimage")
	}

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

	err := feature.ValidateDeps(i.Terms.Features)
	if err != nil {
		return err
	}

	// AMP invoices and hodl invoices are allowed to have no preimage
	// specified.
	isAMP := i.Terms.Features.HasFeature(
		lnwire.AMPOptional,
	)
	if i.Terms.PaymentPreimage == nil && !(i.HodlInvoice || isAMP) {
		return errors.New("non-hodl invoices must have a preimage")
	}

	if len(i.Htlcs) > 0 {
		return ErrInvoiceHasHtlcs
	}

	return nil
}

// IsPending returns true if the invoice is in ContractOpen state.
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

	if err := validateInvoice(newInvoice, paymentHash); err != nil {
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
			return ErrDuplicateInvoice
		}

		// Check that we aren't inserting an invoice with a duplicate
		// payment address. The all-zeros payment address is
		// special-cased to support legacy keysend invoices which don't
		// assign one. This is safe since later we also will avoid
		// indexing them and avoid collisions.
		payAddrIndex := tx.ReadWriteBucket(payAddrIndexBucket)
		if newInvoice.Terms.PaymentAddr != BlankPayAddr {
			if payAddrIndex.Get(newInvoice.Terms.PaymentAddr[:]) != nil {
				return ErrDuplicatePayAddr
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
func (d *DB) InvoicesAddedSince(sinceAddIndex uint64) ([]Invoice, error) {
	var newInvoices []Invoice

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
func (d *DB) LookupInvoice(ref InvoiceRef) (Invoice, error) {
	var invoice Invoice
	err := kvdb.View(d, func(tx kvdb.RTx) error {
		invoices := tx.ReadBucket(invoiceBucket)
		if invoices == nil {
			return ErrNoInvoicesCreated
		}
		invoiceIndex := invoices.NestedReadBucket(invoiceIndexBucket)
		if invoiceIndex == nil {
			return ErrNoInvoicesCreated
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

		var setID *SetID
		switch {
		// If this is a payment address ref, and the blank modified was
		// specified, then we'll use the zero set ID to indicate that
		// we won't want any HTLCs returned.
		case ref.PayAddr() != nil && ref.Modifier() == HtlcSetBlankModifier:
			var zeroSetID SetID
			setID = &zeroSetID

		// If this is a set ID ref, and the htlc set only modified was
		// specified, then we'll pass through the specified setID so
		// only that will be returned.
		case ref.SetID() != nil && ref.Modifier() == HtlcSetOnlyModifier:
			setID = (*SetID)(ref.SetID())
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
	ref InvoiceRef) ([]byte, error) {

	// If the set id is present, we only consult the set id index for this
	// invoice. This type of query is only used to facilitate user-facing
	// requests to lookup, settle or cancel an AMP invoice.
	setID := ref.SetID()
	if setID != nil {
		invoiceNumBySetID := setIDIndex.Get(setID[:])
		if invoiceNumBySetID == nil {
			return nil, ErrInvoiceNotFound
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
			if *payAddr != BlankPayAddr {
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
			return nil, ErrInvRefEquivocation
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
		return nil, ErrInvoiceNotFound
	}
}

// ScanInvoices scans through all invoices and calls the passed scanFunc for
// for each invoice with its respective payment hash. Additionally a reset()
// closure is passed which is used to reset/initialize partial results and also
// to signal if the kvdb.View transaction has been retried.
func (d *DB) ScanInvoices(
	scanFunc func(lntypes.Hash, *Invoice) error, reset func()) error {

	return kvdb.View(d, func(tx kvdb.RTx) error {
		invoices := tx.ReadBucket(invoiceBucket)
		if invoices == nil {
			return ErrNoInvoicesCreated
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
	var resp InvoiceSlice

	err := kvdb.View(d, func(tx kvdb.RTx) error {
		// If the bucket wasn't found, then there aren't any invoices
		// within the database yet, so we can simply exit.
		invoices := tx.ReadBucket(invoiceBucket)
		if invoices == nil {
			return ErrNoInvoicesCreated
		}

		// Get the add index bucket which we will use to iterate through
		// our indexed invoices.
		invoiceAddIndex := invoices.NestedReadBucket(addIndexBucket)
		if invoiceAddIndex == nil {
			return ErrNoInvoicesCreated
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
				opposite := numInvoices - i - 1
				resp.Invoices[i], resp.Invoices[opposite] =
					resp.Invoices[opposite], resp.Invoices[i]
			}
		}

		return nil
	}, func() {
		resp = InvoiceSlice{
			InvoiceQuery: q,
		}
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
func (d *DB) UpdateInvoice(ref InvoiceRef, setIDHint *SetID,
	callback InvoiceUpdateCallback) (*Invoice, error) {

	var updatedInvoice *Invoice
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
func (d *DB) InvoicesSettledSince(sinceSettleIndex uint64) ([]Invoice, error) {
	var settledInvoices []Invoice

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
				setID      *SetID
			)

			valueLen := copy(invoiceKey[:], indexValue)
			if len(indexValue) == invoiceSetIDKeyLen {
				setID = new(SetID)
				copy(setID[:], indexValue[valueLen:])
			}

			// For each key found, we'll look up the actual
			// invoice, then accumulate it into our return value.
			invoice, err := fetchInvoice(invoiceKey[:], invoices, setID)
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

	// Add the invoice to the payment address index, but only if the invoice
	// has a non-zero payment address. The all-zero payment address is still
	// in use by legacy keysend, so we special-case here to avoid
	// collisions.
	if i.Terms.PaymentAddr != BlankPayAddr {
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

	preimage := [32]byte(unknownPreimage)
	if i.Terms.PaymentPreimage != nil {
		preimage = *i.Terms.PaymentPreimage
		if preimage == unknownPreimage {
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
			i.AMPState.recordSize,
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
	ampInvoice := i.Terms.Features.HasFeature(
		lnwire.AMPOptional,
	)
	if ampInvoice {
		return nil
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
func fetchFilteredAmpInvoices(invoiceBucket kvdb.RBucket,
	invoiceNum []byte, setIDs ...*SetID) (map[CircuitKey]*InvoiceHTLC, error) {

	htlcs := make(map[CircuitKey]*InvoiceHTLC)
	for _, setID := range setIDs {
		invoiceSetIDKey := makeInvoiceSetIDKey(invoiceNum, setID[:])

		htlcSetBytes := invoiceBucket.Get(invoiceSetIDKey[:])
		if htlcSetBytes == nil {
			// A set ID was passed in, but we don't have this
			// stored yet, meaning that the setID is being added
			// for the first time.
			return htlcs, ErrInvoiceNotFound
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
func fetchAmpSubInvoices(invoiceBucket kvdb.RBucket,
	invoiceNum []byte, setIDs ...*SetID) (map[CircuitKey]*InvoiceHTLC, error) {

	// If a set of setIDs was specified, then we can skip the cursor and
	// just read out exactly what we need.
	if len(setIDs) != 0 && setIDs[0] != nil {
		return fetchFilteredAmpInvoices(
			invoiceBucket, invoiceNum, setIDs...,
		)
	}

	// Otherwise, iterate over all the htlc sets that are prefixed beside
	// this invoice in the main invoice bucket.
	htlcs := make(map[CircuitKey]*InvoiceHTLC)
	err := forEachAMPInvoice(invoiceBucket, invoiceNum, func(key, htlcSet []byte) error {
		htlcSetReader := bytes.NewReader(htlcSet)
		htlcsBySetID, err := deserializeHtlcs(htlcSetReader)
		if err != nil {
			return err
		}

		for key, htlc := range htlcsBySetID {
			htlcs[key] = htlc
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return htlcs, nil
}

// fetchInvoice attempts to read out the relevant state for the invoice as
// specified by the invoice number. If the setID fields are set, then only the
// HTLC information pertaining to those set IDs is returned.
func fetchInvoice(invoiceNum []byte, invoices kvdb.RBucket, setIDs ...*SetID) (Invoice, error) {
	invoiceBytes := invoices.Get(invoiceNum)
	if invoiceBytes == nil {
		return Invoice{}, ErrInvoiceNotFound
	}

	invoiceReader := bytes.NewReader(invoiceBytes)

	invoice, err := deserializeInvoice(invoiceReader)
	if err != nil {
		return Invoice{}, err
	}

	// If this is an AMP invoice, then we'll also attempt to read out the
	// set of HTLCs that were paid to prior set IDs. However, we'll only do
	// this is the invoice didn't already have HTLCs stored in-line.
	invoiceIsAMP := invoice.Terms.Features.HasFeature(
		lnwire.AMPOptional,
	)
	switch {
	case !invoiceIsAMP:
		return invoice, nil

	// For AMP invoice that already have HTLCs populated (created before
	// recurring invoices), then we don't need to read from the prefix
	// keyed section of the bucket.
	case invoiceIsAMP && len(invoice.Htlcs) != 0:
		return invoice, nil

	// If the "zero" setID was specified, then this means that no HTLC data
	// should be returned alongside of it.
	case invoiceIsAMP && len(setIDs) != 0 && setIDs[0] != nil &&
		*setIDs[0] == BlankPayAddr:

		return invoice, nil
	}

	invoice.Htlcs, err = fetchAmpSubInvoices(
		invoices, invoiceNum, setIDs...,
	)
	if err != nil {
		return invoice, nil
	}

	return invoice, nil
}

// fetchInvoiceStateAMP retrieves the state of all the relevant sub-invoice for
// an AMP invoice. This methods only decode the relevant state vs the entire
// invoice.
func fetchInvoiceStateAMP(invoiceNum []byte,
	invoices kvdb.RBucket) (AMPInvoiceState, error) {

	// Fetch the raw invoice bytes.
	invoiceBytes := invoices.Get(invoiceNum)
	if invoiceBytes == nil {
		return nil, ErrInvoiceNotFound
	}

	r := bytes.NewReader(invoiceBytes)

	var bodyLen int64
	err := binary.Read(r, byteOrder, &bodyLen)
	if err != nil {
		return nil, err
	}

	// Next, we'll make a new TLV stream that only attempts to decode the
	// bytes we actually need.
	ampState := make(AMPInvoiceState)
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

func deserializeInvoice(r io.Reader) (Invoice, error) {
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

	var i Invoice
	i.AMPState = make(AMPInvoiceState)
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
	if preimage != unknownPreimage {
		i.Terms.PaymentPreimage = &preimage
	}

	i.Terms.Value = lnwire.MilliSatoshi(value)
	i.Terms.FinalCltvDelta = int32(cltvDelta)
	i.Terms.Expiry = time.Duration(expiry)
	i.AmtPaid = lnwire.MilliSatoshi(amtPaid)
	i.State = ContractState(state)

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
	if v, ok := val.(*map[CircuitKey]struct{}); ok {
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

func decodeCircuitKeys(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*map[CircuitKey]struct{}); ok {
		// First, we'll read out the varint that encodes the number of
		// circuit keys encoded.
		numKeys, err := tlv.ReadVarInt(r, buf)
		if err != nil {
			return err
		}

		// Now that we know how many keys to expect, iterate reading each
		// one until we're done.
		for i := uint64(0); i < numKeys; i++ {
			var (
				key  CircuitKey
				scid uint64
			)

			if err := tlv.DUint64(r, &scid, buf, 8); err != nil {
				return err
			}

			key.ChanID = lnwire.NewShortChanIDFromInt(scid)

			if err := tlv.DUint64(r, &key.HtlcID, buf, 8); err != nil {
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
	if v, ok := val.(*AMPInvoiceState); ok {
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
			settleDateBytes, err := ampState.SettleDate.MarshalBinary()
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
					ampStateSettleIndexType, &ampState.SettleIndex,
				),
				tlv.MakePrimitiveRecord(
					ampStateSettleDateType, &settleDateBytes,
				),
				tlv.MakeDynamicRecord(
					ampStateCircuitKeysType,
					&ampState.InvoiceKeys,
					func() uint64 {
						// The record takes 8 bytes to encode the
						// set of circuits,  8 bytes for the scid
						// for the key, and 8 bytes for the HTLC
						// index.
						numKeys := uint64(len(ampState.InvoiceKeys))
						return tlv.VarIntSize(numKeys) + (numKeys * 16)
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

			if err := tlvStream.Encode(&ampStateTlvBytes); err != nil {
				return err
			}

			// We encode the record with a varint length followed by
			// the _raw_ TLV bytes.
			tlvLen := uint64(len(ampStateTlvBytes.Bytes()))
			if err := tlv.WriteVarInt(w, tlvLen, buf); err != nil {
				return err
			}

			if _, err := w.Write(ampStateTlvBytes.Bytes()); err != nil {
				return err
			}
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "channeldb.AMPInvoiceState")
}

// ampStateDecoder is a custom TLV decoder for the AMPInvoiceState record.
func ampStateDecoder(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*AMPInvoiceState); ok {
		// First, we'll decode the varint that encodes how many set IDs
		// are encoded within the greater map.
		numRecords, err := tlv.ReadVarInt(r, buf)
		if err != nil {
			return err
		}

		// Now that we know how many records we'll need to read, we can
		// iterate and read them all out in series.
		for i := uint64(0); i < numRecords; i++ {
			// Read out the varint that encodes the size of this inner
			// TLV record
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
				invoiceKeys     = make(map[CircuitKey]struct{})
				amtPaid         uint64
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
					ampStateSettleDateType, &settleDateBytes,
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

			if err := tlvStream.Decode(&innerTlvReader); err != nil {
				return err
			}

			var settleDate time.Time
			err = settleDate.UnmarshalBinary(settleDateBytes)
			if err != nil {
				return err
			}

			(*v)[setID] = InvoiceStateAMP{
				State:       HtlcState(htlcState),
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
func deserializeHtlcs(r io.Reader) (map[CircuitKey]*InvoiceHTLC, error) {
	htlcs := make(map[CircuitKey]*InvoiceHTLC)

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
		htlc.State = HtlcState(state)
		htlc.Amt = lnwire.MilliSatoshi(amt)
		htlc.MppTotalAmt = lnwire.MilliSatoshi(mppTotalAmt)
		if amp != nil && hash != nil {
			htlc.AMP = &InvoiceHtlcAMPData{
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
		HodlInvoice: src.HodlInvoice,
	}

	dest.Terms.Features = src.Terms.Features.Clone()

	if src.Terms.PaymentPreimage != nil {
		preimage := *src.Terms.PaymentPreimage
		dest.Terms.PaymentPreimage = &preimage
	}

	for k, v := range src.Htlcs {
		dest.Htlcs[k] = v.Copy()
	}

	return &dest
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
	htlcsToUpdate map[SetID]map[CircuitKey]*InvoiceHTLC) error {

	for setID, htlcSet := range htlcsToUpdate {
		// First write out the set of HTLCs including all the relevant TLV
		// values.
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
func updateHtlcsAmp(invoice *Invoice,
	updateMap map[SetID]map[CircuitKey]*InvoiceHTLC, htlc *InvoiceHTLC,
	setID SetID, circuitKey CircuitKey) {

	ampState, ok := invoice.AMPState[setID]
	if !ok {
		// If an entry for this set ID doesn't already exist, then
		// we'll need to create it.
		ampState = InvoiceStateAMP{
			State:       HtlcStateAccepted,
			InvoiceKeys: make(map[CircuitKey]struct{}),
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
			(*[32]byte)(&setID), HtlcStateAccepted,
		)
	}
	updateMap[setID][circuitKey] = htlc
}

// cancelHtlcsAmp processes a cancellation of an HTLC that belongs to an AMP
// HTLC set. We'll need to update the meta data in the  main invoice, and also
// apply the new update to the update MAP, since all the HTLCs for a given HTLC
// set need to be written in-line with each other.
func cancelHtlcsAmp(invoice *Invoice,
	updateMap map[SetID]map[CircuitKey]*InvoiceHTLC, htlc *InvoiceHTLC,
	circuitKey CircuitKey) {

	setID := htlc.AMP.Record.SetID()

	// First, we'll update the state of the entire HTLC set to cancelled.
	ampState := invoice.AMPState[setID]
	ampState.State = HtlcStateCanceled

	ampState.InvoiceKeys[circuitKey] = struct{}{}
	ampState.AmtPaid -= htlc.Amt

	// With the state update,d we'll set the new value so the struct
	// changes are propagated.
	invoice.AMPState[setID] = ampState

	if _, ok := updateMap[setID]; !ok {
		// Only HTLCs in the accepted state, can be cancelled, but we
		// also want to merge that with HTLCs that may be canceled as
		// well since it can be cancelled one by one.
		updateMap[setID] = invoice.HTLCSet(&setID, HtlcStateAccepted)

		cancelledHtlcs := invoice.HTLCSet(&setID, HtlcStateCanceled)
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
func settleHtlcsAmp(invoice *Invoice,
	settledSetIDs map[SetID]struct{},
	updateMap map[SetID]map[CircuitKey]*InvoiceHTLC, htlc *InvoiceHTLC,
	circuitKey CircuitKey) {

	// First, add the set ID to the set that was settled in this invoice
	// update. We'll use this later to update the settle index.
	setID := htlc.AMP.Record.SetID()
	settledSetIDs[setID] = struct{}{}

	// Next update the main AMP meta-data to indicate that this HTLC set
	// has been fully settled.
	ampState := invoice.AMPState[setID]
	ampState.State = HtlcStateSettled

	ampState.InvoiceKeys[circuitKey] = struct{}{}

	invoice.AMPState[setID] = ampState

	// Finally, we'll add this to the set of HTLCs that need to be updated.
	if _, ok := updateMap[setID]; !ok {
		updateMap[setID] = make(map[CircuitKey]*InvoiceHTLC)
	}
	updateMap[setID][circuitKey] = htlc
}

// updateInvoice fetches the invoice, obtains the update descriptor from the
// callback and applies the updates in a single db transaction.
func (d *DB) updateInvoice(hash *lntypes.Hash, refSetID *SetID, invoices,
	settleIndex, setIDIndex kvdb.RwBucket, invoiceNum []byte,
	callback InvoiceUpdateCallback) (*Invoice, error) {

	// If the set ID is non-nil, then we'll use that to filter out the
	// HTLCs for AMP invoice so we don't need to read them all out to
	// satisfy the invoice callback below. If it's nil, then we pass in the
	// zero set ID which means no HTLCs will be read out.
	var invSetID SetID
	if refSetID != nil {
		invSetID = *refSetID
	}
	invoice, err := fetchInvoice(invoiceNum, invoices, &invSetID)
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

	var (
		newState = invoice.State
		setID    *[32]byte
	)

	// We can either get the set ID from the main state update (if the
	// state is changing), or via the hint passed in returned by the update
	// call back.
	if update.State != nil {
		setID = update.State.SetID
		newState = update.State.NewState
	} else if update.SetID != nil {
		// When we go to cancel HTLCs, there's no new state, but the
		// set of HTLCs to be cancelled along with the setID affected
		// will be passed in.
		setID = (*[32]byte)(update.SetID)
	}

	now := d.clock.Now()

	invoiceIsAMP := invoiceCopy.Terms.Features.HasFeature(
		lnwire.AMPOptional,
	)

	// Process add actions from update descriptor.
	htlcsAmpUpdate := make(map[SetID]map[CircuitKey]*InvoiceHTLC)
	for key, htlcUpdate := range update.AddHtlcs {
		if _, exists := invoice.Htlcs[key]; exists {
			return nil, fmt.Errorf("duplicate add of htlc %v", key)
		}

		// Force caller to supply htlc without custom records in a
		// consistent way.
		if htlcUpdate.CustomRecords == nil {
			return nil, errors.New("nil custom records map")
		}

		// If a newly added HTLC has an associated set id, use it to
		// index this invoice in the set id index. An error is returned
		// if we find the index already points to a different invoice.
		var setID [32]byte
		if htlcUpdate.AMP != nil {
			setID = htlcUpdate.AMP.Record.SetID()
			setIDInvNum := setIDIndex.Get(setID[:])
			if setIDInvNum == nil {
				err = setIDIndex.Put(setID[:], invoiceNum)
				if err != nil {
					return nil, err
				}
			} else if !bytes.Equal(setIDInvNum, invoiceNum) {
				return nil, ErrDuplicateSetID{setID: setID}
			}
		}

		htlc := &InvoiceHTLC{
			Amt:           htlcUpdate.Amt,
			MppTotalAmt:   htlcUpdate.MppTotalAmt,
			Expiry:        htlcUpdate.Expiry,
			AcceptHeight:  uint32(htlcUpdate.AcceptHeight),
			AcceptTime:    now,
			State:         HtlcStateAccepted,
			CustomRecords: htlcUpdate.CustomRecords,
			AMP:           htlcUpdate.AMP.Copy(),
		}

		invoice.Htlcs[key] = htlc

		// Collect the set of new HTLCs so we can write them properly
		// below, but only if this is an AMP invoice.
		if invoiceIsAMP {
			updateHtlcsAmp(
				&invoice, htlcsAmpUpdate, htlc, setID, key,
			)
		}
	}

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

		// Consistency check to verify that there is no overlap between
		// the add and cancel sets.
		if _, added := update.AddHtlcs[key]; added {
			return nil, fmt.Errorf("added htlc %v canceled", key)
		}

		err := cancelSingleHtlc(now, htlc, newState)
		if err != nil {
			return nil, err
		}

		// Delete processed cancel action, so that we can check later
		// that there are no actions left.
		delete(cancelHtlcs, key)

		// Tally this into the set of HTLCs that need to be updated on
		// disk, but once again, only if this is an AMP invoice.
		if invoiceIsAMP {
			cancelHtlcsAmp(
				&invoice, htlcsAmpUpdate, htlc, key,
			)
		}
	}

	// Verify that we didn't get an action for htlcs that are not present on
	// the invoice.
	if len(cancelHtlcs) > 0 {
		return nil, errors.New("cancel action on non-existent htlc(s)")
	}

	// At this point, the set of accepted HTLCs should be fully
	// populated with added HTLCs or removed of canceled ones. Update
	// invoice state if the update descriptor indicates an invoice state
	// change, which depends on having an accurate view of the accepted
	// HTLCs.
	if update.State != nil {
		newState, err := updateInvoiceState(
			&invoice, hash, *update.State,
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
		if !invoiceIsAMP || *newState == ContractCanceled {
			invoice.State = *newState
		}

		// If this is a non-AMP invoice, then the state can eventually
		// go to ContractSettled, so we pass in  nil value as part of
		// setSettleMetaFields.
		if !invoiceIsAMP && update.State.NewState == ContractSettled {
			err := setSettleMetaFields(
				settleIndex, invoiceNum, &invoice, now, nil,
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
		settledSetIDs = make(map[SetID]struct{})
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
				return nil, ErrHTLCPreimageAlreadyExists
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
			htlcContextState = ContractSettled
		}
		htlcSettled, err := updateHtlc(
			now, htlc, htlcContextState, setID,
		)
		if err != nil {
			return nil, err
		}

		// If the HTLC has being settled for the first time, and this
		// is an AMP invoice, then we'll need to update some additional
		// meta data state.
		if htlcSettled && invoiceIsAMP {
			settleHtlcsAmp(
				&invoice, settledSetIDs, htlcsAmpUpdate, htlc, key,
			)
		}

		invoiceStateReady := (htlc.State == HtlcStateAccepted ||
			htlc.State == HtlcStateSettled)
		if !invoiceIsAMP {
			// Update the running amount paid to this invoice. We
			// don't include accepted htlcs when the invoice is
			// still open.
			if invoice.State != ContractOpen && invoiceStateReady {
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
			if invoice.State == ContractOpen && invoiceStateReady {
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
			settleIndex, invoiceNum, &invoice, now, &settledSetID,
		)
		if err != nil {
			return nil, err
		}
	}

	// Reserialize and update invoice.
	var buf bytes.Buffer
	if err := serializeInvoice(&buf, &invoice); err != nil {
		return nil, err
	}

	if err := invoices.Put(invoiceNum[:], buf.Bytes()); err != nil {
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

	return &invoice, nil
}

// updateInvoiceState validates and processes an invoice state update. The new
// state to transition to is returned, so the caller is able to select exactly
// how the invoice state is updated.
func updateInvoiceState(invoice *Invoice, hash *lntypes.Hash,
	update InvoiceStateUpdateDesc) (*ContractState, error) {

	// Returning to open is never allowed from any state.
	if update.NewState == ContractOpen {
		return nil, ErrInvoiceCannotOpen
	}

	switch invoice.State {
	// Once a contract is accepted, we can only transition to settled or
	// canceled. Forbid transitioning back into this state. Otherwise this
	// state is identical to ContractOpen, so we fallthrough to apply the
	// same checks that we apply to open invoices.
	case ContractAccepted:
		if update.NewState == ContractAccepted {
			return nil, ErrInvoiceCannotAccept
		}

		fallthrough

	// If a contract is open, permit a state transition to accepted, settled
	// or canceled. The only restriction is on transitioning to settled
	// where we ensure the preimage is valid.
	case ContractOpen:
		if update.NewState == ContractCanceled {
			return &update.NewState, nil
		}

		// Sanity check that the user isn't trying to settle or accept a
		// non-existent HTLC set.
		if len(invoice.HTLCSet(update.SetID, HtlcStateAccepted)) == 0 {
			return nil, ErrEmptyHTLCSet
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
			return nil, ErrUnexpectedInvoicePreimage

		// Validate the supplied preimage for non-AMP invoices.
		case update.Preimage != nil:
			if update.Preimage.Hash() != *hash {
				return nil, ErrInvoicePreimageMismatch
			}
			invoice.Terms.PaymentPreimage = update.Preimage

		// Permit non-AMP invoices to be accepted without knowing the
		// preimage. When trying to settle we'll have to pass through
		// the above check in order to not hit the one below.
		case update.NewState == ContractAccepted:

		// Fail if we still don't have a preimage when transitioning to
		// settle the non-AMP invoice.
		case update.NewState == ContractSettled &&
			invoice.Terms.PaymentPreimage == nil:

			return nil, errors.New("unknown preimage")
		}

		return &update.NewState, nil

	// Once settled, we are in a terminal state.
	case ContractSettled:
		return nil, ErrInvoiceAlreadySettled

	// Once canceled, we are in a terminal state.
	case ContractCanceled:
		return nil, ErrInvoiceAlreadyCanceled

	default:
		return nil, errors.New("unknown state transition")
	}
}

// cancelSingleHtlc validates cancellation of a single htlc and update its state.
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

// updateHtlc aligns the state of an htlc with the given invoice state. A
// boolean is returned if the HTLC was settled.
func updateHtlc(resolveTime time.Time, htlc *InvoiceHTLC,
	invState ContractState, setID *[32]byte) (bool, error) {

	trySettle := func(persist bool) (bool, error) {
		if htlc.State != HtlcStateAccepted {
			return false, nil
		}

		// Settle the HTLC if it matches the settled set id. If
		// there're other HTLCs with distinct setIDs, then we'll leave
		// them, as they may eventually be settled as we permit
		// multiple settles to a single pay_addr for AMP.
		var htlcState HtlcState
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
				return false, ErrHTLCPreimageMissing

			// Fail if the accepted AMP HTLC has an invalid
			// preimage.
			case !htlc.AMP.Preimage.Matches(htlc.AMP.Hash):
				return false, ErrHTLCPreimageMismatch
			}

			htlcState = HtlcStateSettled
		}

		// Only persist the changes if the invoice is moving to the
		// settled state, and we're actually updating the state to
		// settled.
		if persist && htlcState == HtlcStateSettled {
			htlc.State = htlcState
			htlc.ResolveTime = resolveTime
		}

		return persist && htlcState == HtlcStateSettled, nil
	}

	if invState == ContractSettled {
		// Check that we can settle the HTLCs. For legacy and MPP HTLCs
		// this will be a NOP, but for AMP HTLCs this asserts that we
		// have a valid hash/preimage pair. Passing true permits the
		// method to update the HTLC to HtlcStateSettled.
		return trySettle(true)
	}

	// We should never find a settled HTLC on an invoice that isn't in
	// ContractSettled.
	if htlc.State == HtlcStateSettled {
		return false, ErrHTLCAlreadySettled
	}

	switch invState {
	case ContractCanceled:
		if htlc.State == HtlcStateAccepted {
			htlc.State = HtlcStateCanceled
			htlc.ResolveTime = resolveTime
		}
		return false, nil

	// TODO(roasbeef): never fully passed thru now?
	case ContractAccepted:
		// Check that we can settle the HTLCs. For legacy and MPP HTLCs
		// this will be a NOP, but for AMP HTLCs this asserts that we
		// have a valid hash/preimage pair. Passing false prevents the
		// method from putting the HTLC in HtlcStateSettled, leaving it
		// in HtlcStateAccepted.
		return trySettle(false)

	case ContractOpen:
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
	invoice *Invoice, now time.Time, setID *SetID) error {

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
	if err := settleIndex.Put(seqNoBytes[:], indexKey[:valueLen]); err != nil {
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
	err := forEachAMPInvoice(invoiceBucket, invoiceNum, func(cursorKey, v []byte) error {
		keysToDel = append(keysToDel, cursorKey)
		return nil
	})
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
func delAMPSettleIndex(invoiceNum []byte, invoices, settleIndex kvdb.RwBucket) error {
	// First, we need to grab the AMP invoice state to see if there's
	// anything that we even need to delete.
	ampState, err := fetchInvoiceStateAMP(invoiceNum, invoices)
	if err != nil {
		return err
	}

	// If there's no AMP state at all (non-AMP invoice), then we can return early.
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

// InvoiceDeleteRef holds a reference to an invoice to be deleted.
type InvoiceDeleteRef struct {
	// PayHash is the payment hash of the target invoice. All invoices are
	// currently indexed by payment hash.
	PayHash lntypes.Hash

	// PayAddr is the payment addr of the target invoice. Newer invoices
	// (0.11 and up) are indexed by payment address in addition to payment
	// hash, but pre 0.8 invoices do not have one at all.
	PayAddr *[32]byte

	// AddIndex is the add index of the invoice.
	AddIndex uint64

	// SettleIndex is the settle index of the invoice.
	SettleIndex uint64
}

// DeleteInvoice attempts to delete the passed invoices from the database in
// one transaction. The passed delete references hold all keys required to
// delete the invoices without also needing to deserialze them.
func (d *DB) DeleteInvoice(invoicesToDelete []InvoiceDeleteRef) error {
	err := kvdb.Update(d, func(tx kvdb.RwTx) error {
		invoices := tx.ReadWriteBucket(invoiceBucket)
		if invoices == nil {
			return ErrNoInvoicesCreated
		}

		invoiceIndex := invoices.NestedReadWriteBucket(
			invoiceIndexBucket,
		)
		if invoiceIndex == nil {
			return ErrNoInvoicesCreated
		}

		invoiceAddIndex := invoices.NestedReadWriteBucket(
			addIndexBucket,
		)
		if invoiceAddIndex == nil {
			return ErrNoInvoicesCreated
		}

		// settleIndex can be nil, as the bucket is created lazily
		// when the first invoice is settled.
		settleIndex := invoices.NestedReadWriteBucket(settleIndexBucket)

		payAddrIndex := tx.ReadWriteBucket(payAddrIndexBucket)

		for _, ref := range invoicesToDelete {
			// Fetch the invoice key for using it to check for
			// consistency and also to delete from the invoice index.
			invoiceKey := invoiceIndex.Get(ref.PayHash[:])
			if invoiceKey == nil {
				return ErrInvoiceNotFound
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
					// Delete from the payment address index.
					// Note that since the payment address
					// index has been introduced with an
					// empty migration it may be possible
					// that the index doesn't have an entry
					// for this invoice.
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
