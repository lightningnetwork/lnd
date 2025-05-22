package batch

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/sqldb"
)

// BoltBatcher is a bbolt implementation of the sqldb.BatchedTx interface.
type BoltBatcher[Q any] struct {
	db kvdb.Backend
}

// NewBoltBackend creates a new BoltBackend instance.
func NewBoltBackend[Q any](db kvdb.Backend) *BoltBatcher[Q] {
	return &BoltBatcher[Q]{db: db}
}

// ExecTx will execute the passed txBody, operating upon generic
// parameter Q (usually a storage interface) in a single transaction.
//
// NOTE: This is part of the sqldb.BatchedTx interface.
func (t *BoltBatcher[Q]) ExecTx(_ context.Context, opts sqldb.TxOptions,
	txBody func(Q) error, reset func()) error {

	if opts.ReadOnly() {
		return kvdb.View(t.db, func(tx kvdb.RTx) error {
			q, ok := any(tx).(Q)
			if !ok {
				return fmt.Errorf("unable to cast tx(%T) "+
					"into the type expected by the "+
					"BoltBatcher(%T)", tx, t)
			}

			return txBody(q)
		}, reset)
	}

	return kvdb.Update(t.db, func(tx kvdb.RwTx) error {
		q, ok := any(tx).(Q)
		if !ok {
			return fmt.Errorf("unable to cast tx(%T) into the "+
				"type expected by the BoltBatcher(%T)", tx, t)
		}

		return txBody(q)
	}, reset)
}
