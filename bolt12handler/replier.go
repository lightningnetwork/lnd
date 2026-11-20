package bolt12handler

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// OnionMessageSender sends an onion message to a peer.
type OnionMessageSender interface {
	// SendOnionMessage sends the onion to the peer with the path key that
	// the peer needs to decrypt its layer.
	SendOnionMessage(ctx context.Context, peerPub [33]byte,
		pathKey *btcec.PublicKey, onion []byte) error
}

// OnionReplier sends an onion message containing an invoice back to the payer
// via the provided reply path.
type OnionReplier interface {
	// SendInvoiceReply sends the encoded invoice bytes as a type-66 TLV
	// payload via the reply path.
	SendInvoiceReply(ctx context.Context, invoiceBytes []byte,
		replyPath *sphinx.BlindedPath) error
}

// A compile-time check that ServerOnionReplier implements OnionReplier.
var _ OnionReplier = (*ServerOnionReplier)(nil)

// ServerOnionReplier implements OnionReplier by constructing an onion message
// from the reply path and sending it via the daemon's SendOnionMessage method.
// The reply path's introduction node must be a direct peer for now.
type ServerOnionReplier struct {
	sender OnionMessageSender
}

// NewServerOnionReplier creates a new OnionReplier backed by the daemon's onion
// message sender.
func NewServerOnionReplier(
	sender OnionMessageSender) *ServerOnionReplier {

	return &ServerOnionReplier{
		sender: sender,
	}
}

// SendInvoiceReply sends the encoded invoice bytes as a type-66 TLV payload via
// the reply path.
//
// NOTE: This is part of the OnionReplier interface.
func (r *ServerOnionReplier) SendInvoiceReply(ctx context.Context,
	invoiceBytes []byte, replyPath *sphinx.BlindedPath) error {

	if replyPath == nil {
		return fmt.Errorf("no reply path provided")
	}

	if len(replyPath.BlindedHops) == 0 {
		return fmt.Errorf("reply path has no hops")
	}

	if replyPath.IntroductionPoint == nil {
		return fmt.Errorf("reply path has no introduction node key")
	}

	finalHopTLVs := []*lnwire.FinalHopTLV{
		{
			TLVType: lnwire.InvoiceNamespaceType,
			Value:   invoiceBytes,
		},
	}

	// TODO: Find a route to an introduction node that is not a peer, and
	// send the reply through the nodes on that route. Until then, a payer
	// whose reply path starts at a non-peer gets no invoice.
	return r.sendDirect(ctx, replyPath, finalHopTLVs)
}

// sendDirect sends the reply onion directly to the reply path's intro node.
func (r *ServerOnionReplier) sendDirect(ctx context.Context,
	replyPath *sphinx.BlindedPath,
	finalHopTLVs []*lnwire.FinalHopTLV) error {

	sphinxPath, err := route.OnionMessageBlindedPathToSphinxPath(
		replyPath, nil, finalHopTLVs,
	)
	if err != nil {
		return fmt.Errorf("build sphinx path: %w", err)
	}

	sessionKey, err := btcec.NewPrivateKey()
	if err != nil {
		return fmt.Errorf("generate session key: %w", err)
	}

	onionPkt, err := sphinx.NewOnionPacket(
		sphinxPath, sessionKey, nil,
		sphinx.DeterministicPacketFiller,
		sphinx.WithMaxPayloadSize(sphinx.MaxOnionMessagePayloadSize),
	)
	if err != nil {
		return fmt.Errorf("build onion packet: %w", err)
	}

	var buf bytes.Buffer
	if err := onionPkt.Encode(&buf); err != nil {
		return fmt.Errorf("encode onion packet: %w", err)
	}

	var peerPub [33]byte
	copy(peerPub[:], replyPath.IntroductionPoint.SerializeCompressed())

	return r.sender.SendOnionMessage(
		ctx, peerPub, replyPath.BlindingPoint, buf.Bytes(),
	)
}
