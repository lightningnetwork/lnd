package routing

import (
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/tlv"
)

// A compile time assertion to ensure IntervalSessionSource meets the
// PaymentSessionSource interface.
var _ PaymentSessionSource = (*IntervalSessionSource)(nil)

// IntervalSessionSource hands out interval router sessions. It wraps the stock
// session source rather than replacing it, both because the empty session it
// produces is a pure bookkeeping device with no routing in it, and because the
// interval router does not cover every payment shape yet and needs somewhere to
// fall back to.
type IntervalSessionSource struct {
	// SessionSource is the stock source, used for the payment shapes the
	// interval router does not handle.
	*SessionSource

	// Store is the node wide liquidity belief the sessions read and write.
	// It outlives every payment, which is what makes what one payment
	// learns available to the next.
	Store *IntervalStore

	// Config holds the search bounds of the interval router.
	Config IntervalConfig
}

// NewIntervalSessionSource builds a source that produces interval router
// sessions, backed by the given stock source for the payments it does not
// handle.
func NewIntervalSessionSource(stock *SessionSource, store *IntervalStore,
	cfg IntervalConfig) *IntervalSessionSource {

	cfg.fillDefaults()

	return &IntervalSessionSource{
		SessionSource: stock,
		Store:         store,
		Config:        cfg,
	}
}

// NewPaymentSession creates a session for the given payment. Payments the
// interval router does not handle are served by the stock session instead, so
// that turning the router on never makes a payment unroutable that would
// otherwise have gone through.
//
// NOTE: Part of the PaymentSessionSource interface.
func (m *IntervalSessionSource) NewPaymentSession(p *LightningPayment,
	firstHopBlob fn.Option[tlv.Blob],
	trafficShaper fn.Option[htlcswitch.AuxTrafficShaper]) (PaymentSession,
	error) {

	if reason := unsupportedByInterval(p); reason != "" {
		log.Debugf("Payment %x falling back to the default router: %v",
			p.Identifier(), reason)

		return m.SessionSource.NewPaymentSession(
			p, firstHopBlob, trafficShaper,
		)
	}

	getBandwidthHints := func(graph Graph) (bandwidthHints, error) {
		return newBandwidthManager(
			graph, m.SourceNode.PubKeyBytes, m.GetLink,
			firstHopBlob, trafficShaper,
		)
	}

	return newIntervalPaymentSession(
		p, m.SourceNode.PubKeyBytes, getBandwidthHints,
		m.GraphSessionFactory, m.Store, m.Config,
	)
}

// unsupportedByInterval returns the reason the interval router cannot serve a
// payment, or the empty string when it can.
func unsupportedByInterval(p *LightningPayment) string {
	// Blinded paths are served by the stock session. The interval model
	// keys its beliefs on a directed channel, and inside a blinded path
	// there is no channel to key on: the hops are opaque and the amounts
	// and expiries of the intermediate ones are deliberately zero. Routing
	// to the introduction node with intervals and through the path without
	// them is a coherent design, but it is not this one.
	if p.BlindedPathSet != nil {
		return "payment is to a blinded path"
	}

	return ""
}
