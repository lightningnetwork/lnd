package lnd

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

// validateAtplConfig is a helper method that makes sure the passed
// configuration is sane. Currently it checks that the heuristic configuration
// makes sense. In case the config is valid, it will return a list of
// WeightedHeuristics that can be combined for use with the autopilot agent.
func validateAtplCfg(cfg *autoPilotConfig) ([]*autopilot.WeightedHeuristic,
	error) {

	var (
		heuristicsStr string
		sum           float64
		heuristics    []*autopilot.WeightedHeuristic
	)

	// Create a help text that we can return in case the config is not
	// correct.
	for _, a := range autopilot.AvailableHeuristics {
		heuristicsStr += fmt.Sprintf(" '%v' ", a.Name())
	}
	availStr := fmt.Sprintf("Avaiblable heuristcs are: [%v]", heuristicsStr)

	// We'll go through the config and make sure all the heuristics exists,
	// and that the sum of their weights is 1.0.
	for name, weight := range cfg.Heuristic {
		a, ok := autopilot.AvailableHeuristics[name]
		if !ok {
			// No heuristic matching this config option was found.
			return nil, fmt.Errorf("Heuristic %v not available. %v",
				name, availStr)
		}

		// If this heuristic was among the registered ones, we add it
		// to the list we'll give to the agent, and keep track of the
		// sum of weights.
		heuristics = append(
			heuristics,
			&autopilot.WeightedHeuristic{
				Weight:              weight,
				AttachmentHeuristic: a,
			},
		)
		sum += weight
	}

	// Check found heuristics. We must have at least one to operate.
	if len(heuristics) == 0 {
		return nil, fmt.Errorf("No active heuristics. %v", availStr)
	}

	if sum != 1.0 {
		return nil, fmt.Errorf("Heuristic weights must sum to 1.0")
	}
	return heuristics, nil
}

// chanController is an implementation of the autopilot.ChannelController
// interface that's backed by a running lnd instance.
type chanController struct {
	server     *server
	private    bool
	minConfs   int32
	confTarget uint32
}

// OpenChannel opens a channel to a target peer, with a capacity of the
// specified amount. This function should un-block immediately after the
// funding transaction that marks the channel open has been broadcast.
func (c *chanController) OpenChannel(target *btcec.PublicKey,
	amt btcutil.Amount) error {

	// With the connection established, we'll now establish our connection
	// to the target peer, waiting for the first update before we exit.
	feePerKw, err := c.server.cc.feeEstimator.EstimateFeePerKW(
		c.confTarget,
	)
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
		subtractFees:    true,
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

// initAutoPilot initializes a new autopilot.ManagerCfg to manage an
// autopilot.Agent instance based on the passed configuration struct. The agent
// and all interfaces needed to drive it won't be launched before the Manager's
// StartAgent method is called.
func initAutoPilot(svr *server, cfg *autoPilotConfig) (*autopilot.ManagerCfg, error) {
	atplLog.Infof("Instantiating autopilot with cfg: %v", spew.Sdump(cfg))

	// Set up the constraints the autopilot heuristics must adhere to.
	atplConstraints := autopilot.NewConstraints(
		btcutil.Amount(cfg.MinChannelSize),
		btcutil.Amount(cfg.MaxChannelSize),
		uint16(cfg.MaxChannels),
		10,
		cfg.Allocation,
	)
	heuristics, err := validateAtplCfg(cfg)
	if err != nil {
		return nil, err
	}

	weightedAttachment, err := autopilot.NewWeightedCombAttachment(
		heuristics...,
	)
	if err != nil {
		return nil, err
	}

	// With the heuristic itself created, we can now populate the remainder
	// of the items that the autopilot agent needs to perform its duties.
	self := svr.identityPriv.PubKey()
	pilotCfg := autopilot.Config{
		Self:      self,
		Heuristic: weightedAttachment,
		ChanController: &chanController{
			server:     svr,
			private:    cfg.Private,
			minConfs:   cfg.MinConfs,
			confTarget: cfg.ConfTarget,
		},
		WalletBalance: func() (btcutil.Amount, error) {
			return svr.cc.wallet.ConfirmedBalance(cfg.MinConfs)
		},
		Graph:       autopilot.ChannelGraphFromDatabase(svr.chanDB.ChannelGraph()),
		Constraints: atplConstraints,
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

	// Create and return the autopilot.ManagerCfg that administrates this
	// agent-pilot instance.
	return &autopilot.ManagerCfg{
		Self:     self,
		PilotCfg: &pilotCfg,
		ChannelState: func() ([]autopilot.Channel, error) {
			// We'll fetch the current state of open
			// channels from the database to use as initial
			// state for the auto-pilot agent.
			activeChannels, err := svr.chanDB.FetchAllChannels()
			if err != nil {
				return nil, err
			}
			chanState := make([]autopilot.Channel,
				len(activeChannels))
			for i, channel := range activeChannels {
				chanState[i] = autopilot.Channel{
					ChanID:   channel.ShortChanID(),
					Capacity: channel.Capacity,
					Node: autopilot.NewNodeID(
						channel.IdentityPub),
				}
			}

			return chanState, nil
		},
		SubscribeTransactions: svr.cc.wallet.SubscribeTransactions,
		SubscribeTopology:     svr.chanRouter.SubscribeTopology,
	}, nil
}
