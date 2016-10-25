package channeldb

import "fmt"

var (
	ErrNoChanDBExists = fmt.Errorf("channel db has not yet been created")

	ErrNoActiveChannels = fmt.Errorf("no active channels exist")
	ErrChannelNoExist   = fmt.Errorf("this channel does not exist")
	ErrNoPastDeltas     = fmt.Errorf("channel has no recorded deltas")

	ErrInvoiceNotFound   = fmt.Errorf("unable to locate invoice")
	ErrNoInvoicesCreated = fmt.Errorf("there are no existing invoices")
	ErrDuplicateInvoice  = fmt.Errorf("invoice with payment hash already exists")

	ErrNodeNotFound = fmt.Errorf("link node with target identity not found")
)
