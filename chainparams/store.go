package chainparams

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// ErrNetworkMismatch is returned by ValidateNetwork when the network stored in
// the database does not match the network lnd is configured to use.
var ErrNetworkMismatch = errors.New("database network mismatch")

// SQLChainParamQueries defines the SQL queries required by Store.
type SQLChainParamQueries interface {
	InsertChainNetwork(ctx context.Context, network string) error
	GetChainNetwork(ctx context.Context) (string, error)
}

// BatchedChainParamQueries is a version of SQLChainParamQueries that is
// capable of batched database operations.
type BatchedChainParamQueries interface {
	SQLChainParamQueries

	sqldb.BatchedTx[SQLChainParamQueries]
}

// Store is a database-backed store that persists and retrieves chain-level
// parameters such as the network the database was initialised for.
type Store struct {
	db BatchedChainParamQueries
}

// NewStore creates a new chain params Store backed by the given BaseDB.
func NewStore(db *sqldb.BaseDB) *Store {
	executor := sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) SQLChainParamQueries {
			return db.WithTx(tx)
		},
	)

	return &Store{db: executor}
}

// ValidateNetwork checks that the network stored in the chain_params table
// matches the provided network. On the first call the network is persisted so
// that subsequent restarts can detect an accidental network switch.
func (s *Store) ValidateNetwork(ctx context.Context,
	net *chaincfg.Params) error {

	network, err := normalizeNetworkName(net)
	if err != nil {
		return err
	}

	return s.db.ExecTx(
		ctx, sqldb.WriteTxOpt(),
		func(tx SQLChainParamQueries) error {
			// Insert the network only if the chain_params table is
			// still empty. This is a no-op on every startup after
			// the first.
			err := tx.InsertChainNetwork(ctx, network)
			if err != nil {
				return fmt.Errorf("unable to set network in "+
					"chain_params: %w", err)
			}

			// Read back whatever is stored. This is either the
			// value we just inserted (first startup) or a value
			// from a previous run.
			storedNetwork, err := tx.GetChainNetwork(ctx)
			if err != nil {
				return fmt.Errorf("unable to read network "+
					"from chain_params: %w", err)
			}

			if storedNetwork != network {
				return fmt.Errorf("%w: the database was "+
					"previously used with network '%s', "+
					"but lnd is now configured for "+
					"network '%s'. To fix this, either "+
					"point lnd at a different database "+
					"or reconfigure lnd to use "+
					"network '%s'", ErrNetworkMismatch,
					storedNetwork, network, storedNetwork)
			}

			return nil
		}, sqldb.NoOpReset,
	)
}

// normalizeNetworkName returns the stable network identifier persisted in the
// chain_params table.
func normalizeNetworkName(net *chaincfg.Params) (string, error) {
	if net == nil {
		return "", fmt.Errorf("chain parameters must not be nil")
	}

	network := lncfg.NormalizeNetwork(net.Name)
	if network == "" {
		return "", fmt.Errorf("chain parameters must define a network")
	}

	return network, nil
}

// Compile-time check that *sqlc.Queries implements SQLChainParamQueries.
var _ SQLChainParamQueries = (*sqlc.Queries)(nil)
