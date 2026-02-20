package lnutils

import "context"

// ContextFromQuit returns a context that is cancelled when the provided quit
// channel is closed. The returned cancel function MUST be called to avoid
// goroutine leaks.
func ContextFromQuit(quit <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		select {
		case <-quit:
			cancel()
		case <-ctx.Done():
		}
	}()

	return ctx, cancel
}
