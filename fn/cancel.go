package fn

import "context"

// CancelOrQuit is used to create a cancellable context that will be
// cancelled if the passed quit channel is signalled.
func CancelOrQuit(ctx context.Context, quit chan struct{}) (context.Context,
	context.CancelFunc) {

	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-quit:
			cancel()
		case <-ctx.Done():
		}
	}()

	return ctx, cancel
}
