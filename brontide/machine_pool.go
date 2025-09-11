package brontide

import (
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

// machinePool is a pool of reusable Machine instances that have their
// large buffers pre-allocated. This reduces GC pressure by reusing the
// ~64KB of buffers per connection.
var machinePool = sync.Pool{
	New: func() interface{} {
		// Create a new Machine with pre-allocated buffers.
		// We don't initialize the handshake state here as it
		// needs connection-specific parameters.
		return &Machine{
			ephemeralGen: ephemeralGen,
		}
	},
}

// getMachineFromPool retrieves a Machine from the pool and initializes it
// for a new connection. This replaces NewBrontideMachine when using pooling.
func getMachineFromPool(initiator bool, localKey keychain.SingleKeyECDH,
	remotePub *btcec.PublicKey, options ...func(*Machine)) *Machine {

	// Get a Machine from the pool.
	m := machinePool.Get().(*Machine)

	// Initialize the handshake state for this connection.
	m.handshakeState = newHandshakeState(
		initiator, lightningPrologue, localKey, remotePub,
	)

	// Reset ephemeral generator to default (can be overridden by options).
	m.ephemeralGen = ephemeralGen

	// Apply any custom options.
	for _, option := range options {
		option(m)
	}

	return m
}

// Reset clears all sensitive cryptographic state from the Machine,
// preparing it for reuse. This MUST be called before returning the
// Machine to the pool to prevent data leaks between connections.
func (m *Machine) Reset() {
	// Clear cipher states.
	m.sendCipher.Reset()
	m.recvCipher.Reset()

	// Clear handshake state.
	m.handshakeState.Reset()

	// Clear the large buffers - we zero the header portions that might
	// contain sensitive data, but can skip the body buffers as they're
	// overwritten before use.
	for i := range m.nextCipherHeader {
		m.nextCipherHeader[i] = 0
	}
	for i := range m.nextHeaderBuffer {
		m.nextHeaderBuffer[i] = 0
	}
	for i := range m.pktLenBuffer {
		m.pktLenBuffer[i] = 0
	}

	// Reset the buffer slices.
	m.nextHeaderSend = nil
	m.nextBodySend = nil

	// Note: We don't need to zero nextBodyBuffer as it's always
	// overwritten before use and doesn't contain key material.
}

// ReturnMachineToPool resets the Machine and returns it to the pool for reuse.
// This should be called when a connection is closed.
func ReturnMachineToPool(m *Machine) {
	if m == nil {
		return
	}

	// Reset all state before returning to pool.
	m.Reset()

	// Return to pool for reuse.
	machinePool.Put(m)
}

// Reset clears all sensitive state from the handshakeState.
func (h *handshakeState) Reset() {
	// Clear symmetric state.
	h.symmetricState.Reset()

	// Clear key references (but don't zero the actual keys as they
	// might be shared).
	h.localStatic = nil
	h.localEphemeral = nil
	h.remoteStatic = nil
	h.remoteEphemeral = nil
	h.initiator = false
}

// Reset clears all sensitive state from the symmetricState.
func (s *symmetricState) Reset() {
	// Clear the cipher state.
	s.cipherState.Reset()

	// Zero out sensitive key material.
	for i := range s.chainingKey {
		s.chainingKey[i] = 0
	}
	for i := range s.tempKey {
		s.tempKey[i] = 0
	}
	for i := range s.handshakeDigest {
		s.handshakeDigest[i] = 0
	}
}

// Reset clears all sensitive state from the cipherState.
func (c *cipherState) Reset() {
	// Zero out the secret key.
	for i := range c.secretKey {
		c.secretKey[i] = 0
	}

	// Zero out the salt.
	for i := range c.salt {
		c.salt[i] = 0
	}

	// Reset the nonce counter.
	c.nonce = 0

	// Zero out the nonce buffer.
	for i := range c.nonceBuffer {
		c.nonceBuffer[i] = 0
	}

	// Clear the cipher instance.
	c.cipher = nil
}