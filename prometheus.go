package main

import (
	"encoding/hex"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coreos/bbolt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/prometheus/client_golang/prometheus"
)

func addressType(addr string) string {
	if len(addr) >= 1 && addr[0] == '[' {
		return "ipv6"
	}
	if len(addr) >= 1 && '0' <= addr[0] && addr[0] <= '9' {
		return "ipv4"
	}
	return "unknown"
}

func exportPrometheusStats(server *server) {
	ltndLog.Info("Adding static Prometheus stats and adding interceptors to gRPC server...")

	// Export some static data.
	versionGauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "lnd_version",
			Help: "Version of LND running.",
		},
		[]string{"version", "commit"})
	versionSimple := build.Version()
	versionCommit := ""
	versionGauge.WithLabelValues(versionSimple, versionCommit).Set(1)
	prometheus.Register(versionGauge)

	startTime := time.Now()
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "lnd_uptime",
			Help: "Uptime of LND in seconds.",
		},
		func() float64 {
			return time.Now().Sub(startTime).Seconds()
		}))

	prometheus.MustRegister(newPeerCollector(server))

	// Could be a counter, as it makes sense to have blocks per hour metric.
	// But it could go back, so it is a gauge for now.
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "lnd_block_height",
			Help: "Height of the best chain.",
		},
		func() float64 {
			_, bestHeight, err := server.cc.chainIO.GetBestBlock()
			if err != nil {
				return math.NaN()
			}
			return float64(bestHeight)
		}))

	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "lnd_synced_to_chain",
			Help: "Is LND synced?",
		},
		func() float64 {
			isSynced, _ /*bestHeaderTimestamp*/, err := server.cc.wallet.IsSynced()
			if err != nil {
				return math.NaN()
			}
			if isSynced {
				return 1.0
			}
			return 0.0
		}))

	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "lnd_best_header_timestamp",
			Help: "What is the header timestamp of the best synced block in a chain.",
		},
		func() float64 {
			_ /*isSynced*/, bestHeaderTimestamp, err := server.cc.wallet.IsSynced()
			if err != nil {
				return math.NaN()
			}
			return float64(bestHeaderTimestamp)
		}))

	// Lightning network graph metadata
	// TODO(baryluk): Cache these stats for some time?
	prometheus.MustRegister(newNodesCollector(server))
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "lnd_graph_edge_count",
			Help: "What is the number of known edges in the network.",
		},
		func() float64 {
			// Obtain the pointer to the global singleton channel graph, this will
			// provide a consistent view of the graph due to bolt db's
			// transactional model.
			graph := server.chanDB.ChannelGraph()

			var edges uint64
			err := graph.ForEachChannel(func(edgeInfo *channeldb.ChannelEdgeInfo,
				c1, c2 *channeldb.ChannelEdgePolicy) error {
				edges++
				return nil
			})
			if err != nil {
				return math.NaN()
			}
			return float64(edges)
		}))

	prometheus.MustRegister(newChannelsCollector(server))

	// TODO(baryluk): Export registeredChains.ActiveChains() on startup.

	// Wallet balance.
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "lnd_wallet_total_balance",
			Help: "Total balance in satoshis, from txs that have >= 0 confirmations.",
		},
		func() float64 {
			totalBal, err := server.cc.wallet.ConfirmedBalance(0)
			if err != nil {
				return math.NaN()
			}
			return float64(totalBal)
		}))
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "lnd_wallet_confirmed_balance",
			Help: "Total balance in satoshis, from txs that have >= 1 confirmations.",
		},
		func() float64 {
			confirmedBal, err := server.cc.wallet.ConfirmedBalance(1)
			if err != nil {
				return math.NaN()
			}
			return float64(confirmedBal)
		}))
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "lnd_wallet_well_confirmed_balance",
			Help: "Total balance in satoshis, from txs that have >= 10 confirmations.",
		},
		func() float64 {
			confirmedBal, err := server.cc.wallet.ConfirmedBalance(10)
			if err != nil {
				return math.NaN()
			}
			return float64(confirmedBal)
		}))

	// Get unconfirmed balance, from txs with 0 confirmations.
	// unconfirmedBal := totalBal - confirmedBal

	// TODO(baryluk): Can be merged into channelsCollector to calculate
	// aggregates in single pass together with per-channel stats.
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "lnd_channels_active_count",
			Help: "Number of active channels across all peers.",
		},
		func() float64 {
			var activeChannels uint32
			serverPeers := server.Peers()
			for _, serverPeer := range serverPeers {
				activeChannels += uint32(len(serverPeer.ChannelSnapshots()))
			}
			return float64(activeChannels)
		}))
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "lnd_channels_pending_count",
			Help: "Number of pending channels.",
		},
		func() float64 {
			pendingChannels, err := server.chanDB.FetchPendingChannels()
			if err != nil {
				return math.NaN()
			}
			return float64(len(pendingChannels))
		}))
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "lnd_channel_balance_total",
			Help: "Total balance in satoshis across all channels.",
		},
		func() float64 {
			openChannels, err := server.chanDB.FetchAllOpenChannels()
			if err != nil {
				return math.NaN()
			}

			var balance btcutil.Amount
			for _, channel := range openChannels {
				balance += channel.LocalCommitment.LocalBalance.ToSatoshis()
			}
			return float64(int64(balance))
		}))
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "lnd_channel_pending_open_balance_total",
			Help: "Total balance in satoshis across all pending open channels.",
		},
		func() float64 {
			pendingChannels, err := server.chanDB.FetchPendingChannels()
			if err != nil {
				return math.NaN()
			}

			var pendingOpenBalance btcutil.Amount
			for _, channel := range pendingChannels {
				pendingOpenBalance += channel.LocalCommitment.LocalBalance.ToSatoshis()
			}
			return float64(int64(pendingOpenBalance))
		}))

	prometheus.MustRegister(newInvoicesCollector(server))

	prometheus.MustRegister(newTransactionCollector(server))
}

// We use custom collector, as this is more efficient that using Set on vectors.
// It also deals with peer disappering automatically.
type peerCollector struct {
	server              *server
	countDesc           *prometheus.Desc
	countByProtocolDesc *prometheus.Desc

	// ByPeer (pub_key)
	pingDesc      *prometheus.Desc
	satSentDesc   *prometheus.Desc
	satRecvDesc   *prometheus.Desc
	bytesSentDesc *prometheus.Desc
	bytesRecvDesc *prometheus.Desc
}

func newPeerCollector(server *server) prometheus.Collector {
	labels := []string{"pub_key"}
	return &peerCollector{
		server: server,
		countDesc: prometheus.NewDesc(
			"lnd_peer_count",
			"Number of server peers.",
			nil,
			nil),
		countByProtocolDesc: prometheus.NewDesc(
			"lnd_peer_count_by_protocol",
			"Number of server peers by protocol used for connection.",
			[]string{"protocol"},
			nil),
		pingDesc: prometheus.NewDesc(
			"lnd_peer_ping_by_peer",
			"Ping to peer in microseconds by peer.",
			labels,
			nil),
		satSentDesc: prometheus.NewDesc(
			"lnd_peer_sat_sent_by_peer",
			"Satosish sent to peer by peer.",
			labels,
			nil),
		satRecvDesc: prometheus.NewDesc(
			"lnd_peer_sat_recv_by_peer",
			"Satosish received from peer by peer.",
			labels,
			nil),
		bytesSentDesc: prometheus.NewDesc(
			"lnd_peer_bytes_sent_by_peer",
			"Bytes sent to peer by peer.",
			labels,
			nil),
		bytesRecvDesc: prometheus.NewDesc(
			"lnd_peer_bytes_recv_by_peer",
			"Bytes received from peer by peer.",
			labels,
			nil),
	}
}

func (c *peerCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.countDesc
	ch <- c.countByProtocolDesc
	ch <- c.pingDesc
	ch <- c.satSentDesc
	ch <- c.satRecvDesc
	ch <- c.bytesSentDesc
	ch <- c.bytesRecvDesc
}

func (c *peerCollector) Collect(ch chan<- prometheus.Metric) {
	serverPeers := c.server.Peers()

	ch <- prometheus.MustNewConstMetric(c.countDesc, prometheus.GaugeValue, float64(len(serverPeers)))

	var peerIPv4 uint64
	var peerIPv6 uint64
	var peerUnknownProtocol uint64

	for _, serverPeer := range serverPeers {
		var (
			satSent int64
			satRecv int64
		)

		// In order to display the total number of satoshis of outbound
		// (sent) and inbound (recv'd) satoshis that have been
		// transported through this peer, we'll sum up the sent/recv'd
		// values for each of the active channels we have with the
		// peer.
		chans := serverPeer.ChannelSnapshots()
		for _, c := range chans {
			satSent += int64(c.TotalMSatSent.ToSatoshis())
			satRecv += int64(c.TotalMSatReceived.ToSatoshis())
		}

		nodePub := serverPeer.addr.IdentityKey.SerializeCompressed()
		pubKey := hex.EncodeToString(nodePub)

		labelValues := []string{pubKey}
		ch <- prometheus.MustNewConstMetric(c.pingDesc, prometheus.GaugeValue, float64(serverPeer.PingTime()), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.satSentDesc, prometheus.CounterValue, float64(satSent), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.satRecvDesc, prometheus.CounterValue, float64(satRecv), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.bytesRecvDesc, prometheus.CounterValue, float64(atomic.LoadUint64(&serverPeer.bytesReceived)), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.bytesSentDesc, prometheus.CounterValue, float64(atomic.LoadUint64(&serverPeer.bytesSent)), labelValues...)

		t := addressType(serverPeer.addr.String())
		if t == "ipv4" {
			peerIPv4++
		} else if t == "ipv6" {
			peerIPv6++
		} else {
			peerUnknownProtocol++
		}
	}

	ch <- prometheus.MustNewConstMetric(c.countByProtocolDesc, prometheus.GaugeValue, float64(peerIPv4), "ipv4")
	ch <- prometheus.MustNewConstMetric(c.countByProtocolDesc, prometheus.GaugeValue, float64(peerIPv6), "ipv6")
	ch <- prometheus.MustNewConstMetric(c.countByProtocolDesc, prometheus.GaugeValue, float64(peerUnknownProtocol), "unknown")
}

// Custom collector, so we only do one atomic pass over transactions,
// instead of 4 passes.
type transactionCollector struct {
	server                  *server
	countDesc               *prometheus.Desc
	feeTotalDesc            *prometheus.Desc
	receivedAmountTotalDesc *prometheus.Desc
	sentAmountTotalDesc     *prometheus.Desc
}

func newTransactionCollector(server *server) prometheus.Collector {
	return &transactionCollector{
		server: server,
		countDesc: prometheus.NewDesc(
			"lnd_chain_transaction_count",
			"Number of all transactions on chain.",
			nil, nil),
		feeTotalDesc: prometheus.NewDesc(
			"lnd_chain_transaction_fee_total",
			"Total of all fees in satoshis across all transactions on chain.",
			nil, nil),
		receivedAmountTotalDesc: prometheus.NewDesc(
			"lnd_chain_transaction_received_amount_total",
			"Total amount of coins in satoshis in all transactions with positive amounts.",
			nil, nil),
		sentAmountTotalDesc: prometheus.NewDesc(
			"lnd_chain_transaction_sent_amount_total",
			"Total amount of coins in satoshis in all transactions with negative amounts.",
			nil, nil),
	}
}

func (c *transactionCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.countDesc
	ch <- c.feeTotalDesc
	ch <- c.receivedAmountTotalDesc
	ch <- c.sentAmountTotalDesc
}

func (c *transactionCollector) Collect(ch chan<- prometheus.Metric) {
	transactions, err := c.server.cc.wallet.ListTransactionDetails()
	if err != nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.countDesc, prometheus.CounterValue, float64(len(transactions)))

	var totalFees int64
	var totalReceived int64
	var totalSent int64
	for _, tx := range transactions {
		totalFees += tx.TotalFees
		if tx.Value > 0 {
			totalReceived += int64(tx.Value)
		} else {
			totalSent += -int64(tx.Value)
		}
	}

	ch <- prometheus.MustNewConstMetric(c.feeTotalDesc, prometheus.CounterValue, float64(totalFees))
	ch <- prometheus.MustNewConstMetric(c.receivedAmountTotalDesc, prometheus.CounterValue, float64(totalReceived))
	ch <- prometheus.MustNewConstMetric(c.sentAmountTotalDesc, prometheus.CounterValue, float64(totalSent))

	// TODO(baryluk): Extract newest transaction, as well sum of positive and negative transactions and total fees.
	// TODO(baryluk): Age of newest transaction (in block height / num confirmations or timestamp).
}

// Custom collector, so we only do one atomic pass over invoices, instead of 3.
type invoicesCollector struct {
	server           *server
	countDesc        *prometheus.Desc
	pendingCountDesc *prometheus.Desc
	settledCountDesc *prometheus.Desc
}

func newInvoicesCollector(server *server) prometheus.Collector {
	return &invoicesCollector{
		server: server,
		countDesc: prometheus.NewDesc(
			"lnd_invoices_count",
			"Number of all invoices.",
			nil, nil),
		pendingCountDesc: prometheus.NewDesc(
			"lnd_invoices_pending_count",
			"Number of pending invoices.",
			nil, nil),
		settledCountDesc: prometheus.NewDesc(
			"lnd_invoices_settled_count",
			"Number of settled invoices.",
			nil, nil),
	}
}

func (c *invoicesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.countDesc
	ch <- c.pendingCountDesc
	ch <- c.settledCountDesc
}

func (c *invoicesCollector) Collect(ch chan<- prometheus.Metric) {
	// TODO(baryluk): This could be done more efficiently, without copying all invoices.
	pendingOnly := false
	dbInvoices, err := c.server.chanDB.FetchAllInvoices(pendingOnly)
	if err != nil {
		return
	}

	allInvoicesCount := len(dbInvoices)
	ch <- prometheus.MustNewConstMetric(c.countDesc, prometheus.CounterValue, float64(allInvoicesCount))

	var pendingInvoicesCount int64
	var settledInvoicesCount int64
	for _, invoice := range dbInvoices {
		if !invoice.Terms.Settled {
			settledInvoicesCount++
		} else {
			pendingInvoicesCount++
		}
	}
	ch <- prometheus.MustNewConstMetric(c.pendingCountDesc, prometheus.GaugeValue, float64(pendingInvoicesCount))
	ch <- prometheus.MustNewConstMetric(c.settledCountDesc, prometheus.CounterValue, float64(settledInvoicesCount))

	// TODO(baryluk): Value in invoices.
}

// Custom collector, so we do not need to deal with expired channel garbage
// collection from metric vectors.
type channelsCollector struct {
	server                    *server
	localBalanceDesc          *prometheus.Desc
	remoteBalanceDesc         *prometheus.Desc
	commitWeightDesc          *prometheus.Desc
	externalCommitFeeDesc     *prometheus.Desc
	capacityDesc              *prometheus.Desc
	feePerKwDesc              *prometheus.Desc
	totalSatoshisSentDesc     *prometheus.Desc
	totalSatoshisReceivedDesc *prometheus.Desc
	updatesCountDesc          *prometheus.Desc
	pendingHtlcsCountDesc     *prometheus.Desc
	csvDelayDesc              *prometheus.Desc
}

func newChannelsCollector(server *server) prometheus.Collector {
	// We do not add 'active' as a label here, as this just pollutes information
	// about channels, as they flip between active and invactive.
	channelLabels := []string{"channel_id", "public"}
	return &channelsCollector{
		server: server,
		localBalanceDesc: prometheus.NewDesc(
			"lnd_channe_local_balance_by_channel",
			"Local balance of a channel in satoshis by channel",
			channelLabels, nil),
		remoteBalanceDesc: prometheus.NewDesc(
			"lnd_channel_remote_balance_by_channel",
			"Remote balance of a channel in satoshis by channel",
			channelLabels, nil),
		commitWeightDesc: prometheus.NewDesc(
			"lnd_channel_commit_weight_by_channel",
			"Commit weight by channel.",
			channelLabels, nil),
		externalCommitFeeDesc: prometheus.NewDesc(
			"lnd_channel_external_commit_fee_by_channel",
			"External commit fee in satoshis by channel.",
			channelLabels, nil),
		capacityDesc: prometheus.NewDesc(
			"lnd_channel_capacity_by_channel",
			"Capacity of the the channel in satoshis by channel",
			channelLabels, nil),
		feePerKwDesc: prometheus.NewDesc(
			"lnd_channel_fee_per_kw_by_channel",
			"Fee per kw in satoshis by channel",
			channelLabels, nil),
		totalSatoshisSentDesc: prometheus.NewDesc(
			"lnd_channel_satoshis_sent_by_channel",
			"Total sent in satoshis over channel by channel",
			channelLabels, nil),
		totalSatoshisReceivedDesc: prometheus.NewDesc(
			"lnd_channel_satoshis_received_by_channel",
			"Total received in satoshis over channel by channel",
			channelLabels, nil),
		updatesCountDesc: prometheus.NewDesc(
			"lnd_channel_updates_count_by_channel",
			"Local commit height by channel.",
			channelLabels, nil),
		pendingHtlcsCountDesc: prometheus.NewDesc(
			"lnd_channel_pending_htlcs_count_by_channel",
			"Local commit HTLCs count by channel.",
			channelLabels, nil),
		csvDelayDesc: prometheus.NewDesc(
			"lnd_channel_csv_delay_by_channel",
			"Local channel config CSV delay by channel.",
			channelLabels, nil),
	}
}

func (c *channelsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.localBalanceDesc
	ch <- c.remoteBalanceDesc
	ch <- c.commitWeightDesc
	ch <- c.externalCommitFeeDesc
	ch <- c.capacityDesc
	ch <- c.feePerKwDesc
	ch <- c.totalSatoshisSentDesc
	ch <- c.totalSatoshisReceivedDesc
	ch <- c.updatesCountDesc
	ch <- c.pendingHtlcsCountDesc
	ch <- c.csvDelayDesc
}

func (c *channelsCollector) Collect(ch chan<- prometheus.Metric) {
	// Per-channel related stats
	graph := c.server.chanDB.ChannelGraph()

	dbChannels, err := c.server.chanDB.FetchAllOpenChannels()
	if err != nil {
		return
	}
	activeCount := 0
	publicCount := 0

	for _, dbChannel := range dbChannels {
		nodePub := dbChannel.IdentityPub
		// nodeID := hex.EncodeToString(nodePub.SerializeCompressed())
		chanPoint := dbChannel.FundingOutpoint

		// With the channel point known, retrieve the network channel
		// ID from the database.
		var chanID uint64
		chanID, _ = graph.ChannelID(&chanPoint)

		var peerOnline bool
		if _, err := c.server.FindPeer(nodePub); err == nil {
			peerOnline = true
		}

		channelID := lnwire.NewChanIDFromOutPoint(&chanPoint)
		var linkActive bool
		if link, err := c.server.htlcSwitch.GetLink(channelID); err == nil {
			// A channel is only considered active if it is known
			// by the switch *and* able to forward
			// incoming/outgoing payments.
			linkActive = link.EligibleToForward()
		}

		// Next, we'll determine whether we should add this channel to
		// our list depending on the type of channels requested to us.
		isActive := peerOnline && linkActive
		isPublic := dbChannel.ChannelFlags&lnwire.FFAnnounceChannel != 0

		// As this is required for display purposes, we'll calculate
		// the weight of the commitment transaction. We also add on the
		// estimated weight of the witness to calculate the weight of
		// the transaction if it were to be immediately unilaterally
		// broadcast.
		localCommit := dbChannel.LocalCommitment
		utx := btcutil.NewTx(localCommit.CommitTx)
		commitBaseWeight := blockchain.GetTransactionWeight(utx)
		commitWeight := commitBaseWeight + lnwallet.WitnessCommitmentTxWeight

		localBalance := localCommit.LocalBalance
		remoteBalance := localCommit.RemoteBalance

		// As an artifact of our usage of mSAT internally, either party
		// may end up in a state where they're holding a fractional
		// amount of satoshis which can't be expressed within the
		// actual commitment output. Since we round down when going
		// from mSAT -> SAT, we may at any point be adding an
		// additional SAT to miners fees. As a result, we display a
		// commitment fee that accounts for this externally.
		var sumOutputs btcutil.Amount
		for _, txOut := range localCommit.CommitTx.TxOut {
			sumOutputs += btcutil.Amount(txOut.Value)
		}
		externalCommitFee := dbChannel.Capacity - sumOutputs

		if isActive {
			activeCount++
		}
		publicString := "false"
		if isPublic {
			publicString = "true"
			publicCount++
		}
		labelValues := []string{strconv.FormatUint(chanID, 10), publicString}

		ch <- prometheus.MustNewConstMetric(c.localBalanceDesc, prometheus.GaugeValue, float64(int64(localBalance.ToSatoshis())), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.remoteBalanceDesc, prometheus.GaugeValue, float64(int64(remoteBalance.ToSatoshis())), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.commitWeightDesc, prometheus.GaugeValue, float64(commitWeight), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.externalCommitFeeDesc, prometheus.GaugeValue, float64(int64(externalCommitFee)), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.capacityDesc, prometheus.GaugeValue, float64(int64(dbChannel.Capacity)), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.feePerKwDesc, prometheus.GaugeValue, float64(int64(localCommit.FeePerKw)), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.totalSatoshisSentDesc, prometheus.GaugeValue, float64(int64(dbChannel.TotalMSatSent.ToSatoshis())), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.totalSatoshisReceivedDesc, prometheus.GaugeValue, float64(int64(dbChannel.TotalMSatReceived.ToSatoshis())), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.updatesCountDesc, prometheus.GaugeValue, float64(localCommit.CommitHeight), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.pendingHtlcsCountDesc, prometheus.GaugeValue, float64(len(localCommit.Htlcs)), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.csvDelayDesc, prometheus.GaugeValue, float64(uint32(dbChannel.LocalChanCfg.CsvDelay)), labelValues...)

		// TODO(baryluk): oldest, newest, biggest Htlcs (from localCommit.Htlcs[].Amt.ToSatoshi())
	}

	// TODO(baryluk): Some totals that are useful to have across inactive channels: total satoshis sent / received, commit fee totals.
}

// Custom collector for stats about nodes on network.
type nodesCollector struct {
	server                         *server
	nodeCountDesc                  *prometheus.Desc
	nodeAddressCountDesc           *prometheus.Desc
	nodeAddressCountByProtocolDesc *prometheus.Desc
	nodeWithoutAddressCountDesc    *prometheus.Desc
}

func newNodesCollector(server *server) prometheus.Collector {
	return &nodesCollector{
		server: server,
		nodeCountDesc: prometheus.NewDesc(
			"lnd_graph_node_count",
			"What is the size of known network.",
			nil, nil),
		nodeAddressCountDesc: prometheus.NewDesc(
			"lnd_graph_node_address_count",
			"Number of all known addresses across all nodes.",
			nil, nil),
		nodeAddressCountByProtocolDesc: prometheus.NewDesc(
			"lnd_graph_node_address_count_by_protocol",
			"Number of all known addresses across all nodes by protocol. Note that some nodes can have multiple addresses even on same protocol. And some nodes might have no known addresses.",
			[]string{"protocol"}, nil),
		nodeWithoutAddressCountDesc: prometheus.NewDesc(
			"lnd_graph_node_without_address_count",
			"Number of all known addresses across all nodes.",
			nil, nil),
	}
}

func (c *nodesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.nodeCountDesc
	ch <- c.nodeAddressCountDesc
	ch <- c.nodeAddressCountByProtocolDesc
	ch <- c.nodeWithoutAddressCountDesc
}

func (c *nodesCollector) Collect(ch chan<- prometheus.Metric) {
	// Obtain the pointer to the global singleton channel graph, this will
	// provide a consistent view of the graph due to bolt db's
	// transactional model.
	graph := c.server.chanDB.ChannelGraph()

	var nodes uint64

	var addressesAll uint64
	var addressesTCPIPv4 uint64
	var addressesTCPIPv6 uint64
	var addressesTCPUnknown uint64
	var addressesUnix uint64
	var addressesUnknown uint64 // exotic transports (udp, sctp, quik, http/2, etc).

	var nodesWithoutAddress uint64

	protocols := map[string]int{}

	// TODO(baryluk): Nodes without channels.

	err := graph.ForEachNode(nil, func(_ *bolt.Tx, node *channeldb.LightningNode) error {
		nodes++

		if len(node.Addresses) == 0 {
			nodesWithoutAddress++
		}

		addressesAll += uint64(len(node.Addresses))
		for _, addr := range node.Addresses {
			protocols[addr.Network()]++
			// Do not assume anything about address format of non-tcp network.
			if addr.Network() == "tcp" {
				t := addressType(addr.String())
				if t == "ipv6" {
					addressesTCPIPv6++
				} else if t == "ipv4" {
					addressesTCPIPv4++
				} else {
					addressesTCPUnknown++
				}
			} else if strings.HasPrefix("unix", addr.Network()) {
				addressesUnix++
			} else {
				addressesUnknown++
			}
		}

		return nil
	})
	if err != nil {
		return
	}

	ch <- prometheus.MustNewConstMetric(c.nodeCountDesc, prometheus.GaugeValue, float64(nodes))
	ch <- prometheus.MustNewConstMetric(c.nodeAddressCountDesc, prometheus.GaugeValue, float64(addressesAll))
	ch <- prometheus.MustNewConstMetric(c.nodeAddressCountByProtocolDesc, prometheus.GaugeValue, float64(addressesTCPIPv4), "ipv4")
	ch <- prometheus.MustNewConstMetric(c.nodeAddressCountByProtocolDesc, prometheus.GaugeValue, float64(addressesTCPIPv6), "ipv6")
	ch <- prometheus.MustNewConstMetric(c.nodeAddressCountByProtocolDesc, prometheus.GaugeValue, float64(addressesUnix), "unix")
	ch <- prometheus.MustNewConstMetric(c.nodeAddressCountByProtocolDesc, prometheus.GaugeValue, float64(addressesUnknown), "unknown")
	ch <- prometheus.MustNewConstMetric(c.nodeWithoutAddressCountDesc, prometheus.GaugeValue, float64(nodesWithoutAddress))
}
