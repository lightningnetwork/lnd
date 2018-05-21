package main

import (
	"time"
	"github.com/quipo/statsd"
)

var (
	statsdInterval = time.Second * 2
)

const (
	statsdPrefix   = "lnd."
	statsdAddress  = "localhost:8125"
)

type statsdConnection struct {
	statsdClient *statsd.StatsdClient
	statsdBuffer *statsd.StatsdBuffer
}

// NewStatsdConnection opens a new statsd connection.
func NewStatsdConnection() (*statsdConnection, error) {
	client := statsd.NewStatsdClient(statsdAddress, statsdPrefix)

	err := client.CreateSocket()
	if err != nil {
		return nil, err
	}

	buffer := statsd.NewStatsdBuffer(statsdInterval, client)

	conn := &statsdConnection{
		statsdClient: client,
		statsdBuffer: buffer,
	}

	return conn, nil
}

// Close closes the statsd connection.
func (c *statsdConnection) Close() error {
	c.statsdBuffer.Close()

	return nil
}

// Increment increments the stat with the count.
func (c *statsdConnection) Increment(stat string, count int64) {
	c.statsdBuffer.Incr(stat, count)
}
