package netann

import (
	"fmt"

	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// SignAnnouncement signs any type of gossip message that is announced on the
// network.
func SignAnnouncement(signer lnwallet.MessageSigner, keyLoc keychain.KeyLocator,
	msg lnwire.Message) (input.Signature, error) {

	var (
		data []byte
		err  error
	)

	switch m := msg.(type) {
	case *lnwire.ChannelAnnouncement1:
		data, err = m.DataToSign()
	case *lnwire.ChannelUpdate1:
		data, err = m.DataToSign()
	case *lnwire.NodeAnnouncement1:
		data, err = m.DataToSign()
	default:
		return nil, fmt.Errorf("can't sign %T message", m)
	}
	if err != nil {
		return nil, fmt.Errorf("unable to get data to sign: %w", err)
	}

	return signer.SignMessage(keyLoc, data, true)
}
