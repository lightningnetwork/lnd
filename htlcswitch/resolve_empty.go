// +build noresolver

package htlcswitch

import (
	"github.com/lightningnetwork/lnd/lnwallet"
)

func isResolverActive() bool {
	return false
}

func asyncResolve(pd *lnwallet.PaymentDescriptor, l *channelLink, obfuscator ErrorEncrypter) {

}
