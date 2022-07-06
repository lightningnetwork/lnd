//go:build bolt12

package itest

import (
	bnditest "github.com/gijswijs/boltnd/itest"
	"github.com/lightningnetwork/lnd/lntest"
)

var allTestCases = []*lntest.TestCase{
	{
		Name:     "onion messages",
		TestFunc: bnditest.OnionMessageTestCase,
	},
	{
		Name:     "forward onion messages",
		TestFunc: bnditest.OnionMsgForwardTestCase,
	},
	{
		Name:     "decode offer",
		TestFunc: bnditest.DecodeOfferTestCase,
	},
	{
		Name:     "subscribe onion payload",
		TestFunc: bnditest.SubscribeOnionPayload,
	},
}
