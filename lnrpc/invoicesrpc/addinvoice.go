package invoicesrpc

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/zpay32"
)

// AddInvoiceConfig contains dependencies for invoice creation.
type AddInvoiceConfig struct {
	// AddInvoice is called to add the invoice to the registry.
	AddInvoice func(invoice *channeldb.Invoice, paymentHash lntypes.Hash) (
		uint64, error)

	// IsChannelActive is used to generate valid hop hints.
	IsChannelActive func(chanID lnwire.ChannelID) bool

	// ChainParams are required to properly decode invoice payment requests
	// that are marshalled over rpc.
	ChainParams *chaincfg.Params

	// NodeSigner is an implementation of the MessageSigner implementation
	// that's backed by the identity private key of the running lnd node.
	NodeSigner *netann.NodeSigner

	// DefaultCLTVExpiry is the default invoice expiry if no values is
	// specified.
	DefaultCLTVExpiry uint32

	// ChanDB is a global boltdb instance which is needed to access the
	// channel graph.
	ChanDB *channeldb.DB

	// GenInvoiceFeatures returns a feature containing feature bits that
	// should be advertised on freshly generated invoices.
	GenInvoiceFeatures func() *lnwire.FeatureVector
}

// AddInvoiceData contains the required data to create a new invoice.
type AddInvoiceData struct {
	// An optional memo to attach along with the invoice. Used for record
	// keeping purposes for the invoice's creator, and will also be set in
	// the description field of the encoded payment request if the
	// description_hash field is not being used.
	Memo string

	// The preimage which will allow settling an incoming HTLC payable to
	// this preimage. If Preimage is set, Hash should be nil. If both
	// Preimage and Hash are nil, a random preimage is generated.
	Preimage *lntypes.Preimage

	// The hash of the preimage. If Hash is set, Preimage should be nil.
	// This condition indicates that we have a 'hold invoice' for which the
	// htlc will be accepted and held until the preimage becomes known.
	Hash *lntypes.Hash

	// The value of this invoice in millisatoshis.
	Value lnwire.MilliSatoshi

	// Hash (SHA-256) of a description of the payment. Used if the
	// description of payment (memo) is too long to naturally fit within the
	// description field of an encoded payment request.
	DescriptionHash []byte

	// Payment request expiry time in seconds. Default is 3600 (1 hour).
	Expiry int64

	// Fallback on-chain address.
	FallbackAddr string

	// Delta to use for the time-lock of the CLTV extended to the final hop.
	CltvExpiry uint64

	// Whether this invoice should include routing hints for private
	// channels.
	Private bool

	// HodlInvoice signals that this invoice shouldn't be settled
	// immediately upon receiving the payment.
	HodlInvoice bool
}

// AddInvoice attempts to add a new invoice to the invoice database. Any
// duplicated invoices are rejected, therefore all invoices *must* have a
// unique payment preimage.
func AddInvoice(ctx context.Context, cfg *AddInvoiceConfig,
	invoice *AddInvoiceData) (*lntypes.Hash, *channeldb.Invoice, error) {

	var (
		paymentPreimage *lntypes.Preimage
		paymentHash     lntypes.Hash
	)

	switch {

	// Only either preimage or hash can be set.
	case invoice.Preimage != nil && invoice.Hash != nil:
		return nil, nil,
			errors.New("preimage and hash both set")

	// If no hash or preimage is given, generate a random preimage.
	case invoice.Preimage == nil && invoice.Hash == nil:
		paymentPreimage = &lntypes.Preimage{}
		if _, err := rand.Read(paymentPreimage[:]); err != nil {
			return nil, nil, err
		}
		paymentHash = paymentPreimage.Hash()

	// If just a hash is given, we create a hold invoice by setting the
	// preimage to unknown.
	case invoice.Preimage == nil && invoice.Hash != nil:
		paymentHash = *invoice.Hash

	// A specific preimage was supplied. Use that for the invoice.
	case invoice.Preimage != nil && invoice.Hash == nil:
		preimage := *invoice.Preimage
		paymentPreimage = &preimage
		paymentHash = invoice.Preimage.Hash()
	}

	// The size of the memo, receipt and description hash attached must not
	// exceed the maximum values for either of the fields.
	if len(invoice.Memo) > channeldb.MaxMemoSize {
		return nil, nil, fmt.Errorf("memo too large: %v bytes "+
			"(maxsize=%v)", len(invoice.Memo), channeldb.MaxMemoSize)
	}
	if len(invoice.DescriptionHash) > 0 && len(invoice.DescriptionHash) != 32 {
		return nil, nil, fmt.Errorf("description hash is %v bytes, must be 32",
			len(invoice.DescriptionHash))
	}

	// We set the max invoice amount to 100k BTC, which itself is several
	// multiples off the current block reward.
	maxInvoiceAmt := btcutil.Amount(btcutil.SatoshiPerBitcoin * 100000)

	switch {
	// The value of the invoice must not be negative.
	case int64(invoice.Value) < 0:
		return nil, nil, fmt.Errorf("payments of negative value "+
			"are not allowed, value is %v", int64(invoice.Value))

	// Also ensure that the invoice is actually realistic, while preventing
	// any issues due to underflow.
	case invoice.Value.ToSatoshis() > maxInvoiceAmt:
		return nil, nil, fmt.Errorf("invoice amount %v is "+
			"too large, max is %v", invoice.Value.ToSatoshis(),
			maxInvoiceAmt)
	}

	amtMSat := invoice.Value

	// We also create an encoded payment request which allows the
	// caller to compactly send the invoice to the payer. We'll create a
	// list of options to be added to the encoded payment request. For now
	// we only support the required fields description/description_hash,
	// expiry, fallback address, and the amount field.
	var options []func(*zpay32.Invoice)

	// We only include the amount in the invoice if it is greater than 0.
	// By not including the amount, we enable the creation of invoices that
	// allow the payee to specify the amount of satoshis they wish to send.
	if amtMSat > 0 {
		options = append(options, zpay32.Amount(amtMSat))
	}

	// If specified, add a fallback address to the payment request.
	if len(invoice.FallbackAddr) > 0 {
		addr, err := btcutil.DecodeAddress(invoice.FallbackAddr,
			cfg.ChainParams)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid fallback address: %v",
				err)
		}
		options = append(options, zpay32.FallbackAddr(addr))
	}

	// If expiry is set, specify it. If it is not provided, no expiry time
	// will be explicitly added to this payment request, which will imply
	// the default 3600 seconds.
	if invoice.Expiry > 0 {

		// We'll ensure that the specified expiry is restricted to sane
		// number of seconds. As a result, we'll reject an invoice with
		// an expiry greater than 1 year.
		maxExpiry := time.Hour * 24 * 365
		expSeconds := invoice.Expiry

		if float64(expSeconds) > maxExpiry.Seconds() {
			return nil, nil, fmt.Errorf("expiry of %v seconds "+
				"greater than max expiry of %v seconds",
				float64(expSeconds), maxExpiry.Seconds())
		}

		expiry := time.Duration(invoice.Expiry) * time.Second
		options = append(options, zpay32.Expiry(expiry))
	}

	// If the description hash is set, then we add it do the list of options.
	// If not, use the memo field as the payment request description.
	if len(invoice.DescriptionHash) > 0 {
		var descHash [32]byte
		copy(descHash[:], invoice.DescriptionHash[:])
		options = append(options, zpay32.DescriptionHash(descHash))
	} else {
		// Use the memo field as the description. If this is not set
		// this will just be an empty string.
		options = append(options, zpay32.Description(invoice.Memo))
	}

	// We'll use our current default CLTV value unless one was specified as
	// an option on the command line when creating an invoice.
	switch {
	case invoice.CltvExpiry > math.MaxUint16:
		return nil, nil, fmt.Errorf("CLTV delta of %v is too large, max "+
			"accepted is: %v", invoice.CltvExpiry, math.MaxUint16)
	case invoice.CltvExpiry != 0:
		// Disallow user-chosen final CLTV deltas below the required
		// minimum.
		if invoice.CltvExpiry < routing.MinCLTVDelta {
			return nil, nil, fmt.Errorf("CLTV delta of %v must be "+
				"greater than minimum of %v",
				routing.MinCLTVDelta, invoice.CltvExpiry)
		}

		options = append(options,
			zpay32.CLTVExpiry(invoice.CltvExpiry))
	default:
		// TODO(roasbeef): assumes set delta between versions
		defaultDelta := cfg.DefaultCLTVExpiry
		options = append(options, zpay32.CLTVExpiry(uint64(defaultDelta)))
	}

	// If we were requested to include routing hints in the invoice, then
	// we'll fetch all of our available private channels and create routing
	// hints for them.
	if invoice.Private {
		openChannels, err := cfg.ChanDB.FetchAllChannels()
		if err != nil {
			return nil, nil, fmt.Errorf("could not fetch all channels")
		}

		if len(openChannels) > 0 {
			// We'll restrict the number of individual route hints
			// to 20 to avoid creating overly large invoices.
			const numMaxHophints = 20
			hopHints := selectHopHints(
				amtMSat, cfg, openChannels, numMaxHophints,
			)

			options = append(options, hopHints...)
		}
	}

	// Set our desired invoice features and add them to our list of options.
	invoiceFeatures := cfg.GenInvoiceFeatures()
	options = append(options, zpay32.Features(invoiceFeatures))

	// Generate and set a random payment address for this invoice. If the
	// sender understands payment addresses, this can be used to avoid
	// intermediaries probing the receiver.
	var paymentAddr [32]byte
	if _, err := rand.Read(paymentAddr[:]); err != nil {
		return nil, nil, err
	}
	options = append(options, zpay32.PaymentAddr(paymentAddr))

	// Create and encode the payment request as a bech32 (zpay32) string.
	creationDate := time.Now()
	payReq, err := zpay32.NewInvoice(
		cfg.ChainParams, paymentHash, creationDate, options...,
	)
	if err != nil {
		return nil, nil, err
	}

	payReqString, err := payReq.Encode(
		zpay32.MessageSigner{
			SignCompact: cfg.NodeSigner.SignDigestCompact,
		},
	)
	if err != nil {
		return nil, nil, err
	}

	newInvoice := &channeldb.Invoice{
		CreationDate:   creationDate,
		Memo:           []byte(invoice.Memo),
		PaymentRequest: []byte(payReqString),
		Terms: channeldb.ContractTerm{
			FinalCltvDelta:  int32(payReq.MinFinalCLTVExpiry()),
			Expiry:          payReq.Expiry(),
			Value:           amtMSat,
			PaymentPreimage: paymentPreimage,
			PaymentAddr:     paymentAddr,
			Features:        invoiceFeatures,
		},
		HodlInvoice: invoice.HodlInvoice,
	}

	log.Tracef("[addinvoice] adding new invoice %v",
		newLogClosure(func() string {
			return spew.Sdump(newInvoice)
		}),
	)

	// With all sanity checks passed, write the invoice to the database.
	_, err = cfg.AddInvoice(newInvoice, paymentHash)
	if err != nil {
		return nil, nil, err
	}

	return &paymentHash, newInvoice, nil
}

// chanCanBeHopHint returns true if the target channel is eligible to be a hop
// hint.
func chanCanBeHopHint(channel *channeldb.OpenChannel,
	graph *channeldb.ChannelGraph,
	cfg *AddInvoiceConfig) (*channeldb.ChannelEdgePolicy, bool) {

	// Since we're only interested in our private channels, we'll skip
	// public ones.
	isPublic := channel.ChannelFlags&lnwire.FFAnnounceChannel != 0
	if isPublic {
		return nil, false
	}

	// Make sure the channel is active.
	chanPoint := lnwire.NewChanIDFromOutPoint(
		&channel.FundingOutpoint,
	)
	if !cfg.IsChannelActive(chanPoint) {
		log.Debugf("Skipping channel %v due to not "+
			"being eligible to forward payments",
			chanPoint)
		return nil, false
	}

	// To ensure we don't leak unadvertised nodes, we'll make sure our
	// counterparty is publicly advertised within the network.  Otherwise,
	// we'll end up leaking information about nodes that intend to stay
	// unadvertised, like in the case of a node only having private
	// channels.
	var remotePub [33]byte
	copy(remotePub[:], channel.IdentityPub.SerializeCompressed())
	isRemoteNodePublic, err := graph.IsPublicNode(remotePub)
	if err != nil {
		log.Errorf("Unable to determine if node %x "+
			"is advertised: %v", remotePub, err)
		return nil, false
	}

	if !isRemoteNodePublic {
		log.Debugf("Skipping channel %v due to "+
			"counterparty %x being unadvertised",
			chanPoint, remotePub)
		return nil, false
	}

	// Fetch the policies for each end of the channel.
	chanID := channel.ShortChanID().ToUint64()
	info, p1, p2, err := graph.FetchChannelEdgesByID(chanID)
	if err != nil {
		log.Errorf("Unable to fetch the routing "+
			"policies for the edges of the channel "+
			"%v: %v", chanPoint, err)
		return nil, false
	}

	// Now, we'll need to determine which is the correct policy for HTLCs
	// being sent from the remote node.
	var remotePolicy *channeldb.ChannelEdgePolicy
	if bytes.Equal(remotePub[:], info.NodeKey1Bytes[:]) {
		remotePolicy = p1
	} else {
		remotePolicy = p2
	}

	return remotePolicy, true
}

// addHopHint creates a hop hint out of the passed channel and channel policy.
// The new hop hint is appended to the passed slice.
func addHopHint(hopHints *[]func(*zpay32.Invoice),
	channel *channeldb.OpenChannel, chanPolicy *channeldb.ChannelEdgePolicy) {

	hopHint := zpay32.HopHint{
		NodeID:      channel.IdentityPub,
		ChannelID:   channel.ShortChanID().ToUint64(),
		FeeBaseMSat: uint32(chanPolicy.FeeBaseMSat),
		FeeProportionalMillionths: uint32(
			chanPolicy.FeeProportionalMillionths,
		),
		CLTVExpiryDelta: chanPolicy.TimeLockDelta,
	}
	*hopHints = append(
		*hopHints, zpay32.RouteHint([]zpay32.HopHint{hopHint}),
	)
}

// selectHopHints will select up to numMaxHophints from the set of passed open
// channels. The set of hop hints will be returned as a slice of functional
// options that'll append the route hint to the set of all route hints.
//
// TODO(roasbeef): do proper sub-set sum max hints usually << numChans
func selectHopHints(amtMSat lnwire.MilliSatoshi, cfg *AddInvoiceConfig,
	openChannels []*channeldb.OpenChannel,
	numMaxHophints int) []func(*zpay32.Invoice) {

	graph := cfg.ChanDB.ChannelGraph()

	// We'll add our hop hints in two passes, first we'll add all channels
	// that are eligible to be hop hints, and also have a local balance
	// above the payment amount.
	var totalHintBandwidth lnwire.MilliSatoshi
	hopHintChans := make(map[wire.OutPoint]struct{})
	hopHints := make([]func(*zpay32.Invoice), 0, numMaxHophints)
	for _, channel := range openChannels {
		// If this channel can't be a hop hint, then skip it.
		edgePolicy, canBeHopHint := chanCanBeHopHint(
			channel, graph, cfg,
		)
		if edgePolicy == nil || !canBeHopHint {
			continue
		}

		// Similarly, in this first pass, we'll ignore all channels in
		// isolation can't satisfy this payment.
		if channel.LocalCommitment.RemoteBalance < amtMSat {
			continue
		}

		// Now that we now this channel use usable, add it as a hop
		// hint and the indexes we'll use later.
		addHopHint(&hopHints, channel, edgePolicy)

		hopHintChans[channel.FundingOutpoint] = struct{}{}
		totalHintBandwidth += channel.LocalCommitment.RemoteBalance
	}

	// If we have enough hop hints at this point, then we'll exit early.
	// Otherwise, we'll continue to add more that may help out mpp users.
	if len(hopHints) >= numMaxHophints {
		return hopHints
	}

	// In this second pass we'll add channels, and we'll either stop when
	// we have 20 hop hints, we've run through all the available channels,
	// or if the sum of available bandwidth in the routing hints exceeds 2x
	// the payment amount. We do 2x here to account for a margin of error
	// if some of the selected channels no longer become operable.
	hopHintFactor := lnwire.MilliSatoshi(2)
	for i := 0; i < len(openChannels); i++ {
		// If we hit either of our early termination conditions, then
		// we'll break the loop here.
		if totalHintBandwidth > amtMSat*hopHintFactor ||
			len(hopHints) >= numMaxHophints {

			break
		}

		channel := openChannels[i]

		// Skip the channel if we already selected it.
		if _, ok := hopHintChans[channel.FundingOutpoint]; ok {
			continue
		}

		// If the channel can't be a hop hint, then we'll skip it.
		// Otherwise, we'll use the policy information to populate the
		// hop hint.
		remotePolicy, canBeHopHint := chanCanBeHopHint(
			channel, graph, cfg,
		)
		if !canBeHopHint || remotePolicy == nil {
			continue
		}

		// Include the route hint in our set of options that will be
		// used when creating the invoice.
		addHopHint(&hopHints, channel, remotePolicy)

		// As we've just added a new hop hint, we'll accumulate it's
		// available balance now to update our tally.
		//
		// TODO(roasbeef): have a cut off based on min bandwidth?
		totalHintBandwidth += channel.LocalCommitment.RemoteBalance
	}

	return hopHints
}
