package sqldb

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestExecutePagedQuery tests the ExecutePagedQuery function which processes
// items in pages, allowing for efficient querying and processing of large
// datasets.
func TestExecutePagedQuery(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("empty input returns nil", func(t *testing.T) {
		var (
			cfg        = DefaultPagedQueryConfig()
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

		err := ExecutePagedQuery(
			ctx, cfg, inputItems, convertFunc, queryFunc, callback,
		)
		require.NoError(t, err)
	})

	t.Run("single page processes all items", func(t *testing.T) {
		var (
			convertedItems  []string
			callbackResults []string
			inputItems      = []int{1, 2, 3, 4, 5}
			cfg             = &PagedQueryConfig{
				PageSize: 10,
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

		err := ExecutePagedQuery(
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
			cfg            = &PagedQueryConfig{
				PageSize: 3,
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

		err := ExecutePagedQuery(
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
			cfg        = DefaultPagedQueryConfig()
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

		err := ExecutePagedQuery(
			ctx, cfg, inputItems, convertFunc, queryFunc, callback,
		)
		require.ErrorContains(t, err, "query failed for page "+
			"starting at 0: query failed")
	})

	t.Run("callback error is propagated", func(t *testing.T) {
		var (
			cfg        = DefaultPagedQueryConfig()
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

		err := ExecutePagedQuery(
			ctx, cfg, inputItems, convertFunc, queryFunc, callback,
		)
		require.ErrorContains(t, err, "callback failed for result: "+
			"callback failed")
	})

	t.Run("query error in second page is propagated", func(t *testing.T) {
		var (
			inputItems = []int{1, 2, 3, 4}
			cfg        = &PagedQueryConfig{
				PageSize: 2,
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

		err := ExecutePagedQuery(
			ctx, cfg, inputItems, convertFunc, queryFunc, callback,
		)
		require.ErrorContains(t, err, "query failed for page "+
			"starting at 2: second page failed")
	})
}
