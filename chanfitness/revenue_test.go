package chanfitness

import (
	"crypto/rand"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// TestGetEvents tests the repeated querying of an events function to obtain
// all results from a paginated database query. It mocks the database query, to
// avoid the need for database setup by setting the number of event each call
// should return and checks whether all expected events are retrieved.
func TestGetEvents(t *testing.T) {
	var txid [32]byte

	if _, err := rand.Read(txid[:]); err != nil {
		t.Fatalf("cannot generate channel id: %v", err)
	}

	// channelIDFound is a map that will successfully lookup an outpoint for
	// a zero channel ID.
	channelIDFound := map[lnwire.ShortChannelID]wire.OutPoint{
		{}: {
			Hash:  txid,
			Index: 1,
		},
	}

	tests := []struct {
		name string

		// queryResponses contains the number of forwarding events the test's
		// mocked query function should return on each sequential call. The
		// index of an item in this array represents the call count and the
		// value is the number of events that should be returned. For example,
		// queryResponses = [10, 2] means that the query should return 10
		// events on first call, followed by 2 events on the second call.
		// Any calls thereafter will panic.
		queryResponses []uint32

		channelMap map[lnwire.ShortChannelID]wire.OutPoint

		// expectedEvents is the number of events we expect to be accumulated.
		expectedEvents uint32

		// expectedError is the error we expect to be returned.
		expectedError error
	}{
		{
			name:           "no events",
			queryResponses: []uint32{0},
			channelMap:     channelIDFound,
			expectedEvents: 0,
			expectedError:  nil,
		},
		{
			name:           "single query",
			queryResponses: []uint32{maxQueryEvents / 2},
			channelMap:     channelIDFound,
			expectedEvents: maxQueryEvents / 2,
			expectedError:  nil,
		},
		{
			name:           "paginated queries",
			queryResponses: []uint32{maxQueryEvents, maxQueryEvents / 2},
			channelMap:     channelIDFound,
			expectedEvents: maxQueryEvents + maxQueryEvents/2,
			expectedError:  nil,
		},
		{
			name:           "can't lookup channel",
			queryResponses: []uint32{maxQueryEvents / 2},
			channelMap:     make(map[lnwire.ShortChannelID]wire.OutPoint),
			expectedEvents: 0,
			expectedError:  errUnknownChannelID,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			// Track the number of calls we have made to the mocked query function.
			i := 0

			query := func(offset, maxEvents uint32) (channeldb.ForwardingLogTimeSlice, error) {
				// Get the number of forward responses the mocked function should
				// return from the test.
				count := test.queryResponses[i]
				i++

				// Return a time slice with the correct number of events.
				return channeldb.ForwardingLogTimeSlice{
					ForwardingEventQuery: channeldb.ForwardingEventQuery{
						IndexOffset:  offset,
						NumMaxEvents: maxEvents,
					},
					ForwardingEvents: make([]channeldb.ForwardingEvent, count),
				}, nil
			}

			events, err := getEvents(test.channelMap, query)
			if err != test.expectedError {
				t.Fatalf("Expected error: %v, got: %v", test.expectedError,
					err)
			}

			// Check that we have accumulated the number of events we expect.
			if len(events) != int(test.expectedEvents) {
				t.Fatalf("Expected %v evetns, got: %v",
					test.expectedEvents, len(events))
			}
		})
	}
}

// TestRevenueReport tests creation of a revenue report for a set of
// revenue events. It covers the case where there are no events, and
// the case where one channel is involved in multiple forwards.
func TestRevenueReport(t *testing.T) {
	var (
		txid1 [32]byte
		txid2 [32]byte
	)

	// Read random bytes into txids.
	if _, err := rand.Read(txid1[:]); err != nil {
		t.Fatalf("cannot generate random txid: %v", err)
	}

	if _, err := rand.Read(txid2[:]); err != nil {
		t.Fatalf("cannot generate random txid: %v", err)
	}

	channel1 := wire.OutPoint{
		Hash:  txid1,
		Index: 1,
	}

	channel2 := wire.OutPoint{
		Hash:  txid2,
		Index: 2,
	}

	// chan1Incoming is a forwarding event where channel 1 is the incoming channel.
	chan1Incoming := revenueEvent{
		incomingChannel: channel1,
		outgoingChannel: channel2,
		incomingAmt:     1000,
		outgoingAmt:     500,
	}

	// chan1Outgoing is a forwarding event where channel1 is the outgoing channel.
	chan1Outgoing := revenueEvent{
		incomingChannel: channel2,
		outgoingChannel: channel1,
		incomingAmt:     400,
		outgoingAmt:     200,
	}

	// chan2Event is a forwarding event that channel1 is not involved in.
	chan2Event := revenueEvent{
		incomingChannel: channel2,
		outgoingChannel: channel2,
		incomingAmt:     100,
		outgoingAmt:     90,
	}

	tests := []struct {
		name              string
		events            []revenueEvent
		attributeIncoming float64
		expectedReport    *RevenueReport
	}{
		{
			name:              "no events",
			events:            []revenueEvent{},
			attributeIncoming: 0.5,
			expectedReport: &RevenueReport{
				ChannelPairs: make(map[wire.OutPoint]map[wire.OutPoint]Revenue),
			},
		},
		{
			name:              "multiple forwards for one channel",
			attributeIncoming: 0.5,
			events: []revenueEvent{
				chan1Incoming,
				chan1Outgoing,
				chan2Event,
			},
			expectedReport: &RevenueReport{
				ChannelPairs: map[wire.OutPoint]map[wire.OutPoint]Revenue{
					channel1: {
						channel2: {
							AmountOutgoing: 200,
							AmountIncoming: 1000,
							FeesOutgoing:   100,
							FeesIncoming:   250,
						},
					},
					channel2: {
						channel1: {
							AmountOutgoing: 500,
							FeesOutgoing:   250,
							AmountIncoming: 400,
							FeesIncoming:   100,
						},
						channel2: {
							AmountOutgoing: 90,
							FeesOutgoing:   5,
							AmountIncoming: 100,
							FeesIncoming:   5,
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			report := revenueReport(test.events, test.attributeIncoming)

			if !reflect.DeepEqual(report, test.expectedReport) {
				t.Fatalf("expected revenue: %v, got: %v",
					test.expectedReport, report)
			}
		})
	}
}
