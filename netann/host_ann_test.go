package netann

import (
	"net"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/ticker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHostAnnouncerUpdates tests that the HostAnnouncer will properly announce
// a new set of addresses each time a target host changes and will noop if not
// change happens during an interval.
func TestHostAnnouncerUpdates(t *testing.T) {
	t.Parallel()

	hosts := []string{"test.com", "example.com"}
	startingAddrs := []net.Addr{
		&net.TCPAddr{
			IP: net.ParseIP("1.1.1.1"),
		},
		&net.TCPAddr{
			IP: net.ParseIP("8.8.8.8"),
		},
	}

	ticker := ticker.NewForce(time.Hour * 24)

	testTimeout := time.Millisecond * 200

	type annReq struct {
		newAddrs     []net.Addr
		removedAddrs map[string]struct{}
	}

	testCases := []struct {
		preAdvertisedIPs map[string]struct{}
		startingAddrs    []net.Addr

		preTickHosts  map[string]net.Addr
		postTickHosts map[string]net.Addr

		updateTriggered bool

		newAddrs     []net.Addr
		removedAddrs map[string]struct{}
	}{
		// The set of addresses are the same before and after a tick we
		// expect no change.
		{
			preTickHosts: map[string]net.Addr{
				"test.com": &net.TCPAddr{
					IP: net.ParseIP("1.1.1.1"),
				},
				"example.com": &net.TCPAddr{
					IP: net.ParseIP("8.8.8.8"),
				},
			},
			startingAddrs: startingAddrs,

			postTickHosts: map[string]net.Addr{
				"test.com": &net.TCPAddr{
					IP: net.ParseIP("1.1.1.1"),
				},
				"example.com": &net.TCPAddr{
					IP: net.ParseIP("8.8.8.8"),
				},
			},

			updateTriggered: false,
		},

		// Half of the addresses are changed out, the new one should be
		// added with the old one forgotten.
		{
			preTickHosts: map[string]net.Addr{
				"test.com": &net.TCPAddr{
					IP: net.ParseIP("1.1.1.1"),
				},
				"example.com": &net.TCPAddr{
					IP: net.ParseIP("8.8.8.8"),
				},
			},
			startingAddrs: startingAddrs,

			postTickHosts: map[string]net.Addr{
				"test.com": &net.TCPAddr{
					IP: net.ParseIP("1.1.1.1"),
				},
				"example.com": &net.TCPAddr{
					IP: net.ParseIP("9.9.9.9"),
				},
			},

			updateTriggered: true,
			newAddrs: []net.Addr{
				&net.TCPAddr{
					IP: net.ParseIP("9.9.9.9"),
				},
			},
			removedAddrs: map[string]struct{}{
				"8.8.8.8:0": {},
			},
		},

		// All addresses change, they should all be refreshed.
		{
			preTickHosts: map[string]net.Addr{
				"test.com": &net.TCPAddr{
					IP: net.ParseIP("1.1.1.1"),
				},
				"example.com": &net.TCPAddr{
					IP: net.ParseIP("8.8.8.8"),
				},
			},
			startingAddrs: startingAddrs,

			postTickHosts: map[string]net.Addr{
				"test.com": &net.TCPAddr{
					IP: net.ParseIP("2.2.2.2"),
				},
				"example.com": &net.TCPAddr{
					IP: net.ParseIP("9.9.9.9"),
				},
			},

			updateTriggered: true,
			newAddrs: []net.Addr{
				&net.TCPAddr{
					IP: net.ParseIP("2.2.2.2"),
				},
				&net.TCPAddr{
					IP: net.ParseIP("9.9.9.9"),
				},
			},
			removedAddrs: map[string]struct{}{
				"8.8.8.8:0": {},
				"1.1.1.1:0": {},
			},
		},

		// Two addresses, one has already been advertised on start up,
		// so we only expect one of them to be announced again. After
		// the tick we don't expect an update trigger since nothing.
		// changed.
		{
			preAdvertisedIPs: map[string]struct{}{
				"1.1.1.1:0": {},
			},
			startingAddrs: []net.Addr{
				&net.TCPAddr{
					IP: net.ParseIP("8.8.8.8"),
				},
			},
			preTickHosts: map[string]net.Addr{
				"test.com": &net.TCPAddr{
					IP: net.ParseIP("1.1.1.1"),
				},
				"example.com": &net.TCPAddr{
					IP: net.ParseIP("8.8.8.8"),
				},
			},
			postTickHosts: map[string]net.Addr{
				"test.com": &net.TCPAddr{
					IP: net.ParseIP("1.1.1.1"),
				},
				"example.com": &net.TCPAddr{
					IP: net.ParseIP("8.8.8.8"),
				},
			},

			updateTriggered: false,
		},
	}
	for idx, testCase := range testCases {
		hostResps := make(chan net.Addr)
		annReqs := make(chan annReq)
		hostAnncer := NewHostAnnouncer(HostAnnouncerConfig{
			Hosts:         hosts,
			AdvertisedIPs: testCase.preAdvertisedIPs,
			RefreshTicker: ticker,
			LookupHost: func(str string) (net.Addr, error) {
				return <-hostResps, nil
			},
			AnnounceNewIPs: func(newAddrs []net.Addr,
				removeAddrs map[string]struct{}) error {

				annReqs <- annReq{
					newAddrs:     newAddrs,
					removedAddrs: removeAddrs,
				}

				return nil
			},
		})
		if err := hostAnncer.Start(); err != nil {
			t.Fatalf("unable to start announcer: %v", err)
		}

		// As soon as the announcer starts, it'll try to query for the
		// state of the hosts. We'll return the preTick state for all
		// hosts.
		for i := 0; i < len(hosts); i++ {
			hostResps <- testCase.preTickHosts[hosts[i]]
		}

		// Since this is the first time the announcer is starting up,
		// we expect it to advertise the hosts as they exist before any
		// updates.
		select {
		case addrUpdate := <-annReqs:
			assert.Equal(
				t, testCase.startingAddrs, addrUpdate.newAddrs,
				"addresses should match",
			)
			assert.Empty(
				t, addrUpdate.removedAddrs,
				"removed addrs should match",
			)

		case <-time.After(testTimeout):
			t.Fatalf("#%v: no addr update sent", idx)
		}

		// We'll now force a tick which'll force another query. This
		// time we'll respond with the set of the hosts as they should
		// be post-tick.
		ticker.Force <- time.Time{}

		for i := 0; i < len(hosts); i++ {
			hostResps <- testCase.postTickHosts[hosts[i]]
		}

		// If we expect an update, then we'll assert that we received
		// the proper set of modified addresses.
		if testCase.updateTriggered {
			select {
			// The receive update should match exactly what the
			// test case dictates.
			case addrUpdate := <-annReqs:
				require.Equal(
					t, testCase.newAddrs, addrUpdate.newAddrs,
					"addresses should match",
				)

				require.Equal(
					t, testCase.removedAddrs, addrUpdate.removedAddrs,
					"removed addrs should match",
				)

			case <-time.After(testTimeout):
				t.Fatalf("#%v: no addr update set", idx)
			}

			if err := hostAnncer.Stop(); err != nil {
				t.Fatalf("unable to stop announcer: %v", err)
			}
			continue
		}

		// Otherwise, no updates should be sent since nothing changed.
		select {
		case <-annReqs:
			t.Fatalf("#%v: expected no call to AnnounceNewIPs", idx)

		case <-time.After(testTimeout):
		}

		if err := hostAnncer.Stop(); err != nil {
			t.Fatalf("unable to stop announcer: %v", err)
		}
	}
}
