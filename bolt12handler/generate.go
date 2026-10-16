package bolt12handler

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/bits"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// errAmountOverflow is returned when offer_amount times
	// invreq_quantity does not fit in a uint64.
	errAmountOverflow = errors.New("invoice amount overflows uint64")

	// errOfferWithoutIssuerID is returned for an offer that has
	// offer_paths but no offer_issuer_id. Its invoice must use the blinded
	// node id of the arrival path, which GenerateInvoice does not support
	// yet.
	errOfferWithoutIssuerID = errors.New("offer without " +
		"offer_issuer_id is not supported")

	// errCurrencyConversion is returned when the invoice amount needs a
	// conversion from offer_currency to msat. The handler has no exchange
	// rate.
	errCurrencyConversion = errors.New("offer_currency amount needs " +
		"conversion to msat")
)

// InvoiceConfig holds the dependencies and node settings of GenerateInvoice.
type InvoiceConfig struct {
	// Signer signs the invoice with the node identity key.
	Signer NodeSigner

	// PathBuilder constructs the blinded payment paths. If it is nil, the
	// invoice uses a single-hop blinded path to this node.
	PathBuilder PaymentPathBuilder

	// Clock provides the invoice creation time.
	Clock clock.Clock

	// FinalCLTVDelta is the final CLTV delta that this node requires for
	// a payment. The daemon passes the same value as for BOLT 11 invoices.
	// The payment paths add routing.BlockPadding to it.
	FinalCLTVDelta uint16
}

// Validate returns an error if a required dependency of the config is not
// set. PathBuilder is optional.
func (c *InvoiceConfig) Validate() error {
	switch {
	case c.Signer == nil:
		return errors.New("invoice config has no signer")

	case c.Clock == nil:
		return errors.New("invoice config has no clock")
	}

	return nil
}

// PaymentPathResult contains the blinded payment paths and corresponding pay
// info for a BOLT 12 invoice.
type PaymentPathResult struct {
	// Paths contains the blinded payment paths for the invoice.
	Paths []lnwire.BlindedPath

	// PayInfos contains the fee and CLTV policy for each path.
	PayInfos []bolt12.BlindedPayInfo
}

// PaymentPathBuilder constructs blinded payment paths for a BOLT 12 invoice.
// The builder receives the invoice amount and a path_id to embed in the final
// hop's encrypted data.
type PaymentPathBuilder interface {
	// BuildPaymentPaths returns blinded payment paths suitable for
	// embedding in a BOLT 12 invoice.
	BuildPaymentPaths(amountMsat uint64,
		pathID []byte) (*PaymentPathResult, error)
}

// InvoiceResult contains the output of invoice generation.
type InvoiceResult struct {
	// Invoice is the generated BOLT 12 invoice.
	Invoice *bolt12.Invoice

	// Encoded is the bech32-encoded invoice string (lni1...).
	Encoded string

	// Preimage is the 32-byte payment preimage.
	Preimage lntypes.Preimage

	// PaymentHash is the SHA256 of the preimage.
	PaymentHash lntypes.Hash

	// PathID is the 32-byte path identifier embedded in the blinded path.
	// Used as payment_addr for invoice lookup.
	PathID [32]byte
}

// GenerateInvoice creates a BOLT 12 invoice in response to a validated invoice
// request. The invoice mirrors the request, and the node identity key signs
// it. If cfg.PathBuilder is nil or fails, a single-hop blinded path is used as
// fallback.
func GenerateInvoice(cfg *InvoiceConfig,
	ir *bolt12.InvoiceRequest) (*InvoiceResult, error) {

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// The invoice_node_id is always the node identity key. For an offer
	// without offer_issuer_id, the payer expects the blinded node id of
	// the arrival path, so such an invoice would fail and show our
	// identity.
	//
	// TODO: Sign with the blinded node id key to support these offers.
	if ir.OfferPaths.IsSome() && !ir.OfferIssuerID.IsSome() {
		return nil, errOfferWithoutIssuerID
	}

	var preimage lntypes.Preimage
	if _, err := rand.Read(preimage[:]); err != nil {
		return nil, fmt.Errorf("generate preimage: %w", err)
	}
	paymentHash := preimage.Hash()

	var pathID [32]byte
	if _, err := rand.Read(pathID[:]); err != nil {
		return nil, fmt.Errorf("generate path_id: %w", err)
	}

	invoiceAmount, err := computeInvoiceAmount(ir)
	if err != nil {
		return nil, err
	}

	var pathResult *PaymentPathResult
	if cfg.PathBuilder != nil {
		pathResult, err = cfg.PathBuilder.BuildPaymentPaths(
			invoiceAmount, pathID[:],
		)
		switch {
		case err != nil:
			log.Debugf("Multi-hop payment path construction "+
				"failed, falling back to single-hop: %v",
				err)

			pathResult = nil

		case pathResult != nil && len(pathResult.Paths) == 0:
			log.Debugf("Multi-hop payment path construction " +
				"returned no paths, falling back to single-hop")

			pathResult = nil
		}
	}

	// The single-hop path keeps the offer payable when the builder finds
	// no path through a peer. It names our node as the introduction node.
	// This shows nothing new, because GenerateInvoice only answers an
	// offer that names our node in offer_issuer_id.
	//
	// TODO: A blinded offer hides our node, so its invoice must fail here
	// instead of using the single-hop path.
	if pathResult == nil {
		path, pathErr := buildSingleHopBlindedPath(
			cfg.Signer.NodePubKey(), pathID[:],
		)
		if pathErr != nil {
			return nil, fmt.Errorf("build single-hop path: %w",
				pathErr)
		}

		log.Debugf("Using single-hop blinded payment path")

		pathResult = &PaymentPathResult{
			Paths: []lnwire.BlindedPath{path},
			PayInfos: []bolt12.BlindedPayInfo{{
				FeeBaseMsat:               0,
				FeeProportionalMillionths: 0,
				// Add BlockPadding so that the receiver does
				// not reject the HTLC if blocks are mined while
				// the payment is in flight. With blinded paths,
				// the receiver adds this padding, not the
				// sender.
				CltvExpiryDelta: cfg.FinalCLTVDelta +
					routing.BlockPadding,
				HtlcMinimumMsat: 0,
				HtlcMaximumMsat: invoiceAmount,
			}},
		}
	} else {
		log.Debugf("Using multi-hop blinded payment path with "+
			"%d path(s)", len(pathResult.Paths))
	}

	inv := buildInvoiceFromRequest(
		ir, cfg.Signer.NodePubKey(), paymentHash, pathResult,
		invoiceAmount, cfg.Clock.Now(),
	)

	signedInv, encoded, err := signAndEncode(inv, cfg.Signer)
	if err != nil {
		return nil, fmt.Errorf("sign invoice: %w", err)
	}

	return &InvoiceResult{
		Invoice:     signedInv,
		Encoded:     encoded,
		Preimage:    preimage,
		PaymentHash: paymentHash,
		PathID:      pathID,
	}, nil
}

// buildInvoiceFromRequest constructs a bolt12.Invoice by mirroring the request
// fields and adding invoice-specific fields.
func buildInvoiceFromRequest(ir *bolt12.InvoiceRequest,
	nodePubKey *btcec.PublicKey, paymentHash lntypes.Hash,
	pathResult *PaymentPathResult,
	invoiceAmount uint64, now time.Time) *bolt12.Invoice {

	// The codec mirrors the request, including unknown TLVs in the signed
	// range. The payer's byte-for-byte check requires them, and a
	// field-by-field copy could not reach them.
	inv := bolt12.NewInvoiceFromRequest(ir)

	inv.InvoiceCreatedAt = tlv.SomeRecordT(
		tlv.RecordT[tlv.TlvType164, bolt12.TUint64]{
			Val: bolt12.TUint64(now.Unix()),
		},
	)

	// TODO: Support a custom invoice expiry from the config. Set
	// invoice_relative_expiry here, and store the invoice with the same
	// expiry.
	amt := bolt12.TUint64(invoiceAmount)
	inv.InvoiceAmount = tlv.SomeRecordT(
		tlv.RecordT[tlv.TlvType170, bolt12.TUint64]{
			Val: amt,
		},
	)

	inv.InvoicePaymentHash = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType168, [32]byte](paymentHash),
	)

	inv.InvoiceNodeID = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType176](nodePubKey),
	)

	inv.InvoicePaths = tlv.SomeRecordT(
		tlv.RecordT[tlv.TlvType160, lnwire.BlindedPaths]{
			Val: lnwire.BlindedPaths{
				Paths: pathResult.Paths,
			},
		},
	)

	// One blinded pay info entry per payment path.
	inv.InvoiceBlindedPay = tlv.SomeRecordT(
		tlv.RecordT[tlv.TlvType162, bolt12.BlindedPayInfos]{
			Val: bolt12.BlindedPayInfos{
				Infos: pathResult.PayInfos,
			},
		},
	)

	// Advertise OPT_BASIC_MPP so payers can split a payment that exceeds
	// the capacity of one channel.
	inv.InvoiceFeatures = tlv.SomeRecordT(
		tlv.RecordT[tlv.TlvType174, lnwire.RawFeatureVector]{
			Val: *lnwire.NewRawFeatureVector(lnwire.MPPOptional),
		},
	)

	return inv
}

// computeInvoiceAmount returns the amount the invoice must pay. The request's
// invreq_amount takes precedence. Without it, the amount comes from the offer,
// and a currency offer or an overflowing product returns an error.
func computeInvoiceAmount(ir *bolt12.InvoiceRequest) (uint64, error) {
	// An absent invreq_quantity means one item. Like CLN, reject an
	// explicit zero, even when invreq_amount sets the amount.
	quantity := uint64(ir.InvreqQuantity.ValOpt().UnwrapOr(1))
	if quantity == 0 {
		return 0, bolt12.ErrQuantityZero
	}

	if ir.InvreqAmount.IsSome() {
		return uint64(ir.InvreqAmount.ValOpt().UnwrapOr(0)), nil
	}

	// The offer_amount unit is offer_currency when that field is set, so
	// it is not a msat value.
	//
	// TODO: Support offers priced in a currency. This needs an exchange
	// rate to convert offer_amount to msat.
	if ir.OfferCurrency.IsSome() {
		return 0, errCurrencyConversion
	}

	// BOLT 12 requires invreq_amount for an offer without offer_amount.
	if !ir.OfferAmount.IsSome() {
		return 0, bolt12.ErrMissingAmount
	}

	// The reader's overflow check runs only when invreq_amount is set, so
	// a payer could otherwise pick a quantity that wraps the product.
	return expectedOfferAmount(
		uint64(ir.OfferAmount.ValOpt().UnwrapOr(0)), quantity,
	)
}

// expectedOfferAmount returns offer_amount times the quantity, the expected
// amount of an offer priced in the chain's currency.
func expectedOfferAmount(offerAmount, quantity uint64) (uint64, error) {
	hi, amount := bits.Mul64(offerAmount, quantity)
	if hi != 0 {
		return 0, errAmountOverflow
	}

	return amount, nil
}

// buildSingleHopBlindedPath creates a single-hop blinded payment path for the
// direct-peer case. The introduction node is the receiver itself, and the
// single hop's encrypted data carries the path_id for invoice lookup.
func buildSingleHopBlindedPath(nodePubKey *btcec.PublicKey,
	pathID []byte) (lnwire.BlindedPath, error) {

	sessionKey, err := btcec.NewPrivateKey()
	if err != nil {
		return lnwire.BlindedPath{}, fmt.Errorf("generate session "+
			"key: %w", err)
	}

	// The path_id lets the receiver match the incoming HTLC to the invoice.
	routeData := record.NewFinalHopBlindedRouteData(nil, pathID)
	plainText, err := record.EncodeBlindedRouteData(routeData)
	if err != nil {
		return lnwire.BlindedPath{}, fmt.Errorf("encode route data: %w",
			err)
	}

	hops := []*sphinx.HopInfo{
		{
			NodePub:   nodePubKey,
			PlainText: plainText,
		},
	}

	blindedPath, err := sphinx.BuildBlindedPath(sessionKey, hops)
	if err != nil {
		return lnwire.BlindedPath{}, fmt.Errorf("build blinded "+
			"path: %w", err)
	}

	path := blindedPath.Path

	bolt12Hops := make([]lnwire.BlindedHop, len(path.BlindedHops))
	for i, hop := range path.BlindedHops {
		bolt12Hops[i] = lnwire.BlindedHop{
			BlindedNodeID: hop.BlindedNodePub,
			EncryptedData: hop.CipherText,
		}
	}

	introNode, err := lnwire.NewPubkeyIntro(nodePubKey)
	if err != nil {
		return lnwire.BlindedPath{}, err
	}

	return lnwire.BlindedPath{
		IntroductionNode: introNode,
		BlindingPoint:    path.BlindingPoint,
		Hops:             bolt12Hops,
	}, nil
}

// signAndEncode attaches the signature to inv in place and returns the bech32
// string of the signed invoice.
func signAndEncode(inv *bolt12.Invoice, signer NodeSigner) (*bolt12.Invoice,
	string, error) {

	// The root function runs the writer validation, so a malformed invoice
	// fails before a key signs it.
	root, err := bolt12.InvoiceSigningRoot(inv)
	if err != nil {
		return nil, "", err
	}

	sig, err := signer.SignInvoiceRoot(root)
	if err != nil {
		return nil, "", fmt.Errorf("sign: %w", err)
	}

	inv.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240, [64]byte](sig),
	)

	encoded, err := bolt12.EncodeInvoiceString(inv)
	if err != nil {
		return nil, "", err
	}

	return inv, encoded, nil
}
