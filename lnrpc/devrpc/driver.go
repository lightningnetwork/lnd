//go:build dev
// +build dev

package devrpc

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
)

// getConfig is a helper method that will fetch the config for sub-server given
// the main config dispatcher method. If we're unable to find the config
// that is meant for us in the config dispatcher, then we'll exit with an
// error. If enforceDependencies is set to true, the function also verifies that
// the dependencies in the config are properly set.
func getConfig(configRegistry lnrpc.SubServerConfigDispatcher,
	enforceDependencies bool) (*Config, error) {

	// We'll attempt to look up the config that we expect, according to our
	// subServerName name. If we can't find this, then we'll exit with an
	// error, as we're unable to properly initialize ourselves without this
	// config.
	subServerConf, ok := configRegistry.FetchConfig(subServerName)
	if !ok {
		return nil, fmt.Errorf("unable to find config for subserver "+
			"type %s", subServerName)
	}

	// Now that we've found an object mapping to our service name, we'll
	// ensure that it's the type we need.
	config, ok := subServerConf.(*Config)
	if !ok {
		return nil, fmt.Errorf("wrong type of config for subserver "+
			"%s, expected %T got %T", subServerName, &Config{},
			subServerConf)
	}

	if enforceDependencies {
		if err := verifyDependencies(config); err != nil {
			return nil, err
		}
	}

	return config, nil
}

// verifyDependencies ensures that the dependencies in the config are properly
// set.
func verifyDependencies(config *Config) error {
	// Before we try to make the new service instance, we'll perform
	// some sanity checks on the arguments to ensure that they're useable.
	if config.ActiveNetParams == nil {
		return fmt.Errorf("ActiveNetParams must be set to create " +
			"DevRPC")
	}

	if config.GraphDB == nil {
		return fmt.Errorf("GraphDB must be set to create DevRPC")
	}

	return nil
}

func init() {
	subServer := &lnrpc.SubServerDriver{
		SubServerName: subServerName,
		NewGrpcHandler: func() lnrpc.GrpcHandler {
			return &ServerShell{}
		},
	}

	// If the build tag is active, then we'll register ourselves as a
	// sub-RPC server within the global lnrpc package namespace.
	if err := lnrpc.RegisterSubServer(subServer); err != nil {
		panic(fmt.Sprintf("failed to register sub server driver "+
			"'%s': %v", subServerName, err))
	}
}
