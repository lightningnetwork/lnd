package healthcheck

import "github.com/btcsuite/btcd/rpcclient"

// CheckOutboundPeers checks the number of outbound peers connected to the
// provided RPC client. If the number of outbound peers is below 6, a warning
// is logged. This function is intended to ensure that the chain backend
// maintains a healthy connection to the network.
func CheckOutboundPeers(bitcoinNode string,
	rpcConfig rpcclient.ConnConfig) error {

	switch bitcoinNode {
	case "bitcoind":
	case "btcd":
	default:
		return nil
	}

	client, err := rpcclient.New(&rpcConfig, nil)
	if err != nil {
		return err
	}

	peers, err := client.GetPeerInfo()
	if err != nil {
		return err
	}

	outboundPeers := 0
	for _, peer := range peers {
		if !peer.Inbound {
			outboundPeers += 1
		}
	}

	if outboundPeers < 6 {
		log.Warnf("The number of outbound peers (%d) is "+
			"below the expected minimum of 6.", outboundPeers)
	}

	return nil
}
