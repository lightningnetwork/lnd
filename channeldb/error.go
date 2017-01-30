package channeldb

import (
	"errors"
)

var (
	ErrNoChanDBExists    = errors.New("channel db has not yet been created")
	ErrLinkNodesNotFound = errors.New("no link nodes exist")

	ErrNoActiveChannels = errors.New("no active channels exist")
	ErrChannelNoExist   = errors.New("this channel does not exist")
	ErrNoPastDeltas     = errors.New("channel has no recorded deltas")

	ErrInvoiceNotFound   = errors.New("unable to locate invoice")
	ErrNoInvoicesCreated = errors.New("there are no existing invoices")
	ErrDuplicateInvoice  = errors.New("invoice with payment hash already exists")

	ErrNoPaymentsCreated = errors.New("there are no existing payments")

	ErrNodeNotFound = errors.New("link node with target identity not found")
	ErrMetaNotFound = errors.New("unable to locate meta information")

	ErrGraphNotFound      = errors.New("graph bucket not initialized")
	ErrGraphNodesNotFound = errors.New("no graph nodes exist")
	ErrGraphNoEdgesFound  = errors.New("no graph edges exist")
	ErrGraphNodeNotFound  = errors.New("unable to find node")
	ErrGraphNeverPruned   = errors.New("graph never pruned")

	ErrEdgeNotFound = errors.New("edge for chanID not found")

	ErrNodeAliasNotFound = errors.New("alias for node not found")

	ErrSourceNodeNotSet = errors.New("source node does not exist")

	ErrCircuitsNotFound  = errors.New("unable to locate circuits")
	ErrNoCircuitsCreated = errors.New("there are no existing circuits")
	ErrDuplicateCircuit  = errors.New("circuit already exists")
)
