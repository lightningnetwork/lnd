package main

import "github.com/lightningnetwork/lnd/lnwire"

// globalFeatures feature vector which affects HTLCs and thus are also
// advertised to other nodes.
var globalFeatures = lnwire.NewFeatureVector([]lnwire.Feature{})

// localFeatures is an feature vector which represent the features which
// only affect the protocol between these two nodes.
var localFeatures = lnwire.NewFeatureVector([]lnwire.Feature{
	{
		Name: "new-ping-and-funding",
		Flag: lnwire.RequiredFlag,
	},
	{
		Name: "node-ann-feature-addr-swap",
		Flag: lnwire.RequiredFlag,
	},
	{
		Name: "dynamic-fees",
		Flag: lnwire.RequiredFlag,
	},
	{
		Name: "shutdown-close-flow",
		Flag: lnwire.RequiredFlag,
	},
	{
		Name: "sphinx-payload",
		Flag: lnwire.RequiredFlag,
	},
	{
		Name: "htlc-dust-accounting",
		Flag: lnwire.RequiredFlag,
	},
	{
		Name: "encrypted-errors",
		Flag: lnwire.RequiredFlag,
	},
})
