//go:build dev
// +build dev

package lncfg

// ExperimentalProtocol is a sub-config that houses any experimental protocol
// features that also require a build-tag to activate.
//
//nolint:ll
type ExperimentalProtocol struct {
	// Bolt12Offers turns on BOLT 12 offers. The node opens the offer store,
	// which needs native SQL and the BOLT 12 development migrations. Only
	// a build with the test_native_sql tag contains those migrations.
	Bolt12Offers bool `long:"bolt12-offers" description:"enable BOLT 12 offers (experimental), which needs db.use-native-sql and a build with the dev and test_native_sql tags"`
}

// Bolt12OffersEnabled returns true if BOLT 12 offers are enabled.
func (p ExperimentalProtocol) Bolt12OffersEnabled() bool {
	return p.Bolt12Offers
}
