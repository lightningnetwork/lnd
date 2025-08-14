package sqldb

import (
	"context"
	"fmt"
)

const (
	// maxSQLiteBatchSize is the maximum number of items that can be
	// included in a batch query IN clause for SQLite. This was determined
	// using the TestSQLSliceQueries test.
	maxSQLiteBatchSize = 32766

	// maxPostgresBatchSize is the maximum number of items that can be
	// included in a batch query IN clause for Postgres. This was determined
	// using the TestSQLSliceQueries test.
	maxPostgresBatchSize = 65535

	// defaultSQLitePageSize is the default page size for SQLite queries.
	defaultSQLitePageSize = 100

	// defaultPostgresPageSize is the default page size for Postgres
	// queries.
	defaultPostgresPageSize = 10500

	// defaultSQLiteBatchSize is the default batch size for SQLite queries.
	defaultSQLiteBatchSize = 250

	// defaultPostgresBatchSize is the default batch size for Postgres
	// queries.
	defaultPostgresBatchSize = 5000
)

// QueryConfig holds configuration values for SQL queries.
//
//nolint:ll
type QueryConfig struct {
	// MaxBatchSize is the maximum number of items included in a batch
	// query IN clauses list.
	MaxBatchSize uint32 `long:"max-batch-size" description:"The maximum number of items to include in a batch query IN clause. This is used for queries that fetch results based on a list of identifiers."`

	// MaxPageSize is the maximum number of items returned in a single page
	// of results. This is used for paginated queries.
	MaxPageSize uint32 `long:"max-page-size" description:"The maximum number of items to return in a single page of results. This is used for paginated queries."`
}

// Validate checks that the QueryConfig values are valid.
func (c *QueryConfig) Validate(sqlite bool) error {
	if c.MaxBatchSize <= 0 {
		return fmt.Errorf("max batch size must be greater than "+
			"zero, got %d", c.MaxBatchSize)
	}
	if c.MaxPageSize <= 0 {
		return fmt.Errorf("max page size must be greater than "+
			"zero, got %d", c.MaxPageSize)
	}

	if sqlite {
		if c.MaxBatchSize > maxSQLiteBatchSize {
			return fmt.Errorf("max batch size for SQLite cannot "+
				"exceed %d, got %d", maxSQLiteBatchSize,
				c.MaxBatchSize)
		}
	} else {
		if c.MaxBatchSize > maxPostgresBatchSize {
			return fmt.Errorf("max batch size for Postgres cannot "+
				"exceed %d, got %d", maxPostgresBatchSize,
				c.MaxBatchSize)
		}
	}

	return nil
}

// DefaultSQLiteConfig returns a default configuration for SQL queries to a
// SQLite backend.
func DefaultSQLiteConfig() *QueryConfig {
	return &QueryConfig{
		MaxBatchSize: defaultSQLiteBatchSize,
		MaxPageSize:  defaultSQLitePageSize,
	}
}

// DefaultPostgresConfig returns a default configuration for SQL queries to a
// Postgres backend.
func DefaultPostgresConfig() *QueryConfig {
	return &QueryConfig{
		MaxBatchSize: defaultPostgresBatchSize,
		MaxPageSize:  defaultPostgresPageSize,
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
	for i := 0; i < len(inputItems); i += int(cfg.MaxBatchSize) {
		// Calculate the end index for this page.
		end := i + int(cfg.MaxBatchSize)
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

// PagedQueryFunc represents a function that fetches a page of results using a
// cursor. It returns the fetched items and should return an empty slice when no
// more results.
type PagedQueryFunc[C any, T any] func(context.Context, C, int32) ([]T, error)

// CursorExtractFunc represents a function that extracts the cursor value from
// an item. This cursor will be used for the next page fetch.
type CursorExtractFunc[T any, C any] func(T) C

// ItemProcessFunc represents a function that processes individual items.
type ItemProcessFunc[T any] func(context.Context, T) error

// ExecutePaginatedQuery executes a cursor-based paginated query. It continues
// fetching pages until no more results are returned, processing each item with
// the provided callback.
//
// Parameters:
// - initialCursor: the starting cursor value (e.g., 0, -1, "", etc.).
// - queryFunc: function that fetches a page given cursor and limit.
// - extractCursor: function that extracts cursor from an item for next page.
// - processItem: function that processes each individual item.
//
// NOTE: it is the caller's responsibility to "undo" any processing done on
// items if the query fails on a later page.
func ExecutePaginatedQuery[C any, T any](ctx context.Context, cfg *QueryConfig,
	initialCursor C, queryFunc PagedQueryFunc[C, T],
	extractCursor CursorExtractFunc[T, C],
	processItem ItemProcessFunc[T]) error {

	cursor := initialCursor

	for {
		// Fetch the next page.
		items, err := queryFunc(ctx, cursor, int32(cfg.MaxPageSize))
		if err != nil {
			return fmt.Errorf("failed to fetch page with "+
				"cursor %v: %w", cursor, err)
		}

		// If no items returned, we're done.
		if len(items) == 0 {
			break
		}

		// Process each item in the page.
		for _, item := range items {
			if err := processItem(ctx, item); err != nil {
				return fmt.Errorf("failed to process item: %w",
					err)
			}

			// Update cursor for next iteration.
			cursor = extractCursor(item)
		}

		// If the number of items is less than the max page size,
		// we assume there are no more items to fetch.
		if len(items) < int(cfg.MaxPageSize) {
			break
		}
	}

	return nil
}

// CollectAndBatchDataQueryFunc represents a function that batch loads
// additional data for collected identifiers, returning the batch data that
// applies to all items.
type CollectAndBatchDataQueryFunc[ID any, BatchData any] func(context.Context,
	[]ID) (BatchData, error)

// ItemWithBatchDataProcessFunc represents a function that processes individual
// items along with shared batch data.
type ItemWithBatchDataProcessFunc[T any, BatchData any] func(context.Context,
	T, BatchData) error

// CollectFunc represents a function that extracts an identifier from a
// paginated item.
type CollectFunc[T any, ID any] func(T) (ID, error)

// ExecuteCollectAndBatchWithSharedDataQuery implements a page-by-page
// processing pattern where each page is immediately processed with batch-loaded
// data before moving to the next page.
//
// It:
// 1. Fetches a page of items using cursor-based pagination
// 2. Collects identifiers from that page and batch loads shared data
// 3. Processes each item in the page with the shared batch data
// 4. Moves to the next page and repeats
//
// Parameters:
// - initialCursor: starting cursor for pagination
// - pageQueryFunc: fetches a page of items
// - extractPageCursor: extracts cursor from paginated item for next page
// - collectFunc: extracts identifier from paginated item
// - batchDataFunc: batch loads shared data from collected IDs for one page
// - processItem: processes each item with the shared batch data
func ExecuteCollectAndBatchWithSharedDataQuery[C any, T any, I any, D any](
	ctx context.Context, cfg *QueryConfig, initialCursor C,
	pageQueryFunc PagedQueryFunc[C, T],
	extractPageCursor CursorExtractFunc[T, C],
	collectFunc CollectFunc[T, I],
	batchDataFunc CollectAndBatchDataQueryFunc[I, D],
	processItem ItemWithBatchDataProcessFunc[T, D]) error {

	cursor := initialCursor

	for {
		// Step 1: Fetch the next page of items.
		items, err := pageQueryFunc(ctx, cursor, int32(cfg.MaxPageSize))
		if err != nil {
			return fmt.Errorf("failed to fetch page with "+
				"cursor %v: %w", cursor, err)
		}

		// If no items returned, we're done.
		if len(items) == 0 {
			break
		}

		// Step 2: Collect identifiers from this page and batch load
		// data.
		pageIDs := make([]I, len(items))
		for i, item := range items {
			pageIDs[i], err = collectFunc(item)
			if err != nil {
				return fmt.Errorf("failed to collect "+
					"identifier from item: %w", err)
			}
		}

		// Batch load shared data for this page.
		batchData, err := batchDataFunc(ctx, pageIDs)
		if err != nil {
			return fmt.Errorf("failed to load batch data for "+
				"page: %w", err)
		}

		// Step 3: Process each item in this page with the shared batch
		// data.
		for _, item := range items {
			err := processItem(ctx, item, batchData)
			if err != nil {
				return fmt.Errorf("failed to process item "+
					"with batch data: %w", err)
			}

			// Update cursor for next page.
			cursor = extractPageCursor(item)
		}

		// If the number of items is less than the max page size,
		// we assume there are no more items to fetch.
		if len(items) < int(cfg.MaxPageSize) {
			break
		}
	}

	return nil
}
