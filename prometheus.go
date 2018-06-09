package main

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"sync/atomic"

	"github.com/coreos/bbolt"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcutil"
)

func exportPrometheusStats(server *server) {
	ltndLog.Info("Adding static Prometheus stats and adding interceptors to gRPC server...")

	// Export some static data.
	versionGauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "lnd_version",
			Help: "Version of LND running.",
		},
		[]string{"version", "commit"})
	versionSimple := fmt.Sprintf("%d.%d.%d", appMajor, appMinor, appPatch)
	versionGauge.WithLabelValues(versionSimple, Commit).Set(1)
	prometheus.Register(versionGauge)

	prometheus.MustRegister(newPeerCollector(server))

	// TODO(baryluk): Merge into peerCollector.
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "lnd_peer_count",
			Help: "Number of server peers.",
		},
		func() float64 {
			return float64(len(server.Peers()))
		}))

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
			// or synced=true/false label.
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
			// or synced=true/false label.
			_ /*isSynced*/, bestHeaderTimestamp, err := server.cc.wallet.IsSynced()
			if err != nil {
				return math.NaN()
			}
			return float64(bestHeaderTimestamp)
		}))

	// Lightning network graph metadata

	// TODO(baryluk): Cache these stats for some time?
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "lnd_graph_node_count",
			Help: "What is the size of know network.",
		},
		func() float64 {
			// Obtain the pointer to the global singleton channel graph, this will
			// provide a consistent view of the graph due to bolt db's
			// transactional model.
			graph := server.chanDB.ChannelGraph()

			// First iterate through all the known nodes (connected or unconnected
			// within the graph), collating their current state into the RPC
			// response.
			var nodes uint64 = 0
			err := graph.ForEachNode(nil, func(_ *bolt.Tx, node *channeldb.LightningNode) error {
				nodes++
				// LastUpdate: uint32(node.LastUpdate.Unix()),
				return nil
			})
			if err != nil {
				return math.NaN()
			}
			return float64(nodes)
		}))
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

			// Next, for each active channel we know of within the graph, create a
			// similar response which details both the edge information as well as
			// the routing policies of th nodes connecting the two edges.
			var edges uint64 = 0
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

	// TODO(baryluk): Export registeredChains.ActiveChains() on startup.

	// Wallet balance.
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "lnd_wallet_total_balance",
			Help: "Total balance, from txs that have >= 0 confirmations.",
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
			Help: "Total balance, from txs that have >= 1 confirmations.",
		},
		func() float64 {
			confirmedBal, err := server.cc.wallet.ConfirmedBalance(1)
			if err != nil {
				return math.NaN()
			}
			return float64(confirmedBal)
		}))

	// Get unconfirmed balance, from txs with 0 confirmations.
	// unconfirmedBal := totalBal - confirmedBal

	prometheus.MustRegister(newChannelsCollector(server))

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
			Help: "Total .",
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
			Help: "Total .",
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
	server        *server
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
		pingDesc: prometheus.NewDesc(
			"lnd_peer_ping_by_peer",
			"Ping to peer by peer.",
			labels,
			nil),
		satSentDesc: prometheus.NewDesc(
			"lnd_peer_sat_sent_by_peer",
			"Satosish sent to peer by peer.",
			labels,
			nil),
		satRecvDesc: prometheus.NewDesc(
			"lnd_peer_sat_recv_by_peer",
			"Satosish sent to peer by peer.",
			labels,
			nil),
		bytesSentDesc: prometheus.NewDesc(
			"lnd_peer_bytes_sent_by_peer",
			"Satosish sent to peer by peer.",
			labels,
			nil),
		bytesRecvDesc: prometheus.NewDesc(
			"lnd_peer_bytes_recv_by_peer",
			"Bytes sent to peer by peer.",
			labels,
			nil),
	}
}

func (c *peerCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pingDesc
	ch <- c.satSentDesc
	ch <- c.satRecvDesc
	ch <- c.bytesSentDesc
	ch <- c.bytesRecvDesc
}

func (c *peerCollector) Collect(ch chan<- prometheus.Metric) {
	serverPeers := c.server.Peers()

	// TODO(baryluk): Number of IPv4 vs IPv6 peers.

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
		//	Address:   serverPeer.conn.RemoteAddr().String(),
		//	Inbound:   !serverPeer.inbound, // Flip for display

		labelValues := []string{pubKey}
		ch <- prometheus.MustNewConstMetric(c.pingDesc, prometheus.GaugeValue, float64(serverPeer.PingTime()), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.satSentDesc, prometheus.CounterValue, float64(satSent), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.satRecvDesc, prometheus.CounterValue, float64(satRecv), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.bytesRecvDesc, prometheus.CounterValue, float64(atomic.LoadUint64(&serverPeer.bytesReceived)), labelValues...)
		ch <- prometheus.MustNewConstMetric(c.bytesSentDesc, prometheus.CounterValue, float64(atomic.LoadUint64(&serverPeer.bytesSent)), labelValues...)
	}
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
			"Total of all fees across all transactions on chain.",
			nil, nil),
		receivedAmountTotalDesc: prometheus.NewDesc(
			"lnd_chain_transaction_received_amount_total",
			"Total amount of coins in all transactions with positive amounts.",
			nil, nil),
		sentAmountTotalDesc: prometheus.NewDesc(
			"lnd_chain_transaction_sent_amount_total",
			"Total amount of coins in all transactions with negative amounts.",
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

	var totalFees int64 = 0
	var totalReceived int64 = 0
	var totalSent int64 = 0
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
	}
}

func (c *invoicesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.countDesc
	ch <- c.pendingCountDesc
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

	var pendingInvoicesCount int64 = 0
	for _, invoice := range dbInvoices {
		if !invoice.Terms.Settled {
			pendingInvoicesCount++
		}
	}
	ch <- prometheus.MustNewConstMetric(c.pendingCountDesc, prometheus.GaugeValue, float64(pendingInvoicesCount))

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
	channel_labels := []string{"channel_id", "public"}
	return &channelsCollector{
		server: server,
		localBalanceDesc: prometheus.NewDesc(
			"lnd_channel_local_balance_by_channel",
			"Local balance of a channel by channel",
			channel_labels, nil),
		remoteBalanceDesc: prometheus.NewDesc(
			"lnd_channel_remote_balance_by_channel",
			"Remote balance of a channel by channel",
			channel_labels, nil),
		commitWeightDesc: prometheus.NewDesc(
			"lnd_channel_commit_weight_by_channel",
			"L...",
			channel_labels, nil),
		externalCommitFeeDesc: prometheus.NewDesc(
			"lnd_channel_external_commit_fee_by_channel",
			"L...",
			channel_labels, nil),
		capacityDesc: prometheus.NewDesc(
			"lnd_channel_capacity_by_channel",
			"L...",
			channel_labels, nil),
		feePerKwDesc: prometheus.NewDesc(
			"lnd_channel_fee_per_kw_by_channel",
			"L...",
			channel_labels, nil),
		totalSatoshisSentDesc: prometheus.NewDesc(
			"lnd_channel_satoshis_sent_by_channel",
			"L...",
			channel_labels, nil),
		totalSatoshisReceivedDesc: prometheus.NewDesc(
			"lnd_channel_satoshis_received_by_channel",
			"L...",
			channel_labels, nil),
		updatesCountDesc: prometheus.NewDesc(
			"lnd_channel_updates_count_by_channel",
			"Local commit height",
			channel_labels, nil),
		pendingHtlcsCountDesc: prometheus.NewDesc(
			"lnd_channel_pending_htlcs_count_by_channel",
			"Local commit HTLCs count",
			channel_labels, nil),
		csvDelayDesc: prometheus.NewDesc(
			"lnd_channel_csv_delay_by_channel",
			"Local channel config CSV delay",
			channel_labels, nil),
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
	active_count := 0
	public_count := 0

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
			active_count++
		}
		publicString := "false"
		if isPublic {
			publicString = "true"
			public_count++
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
