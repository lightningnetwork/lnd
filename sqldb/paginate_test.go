//go:build test_db_postgres || test_db_sqlite

package sqldb

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/require"
)

// TestExecuteBatchQuery tests the ExecuteBatchQuery function which processes
// items in pages, allowing for efficient querying and processing of large
// datasets.
func TestExecuteBatchQuery(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	t.Run("empty input returns nil", func(t *testing.T) {
		var (
			cfg        = DefaultSQLiteConfig()
			inputItems []int
		)

		convertFunc := func(i int) string {
			return fmt.Sprintf("%d", i)
		}

		queryFunc := func(ctx context.Context, items []string) (
			[]string, error) {

			require.Fail(t, "queryFunc should not be called "+
				"with empty input")
			return nil, nil
		}
		callback := func(ctx context.Context, result string) error {
			require.Fail(t, "callback should not be called with "+
				"empty input")

			return nil
		}

		err := ExecuteBatchQuery(
			ctx, cfg, inputItems, convertFunc, queryFunc, callback,
		)
		require.NoError(t, err)
	})

	t.Run("single page processes all items", func(t *testing.T) {
		var (
			convertedItems  []string
			callbackResults []string
			inputItems      = []int{1, 2, 3, 4, 5}
			cfg             = &QueryConfig{
				MaxBatchSize: 10,
			}
		)

		convertFunc := func(i int) string {
			return fmt.Sprintf("converted_%d", i)
		}

		queryFunc := func(ctx context.Context,
			items []string) ([]string, error) {

			convertedItems = append(convertedItems, items...)
			results := make([]string, len(items))
			for i, item := range items {
				results[i] = fmt.Sprintf("result_%s", item)
			}

			return results, nil
		}

		callback := func(ctx context.Context, result string) error {
			callbackResults = append(callbackResults, result)
			return nil
		}

		err := ExecuteBatchQuery(
			ctx, cfg, inputItems, convertFunc, queryFunc, callback,
		)
		require.NoError(t, err)

		require.Equal(t, []string{
			"converted_1", "converted_2", "converted_3",
			"converted_4", "converted_5",
		}, convertedItems)

		require.Equal(t, []string{
			"result_converted_1", "result_converted_2",
			"result_converted_3", "result_converted_4",
			"result_converted_5",
		}, callbackResults)
	})

	t.Run("multiple pages process correctly", func(t *testing.T) {
		var (
			queryCallCount int
			pageSizes      []int
			allResults     []string
			inputItems     = []int{1, 2, 3, 4, 5, 6, 7, 8}
			cfg            = &QueryConfig{
				MaxBatchSize: 3,
			}
		)

		convertFunc := func(i int) string {
			return fmt.Sprintf("item_%d", i)
		}

		queryFunc := func(ctx context.Context,
			items []string) ([]string, error) {

			queryCallCount++
			pageSizes = append(pageSizes, len(items))
			results := make([]string, len(items))
			for i, item := range items {
				results[i] = fmt.Sprintf("result_%s", item)
			}

			return results, nil
		}

		callback := func(ctx context.Context, result string) error {
			allResults = append(allResults, result)
			return nil
		}

		err := ExecuteBatchQuery(
			ctx, cfg, inputItems, convertFunc, queryFunc, callback,
		)
		require.NoError(t, err)

		// Should have 3 pages: [1,2,3], [4,5,6], [7,8]
		require.Equal(t, 3, queryCallCount)
		require.Equal(t, []int{3, 3, 2}, pageSizes)
		require.Len(t, allResults, 8)
	})

	t.Run("query function error is propagated", func(t *testing.T) {
		var (
			cfg        = DefaultSQLiteConfig()
			inputItems = []int{1, 2, 3}
		)

		convertFunc := func(i int) string {
			return fmt.Sprintf("%d", i)
		}

		queryFunc := func(ctx context.Context,
			items []string) ([]string, error) {

			return nil, errors.New("query failed")
		}

		callback := func(ctx context.Context, result string) error {
			require.Fail(t, "callback should not be called when "+
				"query fails")

			return nil
		}

		err := ExecuteBatchQuery(
			ctx, cfg, inputItems, convertFunc, queryFunc, callback,
		)
		require.ErrorContains(t, err, "query failed for page "+
			"starting at 0: query failed")
	})

	t.Run("callback error is propagated", func(t *testing.T) {
		var (
			cfg        = DefaultSQLiteConfig()
			inputItems = []int{1, 2, 3}
		)

		convertFunc := func(i int) string {
			return fmt.Sprintf("%d", i)
		}

		queryFunc := func(ctx context.Context,
			items []string) ([]string, error) {

			return items, nil
		}

		callback := func(ctx context.Context, result string) error {
			if result == "2" {
				return errors.New("callback failed")
			}
			return nil
		}

		err := ExecuteBatchQuery(
			ctx, cfg, inputItems, convertFunc, queryFunc, callback,
		)
		require.ErrorContains(t, err, "callback failed for result: "+
			"callback failed")
	})

	t.Run("query error in second page is propagated", func(t *testing.T) {
		var (
			inputItems = []int{1, 2, 3, 4}
			cfg        = &QueryConfig{
				MaxBatchSize: 2,
			}
			queryCallCount int
		)

		convertFunc := func(i int) string {
			return fmt.Sprintf("%d", i)
		}

		queryFunc := func(ctx context.Context,
			items []string) ([]string, error) {

			queryCallCount++
			if queryCallCount == 2 {
				return nil, fmt.Errorf("second page failed")
			}

			return items, nil
		}

		callback := func(ctx context.Context, result string) error {
			return nil
		}

		err := ExecuteBatchQuery(
			ctx, cfg, inputItems, convertFunc, queryFunc, callback,
		)
		require.ErrorContains(t, err, "query failed for page "+
			"starting at 2: second page failed")
	})
}

// TestSQLSliceQueries tests ExecuteBatchQuery helper by first showing that a
// query the /*SLICE:<field_name>*/ directive has a maximum number of
// parameters it can handle, and then showing that the paginated version which
// uses ExecuteBatchQuery instead of a raw query can handle more parameters by
// executing the query in pages.
func TestSQLSliceQueries(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	db := NewTestDB(t)

	// Increase the number of query strings by an order of magnitude each
	// iteration until we hit the limit of the backing DB.
	//
	// NOTE: from testing, the following limits have been noted:
	// 	- for Postgres, the limit is 65535 parameters.
	//	- for SQLite, the limit is 32766 parameters.
	x := 10
	var queryParams []string
	for {
		for len(queryParams) < x {
			queryParams = append(
				queryParams,
				fmt.Sprintf("%d", len(queryParams)),
			)
		}

		_, err := db.GetChannelsByOutpoints(ctx, queryParams)
		if err != nil {
			if isSQLite {
				require.ErrorContains(
					t, err, "SQL logic error: too many "+
						"SQL variables",
				)
			} else {
				require.ErrorContains(
					t, err, "extended protocol limited "+
						"to 65535 parameters",
				)
			}
			break
		}

		// If it succeeded, we expect it to be under the maximum that
		// we expect for this DB.
		if isSQLite {
			require.LessOrEqual(t, x, maxSQLiteBatchSize,
				"SQLite should not exceed 32766 parameters")
		} else {
			require.LessOrEqual(t, x, maxPostgresBatchSize,
				"Postgres should not exceed 65535 parameters")
		}

		x *= 10
	}

	// Now that we have found the limit that the raw query can handle, we
	// switch to the wrapped version which will perform the query in pages
	// so that the limit is not hit. We use the same number of query params
	// that caused the error above.
	queryWrapper := func(ctx context.Context,
		pageOutpoints []string) ([]sqlc.GetChannelsByOutpointsRow,
		error) {

		return db.GetChannelsByOutpoints(ctx, pageOutpoints)
	}

	err := ExecuteBatchQuery(
		ctx,
		DefaultSQLiteConfig(),
		queryParams,
		func(s string) string {
			return s
		},
		queryWrapper,
		func(context.Context, sqlc.GetChannelsByOutpointsRow) error {
			return nil
		},
	)
	require.NoError(t, err)
}

// TestExecutePaginatedQuery tests the ExecutePaginatedQuery function which
// processes items in pages, allowing for efficient querying and processing of
// large datasets. It simulates a cursor-based pagination system where items
// are fetched in pages, processed, and the cursor is updated for the next
// page until all items are processed or an error occurs.
func TestExecutePaginatedQuery(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	type testItem struct {
		id   int64
		name string
	}

	type testResult struct {
		itemID int64
		value  string
	}

	tests := []struct {
		name          string
		pageSize      uint32
		allItems      []testItem
		initialCursor int64
		queryError    error
		// Which call number to return error on (0 = never).
		queryErrorOnCall int
		processError     error
		// Which item ID to fail processing on (0 = never).
		processErrorOnID int64
		expectedError    string
		expectedResults  []testResult
		expectedPages    int
	}{
		{
			name:     "happy path multiple pages",
			pageSize: 2,
			allItems: []testItem{
				{id: 1, name: "Item1"},
				{id: 2, name: "Item2"},
				{id: 3, name: "Item3"},
				{id: 4, name: "Item4"},
				{id: 5, name: "Item5"}},
			initialCursor: 0,
			expectedResults: []testResult{
				{itemID: 1, value: "Processed-Item1"},
				{itemID: 2, value: "Processed-Item2"},
				{itemID: 3, value: "Processed-Item3"},
				{itemID: 4, value: "Processed-Item4"},
				{itemID: 5, value: "Processed-Item5"},
			},
			expectedPages: 3, // 2+2+1 items across 3 pages.
		},
		{
			name:          "empty results",
			pageSize:      10,
			allItems:      []testItem{},
			initialCursor: 0,
			expectedPages: 1, // One call that returns empty.
		},
		{
			name:     "single page",
			pageSize: 10,
			allItems: []testItem{
				{id: 1, name: "OnlyItem"},
			},
			initialCursor: 0,
			expectedResults: []testResult{
				{itemID: 1, value: "Processed-OnlyItem"},
			},
			// The first page returns less than the max size,
			// indicating no more items to fetch after that.
			expectedPages: 1,
		},
		{
			name:     "query error first call",
			pageSize: 2,
			allItems: []testItem{
				{id: 1, name: "Item1"},
			},
			initialCursor: 0,
			queryError: errors.New(
				"database connection failed",
			),
			queryErrorOnCall: 1,
			expectedError:    "failed to fetch page with cursor 0",
			expectedPages:    1,
		},
		{
			name:     "query error second call",
			pageSize: 1,
			allItems: []testItem{
				{id: 1, name: "Item1"},
				{id: 2, name: "Item2"},
			},
			initialCursor: 0,
			queryError: errors.New(
				"database error on second page",
			),
			queryErrorOnCall: 2,
			expectedError:    "failed to fetch page with cursor 1",
			// First item processed before error.
			expectedResults: []testResult{
				{itemID: 1, value: "Processed-Item1"},
			},
			expectedPages: 2,
		},
		{
			name:     "process error first item",
			pageSize: 10,
			allItems: []testItem{
				{id: 1, name: "Item1"}, {id: 2, name: "Item2"},
			},
			initialCursor:    0,
			processError:     errors.New("processing failed"),
			processErrorOnID: 1,
			expectedError:    "failed to process item",
			// No results since first item failed.
			expectedPages: 1,
		},
		{
			name:     "process error second item",
			pageSize: 10,
			allItems: []testItem{
				{id: 1, name: "Item1"}, {id: 2, name: "Item2"},
			},
			initialCursor:    0,
			processError:     errors.New("processing failed"),
			processErrorOnID: 2,
			expectedError:    "failed to process item",
			// First item processed before error.
			expectedResults: []testResult{
				{itemID: 1, value: "Processed-Item1"},
			},
			expectedPages: 1,
		},
		{
			name:     "different initial cursor",
			pageSize: 2,
			allItems: []testItem{
				{id: 1, name: "Item1"},
				{id: 2, name: "Item2"},
				{id: 3, name: "Item3"},
			},
			// Start from ID > 1.
			initialCursor: 1,
			expectedResults: []testResult{
				{itemID: 2, value: "Processed-Item2"},
				{itemID: 3, value: "Processed-Item3"},
			},
			// 2+0 items across 2 pages.
			expectedPages: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				processedResults []testResult
				queryCallCount   int
				cfg              = &QueryConfig{
					MaxPageSize: tt.pageSize,
				}
			)

			queryFunc := func(ctx context.Context, cursor int64,
				limit int32) ([]testItem, error) {

				queryCallCount++

				// Return error on specific call if configured.
				if tt.queryErrorOnCall > 0 &&
					queryCallCount == tt.queryErrorOnCall {

					return nil, tt.queryError
				}

				// Simulate cursor-based pagination
				var items []testItem
				for _, item := range tt.allItems {
					if item.id > cursor &&
						len(items) < int(limit) {

						items = append(items, item)
					}
				}
				return items, nil
			}

			extractCursor := func(item testItem) int64 {
				return item.id
			}

			processItem := func(ctx context.Context,
				item testItem) error {

				// Return error on specific item if configured.
				if tt.processErrorOnID > 0 &&
					item.id == tt.processErrorOnID {

					return tt.processError
				}

				processedResults = append(
					processedResults, testResult{
						itemID: item.id,
						value: fmt.Sprintf(
							"Processed-%s",
							item.name,
						),
					},
				)

				return nil
			}

			err := ExecutePaginatedQuery(
				ctx, cfg, tt.initialCursor, queryFunc,
				extractCursor, processItem,
			)

			// Check error expectations
			if tt.expectedError != "" {
				require.ErrorContains(t, err, tt.expectedError)
				if tt.queryError != nil {
					require.ErrorIs(t, err, tt.queryError)
				}
				if tt.processError != nil {
					require.ErrorIs(t, err, tt.processError)
				}
			} else {
				require.NoError(t, err)
			}

			// Check processed results.
			require.Equal(t, tt.expectedResults, processedResults)

			// Check number of query calls.
			require.Equal(t, tt.expectedPages, queryCallCount)
		})
	}
}

// TestExecuteCollectAndBatchWithSharedDataQuery tests the
// ExecuteCollectAndBatchWithSharedDataQuery function which processes items in
// pages, allowing for efficient querying and processing of large datasets with
// shared data across batches.
func TestExecuteCollectAndBatchWithSharedDataQuery(t *testing.T) {
	t.Parallel()

	type channelRow struct {
		id        int64
		name      string
		policyIDs []int64
	}

	type channelBatchData struct {
		lookupTable map[int64]string
		sharedInfo  string
	}

	type processedChannel struct {
		id         int64
		name       string
		sharedInfo string
		lookupData string
	}

	tests := []struct {
		name                   string
		maxPageSize            uint32
		allRows                []channelRow
		initialCursor          int64
		pageQueryError         error
		pageQueryErrorOnCall   int
		batchDataError         error
		batchDataErrorOnBatch  int
		processError           error
		processErrorOnID       int64
		earlyTerminationOnPage int
		expectedError          string
		expectedProcessedItems []processedChannel
		expectedPageCalls      int
		expectedBatchCalls     int
	}{
		{
			name:        "multiple pages multiple batches",
			maxPageSize: 2,
			allRows: []channelRow{
				{
					id:        1,
					name:      "Chan1",
					policyIDs: []int64{10, 11},
				},
				{
					id:        2,
					name:      "Chan2",
					policyIDs: []int64{20},
				},
				{
					id:        3,
					name:      "Chan3",
					policyIDs: []int64{30, 31},
				},
				{
					id:        4,
					name:      "Chan4",
					policyIDs: []int64{40},
				},
				{
					id:        5,
					name:      "Chan5",
					policyIDs: []int64{50},
				},
			},
			initialCursor: 0,
			expectedProcessedItems: []processedChannel{
				{
					id:         1,
					name:       "Chan1",
					sharedInfo: "batch-shared",
					lookupData: "lookup-1",
				},
				{
					id:         2,
					name:       "Chan2",
					sharedInfo: "batch-shared",
					lookupData: "lookup-2",
				},
				{
					id:         3,
					name:       "Chan3",
					sharedInfo: "batch-shared",
					lookupData: "lookup-3",
				},
				{
					id:         4,
					name:       "Chan4",
					sharedInfo: "batch-shared",
					lookupData: "lookup-4",
				},
				{
					id:         5,
					name:       "Chan5",
					sharedInfo: "batch-shared",
					lookupData: "lookup-5",
				},
			},
			// Pages: [1,2], [3,4], [5].
			expectedPageCalls: 3,
			// One batch call per page with data: [1,2], [3,4], [5].
			expectedBatchCalls: 3,
		},
		{
			name:          "empty results",
			maxPageSize:   10,
			allRows:       []channelRow{},
			initialCursor: 0,
			// One call that returns empty.
			expectedPageCalls: 1,
			// No batches since no items.
			expectedBatchCalls: 0,
		},
		{
			name:        "single page single batch",
			maxPageSize: 10,
			allRows: []channelRow{
				{
					id:        1,
					name:      "Chan1",
					policyIDs: []int64{10},
				},
				{
					id:        2,
					name:      "Chan2",
					policyIDs: []int64{20},
				},
			},
			initialCursor: 0,
			expectedProcessedItems: []processedChannel{
				{
					id:         1,
					name:       "Chan1",
					sharedInfo: "batch-shared",
					lookupData: "lookup-1",
				},
				{
					id:         2,
					name:       "Chan2",
					sharedInfo: "batch-shared",
					lookupData: "lookup-2",
				},
			},
			// One page with all items.
			expectedPageCalls: 1,
			// One batch call for the single page.
			expectedBatchCalls: 1,
		},
		{
			name:        "page query error first call",
			maxPageSize: 5,
			allRows: []channelRow{
				{
					id:   1,
					name: "Chan1",
				},
			},
			initialCursor: 0,
			pageQueryError: errors.New(
				"database connection failed",
			),
			pageQueryErrorOnCall: 1,
			expectedError: "failed to fetch page with " +
				"cursor 0",
			expectedPageCalls:  1,
			expectedBatchCalls: 0,
		},
		{
			name:        "page query error second call",
			maxPageSize: 1,
			allRows: []channelRow{
				{
					id:   1,
					name: "Chan1",
				},
				{
					id:   2,
					name: "Chan2",
				},
			},
			initialCursor: 0,
			pageQueryError: errors.New("database error on " +
				"second page"),
			pageQueryErrorOnCall: 2,
			expectedError: "failed to fetch page with " +
				"cursor 1",
			expectedProcessedItems: []processedChannel{
				{
					id:         1,
					name:       "Chan1",
					sharedInfo: "batch-shared",
					lookupData: "lookup-1",
				},
			},
			expectedPageCalls:  2,
			expectedBatchCalls: 1,
		},
		{
			name:        "batch data error first batch",
			maxPageSize: 10,
			allRows: []channelRow{
				{
					id:   1,
					name: "Chan1",
				},
				{
					id:   2,
					name: "Chan2",
				},
			},
			initialCursor: 0,
			batchDataError: errors.New("batch loading " +
				"failed"),
			batchDataErrorOnBatch: 1,
			expectedError: "failed to load batch data " +
				"for page",
			expectedPageCalls:  1,
			expectedBatchCalls: 1,
		},
		{
			name:        "batch data error second page",
			maxPageSize: 1,
			allRows: []channelRow{
				{
					id:   1,
					name: "Chan1",
				},
				{
					id:   2,
					name: "Chan2",
				},
			},
			initialCursor: 0,
			batchDataError: errors.New("batch loading " +
				"failed on second page"),
			batchDataErrorOnBatch: 2,
			expectedError: "failed to load batch data " +
				"for page",
			expectedProcessedItems: []processedChannel{
				{
					id:         1,
					name:       "Chan1",
					sharedInfo: "batch-shared",
					lookupData: "lookup-1",
				},
			},
			expectedPageCalls:  2,
			expectedBatchCalls: 2,
		},
		{
			name:        "process error first item",
			maxPageSize: 10,
			allRows: []channelRow{
				{
					id:   1,
					name: "Chan1",
				},
				{
					id:   2,
					name: "Chan2",
				},
			},
			initialCursor:    0,
			processError:     errors.New("processing failed"),
			processErrorOnID: 1,
			expectedError: "failed to process item with " +
				"batch data",
			expectedPageCalls:  1,
			expectedBatchCalls: 1,
		},
		{
			name:        "process error second item",
			maxPageSize: 10,
			allRows: []channelRow{
				{
					id:   1,
					name: "Chan1",
				},
				{
					id:   2,
					name: "Chan2",
				},
			},
			initialCursor:    0,
			processError:     errors.New("processing failed"),
			processErrorOnID: 2,
			expectedError: "failed to process item with batch " +
				"data",
			expectedProcessedItems: []processedChannel{
				{
					id:         1,
					name:       "Chan1",
					sharedInfo: "batch-shared",
					lookupData: "lookup-1",
				},
			},
			expectedPageCalls:  1,
			expectedBatchCalls: 1,
		},
		{
			name:        "early termination partial page",
			maxPageSize: 3,
			allRows: []channelRow{
				{
					id:   1,
					name: "Chan1",
				},
				{
					id:   2,
					name: "Chan2",
				},
			},
			initialCursor:          0,
			earlyTerminationOnPage: 1,
			expectedProcessedItems: []processedChannel{
				{
					id:         1,
					name:       "Chan1",
					sharedInfo: "batch-shared",
					lookupData: "lookup-1",
				},
				{
					id:         2,
					name:       "Chan2",
					sharedInfo: "batch-shared",
					lookupData: "lookup-2",
				},
			},
			expectedPageCalls:  1,
			expectedBatchCalls: 1,
		},
		{
			name:        "different initial cursor",
			maxPageSize: 2,
			allRows: []channelRow{
				{
					id:   1,
					name: "Chan1",
				},
				{
					id:   2,
					name: "Chan2",
				},
				{
					id:   3,
					name: "Chan3",
				},
			},
			initialCursor: 1,
			expectedProcessedItems: []processedChannel{
				{
					id:         2,
					name:       "Chan2",
					sharedInfo: "batch-shared",
					lookupData: "lookup-2",
				},
				{
					id:         3,
					name:       "Chan3",
					sharedInfo: "batch-shared",
					lookupData: "lookup-3",
				},
			},
			// [2,3], [].
			expectedPageCalls: 2,
			// One batch call for the page with [2,3].
			expectedBatchCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			cfg := &QueryConfig{
				MaxPageSize: tt.maxPageSize,
			}

			var (
				processedItems []processedChannel
				pageCallCount  int
				batchCallCount int
			)

			pageQueryFunc := func(ctx context.Context, cursor int64,
				limit int32) ([]channelRow, error) {

				pageCallCount++

				// Return error on specific call if configured.
				//nolint:ll
				if tt.pageQueryErrorOnCall > 0 &&
					pageCallCount == tt.pageQueryErrorOnCall {

					return nil, tt.pageQueryError
				}

				// Simulate cursor-based pagination.
				var items []channelRow
				for _, row := range tt.allRows {
					if row.id > cursor &&
						len(items) < int(limit) {

						items = append(items, row)
					}
				}

				// Handle early termination test case.
				//nolint:ll
				if tt.earlyTerminationOnPage > 0 &&
					pageCallCount == tt.earlyTerminationOnPage {

					// Return fewer items than maxPageSize
					// to trigger termination
					if len(items) >= int(tt.maxPageSize) {
						items = items[:tt.maxPageSize-1]
					}
				}

				return items, nil
			}

			extractPageCursor := func(row channelRow) int64 {
				return row.id
			}

			collectFunc := func(row channelRow) (int64, error) {
				return row.id, nil
			}

			batchDataFunc := func(ctx context.Context,
				ids []int64) (*channelBatchData, error) {

				batchCallCount++

				// Return error on specific batch if configured.
				//nolint:ll
				if tt.batchDataErrorOnBatch > 0 &&
					batchCallCount == tt.batchDataErrorOnBatch {

					return nil, tt.batchDataError
				}

				// Create mock batch data.
				lookupTable := make(map[int64]string)
				for _, id := range ids {
					lookupTable[id] =
						fmt.Sprintf("lookup-%d", id)
				}

				return &channelBatchData{
					lookupTable: lookupTable,
					sharedInfo:  "batch-shared",
				}, nil
			}

			processItem := func(ctx context.Context, row channelRow,
				batchData *channelBatchData) error {

				// Return error on specific item if configured.
				if tt.processErrorOnID > 0 &&
					row.id == tt.processErrorOnID {

					return tt.processError
				}

				processedChan := processedChannel{
					id:         row.id,
					name:       row.name,
					sharedInfo: batchData.sharedInfo,
					lookupData: batchData.
						lookupTable[row.id],
				}

				processedItems = append(
					processedItems, processedChan,
				)
				return nil
			}

			err := ExecuteCollectAndBatchWithSharedDataQuery(
				ctx, cfg, tt.initialCursor,
				pageQueryFunc, extractPageCursor, collectFunc,
				batchDataFunc, processItem,
			)

			// Check error expectations.
			if tt.expectedError != "" {
				require.ErrorContains(t, err, tt.expectedError)
				if tt.pageQueryError != nil {
					require.ErrorIs(
						t, err, tt.pageQueryError,
					)
				}
				if tt.batchDataError != nil {
					require.ErrorIs(
						t, err, tt.batchDataError,
					)
				}
				if tt.processError != nil {
					require.ErrorIs(
						t, err, tt.processError,
					)
				}
			} else {
				require.NoError(t, err)
			}

			// Check processed results.
			require.Equal(
				t, tt.expectedProcessedItems, processedItems,
			)

			// Check call counts.
			require.Equal(
				t, tt.expectedPageCalls, pageCallCount,
			)
			require.Equal(
				t, tt.expectedBatchCalls, batchCallCount,
			)
		})
	}
}
