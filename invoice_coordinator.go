package lnd

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"google.golang.org/grpc"
)

type InvoiceCoordinator struct {
	registry *invoices.InvoiceRegistry
	sphinx   *hop.OnionProcessor

	gwPubKey                         route.Vertex
	gwHost, gwTlsCertPath, gwMacPath string

	conn *grpc.ClientConn

	wg   sync.WaitGroup
	quit chan struct{}
}

type InvoiceCoordinatorConfig struct {
	registry *invoices.InvoiceRegistry
	sphinx   *hop.OnionProcessor
	gwPubKey route.Vertex

	gwHost, gwTlsCertPath, gwMacPath string
}

func NewInvoiceCoordinator(cfg *InvoiceCoordinatorConfig) *InvoiceCoordinator {
	return &InvoiceCoordinator{
		registry: cfg.registry,
		sphinx:   cfg.sphinx,

		gwPubKey:      cfg.gwPubKey,
		gwHost:        cfg.gwHost,
		gwTlsCertPath: cfg.gwTlsCertPath,
		gwMacPath:     cfg.gwMacPath,

		quit: make(chan struct{}),
	}
}

func (p *InvoiceCoordinator) Start() error {
	// Connect to gateway node.
	conn, err := getClientConn(p.gwHost, p.gwMacPath, p.gwTlsCertPath)
	if err != nil {
		return err
	}
	p.conn = conn

	// Test connection.
	infoResp, err := lnrpc.NewLightningClient(conn).GetInfo(context.Background(), &lnrpc.GetInfoRequest{})
	if err != nil {
		return err
	}

	srvrLog.Infof("Invoice coordinator started for gateway %v (%v)",
		p.gwPubKey, infoResp.Alias)

	// Start coordinator main loop.
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()

		err := p.run()
		if err != nil {
			srvrLog.Errorf("Invoice coordinator error: %v", err)
		}
	}()

	return nil
}

func (p *InvoiceCoordinator) Stop() error {
	close(p.quit)
	p.wg.Wait()

	return nil
}

func (p *InvoiceCoordinator) run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Register for block notifications.
	notifierClient := chainrpc.NewChainNotifierClient(p.conn)
	blockStream, err := notifierClient.RegisterBlockEpochNtfn(
		ctx, &chainrpc.BlockEpoch{},
	)
	if err != nil {
		return err
	}

	// Register for htlc interception events.
	routerClient := routerrpc.NewRouterClient(p.conn)
	stream, err := routerClient.HtlcInterceptor(context.Background())
	if err != nil {
		return err
	}

	// Start goroutines convert grpc streams to channels.
	var (
		errChan   = make(chan error)
		htlcChan  = make(chan *routerrpc.ForwardHtlcInterceptRequest)
		blockChan = make(chan uint32)
	)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			event, err := stream.Recv()
			if err != nil {
				errChan <- err
			}

			htlcChan <- event
		}
	}()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			event, err := blockStream.Recv()
			if err != nil {
				errChan <- err
			}

			blockChan <- event.Height
		}
	}()

	// The block stream immediately sends the current block. Read that to
	// set our initial height.
	height := <-blockChan

	for {
		select {
		case err := <-errChan:
			return err

		case height = <-blockChan:

		case htlc := <-htlcChan:
			err := p.ProcessHtlc(htlc, height, stream)
			if err != nil {
				return err
			}

		case <-p.quit:
			return nil
		}
	}
}

func marshallMalformedHtlcFailure(code lnwire.FailCode) (
	routerrpc.MalformedHtlcCode, error) {

	switch code {
	case lnwire.CodeInvalidOnionHmac:
		return routerrpc.MalformedHtlcCode_INVALID_ONION_HMAC, nil

	case lnwire.CodeInvalidOnionVersion:
		return routerrpc.MalformedHtlcCode_INVALID_ONION_VERSION, nil

	case lnwire.CodeInvalidOnionKey:
		return routerrpc.MalformedHtlcCode_INVALID_ONION_KEY, nil
	}

	return 0, fmt.Errorf("unsupported code %v", code)
}

func (p *InvoiceCoordinator) ProcessHtlc(
	htlc *routerrpc.ForwardHtlcInterceptRequest, height uint32,
	stream routerrpc.Router_HtlcInterceptorClient) error {

	circuitKey := channeldb.CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(
			htlc.IncomingCircuitKey.ChanId,
		),
		HtlcID: htlc.IncomingCircuitKey.HtlcId,
	}

	hash, err := lntypes.MakeHash(htlc.PaymentHash)
	if err != nil {
		return err
	}

	fail := func(code lnwire.FailCode) error {
		srvrLog.Debugf("Failing htlc %v: code=%v", circuitKey, code)

		rpcCode, err := marshallMalformedHtlcFailure(code)
		if err != nil {
			return err
		}

		return stream.Send(&routerrpc.ForwardHtlcInterceptResponse{
			Action:               routerrpc.ResolveHoldForwardAction_FAIL,
			IncomingCircuitKey:   htlc.IncomingCircuitKey,
			FailureMalformedHtlc: rpcCode,
		})
	}

	// Try decode final hop onion.
	onionReader := bytes.NewReader(htlc.OnionBlob)
	iterator, failCode := p.sphinx.DecodeHopIterator(
		onionReader, htlc.PaymentHash,
		htlc.IncomingExpiry,
	)
	if failCode != lnwire.CodeNone {
		return fail(failCode)
	}

	payload, err := iterator.HopPayload()
	if err != nil {
		return err
	}

	obfuscator, failCode := iterator.ExtractErrorEncrypter(
		p.sphinx.ExtractErrorEncrypter,
	)
	if failCode != lnwire.CodeNone {
		return fail(failCode)
	}

	// Hodl channel for hodl invoices. Not supported.
	hodlChan := make(chan interface{})

	// Notify the invoice registry of the intercepted htlc.
	resolution, err := p.registry.NotifyExitHopHtlc(
		hash,
		lnwire.MilliSatoshi(htlc.OutgoingAmountMsat),
		htlc.OutgoingExpiry,
		int32(height), circuitKey,
		hodlChan,
		payload,
	)
	if err != nil {
		return err
	}

	// Determine required action for the resolution based on the type of
	// resolution we have received.
	switch res := resolution.(type) {

	case *invoices.HtlcSettleResolution:
		srvrLog.Debugf("received settle resolution for %v "+
			"with outcome: %v", circuitKey, res.Outcome)

		err := stream.Send(&routerrpc.ForwardHtlcInterceptResponse{
			Action:             routerrpc.ResolveHoldForwardAction_SETTLE,
			IncomingCircuitKey: htlc.IncomingCircuitKey,
			Preimage:           res.Preimage[:],
		})
		if err != nil {
			return err
		}

	case *invoices.HtlcFailResolution:
		srvrLog.Debugf("Failing htlc %v: outcome=%v",
			circuitKey,
			res.Outcome)

		incorrectDetails := lnwire.NewFailIncorrectDetails(
			lnwire.MilliSatoshi(htlc.OutgoingAmountMsat),
			uint32(res.AcceptHeight),
		)

		reason, err := obfuscator.EncryptFirstHop(incorrectDetails)
		if err != nil {
			return err
		}

		// Here we need more control over htlc
		// interception so that we can send back an
		// encrypted failure message to the sender.
		err = stream.Send(&routerrpc.ForwardHtlcInterceptResponse{
			Action:             routerrpc.ResolveHoldForwardAction_FAIL,
			IncomingCircuitKey: htlc.IncomingCircuitKey,
			FailureReason:      reason,
		})
		if err != nil {
			return err
		}

	// Fail if we do not get a settle of fail resolution, since we
	// are only expecting to handle settles and fails.
	default:
		return fmt.Errorf("unknown htlc resolution type: %T",
			resolution)
	}

	return nil
}
