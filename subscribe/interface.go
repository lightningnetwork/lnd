package subscribe

// Subscription is an interface implemented by subscriptions to a server
// providing updates.
type Subscription interface {
	// Updates returns a read-only channel where the updates the client has
	// subscribed to will be delivered.
	Updates() <-chan interface{}

	// Quit is a channel that will be closed in case the server decides to
	// no longer deliver updates to this client.
	Quit() <-chan struct{}

	// Cancel should be called in case the client no longer wants to
	// subscribe for updates from the server.
	Cancel()
}
