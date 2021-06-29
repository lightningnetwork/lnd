package lntest

import (
	"io/ioutil"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFeeService tests the itest fee estimating web service.
func TestFeeService(t *testing.T) {
	service := startFeeService()
	defer service.stop()

	service.setFee(5000)

	// Wait for service to start accepting connections.
	var resp *http.Response
	require.Eventually(
		t,
		func() bool {
			var err error
			resp, err = http.Get(service.url) // nolint:bodyclose
			return err == nil
		},
		10*time.Second, time.Second,
	)

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(
		t, "{\"fee_by_block_target\":{\"1\":20000}}", string(body),
	)
}
