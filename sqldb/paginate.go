package sqldb

import (
	"context"
	"fmt"
)

// QueryConfig holds configuration values for SQL queries.
type QueryConfig struct {
	// MaxBatchSize is the maximum number of items included in a batch
	// query IN clauses list.
	MaxBatchSize int
}

// DefaultQueryConfig returns a default configuration for SQL queries.
func DefaultQueryConfig() *QueryConfig {
	return &QueryConfig{
		// TODO(elle): make configurable & have different defaults
		// for SQLite and Postgres.
		MaxBatchSize: 250,
	}
}

// BatchQueryFunc represents a function that takes a batch of converted items
// and returns results.
type BatchQueryFunc[T any, R any] func(context.Context, []T) ([]R, error)

// ItemCallbackFunc represents a function that processes individual results.
type ItemCallbackFunc[R any] func(context.Context, R) error

// ConvertFunc represents a function that converts from input type to query type
// for the batch query.
type ConvertFunc[I any, T any] func(I) T

// ExecuteBatchQuery executes a query in batches over a slice of input items.
// It converts the input items to a query type using the provided convertFunc,
// executes the query in batches using the provided queryFunc, and applies
// the callback to each result. This is useful for queries using the
// "WHERE x IN []slice" pattern. It takes that slice, splits it into batches of
// size MaxBatchSize, and executes the query for each batch.
//
// NOTE: it is the caller's responsibility to ensure that the expected return
// results are unique across all pages. Meaning that if the input items are
// split up, a result that is returned in one page should not be expected to
// be returned in another page.
func ExecuteBatchQuery[I any, T any, R any](ctx context.Context,
	cfg *QueryConfig, inputItems []I, convertFunc ConvertFunc[I, T],
	queryFunc BatchQueryFunc[T, R], callback ItemCallbackFunc[R]) error {

	if len(inputItems) == 0 {
		return nil
	}

	// Process items in pages.
	for i := 0; i < len(inputItems); i += cfg.MaxBatchSize {
		// Calculate the end index for this page.
		end := i + cfg.MaxBatchSize
		if end > len(inputItems) {
			end = len(inputItems)
		}

		// Get the page slice of input items.
		inputPage := inputItems[i:end]

		// Convert only the items needed for this page.
		convertedPage := make([]T, len(inputPage))
		for j, inputItem := range inputPage {
			convertedPage[j] = convertFunc(inputItem)
		}

		// Execute the query for this page.
		results, err := queryFunc(ctx, convertedPage)
		if err != nil {
			return fmt.Errorf("query failed for page "+
				"starting at %d: %w", i, err)
		}

		// Apply the callback to each result.
		for _, result := range results {
			if err := callback(ctx, result); err != nil {
				return fmt.Errorf("callback failed for "+
					"result: %w", err)
			}
		}
	}

	return nil
}
