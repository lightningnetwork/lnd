package main

import (
	"crypto/rand"
	"fmt"
	"github.com/btcsuite/fastsha256"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"time"
)

func GenerateNewPreimage() ([32]byte, error) {
	var a [32]byte
	_, err := rand.Read(a[:])
	if err != nil {
		return a, err
	}
	h := fastsha256.Sum256(a[:])
	return h, nil
}

type PaymentDirection int

const (
	DirectionIncoming PaymentDirection = 1
	DirectionOutgoing PaymentDirection = 2
)

type PaymentState int

const (
	StateInitial             PaymentState = 1
	StateWaitForConfirmation PaymentState = 2
	StateRejected            PaymentState = 3
	StateWaitForSettlement   PaymentState = 4
	StateTimeout             PaymentState = 5
	StateDone                PaymentState = 6
	StateWaitForAdd          PaymentState = 7
)

type Payment struct {
	PaymentID        [32]byte
	InitDate         time.Time
	ConfirmationDate time.Time
	SettlementDate   time.Time
	Direction        PaymentDirection
	Amount           lnwire.CreditsAmount
	PreImage         wire.ShaHash
	HashPreImage     wire.ShaHash
	State            PaymentState
	Path             [][32]byte
	reversePathBlob  []byte
}

// Wrapper around Payment initiation messages to save their source or destination
type PaymentPacket struct {
	src wire.ShaHash
	dst wire.ShaHash

	msg lnwire.Message
}

type sendPaymentCmd struct {
	amount lnwire.CreditsAmount
	path   [][32]byte
	err    chan error
}

type PaymentManager struct {
	LightningID [32]byte

	// For incoming payment messages
	chPaymentIn chan *PaymentPacket

	// For outgoing payment messages
	chPaymentOut chan *PaymentPacket

	// For htlc messages. They should go from htlc switch
	chHTLCIn chan *htlcPacket

	// For htlc messages. They should go to htlc switch
	chHTLCOut chan *htlcPacket

	// Channel for control messages
	chCmd chan interface{}

	payments map[[32]byte]*Payment

	hashToPaymentID map[[32]byte][32]byte

	// For storing invoices
	invoices *invoiceRegistry

	quit chan struct{}
}

func NewPaymentManager() *PaymentManager {
	return &PaymentManager{
		chPaymentIn:     make(chan *PaymentPacket, 1),
		chPaymentOut:    make(chan *PaymentPacket, 1),
		chHTLCIn:        make(chan *htlcPacket, 1),
		chHTLCOut:       make(chan *htlcPacket, 1),
		chCmd:           make(chan interface{}, 1),
		payments:        make(map[[32]byte]*Payment),
		hashToPaymentID: make(map[[32]byte][32]byte),
		quit:            make(chan struct{}),
	}
}

func (p *PaymentManager) Start() {
	// TODO(mkl): Add checking for starting/stopping
	go func() {
		for {
			select {
			case paymentPkt := <-p.chPaymentIn:
				p.handlePaymentPacket(paymentPkt)
			case htlcPkt := <-p.chHTLCIn:
				p.handleHTLCPacket(htlcPkt)
			case cmd := <-p.chCmd:
				p.handleCmd(cmd)
			case <-p.quit:
				return
			}
		}
	}()
}

func (p *PaymentManager) Stop() {
	close(p.quit)
}

func (p *PaymentManager) handleCmd(cmd interface{}) {
	switch cmd := cmd.(type) {
	case *sendPaymentCmd:
		peerLog.Info("[handleCmd] processing")
		if cmd.path == nil || len(cmd.path) == 0 {
			cmd.err <- fmt.Errorf("Path should contain at least one address. Got %v", cmd.path)
		}
		// TODO(mkl): validate path
		paymentID, err := GenerateNewPreimage()
		if err != nil {
			cmd.err <- err
		}
		payment := Payment{
			PaymentID: paymentID,
			InitDate:  time.Now(),
			Direction: DirectionOutgoing,
			Amount:    cmd.amount,
			State:     StateWaitForConfirmation,
			Path:      cmd.path,
		}
		p.payments[paymentID] = &payment
		// Reverse BLOB contains LightningID in reverse order: from destination to sources
				msg := &lnwire.PaymentInitiation{
			Amount:               cmd.amount,
			InvoiceNumber:        paymentID,
		}
		paymentPkt := &PaymentPacket{
			dst: cmd.path[len(cmd.path)-1],
			msg: msg,
		}
		p.chPaymentOut <- paymentPkt
		cmd.err <- nil
	default:
		peerLog.Errorf("Received unknown command message of type %T: %v", cmd, cmd)
	}
}

func (p *PaymentManager) handlePaymentPacket(paymentPkt *PaymentPacket) {
	switch msg := paymentPkt.msg.(type) {
	case *lnwire.PaymentInitiation:
		// We got request for a payment initialisation from some peer
		// 1. Generate new PreImage
		// 2. Create new invoice
		// 3. Send PaymentInitiationConfirmation to that node
		// Assume that node always accept payment
		if _, ok := p.payments[msg.InvoiceNumber]; ok {
			peerLog.Errorf("We received *lnwire.PaymentInitiation with already used PaymentID", msg.InvoiceNumber)
			return
		}
		preimage, err := GenerateNewPreimage()
		if err != nil {
			peerLog.Errorf("Can't generate preimage: %v", err)
			return
		}
		hashPreImage := fastsha256.Sum256(preimage[:])
		payment := &Payment{
			PaymentID:        msg.InvoiceNumber,
			InitDate:         time.Now(),
			ConfirmationDate: time.Now(),
			Direction:        DirectionOutgoing,
			Amount:           msg.Amount,
			PreImage:         preimage,
			HashPreImage:     hashPreImage,
			State:            StateWaitForAdd,
		}
		p.payments[payment.PaymentID] = payment
		p.hashToPaymentID[hashPreImage] = payment.PaymentID
		respMsg := &lnwire.PaymentInitiationConfirmation{
			InvoiceNumber:  payment.PaymentID,
			RedemptionHash: hashPreImage,
		}
		respPaymentPkt := &PaymentPacket{
			dst: paymentPkt.src,
			msg: respMsg,
		}
		p.chPaymentOut <- respPaymentPkt
		peerLog.Info("Invoice added for amount: %v and preimage %v", msg.Amount, preimage)
		p.invoices.addInvoice(btcutil.Amount(msg.Amount), preimage)
	case *lnwire.PaymentInitiationConfirmation:
		// We got confirmation for our request
		// 1. Check if we actually send it
		// 2. Create new HTLCAdd request and send it
		// 3. Change status of our payment
		payment, ok := p.payments[msg.InvoiceNumber]
		if !ok {
			peerLog.Errorf("Received *lnwire.PaymentInitiationConfirmation for not existing PaymentID: %v", msg.InvoiceNumber)
			return
		}
		if payment.State != StateWaitForConfirmation {
			peerLog.Errorf("Received *lnwire.PaymentInitiationConfirmation when in state %v. Need to be in state %v", payment.State, StateWaitForConfirmation)
			return
		}
		payment.HashPreImage = msg.RedemptionHash
		payment.ConfirmationDate = time.Now()
		payment.State = StateWaitForSettlement
		p.hashToPaymentID[payment.HashPreImage] = payment.PaymentID
		onionBlob := make([]byte, 0)
		for i := 1; i < len(payment.Path); i++ {
			onionBlob = append(onionBlob, payment.Path[i][:]...)
		}
		htlcAdd := &lnwire.HTLCAddRequest{
			Amount:           payment.Amount,
			RedemptionHashes: [][32]byte{payment.HashPreImage},
			OnionBlob:        onionBlob,
		}
		htlcPkt := &htlcPacket{
			dest: wire.ShaHash(payment.Path[0]),
			amt:  btcutil.Amount(payment.Amount),
			msg:  htlcAdd,
		}
		p.chHTLCOut <- htlcPkt
	default:
		peerLog.Errorf("Got message of unsupported type %T: %v", paymentPkt.msg, paymentPkt.msg)
	}
}

func (p *PaymentManager) handleHTLCPacket(htlcPkt *htlcPacket) {
	switch msg := htlcPkt.msg.(type) {
	case *lnwire.HTLCAddRequest:
		// We got an incoming payment, check if
		// 1. It is out payment. Validate amount. Send HTLCSettle. Update status
		paymentID, ok := p.hashToPaymentID[msg.RedemptionHashes[0]]
		if !ok {
			peerLog.Errorf("Got an unxexpected *lnwire.HTLCAddRequest")
			return
		}
		payment := p.payments[paymentID]
		if payment.State != StateWaitForAdd {
			peerLog.Errorf("Got *lnwire.HTLCAddRequest but payment state is %v, want %v", payment.State, StateWaitForAdd)
			return
		}
		payment.State = StateDone
		// TODO(mkl): check that amount is correct
		// TODO(mkl): generate settle request
	case *lnwire.HTLCSettleRequest:
		// Our payment reach it's destination
		// Validate if it is really ours
		// Update status
		if len(msg.RedemptionProofs) != 1{
			peerLog.Errorf("Gor incorrect number of redemption proofs: %v, want %v", len(msg.RedemptionProofs), 1)
			return
		}
		hashPreImage := fastsha256.Sum256(msg.RedemptionProofs[0][:])
		paymentID, ok := p.hashToPaymentID[hashPreImage]
		if !ok {
			peerLog.Errorf("Got an unxexpected *lnwire.HTLCSettleRequest")
			return
		}
		payment := p.payments[paymentID]
		if payment.State != StateWaitForSettlement {
			peerLog.Errorf("Got *lnwire.HTLCSettleRequest but payment state is %v, want %v", payment.State, StateWaitForSettlement)
			return
		}
		payment.State = StateDone
	}
}

// Initialises payment sending
func (p *PaymentManager) SendPayment(amount lnwire.CreditsAmount, path [][32]byte) error {
	msg := &sendPaymentCmd{
		amount: amount,
		path: path,
		err: make(chan error, 1),
	}
	p.chCmd <- msg
	return <-msg.err
}