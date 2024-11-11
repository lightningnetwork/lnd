package sources

import "github.com/lightningnetwork/lnd/graph/session"

// GraphSource defines the read-only graph interface required by LND for graph
// related queries.
type GraphSource interface {
	session.ReadOnlyGraph
}
