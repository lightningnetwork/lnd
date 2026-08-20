package channeldb

import (
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
)

// SaveInitialForwardingPolicy saves the serialized forwarding policy for the
// provided permanent channel id to the initialChannelForwardingPolicyBucket.
func (c *ChannelStateDB) SaveInitialForwardingPolicy(chanID lnwire.ChannelID,
	forwardingPolicy *models.ForwardingPolicy) error {

	return c.kvStore.SaveInitialForwardingPolicy(chanID, forwardingPolicy)
}

// GetInitialForwardingPolicy fetches the serialized forwarding policy for the
// provided channel id from the database, or returns ErrChannelNotFound if
// a forwarding policy for this channel id is not found.
func (c *ChannelStateDB) GetInitialForwardingPolicy(
	chanID lnwire.ChannelID) (*models.ForwardingPolicy, error) {

	return c.kvStore.GetInitialForwardingPolicy(chanID)
}

// DeleteInitialForwardingPolicy removes the forwarding policy for a given
// channel from the database.
func (c *ChannelStateDB) DeleteInitialForwardingPolicy(
	chanID lnwire.ChannelID) error {

	return c.kvStore.DeleteInitialForwardingPolicy(chanID)
}
