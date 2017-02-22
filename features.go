package main

import "github.com/lightningnetwork/lnd/lnwire"

// globalFeatures feature vector which affects HTLCs and thus are also
// advertised to other nodes.
var globalFeatures = lnwire.NewFeatureVector([]lnwire.Feature{})

// localFeatures is an feature vector which represent the features which
// only affect the protocol between these two nodes.
var localFeatures = lnwire.NewFeatureVector([]lnwire.Feature{
	{Name: "lcp-stop-and-wait", Flag: lnwire.RequiredFlag},
})
