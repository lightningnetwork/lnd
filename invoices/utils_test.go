package invoices

import (
	"os"
	"runtime/pprof"
	"testing"
	"time"
)

// timeout implements a test level timeout.
func timeout(t *testing.T) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(5 * time.Second):
			pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)

			panic("test timeout")
		case <-done:
		}
	}()

	return func() {
		close(done)
	}
}
