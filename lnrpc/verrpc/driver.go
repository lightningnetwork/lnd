package verrpc

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
)

func init() {
	subServer := &lnrpc.SubServerDriver{
		SubServerName: subServerName,
		New: func(c lnrpc.SubServerConfigDispatcher) (lnrpc.SubServer,
			lnrpc.MacaroonPerms, error) {

			return &Server{}, macPermissions, nil
		},
	}

	// We'll register ourselves as a sub-RPC server within the global lnrpc
	// package namespace.
	if err := lnrpc.RegisterSubServer(subServer); err != nil {
		panic(fmt.Sprintf("failed to register sub server driver '%s': %v",
			subServerName, err))
	}
}
