package netann

import (
	"maps"
	"net"
	"sync"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/ticker"
)

// HostAnnouncerConfig is the main config for the HostAnnouncer.
type HostAnnouncerConfig struct {
	// Hosts is the set of hosts we should watch for IP changes.
	Hosts []string

	// RefreshTicker ticks each time we should check for any address
	// changes.
	RefreshTicker ticker.Ticker

	// LookupHost performs DNS resolution on a given host and returns its
	// addresses.
	LookupHost func(string) (net.Addr, error)

	// InitialAddrs maps each host to the address it resolved to when the
	// backing node started up, and which it therefore already advertises.
	// Seeding the announcer with this lets it recognise a later IP change
	// as a change rather than as a brand new address, so that the address
	// the host used to point at is removed instead of accumulating
	// alongside the new one. Hosts that failed to resolve at startup are
	// absent.
	//
	// Two invariants must hold, or the affected host silently reverts to
	// accumulating stale addresses across restarts with no error:
	//
	//   - Each key must be byte-identical to its Hosts entry, since the
	//     watcher looks these up by the exact strings in Hosts.
	//   - Each value must share the String() normalization that LookupHost
	//     produces, since a change is detected by comparing the two.
	//
	// The simplest way to satisfy both is to derive the keys, values and
	// Hosts from a single source resolved with the same LookupHost.
	InitialAddrs map[string]net.Addr

	// AnnounceNewIPs announces a new set of IP addresses for the backing
	// Lightning node. The first set of addresses is the new set of
	// addresses that we should advertise, while the other set are the
	// stale addresses that we should no longer advertise.
	AnnounceNewIPs func([]net.Addr, map[string]struct{}) error
}

// HostAnnouncer is a sub-system that allows a user to specify a set of hosts
// for lnd that will be continually resolved to notice any IP address changes.
// If the target IP address for a host changes, then we'll generate a new
// NodeAnnouncement1 that includes these new IPs.
type HostAnnouncer struct {
	cfg HostAnnouncerConfig

	quit chan struct{}
	wg   sync.WaitGroup

	startOnce sync.Once
	stopOnce  sync.Once
}

// NewHostAnnouncer returns a new instance of the HostAnnouncer.
func NewHostAnnouncer(cfg HostAnnouncerConfig) *HostAnnouncer {
	return &HostAnnouncer{
		cfg:  cfg,
		quit: make(chan struct{}),
	}
}

// Start starts the HostAnnouncer.
func (h *HostAnnouncer) Start() error {
	h.startOnce.Do(func() {
		log.Info("HostAnnouncer starting")
		h.wg.Add(1)
		go h.hostWatcher()
	})

	return nil
}

// Stop signals the HostAnnouncer for a graceful stop.
func (h *HostAnnouncer) Stop() error {
	h.stopOnce.Do(func() {
		log.Info("HostAnnouncer shutting down...")
		defer log.Debug("HostAnnouncer shutdown complete")

		close(h.quit)
		h.wg.Wait()
	})

	return nil
}

// hostWatcher periodically attempts to resolve the IP for each host, updating
// them if they change within the interval.
func (h *HostAnnouncer) hostWatcher() {
	defer h.wg.Done()

	// Start from the addresses our hosts resolved to when the node started
	// up. Without this we'd treat the first resolution after a restart as a
	// brand new address and simply append it, letting the IPs the host used
	// to point at pile up in our node announcement across restarts.
	ipMapping := make(map[string]net.Addr, len(h.cfg.InitialAddrs))
	maps.Copy(ipMapping, h.cfg.InitialAddrs)

	refreshHosts := func() {
		// We'll now run through each of our hosts to check if they had
		// their backing IPs changed. If so, we'll want to re-announce
		// them.
		var addrsToUpdate []net.Addr
		addrsToRemove := make(map[string]struct{})
		for _, host := range h.cfg.Hosts {
			newAddr, err := h.cfg.LookupHost(host)
			if err != nil {
				log.Warnf("unable to resolve IP for "+
					"host %v: %v", host, err)
				continue
			}

			// If nothing has changed since the last time we
			// checked, then we don't need to do any updates.
			oldAddr, oldAddrFound := ipMapping[host]
			if oldAddrFound && oldAddr.String() == newAddr.String() {
				continue
			}

			ipMapping[host] = newAddr

			// If we've reached this point, then the address this
			// host resolves to differs from the one we last saw.
			log.Debugf("IP change detected! %v: %v -> %v", host,
				oldAddr, newAddr)

			// If we had already advertised an addr for this host,
			// then we'll need to remove that old stale address.
			if oldAddr != nil {
				addrsToRemove[oldAddr.String()] = struct{}{}
			}

			addrsToUpdate = append(addrsToUpdate, newAddr)
		}

		// If we don't have any addresses to update, then we can skip
		// things around until the next round.
		if len(addrsToUpdate) == 0 {
			log.Debugf("No IP changes detected for hosts: %v",
				h.cfg.Hosts)
			return
		}

		// Now that we know the set of IPs we need to update, we'll do
		// them all in a single batch.
		err := h.cfg.AnnounceNewIPs(addrsToUpdate, addrsToRemove)
		if err != nil {
			log.Warnf("unable to announce new IPs: %v", err)
		}
	}

	refreshHosts()

	h.cfg.RefreshTicker.Resume()

	for {
		select {
		case <-h.cfg.RefreshTicker.Ticks():
			log.Debugf("HostAnnouncer checking for any IP " +
				"changes...")

			refreshHosts()

		case <-h.quit:
			return
		}
	}
}

// NodeAnnUpdater describes a function that's able to update our current node
// announcement on disk. It returns the updated node announcement given a set
// of updates to be applied to the current node announcement.
type NodeAnnUpdater func(modifier ...NodeAnnModifier,
) (lnwire.NodeAnnouncement1, error)

// IPAnnouncer is a factory function that generates a new function that uses
// the passed annUpdater function to to announce new IP changes for a given
// host.
func IPAnnouncer(annUpdater NodeAnnUpdater) func([]net.Addr,
	map[string]struct{}) error {

	return func(newAddrs []net.Addr, oldAddrs map[string]struct{}) error {
		_, err := annUpdater(func(
			currentNodeAnn *lnwire.NodeAnnouncement1) {
			// To ensure we don't duplicate any addresses, we'll
			// filter out the same of addresses we should no longer
			// advertise.
			filteredAddrs := make(
				[]net.Addr, 0, len(currentNodeAnn.Addresses),
			)
			for _, addr := range currentNodeAnn.Addresses {
				if _, ok := oldAddrs[addr.String()]; ok {
					continue
				}

				filteredAddrs = append(filteredAddrs, addr)
			}

			filteredAddrs = append(filteredAddrs, newAddrs...)
			currentNodeAnn.Addresses = filteredAddrs
		})
		return err
	}
}
