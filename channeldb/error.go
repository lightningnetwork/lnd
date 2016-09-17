package channeldb

import "fmt"

var (
	ErrNoChanDBExists = fmt.Errorf("channel db has not yet been created")

	ErrNoActiveChannels = fmt.Errorf("no active channels exist")
	ErrChannelNoExist   = fmt.Errorf("this channel does not exist")
	ErrNoPastDeltas     = fmt.Errorf("channel has no recorded deltas")

	ErrInvoiceNotFound  = fmt.Errorf("unable to locate invoice")
	ErrDuplicateInvoice = fmt.Errorf("invoice with payment hash already exists")
)
