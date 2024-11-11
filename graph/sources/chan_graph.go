package sources

import graphdb "github.com/lightningnetwork/lnd/graph/db"

// DBSource is an implementation of the GraphSource interface backed by a local
// persistence layer holding graph related data.
type DBSource struct {
	db *graphdb.ChannelGraph
}

// A compile-time check to ensure that sources.DBSource implements the
// GraphSource interface.
var _ GraphSource = (*DBSource)(nil)

// NewDBGSource returns a new instance of the DBSource backed by a
// graphdb.ChannelGraph instance.
func NewDBGSource(db *graphdb.ChannelGraph) *DBSource {
	return &DBSource{
		db: db,
	}
}
