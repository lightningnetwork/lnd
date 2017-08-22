package macaroons

import (
	"path"

	"gopkg.in/macaroon-bakery.v1/bakery"

	"github.com/boltdb/bolt"
)

var (
	// dbFileName is the filename within the data directory which contains
	// the macaroon stores.
	dbFilename = "macaroons.db"
)

// NewService returns a service backed by the macaroon Bolt DB stored in the
// passed directory.
func NewService(dir string) (*bakery.Service, error) {
	// Open the database that we'll use to store the primary macaroon key,
	// and all generated macaroons+caveats.
	macaroonDB, err := bolt.Open(path.Join(dir, dbFilename), 0600,
		bolt.DefaultOptions)
	if err != nil {
		return nil, err
	}

	rootKeyStore, err := NewRootKeyStorage(macaroonDB)
	if err != nil {
		return nil, err
	}
	macaroonStore, err := NewStorage(macaroonDB)
	if err != nil {
		return nil, err
	}

	macaroonParams := bakery.NewServiceParams{
		Location:     "lnd",
		Store:        macaroonStore,
		RootKeyStore: rootKeyStore,
		// No third-party caveat support for now.
		// TODO(aakselrod): Add third-party caveat support.
		Locator: nil,
		Key:     nil,
	}
	return bakery.NewService(macaroonParams)
}
