package funding

import (
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
)

type annSigNonces struct {
	sent     *sentNonces
	received *receivedNonces
}

type sentNonces struct {
	btc  *musig2.Nonces
	node *musig2.Nonces
}

type receivedNonces struct {
	btc  [musig2.PubNonceSize]byte
	node [musig2.PubNonceSize]byte
}

type nonceManager struct {
	started sync.Once

	idKey *btcec.PublicKey

	store channeldb.AnnouncementNonceStore

	annSigNonces map[lnwire.ChannelID]annSigNonces

	mu sync.Mutex
}

func newNonceManager(store channeldb.AnnouncementNonceStore,
	idKey *btcec.PublicKey) *nonceManager {

	return &nonceManager{
		idKey:        idKey,
		store:        store,
		annSigNonces: make(map[lnwire.ChannelID]annSigNonces),
	}
}

func (m *nonceManager) start() error {
	var returnErr error
	m.started.Do(func() {
		nonces, err := m.store.GetAllAnnouncementNonces()
		if err != nil {
			returnErr = err

			return
		}

		for chanID, n := range nonces {
			m.annSigNonces[chanID] = annSigNonces{
				received: &receivedNonces{
					btc:  n.Btc,
					node: n.Node,
				},
			}
		}
	})

	return returnErr
}

func (m *nonceManager) completed(chanID lnwire.ChannelID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.annSigNonces, chanID)

	return m.store.DeleteAnnouncementNonces(chanID)
}

func (m *nonceManager) getReceivedNonces(chanID lnwire.ChannelID) (
	[musig2.PubNonceSize]byte, [musig2.PubNonceSize]byte, bool) {

	m.mu.Lock()
	defer m.mu.Unlock()

	aggNonces, ok := m.annSigNonces[chanID]
	if !ok || aggNonces.received == nil {
		return [musig2.PubNonceSize]byte{}, [musig2.PubNonceSize]byte{},
			false
	}

	return aggNonces.received.btc, aggNonces.received.node, true
}

func (m *nonceManager) receivedNonces(chanID lnwire.ChannelID, node,
	btc [musig2.PubNonceSize]byte, revProducer shachain.Producer,
	multiSigBTCKey *btcec.PublicKey) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	sendNonces, err := m.getNoncesToSendUnsafe(
		chanID, revProducer, multiSigBTCKey,
	)
	if err != nil {
		return err
	}

	announcementSendNonces, ok := m.annSigNonces[chanID]
	if !ok {
		m.annSigNonces[chanID] = annSigNonces{
			sent: sendNonces,
		}
		announcementSendNonces = m.annSigNonces[chanID]
	}

	announcementSendNonces.received = &receivedNonces{
		btc:  btc,
		node: node,
	}

	m.annSigNonces[chanID] = announcementSendNonces

	return m.store.SaveAnnouncementNonces(
		chanID, &channeldb.AnnouncementNonces{
			Node: node,
			Btc:  btc,
		},
	)
}

func (m *nonceManager) getNoncesToSend(chanID lnwire.ChannelID,
	revProducer shachain.Producer, multiSigBTCKey *btcec.PublicKey) (
	*musig2.Nonces, *musig2.Nonces, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	nonces, err := m.getNoncesToSendUnsafe(
		chanID, revProducer, multiSigBTCKey,
	)
	if err != nil {
		return nil, nil, err
	}

	return nonces.btc, nonces.node, nil
}

func (m *nonceManager) getNoncesToSendUnsafe(chanID lnwire.ChannelID,
	revProducer shachain.Producer, multiSigBTCKey *btcec.PublicKey) (
	*sentNonces, error) {

	announceNonces, ok := m.annSigNonces[chanID]
	if ok && announceNonces.sent != nil {
		return announceNonces.sent, nil
	}

	sent, err := m.genAnnouncementNonces(revProducer, multiSigBTCKey)
	if err != nil {
		return nil, err
	}

	if !ok {
		m.annSigNonces[chanID] = annSigNonces{sent: sent}
	} else {
		announceNonces.sent = sent
	}

	announceNonces = m.annSigNonces[chanID]

	return sent, nil
}

func (m *nonceManager) genAnnouncementNonces(revProducer shachain.Producer,
	multiSigBTCKey *btcec.PublicKey) (*sentNonces, error) {

	musig2ShaChain, err := channeldb.DeriveMusig2Shachain(
		revProducer,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to generate musig "+
			"announcement nonces: %v", err)
	}

	// TODO: replace with unique derivation.
	btcNonce, err := channeldb.NewMusigVerificationNonce(
		multiSigBTCKey,
		0,
		musig2ShaChain,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to generate musig "+
			"announcement nonces: %v", err)
	}

	// TODO: replace with unique derivation.
	nodeNonce, err := channeldb.NewMusigVerificationNonce(
		m.idKey,
		1,
		musig2ShaChain,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to generate musig "+
			"announcement nonces: %v", err)
	}

	return &sentNonces{
		btc:  btcNonce,
		node: nodeNonce,
	}, nil
}
