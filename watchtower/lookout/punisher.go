package lookout

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/labels"
)

// PunisherConfig houses the resources required by the Punisher.
type PunisherConfig struct {
	// PublishTx provides the ability to send a signed transaction to the
	// network.
	PublishTx func(*wire.MsgTx, string) error

	// TODO(conner) add DB tracking and spend ntfn registration to see if
	// ours confirmed or not
}

// BreachPunisher handles the responsibility of constructing and broadcasting
// justice transactions. Justice transactions are constructed from previously
// accepted state updates uploaded by the watchtower's clients.
type BreachPunisher struct {
	cfg *PunisherConfig
}

// NewBreachPunisher constructs a new BreachPunisher given a PunisherConfig.
func NewBreachPunisher(cfg *PunisherConfig) *BreachPunisher {
	return &BreachPunisher{
		cfg: cfg,
	}
}

// Punish constructs a justice transaction given a JusticeDescriptor and
// publishes is it to the network.
func (p *BreachPunisher) Punish(desc *JusticeDescriptor, quit <-chan struct{}) error {
	justiceTxn, err := desc.CreateJusticeTxn()
	if err != nil {
		log.Errorf("Unable to create justice txn for "+
			"client=%s with breach-txid=%s: %v",
			desc.SessionInfo.ID, desc.BreachedCommitTx.TxHash(), err)
		return err
	}

	log.Infof("Publishing justice transaction for client=%s with txid=%s",
		desc.SessionInfo.ID, justiceTxn.TxHash())

	label := labels.MakeLabel(labels.LabelTypeJusticeTransaction, nil)
	err = p.cfg.PublishTx(justiceTxn, label)
	if err != nil {
		log.Errorf("Unable to publish justice txn for client=%s"+
			"with breach-txid=%s: %v",
			desc.SessionInfo.ID, desc.BreachedCommitTx.TxHash(), err)
		return err
	}

	// TODO(conner): register for spend and remove from db after
	// confirmation

	return nil
}
