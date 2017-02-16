package main

import "github.com/lightningnetwork/lnd/lnwire"

// globalFeaturesMap is a map which binds the name of the global feature with it
// index. The index is just an order of the feature and the final binary
// representation of feature vector is determined by decode function.
var globalFeaturesMap = lnwire.FeaturesMap{}

// localFeaturesMap is a map which binds the name of the local feature with it
// index. The index is just an order of the feature and the final binary
// representation of feature vector is determined by decode function.
var localFeaturesMap = lnwire.FeaturesMap{}
