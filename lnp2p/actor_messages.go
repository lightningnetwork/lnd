package lnp2p

import (
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// PeerMessage is the base interface for peer actor messages.
type PeerMessage interface {
	actor.Message

	peerMessage()
}

// PeerResponse is the base interface for peer actor responses.
type PeerResponse interface {
	peerResponse()
}

// basePeerMessage embeds BaseMessage and implements peerMessage.
type basePeerMessage struct {
	actor.BaseMessage
}

func (basePeerMessage) peerMessage() {}

// basePeerResponse implements peerResponse.
type basePeerResponse struct{}

func (basePeerResponse) peerResponse() {}

// ConnectRequest requests establishing a new connection.
type ConnectRequest struct {
	basePeerMessage

	// Target is the node to connect to (optional if already configured).
	Target fn.Option[NodeAddress]

	// Timeout for the connection attempt.
	Timeout time.Duration
}

// MessageType returns the message type name.
func (m *ConnectRequest) MessageType() string {
	return "ConnectRequest"
}

// ConnectResponse is the response to a connection request.
type ConnectResponse struct {
	basePeerResponse

	// Success indicates if the connection was initiated.
	Success bool

	// Future that completes when connection is established.
	Future actor.Future[ConnectionResult]

	// RemotePubKey is set if already connected.
	RemotePubKey *btcec.PublicKey

	// AlreadyConnected indicates the peer was already connected.
	AlreadyConnected bool
}

// DisconnectRequest requests disconnecting from the peer.
type DisconnectRequest struct {
	basePeerMessage

	// Reason for disconnection.
	Reason string
}

// MessageType returns the message type name.
func (m *DisconnectRequest) MessageType() string {
	return "DisconnectRequest"
}

// DisconnectResponse is the response to a disconnection request.
type DisconnectResponse struct {
	basePeerResponse

	// Success indicates if disconnection was successful.
	Success bool

	// Error contains any disconnection error.
	Error error
}

// SendMessageRequest requests sending a message to the peer.
type SendMessageRequest struct {
	basePeerMessage

	// Message to send.
	Message lnwire.Message
}

// MessageType returns the message type name.
func (m *SendMessageRequest) MessageType() string {
	return "SendMessageRequest"
}

// SendMessageResponse is the response to a send message request.
type SendMessageResponse struct {
	basePeerResponse

	// Success indicates if the message was sent.
	Success bool

	// Error contains any send error.
	Error error
}

// MessageReceived notifies about a received message from the peer. This is sent
// to all registered ServiceKeys when a message is received.
type MessageReceived struct {
	basePeerMessage

	// Message that was received.
	Message lnwire.Message

	// From is the peer's public key.
	From *btcec.PublicKey

	// ReceivedAt is when the message was received.
	ReceivedAt time.Time
}

// MessageType returns the message type name.
func (m *MessageReceived) MessageType() string {
	return "MessageReceived"
}

// MessageReceivedAck acknowledges receipt of a message.
type MessageReceivedAck struct {
	basePeerResponse

	// Processed indicates if the message was processed.
	Processed bool

	// Response is an optional response message to send back.
	Response fn.Option[lnwire.Message]
}

// ConnectionStateChange notifies about connection state changes. This is sent
// to all registered ServiceKeys when the connection state changes.
type ConnectionStateChange struct {
	basePeerMessage

	// State is the new connection state.
	State ConnectionState

	// PreviousState is the previous connection state.
	PreviousState ConnectionState

	// Error contains any error associated with the state change.
	Error error

	// Timestamp is when the state changed.
	Timestamp time.Time
}

// MessageType returns the message type name.
func (m *ConnectionStateChange) MessageType() string {
	return "ConnectionStateChange"
}

// ConnectionStateAck acknowledges a connection state change.
type ConnectionStateAck struct {
	basePeerResponse
}

// PeerError notifies about a peer error. This is sent to all registered
// ServiceKeys when an error occurs.
type PeerError struct {
	basePeerMessage

	// Error that occurred.
	Error error

	// ErrorType categorizes the error.
	ErrorType ErrorType

	// From is the peer's public key.
	From *btcec.PublicKey

	// Timestamp is when the error occurred.
	Timestamp time.Time
}

// MessageType returns the message type name.
func (m *PeerError) MessageType() string {
	return "PeerError"
}

// ErrorType categorizes peer errors.
type ErrorType int

const (
	// ErrorTypeConnection indicates a connection error.
	ErrorTypeConnection ErrorType = iota

	// ErrorTypeProtocol indicates a protocol violation.
	ErrorTypeProtocol

	// ErrorTypeMessage indicates a message handling error.
	ErrorTypeMessage

	// ErrorTypeTimeout indicates a timeout occurred.
	ErrorTypeTimeout

	// ErrorTypeRemote indicates the remote peer sent an error.
	ErrorTypeRemote
)

// PeerErrorAck acknowledges a peer error notification.
type PeerErrorAck struct {
	basePeerResponse

	// ShouldDisconnect indicates if the peer should be disconnected.
	ShouldDisconnect bool
}

// PeerWarning notifies about a peer warning. This is sent to all registered
// ServiceKeys for non-critical issues.
type PeerWarning struct {
	basePeerMessage

	// Warning message.
	Warning string

	// From is the peer's public key.
	From *btcec.PublicKey

	// Timestamp is when the warning occurred.
	Timestamp time.Time
}

// MessageType returns the message type name.
func (m *PeerWarning) MessageType() string {
	return "PeerWarning"
}

// PeerWarningAck acknowledges a peer warning.
type PeerWarningAck struct {
	basePeerResponse
}

// GetStatusRequest requests the current peer status.
type GetStatusRequest struct {
	basePeerMessage
}

// MessageType returns the message type name.
func (m *GetStatusRequest) MessageType() string {
	return "GetStatusRequest"
}

// StatusResponse contains the current peer status.
type StatusResponse struct {
	basePeerResponse

	// IsConnected indicates if the peer is connected.
	IsConnected bool

	// RemotePubKey is the remote peer's public key.
	RemotePubKey *btcec.PublicKey

	// LocalPubKey is our public key.
	LocalPubKey *btcec.PublicKey

	// RemoteAddr is the remote address.
	RemoteAddr string

	// LocalAddr is the local address.
	LocalAddr string

	// MessageCount is the number of messages processed.
	MessageCount uint64

	// ConnectedTime is when the connection was established.
	ConnectedTime time.Time

	// LastError is the most recent error.
	LastError error
}

// WaitForConnectionRequest requests a future that completes when connected.
type WaitForConnectionRequest struct {
	basePeerMessage

	// Timeout for waiting (optional).
	Timeout fn.Option[time.Duration]
}

// MessageType returns the message type name.
func (m *WaitForConnectionRequest) MessageType() string {
	return "WaitForConnectionRequest"
}

// WaitForConnectionResponse contains a future for connection completion.
type WaitForConnectionResponse struct {
	basePeerResponse

	// Future that completes when connected.
	Future actor.Future[ConnectionResult]

	// AlreadyConnected indicates if already connected.
	AlreadyConnected bool
}

// AddServiceKeyRequest requests adding a new service key for message
// distribution.
type AddServiceKeyRequest struct {
	basePeerMessage

	// MessageSink contains the service key and optional filter to add.
	MessageSink *MessageSink
}

// MessageType returns the message type name.
func (m *AddServiceKeyRequest) MessageType() string {
	return "AddServiceKeyRequest"
}

// AddServiceKeyResponse is the response to adding a service key.
type AddServiceKeyResponse struct {
	basePeerResponse

	// Success indicates if the key was added.
	Success bool

	// AlreadyExists indicates the key was already registered.
	AlreadyExists bool

	// CurrentKeyCount is the total number of registered keys.
	CurrentKeyCount int
}

// RemoveServiceKeyRequest requests removing a service key from message
// distribution.
type RemoveServiceKeyRequest struct {
	basePeerMessage

	// ServiceKey to remove.
	ServiceKey PeerServiceKey
}

// MessageType returns the message type name.
func (m *RemoveServiceKeyRequest) MessageType() string {
	return "RemoveServiceKeyRequest"
}

// RemoveServiceKeyResponse is the response to removing a service key.
type RemoveServiceKeyResponse struct {
	basePeerResponse

	// Success indicates if the key was removed.
	Success bool

	// NotFound indicates the key was not registered.
	NotFound bool

	// CurrentKeyCount is the total number of registered keys.
	CurrentKeyCount int
}

// GetServiceKeysRequest requests the current list of service keys.
type GetServiceKeysRequest struct {
	basePeerMessage
}

// MessageType returns the message type name.
func (m *GetServiceKeysRequest) MessageType() string {
	return "GetServiceKeysRequest"
}

// GetServiceKeysResponse contains the current list of service keys.
type GetServiceKeysResponse struct {
	basePeerResponse

	// ServiceKeys are the currently registered keys.
	ServiceKeys []PeerServiceKey

	// ActorCount is the total number of resolved actors.
	ActorCount int
}

// GetMessageSinksRequest requests the current list of message sinks.
type GetMessageSinksRequest struct {
	basePeerMessage
}

// MessageType returns the message type name.
func (m *GetMessageSinksRequest) MessageType() string {
	return "GetMessageSinksRequest"
}

// GetMessageSinksResponse contains the current list of message sinks.
type GetMessageSinksResponse struct {
	basePeerResponse

	// MessageSinks are the currently registered sinks.
	MessageSinks []*MessageSink
}
