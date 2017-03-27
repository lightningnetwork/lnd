package lnwallet

import (
	"fmt"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
)

// MessageSigner is used for creation the signatures using the node identity key.
// By message we mean the whole range of data that might require our approve,
// starting from node, channel, channel update announcements and ending by user
// data.
type MessageSigner struct {
	identityKey *btcec.PrivateKey
}

// NewMessageSigner returns the new instance of message signer.
func NewMessageSigner(key *btcec.PrivateKey) *MessageSigner {
	return &MessageSigner{
		identityKey: key,
	}
}

// SignData sign the message with the node private key.
func (s *MessageSigner) SignData(data []byte) (*btcec.Signature, error) {
	sign, err := s.identityKey.Sign(chainhash.DoubleHashB(data))
	if err != nil {
		return nil, fmt.Errorf("can't sign the message: %v", err)
	}

	return sign, nil
}
