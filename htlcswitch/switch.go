package htlcswitch

import (
	"github.com/btcsuite/fastsha256"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"sync"
	"time"
)

var (
	// ErrHTLCManagerNotFound is used when htlc manager was not found.
	ErrHTLCManagerNotFound = errors.New("htlc manager not found")
)

// HTLCSwitch is a central messaging bus for all incoming/outgoing HTLC's.
// The goal of the switch is forward the incoming/outgoing HTLC messages from
// one channel to another, and also propagate the settle HTLC messages back to
// original requester. In order to better understand the whole view:
//	alice			   bob				   carol
//	server - <-connection-> - server - - <-connection-> - - - server
//	 |		   	  |				    |
//   alice htlc			bob htlc		       carol htlc
//     switch			switch			         switch
//	|			 |       \			   |
//	|			 |        \			   |
// alice htlc  <-channel->  first bob    second bob <-channel-> carol htlc
// manager	    	  htlc manager   htlc manager		manager
type HTLCSwitch struct {
	service *Service
	mutex   sync.RWMutex

	// circuits is an structure which is used to forward the settle HTLC
	// back to the add HTLC initiator.
	circuits *circuitMap

	// managers is a map of channel output and HTLC manager which manages
	// this channel.
	managers map[wire.OutPoint]HTLCManager

	// commands...
	commands chan *handleRequestCommand
}

// NewHTLCSwitch creates the HTLCSwitch instance.
func NewHTLCSwitch() (*HTLCSwitch, error) {
	circuitMap, err := newCircuitMap()
	if err != nil {
		return nil, err
	}

	return &HTLCSwitch{
		service:  NewService("HTLC Switch"),
		circuits: circuitMap,
		managers: make(map[wire.OutPoint]HTLCManager),
		commands: make(chan *handleRequestCommand),
	}, nil
}

// Forward is used by HTLC managers to propagate the HTLC after it isn't
// reached its final destination and eligible for forwarding. HTLC are
// encapsulated in switch request in order to carry additional information.
func (s *HTLCSwitch) Forward(request *SwitchRequest) error {
	command := &handleRequestCommand{
		request: request,
		err:     make(chan error),
	}

	select {
	case s.commands <- command:
		return <-command.err
	case <-s.service.Quit:
		return nil
	}
}

// forwardRequest handles incoming forward requests received from htlc managers.
func (s *HTLCSwitch) forwardRequest(request *SwitchRequest) error {
	switch request.Type {

	// User sent us new payment request, therefore we trying to find the
	// HTLC appropriate manager in terms of destination and bandwidth.
	case UserAddRequest:
		htlc := request.Htlc.(*lnwire.HTLCAddRequest)

		managers, err := s.getManagersByDest(request.Dest)
		if err != nil {
			return err
		}

		var destination HTLCManager
		for _, manager := range managers {
			if manager.Bandwidth() >= htlc.Amount {
				destination = manager
				break
			}
		}

		if destination == nil {
			request.Error() <- errors.New("unable to send payment, " +
				"insufficient bandwidth")
			return nil
		}

		log.Debugf("Sending %v to %x", htlc.Amount, request.Dest.String())
		return destination.HandleRequest(request)

	// HTLC manager forwarded us a new HTLC, therefore we initiate the
	// payment circuit within our internal state so we can properly forward
	// the ultimate settle message back latter.
	case ForwardAddRequest:
		htlc := request.Htlc.(*lnwire.HTLCAddRequest)

		source, err := s.Get(request.ChannelPoint)
		if err != nil {
			return errors.Errorf("unable to find source htlc "+
				"manager %v", err)
		}

		managers, err := s.getManagersByDest(request.Dest)
		if err != nil {
			log.Errorf("unable to find managers with "+
				"destination %v", err)

			source.HandleRequest(NewCancelRequest(
				request.ChannelPoint,
				&lnwire.CancelHTLC{
					Reason: lnwire.UnknownDestination,
				},
				htlc.RedemptionHashes[0],
			))
			return nil
		}

		var destination HTLCManager
		for _, manager := range managers {
			if manager.Bandwidth() >= htlc.Amount {
				destination = manager
				break
			}
		}

		// If the htlc manager we're attempting to forward the
		// HTLC over has insufficient capacity, then
		// we'll cancel the HTLC as the payment cannot
		// succeed.
		if destination == nil {
			log.Errorf("unable to forward HTLC channels has "+
				"insufficient capacity, need %v", htlc.Amount)
			source.HandleRequest(NewCancelRequest(
				request.ChannelPoint,
				&lnwire.CancelHTLC{
					Reason: lnwire.InsufficientCapacity,
				},
				htlc.RedemptionHashes[0],
			))
			return nil
		}

		err = s.circuits.add(NewPaymentCircuit(
			source.ID(),
			destination.ID(),
			htlc.RedemptionHashes[0],
		))
		if err != nil {
			return errors.Errorf("unable to add circuit: %v", err)
		}

		// With the circuit initiated, send the request
		// to the htlc manager which manages destination channel.
		return destination.HandleRequest(request)

	// We've just received a settle request which means we can finalize the
	// payment circuit by forwarding the settle msg to the channel from
	// which HTLC add request was initially received.
	case ForwardSettleRequest:
		htlc := request.Htlc.(*lnwire.HTLCSettleRequest)
		rHash := fastsha256.Sum256(htlc.RedemptionProofs[0][:])

		// Exit if we can't find and remove the active circuit to
		// continue propagating the cancel over.
		circuit, err := s.circuits.remove(rHash, request.ChannelPoint)
		if err != nil {
			return errors.Errorf("unable to remove circuit for "+
				"payment hash: %v", rHash)
		}

		// Propagating settle htlc back to source of add htlc request.
		source, err := s.Get(circuit.Src)
		if err != nil {
			return errors.Errorf("unable to get source htlc manager to "+
				"forward settle htlc:", err)
		}

		log.Debugf("Closing completed onion "+
			"circuit for %x: %v<->%v", rHash[:],
			circuit.Src, circuit.Dest)

		return source.HandleRequest(request)

	case CancelRequest:
		// Exit if we can't find and remove the active circuit to
		// continue propagating the cancel over.
		circuit, err := s.circuits.remove(request.PayHash, request.ChannelPoint)
		if err != nil {
			return errors.Errorf("unable to remove circuit for "+
				"payment hash: %v", err)
		}

		// Propagating cancel htlc back to source of add htlc request.
		source, err := s.Get(circuit.Src)
		if err != nil {
			return errors.Errorf("unable to get source htlc manager to "+
				"forward settle htlc:", err)
		}

		log.Debugf("Closing canceled onion "+
			"circuit for %x: %v<->%v", request.PayHash[:],
			circuit.Src,
			circuit.Dest)

		return source.HandleRequest(request)
	default:
		return errors.New("wrong request type")
	}
}

// startHandling start handling inner command requests and print the
// htlc switch statistics.
// NOTE: Should be run as goroutine.
func (s *HTLCSwitch) startHandling() {
	defer s.service.Done()

	// TODO(roasbeef): cleared vs settled distinction
	var prevNumUpdates uint64
	var prevSatSent btcutil.Amount
	var prevSatRecv btcutil.Amount

	statsTicker := time.NewTicker(10 * time.Second)

	for {
		select {
		case command := <-s.commands:
			command.err <- s.forwardRequest(command.request)

		case <-statsTicker.C:
			var overallNumUpdates uint64
			var overallSatSent btcutil.Amount
			var overallSatRecv btcutil.Amount

			for _, manager := range s.managers {
				overallNumUpdates += manager.NumUpdates()
				overallSatSent += manager.SatSent()
				overallSatRecv += manager.SatRecv()
			}

			if overallNumUpdates == 0 {
				continue
			}

			diffNumUpdates := overallNumUpdates - prevNumUpdates
			diffSatSent := overallSatSent - prevSatSent
			diffSatRecv := overallSatRecv - prevSatRecv

			log.Infof("Sent %v satoshis, received %v satoshi in "+
				"the last 10 seconds (%v tx/sec)",
				diffSatSent.ToUnit(btcutil.AmountSatoshi),
				diffSatRecv.ToUnit(btcutil.AmountSatoshi),
				float64(diffNumUpdates)/10)

			prevNumUpdates = overallNumUpdates
			prevSatSent = overallSatSent
			prevSatRecv = overallSatRecv

		case <-s.service.Quit:
			return
		}
	}
}

// Start starts all helper goroutines required for the operation of the switch.
func (s *HTLCSwitch) Start() error {
	log.Infof("HTLC Switch starting")
	if err := s.service.Start(); err != nil {
		return err
	}

	s.service.Go(s.startHandling)
	return nil
}

// Stop gracefully stops all active helper goroutines, then waits until they've
// exited.
func (s *HTLCSwitch) Stop() error {
	log.Infof("HTLC Switch shutting down")
	if err := s.service.Stop(nil); err != nil {
		return err
	}

	for _, manager := range s.managers {
		s.remove(manager)
	}

	return nil
}

// Add is used to add and start the newly created HTLC manager ans start use it
// to propagate HTLCs.
func (s *HTLCSwitch) Add(manager HTLCManager) error {
	if err := manager.Start(); err != nil {
		return err
	}

	s.mutex.Lock()
	s.managers[*manager.ID()] = manager
	s.mutex.Unlock()

	log.Infof("Added htlc manager for ChannelPoint(%v), bandwidth=%v",
		manager.ID(), manager.Bandwidth())
	return nil
}

// Remove is used to remove and stop the htlc manager by channel point of the
// channel which htlc manager is managing.
func (s *HTLCSwitch) RemoveByChan(chanPoint *wire.OutPoint) error {
	manager, err := s.Get(chanPoint)
	if err != nil {
		return err
	}

	return s.remove(manager)
}

// Get returns the htlc manager which corresponds to the channel which he is
// managing.
func (s *HTLCSwitch) Get(chanPoint *wire.OutPoint) (HTLCManager, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	manager, ok := s.managers[*chanPoint]
	if !ok {
		return nil, ErrHTLCManagerNotFound
	}

	return manager, nil
}

// RemoveById removes all HTLC managers which correspond to destination id.
func (s *HTLCSwitch) RemoveById(id *routing.HopID) error {
	managers, err := s.getManagersByDest(id)
	if err != nil {
		return err
	}

	for _, manager := range managers {
		if err := s.remove(manager); err != nil {
			return err
		}
	}

	return nil
}

// remove is helper function which removes and stops HTLC manager.
func (s *HTLCSwitch) remove(manager HTLCManager) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	chanPoint := manager.ID()
	delete(s.managers, *chanPoint)
	manager.Stop()

	log.Infof("Remove htlc manager for ChannelPoint(%v)", manager.ID())
	return nil
}

// getManagersByDest is helper function which returns the htlc managers by hop
// destination id.
func (s *HTLCSwitch) getManagersByDest(destination *routing.HopID) ([]HTLCManager,
	error) {

	s.mutex.RLock()
	defer s.mutex.RUnlock()

	var result []HTLCManager
	for _, manager := range s.managers {
		if manager.HopID().Equal(destination) {
			result = append(result, manager)
		}
	}
	if result == nil {
		return nil, errors.Errorf("unable to locate htlc manager "+
			"destination hop id %v", destination.String())
	}
	return result, nil
}
