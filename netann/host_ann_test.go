package netann

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testHost = "test.com:9735"

var (
	addr1 = &net.TCPAddr{IP: net.ParseIP("1.1.1.1"), Port: 9735}
	addr2 = &net.TCPAddr{IP: net.ParseIP("2.2.2.2"), Port: 9735}

	onionAddr = &tor.OnionAddr{
		OnionService: "abcdefghijklmnop.onion",
		Port:         9735,
	}
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
		initialAddrs  map[string]net.Addr
		startingAddrs []net.Addr

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

		// Both hosts resolve to the same IP and only one of them then
		// changes. The address they used to share must stay advertised,
		// since the host that didn't change still resolves to it.
		{
			preTickHosts: map[string]net.Addr{
				"test.com": &net.TCPAddr{
					IP: net.ParseIP("1.1.1.1"),
				},
				"example.com": &net.TCPAddr{
					IP: net.ParseIP("1.1.1.1"),
				},
			},
			startingAddrs: []net.Addr{
				&net.TCPAddr{
					IP: net.ParseIP("1.1.1.1"),
				},
				&net.TCPAddr{
					IP: net.ParseIP("1.1.1.1"),
				},
			},

			postTickHosts: map[string]net.Addr{
				"test.com": &net.TCPAddr{
					IP: net.ParseIP("2.2.2.2"),
				},
				"example.com": &net.TCPAddr{
					IP: net.ParseIP("1.1.1.1"),
				},
			},

			updateTriggered: true,
			newAddrs: []net.Addr{
				&net.TCPAddr{
					IP: net.ParseIP("2.2.2.2"),
				},
			},
			removedAddrs: map[string]struct{}{},
		},

		// Two addresses, one of which already resolved to the same IP
		// on start up and is therefore already advertised, so we only
		// expect the other one to be announced. After the tick we don't
		// expect an update trigger since nothing changed.
		{
			initialAddrs: map[string]net.Addr{
				"test.com": &net.TCPAddr{
					IP: net.ParseIP("1.1.1.1"),
				},
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
			InitialAddrs:  testCase.initialAddrs,
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

// annHarness drives a HostAnnouncer against a single host, applying the
// announcements it makes to nodeAnn the same way the server does, so that tests
// can assert on the address set we'd end up advertising.
type annHarness struct {
	t *testing.T

	nodeAnn *lnwire.NodeAnnouncement1
	ticker  *ticker.Force

	mu      sync.Mutex
	curAddr net.Addr

	resolved  chan struct{}
	announced chan struct{}
}

// newAnnHarness starts an announcer for a single host which currently resolves
// to curAddr, with nodeAnn as the announcement restored from disk and
// initialAddrs as the addresses the host resolved to at startup.
func newAnnHarness(t *testing.T, nodeAnn *lnwire.NodeAnnouncement1,
	initialAddrs map[string]net.Addr, curAddr net.Addr) *annHarness {

	h := &annHarness{
		t:         t,
		nodeAnn:   nodeAnn,
		ticker:    ticker.NewForce(time.Hour),
		curAddr:   curAddr,
		resolved:  make(chan struct{}, 10),
		announced: make(chan struct{}, 10),
	}

	annCer := NewHostAnnouncer(HostAnnouncerConfig{
		Hosts:         []string{testHost},
		RefreshTicker: h.ticker,
		InitialAddrs:  initialAddrs,
		LookupHost: func(string) (net.Addr, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			defer func() { h.resolved <- struct{}{} }()

			return h.curAddr, nil
		},
		AnnounceNewIPs: func(newAddrs []net.Addr,
			oldAddrs map[string]struct{}) error {

			err := IPAnnouncer(func(mods ...NodeAnnModifier) (
				lnwire.NodeAnnouncement1, error) {

				for _, mod := range mods {
					mod(h.nodeAnn)
				}

				return *h.nodeAnn, nil
			})(newAddrs, oldAddrs)

			h.announced <- struct{}{}

			return err
		},
	})
	require.NoError(t, annCer.Start())
	t.Cleanup(func() {
		require.NoError(t, annCer.Stop())
	})

	// Wait for the announcer's initial resolution so that the host's IP can
	// only change once it has taken stock of the current one.
	h.waitResolved()

	return h
}

// resolveTo points the host at addr and forces a refresh, blocking until the
// announcer has resolved the host again.
func (h *annHarness) resolveTo(addr net.Addr) {
	h.t.Helper()

	h.mu.Lock()
	h.curAddr = addr
	h.mu.Unlock()

	h.ticker.Force <- time.Now()
	h.waitResolved()
}

// waitResolved blocks until the announcer resolves the host.
func (h *annHarness) waitResolved() {
	h.t.Helper()

	select {
	case <-h.resolved:
	case <-time.After(time.Second):
		h.t.Fatal("host was never resolved")
	}
}

// assertAddrs waits for any in-flight announcement to be applied, then asserts
// on the addresses we'd advertise.
func (h *annHarness) assertAddrs(addrs ...net.Addr) {
	h.t.Helper()

	select {
	case <-h.announced:
	case <-time.After(100 * time.Millisecond):
	}

	require.Equal(h.t, addrs, h.nodeAnn.Addresses)
}

// TestHostAnnouncerRestart tests that after a restart the announcer replaces
// the address its host used to resolve to, rather than announcing the newly
// resolved address alongside it. Without this, every restart that follows an
// IP change would leave another dead IP behind in our node announcement.
func TestHostAnnouncerRestart(t *testing.T) {
	t.Parallel()

	// Our node announcement was restored from disk, and still advertises
	// the address our host resolved to when we last ran, alongside an onion
	// address.
	nodeAnn := &lnwire.NodeAnnouncement1{
		Addresses: []net.Addr{addr1, onionAddr},
	}
	initialAddrs := map[string]net.Addr{testHost: addr1}

	h := newAnnHarness(t, nodeAnn, initialAddrs, addr1)

	// Nothing has changed yet, so we should still advertise what we started
	// with.
	h.assertAddrs(addr1, onionAddr)

	// Our ISP now hands us a new IP, which our host resolves to.
	h.resolveTo(addr2)

	// The address the host used to resolve to is no longer reachable, so it
	// should be replaced rather than added to. Our onion address is
	// unrelated to the host and must survive.
	h.assertAddrs(onionAddr, addr2)
}

// TestHostAnnouncerAddrFlipBack tests that we correctly track our host's
// address when it changes back to one we advertised earlier in this run.
func TestHostAnnouncerAddrFlipBack(t *testing.T) {
	t.Parallel()

	nodeAnn := &lnwire.NodeAnnouncement1{
		Addresses: []net.Addr{addr1},
	}
	initialAddrs := map[string]net.Addr{testHost: addr1}

	h := newAnnHarness(t, nodeAnn, initialAddrs, addr1)

	// The host's address changes, which should swap out the old one.
	h.resolveTo(addr2)
	h.assertAddrs(addr2)

	// It now flips back to the address we started with. We should end up
	// advertising that address and nothing else, rather than holding on to
	// the address that is no longer reachable.
	h.resolveTo(addr1)
	h.assertAddrs(addr1)
}
