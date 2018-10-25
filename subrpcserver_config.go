package main

import (
	"fmt"
	"reflect"

	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/macaroons"
)

// subRPCServerConfigs is special sub-config in the main configuration that
// houses the configuration for the optional sub-servers. These sub-RPC servers
// are meant to house experimental new features that may eventually make it
// into the main RPC server that lnd exposes. Special methods are present on
// this struct to allow the main RPC server to create and manipulate the
// sub-RPC servers in a generalized manner.
type subRPCServerConfigs struct {
	// SignRPC is a sub-RPC server that exposes signing of arbitrary inputs
	// as a gRPC service.
	SignRPC *signrpc.Config `group:"signrpc" namespace:"signrpc"`

	// WalletKitRPC is a sub-RPC server that exposes functionality allowing
	// a client to send transactions through a wallet, publish them, and
	// also requests keys and addresses under control of the backing
	// wallet.
	WalletKitRPC *walletrpc.Config `group:"walletrpc" namespace:"walletrpc"`
}

// PopulateDependencies attempts to iterate through all the sub-server configs
// within this struct, and populate the items it requires based on the main
// configuration file, and the chain control.
//
// NOTE: This MUST be called before any callers are permitted to execute the
// FetchConfig method.
func (s *subRPCServerConfigs) PopulateDependencies(cc *chainControl,
	networkDir string, macService *macaroons.Service) error {

	// First, we'll use reflect to obtain a version of the config struct
	// that allows us to programmatically inspect its fields.
	selfVal := extractReflectValue(s)
	selfType := selfVal.Type()

	numFields := selfVal.NumField()
	for i := 0; i < numFields; i++ {
		field := selfVal.Field(i)
		fieldElem := field.Elem()
		fieldName := selfType.Field(i).Name

		ltndLog.Debugf("Populating dependencies for sub RPC "+
			"server: %v", fieldName)

		// If this sub-config doesn't actually have any fields, then we
		// can skip it, as the build tag for it is likely off.
		if fieldElem.NumField() == 0 {
			continue
		}
		if !fieldElem.CanSet() {
			continue
		}

		switch cfg := field.Interface().(type) {
		case *signrpc.Config:
			subCfgValue := extractReflectValue(cfg)

			subCfgValue.FieldByName("MacService").Set(
				reflect.ValueOf(macService),
			)
			subCfgValue.FieldByName("NetworkDir").Set(
				reflect.ValueOf(networkDir),
			)
			subCfgValue.FieldByName("Signer").Set(
				reflect.ValueOf(cc.signer),
			)

		case *walletrpc.Config:
			subCfgValue := extractReflectValue(cfg)

			subCfgValue.FieldByName("NetworkDir").Set(
				reflect.ValueOf(networkDir),
			)
			subCfgValue.FieldByName("MacService").Set(
				reflect.ValueOf(macService),
			)
			subCfgValue.FieldByName("FeeEstimator").Set(
				reflect.ValueOf(cc.feeEstimator),
			)
			subCfgValue.FieldByName("Wallet").Set(
				reflect.ValueOf(cc.wallet),
			)
			subCfgValue.FieldByName("KeyRing").Set(
				reflect.ValueOf(cc.keyRing),
			)

		default:
			return fmt.Errorf("unknown field: %v, %T", fieldName,
				cfg)
		}
	}

	return nil
}

// FetchConfig attempts to locate an existing configuration file mapped to the
// target sub-server. If we're unable to find a config file matching the
// subServerName name, then false will be returned for the second parameter.
//
// NOTE: Part of the lnrpc.SubServerConfigDispatcher interface.
func (s *subRPCServerConfigs) FetchConfig(subServerName string) (interface{}, bool) {
	// First, we'll use reflect to obtain a version of the config struct
	// that allows us to programmatically inspect its fields.
	selfVal := extractReflectValue(s)

	// Now that we have the value of the struct, we can check to see if it
	// has an attribute with the same name as the subServerName.
	configVal := selfVal.FieldByName(subServerName)

	// We'll now ensure that this field actually exists in this value. If
	// not, then we'll return false for the ok value to indicate to the
	// caller that this field doesn't actually exist.
	if !configVal.IsValid() {
		return nil, false
	}

	configValElem := configVal.Elem()

	// If a config of this type is found, it doesn't have any fields, then
	// it's the same as if it wasn't present. This can happen if the build
	// tag for the sub-server is inactive.
	if configValElem.NumField() == 0 {
		return nil, false
	}

	// At this pint, we know that the field is actually present in the
	// config struct, so we can return it directly.
	return configVal.Interface(), true
}

// extractReflectValue attempts to extract the value from an interface using
// the reflect package. The resulting reflect.Value allows the caller to
// programmatically examine and manipulate the underlying value.
func extractReflectValue(instance interface{}) reflect.Value {
	var val reflect.Value

	// If the type of the instance is a pointer, then we need to deference
	// the pointer one level to get its value. Otherwise, we can access the
	// value directly.
	if reflect.TypeOf(instance).Kind() == reflect.Ptr {
		val = reflect.ValueOf(instance).Elem()
	} else {
		val = reflect.ValueOf(instance)
	}

	return val
}
