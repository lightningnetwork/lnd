package rpcwallet

import (
	"errors"

	"github.com/lightningnetwork/lnd/lncfg"
)

// RemoteSignerBuilder is creates instances of the RemoteSigner interface, based
// on the provided configuration.
type RemoteSignerBuilder struct {
	cfg *lncfg.RemoteSigner
}

// NewRemoteSignerBuilder creates a new instance of the RemoteSignerBuilder.
func NewRemoteSignerBuilder(cfg *lncfg.RemoteSigner) *RemoteSignerBuilder {
	return &RemoteSignerBuilder{cfg}
}

// Build creates a new RemoteSigner instance. If the configuration specifies
// that an inbound remote signer should be used, a new InboundRemoteSigner is
// created. If the configuration specifies that an outbound remote signer should
// be used, a new OutboundRemoteSigner is created.
// The function returns the created RemoteSigner instance, and a cleanup
// function that should be called when the RemoteSigner is no longer needed.
func (b *RemoteSignerBuilder) Build() (RemoteSigner, func(), error) {
	if b.cfg == nil {
		return nil, nil, errors.New("remote signer config is nil")
	}

	// Validate that the configuration has valid values set.
	err := b.cfg.Validate()
	if err != nil {
		return nil, nil, err
	}

	if !b.cfg.Enable {
		// This should be unreachable, but this is an extra sanity check
		return nil, nil, errors.New("remote signer not enabled in " +
			"config")
	}

	// Create the remote signer based on the configuration.
	switch b.cfg.SignerType {
	case lncfg.DefaultInboundRemoteSignerType:
		return b.createInboundRemoteSigner()

	case lncfg.OutboundRemoteSignerType:
		return b.createOutboundRemoteSigner()

	default:
		return nil, nil, errors.New("unknown remote signer type")
	}
}

// createInboundRemoteSigner creates a new InboundRemoteSigner instance.
// The function returns the created InboundRemoteSigner instance, and a cleanup
// function that should be called when the InboundRemoteSigner is no longer
// needed.
func (b *RemoteSignerBuilder) createInboundRemoteSigner() (
	*InboundRemoteSigner, func(), error) {

	return NewInboundRemoteSigner(
		b.cfg.RPCHost, b.cfg.TLSCertPath, b.cfg.MacaroonPath,
		b.cfg.Timeout,
	)
}

// createOutboundRemoteSigner creates a new OutboundRemoteSigner instance.
// The function returns the created OutboundRemoteSigner instance, and a cleanup
// function that should be called when the OutboundRemoteSigner is no longer
// needed.
func (b *RemoteSignerBuilder) createOutboundRemoteSigner() (
	*OutboundRemoteSigner, func(), error) {

	outboundRemoteSigner, cleanUp := NewOutboundRemoteSigner(
		b.cfg.RequestTimeout, b.cfg.Timeout,
	)

	return outboundRemoteSigner, cleanUp, nil
}
