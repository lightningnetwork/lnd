package txscope

import "context"

type Queries interface {
	Fetch(context.Context) error
}

type DB interface {
	Queries
	ExecTx(context.Context, func(Queries) error, func()) error
}

type Store struct {
	db      DB
	otherDB DB
}

func (s *Store) valid(ctx context.Context) error {
	if err := s.db.Fetch(ctx); err != nil {
		return err
	}

	return s.db.ExecTx(ctx, func(tx Queries) error {
		if err := tx.Fetch(ctx); err != nil {
			return err
		}

		if err := helper(ctx, tx); err != nil {
			return err
		}

		if err := s.otherDB.Fetch(ctx); err != nil {
			return err
		}

		return s.otherDB.ExecTx(
			ctx, func(otherTx Queries) error {
				return otherTx.Fetch(ctx)
			}, func() {},
		)
	}, func() {})
}

func (s *Store) shadowedCallbackParameter(ctx context.Context) error {
	return s.db.ExecTx(ctx, func(db Queries) error {
		return db.Fetch(ctx)
	}, func() {})
}

func (s *Store) directQuery(ctx context.Context) error {
	return s.db.ExecTx(ctx, func(tx Queries) error {
		return s.db.Fetch(ctx) // want `transaction origin accessed`
	}, func() {})
}

func (s *Store) nestedTransaction(ctx context.Context) error {
	return s.db.ExecTx(ctx, func(tx Queries) error {
		return s.db.ExecTx( // want `transaction origin accessed`
			ctx, func(nested Queries) error {
				return nested.Fetch(ctx)
			}, func() {},
		)
	}, func() {})
}

func (s *Store) alias(ctx context.Context) error {
	return s.db.ExecTx(ctx, func(tx Queries) error {
		first := s.db   // want `transaction origin accessed`
		second := first // want `transaction origin accessed`

		return second.Fetch(ctx) // want `transaction origin accessed`
	}, func() {})
}

func (s *Store) passedToHelper(ctx context.Context) error {
	return s.db.ExecTx(ctx, func(tx Queries) error {
		return helper(ctx, s.db) // want `transaction origin accessed`
	}, func() {})
}

func (s *Store) convertedAlias(ctx context.Context) error {
	return s.db.ExecTx(ctx, func(tx Queries) error {
		queries := Queries(s.db) // want `transaction origin accessed`

		return queries.Fetch(ctx) // want `transaction origin accessed`
	}, func() {})
}

func (s *Store) namedCallback(ctx context.Context) error {
	callback := func(tx Queries) error {
		return tx.Fetch(ctx)
	}

	return s.db.ExecTx( // want `ExecTx callback must be an inline function`
		ctx, callback, func() {},
	)
}

func helper(ctx context.Context, queries Queries) error {
	return queries.Fetch(ctx)
}
