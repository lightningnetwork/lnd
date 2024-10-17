package wtclient

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
	"github.com/lightningnetwork/lnd/watchtower/wtserver"
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

// SessionNegotiator is an interface for asynchronously requesting new sessions.
type SessionNegotiator interface {
	// RequestSession signals to the session negotiator that the client
	// needs another session. Once the session is negotiated, it should be
	// returned via NewSessions.
	RequestSession()

	// NewSessions is a read-only channel where newly negotiated sessions
	// will be delivered.
	NewSessions() <-chan *ClientSession

	// Start safely initializes the session negotiator.
	Start() error

	// Stop safely shuts down the session negotiator.
	Stop() error
}

// NegotiatorConfig provides access to the resources required by a
// SessionNegotiator to faithfully carry out its duties. All nil-able field must
// be initialized.
type NegotiatorConfig struct {
	// DB provides access to a persistent storage medium used by the tower
	// to properly allocate session ephemeral keys and record successfully
	// negotiated sessions.
	DB DB

	// SecretKeyRing allows the client to derive new session private keys
	// when attempting to negotiate session with a tower.
	SecretKeyRing ECDHKeyRing

	// Candidates is an abstract set of tower candidates that the negotiator
	// will traverse serially when attempting to negotiate a new session.
	Candidates TowerCandidateIterator

	// Policy defines the session policy that will be proposed to towers
	// when attempting to negotiate a new session. This policy will be used
	// across all negotiation proposals for the lifetime of the negotiator.
	Policy wtpolicy.Policy

	// Dial initiates an outbound brontide connection to the given address
	// using a specified private key. The peer is returned in the event of a
	// successful connection.
	Dial func(keychain.SingleKeyECDH, *lnwire.NetAddress) (wtserver.Peer,
		error)

	// SendMessage writes a wtwire message to remote peer.
	SendMessage func(wtserver.Peer, wtwire.Message) error

	// ReadMessage reads a message from a remote peer and returns the
	// decoded wtwire message.
	ReadMessage func(wtserver.Peer) (wtwire.Message, error)

	// ChainHash the genesis hash identifying the chain for any negotiated
	// sessions. Any state updates sent to that session should also
	// originate from this chain.
	ChainHash chainhash.Hash

	// MinBackoff defines the initial backoff applied by the session
	// negotiator after all tower candidates have been exhausted and
	// reattempting negotiation with the same set of candidates. Subsequent
	// backoff durations will grow exponentially.
	MinBackoff time.Duration

	// MaxBackoff defines the maximum backoff applied by the session
	// negotiator after all tower candidates have been exhausted and
	// reattempting negotiation with the same set of candidates. If the
	// exponential backoff produces a timeout greater than this value, the
	// backoff duration will be clamped to MaxBackoff.
	MaxBackoff time.Duration

	// Log specifies the desired log output, which should be prefixed by the
	// client type, e.g. anchor or legacy.
	Log btclog.Logger
}

// sessionNegotiator is concrete SessionNegotiator that is able to request new
// sessions from a set of candidate towers asynchronously and return successful
// sessions to the primary client.
type sessionNegotiator struct {
	started sync.Once
	stopped sync.Once

	localInit *wtwire.Init

	cfg *NegotiatorConfig
	log btclog.Logger

	dispatcher             chan struct{}
	newSessions            chan *ClientSession
	successfulNegotiations chan *ClientSession

	wg   sync.WaitGroup
	quit chan struct{}
}

// Compile-time constraint to ensure a *sessionNegotiator implements the
// SessionNegotiator interface.
var _ SessionNegotiator = (*sessionNegotiator)(nil)

// newSessionNegotiator initializes a fresh sessionNegotiator instance.
func newSessionNegotiator(cfg *NegotiatorConfig) *sessionNegotiator {
	// Generate the set of features the negotiator will present to the tower
	// upon connection.
	features := cfg.Policy.FeatureBits()

	localInit := wtwire.NewInitMessage(
		lnwire.NewRawFeatureVector(features...),
		cfg.ChainHash,
	)

	return &sessionNegotiator{
		cfg:                    cfg,
		log:                    cfg.Log,
		localInit:              localInit,
		dispatcher:             make(chan struct{}, 1),
		newSessions:            make(chan *ClientSession),
		successfulNegotiations: make(chan *ClientSession),
		quit:                   make(chan struct{}),
	}
}

// Start safely starts up the sessionNegotiator.
func (n *sessionNegotiator) Start() error {
	n.started.Do(func() {
		n.log.Debugf("Starting session negotiator")

		n.wg.Add(1)
		go n.negotiationDispatcher()
	})

	return nil
}

// Stop safely shuts down the sessionNegotiator.
func (n *sessionNegotiator) Stop() error {
	n.stopped.Do(func() {
		n.log.Debugf("Stopping session negotiator")

		close(n.quit)
		n.wg.Wait()
	})

	return nil
}

// NewSessions returns a receive-only channel from which newly negotiated
// sessions will be returned.
func (n *sessionNegotiator) NewSessions() <-chan *ClientSession {
	return n.newSessions
}

// RequestSession sends a request to the sessionNegotiator to begin requesting a
// new session. If one is already in the process of being negotiated, the
// request will be ignored.
func (n *sessionNegotiator) RequestSession() {
	select {
	case n.dispatcher <- struct{}{}:
	default:
	}
}

// negotiationDispatcher acts as the primary event loop for the
// sessionNegotiator, coordinating requests for more sessions and dispatching
// attempts to negotiate them from a list of candidates.
func (n *sessionNegotiator) negotiationDispatcher() {
	defer n.wg.Done()

	var pendingNegotiations int
	for {
		select {
		case <-n.dispatcher:
			pendingNegotiations++

			if pendingNegotiations > 1 {
				n.log.Debugf("Already negotiating session, " +
					"waiting for existing negotiation to " +
					"complete")
				continue
			}

			// TODO(conner): consider reusing good towers

			n.log.Debugf("Dispatching session negotiation")

			n.wg.Add(1)
			go n.negotiate()

		case session := <-n.successfulNegotiations:
			select {
			case n.newSessions <- session:
				pendingNegotiations--
			case <-n.quit:
				return
			}

			if pendingNegotiations > 0 {
				n.log.Debugf("Dispatching pending session " +
					"negotiation")

				n.wg.Add(1)
				go n.negotiate()
			}

		case <-n.quit:
			return
		}
	}
}

// negotiate handles the process of iterating through potential tower candidates
// and attempting to negotiate a new session until a successful negotiation
// occurs. If the candidate iterator becomes exhausted because none were
// successful, this method will back off exponentially up to the configured max
// backoff. This method will continue trying until a negotiation is successful
// before returning the negotiated session to the dispatcher via the succeed
// channel.
//
// NOTE: This method MUST be run as a goroutine.
func (n *sessionNegotiator) negotiate() {
	defer n.wg.Done()

	// On the first pass, initialize the backoff to our configured min
	// backoff.
	var backoff time.Duration

	// Create a closure to update the backoff upon failure such that it
	// stays within our min and max backoff parameters.
	updateBackoff := func() {
		if backoff == 0 {
			backoff = n.cfg.MinBackoff
		} else {
			backoff *= 2
			if backoff > n.cfg.MaxBackoff {
				backoff = n.cfg.MaxBackoff
			}
		}
	}

retryWithBackoff:
	// If we are retrying, wait out the delay before continuing.
	if backoff > 0 {
		select {
		case <-time.After(backoff):
		case <-n.quit:
			return
		}
	}

tryNextCandidate:
	for {
		select {
		case <-n.quit:
			return
		default:
		}

		// Pull the next candidate from our list of addresses.
		tower, err := n.cfg.Candidates.Next()
		if err != nil {
			// We've run out of addresses, update our backoff.
			updateBackoff()

			n.log.Debugf("Unable to get new tower candidate, "+
				"retrying after %v -- reason: %v", backoff, err)

			// Only reset the iterator once we've exhausted all
			// candidates. Doing so allows us to load balance
			// sessions better amongst all of the tower candidates.
			if err == ErrTowerCandidatesExhausted {
				n.cfg.Candidates.Reset()
			}

			goto retryWithBackoff
		}

		towerPub := tower.IdentityKey.SerializeCompressed()
		n.log.Debugf("Attempting session negotiation with tower=%x",
			towerPub)

		var forceNextKey bool
		for {
			// Before proceeding, we will reserve a session key
			// index to use with this specific tower. If one is
			// already reserved, the existing index will be
			// returned.
			keyIndex, err := n.cfg.DB.NextSessionKeyIndex(
				tower.ID, n.cfg.Policy.BlobType, forceNextKey,
			)
			if err != nil {
				n.log.Debugf("Unable to reserve session key "+
					"index for tower=%x: %v", towerPub, err)

				goto tryNextCandidate
			}

			// We'll now attempt the CreateSession dance with the
			// tower to get a new session, trying all addresses if
			// necessary.
			err = n.createSession(tower, keyIndex)
			if err == nil {
				return
			} else if errors.Is(err, ErrSessionKeyAlreadyUsed) {
				forceNextKey = true
				continue
			}

			// An unexpected error occurred, update our backoff.
			updateBackoff()

			n.log.Debugf("Session negotiation with tower=%x "+
				"failed, trying again -- reason: %v", towerPub,
				err)

			goto retryWithBackoff
		}
	}
}

// createSession takes a tower and attempts to negotiate a session using any of
// its stored addresses. This method returns after the first successful
// negotiation, or after all addresses have failed with ErrFailedNegotiation.
func (n *sessionNegotiator) createSession(tower *Tower, keyIndex uint32) error {
	sessionKeyDesc, err := n.cfg.SecretKeyRing.DeriveKey(
		keychain.KeyLocator{
			Family: keychain.KeyFamilyTowerSession,
			Index:  keyIndex,
		},
	)
	if err != nil {
		return err
	}
	sessionKey := keychain.NewPubKeyECDH(
		sessionKeyDesc, n.cfg.SecretKeyRing,
	)

	addr := tower.Addresses.PeekAndLock()
	for {
		lnAddr := &lnwire.NetAddress{
			IdentityKey: tower.IdentityKey,
			Address:     addr,
		}

		err = n.tryAddress(sessionKey, keyIndex, tower, lnAddr)
		tower.Addresses.ReleaseLock(addr)
		switch {
		case errors.Is(err, ErrSessionKeyAlreadyUsed):
			return err

		case errors.Is(err, ErrPermanentTowerFailure):
			// TODO(conner): report to iterator? can then be reset
			// with restart
			fallthrough

		case err != nil:
			n.log.Debugf("Request for session negotiation with "+
				"tower=%s failed, trying again -- reason: "+
				"%v", lnAddr, err)

			// Get the next tower address if there is one.
			addr, err = tower.Addresses.NextAndLock()
			if err == ErrAddressesExhausted {
				tower.Addresses.Reset()

				return ErrFailedNegotiation
			}

			continue

		default:
			return nil
		}
	}
}

// tryAddress executes a single create session dance using the given address.
// The address should belong to the tower's set of addresses. This method only
// returns true if all steps succeed and the new session has been persisted, and
// fails otherwise.
func (n *sessionNegotiator) tryAddress(sessionKey keychain.SingleKeyECDH,
	keyIndex uint32, tower *Tower, lnAddr *lnwire.NetAddress) error {

	// Connect to the tower address using our generated session key.
	conn, err := n.cfg.Dial(sessionKey, lnAddr)
	if err != nil {
		return err
	}

	// Send local Init message.
	err = n.cfg.SendMessage(conn, n.localInit)
	if err != nil {
		return fmt.Errorf("unable to send Init: %w", err)
	}

	// Receive remote Init message.
	remoteMsg, err := n.cfg.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("unable to read Init: %w", err)
	}

	// Check that returned message is wtwire.Init.
	remoteInit, ok := remoteMsg.(*wtwire.Init)
	if !ok {
		return fmt.Errorf("expected Init, got %T in reply", remoteMsg)
	}

	// Verify the watchtower's remote Init message against our own.
	err = n.localInit.CheckRemoteInit(remoteInit, wtwire.FeatureNames)
	if err != nil {
		return err
	}

	policy := n.cfg.Policy
	createSession := &wtwire.CreateSession{
		BlobType:     policy.BlobType,
		MaxUpdates:   policy.MaxUpdates,
		RewardBase:   policy.RewardBase,
		RewardRate:   policy.RewardRate,
		SweepFeeRate: policy.SweepFeeRate,
	}

	// Send CreateSession message.
	err = n.cfg.SendMessage(conn, createSession)
	if err != nil {
		return fmt.Errorf("unable to send CreateSession: %w", err)
	}

	// Receive CreateSessionReply message.
	remoteMsg, err = n.cfg.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("unable to read CreateSessionReply: %w", err)
	}

	// Check that returned message is wtwire.CreateSessionReply.
	createSessionReply, ok := remoteMsg.(*wtwire.CreateSessionReply)
	if !ok {
		return fmt.Errorf("expected CreateSessionReply, got %T in "+
			"reply", remoteMsg)
	}

	switch createSessionReply.Code {
	case wtwire.CodeOK:
		// TODO(conner): validate reward address
		rewardPkScript := createSessionReply.Data

		sessionID := wtdb.NewSessionIDFromPubKey(sessionKey.PubKey())
		dbClientSession := &wtdb.ClientSession{
			ClientSessionBody: wtdb.ClientSessionBody{
				TowerID:        tower.ID,
				KeyIndex:       keyIndex,
				Policy:         n.cfg.Policy,
				RewardPkScript: rewardPkScript,
			},
			ID: sessionID,
		}

		err = n.cfg.DB.CreateClientSession(dbClientSession)
		if err != nil {
			return fmt.Errorf("unable to persist ClientSession: %w",
				err)
		}

		n.log.Debugf("New session negotiated with %s, policy: %s",
			lnAddr, dbClientSession.Policy)

		clientSession := &ClientSession{
			ID:                sessionID,
			ClientSessionBody: dbClientSession.ClientSessionBody,
			Tower:             tower,
			SessionKeyECDH:    sessionKey,
		}

		// We have a newly negotiated session, return it to the
		// dispatcher so that it can update how many outstanding
		// negotiation requests we have.
		select {
		case n.successfulNegotiations <- clientSession:
			return nil
		case <-n.quit:
			return ErrNegotiatorExiting
		}

	case wtwire.CreateSessionCodeAlreadyExists:
		// TODO(conner): use the last-applied in the create session
		//  reply to handle case where we lose state, session already
		//  exists, and we want to possibly resume using the session.
		//  NOTE that this should not be done until the server code
		//  has been adapted to first check that the CreateSession
		//  request is for the same blob-type as the initial session.

		return ErrSessionKeyAlreadyUsed

	// TODO(conner): handle error codes properly
	case wtwire.CreateSessionCodeRejectBlobType:
		return fmt.Errorf("tower rejected blob type: %v",
			policy.BlobType)

	case wtwire.CreateSessionCodeRejectMaxUpdates:
		return fmt.Errorf("tower rejected max updates: %v",
			policy.MaxUpdates)

	case wtwire.CreateSessionCodeRejectRewardRate:
		// The tower rejected the session because of the reward rate. If
		// we didn't request a reward session, we'll treat this as a
		// permanent tower failure.
		if !policy.BlobType.Has(blob.FlagReward) {
			return ErrPermanentTowerFailure
		}

		return fmt.Errorf("tower rejected reward rate: %v",
			policy.RewardRate)

	case wtwire.CreateSessionCodeRejectSweepFeeRate:
		return fmt.Errorf("tower rejected sweep fee rate: %v",
			policy.SweepFeeRate)

	default:
		return fmt.Errorf("received unhandled error code: %v",
			createSessionReply.Code)
	}
}
