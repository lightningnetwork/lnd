// +build invoicesrpc

package invoicesrpc

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
)

// createNewSubServer is a helper method that will create the new sub server
// given the main config dispatcher method. If we're unable to find the config
// that is meant for us in the config dispatcher, then we'll exit with an
// error.
func createNewSubServer(configRegistry lnrpc.SubServerConfigDispatcher) (
	lnrpc.SubServer, lnrpc.MacaroonPerms, error) {

	// We'll attempt to look up the config that we expect, according to our
	// subServerName name. If we can't find this, then we'll exit with an
	// error, as we're unable to properly initialize ourselves without this
	// config.
	subServerConf, ok := configRegistry.FetchConfig(subServerName)
	if !ok {
		return nil, nil, fmt.Errorf("unable to find config for "+
			"subserver type %s", subServerName)
	}

	// Now that we've found an object mapping to our service name, we'll
	// ensure that it's the type we need.
	config, ok := subServerConf.(*Config)
	if !ok {
		return nil, nil, fmt.Errorf("wrong type of config for "+
			"subserver %s, expected %T got %T", subServerName,
			&Config{}, subServerConf)
	}

	return New(config)
}

func init() {
	subServer := &lnrpc.SubServerDriver{
		SubServerName: subServerName,
		New: func(c lnrpc.SubServerConfigDispatcher) (lnrpc.SubServer,
			lnrpc.MacaroonPerms, error) {
			return createNewSubServer(c)
		},
	}

	// If the build tag is active, then we'll register ourselves as a
	// sub-RPC server within the global lnrpc package namespace.
	if err := lnrpc.RegisterSubServer(subServer); err != nil {
		panic(fmt.Sprintf("failed to register sub server driver "+
			"'%s': %v", subServerName, err))
	}
}
