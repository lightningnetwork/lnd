package sqlc

import "github.com/lightningnetwork/lnd/sqldb"

// wrappedTX is a wrapper around a DBTX that also stores the database backend
// type.
type wrappedTX struct {
	DBTX

	backendType sqldb.BackendType
}

// Backend returns the type of database backend we're using.
func (q *Queries) Backend() sqldb.BackendType {
	wtx, ok := q.db.(*wrappedTX)
	if !ok {
		// Shouldn't happen unless a new database backend type is added
		// but not initialized correctly.
		return sqldb.BackendTypeUnknown
	}

	return wtx.backendType
}

// NewForType creates a new Queries instance for the given database type.
func NewForType(db DBTX, typ sqldb.BackendType) *Queries {
	return &Queries{db: &wrappedTX{db, typ}}
}
