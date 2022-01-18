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
	conn, err := getClientConn(p.gwHost, p.gwMacPath, p.gwTlsCertPath)
	if err != nil {
		return err
	}
	p.conn = conn

	infoResp, err := lnrpc.NewLightningClient(conn).GetInfo(context.Background(), &lnrpc.GetInfoRequest{})
	if err != nil {
		return err
	}

	srvrLog.Infof("Invoice coordinator started for gateway %v (%v)",
		p.gwPubKey, infoResp.Alias)

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

	notifierClient := chainrpc.NewChainNotifierClient(p.conn)
	blockStream, err := notifierClient.RegisterBlockEpochNtfn(
		ctx, &chainrpc.BlockEpoch{},
	)
	if err != nil {
		return err
	}

	routerClient := routerrpc.NewRouterClient(p.conn)
	stream, err := routerClient.HtlcInterceptor(context.Background())
	if err != nil {
		return err
	}

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

	height := <-blockChan

	hodlChan := make(chan interface{})

	for {
		select {
		case err := <-errChan:
			return err

		case height = <-blockChan:

		case htlc := <-htlcChan:
			circuitKey := channeldb.CircuitKey{
				ChanID: lnwire.NewShortChanIDFromInt(
					htlc.IncomingCircuitKey.ChanId,
				),
				HtlcID: htlc.IncomingCircuitKey.HtlcId,
			}

			fail := func() error {
				srvrLog.Debugf("Failing htlc %v", circuitKey)

				return stream.Send(&routerrpc.ForwardHtlcInterceptResponse{
					Action:             routerrpc.ResolveHoldForwardAction_FAIL,
					IncomingCircuitKey: htlc.IncomingCircuitKey,
				})
			}

			hash, err := lntypes.MakeHash(htlc.PaymentHash)
			if err != nil {
				return err
			}

			// Try decode final hop onion.
			onionReader := bytes.NewReader(htlc.OnionBlob)
			iterator, failCode := p.sphinx.DecodeHopIterator(
				onionReader, htlc.PaymentHash,
				htlc.IncomingExpiry,
			)
			if failCode != lnwire.CodeNone {
				if err := fail(); err != nil {
					return err
				}
			}

			payload, err := iterator.HopPayload()
			if err != nil {
				return err
			}

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
			// Settle htlcs that returned a settle resolution using the preimage
			// in the resolution.
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

			// For htlc failures, we get the relevant failure message based
			// on the failure resolution and then fail the htlc.
			case *invoices.HtlcFailResolution:
				if err := fail(); err != nil {
					return err
				}

			// Fail if we do not get a settle of fail resolution, since we
			// are only expecting to handle settles and fails.
			default:
				return fmt.Errorf("unknown htlc resolution type: %T",
					resolution)
			}

		case <-p.quit:
			return nil

		}
	}
}
