package invoicesrpc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"math"
	"time"

	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/zpay32"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
)

// AddInvoiceConfig contains dependencies for invoice creation.
type AddInvoiceConfig struct {
	// InvoiceRegistry is a central registry of all the outstanding invoices
	// created by the daemon.
	InvoiceRegistry *invoices.InvoiceRegistry

	// Switch is used by the invoices subserver to control acceptance and
	// cancelation of invoices.
	Switch *htlcswitch.Switch

	// ChainParams are required to properly decode invoice payment requests
	// that are marshalled over rpc.
	ChainParams *chaincfg.Params

	// NodeSigner is an implementation of the MessageSigner implementation
	// that's backed by the identity private key of the running lnd node.
	NodeSigner *netann.NodeSigner

	// MaxPaymentMSat is the maximum allowed payment.
	MaxPaymentMSat lnwire.MilliSatoshi

	// DefaultCLTVExpiry is the default invoice expiry if no values is
	// specified.
	DefaultCLTVExpiry uint32

	// ChanDB is a global boltdb instance which is needed to access the
	// channel graph.
	ChanDB *channeldb.DB
}

// AddInvoice attempts to add a new invoice to the invoice database. Any
// duplicated invoices are rejected, therefore all invoices *must* have a
// unique payment preimage.
func AddInvoice(ctx context.Context, cfg *AddInvoiceConfig,
	invoice *lnrpc.Invoice) (*lnrpc.AddInvoiceResponse, error) {

	var paymentPreimage [32]byte

	switch {
	// If a preimage wasn't specified, then we'll generate a new preimage
	// from fresh cryptographic randomness.
	case len(invoice.RPreimage) == 0:
		if _, err := rand.Read(paymentPreimage[:]); err != nil {
			return nil, err
		}

	// Otherwise, if a preimage was specified, then it MUST be exactly
	// 32-bytes.
	case len(invoice.RPreimage) > 0 && len(invoice.RPreimage) != 32:
		return nil, fmt.Errorf("payment preimage must be exactly "+
			"32 bytes, is instead %v", len(invoice.RPreimage))

	// If the preimage meets the size specifications, then it can be used
	// as is.
	default:
		copy(paymentPreimage[:], invoice.RPreimage[:])
	}

	// The size of the memo, receipt and description hash attached must not
	// exceed the maximum values for either of the fields.
	if len(invoice.Memo) > channeldb.MaxMemoSize {
		return nil, fmt.Errorf("memo too large: %v bytes "+
			"(maxsize=%v)", len(invoice.Memo), channeldb.MaxMemoSize)
	}
	if len(invoice.Receipt) > channeldb.MaxReceiptSize {
		return nil, fmt.Errorf("receipt too large: %v bytes "+
			"(maxsize=%v)", len(invoice.Receipt), channeldb.MaxReceiptSize)
	}
	if len(invoice.DescriptionHash) > 0 && len(invoice.DescriptionHash) != 32 {
		return nil, fmt.Errorf("description hash is %v bytes, must be %v",
			len(invoice.DescriptionHash), channeldb.MaxPaymentRequestSize)
	}

	// The value of the invoice must not be negative.
	if invoice.Value < 0 {
		return nil, fmt.Errorf("payments of negative value "+
			"are not allowed, value is %v", invoice.Value)
	}

	amt := btcutil.Amount(invoice.Value)
	amtMSat := lnwire.NewMSatFromSatoshis(amt)

	// The value of the invoice must also not exceed the current soft-limit
	// on the largest payment within the network.
	if amtMSat > cfg.MaxPaymentMSat {
		return nil, fmt.Errorf("payment of %v is too large, max "+
			"payment allowed is %v", amt,
			cfg.MaxPaymentMSat.ToSatoshis(),
		)
	}

	// Next, generate the payment hash itself from the preimage. This will
	// be used by clients to query for the state of a particular invoice.
	rHash := sha256.Sum256(paymentPreimage[:])

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
			return nil, fmt.Errorf("invalid fallback address: %v",
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
			return nil, fmt.Errorf("expiry of %v seconds "+
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
		return nil, fmt.Errorf("CLTV delta of %v is too large, max "+
			"accepted is: %v", invoice.CltvExpiry, math.MaxUint16)
	case invoice.CltvExpiry != 0:
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
			return nil, fmt.Errorf("could not fetch all channels")
		}

		graph := cfg.ChanDB.ChannelGraph()

		numHints := 0
		for _, channel := range openChannels {
			// We'll restrict the number of individual route hints
			// to 20 to avoid creating overly large invoices.
			if numHints > 20 {
				break
			}

			// Since we're only interested in our private channels,
			// we'll skip public ones.
			isPublic := channel.ChannelFlags&lnwire.FFAnnounceChannel != 0
			if isPublic {
				continue
			}

			// Make sure the counterparty has enough balance in the
			// channel for our amount. We do this in order to reduce
			// payment errors when attempting to use this channel
			// as a hint.
			chanPoint := lnwire.NewChanIDFromOutPoint(
				&channel.FundingOutpoint,
			)
			if amtMSat >= channel.LocalCommitment.RemoteBalance {
				log.Debugf("Skipping channel %v due to "+
					"not having enough remote balance",
					chanPoint)
				continue
			}

			// Make sure the channel is active.
			link, err := cfg.Switch.GetLink(chanPoint)
			if err != nil {
				log.Errorf("Unable to get link for "+
					"channel %v: %v", chanPoint, err)
				continue
			}

			if !link.EligibleToForward() {
				log.Debugf("Skipping channel %v due to not "+
					"being eligible to forward payments",
					chanPoint)
				continue
			}

			// To ensure we don't leak unadvertised nodes, we'll
			// make sure our counterparty is publicly advertised
			// within the network. Otherwise, we'll end up leaking
			// information about nodes that intend to stay
			// unadvertised, like in the case of a node only having
			// private channels.
			var remotePub [33]byte
			copy(remotePub[:], channel.IdentityPub.SerializeCompressed())
			isRemoteNodePublic, err := graph.IsPublicNode(remotePub)
			if err != nil {
				log.Errorf("Unable to determine if node %x "+
					"is advertised: %v", remotePub, err)
				continue
			}

			if !isRemoteNodePublic {
				log.Debugf("Skipping channel %v due to "+
					"counterparty %x being unadvertised",
					chanPoint, remotePub)
				continue
			}

			// Fetch the policies for each end of the channel.
			chanID := channel.ShortChanID().ToUint64()
			info, p1, p2, err := graph.FetchChannelEdgesByID(chanID)
			if err != nil {
				log.Errorf("Unable to fetch the routing "+
					"policies for the edges of the channel "+
					"%v: %v", chanPoint, err)
				continue
			}

			// Now, we'll need to determine which is the correct
			// policy for HTLCs being sent from the remote node.
			var remotePolicy *channeldb.ChannelEdgePolicy
			if bytes.Equal(remotePub[:], info.NodeKey1Bytes[:]) {
				remotePolicy = p1
			} else {
				remotePolicy = p2
			}

			// If for some reason we don't yet have the edge for
			// the remote party, then we'll just skip adding this
			// channel as a routing hint.
			if remotePolicy == nil {
				continue
			}

			// Finally, create the routing hint for this channel and
			// add it to our list of route hints.
			hint := zpay32.HopHint{
				NodeID:      channel.IdentityPub,
				ChannelID:   chanID,
				FeeBaseMSat: uint32(remotePolicy.FeeBaseMSat),
				FeeProportionalMillionths: uint32(
					remotePolicy.FeeProportionalMillionths,
				),
				CLTVExpiryDelta: remotePolicy.TimeLockDelta,
			}

			// Include the route hint in our set of options that
			// will be used when creating the invoice.
			routeHint := []zpay32.HopHint{hint}
			options = append(options, zpay32.RouteHint(routeHint))

			numHints++
		}

	}

	// Create and encode the payment request as a bech32 (zpay32) string.
	creationDate := time.Now()
	payReq, err := zpay32.NewInvoice(
		cfg.ChainParams, rHash, creationDate, options...,
	)
	if err != nil {
		return nil, err
	}

	payReqString, err := payReq.Encode(
		zpay32.MessageSigner{
			SignCompact: cfg.NodeSigner.SignDigestCompact,
		},
	)
	if err != nil {
		return nil, err
	}

	newInvoice := &channeldb.Invoice{
		CreationDate:   creationDate,
		Memo:           []byte(invoice.Memo),
		Receipt:        invoice.Receipt,
		PaymentRequest: []byte(payReqString),
		Terms: channeldb.ContractTerm{
			Value:           amtMSat,
			PaymentPreimage: paymentPreimage,
		},
	}

	log.Tracef("[addinvoice] adding new invoice %v",
		newLogClosure(func() string {
			return spew.Sdump(newInvoice)
		}),
	)

	// With all sanity checks passed, write the invoice to the database.
	addIndex, err := cfg.InvoiceRegistry.AddInvoice(newInvoice, rHash)
	if err != nil {
		return nil, err
	}

	return &lnrpc.AddInvoiceResponse{
		RHash:          rHash[:],
		PaymentRequest: payReqString,
		AddIndex:       addIndex,
	}, nil
}
