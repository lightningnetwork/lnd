package main

import (
	"errors"
	"fmt"
	"net"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tor"
)

// chanController is an implementation of the autopilot.ChannelController
// interface that's backed by a running lnd instance.
type chanController struct {
	server   *server
	private  bool
	minConfs int32
}

// OpenChannel opens a channel to a target peer, with a capacity of the
// specified amount. This function should un-block immediately after the
// funding transaction that marks the channel open has been broadcast.
func (c *chanController) OpenChannel(target *btcec.PublicKey,
	amt btcutil.Amount) error {

	// With the connection established, we'll now establish our connection
	// to the target peer, waiting for the first update before we exit.
	feePerKw, err := c.server.cc.feeEstimator.EstimateFeePerKW(3)
	if err != nil {
		return err
	}

	// TODO(halseth): make configurable?
	minHtlc := lnwire.NewMSatFromSatoshis(1)

	// Construct the open channel request and send it to the server to begin
	// the funding workflow.
	req := &openChanReq{
		targetPubkey:    target,
		chainHash:       *activeNetParams.GenesisHash,
		localFundingAmt: amt,
		pushAmt:         0,
		minHtlc:         minHtlc,
		fundingFeePerKw: feePerKw,
		private:         c.private,
		remoteCsvDelay:  0,
		minConfs:        c.minConfs,
	}

	updateStream, errChan := c.server.OpenChannel(req)
	select {
	case err := <-errChan:
		return err
	case <-updateStream:
		return nil
	case <-c.server.quit:
		return nil
	}
}

func (c *chanController) CloseChannel(chanPoint *wire.OutPoint) error {
	return nil
}
func (c *chanController) SpliceIn(chanPoint *wire.OutPoint,
	amt btcutil.Amount) (*autopilot.Channel, error) {
	return nil, nil
}
func (c *chanController) SpliceOut(chanPoint *wire.OutPoint,
	amt btcutil.Amount) (*autopilot.Channel, error) {
	return nil, nil
}

// A compile time assertion to ensure chanController meets the
// autopilot.ChannelController interface.
var _ autopilot.ChannelController = (*chanController)(nil)

// initAutoPilot initializes a new autopilot.Agent instance based on the passed
// configuration struct. All interfaces needed to drive the pilot will be
// registered and launched.
func initAutoPilot(svr *server, cfg *autoPilotConfig) (*autopilot.Agent, error) {
	atplLog.Infof("Instantiating autopilot with cfg: %v", spew.Sdump(cfg))

	// First, we'll create the preferential attachment heuristic,
	// initialized with the passed auto pilot configuration parameters.
	prefAttachment := autopilot.NewConstrainedPrefAttachment(
		btcutil.Amount(cfg.MinChannelSize),
		btcutil.Amount(cfg.MaxChannelSize),
		uint16(cfg.MaxChannels), cfg.Allocation,
	)

	// With the heuristic itself created, we can now populate the remainder
	// of the items that the autopilot agent needs to perform its duties.
	self := svr.identityPriv.PubKey()
	pilotCfg := autopilot.Config{
		Self:      self,
		Heuristic: prefAttachment,
		ChanController: &chanController{
			server:   svr,
			private:  cfg.Private,
			minConfs: cfg.MinConfs,
		},
		WalletBalance: func() (btcutil.Amount, error) {
			return svr.cc.wallet.ConfirmedBalance(cfg.MinConfs)
		},
		Graph:           autopilot.ChannelGraphFromDatabase(svr.chanDB.ChannelGraph()),
		MaxPendingOpens: 10,
		ConnectToPeer: func(target *btcec.PublicKey, addrs []net.Addr) (bool, error) {
			// First, we'll check if we're already connected to the
			// target peer. If we are, we can exit early. Otherwise,
			// we'll need to establish a connection.
			if _, err := svr.FindPeer(target); err == nil {
				return true, nil
			}

			// We can't establish a channel if no addresses were
			// provided for the peer.
			if len(addrs) == 0 {
				return false, errors.New("no addresses specified")
			}

			atplLog.Tracef("Attempting to connect to %x",
				target.SerializeCompressed())

			lnAddr := &lnwire.NetAddress{
				IdentityKey: target,
				ChainNet:    activeNetParams.Net,
			}

			// We'll attempt to successively connect to each of the
			// advertised IP addresses until we've either exhausted
			// the advertised IP addresses, or have made a
			// connection.
			var connected bool
			for _, addr := range addrs {
				switch addr.(type) {
				case *net.TCPAddr, *tor.OnionAddr:
					lnAddr.Address = addr
				default:
					return false, fmt.Errorf("unknown "+
						"address type %T", addr)
				}

				err := svr.ConnectToPeer(lnAddr, false)
				if err != nil {
					// If we weren't able to connect to the
					// peer at this address, then we'll move
					// onto the next.
					continue
				}

				connected = true
				break
			}

			// If we weren't able to establish a connection at all,
			// then we'll error out.
			if !connected {
				return false, errors.New("exhausted all " +
					"advertised addresses")
			}

			return false, nil
		},
		DisconnectPeer: svr.DisconnectPeer,
	}

	// Next, we'll fetch the current state of open channels from the
	// database to use as initial state for the auto-pilot agent.
	activeChannels, err := svr.chanDB.FetchAllChannels()
	if err != nil {
		return nil, err
	}
	initialChanState := make([]autopilot.Channel, len(activeChannels))
	for i, channel := range activeChannels {
		initialChanState[i] = autopilot.Channel{
			ChanID:   channel.ShortChanID(),
			Capacity: channel.Capacity,
			Node:     autopilot.NewNodeID(channel.IdentityPub),
		}
	}

	// Now that we have all the initial dependencies, we can create the
	// auto-pilot instance itself.
	pilot, err := autopilot.New(pilotCfg, initialChanState)
	if err != nil {
		return nil, err
	}

	// Finally, we'll need to subscribe to two things: incoming
	// transactions that modify the wallet's balance, and also any graph
	// topology updates.
	txnSubscription, err := svr.cc.wallet.SubscribeTransactions()
	if err != nil {
		return nil, err
	}
	graphSubscription, err := svr.chanRouter.SubscribeTopology()
	if err != nil {
		return nil, err
	}

	// We'll launch a goroutine to provide the agent with notifications
	// whenever the balance of the wallet changes.
	svr.wg.Add(2)
	go func() {
		defer txnSubscription.Cancel()
		defer svr.wg.Done()

		for {
			select {
			case <-txnSubscription.ConfirmedTransactions():
				pilot.OnBalanceChange()
			case <-svr.quit:
				return
			}
		}

	}()
	go func() {
		defer svr.wg.Done()

		for {
			select {
			// We won't act upon new unconfirmed transaction, as
			// we'll only use confirmed outputs when funding.
			// However, we will still drain this request in order
			// to avoid goroutine leaks, and ensure we promptly
			// read from the channel if available.
			case <-txnSubscription.UnconfirmedTransactions():
			case <-svr.quit:
				return
			}
		}

	}()

	// We'll also launch a goroutine to provide the agent with
	// notifications for when the graph topology controlled by the node
	// changes.
	svr.wg.Add(1)
	go func() {
		defer graphSubscription.Cancel()
		defer svr.wg.Done()

		for {
			select {
			case topChange, ok := <-graphSubscription.TopologyChanges:
				// If the router is shutting down, then we will
				// as well.
				if !ok {
					return
				}

				for _, edgeUpdate := range topChange.ChannelEdgeUpdates {
					// If this isn't an advertisement by
					// the backing lnd node, then we'll
					// continue as we only want to add
					// channels that we've created
					// ourselves.
					if !edgeUpdate.AdvertisingNode.IsEqual(self) {
						continue
					}

					// If this is indeed a channel we
					// opened, then we'll convert it to the
					// autopilot.Channel format, and notify
					// the pilot of the new channel.
					chanNode := autopilot.NewNodeID(
						edgeUpdate.ConnectingNode,
					)
					chanID := lnwire.NewShortChanIDFromInt(
						edgeUpdate.ChanID,
					)
					edge := autopilot.Channel{
						ChanID:   chanID,
						Capacity: edgeUpdate.Capacity,
						Node:     chanNode,
					}
					pilot.OnChannelOpen(edge)
				}

				// For each closed channel, we'll obtain
				// the chanID of the closed channel and send it
				// to the pilot.
				for _, chanClose := range topChange.ClosedChannels {
					chanID := lnwire.NewShortChanIDFromInt(
						chanClose.ChanID,
					)

					pilot.OnChannelClose(chanID)
				}

				// If new nodes were added to the graph, or nod
				// information has changed, we'll poke autopilot
				// to see if it can make use of them.
				if len(topChange.NodeUpdates) > 0 {
					pilot.OnNodeUpdates()
				}

			case <-svr.quit:
				return
			}
		}
	}()

	return pilot, nil
}
