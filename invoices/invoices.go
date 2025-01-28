package invoices

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
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
)

var (
	// unknownPreimage is an all-zeroes preimage that indicates that the
	// preimage for this invoice is not yet known.
	UnknownPreimage lntypes.Preimage

	// BlankPayAddr is a sentinel payment address for legacy invoices.
	// Invoices with this payment address are special-cased in the insertion
	// logic to prevent being indexed in the payment address index,
	// otherwise they would cause collisions after the first insertion.
	BlankPayAddr [32]byte
)

// RefModifier is a modification on top of a base invoice ref. It allows the
// caller to opt to skip out on HTLCs for a given payAddr, or only return the
// set of specified HTLCs for a given setID.
type RefModifier uint8

const (
	// DefaultModifier is the base modifier that doesn't change any
	// behavior.
	DefaultModifier RefModifier = iota

	// HtlcSetOnlyModifier can only be used with a setID based invoice ref,
	// and specifies that only the set of HTLCs related to that setID are
	// to be returned.
	HtlcSetOnlyModifier

	// HtlcSetOnlyModifier can only be used with a payAddr based invoice
	// ref, and specifies that the returned invoice shouldn't include any
	// HTLCs at all.
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

// InvoiceRefByAddrBlankHtlc creates an InvoiceRef that queries the payment addr
// index for an invoice with the provided payment address, but excludes any of
// the core HTLC information.
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

// IsHashOnly returns true if the invoice ref only contains a payment hash.
func (r InvoiceRef) IsHashOnly() bool {
	return r.payHash != nil && r.payAddr == nil && r.setID == nil
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

	// ContractSettled means the htlc is settled and the invoice has been
	// paid.
	ContractSettled ContractState = 1

	// ContractCanceled means the invoice has been canceled.
	ContractCanceled ContractState = 2

	// ContractAccepted means the HTLC has been accepted but not settled
	// yet.
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

// copy makes a deep copy of the underlying InvoiceStateAMP.
func (i *InvoiceStateAMP) copy() (InvoiceStateAMP, error) {
	result := *i

	// Make a copy of the InvoiceKeys map.
	result.InvoiceKeys = make(map[CircuitKey]struct{})
	for k := range i.InvoiceKeys {
		result.InvoiceKeys[k] = struct{}{}
	}

	// As a safety measure, copy SettleDate. time.Time is concurrency safe
	// except when using any of the (un)marshalling methods.
	settleDateBytes, err := i.SettleDate.MarshalBinary()
	if err != nil {
		return InvoiceStateAMP{}, err
	}

	err = result.SettleDate.UnmarshalBinary(settleDateBytes)
	if err != nil {
		return InvoiceStateAMP{}, err
	}

	return result, nil
}

// AMPInvoiceState represents a type that stores metadata related to the set of
// settled AMP "sub-invoices".
type AMPInvoiceState map[SetID]InvoiceStateAMP

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
func (i *Invoice) HTLCSet(setID *[32]byte,
	state HtlcState) map[CircuitKey]*InvoiceHTLC {

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

// IsKeysend returns true if the invoice is a Keysend invoice.
func (i *Invoice) IsKeysend() bool {
	// TODO(positiveblue): look for a more reliable way to tests if
	// an invoice is keysend.
	return len(i.PaymentRequest) == 0 && !i.IsAMP()
}

// IsAMP returns true if the invoice is an AMP invoice.
func (i *Invoice) IsAMP() bool {
	if i.Terms.Features == nil {
		return false
	}

	return i.Terms.Features.HasFeature(
		lnwire.AMPRequired,
	)
}

// IsBlinded returns true if the invoice contains blinded paths.
func (i *Invoice) IsBlinded() bool {
	if i.Terms.Features == nil {
		return false
	}

	return i.Terms.Features.IsSet(lnwire.Bolt11BlindedPathsRequired)
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

	// WireCustomRecords contains the custom key/value pairs that were only
	// included in p2p wire message of the HTLC and not in the onion
	// payload.
	WireCustomRecords lnwire.CustomRecords

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

	result.WireCustomRecords = h.WireCustomRecords.Copy()
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
	// Hash. The preimage will be derived either from secret share
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

// UpdateType is an enum that describes the type of update that was applied to
// an invoice.
type UpdateType uint8

const (
	// UnknownUpdate indicates that the UpdateType has not been set for a
	// given udpate. This kind of updates are not allowed.
	UnknownUpdate UpdateType = iota

	// CancelHTLCsUpdate indicates that this update cancels one or more
	// HTLCs.
	CancelHTLCsUpdate

	// AddHTLCsUpdate indicates that this update adds one or more HTLCs.
	AddHTLCsUpdate

	// SettleHodlInvoiceUpdate indicates that this update settles one or
	// more HTLCs from a hodl invoice.
	SettleHodlInvoiceUpdate

	// CancelInvoiceUpdate indicates that this update is trying to cancel
	// an invoice.
	CancelInvoiceUpdate
)

// String returns a human readable string for the UpdateType.
func (u UpdateType) String() string {
	switch u {
	case CancelHTLCsUpdate:
		return "CancelHTLCsUpdate"

	case AddHTLCsUpdate:
		return "AddHTLCsUpdate"

	case SettleHodlInvoiceUpdate:
		return "SettleHodlInvoiceUpdate"

	case CancelInvoiceUpdate:
		return "CancelInvoiceUpdate"

	default:
		return fmt.Sprintf("unknown invoice update type: %d", u)
	}
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

	// UpdateType indicates what type of update is being applied.
	UpdateType UpdateType
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
// TODO(ziggie): Add the option of additional return values to the callback
// for example the resolution which is currently assigned via an outer scope
// variable.
type InvoiceUpdateCallback = func(invoice *Invoice) (*InvoiceUpdateDesc, error)

// ValidateInvoice assures the invoice passes the checks for all the relevant
// constraints.
func ValidateInvoice(i *Invoice, paymentHash lntypes.Hash) error {
	// Avoid conflicts with all-zeroes magic value in the database.
	if paymentHash == UnknownPreimage.Hash() {
		return fmt.Errorf("cannot use hash of all-zeroes preimage")
	}

	if len(i.Memo) > MaxMemoSize {
		return fmt.Errorf("max length a memo is %v, and invoice "+
			"of length %v was provided", MaxMemoSize, len(i.Memo))
	}
	if len(i.PaymentRequest) > MaxPaymentRequestSize {
		return fmt.Errorf("max length of payment request is %v, "+
			"length provided was %v", MaxPaymentRequestSize,
			len(i.PaymentRequest))
	}
	if i.Terms.Features == nil {
		return errors.New("invoice must have a feature vector")
	}

	err := feature.ValidateDeps(i.Terms.Features)
	if err != nil {
		return err
	}

	if i.requiresPreimage() && i.Terms.PaymentPreimage == nil {
		return errors.New("this invoice must have a preimage")
	}

	if len(i.Htlcs) > 0 {
		return ErrInvoiceHasHtlcs
	}

	return nil
}

// requiresPreimage returns true if the invoice requires a preimage to be valid.
func (i *Invoice) requiresPreimage() bool {
	// AMP invoices and hodl invoices are allowed to have no preimage
	// specified.
	if i.HodlInvoice || i.IsAMP() {
		return false
	}

	return true
}

// IsPending returns true if the invoice is in ContractOpen state.
func (i *Invoice) IsPending() bool {
	return i.State == ContractOpen || i.State == ContractAccepted
}

// copySlice allocates a new slice and copies the source into it.
func copySlice(src []byte) []byte {
	dest := make([]byte, len(src))
	copy(dest, src)
	return dest
}

// CopyInvoice makes a deep copy of the supplied invoice.
func CopyInvoice(src *Invoice) (*Invoice, error) {
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
		AMPState:    make(map[SetID]InvoiceStateAMP),
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

	// Lastly, copy the amp invoice state.
	for k, v := range src.AMPState {
		ampInvState, err := v.copy()
		if err != nil {
			return nil, err
		}

		dest.AMPState[k] = ampInvState
	}

	return &dest, nil
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
