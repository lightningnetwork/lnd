package sqldb

import (
	"context"
	"fmt"
)

// PagedQueryFunc represents a function that takes a slice of converted items
// and returns results.
type PagedQueryFunc[T any, R any] func(context.Context, []T) ([]R, error)

// ItemCallbackFunc represents a function that processes individual results.
type ItemCallbackFunc[R any] func(context.Context, R) error

// ConvertFunc represents a function that converts from input type to query type
type ConvertFunc[I any, T any] func(I) T

// PagedQueryConfig holds configuration values for calls to ExecutePagedQuery.
type PagedQueryConfig struct {
	PageSize int
}

// DefaultPagedQueryConfig returns a default configuration
func DefaultPagedQueryConfig() *PagedQueryConfig {
	return &PagedQueryConfig{
		// TODO(elle): make configurable & have different defaults
		// for SQLite and Postgres.
		PageSize: 250,
	}
}

// ExecutePagedQuery executes a paginated query over a slice of input items.
// It converts the input items to a query type using the provided convertFunc,
// executes the query using the provided queryFunc, and applies the callback
// to each result.
func ExecutePagedQuery[I any, T any, R any](ctx context.Context,
	cfg *PagedQueryConfig, inputItems []I, convertFunc ConvertFunc[I, T],
	queryFunc PagedQueryFunc[T, R], callback ItemCallbackFunc[R]) error {

	if len(inputItems) == 0 {
		return nil
	}

	// Process items in pages.
	for i := 0; i < len(inputItems); i += cfg.PageSize {
		// Calculate the end index for this page.
		end := i + cfg.PageSize
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
