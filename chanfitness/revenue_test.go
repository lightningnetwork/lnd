package chanfitness

import (
	"crypto/rand"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/wire"
)

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
