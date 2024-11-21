package chainfee

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// fetchFilterInterval is the interval between successive fetches of
	// our peers' feefilters.
	fetchFilterInterval = time.Minute * 5

	// minNumFilters is the minimum number of feefilters we need during a
	// polling interval. If we have fewer than this, we won't consider the
	// data.
	minNumFilters = 6
)

var (
	// errNoData is an error that's returned if fetchMedianFilter is
	// called and there is no data available.
	errNoData = fmt.Errorf("no data available")
)

// filterManager is responsible for determining an acceptable minimum fee to
// use based on our peers' feefilter values.
type filterManager struct {
	// median stores the median of our outbound peer's feefilter values.
	median    fn.Option[SatPerKWeight]
	medianMtx sync.RWMutex

	fetchFunc func() ([]SatPerKWeight, error)

	wg   sync.WaitGroup
	quit chan struct{}
}

// newFilterManager constructs a filterManager with a callback that fetches the
// set of peers' feefilters.
func newFilterManager(cb func() ([]SatPerKWeight, error)) *filterManager {
	f := &filterManager{
		quit: make(chan struct{}),
	}

	f.fetchFunc = cb

	return f
}

// Start starts the filterManager.
func (f *filterManager) Start() {
	f.wg.Add(1)
	go f.fetchPeerFilters()
}

// Stop stops the filterManager.
func (f *filterManager) Stop() {
	close(f.quit)
	f.wg.Wait()
}

// fetchPeerFilters fetches our peers' feefilter values and calculates the
// median.
func (f *filterManager) fetchPeerFilters() {
	defer f.wg.Done()

	filterTicker := time.NewTicker(fetchFilterInterval)
	defer filterTicker.Stop()

	for {
		select {
		case <-filterTicker.C:
			filters, err := f.fetchFunc()
			if err != nil {
				log.Errorf("Encountered err while fetching "+
					"fee filters: %v", err)
				return
			}

			f.updateMedian(filters)

		case <-f.quit:
			return
		}
	}
}

// fetchMedianFilter fetches the median feefilter value.
func (f *filterManager) FetchMedianFilter() (SatPerKWeight, error) {
	f.medianMtx.RLock()
	defer f.medianMtx.RUnlock()

	// If there is no median, return errNoData so the caller knows to
	// ignore the output and continue.
	return f.median.UnwrapOrErr(errNoData)
}

type bitcoindPeerInfoResp struct {
	Inbound      bool    `json:"inbound"`
	MinFeeFilter float64 `json:"minfeefilter"`
}

func fetchBitcoindFilters(client *rpcclient.Client) ([]SatPerKWeight, error) {
	resp, err := client.RawRequest("getpeerinfo", nil)
	if err != nil {
		return nil, err
	}

	var peerResps []bitcoindPeerInfoResp
	err = json.Unmarshal(resp, &peerResps)
	if err != nil {
		return nil, err
	}

	// We filter for outbound peers since it is harder for an attacker to
	// be our outbound peer and therefore harder for them to manipulate us
	// into broadcasting high-fee or low-fee transactions.
	var outboundPeerFilters []SatPerKWeight
	for _, peerResp := range peerResps {
		if peerResp.Inbound {
			continue
		}

		// The value that bitcoind returns for the "minfeefilter" field
		// is in fractions of a bitcoin that represents the satoshis
		// per KvB. We need to convert this fraction to whole satoshis
		// by multiplying with COIN. Then we need to convert the
		// sats/KvB to sats/KW.
		//
		// Convert the sats/KvB from fractions of a bitcoin to whole
		// satoshis.
		filterKVByte := SatPerKVByte(
			peerResp.MinFeeFilter * btcutil.SatoshiPerBitcoin,
		)

		if !isWithinBounds(filterKVByte) {
			continue
		}

		// Convert KvB to KW and add it to outboundPeerFilters.
		outboundPeerFilters = append(
			outboundPeerFilters, filterKVByte.FeePerKWeight(),
		)
	}

	// Check that we have enough data to use. We don't return an error as
	// that would stop the querying goroutine.
	if len(outboundPeerFilters) < minNumFilters {
		return nil, nil
	}

	return outboundPeerFilters, nil
}

func fetchBtcdFilters(client *rpcclient.Client) ([]SatPerKWeight, error) {
	resp, err := client.GetPeerInfo()
	if err != nil {
		return nil, err
	}

	var outboundPeerFilters []SatPerKWeight
	for _, peerResp := range resp {
		// We filter for outbound peers since it is harder for an
		// attacker to be our outbound peer and therefore harder for
		// them to manipulate us into broadcasting high-fee or low-fee
		// transactions.
		if peerResp.Inbound {
			continue
		}

		// The feefilter is already in units of sat/KvB.
		filter := SatPerKVByte(peerResp.FeeFilter)

		if !isWithinBounds(filter) {
			continue
		}

		outboundPeerFilters = append(
			outboundPeerFilters, filter.FeePerKWeight(),
		)
	}

	// Check that we have enough data to use. We don't return an error as
	// that would stop the querying goroutine.
	if len(outboundPeerFilters) < minNumFilters {
		return nil, nil
	}

	return outboundPeerFilters, nil
}

// updateMedian takes a slice of feefilter values, computes the median, and
// updates our stored median value.
func (f *filterManager) updateMedian(feeFilters []SatPerKWeight) {
	// If there are no elements, don't update.
	numElements := len(feeFilters)
	if numElements == 0 {
		return
	}

	f.medianMtx.Lock()
	defer f.medianMtx.Unlock()

	// Log the new median.
	median := med(feeFilters)
	f.median = fn.Some(median)
	log.Debugf("filterManager updated moving median to: %v",
		median.FeePerKVByte())
}

// isWithinBounds returns false if the filter is unusable and true if it is.
func isWithinBounds(filter SatPerKVByte) bool {
	// Ignore values of 0 and MaxSatoshi. A value of 0 likely means that
	// the peer hasn't sent over a feefilter and a value of MaxSatoshi
	// means the peer is using bitcoind and is in IBD.
	switch filter {
	case 0:
		return false

	case btcutil.MaxSatoshi:
		return false
	}

	return true
}

// med calculates the median of a slice.
// NOTE: Passing in an empty slice will panic!
func med(f []SatPerKWeight) SatPerKWeight {
	// Copy the original slice so that sorting doesn't modify the original.
	fCopy := make([]SatPerKWeight, len(f))
	copy(fCopy, f)

	sort.Slice(fCopy, func(i, j int) bool {
		return fCopy[i] < fCopy[j]
	})

	var median SatPerKWeight

	numElements := len(fCopy)
	switch numElements % 2 {
	case 0:
		// There's an even number of elements, so we need to average.
		middle := numElements / 2
		upper := fCopy[middle]
		lower := fCopy[middle-1]
		median = (upper + lower) / 2

	case 1:
		median = fCopy[numElements/2]
	}

	return median
}
