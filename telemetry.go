package main

import (
	"sync"
	"sync/atomic"

	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
)

type telemeter struct {
	clientsCounter uint64
	clients        map[uint64]*telemetryClient
	clientUpdates  chan *telemetryClientUpdate

	started  int32 // To be used atomically.
	shutdown int32 // To be used atomically.

	wg sync.WaitGroup
	sync.RWMutex

	quit chan struct{}
}

type TelemetryClient struct {
	TelemetryUpdates <-chan *TelemetryUpdate
	Cancel           func()
}

type telemetryClientUpdate struct {
	cancel        bool
	clientID      uint64
	telemetryChan chan<- *TelemetryUpdate
}

type TelemetryUpdate struct {
	MetricName string
	Value      uint64
}

type telemetryClient struct {
	updatesChan chan<- *TelemetryUpdate
	exit        chan struct{}
	wg          sync.WaitGroup
}

func NewTelemeter() (*telemeter, error) {

	return &telemeter{
		clients:       make(map[uint64]*telemetryClient),
		clientUpdates: make(chan *telemetryClientUpdate),
	}, nil
}

func (t *telemeter) Start() error {
	if atomic.AddInt32(&t.started, 1) != 1 {
		return nil
	}

	t.wg.Add(1)
	go t.handler()

	return nil
}

func (t *telemeter) handler() {
	defer t.wg.Done()

	for {
		select {
		case update := <-t.clientUpdates:
			clientID := update.clientID

			if update.cancel {
				t.RLock()
				client, ok := t.clients[clientID]
				t.RUnlock()

				if ok {
					t.Lock()
					delete(t.clients, clientID)
					t.Unlock()

					close(client.exit)
					client.wg.Wait()

					close(client.updatesChan)
				}

				continue
			}

			t.Lock()
			t.clients[update.clientID] = &telemetryClient{
				updatesChan: update.telemetryChan,
				exit:        make(chan struct{}),
			}
			t.Unlock()

		case <-t.quit:
			return
		}
	}
}

func (t *telemeter) Stop() error {
	if atomic.AddInt32(&t.shutdown, 1) != 1 {
		return nil
	}

	close(t.quit)

	return nil
}

func (t *telemeter) Subscribe() (*TelemetryClient, error) {
	clientID := atomic.AddUint64(&t.clientsCounter, 1)

	tlmrLog.Debugf("New telemetry client subscription, client %v",
		clientID)

	telemetryChan := make(chan *TelemetryUpdate, 10)

	select {
	case t.clientUpdates <- &telemetryClientUpdate{
		cancel:        false,
		clientID:      clientID,
		telemetryChan: telemetryChan,
	}:
	case <-t.quit:
		return nil, errors.New("Server shutting down")
	}

	return &TelemetryClient{
		TelemetryUpdates: telemetryChan,
		Cancel: func() {
			select {
			case t.clientUpdates <- &telemetryClientUpdate{
				cancel:   true,
				clientID: clientID,
			}:
			case <-t.quit:
				return
			}
		},
	}, nil
}

func (t *telemeter) Update(update *TelemetryUpdate) {
	t.RLock()
	numClients := len(t.clients)
	t.RUnlock()

	if numClients != 0 {
		tlmrLog.Tracef("Sending telemetry update to %v clients %v",
			numClients,
			newLogClosure(func() string {
				return spew.Sdump(update)
			}),
		)
	}

	t.RLock()
	for _, client := range t.clients {
		client.wg.Add(1)

		go func(c *telemetryClient) {
			defer c.wg.Done()

			select {

			case c.updatesChan <- update:

			case <-c.exit:

			case <-t.quit:

			}
		}(client)
	}
	t.RUnlock()
}
