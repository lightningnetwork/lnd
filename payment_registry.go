package main

import (
	"sync"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/channeldb"
)

// paymentRegistry is a central registry of all the outgoing payments
// its main functionaity is to manage and notify subscrie with new added payments
type paymentRegistry struct {
	sync.RWMutex

	cdb                  *channeldb.DB
	nextClientID         uint32
	notificationClients  map[uint32]*paymentSubscription
	newSubscriptions     chan *paymentSubscription
	paymentNotifications chan *channeldb.OutgoingPayment
	subscriptionCancels  chan uint32

	wg   sync.WaitGroup
	quit chan struct{}
}

// newPaymentRegistry creates a new paymentRegistry. The payment registry
// exposes storage related payments API that requires clients to be notified
// e.g AddPayment
func newPaymentRegistry(cdb *channeldb.DB) *paymentRegistry {
	return &paymentRegistry{
		cdb:                  cdb,
		notificationClients:  make(map[uint32]*paymentSubscription),
		newSubscriptions:     make(chan *paymentSubscription),
		paymentNotifications: make(chan *channeldb.OutgoingPayment),
		subscriptionCancels:  make(chan uint32),
		quit:                 make(chan struct{}),
	}
}

// Start starts the go routine that is responsible for handling subscriptions
// and dispatching notifications
func (p *paymentRegistry) Start() {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.paymentsNotifier()
	}()
}

// Stop signals the registry for a graceful shutdown and wait for the
// main go routine to finish
func (p *paymentRegistry) Stop() {
	close(p.quit)
	p.wg.Wait()
}

// paymentsNotifier is the dedicated goroutine responsible for accepting
// new notification subscriptions, cancelling old subscriptions, and
// dispatching new payment events.
func (p *paymentRegistry) paymentsNotifier() {

	for {
		select {
		case newClient := <-p.newSubscriptions:

			//Check if we need to deliver any missing payments
			if newClient.addIndex > 0 {
				err := p.deliverMissedPayments(newClient)
				if err != nil {
					ltndLog.Errorf("unable to deliver backlog payment "+
						"notifications: %v", err)
				}
			}

			ltndLog.Infof("New payment subscription "+
				"client: id=%v", newClient.id)

			p.notificationClients[newClient.id] = newClient

		case clientID := <-p.subscriptionCancels:
			ltndLog.Infof("Cancelling payment subscription for "+
				"client=%v", clientID)

			delete(p.notificationClients, clientID)
		case payment := <-p.paymentNotifications:
			p.notifyClients(payment)
		case <-p.quit:
			return
		}
	}
}

// notifyClients notify all subscribers about a payment
func (p *paymentRegistry) notifyClients(payment *channeldb.OutgoingPayment) {
	for _, client := range p.notificationClients {
		targetChan := client.NewPayments
		go func() {
			select {
			case targetChan <- payment:
			case <-p.quit:
				return
			}
		}()
	}
}

// deliverMissedPayments notify the client with all existing payments
// that this client has missed since last connected.
func (p *paymentRegistry) deliverMissedPayments(client *paymentSubscription) error {
	existingPayments, err := p.cdb.PaymentsAddedSince(client.addIndex)
	if err != nil {
		return err
	}

	for _, payment := range existingPayments {

		select {
		case client.NewPayments <- payment:
		case <-p.quit:
			return nil
		}
	}

	return nil
}

// AddPayment saves an OutgoingPayment to disk and dispatches a notification
// for all subscribers.
func (p *paymentRegistry) AddPayment(payment *channeldb.OutgoingPayment) (uint64, error) {
	p.Lock()
	defer p.Unlock()

	addIndex, err := p.cdb.AddPayment(payment)
	if err != nil {
		return 0, err
	}

	// notify the clients of this new payment.
	select {
	case p.paymentNotifications <- payment:
	case <-p.quit:
	}

	return addIndex, nil
}

// paymentSubscription represents an intent to receive updates for newly added
// payments. For each newly added payment, a copy of the payment
// will be sent over the NewPayments channel.
type paymentSubscription struct {

	// NewPayments is a channel that we'll use to send all newly created
	// payments with a payments index greater than addIndex
	NewPayments chan *channeldb.OutgoingPayment

	// id is the identifier for this subscription
	id uint32

	// addIndex is the payment index from where the caller wants to get
	// notifications. When a new subscription is registered we will dispatch
	// first all backward notifictions from that index.
	addIndex uint64

	paymentReg *paymentRegistry
}

// Cancel unregisters the paymentSubscription
func (p *paymentSubscription) Cancel() {

	select {
	case p.paymentReg.subscriptionCancels <- p.id:
	case <-p.paymentReg.quit:
	}
}

// SubscribeNotifications returns an paymentSubscription that receives
// async notficiations on every outgoing payment that is saved.
// upon subscription, existing payments starting from "sinceAddIndex" will be
// delivered and after that every new payment will be delivered.
func (p *paymentRegistry) SubscribeNotifications(sinceAddIndex uint64) *paymentSubscription {
	client := &paymentSubscription{
		NewPayments: make(chan *channeldb.OutgoingPayment),
		id:          atomic.AddUint32(&p.nextClientID, 1),
		addIndex:    sinceAddIndex,
		paymentReg:  p,
	}

	select {
	case p.newSubscriptions <- client:
	case <-p.quit:
	}

	return client
}
