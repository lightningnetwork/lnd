package onion_message

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/msgmux"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// defaultOnionMessageQueueSize is the default size of the onion
	// message queue.
	defaultOnionMessageQueueSize = 50

	// defaultMinRedThreshold is the default minimum threshold for the Random
	// Early Drop queue. It is set to 80% of the queue size.
	defaultMinRedThreshold = 40
)

var (
	// ErrBadMessage is returned when we can't process an onion message.
	ErrBadMessage = errors.New("onion message processing failed")

	// ErrBadOnionMsg is returned when we receive a bad onion message.
	ErrBadOnionMsg = errors.New("invalid onion message")

	// ErrBadOnionBlob is returned when we receive a bad onion blob within
	// our onion message.
	ErrBadOnionBlob = errors.New("invalid onion blob")

	// ErrNoForwardingOnion is returned when we try to forward an onion
	// message but no next onion is provided.
	ErrNoForwardingOnion = errors.New("no next onion provided to forward")

	// ErrNoEncryptedData is returned when the encrypted data TLV is not
	// present when it is required.
	ErrNoEncryptedData = errors.New("encrypted data blob required")

	// ErrNoForwardingPayload is returned when no onion message payload
	// is provided to allow forwarding messages.
	ErrNoForwardingPayload = errors.New("no payload provided for " +
		"forwarding")

	// ErrNoNextNodeID is returned when we require a next node id in our
	// encrypted data blob and one was not provided.
	ErrNoNextNodeID = errors.New("next node ID required")
)

// OnionMessageSender is a function type that defines how to send an onion
// message. It takes the next node's public key (as [33]byte), the blinding
// point (*btcec.PublicKey), and the onion packet ([]byte) to send to a peer.
type OnionMessageSender func([33]byte, *btcec.PublicKey, []byte) error

// OnionMessageUpdate is onion message update dispatched to any potential
// subscriber.
type OnionMessageUpdate struct {
	// Peer is the peer pubkey
	Peer [33]byte

	// BlindingPoint is the route blinding ephemeral pubkey to be used for
	// the onion message.
	BlindingPoint []byte

	// OnionBlob is the raw serialized mix header used to relay messages in
	// a privacy-preserving manner. This blob should be handled in the same
	// manner as onions used to route HTLCs, with the exception that it uses
	// blinded routes by default.
	OnionBlob []byte

	CustomRecords record.CustomSet

	ReplyPath *lnwire.ReplyPath

	EncryptedRecipientData []byte
}

// OnionEndpointOption defines a function that can be used to configure
// an OnionEndpoint. This allows for flexible configuration of the endpoint
// when creating a new instance.
type OnionEndpointOption func(*OnionEndpoint)

// WithMessageServer sets the subscribe.Server for the OnionEndpoint.
func WithMessageServer(server *subscribe.Server) OnionEndpointOption {
	return func(o *OnionEndpoint) {
		o.onionMessageServer = server
	}
}

// WithMessageSender sets the onion message sender for the OnionEndpoint.
func WithMessageSender(msgSender OnionMessageSender) OnionEndpointOption {
	return func(o *OnionEndpoint) {
		o.MsgSender = msgSender
	}
}

// WithOnionProcessor sets the onion processor for the OnionEndpoint.
func WithOnionProcessor(processor *hop.OnionProcessor) OnionEndpointOption {
	return func(o *OnionEndpoint) {
		o.onionProcessor = processor
	}
}

// OnionEndpoint handles incoming onion messages.
type OnionEndpoint struct {
	// subscribe.Server is used for subscriptions to onion messages.
	onionMessageServer *subscribe.Server

	// onionProcessor is the onion processor used to process onion packets.
	onionProcessor *hop.OnionProcessor

	// MsgSender sends a onion message to the target peer.
	MsgSender OnionMessageSender

	// onionMsgQueue is a queue that contains incoming onion messages.
	onionMsgQueue *queue.BackpressureQueue[msgmux.PeerMsg]

	// quit is a channel that is closed when the endpoint is shutting down.
	quit chan struct{}

	// wg is a wait group that is used to wait for the message handler to
	// exit.
	wg sync.WaitGroup
}

// A compile-time check to ensure OnionEndpoint implements the Endpoint
// interface.
var _ msgmux.Endpoint = (*OnionEndpoint)(nil)

// NewOnionEndpoint creates a new OnionEndpoint with the given options.
func NewOnionEndpoint(opts ...OnionEndpointOption) *OnionEndpoint {
	// By default, we will drop onion messages if the queue is full.
	dropPredicate := queue.RandomEarlyDrop[msgmux.PeerMsg](
		defaultMinRedThreshold, defaultOnionMessageQueueSize,
	)

	o := &OnionEndpoint{
		onionMessageServer: nil,
		onionMsgQueue: queue.NewBackpressureQueue[msgmux.PeerMsg](
			defaultOnionMessageQueueSize, dropPredicate,
		),
		quit: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Name returns the unique name of the endpoint.
func (o *OnionEndpoint) Name() string {
	return "OnionMessageHandler"
}

// CanHandle checks if the endpoint can handle the incoming message.
// It returns true if the message is an lnwire.OnionMessage.
func (o *OnionEndpoint) CanHandle(msg msgmux.PeerMsg) bool {
	_, ok := msg.Message.(*lnwire.OnionMessage)
	return ok
}

// SendMessage processes the incoming onion message. It returns true if the
// message was successfully processed.
func (o *OnionEndpoint) SendMessage(ctx context.Context,
	msg msgmux.PeerMsg) bool {

	_, ok := msg.Message.(*lnwire.OnionMessage)
	if !ok {
		return false
	}

	err := o.onionMsgQueue.Enqueue(ctx, msg)
	if err != nil {
		if errors.Is(err, queue.ErrQueueFullAndDropped) {
			log.Warnf("Onion message queue full, dropping message")
		} else {
			log.Errorf("Failed to enqueue onion message: %v", err)
		}
	}

	return true
}

// Start starts the onion message handler.
func (o *OnionEndpoint) Start() {
	o.wg.Add(1)
	go o.messageHandler()
}

// Stop stops the onion message handler.
func (o *OnionEndpoint) Stop() {
	close(o.quit)
	o.wg.Wait()
}

// messageHandler is the main goroutine that processes onion messages from the
// queue.
func (o *OnionEndpoint) messageHandler() {
	defer o.wg.Done()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-o.quit
		cancel()
	}()

	for {
		result := o.onionMsgQueue.Dequeue(ctx)

		if result.IsErr() {
			if errors.Is(result.Err(), context.Canceled) {
				return
			}

			log.Errorf("OnionEndpoint Dequeue failed: %v",
				result.Err())
			continue
		}

		result.WhenOk(o.processMessage)
	}
}

// processMessage processes an onion message.
func (o *OnionEndpoint) processMessage(msg msgmux.PeerMsg) {
	onionMsg, ok := msg.Message.(*lnwire.OnionMessage)
	if !ok {
		// This should not happen as we check it before enqueueing.
		return
	}

	peer := msg.PeerPub.SerializeCompressed()
	log.Debugf("OnionEndpoint received OnionMessage from peer %x: "+
		"BlindingPoint=%v, OnionPacket[:10]=%10x...", peer,
		onionMsg.BlindingPoint, onionMsg.OnionBlob)

	payload, err := o.handleOnionMessage(*onionMsg)
	if err != nil {
		log.Errorf("Failed to handle onion message: %v", err)
	}

	var peerArr [33]byte
	copy(peerArr[:], peer)

	update := &OnionMessageUpdate{
		Peer:          peerArr,
		BlindingPoint: onionMsg.BlindingPoint.SerializeCompressed(),
		OnionBlob:     onionMsg.OnionBlob,
	}

	// If we have a payload (no error), add its contents to our update.
	if payload != nil {
		update.CustomRecords = payload.CustomRecords()
		update.ReplyPath = payload.ReplyPath()
		update.EncryptedRecipientData = payload.EncryptedData()
	}

	// Send the update to any subscribers.
	if sendErr := o.onionMessageServer.SendUpdate(update); sendErr != nil {
		log.Errorf("Failed to send onion message update: %v", sendErr)
	}
}

// handleOnionMessage decodes and processes an onion message.
func (o *OnionEndpoint) handleOnionMessage(msg lnwire.OnionMessage) (
	*hop.Payload, error) {
	reqs := []hop.DecodeHopIteratorRequest{
		{
			OnionReader:    bytes.NewReader(msg.OnionBlob),
			RHash:          nil,
			IncomingCltv:   0,
			IncomingAmount: 0,
			BlindingPoint: tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[lnwire.BlindingPointTlvType](
					msg.BlindingPoint,
				),
			),
			IsOnionMessage: true,
		},
	}

	resps, err := o.onionProcessor.DecodeHopIterators(
		nil, reqs, false,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: could not process onion packet: %v",
			ErrBadOnionBlob, err)
	}

	if resps[0].FailCode != lnwire.CodeNone {
		return nil, fmt.Errorf("%w: could not process onion packet, "+
			"fail code: %v", ErrBadOnionBlob, resps[0].FailCode)
	}

	payload, routeRole, err := resps[0].HopIterator.HopPayload()
	if err != nil {
		return nil, fmt.Errorf("%w: could not process onion packet: %v",
			ErrBadOnionBlob, err)
	}

	if payload.IsFinal() {
		return payload, nil
	}

	if routeRole != hop.RouteRoleRelaying {
		return nil, fmt.Errorf("lnd only supports onion messaging " +
			"forwarding at the moment")
	}

	nextBlindingpoint, err := payload.FwdInfo.NextBlinding.UnwrapOrErr(
		fmt.Errorf("no next blinding point provided in onion message"),
	)
	if err != nil {
		return nil, err
	}
	nextNodeID := payload.FwdInfo.NextNodeID
	buf := new(bytes.Buffer)
	if err := resps[0].HopIterator.EncodeNextHop(buf); err != nil {
		return nil, fmt.Errorf("could not encode next packet: %w", err)
	}
	nextPacket := buf.Bytes()

	err = o.forwardMessage(nextNodeID, nextBlindingpoint.Val, nextPacket)
	if err != nil {
		return nil, fmt.Errorf("forwarding onion message failed: %v",
			err)
	}

	return payload, nil
}

func (o *OnionEndpoint) forwardMessage(nextNodeID *btcec.PublicKey,
	nextBlindingPoint *btcec.PublicKey, nextPacket []byte) error {

	var nextNodeIDBytes [33]byte
	copy(nextNodeIDBytes[:], nextNodeID.SerializeCompressed())

	err := o.MsgSender(
		nextNodeIDBytes, nextBlindingPoint, nextPacket,
	)

	if err != nil {
		return fmt.Errorf("could not send message: %w", err)
	}

	return nil
}
