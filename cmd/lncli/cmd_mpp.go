package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc"

	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/urfave/cli"
)

var mppCommand = cli.Command{
	Name:     "mpp",
	Category: "Payments",
	Action:   mpp,
}

func mpp(ctx *cli.Context) error {
	// Show command help if no arguments provided.
	if ctx.NArg() == 0 {
		_ = cli.ShowCommandHelp(ctx, "mpp")
		return nil
	}

	executor, err := newRouter(ctx)
	if err != nil {
		return err
	}
	defer executor.closeConnection()

	err = executor.launch()
	if err != nil {
		return err
	}

	return nil
}

type router struct {
	conn         *grpc.ClientConn
	routerClient routerrpc.RouterClient
	amt          int64
	session      *paymentSession
}

func newRouter(ctx *cli.Context) (*router, error) {
	conn := getClientConn(ctx, false)

	mainClient := lnrpc.NewLightningClient(conn)
	routerClient := routerrpc.NewRouterClient(conn)

	payReqText := ctx.Args().First()
	payReq, err := mainClient.DecodePayReq(context.Background(), &lnrpc.PayReqString{
		PayReq: payReqText,
	})
	if err != nil {
		return nil, err
	}

	invoiceHash, err := lntypes.MakeHashFromStr(payReq.PaymentHash)
	if err != nil {
		return nil, err
	}

	session := &paymentSession{
		invoiceHash: invoiceHash,
		dest:        payReq.Destination,
		mainClient:  mainClient,
		totalAmt:    payReq.NumSatoshis,
		paymentAddr: payReq.PaymentAddr,
		maxAmt:      math.MaxInt64,
	}

	return &router{
		routerClient: routerClient,
		amt:          payReq.NumSatoshis,
		conn:         conn,
		session:      session,
	}, nil
}

func (m *router) closeConnection() {
	m.conn.Close()
}

type launchResult struct {
	id      int
	failure *routerrpc.Failure
	amt     int64
	err     error
}

const (
	minAmt = 5000
)

func (m *router) launch() error {
	resultChan := make(chan launchResult, 0)

	var (
		amtPaid, amtInFlight int64
		nextId               int
		inFlight             = make(map[int]int64, 0)
	)

	inFlightStr := func() string {
		var b bytes.Buffer
		first := true
		for id, amt := range inFlight {
			if first {
				first = false
			} else {
				fmt.Fprintf(&b, ", ")
			}
			fmt.Fprintf(&b, "%v:%v", id, amt)
		}
		return b.String()
	}

	for {
		remainingAmt := m.amt - amtPaid - amtInFlight

		// Kick off pathfinding for the currently remaining amt.
		var routeChan chan *htlc
		if remainingAmt > 0 {
			routeChan = m.session.prepareRoute(remainingAmt)
		}

		// Wait for a result or route to become available.
		select {
		case r := <-resultChan:
			if r.err != nil {
				return r.err
			}

			amtInFlight -= r.amt
			delete(inFlight, r.id)
			if r.failure == nil {
				fmt.Printf("%v: Partial payment success for amt=%v (%v)\n", r.id, r.amt, inFlightStr())

				amtPaid += r.amt

				if amtPaid == m.amt {
					fmt.Printf("Payment complete\n")
					return nil
				}
			} else {
				fmt.Printf("%v: Partial payment failed for amt=%v: %v @ %v (%v)\n",
					r.id, r.amt, r.failure.Code, r.failure.FailureSourceIndex, inFlightStr())

				switch r.failure.Code {
				case routerrpc.Failure_INCORRECT_OR_UNKNOWN_PAYMENT_DETAILS:
					return errors.New("Incorrect details")
				case routerrpc.Failure_MPP_TIMEOUT:
					// Just retry.
				case routerrpc.Failure_TEMPORARY_CHANNEL_FAILURE:
					// reduceMax(r.amt)
				default:
					// Just retry.
				}
			}

		case htlc := <-routeChan:
			if htlc.err != nil {
				return htlc.err
			}
			route := htlc.route
			if route == nil {
				if amtInFlight == 0 {
					return errors.New("no more routes")
				}

				// Otherwise wait for results to come in.
				break
			}

			id := nextId
			nextId++

			go func() {
				resultChan <- m.launchShard(id, htlc.hash, route)
			}()

			fmt.Printf("sleeping 3 secs to allow local chan balance to be updated\n")
			time.Sleep(3 * time.Second)

			amt := route.Hops[len(route.Hops)-1].AmtToForward

			inFlight[id] = amt
			amtInFlight += amt

			path := route.Hops
			pathText := fmt.Sprintf("%v", path[0].ChanId)
			for _, h := range path[1:] {
				pathText += fmt.Sprintf(" -> %v", h.ChanId)
			}
			fmt.Printf("%v: Launched shard for amt=%v, path=%v, timelock=%v (%v)\n",
				id, amt, pathText, route.TotalTimeLock, inFlightStr())

		}

		// Close the channel to prevent losing routes in the send to
		// route case.
		if routeChan != nil {
			close(routeChan)
		}
	}
}

func (m *router) launchShard(id int, hash lntypes.Hash, route *lnrpc.Route) launchResult {
	ctxb := context.Background()

	amt := route.Hops[len(route.Hops)-1].AmtToForward

	sendReq := &routerrpc.SendToRouteRequest{
		PaymentHash: hash[:],
		Route:       route,
	}

	resp, err := m.routerClient.SendToRoute(ctxb, sendReq)
	if err != nil {
		return launchResult{id: id, err: err}
	}

	if len(resp.Preimage) > 0 {
		return launchResult{id: id, amt: amt}
	}

	return launchResult{id: id, failure: resp.Failure, amt: amt}
}

type htlc struct {
	route *lnrpc.Route
	hash  lntypes.Hash
	err   error
}

type paymentSession struct {
	invoiceHash lntypes.Hash
	dest        string
	mainClient  lnrpc.LightningClient
	totalAmt    int64
	paymentAddr []byte
	maxAmt      int64
}

func (p *paymentSession) prepareRoute(amt int64) chan *htlc {
	c := make(chan *htlc)

	go func() {
		var route *htlc
		for {
			// Todo: lock p.maxAmt
			if amt > p.maxAmt {
				amt = p.maxAmt
			}

			// No routes below min amt.
			if amt < minAmt {
				route = &htlc{}
				break
			}

			route = p.queryRoute(amt)
			if route.err != nil {
				route = &htlc{
					err: route.err,
				}
				break
			}
			if route.route != nil {
				break
			}

			// No route found, reduce max and retry.
			newMax := amt / 2
			if newMax < p.maxAmt {
				p.maxAmt = newMax
				fmt.Printf("Lowering max amt to %v\n", p.maxAmt)
			}
		}
		c <- route
	}()

	return c
}

func (p *paymentSession) queryRoute(amt int64) *htlc {
	ctxb := context.Background()

	routeResp, err := p.mainClient.QueryRoutes(ctxb, &lnrpc.QueryRoutesRequest{
		Amt:               amt,
		FinalCltvDelta:    40,
		PubKey:            p.dest,
		UseMissionControl: true,
		DestFeatures:      []lnrpc.FeatureBit{lnrpc.FeatureBit_MPP_OPT, lnrpc.FeatureBit_TLV_ONION_OPT, lnrpc.FeatureBit_PAYMENT_ADDR_OPT},
	})
	if err != nil {
		// fmt.Printf("%v: query routes for amt %v: %v\n", id, amt, err)
		return &htlc{}
	}
	route := routeResp.Routes[0]

	finalHop := route.Hops[len(route.Hops)-1]

	finalHop.MppRecord = &lnrpc.MPPRecord{
		TotalAmtMsat: p.totalAmt * 1000,
		PaymentAddr:  p.paymentAddr,
	}

	return &htlc{
		route: route,
		hash:  p.invoiceHash,
	}
}
